package domain_test

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
)

func sptr(s string) *string { return &s }

func (d *dashEnv) ver(t *testing.T, project, name string) domain.Version {
	t.Helper()
	v, err := d.Svc.CreateVersion(ctx, d.Admin, project, domain.VersionIn{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (d *dashEnv) keysOf(t *testing.T, a domain.Actor, q string) []string {
	t.Helper()
	ks, err := d.pg.Keys(ctx, d.scope(t, a), q, 100)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	sort.Strings(ks)
	return ks
}

// mk makes an issue labelled vt (so a query can pick out this test's own issues); done moves it to the Done category.
func (d *dashEnv) mk(t *testing.T, project, summary string, done bool) string {
	t.Helper()
	is, err := d.Svc.CreateIssue(ctx, d.Alice, domain.CreateIn{Project: project, Type: "Task", Summary: summary, Labels: []string{"vt"}})
	if err != nil {
		t.Fatal(err)
	}
	if done {
		tid, ok := trans(t, d.Env, d.Alice, is.Key, "Resolve")
		if !ok {
			t.Fatal("no Resolve transition")
		}
		if _, err := d.Svc.Transition(ctx, d.Alice, is.Key, tid, nil); err != nil {
			t.Fatal(err)
		}
	}
	return is.Key
}

func isInvalid(err error) bool {
	var ie *domain.InvalidError
	return errors.As(err, &ie)
}

func (d *dashEnv) setFix(t *testing.T, a domain.Actor, key string, names ...string) {
	t.Helper()
	if _, err := d.Svc.SetIssueVersions(ctx, a, key, domain.SetVersionsIn{Fix: &names}); err != nil {
		t.Fatalf("set fix %s: %v", key, err)
	}
}

// VER01: versions belong to a project; its administrators manage them; names are unique without regard to case; dates are checked;
// people who cannot see the project do not learn it has versions.
func TestVER01_VersionsAreManagedByProjectAdministrators(t *testing.T) {
	d := newDash(t)
	if _, err := d.Svc.CreateVersion(ctx, d.Alice, "ABC", domain.VersionIn{Name: sptr("1.0")}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member made a version: %v", err)
	}
	v := d.ver(t, "ABC", "1.0")
	if v.Project != "ABC" || v.Released || v.Progress == nil || v.Progress.Total != 0 {
		t.Fatalf("new version: %+v", v)
	}
	if _, err := d.Svc.CreateVersion(ctx, d.Admin, "ABC", domain.VersionIn{Name: sptr(" 1.0 ")}); !isInvalid(err) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := d.Svc.CreateVersion(ctx, d.Admin, "ABC", domain.VersionIn{Name: sptr("")}); !isInvalid(err) {
		t.Fatalf("empty name: %v", err)
	}
	if _, err := d.Svc.CreateVersion(ctx, d.Admin, "ABC", domain.VersionIn{Name: sptr("2.0"), Start: sptr("2026-05-01"), ReleaseDate: sptr("2026-04-01")}); !isInvalid(err) {
		t.Fatalf("release before start: %v", err)
	}
	if _, err := d.Svc.CreateVersion(ctx, d.Admin, "ABC", domain.VersionIn{Name: sptr("2.0"), ReleaseDate: sptr("next week")}); !isInvalid(err) {
		t.Fatalf("a bad date: %v", err)
	}
	// the same name in another project is fine; each project sees only its own
	d.ver(t, "XYZ", "1.0")
	vs, err := d.Svc.ListVersions(ctx, d.Alice, "ABC", false)
	if err != nil || len(vs) != 1 || vs[0].Project != "ABC" {
		t.Fatalf("list: %+v %v", vs, err)
	}
	// Bob is in ABC only: XYZ and its versions do not exist for him
	if _, err := d.Svc.ListVersions(ctx, d.Bob, "XYZ", false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider listed versions: %v", err)
	}
	xv, _ := d.Svc.ListVersions(ctx, d.Alice, "XYZ", false)
	if _, err := d.Svc.GetVersion(ctx, d.Bob, xv[0].ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider read a version: %v", err)
	}
	// rename, describe, archive; an archived version leaves the default list
	u, err := d.Svc.UpdateVersion(ctx, d.Admin, v.ID, domain.VersionIn{Name: sptr("1.0.1"), Description: sptr("patch"), Archived: func() *bool { b := true; return &b }()})
	if err != nil || u.Name != "1.0.1" || !u.Archived {
		t.Fatalf("update: %+v %v", u, err)
	}
	if vs, _ = d.Svc.ListVersions(ctx, d.Alice, "ABC", false); len(vs) != 0 {
		t.Fatalf("archived listed: %+v", vs)
	}
	if vs, _ = d.Svc.ListVersions(ctx, d.Alice, "ABC", true); len(vs) != 1 {
		t.Fatalf("archived not listed on request: %+v", vs)
	}
	// overdue: a release date in the past on an unreleased version
	o, err := d.Svc.CreateVersion(ctx, d.Admin, "ABC", domain.VersionIn{Name: sptr("old"), ReleaseDate: sptr("2020-01-01")})
	if err != nil || !o.Overdue {
		t.Fatalf("overdue: %+v %v", o, err)
	}
}

// VER02: an issue's fix and affects versions are those of its own project, are recorded in its history, and show in its view.
func TestVER02_IssueVersions(t *testing.T) {
	d := newDash(t)
	d.ver(t, "ABC", "1.0")
	d.ver(t, "ABC", "2.0")
	d.ver(t, "XYZ", "x-only")
	key := d.mk(t, "ABC", "Version a1", false)
	fix, aff := []string{"1.0", "2.0"}, []string{"1.0"}
	got, err := d.Svc.SetIssueVersions(ctx, d.Bob, key, domain.SetVersionsIn{Fix: &fix, Affects: &aff})
	if err != nil || len(got.Fix) != 2 || len(got.Affects) != 1 {
		t.Fatalf("set: %+v %v", got, err)
	}
	v, _ := d.Svc.View(ctx, d.Alice, key)
	if len(v.Versions.Fix) != 2 || v.Versions.Affects[0] != "1.0" {
		t.Fatalf("view: %+v", v.Versions)
	}
	var hist int
	d.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue_history h JOIN issue i ON i.id=h.issue_id WHERE i.key=$1 AND h.field IN ('fix_versions','affects_versions')`, key).Scan(&hist)
	if hist != 2 {
		t.Fatalf("history records: %d", hist)
	}
	// a name that is not the project's, another project's version, and a missing project are all refused
	for _, bad := range [][]string{{"nope"}, {"x-only"}} {
		if _, err := d.Svc.SetIssueVersions(ctx, d.Bob, key, domain.SetVersionsIn{Fix: &bad}); !isInvalid(err) {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	// the same set again changes nothing (no new history); case does not matter
	same := []string{"2.0", "1.0"}
	d.Svc.SetIssueVersions(ctx, d.Bob, key, domain.SetVersionsIn{Fix: &same})
	d.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue_history h JOIN issue i ON i.id=h.issue_id WHERE i.key=$1 AND h.field='fix_versions'`, key).Scan(&hist)
	if hist != 1 {
		t.Fatalf("an unchanged set wrote history: %d", hist)
	}
	// an archived version cannot be newly added but stays where it already is
	vs, _ := d.Svc.ListVersions(ctx, d.Admin, "ABC", false)
	for _, x := range vs {
		if x.Name == "2.0" {
			d.Svc.UpdateVersion(ctx, d.Admin, x.ID, domain.VersionIn{Archived: func() *bool { b := true; return &b }()})
		}
	}
	keep := []string{"2.0"}
	if _, err := d.Svc.SetIssueVersions(ctx, d.Bob, key, domain.SetVersionsIn{Fix: &keep}); err != nil {
		t.Fatalf("keeping an archived version: %v", err)
	}
	other := d.mk(t, "ABC", "Version a2", false)
	if _, err := d.Svc.SetIssueVersions(ctx, d.Bob, other, domain.SetVersionsIn{Fix: &keep}); !isInvalid(err) {
		t.Fatalf("adding an archived version: %v", err)
	}
	// someone who cannot see the project learns nothing
	xk := d.mk(t, "XYZ", "Version hidden", false)
	if _, err := d.Svc.SetIssueVersions(ctx, d.Bob, xk, domain.SetVersionsIn{Fix: &keep}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider: %v", err)
	}
}

// VER03: progress counts the version's issues by status; releasing warns about what is not done, can move it on, and can be undone.
func TestVER03_ProgressAndRelease(t *testing.T) {
	d := newDash(t)
	a, b := d.ver(t, "ABC", "1.0"), d.ver(t, "ABC", "1.1")
	a1 := d.mk(t, "ABC", "Version a1", false)
	a2 := d.mk(t, "ABC", "Version a2", false)
	a5 := d.mk(t, "ABC", "Version a5", true)
	d.setFix(t, d.Alice, a1, "1.0")
	d.setFix(t, d.Alice, a2, "1.0")
	d.setFix(t, d.Alice, a5, "1.0")
	p, _ := d.Svc.GetVersion(ctx, d.Alice, a.ID)
	if p.Progress.Total != 3 || p.Progress.Done != 1 || p.Progress.Unresolved != 2 {
		t.Fatalf("progress: %+v", p.Progress)
	}
	if _, err := d.Svc.ReleaseVersion(ctx, d.Alice, a.ID, domain.ReleaseIn{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member released: %v", err)
	}
	// the first answer is the warning: nothing was released
	r, err := d.Svc.ReleaseVersion(ctx, d.Admin, a.ID, domain.ReleaseIn{})
	if err != nil || r.Released || len(r.Unresolved) != 2 {
		t.Fatalf("warning: %+v %v", r, err)
	}
	if again, _ := d.Svc.GetVersion(ctx, d.Admin, a.ID); again.Released {
		t.Fatal("the warning released the version")
	}
	// moving the open work to 1.1 releases 1.0 with only the finished issue left on it
	r, err = d.Svc.ReleaseVersion(ctx, d.Admin, a.ID, domain.ReleaseIn{MoveTo: b.ID, Date: "2026-06-01"})
	if err != nil || !r.Released || r.Moved != 2 || r.Version.ReleaseDate != "2026-06-01" || r.Version.ReleasedAt == "" {
		t.Fatalf("release: %+v %v", r, err)
	}
	if p, _ = d.Svc.GetVersion(ctx, d.Alice, a.ID); p.Progress.Total != 1 || p.Progress.Unresolved != 0 {
		t.Fatalf("after: %+v", p.Progress)
	}
	if p, _ = d.Svc.GetVersion(ctx, d.Alice, b.ID); p.Progress.Total != 2 {
		t.Fatalf("moved to: %+v", p.Progress)
	}
	if got := d.keysOf(t, d.Alice, `fixVersion = "1.1"`); len(got) != 2 {
		t.Fatalf("moved issues by query: %v", got)
	}
	if _, err := d.Svc.ReleaseVersion(ctx, d.Admin, a.ID, domain.ReleaseIn{}); !isInvalid(err) {
		t.Fatalf("released twice: %v", err)
	}
	// moving onto a released version, or onto itself, is refused
	c := d.ver(t, "ABC", "1.2")
	d.setFix(t, d.Alice, a1, "1.2")
	if _, err := d.Svc.ReleaseVersion(ctx, d.Admin, c.ID, domain.ReleaseIn{MoveTo: a.ID}); !isInvalid(err) {
		t.Fatalf("move to a released version: %v", err)
	}
	if _, err := d.Svc.ReleaseVersion(ctx, d.Admin, c.ID, domain.ReleaseIn{MoveTo: c.ID}); !isInvalid(err) {
		t.Fatalf("move to itself: %v", err)
	}
	// confirming releases and says what was left behind
	r, err = d.Svc.ReleaseVersion(ctx, d.Admin, c.ID, domain.ReleaseIn{Confirm: true})
	if err != nil || !r.Released || len(r.Unresolved) != 1 {
		t.Fatalf("confirmed release: %+v %v", r, err)
	}
	u, err := d.Svc.UnreleaseVersion(ctx, d.Admin, c.ID)
	if err != nil || u.Released || u.ReleasedAt != "" {
		t.Fatalf("unrelease: %+v %v", u, err)
	}
	if _, err := d.Svc.UnreleaseVersion(ctx, d.Admin, c.ID); !isInvalid(err) {
		t.Fatalf("unreleased twice: %v", err)
	}
}

// VER04: TQL names versions: by name, by released state, and empty; and a person never finds an issue through a version of a project
// they cannot read.
func TestVER04_VersionQueries(t *testing.T) {
	d := newDash(t)
	d.ver(t, "ABC", "1.0")
	d.ver(t, "ABC", "2.0")
	d.ver(t, "XYZ", "1.0")
	a1 := d.mk(t, "ABC", "Version a1", false)
	a2 := d.mk(t, "ABC", "Version a2", false)
	x1 := d.mk(t, "XYZ", "Version x1", false)
	d.setFix(t, d.Alice, a1, "1.0")
	d.setFix(t, d.Alice, a2, "2.0")
	d.setFix(t, d.Alice, x1, "1.0")
	aff := []string{"2.0"}
	d.Svc.SetIssueVersions(ctx, d.Alice, a1, domain.SetVersionsIn{Affects: &aff})
	want := func(who domain.Actor, q string, keys ...string) {
		t.Helper()
		sort.Strings(keys)
		if got := d.keysOf(t, who, q); strings.Join(got, ",") != strings.Join(keys, ",") {
			t.Fatalf("%s: got %v, want %v", q, got, keys)
		}
	}
	want(d.Alice, `fixVersion = "1.0"`, a1, x1)
	want(d.Bob, `fixVersion = "1.0"`, a1) // XYZ is not Bob's
	want(d.Alice, `fixVersion = "1.0" AND project = ABC`, a1)
	want(d.Alice, `fixVersion in ("1.0","2.0") AND project = ABC`, a1, a2)
	want(d.Alice, `fixVersion != "1.0" AND project = ABC AND fixVersion is not EMPTY`, a2)
	want(d.Alice, `affectedVersion = "2.0"`, a1)
	a3 := d.mk(t, "ABC", "Version a3", false)
	want(d.Alice, `labels = vt AND fixVersion is EMPTY`, a3)
	want(d.Alice, `fixVersion in (releasedVersions())`) // nothing released yet
	vs, _ := d.Svc.ListVersions(ctx, d.Admin, "ABC", false)
	for _, v := range vs {
		if v.Name == "1.0" {
			d.Svc.ReleaseVersion(ctx, d.Admin, v.ID, domain.ReleaseIn{Confirm: true})
		}
	}
	want(d.Alice, `fixVersion in (releasedVersions()) AND project = ABC`, a1)
	want(d.Alice, `fixVersion in (unreleasedVersions()) AND project = ABC`, a2)
	if _, err := d.pg.Keys(ctx, d.scope(t, d.Alice), `fixVersion = currentUser()`, 10); err == nil || !strings.Contains(err.Error(), "cannot be used") {
		t.Fatalf("a wrong function: %v", err)
	}
}

// VER05: release notes list the finished issues by kind and say what is not done; deleting a version can move its issues on.
func TestVER05_NotesAndDelete(t *testing.T) {
	d := newDash(t)
	v, w := d.ver(t, "ABC", "1.0"), d.ver(t, "ABC", "1.1")
	a1 := d.mk(t, "ABC", "Version a1", false)
	a5 := d.mk(t, "ABC", "Version a5", true)
	d.setFix(t, d.Alice, a1, "1.0")
	d.setFix(t, d.Alice, a5, "1.0")
	n, err := d.Svc.ReleaseNotes(ctx, d.Bob, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# ABC 1.0", "## Task", a5 + " Version a5", "## Not done", a1 + " Version a1"} {
		if !strings.Contains(n.Markdown, want) {
			t.Fatalf("notes lack %q:\n%s", want, n.Markdown)
		}
	}
	if strings.Index(n.Markdown, a1) < strings.Index(n.Markdown, "## Not done") {
		t.Fatalf("an unfinished issue is listed among the done:\n%s", n.Markdown)
	}
	if err := d.Svc.DeleteVersion(ctx, d.Alice, v.ID, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member deleted: %v", err)
	}
	if err := d.Svc.DeleteVersion(ctx, d.Admin, v.ID, v.ID); !isInvalid(err) {
		t.Fatalf("moved onto itself: %v", err)
	}
	if err := d.Svc.DeleteVersion(ctx, d.Admin, v.ID, w.ID); err != nil {
		t.Fatal(err)
	}
	if got := d.keysOf(t, d.Alice, `fixVersion = "1.1"`); len(got) != 2 {
		t.Fatalf("moved by delete: %v", got)
	}
	if _, err := d.Svc.GetVersion(ctx, d.Admin, v.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted version: %v", err)
	}
	if err := d.Svc.DeleteVersion(ctx, d.Admin, w.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := d.keysOf(t, d.Alice, `fixVersion is not EMPTY`); len(got) != 0 {
		t.Fatalf("links left after delete: %v", got)
	}
}
