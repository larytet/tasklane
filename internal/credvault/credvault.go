// Package credvault holds Jira credentials in the memory of the running process and nowhere else (SPEC 18.1.1, WT-01 to WT-06).
//
// There are two kinds of entry. A service credential belongs to a Jira connection: it is what the mirror's sync reads with, and an
// administrator supplies it after every start ("unlock"). A personal token belongs to one person and one connection: it is what that
// person's edits are written with, and it ends with their session (logout or the idle limit).
//
// The vault never writes, logs or prints a token: a Cred formats as a placeholder in every verb, and encodes as JSON to a placeholder
// too, so an accidental `%v`, `%+v`, `%#v` or Marshal cannot leak one. Removed tokens are overwritten before they are released.
package credvault

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Cred is a credential handed out for one request. It must not be kept: ask the vault again for the next one.
type Cred struct {
	Email     string
	AccountID string // the Jira account the token belongs to, as Jira reported it when the token was checked
	Token     string
}

const redacted = "credvault.Cred{token redacted}"

// Format makes every fmt verb print a placeholder, never the token.
func (c Cred) Format(f fmt.State, _ rune) { fmt.Fprint(f, redacted) }

// String, GoString and MarshalJSON cover the callers that do not go through Format.
func (c Cred) String() string               { return redacted }
func (c Cred) GoString() string             { return redacted }
func (c Cred) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

type entry struct {
	email, accountID string
	tok              []byte
	at, exp          time.Time // when it was stored, and when it ends (zero: no end)
}

func (e *entry) cred() Cred {
	return Cred{Email: e.email, AccountID: e.accountID, Token: string(e.tok)}
}

func (e *entry) wipe() {
	for i := range e.tok {
		e.tok[i] = 0
	}
	e.tok = nil
}

type userKey struct{ user, conn string }

// Vault is safe for concurrent use. The zero value is not usable: call New.
type Vault struct {
	mu  sync.Mutex
	now func() time.Time
	ttl time.Duration
	svc map[string]*entry
	usr map[userKey]*entry
}

// New makes an empty vault. A personal token lasts ttl from the time it was stored (zero: until it is dropped).
func New(now func() time.Time, ttl time.Duration) *Vault {
	if now == nil {
		now = time.Now
	}
	return &Vault{now: now, ttl: ttl, svc: map[string]*entry{}, usr: map[userKey]*entry{}}
}

func newEntry(email, accountID, token string, at, exp time.Time) *entry {
	return &entry{email: strings.TrimSpace(email), accountID: accountID, tok: []byte(token), at: at, exp: exp}
}

// PutService stores (or replaces) the service credential of a connection.
func (v *Vault) PutService(conn, email, accountID, token string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if old := v.svc[conn]; old != nil {
		old.wipe()
	}
	v.svc[conn] = newEntry(email, accountID, token, v.now(), time.Time{})
}

// Service returns the service credential of a connection, or false when the connection is locked.
func (v *Vault) Service(conn string) (Cred, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e := v.svc[conn]
	if e == nil {
		return Cred{}, false
	}
	return e.cred(), true
}

// ServiceSince says when the service credential was stored (false when locked).
func (v *Vault) ServiceSince(conn string) (time.Time, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.svc[conn]; e != nil {
		return e.at, true
	}
	return time.Time{}, false
}

// DropService locks a connection.
func (v *Vault) DropService(conn string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.svc[conn]; e != nil {
		e.wipe()
		delete(v.svc, conn)
	}
}

// PutUser stores (or replaces) one person's token for a connection.
func (v *Vault) PutUser(user, conn, email, accountID, token string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := userKey{user, conn}
	if old := v.usr[k]; old != nil {
		old.wipe()
	}
	var exp time.Time
	if v.ttl > 0 {
		exp = v.now().Add(v.ttl)
	}
	v.usr[k] = newEntry(email, accountID, token, v.now(), exp)
}

// User returns a person's token for a connection, or false when there is none or it has ended.
func (v *Vault) User(user, conn string) (Cred, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := userKey{user, conn}
	e := v.usr[k]
	if e == nil {
		return Cred{}, false
	}
	if !e.exp.IsZero() && !v.now().Before(e.exp) {
		e.wipe()
		delete(v.usr, k)
		return Cred{}, false
	}
	return e.cred(), true
}

// DropUser forgets every token of one person (logout, session end) and reports how many it removed.
func (v *Vault) DropUser(user string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for k, e := range v.usr {
		if k.user == user {
			e.wipe()
			delete(v.usr, k)
			n++
		}
	}
	return n
}

// DropUserConn forgets one person's token for one connection (Jira refused it).
func (v *Vault) DropUserConn(user, conn string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := userKey{user, conn}
	if e := v.usr[k]; e != nil {
		e.wipe()
		delete(v.usr, k)
	}
}

// DropConn forgets everything held for a connection: its service credential and every person's token.
func (v *Vault) DropConn(conn string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.svc[conn]; e != nil {
		e.wipe()
		delete(v.svc, conn)
	}
	for k, e := range v.usr {
		if k.conn == conn {
			e.wipe()
			delete(v.usr, k)
		}
	}
}

// DropUsers forgets every person's token for a connection and keeps its service credential (a project left write-through).
func (v *Vault) DropUsers(conn string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for k, e := range v.usr {
		if k.conn == conn {
			e.wipe()
			delete(v.usr, k)
		}
	}
}

// Users counts the live personal tokens held for a connection. The Admin tab shows the count and never who holds one.
func (v *Vault) Users(conn string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for k, e := range v.usr {
		if k.conn == conn && (e.exp.IsZero() || v.now().Before(e.exp)) {
			n++
		}
	}
	return n
}

// Sweep removes personal tokens whose time has passed.
func (v *Vault) Sweep() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for k, e := range v.usr {
		if !e.exp.IsZero() && !v.now().Before(e.exp) {
			e.wipe()
			delete(v.usr, k)
		}
	}
}
