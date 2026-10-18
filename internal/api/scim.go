package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/larytet/tasklane/internal/domain"
)

const scimUser = "urn:ietf:params:scim:schemas:core:2.0:User"
const scimList = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
const scimErr = "urn:ietf:params:scim:api:messages:2.0:Error"

func (s *Server) scimJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) scimFail(w http.ResponseWriter, code int, scimType, detail string) {
	body := map[string]any{"schemas": []string{scimErr}, "status": strconv.Itoa(code), "detail": detail}
	if scimType != "" {
		body["scimType"] = scimType
	}
	s.scimJSON(w, code, body)
}

func (s *Server) scimErrFrom(w http.ResponseWriter, err error) {
	var inv *domain.InvalidError
	var conflict *domain.ConflictError
	switch {
	case errors.Is(err, domain.ErrNotFound):
		s.scimFail(w, 404, "", "not found")
	case errors.As(err, &conflict):
		s.scimFail(w, 409, "uniqueness", conflict.Error())
	case errors.As(err, &inv):
		s.scimFail(w, 400, "invalidValue", inv.Error())
	default:
		s.scimFail(w, 500, "", "internal error")
	}
}

func (s *Server) scimAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.Svc.AuthenticateSCIM(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); err != nil {
			s.scimFail(w, 401, "", "a SCIM token is required")
			return
		}
		next(w, r)
	}
}

func scimRender(r *http.Request, u domain.SCIMUser) map[string]any {
	given, family := u.DisplayName, ""
	if i := strings.LastIndex(u.DisplayName, " "); i > 0 {
		given, family = u.DisplayName[:i], u.DisplayName[i+1:]
	}
	m := map[string]any{
		"schemas": []string{scimUser}, "id": u.ID, "userName": u.UserName, "displayName": u.DisplayName, "active": u.Active,
		"name":   map[string]any{"formatted": u.DisplayName, "givenName": given, "familyName": family},
		"emails": []map[string]any{{"value": u.UserName, "primary": true}},
		"meta":   map[string]any{"resourceType": "User", "created": u.Created.UTC().Format("2006-01-02T15:04:05Z"), "location": "/scim/v2/Users/" + u.ID},
	}
	if u.ExternalID != "" {
		m["externalId"] = u.ExternalID
	}
	return m
}

type scimBody struct {
	UserName    string `json:"userName"`
	ExternalID  string `json:"externalId"`
	DisplayName string `json:"displayName"`
	Active      *bool  `json:"active"`
	Name        struct {
		Formatted  string `json:"formatted"`
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
	} `json:"name"`
	Emails []struct {
		Value   string `json:"value"`
		Primary bool   `json:"primary"`
	} `json:"emails"`
}

func (b scimBody) toIn() domain.SCIMIn {
	in := domain.SCIMIn{UserName: b.UserName, ExternalID: b.ExternalID, DisplayName: b.DisplayName, Active: b.Active}
	if in.UserName == "" && len(b.Emails) > 0 {
		in.UserName = b.Emails[0].Value
		for _, e := range b.Emails {
			if e.Primary {
				in.UserName = e.Value
			}
		}
	}
	if in.DisplayName == "" {
		in.DisplayName = strings.TrimSpace(b.Name.Formatted)
		if in.DisplayName == "" {
			in.DisplayName = strings.TrimSpace(b.Name.GivenName + " " + b.Name.FamilyName)
		}
	}
	return in
}

var scimFilterRe = regexp.MustCompile(`^\s*(userName|externalId)\s+eq\s+"([^"]*)"\s*$`)

