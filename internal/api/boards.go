package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/larytet/tasklane/internal/calendar"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
)

func (s *Server) registerBoards(mux *http.ServeMux) {
	s.handle(mux, "GET /api/v1/projects/{key}/boards", s.auth(func(w http.ResponseWriter, r *http.Request) {
		bs, err := s.Svc.ListBoards(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, bs, err)
	}))
	s.handle(mux, "POST /api/v1/projects/{key}/boards", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Kind string }
		if !s.decode(w, r, &in) {
			return
		}
		b, err := s.Svc.CreateBoard(r.Context(), actorOf(r), r.PathValue("key"), in.Name, in.Kind)
		s.replyStatus(w, 201, b, err)
	}))
	// the board with its cards; ?quick=Name,Name turns quick filters on. The board filter and chosen quick filters are
	// TQL, evaluated inside the caller's permission scope, and only the matching keys reach the board view.
	s.handle(mux, "GET /api/v1/boards/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		a := actorOf(r)
		b, _, _, err := s.Svc.BoardAccess(r.Context(), a, r.PathValue("id"))
		if err != nil {
			s.reply(w, nil, err)
			return
		}
		restrict, ok := s.boardRestrict(w, r, b, r.URL.Query().Get("quick"))
		if !ok {
			return
		}
		v, err := s.Svc.GetBoard(r.Context(), a, b.ID, restrict)
		s.reply(w, v, err)
	}))
	s.handle(mux, "PATCH /api/v1/boards/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.BoardPatch
		if !s.decode(w, r, &in) {
			return
		}
		b, err := s.Svc.UpdateBoard(r.Context(), actorOf(r), r.PathValue("id"), in)
		s.reply(w, b, err)
	}))
	s.handle(mux, "PUT /api/v1/boards/{id}/columns", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Columns []domain.ColumnIn `json:"columns"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.SetColumns(r.Context(), actorOf(r), r.PathValue("id"), in.Columns))
	}))
	s.handle(mux, "POST /api/v1/boards/{id}/move", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Key string `json:"key"`
			domain.MoveIn
		}
		if !s.decode(w, r, &in) {
			return
		}
		res, err := s.Svc.MoveCard(r.Context(), actorOf(r), r.PathValue("id"), in.Key, in.MoveIn)
		s.reply(w, res, err)
	}))
}

func (s *Server) registerSprints(mux *http.ServeMux) {
	s.handle(mux, "GET /api/v1/boards/{id}/backlog", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.Backlog(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/boards/{id}/sprints", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.SprintIn
		if !s.decode(w, r, &in) {
			return
		}
		sp, err := s.Svc.CreateSprint(r.Context(), actorOf(r), r.PathValue("id"), in)
		s.replyStatus(w, 201, sp, err)
	}))
	// plan: put issues into a sprint (or the backlog when sprint is empty); rank: reorder one issue inside its list
	s.handle(mux, "POST /api/v1/boards/{id}/plan", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Sprint string   `json:"sprint"`
			Keys   []string `json:"keys"`
			domain.MoveIn
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.AssignSprint(r.Context(), actorOf(r), r.PathValue("id"), in.Sprint, in.Keys, in.MoveIn))
	}))
	s.handle(mux, "POST /api/v1/boards/{id}/rank", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Key string `json:"key"`
			domain.MoveIn
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.RankInContainer(r.Context(), actorOf(r), r.PathValue("id"), in.Key, in.MoveIn))
	}))
	s.handle(mux, "GET /api/v1/sprints/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.GetSprint(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PATCH /api/v1/sprints/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Goal *string }
		if !s.decode(w, r, &in) {
			return
		}
		sp, err := s.Svc.UpdateSprint(r.Context(), actorOf(r), r.PathValue("id"), in.Name, in.Goal)
		s.reply(w, sp, err)
	}))
	s.handle(mux, "POST /api/v1/sprints/{id}/start", s.auth(func(w http.ResponseWriter, r *http.Request) {
		sp, err := s.Svc.StartSprint(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, sp, err)
	}))
	s.handle(mux, "POST /api/v1/sprints/{id}/complete", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Destination string `json:"destination"`
			DryRun      bool   `json:"dry_run"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		res, err := s.Svc.CompleteSprint(r.Context(), actorOf(r), r.PathValue("id"), in.Destination, in.DryRun)
		s.reply(w, res, err)
	}))
	s.handle(mux, "POST /api/v1/sprints/{id}/recompute", s.auth(func(w http.ResponseWriter, r *http.Request) {
		sp, err := s.Svc.RecomputeSprint(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, sp, err)
	}))
	s.handle(mux, "PUT /api/v1/sprints/{id}/retro", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Text string }
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.SetRetro(r.Context(), actorOf(r), r.PathValue("id"), in.Text))
	}))
	s.handle(mux, "PUT /api/v1/sprints/{id}/capacity/{user}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			OffHours float64 `json:"off_hours"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.SetCapacity(r.Context(), actorOf(r), r.PathValue("id"), r.PathValue("user"), in.OffHours))
	}))
	s.handle(mux, "PUT /api/v1/users/{id}/calendar", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Calendar *calendar.Calendar `json:"calendar"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.SetUserCalendar(r.Context(), actorOf(r), r.PathValue("id"), in.Calendar))
	}))
}

// boardRestrict evaluates the board filter and the chosen quick filters (TQL, inside the caller's permission scope) and
// returns the matching keys; nil means no restriction. On a bad filter it has already answered with 400.
func (s *Server) boardRestrict(w http.ResponseWriter, r *http.Request, b domain.Board, quick string) ([]string, bool) {
	clauses := []string{}
	if b.Filter != "" {
		clauses = append(clauses, "("+b.Filter+")")
	}
	for _, name := range strings.Split(quick, ",") {
		for _, q := range b.QuickFilters {
			if name != "" && strings.EqualFold(q.Name, strings.TrimSpace(name)) {
				clauses = append(clauses, "("+q.Query+")")
			}
		}
	}
	if len(clauses) == 0 {
		return nil, true
	}
	sc, err := s.scope(r)
	if err != nil {
		s.reply(w, nil, err)
		return nil, false
	}
	keys, err := s.Search.Keys(r.Context(), sc, "project = "+b.Project+" AND "+strings.Join(clauses, " AND "), 5000)
	if err != nil {
		if te, ok := search.IsQueryError(err); ok {
			s.fail(w, 400, "tql_error", "the board filter does not parse: "+te.Msg, map[string]any{"pos": te.Pos})
			return nil, false
		}
		s.reply(w, nil, err)
		return nil, false
	}
	return keys, true
}

func (s *Server) registerReports(mux *http.ServeMux) {
	days := func(r *http.Request) int { n, _ := strconv.Atoi(r.URL.Query().Get("days")); return n }
	flow := func(path string, run func(r *http.Request, a domain.Actor, id string, restrict []string) (any, error)) {
		s.handle(mux, "GET /api/v1/boards/{id}/reports/"+path, s.auth(func(w http.ResponseWriter, r *http.Request) {
			a := actorOf(r)
			b, _, _, err := s.Svc.BoardAccess(r.Context(), a, r.PathValue("id"))
			if err != nil {
				s.reply(w, nil, err)
				return
			}
			restrict, ok := s.boardRestrict(w, r, b, r.URL.Query().Get("quick"))
			if !ok {
				return
			}
			v, err := run(r, a, b.ID, restrict)
			s.reply(w, v, err)
		}))
	}
	flow("cumulative-flow", func(r *http.Request, a domain.Actor, id string, rs []string) (any, error) {
		return s.Svc.CumulativeFlow(r.Context(), a, id, days(r), rs)
	})
	flow("control-chart", func(r *http.Request, a domain.Actor, id string, rs []string) (any, error) {
		return s.Svc.ControlChart(r.Context(), a, id, days(r), rs)
	})
	flow("created-vs-resolved", func(r *http.Request, a domain.Actor, id string, rs []string) (any, error) {
		return s.Svc.CreatedVsResolved(r.Context(), a, id, days(r), rs)
	})
	flow("workload", func(r *http.Request, a domain.Actor, id string, rs []string) (any, error) {
		return s.Svc.WorkloadByAssignee(r.Context(), a, id, rs)
	})
	flow("time-in-status", func(r *http.Request, a domain.Actor, id string, rs []string) (any, error) {
		return s.Svc.TimeInStatus(r.Context(), a, id, days(r), rs)
	})
	s.handle(mux, "GET /api/v1/boards/{id}/reports/velocity", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("sprints"))
		v, err := s.Svc.Velocity(r.Context(), actorOf(r), r.PathValue("id"), n)
		s.reply(w, v, err)
	}))
	s.handle(mux, "GET /api/v1/boards/{id}/reports/epic/{key}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.EpicBurndown(r.Context(), actorOf(r), r.PathValue("id"), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "GET /api/v1/sprints/{id}/report", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.SprintReport(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
}
