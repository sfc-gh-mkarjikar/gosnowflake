package gosnowflake

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	gcsMetadataPrefix             = "x-goog-meta-"
	gcsMetadataSfcDigest          = gcsMetadataPrefix + sfcDigest
	gcsMetadataMatdescKey         = gcsMetadataPrefix + "matdesc"
	gcsMetadataEncryptionDataProp = gcsMetadataPrefix + "encryptiondata"
	gcsFileHeaderDigest           = "gcs-file-header-digest"
	gcsRegionMeCentral2           = "me-central2"
)

type snowflakeGcsClient struct {
	cfg *Config
}

type gcsLocation struct {
	bucketName string
	path       string
}

func (util *snowflakeGcsClient) createClient(info *execResponseStageInfo, _ bool) (cloudClient, error) {
	if info.Creds.GcsAccessToken != "" {
		logger.Debug("Using GCS downscoped token")
		return info.Creds.GcsAccessToken, nil
	}
	logger.Debugf("No access token received from GS, using presigned url: %s", info.PresignedURL)
	return "", nil
}

// cloudUtil implementation
func (util *snowflakeGcsClient) getFileHeader(meta *fileMetadata, filename string) (*fileHeader, error) {
	if meta.resStatus == uploaded || meta.resStatus == downloaded {
		return &fileHeader{
			digest:             meta.gcsFileHeaderDigest,
			contentLength:      meta.gcsFileHeaderContentLength,
			encryptionMetadata: meta.gcsFileHeaderEncryptionMeta,
		}, nil
	}
	if meta.presignedURL != nil {
		meta.resStatus = notFoundFile
	} else {
		URL, err := util.generateFileURL(meta.stageInfo, strings.TrimLeft(filename, "/"))
		if err != nil {
			return nil, err
		}
		accessToken, ok := meta.client.(string)
		if !ok {
			return nil, fmt.Errorf("interface convertion. expected type string but got %T", meta.client)
		}
		gcsHeaders := map[string]string{
			"Authorization": "Bearer " + accessToken,
		}

		resp, err := withCloudStorageTimeout(util.cfg, func(ctx context.Context) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, "HEAD", URL.String(), nil)
			if err != nil {
				return nil, err
			}
			for k, v := range gcsHeaders {
				req.Header.Add(k, v)
			}
			client := newGcsClient(util.cfg)
			// for testing only
			if meta.mockGcsClient != nil {
				client = meta.mockGcsClient
			}
			resp, err := client.Do(req)
			if err != nil && strings.HasSuffix(err.Error(), "EOF") {
				logger.Debug("Retrying HEAD request because of EOF")
				resp, err = client.Do(req)
			}
			return resp, err
		})
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			meta.lastError = fmt.Errorf("%v", resp.Status)
			meta.resStatus = errStatus
			if resp.StatusCode == 403 || resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 503 {
				meta.lastError = fmt.Errorf("%v", resp.Status)
				meta.resStatus = needRetry
				return nil, meta.lastError
			}
			if resp.StatusCode == 404 {
				meta.resStatus = notFoundFile
			} else if util.isTokenExpired(resp) {
				meta.lastError = fmt.Errorf("%v", resp.Status)
				meta.resStatus = renewToken
			}
			return nil, meta.lastError
		}

		digest := resp.Header.Get(gcsMetadataSfcDigest)
		contentLength, err := strconv.Atoi(resp.Header.Get("content-length"))
		if err != nil {
			return nil, err
		}
		var encryptionMeta *encryptMetadata
		if resp.Header.Get(gcsMetadataEncryptionDataProp) != "" {
			var encryptData *encryptionData
			err := json.Unmarshal([]byte(resp.Header.Get(gcsMetadataEncryptionDataProp)), &encryptData)
			if err != nil {
				logger.Error(err)
			}
			if encryptData != nil {
				encryptionMeta = &encryptMetadata{
					key: encryptData.WrappedContentKey.EncryptionKey,
					iv:  encryptData.ContentEncryptionIV,
				}
				if resp.Header.Get(gcsMetadataMatdescKey) != "" {
					encryptionMeta.matdesc = resp.Header.Get(gcsMetadataMatdescKey)
				}
			}
		}
		meta.resStatus = uploaded
		return &fileHeader{
			digest:             digest,
			contentLength:      int64(contentLength),
			encryptionMetadata: encryptionMeta,
		}, nil
	}
	return nil, nil
}

type gcsAPI interface {
	Do(req *http.Request) (*http.Response, error)
}

