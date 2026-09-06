package prompts

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// approxTokens is characters divided by four.
//
// It is an APPROXIMATION, not a tokenizer, and it is deliberately the same
// approximation the budgets below were set against. A real tokenizer would give
// different numbers, which would make every budget a value nobody could trace
// to the failure that set it — and it would tie a pure text package to a model
// vendor's vocabulary file.
//
// "Character" means one Unicode CODE POINT. These prompts are dense with em
// dashes and arrows; counting bytes instead would inflate every measurement by
// roughly 4% and quietly re-tighten every budget below.
func approxTokens(s string) int { return utf8.RuneCountInString(s) / 4 }

// The measure is part of the contract: someone "simplifying" it to len(s)/4
// would change every budget without touching a number.
func TestApproxTokensCountsCodePointsNotBytes(t *testing.T) {
	t.Parallel()
	const fourEmDashes = "————" // 4 code points, 12 bytes
	if got := approxTokens(fourEmDashes); got != 1 {
		t.Errorf("approxTokens(4 em dashes) = %d, want 1 (a byte count would give 3)", got)
	}
}

// withinBudget reports the measurement whether it passes or fails, so a
// change that spends the remaining headroom is visible in `go test -v`
// rather than only on the day it breaks.
func withinBudget(t *testing.T, name, prompt string, budget int) {
	t.Helper()
	got := approxTokens(prompt)
	if got >= budget {
		t.Errorf("%s prompt too large: ~%d tokens, budget %d", name, got, budget)
		return
	}
	t.Logf("%s prompt: ~%d tokens (budget %d, headroom %d)", name, got, budget, budget-got)
}

// Reference role: a lead, with a roster, three MCP servers, and a 100-tool
// catalogue.
//
// The budget covers the identity scaffold, the finish-the-arc rule, the
// stay-reachable rule, the verbose-decline rule, the submission contract, and
// the slim-catalogue + list_mcp_server_tools discovery flow. The ~50 tokens
// of discovery prose replaces the typical 1500-token MCP tool listing in a
// real workload — a net win as soon as one MCP server with more than five
// tools is wired.
//
// 2400 -> 2200 when the turn collapsed to one loop, which is DOWN: the
// executor's prompt is the old Plan prompt's identity scaffold plus the act
// contract Execute carried, minus everything the two-phase split needed — the
// tools_needed declaration, the phantom-tool warning, and the model-split
// hint. The frame that DECIDES is now the frame that acts, so nothing has to
// be described to a second conversation.
//
// Measured at ~1994 with a 100-tool catalogue, so this leaves ~10%: enough
// that a sentence can be added, tight enough that a section cannot.
func TestTheExecutorPromptStaysUnderBudgetWithABigCatalogue(t *testing.T) {
	t.Parallel()
	// 100 tools x ~60 chars = ~6k chars of catalogue = ~1500 tokens.
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = fmt.Sprintf("- tool_%d: Short description of tool number %d.", i, i)
	}
	p := BuildExecutor(lead(), ExecutorInput{ToolCatalogue: strings.Join(lines, "\n")})
	withinBudget(t, "executor", p, 2200)
}

// Review is identity + the decision enum + the settled-delivery note, the
// incomplete / sandbox / duplicate-delivery / missing-tool / blocked rules and
// the completed_work instruction.
//
// The original budget was 300 (identity + a six-line enum). The rules added
// ~290 tokens to prevent real production failures: the sandbox rule, which
// stops the reviewer looping a turn forever by misreading a
// run_sandbox-delegated investigation as fabrication; and the cross-round
// duplicate rule, which stops a second pass re-firing a side effect that
// already landed. Each maps to a turn-ending bug that was actually observed.
//
// Raised 560 -> 600 when the duplicate rule had to be keyed on target and
// content rather than tool name: keyed on the name alone it fired on the
// in-thread follow-up PriorWorkHeader explicitly asks for, so every corrected
// turn looped to max_iterations and terminated failed.
//
// 600 -> 750 for the two-stage turn: the tool-delivery rule is gone (the
// engine settles delivery before this prompt is built) and what replaced it
// is the note saying so plus the incomplete rule, which is what stops a
// reviewer grading an engine-written outcome as the agent's own verdict.
//
// The headroom is small on purpose: the next addition should have to justify
// itself here, not slip in.
func TestReviewPromptIsSmall(t *testing.T) {
	t.Parallel()
	withinBudget(t, "review", BuildReview(lead(), ReviewInput{}), 750)
}

// THE WHOLE TURN, which is the number that actually bills.
//
// Two prompts where there were three, and the executor's is the only one that
// carries the scaffold. A budget on each prompt separately cannot catch the
// regression that matters here — moving a section from one prompt to another
// leaves both under their own budgets while the turn costs the same.
//
// Measured at ~2712, so ~10% headroom, as above.
func TestTheWholeTurnStaysUnderBudget(t *testing.T) {
	t.Parallel()
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = fmt.Sprintf("- tool_%d: Short description of tool number %d.", i, i)
	}
	whole := BuildExecutor(lead(), ExecutorInput{ToolCatalogue: strings.Join(lines, "\n")}) +
		BuildReview(lead(), ReviewInput{})
	withinBudget(t, "turn", whole, 3000)
}
