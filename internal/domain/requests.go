package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Request types and portal requests (SPEC 15, 15.1). A request type is a form the portal offers: a name, help text, the issue type
// the request becomes, and the custom fields it asks for. A request is an ordinary issue (so staff work it with everything the tracker
// has) with a customer attached. What a customer sees is a short list: the summary, their own description, the status, the form's
// fields, the comments that were not marked internal, and their own comments. Nothing else of the issue is sent: not who it is
// assigned to, not internal comments or fields, work logs, approvals, links, development data, history or other issues.

type RequestField struct {
	Key      string `json:"key"` // customfield_<id>
	Label    string `json:"label"`
	Help     string `json:"help,omitempty"`
	Required bool   `json:"required"`
	// filled in for the portal form only
	Type    string   `json:"type,omitempty"`
	Options []string `json:"options,omitempty"`
}

type RequestType struct {
	ID          string         `json:"id"`
	Project     string         `json:"project"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Help        string         `json:"help_text"`
	IssueType   string         `json:"issue_type"`
	Fields      []RequestField `json:"fields"`
	Visible     bool           `json:"visible"`
	Position    int            `json:"position"`
	SLA         SLAPolicy      `json:"sla"`
}

type RequestTypeIn struct {
	Name        *string         `json:"name"`
	Description *string         `json:"description"`
	Help        *string         `json:"help_text"`
	IssueType   *string         `json:"issue_type"`
	Fields      *[]RequestField `json:"fields"`
	Visible     *bool           `json:"visible"`
	Position    *int            `json:"position"`
	SLA         *SLAPolicy      `json:"sla"`
}

const rtCols = `rt.id::text, p.key, rt.name, rt.description, rt.help_text, it.name, rt.fields, rt.visible, rt.position, rt.sla`

func scanRT(row pgx.Row) (RequestType, error) {
	var r RequestType
	var raw, sla []byte
	if err := row.Scan(&r.ID, &r.Project, &r.Name, &r.Description, &r.Help, &r.IssueType, &raw, &r.Visible, &r.Position, &sla); err != nil {
		return r, err
	}
	json.Unmarshal(sla, &r.SLA)
	json.Unmarshal(raw, &r.Fields)
	if r.Fields == nil {
		r.Fields = []RequestField{}
	}
	return r, nil
}

const rtFrom = ` FROM request_type rt JOIN project p ON p.id=rt.project_id JOIN issue_type it ON it.id=rt.issue_type_id `

// ListRequestTypes lists a project's request types for people who can see the project.
func (s *Service) ListRequestTypes(ctx context.Context, a Actor, projectKey string) ([]RequestType, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	if s.role(ctx, a, pid) == "" {
		return nil, ErrNotFound
	}
	return s.requestTypes(ctx, pid, false)
}

func (s *Service) requestTypes(ctx context.Context, pid string, onlyVisible bool) ([]RequestType, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+rtCols+rtFrom+`WHERE rt.project_id=$1 AND ($2 OR rt.visible) ORDER BY rt.position, lower(rt.name)`, pid, !onlyVisible)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RequestType{}
	for rows.Next() {
		r, err := scanRT(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) requestTypeAdmin(ctx context.Context, a Actor, projectKey string) (string, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return "", err
	}
	switch s.role(ctx, a, pid) {
	case "":
		return "", ErrNotFound
	case "admin":
		return pid, nil
	}
	return "", ErrForbidden
}

// checkRequestFields: every field named exists and applies to the issue type in this project; labels and help are short.
func (s *Service) checkRequestFields(ctx context.Context, pid, typeID string, fields []RequestField) ([]RequestField, error) {
	if len(fields) > 20 {
		return nil, invalid("a form asks for at most 20 fields")
	}
	cfgs, err := s.FieldsFor(ctx, pid, typeID)
	if err != nil {
		return nil, err
	}
	byKey := map[string]FieldConfig{}
	for _, c := range cfgs {
		byKey[c.Key] = c
	}
	seen := map[string]bool{}
	out := []RequestField{}
	for _, f := range fields {
		id, ok := parseFieldKey(f.Key)
		key := fieldKey(id)
		c, found := byKey[key]
		if !ok || !found {
			return nil, invalid("the field %q does not apply to this issue type in this project", f.Key)
		}
		if seen[key] {
			return nil, invalid("the field %q is listed twice", f.Key)
		}
		seen[key] = true
		switch c.Type {
		case "text", "textarea", "number", "select", "radio", "url", "date", "checkbox":
		default:
			return nil, invalid("a form cannot ask for a %s field (%s)", c.Type, c.Name)
		}
		label := strings.TrimSpace(f.Label)
		if label == "" {
			label = c.Name
		}
		if len([]rune(label)) > 100 || len([]rune(f.Help)) > 500 {
			return nil, invalid("a label is at most 100 characters and help at most 500")
		}
		out = append(out, RequestField{Key: key, Label: label, Help: f.Help, Required: f.Required})
	}
	return out, nil
}

func (s *Service) issueTypeID(ctx context.Context, name string) (string, error) {
	var id string
	if err := s.DB.QueryRow(ctx, `SELECT id::text FROM issue_type WHERE lower(name)=lower($1) AND level >= 0`, strings.TrimSpace(name)).Scan(&id); err != nil {
		return "", invalid("unknown issue type %q (a request is not a subtask)", name)
	}
	return id, nil
}

func rtError(err error) error {
	if err != nil && strings.Contains(err.Error(), "request_type_name_uq") {
		return invalid("this project already has a request type with that name")
	}
	return err
}

// CreateRequestType adds a form to a project's portal (project administrators).
func (s *Service) CreateRequestType(ctx context.Context, a Actor, projectKey string, in RequestTypeIn) (RequestType, error) {
	pid, err := s.requestTypeAdmin(ctx, a, projectKey)
	if err != nil {
		return RequestType{}, err
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" || len([]rune(*in.Name)) > 100 {
		return RequestType{}, invalid("a request type needs a name of at most 100 characters")
	}
	typeName := "Task"
	if in.IssueType != nil && *in.IssueType != "" {
		typeName = *in.IssueType
	}
	typeID, err := s.issueTypeID(ctx, typeName)
	if err != nil {
		return RequestType{}, err
	}
	var fields []RequestField
	if in.Fields != nil {
		if fields, err = s.checkRequestFields(ctx, pid, typeID, *in.Fields); err != nil {
			return RequestType{}, err
		}
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if len([]rune(str(in.Description))) > 1000 || len([]rune(str(in.Help))) > 2000 {
		return RequestType{}, invalid("the description is at most 1000 characters and the help at most 2000")
	}
	raw, _ := json.Marshal(append([]RequestField{}, fields...))
	var policy SLAPolicy
	if in.SLA != nil {
		if policy, err = s.checkSLAPolicy(ctx, pid, *in.SLA); err != nil {
			return RequestType{}, err
		}
	}
	slaRaw, _ := json.Marshal(policy)
	vis := in.Visible == nil || *in.Visible
	pos := 0
	if in.Position != nil {
		pos = *in.Position
	}
	var id string
	if err := s.DB.QueryRow(ctx, `INSERT INTO request_type(project_id,name,description,help_text,issue_type_id,fields,visible,position,sla) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id::text`,
		pid, strings.TrimSpace(*in.Name), str(in.Description), str(in.Help), typeID, raw, vis, pos, slaRaw).Scan(&id); err != nil {
		return RequestType{}, rtError(err)
	}
	return s.requestTypeByID(ctx, id)
}

func (s *Service) requestTypeByID(ctx context.Context, id string) (RequestType, error) {
	r, err := scanRT(s.DB.QueryRow(ctx, `SELECT `+rtCols+rtFrom+`WHERE rt.id::text=$1`, id))
	if err != nil {
		return r, ErrNotFound // also a malformed id
	}
	return r, nil
}

// UpdateRequestType changes a form (project administrators).
func (s *Service) UpdateRequestType(ctx context.Context, a Actor, id string, in RequestTypeIn) (RequestType, error) {
	cur, err := s.requestTypeByID(ctx, id)
	if err != nil {
		return cur, err
	}
	pid, err := s.requestTypeAdmin(ctx, a, cur.Project)
	if err != nil {
		return cur, err
	}
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" || len([]rune(*in.Name)) > 100 {
			return cur, invalid("a request type needs a name of at most 100 characters")
		}
		cur.Name = strings.TrimSpace(*in.Name)
	}
	if in.Description != nil {
		cur.Description = *in.Description
	}
	if in.Help != nil {
		cur.Help = *in.Help
	}
	if in.IssueType != nil {
		cur.IssueType = *in.IssueType
	}
	if in.Visible != nil {
		cur.Visible = *in.Visible
	}
	if in.Position != nil {
		cur.Position = *in.Position
	}
	if len([]rune(cur.Description)) > 1000 || len([]rune(cur.Help)) > 2000 {
		return cur, invalid("the description is at most 1000 characters and the help at most 2000")
	}
	typeID, err := s.issueTypeID(ctx, cur.IssueType)
	if err != nil {
		return cur, err
	}
	fields := cur.Fields
	if in.Fields != nil {
		fields = *in.Fields
	}
	if fields, err = s.checkRequestFields(ctx, pid, typeID, fields); err != nil {
		return cur, err
	}
	raw, _ := json.Marshal(append([]RequestField{}, fields...))
	if in.SLA != nil {
		if cur.SLA, err = s.checkSLAPolicy(ctx, pid, *in.SLA); err != nil {
			return cur, err
		}
	}
	slaRaw, _ := json.Marshal(cur.SLA)
	if _, err := s.DB.Exec(ctx, `UPDATE request_type SET name=$2,description=$3,help_text=$4,issue_type_id=$5,fields=$6,visible=$7,position=$8,sla=$9 WHERE id::text=$1`,
		id, cur.Name, cur.Description, cur.Help, typeID, raw, cur.Visible, cur.Position, slaRaw); err != nil {
		return cur, rtError(err)
	}
	return s.requestTypeByID(ctx, id)
}

// DeleteRequestType removes a form; requests already raised through it stay (they are issues) and just lose their type.
func (s *Service) DeleteRequestType(ctx context.Context, a Actor, id string) error {
	cur, err := s.requestTypeByID(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.requestTypeAdmin(ctx, a, cur.Project); err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM request_type WHERE id::text=$1`, id)
	return err
}

