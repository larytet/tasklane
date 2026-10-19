package domain_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
)

const eicarText = `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`

func zipBytes() []byte {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, _ := w.Create("a.txt")
	f.Write([]byte("hello"))
	w.Close()
	return b.Bytes()
}

func (c *custMail) attachCount(t *testing.T) (n int) {
	t.Helper()
	c.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM attachment a JOIN issue i ON i.id=a.issue_id WHERE i.key=$1`, c.key).Scan(&n)
	return
}

func (c *custMail) upload(cu domain.Customer, name, mime string, data []byte) (domain.PortalRequest, error) {
	return c.Svc.PortalAttach(ctx, cu, "ABC", c.key, name, mime, bytes.NewReader(data))
}

// PORT-05 (files): off by default; within the portal's rules (size, count, classes) checked on the real bytes; refused and nothing stored for a
// flagged file, an archive, a file that is not what its name says, an empty or oversize one; visible to the customer and the people the request
// is shared with and nobody else; staff files hidden until shared; downloads never inline; a customer deletes only their own.
func TestPORT05_CustomerFiles(t *testing.T) {
	c := newCustMail(t)
	e := c.Env
	e.WithBlobs()
	png := pngBytes(40, 30)
	if _, err := c.upload(c.cora, "shot.png", "image/png", png); !isInvalid(err) || !strings.Contains(err.Error(), "cannot be added") {
		t.Fatalf("files are off by default: %v", err)
	}
	c.p.policy(t, domain.PortalPolicy{Enabled: true, AttachEnabled: true, AttachMaxMB: 1, AttachMaxFiles: 2})
	r, err := c.upload(c.cora, "shot.png", "image/png", png)
	if err != nil || len(r.Attachments) != 1 || !r.Attachments[0].Mine || r.Attachments[0].Name != "shot.png" {
		t.Fatalf("an allowed file: %+v %v", r.Attachments, err)
	}
	staff, err := e.Svc.View(ctx, e.Admin, c.key)
	if err != nil || len(staff.Attachments) != 1 || !staff.Attachments[0].FromCustomer || !staff.Attachments[0].CustomerVisible {
		t.Fatalf("staff see the customer's file as the customer's: %+v %v", staff.Attachments, err)
	}
	before := c.attachCount(t)
	for _, bad := range []struct {
		name, mime, want string
		data             []byte
	}{
		{"virus.txt", "text/plain", "refused", []byte(eicarText)},
		{"bundle.zip", "application/zip", "archive", zipBytes()},
		{"page.png", "image/png", "not the kind", []byte("<html><script>alert(1)</script></html>")},
		{"empty.txt", "text/plain", "empty", nil},
		{"big.txt", "text/plain", "at most 1 MB", bytes.Repeat([]byte("a"), 1<<20+1)},
	} {
		if _, err := c.upload(c.cora, bad.name, bad.mime, bad.data); !isInvalid(err) || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: want a refusal naming %q, got %v", bad.name, bad.want, err)
		}
	}
	if c.attachCount(t) != before {
		t.Fatalf("a refused file was stored")
	}
	var blobs int
	c.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM blob`).Scan(&blobs)
	if blobs != 1 {
		t.Fatalf("%d blobs after refusals: a refused file left bytes behind", blobs)
	}
	// the count limit, per request
	if _, err := c.upload(c.cora, "notes.txt", "text/plain", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.upload(c.cora, "more.txt", "text/plain", []byte("third")); !isInvalid(err) || !strings.Contains(err.Error(), "at most 2") {
		t.Fatalf("the third file: %v", err)
	}
	// who sees it
	dave := c.p.signIn(t, "dave@other.example")
	id := r.Attachments[0].ID
	if _, err := c.Svc.PortalFileInfo(ctx, dave, "ABC", id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a stranger reached the file: %v", err)
	}
	if _, err := c.upload(dave, "x.txt", "text/plain", []byte("x")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a stranger attached to the request: %v", err)
	}
	d, err := c.Svc.PortalFileInfo(ctx, c.cora, "ABC", id)
	if err != nil || d.Inline {
		t.Fatalf("download: %+v %v (a customer's file is never shown inline)", d, err)
	}
	if o, err := c.Svc.OpenBlob(d.SHA256); err != nil {
		t.Fatal(err)
	} else {
		got, _ := io.ReadAll(o)
		o.Close()
		if !bytes.Equal(got, png) {
			t.Fatal("the downloaded bytes differ")
		}
	}
	// staff files: hidden until shared, and never changed by a customer
	sa := attach(t, e, e.Admin, c.key, "internal-notes.txt", "text/plain", []byte("for staff only"))
	r, _ = c.Svc.PortalGetRequest(ctx, c.cora, "ABC", c.key)
	for _, a := range r.Attachments {
		if a.Name == "internal-notes.txt" {
			t.Fatal("a staff file was visible before it was shared")
		}
	}
	if _, err := c.Svc.PortalFileInfo(ctx, c.cora, "ABC", sa.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a staff file downloaded before it was shared: %v", err)
	}
	if err := e.Svc.SetAttachmentCustomerVisible(ctx, e.Alice, sa.ID, true); err != nil {
		t.Fatalf("a member shares: %v", err)
	}
	r, _ = c.Svc.PortalGetRequest(ctx, c.cora, "ABC", c.key)
	if len(r.Attachments) != 3 {
		t.Fatalf("after sharing: %+v", r.Attachments)
	}
	if err := c.Svc.PortalDeleteAttachment(ctx, c.cora, "ABC", sa.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a customer deleted a staff file: %v", err)
	}
	if err := e.Svc.SetAttachmentCustomerVisible(ctx, e.Alice, id, false); !isInvalid(err) {
		t.Fatalf("staff hid the customer's own file: %v", err)
	}
	if err := e.Svc.SetAttachmentCustomerVisible(ctx, e.Alice, sa.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.PortalFileInfo(ctx, c.cora, "ABC", sa.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a file taken back is still there: %v", err)
	}
	blocked := attach(t, e, e.Admin, c.key, "eicar.txt", "text/plain", []byte(eicarText))
	if err := e.Svc.SetAttachmentCustomerVisible(ctx, e.Admin, blocked.ID, true); !isInvalid(err) {
		t.Fatalf("a flagged file was shared: %v", err)
	}
	other := e.Issue("not a request", "")
	oa := attach(t, e, e.Admin, other.Key, "a.txt", "text/plain", []byte("a"))
	if err := e.Svc.SetAttachmentCustomerVisible(ctx, e.Admin, oa.ID, true); !isInvalid(err) {
		t.Fatalf("a file on an ordinary issue was shared: %v", err)
	}
	// a colleague the request is shared with sees the customer's file
	if _, err := c.Svc.PortalShare(ctx, c.cora, "ABC", c.key, domain.PortalShareIn{Add: []string{"dave@other.example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.PortalFileInfo(ctx, dave, "ABC", id); err != nil {
		t.Fatalf("a colleague with the request cannot download: %v", err)
	}
	// the owner deletes their own; a resolved request takes no more files
	if err := c.Svc.PortalDeleteAttachment(ctx, c.cora, "ABC", id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.PortalFileInfo(ctx, c.cora, "ABC", id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a deleted file: %v", err)
	}
	c.Store.Pool.Exec(ctx, `UPDATE issue SET status_id=(SELECT st.id FROM status st WHERE st.workflow_id=(SELECT workflow_id FROM project WHERE key='ABC') AND st.category='done' LIMIT 1) WHERE key=$1`, c.key)
	if _, err := c.upload(c.cora, "late.txt", "text/plain", []byte("late")); !isInvalid(err) || !strings.Contains(err.Error(), "resolved") {
		t.Fatalf("a file on a resolved request: %v", err)
	}
}

// The rules themselves: bounds are checked, the classes a customer may be allowed exclude archives and video, and an allowed-class list narrows what is accepted.
func TestPORT05_FileRulesAreValidatedAndApplied(t *testing.T) {
	c := newCustMail(t)
	c.WithBlobs()
	for _, bad := range []domain.PortalPolicy{
		{Enabled: true, AttachEnabled: true, AttachMaxMB: 101},
		{Enabled: true, AttachEnabled: true, AttachMaxFiles: 21},
		{Enabled: true, AttachEnabled: true, AttachClasses: []string{"archive"}},
		{Enabled: true, AttachEnabled: true, AttachClasses: []string{"video"}},
	} {
		if _, err := c.Svc.SetPortalPolicy(ctx, c.Admin, "ABC", bad); !isInvalid(err) {
			t.Errorf("accepted %+v: %v", bad, err)
		}
	}
	c.p.policy(t, domain.PortalPolicy{Enabled: true, AttachEnabled: true, AttachClasses: []string{"image"}})
	if _, err := c.upload(c.cora, "notes.txt", "text/plain", []byte("plain text is a log-class file here")); !isInvalid(err) || !strings.Contains(err.Error(), "not accepted") {
		t.Fatalf("a class that is not allowed: %v", err)
	}
	if _, err := c.upload(c.cora, "shot.png", "image/png", pngBytes(10, 10)); err != nil {
		t.Fatal(err)
	}
	got, err := c.Svc.GetPortalPolicy(ctx, c.Admin, "ABC")
	if err != nil || got.AttachMaxMB != 10 || got.AttachMaxFiles != 5 || len(got.AttachClasses) != 1 {
		t.Fatalf("policy round trip: %+v %v", got, err)
	}
}

// Erasing a customer removes the files they sent, from the store's accounting and from the history, and leaves staff files alone.
func TestPORT05_ErasingACustomerRemovesTheirFiles(t *testing.T) {
	p := newPrivCust(t)
	p.WithBlobs()
	p.p.policy(t, domain.PortalPolicy{Enabled: true, AttachEnabled: true})
	if _, err := p.upload(p.cora, "cora-czerny-passport.png", "image/png", pngBytes(20, 20)); err != nil {
		t.Fatal(err)
	}
	attach(t, p.Env, p.Admin, p.key, "staff-note.txt", "text/plain", []byte("kept"))
	exp, err := p.Svc.ExportCustomerData(ctx, p.Admin, p.id)
	if err != nil || !strings.Contains(string(mustJSON(exp)), "cora-czerny-passport.png") {
		t.Fatalf("the export lacks the file: %v", err)
	}
	if _, err := p.Svc.EraseCustomer(ctx, p.Admin, p.id, domain.EraseOpts{ScrubMentions: true}); err != nil {
		t.Fatal(err)
	}
	if found := p.traces(t, "cora-czerny-passport"); len(found) > 0 {
		t.Fatalf("the file name survived: %v", found)
	}
	var n int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM attachment a JOIN issue i ON i.id=a.issue_id WHERE i.key=$1`, p.key).Scan(&n)
	if n != 1 {
		t.Fatalf("%d attachments left on the request, want the staff file only", n)
	}
	var held int // the bytes are no longer referenced (the collector may already have removed them)
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM blob WHERE mime='image/png' AND refcount>0`).Scan(&held)
	if held != 0 {
		t.Fatalf("the customer's file is still referenced: %d", held)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
