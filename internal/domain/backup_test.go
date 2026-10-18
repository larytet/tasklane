package domain_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func backupTo(t *testing.T, e *testutil.Env) (string, *domain.BackupManifest) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "backup")
	man, err := e.Svc.Backup(ctx, dir, domain.BackupOpts{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return dir, man
}

func freshTarget(t *testing.T) *testutil.Env {
	dst := testutil.New(t)
	dst.WithBlobs()
	// New creates the administrators and the default data every installation starts with; a restore target is an empty database
	dst.Store.Pool.Exec(ctx, `TRUNCATE app_user CASCADE`)
	return dst
}

// restoredActor is a person as the restored database knows them (their identifiers are the source's, not the target environment's).
func restoredActor(t *testing.T, e *testutil.Env, email string) domain.Actor {
	t.Helper()
	us, err := e.Svc.ListUsersFor(ctx, domain.Actor{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range us {
		if u.Email == email {
			a, err := e.Svc.ActorFor(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			return a
		}
	}
	t.Fatalf("%s is not in the restored database", email)
	return domain.Actor{}
}

// PAUSE-02: a backup of a database with data in it, restored into an empty one, is the same instance: every person, issue, comment,
// version, history entry, attachment and audit entry, with identifiers and sequences continuing where they left off.
func TestPAUSE02_ABackupRestoresIntoAnEmptyDatabaseAsTheSameInstance(t *testing.T) {
	src := testutil.New(t)
	rich(t, src)
	src.Svc.SetFieldTracking(ctx, src.Admin, "priority", true)
	dir, man := backupTo(t, src)
	if man.Format != domain.BackupFormat || man.Version != domain.BackupVersion || man.Tables["issue"].Rows < 7 || man.Blobs < 2 || len(man.BlobsMissing) != 0 || man.SchemaVersion < 50 {
		t.Fatalf("%+v", man)
	}
	if _, err := domain.VerifyBackup(dir); err != nil {
		t.Fatal(err)
	}
	for _, skipped := range []string{"session", "cluster_node", "pause_lease", "mfa_pending"} {
		if _, ok := man.Tables[skipped]; ok {
			t.Errorf("%s is in the backup: it describes the moment, not the data", skipped)
		}
	}
	dst := freshTarget(t)
	rep, err := dst.Svc.Restore(ctx, dir)
	if err != nil {
		t.Fatalf("%v %+v", err, rep)
	}
	if !rep.AuditOK || !rep.FsckClean || rep.Tables < 50 {
		t.Fatalf("%+v", rep)
	}
	for _, k := range goldenKeys {
		sameSnap(t, k, snapshotOf(t, src, src.Admin, k), snapshotOf(t, dst, dst.Admin, k))
	}
	// the trash and the people, with their passwords: Alice signs in on the restored instance with her old password
	for _, q := range []string{`SELECT count(*) FROM app_user`, `SELECT count(*) FROM comment`, `SELECT count(*) FROM issue_history`, `SELECT count(*) FROM text_version`, `SELECT count(*) FROM audit_event`, `SELECT count(*) FROM issue WHERE deleted_at IS NOT NULL`, `SELECT count(*) FROM attachment`} {
		var a, b int
		src.Store.Pool.QueryRow(ctx, q).Scan(&a)
		dst.Store.Pool.QueryRow(ctx, q).Scan(&b)
		if a != b {
			t.Errorf("%s: source %d, restored %d", q, a, b)
		}
	}
	if _, _, err := dst.Svc.Login(ctx, "alice@example.com", "password123", time.Hour); err != nil {
		t.Fatalf("a person cannot sign in on the restored instance: %v", err)
	}
	// the audit chain verifies and keeps growing without a seam
	if c, _ := dst.Svc.VerifyAudit(ctx, dst.Admin); !c.OK {
		t.Fatalf("%+v", c)
	}
	// sequences continue: a new issue gets the next number, a new comment a new id, and nothing collides
	alice := restoredActor(t, dst, "alice@example.com")
	is, err := dst.Svc.CreateIssue(ctx, alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "after the restore"})
	if err != nil {
		t.Fatalf("a new issue after the restore: %v", err)
	}
	var before string
	src.Store.Pool.QueryRow(ctx, `SELECT 'ABC-'||(max(number)+1) FROM issue WHERE project_id=(SELECT id FROM project WHERE key='ABC')`).Scan(&before)
	if is.Key != before {
		t.Fatalf("the next issue is %s, the source would have made %s", is.Key, before)
	}
	if _, err := dst.Svc.AddComment(ctx, alice, is.Key, "continues", false); err != nil {
		t.Fatal(err)
	}
	if c, _ := dst.Svc.VerifyAudit(ctx, dst.Admin); !c.OK {
		t.Fatalf("the audit chain broke after new entries: %+v", c)
	}
	// sessions are not carried: signing in to the source left one there and none here
	var ss int
	dst.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM session WHERE created_at < now() - interval '1 second'`).Scan(&ss)
	if ss != 0 {
		t.Fatalf("%d sessions came across", ss)
	}
	// a restore never goes over data that is there
	if _, err := dst.Svc.Restore(ctx, dir); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("a restore into a database with data: %v", err)
	}
}

// PAUSE-02 (with active users): writers keep saving while the backup runs. What comes back is a consistent state at one instant:
// for each writer the comments that exist are the first k it made, in order, with none missing in between, and at least those that
// were acknowledged before the backup began. A client holding a newer version than the restored one gets a conflict, not a silent
// overwrite.
func TestPAUSE02_BackupWhileUsersAreWritingIsConsistentAtOneInstant(t *testing.T) {
	src := testutil.New(t)
	src.WithBlobs()
	src.Project()
	is := src.Issue("busy", "text")
	const writers = 8
	var stop atomic.Bool
	acked := make([]atomic.Int64, writers)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				if _, err := src.Svc.AddComment(ctx, src.Alice, is.Key, fmt.Sprintf("w%d-%05d", w, i), false); err != nil {
					return
				}
				acked[w].Store(int64(i + 1))
			}
		}(w)
	}
	time.Sleep(300 * time.Millisecond)
	var floor [writers]int64
	started := make(chan struct{})
	src.Svc.BackupHook = func(string) { close(started) }
	for w := range floor {
		floor[w] = acked[w].Load() // acknowledged before the backup began: certainly in it
	}
	dir, man := backupTo(t, src)
	<-started
	time.Sleep(200 * time.Millisecond) // the writers go on after the snapshot
	stop.Store(true)
	wg.Wait()
	dst := freshTarget(t)
	if _, err := dst.Svc.Restore(ctx, dir); err != nil {
		t.Fatal(err)
	}
	alice := restoredActor(t, dst, "alice@example.com")
	v, err := dst.Svc.View(ctx, alice, is.Key)
	if err != nil {
		t.Fatal(err)
	}
	next := make([]int64, writers)
	for _, c := range v.Comments {
		var w int
		var i int64
		if _, err := fmt.Sscanf(c.Body, "w%d-%d", &w, &i); err != nil {
			t.Fatalf("%q", c.Body)
		}
		if i != next[w] {
			t.Fatalf("writer %d: found comment %d where %d was next: the restore is not one instant's state", w, i, next[w])
		}
		next[w]++
	}
	total := int64(0)
	for w := 0; w < writers; w++ {
		if next[w] < floor[w] {
			t.Fatalf("writer %d: %d comments came back, %d were acknowledged before the backup began", w, next[w], floor[w])
		}
		total += next[w]
	}
	var after int64
	for w := range acked {
		after += acked[w].Load()
	}
	if total >= after {
		t.Logf("the backup caught everything (%d of %d); the writers had not got ahead of it this time", total, after)
	}
	var restoredMax int64
	dst.Store.Pool.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM outbox`).Scan(&restoredMax)
	if restoredMax != man.Marker.OutboxID {
		t.Fatalf("the restored log ends at %d, the marker says %d", restoredMax, man.Marker.OutboxID)
	}
	// a client that had a newer version than the restored one is stopped, not allowed to overwrite
	seq := v.Issue.DescSeq + 3
	if _, err := dst.Svc.SaveText(ctx, alice, is.Key, domain.SaveIn{Field: "description", Body: "from a client that is ahead of the restore", BaseSeq: seq, Mode: "save", SaveID: "ahead-1"}); err == nil {
		t.Fatal("a save based on a version the restored instance never reached was applied")
	}
}

// PAUSE-09: the attachments are copied after the snapshot, and the garbage collector is held off while they are: a file the snapshot
// refers to is never deleted before it is copied, so the restored database never refers to a file the backup lacks.
func TestPAUSE09_AnAttachmentThatBecomesGarbageDuringABackupIsStillInIt(t *testing.T) {
	src := testutil.New(t)
	rich(t, src)
	src.Svc.Blobs.Retention = 0 // an unreferenced file is collectable at once
	v, _ := src.Svc.View(ctx, src.Alice, "ABC-2")
	var gone string
	for _, a := range v.Attachments {
		if a.Name == "notes.txt" {
			gone = a.SHA256
			// at the moment the snapshot is fixed, the only reference to this file is removed and the collector runs
			src.Svc.BackupHook = func(string) {
				if err := src.Svc.DeleteAttachment(ctx, src.Alice, "ABC-2", a.ID); err != nil {
					t.Errorf("%v", err)
				}
				time.Sleep(50 * time.Millisecond)
				n, err := src.Svc.GC(ctx)
				if err != nil || n != 0 {
					t.Errorf("the collector ran during a backup: removed %d (%v)", n, err)
				}
			}
		}
	}
	if gone == "" {
		t.Fatal("no attachment to remove")
	}
	dir, man := backupTo(t, src)
	if len(man.BlobsMissing) != 0 {
		t.Fatalf("the backup lost an attachment the snapshot refers to: %v", man.BlobsMissing)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", gone[:2], gone[2:4], gone)); err != nil {
		t.Fatalf("the file the snapshot refers to is not in the backup: %v", err)
	}
	// the hold ends with the backup: the collector works again and removes it now
	src.Svc.BackupHook = nil
	if n, _ := src.Svc.GC(ctx); n != 1 {
		t.Fatalf("after the backup the collector removed %d files, want 1", n)
	}
	// and what the snapshot referred to restores cleanly: no row without its file
	dst := freshTarget(t)
	rep, err := dst.Svc.Restore(ctx, dir)
	if err != nil || !rep.FsckClean {
		t.Fatalf("%v %+v", err, rep)
	}
	fr, _ := dst.Svc.Fsck(ctx, false)
	if len(fr.Missing) != 0 || len(fr.Corrupt) != 0 {
		t.Fatalf("%+v", fr)
	}
}

// PAUSE-12: the manifest says what the backup is, and a restore checks it: the format and its version, the schema it was taken at,
// every file's checksum, the marker, the secret key. An interrupted backup has no manifest and is refused.
func TestPAUSE12_TheManifestIsVerifiedBeforeAndAfterARestore(t *testing.T) {
	src := testutil.New(t)
	rich(t, src)
	dir, man := backupTo(t, src)
	if man.Marker.Mode != "snapshot" || man.Marker.LSN == "" || man.Marker.OutboxID == 0 || man.CreatedAt.IsZero() || man.TasklaneVersion != "test" || man.SecretCheck == "" {
		t.Fatalf("%+v", man.Marker)
	}
	// a second backup into the same directory is refused
	if _, err := src.Svc.Backup(ctx, dir, domain.BackupOpts{}); err == nil || !strings.Contains(err.Error(), "already holds a backup") {
		t.Fatalf("%v", err)
	}
	copyDir := func() string {
		d := filepath.Join(t.TempDir(), "copy")
		if out, err := execCopy(dir, d); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return d
	}
	edit := func(d string, fn func(m map[string]any)) {
		b, _ := os.ReadFile(filepath.Join(d, "manifest.json"))
		var m map[string]any
		json.Unmarshal(b, &m)
		fn(m)
		b, _ = json.Marshal(m)
		os.WriteFile(filepath.Join(d, "manifest.json"), b, 0o640)
	}
	expect := func(name, d, want string) {
		t.Helper()
		dst := freshTarget(t)
		_, err := dst.Svc.Restore(ctx, d)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want %q)", name, err, want)
		}
		var n int
		dst.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue`).Scan(&n)
		if n != 0 {
			t.Errorf("%s: a refused restore left %d issues", name, n)
		}
	}
	d := copyDir()
	os.Remove(filepath.Join(d, "manifest.json"))
	expect("interrupted", d, "manifest.json is missing")
	d = copyDir()
	edit(d, func(m map[string]any) { m["version"] = domain.BackupVersion + 1 })
	expect("newer format", d, "newer than this release understands")
	d = copyDir()
	edit(d, func(m map[string]any) { m["format"] = "something-else" })
	expect("another format", d, "not a Tasklane backup")
	d = copyDir()
	edit(d, func(m map[string]any) { m["schema_version"] = 99999 })
	expect("newer schema", d, "schema version")
	d = copyDir()
	f, _ := os.OpenFile(filepath.Join(d, "data", "issue.copy"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("tampered\n")
	f.Close()
	expect("a changed table", d, "does not match its checksum")
	d = copyDir()
	filepath.WalkDir(filepath.Join(d, "blobs"), func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			os.WriteFile(p, []byte("not the file"), 0o640)
		}
		return nil
	})
	expect("a changed attachment", d, "does not match its name")
	d = copyDir()
	os.Remove(filepath.Join(d, "data", "comment.copy"))
	expect("a missing table", d, "incomplete")
	d = copyDir()
	edit(d, func(m map[string]any) {
		tb := m["tables"].(map[string]any)["issue"].(map[string]any)
		tb["rows"] = tb["rows"].(float64) + 1
	})
	expect("a wrong row count", d, "")
	// the marker is checked against the restored log
	d = copyDir()
	edit(d, func(m map[string]any) { m["marker"].(map[string]any)["outbox_id"] = 1 })
	dst := freshTarget(t)
	if _, err := dst.Svc.Restore(ctx, d); err == nil || !strings.Contains(err.Error(), "marker") {
		t.Fatalf("a marker that does not match the log: %v", err)
	}
	// a different secret key: the restore succeeds and says what it means
	dst2 := freshTarget(t)
	dst2.Svc.SecretKey = bytes.Repeat([]byte{7}, 32)
	rep, err := dst2.Svc.Restore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(rep.Warnings, "\n"); !strings.Contains(w, "secret key differs") {
		t.Fatalf("a different secret key was not reported: %v", rep.Warnings)
	}
}

func execCopy(src, dst string) (string, error) {
	out, err := exec.Command("cp", "-a", src, dst).CombinedOutput()
	return string(out), err
}
