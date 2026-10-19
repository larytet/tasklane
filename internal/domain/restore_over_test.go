package domain_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func issueCount(t *testing.T, e *testutil.Env) (n int) {
	t.Helper()
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue`).Scan(&n)
	return
}

// PAUSE-02 (live): a restore over a running instance holds the writers, replaces the data in one transaction, writes a backup of what it
// replaced first, ends its pause whatever happens, signs everyone out, and is recorded in the audit log.
func TestPAUSE02_RestoreOverARunningInstance(t *testing.T) {
	e := testutil.New(t)
	e.WithBlobs()
	rich(t, e)
	e.Clock.T = time.Now()
	dir, man := backupTo(t, e)
	atBackup := issueCount(t, e)
	// life goes on after the backup: more work, a person signed in, an attachment's issue
	extra := e.Issue("made after the backup", "")
	if _, err := e.Svc.AddComment(ctx, e.Admin, extra.Key, "a comment after the backup", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.Pool.Exec(ctx, `INSERT INTO session(token_hash,user_id,expires_at) VALUES('\x0102'::bytea,$1::uuid,now()+interval '1 hour')`, e.Admin.ID); err != nil {
		t.Fatal(err)
	}
	if issueCount(t, e) != atBackup+1 {
		t.Fatal("setup")
	}
	if _, err := e.Svc.RestoreOver(ctx, e.Alice, dir, domain.RestoreOverOpts{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member restored over the instance: %v", err)
	}
	// writers keep working until the restore pauses them and are refused (not broken) while it runs
	domain.PauseAdmitWait = 50 * time.Millisecond
	defer func() { domain.PauseAdmitWait = 150 * time.Millisecond }()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var mu sync.Mutex
	paused, other, okAfter := 0, 0, 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, err := e.Svc.AddComment(ctx, e.Admin, extra.Key, "writer "+strings.Repeat("x", i%3), false)
			mu.Lock()
			var p *domain.ErrPaused
			switch {
			case err == nil:
				okAfter++
			case errors.As(err, &p):
				paused++
			case errors.Is(err, domain.ErrNotFound): // the issue it writes to was made after the backup: gone once the restore has run
				mu.Unlock()
				return
			default:
				other++
				t.Logf("writer: %v", err)
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	safety := filepath.Join(t.TempDir(), "before")
	rep, err := e.Svc.RestoreOver(ctx, e.Admin, dir, domain.RestoreOverOpts{SafetyDir: safety})
	close(stop)
	wg.Wait()
	if err != nil || rep == nil {
		t.Fatalf("%v %+v", err, rep)
	}
	if !rep.AuditOK || !rep.FsckClean || rep.Marker.OutboxID != man.Marker.OutboxID {
		t.Fatalf("report: %+v", rep)
	}
	if other != 0 {
		t.Fatalf("%d writes failed with something other than a pause", other)
	}
	if paused == 0 {
		t.Fatal("no writer was ever held: the pause was not in force")
	}
	// the data is the backup's: the issue made afterwards is gone, the rest is there
	if n := issueCount(t, e); n != atBackup {
		t.Fatalf("%d issues after the restore, the backup had %d", n, atBackup)
	}
	var gone int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE key=$1`, extra.Key).Scan(&gone)
	if gone != 0 {
		t.Fatal("work made after the backup survived the restore")
	}
	// everyone is signed out
	var sessions int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM session`).Scan(&sessions)
	if sessions != 0 {
		t.Fatalf("%d sessions survived", sessions)
	}
	// the pause is over
	if l, _ := e.Svc.PauseStatus(ctx, e.Admin); len(l) != 0 {
		t.Fatalf("a pause is still held: %+v", l)
	}
	admin := restoredActor(t, e, "admin@example.com")
	if _, err := e.Svc.CreateIssue(ctx, admin, domain.CreateIn{Project: "ABC", Summary: "after the restore"}); err != nil {
		t.Fatalf("writes after the restore: %v", err)
	}
	// the audit log says what happened
	for _, a := range []string{"restore.completed"} {
		var n int
		e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action=$1`, a).Scan(&n)
		if n != 1 {
			t.Fatalf("%d %s events", n, a)
		}
	}
	if c, err := e.Svc.VerifyAudit(ctx, domain.Actor{Admin: true}); err != nil || !c.OK {
		t.Fatalf("the audit chain after the restore: %+v %v", c, err)
	}
	// the backup of what was replaced is a real backup, and has the work the restore removed
	if _, err := domain.VerifyBackup(safety); err != nil {
		t.Fatalf("the safety backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(safety, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	rep2, err := e.Svc.RestoreOver(ctx, admin, safety, domain.RestoreOverOpts{})
	if err != nil || rep2 == nil {
		t.Fatalf("undoing the restore: %v", err)
	}
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE key=$1`, extra.Key).Scan(&gone)
	if gone != 1 {
		t.Fatal("restoring the safety backup did not bring back the replaced work")
	}
}

// A restore that cannot proceed changes nothing and leaves no pause behind: a damaged backup, an overlapping pause, a safety directory that
// is already a backup.
func TestPAUSE02_RestoreOverRefusalsChangeNothing(t *testing.T) {
	e := testutil.New(t)
	e.WithBlobs()
	rich(t, e)
	dir, _ := backupTo(t, e)
	before := issueCount(t, e)
	check := func(what string) {
		t.Helper()
		if issueCount(t, e) != before {
			t.Fatalf("%s: the data changed", what)
		}
		if l, _ := e.Svc.PauseStatus(ctx, e.Admin); len(l) != 0 && what != "overlapping pause" {
			t.Fatalf("%s: a pause is left behind: %+v", what, l)
		}
	}
	// the safety directory already holds a backup: refused before anything is replaced
	if _, err := e.Svc.RestoreOver(ctx, e.Admin, dir, domain.RestoreOverOpts{SafetyDir: dir}); err == nil || !strings.Contains(err.Error(), "nothing was replaced") {
		t.Fatalf("a safety backup that failed: %v", err)
	}
	check("failed safety backup")
	// another pause is held: refused, and that pause is not ours to end
	l, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "an import"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.RestoreOver(ctx, e.Admin, dir, domain.RestoreOverOpts{}); err == nil {
		t.Fatal("restored over another job's pause")
	}
	check("overlapping pause")
	if ls, _ := e.Svc.PauseStatus(ctx, e.Admin); len(ls) != 1 || ls[0].ID != l.ID {
		t.Fatalf("the other job's pause was disturbed: %+v", ls)
	}
	e.Svc.ReleasePause(ctx, e.Admin, l.ID)
	// a damaged backup is refused before a pause is taken
	f, _ := os.OpenFile(filepath.Join(dir, "data", "app_user.copy"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("x\n")
	f.Close()
	if _, err := e.Svc.RestoreOver(ctx, e.Admin, dir, domain.RestoreOverOpts{}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a damaged backup: %v", err)
	}
	check("damaged backup")
	// without the live mode the old rule stands: a database with data is not restored over
	good, _ := backupTo(t, e)
	if _, err := e.Svc.Restore(ctx, good); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("a plain restore over data: %v", err)
	}
}
