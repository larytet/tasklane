package domain

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/blob"
)

// Files on customer requests (SPEC 15, 17.6). Customers may add files to requests they can see when the portal allows it, within the portal's
// rules (size, count, classes), checked on what the bytes really are and not on the name or declared type: a file whose content is not what it
// claims, a blocked file (one the scanner flagged), an archive or a video is refused and nothing is stored. A customer sees only the files marked
// customer-visible (their own uploads are marked at once; staff choose for theirs), downloads them only through their own session, and never
// gets one inline. Their uploads are theirs to delete and go with them when they are erased.

const portalUploadsPerHour = 30

type PortalAttachment struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mine    bool      `json:"mine"`
	Created time.Time `json:"created"`
}

func (s *Service) portalAttachments(ctx context.Context, c Customer, issueID string) []PortalAttachment {
	out := []PortalAttachment{}
	rows, err := s.DB.Query(ctx, `SELECT a.id::text,a.name,b.size,a.customer_id::text=$2,a.created_at FROM attachment a JOIN blob b ON b.id=a.blob_id
		WHERE a.issue_id::text=$1 AND a.customer_visible AND b.scan_state='clean' ORDER BY a.created_at,a.id`, issueID, c.ID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p PortalAttachment
		var mine *bool
		rows.Scan(&p.ID, &p.Name, &p.Size, &mine, &p.Created)
		p.Mine = mine != nil && *mine
		out = append(out, p)
	}
	return out
}

