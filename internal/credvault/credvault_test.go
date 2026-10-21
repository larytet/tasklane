package credvault

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const secretTok = "ATATT3xFfGF0-very-secret-token-value"

func TestServiceLockedUntilPut(t *testing.T) {
	v := New(nil, 0)
	if _, ok := v.Service("c1"); ok {
		t.Fatal("a connection with nothing stored must be locked")
	}
	v.PutService("c1", "sync@example.com", "acc-1", secretTok)
	c, ok := v.Service("c1")
	if !ok || c.Token != secretTok || c.Email != "sync@example.com" || c.AccountID != "acc-1" {
		t.Fatalf("got %v %v", c.AccountID, ok)
	}
	if _, ok := v.Service("c2"); ok {
		t.Fatal("another connection must stay locked")
	}
	v.DropService("c1")
	if _, ok := v.Service("c1"); ok {
		t.Fatal("a dropped credential must be gone")
	}
}

func TestPersonalTokensAreSeparatePerPersonAndExpire(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	v := New(clock, time.Hour)
	v.PutUser("u1", "c1", "a@example.com", "acc-a", "tok-a")
	v.PutUser("u2", "c1", "b@example.com", "acc-b", "tok-b")
	if c, ok := v.User("u1", "c1"); !ok || c.Token != "tok-a" {
		t.Fatalf("u1: %v", ok)
	}
	if c, ok := v.User("u2", "c1"); !ok || c.Token != "tok-b" {
		t.Fatalf("u2: %v", ok)
	}
	if _, ok := v.User("u1", "c2"); ok {
		t.Fatal("a token is for one connection only")
	}
	if n := v.Users("c1"); n != 2 {
		t.Fatalf("count %d", n)
	}
	now = now.Add(61 * time.Minute)
	if _, ok := v.User("u1", "c1"); ok {
		t.Fatal("a token past its time must not be handed out")
	}
	if n := v.Users("c1"); n != 0 {
		t.Fatalf("count after expiry %d", n)
	}
}

func TestDropPaths(t *testing.T) {
	v := New(nil, 0)
	v.PutService("c1", "s@example.com", "acc-s", "svc")
	v.PutUser("u1", "c1", "a@example.com", "acc-a", "a")
	v.PutUser("u1", "c2", "a@example.com", "acc-a", "a2")
	v.PutUser("u2", "c1", "b@example.com", "acc-b", "b")
	if n := v.DropUser("u1"); n != 2 {
		t.Fatalf("dropped %d", n)
	}
	if _, ok := v.User("u1", "c1"); ok {
		t.Fatal("logout must remove the person's tokens")
	}
	if _, ok := v.User("u2", "c1"); !ok {
		t.Fatal("logout of one person must not touch another")
	}
	v.DropConn("c1")
	if _, ok := v.Service("c1"); ok {
		t.Fatal("DropConn must lock the connection")
	}
	if _, ok := v.User("u2", "c1"); ok {
		t.Fatal("DropConn must remove every person's token for it")
	}
}

func TestDroppedTokenIsOverwritten(t *testing.T) {
	v := New(nil, 0)
	v.PutService("c1", "s@example.com", "acc", secretTok)
	e := v.svc["c1"]
	buf := e.tok
	v.DropService("c1")
	for _, b := range buf {
		if b != 0 {
			t.Fatal("the dropped token's bytes were not overwritten")
		}
	}
}

// A token must not reach a log line, an error message or an API answer by accident.
func TestCredNeverPrintsTheToken(t *testing.T) {
	c := Cred{Email: "a@example.com", AccountID: "acc", Token: secretTok}
	for _, s := range []string{
		fmt.Sprint(c), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), fmt.Sprintf("%s", c), fmt.Sprintf("%q", c),
		fmt.Sprintf("%v", &c), fmt.Sprintf("%+v", []Cred{c}), fmt.Sprintf("%v", map[string]Cred{"k": c}),
		fmt.Sprintf("%+v", struct{ C Cred }{c}),
	} {
		if strings.Contains(s, secretTok) || strings.Contains(s, "very-secret") {
			t.Fatalf("the token was printed: %s", s)
		}
	}
	b, err := json.Marshal(map[string]any{"c": c, "list": []Cred{c}})
	if err != nil || strings.Contains(string(b), "very-secret") {
		t.Fatalf("the token was encoded: %s %v", b, err)
	}
}

func TestReplaceWipesTheOldToken(t *testing.T) {
	v := New(nil, 0)
	v.PutUser("u1", "c1", "a@example.com", "acc", "first")
	old := v.usr[userKey{"u1", "c1"}].tok
	v.PutUser("u1", "c1", "a@example.com", "acc", "second")
	for _, b := range old {
		if b != 0 {
			t.Fatal("a replaced token was not overwritten")
		}
	}
	if c, _ := v.User("u1", "c1"); c.Token != "second" {
		t.Fatal("the new token must be the one handed out")
	}
}
