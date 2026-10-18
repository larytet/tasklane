// Package release signs and verifies release artifacts (INST-05). A release is a directory of tarballs, a SHA256SUMS file listing
// them, and SHA256SUMS.sig: an Ed25519 signature over the checksum file. Verifying a release checks the signature, every
// archive's hash, that no unlisted archive sits beside them, and that what each archive says about itself is true: the binary's
// embedded build information must match the SBOM and NOTICE it ships with, its architecture, and the version in its name.
package release

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	SumsFile = "SHA256SUMS"
	SigFile  = "SHA256SUMS.sig"
	// the signed message is this label, a line feed and the checksum file, so a signature made for one purpose cannot be
	// replayed as another
	sigLabel  = "tasklane-release-v1\n"
	sigHeader = "tasklane-release-signature v1"

	maxBinary = 300 << 20
	maxText   = 16 << 20
)

// GenerateKey makes a signing key pair as PEM (PKCS #8 private, PKIX public).
func GenerateKey() (pub, priv []byte, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	pd, _ := x509.MarshalPKIXPublicKey(pk)
	sd, _ := x509.MarshalPKCS8PrivateKey(sk)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pd}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: sd}), nil
}

func parsePub(p []byte) (ed25519.PublicKey, error) {
	b, _ := pem.Decode(p)
	if b == nil {
		return nil, errors.New("the public key is not PEM")
	}
	k, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	pk, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("the public key is not an Ed25519 key")
	}
	return pk, nil
}

func parsePriv(p []byte) (ed25519.PrivateKey, error) {
	b, _ := pem.Decode(p)
	if b == nil {
		return nil, errors.New("the private key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	sk, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("the private key is not an Ed25519 key")
	}
	return sk, nil
}

// KeyID names a public key: the first 16 bytes of the SHA-256 of its DER encoding, in hex.
func KeyID(pub ed25519.PublicKey) string {
	d, _ := x509.MarshalPKIXPublicKey(pub)
	h := sha256.Sum256(d)
	return hex.EncodeToString(h[:16])
}

// Message is what is signed for a checksum file.
func Message(sums []byte) []byte { return append([]byte(sigLabel), sums...) }

// Sign writes SHA256SUMS.sig next to SHA256SUMS in dir.
func Sign(dir string, privPEM []byte) error {
	sk, err := parsePriv(privPEM)
	if err != nil {
		return err
	}
	sums, err := os.ReadFile(filepath.Join(dir, SumsFile))
	if err != nil {
		return err
	}
	sig := ed25519.Sign(sk, Message(sums))
	out := fmt.Sprintf("%s\nkeyid: %s\nsignature: %s\n", sigHeader, KeyID(sk.Public().(ed25519.PublicKey)), base64.StdEncoding.EncodeToString(sig))
	return os.WriteFile(filepath.Join(dir, SigFile), []byte(out), 0o644)
}

func readSig(b []byte) (keyid string, sig []byte, err error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != sigHeader {
		return "", nil, errors.New("the signature file is not a Tasklane release signature")
	}
	var enc string
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ":")
		switch strings.TrimSpace(k) {
		case "keyid":
			keyid = strings.TrimSpace(v)
		case "signature":
			enc = strings.TrimSpace(v)
		}
	}
	sig, err = base64.StdEncoding.DecodeString(enc)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", nil, errors.New("the signature is malformed")
	}
	return keyid, sig, nil
}

// ArchiveReport is what verification found about one archive.
type ArchiveReport struct {
	Name    string
	SHA256  string
	Version string
	Arch    string
	Modules int // modules in the binary, all matching the SBOM and NOTICE when there are no problems
}

type Report struct {
	KeyID    string
	Archives []ArchiveReport
	Problems []string // empty when the release verifies
}

func (r *Report) OK() bool { return len(r.Problems) == 0 }

var reName = regexp.MustCompile(`^tasklane-(.+)-linux-(amd64|arm64)\.tar\.gz$`)

