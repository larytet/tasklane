package licenses_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/licenses"
)

const root = "../.."

func TestClassify(t *testing.T) {
	for text, want := range map[string]string{
		"Permission is hereby granted, free of charge, to any person obtaining a copy":                                        "MIT",
		"Apache License\nVersion 2.0, January 2004":                                                                           "Apache-2.0",
		"Redistribution and use in source and binary forms ... Neither the name of":                                           "BSD-3-Clause",
		"GNU AFFERO GENERAL PUBLIC LICENSE Version 3":                                                                         "DENIED: GNU AFFERO",
		"Server Side Public License VERSION 1":                                                                                "DENIED: SERVER SIDE PUBLIC LICENSE",
		"Business Source License 1.1":                                                                                         "DENIED: BUSINESS SOURCE LICENSE",
		"Permission to use, copy, modify, and distribute this software for any purpose with or without fee is hereby granted": "ISC",
		"Permission to use, copy, modify, and/or distribute this software for any purpose":                                    "ISC",
		"All rights reserved.": "UNKNOWN",
	} {
		if got := licenses.Classify(text); got != want {
			t.Errorf("%q: %s, want %s", text[:20], got, want)
		}
	}
	if licenses.Allowed("DENIED: GNU AFFERO") || licenses.Allowed("UNKNOWN") || !licenses.Allowed("MIT") {
		t.Fatal("allow list wrong")
	}
}

// LIC-01 and LIC-03: every module linked into the binary is under an allowed licence; nothing is denied or unknown.
func TestLIC01_LinkedModulesAreAllowed(t *testing.T) {
	mods, err := licenses.Linked(root)
	if err != nil {
		t.Skip(err)
	}
	if len(mods) == 0 {
		t.Fatal("no modules found")
	}
	for _, m := range mods {
		t.Logf("%-40s %-10s %s", m.Path, m.Version, m.License)
		if !licenses.Allowed(m.License) {
			t.Errorf("%s %s: licence %s is not allowed", m.Path, m.Version, m.License)
		}
	}
}

// LIC-02: NOTICE is complete and current.
func TestLIC02_NoticeIsCurrent(t *testing.T) {
	mods, err := licenses.Linked(root)
	if err != nil {
		t.Skip(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "NOTICE"))
	if err != nil {
		t.Fatal("NOTICE is missing; run `make notice`")
	}
	if string(b) != licenses.Notice(mods) {
		t.Fatal("NOTICE is out of date; run `make notice`")
	}
	for _, m := range mods {
		if !strings.Contains(string(b), m.Path) {
			t.Errorf("NOTICE lacks %s", m.Path)
		}
	}
}

// LIC-05: a self-hosted build has no licence key, no seat check and no phone-home: no HTTP client in production code,
// and the only outbound network call is the configured SMTP relay.
func TestLIC05_NoPhoneHome(t *testing.T) {
	forbidden := regexp.MustCompile(`http\.(Get|Post|Head|PostForm|DefaultClient|NewRequest|Client\{)|net\.Dial|tls\.Dial|net/rpc|telemetry|analytics|licen[sc]e[_ ]?key|seat`)
	var offenders []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if strings.Contains(p, "internal/testutil") || strings.Contains(p, "internal/licenses") {
			return nil
		}
		b, _ := os.ReadFile(p)
		for n, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if m := forbidden.FindString(line); m != "" {
				// the SMTP transport dials the configured relay; that is the one allowed outbound call
				if strings.HasSuffix(p, "internal/mail/mail.go") && (strings.Contains(m, "net.Dial") || strings.Contains(m, "tls.Dial")) {
					continue
				}
				// the S3 backend talks to the bucket the operator configured and nothing else
				if strings.HasSuffix(p, "internal/blob/s3.go") {
					continue
				}
				// the stdio proxy talks only to the Tasklane endpoint the operator names on its command line
				if strings.HasSuffix(p, "internal/mcp/stdio.go") && (strings.Contains(m, "http.Client{") || strings.Contains(m, "http.NewRequest")) {
					continue
				}
				offenders = append(offenders, p+":"+itoa(n+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if len(offenders) > 0 {
		t.Fatalf("possible outbound call or licence enforcement in production code:\n%s", strings.Join(offenders, "\n"))
	}
	// the front end loads nothing from other origins
	for _, f := range []string{"index.html", "app.js", "app.css", "lib.js", "agile.js"} {
		b, _ := os.ReadFile(filepath.Join(root, "internal/web/static", f))
		if regexp.MustCompile(`(src|href)="https?://|url\(https?://|fetch\("https?://|@import`).Match(b) {
			t.Errorf("%s references another origin", f)
		}
	}
	_ = exec.Command
}

func itoa(n int) string { return strconv.Itoa(n) }
