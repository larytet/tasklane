package blob

import (
	"errors"

	"github.com/larytet/tasklane/internal/config"
)

// NewFromConfig opens the configured backend. The S3 backend needs a bucket and credentials; its local directory
// (blob_dir) holds upload staging only.
func NewFromConfig(c config.Config) (Store, error) {
	if c.BlobBackend != "s3" {
		return NewFS(c.BlobDir)
	}
	if c.S3Bucket == "" {
		return nil, errors.New("blob_backend=s3 needs s3_bucket")
	}
	cl, err := NewS3Client(S3Config{Endpoint: c.S3Endpoint, Region: c.S3Region, Bucket: c.S3Bucket, Prefix: c.S3Prefix, AccessKey: c.S3AccessKey, SecretKey: c.S3SecretKey,
		PathStyle: c.S3PathStyle, SSE: c.S3SSE, KMSKeyID: c.S3KMSKeyID, PartSize: c.S3PartSizeMB << 20})
	if err != nil {
		return nil, err
	}
	return NewS3(cl, c.BlobDir)
}
