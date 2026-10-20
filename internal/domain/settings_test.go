package domain_test

import (
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
)

// UI-08: interface settings are kept per person on the server; the defaults hold until a person changes them, and bad values are refused.
func TestUI08_SettingsPerPersonAndSnippets(t *testing.T) {
	e, _ := pauseEnv(t)
	d := e.Svc.MySettings(ctx, e.Alice)
	if d.ListView != "short" || d.PageWidth != "wide" || d.SnippetChars != 500 {
		t.Fatalf("defaults: %+v", d)
	}
	if _, err := e.Svc.SetSettings(ctx, e.Alice, domain.UserSettings{ListView: "long", PageWidth: "narrow", SnippetChars: 120}); err != nil {
		t.Fatal(err)
	}
	if got := e.Svc.MySettings(ctx, e.Alice); got.ListView != "long" || got.PageWidth != "narrow" || got.SnippetChars != 120 {
		t.Fatalf("stored: %+v", got)
	}
	if b := e.Svc.MySettings(ctx, e.Bob); b != d {
		t.Fatalf("another person's settings changed: %+v", b)
	}
	for _, bad := range []domain.UserSettings{{"tall", "wide", 500}, {"short", "huge", 500}, {"short", "wide", 0}, {"short", "wide", domain.MaxSnippetChars + 1}} {
		if _, err := e.Svc.SetSettings(ctx, e.Alice, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	is, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "with text", Description: strings.Repeat("é", 30)})
	if err != nil {
		t.Fatal(err)
	}
	k := is.Key
	sn, err := e.Svc.Snippets(ctx, []string{k}, 10)
	if err != nil || sn[k] != strings.Repeat("é", 10)+"…" { // counted in characters, not bytes
		t.Fatalf("%v %q", err, sn[k])
	}
	if sn, _ := e.Svc.Snippets(ctx, []string{k}, 100); sn[k] != strings.Repeat("é", 30) {
		t.Fatalf("short text must not be cut: %q", sn[k])
	}
}
