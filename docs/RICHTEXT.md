# Rich text: Markdown and Atlassian Document Format

`internal/richtext` holds a document model for the text people write in tickets, and converters between it, Markdown and Atlassian Document Format (ADF). `POST /api/v1/richtext/convert` exposes them to any signed-in person or agent: `{"from": "markdown" | "adf", "to": "markdown" | "adf", "body": ...}` answers `{"body": ...}` and stores nothing.

What the model holds: paragraphs, headings (1 to 6), bullet and numbered lists (nested, with the number a list starts at), quotes, code blocks (with a language), rules, tables (a header row, then body rows, one paragraph per cell), and in text the marks bold, italic, strike, code and link, hard breaks, mentions, links to issues (`ABC-12`) and inline attachments.

**Markdown** is a strict dialect that reads back exactly: `**bold**`, `_italic_`, `~~strike~~`, `` `code` ``, `[text](url)`, a backslash at the end of a line for a hard break, `@[Name](user:ID)` for a mention, `[ABC-1](issue:ABC-1)` for an issue, `![name](attachment:ID)` for an attachment, `\` before any character that would otherwise mean something. Ordinary Markdown that people paste is understood as well (headings, lists, quotes, fences, pipe tables).

**ADF**: what the model holds converts both ways. Anything else (colours, emoji, status lozenges, panels, expands, media, a table with merged cells, a heading with a line break) becomes an opaque block that keeps the original JSON, so writing the document back as ADF gives the same JSON for those blocks, and in Markdown they are a code fence with the language `adf-opaque`. Two things are not kept on purpose: an empty paragraph, and a line break at the end of a block.

The stored text of issues is still plain text with blank-line blocks (the editor is a text area), so none of this changes what is saved today; the converters are for importers, agents and the planned editor.
