package domain_test

import (
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// UPG-03 (cache invalidation): what a node remembers about an organization's security policy follows a change made on another node.
// Over the bus at once; without it (bus down, or the other node an older release) within the cache time-out.
func TestUPG03_AnotherNodesPolicyChangeIsHonoredAtOnce(t *testing.T) {
	a, b, _, e, _ := busCluster(t)
	root := domain.Actor{ID: e.Alice.ID, Label: "root", Admin: true}
	const outside = "198.51.100.7"
	if err := b.svc.EnforceNetwork(ctx, e.Alice, outside); err != nil { // b now remembers "no restriction"
		t.Fatalf("before any list: %v", err)
	}
	if err := a.svc.SetSecurityPolicy(ctx, root, "", domain.SecurityPolicy{IPAllowlist: []string{"203.0.113.0/24"}}, ""); err != nil {
		t.Fatal(err)
	}
	withinBus(t, 2*time.Second, "b refuses an address outside the new list", func() bool { return b.svc.EnforceNetwork(ctx, e.Alice, outside) != nil })
	// and the list taken away again
	if err := a.svc.SetSecurityPolicy(ctx, root, "", domain.SecurityPolicy{}, ""); err != nil {
		t.Fatal(err)
	}
	withinBus(t, 2*time.Second, "b admits it again", func() bool { return b.svc.EnforceNetwork(ctx, e.Alice, outside) == nil })
}

func TestUPG03_WithoutTheBusThePolicyCacheTimeOutIsTheBound(t *testing.T) {
	e := testutil.New(t)
	a, b := domain.New(e.Store.Pool), domain.New(e.Store.Pool)
	var off time.Duration
	b.Clock = func() time.Time { return time.Now().Add(off) }
	root := domain.Actor{ID: e.Alice.ID, Label: "root", Admin: true}
	const outside = "198.51.100.7"
	if err := b.EnforceNetwork(ctx, e.Alice, outside); err != nil { // b remembers "no list"
		t.Fatal(err)
	}
	if err := a.SetSecurityPolicy(ctx, root, "", domain.SecurityPolicy{IPAllowlist: []string{"203.0.113.0/24"}}, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.EnforceNetwork(ctx, e.Alice, outside); err != nil {
		t.Fatalf("with no way to hear of it b may still use its copy, but it refused at once: %v", err)
	}
	off = 6 * time.Second
	if err := b.EnforceNetwork(ctx, e.Alice, outside); err == nil {
		t.Fatal("six seconds later b still admits an address the list refuses")
	}
}

// A message kind this node does not know is ignored: an older release beside a newer one must not break on it.
func TestUPG03_UnknownBusMessagesAreIgnored(t *testing.T) {
	a, b, _, e, key := busCluster(t)
	a.bus.Publish("tasklane:cluster", []byte(`{"node":"future","kind":"quantum","payload":{"x":1}}`))
	a.bus.Publish("tasklane:cluster", []byte(`{"node":"future","kind":"forget","forget":{"org":"","user":"","galaxy":"far"}}`))
	comment(t, a, e, key, "still working")
	waitFor(t, "b hears the change after the strange messages", func() bool { return b.seen("comment.added") == 1 })
}

// within is waitFor with a bound shorter than the cache time-out, so that only the bus can meet it.
func withinBus(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("%s: not within %s", what, d)
}
