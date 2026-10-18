package dist_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func build(t *testing.T, dir string) map[string]string {
	t.Helper()
	cmd := exec.Command("./scripts/dist.sh", dir)
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), "GO="+filepath.Join(runtime.GOROOT(), "bin", "go"), "VERSION=0.0.0-test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dist.sh: %v\n%s", err, out)
	}
	sums := map[string]string{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			h := sha256.Sum256(b)
			sums[e.Name()] = hex.EncodeToString(h[:])
		}
	}
	return sums
}

// INST-05 (reproducibility) and INST-03 (amd64 and arm64 both build): two builds give identical archives.
func TestINST05_ReproducibleTarballs(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two release archives for two architectures")
	}
	a, b := build(t, t.TempDir()), build(t, t.TempDir())
	if len(a) != 2 {
		t.Fatalf("expected amd64 and arm64 archives, got %v", a)
	}
	for name, h := range a {
		if b[name] != h {
			t.Errorf("%s is not reproducible: %s vs %s", name, h, b[name])
		}
	}
}

// The archive carries what an operator needs, and the checksum file matches.
func TestINST01_TarballContents(t *testing.T) {
	if testing.Short() {
		t.Skip("builds release archives")
	}
	dir := t.TempDir()
	sums := build(t, dir)
	var arch string
	for n := range sums {
		if strings.Contains(n, "amd64") {
			arch = n
		}
	}
	f, err := os.Open(filepath.Join(dir, arch))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, _ := gzip.NewReader(f)
	tr := tar.NewReader(zr)
	got := map[string]*tar.Header{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[strings.TrimPrefix(h.Name, "tasklane-0.0.0-test-linux-amd64/")] = h
		if h.ModTime.Unix() != 0 || h.Uid != 0 || h.Gid != 0 {
			t.Errorf("%s: mtime %v uid %d gid %d are not normalized", h.Name, h.ModTime, h.Uid, h.Gid)
		}
	}
	for _, want := range []string{"tasklane", "NOTICE", "SBOM.txt", "README.md", "CONFIGURATION.md", "deploy/tasklane.service", "deploy/tasklane.conf.example"} {
		if got[want] == nil {
			t.Errorf("archive lacks %s", want)
		}
	}
	if h := got["tasklane"]; h == nil || h.Mode&0o111 == 0 {
		t.Errorf("binary is not executable: %+v", h)
	}
	sf, _ := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	for name, h := range sums {
		if !strings.Contains(string(sf), h+"  "+name) {
			t.Errorf("SHA256SUMS lacks %s", name)
		}
	}
	unit, _ := os.ReadFile("../../deploy/systemd/tasklane.service")
	for _, must := range []string{"ExecStartPre=/usr/local/bin/tasklane doctor", "NoNewPrivileges=true", "ProtectSystem=strict", "User=tasklane", "LimitNOFILE"} {
		if !strings.Contains(string(unit), must) {
			t.Errorf("systemd unit lacks %s", must)
		}
	}
}

// INST-05: the real release, signed with a freshly made key, verifies with the real binary from the archive; each way of
// tampering with a copy of it is refused; and signing does not disturb reproducibility.
func TestINST05_TheRealSignedReleaseVerifiesAndTamperedCopiesDoNot(t *testing.T) {
	if testing.Short() {
		t.Skip("builds release archives")
	}
	keyDir := t.TempDir()
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	relsign := func(args ...string) ([]byte, error) {
		c := exec.Command(goBin, append([]string{"run", "./cmd/relsign"}, args...)...)
		c.Dir = "../.."
		return c.CombinedOutput()
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("runs the amd64 binary from the archive")
	}
	if out, err := relsign("keygen", keyDir); err != nil {
		t.Fatalf("relsign: %v\n%s", err, out)
	}
	signedBuild := func(dir string) {
		cmd := exec.Command("./scripts/dist.sh", dir)
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(), "GO="+goBin, "VERSION=0.0.0-test", "RELEASE_KEY="+filepath.Join(keyDir, "release.key"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("dist.sh: %v\n%s", err, out)
		}
	}
	a, b := t.TempDir(), t.TempDir()
	signedBuild(a)
	signedBuild(b)
	sa, _ := os.ReadFile(filepath.Join(a, "SHA256SUMS.sig"))
	sb, _ := os.ReadFile(filepath.Join(b, "SHA256SUMS.sig"))
	if len(sa) == 0 || !bytes.Equal(sa, sb) {
		t.Fatalf("two builds of the same source are not signed identically:\n%s\n%s", sa, sb)
	}
	// the tasklane binary from the archive verifies the release it came in
	tasklane := filepath.Join(t.TempDir(), "tasklane")
	extractBinary(t, filepath.Join(a, "tasklane-0.0.0-test-linux-amd64.tar.gz"), tasklane)
	verify := func(dir, pub string) (string, error) {
		out, err := exec.Command(tasklane, "verify-release", pub, dir).CombinedOutput()
		return string(out), err
	}
	pub := filepath.Join(keyDir, "release.pub")
	out, err := verify(a, pub)
	if err != nil || !strings.Contains(out, "verified: signed by key") || strings.Count(out, "modules") < 2 {
		t.Fatalf("the real release does not verify: %v\n%s", err, out)
	}
	if v, _ := exec.Command(tasklane, "version").Output(); strings.TrimSpace(string(v)) != "0.0.0-test" {
		t.Fatalf("`tasklane version` says %q", v)
	}
	copyDir := func() string {
		d := t.TempDir()
		ents, _ := os.ReadDir(a)
		for _, e := range ents {
			bts, _ := os.ReadFile(filepath.Join(a, e.Name()))
			os.WriteFile(filepath.Join(d, e.Name()), bts, 0o644)
		}
		return d
	}
	refused := func(name, dir, pubFile, want string) {
		t.Helper()
		out, err := verify(dir, pubFile)
		if err == nil || !strings.Contains(out, want) {
			t.Errorf("%s: err=%v\n%s", name, err, out)
		}
	}
	// a changed byte in an archive
	d := copyDir()
	arch := filepath.Join(d, "tasklane-0.0.0-test-linux-arm64.tar.gz")
	bts, _ := os.ReadFile(arch)
	bts[len(bts)/3] ^= 1
	os.WriteFile(arch, bts, 0o644)
	refused("flipped byte", d, pub, "the signed list says")
	// the signature removed
	d = copyDir()
	os.Remove(filepath.Join(d, "SHA256SUMS.sig"))
	refused("unsigned", d, pub, "not signed")
	// someone else's key
	other := t.TempDir()
	relsign("keygen", other)
	refused("another key", copyDir(), filepath.Join(other, "release.pub"), "different key")
	// an archive added to the directory
	d = copyDir()
	os.WriteFile(filepath.Join(d, "tasklane-6.6.6-linux-amd64.tar.gz"), []byte("planted"), 0o644)
	refused("planted archive", d, pub, "not part of the release")
	// no key file, no directory: exit code 2 and a message, not a pass
	if out, err := verify(a, filepath.Join(t.TempDir(), "missing.pub")); err == nil || !strings.Contains(out, "cannot read the public key") {
		t.Errorf("%v\n%s", err, out)
	}
}

func extractBinary(t *testing.T, archive, dest string) {
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, _ := gzip.NewReader(f)
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatal("no binary in the archive")
		}
		if strings.HasSuffix(h.Name, "/tasklane") && h.Typeflag == tar.TypeReg {
			out, _ := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY, 0o755)
			io.Copy(out, tr)
			out.Close()
			return
		}
	}
}