// Verify checks a release directory against a public key. It returns an error only when the verification could not be done
// (no checksum file, an unreadable key); everything wrong with the release itself is a problem in the report.
func Verify(dir string, pubPEM []byte) (*Report, error) {
	pub, err := parsePub(pubPEM)
	if err != nil {
		return nil, err
	}
	rep := &Report{KeyID: KeyID(pub)}
	sums, err := os.ReadFile(filepath.Join(dir, SumsFile))
	if err != nil {
		return nil, fmt.Errorf("no %s in %s", SumsFile, dir)
	}
	sigb, err := os.ReadFile(filepath.Join(dir, SigFile))
	if err != nil {
		rep.Problems = append(rep.Problems, "the release is not signed: "+SigFile+" is missing")
		return rep, nil
	}
	keyid, sig, err := readSig(sigb)
	switch {
	case err != nil:
		rep.Problems = append(rep.Problems, err.Error())
		return rep, nil
	case keyid != rep.KeyID:
		rep.Problems = append(rep.Problems, fmt.Sprintf("signed by a different key (%s, not %s)", keyid, rep.KeyID))
		return rep, nil
	case !ed25519.Verify(pub, Message(sums), sig):
		rep.Problems = append(rep.Problems, "the signature does not match the checksum file: it was altered or signed with another key")
		return rep, nil
	}
	// the checksum file is now the signer's word; everything else is checked against it
	listed := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			rep.Problems = append(rep.Problems, fmt.Sprintf("a line of %s is not \"hash  name\": %q", SumsFile, line))
			continue
		}
		name := strings.TrimPrefix(f[1], "*")
		if name != filepath.Base(name) || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s lists a path, not a file name: %q", SumsFile, name))
			continue
		}
		if _, dup := listed[name]; dup {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s lists %s twice", SumsFile, name))
		}
		listed[name] = strings.ToLower(f[0])
	}
	names := make([]string, 0, len(listed))
	for n := range listed {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s is listed but cannot be read: %v", name, err))
			continue
		}
		h := sha256.Sum256(b)
		got := hex.EncodeToString(h[:])
		ar := ArchiveReport{Name: name, SHA256: got}
		if got != listed[name] {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: the hash is %s but the signed list says %s", name, got[:16], listed[name][:min(16, len(listed[name]))]))
			rep.Archives = append(rep.Archives, ar)
			continue
		}
		if m := reName.FindStringSubmatch(name); m != nil {
			ar.Version, ar.Arch = m[1], m[2]
			ar.Modules = checkArchive(b, ar, rep)
		}
		rep.Archives = append(rep.Archives, ar)
	}
	// an archive that is not in the signed list is not part of the release
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			if _, ok := listed[e.Name()]; !ok {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s is here but not in the signed list: it is not part of the release", e.Name()))
			}
		}
	}
	if len(rep.Archives) == 0 {
		rep.Problems = append(rep.Problems, "the signed list names no archive")
	}
	return rep, nil
}

// readTar reads the three files an archive must carry, with bounds, and refuses unsafe entries.
func readTar(b []byte) (top string, files map[string][]byte, err error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return "", nil, errors.New("not a gzip file")
	}
	tr := tar.NewReader(zr)
	files = map[string][]byte{}
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("damaged archive: %v", err)
		}
		if n > 200 {
			return "", nil, errors.New("too many entries")
		}
		name := filepath.ToSlash(filepath.Clean(h.Name))
		if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || name == ".." || strings.Contains(name, "/../") {
			return "", nil, fmt.Errorf("an entry leaves the archive: %q", h.Name)
		}
		if h.Typeflag == tar.TypeSymlink || h.Typeflag == tar.TypeLink {
			return "", nil, fmt.Errorf("the archive contains a link: %q", h.Name)
		}
		parts := strings.Split(name, "/")
		if top == "" {
			top = parts[0]
		} else if parts[0] != top {
			return "", nil, fmt.Errorf("the archive has more than one top directory (%s and %s)", top, parts[0])
		}
		base := strings.TrimPrefix(name, top+"/")
		if h.Typeflag != tar.TypeReg || (base != "tasklane" && base != "NOTICE" && base != "SBOM.txt") {
			continue
		}
		limit := int64(maxText)
		if base == "tasklane" {
			limit = maxBinary
		}
		buf, err := io.ReadAll(io.LimitReader(tr, limit+1))
		if err != nil || int64(len(buf)) > limit {
			return "", nil, fmt.Errorf("%s is damaged or too large", base)
		}
		files[base] = buf
	}
	return top, files, nil
}

