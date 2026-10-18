package domain_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func inboxOf(t *testing.T, e *testutil.Env, a domain.Actor, state string) []domain.Notification {
	t.Helper()
	out, err := e.Svc.Inbox(ctx, a, domain.InboxQuery{State: state})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func notify(t *testing.T, e *testutil.Env) {
	t.Helper()
	if _, err := e.Svc.NotifyOnce(ctx); err != nil {
		t.Fatal(err)
	}
}

func kinds(ns []domain.Notification) string {
	var k []string
	for _, n := range ns {
		k = append(k, n.Kind)
	}
	return strings.Join(k, ",")
}

// CORE-10: the inbox exists with no mail configured, reaches the right people once per event, and respects preferences.
func TestCORE10_InboxRecipientsWatchersAndSelf(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	carol := e.NewUser("carol@example.com", "Carol", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", carol.ID, "member")
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "inbox", Assignee: e.Bob.ID})
	notify(t, e)
	// created and assigned to Bob: Bob is told, Alice (the actor) is not
	if got := kinds(inboxOf(t, e, e.Bob, "")); got != "created" {
		t.Fatalf("bob: %q", got)
	}
	if len(inboxOf(t, e, e.Alice, "")) != 0 {
		t.Fatal("people are notified of their own actions")
	}
	// Carol watches; a comment reaches Alice (reporter), Bob (assignee) and Carol (watcher), not the commenter
	if _, err := e.Svc.SetWatch(ctx, carol, is.Key, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddComment(ctx, e.Bob, is.Key, "first comment", false); err != nil {
		t.Fatal(err)
	}
	notify(t, e)
	if got := kinds(inboxOf(t, e, e.Alice, "")); got != "comment" {
		t.Fatalf("alice: %q", got)
	}
	if got := kinds(inboxOf(t, e, carol, "")); got != "comment" {
		t.Fatalf("carol: %q", got)
	}
	if got := kinds(inboxOf(t, e, e.Bob, "")); got != "created" {
		t.Fatalf("the commenter heard about their own comment: %q", got)
	}
	// replaying the whole outbox changes nothing: one notification per person and event
	e.Store.Pool.Exec(ctx, `UPDATE outbox_cursor SET last_id=0 WHERE consumer='notifier'`)
	notify(t, e)
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM notification`).Scan(&n)
	if n != 3 {
		t.Fatalf("a replay created duplicates: %d notifications", n)
	}
	// an explicit "do not tell me" beats being the reporter
	if _, err := e.Svc.SetWatch(ctx, e.Alice, is.Key, false); err != nil {
		t.Fatal(err)
	}
	e.Svc.AddComment(ctx, e.Bob, is.Key, "second comment", false)
	notify(t, e)
	if got := len(inboxOf(t, e, e.Alice, "")); got != 1 {
		t.Fatalf("a muted issue still notified: %d", got)
	}
	ws, _ := e.Svc.WatchState(ctx, e.Alice, is.Key)
	if ws.Watching || !ws.Muted {
		t.Fatalf("%+v", ws)
	}
	if ws, _ = e.Svc.ClearWatch(ctx, e.Alice, is.Key); !ws.Watching || ws.Muted {
		t.Fatalf("back to the default: %+v", ws)
	}
	// auto-watch off: Bob (assignee) stops hearing about comments by others unless he watches, but still hears of assignments
	if _, err := e.Svc.SetNotifyPrefs(ctx, e.Bob, domain.NotifyPrefs{AutoWatch: false, Inbox: true, Email: true}); err != nil {
		t.Fatal(err)
	}
	before := len(inboxOf(t, e, e.Bob, ""))
	e.Svc.AddComment(ctx, e.Alice, is.Key, "third comment", false)
	notify(t, e)
	if got := len(inboxOf(t, e, e.Bob, "")); got != before {
		t.Fatalf("auto-watch off still notified Bob: %d -> %d", before, got)
	}
	other, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "assigned later"})
	b := e.Bob.ID
	e.Svc.UpdateFields(ctx, e.Alice, other.Key, nil, false, domain.Changes{Assignee: &b})
	notify(t, e)
	if got := len(inboxOf(t, e, e.Bob, "")); got != before+1 {
		for _, n := range inboxOf(t, e, e.Bob, "") {
			t.Logf("%s %s %q", n.Issue, n.Kind, n.What)
		}
		t.Fatalf("an assignment must always reach the assignee: %d (was %d)", got, before)
	}
}

func TestCORE10_PreferencesMuteKindsAndChannels(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "prefs", Assignee: e.Bob.ID})
	if _, err := e.Svc.SetNotifyPrefs(ctx, e.Bob, domain.NotifyPrefs{AutoWatch: true, Inbox: true, Email: true, Muted: []string{"comment"}}); err != nil {
		t.Fatal(err)
	}
	e.Svc.AddComment(ctx, e.Alice, is.Key, "muted kind", false)
	tid, _ := trans(t, e, e.Alice, is.Key, "Start progress")
	e.Svc.Transition(ctx, e.Alice, is.Key, tid, nil)
	notify(t, e)
	if got := kinds(inboxOf(t, e, e.Bob, "")); got != "status,created" && got != "created,status" {
		t.Fatalf("muting comments should leave created and status: %q", got)
	}
	// the inbox switched off: nothing is written for that person
	e.Svc.SetNotifyPrefs(ctx, e.Bob, domain.NotifyPrefs{AutoWatch: true, Inbox: false, Email: true})
	b := len(inboxOf(t, e, e.Bob, ""))
	e.Svc.AddComment(ctx, e.Alice, is.Key, "no inbox", false)
	notify(t, e)
	if len(inboxOf(t, e, e.Bob, "")) != b {
		t.Fatal("inbox off still wrote")
	}
	// bad preferences are refused
	for _, bad := range []domain.NotifyPrefs{
		{Muted: []string{"nonsense"}}, {QuietStart: "22:00"}, {QuietStart: "25:00", QuietEnd: "07:00"}, {Timezone: "Mars/Olympus"},
	} {
		if _, err := e.Svc.SetNotifyPrefs(ctx, e.Bob, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestCORE10_NotificationSchemePerProject(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "scheme", Assignee: e.Bob.ID})
	notify(t, e)
	base := len(inboxOf(t, e, e.Bob, ""))
	// by default a status change reaches the reporter and the assignee
	tid, _ := trans(t, e, e.Alice, is.Key, "Start progress")
	e.Svc.Transition(ctx, e.Bob, is.Key, tid, nil)
	notify(t, e)
	if got := len(inboxOf(t, e, e.Alice, "")); got != 1 {
		t.Fatalf("default scheme: alice %d", got)
	}
	// the project says status changes notify nobody and comments only the assignee
	if err := e.Svc.SetNotificationScheme(ctx, e.Admin, "ABC", map[string][]string{"issue.transitioned": {}, "comment.added": {"assignee"}}); err != nil {
		t.Fatal(err)
	}
	tid2, _ := trans(t, e, e.Alice, is.Key, "Done")
	e.Svc.Transition(ctx, e.Bob, is.Key, tid2, nil)
	e.Svc.AddComment(ctx, e.Bob, is.Key, "assignee comment", false)
	e.Svc.AddComment(ctx, e.Alice, is.Key, "reporter comment", false)
	notify(t, e)
	if got := len(inboxOf(t, e, e.Alice, "")); got != 1 {
		t.Fatalf("scheme: alice should not hear of the second transition or of Bob's comment: %d", got)
	}
	if got := len(inboxOf(t, e, e.Bob, "")); got != base+1 {
		t.Fatalf("scheme: bob should hear Alice's comment as assignee: %d (was %d)", got, base)
	}
	s, _ := e.Svc.NotificationScheme(ctx, e.Alice, "ABC")
	if len(s["issue.transitioned"]) != 0 || strings.Join(s["comment.added"], ",") != "assignee" || strings.Join(s["issue.created"], ",") != "assignee" {
		t.Fatalf("%v", s)
	}
	for _, bad := range []map[string][]string{{"nonsense": {"assignee"}}, {"comment.added": {"everyone"}}} {
		if err := e.Svc.SetNotificationScheme(ctx, e.Admin, "ABC", bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	if err := e.Svc.SetNotificationScheme(ctx, e.Alice, "ABC", map[string][]string{}); err == nil {
		t.Fatal("a member changed the scheme")
	}
}

// SEC: an internal comment never reaches someone who may not see it, and revoking access hides what was already delivered.
func TestCORE10_InboxRespectsPermissions(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	viewer := e.NewUser("vera@example.com", "Vera", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", viewer.ID, "viewer")
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "secret thread"})
	e.Svc.SetWatch(ctx, viewer, is.Key, true)
	e.Svc.SetWatch(ctx, e.Bob, is.Key, true)
	e.Svc.AddComment(ctx, e.Alice, is.Key, "visible to all", false)
	e.Svc.AddComment(ctx, e.Alice, is.Key, "internal only", true)
	notify(t, e)
	var bodies []string
	for _, n := range inboxOf(t, e, viewer, "") {
		bodies = append(bodies, n.Body)
	}
	if len(bodies) != 1 || strings.Contains(strings.Join(bodies, "|"), "internal") {
		t.Fatalf("the viewer's inbox: %v", bodies)
	}
	if len(inboxOf(t, e, e.Bob, "")) != 2 {
		t.Fatal("a member should see both comments")
	}
	// Bob loses access: what was delivered disappears from his inbox and his count, and comes back with the access
	e.Store.Pool.Exec(ctx, `DELETE FROM project_member WHERE user_id::text=$1`, e.Bob.ID)
	if got := len(inboxOf(t, e, e.Bob, "")); got != 0 {
		t.Fatalf("an inbox outlived the access: %d", got)
	}
	if n, _ := e.Svc.UnreadCount(ctx, e.Bob); n != 0 {
		t.Fatalf("unread count after revocation: %d", n)
	}
	e.Svc.AddMember(ctx, e.Admin, "ABC", e.Bob.ID, "member")
	if n, _ := e.Svc.UnreadCount(ctx, e.Bob); n != 2 {
		t.Fatalf("unread count after the access came back: %d", n)
	}
	// a trashed issue's notifications are not shown
	e.Svc.Lifecycle(ctx, e.Admin, is.Key, domain.LifecycleIn{Op: "delete"})
	if got := len(inboxOf(t, e, e.Bob, "")); got != 0 {
		t.Fatalf("notifications of a trashed issue: %d", got)
	}
}

func TestCORE10_InboxStates(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "states", Assignee: e.Bob.ID})
	for i := 0; i < 3; i++ {
		e.Svc.AddComment(ctx, e.Alice, is.Key, fmt.Sprint("c", i), false)
	}
	notify(t, e)
	in := inboxOf(t, e, e.Bob, "unread")
	if len(in) != 4 {
		t.Fatalf("%d", len(in))
	}
	if n, _ := e.Svc.UnreadCount(ctx, e.Bob); n != 4 {
		t.Fatalf("%d", n)
	}
	if err := e.Svc.MarkNotification(ctx, e.Bob, in[0].ID, "read"); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.MarkNotification(ctx, e.Bob, in[1].ID, "archived"); err != nil {
		t.Fatal(err)
	}
	if got := len(inboxOf(t, e, e.Bob, "unread")); got != 2 {
		t.Fatalf("unread %d", got)
	}
	if got := len(inboxOf(t, e, e.Bob, "")); got != 3 { // unread and read, not archived
		t.Fatalf("default %d", got)
	}
	if got := len(inboxOf(t, e, e.Bob, "archived")); got != 1 {
		t.Fatalf("archived %d", got)
	}
	if err := e.Svc.MarkNotification(ctx, e.Bob, in[0].ID, "unread"); err != nil {
		t.Fatal(err)
	}
	// nobody else can touch it, and not even learn it exists
	if err := e.Svc.MarkNotification(ctx, e.Alice, in[0].ID, "read"); err != domain.ErrNotFound {
		t.Fatalf("%v", err)
	}
	if err := e.Svc.MarkNotification(ctx, e.Bob, in[0].ID, "deleted"); err == nil {
		t.Fatal("a bad state was accepted")
	}
	if n, err := e.Svc.MarkAllRead(ctx, e.Bob); err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	if n, _ := e.Svc.UnreadCount(ctx, e.Bob); n != 0 {
		t.Fatalf("%d", n)
	}
	// paging
	p1, _ := e.Svc.Inbox(ctx, e.Bob, domain.InboxQuery{Limit: 2})
	p2, _ := e.Svc.Inbox(ctx, e.Bob, domain.InboxQuery{Limit: 2, Before: p1[1].ID})
	if len(p1) != 2 || len(p2) != 1 || p2[0].ID >= p1[1].ID {
		t.Fatalf("%d %d", len(p1), len(p2))
	}
}

// Email waits for quiet hours to end; the inbox does not.
func TestCORE10_QuietHoursDelayMailNotTheInbox(t *testing.T) {
	e := newMailEnv(t)
	berlin, _ := time.LoadLocation("Europe/Berlin")
	e.Clock.T = time.Date(2026, 10, 19, 23, 30, 0, 0, time.UTC) // 01:30 in Berlin (summer time until 25 October): inside 22:00 to 07:00
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "quiet", Assignee: e.Bob.ID})
	e.Svc.SetNotifyPrefs(ctx, e.Bob, domain.NotifyPrefs{AutoWatch: true, Inbox: true, Email: true, QuietStart: "22:00", QuietEnd: "07:00", Timezone: "Europe/Berlin"})
	e.Svc.AddComment(ctx, e.Alice, is.Key, "late comment", false)
	q, sent := e.flush()
	if q == 0 || sent != 0 {
		t.Fatalf("queued %d, sent %d: mail should wait", q, sent)
	}
	if len(inboxOf(t, e.Env, e.Bob, "")) == 0 {
		t.Fatal("the inbox must not wait")
	}
	var due time.Time
	e.Store.Pool.QueryRow(ctx, `SELECT next_attempt_at FROM mail_message WHERE recipient LIKE '%bob@%' AND kind='event' ORDER BY id DESC LIMIT 1`).Scan(&due)
	want := time.Date(2026, 10, 20, 7, 0, 0, 0, berlin) // 07:00 in Berlin on the 20th
	if !due.Equal(want) {
		t.Fatalf("mail is due %v, want %v", due, want)
	}
	e.Clock.T = want.Add(time.Minute)
	if _, sent := e.flush(); sent == 0 {
		t.Fatal("the mail was not sent after quiet hours")
	}
	// mail off for a person: no mail, but the inbox still works
	e.Svc.SetNotifyPrefs(ctx, e.Bob, domain.NotifyPrefs{AutoWatch: true, Inbox: true, Email: false})
	e.Svc.AddComment(ctx, e.Alice, is.Key, "no mail please", false)
	q, _ = e.flush()
	for _, m := range e.msgTo("bob@example.com") {
		if strings.Contains(string(m.Raw), "no mail please") {
			t.Fatal("mail was sent to someone who turned it off")
		}
	}
	_ = q
}
