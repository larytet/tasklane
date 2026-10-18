package domain_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

type tierEnv struct {
	*testutil.Env
	hotDir, coldDir string
	tiered          *blob.Tiered
	issue           domain.Issue
}

func newTierEnv(t *testing.T, wrap func(blob.Store) blob.Store) *tierEnv {
	e := &tierEnv{Env: testutil.New(t)}
	e.Project()
	e.hotDir, e.coldDir = t.TempDir(), t.TempDir()
	hot, _ := blob.NewFS(e.hotDir)
	cold, _ := blob.NewFS(e.coldDir)
	e.tiered = blob.NewTiered(hot, cold)
	var st blob.Store = e.tiered
	if wrap != nil {
		st = wrap(st)
	}
	if err := e.Svc.ConfigureBlobs(ctx, &domain.Blobs{Store: st, Retention: time.Hour}); err != nil {
		t.Fatal(err)
	}
	e.issue = e.Issue("tiered attachments", "x")
	return e
}

// attach uploads a file and returns its blob's hash; created ages the blob that many days.
func (e *tierEnv) attach(name string, size int, ageDays int) (sha string, body []byte) {
	e.T.Helper()
	body = bytes.Repeat([]byte(fmt.Sprintf("%s-%d|", name, size)), size/len(name)+1)[:size]
	body = append([]byte(fmt.Sprintf("content of %s %d ", name, size)), body...)
	at, err := e.Svc.AttachStream(ctx, e.Alice, e.issue.Key, name, "", bytes.NewReader(body))
	if err != nil {
		e.T.Fatal(err)
	}
	sha = at.SHA256
	e.Store.Pool.Exec(ctx, `UPDATE blob SET created_at=now() - make_interval(days => $2) WHERE sha256=$1`, sha, ageDays)
	return
}

func (e *tierEnv) tier(sha string) string {
	var tr string
	e.Store.Pool.QueryRow(ctx, `SELECT tier FROM blob WHERE sha256=$1`, sha).Scan(&tr)
	return tr
}

func (e *tierEnv) rules(rs ...domain.TierRule) {
	e.T.Helper()
	if err := e.Svc.SetTierRules(ctx, e.Admin, rs); err != nil {
		e.T.Fatal(err)
	}
}

func cls(s string) *string { return &s }

func (e *tierEnv) read(sha string) []byte {
	e.T.Helper()
	o, err := e.Svc.OpenBlob(sha)
	if err != nil {
		e.T.Fatalf("%s: %v", sha[:8], err)
	}
	defer o.Close()
	b, _ := io.ReadAll(o)
	return b
}

func TestBLOB15_RulesMoveByClassAgeAndSizeAndReadsStayTransparent(t *testing.T) {
	e := newTierEnv(t, nil)
	oldLog, oldLogBody := e.attach("app.log", 2000, 60)
	newLog, _ := e.attach("fresh.log", 2000, 1)
	oldDoc, _ := e.attach("notes.txt", 3000, 60)
	bigDoc, bigBody := e.attach("manual.txt", 300000, 2)
	ancient, _ := e.attach("ancient.txt", 500, 400)
	e.rules(
		domain.TierRule{Name: "old logs", Class: cls("log"), MinAgeDays: 30, Enabled: true},
		domain.TierRule{Name: "big files", MinSizeBytes: 100000, Enabled: true},
		domain.TierRule{Name: "very old", MinAgeDays: 365, Enabled: true},
		domain.TierRule{Name: "disabled", Class: cls("document"), Enabled: false},
	)
	// nothing moves until the rules run, and every object is readable the whole time
	for _, sha := range []string{oldLog, newLog, oldDoc, bigDoc, ancient} {
		if e.tier(sha) != "hot" {
			t.Fatalf("%s starts %s", sha[:8], e.tier(sha))
		}
	}
	run, err := e.Svc.RunTiering(ctx, 100)
	if err != nil || run.Moved != 3 || run.Failed != 0 {
		t.Fatalf("%+v %v", run, err)
	}
	want := map[string]string{oldLog: "cold", newLog: "hot", oldDoc: "hot", bigDoc: "cold", ancient: "cold"}
	for sha, tier := range want {
		if e.tier(sha) != tier {
			t.Errorf("%s is %s, want %s", sha[:8], e.tier(sha), tier)
		}
		if where := e.tiered.Where(sha); where != tier {
			t.Errorf("%s is in the %s store, want %s", sha[:8], where, tier)
		}
	}
	if !bytes.Equal(e.read(oldLog), oldLogBody) || !bytes.Equal(e.read(bigDoc), bigBody) {
		t.Fatal("a cold object reads differently")
	}
	// the attachment itself is unchanged (name, size, link), and integrity checks cover both tiers
	v, _ := e.Svc.View(ctx, e.Alice, e.issue.Key)
	if len(v.Attachments) != 5 {
		t.Fatalf("%d attachments", len(v.Attachments))
	}
	if fr, err := e.Svc.Fsck(ctx, false); err != nil || !fr.Clean() || fr.Checked != 5 {
		t.Fatalf("%+v %v", fr, err)
	}
	// running again moves nothing; what already moved stays where it is
	if again, _ := e.Svc.RunTiering(ctx, 100); again.Moved != 0 {
		t.Fatalf("%+v", again)
	}
	// a rule that is enabled later applies to what it matches
	e.rules(domain.TierRule{Name: "documents", Class: cls("document"), MinAgeDays: 30, Enabled: true})
	if r, _ := e.Svc.RunTiering(ctx, 100); r.Moved != 1 || e.tier(oldDoc) != "cold" || e.tier(newLog) != "hot" {
		t.Fatalf("%+v %s %s", r, e.tier(oldDoc), e.tier(newLog))
	}
	// the status says where things are, and what a read cost
	st, err := e.Svc.TierStatus(ctx, e.Admin)
	if err != nil || !st.Configured || st.LastRun == nil || st.ColdReads == 0 {
		t.Fatalf("%+v %v", st, err)
	}
	use := map[string]int64{}
	for _, u := range st.Use {
		use[u.Tier] = u.Objects
	}
	if use["cold"] != 4 || use["hot"] != 1 {
		t.Fatalf("%+v", st.Use)
	}
}

