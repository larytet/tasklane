package mail

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DKIM verification (RFC 6376, with ed25519 from RFC 8463), done by Tasklane itself on the stored raw message so that proof of
// origin does not depend on the transport. A signature counts only if all of these hold: it is well formed, it signs the From
// header, it covers the whole body (no `l=` tag), it has not expired, its key is published in DNS and is not revoked or
// weak (RSA of at least 1024 bits), and the body hash and the signature both verify. Whether the signing domain is aligned
// with the sender's domain is the caller's decision (DMARC-style alignment, see Aligned).

// TXTResolver looks up DNS TXT records; tests supply a map, production uses the system resolver.
type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// DKIMResult is the outcome for one DKIM-Signature header.
type DKIMResult struct {
	Domain   string // d=
	Selector string // s=
	Pass     bool
	Reason   string // why it did not pass
}

const (
	maxSignatures = 5
	maxDNSBytes   = 8192
)

type header struct{ name, raw string } // raw keeps the original folding and the line ending

func splitMessage(raw []byte) (hdrs []header, body []byte) {
	norm := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n", "\r\n") // signers sign the CRLF form
	head, rest := norm, ""
	if i := strings.Index(norm, "\r\n\r\n"); i >= 0 {
		head, rest = norm[:i+2], norm[i+4:]
	}
	var cur *header
	for _, line := range strings.SplitAfter(head, "\r\n") {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && cur != nil {
			cur.raw += line
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			cur = nil
			continue
		}
		hdrs = append(hdrs, header{name: strings.TrimSpace(name), raw: line})
		cur = &hdrs[len(hdrs)-1]
	}
	return hdrs, []byte(rest)
}

func collapseWSP(s string) string {
	var b strings.Builder
	sp := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			sp = true
			continue
		}
		if sp {
			b.WriteByte(' ')
			sp = false
		}
		b.WriteRune(r)
	}
	if sp {
		b.WriteByte(' ')
	}
	return b.String()
}

func relaxedHeader(h header) string {
	_, v, _ := strings.Cut(h.raw, ":")
	v = strings.ReplaceAll(strings.ReplaceAll(v, "\r\n", ""), "\n", "")
	v = strings.TrimSpace(collapseWSP(v))
	return strings.ToLower(h.name) + ":" + v + "\r\n"
}

func canonBody(body []byte, relaxed bool) []byte {
	lines := strings.Split(string(body), "\r\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if relaxed {
		for i, l := range lines {
			lines[i] = strings.TrimRight(collapseWSP(l), " ")
		}
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		if relaxed {
			return nil
		}
		return []byte("\r\n")
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

func parseTags(v string) (map[string]string, error) {
	m := map[string]string{}
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, val, ok := strings.Cut(part, "=")
		if !ok {
			return nil, errors.New("a tag without a value")
		}
		k = strings.TrimSpace(k)
		if _, dup := m[k]; dup {
			return nil, errors.New("a tag appears twice")
		}
		m[k] = strings.Join(strings.Fields(val), "")
		if k == "h" || k == "z" { // these may contain spaces inside names
			m[k] = strings.TrimSpace(val)
		}
	}
	return m, nil
}

// VerifyDKIM checks every DKIM-Signature of the message (at most five) and reports each.
func VerifyDKIM(ctx context.Context, raw []byte, res TXTResolver, now time.Time) []DKIMResult {
	hdrs, body := splitMessage(raw)
	var out []DKIMResult
	for i, h := range hdrs {
		if !strings.EqualFold(h.name, "DKIM-Signature") {
			continue
		}
		if len(out) >= maxSignatures {
			break
		}
		out = append(out, verifyOne(ctx, hdrs, body, i, res, now))
	}
	return out
}

func verifyOne(ctx context.Context, hdrs []header, body []byte, idx int, res TXTResolver, now time.Time) DKIMResult {
	_, val, _ := strings.Cut(hdrs[idx].raw, ":")
	tags, err := parseTags(val)
	r := DKIMResult{Domain: strings.ToLower(tags["d"]), Selector: tags["s"]}
	fail := func(f string, a ...any) DKIMResult { r.Reason = fmt.Sprintf(f, a...); return r }
	if err != nil {
		return fail("malformed signature: %v", err)
	}
	for _, t := range []string{"v", "a", "b", "bh", "d", "h", "s"} {
		if tags[t] == "" {
			return fail("the signature has no %s= tag", t)
		}
	}
	if tags["v"] != "1" {
		return fail("unsupported version")
	}
	if _, has := tags["l"]; has {
		return fail("a signature that covers only part of the body (l=) is not accepted")
	}
	if q := tags["q"]; q != "" && !strings.HasPrefix(q, "dns/txt") {
		return fail("unsupported key query")
	}
	alg := strings.ToLower(tags["a"])
	if alg != "rsa-sha256" && alg != "ed25519-sha256" {
		return fail("unsupported algorithm %s", alg)
	}
	if x := tags["x"]; x != "" {
		if sec, err := strconv.ParseInt(x, 10, 64); err != nil || now.Unix() > sec {
			return fail("the signature has expired")
		}
	}
	signed := strings.Fields(strings.ReplaceAll(strings.ToLower(tags["h"]), ":", " "))
	hasFrom := false
	for _, n := range signed {
		hasFrom = hasFrom || n == "from"
	}
	if !hasFrom {
		return fail("the signature does not cover the From header")
	}
	headerC, bodyC := "simple", "simple"
	if c := strings.ToLower(tags["c"]); c != "" {
		a, b, _ := strings.Cut(c, "/")
		headerC = a
		if b != "" {
			bodyC = b
		}
	}
	if (headerC != "simple" && headerC != "relaxed") || (bodyC != "simple" && bodyC != "relaxed") {
		return fail("unsupported canonicalization")
	}
	// body hash
	sum := sha256.Sum256(canonBody(body, bodyC == "relaxed"))
	want, err := base64.StdEncoding.DecodeString(tags["bh"])
	if err != nil || !bytes.Equal(sum[:], want) {
		return fail("the body hash does not match: the body was changed")
	}
	// the key
	key, kerr := lookupKey(ctx, res, tags["s"], r.Domain)
	if kerr != nil {
		return fail("%v", kerr)
	}
	// header hash: the named headers from the bottom up, then this signature with an empty b=
	used := map[int]bool{idx: true}
	var data bytes.Buffer
	for _, n := range signed {
		for j := len(hdrs) - 1; j >= 0; j-- {
			if used[j] || !strings.EqualFold(hdrs[j].name, n) {
				continue
			}
			used[j] = true
			if headerC == "relaxed" {
				data.WriteString(relaxedHeader(hdrs[j]))
			} else {
				data.WriteString(hdrs[j].raw)
			}
			break
		}
	}
	sigHdr := stripB(hdrs[idx])
	if headerC == "relaxed" {
		data.WriteString(strings.TrimSuffix(relaxedHeader(sigHdr), "\r\n"))
	} else {
		data.WriteString(strings.TrimSuffix(sigHdr.raw, "\r\n"))
	}
	sig, err := base64.StdEncoding.DecodeString(tags["b"])
	if err != nil {
		return fail("the signature value is not base64")
	}
	digest := sha256.Sum256(data.Bytes())
	switch k := key.(type) {
	case *rsa.PublicKey:
		if alg != "rsa-sha256" {
			return fail("the key is RSA but the signature says %s", alg)
		}
		if k.N.BitLen() < 1024 {
			return fail("the signing key is too weak (%d bits)", k.N.BitLen())
		}
		if rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) != nil {
			return fail("the signature does not verify")
		}
	case ed25519.PublicKey:
		if alg != "ed25519-sha256" {
			return fail("the key is ed25519 but the signature says %s", alg)
		}
		if !ed25519.Verify(k, digest[:], sig) {
			return fail("the signature does not verify")
		}
	}
	r.Pass = true
	return r
}

