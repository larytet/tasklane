package api

import (
	"io"
	"net/http"
	"strconv"
	"strings"
)

// A person's own UI settings (SPEC 4.6). The version travels as the ETag; a save with If-Match on an older version gets 412 and the
// current settings, so a second tab merges instead of silently overwriting the first.
func (s *Server) registerUIPrefs(mux *http.ServeMux) {
	s.handle(mux, "GET /api/v1/me/ui-prefs", s.auth(func(w http.ResponseWriter, r *http.Request) {
		d, err := s.Svc.MyUIPrefs(r.Context(), actorOf(r))
		if err == nil {
			w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(d.Version, 10)))
		}
		s.reply(w, d, err)
	}))
	s.handle(mux, "PUT /api/v1/me/ui-prefs", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var im *int64
		if h := r.Header.Get("If-Match"); h != "" {
			n, err := strconv.ParseInt(strings.Trim(h, `"`), 10, 64)
			if err != nil {
				s.fail(w, 400, "bad_request", "If-Match must be the settings version", nil)
				return
			}
			im = &n
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			s.fail(w, 400, "bad_request", "the settings are too large", nil)
			return
		}
		d, err := s.Svc.SetUIPrefs(r.Context(), actorOf(r), raw, im)
		if err == nil {
			w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(d.Version, 10)))
		}
		s.reply(w, d, err)
	}))
}
