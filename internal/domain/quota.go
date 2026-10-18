package domain

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Storage quotas (BLOB-18): a project's attachments may not grow past its quota. The check is made when an upload starts
// (from the size the client declares) and again when the file is registered (from the size that was received), with a
// message that says what to do. Concurrent uploads can overshoot the quota by what they carry; deleting a file frees its space at once.

type QuotaError struct{ Quota, Used, Add int64 }

func (e *QuotaError) Error() string {
	return fmt.Sprintf("this project's storage quota of %s is full (%s used, this file needs %s): delete files or ask an administrator to raise it", human(e.Quota), human(e.Used), human(e.Add))
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func (s *Service) storageUsed(ctx context.Context, projectID string) int64 {
	var n int64
	s.DB.QueryRow(ctx, `SELECT COALESCE(sum(b.size),0) FROM attachment a JOIN blob b ON b.id=a.blob_id JOIN issue i ON i.id=a.issue_id WHERE i.project_id::text=$1`, projectID).Scan(&n)
	return n
}

// checkQuota refuses an upload that would take the project past its quota.
func (s *Service) checkQuota(ctx context.Context, projectKey string, add int64) error {
	var quota *int64
	var pid string
	if err := s.DB.QueryRow(ctx, `SELECT id::text,storage_quota_bytes FROM project WHERE key=$1`, strings.ToUpper(projectKey)).Scan(&pid, &quota); err != nil || quota == nil {
		return nil
	}
	if used := s.storageUsed(ctx, pid); used+add > *quota {
		return &QuotaError{Quota: *quota, Used: used, Add: add}
	}
	return nil
}

// SetStorageQuota sets a project's quota in MiB (nil removes it). Administrators only.
func (s *Service) SetStorageQuota(ctx context.Context, a Actor, projectKey string, mib *int64) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	if mib != nil && (*mib < 0 || *mib > 1<<30) {
		return invalid("a quota is between 0 and a million GiB, or none")
	}
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return err
	}
	var bytes any
	if mib != nil {
		bytes = *mib << 20
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE project SET storage_quota_bytes=$2 WHERE id::text=$1`, pid, bytes); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "project.storage_quota", strings.ToUpper(projectKey), map[string]any{"mib": mib})
	})
}

type StorageReport struct {
	Project   string           `json:"project"`
	Used      int64            `json:"used_bytes"`
	Quota     *int64           `json:"quota_bytes"`
	Files     int              `json:"files"`
	ByClass   map[string]int64 `json:"by_class"`
	PercentOf *float64         `json:"percent_of_quota,omitempty"`
}

func (s *Service) storageReport(ctx context.Context, projectID, key string, quota *int64) (StorageReport, error) {
	r := StorageReport{Project: key, Quota: quota, ByClass: map[string]int64{}}
	rows, err := s.DB.Query(ctx, `SELECT b.class,COALESCE(sum(b.size),0),count(*) FROM attachment a JOIN blob b ON b.id=a.blob_id JOIN issue i ON i.id=a.issue_id WHERE i.project_id::text=$1 GROUP BY b.class`, projectID)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		var n int64
		var files int
		rows.Scan(&c, &n, &files)
		r.ByClass[c] = n
		r.Used += n
		r.Files += files
	}
	if quota != nil && *quota > 0 {
		p := float64(r.Used) * 100 / float64(*quota)
		r.PercentOf = &p
	}
	return r, rows.Err()
}

// StorageUsage reports a project's attachment storage; members may read it.
func (s *Service) StorageUsage(ctx context.Context, a Actor, projectKey string) (StorageReport, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return StorageReport{}, err
	}
	if s.role(ctx, a, pid) == "" {
		return StorageReport{}, ErrNotFound
	}
	var quota *int64
	s.DB.QueryRow(ctx, `SELECT storage_quota_bytes FROM project WHERE id::text=$1`, pid).Scan(&quota)
	return s.storageReport(ctx, pid, strings.ToUpper(projectKey), quota)
}

// StorageOverview reports every project's usage (administrators).
func (s *Service) StorageOverview(ctx context.Context, a Actor) ([]StorageReport, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text,key,storage_quota_bytes FROM project ORDER BY key`)
	if err != nil {
		return nil, err
	}
	type pr struct {
		id, key string
		quota   *int64
	}
	var ps []pr
	for rows.Next() {
		var p pr
		rows.Scan(&p.id, &p.key, &p.quota)
		ps = append(ps, p)
	}
	rows.Close()
	out := []StorageReport{}
	for _, p := range ps {
		r, err := s.storageReport(ctx, p.id, p.key, p.quota)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
