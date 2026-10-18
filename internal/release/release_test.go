package release_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/release"
)

var goBin = filepath.Join(runtime.GOROOT(), "bin", "go")

// tiny builds a small Go program the way the release script builds the real one: version in the linker flags, trimmed paths,
// and, with withDep, one module dependency so the build information has something to compare with the SBOM.
func tiny(t *testing.T, version, arch string, withDep bool) []byte {
	t.Helper()
	dir := t.TempDir()
	main := "package main\nvar versionMarker = \"tasklane-release-version=<none>;\"\nfunc main() { println(versionMarker) }\n"
	mod := "module example.com/tiny\n\ngo 1.21\n"
	if withDep {
		main = "package main\nimport _ \"golang.org/x/text/encoding/htmlindex\"\nvar versionMarker = \"tasklane-release-version=<none>;\"\nfunc main() { println(versionMarker) }\n"
		mod += "\nrequire golang.org/x/text v0.43.0\n"
		sums, _ := os.ReadFile("../../go.sum")
		var keep []string
		for _, l := range strings.Split(string(sums), "\n") {
			if strings.HasPrefix(l, "golang.org/x/text v0.43.0") {
				keep = append(keep, l)
			}
		}
		os.WriteFile(filepath.Join(dir, "go.sum"), []byte(strings.Join(keep, "\n")+"\n"), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(main), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644)
	out := filepath.Join(dir, "tasklane")
	cmd := exec.Command(goBin, "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w -buildid= -X main.versionMarker=tasklane-release-version="+version+";", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch, "GOFLAGS=-mod=mod", "GOPROXY=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the test binary: %v\n%s", err, b)
	}
	b, _ := os.ReadFile(out)
	return b
}

type arc struct {
	version, arch string
	bin           []byte
	notice, sbom  string
	omit          string // a file to leave out
	extra         func(*tar.Writer, string)
	top           string
}

const depSBOM = "golang.org/x/text v0.43.0 BSD-3-Clause\n"
const depNotice = "golang.org/x/text v0.43.0\nLicence: BSD-3-Clause\n"

