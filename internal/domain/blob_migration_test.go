package domain_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// BLOB-14 end to end: attachments uploaded on the filesystem keep working while a deployment moves to S3, new uploads go
// to S3, and once everything is copied the filesystem can be dropped and the database still never refers to a missing object.
func TestBLOB14_AttachmentsSurviveTheMoveFromFilesystemToS3(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	fs := e.WithBlobs()
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "move my files"})
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		if _, err := e.Svc.AttachStream(ctx, e.Alice, is.Key, name, "text/plain", strings.NewReader("contents of "+name)); err != nil {
			t.Fatal(err)
		}
	}
	var shas []string
	rows, _ := e.Store.Pool.Query(ctx, `SELECT sha256 FROM blob ORDER BY sha256`)
	for rows.Next() {
		var h string
		rows.Scan(&h)
		shas = append(shas, h)
	}
	rows.Close()
	if len(shas) != 3 {
		t.Fatalf("%d blobs", len(shas))
	}
	// the new backend is S3, the old one stays as the fallback
	s3, _ := e.WithS3Blobs()
	fb := blob.NewFallback(s3, fs)
	if err := e.Svc.ConfigureBlobs(context.Background(), &domain.Blobs{Store: fb}); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		v, _ := e.Svc.View(ctx, e.Alice, is.Key)
		for _, at := range v.Attachments {
			if at.Name == name {
				o, err := e.Svc.OpenBlob(at.SHA256)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				defer o.Close()
				b, _ := io.ReadAll(o)
				return string(b)
			}
		}
		t.Fatalf("no attachment %s", name)
		return ""
	}
	if read("one.txt") != "contents of one.txt" {
		t.Fatal("an old attachment is not readable during the move")
	}
	// a new upload goes to S3 only
	if _, err := e.Svc.AttachStream(ctx, e.Alice, is.Key, "four.txt", "text/plain", strings.NewReader("contents of four.txt")); err != nil {
		t.Fatal(err)
	}
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	var four string
	for _, at := range v.Attachments {
		if at.Name == "four.txt" {
			four = at.SHA256
		}
	}
	if _, err := fs.Open(four); err != blob.ErrNotFound {
		t.Fatalf("a new upload reached the old store: %v", err)
	}
	// copy the rest, then verify the new backend alone holds everything the database refers to
	rep := blob.Migrate(ctx, fs, s3, shas, blob.MigrateOpts{Verify: true})
	if len(rep.Failed) != 0 {
		t.Fatalf("%+v", rep)
	}
	if err := e.Svc.ConfigureBlobs(context.Background(), &domain.Blobs{Store: s3}); err != nil {
		t.Fatal(err)
	}
	fr, err := e.Svc.Fsck(ctx, false)
	if err != nil || len(fr.Missing) != 0 || len(fr.Corrupt) != 0 || fr.Checked != 4 {
		t.Fatalf("%+v %v", fr, err)
	}
	for _, n := range []string{"one.txt", "two.txt", "three.txt", "four.txt"} {
		if got := read(n); got != "contents of "+n {
			t.Fatalf("%s: %q", n, got)
		}
	}
	// and back: the rollback is the same copy in the other direction
	back, _ := blob.NewFS(t.TempDir())
	all := append(append([]string{}, shas...), four)
	if rb := blob.Migrate(ctx, s3, back, all, blob.MigrateOpts{Verify: true}); len(rb.Failed) != 0 || rb.Copied != 4 {
		t.Fatalf("%+v", rb)
	}
}
