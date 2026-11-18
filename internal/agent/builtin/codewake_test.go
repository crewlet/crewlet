package builtin_test

import (
	"regexp"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/notify"
)

// A CODE WAKE NAMES ONLY CODE TOOLS THIS BUILD REGISTERS — and the merge
// gate's wakes offer the gate's own tools. gitlet's Prompt names the tools as
// literals (this package imports internal/gitlet, not the reverse), so this is
// the one place the two are held together: a drifted name tells a seat to call
// something that is not there.
//
// Mutations, each red: walk a broken landing through code_create_branch,
// code_revert and code_open_change in place of code_revert_change; tell a
// ready change's recipients to tell whoever merges it, or a dequeued change's
// authors to say when it may queue again, in place of offering code_merge_change.
func TestThePromptNamesOnlyRegisteredChangeTools(t *testing.T) {
	t.Parallel()
	registered := map[string]bool{}
	for _, name := range append(builtin.CodeToolNames(), builtin.CodeWrites()...) {
		registered[name] = true
	}
	tool := regexp.MustCompile("`(code_[a-z]+(?:_[a-z]+)*)`")
	named := map[string]bool{}
	byReason := map[gitlet.Reason]map[string]bool{}
	for _, reason := range gitlet.Reasons() {
		byReason[reason] = map[string]bool{}
		text := (gitlet.Prompt{}).Build(notify.Inbound{
			Source: gitlet.Source, EventType: string(gitlet.OpComment), Subject: "web!42: Retry the upload",
			Body: "Is the retry bounded?",
			Metadata: map[string]string{gitlet.MetaVia: string(reason), gitlet.MetaChangeRef: "web!42",
				gitlet.MetaChangeID: "c-1"},
		}, nil)
		for _, m := range tool.FindAllStringSubmatch(text, -1) {
			named[m[1]], byReason[reason][m[1]] = true, true
			if !registered[m[1]] {
				t.Errorf("a wake under %s names `%s`, which this build does not register:\n%s", reason, m[1], text)
			}
		}
	}
	// AND THE GUARD SEES SOMETHING: a pattern matching nothing passes every prompt.
	for _, want := range []string{builtin.CodeGetChangeTool, builtin.CodeReviewChangeTool,
		builtin.CodeCommentOnChangeTool, builtin.CodeResolveFindingTool} {
		if !named[want] {
			t.Errorf("no code wake names `%s`, so this guard is not reading what it guards", want)
		}
	}
	// AND THE GATE'S WAKES OFFER THE GATE'S TOOLS (D-QUEUE, M5-D25, M5-D26): a
	// broken landing the one-write revert, a change ready to merge or dequeued
	// the merge. Without one a seat rebuilds by hand what one tool does, or asks
	// somebody else to do what it may do itself.
	for reason, want := range map[gitlet.Reason]string{
		gitlet.ReasonTargetBroken: builtin.CodeRevertChangeTool,
		gitlet.ReasonReadyToMerge: builtin.CodeMergeChangeTool,
		gitlet.ReasonDequeued:     builtin.CodeMergeChangeTool,
	} {
		if !byReason[reason][want] {
			t.Errorf("a wake under %s does not offer `%s`", reason, want)
		}
	}
}
