// Package osmock is the in-process OpenSearch test double (TESTPLAN 3.2): indexes and aliases, bulk writes with external
// versioning and tombstones, strict mappings, and the subset of the query DSL that Tasklane generates (bool, term, terms,
// range, exists, match, prefix, wildcard), with sorting, search_after and totals. It is deliberately strict, so a query or a
// document the real engine would refuse fails here too; the same checks run against a real cluster in the compose profile.
package osmock

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type doc struct {
	src     map[string]any
	version int64
}

type index struct {
	docs       map[string]*doc
	tombstones map[string]int64
	fields     map[string]bool
}

type fault struct {
	remaining int
	status    int
	match     func(*http.Request) bool
}

type Server struct {
	*httptest.Server
	mu      sync.Mutex
	indexes map[string]*index
	aliases map[string]map[string]bool
	faults  []*fault
	reqs    []string
	User    string
	Pass    string
	Health  string // cluster status reported, default green
	// Reject, when set, makes index writes for ids it returns true for fail with a mapping error (a poison document).
	Reject func(id string) bool
}

func New() *Server {
	s := &Server{indexes: map[string]*index{}, aliases: map[string]map[string]bool{}, Health: "green"}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Fail makes the next n matching requests answer with the status (nil matches all): 503 simulates an outage.
func (s *Server) Fail(n, status int, match func(*http.Request) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, &fault{n, status, match})
}

func (s *Server) ClearFaults() { s.mu.Lock(); s.faults = nil; s.mu.Unlock() }

func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

func (s *Server) Count(method, contains string) int {
	n := 0
	for _, r := range s.Requests() {
		if strings.HasPrefix(r, method+" ") && strings.Contains(r, contains) {
			n++
		}
	}
	return n
}

// Indexes lists index names; Docs returns a copy of the sources in an index (or the one an alias points at).
func (s *Server) Indexes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n []string
	for k := range s.indexes {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func (s *Server) AliasTarget(alias string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.aliases[alias] {
		return k
	}
	return ""
}

func (s *Server) Docs(name string) map[string]map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]any{}
	ix := s.indexes[s.resolve(name)[0]]
	if ix == nil {
		return out
	}
	for id, d := range ix.docs {
		c := map[string]any{}
		for k, v := range d.src {
			c[k] = v
		}
		out[id] = c
	}
	return out
}

// Put writes a document behind the application's back (to simulate drift); Remove deletes one.
func (s *Server) Put(name, id string, src map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ix := s.indexes[s.resolve(name)[0]]; ix != nil {
		v := int64(1)
		if d := ix.docs[id]; d != nil {
			v = d.version + 1
		}
		ix.docs[id] = &doc{src: src, version: v}
	}
}

func (s *Server) Remove(name, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ix := s.indexes[s.resolve(name)[0]]; ix != nil {
		delete(ix.docs, id)
	}
}

