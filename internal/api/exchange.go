package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/larytet/tasklane/internal/domain"
)

// lazyWriter sends the download headers with the first byte, so an export that fails early can still answer with a JSON error.
type lazyWriter struct {
	w       http.ResponseWriter
	started bool
	name    string
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		h := l.w.Header()
		h.Set("Content-Type", "application/zip")
		h.Set("Content-Disposition", `attachment; filename="`+l.name+`"`)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
	}
	return l.w.Write(p)
}

func (s *Server) registerExchange(mux *http.ServeMux) {
	s.handle(mux, "POST /api/v1/export", s.auth(func(w http.ResponseWriter, r *http.Request) {
		o := domain.DefaultExport()
		if !s.decode(w, r, &o) {
			return
		}
		lw := &lazyWriter{w: w, name: "tasklane-export-" + o.Format + ".zip"}
		if o.Format == "" {
			lw.name = "tasklane-export-json.zip"
		}
		man, err := s.Svc.Export(r.Context(), actorOf(r), lw, o)
		if err != nil {
			if lw.started { // the download already began: the truncated zip fails its checksum check on import, which is the safe outcome
				return
			}
			s.reply(w, man, err)
		}
	}))
	s.handle(mux, "POST /api/v1/import", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if !actorOf(r).Admin {
			s.fail(w, 403, "forbidden", "administrators only", nil)
			return
		}
		// the body goes to a temporary file, so a large bundle is never held in memory
		tmp, err := os.CreateTemp("", "tasklane-import-*.zip")
		if err != nil {
			s.fail(w, 500, "internal", "internal error", nil)
			return
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, 4<<30))
		if err != nil {
			s.fail(w, 400, "bad_request", "could not read the upload: "+err.Error(), nil)
			return
		}
		q := r.URL.Query()
		o := domain.ImportOpts{DryRun: q.Get("dry_run") == "1", Pause: q.Get("pause") == "1", Source: q.Get("source"), RenameProjects: map[string]string{}}
		for _, rn := range q["rename"] {
			if p := strings.SplitN(rn, ":", 2); len(p) == 2 {
				o.RenameProjects[strings.ToUpper(p[0])] = strings.ToUpper(p[1])
			}
		}
		rep, err := s.Svc.Import(r.Context(), actorOf(r), tmp, n, o)
		if err != nil {
			if rep != nil && len(rep.Errors) > 0 {
				s.fail(w, 409, "import_blocked", err.Error(), rep)
				return
			}
			s.reply(w, rep, err)
			return
		}
		s.json(w, 200, rep)
	}))
	s.handle(mux, "POST /api/v1/import/{id}/rollback", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, err := s.Svc.RollbackImport(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, map[string]int{"issues_removed": n}, err)
	}))
	s.handle(mux, "POST /api/v1/import/csv", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			domain.CSVImportIn
			CSV string `json:"csv"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		rep, err := s.Svc.ImportCSV(r.Context(), actorOf(r), []byte(in.CSV), in.CSVImportIn)
		s.reply(w, rep, err)
	}))
	// the third-party importers: the tool's own export goes in as the "data" of the request (up to 64 MB); the answer is the usual report
	s.handle(mux, "GET /api/v1/import/tools", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.json(w, 200, domain.ImportTools())
	}))
	s.handle(mux, "POST /api/v1/import/{tool}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			domain.ToolImportIn
			Data string `json:"data"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&in); err != nil {
			s.fail(w, 400, "bad_request", "invalid JSON body: "+err.Error(), nil)
			return
		}
		rep, err := s.Svc.ImportTool(r.Context(), actorOf(r), r.PathValue("tool"), []byte(in.Data), in.ToolImportIn)
		s.reply(w, rep, err)
	}))
	s.handle(mux, "PUT /api/v1/import/csv/templates/{name}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var m domain.CSVMapping
		if !s.decode(w, r, &m) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.SaveCSVTemplate(r.Context(), actorOf(r), r.PathValue("name"), m))
	}))
	s.handle(mux, "GET /api/v1/import/csv/templates/{name}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		m, err := s.Svc.CSVTemplate(r.Context(), actorOf(r), r.PathValue("name"))
		s.reply(w, m, err)
	}))
}
