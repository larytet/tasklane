package domain

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// hashingWriter counts and hashes what goes into one file of the bundle, so the manifest can list it.
type hashingWriter struct {
	w io.Writer
	h hashLike
	n int64
}
type hashLike interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func (hw *hashingWriter) Write(p []byte) (int, error) {
	hw.h.Write(p)
	hw.n += int64(len(p))
	return hw.w.Write(p)
}

type bundleWriter struct {
	zw     *zip.Writer
	files  map[string]BFile
	counts map[string]int
}

func newBundleWriter(w io.Writer) *bundleWriter {
	return &bundleWriter{zw: zip.NewWriter(w), files: map[string]BFile{}, counts: map[string]int{}}
}

// file opens one entry; the caller writes to the returned writer and calls done.
func (b *bundleWriter) file(name string) (io.Writer, func(), error) {
	fw, err := b.zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Unix(0, 0)}) // fixed mtime: reproducible archives
	if err != nil {
		return nil, nil, err
	}
	hw := &hashingWriter{w: fw, h: sha256.New()}
	return hw, func() { b.files[name] = BFile{SHA256: hex.EncodeToString(hw.h.Sum(nil)), Bytes: hw.n} }, nil
}

func (b *bundleWriter) jsonl(name string, items []any) error {
	w, done, err := b.file(name)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	done()
	b.counts[strings.TrimSuffix(name, ".jsonl")] = len(items)
	return nil
}

func (b *bundleWriter) csvFile(name string, header []string, rows [][]string) error {
	w, done, err := b.file(name)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	cw.Write(header)
	for _, r := range rows {
		for i := range r {
			r[i] = csvSafe(r[i])
		}
		cw.Write(r)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	done()
	b.counts[strings.TrimSuffix(name, ".csv")] = len(rows)
	return nil
}

// csvSafe neutralizes spreadsheet formula injection: a cell that starts with = + - @ tab or CR is prefixed with an apostrophe.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		// negative numbers and ISO dates are data, not formulas
		if v[0] == '-' && len(v) > 1 && (v[1] >= '0' && v[1] <= '9') {
			return v
		}
		return "'" + v
	}
	return v
}

// BundleData is the whole snapshot in memory-friendly slices; entities stream into the zip one file at a time.
type BundleData struct {
	Users        []BUser
	Workflows    []BWorkflow
	Fields       []BField
	Projects     []BProject
	Issues       []BIssue
	Comments     []BComment
	Links        []BLink
	Attachments  []BAttachment
	History      []BHistory
	TextVersions []BTextVersion
	Aliases      []BAlias
	Removed      []BRemoved
	Skipped      []string
}

