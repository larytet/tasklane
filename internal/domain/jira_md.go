package domain

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/larytet/tasklane/internal/jira"
	"github.com/larytet/tasklane/internal/richtext"
)

// Descriptions of a project that writes through to Jira are kept and edited as Markdown (SPEC 18.1.1, WT-12). The Markdown is a working
// copy: it is made from Jira's own document when the sync applies an issue (and when a project is switched to write-through), and it
// is turned back into a Jira document only when a person saves. Jira stays the source of truth, and every conversion in either
// direction is proved to lose nothing or the edit is refused.

// jsonRoundTrip puts a document through JSON, as it is when it is sent to Jira and read back, so numbers have the types Jira's JSON gives.
func jsonRoundTrip(adf map[string]any) any {
	b, err := json.Marshal(adf)
	if err != nil {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v
}

// adfRoundTrips says whether a document written for the model reads back, after JSON, as the same model.
func adfRoundTrips(adf map[string]any, want richtext.Node) bool {
	v := jsonRoundTrip(adf)
	return v != nil && richtext.Equal(richtext.FromADF(v), want)
}

// descriptionMarkdown makes the Markdown a person edits from Jira's description document. The second result is false when the
// document cannot be turned into Markdown and back without losing something: such a description is edited in Jira.
func descriptionMarkdown(adf any) (string, bool) {
	if adf == nil {
		return "", true
	}
	m := richtext.FromADF(adf)
	md := richtext.ToMarkdown(m)
	if !richtext.Equal(richtext.FromMarkdown(md), m) || !adfRoundTrips(richtext.ToADF(m), m) {
		return "", false
	}
	return md, true
}

// markdownToADF turns edited Markdown into a Jira document and proves that the document reads back as what the Markdown meant. Blocks
// the model does not understand travel as opaque blocks (a fenced block in the Markdown) and are written back unchanged.
func markdownToADF(md string) (map[string]any, bool) {
	m := richtext.FromMarkdown(md)
	adf := richtext.ToADF(m)
	if !adfRoundTrips(adf, m) || !adfLinksSafe(adf) {
		return nil, false
	}
	return adf, true
}

// adfLinksSafe says that every link in a document points at http, https, mailto or a relative address: the dialect's own schemes became
// nodes (mentions, issue links, attachments), so a link that still carries another scheme is one a person typed, and is refused.
func adfLinksSafe(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if ms, ok := x["marks"].([]any); ok {
			for _, m := range ms {
				if mm, ok := m.(map[string]any); ok && mm["type"] == "link" {
					attrs, _ := mm["attrs"].(map[string]any)
					if href, _ := attrs["href"].(string); !schemeOK(href) {
						return false
					}
				}
			}
		}
		if c, ok := x["content"].([]any); ok {
			for _, e := range c {
				if !adfLinksSafe(e) {
					return false
				}
			}
		}
	}
	return true
}

// descriptionBody is what the mirror stores as the description of an issue: Markdown in a project that writes through (when Jira's
// document converts without loss), the plain text the mirror has always kept otherwise.
func descriptionBody(adf any, markdown bool) string {
	if adf == nil {
		return ""
	}
	if markdown {
		if md, ok := descriptionMarkdown(adf); ok {
			return md
		}
	}
	plain, _ := jira.ADFText(adf)
	return plain
}

// renderDescriptions rewrites the stored description of every issue of a mirrored project, as Markdown (the project now writes through)
// or as plain text (it reads only again), from the original Jira documents the sync kept. A changed text is a new version of it, from Jira.
func (s *Service) renderDescriptions(ctx context.Context, projectKey string, markdown bool) error {
	end, err := s.admitGate(ctx, projectKey)
	if err != nil {
		return err
	}
	defer end()
	rows, err := s.DB.Query(ctx, `SELECT i.id::text, a.adf FROM issue i JOIN project p ON p.id=i.project_id
		JOIN jira_adf a ON a.issue_id=i.id AND a.field='description' WHERE p.key=$1`, strings.ToUpper(projectKey))
	if err != nil {
		return err
	}
	type item struct {
		id  string
		adf any
	}
	var items []item
	for rows.Next() {
		var it item
		var raw []byte
		if err := rows.Scan(&it.id, &raw); err != nil {
			rows.Close()
			return err
		}
		json.Unmarshal(raw, &it.adf)
		items = append(items, it)
	}
	rows.Close()
	for _, it := range items {
		body := descriptionBody(it.adf, markdown)
		err := s.tx(ctx, func(tx pgx.Tx) error {
			var seq int64
			err := tx.QueryRow(ctx, `UPDATE issue_text SET body=$2,text_seq=text_seq+1,updated_at=now() WHERE issue_id::text=$1 AND field='description' AND body<>$2 RETURNING text_seq`, it.id, body).Scan(&seq)
			if err == pgx.ErrNoRows {
				return nil // already as wanted
			}
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO text_version(issue_id,field,seq,body,actor_id,actor_label,mode,at) VALUES($1::uuid,'description',$2,$3,NULL,'Jira','import',$4)`, it.id, seq, body, s.Now())
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
