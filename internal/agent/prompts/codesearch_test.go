package prompts

import (
	"slices"
	"testing"
)

// THE SEARCH GUIDANCE NEEDS THE SEARCH: it renders, as its own section after
// the reading guidance, only for a seat holding code_search, so no seat without
// one is told to search before reading whole trees. And it promises no
// search of EVERY repository: an unfiltered search reads only the indexed ones,
// a `search: grep` one only where a repo: filter names it (the tool's
// description says how), so a seat told otherwise would read a grep
// repository's silence as an answer.
//
// Mutation: render it unconditionally, fold it into code_history (the outline
// holds no code_search part), or promise every repository, and it goes red.
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