// Export writes a bundle of the projects the caller may read. The whole export reads one REPEATABLE READ snapshot, so it
// contains every save acknowledged before the marker and nothing after it, and no writer is held (SPEC 18.2, PAUSE-01).
func (s *Service) Export(ctx context.Context, a Actor, w io.Writer, o ExportOpts) (*BManifest, error) {
	if o.Format == "" {
		o.Format = "json"
	}
	if o.Format != "json" && o.Format != "csv" {
		return nil, invalid("format must be json or csv")
	}
	if o.Full && !a.Admin {
		return nil, ErrForbidden // the audited full export is for administrators
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// the marker is read inside the snapshot, so it is exactly the boundary of what the export contains
	mk := Marker{At: s.Now(), Mode: "snapshot"}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(id),0), pg_current_wal_lsn()::text FROM outbox`).Scan(&mk.OutboxID, &mk.LSN); err != nil {
		return nil, err
	}
	// scope: the projects named, or everything the caller can read
	rows, err := tx.Query(ctx, `SELECT id,key,name,type,workflow_id,lead_id,next_number,archived FROM project ORDER BY key`)
	if err != nil {
		return nil, err
	}
	type pr struct {
		id, key, name, typ, wf string
		lead                   *string
		next                   int64
		archived               bool
		role                   string
	}
	var projs []pr
	want := map[string]bool{}
	for _, k := range o.Projects {
		want[strings.ToUpper(k)] = true
	}
	for rows.Next() {
		var p pr
		rows.Scan(&p.id, &p.key, &p.name, &p.typ, &p.wf, &p.lead, &p.next, &p.archived)
		projs = append(projs, p)
	}
	rows.Close()
	var scoped []pr
	for _, p := range projs {
		if len(want) > 0 && !want[p.key] {
			continue
		}
		p.role = s.projectRole(ctx, tx, a, p.id)
		if p.role == "" {
			if len(want) > 0 {
				return nil, ErrNotFound // asking for a project one cannot see answers like a project that does not exist
			}
			continue
		}
		scoped = append(scoped, p)
	}
	if len(want) == 0 && len(scoped) == 0 {
		return nil, ErrNotFound
	}
	if len(want) == 0 && !a.Admin && len(o.Projects) == 0 {
		// without naming projects, non-administrators export what they can read, which is allowed
	}
	d := &BundleData{}
	emails := map[string]BUser{}
	user := func(id *string) string {
		if id == nil {
			return ""
		}
		var u BUser
		if err := tx.QueryRow(ctx, `SELECT name,email,kind FROM app_user WHERE id=$1`, *id).Scan(&u.Name, &u.Email, &u.Kind); err != nil {
			return ""
		}
		emails[u.Email] = u
		return u.Email
	}
	wfSeen := map[string]bool{}
	keyOf := map[string]string{} // issue id -> key, for references inside the bundle
	var projectKeys []string
	for _, p := range scoped {
		projectKeys = append(projectKeys, p.key)
		bp := BProject{Key: p.key, Name: p.name, Type: p.typ, Next: p.next, Archived: p.archived, Lead: user(p.lead)}
		var wfName string
		tx.QueryRow(ctx, `SELECT name FROM workflow WHERE id=$1`, p.wf).Scan(&wfName)
		bp.Workflow = wfName
		if !wfSeen[p.wf] {
			wfSeen[p.wf] = true
			bw := BWorkflow{Name: wfName}
			sr, _ := tx.Query(ctx, `SELECT name,category,position FROM status WHERE workflow_id=$1 AND NOT retired ORDER BY position`, p.wf)
			for sr.Next() {
				var st BStatus
				sr.Scan(&st.Name, &st.Category, &st.Position)
				bw.Statuses = append(bw.Statuses, st)
			}
			sr.Close()
			tr, _ := tx.Query(ctx, `SELECT t.name,COALESCE(f.name,''),tt.name,t.requires_human,t.rules FROM transition t LEFT JOIN status f ON f.id=t.from_status JOIN status tt ON tt.id=t.to_status WHERE t.workflow_id=$1 AND NOT t.system ORDER BY t.name,tt.name`, p.wf)
			for tr.Next() {
				var t BTransition
				var raw []byte
				tr.Scan(&t.Name, &t.From, &t.To, &t.RequiresHuman, &raw)
				if r := parseRules(raw); !r.empty() || len(r.Screen) > 0 {
					t.Rules = &r
				}
				bw.Transitions = append(bw.Transitions, t)
			}
			tr.Close()
			d.Workflows = append(d.Workflows, bw)
		}
		mr, _ := tx.Query(ctx, `SELECT u.name,u.email,u.kind,m.role FROM project_member m JOIN app_user u ON u.id=m.user_id WHERE m.project_id=$1 ORDER BY u.email`, p.id)
		for mr.Next() {
			var u BUser
			var role string
			mr.Scan(&u.Name, &u.Email, &u.Kind, &role)
			emails[u.Email] = u
			bp.Members = append(bp.Members, BMember{Email: u.Email, Role: role})
		}
		mr.Close()
		d.Projects = append(d.Projects, bp)
		// issues
		trashCond := " AND i.deleted_at IS NULL"
		if o.Trash || o.Full {
			trashCond = ""
		}
		ir, err := tx.Query(ctx, `SELECT i.id,i.key,t.name,st.name,i.priority,i.reporter_id,i.assignee_id,i.labels,i.parent_id,i.rank,i.summary,COALESCE(d.body,''),i.created_at,i.updated_at,i.resolved_at,i.deleted_at,i.purge_after,i.version,i.estimate,i.archived_at
			FROM issue i JOIN issue_type t ON t.id=i.issue_type_id JOIN status st ON st.id=i.status_id LEFT JOIN issue_text d ON d.issue_id=i.id AND d.field='description'
			WHERE i.project_id=$1`+trashCond+` ORDER BY i.number`, p.id)
		if err != nil {
			return nil, err
		}
		type rawI struct {
			bi            BIssue
			rep, asg, par *string
		}
		var raws []rawI
		for ir.Next() {
			var r rawI
			r.bi.Project = p.key
			ir.Scan(&r.bi.ID, &r.bi.Key, &r.bi.Type, &r.bi.Status, &r.bi.Priority, &r.rep, &r.asg, &r.bi.Labels, &r.par, &r.bi.Rank, &r.bi.Summary, &r.bi.Description, &r.bi.Created, &r.bi.Updated, &r.bi.Resolved, &r.bi.DeletedAt, &r.bi.PurgeAfter, &r.bi.Version, &r.bi.Estimate, &r.bi.Archived)
			raws = append(raws, r)
		}
		ir.Close()
		for _, r := range raws {
			keyOf[r.bi.ID] = r.bi.Key
		}
		for _, r := range raws {
			r.bi.ExternalID = r.bi.Key
			r.bi.Reporter, r.bi.Assignee = user(r.rep), user(r.asg)
			r.bi.Deleted = r.bi.DeletedAt != nil
			if r.par != nil {
				r.bi.Parent = keyOf[*r.par]
				if r.bi.Parent == "" { // a parent in another project or in the trash that is not exported
					tx.QueryRow(ctx, `SELECT key FROM issue WHERE id=$1`, *r.par).Scan(&r.bi.Parent)
					if !containsKeyPrefix(r.bi.Parent, p.key) {
						r.bi.Parent = ""
					}
				}
			}
			if r.bi.Labels == nil {
				r.bi.Labels = []string{}
			}
			d.Issues = append(d.Issues, r.bi)
		}
		canSeeInternal := p.role == "member" || p.role == "admin"
		if o.Comments {
			cr, err := tx.Query(ctx, `SELECT c.id,i.key,c.author_id,c.body,c.internal,c.created_at,c.edited_at FROM comment c JOIN issue i ON i.id=c.issue_id WHERE i.project_id=$1`+trashCond+` ORDER BY c.created_at,c.id`, p.id)
			if err != nil {
				return nil, err
			}
			type rc struct {
				c   BComment
				aid string
			}
			var cs []rc
			for cr.Next() {
				var x rc
				cr.Scan(&x.c.ID, &x.c.Issue, &x.aid, &x.c.Body, &x.c.Internal, &x.c.Created, &x.c.Edited)
				cs = append(cs, x)
			}
			cr.Close()
			for _, x := range cs {
				if x.c.Internal && !canSeeInternal && !o.Full {
					continue // restricted data stays out unless the caller may see it
				}
				x.c.Author = user(&x.aid)
				d.Comments = append(d.Comments, x.c)
			}
		}
		lr, _ := tx.Query(ctx, `SELECT f.key,t.key,l.type FROM issue_link l JOIN issue f ON f.id=l.from_id JOIN issue t ON t.id=l.to_id WHERE f.project_id=$1 AND f.deleted_at IS NULL AND t.deleted_at IS NULL ORDER BY l.id`, p.id)
		for lr.Next() {
			var l BLink
			lr.Scan(&l.From, &l.To, &l.Type)
			d.Links = append(d.Links, l)
		}
		lr.Close()
		ar, _ := tx.Query(ctx, `SELECT i.key,a.name,b.mime,b.sha256,b.size,a.uploader_id,a.created_at,b.scan_state FROM attachment a JOIN blob b ON b.id=a.blob_id JOIN issue i ON i.id=a.issue_id WHERE i.project_id=$1`+trashCond+` ORDER BY a.created_at,a.id`, p.id)
		type ra struct {
			a    BAttachment
			up   *string
			scan string
		}
		var as []ra
		for ar.Next() {
			var x ra
			ar.Scan(&x.a.Issue, &x.a.Name, &x.a.MIME, &x.a.SHA256, &x.a.Size, &x.up, &x.a.Created, &x.scan)
			as = append(as, x)
		}
		ar.Close()
		for _, x := range as {
			if x.scan != "clean" {
				d.Skipped = append(d.Skipped, fmt.Sprintf("attachment %q of %s is %s and was not exported", x.a.Name, x.a.Issue, x.scan))
				continue
			}
			x.a.Uploader = user(x.up)
			if o.Attachments {
				x.a.Path = "attachments/" + x.a.SHA256
			}
			d.Attachments = append(d.Attachments, x.a)
		}
		// aliases (the key map) and removed-value records round trip, so old keys and converting back keep working
		alr, _ := tx.Query(ctx, `SELECT a.old_key,i.key FROM issue_key_alias a JOIN issue i ON i.id=a.issue_id WHERE i.project_id=$1`+trashCond+` ORDER BY a.created_at,a.old_key`, p.id)
		for alr.Next() {
			var x BAlias
			alr.Scan(&x.OldKey, &x.Issue)
			d.Aliases = append(d.Aliases, x)
		}
		alr.Close()
		rvr, _ := tx.Query(ctx, `SELECT i.key,r.kind,r.data FROM removed_values r JOIN issue i ON i.id=r.issue_id WHERE i.project_id=$1 AND r.expires_at>$2`+trashCond+` ORDER BY r.id`, p.id, s.Now())
		var removed []BRemoved
		for rvr.Next() {
			var x BRemoved
			var raw []byte
			rvr.Scan(&x.Issue, &x.Kind, &raw)
			json.Unmarshal(raw, &x.Data)
			removed = append(removed, x)
		}
		rvr.Close()
		for _, x := range removed {
			// ids differ between instances, so records that point at issues carry keys in a bundle (translated after the cursor is closed)
			if cs, ok := x.Data["children"].([]any); ok {
				var keys []any
				for _, c := range cs {
					if id, _ := c.(string); id != "" {
						var k string
						if err := tx.QueryRow(ctx, `SELECT key FROM issue WHERE id=$1`, id).Scan(&k); err == nil {
							keys = append(keys, k)
						}
					}
				}
				x.Data["children"] = keys
			}
			d.Removed = append(d.Removed, x)
		}
		if o.History || o.Full {
			hr, _ := tx.Query(ctx, `SELECT i.key,h.field,h.before,h.after,h.actor_label,h.at FROM issue_history h JOIN issue i ON i.id=h.issue_id WHERE i.project_id=$1`+trashCond+` ORDER BY h.id`, p.id)
			for hr.Next() {
				var x BHistory
				hr.Scan(&x.Issue, &x.Field, &x.Before, &x.After, &x.Actor, &x.At)
				d.History = append(d.History, x)
			}
			hr.Close()
			vr, _ := tx.Query(ctx, `SELECT i.key,v.field,v.seq,v.body,v.actor_label,v.mode,v.at FROM text_version v JOIN issue i ON i.id=v.issue_id WHERE i.project_id=$1`+trashCond+` ORDER BY i.number,v.field,v.seq`, p.id)
			for vr.Next() {
				var x BTextVersion
				vr.Scan(&x.Issue, &x.Field, &x.Seq, &x.Body, &x.Actor, &x.Mode, &x.At)
				d.TextVersions = append(d.TextVersions, x)
			}
			vr.Close()
		}
	}
	roles := map[string]string{}
	for _, p := range scoped {
		roles[p.key] = p.role
	}
	if err := s.exportFields(ctx, tx, a, d, roles, "", user); err != nil {
		return nil, err
	}
	var es []string
	for e := range emails {
		es = append(es, e)
	}
	sort.Strings(es)
	for _, e := range es {
		d.Users = append(d.Users, emails[e])
	}
	// ---- write the zip: entities first, attachment files, then the manifest with every checksum
	bw := newBundleWriter(w)
	man := &BManifest{Format: BundleFormat, Version: BundleVersion, Kind: o.Format, Source: "tasklane", ExportedAt: s.Now(), Mode: "snapshot", Marker: mk, Projects: projectKeys, Options: o,
		Derived: "history, search and notifications may trail the marker by seconds; saved content is exact", Notes: d.Skipped}
	anys := func(n int, f func(i int) any) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = f(i)
		}
		return out
	}
	if o.Format == "json" {
		steps := []struct {
			name string
			n    int
			f    func(int) any
		}{
			{"users.jsonl", len(d.Users), func(i int) any { return d.Users[i] }},
			{"workflows.jsonl", len(d.Workflows), func(i int) any { return d.Workflows[i] }},
			{"fields.jsonl", len(d.Fields), func(i int) any { return d.Fields[i] }},
			{"projects.jsonl", len(d.Projects), func(i int) any { return d.Projects[i] }},
			{"issues.jsonl", len(d.Issues), func(i int) any { return d.Issues[i] }},
			{"comments.jsonl", len(d.Comments), func(i int) any { return d.Comments[i] }},
			{"links.jsonl", len(d.Links), func(i int) any { return d.Links[i] }},
			{"attachments.jsonl", len(d.Attachments), func(i int) any { return d.Attachments[i] }},
			{"aliases.jsonl", len(d.Aliases), func(i int) any { return d.Aliases[i] }},
			{"removed_values.jsonl", len(d.Removed), func(i int) any { return d.Removed[i] }},
		}
		if o.History || o.Full {
			steps = append(steps,
				struct {
					name string
					n    int
					f    func(int) any
				}{"history.jsonl", len(d.History), func(i int) any { return d.History[i] }},
				struct {
					name string
					n    int
					f    func(int) any
				}{"text_versions.jsonl", len(d.TextVersions), func(i int) any { return d.TextVersions[i] }})
		}
		for _, st := range steps {
			if err := bw.jsonl(st.name, anys(st.n, st.f)); err != nil {
				return nil, err
			}
		}
	} else if err := writeCSVBundle(bw, d, o); err != nil {
		return nil, err
	}
	if o.Attachments {
		done := map[string]bool{}
		for _, at := range d.Attachments {
			if done[at.SHA256] {
				continue
			}
			done[at.SHA256] = true
			if s.Blobs == nil || s.Blobs.Store == nil {
				return nil, invalid("attachments are not configured, so files cannot be exported")
			}
			f, err := s.Blobs.Store.Open(at.SHA256)
			if err != nil {
				man.Notes = append(man.Notes, fmt.Sprintf("the stored file for attachment %q (%s) is missing and was not exported", at.Name, at.SHA256))
				continue
			}
			fw, finish, err := bw.file("attachments/" + at.SHA256)
			if err != nil {
				f.Close()
				return nil, err
			}
			_, err = io.Copy(fw, f) // streamed with constant memory
			f.Close()
			if err != nil {
				return nil, err
			}
			finish()
		}
		bw.counts["attachment_files"] = len(done)
	}
	man.Counts, man.Files = bw.counts, bw.files
	mf, err := bw.zw.Create("manifest.json")
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(mf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(man); err != nil {
		return nil, err
	}
	if err := bw.zw.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	kind := "export"
	if o.Full {
		kind = "export.full"
	}
	s.DB.Exec(ctx, `INSERT INTO audit_event(actor_id,actor_label,action,target,detail) VALUES($1,$2,$3,$4,$5)`, nilIfEmpty(a.ID), a.Label, kind, strings.Join(projectKeys, ","),
		fmt.Sprintf(`{"format":%q,"issues":%d,"marker_outbox":%d}`, o.Format, len(d.Issues), mk.OutboxID))
	return man, nil
}

func containsKeyPrefix(key, project string) bool { return strings.HasPrefix(key, project+"-") }

func ts(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func writeCSVBundle(bw *bundleWriter, d *BundleData, o ExportOpts) error {
	rows := [][]string{}
	for _, i := range d.Issues {
		rows = append(rows, []string{i.ExternalID, i.Key, i.Project, i.Type, i.Status, i.Priority, i.Reporter, i.Assignee, strings.Join(i.Labels, ";"), i.Parent, i.Rank, i.Summary, i.Description,
			i.Created.UTC().Format(time.RFC3339Nano), i.Updated.UTC().Format(time.RFC3339Nano), ts(i.Resolved), fmt.Sprint(i.Deleted), ts(i.DeletedAt), fmt.Sprint(i.Version), estStr(i.Estimate), ts(i.Archived)})
	}
	if err := bw.csvFile("issues.csv", []string{"external_id", "key", "project", "type", "status", "priority", "reporter", "assignee", "labels", "parent", "rank", "summary", "description", "created", "updated", "resolved", "deleted", "deleted_at", "version", "estimate", "archived_at"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, c := range d.Comments {
		rows = append(rows, []string{c.ID, c.Issue, c.Author, c.Body, fmt.Sprint(c.Internal), c.Created.UTC().Format(time.RFC3339Nano), ts(c.Edited)})
	}
	if err := bw.csvFile("comments.csv", []string{"id", "issue", "author", "body", "internal", "created", "edited"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, l := range d.Links {
		rows = append(rows, []string{l.From, l.To, l.Type})
	}
	if err := bw.csvFile("links.csv", []string{"from", "to", "type"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, a := range d.Attachments {
		rows = append(rows, []string{a.Issue, a.Name, a.MIME, a.SHA256, fmt.Sprint(a.Size), a.Uploader, a.Created.UTC().Format(time.RFC3339Nano), a.Path})
	}
	if err := bw.csvFile("attachments.csv", []string{"issue", "name", "mime", "sha256", "size", "uploader", "created", "path"}, rows); err != nil {
		return err
	}
	if err := writeFieldsCSV(bw, d); err != nil {
		return err
	}
	rows = nil
	for _, u := range d.Users {
		rows = append(rows, []string{u.Email, u.Name, u.Kind})
	}
	if err := bw.csvFile("users.csv", []string{"email", "name", "kind"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, w := range d.Workflows {
		for _, st := range w.Statuses {
			rows = append(rows, []string{w.Name, "status", st.Name, st.Category, fmt.Sprint(st.Position), "", "", ""})
		}
		for _, t := range w.Transitions {
			rj := ""
			if t.Rules != nil {
				b, _ := json.Marshal(t.Rules)
				rj = string(b)
			}
			rows = append(rows, []string{w.Name, "transition", t.Name, "", "", t.From + ">" + t.To, fmt.Sprint(t.RequiresHuman), rj})
		}
	}
	if err := bw.csvFile("workflows.csv", []string{"workflow", "kind", "name", "category", "position", "from>to", "requires_human", "rules"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, p := range d.Projects {
		var ms []string
		for _, m := range p.Members {
			ms = append(ms, m.Email+":"+m.Role)
		}
		rows = append(rows, []string{p.Key, p.Name, p.Type, p.Workflow, p.Lead, fmt.Sprint(p.Next), fmt.Sprint(p.Archived), strings.Join(ms, ";")})
	}
	if err := bw.csvFile("projects.csv", []string{"key", "name", "type", "workflow", "lead", "next_number", "archived", "members"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, x := range d.Aliases {
		rows = append(rows, []string{x.OldKey, x.Issue})
	}
	if err := bw.csvFile("aliases.csv", []string{"old_key", "issue"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, x := range d.Removed {
		b, _ := json.Marshal(x.Data)
		rows = append(rows, []string{x.Issue, x.Kind, string(b)})
	}
	if err := bw.csvFile("removed_values.csv", []string{"issue", "kind", "data"}, rows); err != nil {
		return err
	}
	if o.History || o.Full {
		rows = nil
		for _, h := range d.History {
			bf, af := "", ""
			if h.Before != nil {
				bf = *h.Before
			}
			if h.After != nil {
				af = *h.After
			}
			rows = append(rows, []string{h.Issue, h.Field, bf, af, h.Actor, h.At.UTC().Format(time.RFC3339Nano)})
		}
		if err := bw.csvFile("history.csv", []string{"issue", "field", "before", "after", "actor", "at"}, rows); err != nil {
			return err
		}
	}
	return nil
}

func estStr(e *float64) string {
	if e == nil {
		return ""
	}
	return strconv.FormatFloat(*e, 'f', -1, 64)
}
