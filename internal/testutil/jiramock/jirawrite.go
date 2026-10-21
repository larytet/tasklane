package jiramock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The write half of the recorded Jira double (SPEC 18.1.1): several people with their own tokens, the edit, comment and transition
// endpoints, and ways to make Jira refuse, slow down, or change under the client's feet. It is written from what Jira Cloud documents;
// it is not a claim about what a real site does (the real-site runs, JREAL, settle that).

// Account is one person who can authenticate with their own email and token.
type Account struct {
	Email, Token, AccountID, Name string
	CannotEdit                    bool // Jira answers 403 to this person's writes
}

// Trans is a workflow move: from a status name ("" for any) to another.
type Trans struct{ ID, Name, From, To string }

// Write is one write the double received, with the identity that made it.
type Write struct {
	Method, Path, Email string
	Body                string
}

type injected struct {
	match  string
	status int
	body   string
	retry  string
	n      int
}

// AddAccount lets another person sign in with their own token.
func (s *Server) AddAccount(email, token, accountID, name string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Accounts == nil {
		s.Accounts = map[string]*Account{}
	}
	a := &Account{Email: email, Token: token, AccountID: accountID, Name: name}
	s.Accounts[email] = a
	return a
}

// identity resolves the basic-authentication pair to a person, or false for a wrong pair.
func (s *Server) identity(email, token string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if email == s.Email && token == s.Token {
		return Account{Email: s.Email, Token: s.Token, AccountID: "acc-sync", Name: "Sync Bot"}, true
	}
	if a, ok := s.Accounts[email]; ok && a.Token == token {
		return *a, true
	}
	return Account{}, false
}

// FailNext makes the next n writes whose "METHOD /path" contains match answer with status and body (and Retry-After when set).
func (s *Server) FailNext(match string, status int, body string, n int) {
	s.mu.Lock()
	s.fail = append(s.fail, injected{match: match, status: status, body: body, n: n})
	s.mu.Unlock()
}

// FailRate makes the next write answer 429 with Retry-After.
func (s *Server) FailRate(match string, retryAfter int) {
	s.mu.Lock()
	s.fail = append(s.fail, injected{match: match, status: 429, retry: strconv.Itoa(retryAfter), n: 1})
	s.mu.Unlock()
}

// SlowWrites delays every write by d.
func (s *Server) SlowWrites(d time.Duration) { s.mu.Lock(); s.slowWrite = d; s.mu.Unlock() }

// Writes returns what the double has been asked to write, oldest first.
func (s *Server) Writes() []Write {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Write{}, s.writes...)
}

// ByKey returns the issue with that key (nil when there is none). The caller must not change it while requests are running.
func (s *Server) ByKey(key string) *Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byKeyOrID(key)
}

// Field returns a field's current value.
func (s *Server) Field(key, field string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is := s.byKeyOrID(key); is != nil {
		return is.Fields[field]
	}
	return nil
}

// Change sets a field the way another person editing in Jira would: the value, a new `updated`, and a history entry in their name.
func (s *Server) Change(key, field string, value any, by Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is := s.byKeyOrID(key); is != nil {
		s.apply(is, field, value, by)
	}
}

var reKeyPath = regexp.MustCompile(`^/rest/api/3/issue/([^/]+)(/comment|/transitions)?(/[^/]+)?$`)

func (s *Server) byKeyOrID(k string) *Issue {
	for _, x := range s.Issues {
		if x.Key == k || x.ID == k {
			return x
		}
	}
	return nil
}

func (s *Server) person(a Account) M {
	return M{"accountId": a.AccountID, "displayName": a.Name, "emailAddress": a.Email}
}

func (s *Server) nextUpdated(is *Issue) string {
	t := s.Now().UTC()
	if prev := parseJiraTime(is.Fields["updated"]); !t.After(prev) {
		t = prev.Add(1500 * time.Millisecond)
	}
	return t.Format("2006-01-02T15:04:05.000-0700")
}

func shown(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case M:
		if n, ok := x["name"].(string); ok {
			return n
		}
		if n, ok := x["displayName"].(string); ok {
			return n
		}
		return fmt.Sprint(x["accountId"])
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = fmt.Sprint(e)
		}
		return strings.Join(parts, " ")
	}
	return fmt.Sprint(v)
}

// apply sets one field and records the change. The caller holds the lock.
func (s *Server) apply(is *Issue, field string, value any, by Account) {
	before := is.Fields[field]
	is.Fields[field] = value
	now := s.nextUpdated(is)
	is.Fields["updated"] = now
	is.History = append([]M{{
		"id": strconv.Itoa(1000 + len(is.History)), "author": s.person(by), "created": now,
		"items": []any{M{"field": field, "fromString": shown(before), "toString": shown(value)}},
	}}, is.History...)
}

