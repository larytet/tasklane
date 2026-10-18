package blob

import (
	"context"
	"time"
)

// DirectStore is implemented by backends that let a browser send bytes straight to the store with presigned URLs. The
// application never sees those bytes until completion, when it reads them back to verify them (size, hash, type, scan,
// archive limits) before the object takes its place under its hash. Nothing is visible before that.
type DirectStore interface {
	PartSize() int64
	StagingKey(id string) string
	PresignPut(key string, expires time.Duration) string
	BeginMultipart(ctx context.Context, key string) (string, error)
	PresignPart(key, uploadID string, n int, expires time.Duration) string
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error
	AbortMultipart(ctx context.Context, key, uploadID string) error
	StatKey(ctx context.Context, key string) (int64, error)
	OpenKey(ctx context.Context, key string) (Object, error)
	DeleteKey(ctx context.Context, key string) error
	// Adopt moves a verified staged object to its hash key on the server side, or drops it when the hash exists.
	Adopt(ctx context.Context, key, sha string) (existed bool, err error)
}

var _ DirectStore = (*S3)(nil)

func (s *S3) PartSize() int64             { return s.C.cfg.PartSize }
func (s *S3) StagingKey(id string) string { return "staging/" + id }
func (s *S3) PresignPut(key string, d time.Duration) string {
	return s.C.Presign("PUT", key, d, 0, "")
}
func (s *S3) BeginMultipart(ctx context.Context, key string) (string, error) {
	return s.C.CreateMultipart(ctx, key)
}
func (s *S3) PresignPart(key, id string, n int, d time.Duration) string {
	return s.C.Presign("PUT", key, d, n, id)
}
func (s *S3) CompleteMultipart(ctx context.Context, key, id string, parts []Part) error {
	return s.C.CompleteMultipart(ctx, key, id, parts)
}
func (s *S3) AbortMultipart(ctx context.Context, key, id string) error {
	return s.C.AbortMultipart(ctx, key, id)
}
func (s *S3) StatKey(ctx context.Context, key string) (int64, error) { return s.C.Stat(ctx, key) }
func (s *S3) DeleteKey(ctx context.Context, key string) error        { return s.C.Delete(ctx, key) }
func (s *S3) OpenKey(ctx context.Context, key string) (Object, error) {
	size, err := s.C.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	return &s3Reader{c: s.C, key: key, size: size}, nil
}

func (s *S3) Adopt(ctx context.Context, key, sha string) (bool, error) {
	dst, err := objKey(sha)
	if err != nil {
		return false, err
	}
	if _, err := s.C.Stat(ctx, dst); err == nil {
		return true, s.C.Delete(ctx, key)
	} else if err != ErrNotFound {
		return false, err
	}
	if err := s.C.Copy(ctx, key, dst); err != nil {
		return false, err
	}
	return false, s.C.Delete(ctx, key)
}
