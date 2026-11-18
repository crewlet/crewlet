package prompts

import (
	"slices"
	"testing"
)

// THE MERGING GUIDANCE NEEDS THE MERGE TOOL: it renders, as its own section,
// only for a seat holding code_merge_change, so no seat that can merge nothing
// is told how a merge waits.
//
// Mutation: the gate dropped (rendered unconditionally) → red.
func TestTheCodeMergingGuidanceNeedsTheMergeTool(t *testing.T) {
	t.Parallel()
	const heading = "## Merging a change"
	held := slices.Concat([]string{"submit_work"}, CodeHistoryTools(), CodeWriteTools())
	excludes(t, BuildExecutor(engineer(), ExecutorInput{AvailableTools: held}).Text, heading)
	p := BuildExecutor(engineer(), ExecutorInput{AvailableTools: slices.Concat(held, CodeMergeTools())})
	contains(t, p.Text, heading, "`code_merge_change`", "end the turn", "`ready_to_merge`")
	if _, ok := slice(p, KeyCodeMerging); !ok {
		t.Errorf("the guidance is not its own %q section of the outline", KeyCodeMerging)
	}
}
