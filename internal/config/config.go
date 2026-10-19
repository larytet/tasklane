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
	Listen                                    string
	DatabaseURL                               string
	BlobDir                                   string
	SearchEngine                              string
	OSURL                                     string
	OSUser                                    string
	OSPassword                                string
	OSAPIKey                                  string
	OSAllowPrivate                            bool
	WebhookAllowPrivate                       bool
	PasswordMinLength                         int64
	SSOAllowPrivate                           bool
	OSIndex                                   string
	OSShards                                  int64
	OSReplicas                                int64
	SecretKey                                 string
	BlobBackend                               string
	BlobFallback                              string
	BlobFallbackDir                           string
	BlobEncKeys                               string
	IMAPAllowPrivate                          bool
	MailRawDays, MailBounceDays, MinClient    int
	WebAuthnRPID                              string
	TrustedProxies                            []string
	DashboardIframeHosts                      []string
	DrainDelay                                time.Duration
	Realtime                                  bool
	Hosted                                    bool
	HostedRegion                              string
	WebAuthnOrigins                           []string
	WebAuthnRequireUV                         bool
	BlobCold, BlobColdDir                     string
	SMTPListen, SMTPListenCert, SMTPListenKey string
	BlobEncCurrent                            string
	S3Endpoint                                string
	S3Region                                  string
	S3Bucket                                  string
	S3Prefix                                  string
	S3AccessKey                               string
	S3SecretKey                               string
	S3PathStyle                               bool
	S3SSE                                     string
	S3KMSKeyID                                string
	S3PartSizeMB                              int64
	MaxUploadMB                               int64
	SessionTTL                                time.Duration
	BaseURL                                   string
	SMTPHost                                  string
	SMTPPort                                  int
	SMTPFrom                                  string
	SMTPTLS                                   string
	SMTPUser                                  string
	SMTPPass                                  string
	AdminEmail                                string
	AdminPassword                             string
	PresenceTTL                               time.Duration
}

