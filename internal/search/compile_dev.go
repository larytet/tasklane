package search

import (
	"strconv"
	"strings"

	"github.com/larytet/tasklane/internal/search/tql"
)

// Development fields (SPEC 18.4, GIT-19): what the Git integration knows about an issue, as TQL.
//
//	pullRequest.state = open          an issue with a pull request in that state (draft, open, merged, closed)
//	pullRequest.review = approved     ... whose review state is approved or changes_requested
//	pullRequest.count >= 2            how many pull requests are linked
//	branch ~ "login"                  a linked branch whose name contains the text (= is an exact name)
//	build.state = failing             the worst state across the issue's builds: failing, then running, then passing
//	deployment.environment = production   currently deployed there: of the deployments of the issue's own commits to that environment, the latest succeeded
//
// Every one is a condition on the issue row, which the search scope has already limited to the projects the caller may read,
// so a query can never return an issue the caller cannot see. By default anyone who can see an issue sees its development
// data. These fields are not in the search index; the router sends a query that uses them to PostgreSQL.

// vis is the visibility condition on the code object o (GIT-11): a restricted organization does not let a query see, or be
// answered by, the details of a repository the caller may not see. Administrators and the system see everything.
func (c *compiler) vis() string {
	if c.sc.All || c.sc.UserID == "" {
		return ""
	}
	return " AND git_repo_visible(o.repo_id, " + c.arg(c.sc.UserID) + "::uuid, " + c.arg(c.nowFn()) + "::timestamptz)"
}

func (c *compiler) prLinked() string {
	return "EXISTS (SELECT 1 FROM git_link l JOIN git_object o ON o.id=l.object_id AND o.kind='pr' WHERE l.issue_id=i.id" + c.vis()
}

func (c *compiler) buildAgg() string {
	return `(SELECT CASE WHEN bool_or(b.state='failing') THEN 'failing' WHEN bool_or(b.state='running') THEN 'running' WHEN bool_or(b.state='passing') THEN 'passing' END
		FROM git_link l JOIN git_object o ON o.id=l.object_id AND o.kind IN ('pr','branch') AND o.sha<>''` + c.vis() + `
		JOIN git_object b ON b.repo_id=o.repo_id AND b.kind='build' AND b.sha=o.sha WHERE l.issue_id=i.id)`
}

var (
	prStates    = map[string]bool{"draft": true, "open": true, "merged": true, "closed": true}
	prReviews   = map[string]bool{"approved": true, "changes_requested": true}
	buildStates = map[string]bool{"passing": true, "running": true, "failing": true}
)

