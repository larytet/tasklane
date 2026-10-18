package dist_test

import (
	"archive/tar"
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
