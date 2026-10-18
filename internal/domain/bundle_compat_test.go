package domain_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// UPG-09 (SPEC 17.8): the data formats stay readable by the next releases. A bundle written by every released format version is kept
// under testdata/bundles/vN together with what it must import as; each release must still import all of them, and a release that
// raises BundleVersion must add its own fixture (a test fails otherwise). To make the fixture of a new version:
//
//	TASKLANE_UPDATE_GOLDEN=1 go test ./internal/domain -run TestUPG09_WriteGolden
//
// and commit it. Fixtures are never regenerated for versions already released: they are the evidence that old files still read.

const goldenDir = "testdata/bundles"

type goldenIssue struct {
	Key         string   `json:"key"`
	Type        string   `json:"type"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    string   `json:"priority"`
	Labels      []string `json:"labels"`
	Assignee    string   `json:"assignee"`
	Reporter    string   `json:"reporter"`
	Parent      string   `json:"parent"`
	Created     string   `json:"created"`
	Comments    []string `json:"comments"`
	Links       []string `json:"links"`
	Attachments []string `json:"attachments"`
}

func goldenOf(t *testing.T, e *testutil.Env, a domain.Actor, key string) goldenIssue {
	t.Helper()
	s := snapshotOf(t, e, a, key)
	i := s.Issue
	em := func(u *domain.User) string {
		if u == nil {
			return ""
		}
		return u.Email
	}
	return goldenIssue{Key: i.Key, Type: i.Type, Summary: i.Summary, Description: i.Description, Status: i.Status, Priority: i.Priority, Labels: append([]string{}, i.Labels...),
		Assignee: em(i.Assignee), Reporter: em(i.Reporter), Parent: i.Parent, Created: i.Created.UTC().Format(time.RFC3339), Comments: s.Comments, Links: s.Links, Attachments: s.Attachments}
}

var goldenKeys = []string{"ABC-1", "ABC-2", "ABC-3", "ABC-4", "ABC-6", "ABC-7", "XYZ-1"}

func TestUPG09_WriteGolden(t *testing.T) {
	if os.Getenv("TASKLANE_UPDATE_GOLDEN") == "" {
		t.Skip("set TASKLANE_UPDATE_GOLDEN=1 to write the fixture of the current bundle version")
	}
	dir := filepath.Join(goldenDir, fmt.Sprintf("v%d", domain.BundleVersion))
	if _, err := os.Stat(filepath.Join(dir, "expected.json")); err == nil {
		t.Fatalf("%s exists: the fixture of a released version is evidence and is not rewritten", dir)
	}
	os.MkdirAll(dir, 0o755)
	src := testutil.New(t)
	rich(t, src)
	for _, format := range []string{"json", "csv"} {
		o := domain.DefaultExport()
		o.Format, o.History = format, true
		b, _ := exportBytes(t, src, src.Admin, o)
		if err := os.WriteFile(filepath.Join(dir, "bundle-"+format+".zip"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var exp []goldenIssue
	for _, k := range goldenKeys {
		exp = append(exp, goldenOf(t, src, src.Admin, k))
	}
	b, _ := json.MarshalIndent(exp, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "expected.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readGolden(t *testing.T, version int) (map[string][]byte, []goldenIssue) {
	t.Helper()
	dir := filepath.Join(goldenDir, fmt.Sprintf("v%d", version))
	files := map[string][]byte{}
	for _, f := range []string{"bundle-json.zip", "bundle-csv.zip"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("version %d has no %s: %v (a release that raises BundleVersion must add its fixture)", version, f, err)
		}
		files[f] = b
	}
	var exp []goldenIssue
	b, err := os.ReadFile(filepath.Join(dir, "expected.json"))
	if err != nil || json.Unmarshal(b, &exp) != nil || len(exp) == 0 {
		t.Fatalf("version %d has no usable expected.json: %v", version, err)
	}
	return files, exp
}

func checkImported(t *testing.T, e *testutil.Env, exp []goldenIssue) {
	t.Helper()
	for _, want := range exp {
		got := goldenOf(t, e, e.Admin, want.Key)
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		if string(gb) != string(wb) {
			t.Errorf("%s imported differently from what the file says it holds:\n  got  %s\n  want %s", want.Key, gb, wb)
		}
	}
}

func importFresh(t *testing.T, b []byte) *testutil.Env {
	t.Helper()
	dst := testutil.New(t)
	dst.WithBlobs()
	rep, err := dst.Svc.Import(ctx, dst.Admin, bytes.NewReader(b), int64(len(b)), domain.ImportOpts{})
	if err != nil {
		t.Fatalf("import: %v (%+v)", err, rep)
	}
	if len(rep.Errors) > 0 {
		t.Fatalf("import errors: %v", rep.Errors)
	}
	return dst
}

// A fixture exists for every bundle version up to the current one: raising BundleVersion without writing its fixture fails here.
func TestUPG09_EveryBundleVersionHasAFixture(t *testing.T) {
	for v := 1; v <= domain.BundleVersion; v++ {
		readGolden(t, v)
	}
	// and nothing is waiting for a version that does not exist yet
	ents, _ := os.ReadDir(goldenDir)
	for _, e := range ents {
		var v int
		if _, err := fmt.Sscanf(e.Name(), "v%d", &v); err != nil || v > domain.BundleVersion {
			t.Errorf("fixture %s is for a version this code does not write", e.Name())
		}
	}
}

// Every released version's bundles, JSON and CSV, still import and come out as they always did.
func TestUPG09_BundlesWrittenByEveryEarlierVersionStillImport(t *testing.T) {
	for v := 1; v <= domain.BundleVersion; v++ {
		files, exp := readGolden(t, v)
		for name, b := range files {
			t.Run(fmt.Sprintf("v%d/%s", v, name), func(t *testing.T) {
				man, _, _, err := domain.ReadBundle(bytes.NewReader(b), int64(len(b)))
				if err != nil || man.Version != v {
					t.Fatalf("%v %+v", err, man)
				}
				checkImported(t, importFresh(t, b), exp)
			})
		}
	}
}

// rewriteBundle opens a bundle, lets edit change the files (name -> content) and the manifest, and writes it again with the manifest's
// sizes and checksums brought up to date, as a later release that adds to the format would.
func rewriteBundle(t *testing.T, in []byte, edit func(files map[string][]byte, man map[string]any)) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(in), int64(len(in)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	var order []string
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = b
		order = append(order, f.Name)
	}
	var man map[string]any
	if err := json.Unmarshal(files["manifest.json"], &man); err != nil {
		t.Fatal(err)
	}
	edit(files, man)
	listed, _ := man["files"].(map[string]any)
	for name, b := range files {
		if name == "manifest.json" {
			continue
		}
		if _, ok := listed[name]; ok {
			sum := sha256.Sum256(b)
			listed[name] = map[string]any{"sha256": hex.EncodeToString(sum[:]), "bytes": len(b)}
		}
	}
	mb, _ := json.Marshal(man)
	files["manifest.json"] = mb
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	names := append([]string(nil), order...)
	for n := range files {
		found := false
		for _, o := range order {
			if o == n {
				found = true
			}
		}
		if !found {
			names = append(names, n)
		}
	}
	sort.SliceStable(names, func(i, j int) bool { return false })
	for _, n := range names {
		w, _ := zw.Create(n)
		w.Write(files[n])
	}
	zw.Close()
	return out.Bytes()
}

// A later release that only adds to the format (a field on a record, a column, a file, a key in the manifest) must not break an earlier
// reader: unknown things are ignored. This is the shape of change a minor release may make without raising BundleVersion.
func TestUPG09_AdditionsFromALaterReleaseAreIgnored(t *testing.T) {
	files, exp := readGolden(t, 1)
	// JSON: a new field on every line, a new top-level key and a new file in the manifest
	future := rewriteBundle(t, files["bundle-json.zip"], func(f map[string][]byte, man map[string]any) {
		for name, b := range f {
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			var out bytes.Buffer
			for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				var m map[string]any
				if err := json.Unmarshal(line, &m); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				m["added_in_a_later_release"] = map[string]any{"nested": []any{1, 2, 3}, "note": "ünïcode"}
				nb, _ := json.Marshal(m)
				out.Write(nb)
				out.WriteByte('\n')
			}
			f[name] = out.Bytes()
		}
		man["added_manifest_key"] = map[string]any{"anything": true}
		f["extra/readme.txt"] = []byte("a file this release has never heard of")
		man["files"].(map[string]any)["extra/readme.txt"] = map[string]any{}
	})
	checkImported(t, importFresh(t, future), exp)
	// CSV: a new column on every file
	futureCSV := rewriteBundle(t, files["bundle-csv.zip"], func(f map[string][]byte, man map[string]any) {
		for name, b := range f {
			if !strings.HasSuffix(name, ".csv") {
				continue
			}
			rows, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
			if err != nil || len(rows) == 0 {
				continue
			}
			var out bytes.Buffer
			w := csv.NewWriter(&out)
			for i, r := range rows {
				extra := "from the future"
				if i == 0 {
					extra = "added_in_a_later_release"
				}
				w.Write(append(r, extra))
			}
			w.Flush()
			f[name] = out.Bytes()
		}
	})
	checkImported(t, importFresh(t, futureCSV), exp)
}

// A bundle whose version is newer than this server is refused whole, with words that say what to do, before anything is changed; and
// damage is still caught.
func TestUPG09_ANewerVersionIsRefusedBeforeAnythingChangesAndDamageIsStillCaught(t *testing.T) {
	files, _ := readGolden(t, 1)
	newer := rewriteBundle(t, files["bundle-json.zip"], func(f map[string][]byte, man map[string]any) { man["version"] = domain.BundleVersion + 1 })
	dst := testutil.New(t)
	dst.WithBlobs()
	_, err := dst.Svc.Import(ctx, dst.Admin, bytes.NewReader(newer), int64(len(newer)), domain.ImportOpts{})
	if err == nil || !strings.Contains(err.Error(), "newer than this server understands") || !strings.Contains(err.Error(), "upgrade Tasklane") {
		t.Fatalf("%v", err)
	}
	var n int
	dst.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue`).Scan(&n)
	if n != 0 {
		t.Fatalf("a refused bundle changed %d issue(s)", n)
	}
	// a bundle of another kind of file
	if _, _, _, err := domain.ReadBundle(bytes.NewReader([]byte("not a zip")), 9); err == nil {
		t.Fatal("garbage was read as a bundle")
	}
	// a file that no longer matches its checksum
	tampered := rewriteBundle(t, files["bundle-json.zip"], func(f map[string][]byte, man map[string]any) {})
	zr, _ := zip.NewReader(bytes.NewReader(tampered), int64(len(tampered)))
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if f.Name == "issues.jsonl" {
			b = append(b, ' ')
		}
		w, _ := zw.Create(f.Name)
		w.Write(b)
	}
	zw.Close()
	if _, _, _, err := domain.ReadBundle(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("a tampered bundle was read: %v", err)
	}
}