// stripB returns the signature header with the value of b= removed, keeping everything else byte for byte.
func stripB(h header) header {
	raw := h.raw
	for i := 0; i < len(raw); {
		j := strings.Index(raw[i:], "b=")
		if j < 0 {
			break
		}
		p := i + j
		k := p - 1
		for k >= 0 && (raw[k] == ' ' || raw[k] == '\t' || raw[k] == '\r' || raw[k] == '\n') {
			k--
		}
		if k < 0 || raw[k] == ';' || raw[k] == ':' { // a tag name, not the end of bh= or another value
			end := len(strings.TrimSuffix(raw, "\r\n"))
			if e := strings.IndexByte(raw[p+2:], ';'); e >= 0 {
				end = p + 2 + e
			}
			return header{name: h.name, raw: raw[:p+2] + raw[end:]}
		}
		i = p + 2
	}
	return h
}

func lookupKey(ctx context.Context, res TXTResolver, selector, domain string) (any, error) {
	if res == nil {
		return nil, errors.New("no resolver")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	txts, err := res.LookupTXT(ctx, selector+"._domainkey."+domain)
	if err != nil || len(txts) == 0 {
		return nil, errors.New("the signing key could not be found in DNS")
	}
	rec := strings.Join(txts, "")
	if len(rec) > maxDNSBytes {
		return nil, errors.New("the key record is too large")
	}
	t, err := parseTags(rec)
	if err != nil {
		return nil, errors.New("the key record is malformed")
	}
	if v := t["v"]; v != "" && v != "DKIM1" {
		return nil, errors.New("the key record has an unknown version")
	}
	if t["p"] == "" {
		return nil, errors.New("the signing key has been revoked")
	}
	if strings.Contains(t["t"], "y") { // testing mode: the domain asks that signatures not be relied on
		return nil, errors.New("the domain publishes this key in testing mode")
	}
	der, err := base64.StdEncoding.DecodeString(t["p"])
	if err != nil {
		return nil, errors.New("the key is not base64")
	}
	if strings.EqualFold(t["k"], "ed25519") {
		if len(der) != ed25519.PublicKeySize {
			return nil, errors.New("the ed25519 key has the wrong size")
		}
		return ed25519.PublicKey(der), nil
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, errors.New("the key cannot be parsed")
	}
	rk, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("the key is not RSA")
	}
	return rk, nil
}

// Aligned reports whether a signing domain is the sender's domain or a parent of it on a label boundary
// (example.com for mail.example.com, never example.com.evil.net or notexample.com).
func Aligned(signing, from string) bool {
	signing, from = strings.ToLower(strings.TrimSuffix(signing, ".")), strings.ToLower(strings.TrimSuffix(from, "."))
	return signing != "" && (signing == from || strings.HasSuffix(from, "."+signing))
}
