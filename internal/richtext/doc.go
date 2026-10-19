// Package richtext is the document model of Tasklane's rich text (SPEC 4.5, LIVE-06) and its converters: Markdown (a strict, documented dialect that
// round trips exactly) and Atlassian Document Format. The model has the nodes people use in tickets: paragraphs, headings, bullet and numbered
// lists, quotes, code blocks, rules, tables, and in text the marks bold, italic, strike, code and links, hard breaks, mentions, links to issues and
// inline attachments. Anything else that arrives in ADF is kept as an opaque block holding the original JSON, so nothing is lost on a round trip.
//
// Markdown dialect: `**bold**`, `_italic_`, `~~strike~~`, “ `code` “, `[text](url)`, `\` + newline for a hard break; a mention is
// `@[Name](user:ID)`, a link to an issue `[ABC-1](issue:ABC-1)`, an inline attachment `![name](attachment:ID)`; special characters in text are
// escaped with a backslash; a table is a header row, a separator row and body rows with one paragraph per cell; an opaque ADF block is a code fence
// whose language is `adf-opaque`.
package richtext

// Node types.
const (
	Doc         = "doc"
	Paragraph   = "paragraph"
	Heading     = "heading"
	BulletList  = "bulletList"
	OrderedList = "orderedList"
	ListItem    = "listItem"
	Quote       = "blockquote"
	CodeBlock   = "codeBlock"
	Rule        = "rule"
	Table       = "table"
	Row         = "tableRow"
	HeaderCell  = "tableHeader"
	Cell        = "tableCell"
	Text        = "text"
	HardBreak   = "hardBreak"
	Mention     = "mention"
	IssueLink   = "issueLink"
	Attachment  = "attachment"
	Opaque      = "opaque"
)

// Mark types, in the order they nest from the outside in.
const (
	MLink   = "link"
	MStrong = "strong"
	MEm     = "em"
	MStrike = "strike"
	MCode   = "code"
)

var markOrder = []string{MLink, MStrong, MEm, MStrike, MCode}

// Mark is a mark on a run of text; a link carries its target.
type Mark struct {
	Type string `json:"type"`
	Href string `json:"href,omitempty"`
}

// Node is a node of a document. Which fields matter depends on Type: Text has Text and Marks; Heading has Level; OrderedList has Start; CodeBlock has
// Text and Lang; Mention has ID and Text (the name); IssueLink has Text (the key); Attachment has ID and Text (the file name); Opaque has Raw (the
// original ADF JSON); the others have Content.
type Node struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Marks   []Mark `json:"marks,omitempty"`
	Level   int    `json:"level,omitempty"`
	Start   int    `json:"start,omitempty"`
	Lang    string `json:"lang,omitempty"`
	ID      string `json:"id,omitempty"`
	Raw     string `json:"raw,omitempty"`
	Content []Node `json:"content,omitempty"`
}