// PortalAttach adds a file to a request the customer can see.
func (s *Service) PortalAttach(ctx context.Context, c Customer, projectKey, key, name, declaredMIME string, r io.Reader) (PortalRequest, error) {
	req, err := s.PortalGetRequest(ctx, c, projectKey, key)
	if err != nil {
		return req, err
	}
	pid, _ := s.projectID(ctx, projectKey)
	var pol PortalPolicy
	if pol, err = s.portalPolicyOf(ctx, pid); err != nil {
		return req, err
	}
	if !pol.AttachEnabled {
		return req, invalid("files cannot be added to requests here")
	}
	if err := s.blobsReady(); err != nil {
		return req, invalid("files cannot be added to requests here")
	}
	if req.Resolved {
		return req, invalid("this request is resolved: open a new one to send more files")
	}
	var issueID string
	var have int
	s.DB.QueryRow(ctx, `SELECT i.id::text, (SELECT count(*) FROM attachment a WHERE a.issue_id=i.id AND a.customer_id IS NOT NULL) FROM issue i WHERE i.key=$1`, req.Key).Scan(&issueID, &have)
	if have >= pol.AttachMaxFiles {
		return req, invalid("a request can have at most %d files from customers", pol.AttachMaxFiles)
	}
	if s.portalCount(ctx, "upload", c.ID, time.Hour) >= portalUploadsPerHour {
		return req, &LockedError{Until: s.Now().Add(time.Hour)}
	}
	actor, err := s.portalActor(ctx, c.OrgID, pid)
	if err != nil {
		return req, err
	}
	is, _, err := s.access(ctx, actor, req.Key)
	if err != nil {
		return req, err
	}
	end, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return req, perr
	}
	defer end()
	release, err := s.acquire()
	if err != nil {
		return req, err
	}
	defer release()
	id, f, err := s.Blobs.Store.NewStaging()
	if err != nil {
		return req, err
	}
	max := int64(pol.AttachMaxMB) << 20
	n, err := io.Copy(f, io.LimitReader(r, max+1))
	f.Close()
	path, _ := s.Blobs.Store.StagingPath(id)
	if err != nil || n > max || n == 0 {
		os.Remove(path)
		switch {
		case err != nil:
			return req, err
		case n == 0:
			return req, invalid("that file is empty")
		}
		return req, invalid("files can be at most %d MB", pol.AttachMaxMB)
	}
	allowed := map[string]bool{}
	for _, cl := range pol.AttachClasses {
		allowed[cl] = true
	}
	check := func(mime string, class blob.Class, size int64, scan, flag string) error {
		switch {
		case scan != "clean":
			return invalid("that file was refused")
		case flag != "":
			return invalid("that file is not the kind of file its name says, so it was refused")
		case !allowed[string(class)]:
			return invalid("%s files are not accepted here", class)
		}
		return nil
	}
	att, err := s.commitStagedChecked(ctx, actor, is, is.ID, id, path, blob.SafeName(name), declaredMIME, check)
	if err != nil {
		return req, err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE attachment SET customer_id=$2, customer_visible=true WHERE id::text=$1`, att.ID, c.ID); err != nil {
		return req, err
	}
	s.portalNote(ctx, "upload", c.ID)
	return s.PortalGetRequest(ctx, c, projectKey, key)
}

func (s *Service) portalPolicyOf(ctx context.Context, pid string) (PortalPolicy, error) {
	p := PortalPolicy{AttachMaxMB: 10, AttachMaxFiles: 5, AttachClasses: []string{"image", "document", "log"}}
	err := s.DB.QueryRow(ctx, `SELECT enabled,attach_enabled,attach_max_mb,attach_max_files,attach_classes FROM portal_setting WHERE project_id::text=$1`, pid).
		Scan(&p.Enabled, &p.AttachEnabled, &p.AttachMaxMB, &p.AttachMaxFiles, &p.AttachClasses)
	if err != nil && err != pgx.ErrNoRows {
		return p, err
	}
	return p, nil
}

// portalFile finds an attachment the customer may see on a request they may see.
func (s *Service) portalFile(ctx context.Context, c Customer, projectKey, attID string) (string, string, error) {
	pid, err := s.portalProject(ctx, c, projectKey)
	if err != nil {
		return "", "", err
	}
	var key, issueID string
	err = s.DB.QueryRow(ctx, `SELECT i.key,i.id::text FROM attachment a JOIN issue i ON i.id=a.issue_id AND i.deleted_at IS NULL AND i.project_id=$3
		JOIN portal_request pr ON pr.issue_id=i.id WHERE a.id::text=$2 AND a.customer_visible AND `+customerSees, c.ID, attID, pid).Scan(&key, &issueID)
	if err != nil {
		return "", "", ErrNotFound
	}
	return key, issueID, nil
}

// PortalFileInfo is what the download handler needs; the file is always sent as a download, never shown inline.
func (s *Service) PortalFileInfo(ctx context.Context, c Customer, projectKey, attID string) (DownloadInfo, error) {
	if _, _, err := s.portalFile(ctx, c, projectKey, attID); err != nil {
		return DownloadInfo{}, err
	}
	d, err := s.fileInfo(ctx, attID, false)
	d.Inline = false
	return d, err
}

// PortalDeleteAttachment removes a file the customer uploaded themselves.
func (s *Service) PortalDeleteAttachment(ctx context.Context, c Customer, projectKey, attID string) error {
	key, _, err := s.portalFile(ctx, c, projectKey, attID)
	if err != nil {
		return err
	}
	var mine bool
	s.DB.QueryRow(ctx, `SELECT customer_id::text=$2 FROM attachment WHERE id::text=$1`, attID, c.ID).Scan(&mine)
	if !mine {
		return ErrForbidden
	}
	pid, _ := s.projectID(ctx, projectKey)
	actor, err := s.portalActor(ctx, c.OrgID, pid)
	if err != nil {
		return err
	}
	return s.DeleteAttachment(ctx, actor, key, attID)
}

// SetAttachmentCustomerVisible lets staff share a file on a customer request with its customers, or take it back.
func (s *Service) SetAttachmentCustomerVisible(ctx context.Context, a Actor, attID string, visible bool) error {
	var key string
	var clean bool
	err := s.DB.QueryRow(ctx, `SELECT i.key,b.scan_state='clean' FROM attachment a JOIN issue i ON i.id=a.issue_id AND i.deleted_at IS NULL JOIN blob b ON b.id=a.blob_id WHERE a.id::text=$1`, attID).Scan(&key, &clean)
	if err != nil {
		return ErrNotFound
	}
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return err
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	var isRequest bool
	s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM portal_request WHERE issue_id::text=$1)`, is.ID).Scan(&isRequest)
	if !isRequest {
		return invalid("this issue is not a customer request")
	}
	if visible && !clean {
		return invalid("a file that has not passed the scan cannot be shared")
	}
	end, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return perr
	}
	defer end()
	var theirs bool
	s.DB.QueryRow(ctx, `SELECT customer_id IS NOT NULL FROM attachment WHERE id::text=$1`, attID).Scan(&theirs)
	if theirs {
		return invalid("a file a customer sent stays visible to them")
	}
	if _, err := s.DB.Exec(ctx, `UPDATE attachment SET customer_visible=$2 WHERE id::text=$1 AND customer_id IS NULL`, attID, visible); err != nil {
		return err
	}
	s.emit(Event{Type: "attachment.changed", Issue: is.Key, Actor: a.Label, ActorID: a.ID})
	return nil
}

// eraseCustomerFiles drops the files a customer uploaded and the file names in the history of those requests.
func (s *Service) eraseCustomerFiles(ctx context.Context, tx pgx.Tx, id string, counts map[string]int) error {
	rows, err := tx.Query(ctx, `DELETE FROM attachment WHERE customer_id::text=$1 RETURNING blob_id::text,name,issue_id::text`, id)
	if err != nil {
		return err
	}
	type gone struct{ blob, name, issue string }
	var list []gone
	for rows.Next() {
		var g gone
		rows.Scan(&g.blob, &g.name, &g.issue)
		list = append(list, g)
	}
	rows.Close()
	for _, g := range list {
		if _, err := tx.Exec(ctx, `UPDATE blob SET refcount=refcount-1, zero_since=CASE WHEN refcount-1<=0 THEN $2::timestamptz ELSE zero_since END WHERE id::text=$1`, g.blob, s.Now()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE issue_history SET before=CASE WHEN before=$3 THEN 'a file from a deleted customer' ELSE before END, after=CASE WHEN after=$3 THEN 'a file from a deleted customer' ELSE after END
			WHERE issue_id::text=$1 AND field=$2`, g.issue, "attachment", g.name); err != nil {
			return err
		}
		counts["files_removed"]++
	}
	return nil
}