func (s *Server) registerSCIM(mux *http.ServeMux) {
	s.handle(mux, "GET /scim/v2/ServiceProviderConfig", s.scimAuth(func(w http.ResponseWriter, r *http.Request) {
		s.scimJSON(w, 200, map[string]any{
			"schemas":               []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
			"patch":                 map[string]bool{"supported": true},
			"bulk":                  map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
			"filter":                map[string]any{"supported": true, "maxResults": 100},
			"changePassword":        map[string]bool{"supported": false},
			"sort":                  map[string]bool{"supported": false},
			"etag":                  map[string]bool{"supported": false},
			"authenticationSchemes": []map[string]any{{"type": "oauthbearertoken", "name": "Bearer token", "description": "A SCIM token created by an administrator"}},
		})
	}))
	s.handle(mux, "GET /scim/v2/Users", s.scimAuth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		attr, val := "", ""
		if f := q.Get("filter"); f != "" {
			m := scimFilterRe.FindStringSubmatch(f)
			if m == nil {
				s.scimFail(w, 400, "invalidFilter", `only filters of the form userName eq "..." or externalId eq "..." are supported`)
				return
			}
			attr, val = m[1], m[2]
		}
		start, _ := strconv.Atoi(q.Get("startIndex"))
		count := 100
		if c := q.Get("count"); c != "" {
			count, _ = strconv.Atoi(c)
		}
		us, total, err := s.Svc.SCIMList(r.Context(), attr, val, start, count)
		if err != nil {
			s.scimErrFrom(w, err)
			return
		}
		res := make([]map[string]any, len(us))
		for i, u := range us {
			res[i] = scimRender(r, u)
		}
		if start < 1 {
			start = 1
		}
		s.scimJSON(w, 200, map[string]any{"schemas": []string{scimList}, "totalResults": total, "startIndex": start, "itemsPerPage": len(res), "Resources": res})
	}))
	s.handle(mux, "POST /scim/v2/Users", s.scimAuth(func(w http.ResponseWriter, r *http.Request) {
		var b scimBody
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&b) != nil {
			s.scimFail(w, 400, "invalidSyntax", "the body is not valid JSON")
			return
		}
		u, created, err := s.Svc.SCIMCreate(r.Context(), b.toIn())
		if err != nil {
			s.scimErrFrom(w, err)
			return
		}
		code := 200
		if created {
			code = 201
		}
		s.scimJSON(w, code, scimRender(r, u))
	}))
	s.handle(mux, "GET /scim/v2/Users/{id}", s.scimAuth(func(w http.ResponseWriter, r *http.Request) {
		u, err := s.Svc.SCIMGet(r.Context(), r.PathValue("id"))
		if err != nil {
			s.scimErrFrom(w, err)
			return
		}
		s.scimJSON(w, 200, scimRender(r, u))
	}))
	s.handle(mux, "PUT /scim/v2/Users/{id}", s.scimAuth(func(w http.ResponseWriter, r *http.Request) {
		var b scimBody
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&b) != nil {
			s.scimFail(w, 400, "invalidSyntax", "the body is not valid JSON")
			return
		}
		u, err := s.Svc.SCIMUpdate(r.Context(), r.PathValue("id"), b.toIn())
		if err != nil {
			s.scimErrFrom(w, err)
			return
		}
		s.scimJSON(w, 200, scimRender(r, u))
	}))
	s.handle(mux, "PATCH /scim/v2/Users/{id}", s.scimAuth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Operations []struct {
				Op    string          `json:"op"`
				Path  string          `json:"path"`
				Value json.RawMessage `json:"value"`
			} `json:"Operations"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body) != nil || len(body.Operations) == 0 {
			s.scimFail(w, 400, "invalidSyntax", "a PATCH needs Operations")
			return
		}
		var in domain.SCIMIn
		set := func(path string, raw json.RawMessage) {
			var str string
			var bl bool
			switch strings.ToLower(path) {
			case "active":
				if json.Unmarshal(raw, &bl) == nil {
					in.Active = &bl
				} else if json.Unmarshal(raw, &str) == nil {
					bl = strings.EqualFold(str, "true")
					in.Active = &bl
				}
			case "username":
				json.Unmarshal(raw, &in.UserName)
			case "displayname", "name.formatted":
				json.Unmarshal(raw, &in.DisplayName)
			case "externalid":
				json.Unmarshal(raw, &in.ExternalID)
			}
		}
		for _, op := range body.Operations {
			if o := strings.ToLower(op.Op); o != "replace" && o != "add" {
				s.scimFail(w, 400, "invalidValue", "only replace and add are supported")
				return
			}
			if op.Path != "" {
				set(op.Path, op.Value)
				continue
			}
			var m map[string]json.RawMessage // the form without a path: {"op":"replace","value":{"active":false}}
			if json.Unmarshal(op.Value, &m) == nil {
				for k, v := range m {
					set(k, v)
				}
			}
		}
		u, err := s.Svc.SCIMUpdate(r.Context(), r.PathValue("id"), in)
		if err != nil {
			s.scimErrFrom(w, err)
			return
		}
		s.scimJSON(w, 200, scimRender(r, u))
	}))
	s.handle(mux, "DELETE /scim/v2/Users/{id}", s.scimAuth(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Svc.SCIMDelete(r.Context(), r.PathValue("id")); err != nil {
			s.scimErrFrom(w, err)
			return
		}
		w.WriteHeader(204)
	}))
}
