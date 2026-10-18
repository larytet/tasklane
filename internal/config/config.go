// Package config loads settings from one schema with documented precedence:
// defaults, then the config file, then TASKLANE_* environment variables, then command-line flags.
// Secrets can come from a file (TASKLANE_<KEY>_FILE) and are never printed.
package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen              string
	DatabaseURL         string
	BlobDir             string
	SearchEngine        string
	OSURL               string
	OSUser              string
	OSPassword          string
	OSAPIKey            string
	OSAllowPrivate      bool
	WebhookAllowPrivate bool
	PasswordMinLength   int64
	OSIndex             string
	OSShards            int64
	OSReplicas          int64
	SecretKey           string
	BlobBackend         string
	S3Endpoint          string
	S3Region            string
	S3Bucket            string
	S3Prefix            string
	S3AccessKey         string
	S3SecretKey         string
	S3PathStyle         bool
	S3SSE               string
	S3KMSKeyID          string
	S3PartSizeMB        int64
	MaxUploadMB         int64
	SessionTTL          time.Duration
	BaseURL             string
	SMTPHost            string
	SMTPPort            int
	SMTPFrom            string
	SMTPTLS             string
	SMTPUser            string
	SMTPPass            string
	AdminEmail          string
	AdminPassword       string
	PresenceTTL         time.Duration
}

// Key is one setting. The environment variable is TASKLANE_ plus the upper-cased name, the flag is --name
// with dashes, and the config file key is the name.
type Key struct {
	Name    string
	Default string
	Kind    string // string, int, duration, enum
	Enum    []string
	Secret  bool
	Doc     string
}

