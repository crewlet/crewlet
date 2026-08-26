package confluence

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// Reading Confluence's storage format.
//
// A page's body is XHTML with Confluence's own macro elements mixed in —
// `<ac:structured-macro>`, `<ri:user>` — and every consumer here wants
// PROSE: a snippet for a search hit, the authored text of a skill, the body
// of a notification. So flattening is deliberately lossy, and the one thing
// it must not lose is where the line breaks were: a page whose bulleted list
// collapses into one sentence reads to a model as a different document.

var (
	blockBreak = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr|pre|blockquote|ac:rich-text-body)>`)
	lineBreak  = regexp.MustCompile(`(?i)<br\s*/?>`)
	anyTag     = regexp.MustCompile(`<[^>]+>`)
	blankRun   = regexp.MustCompile(`\n{3,}`)
	spaceRun   = regexp.MustCompile(`[ \t]+`)
)

// Flatten turns storage-format XHTML into the text a model reads.
//
// BLOCK BOUNDARIES SURVIVE as newlines and everything else collapses. The
// tag strip is a regular expression rather than a parse because the input is
// already trusted content the instance served — this is not a sanitiser, and
// treating it as one would mean refusing pages that render perfectly well.
func Flatten(storage string) string {
	if storage == "" {
		return ""
	}
	text := lineBreak.ReplaceAllString(storage, "\n")
	text = blockBreak.ReplaceAllString(text, "\n")
	text = anyTag.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = spaceRun.ReplaceAllString(text, " ")

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	text = strings.Join(lines, "\n")
	return strings.TrimSpace(blankRun.ReplaceAllString(text, "\n\n"))
}

// Snippet is the first sentence's worth of a page, capped.
//
// The knowledge block exists to tell a planner WHICH page to go and read,
// not to be the page — and a longer snippet buys nothing while multiplying
// by the hit count and the Plan phase's round cap.
func Snippet(text string, limit int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	for _, boundary := range []string{"\n", ". "} {
		if i := strings.Index(text, boundary); i > 0 {
			text = strings.TrimSpace(text[:i])
			break
		}
	}
	text = strings.ReplaceAll(text, "\n", " ")
	if limit > 0 && len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}

// MentionIDs are the accounts named in a page or comment body.
//
// Confluence writes a mention as `<ri:user ri:account-id="…"/>` on Cloud and
// `ri:userkey` on Data Center, so both are read — a parser that knew one
// would route nothing on the other, silently, because "nobody was mentioned"
// is an ordinary answer.
var mentionPattern = regexp.MustCompile(
	`<ri:user[^>]*ri:(?:account-id|userkey|username)="([^"]+)"`)

// MentionIDs returns the mentioned accounts in document order, deduped.
func MentionIDs(storage string) []string {
	var (
		out  []string
		seen = map[string]bool{}
	)
	for _, m := range mentionPattern.FindAllStringSubmatch(storage, -1) {
		if id := m[1]; id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// EscapeCQL escapes a value for a double-quoted CQL literal.
//
// Backslash first, or the quote escape's own backslash would be escaped a
// second time and the literal would end early — which is a query that either
// fails or, worse, means something the caller did not write.
func EscapeCQL(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}

// MaxQueryChars caps the text fed into a CQL `text ~` clause.
//
// A pathologically long fragment is both a slow query and a sign the
// auxiliary model that generated it misbehaved — and Confluence's own limit
// is not documented, so a bound here is what turns "the server refused a
// 4KB query" into "the search used the first 200 characters".
const MaxQueryChars = 200

// BuildCQL renders one search.
//
// Shape: `space IN ("ENG","HANDBOOK") AND type = page AND text ~ "…"`.
//
// AN EMPTY RESULT IS A REFUSAL, and the caller skips the request rather than
// searching. That happens in exactly one case worth stating: no scope AND a
// seat that does not authenticate as itself — searching the whole instance
// on the shared org credential is how one seat reads a page its own account
// never could. See [knowledge.Permitted].
func BuildCQL(text string, spaces []string, allowUnscoped bool) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if len(text) > MaxQueryChars {
		text = text[:MaxQueryChars]
	}
	if len(spaces) > 0 {
		quoted := make([]string, 0, len(spaces))
		for _, space := range spaces {
			quoted = append(quoted, `"`+EscapeCQL(strings.ToUpper(strings.TrimSpace(space)))+`"`)
		}
		return fmt.Sprintf(`space IN (%s) AND type = page AND text ~ "%s"`,
			strings.Join(quoted, ","), EscapeCQL(text))
	}
	if allowUnscoped {
		return fmt.Sprintf(`type = page AND text ~ "%s"`, EscapeCQL(text))
	}
	return ""
}
