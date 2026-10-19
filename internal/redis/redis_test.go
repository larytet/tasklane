package redis_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/redis"
	"github.com/larytet/tasklane/internal/testutil/redismock"
)

func wait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type got struct {
	mu sync.Mutex
	v  []string
}

func (g *got) add(b []byte) { g.mu.Lock(); g.v = append(g.v, string(b)); g.mu.Unlock() }
func (g *got) n() int       { g.mu.Lock(); defer g.mu.Unlock(); return len(g.v) }

func node(t *testing.T, srv *redismock.Server, user string, g *got) (*redis.Bus, context.CancelFunc) {
	o, err := redis.ParseURL("redis://" + user + ":pw@" + srv.Addr() + "/0")
	if err != nil {
		t.Fatal(err)
	}
	b := redis.New(o)
	b.Subscribe("events", g.add)
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	t.Cleanup(cancel)
	return b, cancel
}

func TestParseURL(t *testing.T) {
	o, err := redis.ParseURL("rediss://u:p%40ss@cache.example:6380/2")
	if err != nil || o.Addr != "cache.example:6380" || o.User != "u" || o.Password != "p@ss" || o.DB != 2 || o.TLS == nil {
		t.Fatalf("%+v %v", o, err)
	}
	if o, _ := redis.ParseURL("redis://cache.example"); o.Addr != "cache.example:6379" || o.TLS != nil {
		t.Fatalf("%+v", o)
	}
	for _, bad := range []string{"", "http://x", "redis://", "redis://h/x", "not a url"} {
		if _, err := redis.ParseURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPublishReachesEveryNodeAndAuthIsChecked(t *testing.T) {
	srv := redismock.New(t)
	srv.Password = "pw"
	var ga, gb got
	a, _ := node(t, srv, "a", &ga)
	node(t, srv, "b", &gb)
	wait(t, "both subscribed", func() bool { return srv.Subscribers("events") == 2 && a.Status().Connected })
	a.Publish("events", []byte("hello"))
	wait(t, "delivery", func() bool { return ga.n() == 1 && gb.n() == 1 })
	if st := a.Status(); !st.Connected || st.Published != 1 || st.Received != 1 {
		t.Fatalf("%+v", st)
	}
	// a wrong password never connects and says why
	o, _ := redis.ParseURL("redis://x:wrong@" + srv.Addr())
	bad := redis.New(o)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bad.Run(ctx)
	wait(t, "an error", func() bool { return bad.Status().LastError != "" })
	if bad.Status().Connected {
		t.Fatal("connected with a wrong password")
	}
	if _, err := redis.Ping(context.Background(), o); err == nil {
		t.Fatal("ping with a wrong password")
	}
	o2, _ := redis.ParseURL("redis://x:pw@" + srv.Addr())
	if v, err := redis.Ping(context.Background(), o2); err != nil || v != "Redis 7.0.0-mock" {
		t.Fatalf("%q %v", v, err)
	}
}

func TestRedisGoesAwayAndComesBack(t *testing.T) {
	srv := redismock.New(t)
	var ga, gb got
	a, _ := node(t, srv, "a", &ga)
	node(t, srv, "b", &gb)
	wait(t, "up", func() bool { return a.Status().Connected && srv.Subscribers("events") == 2 })
	srv.Stop()
	wait(t, "noticed", func() bool { return !a.Status().Connected })
	// publishing while it is down never blocks and is counted
	start := time.Now()
	for i := 0; i < 50; i++ {
		a.Publish("events", []byte("lost"))
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("publishing blocked while Redis was down")
	}
	srv.Start()
	wait(t, "reconnected", func() bool { return a.Status().Connected && srv.Subscribers("events") == 2 })
	if d := a.Status().Dropped; d == 0 {
		t.Fatal("messages sent into a dead link were not counted as dropped")
	}
	before := ga.n()
	a.Publish("events", []byte("after"))
	wait(t, "events resume", func() bool { return ga.n() == before+1 && gb.n() >= 1 })
	for _, v := range ga.v {
		if v == "lost" {
			t.Fatal("a message sent while the link was down arrived late")
		}
	}
}

func TestOneNodeCutOffFromRedis(t *testing.T) {
	srv := redismock.New(t)
	var ga, gb got
	a, _ := node(t, srv, "a", &ga)
	b, _ := node(t, srv, "b", &gb)
	wait(t, "up", func() bool { return a.Status().Connected && b.Status().Connected && srv.Subscribers("events") == 2 })
	srv.Cut("b")
	wait(t, "b noticed", func() bool { return !b.Status().Connected })
	if !a.Status().Connected {
		t.Fatal("a node that was not cut lost its link")
	}
	a.Publish("events", []byte("one"))
	wait(t, "a still hears itself", func() bool { return ga.n() == 1 })
	time.Sleep(100 * time.Millisecond)
	if gb.n() != 0 {
		t.Fatal("the cut node received a message")
	}
	srv.Heal("b")
	wait(t, "b back", func() bool { return b.Status().Connected && srv.Subscribers("events") == 2 })
	a.Publish("events", []byte("two"))
	wait(t, "b receives again", func() bool { return gb.n() == 1 })
}

func TestPingNamesValkeyByItsOwnVersion(t *testing.T) {
	srv := redismock.New(t)
	srv.InfoExtra = "server_name:valkey\r\nvalkey_version:8.0.1\r\n"
	o, _ := redis.ParseURL("redis://" + srv.Addr())
	if v, err := redis.Ping(context.Background(), o); err != nil || v != "Valkey 8.0.1" {
		t.Fatalf("%q %v", v, err)
	}
}
