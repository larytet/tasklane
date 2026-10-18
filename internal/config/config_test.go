package config_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/config"
)

func opts(args []string, env map[string]string, files map[string]string) config.Options {
	var e []string
	for k, v := range env {
		e = append(e, k+"="+v)
	}
	return config.Options{Args: args, Environ: func() []string { return e }, ReadFile: func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, os.ErrNotExist
	}}
}

func TestCFG01_PrecedenceDefaultsFileEnvFlags(t *testing.T) {
	files := map[string]string{"tasklane.conf": "# comment\nlisten = :1111\nblob_dir = /file/blobs\nmax_upload_mb = 5\nbase_url = \"https://file.example.com\"\n"}
	env := map[string]string{"TASKLANE_LISTEN": ":2222", "TASKLANE_BLOB_DIR": "/env/blobs"}
	c, err := config.Load(opts([]string{"--listen=:3333"}, env, files))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":3333" { // flag beats environment beats file beats default
		t.Fatalf("listen %q", c.Listen)
	}
	if c.BlobDir != "/env/blobs" || c.MaxUploadMB != 5 || c.BaseURL != "https://file.example.com" || c.SMTPPort != 25 || c.SessionTTL != 336*time.Hour {
		t.Fatalf("%+v", c)
	}
	es, _ := config.Resolve(opts([]string{"--listen", ":3333"}, env, files))
	src := map[string]config.Source{}
	for _, e := range es {
		src[e.Key.Name] = e.Source
	}
	if src["listen"] != config.SrcFlag || src["blob_dir"] != config.SrcEnv || src["max_upload_mb"] != config.SrcFile || src["smtp_port"] != config.SrcDefault {
		t.Fatalf("%v", src)
	}
	// an explicit --config that cannot be read is an error; the implicit file is optional
	if _, err := config.Load(opts([]string{"--config=/nope.conf"}, nil, nil)); err == nil || !strings.Contains(err.Error(), "cannot read config file") {
		t.Fatalf("%v", err)
	}
	if _, err := config.Load(opts(nil, nil, nil)); err != nil {
		t.Fatal(err)
	}
}

func TestCFG01_SecretsFromFilesAndNeverPrinted(t *testing.T) {
	env := map[string]string{"TASKLANE_DATABASE_URL_FILE": "/run/secrets/db", "TASKLANE_SMTP_PASS": "hunter2-direct"}
	files := map[string]string{"/run/secrets/db": "postgres://u:s3cret-from-file@db/app\n"}
	c, err := config.Load(opts(nil, env, files))
	if err != nil || c.DatabaseURL != "postgres://u:s3cret-from-file@db/app" || c.SMTPPass != "hunter2-direct" {
		t.Fatalf("%v %+v", err, c)
	}
	es, _ := config.Resolve(opts(nil, env, files))
	out := config.Print(es)
	for _, secret := range []string{"s3cret-from-file", "hunter2-direct", "postgres://u"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q printed:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "********") || !strings.Contains(out, "admin_password") || !strings.Contains(out, "(unset)") {
		t.Fatal(out)
	}
	// errors never echo a secret value either
	_, err = config.Load(opts(nil, map[string]string{"TASKLANE_DATABASE_URL_FILE": "/missing", "TASKLANE_SMTP_PASS": "leak-me", "TASKLANE_SMTP_PORT": "x"}, nil))
	if err == nil || strings.Contains(err.Error(), "leak-me") {
		t.Fatalf("%v", err)
	}
	// _FILE on a non-secret, and both forms at once
	if _, err := config.Load(opts(nil, map[string]string{"TASKLANE_LISTEN_FILE": "/x"}, map[string]string{"/x": ":1"})); err == nil {
		t.Fatal("non-secret _FILE accepted")
	}
	if _, err := config.Load(opts(nil, map[string]string{"TASKLANE_SMTP_PASS_FILE": "/p", "TASKLANE_SMTP_PASS": "x"}, map[string]string{"/p": "y"})); err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("%v", err)
	}
}

func TestCFG01_UnknownKeysSuggestAndTypeErrors(t *testing.T) {
	_, err := config.Load(opts([]string{"--listn=:1", "--bogus"}, map[string]string{"TASKLANE_BLOBDIR": "/x", "TASKLANE_SMTP_TLS": "ssl", "TASKLANE_MAX_UPLOAD_MB": "-3", "TASKLANE_SESSION_TTL": "two weeks"},
		map[string]string{"tasklane.conf": "databse_url = x\nnonsense\n"}))
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	for _, want := range []string{`unknown flag "--listn" (did you mean "--listen"?)`, `unknown environment variable "TASKLANE_BLOBDIR" (did you mean "TASKLANE_BLOB_DIR"?)`, `unknown setting "databse_url" (did you mean "database_url"?)`,
		`unknown flag "--bogus"`, `"ssl" is not one of none, starttls, tls`, "not a non-negative whole number", "not a duration", "expected key = value"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg) // every problem is reported at once
		}
	}
}