func (s *Server) resolve(name string) []string {
	if t, ok := s.aliases[name]; ok {
		var out []string
		for k := range t {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	return []string{name}
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}

func osErr(w http.ResponseWriter, status int, typ, reason string) {
	reply(w, status, map[string]any{"error": map[string]any{"type": typ, "reason": reason}, "status": status})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Method+" "+r.URL.RequestURI())
	for _, f := range s.faults {
		if f.remaining > 0 && (f.match == nil || f.match(r)) {
			f.remaining--
			s.mu.Unlock()
			io.Copy(io.Discard, r.Body)
			osErr(w, f.status, "injected", "injected failure")
			return
		}
	}
	s.mu.Unlock()
	if s.User != "" {
		if u, p, ok := r.BasicAuth(); !ok || u != s.User || p != s.Pass {
			osErr(w, 401, "security_exception", "unable to authenticate")
			return
		}
	}
	body, _ := io.ReadAll(r.Body)
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case path == "" && r.Method == "GET":
		reply(w, 200, map[string]any{"version": map[string]any{"number": "2.11.0", "distribution": "opensearch"}})
	case path == "_cluster/health":
		reply(w, 200, map[string]any{"status": s.Health})
	case path == "_aliases" && r.Method == "POST":
		s.aliasesDo(w, body)
	case parts[0] == "_alias" && len(parts) == 2 && r.Method == "GET":
		var out = map[string]any{}
		for idx := range s.aliases[parts[1]] {
			out[idx] = map[string]any{"aliases": map[string]any{parts[1]: map[string]any{}}}
		}
		if len(out) == 0 {
			osErr(w, 404, "aliases_not_found_exception", "alias ["+parts[1]+"] missing")
			return
		}
		reply(w, 200, out)
	case path == "_bulk" && r.Method == "POST":
		s.bulk(w, body)
	case len(parts) == 1 && r.Method == "HEAD":
		if _, ok := s.indexes[parts[0]]; !ok {
			if _, isAlias := s.aliases[parts[0]]; !isAlias {
				w.WriteHeader(404)
				return
			}
		}
		w.WriteHeader(200)
	case len(parts) == 1 && r.Method == "PUT":
		s.create(w, parts[0], body)
	case len(parts) == 1 && r.Method == "DELETE":
		if _, ok := s.indexes[parts[0]]; !ok {
			osErr(w, 404, "index_not_found_exception", "no such index ["+parts[0]+"]")
			return
		}
		delete(s.indexes, parts[0])
		for a := range s.aliases {
			delete(s.aliases[a], parts[0])
		}
		reply(w, 200, map[string]any{"acknowledged": true})
	case len(parts) == 2 && parts[1] == "_search":
		s.search(w, parts[0], body)
	case len(parts) == 2 && parts[1] == "_count":
		var req map[string]any
		json.Unmarshal(body, &req)
		n := 0
		for _, ixn := range s.resolve(parts[0]) {
			if ix := s.indexes[ixn]; ix != nil {
				for _, d := range ix.docs {
					if ok, _ := evalQuery(req["query"], d.src); ok {
						n++
					}
				}
			}
		}
		reply(w, 200, map[string]any{"count": n})
	case len(parts) == 2 && parts[1] == "_delete_by_query":
		var req map[string]any
		json.Unmarshal(body, &req)
		n := 0
		for _, ixn := range s.resolve(parts[0]) {
			if ix := s.indexes[ixn]; ix != nil {
				for id, d := range ix.docs {
					if ok, _ := evalQuery(req["query"], d.src); ok {
						delete(ix.docs, id)
						n++
					}
				}
			}
		}
		reply(w, 200, map[string]any{"deleted": n})
	case len(parts) == 2 && parts[1] == "_refresh":
		reply(w, 200, map[string]any{"_shards": map[string]any{"failed": 0}})
	default:
		osErr(w, 400, "illegal_argument_exception", "not supported by the test double: "+r.Method+" /"+path)
	}
}

