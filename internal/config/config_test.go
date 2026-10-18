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
