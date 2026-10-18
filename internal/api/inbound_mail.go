package api

import (
	"io"
	"net/http"
	"strconv"
)

func (s *Server) registerInboundMail(mux *http.ServeMux) {
	// The webhook transport: the body is the raw message, signed with the source's secret. Every refusal looks the same (403)
	// and a stored message is acknowledged (200) only after its raw copy is kept; a failure to keep it is a 5xx so the sender retries.
	s.handle(mux, "POST /api/v1/mail/inbound/{id}", func(w http.ResponseWriter, r *http.Request) {
		max := int64(26 << 20)
		raw, err := io.ReadAll(io.LimitReader(r.Body, max+1))
		if err != nil {
			s.fail(w, 400, "bad_request", "could not read the request", nil)
			return
		}
		if int64(len(raw)) > max {
			s.fail(w, 413, "too_large", "the message is larger than the limit", nil)
			return
		}
		res, err := s.Svc.ReceiveInboundMail(r.Context(), r.PathValue("id"), r.Header.Get("X-Tasklane-Signature"), raw)
		s.replyStatus(w, 200, res, err)
	})
	s.handle(mux, "GET /api/v1/admin/mail/inbound", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		v, err := s.Svc.ListInbound(r.Context(), actorOf(r), r.URL.Query().Get("outcome"), n)
		s.reply(w, v, err)
	}))
	s.handle(mux, "GET /api/v1/admin/mail/inbound/{id}/raw", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		b, err := s.Svc.InboundRaw(r.Context(), actorOf(r), id)
		if err != nil {
			s.reply(w, nil, err)
			return
		}
		w.Header().Set("Content-Type", "message/rfc822")
		w.Header().Set("Content-Disposition", `attachment; filename="message.eml"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(b)
	}))
	s.handle(mux, "POST /api/v1/admin/mail/inbound/{id}/release", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		c, err := s.Svc.ReleaseInbound(r.Context(), actorOf(r), id)
		s.reply(w, c, err)
	}))
	s.handle(mux, "DELETE /api/v1/admin/mail/inbound/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		s.reply(w, map[string]bool{"ok": true}, s.Svc.DeleteInbound(r.Context(), actorOf(r), id))
	}))
	s.handle(mux, "GET /api/v1/admin/mail/sources", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListMailSources(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/admin/mail/sources", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name    string `json:"name"`
			Trusted bool   `json:"trusted"`
		}
		if s.decode(w, r, &in) {
			v, err := s.Svc.CreateMailSource(r.Context(), actorOf(r), in.Name, in.Trusted)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/admin/mail/sources/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, map[string]bool{"ok": true}, s.Svc.DeleteMailSource(r.Context(), actorOf(r), r.PathValue("id")))
	}))
	s.handle(mux, "POST /api/v1/admin/mail/reply-keys", s.auth(func(w http.ResponseWriter, r *http.Request) {
		kid, err := s.Svc.RotateReplyKey(r.Context(), actorOf(r))
		s.replyStatus(w, 201, map[string]int{"kid": kid}, err)
	}))
	s.handle(mux, "DELETE /api/v1/admin/mail/reply-keys/{kid}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		kid, _ := strconv.Atoi(r.PathValue("kid"))
		s.reply(w, map[string]bool{"ok": true}, s.Svc.RevokeReplyKey(r.Context(), actorOf(r), kid))
	}))
	s.handle(mux, "GET /api/v1/projects/{key}/mail", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ProjectMail(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/projects/{key}/mail-policy", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Policy string `json:"policy"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, map[string]bool{"ok": true}, s.Svc.SetMailPolicy(r.Context(), actorOf(r), r.PathValue("key"), in.Policy))
		}
	}))
	s.handle(mux, "POST /api/v1/mail/bounces/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			s.fail(w, 400, "bad_request", "could not read the request", nil)
			return
		}
		n, err := s.Svc.ReceiveBounceEvents(r.Context(), r.PathValue("id"), r.Header.Get("X-Tasklane-Signature"), body)
		s.replyStatus(w, 200, map[string]int{"applied": n}, err)
	})
	s.handle(mux, "GET /api/v1/admin/mail/suppressions", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListSuppressions(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "DELETE /api/v1/admin/mail/suppressions/{email}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, map[string]bool{"ok": true}, s.Svc.ClearSuppression(r.Context(), actorOf(r), r.PathValue("email")))
	}))
}
