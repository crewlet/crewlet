package builtin_test

import (
	"regexp"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/notify"
)

// A CODE WAKE NAMES ONLY CODE TOOLS THIS BUILD REGISTERS.
//
// The code prompt (internal/code's Prompt) names the tools a woken seat
// answers with — they ship with this build, under names this build chose —
// and a seat acts on what it is shown, so a name that drifted from the
// registered one is a seat told to call something that is not there. The
// names are literals in the prompt because this package, which registers
// them, imports internal/code; this is the one place the two are held
// together.
func TestThePromptNamesOnlyRegisteredChangeTools(t *testing.T) {
	t.Parallel()
	registered := map[string]bool{}
	for _, name := range append(builtin.CodeToolNames(), builtin.CodeWrites()...) {
		registered[name] = true
	}
	tool := regexp.MustCompile("`(code_[a-z]+(?:_[a-z]+)*)`")
	named := map[string]bool{}
	for _, reason := range code.Reasons() {
		text := (code.Prompt{}).Build(notify.Inbound{
			Source: code.Source, EventType: string(code.OpComment), Subject: "web!42: Retry the upload",
			Body: "Is the retry bounded?",
			Metadata: map[string]string{code.MetaVia: string(reason), code.MetaChangeRef: "web!42",
				code.MetaChangeID: "c-1"},
		}, nil)
		for _, m := range tool.FindAllStringSubmatch(text, -1) {
			named[m[1]] = true
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
}
