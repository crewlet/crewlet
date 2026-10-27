package dashspec

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/jsonform"
)

// Reference is the board format as an agent reads it before writing a
// dashboard: every key of `dashboard.json` with its type ([Form], the strict
// decode's own tree) and the closed sets the reader checks against.
func Reference() string {
	var b strings.Builder
	b.WriteString("# A dashboard: " + EntryPath + "\n\n")
	b.WriteString("A dashboard is `" + EntryPath + "` at the version's root, and any CSV or JSON " +
		"files its file datasets read. A key not listed is refused, and so is a value of another " +
		"type; a widget names only the fields its type reads. `[]` is each element of a list " +
		"and `{}` each value of an object keyed by name. list_dashboard_sources lists the " +
		"engine sources.\n\n## Every key of " + EntryPath + "\n\n")
	b.WriteString(Form.Outline(map[string]string{
		"v":                 fmt.Sprintf("required, always %d", FormatVersion),
		"datasets[].source": jsonform.OneOf(SourceKinds),
		"widgets[].type":    jsonform.OneOf(WidgetTypes),
		"widgets[].agg":     jsonform.OneOf(Aggs),
		"widgets[].format":  jsonform.OneOf(Formats),
		"widgets[].tone":    jsonform.OneOf(Tones),
	}))
	b.WriteString("\n## Closed sets\n\n")
	fmt.Fprintf(&b, "- A calc step's `op`: %s.\n", jsonform.OneOf(Ops))
	fmt.Fprintf(&b, "- A comparison: %s.\n", jsonform.OneOf(Comparisons))
	fmt.Fprintf(&b, "- A compute function: %s.\n", jsonform.OneOf(ComputeFns))
	fmt.Fprintf(&b, "- A column's type: %s.\n", jsonform.OneOf(ColumnTypes))
	fmt.Fprintf(&b, "- A file dataset's file: %s.\n", jsonform.OneOf(FileEssences))
	return b.String()
}
