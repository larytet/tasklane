package domain

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// UserSettings are a person's interface settings, stored on the server so that they follow the person between browsers.
type UserSettings struct {
	ListView     string `json:"list_view"`     // "short" (title, status, assignee) or "long" (adds the start of the description)
	PageWidth    string `json:"page_width"`    // "wide" (the whole window) or "narrow" (a column, like a printed page)
	SnippetChars int    `json:"snippet_chars"` // how much of a description the long view shows
}

const (
	DefaultSnippetChars = 500
	MaxSnippetChars     = 5000
)

func defaultSettings() UserSettings {
	return UserSettings{ListView: "short", PageWidth: "wide", SnippetChars: DefaultSnippetChars}
}

// MySettings and SetSettings are the first three settings (GET and PUT /api/v1/me/settings); they share the row and the version with
// the rest (UIPrefs), so a save through either route is seen by the other.
func (s *Service) MySettings(ctx context.Context, a Actor) UserSettings {
	d, err := s.uiPrefsOf(ctx, s.DB, a.ID, false)
	if err != nil {
		return defaultSettings()
	}
	return d.Prefs.UserSettings
}

func (s *Service) SetSettings(ctx context.Context, a Actor, p UserSettings) (UserSettings, error) {
	if p.ListView != "short" && p.ListView != "long" {
		return p, invalid("list_view is short or long")
	}
	if p.PageWidth != "wide" && p.PageWidth != "narrow" {
		return p, invalid("page_width is wide or narrow")
	}
	if p.SnippetChars < 1 || p.SnippetChars > MaxSnippetChars {
		return p, invalid("snippet_chars is between 1 and %d", MaxSnippetChars)
	}
	raw, _ := json.Marshal(p)
	d, err := s.SetUIPrefs(ctx, a, raw, nil)
	return d.Prefs.UserSettings, err
}

// Snippets returns the start of the description of each issue named, cut at n characters (not bytes) with an ellipsis. The keys come from a
// search that already applied the person's read access; this does no access check of its own and must not be given other keys.
func (s *Service) Snippets(ctx context.Context, keys []string, n int) (map[string]string, error) {
	out := map[string]string{}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT i.key, t.body FROM issue i JOIN issue_text t ON t.issue_id=i.id AND t.field='description' WHERE i.key = ANY($1)`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, body string
		if rows.Scan(&k, &body) != nil {
			continue
		}
		body = strings.TrimSpace(body)
		if utf8.RuneCountInString(body) > n {
			body = strings.TrimRightFunc(string([]rune(body)[:n]), func(r rune) bool { return r == ' ' || r == '\n' }) + "…"
		}
		if body != "" {
			out[k] = body
		}
	}
	return out, rows.Err()
}