// Schema is the single source of truth: loading, `config print` and the generated reference all read it.
var Schema = []Key{
	{Name: "listen", Default: ":8080", Kind: "string", Doc: "Address the HTTP server listens on"},
	{Name: "database_url", Default: "postgres://tl:tl@localhost:55432/tasklane", Kind: "string", Secret: true, Doc: "PostgreSQL connection string. The role must be able to create the `pg_trgm` and `unaccent` extensions (trusted extensions on PostgreSQL 13 and later, so the database owner can)"},
	{Name: "base_url", Default: "http://localhost:8080", Kind: "string", Doc: "Public URL. An `https://` value turns on the `Secure` flag of the session cookie and is used in links in mail"},
	{Name: "blob_dir", Default: "./data/blobs", Kind: "string", Doc: "Directory of the local filesystem blob store (attachments). Must be writable"},
	{Name: "blob_backend", Default: "fs", Kind: "enum", Enum: []string{"fs", "s3"}, Doc: "Where attachments are kept: `fs` (a directory, `blob_dir`) or `s3` (any S3-compatible service; `blob_dir` then holds only upload staging)"},
	{Name: "s3_endpoint", Default: "", Kind: "string", Doc: "S3 endpoint URL for MinIO and other compatible services, such as `http://minio:9000`. Empty means AWS S3. A custom endpoint implies path-style addressing"},
	{Name: "s3_region", Default: "us-east-1", Kind: "string", Doc: "S3 region used for request signing"},
	{Name: "s3_bucket", Default: "", Kind: "string", Doc: "S3 bucket (required with `blob_backend=s3`; it must already exist)"},
	{Name: "s3_prefix", Default: "", Kind: "string", Doc: "Optional key prefix, so a bucket can be shared, for example `tasklane/`"},
	{Name: "s3_access_key", Default: "", Kind: "string", Secret: true, Doc: "S3 access key id"},
	{Name: "s3_secret_key", Default: "", Kind: "string", Secret: true, Doc: "S3 secret access key"},
	{Name: "s3_path_style", Default: "false", Kind: "enum", Enum: []string{"true", "false"}, Doc: "Address the bucket in the URL path instead of the host name (always on with a custom endpoint)"},
	{Name: "s3_sse", Default: "", Kind: "enum", Enum: []string{"", "AES256", "aws:kms"}, Doc: "Server-side encryption requested on every write"},
	{Name: "s3_kms_key_id", Default: "", Kind: "string", Doc: "KMS key for `s3_sse=aws:kms`; empty uses the bucket default key"},
	{Name: "s3_part_size_mb", Default: "16", Kind: "int", Doc: "Multipart part size in MiB (at least 5); larger files are uploaded in parts"},
	{Name: "secret_key", Default: "", Kind: "string", Secret: true, Doc: "64 hexadecimal characters (32 bytes) that encrypt stored credentials such as Git tokens and webhook secrets. Set it (for example from a secret file) so a database dump alone cannot decrypt them; when unset a key kept in the database is used"},
	{Name: "search_engine", Default: "postgres", Kind: "enum", Enum: []string{"postgres", "opensearch"}, Doc: "`postgres` (built in, designed for up to 100,000 issues) or `opensearch` (a separate cluster you run; PostgreSQL stays the source of truth and answers whenever the cluster is unavailable)"},
	{Name: "opensearch_url", Default: "", Kind: "string", Doc: "Base URL of the OpenSearch cluster, such as `https://opensearch.internal:9200`. Never expose the cluster publicly"},
	{Name: "opensearch_user", Default: "", Kind: "string", Doc: "OpenSearch user for basic authentication"},
	{Name: "opensearch_password", Default: "", Kind: "string", Secret: true, Doc: "OpenSearch password"},
	{Name: "opensearch_api_key", Default: "", Kind: "string", Secret: true, Doc: "OpenSearch API key (used instead of user and password when set)"},
	{Name: "opensearch_allow_private", Default: "false", Kind: "enum", Enum: []string{"true", "false"}, Doc: "Allow the cluster to be on a private or loopback address (the usual case in a private network)"},
	{Name: "webhook_allow_private", Default: "false", Kind: "enum", Enum: []string{"true", "false"}, Doc: "Allow workflow webhook post-functions to call private or loopback addresses (refused by default so a webhook cannot reach internal services)"},
	{Name: "password_min_length", Default: "8", Kind: "int", Doc: "Shortest accepted password for local accounts (at least 8)"},
	{Name: "opensearch_index", Default: "tasklane-issues", Kind: "string", Doc: "Alias name for the issue index; versioned indexes are created behind it"},
	{Name: "opensearch_shards", Default: "1", Kind: "int", Doc: "Primary shards for new indexes"},
	{Name: "opensearch_replicas", Default: "0", Kind: "int", Doc: "Replicas for new indexes (use at least 1 on a multi-node cluster)"},
	{Name: "max_upload_mb", Default: "100", Kind: "int", Doc: "Largest accepted attachment, in MiB"},
	{Name: "session_ttl", Default: "336h", Kind: "duration", Doc: "How long a sign-in lasts"},
	{Name: "presence_ttl", Default: "30s", Kind: "duration", Doc: "How long an editing indicator lives without a heartbeat"},
	{Name: "admin_email", Default: "", Kind: "string", Doc: "On start, creates this administrator if it does not exist. Remove after the first start"},
	{Name: "admin_password", Default: "", Kind: "string", Secret: true, Doc: "Password for the administrator created from `admin_email`"},
	{Name: "smtp_host", Default: "", Kind: "string", Doc: "Outbound mail server. Unset means mail is off"},
	{Name: "smtp_port", Default: "25", Kind: "int", Doc: "Outbound mail server port"},
	{Name: "smtp_tls", Default: "starttls", Kind: "enum", Enum: []string{"none", "starttls", "tls"}, Doc: "`starttls` refuses to continue without it, `tls` is implicit TLS, `none` is plaintext. Certificates are always verified"},
	{Name: "smtp_from", Default: "Tasklane <tasklane@localhost>", Kind: "string", Doc: "Sender address"},
	{Name: "smtp_user", Default: "", Kind: "string", Doc: "SMTP user. Credentials are never sent over an unencrypted connection to a non-local host"},
	{Name: "smtp_pass", Default: "", Kind: "string", Secret: true, Doc: "SMTP password"},
}

// Source says where an effective value came from.
type Source string

const (
	SrcDefault Source = "default"
	SrcFile    Source = "file"
	SrcEnv     Source = "environment"
	SrcFlag    Source = "flag"
)

// Entry is one effective value, for `config print`.
type Entry struct {
	Key    Key
	Value  string
	Source Source
}

// Options feed Load; tests replace the environment and the file reader.
type Options struct {
	Args     []string // flags, after the subcommand
	Environ  func() []string
	ReadFile func(path string) ([]byte, error)
}

func envName(k string) string { return "TASKLANE_" + strings.ToUpper(k) }