func (a arc) write(t *testing.T, dir string) string {
	name := fmt.Sprintf("tasklane-%s-linux-%s.tar.gz", a.version, a.arch)
	top := a.top
	if top == "" {
		top = fmt.Sprintf("tasklane-%s-linux-%s", a.version, a.arch)
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, 9)
	tw := tar.NewWriter(zw)
	put := func(n string, b []byte, mode int64) {
		if n == a.omit {
			return
		}
		tw.WriteHeader(&tar.Header{Name: top + "/" + n, Size: int64(len(b)), Mode: mode, Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	put("tasklane", a.bin, 0o755)
	put("NOTICE", []byte(a.notice), 0o644)
	put("SBOM.txt", []byte(a.sbom), 0o644)
	put("README.md", []byte("readme"), 0o644)
	if a.extra != nil {
		a.extra(tw, top)
	}
	tw.Close()
	zw.Close()
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

func sums(t *testing.T, dir string, names ...string) {
	var b strings.Builder
	for _, n := range names {
		d, _ := os.ReadFile(filepath.Join(dir, n))
		h := sha256.Sum256(d)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(h[:]), n)
	}
	os.WriteFile(filepath.Join(dir, release.SumsFile), []byte(b.String()), 0o644)
}

type rel struct {
	dir      string
	pub, key []byte
}

func newRel(t *testing.T, arcs ...arc) *rel {
	pub, key, err := release.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	r := &rel{dir: t.TempDir(), pub: pub, key: key}
	var names []string
	for _, a := range arcs {
		names = append(names, a.write(t, r.dir))
	}
	sums(t, r.dir, names...)
	if err := release.Sign(r.dir, key); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rel) verify(t *testing.T) *release.Report {
	rep, err := release.Verify(r.dir, r.pub)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func wantProblem(t *testing.T, name string, rep *release.Report, sub string) {
	t.Helper()
	if rep.OK() {
		t.Errorf("%s: the release verified", name)
		return
	}
	for _, p := range rep.Problems {
		if strings.Contains(p, sub) {
			return
		}
	}
	t.Errorf("%s: no problem mentions %q: %q", name, sub, rep.Problems)
}

func TestINST05_AGoodReleaseVerifiesAndSaysWhatItChecked(t *testing.T) {
	r := newRel(t,
		arc{version: "1.2.3", arch: "amd64", bin: tiny(t, "1.2.3", "amd64", true), notice: depNotice, sbom: depSBOM},
		arc{version: "1.2.3", arch: "arm64", bin: tiny(t, "1.2.3", "arm64", true), notice: depNotice, sbom: depSBOM})
	rep := r.verify(t)
	if !rep.OK() || len(rep.Archives) != 2 || rep.Archives[0].Modules != 1 || rep.Archives[0].Version != "1.2.3" || rep.Archives[1].Arch != "arm64" {
		t.Fatalf("%+v", rep)
	}
	// the key id is the one in the signature, and a key's id is stable
	pub2, _, _ := release.GenerateKey()
	rep2, _ := release.Verify(r.dir, pub2)
	wantProblem(t, "another key", rep2, "signed by a different key")
}

func TestINST05_AnAlteredUnsignedOrMisattributedReleaseIsRefused(t *testing.T) {
	good := func() *rel {
		return newRel(t, arc{version: "1.0.0", arch: "amd64", bin: tiny(t, "1.0.0", "amd64", false), notice: "x", sbom: ""})
	}
	arch := "tasklane-1.0.0-linux-amd64.tar.gz"
	// an archive changed after signing
	r := good()
	b, _ := os.ReadFile(filepath.Join(r.dir, arch))
	b[len(b)/2] ^= 0xff
	os.WriteFile(filepath.Join(r.dir, arch), b, 0o644)
	wantProblem(t, "flipped byte", r.verify(t), "the signed list says")
	// the checksum list replaced with one that matches a replaced archive, without the key
	r = good()
	evil := tiny(t, "1.0.0", "amd64", false)
	arc{version: "1.0.0", arch: "amd64", bin: evil, notice: "evil", sbom: ""}.write(t, r.dir)
	sums(t, r.dir, arch)
	wantProblem(t, "replaced archive and list", r.verify(t), "signature does not match")
	// no signature at all, a junk one, a truncated one
	r = good()
	os.Remove(filepath.Join(r.dir, release.SigFile))
	wantProblem(t, "unsigned", r.verify(t), "not signed")
	r = good()
	os.WriteFile(filepath.Join(r.dir, release.SigFile), []byte("hello"), 0o644)
	wantProblem(t, "junk signature", r.verify(t), "not a Tasklane release signature")
	r = good()
	sig, _ := os.ReadFile(filepath.Join(r.dir, release.SigFile))
	os.WriteFile(filepath.Join(r.dir, release.SigFile), sig[:len(sig)-30], 0o644)
	wantProblem(t, "truncated signature", r.verify(t), "malformed")
	// a signature moved to another checksum file (the label and the file are both signed)
	r = good()
	other := good()
	os.WriteFile(filepath.Join(r.dir, release.SigFile), mustRead(t, filepath.Join(other.dir, release.SigFile)), 0o644)
	wantProblem(t, "signature of another release", r.verify(t), "different key")
	// an archive that is here but not in the signed list; a listed one that is gone
	r = good()
	os.WriteFile(filepath.Join(r.dir, "tasklane-9.9.9-linux-amd64.tar.gz"), []byte("planted"), 0o644)
	wantProblem(t, "planted archive", r.verify(t), "not part of the release")
	r = good()
	os.Remove(filepath.Join(r.dir, arch))
	wantProblem(t, "missing archive", r.verify(t), "cannot be read")
	// paths, duplicates and an empty list in the signed file itself
	for name, body := range map[string]string{
		"path":      "0000  ../etc/passwd\n",
		"duplicate": "00  a.tar.gz\n00  a.tar.gz\n",
		"malformed": "just one field\n",
		"empty":     "\n",
	} {
		r := good()
		os.WriteFile(filepath.Join(r.dir, release.SumsFile), []byte(body), 0o644)
		release.Sign(r.dir, r.key)
		if rep := r.verify(t); rep.OK() {
			t.Errorf("a signed list with %s verified", name)
		}
	}
	// nothing to verify against
	if _, err := release.Verify(t.TempDir(), good().pub); err == nil {
		t.Error("an empty directory verified")
	}
	if _, err := release.Verify(good().dir, []byte("not a key")); err == nil {
		t.Error("a bad key was accepted")
	}
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The signature proves who published a release, not that what they published is what it says. These releases are correctly
// signed and still wrong, and verification refuses each.
func TestINST05_ACorrectlySignedReleaseThatDoesNotMatchItsOwnContentsIsRefused(t *testing.T) {
	bin := tiny(t, "1.0.0", "amd64", true)
	good := arc{version: "1.0.0", arch: "amd64", bin: bin, notice: depNotice, sbom: depSBOM}
	if rep := newRel(t, good).verify(t); !rep.OK() {
		t.Fatalf("the baseline does not verify: %v", rep.Problems)
	}
	cases := []struct {
		name, want string
		mut        func(a *arc)
	}{
		{"SBOM omits a module", "which the SBOM does not list", func(a *arc) { a.sbom = "" }},
		{"SBOM has a module that is not linked", "which is not in the binary", func(a *arc) { a.sbom = depSBOM + "github.com/evil/extra v1.0.0 MIT\n" }},
		{"SBOM has another version", "the SBOM says v0.42.0", func(a *arc) { a.sbom = "golang.org/x/text v0.42.0 BSD-3-Clause\n" }},
		{"NOTICE omits a module", "the NOTICE has no entry", func(a *arc) { a.notice = "nothing\n" }},
		{"binary of another architecture", "linux/amd64, the archive name says linux/arm64", func(a *arc) { a.arch = "arm64" }},
		{"binary of another version", "the binary says version 1.0.0, the archive name says 2.0.0", func(a *arc) { a.version = "2.0.0" }},
		{"top directory renamed", "the top directory is", func(a *arc) { a.top = "tasklane-latest" }},
		{"binary missing", "tasklane is missing", func(a *arc) { a.omit = "tasklane" }},
		{"NOTICE missing", "NOTICE is missing", func(a *arc) { a.omit = "NOTICE" }},
		{"SBOM missing", "SBOM.txt is missing", func(a *arc) { a.omit = "SBOM.txt" }},
		{"not a Go binary", "no build information", func(a *arc) { a.bin = []byte("#!/bin/sh\necho hi\n") }},
		{"a link inside", "contains a link", func(a *arc) {
			a.extra = func(tw *tar.Writer, top string) {
				tw.WriteHeader(&tar.Header{Name: top + "/evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
			}
		}},
		{"an entry that leaves the directory", "leaves the archive", func(a *arc) {
			a.extra = func(tw *tar.Writer, top string) {
				tw.WriteHeader(&tar.Header{Name: top + "/../../etc/cron.d/x", Size: 1, Typeflag: tar.TypeReg})
				tw.Write([]byte("x"))
			}
		}},
		{"two top directories", "more than one top directory", func(a *arc) {
			a.extra = func(tw *tar.Writer, top string) {
				tw.WriteHeader(&tar.Header{Name: "other/file", Size: 1, Typeflag: tar.TypeReg})
				tw.Write([]byte("x"))
			}
		}},
	}
	for _, c := range cases {
		a := good
		c.mut(&a)
		// the arch/version mutations must still be signed under a name the checker will read
		wantProblem(t, c.name, newRel(t, a).verify(t), c.want)
	}
	// a file that is not gzip
	r := newRel(t, good)
	os.WriteFile(filepath.Join(r.dir, "tasklane-1.0.0-linux-amd64.tar.gz"), []byte("not gzip"), 0o644)
	sums(t, r.dir, "tasklane-1.0.0-linux-amd64.tar.gz")
	release.Sign(r.dir, r.key)
	wantProblem(t, "not gzip", r.verify(t), "not a gzip file")
}

func TestINST05_KeysRoundTripAndTheSignatureIsStandardEd25519(t *testing.T) {
	pub, key, err := release.GenerateKey()
	if err != nil || !bytes.HasPrefix(pub, []byte("-----BEGIN PUBLIC KEY-----")) || !bytes.HasPrefix(key, []byte("-----BEGIN PRIVATE KEY-----")) {
		t.Fatalf("%v", err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, release.SumsFile), []byte("abc  file\n"), 0o644)
	if err := release.Sign(dir, pub); err == nil { // a public key is not a signing key
		t.Fatal("signed with a public key")
	}
	if err := release.Sign(dir, key); err != nil {
		t.Fatal(err)
	}
	// verified by OpenSSL, an implementation that shares no code with this one
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	sigFile := mustRead(t, filepath.Join(dir, release.SigFile))
	var enc string
	for _, l := range strings.Split(string(sigFile), "\n") {
		if strings.HasPrefix(l, "signature: ") {
			enc = strings.TrimPrefix(l, "signature: ")
		}
	}
	raw := mustDecode(t, enc)
	os.WriteFile(filepath.Join(dir, "msg"), release.Message(mustRead(t, filepath.Join(dir, release.SumsFile))), 0o644)
	os.WriteFile(filepath.Join(dir, "sig.bin"), raw, 0o644)
	os.WriteFile(filepath.Join(dir, "pub.pem"), pub, 0o644)
	out, err := exec.Command("openssl", "pkeyutl", "-verify", "-pubin", "-inkey", filepath.Join(dir, "pub.pem"), "-rawin", "-in", filepath.Join(dir, "msg"), "-sigfile", filepath.Join(dir, "sig.bin")).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Signature Verified Successfully") {
		t.Fatalf("OpenSSL does not verify the signature: %v\n%s", err, out)
	}
	// and it does not verify a different message
	os.WriteFile(filepath.Join(dir, "msg2"), []byte("tasklane-release-v1\nabd  file\n"), 0o644)
	if out, err := exec.Command("openssl", "pkeyutl", "-verify", "-pubin", "-inkey", filepath.Join(dir, "pub.pem"), "-rawin", "-in", filepath.Join(dir, "msg2"), "-sigfile", filepath.Join(dir, "sig.bin")).CombinedOutput(); err == nil {
		t.Fatalf("OpenSSL verified a different message: %s", out)
	}
}

func mustDecode(t *testing.T, s string) []byte {
	b, err := base64Decode(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func base64Decode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
