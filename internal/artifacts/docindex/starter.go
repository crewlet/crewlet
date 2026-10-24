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
	return Written(title, "Start writing here. Press Edit to change this text, and Save to publish it as a new version.")
}

// Written is the content a doc begun in the dashboard with its text already
// written publishes: one tab, the title as its heading and the body, as the
// writer typed it, under it. A body of nothing but space leaves the heading
// alone. Held to the doc format at publish like any version.
func Written(title, body string) map[string][]byte {
	var b strings.Builder
	b.WriteString("# ")
	for _, r := range strings.TrimSpace(title) {
		if strings.ContainsRune(markdownPunctuation, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('\n')
	if body = strings.TrimRight(body, " \t\r\n"); strings.TrimSpace(body) != "" {
		b.WriteByte('\n')
		b.WriteString(strings.TrimLeft(body, "\r\n"))
		b.WriteByte('\n')
	}
	return map[string][]byte{StarterEntry: []byte(b.String())}
}
