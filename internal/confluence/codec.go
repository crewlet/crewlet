package confluence

import (
	"html"
	"regexp"
	"strings"
)

// The skill-page wire format.
//
// A skill page leads with a code block holding the YAML frontmatter,
// followed by the markdown body. Confluence writes a code block TWO ways and
// both have to decode, because which one a page carries depends on how it
// was authored:
//
//  1. `<ac:structured-macro ac:name="code">` with an `<ac:plain-text-body>`
//     CDATA section — what the editor produces, and what a page written by a
//     person will hold.
//  2. A plain `<pre><code>` block — valid storage format, and what an
//     importer or an API writer produces.
//
// A decoder that knew one would silently fail to recognise every page
// authored the other way, and an unrecognised skill page is indistinguishable
// from an ordinary page: the registry just does not have it, and every seat
// runs without that guidance.

var (
	// codeMacro matches Confluence's own code block, CDATA and all.
	codeMacro = regexp.MustCompile(
		`(?is)\A\s*<ac:structured-macro[^>]*ac:name="code".*?` +
			`<ac:plain-text-body>\s*(?:<!\[CDATA\[)?(.*?)(?:\]\]>)?\s*</ac:plain-text-body>.*?` +
			`</ac:structured-macro>`)

	// preCode matches a plain XHTML code block, tolerant of attributes.
	preCode = regexp.MustCompile(
		`(?is)\A\s*<pre\b[^>]*>\s*(?:<code\b[^>]*>)?(.*?)(?:</code>)?\s*</pre>`)
)

// DecodeSkillPage turns a page's storage format into the authored skill
// text: the frontmatter, then the flattened body.
//
// The SAME SHAPE the skill parser reads from a file, so a page and a file
// cannot diverge in what they mean.
//
// Empty when the page carries no leading code block, which is how an
// ORDINARY page in the same space is recognised — a space home page or an
// operator's notes is not a broken skill, and reporting it as one would fill
// the log with findings nobody can act on.
func DecodeSkillPage(storage string) string {
	frontmatter, rest := leadingCode(storage)
	if frontmatter == "" {
		return ""
	}
	body := Flatten(rest)
	if body == "" {
		return frontmatter
	}
	return frontmatter + "\n\n" + body
}

// leadingCode splits a page into its leading code block and what follows.
func leadingCode(storage string) (string, string) {
	for _, pattern := range []*regexp.Regexp{codeMacro, preCode} {
		if loc := pattern.FindStringSubmatchIndex(storage); loc != nil {
			block := strings.TrimSpace(html.UnescapeString(storage[loc[2]:loc[3]]))
			if block == "" {
				continue
			}
			return block, storage[loc[1]:]
		}
	}
	return "", storage
}

// EncodeSkillPage renders authored skill text as storage format.
//
// The INVERSE of the decode, and it has to be: a skill promoted from a draft
// is re-written, and a round trip that changed what the page means would
// change what every seat is told. The frontmatter goes into the macro form,
// because that is what the editor produces — a page written in the plain
// form and saved by a person comes back in the macro form anyway.
func EncodeSkillPage(frontmatter, body string) string {
	var b strings.Builder
	b.WriteString(`<ac:structured-macro ac:name="code">`)
	b.WriteString(`<ac:parameter ac:name="language">yaml</ac:parameter>`)
	b.WriteString(`<ac:plain-text-body><![CDATA[`)
	b.WriteString(strings.TrimSpace(frontmatter))
	b.WriteString(`]]></ac:plain-text-body></ac:structured-macro>`)
	for _, paragraph := range strings.Split(strings.TrimSpace(body), "\n\n") {
		if paragraph = strings.TrimSpace(paragraph); paragraph != "" {
			b.WriteString("<p>" + html.EscapeString(paragraph) + "</p>")
		}
	}
	return b.String()
}
