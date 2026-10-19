package docindex

import "strings"

// StarterEntry is where a doc begun from its starter opens: one tab, the
// kind's first default entry.
const StarterEntry = "index.md"

// markdownPunctuation are the characters a title is escaped against, so a
// title reads as the words somebody typed rather than as Markdown: `*Q3*`
// is not emphasis and `#1` is not a heading.
const markdownPunctuation = "\\`*_{}[]()<>#+-.!|~"

// Starter is the content a doc begun in the dashboard publishes: one tab,
// titled, with a line saying where to begin. It is held to the doc format at
// publish like any version.
func Starter(title string) map[string][]byte {
	var b strings.Builder
	b.WriteString("# ")
	for _, r := range strings.TrimSpace(title) {
		if strings.ContainsRune(markdownPunctuation, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteString("\n\nStart writing here. Press Edit to change this text, and Save to publish it as a new version.\n")
	return map[string][]byte{StarterEntry: []byte(b.String())}
}
