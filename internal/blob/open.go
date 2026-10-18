package blob

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/larytet/tasklane/internal/config"
)

func openBackend(c config.Config, backend, fsDir string) (Store, error) {
	return openBackendStaging(c, backend, fsDir, c.BlobDir)
}

// openBackendStaging opens a backend; an S3 one stages uploads in its own local directory.
func openBackendStaging(c config.Config, backend, fsDir, staging string) (Store, error) {
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
	return NewS3(cl, staging)
}

// OpenPair opens the configured backend and, during a migration, the old one. secondary is nil when no migration is set up.
func OpenPair(c config.Config) (primary, secondary Store, err error) {
	if primary, err = openBackend(c, c.BlobBackend, c.BlobDir); err != nil {
		return nil, nil, err
	}
	if primary, err = withTiering(c, primary); err != nil {
		return nil, nil, err
	}
	if primary, err = withEncryption(c, primary); err != nil {
		return nil, nil, err
	}
	if c.BlobFallback == "" || c.BlobFallback == "none" {
		return primary, nil, nil
	}
	if c.BlobFallback == c.BlobBackend && c.BlobBackend != "fs" {
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

// ParseKeys reads "id:hex,id:hex" into master keys.
func ParseKeys(list string) (map[byte][]byte, error) {
	keys := map[byte][]byte{}
	for _, part := range strings.Split(list, ",") {
		id, h, ok := strings.Cut(strings.TrimSpace(part), ":")
		n, err := strconv.ParseUint(id, 10, 8)
		if !ok || err != nil {
			return nil, fmt.Errorf("blob_encryption_keys: %q is not id:hex with an id from 0 to 255", part)
		}
		k, err := hex.DecodeString(h)
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("blob_encryption_keys: key %d must be 64 hex characters (32 bytes)", n)
		}
		if _, dup := keys[byte(n)]; dup {
			return nil, fmt.Errorf("blob_encryption_keys: key id %d appears twice", n)
		}
		keys[byte(n)] = k
	}
	return keys, nil
}

func withEncryption(c config.Config, s Store) (Store, error) {
	if c.BlobEncKeys == "" {
		return s, nil
	}
	keys, err := ParseKeys(c.BlobEncKeys)
	if err != nil {
		return nil, err
	}
	cur, err := strconv.ParseUint(c.BlobEncCurrent, 10, 8)
	if err != nil {
		return nil, errors.New("blob_encryption_keys needs blob_encryption_current, the id of the key to use (0-255)")
	}
	return NewEncrypted(s, byte(cur), keys)
}

// withTiering puts a cold tier behind the configured backend. The cold tier must be a different place: the other kind of
// backend, or another directory.
func withTiering(c config.Config, hot Store) (Store, error) {
	if c.BlobCold == "" || c.BlobCold == "none" {
		return hot, nil
	}
	switch {
	case c.BlobCold == "fs" && c.BlobColdDir == "":
		return nil, errors.New("blob_cold_backend=fs needs blob_cold_dir")
	case c.BlobCold == "fs" && c.BlobBackend == "fs" && (c.BlobColdDir == c.BlobDir):
		return nil, errors.New("blob_cold_dir must be a different directory from blob_dir")
	case c.BlobCold == "s3" && c.BlobBackend == "s3":
		return nil, errors.New("blob_cold_backend=s3 needs blob_backend=fs: there is one set of s3_* settings, so both tiers cannot be S3")
	}
	cold, err := openBackendStaging(c, c.BlobCold, c.BlobColdDir, strings.TrimRight(c.BlobDir, "/")+"/cold-staging")
	if err != nil {
		return nil, fmt.Errorf("cold tier: %w", err)
	}
	return NewTiered(hot, cold), nil
}
