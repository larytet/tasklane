package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerAttachments(mux *http.ServeMux) {
	// single-request fast path: multipart (streamed, constant memory) or a raw body with ?name=
	s.handle(mux, "POST /api/v1/issues/{key}/attachments", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var name, ctype string
		var body io.Reader = r.Body
		if mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && mt == "multipart/form-data" {
			mr, err := r.MultipartReader()
			if err != nil {
				s.fail(w, 400, "bad_request", "invalid multipart body", nil)
				return
			}
			_ = params
			for {
				p, err := mr.NextPart()
				if err != nil {
					s.fail(w, 400, "bad_request", "no file part", nil)
					return
				}
				if p.FileName() != "" {
					name, ctype, body = p.FileName(), p.Header.Get("Content-Type"), p
					break
				}
			}
		} else {
			name, ctype = r.URL.Query().Get("name"), r.Header.Get("Content-Type")
			if name == "" {
				name = "file"
			}
		}
		att, err := s.Svc.AttachStream(r.Context(), actorOf(r), r.PathValue("key"), name, ctype, body)
		s.replyStatus(w, 201, att, err)
	}))
	s.handle(mux, "POST /api/v1/issues/{key}/uploads", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			Type string `json:"type"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		us, err := s.Svc.StartUpload(r.Context(), actorOf(r), r.PathValue("key"), in.Name, in.Type, in.Size)
		s.replyStatus(w, 201, us, err)
	}))
	// direct uploads: the browser sends bytes straight to the object store with presigned URLs, then asks us to verify
	s.handle(mux, "POST /api/v1/issues/{key}/uploads/direct", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			Type   string `json:"type"`
			SHA256 string `json:"sha256"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		du, err := s.Svc.StartDirectUpload(r.Context(), actorOf(r), r.PathValue("key"), in.Name, in.Type, in.Size, in.SHA256)
		s.replyStatus(w, 201, du, err)
	}))
	s.handle(mux, "POST /api/v1/uploads/direct/{id}/complete", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Parts []domain.DirectPartDone `json:"parts"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		att, err := s.Svc.CompleteDirectUpload(r.Context(), actorOf(r), r.PathValue("id"), in.Parts)
		s.replyStatus(w, 201, att, err)
	}))
	s.handle(mux, "DELETE /api/v1/uploads/direct/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		err := s.Svc.AbortDirectUpload(r.Context(), actorOf(r), r.PathValue("id"))
		if err != nil {
			s.replyStatus(w, 200, nil, err)
			return
		}
		w.WriteHeader(204)
	}))
	s.handle(mux, "HEAD /api/v1/uploads/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		us, err := s.Svc.UploadStatus(r.Context(), actorOf(r), r.PathValue("id"))
		if err != nil {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Upload-Offset", strconv.FormatInt(us.Offset, 10))
		w.Header().Set("Upload-Length", strconv.FormatInt(us.Size, 10))
		w.WriteHeader(200)
	}))
	s.handle(mux, "GET /api/v1/uploads/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		us, err := s.Svc.UploadStatus(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, us, err)
	}))
	s.handle(mux, "PATCH /api/v1/uploads/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		off, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
		if err != nil {
			s.fail(w, 400, "bad_request", "Upload-Offset header is required", nil)
			return
		}
		us, err := s.Svc.AppendChunk(r.Context(), actorOf(r), r.PathValue("id"), off, r.Body)
		w.Header().Set("Upload-Offset", strconv.FormatInt(us.Offset, 10))
		s.reply(w, us, err)
	}))
	s.handle(mux, "POST /api/v1/uploads/{id}/commit", s.auth(func(w http.ResponseWriter, r *http.Request) {
		att, err := s.Svc.CommitUpload(r.Context(), actorOf(r), r.PathValue("id"))
		s.replyStatus(w, 201, att, err)
	}))
	s.handle(mux, "DELETE /api/v1/issues/{key}/attachments/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, map[string]bool{"ok": true}, s.Svc.DeleteAttachment(r.Context(), actorOf(r), r.PathValue("key"), r.PathValue("id")))
	}))
	s.handle(mux, "POST /api/v1/attachments/{id}/thumbnail", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, map[string]bool{"ok": true}, s.Svc.RetryThumbnail(r.Context(), actorOf(r), r.PathValue("id")))
	}))
	// links are minted only after the permission checks and expire in minutes
	s.handle(mux, "POST /api/v1/attachments/{id}/link", s.auth(func(w http.ResponseWriter, r *http.Request) {
		path, exp, err := s.Svc.MintLink(r.Context(), actorOf(r), r.PathValue("id"), r.URL.Query().Get("thumb") == "1")
		if err == nil && s.BlobOrigin != "" {
			path = strings.TrimRight(s.BlobOrigin, "/") + path
		}
		s.reply(w, map[string]any{"url": path, "expires": exp}, err)
	}))
	// authenticated download for the UI itself (checks the permission on every request)
	s.handle(mux, "GET /api/v1/attachments/{id}/download", s.auth(func(w http.ResponseWriter, r *http.Request) {
		d, err := s.Svc.FileInfo(r.Context(), actorOf(r), r.PathValue("id"), r.URL.Query().Get("thumb") == "1")
		s.serveFile(w, r, d, err)
	}))
	// signed link: no cookie, so it can be served from a separate origin
	s.handle(mux, "GET /files/{id}", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		d, err := s.Svc.VerifyLink(r.Context(), r.PathValue("id"), q.Get("exp"), q.Get("sig"), q.Get("v"))
		s.serveFile(w, r, d, err)
	})
}

// serveFile streams with range requests and ETag (http.ServeContent). Only safe types preview inline;
// everything else is forced to download, with nosniff and a sandboxing CSP.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, d domain.DownloadInfo, err error) {
	var nd *domain.NotDownloadable
	switch {
	case errors.As(err, &nd):
		s.fail(w, 423, "not_downloadable", "this file is "+nd.State+" and cannot be downloaded", nil)
		return
	case err != nil:
		s.replyStatus(w, 200, nil, err)
		return
	}
	f, err := s.Svc.OpenBlob(d.SHA256)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			s.fail(w, 404, "blob_missing", "the stored file is missing; an administrator should run fsck", nil)
			return
		}
		s.fail(w, 500, "internal", "internal error", nil)
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'")
	h.Set("Cache-Control", "private, max-age=300")
	h.Set("ETag", `"`+d.SHA256+`"`)
	ct := d.MIME
	disp := "attachment"
	if d.Inline {
		disp = "inline"
	} else {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Disposition", disp+"; filename*=UTF-8''"+url.PathEscape(d.Name))
	http.ServeContent(w, r, "", d.Created, f)
}
