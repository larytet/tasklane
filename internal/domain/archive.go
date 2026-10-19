package domain

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The issue archive (SPEC 9.1). An archived issue is finished work kept out of the way: read-only for everyone, hidden
// from boards, backlogs and default searches, found by `archived = true`, and still part of historical reports.

// ArchiveIssue archives a finished issue (its status is in the Done category). Members of the project may do it.
func (s *Service) ArchiveIssue(ctx context.Context, a Actor, key string) (Issue, error) {
	return s.setArchived(ctx, a, key, true)
}

// UnarchiveIssue brings an archived issue back.
func (s *Service) UnarchiveIssue(ctx context.Context, a Actor, key string) (Issue, error) {
	return s.setArchived(ctx, a, key, false)
}

func (s *Service) setArchived(ctx context.Context, a Actor, key string, archive bool) (Issue, error) {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return is, err
	}
	// access reports an archived issue as read-only; the person's real role decides who may bring it back
	if is.Archived != nil {
		pid, perr := s.projectID(ctx, is.Project)
		if perr != nil {
			return is, perr
		}
		role = s.role(ctx, a, pid)
	}
	if !canWrite(role) {
		return is, ErrForbidden
	}
	endWrite, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return is, perr
	}
	defer endWrite()
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var cat string
		var cur *time.Time
		var ver int64
		if err := tx.QueryRow(ctx, `SELECT st.category,i.archived_at,i.version FROM issue i JOIN status st ON st.id=i.status_id WHERE i.id=$1 FOR UPDATE OF i`, is.ID).Scan(&cat, &cur, &ver); err != nil {
			return err
		}
		if archive {
			if cur != nil {
				return invalid("%s is already archived", is.Key)
			}
			if cat != "done" {
				return invalid("only finished work is archived: %s is %q, which is not in a Done status", is.Key, is.Status)
			}
		} else if cur == nil {
			return invalid("%s is not archived", is.Key)
		}
		val, label := any(nil), "restored"
		if archive {
			val, label = s.Now(), "archived"
		}
		next := ver + 1
		if _, err := tx.Exec(ctx, `UPDATE issue SET archived_at=$2,version=$3,updated_at=now() WHERE id=$1`, is.ID, val, next); err != nil {
			return err
		}
		before, after := "", label
		if !archive {
			before, after = "archived", ""
		}
		if err := s.hist(ctx, tx, is.ID, a, "archived", &before, &after); err != nil {
			return err
		}
		return outbox(ctx, tx, "issue:"+is.Key, "issue.updated", map[string]any{"key": is.Key, "fields": []string{"archived"}, "version": next, "actor": a.Label, "actor_id": a.ID})
	})
	if err != nil {
		return is, err
	}
	out, gerr := s.getIssue(ctx, key)
	if gerr == nil {
		s.emit(Event{Type: "issue.updated", Issue: out.Key, Fields: []string{"archived"}, Version: out.Version, Actor: a.Label, ActorID: a.ID})
	}
	return out, gerr
}

// SetArchivePolicy sets how many days after resolution a project archives its finished issues (nil turns it off).
func (s *Service) SetArchivePolicy(ctx context.Context, a Actor, projectKey string, days *int) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	if days != nil && (*days < 1 || *days > 3650) {
		return invalid("archive after 1 to 3650 days, or turn it off")
	}
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE project SET auto_archive_days=$2 WHERE id::text=$1`, pid, days); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "project.archive_policy", strings.ToUpper(projectKey), map[string]any{"days": days})
	})
}

// ArchivePolicy returns the project's auto-archive setting (nil = off).
func (s *Service) ArchivePolicy(ctx context.Context, projectKey string) (*int, error) {
	var d *int
	err := s.DB.QueryRow(ctx, `SELECT auto_archive_days FROM project WHERE key=$1`, strings.ToUpper(projectKey)).Scan(&d)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	return d, err
}

// AutoArchive archives resolved issues older than their project's policy, at most limit per call, and returns how many.
// Each issue is archived on its own so a failure on one does not hold up the rest.
func (s *Service) AutoArchive(ctx context.Context, limit int) (int, error) {
	rows, err := s.DB.Query(ctx, `SELECT i.id::text,i.key FROM issue i JOIN project p ON p.id=i.project_id JOIN status st ON st.id=i.status_id
		WHERE p.auto_archive_days IS NOT NULL AND NOT p.archived AND i.archived_at IS NULL AND i.deleted_at IS NULL AND st.category='done' AND i.resolved_at IS NOT NULL
		  AND i.resolved_at < $1::timestamptz - make_interval(days => p.auto_archive_days) ORDER BY i.resolved_at LIMIT $2`, s.Now(), limit)
	if err != nil {
		return 0, err
	}
	type row struct{ id, key string }
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.key); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, r)
	}
	rows.Close()
	sys := Actor{Label: "auto-archive", Admin: true}
	n := 0
	for _, r := range todo {
		if i := strings.LastIndex(r.key, "-"); i > 0 && s.blockedBy(ctx, r.key[:i]) != nil {
			continue // a pause covers the project: stand still instead of waiting out the admission time for every issue; the next pass archives it
		}
		if _, err := s.ArchiveIssue(ctx, sys, r.key); err == nil {
			n++
		}
	}
	return n, nil
}
