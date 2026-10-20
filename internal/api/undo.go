package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/larytet/tasklane/internal/domain"
)

// Undo instead of confirm (SPEC 4.6). A request with "Tasklane-Want-Undo: 1" to a reversible action gets its single-use token back
// in "Tasklane-Undo" (and the seconds it lasts in "Tasklane-Undo-Expires"); POST /api/v1/undo/{token} reverts it if nobody changed
// the issue since (409 changed_since otherwise). A token that is used, expired or another person's is 404.
func (s *Server) registerUndo(mux *http.ServeMux) {
	// a reaction on a comment: {"emoji": "👍", "on": true} adds it, false takes it away; the answer is the comment's reactions
	s.handle(mux, "PUT /api/v1/issues/{key}/comments/{id}/reactions", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Emoji string `json:"emoji"`
			On    bool   `json:"on"`
		}
		if s.decode(w, r, &in) {
			out, err := s.Svc.React(r.Context(), actorOf(r), r.PathValue("key"), r.PathValue("id"), in.Emoji, in.On)
			s.reply(w, out, err)
		}
	}))
	s.handle(mux, "POST /api/v1/undo/{token}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		res, err := s.Svc.Undo(r.Context(), actorOf(r), r.PathValue("token"))
		s.reply(w, res, err)
	}))
}

func (s *Server) wantUndo(r *http.Request) (context.Context, *domain.UndoSlot) {
	if r.Header.Get("Tasklane-Want-Undo") != "1" {
		return r.Context(), nil
	}
	return domain.WithUndo(r.Context())
}

func (s *Server) giveUndo(w http.ResponseWriter, sl *domain.UndoSlot, err error) {
	if sl == nil || err != nil || sl.Token == "" {
		return
	}
	w.Header().Set("Tasklane-Undo", sl.Token)
	w.Header().Set("Tasklane-Undo-Expires", strconv.Itoa(int(domain.UndoWindow.Seconds())))
}
