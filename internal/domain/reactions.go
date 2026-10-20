package domain

import (
	"context"
	"strings"
)

// Reactions on comments (SPEC 4.2): a fixed set of eight emoji, one per person per emoji. Only someone who can see the comment
// may react (an internal comment only for those who may read internal comments); archived and mirrored issues are read-only. A
// reaction touches nothing on the issue (no version, no updated time, no history) and notifies nobody; people looking at the
// issue see it live.
var Reactions = []string{"👍", "👎", "😄", "🎉", "😕", "❤️", "🚀", "👀"}

func validReaction(e string) bool {
	for _, r := range Reactions {
		if e == r {
			return true
		}
	}
	return false
}

// Reaction is one emoji on a comment: how many, who (names, for the tooltip), and whether the person reading is one of them.
type Reaction struct {
	Emoji string   `json:"emoji"`
	Count int      `json:"count"`
	Names []string `json:"names"`
	Mine  bool     `json:"mine"`
}

// React adds (on) or removes the person's emoji on a comment of the issue and answers the comment's reactions.
func (s *Service) React(ctx context.Context, a Actor, key, commentID, emoji string, on bool) ([]Reaction, error) {
	if !validReaction(emoji) { // the exact eight: other emoji, text, skin-tone or variation-selector variants and empty values are refused
		return nil, invalid("a reaction is one of %s", strings.Join(Reactions, " "))
	}
	if a.ID == "" || a.Kind == "agent" {
		return nil, ErrForbidden
	}
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return nil, err
	}
	if merr := s.MirrorRefusal(ctx, is.Project); merr != nil {
		return nil, merr
	}
	if is.Archived != nil {
		return nil, invalid("%s is archived and read-only", is.Key)
	}
	var internal bool
	if err := s.DB.QueryRow(ctx, `SELECT internal FROM comment WHERE id::text=$1 AND issue_id=$2`, commentID, is.ID).Scan(&internal); err != nil {
		return nil, ErrNotFound
	}
	if internal && !canWrite(role) { // a comment the person cannot read does not exist for them
		return nil, ErrNotFound
	}
	if on {
		_, err = s.DB.Exec(ctx, `INSERT INTO comment_reaction(comment_id,user_id,emoji) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, commentID, a.ID, emoji)
	} else {
		_, err = s.DB.Exec(ctx, `DELETE FROM comment_reaction WHERE comment_id::text=$1 AND user_id=$2 AND emoji=$3`, commentID, a.ID, emoji)
	}
	if err != nil {
		return nil, err
	}
	s.emit(Event{Type: "comment.reacted", Issue: is.Key, Actor: a.Label, ActorID: a.ID}) // live only: no outbox, so no notification
	m, err := s.reactionsFor(ctx, []string{commentID}, a.ID)
	return m[commentID], err
}

// reactionsFor gives each comment's reactions in the fixed order of the set, as seen by viewerID.
func (s *Service) reactionsFor(ctx context.Context, commentIDs []string, viewerID string) (map[string][]Reaction, error) {
	out := map[string][]Reaction{}
	if len(commentIDs) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT r.comment_id::text, r.emoji, r.user_id::text, u.name FROM comment_reaction r JOIN app_user u ON u.id=r.user_id
		WHERE r.comment_id::text = ANY($1) ORDER BY r.created_at, u.name`, commentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string]map[string]*Reaction{}
	for rows.Next() {
		var cid, emoji, uid, name string
		if rows.Scan(&cid, &emoji, &uid, &name) != nil {
			continue
		}
		if by[cid] == nil {
			by[cid] = map[string]*Reaction{}
		}
		r := by[cid][emoji]
		if r == nil {
			r = &Reaction{Emoji: emoji, Names: []string{}}
			by[cid][emoji] = r
		}
		r.Count++
		r.Names = append(r.Names, name)
		if uid == viewerID {
			r.Mine = true
		}
	}
	for cid, m := range by {
		for _, e := range Reactions {
			if r := m[e]; r != nil {
				out[cid] = append(out[cid], *r)
			}
		}
	}
	return out, rows.Err()
}

// withReactions fills the reactions of a thread's staff comments for the person reading it.
func (s *Service) withReactions(ctx context.Context, cs []Comment, viewerID string) {
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		if !strings.HasPrefix(c.Author.ID, "customer:") {
			ids = append(ids, c.ID)
		}
	}
	m, err := s.reactionsFor(ctx, ids, viewerID)
	if err != nil {
		return
	}
	for i := range cs {
		cs[i].Reactions = m[cs[i].ID]
	}
}
