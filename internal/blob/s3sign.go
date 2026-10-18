package blob

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4, written against the public specification so the S3 backend needs no SDK. Header signing is
// used for the server's own requests; query signing (presign) for URLs handed to browsers.

const (
	sigAlgo         = "AWS4-HMAC-SHA256"
	unsignedPayload = "UNSIGNED-PAYLOAD"
)

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// uriEncode percent-encodes as SigV4 requires: everything except unreserved characters, and "/" kept in paths.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

// canonicalRequest builds the string that is hashed and signed. headers holds the headers to sign (lower-case names).
func canonicalRequest(method, path string, q url.Values, headers map[string]string, signed []string, payloadHash string) string {
	var ch strings.Builder
	for _, n := range signed {
		ch.WriteString(n + ":" + strings.Join(strings.Fields(headers[n]), " ") + "\n")
	}
	return strings.Join([]string{method, path, canonicalQuery(q), ch.String(), strings.Join(signed, ";"), payloadHash}, "\n")
}

func stringToSign(amzDate, scope, canonical string) string {
	h := sha256.Sum256([]byte(canonical))
	return strings.Join([]string{sigAlgo, amzDate, scope, hex.EncodeToString(h[:])}, "\n")
}

type signer struct {
	AccessKey, SecretKey, Region, Service string
}

func (s signer) sign(secretDate, canonical, amzDate string) (scope, sig string) {
	scope = strings.Join([]string{secretDate, s.Region, s.Service, "aws4_request"}, "/")
	sig = hex.EncodeToString(hmacSHA256(signingKey(s.SecretKey, secretDate, s.Region, s.Service), stringToSign(amzDate, scope, canonical)))
	return scope, sig
}

// signRequest adds the Authorization header (and x-amz-date, x-amz-content-sha256) to req. The path must already be the
// encoded request path.
func (s signer) signRequest(req *http.Request, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate, date := now.Format("20060102T150405Z"), now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	hdr := map[string]string{"host": req.URL.Host}
	signed := []string{"host"}
	for k := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || lk == "content-md5" || lk == "content-type" || lk == "range" {
			hdr[lk] = req.Header.Get(k)
			signed = append(signed, lk)
		}
	}
	sort.Strings(signed)
	canon := canonicalRequest(req.Method, req.URL.EscapedPath(), req.URL.Query(), hdr, signed, payloadHash)
	scope, sig := s.sign(date, canon, amzDate)
	req.Header.Set("Authorization", sigAlgo+" Credential="+s.AccessKey+"/"+scope+", SignedHeaders="+strings.Join(signed, ";")+", Signature="+sig)
}

// presign returns a URL valid for `expires` that anyone holding it can use for exactly this method and path.
func (s signer) presign(method string, u *url.URL, expires time.Duration, now time.Time) string {
	now = now.UTC()
	amzDate, date := now.Format("20060102T150405Z"), now.Format("20060102")
	scope := strings.Join([]string{date, s.Region, s.Service, "aws4_request"}, "/")
	q := u.Query()
	q.Set("X-Amz-Algorithm", sigAlgo)
	q.Set("X-Amz-Credential", s.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", itoa(int64(expires/time.Second)))
	q.Set("X-Amz-SignedHeaders", "host")
	canon := canonicalRequest(method, u.EscapedPath(), q, map[string]string{"host": u.Host}, []string{"host"}, unsignedPayload)
	_, sig := s.sign(date, canon, amzDate)
	q.Set("X-Amz-Signature", sig)
	out := *u
	out.RawQuery = canonicalQuery(q)
	return out.String()
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// VerifySigV4 checks the signature of an incoming request, header-signed or presigned, the way a service does. It
// exists for the in-process S3 test double: a request our client signs wrongly must fail there, as it would on AWS.
func VerifySigV4(r *http.Request, accessKey, secretKey, region string, now time.Time) error {
	q := r.URL.Query()
	var credential, signedHeaders, sig, amzDate, payloadHash string
	presigned := q.Get("X-Amz-Signature") != ""
	if presigned {
		credential, signedHeaders, sig, amzDate = q.Get("X-Amz-Credential"), q.Get("X-Amz-SignedHeaders"), q.Get("X-Amz-Signature"), q.Get("X-Amz-Date")
		payloadHash = unsignedPayload
		exp, err := time.ParseDuration(q.Get("X-Amz-Expires") + "s")
		if err != nil || exp <= 0 || exp > 7*24*time.Hour {
			return errors.New("bad expiry")
		}
		t, err := time.Parse("20060102T150405Z", amzDate)
		if err != nil {
			return errors.New("bad date")
		}
		if now.After(t.Add(exp)) {
			return errors.New("expired")
		}
		q.Del("X-Amz-Signature")
	} else {
		a := r.Header.Get("Authorization")
		if !strings.HasPrefix(a, sigAlgo+" ") {
			return errors.New("missing authorization")
		}
		for _, part := range strings.Split(strings.TrimPrefix(a, sigAlgo+" "), ",") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch kv[0] {
			case "Credential":
				credential = kv[1]
			case "SignedHeaders":
				signedHeaders = kv[1]
			case "Signature":
				sig = kv[1]
			}
		}
		amzDate, payloadHash = r.Header.Get("x-amz-date"), r.Header.Get("x-amz-content-sha256")
		t, err := time.Parse("20060102T150405Z", amzDate)
		if err != nil || now.Sub(t) > 15*time.Minute || t.Sub(now) > 15*time.Minute {
			return errors.New("request time too skewed")
		}
	}
	cred := strings.Split(credential, "/")
	if len(cred) != 5 || cred[0] != accessKey || cred[2] != region || cred[3] != "s3" || cred[4] != "aws4_request" {
		return errors.New("bad credential scope")
	}
	signed := strings.Split(signedHeaders, ";")
	hdr := map[string]string{}
	for _, n := range signed {
		if n == "host" {
			hdr[n] = r.Host
		} else {
			hdr[n] = r.Header.Get(n)
		}
	}
	canon := canonicalRequest(r.Method, r.URL.EscapedPath(), q, hdr, signed, payloadHash)
	_, want := signer{AccessKey: accessKey, SecretKey: secretKey, Region: region, Service: "s3"}.sign(cred[1], canon, amzDate)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return errors.New("signature does not match")
	}
	return nil
}
