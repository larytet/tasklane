package api_test

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// MAIL-09, MAIL-21: the webhook transport and the quarantine administration over HTTP.
func TestMAIL09_21_InboundWebhookAndQuarantineOverHTTP(t *testing.T) {
	e, alice, _, key := setup(t)
	e.WithBlobs()
	e.Svc.Mail = &domain.MailConfig{From: "t@example.com", Domain: "tasklane.example.com", BaseURL: "https://tasklane.example.com"}
	src, err := e.Svc.CreateMailSource(testutil.Ctx(), e.Admin, "relay", true)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := e.Svc.CreateAPIToken(testutil.Ctx(), e.Admin.ID, "t")
	admin := &client{t: t, base: alice.base, token: tok}
	post := func(id, sig string, body []byte) (int, string) {
		req, _ := http.NewRequest("POST", alice.base+"/api/v1/mail/inbound/"+id, bytes.NewReader(body))
		req.Header.Set("X-Tasklane-Signature", sig)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	raw := func(from, mid, auth string) []byte {
		return []byte("From: " + from + "\r\nTo: tasklane@example.com\r\nSubject: Re: [" + key + "] Checkout fails\r\nMessage-ID: <" + mid + "@mail.example.com>\r\n" + auth + "Content-Type: text/plain\r\n\r\nreply by webhook\r\n")
	}
	ok := raw("bob@example.com", "w1", "Authentication-Results: mx; dkim=pass header.d=example.com\r\n")
	if c, b := post(src.ID, domain.SignInbound("wrong", ok), ok); c != 403 || strings.Contains(b, "accepted") {
		t.Fatalf("%d %s", c, b)
	}
	if c, _ := post(src.ID, "", ok); c != 403 {
		t.Fatalf("unsigned: %d", c)
	}
	if c, b := post(src.ID, domain.SignInbound(src.Secret, ok), ok); c != 200 || !strings.Contains(b, `"outcome":"accepted"`) {
		t.Fatalf("%d %s", c, b)
	}
	if c, b := post(src.ID, domain.SignInbound(src.Secret, ok), ok); c != 200 || !strings.Contains(b, `"duplicate":true`) {
		t.Fatalf("%d %s", c, b)
	}
	if c, _ := post(src.ID, "x", bytes.Repeat([]byte("x"), 27<<20)); c != 413 {
		t.Fatalf("oversized body: %d", c)
	}
	// the comment shows "via email"
	_, out, _ := alice.do("GET", "/api/v1/issues/"+key+"/view", nil)
	cs, _ := out["comments"].([]any)
	if len(cs) != 1 || cs[0].(map[string]any)["via_email"] != true {
		t.Fatalf("%v", out["comments"])
	}
	// an unproven one is quarantined; only an administrator sees the queue
	un := raw("bob@example.com", "w2", "")
	_, b := post(src.ID, domain.SignInbound(src.Secret, un), un)
	if !strings.Contains(b, "quarantined") {
		t.Fatal(b)
	}
	if c, _, _ := alice.do("GET", "/api/v1/admin/mail/inbound?outcome=quarantined", nil); c != 403 {
		t.Fatalf("member read the queue: %d", c)
	}
	if c, _, _ := admin.do("GET", "/api/v1/admin/mail/inbound?outcome=quarantined", nil); c != 200 {
		t.Fatalf("%d", c)
	}
	list, _ := e.Svc.ListInbound(testutil.Ctx(), e.Admin, "quarantined", 10)
	if len(list) != 1 || list[0].Issue != key {
		t.Fatalf("%+v", list)
	}
	rawURL := "/api/v1/admin/mail/inbound/" + itoa(list[0].ID)
	req, _ := http.NewRequest("GET", alice.base+rawURL+"/raw", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	r, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "message/rfc822" || r.Header.Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(body, un) {
		t.Fatalf("%d %v", r.StatusCode, r.Header)
	}
	if c, _, _ := admin.do("POST", rawURL+"/release", nil); c != 200 {
		t.Fatalf("release: %d", c)
	}
	if c, _, _ := admin.do("POST", rawURL+"/release", nil); c != 400 {
		t.Fatalf("second release: %d", c)
	}
	_, out, _ = alice.do("GET", "/api/v1/issues/"+key+"/view", nil)
	if cs, _ := out["comments"].([]any); len(cs) != 2 {
		t.Fatalf("%v", out["comments"])
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
