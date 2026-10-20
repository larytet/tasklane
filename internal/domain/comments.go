package domain

import (
	"context"
	"github.com/jackc/pgx/v5"
	"sort"
	"strings"
	"time"
)

type Comment struct {
	ID       string     `json:"id"`
	Author   User       `json:"author"`
	Body     string     `json:"body"`
	Internal bool       `json:"internal"`
	Created  time.Time  `json:"created"`
	Edited   *time.Time `json:"edited,omitempty"`
	ViaEmail bool       `json:"via_email,omitempty"` // posted from an email; an administrator can open the original
	// Reactions are filled for the issue view (the person reading decides Mine)
	Reactions []Reaction `json:"reactions,omitempty"`
}

// listComments returns the thread; internal comments only for people who may see them (write access), so a viewer never reads them.
func (s *Service) listComments(ctx context.Context, issueID string, internalToo bool) ([]Comment, error) {
	rows, err := s.DB.Query(ctx, `SELECT c.id,u.id,u.name,u.email,c.body,c.internal,c.created_at,c.edited_at,c.mail_inbound_id IS NOT NULL FROM comment c JOIN app_user u ON u.id=c.author_id WHERE c.issue_id=$1 AND ($2 OR NOT c.internal) ORDER BY c.created_at,c.id`, issueID, internalToo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var c Comment
		rows.Scan(&c.ID, &c.Author.ID, &c.Author.Name, &c.Author.Email, &c.Body, &c.Internal, &c.Created, &c.Edited, &c.ViaEmail)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// a customer's own comments on a request they raised through the portal, in the same thread
	cr, err := s.DB.Query(ctx, `SELECT cc.id::text,cc.customer_id::text,COALESCE(NULLIF(c.name,''),c.email),c.email,cc.body,cc.created_at FROM customer_comment cc JOIN customer c ON c.id=cc.customer_id WHERE cc.issue_id=$1 ORDER BY cc.created_at`, issueID)
	if err != nil {
		return nil, err
	}
	defer cr.Close()
	added := false
	for cr.Next() {
		var c Comment
		cr.Scan(&c.ID, &c.Author.ID, &c.Author.Name, &c.Author.Email, &c.Body, &c.Created)
		c.Author.Name += " (customer)"
		c.Author.ID = "customer:" + c.Author.ID
		out = append(out, c)
		added = true
	}
	if added {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	}
	return out, cr.Err()
}

// AddComment never conflicts with other comments (SPEC 4.5).
func (s *Service) AddComment(ctx context.Context, a Actor, key, body string, internal bool) (Comment, error) {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return Comment{}, err
	}
	if is.Archived != nil || !s.canIn(ctx, a, is.Project, role, "comment") { // archived work stays read-only whatever a scheme says
		return Comment{}, ErrForbidden
	}
	endWrite, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return Comment{}, perr
	}
	defer endWrite()
	body, _ = Sanitize(body)
	if strings.TrimSpace(body) == "" {
		return Comment{}, invalid("comment is empty")
	}
	if err := checkSize(body); err != nil {
		return Comment{}, err
	}
	var c Comment
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO comment(issue_id,author_id,body,internal) VALUES($1,$2,$3,$4) RETURNING id,created_at`, is.ID, a.ID, body, internal).Scan(&c.ID, &c.Created); err != nil {
			return err
		}
		tx.Exec(ctx, `DELETE FROM draft WHERE user_id=$1 AND issue_id=$2 AND field='comment'`, a.ID, is.ID)
		if a.Kind != "agent" { // a person's reply answers an agent's question and lets the agent carry on
			if _, err := tx.Exec(ctx, `UPDATE issue SET input_requested_at=NULL,input_requested_by=NULL,input_question=NULL WHERE id=$1 AND input_requested_at IS NOT NULL`, is.ID); err != nil {
				return err
			}
		}
		return outbox(ctx, tx, "issue:"+is.Key, "comment.added", map[string]any{"key": is.Key, "comment": c.ID, "actor": a.Label, "actor_id": a.ID})
	})
	s.touchLease(ctx, a, is.ID)
	if err == nil {
		s.slaTouch(ctx, is.ID) // a first public reply meets the first response goal
	}
	c.Author = User{ID: a.ID, Name: a.Label}
	c.Body, c.Internal = body, internal
	if err == nil {
		s.emit(Event{Type: "comment.added", Issue: is.Key, Actor: a.Label, ActorID: a.ID})
	}
	return c, err
}

// EditComment lets an author change their own comment; it conflicts with nobody.
func (s *Service) EditComment(ctx context.Context, a Actor, key, commentID, body string) error {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return err
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	endWrite, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return perr
	}
	defer endWrite()
	body, _ = Sanitize(body)
	if strings.TrimSpace(body) == "" {
		return invalid("comment is empty")
	}
	tag, err := s.DB.Exec(ctx, `UPDATE comment SET body=$1,edited_at=now() WHERE id=$2 AND issue_id=$3 AND author_id=$4`, body, commentID, is.ID, a.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrForbidden
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO outbox(aggregate,type,payload) VALUES($1,'comment.edited',$2)`, "issue:"+is.Key, `{"key":"`+is.Key+`"}`)
	return err
}
