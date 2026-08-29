package plane

import (
	"html"
	"regexp"
	"strings"
)

// The skill-page wire format.
//
// A skill page is a leading `<pre><code>` block holding the YAML
// frontmatter, followed by the markdown body rendered to HTML. Three facts
// about the fork make that safe, and each was checked rather than assumed:
//
//  1. The sanitizer KEEPS the block. Inbound description_html goes through
//     a validator that allows `class` globally plus `language` on pre and
//     code, so `<pre><code class="language-yaml">` survives unchanged.
//  2. The live editor REGENERATES from the html — every content write clears
//     the binary document — so the block round-trips through the editor as a
//     real code block. Which is why the matcher below is tolerant of
//     re-serialised attributes.
//  3. Search sees the CURRENT content: the page re-derives its stripped text
//     from the html on save.
//
// Decoding is deliberately lossy in the other direction: the body becomes
// plain text, because the consumer is a model reading prose rather than a
// browser rendering a document.

// leadingCodeBlock matches the `<pre><code …>` block carrying the
// frontmatter.
//
// TOLERANT of editor re-serialisation — leading whitespace, arbitrary
// attributes on both tags — because the editor rewrites them and a strict
// matcher would decode a page on import and fail to decode the same page
// after somebody opened it.
var leadingCodeBlock = regexp.MustCompile(
	`(?is)\A\s*<pre\b[^>]*>\s*<code\b[^>]*>(.*?)</code>\s*</pre>`)

var (
	blockBreak = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr|pre|blockquote)>`)
	lineBreak  = regexp.MustCompile(`(?i)<br\s*/?>`)
	anyHTMLTag = regexp.MustCompile(`<[^>]+>`)
	blankRun   = regexp.MustCompile(`\n{3,}`)
)

// DecodeSkillPage turns a page's html into the authored skill text.
//
// The result is the frontmatter followed by the flattened body — the same
// shape the skill parser reads from a file, so a page and a file cannot
// diverge in what they mean.
//
// Empty when the page carries no leading code block, which is how an
// ORDINARY page in the same container is recognised: a project home page or
// an operator's notes is not a broken skill.
func DecodeSkillPage(pageHTML string) string {
	match := leadingCodeBlock.FindStringSubmatch(pageHTML)
	if match == nil {
		return ""
	}
	frontmatter := strings.TrimSpace(html.UnescapeString(match[1]))
	if frontmatter == "" {
		return ""
	}
	body := HTMLToText(pageHTML[len(match[0]):])
	return "---\n" + frontmatter + "\n---\n" + body
}

// HTMLToText flattens rendered html to the prose a model reads.
//
// BLOCK ELEMENTS BECOME NEWLINES rather than disappearing: a list whose
// items ran together into one line reads as a single sentence, and a skill's
// body is mostly lists of steps.
func HTMLToText(fragment string) string {
	text := lineBreak.ReplaceAllString(fragment, "\n")
	text = blockBreak.ReplaceAllString(text, "\n\n")
	text = anyHTMLTag.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	// Trailing spaces on every line, because a stripped tag leaves the
	// space that separated it from the next word.
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		lines = append(lines, strings.TrimRight(line, " \t"))
	}
	text = strings.Join(lines, "\n")
	return strings.TrimSpace(blankRun.ReplaceAllString(text, "\n\n"))
}

// EncodeSkillPage builds the description_html for a skill page.
//
// The inverse of [DecodeSkillPage], used by the publishing CLI. The YAML
// goes in a code block because that is what the editor renders it as — an
// operator opening the page sees a code block they can edit, not a wall of
// escaped text — and `language-yaml` is what makes it highlight.
func EncodeSkillPage(frontmatter, bodyHTML string) string {
	return `<pre><code class="language-yaml">` +
		html.EscapeString(strings.TrimSpace(frontmatter)) +
		"</code></pre>" + bodyHTML
}
