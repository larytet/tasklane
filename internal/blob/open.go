package blob

import (
	"errors"

	"github.com/larytet/tasklane/internal/config"
)

func openBackend(c config.Config, backend, fsDir string) (Store, error) {
	if backend != "s3" {
		return NewFS(fsDir)
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

// OpenPair opens the configured backend and, during a migration, the old one. secondary is nil when no migration is set up.
func OpenPair(c config.Config) (primary, secondary Store, err error) {
	if primary, err = openBackend(c, c.BlobBackend, c.BlobDir); err != nil {
		return nil, nil, err
	}
	if c.BlobFallback == "" || c.BlobFallback == "none" {
		return primary, nil, nil
	}
	if c.BlobFallback == c.BlobBackend {
		return nil, nil, errors.New("blob_fallback must be the other backend: it is the same as blob_backend")
	}
	if c.BlobFallback == "fs" {
		if c.BlobFallbackDir == "" || c.BlobFallbackDir == c.BlobDir {
			return nil, nil, errors.New("blob_fallback=fs needs blob_fallback_dir, the old attachment directory (not blob_dir)")
		}
	}
	if secondary, err = openBackend(c, c.BlobFallback, c.BlobFallbackDir); err != nil {
		return nil, nil, err
	}
	return primary, secondary, nil
}

// NewFromConfig opens the configured backend. The S3 backend needs a bucket and credentials; its local directory
// (blob_dir) holds upload staging only. With blob_fallback set, the old backend answers what the new one does not have yet.
func NewFromConfig(c config.Config) (Store, error) {
	p, sec, err := OpenPair(c)
	if err != nil || sec == nil {
		return p, err
	}
	return NewFallback(p, sec), nil
}