func checkArchive(b []byte, ar ArchiveReport, rep *Report) int {
	bad := func(f string, a ...any) { rep.Problems = append(rep.Problems, ar.Name+": "+fmt.Sprintf(f, a...)) }
	top, files, err := readTar(b)
	if err != nil {
		bad("%v", err)
		return 0
	}
	if want := "tasklane-" + ar.Version + "-linux-" + ar.Arch; top != want {
		bad("the top directory is %q, not %q", top, want)
	}
	for _, f := range []string{"tasklane", "NOTICE", "SBOM.txt"} {
		if _, ok := files[f]; !ok {
			bad("%s is missing", f)
		}
	}
	bin, ok := files["tasklane"]
	if !ok {
		return 0
	}
	bi, err := buildinfo.Read(bytes.NewReader(bin))
	if err != nil {
		bad("the binary carries no build information: %v", err)
		return 0
	}
	set := map[string]string{}
	for _, s := range bi.Settings {
		set[s.Key] = s.Value
	}
	if set["GOOS"] != "linux" || set["GOARCH"] != ar.Arch {
		bad("the binary is for %s/%s, the archive name says linux/%s", set["GOOS"], set["GOARCH"], ar.Arch)
	}
	// the version the release build wrote into the binary (see versionMarker in cmd/tasklane): every marker must say the archive's version
	markers := regexp.MustCompile(`tasklane-release-version=([0-9A-Za-z._+~-]+);`).FindAllSubmatch(bin, -1)
	switch {
	case len(markers) == 0:
		bad("the binary does not record its version")
	default:
		for _, m := range markers {
			if string(m[1]) != ar.Version {
				bad("the binary says version %s, the archive name says %s", m[1], ar.Version)
				break
			}
		}
	}
	// the modules linked into the binary, the SBOM and the NOTICE must say the same thing
	binMods := map[string]string{}
	for _, d := range bi.Deps {
		if d.Replace != nil {
			bad("module %s is replaced by %s in the build", d.Path, d.Replace.Path)
		}
		binMods[d.Path] = d.Version
	}
	sbomMods := map[string]string{}
	if sb, ok := files["SBOM.txt"]; ok {
		for _, line := range strings.Split(strings.TrimSpace(string(sb)), "\n") {
			if f := strings.Fields(line); len(f) >= 2 {
				sbomMods[f[0]] = f[1]
			}
		}
	}
	if _, ok := files["SBOM.txt"]; ok {
		for p, v := range binMods {
			if sv, ok := sbomMods[p]; !ok {
				bad("the binary contains %s %s, which the SBOM does not list", p, v)
			} else if sv != v {
				bad("the binary contains %s %s, the SBOM says %s", p, v, sv)
			}
		}
		for p, v := range sbomMods {
			if _, ok := binMods[p]; !ok {
				bad("the SBOM lists %s %s, which is not in the binary", p, v)
			}
		}
	}
	if nt, ok := files["NOTICE"]; ok {
		for p, v := range binMods {
			if !bytes.Contains(nt, []byte(p+" "+v)) {
				bad("the NOTICE has no entry for %s %s", p, v)
			}
		}
	}
	return len(binMods)
}