// CFG-02: the reference table in docs/CONFIGURATION.md is generated from the schema and must not drift.
func TestCFG02_ReferenceMatchesSchema(t *testing.T) {
	b, err := os.ReadFile("../../docs/CONFIGURATION.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	const begin, end = "<!-- config-table:begin -->\n", "<!-- config-table:end -->"
	i, j := strings.Index(doc, begin), strings.Index(doc, end)
	if i < 0 || j < i {
		t.Fatal("docs/CONFIGURATION.md has no generated table markers")
	}
	if got := doc[i+len(begin) : j]; got != config.Docs() {
		t.Fatalf("docs/CONFIGURATION.md is out of date; run `tasklane config docs` and paste the table.\nwant:\n%s\ngot:\n%s", config.Docs(), got)
	}
}

// UPG-11: a setting that was renamed or removed still works for one minor release, with a warning.
func TestUPG11_RenamedAndRemovedSettingsAreMappedWithAWarning(t *testing.T) {
	renames := []config.Rename{
		{Old: "smtp_server", New: "smtp_host", Since: "1.3.0"},
		{Old: "smtp_password", New: "smtp_pass", Since: "1.3.0"},
		{Old: "legacy_search", Since: "1.3.0", Note: "use search_engine"},
	}
	run := func(env []string, file string, args ...string) (map[string]config.Entry, []string, error) {
		var warns []string
		es, err := config.Resolve(config.Options{Args: args, Environ: func() []string { return env },
			ReadFile: func(p string) ([]byte, error) {
				if p == "tasklane.conf" && file != "" {
					return []byte(file), nil
				}
				if p == "/run/secret" {
					return []byte("s3cret\n"), nil
				}
				return nil, os.ErrNotExist
			}, Renames: renames, Warn: func(m string) { warns = append(warns, m) }})
		out := map[string]config.Entry{}
		for _, e := range es {
			out[e.Key.Name] = e
		}
		return out, warns, err
	}
	// the three layers each accept the old name
	for name, c := range map[string]struct {
		env  []string
		file string
		args []string
		src  config.Source
	}{
		"environment": {env: []string{"TASKLANE_SMTP_SERVER=mx.example.com"}, src: config.SrcEnv},
		"file":        {file: "smtp_server = mx.example.com", src: config.SrcFile},
		"flag":        {args: []string{"--smtp-server", "mx.example.com"}, src: config.SrcFlag},
	} {
		es, warns, err := run(c.env, c.file, c.args...)
		if err != nil || es["smtp_host"].Value != "mx.example.com" || es["smtp_host"].Source != c.src {
			t.Errorf("%s: %+v %v", name, es["smtp_host"], err)
		}
		if len(warns) != 1 || !strings.Contains(warns[0], `"smtp_server" was renamed to "smtp_host" in 1.3.0`) || !strings.Contains(warns[0], "next minor release") {
			t.Errorf("%s: warnings %q", name, warns)
		}
	}
	// the new name wins over the old one in the same layer, whichever is written first
	for _, env := range [][]string{{"TASKLANE_SMTP_SERVER=old", "TASKLANE_SMTP_HOST=new"}, {"TASKLANE_SMTP_HOST=new", "TASKLANE_SMTP_SERVER=old"}} {
		es, warns, err := run(env, "")
		if err != nil || es["smtp_host"].Value != "new" || len(warns) < 1 {
			t.Errorf("%v: %+v %v %q", env, es["smtp_host"], err, warns)
		}
	}
	// precedence between layers is unchanged: an old name in the environment still beats a new name in the file
	if es, _, _ := run([]string{"TASKLANE_SMTP_SERVER=from-env"}, "smtp_host = from-file"); es["smtp_host"].Value != "from-env" {
		t.Errorf("%+v", es["smtp_host"])
	}
	// a secret read from a file under its old name
	if es, _, err := run([]string{"TASKLANE_SMTP_PASSWORD_FILE=/run/secret"}, ""); err != nil || es["smtp_pass"].Value != "s3cret" {
		t.Errorf("%+v %v", es["smtp_pass"], err)
	}
	// a removed setting is ignored, with a warning that says what replaces it, in every layer
	for name, c := range map[string]struct {
		env  []string
		file string
		args []string
	}{"environment": {env: []string{"TASKLANE_LEGACY_SEARCH=on"}}, "file": {file: "legacy_search = on"}, "flag": {args: []string{"--legacy-search", "on"}}} {
		_, warns, err := run(c.env, c.file, c.args...)
		if err != nil || len(warns) != 1 || !strings.Contains(warns[0], "was removed in 1.3.0") || !strings.Contains(warns[0], "use search_engine") {
			t.Errorf("%s: %v %q", name, err, warns)
		}
	}
	// the old name is an ordinary unknown setting when it is not in the table (after the release it was kept for)
	var warns []string
	_, err := config.Resolve(config.Options{Environ: func() []string { return []string{"TASKLANE_SMTP_SERVER=x"} }, ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist }, Renames: []config.Rename{}, Warn: func(m string) { warns = append(warns, m) }})
	if err == nil || !strings.Contains(err.Error(), "unknown environment variable") || len(warns) != 0 {
		t.Errorf("%v %q", err, warns)
	}
}
