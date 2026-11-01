package builtin_test

import (
	"regexp"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/notify"
)

// A CODE WAKE NAMES ONLY CODE TOOLS THIS BUILD REGISTERS — and the merge
// gate's wakes offer the gate's own tools.
//
// The code prompt (internal/code's Prompt) names the tools a woken seat
// answers with — they ship with this build, under names this build chose —
// and a seat acts on what it is shown, so a name that drifted from the
// registered one is a seat told to call something that is not there. The
// names are literals in the prompt because this package, which registers
// them, imports internal/code; this is the one place the two are held
// together.
//
// Mutations: walk a broken landing's seat through code_create_branch,
// code_revert and code_open_change in place of code_revert_change — the
// target_broken wake offers no revert tool, red; tell a ready change's
// recipients to say so to whoever merges it in place of offering
// code_merge_change — red; tell a dequeued change's authors to say when it is
// ready to queue again in place of offering code_merge_change — red.
func TestThePromptNamesOnlyRegisteredChangeTools(t *testing.T) {
	t.Parallel()
	registered := map[string]bool{}
	for _, name := range append(builtin.CodeToolNames(), builtin.CodeWrites()...) {
		registered[name] = true
	}
	tool := regexp.MustCompile("`(code_[a-z]+(?:_[a-z]+)*)`")
	named := map[string]bool{}
	byReason := map[code.Reason]map[string]bool{}
	for _, reason := range code.Reasons() {
		byReason[reason] = map[string]bool{}
		text := (code.Prompt{}).Build(notify.Inbound{
			Source: code.Source, EventType: string(code.OpComment), Subject: "web!42: Retry the upload",
			Body: "Is the retry bounded?",
			Metadata: map[string]string{code.MetaVia: string(reason), code.MetaChangeRef: "web!42",
				code.MetaChangeID: "c-1"},
		}, nil)
		for _, m := range tool.FindAllStringSubmatch(text, -1) {
			named[m[1]], byReason[reason][m[1]] = true, true
			if !registered[m[1]] {
				t.Errorf("a wake under %s names `%s`, which this build does not register:\n%s", reason, m[1], text)
			}
		}
	}
	// AND THE GUARD SEES SOMETHING: a pattern that matched nothing would pass
	// every prompt, renamed tools included.
	for _, want := range []string{builtin.CodeGetChangeTool, builtin.CodeReviewChangeTool,
		builtin.CodeCommentOnChangeTool, builtin.CodeResolveFindingTool} {
		if !named[want] {
			t.Errorf("no code wake names `%s`, so this guard is not reading what it guards", want)
		}
	}
	// AND THE GATE'S WAKES OFFER THE GATE'S TOOLS, each on the wake that is
	// about it (D-QUEUE, M5-D25, M5-D26): a broken landing the revert that
	// undoes the change in one write; a change ready to merge — a wake that
	// reaches whoever may merge it — the merge; and a change put out of its
	// queue the merge that queues it again. A wake that stops offering one
	// tells its seat to rebuild by hand what one tool does, or to tell
	// somebody else to do what it may do itself.
	for reason, want := range map[code.Reason]string{
		code.ReasonTargetBroken: builtin.CodeRevertChangeTool,
		code.ReasonReadyToMerge: builtin.CodeMergeChangeTool,
		code.ReasonDequeued:     builtin.CodeMergeChangeTool,
	} {
		if !byReason[reason][want] {
			t.Errorf("a wake under %s does not offer `%s`", reason, want)
		}
	}
}