func TestBLOB15_UnreferencedAndUnscannedObjectsAreLeftAlone(t *testing.T) {
	e := newTierEnv(t, nil)
	sha, _ := e.attach("gone.log", 1000, 90)
	kept, _ := e.attach("kept.log", 1000, 90)
	scanning, _ := e.attach("scanning.log", 1000, 90)
	e.Store.Pool.Exec(ctx, `UPDATE blob SET scan_state='scanning' WHERE sha256=$1`, scanning)
	v, _ := e.Svc.View(ctx, e.Alice, e.issue.Key)
	for _, a := range v.Attachments {
		if a.SHA256 == sha {
			e.Svc.DeleteAttachment(ctx, e.Alice, e.issue.Key, a.ID)
		}
	}
	e.rules(domain.TierRule{Name: "logs", Class: cls("log"), MinAgeDays: 30, Enabled: true})
	if r, _ := e.Svc.RunTiering(ctx, 100); r.Moved != 1 {
		t.Fatalf("%+v", r)
	}
	if e.tier(kept) != "cold" || e.tier(sha) != "hot" || e.tier(scanning) != "hot" {
		t.Fatalf("%s %s %s", e.tier(kept), e.tier(sha), e.tier(scanning))
	}
}

func TestBLOB15_RulesAreValidatedAdministeredAndAudited(t *testing.T) {
	e := newTierEnv(t, nil)
	good := domain.TierRule{Name: "ok", Class: cls("log"), MinAgeDays: 1, Enabled: true}
	for name, r := range map[string]domain.TierRule{
		"no criteria":   {Name: "everything", Enabled: true},
		"bad class":     {Name: "x", Class: cls("music"), MinAgeDays: 1},
		"no name":       {Class: cls("log"), MinAgeDays: 1},
		"negative age":  {Name: "x", MinAgeDays: -1, Class: cls("log")},
		"negative size": {Name: "x", MinSizeBytes: -5, Class: cls("log")},
	} {
		if err := e.Svc.SetTierRules(ctx, e.Admin, []domain.TierRule{r}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := e.Svc.SetTierRules(ctx, e.Alice, []domain.TierRule{good}); err == nil {
		t.Fatal("a member set rules")
	}
	if _, err := e.Svc.TierRules(ctx, e.Alice); err == nil {
		t.Fatal("a member read rules")
	}
	if _, err := e.Svc.TierStatus(ctx, e.Alice); err == nil {
		t.Fatal("a member read the status")
	}
	e.rules(good, domain.TierRule{Name: "two", MinSizeBytes: 1 << 20, Enabled: true})
	if rs, _ := e.Svc.TierRules(ctx, e.Admin); len(rs) != 2 || rs[0].Name != "ok" {
		t.Fatalf("%+v", rs)
	}
	e.rules() // an empty list removes every rule
	if rs, _ := e.Svc.TierRules(ctx, e.Admin); len(rs) != 0 {
		t.Fatalf("%+v", rs)
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='blob.tier_rules'`).Scan(&n)
	if n != 2 { // one per accepted change; the refused ones leave nothing
		t.Fatalf("%d audit rows", n)
	}
	// without a cold tier, or without rules, running does nothing
	e.attach("a.log", 100, 90)
	if r, err := e.Svc.RunTiering(ctx, 10); err != nil || r.Moved != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	plain := testutil.New(t)
	plain.WithBlobs()
	if st, _ := plain.Svc.TierStatus(ctx, plain.Admin); st.Configured {
		t.Fatal("a store with no cold tier says it is tiered")
	}
	if r, err := plain.Svc.RunTiering(ctx, 10); err != nil || r.Moved != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestBLOB15_AColdTierThatFailsLeavesObjectsHotAndTheNextRunFinishes(t *testing.T) {
	e := newTierEnv(t, nil)
	var shas []string
	for i := 0; i < 4; i++ {
		sha, _ := e.attach(fmt.Sprintf("f%d.log", i), 4000, 90)
		shas = append(shas, sha)
	}
	e.rules(domain.TierRule{Name: "logs", Class: cls("log"), MinAgeDays: 30, Enabled: true})
	os.Chmod(e.coldDir, 0o500) // the cold tier refuses writes
	r, err := e.Svc.RunTiering(ctx, 10)
	os.Chmod(e.coldDir, 0o700)
	if err != nil || r.Moved != 0 || r.Failed != 4 || r.Error == "" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, sha := range shas {
		if e.tier(sha) != "hot" || e.tiered.Where(sha) != "hot" || len(e.read(sha)) == 0 {
			t.Fatalf("%s was damaged by a failed move: %s %s", sha[:8], e.tier(sha), e.tiered.Where(sha))
		}
	}
	if st, _ := e.Svc.TierStatus(ctx, e.Admin); st.LastRun == nil || st.LastRun.Failed != 4 || st.LastRun.Error == "" {
		t.Fatalf("%+v", st.LastRun)
	}
	if r, _ := e.Svc.RunTiering(ctx, 10); r.Moved != 4 || r.Failed != 0 {
		t.Fatalf("%+v", r)
	}
	// an object the cold tier already holds (a move that stopped before the hot copy was removed) is finished, not copied again
	sha, _ := e.attach("half.log", 4000, 90)
	src, _ := os.ReadFile(filepath.Join(e.hotDir, sha[:2], sha[2:4], sha))
	os.MkdirAll(filepath.Join(e.coldDir, sha[:2], sha[2:4]), 0o750)
	os.WriteFile(filepath.Join(e.coldDir, sha[:2], sha[2:4], sha), src, 0o440)
	if r, _ := e.Svc.RunTiering(ctx, 10); r.Moved != 1 || e.tiered.Where(sha) != "cold" {
		t.Fatalf("%+v %q", r, e.tiered.Where(sha))
	}
}

func TestBLOB15_TwoNodesMoveEachObjectOnce(t *testing.T) {
	e := newTierEnv(t, nil)
	var shas []string
	for i := 0; i < 30; i++ {
		sha, _ := e.attach(fmt.Sprintf("n%d.log", i), 1500, 90)
		shas = append(shas, sha)
	}
	e.rules(domain.TierRule{Name: "logs", Class: cls("log"), MinAgeDays: 30, Enabled: true})
	var wg sync.WaitGroup
	runs := make([]domain.TierRun, 2)
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runs[i], _ = e.Svc.RunTiering(context.Background(), 100)
		}(i)
	}
	wg.Wait()
	if runs[0].Moved+runs[1].Moved != 30 || runs[0].Failed+runs[1].Failed != 0 {
		t.Fatalf("%+v", runs)
	}
	for _, sha := range shas {
		if e.tier(sha) != "cold" || e.tiered.Where(sha) != "cold" {
			t.Fatalf("%s: %s %s", sha[:8], e.tier(sha), e.tiered.Where(sha))
		}
	}
}

func TestBLOB15_GarbageCollectionRemovesColdObjectsToo(t *testing.T) {
	e := newTierEnv(t, nil)
	sha, _ := e.attach("temp.log", 2000, 90)
	e.rules(domain.TierRule{Name: "logs", Class: cls("log"), MinAgeDays: 30, Enabled: true})
	e.Svc.RunTiering(ctx, 10)
	if e.tiered.Where(sha) != "cold" {
		t.Fatal("not cold")
	}
	v, _ := e.Svc.View(ctx, e.Alice, e.issue.Key)
	e.Svc.DeleteAttachment(ctx, e.Alice, e.issue.Key, v.Attachments[0].ID)
	e.Store.Pool.Exec(ctx, `UPDATE blob SET zero_since=now() - interval '3 hours' WHERE sha256=$1`, sha)
	if n, err := e.Svc.GC(ctx); err != nil || n != 1 || e.tiered.Where(sha) != "" {
		t.Fatalf("%d %v %q", n, err, e.tiered.Where(sha))
	}
}

func TestBLOB15_EncryptedObjectsTierWithoutAKey(t *testing.T) {
	var enc *blob.Encrypted
	e := newTierEnv(t, func(s blob.Store) blob.Store {
		enc, _ = blob.NewEncrypted(s, 1, map[byte][]byte{1: bytes.Repeat([]byte{7}, 32)})
		return enc
	})
	sha, body := e.attach("secret.log", 5000, 90)
	e.rules(domain.TierRule{Name: "logs", Class: cls("log"), MinAgeDays: 30, Enabled: true})
	if r, _ := e.Svc.RunTiering(ctx, 10); r.Moved != 1 || r.Failed != 0 {
		t.Fatalf("%+v", r)
	}
	raw, _ := os.ReadFile(filepath.Join(e.coldDir, sha[:2], sha[2:4], sha))
	if len(raw) <= len(body) || strings.Contains(string(raw), "content of secret.log") {
		t.Fatal("the cold tier holds readable content")
	}
	if !bytes.Equal(e.read(sha), body) {
		t.Fatal("a cold encrypted object does not read back")
	}
	if fr, err := e.Svc.Fsck(ctx, false); err != nil || !fr.Clean() {
		t.Fatalf("%+v %v", fr, err)
	}
}