func find(name string) (Key, bool) {
	for _, k := range Schema {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}

// suggest returns the closest known key, or "" when nothing is close.
func suggest(name string) string {
	best, bd := "", 4
	for _, k := range Schema {
		if d := lev(name, k.Name); d < bd {
			best, bd = k.Name, d
		}
	}
	return best
}

func lev(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := 1
			if a[i-1] == b[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev = cur
	}
	return prev[len(b)]
}

// unknown reports an unrecognized key; the suggestion is computed on the bare setting name and shown in the
// form the person typed it (setting, TASKLANE_ variable or --flag).
func unknown(kind, shown string) error {
	bare := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(shown, "--"), "TASKLANE_"))
	bare = strings.ReplaceAll(bare, "-", "_")
	if s := suggest(bare); s != "" {
		sugg := s
		switch kind {
		case "environment variable":
			sugg = envName(s)
		case "flag":
			sugg = "--" + strings.ReplaceAll(s, "_", "-")
		}
		return fmt.Errorf("unknown %s %q (did you mean %q?)", kind, shown, sugg)
	}
	return fmt.Errorf("unknown %s %q", kind, shown)
}

// Resolve applies the precedence and returns every effective entry. All problems are reported together.
func Resolve(o Options) ([]Entry, error) {
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.ReadFile == nil {
		o.ReadFile = os.ReadFile
	}
	var errs []string
	vals := map[string]Entry{}
	for _, k := range Schema {
		vals[k.Name] = Entry{Key: k, Value: k.Default, Source: SrcDefault}
	}
	set := func(name, v string, src Source) {
		k, _ := find(name)
		vals[name] = Entry{Key: k, Value: v, Source: src}
	}
	env := map[string]string{}
	for _, kv := range o.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	// the file: TASKLANE_CONFIG, or ./tasklane.conf when present
	path := env["TASKLANE_CONFIG"]
	for i, a := range o.Args {
		if a == "--config" && i+1 < len(o.Args) {
			path = o.Args[i+1]
		} else if strings.HasPrefix(a, "--config=") {
			path = strings.TrimPrefix(a, "--config=")
		}
	}
	explicit := path != ""
	if path == "" {
		path = "tasklane.conf"
	}
	if b, err := o.ReadFile(path); err == nil {
		for n, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			i := strings.IndexByte(line, '=')
			if i < 0 {
				errs = append(errs, fmt.Sprintf("%s:%d: expected key = value", path, n+1))
				continue
			}
			name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[:i]), "TASKLANE_")))
			v := strings.Trim(strings.TrimSpace(line[i+1:]), `"`)
			if _, ok := find(name); !ok {
				errs = append(errs, fmt.Sprintf("%s:%d: %v", path, n+1, unknown("setting", name)))
				continue
			}
			set(name, v, SrcFile)
		}
	} else if explicit {
		errs = append(errs, fmt.Sprintf("cannot read config file %s: %v", path, err))
	}
	// the environment, including *_FILE secrets
	var envNames []string
	for k := range env {
		envNames = append(envNames, k)
	}
	sort.Strings(envNames)
	for _, ev := range envNames {
		if !strings.HasPrefix(ev, "TASKLANE_") || ev == "TASKLANE_CONFIG" {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(ev, "TASKLANE_"))
		fromFile := false
		if strings.HasSuffix(name, "_file") {
			if _, ok := find(strings.TrimSuffix(name, "_file")); ok {
				name, fromFile = strings.TrimSuffix(name, "_file"), true
			}
		}
		k, ok := find(name)
		if !ok {
			errs = append(errs, unknown("environment variable", ev).Error())
			continue
		}
		v := env[ev]
		if fromFile {
			if !k.Secret {
				errs = append(errs, fmt.Sprintf("%s: only secret settings can be read from a file", ev))
				continue
			}
			b, err := o.ReadFile(v)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: cannot read secret file: %v", ev, err))
				continue
			}
			v = strings.TrimRight(string(b), "\r\n")
			if _, direct := env[envName(name)]; direct {
				errs = append(errs, fmt.Sprintf("%s and %s are both set; use one", envName(name), ev))
			}
		}
		set(name, v, SrcEnv)
	}
	// flags
	for i := 0; i < len(o.Args); i++ {
		a := o.Args[i]
		if !strings.HasPrefix(a, "--") {
			errs = append(errs, fmt.Sprintf("unexpected argument %q", a))
			continue
		}
		a = strings.TrimPrefix(a, "--")
		var v string
		hasV := false
		if j := strings.IndexByte(a, '='); j >= 0 {
			a, v, hasV = a[:j], a[j+1:], true
		}
		if a == "config" || a == "redacted" || a == "reveal" {
			if !hasV && a == "config" {
				i++
			}
			continue
		}
		name := strings.ReplaceAll(a, "-", "_")
		if _, ok := find(name); !ok {
			errs = append(errs, unknown("flag", "--"+a).Error())
			continue
		}
		if !hasV {
			if i+1 >= len(o.Args) {
				errs = append(errs, fmt.Sprintf("flag --%s needs a value", a))
				continue
			}
			i++
			v = o.Args[i]
		}
		set(name, v, SrcFlag)
	}
	// types and units
	for _, k := range Schema {
		e := vals[k.Name]
		if err := validate(k, e.Value); err != nil {
			errs = append(errs, fmt.Sprintf("%s (from %s): %v", k.Name, e.Source, err))
		}
	}
	out := make([]Entry, 0, len(Schema))
	for _, k := range Schema {
		out = append(out, vals[k.Name])
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return out, nil
}

