package prompts

import (
	"slices"
	"testing"
)

// THE SEARCH GUIDANCE NEEDS THE SEARCH: it is rendered, as its own section
// after the reading guidance, for a seat that holds code_search and for no
// other, so no prompt tells a seat to search before reading whole trees when
// it has no search to make. And it promises no search of EVERY repository:
// an unfiltered search reads only the repositories that keep an index, a
// `search: grep` one only where a repo: filter names it (the tool's own
// description says how), so a seat told otherwise would read a grep
// repository's silence as an answer.
//
// Mutation: the gate dropped (rendered unconditionally) → red; the section
// folded into code_history → the outline holds no code_search part, red; the
// sentence promising every repository → red.
func TestTheCodeSearchGuidanceNeedsTheSearchTool(t *testing.T) {
	t.Parallel()
	const heading = "## Searching the company's code"
	held := slices.Concat([]string{"submit_work"}, CodeHistoryTools())
	excludes(t, BuildExecutor(engineer(), ExecutorInput{AvailableTools: held}).Text, heading, "`code_search`")
	p := BuildExecutor(engineer(), ExecutorInput{AvailableTools: slices.Concat(held, CodeSearchTools())})
	contains(t, p.Text, heading, "`code_search`", "search before reading whole trees")
	section, ok := slice(p, KeyCodeSearch)
	if !ok {
		t.Errorf("the guidance is not its own %q section of the outline", KeyCodeSearch)
	}
	excludes(t, section, "every repository")
}