// Key is one setting. The environment variable is TASKLANE_ plus the upper-cased name, the flag is --name
// with dashes, and the config file key is the name.
type Key struct {
	Name    string
	Default string
	Kind    string // string, int, duration, enum
	Enum    []string
	Secret  bool
	Zero    bool // a duration may be 0 (meaning none)
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
	{Name: "blob_fallback", Default: "none", Kind: "enum", Enum: []string{"none", "fs", "s3"}, Doc: "While moving attachments from one backend to another: the old backend. Reads that are not in `blob_backend` are answered from it (and copied across), writes go to `blob_backend` only. `fs` uses `blob_fallback_dir`; `s3` uses the `s3_*` settings. Run `tasklane blobs migrate` to copy the rest, then set this back to `none`"},
	{Name: "blob_encryption_keys", Default: "", Kind: "string", Secret: true, Doc: "Encrypt attachments at rest. A comma-separated list of `id:hex` master keys (id 0-255, 64 hex characters = 32 bytes each), e.g. `1:ab12...`. Generate one with `openssl rand -hex 32`. Keep old keys in the list until `tasklane blobs rewrap` has moved every object to the current one. Empty: attachments are stored as they are"},
	{Name: "blob_encryption_current", Default: "", Kind: "string", Doc: "The id of the master key new and rewrapped attachments use. Required when `blob_encryption_keys` is set"},
	{Name: "blob_cold_backend", Default: "none", Kind: "enum", Enum: []string{"none", "fs", "s3"}, Doc: "A cold tier for attachments (SPEC 17.6): objects that match a tiering rule (class, age, size; set in Admin or with the API) move from `blob_backend` to this one, and reads stay transparent. `fs` uses `blob_cold_dir`; `s3` uses the `s3_*` settings, so `blob_backend` must then be `fs`. It must differ from `blob_backend`"},
	{Name: "blob_cold_dir", Default: "", Kind: "string", Doc: "The cold tier directory when `blob_cold_backend=fs`"},
	{Name: "smtp_listen", Default: "", Kind: "string", Doc: "Receive inbound email directly: the address for the built-in SMTP receiver to listen on, such as `:25` (empty: off). It accepts mail only for the reply, bounce and project addresses of the mail domain (the host of `base_url`), never relays, and never sends a bounce. Mail it receives needs its own DKIM signature to count; put a receiver you run in front and use the webhook if you want its verdict believed"},
	{Name: "smtp_listen_tls_cert", Default: "", Kind: "string", Doc: "Certificate file (PEM) for STARTTLS on the built-in SMTP receiver; with `smtp_listen_tls_key`. Without them STARTTLS is not offered"},
	{Name: "smtp_listen_tls_key", Default: "", Kind: "string", Doc: "Private key file (PEM) for STARTTLS on the built-in SMTP receiver"},
	{Name: "dashboard_iframe_hosts", Default: "", Kind: "string", Doc: "Comma-separated host names that dashboard iframe gadgets may embed (https only), for example `wiki.example.com`. Empty: no iframe gadget can be made. The page's Content-Security-Policy allows frames from exactly these hosts and no others"},
	{Name: "hosted", Default: "false", Kind: "enum", Enum: []string{"true", "false"}, Doc: "Run as the hosted edition (SPEC 17.9): the operator can give each organization a plan (seats, projects, storage, region) that is enforced, and usage is metered daily for billing. Off, none of this exists and nothing is limited or counted"},
	{Name: "hosted_region", Default: "", Kind: "string", Doc: "The region this deployment serves when `hosted=true`, such as `eu-west`. A tenant whose plan names another region is refused here"},
	{Name: "realtime", Default: "true", Kind: "enum", Enum: []string{"true", "false"}, Doc: "The realtime layer: live presence and change events over a WebSocket. Off, pages work without it: people appear as present through periodic requests, saves still go through the normal conflict check, and nothing depends on the socket"},
	{Name: "shutdown_drain_delay", Default: "5s", Kind: "duration", Zero: true, Doc: "How long a stopping server keeps serving after it starts failing its readiness check, so that a load balancer notices and moves new requests to other nodes before this one stops accepting them. Open pages are told to reconnect (spread over a second); requests in flight finish. 0 stops at once"},
	{Name: "trusted_proxies", Default: "", Kind: "string", Doc: "Comma-separated networks of reverse proxies in front of Tasklane. Only from these is the X-Forwarded-For header believed, to learn a visitor's real address (the sign-in record, lockouts and an organization's allowed networks use it). Empty: the connecting address is used and the header is ignored. Never list a network that visitors can reach directly"},
	{Name: "webauthn_rp_id", Default: "", Kind: "string", Doc: "The relying party id that passkeys and security keys are bound to: a host name (never an IP address). Empty: the host of `base_url`. A key registered for one id does not work for another, so change it only if you mean to re-register keys"},
	{Name: "webauthn_origins", Default: "", Kind: "string", Doc: "Comma-separated origins (scheme, host and port) pages may use passkeys from. Empty: the origin of `base_url`"},
	{Name: "webauthn_require_user_verification", Default: "false", Kind: "enum", Enum: []string{"true", "false"}, Doc: "Require the key to verify the person (a PIN or biometric) as well as being present. Off: presence is enough as a second factor after the password"},
	{Name: "min_client_version", Default: "0", Kind: "int", Doc: "The oldest browser client the server still serves (0: the previous one). A page older than this is told to reload, keeping its drafts. Raise it to force a reload after an upgrade that a cached page cannot survive; API tokens and MCP clients are never affected"},
	{Name: "mail_raw_retention_days", Default: "90", Kind: "int", Doc: "How long the original of an inbound message, its text, its subject and its sender are kept, in days (SPEC 17.3). After that the message row stays without them, so a redelivery is still recognised. 0 keeps them for ever. Mail waiting for a decision and mail on an issue under legal hold is never purged. Erasing a person removes their mail at once, whatever this is"},
	{Name: "mail_bounce_retention_days", Default: "365", Kind: "int", Doc: "How long bounce records and cleared suppressions are kept, in days (0 keeps them). An address that is still suppressed is never removed by age"},
	{Name: "imap_allow_private", Default: "false", Kind: "enum", Enum: []string{"true", "false"}, Doc: "Allow inbound-mail mailboxes (IMAP) on loopback and private addresses. Off by default so a mailbox setting cannot be used to reach internal services; cloud metadata addresses stay blocked either way"},
	{Name: "blob_fallback_dir", Default: "", Kind: "string", Doc: "The old attachment directory when `blob_fallback=fs`"},
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
	{Name: "sso_allow_private", Default: "false", Kind: "enum", Enum: []string{"true", "false"}, Doc: "Allow single sign-on identity providers on private or loopback addresses (for a provider inside your network); refused by default"},
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

// Rename records a setting whose name changed or that was removed (UPG-11). For one minor release the old name still works: its
// value is used under the new name (or ignored when the setting is gone) and a warning says so; after that the entry is deleted
// here and the old name is an unknown setting. Nothing is listed yet because there has been no release to rename anything in.
type Rename struct {
	Old   string // the old setting name, lower case with underscores
	New   string // the new name; empty when the setting was removed
	Since string // the release that renamed or removed it, for the message
	Note  string // for a removed setting: what replaces it
}

var Renamed = []Rename{}

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
	// Renames overrides the table of renamed settings (tests); Warn receives the warnings (default: standard error).
	Renames []Rename
	Warn    func(string)
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
	renames := o.Renames
	if renames == nil {
		renames = Renamed
	}
	warn := o.Warn
	if warn == nil {
		warn = func(m string) { fmt.Fprintln(os.Stderr, "warning: "+m) }
	}
	// tr maps an old name to its current one; skip says the setting is gone and its value is dropped
	tr := func(name string, src Source) (cur string, skip bool) {
		for _, r := range renames {
			if r.Old != name {
				continue
			}
			if r.New == "" {
				warn(fmt.Sprintf("setting %q was removed in %s and is ignored%s; delete it from your configuration (it will be an error in the next minor release)", name, r.Since, map[bool]string{true: ": " + r.Note, false: ""}[r.Note != ""]))
				return "", true
			}
			warn(fmt.Sprintf("setting %q was renamed to %q in %s: using it for now; rename it (the old name stops working in the next minor release)", name, r.New, r.Since))
			return r.New, false
		}
		return name, false
	}
	vals := map[string]Entry{}
	for _, k := range Schema {
		vals[k.Name] = Entry{Key: k, Value: k.Default, Source: SrcDefault}
	}
	set := func(name, v string, src Source) {
		k, _ := find(name)
		vals[name] = Entry{Key: k, Value: v, Source: src}
	}
	// setRenamed applies a value given under an old name, unless the same layer also gave the new name: the new name wins
	setRenamed := func(old, cur, v string, src Source) {
		if e := vals[cur]; e.Source == src {
			warn(fmt.Sprintf("both %q and its new name %q are set; using %q", old, cur, cur))
			return
		}
		set(cur, v, src)
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
			old := name
			if _, ok := find(name); !ok {
				var skip bool
				if name, skip = tr(name, SrcFile); skip {
					continue
				}
			}
			if _, ok := find(name); !ok {
				errs = append(errs, fmt.Sprintf("%s:%d: %v", path, n+1, unknown("setting", name)))
				continue
			}
			if old != name {
				setRenamed(old, name, v, SrcFile)
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
		oldName := ""
		if _, known := find(strings.TrimSuffix(name, "_file")); !known {
			base, suffix := name, ""
			if strings.HasSuffix(name, "_file") {
				base, suffix = strings.TrimSuffix(name, "_file"), "_file"
			}
			if cur, skip := tr(base, SrcEnv); skip {
				continue
			} else if cur != base {
				oldName, name = base, cur+suffix
			}
		}
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
		if oldName != "" {
			setRenamed(oldName, name, v, SrcEnv)
			continue
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
		oldFlag := ""
		if _, ok := find(name); !ok {
			cur, skip := tr(name, SrcFlag)
			if skip {
				if !hasV && i+1 < len(o.Args) && !strings.HasPrefix(o.Args[i+1], "--") {
					i++
				}
				continue
			}
			if cur != name {
				oldFlag, name = name, cur
			}
		}
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
		if oldFlag != "" {
			setRenamed(oldFlag, name, v, SrcFlag)
			continue
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
		if d, err := time.ParseDuration(v); err != nil || d < 0 || (d == 0 && !k.Zero) {
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
		OSAPIKey: m["opensearch_api_key"], OSAllowPrivate: m["opensearch_allow_private"] == "true", WebhookAllowPrivate: m["webhook_allow_private"] == "true", PasswordMinLength: n("password_min_length"), SSOAllowPrivate: m["sso_allow_private"] == "true", OSIndex: m["opensearch_index"], OSShards: n("opensearch_shards"), OSReplicas: n("opensearch_replicas"), MaxUploadMB: n("max_upload_mb"),
		BlobBackend: m["blob_backend"], BlobFallback: m["blob_fallback"], BlobFallbackDir: m["blob_fallback_dir"], BlobEncKeys: m["blob_encryption_keys"], IMAPAllowPrivate: m["imap_allow_private"] == "true", MinClient: int(n("min_client_version")), WebAuthnRPID: m["webauthn_rp_id"], TrustedProxies: splitList(m["trusted_proxies"]), DashboardIframeHosts: splitList(m["dashboard_iframe_hosts"]), DrainDelay: d("shutdown_drain_delay"), Realtime: m["realtime"] != "false", Hosted: m["hosted"] == "true", HostedRegion: m["hosted_region"], WebAuthnOrigins: splitList(m["webauthn_origins"]), WebAuthnRequireUV: m["webauthn_require_user_verification"] == "true", MailRawDays: int(n("mail_raw_retention_days")), MailBounceDays: int(n("mail_bounce_retention_days")), BlobCold: m["blob_cold_backend"], BlobColdDir: m["blob_cold_dir"], SMTPListen: m["smtp_listen"], SMTPListenCert: m["smtp_listen_tls_cert"], SMTPListenKey: m["smtp_listen_tls_key"], BlobEncCurrent: m["blob_encryption_current"], S3Endpoint: m["s3_endpoint"], S3Region: m["s3_region"], S3Bucket: m["s3_bucket"], S3Prefix: m["s3_prefix"], S3AccessKey: m["s3_access_key"],
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

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
