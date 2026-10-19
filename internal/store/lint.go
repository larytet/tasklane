package store

import (
	"fmt"
	"regexp"
	"strings"
)

// Phase is the migration's place in a rolling upgrade (SPEC 17.8): expand changes are safe while the previous release
// still runs; contract changes remove what no running release uses and may only follow a release that stopped using it.
type Phase string

const (
	Expand   Phase = "expand"
	Contract Phase = "contract"
)

var (
	phaseRe   = regexp.MustCompile(`(?m)^--\s*phase:\s*(expand|contract)\s*$`)
	noTxRe    = regexp.MustCompile(`(?m)^--\s*no-transaction\s*$`)
	widensRe  = regexp.MustCompile(`(?m)^--\s*widens:\s*([a-z_][a-z0-9_]*)\s*$`)
	createTbl = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_.]*)`)
	createIdx = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+(CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?[a-z_][a-z0-9_]*\s+ON\s+([a-z_][a-z0-9_.]*)`)
)

func phaseOf(sql string) (Phase, bool) {
	m := phaseRe.FindStringSubmatch(sql)
	if m == nil {
		return "", false
	}
	return Phase(m[1]), true
}

func hasNoTx(sql string) bool { return noTxRe.MatchString(sql) }

// stripComments removes -- comments so that commented-out SQL is not linted.
func stripComments(sql string) string {
	var out []string
	for _, l := range strings.Split(sql, "\n") {
		if i := strings.Index(l, "--"); i >= 0 {
			l = l[:i]
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// Lint checks a migration against the expand and contract rules. earlier is the set of tables that existed before this
// migration; a plain CREATE INDEX on one of them takes a write lock for the duration of the build, so it must be
// CREATE INDEX CONCURRENTLY in a migration marked no-transaction.
func Lint(name, sql string, earlier map[string]bool) []string {
	var problems []string
	add := func(f string, a ...any) { problems = append(problems, name+": "+fmt.Sprintf(f, a...)) }
	ph, ok := phaseOf(sql)
	if !ok {
		add("missing the header comment `-- phase: expand` or `-- phase: contract`")
		return problems
	}
	body := strings.ToUpper(stripComments(sql))
	// a trigger that refuses TRUNCATE protects data; only a statement that runs it destroys any
	body = regexp.MustCompile(`\bBEFORE\s+(?:[A-Z]+\s+OR\s+)*TRUNCATE\b`).ReplaceAllString(body, "BEFORE")
	// a CHECK constraint replaced by a wider one (more allowed values) is safe for the previous release, which never writes the new
	// values; the migration says so with `-- widens: name` and must add the constraint of that name back as a CHECK. Whether it is
	// really wider is the reviewer's to judge: the header is the flag for that review.
	for _, m := range widensRe.FindAllStringSubmatch(sql, -1) {
		name := strings.ToUpper(m[1])
		if !regexp.MustCompile(`\bADD\s+CONSTRAINT\s+` + name + `\s+CHECK\b`).MatchString(body) {
			add("`-- widens: %s` but the migration does not add %s back as a CHECK constraint", m[1], m[1])
			continue
		}
		body = regexp.MustCompile(`\bDROP\s+CONSTRAINT\s+(?:IF\s+EXISTS\s+)?`+name+`\b`).ReplaceAllString(body, "")
	}
	if ph == Expand {
		for _, bad := range []struct{ re, why string }{
			{`\bDROP\s+TABLE\b`, "DROP TABLE removes data the previous release may still read"},
			{`\bDROP\s+COLUMN\b`, "DROP COLUMN breaks the previous release"},
			{`\bALTER\s+TABLE\s+\S+\s+RENAME\b`, "renaming breaks the previous release"},
			{`\bRENAME\s+COLUMN\b`, "renaming breaks the previous release"},
			{`\bALTER\s+COLUMN\s+\S+\s+(SET\s+DATA\s+)?TYPE\b`, "changing a column type rewrites the table under a lock"},
			{`\bSET\s+NOT\s+NULL\b`, "SET NOT NULL scans the table under a lock and breaks writers that omit the column; add the column nullable with a default first"},
			{`\bTRUNCATE\b`, "TRUNCATE destroys data"},
			{`\bDROP\s+INDEX\b`, "dropping an index belongs in a contract migration"},
			{`\bDROP\s+(CONSTRAINT|DEFAULT|NOT\s+NULL)\b`, "dropping a constraint belongs in a contract migration"},
		} {
			if regexp.MustCompile(bad.re).MatchString(body) {
				add("an expand migration may not do this: %s", bad.why)
			}
		}
	}
	created := map[string]bool{}
	for _, m := range createTbl.FindAllStringSubmatch(sql, -1) {
		created[strings.ToLower(m[1])] = true
	}
	for _, m := range createIdx.FindAllStringSubmatch(stripComments(sql), -1) {
		table := strings.ToLower(m[2])
		concurrently := m[1] != ""
		if created[table] || !earlier[table] {
			continue // an index on a table created in this same migration is free
		}
		if !concurrently {
			add("CREATE INDEX on the existing table %s must be CREATE INDEX CONCURRENTLY (a plain build blocks writes)", table)
		} else if !hasNoTx(sql) {
			add("CREATE INDEX CONCURRENTLY cannot run inside a transaction: mark the migration `-- no-transaction`")
		}
	}
	if hasNoTx(sql) && len(regexp.MustCompile(`;`).FindAllString(stripComments(sql), -1)) > 1 {
		add("a no-transaction migration should hold a single statement so a failure leaves a clean state")
	}
	return problems
}

// tablesIn returns the tables created by the SQL, for tracking what existed before a migration.
func tablesIn(sql string) []string {
	var out []string
	for _, m := range createTbl.FindAllStringSubmatch(sql, -1) {
		out = append(out, strings.ToLower(m[1]))
	}
	return out
}
