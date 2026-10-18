package mail_test

import (
	"testing"

	"github.com/larytet/tasklane/internal/mail"
)

func TestMAIL06_ParseReport(t *testing.T) {
	dsn := "From: MAILER-DAEMON@x\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nfailed\r\n" +
		"--B\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; mx\r\n\r\nFinal-Recipient: rfc822; Bob@Example.com\r\nAction: failed\r\nStatus: 5.1.1\r\n\r\nFinal-Recipient: rfc822; second@example.com\r\nAction: delayed\r\nStatus: 4.0.0\r\n\r\n" +
		"--B\r\nContent-Type: message/rfc822\r\n\r\nMessage-ID: <orig@example.com>\r\nSubject: x\r\n\r\nbody\r\n--B--\r\n"
	r := mail.ParseReport([]byte(dsn))
	if r.Kind != "dsn" || r.Action != "failed" || r.Status != "5.1.1" || r.Recipient != "bob@example.com" || r.Original != "<orig@example.com>" {
		t.Fatalf("%+v", r)
	}
	arf := "From: a@isp\r\nContent-Type: multipart/report; report-type=feedback-report; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nx\r\n--B\r\nContent-Type: message/feedback-report\r\n\r\nFeedback-Type: Abuse\r\nOriginal-Rcpt-To: <Bob@example.com>\r\n\r\n--B--\r\n"
	if r := mail.ParseReport([]byte(arf)); r.Kind != "feedback" || r.FeedType != "abuse" || r.Recipient != "bob@example.com" {
		t.Fatalf("%+v", r)
	}
	for _, not := range []string{"", "From: a@b\r\n\r\nplain", "From: a@b\r\nContent-Type: multipart/report; boundary=B\r\n\r\n--B--", "From: a@b\r\nContent-Type: multipart/report; report-type=other; boundary=B\r\n\r\n--B--", "garbage\x00"} {
		if r := mail.ParseReport([]byte(not)); r.Kind != "" {
			t.Errorf("%q: %+v", not, r)
		}
	}
}
