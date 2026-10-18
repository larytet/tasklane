package mail

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
)

// Report is what a machine-generated report says: a delivery status notification (RFC 3464) or an abuse feedback report (RFC 5965).
type Report struct {
	Kind      string // "dsn", "feedback" or "" when the message is neither
	Action    string // dsn: failed, delayed, delivered, relayed, expanded
	Status    string // dsn: the enhanced status code, such as 5.1.1 or 4.2.2
	Recipient string // the address the report is about, lowercase
	Original  string // Message-ID of the message the report is about, with angle brackets, when it was returned
	FeedType  string // feedback: abuse, fraud, virus, other
}

// ParseReport reads a multipart/report message. It is bounded: only the first few parts are examined and nothing is
// returned beyond short header values.
func ParseReport(raw []byte) *Report {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return &Report{}
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/report" || params["boundary"] == "" {
		return &Report{}
	}
	r := &Report{}
	switch strings.ToLower(params["report-type"]) {
	case "delivery-status":
		r.Kind = "dsn"
	case "feedback-report":
		r.Kind = "feedback"
	default:
		return &Report{}
	}
	mr := multipart.NewReader(io.LimitReader(m.Body, 4<<20), params["boundary"])
	for i := 0; i < 8; i++ {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		pt, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		body, _ := io.ReadAll(io.LimitReader(part, 256<<10))
		switch pt {
		case "message/delivery-status":
			// a per-message block, then one block per recipient
			tr := textproto.NewReader(bufioReader(body))
			for first := true; ; first = false {
				h, err := tr.ReadMIMEHeader()
				if len(h) == 0 && err != nil {
					break
				}
				if first {
					if r.Original == "" {
						r.Original = oneID(h.Get("Original-Envelope-Id"))
					}
					continue
				}
				if r.Status == "" {
					r.Status = strings.TrimSpace(h.Get("Status"))
					r.Action = strings.ToLower(strings.TrimSpace(h.Get("Action")))
					r.Recipient = addrAfterType(h.Get("Final-Recipient"))
					if r.Recipient == "" {
						r.Recipient = addrAfterType(h.Get("Original-Recipient"))
					}
				}
			}
		case "message/feedback-report":
			h, _ := textproto.NewReader(bufioReader(body)).ReadMIMEHeader()
			r.FeedType = strings.ToLower(strings.TrimSpace(h.Get("Feedback-Type")))
			r.Recipient = addrAfterType(h.Get("Original-Rcpt-To"))
			if r.Recipient == "" {
				r.Recipient = strings.ToLower(strings.Trim(strings.TrimSpace(h.Get("Original-Mail-From")), "<>"))
			}
		case "message/rfc822", "text/rfc822-headers":
			if mm, err := mail.ReadMessage(bytes.NewReader(body)); err == nil {
				if id := oneID(mm.Header.Get("Message-Id")); id != "" {
					r.Original = id
				}
			}
		}
	}
	return r
}

func oneID(v string) string {
	v = strings.TrimSpace(v)
	if i, j := strings.Index(v, "<"), strings.Index(v, ">"); i >= 0 && j > i && j-i < 260 {
		return v[i : j+1]
	}
	return ""
}

// addrAfterType takes "rfc822; user@example.com" to "user@example.com".
func addrAfterType(v string) string {
	if _, a, ok := strings.Cut(v, ";"); ok {
		v = a
	}
	return strings.ToLower(strings.Trim(strings.TrimSpace(v), "<>"))
}