func (s *Server) aliasesDo(w http.ResponseWriter, body []byte) {
	var req struct {
		Actions []map[string]map[string]string `json:"actions"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		osErr(w, 400, "parse_exception", err.Error())
		return
	}
	// atomic: validate everything first, then apply
	for _, a := range req.Actions {
		for _, spec := range a {
			if _, ok := s.indexes[spec["index"]]; !ok {
				osErr(w, 404, "index_not_found_exception", "no such index ["+spec["index"]+"]")
				return
			}
		}
	}
	for _, a := range req.Actions {
		for verb, spec := range a {
			if s.aliases[spec["alias"]] == nil {
				s.aliases[spec["alias"]] = map[string]bool{}
			}
			if verb == "add" {
				s.aliases[spec["alias"]][spec["index"]] = true
			} else {
				delete(s.aliases[spec["alias"]], spec["index"])
			}
		}
	}
	reply(w, 200, map[string]any{"acknowledged": true})
}

func (s *Server) create(w http.ResponseWriter, name string, body []byte) {
	if _, ok := s.indexes[name]; ok {
		osErr(w, 400, "resource_already_exists_exception", "index ["+name+"] already exists")
		return
	}
	var req struct {
		Mappings struct {
			Dynamic    any            `json:"dynamic"`
			Properties map[string]any `json:"properties"`
		} `json:"mappings"`
	}
	json.Unmarshal(body, &req)
	ix := &index{docs: map[string]*doc{}, tombstones: map[string]int64{}, fields: map[string]bool{}}
	for f := range req.Mappings.Properties {
		ix.fields[f] = true
	}
	s.indexes[name] = ix
	reply(w, 200, map[string]any{"acknowledged": true, "index": name})
}

func (s *Server) bulk(w http.ResponseWriter, body []byte) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var items []any
	errs := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var meta map[string]map[string]any
		if err := json.Unmarshal(line, &meta); err != nil || len(meta) != 1 {
			osErr(w, 400, "illegal_argument_exception", "malformed bulk action")
			return
		}
		for verb, m := range meta {
			idxName, _ := m["_index"].(string)
			id, _ := m["_id"].(string)
			ver, _ := m["version"].(float64)
			vt, _ := m["version_type"].(string)
			var src map[string]any
			if verb == "index" {
				if !sc.Scan() {
					osErr(w, 400, "illegal_argument_exception", "missing source")
					return
				}
				json.Unmarshal(sc.Bytes(), &src)
			}
			ixs := s.resolve(idxName)
			ix := s.indexes[ixs[0]]
			item := map[string]any{"_index": ixs[0], "_id": id}
			switch {
			case ix == nil:
				item["status"], item["error"] = 404, map[string]any{"type": "index_not_found_exception", "reason": "no such index"}
				errs = true
			case vt != "external" && vt != "external_gte":
				item["status"], item["error"] = 400, map[string]any{"type": "illegal_argument_exception", "reason": "this double requires external versioning"}
				errs = true
			default:
				cur := int64(-1)
				if d := ix.docs[id]; d != nil {
					cur = d.version
				}
				if t, ok := ix.tombstones[id]; ok && t > cur {
					cur = t
				}
				if int64(ver) < cur || (int64(ver) == cur && vt != "external_gte") {
					item["status"], item["error"] = 409, map[string]any{"type": "version_conflict_engine_exception", "reason": fmt.Sprintf("current version [%d] is higher or equal to the one provided [%d]", cur, int64(ver))}
					break
				}
				if verb == "index" && s.Reject != nil && s.Reject(id) {
					item["status"], item["error"] = 400, map[string]any{"type": "mapper_parsing_exception", "reason": "failed to parse (injected)"}
					errs = true
					break
				}
				if verb == "index" {
					bad := ""
					for f := range src {
						if !ix.fields[f] {
							bad = f
						}
					}
					if bad != "" {
						item["status"], item["error"] = 400, map[string]any{"type": "strict_dynamic_mapping_exception", "reason": "mapping set to strict, dynamic introduction of [" + bad + "] is not allowed"}
						errs = true
						break
					}
					_, existed := ix.docs[id]
					ix.docs[id] = &doc{src: src, version: int64(ver)}
					delete(ix.tombstones, id)
					item["status"], item["result"] = 201, "created"
					if existed {
						item["status"], item["result"] = 200, "updated"
					}
				} else {
					_, existed := ix.docs[id]
					delete(ix.docs, id)
					ix.tombstones[id] = int64(ver)
					item["status"], item["result"] = 200, "deleted"
					if !existed {
						item["status"], item["result"] = 404, "not_found"
					}
				}
			}
			items = append(items, map[string]any{verb: item})
		}
	}
	reply(w, 200, map[string]any{"errors": errs, "items": items})
}

type hit struct {
	id   string
	src  map[string]any
	keys []any
}

func (s *Server) search(w http.ResponseWriter, name string, body []byte) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		osErr(w, 400, "parse_exception", err.Error())
		return
	}
	var hits []hit
	for _, ixn := range s.resolve(name) {
		ix := s.indexes[ixn]
		if ix == nil {
			osErr(w, 404, "index_not_found_exception", "no such index ["+name+"]")
			return
		}
		for id, d := range ix.docs {
			ok, err := evalQuery(req["query"], d.src)
			if err != nil {
				osErr(w, 400, "parsing_exception", err.Error())
				return
			}
			if ok {
				hits = append(hits, hit{id: id, src: d.src})
			}
		}
	}
	sorts, _ := req["sort"].([]any)
	less := func(a, b hit) bool {
		for _, so := range sorts {
			var field, order, missing string = "", "asc", "_last"
			switch x := so.(type) {
			case string:
				field = x
			case map[string]any:
				for f, v := range x {
					field = f
					if m, ok := v.(map[string]any); ok {
						if o, ok := m["order"].(string); ok {
							order = o
						}
						if o, ok := m["missing"].(string); ok {
							missing = o
						}
					} else if o, ok := v.(string); ok {
						order = o
					}
				}
			}
			if field == "_score" {
				continue
			}
			av, bv := fieldVal(a, field), fieldVal(b, field)
			if av == nil || bv == nil {
				if av == nil && bv == nil {
					continue
				}
				if missing == "_first" {
					return av == nil
				}
				return bv == nil
			}
			c := cmpVals(av, bv)
			if c == 0 {
				continue
			}
			if order == "desc" {
				return c > 0
			}
			return c < 0
		}
		return a.id < b.id
	}
	sort.Slice(hits, func(i, j int) bool { return less(hits[i], hits[j]) })
	for i := range hits {
		for _, so := range sorts {
			f := ""
			switch x := so.(type) {
			case string:
				f = x
			case map[string]any:
				for k := range x {
					f = k
				}
			}
			hits[i].keys = append(hits[i].keys, fieldVal(hits[i], f))
		}
	}
	if sa, ok := req["search_after"].([]any); ok && len(sa) > 0 {
		cut := 0
		for cut < len(hits) && cmpVals(hits[cut].keys[0], sa[0]) <= 0 {
			cut++
		}
		hits = hits[cut:]
	}
	total := len(hits)
	rel := "eq"
	switch th := req["track_total_hits"].(type) {
	case float64:
		if total > int(th) {
			total, rel = int(th), "gte"
		}
	case bool:
		if !th {
			total = 0
		}
	}
	from, size := 0, 10
	if f, ok := req["from"].(float64); ok {
		from = int(f)
	}
	if f, ok := req["size"].(float64); ok {
		size = int(f)
	}
	if from > len(hits) {
		from = len(hits)
	}
	end := from + size
	if end > len(hits) {
		end = len(hits)
	}
	var out []any
	for _, h := range hits[from:end] {
		item := map[string]any{"_id": h.id, "_score": 1.0}
		switch src := req["_source"].(type) {
		case bool:
			if src {
				item["_source"] = h.src
			}
		case []any:
			m := map[string]any{}
			for _, f := range src {
				if v, ok := h.src[fmt.Sprint(f)]; ok {
					m[fmt.Sprint(f)] = v
				}
			}
			item["_source"] = m
		case nil:
			item["_source"] = h.src
		}
		item["sort"] = h.keys
		out = append(out, item)
	}
	reply(w, 200, map[string]any{"hits": map[string]any{"total": map[string]any{"value": total, "relation": rel}, "hits": out}})
}

func fieldVal(h hit, f string) any {
	if f == "id" || f == "_id" {
		return h.id
	}
	return h.src[f]
}

func cmpVals(a, b any) int {
	af, aok := toF(a)
	bf, bok := toF(b)
	if aok && bok {
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		}
		return 0
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

func toF(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func vals(v any) []any {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		return x
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	}
	return []any{v}
}

func analyze(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func evalQuery(q any, src map[string]any) (bool, error) {
	m, ok := q.(map[string]any)
	if !ok || len(m) == 0 {
		return true, nil
	}
	for kind, spec := range m {
		switch kind {
		case "match_all":
			return true, nil
		case "bool":
			b, _ := spec.(map[string]any)
			list := func(k string) []any {
				switch x := b[k].(type) {
				case []any:
					return x
				case map[string]any:
					return []any{x}
				}
				return nil
			}
			for _, c := range list("must") {
				if ok, err := evalQuery(c, src); err != nil || !ok {
					return false, err
				}
			}
			for _, c := range list("filter") {
				if ok, err := evalQuery(c, src); err != nil || !ok {
					return false, err
				}
			}
			for _, c := range list("must_not") {
				if ok, err := evalQuery(c, src); err != nil || ok {
					return false, err
				}
			}
			should := list("should")
			if len(should) > 0 {
				min := 0
				if len(list("must"))+len(list("filter")) == 0 {
					min = 1
				}
				if v, ok := b["minimum_should_match"].(float64); ok {
					min = int(v)
				}
				n := 0
				for _, c := range should {
					if ok, err := evalQuery(c, src); err != nil {
						return false, err
					} else if ok {
						n++
					}
				}
				if n < min {
					return false, nil
				}
			}
			return true, nil
		case "term", "terms":
			sp, _ := spec.(map[string]any)
			for f, want := range sp {
				have := vals(src[f])
				wants := vals(want)
				for _, h := range have {
					for _, wv := range wants {
						if cmpVals(h, wv) == 0 {
							return true, nil
						}
					}
				}
				// an absent field matches the empty string and zero, as an indexed empty keyword would
				if len(have) == 0 {
					for _, wv := range wants {
						if s, ok := wv.(string); ok && s == "" {
							return true, nil
						}
					}
				}
				return false, nil
			}
		case "exists":
			f, _ := spec.(map[string]any)["field"].(string)
			v := src[f]
			if arr, ok := v.([]any); ok {
				return len(arr) > 0, nil
			}
			return v != nil && v != "", nil
		case "range":
			sp, _ := spec.(map[string]any)
			for f, c := range sp {
				cond, _ := c.(map[string]any)
				v := src[f]
				if v == nil {
					return false, nil
				}
				for op, bound := range cond {
					cmp := cmpTimeAware(v, bound)
					switch op {
					case "gt":
						if cmp <= 0 {
							return false, nil
						}
					case "gte":
						if cmp < 0 {
							return false, nil
						}
					case "lt":
						if cmp >= 0 {
							return false, nil
						}
					case "lte":
						if cmp > 0 {
							return false, nil
						}
					default:
						return false, fmt.Errorf("unsupported range operator %q", op)
					}
				}
				return true, nil
			}
		case "match":
			sp, _ := spec.(map[string]any)
			for f, c := range sp {
				query := ""
				switch x := c.(type) {
				case string:
					query = x
				case map[string]any:
					query, _ = x["query"].(string)
				}
				have := map[string]bool{}
				for _, t := range analyze(fmt.Sprint(src[f])) {
					have[t] = true
				}
				toks := analyze(query)
				if len(toks) == 0 {
					return false, nil
				}
				for _, t := range toks {
					if !have[t] {
						return false, nil
					}
				}
				return true, nil
			}
		case "prefix":
			sp, _ := spec.(map[string]any)
			for f, want := range sp {
				for _, h := range vals(src[f]) {
					if strings.HasPrefix(fmt.Sprint(h), fmt.Sprint(want)) {
						return true, nil
					}
				}
				return false, nil
			}
		case "wildcard":
			sp, _ := spec.(map[string]any)
			for f, want := range sp {
				re := wildcardRE(fmt.Sprint(want))
				for _, h := range vals(src[f]) {
					if re.MatchString(fmt.Sprint(h)) {
						return true, nil
					}
				}
				return false, nil
			}
		default:
			return false, fmt.Errorf("unsupported query type %q", kind)
		}
	}
	return false, nil
}

func wildcardRE(p string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '\\':
			if i+1 < len(p) {
				i++
				b.WriteString(regexp.QuoteMeta(string(p[i])))
			}
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile("(?s)" + b.String())
}

func cmpTimeAware(a, b any) int {
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		ta, e1 := time.Parse(time.RFC3339Nano, as)
		tb, e2 := time.Parse(time.RFC3339Nano, bs)
		if e1 == nil && e2 == nil {
			switch {
			case ta.Before(tb):
				return -1
			case ta.After(tb):
				return 1
			}
			return 0
		}
	}
	return cmpVals(a, b)
}
