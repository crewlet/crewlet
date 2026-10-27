package docindex

import (
	"fmt"
	"strings"
)

// Reference is the doc format as an agent reads it before writing a doc of
// several tabs: every key of `doc.json` with its type ([IndexForm], the strict
// decode's own tree) and its bounds.
func Reference() string {
	var b strings.Builder
	b.WriteString("# A doc: one Markdown file, or " + IndexPath + " and its tabs\n\n")
	fmt.Fprintf(&b, "A doc of one tab is one Markdown file (`index.md`). A doc of several tabs is "+
		"`%s` at the version's root naming the Markdown file of each tab. A key not listed is "+
		"refused, and so is a value of another type.\n\n## Every key of %s\n\n", IndexPath, IndexPath)
	b.WriteString(IndexForm.Outline(map[string]string{
		"v":            fmt.Sprintf("required, always %d", IndexVersion),
		"tabs":         fmt.Sprintf("1 to %d tabs, in order", MaxTabs),
		"tabs[].path":  "a Markdown file of the version (text/markdown), each tab its own",
		"tabs[].title": fmt.Sprintf("the tab's title, at most %d bytes", MaxTitleBytes),
	}))
	return b.String()
}
