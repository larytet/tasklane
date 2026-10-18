package store

import (
	"context"
	"fmt"
	"strings"
)

// Compatibility between a binary and the schema it finds (SPEC 17.8, UPG-05, UPG-06). A node refuses to start when the
// schema is outside the range it can run on, and says why and what to do instead of failing later in a query.
//
//   - A schema behind the binary is not migrated by the server (the schema change is an explicit step): the node refuses and
//     names the command. An empty database is the exception, it is initialized.
//   - A schema older than this release can upgrade from (the floor) is refused with the stepping path: an offline upgrade
//     is supported from the previous two releases, so a third is reached through one in between.
//   - A schema ahead of the binary is fine only inside the expand-only window: every migration the binary does not know
//     is an expand migration, which the previous release can run beside. Once any contract migration has been applied
//     (`migrate finalize`), the older binary refuses and the way back is a restore.

type Policy struct {
	Latest       int    // the highest migration this binary has
	Floor        int    // the lowest schema version this release upgrades from (0: no floor yet)
	FloorRelease string // the release that ends at that schema, named in the message
	Stepping     string // what to do for an older database, in words
}

func DefaultPolicy() Policy {
	ms, _ := Migrations()
	p := Policy{Floor: floorSchema, FloorRelease: floorRelease, Stepping: steppingPath}
	if len(ms) > 0 {
		p.Latest = ms[len(ms)-1].Version
	}
	return p
}

// What this release can upgrade from. There has been no tagged release yet, so there is no floor; each release sets these
// when it is cut (UPG-05).
const (
	floorSchema  = 0
	floorRelease = ""
	steppingPath = ""
)

type Verdict struct {
	Applied int    // the highest applied migration
	Latest  int    // the binary's
	Fresh   bool   // an empty database: the caller initializes it
	Window  bool   // running beside a newer, expand-only schema
	Note    string // what a person should know
}

// IncompatibleError says why a node will not run on this schema and what to do.
type IncompatibleError struct{ Reason, Fix string }

func (e *IncompatibleError) Error() string { return e.Reason + ". " + e.Fix }

func (s *Store) CheckCompat(ctx context.Context, p Policy) (Verdict, error) {
	v := Verdict{Latest: p.Latest}
	var have bool
	if err := s.Pool.QueryRow(ctx, `SELECT to_regclass('schema_migration') IS NOT NULL`).Scan(&have); err != nil {
		return v, fmt.Errorf("the database cannot be read: %w", err)
	}
	if !have {
		v.Fresh = true
		return v, nil
	}
	type row struct {
		ver   int
		phase string
	}
	var rs []row
	q := `SELECT version, COALESCE(phase,'') FROM schema_migration ORDER BY version`
	rows, err := s.Pool.Query(ctx, q)
	if err != nil { // a database from before phases were recorded
		rows, err = s.Pool.Query(ctx, `SELECT version, '' FROM schema_migration ORDER BY version`)
		if err != nil {
			return v, fmt.Errorf("the database cannot be read: %w", err)
		}
	}
	for rows.Next() {
		var r row
		rows.Scan(&r.ver, &r.phase)
		rs = append(rs, r)
		if r.ver > v.Applied {
			v.Applied = r.ver
		}
	}
	rows.Close()
	if v.Applied == 0 {
		v.Fresh = true
		return v, nil
	}
	switch {
	case p.Floor > 0 && v.Applied < p.Floor:
		fix := "Upgrade to " + p.FloorRelease + " first, then to this release."
		if p.Stepping != "" {
			fix = p.Stepping
		}
		return v, &IncompatibleError{fmt.Sprintf("the database schema (version %d) is older than the oldest this release upgrades from (version %d, release %s)", v.Applied, p.Floor, p.FloorRelease), fix}
	case v.Applied < p.Latest:
		return v, &IncompatibleError{fmt.Sprintf("the database schema (version %d) is behind this release (version %d)", v.Applied, p.Latest), "Run `tasklane migrate` first: schema changes are an explicit step, never a side effect of starting"}
	case v.Applied > p.Latest:
		var finalized, unknown []string
		for _, r := range rs {
			if r.ver <= p.Latest {
				continue
			}
			switch r.phase {
			case string(Expand):
			case string(Contract):
				finalized = append(finalized, fmt.Sprint(r.ver))
			default:
				unknown = append(unknown, fmt.Sprint(r.ver))
			}
		}
		if len(finalized) > 0 {
			return v, &IncompatibleError{fmt.Sprintf("the database schema (version %d) was finalized by a newer release (contract migration %s removed what this release needs)", v.Applied, strings.Join(finalized, ", ")), "Run the newer release, or restore a backup taken before `migrate finalize`"}
		}
		if len(unknown) > 0 {
			return v, &IncompatibleError{fmt.Sprintf("the database schema (version %d) has migrations this release cannot classify (%s)", v.Applied, strings.Join(unknown, ", ")), "Run the newer release"}
		}
		v.Window = true
		v.Note = fmt.Sprintf("running beside a newer schema (version %d, this release has %d): every newer migration is expand-only, so this release is safe until `migrate finalize`", v.Applied, p.Latest)
	}
	return v, nil
}
