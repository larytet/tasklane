package domain

import "time"

// The exchange model (SPEC 18.2): the same data in a JSON bundle (JSON Lines per entity) and a CSV bundle, both zips
// with a manifest that lists every file with its SHA-256. Users are carried by name and email, never by credentials.

const BundleFormat = "tasklane-bundle"
const BundleVersion = 1

type BManifest struct {
	Format     string           `json:"format"`
	Version    int              `json:"version"`
	Kind       string           `json:"kind"` // json or csv
	Source     string           `json:"source"`
	ExportedAt time.Time        `json:"exported_at"`
	Mode       string           `json:"mode"`   // snapshot: one consistent database snapshot, no write hold
	Marker     Marker           `json:"marker"` // the outbox and log position of that snapshot
	Projects   []string         `json:"projects"`
	Options    ExportOpts       `json:"options"`
	Counts     map[string]int   `json:"counts"`
	Files      map[string]BFile `json:"files"`
	Notes      []string         `json:"notes,omitempty"`
	Derived    string           `json:"derived_data"` // what may trail the marker (SPEC 21)
}

type BFile struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type BUser struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Kind  string `json:"kind"`
}

type BMember struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type BStatus struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Position int    `json:"position"`
}

type BTransition struct {
	Name string `json:"name"`
	From string `json:"from"` // empty: from any status
	To   string `json:"to"`
}

type BWorkflow struct {
	Name        string        `json:"name"`
	Statuses    []BStatus     `json:"statuses"`
	Transitions []BTransition `json:"transitions"`
}

type BProject struct {
	Key      string    `json:"key"`
	Name     string    `json:"name"`
	Type     string    `json:"type"`
	Workflow string    `json:"workflow"`
	Lead     string    `json:"lead"`
	Members  []BMember `json:"members"`
	Next     int64     `json:"next_number"`
	Archived bool      `json:"archived"`
}

type BIssue struct {
	ID          string     `json:"id"`
	ExternalID  string     `json:"external_id"`
	Key         string     `json:"key"`
	Project     string     `json:"project"`
	Type        string     `json:"type"`
	Status      string     `json:"status"`
	Priority    string     `json:"priority"`
	Reporter    string     `json:"reporter"`
	Assignee    string     `json:"assignee"`
	Labels      []string   `json:"labels"`
	Parent      string     `json:"parent"` // the parent's key in this bundle
	Rank        string     `json:"rank"`
	Summary     string     `json:"summary"`
	Description string     `json:"description"`
	Created     time.Time  `json:"created"`
	Updated     time.Time  `json:"updated"`
	Resolved    *time.Time `json:"resolved"`
	Deleted     bool       `json:"deleted"`
	DeletedAt   *time.Time `json:"deleted_at"`
	Archived    *time.Time `json:"archived_at"`
	PurgeAfter  *time.Time `json:"purge_after"`
	Version     int64      `json:"version"`
	Estimate    *float64   `json:"estimate,omitempty"`
}

type BComment struct {
	ID       string     `json:"id"`
	Issue    string     `json:"issue"` // issue key
	Author   string     `json:"author"`
	Body     string     `json:"body"`
	Internal bool       `json:"internal"`
	Created  time.Time  `json:"created"`
	Edited   *time.Time `json:"edited"`
}

type BLink struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

type BAttachment struct {
	Issue    string    `json:"issue"`
	Name     string    `json:"name"`
	MIME     string    `json:"mime"`
	SHA256   string    `json:"sha256"`
	Size     int64     `json:"size"`
	Uploader string    `json:"uploader"`
	Created  time.Time `json:"created"`
	Path     string    `json:"path"` // attachments/<sha256> inside the bundle, empty when files were not included
}

type BHistory struct {
	Issue  string    `json:"issue"`
	Field  string    `json:"field"`
	Before *string   `json:"before"`
	After  *string   `json:"after"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
}

type BTextVersion struct {
	Issue string    `json:"issue"`
	Field string    `json:"field"`
	Seq   int64     `json:"seq"`
	Body  string    `json:"body"`
	Actor string    `json:"actor"`
	Mode  string    `json:"mode"`
	At    time.Time `json:"at"`
}

type BAlias struct {
	OldKey string `json:"old_key"`
	Issue  string `json:"issue"` // the issue's key in this bundle
}

type BRemoved struct {
	Issue string         `json:"issue"`
	Kind  string         `json:"kind"`
	Data  map[string]any `json:"data"`
}

// ExportOpts: what to include and for whom.
type ExportOpts struct {
	Projects    []string `json:"projects"`    // empty means every project the caller can read (administrators: the instance)
	Format      string   `json:"format"`      // json or csv
	Attachments bool     `json:"attachments"` // include the files
	History     bool     `json:"history"`     // include history and text versions
	Comments    bool     `json:"comments"`    // include comments (default true when unset by callers using DefaultExport)
	Trash       bool     `json:"trash"`       // include deleted issues
	Full        bool     `json:"full"`        // administrators only: everything, audited
}

func DefaultExport() ExportOpts { return ExportOpts{Format: "json", Comments: true, Attachments: true} }