// ---- the portal side ----

// portalActor is the account that stands as the reporter of requests a customer raises: one hidden, passwordless account per
// organization (kind portal) that is a member of the portal's project. It never appears in lists of people and cannot sign in.
func (s *Service) portalActor(ctx context.Context, org, pid string) (Actor, error) {
	var id string
	err := s.DB.QueryRow(ctx, `SELECT id::text FROM app_user WHERE kind='portal' AND org_id::text=$1`, org).Scan(&id)
	if err == pgx.ErrNoRows {
		err = s.DB.QueryRow(ctx, `INSERT INTO app_user(email,name,kind,org_id) VALUES($1,'Customer portal','portal',$2) ON CONFLICT (email) DO UPDATE SET name=app_user.name RETURNING id::text`,
			"portal+"+org[:8]+"@portal.invalid", org).Scan(&id)
	}
	if err != nil {
		return Actor{}, err
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role) VALUES($1,$2,'member') ON CONFLICT DO NOTHING`, pid, id); err != nil {
		return Actor{}, err
	}
	return Actor{ID: id, Label: "Customer portal", Kind: "portal", Org: org}, nil
}

// portalProject checks the customer belongs to the organization that owns an enabled portal and returns its project.
func (s *Service) portalProject(ctx context.Context, c Customer, projectKey string) (pid string, err error) {
	org, pid, _, _, err := s.portalOf(ctx, projectKey)
	if err != nil || org != c.OrgID {
		return "", ErrNotFound
	}
	return pid, nil
}

// PortalRequestTypes is what the portal's "new request" page offers a signed-in customer: the visible forms and their fields.
func (s *Service) PortalRequestTypes(ctx context.Context, c Customer, projectKey string) ([]RequestType, error) {
	pid, err := s.portalProject(ctx, c, projectKey)
	if err != nil {
		return nil, err
	}
	rts, err := s.requestTypes(ctx, pid, true)
	if err != nil {
		return nil, err
	}
	for i := range rts {
		typeID, _ := s.issueTypeID(ctx, rts[i].IssueType)
		cfgs, _ := s.FieldsFor(ctx, pid, typeID)
		for j, f := range rts[i].Fields {
			for _, c := range cfgs {
				if c.Key == f.Key {
					rts[i].Fields[j].Type = c.Type
					for _, o := range c.Options {
						if !o.Archived {
							rts[i].Fields[j].Options = append(rts[i].Fields[j].Options, o.Value)
						}
					}
				}
			}
		}
		rts[i].IssueType = "" // an internal word: the customer is not told
	}
	return rts, nil
}

type PortalRequestIn struct {
	Type        string         `json:"request_type"`
	Summary     string         `json:"summary"`
	Description string         `json:"description"`
	Fields      map[string]any `json:"fields"`
	Website     string         `json:"website"` // the honeypot
}

// PortalRequest is a request as its customer sees it.
type PortalRequest struct {
	Key         string          `json:"key"`
	Summary     string          `json:"summary"`
	Description string          `json:"description,omitempty"`
	Status      string          `json:"status"`
	Resolved    bool            `json:"resolved"`
	Type        string          `json:"request_type,omitempty"`
	Created     time.Time       `json:"created"`
	Updated     time.Time       `json:"updated"`
	Fields      []PortalField   `json:"fields,omitempty"`
	Comments    []PortalComment `json:"comments,omitempty"`
}

type PortalField struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type PortalComment struct {
	Author   string    `json:"author"` // "You", or "Support"; never a staff member's name
	Body     string    `json:"body"`
	Created  time.Time `json:"created"`
	Customer bool      `json:"from_customer"`
}

const portalRequestsPerHour = 10

// PortalCreateRequest raises a request through a form.
func (s *Service) PortalCreateRequest(ctx context.Context, c Customer, projectKey string, in PortalRequestIn, meta ClientMeta) (PortalRequest, error) {
	pid, err := s.portalProject(ctx, c, projectKey)
	if err != nil {
		return PortalRequest{}, err
	}
	if in.Website != "" { // the honeypot: a request that looks accepted and is dropped
		return PortalRequest{Key: "", Summary: in.Summary, Status: "Received", Created: s.Now(), Updated: s.Now()}, nil
	}
	if s.portalCount(ctx, "request", c.ID, time.Hour) >= portalRequestsPerHour {
		return PortalRequest{}, &LockedError{Until: s.Now().Add(time.Hour)}
	}
	rt, err := s.requestTypeByID(ctx, in.Type)
	if err != nil || rt.Project != strings.ToUpper(projectKey) || !rt.Visible {
		return PortalRequest{}, invalid("choose what you need help with")
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return PortalRequest{}, invalid("tell us what this is about in a few words")
	}
	typeID, _ := s.issueTypeID(ctx, rt.IssueType)
	cfgs, _ := s.FieldsFor(ctx, pid, typeID)
	cfgByKey := map[string]FieldConfig{}
	for _, f := range cfgs {
		cfgByKey[f.Key] = f
	}
	custom := map[string]any{}
	for _, f := range rt.Fields {
		raw, given := in.Fields[f.Key]
		if !given || isEmptyValue(raw) {
			if f.Required {
				return PortalRequest{}, invalid("%s is required", f.Label)
			}
			continue
		}
		custom[f.Key] = raw
	}
	for k := range in.Fields { // only what the form asks for: a client cannot set other fields
		found := false
		for _, f := range rt.Fields {
			found = found || f.Key == k
		}
		if !found {
			return PortalRequest{}, invalid("this form has no field %q", k)
		}
	}
	actor, err := s.portalActor(ctx, c.OrgID, pid)
	if err != nil {
		return PortalRequest{}, err
	}
	is, err := s.CreateIssue(ctx, actor, CreateIn{Project: strings.ToUpper(projectKey), Type: rt.IssueType, Summary: summary, Description: in.Description, Custom: custom})
	if err != nil {
		var inv *InvalidError
		if asInvalid(err, &inv) {
			return PortalRequest{}, err
		}
		return PortalRequest{}, fmt.Errorf("the request could not be created: %w", err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO portal_request(issue_id,customer_id,request_type_id) VALUES($1,$2,$3)`, is.ID, c.ID, rt.ID); err != nil {
		return PortalRequest{}, err
	}
	s.portalNote(ctx, "request", c.ID)
	if !rt.SLA.empty() {
		if err := s.startClocks(ctx, is.ID, rt.SLA, s.Now()); err != nil {
			return PortalRequest{}, err
		}
	}
	return s.PortalGetRequest(ctx, c, projectKey, is.Key)
}

