package knowledge

import (
	"strings"

	"github.com/crewlet/crewlet/internal/provenance"
)

// Render is ranked hits as a model reads them: a bullet per page, title and
// snippet, inside ONE fence of b (ADR-0040) labelled with the hits' merged
// provenance ([provenance.Merge]), or "" when no hit has either; a page with
// no snippet is its title alone. THE ONE RENDERER of a hit into a model's text
// (turn-start block and `search_knowledge`): unfenced, a title and opening
// would read as the seat's instructions, and a snippet spelling a fence's
// closer would step out of any fence beside it.
//
// ONE FENCE FOR THE LIST, not per hit: the hits share a backend and a label,
// and a fence per bullet costs a hundred bytes each in a block re-sent every
// round. The TITLES ARE INSIDE IT, because whoever named the page wrote them;
// the engine's own pointer sentence stays outside.
func Render(b provenance.Boundary, hits []Hit) string {
	var (
		lines  []string
		labels []provenance.Label
	)
	for _, hit := range hits {
		line := bullet(hit)
		if line == "" {
			continue
		}
		lines = append(lines, line)
		labels = append(labels, hit.Provenance)
	}
	if len(lines) == 0 {
		return ""
	}
	label := provenance.Merge(labels...)
	return label.Fence(b, strings.Join(lines, "\n"))
}

// bullet is one hit as a line of the list.
func bullet(hit Hit) string {
	title := strings.Join(strings.Fields(hit.Title), " ")
	snippet := strings.Join(strings.Fields(hit.Snippet), " ")
	switch {
	case title == "" && snippet == "":
		return ""
	case title == "":
		title = "(untitled)"
	}
	if snippet == "" {
		return "- **" + title + "**"
	}
	return "- **" + title + "**: " + snippet
}
