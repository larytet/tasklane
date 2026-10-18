package search

import (
	"strings"

	"github.com/larytet/tasklane/internal/search/tql"
)

// Service level fields (SPEC 15): the state of an issue's clocks.
//
//	sla.state = breached                 any clock of the issue is in that state (running, paused, met, breached)
//	sla.firstResponse.state = running    the first response clock
//	sla.resolution.state in (paused, breached)
//
// The clocks are stored with a state that is refreshed when the issue changes and every minute, so a query sees a goal that has just
// passed within a minute. These fields are not in the search index; the router falls back to PostgreSQL for them.
var slaStates = map[string]bool{"running": true, "paused": true, "met": true, "breached": true}

func (c *compiler) slaClause(x tql.Cmp, isNeg bool) (string, error) {
	kind := ""
	switch x.Field {
	case "sla.firstresponse.state":
		kind = " AND k.kind='first_response'"
	case "sla.resolution.state":
		kind = " AND k.kind='resolution'"
	}
	switch x.Op {
	case "=", "!=", "in", "notin":
		var states []string
		for _, v := range x.Vals {
			s := strings.ToLower(strings.TrimSpace(v.S))
			if v.Empty || v.Fn != "" || !slaStates[s] {
				return "", errAt(v.Pos, "%s is running, paused, met or breached", x.Field)
			}
			states = append(states, s)
		}
		return negate(isNeg, "EXISTS (SELECT 1 FROM sla_clock k WHERE k.issue_id=i.id"+kind+" AND k.state = ANY("+c.arg(states)+"::text[]))"), nil
	case "is":
		return "NOT EXISTS (SELECT 1 FROM sla_clock k WHERE k.issue_id=i.id" + kind + ")", nil
	case "isnot":
		return "EXISTS (SELECT 1 FROM sla_clock k WHERE k.issue_id=i.id" + kind + ")", nil
	}
	return "", errAt(x.Pos, "operator %s cannot be used with %s", opName(x.Op), x.Field)
}