func asInvalid(err error, target **InvalidError) bool {
	for err != nil {
		if e, ok := err.(*InvalidError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// PortalMyRequests lists the customer's own requests, newest activity first.
func (s *Service) PortalMyRequests(ctx context.Context, c Customer, projectKey string) ([]PortalRequest, error) {
	pid, err := s.portalProject(ctx, c, projectKey)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT i.key,i.summary,st.name,st.category='done',COALESCE(rt.name,''),i.created_at,i.updated_at
		FROM portal_request pr JOIN issue i ON i.id=pr.issue_id AND i.deleted_at IS NULL AND i.project_id=$2 JOIN status st ON st.id=i.status_id LEFT JOIN request_type rt ON rt.id=pr.request_type_id
		WHERE pr.customer_id::text=$1 ORDER BY i.updated_at DESC LIMIT 200`, c.ID, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PortalRequest{}
	for rows.Next() {
		var r PortalRequest
		if err := rows.Scan(&r.Key, &r.Summary, &r.Status, &r.Resolved, &r.Type, &r.Created, &r.Updated); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PortalGetRequest is one request, only if it is the customer's own: any other key, and a key that does not exist, answer the same.
func (s *Service) PortalGetRequest(ctx context.Context, c Customer, projectKey, key string) (PortalRequest, error) {
	pid, err := s.portalProject(ctx, c, projectKey)
	if err != nil {
		return PortalRequest{}, err
	}
	var r PortalRequest
	var id, rtID string
	pidIssue := ""
	err = s.DB.QueryRow(ctx, `SELECT i.id::text,i.key,i.summary,st.name,st.category='done',COALESCE(rt.name,''),COALESCE(rt.id::text,''),i.created_at,i.updated_at,COALESCE(t.body,'')
		FROM portal_request pr JOIN issue i ON i.id=pr.issue_id AND i.deleted_at IS NULL AND i.project_id=$3 JOIN status st ON st.id=i.status_id LEFT JOIN request_type rt ON rt.id=pr.request_type_id
		LEFT JOIN issue_text t ON t.issue_id=i.id AND t.field='description'
		WHERE pr.customer_id::text=$1 AND i.key=$2`, c.ID, strings.ToUpper(key), pid).Scan(&id, &r.Key, &r.Summary, &r.Status, &r.Resolved, &r.Type, &rtID, &r.Created, &r.Updated, &r.Description)
	if err != nil {
		return PortalRequest{}, ErrNotFound
	}
	pidIssue = id
	if rtID != "" { // the form's fields, with the labels the customer saw
		if rt, e := s.requestTypeByID(ctx, rtID); e == nil {
			for _, f := range rt.Fields {
				if id, ok := parseFieldKey(f.Key); ok {
					var raw []byte
					if s.DB.QueryRow(ctx, `SELECT value FROM issue_field_value WHERE issue_id::text=$1 AND field_id=$2`, pidIssue, id).Scan(&raw) == nil {
						var v any
						json.Unmarshal(raw, &v)
						r.Fields = append(r.Fields, PortalField{Label: f.Label, Value: plainValue(v)})
					}
				}
			}
		}
	}
	// comments: staff comments that were not marked internal (as "Support"), and the customer's own
	rows, err := s.DB.Query(ctx, `SELECT 'Support', c.body, c.created_at, false FROM comment c WHERE c.issue_id::text=$1 AND NOT c.internal
		UNION ALL SELECT 'You', cc.body, cc.created_at, true FROM customer_comment cc WHERE cc.issue_id::text=$1 AND cc.customer_id::text=$2 ORDER BY 3`, id, c.ID)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var pc PortalComment
		rows.Scan(&pc.Author, &pc.Body, &pc.Created, &pc.Customer)
		r.Comments = append(r.Comments, pc)
	}
	return r, rows.Err()
}

func plainValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, plainValue(e))
		}
		sort.Strings(parts)
		return strings.Join(parts, ", ")
	case float64:
		return fmt.Sprint(x)
	case bool:
		if x {
			return "yes"
		}
		return "no"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// PortalComment adds the customer's comment to their own request; staff are told.
func (s *Service) PortalComment(ctx context.Context, c Customer, projectKey, key, body string) (PortalRequest, error) {
	r, err := s.PortalGetRequest(ctx, c, projectKey, key)
	if err != nil {
		return r, err
	}
	body, _ = Sanitize(strings.TrimSpace(body))
	if body == "" {
		return r, invalid("write something first")
	}
	if len([]rune(body)) > 10000 {
		return r, invalid("that is too long (at most 10000 characters)")
	}
	if s.portalCount(ctx, "comment", c.ID, time.Hour) >= 60 {
		return r, &LockedError{Until: s.Now().Add(time.Hour)}
	}
	end, perr := s.admit(ctx, strings.ToUpper(projectKey))
	if perr != nil {
		return r, perr
	}
	defer end()
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var iid string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM issue WHERE key=$1`, r.Key).Scan(&iid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO customer_comment(issue_id,customer_id,body) VALUES($1,$2,$3)`, iid, c.ID, body); err != nil {
			return err
		}
		who := c.Name
		if who == "" {
			who = c.Email
		}
		return outbox(ctx, tx, "issue:"+r.Key, "customer.commented", map[string]any{"key": r.Key, "actor": who + " (customer)", "actor_id": "", "text": body})
	})
	if err != nil {
		return r, err
	}
	s.portalNote(ctx, "comment", c.ID)
	s.emit(Event{Type: "comment.added", Issue: r.Key, Actor: c.Email})
	return s.PortalGetRequest(ctx, c, projectKey, key)
}

// RequestInfo is what staff see on a request: who raised it and through which form.
type RequestInfo struct {
	Customer string `json:"customer"`
	Email    string `json:"email"`
	Type     string `json:"request_type,omitempty"`
}

func (s *Service) requestInfo(ctx context.Context, issueID string) *RequestInfo {
	var r RequestInfo
	err := s.DB.QueryRow(ctx, `SELECT c.name,c.email,COALESCE(rt.name,'') FROM portal_request pr JOIN customer c ON c.id=pr.customer_id LEFT JOIN request_type rt ON rt.id=pr.request_type_id WHERE pr.issue_id::text=$1`, issueID).Scan(&r.Customer, &r.Email, &r.Type)
	if err != nil {
		return nil
	}
	return &r
}