// devClause compiles a development field. The second result is false when the field is not one of them.
func (c *compiler) devClause(x tql.Cmp) (string, bool, error) {
	switch x.Field {
	case "pullrequest.state", "pullrequest.review", "pullrequest.count", "branch", "build.state", "deployment.environment":
	default:
		return "", false, nil
	}
	isNeg := false
	switch x.Op {
	case "!=", "notin", "!~", "isnot":
		isNeg = true
	}
	// values: plain words only
	vals := func(allowed map[string]bool) ([]string, error) {
		var out []string
		for _, v := range x.Vals {
			if v.Empty || v.Fn != "" {
				return nil, errAt(v.Pos, "%s needs a plain value", x.Field)
			}
			s := strings.ToLower(strings.TrimSpace(v.S))
			if allowed != nil && !allowed[s] {
				names := make([]string, 0, len(allowed))
				for k := range allowed {
					names = append(names, k)
				}
				sortStrings(names)
				return nil, errAt(v.Pos, "%s is %s", x.Field, strings.Join(names, ", "))
			}
			out = append(out, s)
		}
		return out, nil
	}
	switch x.Field {
	case "pullrequest.state", "pullrequest.review":
		col, allowed := "o.state", prStates
		if x.Field == "pullrequest.review" {
			col, allowed = "o.review", prReviews
		}
		switch x.Op {
		case "is":
			return "(NOT " + c.prLinked() + "))", true, nil
		case "isnot":
			return "(" + c.prLinked() + "))", true, nil
		case "=", "!=", "in", "notin":
			vs, err := vals(allowed)
			if err != nil {
				return "", true, err
			}
			return negate(isNeg, c.prLinked()+" AND "+col+" = ANY("+c.arg(vs)+"::text[]))"), true, nil
		}
	case "pullrequest.count":
		switch x.Op {
		case "=", "!=", ">", "<", ">=", "<=":
			v := x.Vals[0]
			n, err := strconv.Atoi(strings.TrimSpace(v.S))
			if err != nil || v.Empty || v.Fn != "" || n < 0 {
				return "", true, errAt(v.Pos, "pullRequest.count is compared with a whole number")
			}
			return "((SELECT count(*) FROM git_link l JOIN git_object o ON o.id=l.object_id AND o.kind='pr' WHERE l.issue_id=i.id" + c.vis() + ") " + x.Op + " " + c.arg(n) + "::int)", true, nil
		}
	case "branch":
		linked := "EXISTS (SELECT 1 FROM git_link l JOIN git_object o ON o.id=l.object_id AND o.kind='branch' WHERE l.issue_id=i.id" + c.vis()
		switch x.Op {
		case "is":
			return "(NOT " + linked + "))", true, nil
		case "isnot":
			return "(" + linked + "))", true, nil
		case "~", "!~":
			vs, err := vals(nil)
			if err != nil || len(vs) != 1 || vs[0] == "" {
				return "", true, errAt(x.Pos, "branch ~ needs some text")
			}
			return negate(isNeg, linked+" AND strpos(lower(o.title), "+c.arg(vs[0])+") > 0)"), true, nil
		case "=", "!=", "in", "notin":
			vs, err := vals(nil)
			if err != nil {
				return "", true, err
			}
			return negate(isNeg, linked+" AND lower(o.title) = ANY("+c.arg(vs)+"::text[]))"), true, nil
		}
	case "build.state":
		switch x.Op {
		case "is":
			return "(" + c.buildAgg() + " IS NULL)", true, nil
		case "isnot":
			return "(" + c.buildAgg() + " IS NOT NULL)", true, nil
		case "=", "!=", "in", "notin":
			vs, err := vals(buildStates)
			if err != nil {
				return "", true, err
			}
			return negate(isNeg, c.buildAgg()+" = ANY("+c.arg(vs)+"::text[])"), true, nil
		}
	case "deployment.environment":
		dep := `EXISTS (SELECT 1 FROM git_link l JOIN git_object o ON o.id=l.object_id AND o.sha<>'' AND o.kind IN ('pr','branch','commit')
			JOIN git_object x ON x.repo_id=o.repo_id AND x.kind='deployment' AND x.sha=o.sha WHERE l.issue_id=i.id` + c.vis()
		switch x.Op {
		case "is":
			return "(NOT " + dep + "))", true, nil
		case "isnot":
			return "(" + dep + "))", true, nil
		case "=", "!=", "in", "notin":
			vs, err := vals(nil)
			if err != nil {
				return "", true, err
			}
			// currently there: among the deployments of this issue's own commits to that environment, the latest one succeeded (as the
			// development panel shows it). Another issue's failed deployment of its own commit does not undo this one.
			cond := dep + ` AND x.state='success' AND lower(x.env) = ANY(` + c.arg(vs) + `::text[])
				AND NOT EXISTS (SELECT 1 FROM git_link l2 JOIN git_object o2 ON o2.id=l2.object_id AND o2.sha<>'' AND o2.kind IN ('pr','branch','commit')
					JOIN git_object y ON y.repo_id=o2.repo_id AND y.kind='deployment' AND y.sha=o2.sha AND lower(y.env)=lower(x.env)
					WHERE l2.issue_id=i.id AND y.provider_at > x.provider_at))`
			return negate(isNeg, cond), true, nil
		}
	}
	return "", true, errAt(x.Pos, "operator %s cannot be used with %s", opName(x.Op), x.Field)
}

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}