func (s *Server) availableTransitions(is *Issue) []M {
	cur := shown(is.Fields["status"])
	out := []M{}
	for _, t := range s.Transitions {
		if t.From == "" || strings.EqualFold(t.From, cur) {
			out = append(out, M{"id": t.ID, "name": t.Name, "to": M{"name": t.To}})
		}
	}
	return out
}

func category(status string) string {
	switch strings.ToLower(status) {
	case "done", "closed", "resolved":
		return "done"
	case "to do", "open", "backlog":
		return "new"
	}
	return "indeterminate"
}

// issueItem is one issue as GET /issue/{key} shows it: the requested fields, and the history when asked for.
func (s *Server) issueItem(is *Issue, fieldsCSV, expand string) M {
	want := map[string]bool{}
	for _, f := range strings.Split(fieldsCSV, ",") {
		want[f] = true
	}
	fields := M{}
	for k, v := range is.Fields {
		if want["*all"] || want[k] {
			fields[k] = v
		}
	}
	if want["comment"] {
		fields["comment"] = M{"comments": is.Comments, "total": len(is.Comments)}
	}
	item := M{"id": is.ID, "key": is.Key, "fields": fields}
	if strings.Contains(expand, "changelog") {
		item["changelog"] = M{"histories": is.History, "total": len(is.History)}
	}
	return item
}

func jerr(w http.ResponseWriter, status int, msgs []string, fields map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(M{"errorMessages": msgs, "errors": fields})
}

// write answers PUT and POST.
func (s *Server) write(w http.ResponseWriter, r *http.Request, who Account) {
	body, _ := io.ReadAll(r.Body)
	label := r.Method + " " + r.URL.Path
	s.mu.Lock()
	s.writes = append(s.writes, Write{Method: r.Method, Path: r.URL.Path, Email: who.Email, Body: string(body)})
	slow, hook := s.slowWrite, s.OnWrite
	var inj *injected
	for i := range s.fail {
		if s.fail[i].n > 0 && strings.Contains(label, s.fail[i].match) {
			s.fail[i].n--
			c := s.fail[i]
			inj = &c
			break
		}
	}
	s.mu.Unlock()
	if slow > 0 {
		time.Sleep(slow)
	}
	if inj != nil {
		if inj.retry != "" {
			w.Header().Set("Retry-After", inj.retry)
		}
		w.WriteHeader(inj.status)
		io.WriteString(w, inj.body)
		return
	}
	if hook != nil {
		hook(r.Method, r.URL.Path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if who.CannotEdit {
		jerr(w, 403, []string{"You do not have permission to edit issues in this project."}, nil)
		return
	}
	m := reKeyPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		w.WriteHeader(404)
		return
	}
	is := s.byKeyOrID(m[1])
	if is == nil {
		jerr(w, 404, []string{"Issue does not exist or you do not have permission to see it."}, nil)
		return
	}
	var in struct {
		Fields     M   `json:"fields"`
		Body       any `json:"body"`
		Transition M   `json:"transition"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		jerr(w, 400, []string{"invalid JSON"}, nil)
		return
	}
	switch {
	case m[2] == "" && r.Method == "PUT":
		if v, ok := in.Fields["summary"]; ok && strings.TrimSpace(shown(v)) == "" {
			jerr(w, 400, nil, map[string]string{"summary": "You must specify a summary of the issue."})
			return
		}
		for k, v := range in.Fields {
			if k == "assignee" && v != nil {
				if id := shown(M{"accountId": v.(map[string]any)["accountId"]}); id != "" {
					name := id
					if u, ok := s.Users[id]; ok {
						name = shown(u["displayName"])
					}
					v = M{"accountId": id, "displayName": name}
				}
			}
			s.apply(is, k, v, who)
		}
		w.WriteHeader(204)
	case m[2] == "/comment" && r.Method == "POST":
		now := s.nextUpdated(is)
		c := M{"id": strconv.Itoa(20000 + len(is.Comments)), "author": s.person(who), "body": in.Body, "created": now, "updated": now}
		is.Comments = append(is.Comments, c)
		is.Fields["updated"] = now
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(c)
	case m[2] == "/transitions" && r.Method == "POST":
		id := shown(in.Transition["id"])
		for _, t := range s.availableTransitions(is) {
			if t["id"] == id {
				to := shown(t["to"])
				s.apply(is, "status", M{"name": to, "statusCategory": M{"key": category(to)}}, who)
				w.WriteHeader(204)
				return
			}
		}
		jerr(w, 400, []string{fmt.Sprintf("Transition id '%s' is not valid for this issue.", id)}, nil)
	default:
		w.WriteHeader(405)
	}
}
