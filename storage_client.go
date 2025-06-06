package gosnowflake

import (
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"time"
)

const (
	defaultConcurrency = 1
	defaultMaxRetry    = 5
)

// implemented by localUtil and remoteStorageUtil
type storageUtil interface {
	createClient(*execResponseStageInfo, bool, *Config) (cloudClient, error)
	uploadOneFileWithRetry(*fileMetadata) error
	downloadOneFile(*fileMetadata) error
}

// implemented by snowflakeS3Util, snowflakeAzureUtil and snowflakeGcsUtil
type cloudUtil interface {
	createClient(*execResponseStageInfo, bool) (cloudClient, error)
	getFileHeader(*fileMetadata, string) (*fileHeader, error)
	uploadFile(string, *fileMetadata, int, int64) error
	nativeDownloadFile(*fileMetadata, string, int64) error
}

type cloudClient interface{}

type remoteStorageUtil struct {
	cfg *Config
}

func (rsu *remoteStorageUtil) getNativeCloudType(cli string, cfg *Config) cloudUtil {
	if cloudType(cli) == s3Client {
		return &snowflakeS3Client{
			cfg,
		}
	} else if cloudType(cli) == azureClient {
		return &snowflakeAzureClient{
			cfg,
		}
	} else if cloudType(cli) == gcsClient {
		return &snowflakeGcsClient{
			cfg,
		}
	}
	return nil
}

// call cloud utils' native create client methods
func (rsu *remoteStorageUtil) createClient(info *execResponseStageInfo, useAccelerateEndpoint bool, cfg *Config) (cloudClient, error) {
	utilClass := rsu.getNativeCloudType(info.LocationType, cfg)
	return utilClass.createClient(info, useAccelerateEndpoint)
}

func (rsu *remoteStorageUtil) uploadOneFile(meta *fileMetadata) error {
	utilClass := rsu.getNativeCloudType(meta.stageInfo.LocationType, meta.sfa.sc.cfg)
	maxConcurrency := int(meta.parallel)
	var lastErr error
	maxRetry := defaultMaxRetry
	for retry := 0; retry < maxRetry; retry++ {
		if !meta.overwrite {
			header, err := utilClass.getFileHeader(meta, meta.dstFileName)
			if meta.resStatus == notFoundFile {
				err := utilClass.uploadFile(meta.realSrcFileName, meta, maxConcurrency, meta.options.MultiPartThreshold)
				if err != nil {
					logger.Warnf("Error uploading %v. err: %v", meta.realSrcFileName, err)
				}
			} else if err != nil {
				return err
			}
			if header != nil && meta.resStatus == uploaded {
				meta.dstFileSize = 0
				meta.resStatus = skipped
				return nil
			}
		}
		if meta.overwrite || meta.resStatus == notFoundFile {
			err := utilClass.uploadFile(meta.realSrcFileName, meta, maxConcurrency, meta.options.MultiPartThreshold)
			if err != nil {
				logger.Debugf("Error uploading %v. err: %v", meta.realSrcFileName, err)
			}
		}
		if meta.resStatus == uploaded || meta.resStatus == renewToken || meta.resStatus == renewPresignedURL {
			return nil
		} else if meta.resStatus == needRetry {
			if !meta.noSleepingTime {
				sleepingTime := intMin(int(math.Exp2(float64(retry))), 16)
				time.Sleep(time.Second * time.Duration(sleepingTime))
			}
		} else if meta.resStatus == needRetryWithLowerConcurrency {
			maxConcurrency = int(meta.parallel) - (retry * int(meta.parallel) / maxRetry)
			maxConcurrency = intMax(defaultConcurrency, maxConcurrency)
			meta.lastMaxConcurrency = maxConcurrency

			if !meta.noSleepingTime {
				sleepingTime := intMin(int(math.Exp2(float64(retry))), 16)
				time.Sleep(time.Second * time.Duration(sleepingTime))
			}
		}
		lastErr = meta.lastError
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("unkown error uploading %v", meta.realSrcFileName)
}

func (rsu *remoteStorageUtil) uploadOneFileWithRetry(meta *fileMetadata) error {
	utilClass := rsu.getNativeCloudType(meta.stageInfo.LocationType, rsu.cfg)
	retryOuter := true
	for i := 0; i < 10; i++ {
		// retry
		if err := rsu.uploadOneFile(meta); err != nil {
			return err
		}
		retryInner := true
		if meta.resStatus == uploaded || meta.resStatus == skipped {
			for j := 0; j < 10; j++ {
				status := meta.resStatus
				if _, err := utilClass.getFileHeader(meta, meta.dstFileName); err != nil {
					logger.Infof("error while getting file %v header. %v", meta.dstFileSize, err)
				}
				// check file header status and verify upload/skip
				if meta.resStatus == notFoundFile {
					time.Sleep(time.Second) // wait 1 second
					continue
				} else {
					retryInner = false
					meta.resStatus = status
					break
				}
			}
		}
		if !retryInner {
			retryOuter = false
			break
		} else {
			continue
		}
	}
	if retryOuter {
		// wanted to continue retrying but could not upload/find file
		meta.resStatus = errStatus
	}
	return nil
}

func (rsu *remoteStorageUtil) downloadOneFile(meta *fileMetadata) error {
	fullDstFileName := path.Join(meta.localLocation, baseName(meta.dstFileName))
	fullDstFileName, err := expandUser(fullDstFileName)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(fullDstFileName) {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		fullDstFileName = filepath.Join(cwd, fullDstFileName)
	}
	baseDir, err := getDirectory()
	if err != nil {
		return err
	}
	if _, err = os.Stat(baseDir); os.IsNotExist(err) {
		if err = os.MkdirAll(baseDir, os.ModePerm); err != nil {
			return err
		}
	}

	utilClass := rsu.getNativeCloudType(meta.stageInfo.LocationType, meta.sfa.sc.cfg)
	header, err := utilClass.getFileHeader(meta, meta.srcFileName)
	if err != nil {
		return err
	}
	if header != nil {
		meta.srcFileSize = header.contentLength
	}

	maxConcurrency := meta.parallel
	var lastErr error
	maxRetry := defaultMaxRetry
	for retry := 0; retry < maxRetry; retry++ {
		if err = utilClass.nativeDownloadFile(meta, fullDstFileName, maxConcurrency); err != nil {
			return err
		}
		if meta.resStatus == downloaded {
			if meta.encryptionMaterial != nil {
				if meta.presignedURL != nil {
					header, err = utilClass.getFileHeader(meta, meta.srcFileName)
					if err != nil {
						return err
					}
				}
				if meta.options.GetFileToStream {
					totalFileSize, err := decryptStreamCBC(header.encryptionMetadata,
						meta.encryptionMaterial, 0, meta.dstStream, meta.sfa.streamBuffer)
					if err != nil {
						return err
					}
					meta.sfa.streamBuffer.Truncate(totalFileSize)
					meta.dstFileSize = int64(totalFileSize)
				} else {
					tmpDstFileName, err := decryptFileCBC(header.encryptionMetadata,
						meta.encryptionMaterial, fullDstFileName, 0, meta.tmpDir)
					if err != nil {
						return err
					}
					if err = os.Rename(tmpDstFileName, fullDstFileName); err != nil {
						return err
					}
				}

			}
			if !meta.options.GetFileToStream {
				if fi, err := os.Stat(fullDstFileName); err == nil {
					meta.dstFileSize = fi.Size()
				}
			}
			return nil
		}
		lastErr = meta.lastError
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("unkown error downloading %v", fullDstFileName)
}