// cloudUtil implementation
func (util *snowflakeGcsClient) uploadFile(
	dataFile string,
	meta *fileMetadata,
	maxConcurrency int,
	multiPartThreshold int64) error {
	uploadURL := meta.presignedURL
	var accessToken string
	var err error

	if uploadURL == nil {
		uploadURL, err = util.generateFileURL(meta.stageInfo, strings.TrimLeft(meta.dstFileName, "/"))
		if err != nil {
			return err
		}
		var ok bool
		accessToken, ok = meta.client.(string)
		if !ok {
			return fmt.Errorf("interface convertion. expected type string but got %T", meta.client)
		}
	}

	var contentEncoding string
	if meta.dstCompressionType != nil {
		contentEncoding = strings.ToLower(meta.dstCompressionType.name)
	}

	if contentEncoding == "gzip" {
		contentEncoding = ""
	}

	gcsHeaders := make(map[string]string)
	gcsHeaders[httpHeaderContentEncoding] = contentEncoding
	gcsHeaders[gcsMetadataSfcDigest] = meta.sha256Digest
	if accessToken != "" {
		gcsHeaders["Authorization"] = "Bearer " + accessToken
	}

	if meta.encryptMeta != nil {
		encryptData := encryptionData{
			"FullBlob",
			contentKey{
				"symmKey1",
				meta.encryptMeta.key,
				"AES_CBC_256",
			},
			encryptionAgent{
				"1.0",
				"AES_CBC_256",
			},
			meta.encryptMeta.iv,
			keyMetadata{
				"Java 5.3.0",
			},
		}
		b, err := json.Marshal(&encryptData)
		if err != nil {
			return err
		}
		gcsHeaders[gcsMetadataEncryptionDataProp] = string(b)
		gcsHeaders[gcsMetadataMatdescKey] = meta.encryptMeta.matdesc
	}

	var uploadSrc io.Reader
	if meta.srcStream != nil {
		uploadSrc = meta.srcStream
		if meta.realSrcStream != nil {
			uploadSrc = meta.realSrcStream
		}
	} else {
		uploadSrc, err = os.Open(dataFile)
		if err != nil {
			return err
		}
	}

	resp, err := withCloudStorageTimeout(util.cfg, func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL.String(), uploadSrc)
		if err != nil {
			return nil, err
		}
		for k, v := range gcsHeaders {
			req.Header.Add(k, v)
		}
		client := newGcsClient(util.cfg)
		// for testing only
		if meta.mockGcsClient != nil {
			client = meta.mockGcsClient
		}
		return client.Do(req)
	})

	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 403 || resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 503 {
			meta.lastError = fmt.Errorf("%v", resp.Status)
			meta.resStatus = needRetry
		} else if accessToken == "" && resp.StatusCode == 400 && meta.lastError == nil {
			meta.lastError = fmt.Errorf("%v", resp.Status)
			meta.resStatus = renewPresignedURL
		} else if accessToken != "" && util.isTokenExpired(resp) {
			meta.lastError = fmt.Errorf("%v", resp.Status)
			meta.resStatus = renewToken
		} else {
			meta.lastError = fmt.Errorf("%v", resp.Status)
		}
		return meta.lastError
	}

	if meta.options.putCallback != nil {
		meta.options.putCallback = &snowflakeProgressPercentage{
			filename:        dataFile,
			fileSize:        float64(meta.srcFileSize),
			outputStream:    meta.options.putCallbackOutputStream,
			showProgressBar: meta.options.showProgressBar,
		}
	}

	meta.dstFileSize = meta.uploadSize
	meta.resStatus = uploaded

	meta.gcsFileHeaderDigest = gcsHeaders[gcsFileHeaderDigest]
	meta.gcsFileHeaderContentLength = meta.uploadSize
	if err = json.Unmarshal([]byte(gcsHeaders[gcsMetadataEncryptionDataProp]), &meta.encryptMeta); err != nil {
		return err
	}
	meta.gcsFileHeaderEncryptionMeta = meta.encryptMeta
	return nil
}

