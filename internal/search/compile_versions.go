package search

import (
	"strings"

	"github.com/larytet/tasklane/internal/search/tql"
)

// Version fields (SPEC 12): fixVersion and affectedVersion name versions of the issue's own project, without regard to case.
//
//	fixVersion = "2.1"                 fixed in a version called 2.1 (in any project that has one)
//	fixVersion in (releasedVersions())   fixed in any version that has been released
//	fixVersion in (unreleasedVersions()) ... that has not
//	fixVersion is EMPTY                no fix version
//
// The condition is on the issue row, which the search scope has already limited, so versions of projects the caller cannot read never
// matter. These fields are not in the search index; the router falls back to PostgreSQL for them.
func (c *compiler) versionClause(x tql.Cmp, isNeg bool) (string, error) {
	kind := "fix"
	if x.Field != "fixversion" {
		kind = "affects"
	}
	link := "SELECT 1 FROM issue_version iv JOIN version v ON v.id=iv.version_id WHERE iv.issue_id=i.id AND iv.kind='" + kind + "'"
	switch x.Op {
	case "is":
		return "NOT EXISTS (" + link + ")", nil
	case "isnot":
		return "EXISTS (" + link + ")", nil
	case "=", "!=", "in", "notin":
		var names []string
		var conds []string
		for _, v := range x.Vals {
			switch {
			case v.Empty:
				conds = append(conds, "NOT EXISTS ("+link+")")
			case v.Fn == "releasedversions":
				conds = append(conds, "EXISTS ("+link+" AND v.released)")
			case v.Fn == "unreleasedversions":
				conds = append(conds, "EXISTS ("+link+" AND NOT v.released)")
			case v.Fn != "":
				return "", errAt(v.Pos, "function %s() cannot be used with %s", v.Fn, x.Field)
			default:
				names = append(names, strings.ToLower(strings.TrimSpace(v.S)))
			}
		}
		if len(names) > 0 {
			conds = append(conds, "EXISTS ("+link+" AND lower(v.name) = ANY("+c.arg(names)+"::text[]))")
		}
		return negate(isNeg, strings.Join(conds, " OR ")), nil
	}
	return "", errAt(x.Pos, "operator %s cannot be used with %s", opName(x.Op), x.Field)
}
