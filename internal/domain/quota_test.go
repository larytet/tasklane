package domain_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func TestBLOB18_StorageQuotaIsEnforcedWithAClearMessageAndFreedByDeleting(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.WithBlobs()
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "files"})
	file := func(n int, seed byte) *bytes.Reader { return bytes.NewReader(bytes.Repeat([]byte{seed}, n)) }
	one := int64(1)
	if err := e.Svc.SetStorageQuota(ctx, e.Admin, "ABC", &one); err != nil { // one MiB
		t.Fatal(err)
	}
	a1, err := e.Svc.AttachStream(ctx, e.Alice, is.Key, "a.bin", "application/octet-stream", file(600<<10, 'a'))
	if err != nil {
		t.Fatal(err)
	}
	// the second file does not fit: refused, with the numbers and what to do, and nothing is stored
	_, err = e.Svc.AttachStream(ctx, e.Alice, is.Key, "b.bin", "application/octet-stream", file(600<<10, 'b'))
	var qe *domain.QuotaError
	if !errors.As(err, &qe) || qe.Quota != 1<<20 || qe.Used != 600<<10 || !strings.Contains(err.Error(), "ask an administrator") {
		t.Fatalf("%v", err)
	}
	var files int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM attachment`).Scan(&files)
	if files != 1 {
		t.Fatalf("%d attachments", files)
	}
	var blobs int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM blob`).Scan(&blobs)
	if blobs != 1 {
		t.Fatalf("a refused upload left a blob row: %d", blobs)
	}
	// a file that fits still goes in; the resumable path refuses early from the size it is told
	if _, err := e.Svc.AttachStream(ctx, e.Alice, is.Key, "small.bin", "application/octet-stream", file(100<<10, 'c')); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.StartUpload(ctx, e.Alice, is.Key, "big.bin", "application/octet-stream", 700<<10); !errors.As(err, &qe) {
		t.Fatalf("%v", err)
	}
	// the report says where the space went
	r, err := e.Svc.StorageUsage(ctx, e.Alice, "ABC")
	if err != nil || r.Used != 700<<10 || r.Files != 2 || *r.Quota != 1<<20 || r.PercentOf == nil || *r.PercentOf < 66 || *r.PercentOf > 69 {
		t.Fatalf("%+v %v", r, err)
	}
	if r.ByClass["document"]+r.ByClass["archive"]+r.ByClass["log"]+r.ByClass["image"]+r.ByClass["video"] != r.Used {
		t.Fatalf("%v", r.ByClass)
	}
	// deleting a file frees its space at once
	if err := e.Svc.DeleteAttachment(ctx, e.Alice, is.Key, a1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AttachStream(ctx, e.Alice, is.Key, "b.bin", "application/octet-stream", file(600<<10, 'b')); err != nil {
		t.Fatalf("space was not freed: %v", err)
	}
	// removing the quota lifts the limit; another project is not affected by this one's
	if err := e.Svc.SetStorageQuota(ctx, e.Admin, "ABC", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AttachStream(ctx, e.Alice, is.Key, "c.bin", "application/octet-stream", file(2<<20, 'd')); err != nil {
		t.Fatal(err)
	}
	// who may set it and read it
	if err := e.Svc.SetStorageQuota(ctx, e.Alice, "ABC", &one); err == nil {
		t.Fatal("a member set a quota")
	}
	neg := int64(-1)
	if err := e.Svc.SetStorageQuota(ctx, e.Admin, "ABC", &neg); err == nil {
		t.Fatal("a negative quota")
	}
	outsider := e.NewUser("out@example.com", "Out", false)
	if _, err := e.Svc.StorageUsage(ctx, outsider, "ABC"); err != domain.ErrNotFound {
		t.Fatalf("%v", err)
	}
	ov, err := e.Svc.StorageOverview(ctx, e.Admin)
	if err != nil || len(ov) == 0 || ov[0].Project != "ABC" || ov[0].Quota != nil {
		t.Fatalf("%+v %v", ov, err)
	}
	if _, err := e.Svc.StorageOverview(ctx, e.Alice); err == nil {
		t.Fatal("a member read the overview")
	}
}