func validate(k Key, v string) error {
	switch k.Kind {
	case "int":
		if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
			return fmt.Errorf("%q is not a non-negative whole number", v)
		}
	case "duration":
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			return fmt.Errorf("%q is not a duration such as 30s, 15m or 336h", v)
		}
	case "enum":
		for _, e := range k.Enum {
			if v == e {
				return nil
			}
		}
		return fmt.Errorf("%q is not one of %s", v, strings.Join(k.Enum, ", "))
	}
	return nil
}

// Load resolves the configuration or returns the combined error.
func Load(o Options) (Config, error) {
	es, err := Resolve(o)
	if err != nil {
		return Config{}, err
	}
	m := map[string]string{}
	for _, e := range es {
		m[e.Key.Name] = e.Value
	}
	n := func(k string) int64 { v, _ := strconv.ParseInt(m[k], 10, 64); return v }
	d := func(k string) time.Duration { v, _ := time.ParseDuration(m[k]); return v }
	return Config{Listen: m["listen"], DatabaseURL: m["database_url"], BlobDir: m["blob_dir"], SecretKey: m["secret_key"], SearchEngine: m["search_engine"], OSURL: m["opensearch_url"], OSUser: m["opensearch_user"], OSPassword: m["opensearch_password"],
		OSAPIKey: m["opensearch_api_key"], OSAllowPrivate: m["opensearch_allow_private"] == "true", WebhookAllowPrivate: m["webhook_allow_private"] == "true", PasswordMinLength: n("password_min_length"), OSIndex: m["opensearch_index"], OSShards: n("opensearch_shards"), OSReplicas: n("opensearch_replicas"), MaxUploadMB: n("max_upload_mb"),
		BlobBackend: m["blob_backend"], S3Endpoint: m["s3_endpoint"], S3Region: m["s3_region"], S3Bucket: m["s3_bucket"], S3Prefix: m["s3_prefix"], S3AccessKey: m["s3_access_key"],
		S3SecretKey: m["s3_secret_key"], S3PathStyle: m["s3_path_style"] == "true", S3SSE: m["s3_sse"], S3KMSKeyID: m["s3_kms_key_id"], S3PartSizeMB: n("s3_part_size_mb"),
		SessionTTL: d("session_ttl"), BaseURL: m["base_url"], SMTPHost: m["smtp_host"], SMTPPort: int(n("smtp_port")), SMTPFrom: m["smtp_from"],
		SMTPTLS: m["smtp_tls"], SMTPUser: m["smtp_user"], SMTPPass: m["smtp_pass"], AdminEmail: m["admin_email"], AdminPassword: m["admin_password"],
		PresenceTTL: d("presence_ttl")}, nil
}

// LoadEnv is the convenience used by tools that take no flags.
func LoadEnv() Config { c, _ := Load(Options{}); return c }

// Print renders the effective configuration. Secrets are always shown as a mask (or "(unset)"), never their value.
func Print(es []Entry) string {
	var b strings.Builder
	for _, e := range es {
		v := e.Value
		if e.Key.Secret {
			if v == "" {
				v = "(unset)"
			} else {
				v = "********"
			}
		}
		fmt.Fprintf(&b, "%-16s = %-40s [%s]\n", e.Key.Name, v, e.Source)
	}
	return b.String()
}

// Docs renders the reference table from the schema; a test keeps docs/CONFIGURATION.md in step with it.
func Docs() string {
	var b strings.Builder
	b.WriteString("| Setting | Environment variable | Flag | Default | Meaning |\n|---|---|---|---|---|\n")
	for _, k := range Schema {
		def := "`" + k.Default + "`"
		if k.Default == "" {
			def = "unset"
		}
		if k.Secret {
			def = map[bool]string{true: "unset", false: "(secret)"}[k.Default == ""]
		}
		env := "`" + envName(k.Name) + "`"
		if k.Secret {
			env += ", `" + envName(k.Name) + "_FILE`"
		}
		fmt.Fprintf(&b, "| `%s` | %s | `--%s` | %s | %s |\n", k.Name, env, strings.ReplaceAll(k.Name, "_", "-"), def, k.Doc)
	}
	return b.String()
}
