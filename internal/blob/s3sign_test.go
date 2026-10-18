package blob

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The worked example from the AWS documentation ("Examples of how to derive a signing key" and the IAM ListUsers request):
// the signature below is the published one, so a misreading of the specification fails here and not against a real bucket.
func TestSigV4_AWSPublishedExample(t *testing.T) {
	s := signer{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", Region: "us-east-1", Service: "iam"}
	q := url.Values{"Action": {"ListUsers"}, "Version": {"2010-05-08"}}
	hdr := map[string]string{"content-type": "application/x-www-form-urlencoded; charset=utf-8", "host": "iam.amazonaws.com", "x-amz-date": "20150830T123600Z"}
	canon := canonicalRequest("GET", "/", q, hdr, []string{"content-type", "host", "x-amz-date"}, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	_, sig := s.sign("20150830", canon, "20150830T123600Z")
	if sig != "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7" {
		t.Fatalf("signature %s", sig)
	}
}

func TestSigV4_PresignedS3Example(t *testing.T) {
	// the S3 documentation's presigned GET example (examplebucket, test.txt, 86400 seconds)
	s := signer{AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", Region: "us-east-1", Service: "s3"}
	u, _ := url.Parse("https://examplebucket.s3.amazonaws.com/test.txt")
	got := s.presign("GET", u, 86400*time.Second, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(got, "X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404") {
		t.Fatalf("%s", got)
	}
}

func TestSigV4_PathEncoding(t *testing.T) {
	if uriEncode("a b/c+d~e", true) != "a%20b/c%2Bd~e" || uriEncode("a/b", false) != "a%2Fb" {
		t.Fatal("encoding")
	}
	req, _ := http.NewRequest("PUT", "http://127.0.0.1:9000/bkt/ab/cd/x", nil)
	signer{AccessKey: "k", SecretKey: "s", Region: "r", Service: "s3"}.signRequest(req, unsignedPayload, time.Now())
	if !strings.Contains(req.Header.Get("Authorization"), "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Fatal(req.Header.Get("Authorization"))
	}
}
