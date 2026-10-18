package api

import (
	"net/http"
	"strconv"

	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerInbox(mux *http.ServeMux) {
	s.handle(mux, "GET /api/v1/notifications", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		limit, _ := strconv.Atoi(v.Get("limit"))
		before, _ := strconv.ParseInt(v.Get("before"), 10, 64)
		out, err := s.Svc.Inbox(r.Context(), actorOf(r), domain.InboxQuery{State: v.Get("state"), Limit: limit, Before: before})
		s.reply(w, out, err)
	}))
	s.handle(mux, "GET /api/v1/notifications/unread-count", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, err := s.Svc.UnreadCount(r.Context(), actorOf(r))
		s.reply(w, map[string]int{"unread": n}, err)
	}))
	s.handle(mux, "POST /api/v1/notifications/read-all", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, err := s.Svc.MarkAllRead(r.Context(), actorOf(r))
		s.reply(w, map[string]int{"marked": n}, err)
	}))
	s.handle(mux, "POST /api/v1/notifications/{id}/state", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		if !ok {
			s.fail(w, 404, "not_found", "not found", nil)
			return
		}
		var in struct {
			State string `json:"state"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, map[string]bool{"ok": true}, s.Svc.MarkNotification(r.Context(), actorOf(r), id, in.State))
		}
	}))
	s.handle(mux, "GET /api/v1/me/notification-prefs", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, s.Svc.MyNotifyPrefs(r.Context(), actorOf(r)), nil)
	}))
	s.handle(mux, "PUT /api/v1/me/notification-prefs", s.auth(func(w http.ResponseWriter, r *http.Request) {
		in := domain.NotifyPrefs{AutoWatch: true, Inbox: true, Email: true}
		if s.decode(w, r, &in) {
			v, err := s.Svc.SetNotifyPrefs(r.Context(), actorOf(r), in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "GET /api/v1/issues/{key}/watch", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.WatchState(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/issues/{key}/watch", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Watch bool `json:"watch"`
		}
		if s.decode(w, r, &in) {
			v, err := s.Svc.SetWatch(r.Context(), actorOf(r), r.PathValue("key"), in.Watch)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/issues/{key}/watch", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ClearWatch(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "GET /api/v1/projects/{key}/notification-scheme", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.NotificationScheme(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/projects/{key}/notification-scheme", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in map[string][]string
		if s.decode(w, r, &in) {
			s.reply(w, map[string]bool{"ok": true}, s.Svc.SetNotificationScheme(r.Context(), actorOf(r), r.PathValue("key"), in))
		}
	}))
}