// cloudUtil implementation
func (util *snowflakeGcsClient) nativeDownloadFile(
	meta *fileMetadata,
	fullDstFileName string,
	maxConcurrency int64) error {
	downloadURL := meta.presignedURL
	var accessToken string
	var err error
	gcsHeaders := make(map[string]string)

	if downloadURL == nil || downloadURL.String() == "" {
		downloadURL, err = util.generateFileURL(meta.stageInfo, strings.TrimLeft(meta.srcFileName, "/"))
		if err != nil {
			return err
		}
		var ok bool
		accessToken, ok = meta.client.(string)
		if !ok {
			return fmt.Errorf("interface convertion. expected type string but got %T", meta.client)
		}
		if accessToken != "" {
			gcsHeaders["Authorization"] = "Bearer " + accessToken
		}
	}

	resp, err := withCloudStorageTimeout(util.cfg, func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL.String(), nil)
		if err != nil {
			return nil, err
		}
		for k, v := range gcsHeaders {
			req.Header.Add(k, v)
		}
		client := newGcsClient(util.cfg)
		// for testing only
		if meta.mockGcsClient != nil {
			client = meta.mockGcsClient
		}
		return client.Do(req)
	})

	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 403 || resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 503 {
			meta.lastError = fmt.Errorf("%v", resp.Status)
			meta.resStatus = needRetry
		} else if resp.StatusCode == 404 {
			meta.lastError = fmt.Errorf("%v", resp.Status)
			meta.resStatus = notFoundFile
		} else if accessToken == "" && resp.StatusCode == 400 && meta.lastError == nil {
			meta.lastError = fmt.Errorf("%v", resp.Status)
			meta.resStatus = renewPresignedURL
		} else if accessToken != "" && util.isTokenExpired(resp) {
			meta.lastError = fmt.Errorf("%v", resp.Status)
			meta.resStatus = renewToken
		} else {
			meta.lastError = fmt.Errorf("%v", resp.Status)
		}
		return meta.lastError
	}

	if meta.options.GetFileToStream {
		if _, err := io.Copy(meta.dstStream, resp.Body); err != nil {
			return err
		}
	} else {
		f, err := os.OpenFile(fullDstFileName, os.O_CREATE|os.O_WRONLY, readWriteFileMode)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err = io.Copy(f, resp.Body); err != nil {
			return err
		}
		fi, err := os.Stat(fullDstFileName)
		if err != nil {
			return err
		}
		meta.srcFileSize = fi.Size()
	}

	var encryptMeta encryptMetadata
	if resp.Header.Get(gcsMetadataEncryptionDataProp) != "" {
		var encryptData *encryptionData
		if err = json.Unmarshal([]byte(resp.Header.Get(gcsMetadataEncryptionDataProp)), &encryptData); err != nil {
			return err
		}
		if encryptData != nil {
			encryptMeta = encryptMetadata{
				encryptData.WrappedContentKey.EncryptionKey,
				encryptData.ContentEncryptionIV,
				"",
			}
			if key := resp.Header.Get(gcsMetadataMatdescKey); key != "" {
				encryptMeta.matdesc = key
			}
		}
	}
	meta.resStatus = downloaded
	meta.gcsFileHeaderDigest = resp.Header.Get(gcsMetadataSfcDigest)
	meta.gcsFileHeaderContentLength = resp.ContentLength
	meta.gcsFileHeaderEncryptionMeta = &encryptMeta
	return nil
}

func (util *snowflakeGcsClient) extractBucketNameAndPath(location string) *gcsLocation {
	containerName := location
	var path string
	if strings.Contains(location, "/") {
		containerName = location[:strings.Index(location, "/")]
		path = location[strings.Index(location, "/")+1:]
		if path != "" && !strings.HasSuffix(path, "/") {
			path += "/"
		}
	}
	return &gcsLocation{containerName, path}
}

func (util *snowflakeGcsClient) generateFileURL(stageInfo *execResponseStageInfo, filename string) (*url.URL, error) {
	gcsLoc := util.extractBucketNameAndPath(stageInfo.Location)
	fullFilePath := gcsLoc.path + filename
	endPoint := getGcsCustomEndpoint(stageInfo)
	URL, err := url.Parse(endPoint + "/" + gcsLoc.bucketName + "/" + url.QueryEscape(fullFilePath))
	if err != nil {
		return nil, err
	}
	return URL, nil
}

func (util *snowflakeGcsClient) isTokenExpired(resp *http.Response) bool {
	return resp.StatusCode == 401
}

func newGcsClient(cfg *Config) gcsAPI {
	return &http.Client{
		Transport: getTransport(cfg),
	}
}

func getGcsCustomEndpoint(info *execResponseStageInfo) string {
	endpoint := "https://storage.googleapis.com"

	// TODO: SNOW-1789759 hardcoded region will be replaced in the future
	isRegionalURLEnabled := (strings.ToLower(info.Region) == gcsRegionMeCentral2) || info.UseRegionalURL
	if info.EndPoint != "" {
		endpoint = fmt.Sprintf("https://%s", info.EndPoint)
	} else if info.Region != "" && isRegionalURLEnabled {
		endpoint = fmt.Sprintf("https://storage.%s.rep.googleapis.com", strings.ToLower(info.Region))
	}
	return endpoint
}
