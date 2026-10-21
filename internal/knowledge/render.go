package knowledge

import (
	"strings"

	"github.com/crewlet/crewlet/internal/provenance"
)

// Render is ranked hits as a model reads them: a bullet per page, its title
// and its snippet, all of them inside ONE fence of b (ADR-0040) labelled with
// the hits' provenance merged ([provenance.Merge]) — or "" when no hit has a
// title or a snippet to show.
//
// THE ONE RENDERER of a hit into a model's text, for the turn-start block and
// `search_knowledge` alike, so the two label the same page one way. It used
// to be two private copies, and neither fenced anything: a page's title and
// opening reached the seat's prompt as bare prose, so a page anybody could
// write addressed the seat in the voice of its instructions, and a snippet
// spelling a fence's closer stepped out of any fence the text sat beside.
//
// ONE FENCE FOR THE LIST rather than one per hit, as the thread block fences a
// thread: the hits of one search come from one backend under one label, and a
// fence per bullet costs a hundred bytes of markers six times over in a block
// re-sent on every round. The TITLES ARE INSIDE IT, because a title is written
// by whoever named the page and says what they chose; the engine's own pointer
// sentence stays outside, for the caller to add.
//
// A page with no snippet is its title alone, which is still enough to decide
// whether to open it; one with neither is left out.
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
