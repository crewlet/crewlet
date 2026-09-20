package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/tools"
)

// A TOOL THAT NAMES A PROMPT BLOCK IS A SECOND COPY OF THAT BLOCK'S HEADING,
// and this repository's whole experience of a value written twice is that it
// drifts on the half nobody re-reads. These two descriptions each send the
// model to a section by name — `recall_iteration` to the prior-work ledger,
// `use_skill` to the synthesized-skill block — so a heading reworded in
// internal/agent/prompts and not here leaves the model hunting a section that
// no longer exists, silently, on the one path where it was told to look
// something up rather than guess.
//
// A guard rather than an import: the prompt constants are whole paragraphs of
// instruction and these tools quote six words of each, so making the
// description a composition would drag the prompt builder into every registry
// that holds a builtin. This is the arrangement the exported hint accessor and
// internal/clientsource both settle on for the same shape of problem — the
// copy stays, and something fails when the two disagree.
func TestAToolThatNamesAPromptBlockStillNamesOne(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tool    string
		quoted  string
		carrier string
	}{
		{
			tool:    builtin.RecallIterationTool,
			quoted:  "Already done earlier in this turn",
			carrier: prompts.PriorWorkHeader,
		},
		{
			tool: builtin.UseSkillTool,
			// The one this test was written for as much as for the
			// tool above: it has quoted this heading since it was
			// written, with nothing holding the two together.
			quoted:  "Synthesized skills you've learned",
			carrier: prompts.BuildExecutor(prompts.Seat{}, prompts.ExecutorInput{SynthesizedSkills: "x"}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()
			desc := describe(t, tc.tool)
			if !strings.Contains(desc, tc.quoted) {
				t.Fatalf("%s no longer quotes %q:\n%s", tc.tool, tc.quoted, desc)
			}
			if !strings.Contains(tc.carrier, tc.quoted) {
				t.Errorf("%s points the model at a %q block, and the prompt no "+
					"longer has one — reword both or neither", tc.tool, tc.quoted)
			}
		})
	}
}

// describe is one registered builtin's description, as a model is shown it.
func describe(t *testing.T, name string) string {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, fullDeps(t)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	entry, ok := reg.Snapshot().Lookup(name)
	if !ok {
		t.Fatalf("%s was not registered", name)
	}
	return entry.Tool.Description()
}
