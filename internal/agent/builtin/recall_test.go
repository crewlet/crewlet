package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// recall_iteration exists to give the iteration ledger's budgets a bottom.
// Every case here names the cut it is the floor under, and each asserts the
// SAME round through both renderers — the prompt's block and the tool — so a
// budget that stopped cutting or a tool that started would both go red.

// recallTool is the tool as a registry actually hands it out.
func recallTool(t *testing.T) tools.Callable {
	t.Helper()
	return registered(t, builtin.Deps{}, builtin.RecallIterationTool)
}

// turnWithRounds is a seat mid-turn, carrying closed rounds.
func turnWithRounds(t *testing.T, rounds ...ledger.Iteration) *turnctx.Turn {
	t.Helper()
	return turnFor(t, "agent-ceo").WithRounds(rounds)
}

// block is what the model was shown in its prompt, for the same rounds.
func block(rounds ...ledger.Iteration) string {
	return ledger.RenderIterations(rounds, nil)
}

// A message body past ValueLimit is elided in the block and whole in the
// recall. THE CENTRAL CASE: budgets.go bounds an argument because the block is
// re-sent every round, and that is only defensible while the whole value is
// one call away. Before this tool a round-five executor could read that it had
// called post_message and had no way to learn what it had sent.
func TestRecallReturnsAnArgumentTheBlockElided(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("s", ledger.ValueLimit*3)
	round := ledger.Iteration{
		Iteration: 1,
		Intent:    "Answer the founder",
		Calls: []ledger.Call{{
			Name: "post_message",
			Args: map[string]any{"channel": "C0FOUNDERS", "text": body},
		}},
	}

	if rendered := block(round); strings.Contains(rendered, body) {
		t.Fatal("the prompt block carried the body whole; there would be " +
			"nothing for this tool to be the floor under")
	}
	got := callFor(t, recallTool(t), turnWithRounds(t, round), map[string]any{})
	if got.Failed {
		t.Fatalf("recall failed: %s", got.Output)
	}
	if !strings.Contains(got.Output, body) {
		t.Errorf("recall did not return the elided body; got:\n%s", got.Output)
	}
	// The discriminator is in BOTH, which is the division of labour: the
	// block says which delivery fired, the tool says what it said.
	if !strings.Contains(got.Output, "C0FOUNDERS") {
		t.Errorf("recall dropped the channel; got:\n%s", got.Output)
	}
}

// A round's produced text is tail-cut into the block at RenderedArtifactLimit
// and whole here. The record keeps it; only the render cuts.
func TestRecallReturnsProducedTextTheBlockTailCut(t *testing.T) {
	t.Parallel()
	head := "THE-OPENING-THOUGHT"
	round := ledger.Iteration{
		Iteration: 1,
		Text:      head + strings.Repeat("x", ledger.RenderedArtifactLimit+500),
	}

	if rendered := block(round); strings.Contains(rendered, head) {
		t.Fatal("the block kept the head of the produced text; this case " +
			"no longer covers the tail cut")
	}
	got := callFor(t, recallTool(t), turnWithRounds(t, round), map[string]any{})
	if !strings.Contains(got.Output, head) {
		t.Error("recall did not return the produced text whole")
	}
}

// Read calls past MaxReadCalls are dropped from the block and returned here.
// The block's "+N further read call(s) omitted" line said how many; this says
// which.
func TestRecallReturnsReadCallsTheBlockDropped(t *testing.T) {
	t.Parallel()
	var calls []ledger.Call
	for i := 0; i < ledger.MaxReadCalls+3; i++ {
		calls = append(calls, ledger.Call{Name: "get_page", Args: map[string]any{"id": itoa(i)}})
	}
	// The last one is the one the block has no room for.
	last := calls[len(calls)-1].Args["id"].(string)
	round := ledger.Iteration{Iteration: 1, Calls: calls, Reads: []string{"get_page"}}

	rendered := block(round)
	if !strings.Contains(rendered, "further read call(s) omitted") {
		t.Fatalf("the block dropped no reads; got:\n%s", rendered)
	}
	got := callFor(t, recallTool(t), turnWithRounds(t, round), map[string]any{})
	if strings.Count(got.Output, "get_page(") != len(calls) {
		t.Errorf("recall returned %d of %d read calls; got:\n%s",
			strings.Count(got.Output, "get_page("), len(calls), got.Output)
	}
	if !strings.Contains(got.Output, `"id":"`+last+`"`) {
		t.Errorf("recall dropped the read the block had no room for; got:\n%s", got.Output)
	}
}

// A FAILED call's error text is elided in the block and whole here, and that
// is the line the re-run rule does not cover: the call may succeed next time,
// or fail differently, so running it again is not a way to read what it said.
func TestRecallReturnsAFailedCallsErrorWhole(t *testing.T) {
	t.Parallel()
	detail := strings.Repeat("w", ledger.ValueLimit*2)
	round := ledger.Iteration{
		Iteration: 1,
		Calls:     []ledger.Call{{Name: "save_page", Result: detail, Failed: true}},
	}

	if strings.Contains(block(round), detail) {
		t.Fatal("the block carried the error whole; this case covers nothing")
	}
	got := callFor(t, recallTool(t), turnWithRounds(t, round), map[string]any{})
	if !strings.Contains(got.Output, detail) {
		t.Errorf("recall did not return the error whole; got:\n%s", got.Output)
	}
}

// A SUCCEEDED call's result is NOT returned, although the record holds it.
// internal/agent/ledger's own rule: tool results are deliberately not carried
// across rounds, because a read's answer can have moved since and replaying a
// stale copy is worse than re-reading it. The boundary is the whole reason
// this tool is defensible as a read of the record rather than a transcript.
func TestRecallWithholdsASucceededCallsResult(t *testing.T) {
	t.Parallel()
	round := ledger.Iteration{
		Iteration: 1,
		Calls: []ledger.Call{{
			Name: "get_page", Result: "THE-PAGE-BODY-NOBODY-SHOULD-REPLAY",
		}},
		Reads: []string{"get_page"},
	}
	got := callFor(t, recallTool(t), turnWithRounds(t, round), map[string]any{})
	if strings.Contains(got.Output, "THE-PAGE-BODY-NOBODY-SHOULD-REPLAY") {
		t.Errorf("recall replayed a succeeded read's result; got:\n%s", got.Output)
	}
	// And it says so, so the model's next move is the re-run rather than an
	// invented answer. The marker is checked ON THE CALL'S OWN LINE: the
	// footer explaining it also contains the word, so a whole-output match
	// would pass with the marker gone from every line it labels.
	if !strings.HasSuffix(callLine(t, got.Output, "get_page"), " (read)") {
		t.Errorf("the recalled call lost its (read) marker; got:\n%s", got.Output)
	}
	if !strings.Contains(got.Output, "safe to run again") {
		t.Errorf("recall did not point at the re-run; got:\n%s", got.Output)
	}
}

// callLine is the one rendered line for a named call, so an assertion about a
// line cannot be satisfied by prose elsewhere in the answer.
func callLine(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, name+"(") {
			return line
		}
	}
	t.Fatalf("no line for %s in:\n%s", name, out)
	return ""
}

// With no argument it answers the LAST closed round, which is the one a model
// asking "what did I just send?" means.
func TestRecallDefaultsToTheMostRecentClosedRound(t *testing.T) {
	t.Parallel()
	turn := turnWithRounds(t,
		ledger.Iteration{Iteration: 1, Intent: "FIRST"},
		ledger.Iteration{Iteration: 2, Intent: "SECOND"},
	)
	got := callFor(t, recallTool(t), turn, map[string]any{})
	if !strings.Contains(got.Output, "SECOND") || strings.Contains(got.Output, "FIRST") {
		t.Errorf("recall did not default to the last closed round; got:\n%s", got.Output)
	}
	// And an explicit number reaches the earlier one.
	got = callFor(t, recallTool(t), turn, map[string]any{"iteration": 1})
	if !strings.Contains(got.Output, "FIRST") || strings.Contains(got.Output, "SECOND") {
		t.Errorf("recall(iteration: 1) did not answer round one; got:\n%s", got.Output)
	}
}

// A RESUMED turn's history starts at the round it suspended on, so index N and
// round N are the same value only for a turn that never parked. The model
// reads these numbers off the `Iteration N` headings either way.
func TestRecallAddressesARoundByItsOwnNumber(t *testing.T) {
	t.Parallel()
	turn := turnWithRounds(t,
		ledger.Iteration{Iteration: 4, Intent: "AFTER-THE-SUSPEND"},
		ledger.Iteration{Iteration: 5, Intent: "THE-ONE-AFTER"},
	)
	got := callFor(t, recallTool(t), turn, map[string]any{"iteration": 4})
	if !strings.Contains(got.Output, "AFTER-THE-SUSPEND") {
		t.Errorf("recall(iteration: 4) missed the round numbered 4; got:\n%s", got.Output)
	}
	// Position 1 is round 4, so a positional reading would answer here.
	got = callFor(t, recallTool(t), turn, map[string]any{"iteration": 1})
	if !got.Failed {
		t.Errorf("recall(iteration: 1) answered a round this turn never closed:\n%s",
			got.Output)
	}
	if !strings.Contains(got.Output, "4, 5") {
		t.Errorf("the refusal did not name the rounds to retry with; got:\n%s", got.Output)
	}
}

// Round one is a real answer, not a failure: asking is reasonable and the
// model cannot fix it by calling differently.
func TestRecallOnTheFirstRoundAnswersRatherThanFails(t *testing.T) {
	t.Parallel()
	got := callFor(t, recallTool(t), turnFor(t, "agent-ceo"), map[string]any{})
	if got.Failed {
		t.Errorf("round one was reported as a failure: %s", got.Output)
	}
	if !strings.Contains(got.Output, "first round") {
		t.Errorf("round one was not explained; got:\n%s", got.Output)
	}
}

// A surface never bound to a turn is a WIRING fault, and reporting it as
// "no rounds yet" would hide it behind an ordinary answer.
func TestRecallOutsideATurnIsRefused(t *testing.T) {
	t.Parallel()
	tool := recallTool(t).(tools.SeatCallable)
	got, err := tool.CallForTurn(t.Context(), nil, map[string]any{})
	if err != nil {
		t.Fatalf("CallForTurn: %v", err)
	}
	if !got.Failed {
		t.Errorf("a surface with no turn answered: %s", got.Output)
	}
}

// Naming a tool narrows the answer to those calls AND drops the rest of the
// round, because narrowing is the whole point: a round's produced text is the
// phase's entire tool loop, thinking included.
func TestRecallNarrowedToOneToolOmitsTheRest(t *testing.T) {
	t.Parallel()
	round := ledger.Iteration{
		Iteration: 2,
		Intent:    "Post, then look something up",
		Calls: []ledger.Call{
			{Name: "post_message", Args: map[string]any{"text": "THE-POST"}},
			{Name: "get_page", Args: map[string]any{"id": "THE-PAGE"}},
		},
		Text:  "THE-WHOLE-TRANSCRIPT",
		Reads: []string{"get_page"},
	}
	got := callFor(t, recallTool(t), turnWithRounds(t, round),
		map[string]any{"tool": "post_message"})
	switch {
	case got.Failed:
		t.Fatalf("narrowed recall failed: %s", got.Output)
	case !strings.Contains(got.Output, "THE-POST"):
		t.Errorf("narrowed recall lost the call it was asked for; got:\n%s", got.Output)
	case strings.Contains(got.Output, "THE-PAGE"):
		t.Errorf("narrowed recall returned the other call; got:\n%s", got.Output)
	case strings.Contains(got.Output, "THE-WHOLE-TRANSCRIPT"):
		t.Errorf("narrowed recall returned the produced text it exists to skip; got:\n%s",
			got.Output)
	}

	// Narrowing to a READ is the likelier of the two ways in — a model does
	// it looking for what the call came back with — so the pointer at the
	// re-run has to be here too, not only on the whole-round answer.
	got = callFor(t, recallTool(t), turnWithRounds(t, round),
		map[string]any{"tool": "get_page"})
	if !strings.Contains(got.Output, "safe to run again") {
		t.Errorf("a narrowed recall of a read did not point at the re-run; got:\n%s",
			got.Output)
	}
}

// A tool the round never called is refused with what it DID call, so the
// model's retry is a name that works rather than another guess.
func TestRecallNarrowedToAnAbsentToolNamesWhatRan(t *testing.T) {
	t.Parallel()
	round := ledger.Iteration{
		Iteration: 1,
		Calls:     []ledger.Call{{Name: "post_message"}, {Name: "get_page"}},
	}
	got := callFor(t, recallTool(t), turnWithRounds(t, round),
		map[string]any{"tool": "create_work_item"})
	if !got.Failed {
		t.Fatalf("a call to a tool the round never made was answered: %s", got.Output)
	}
	if !strings.Contains(got.Output, "post_message") ||
		!strings.Contains(got.Output, "get_page") {
		t.Errorf("the refusal did not name what the round called; got:\n%s", got.Output)
	}
}

// Registered on every node, whatever else is wired: its corpus is the turn's
// own closed rounds. Unconditional also keeps the system+tools prefix stable
// across a turn — a tool appearing at round two would cost a prompt-cache miss
// on every turn that iterates.
func TestRecallIsRegisteredWithNoDependenciesAtAll(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	names, err := builtin.Register(reg, builtin.Deps{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	found := false
	for _, n := range names {
		found = found || n == builtin.RecallIterationTool
	}
	if !found {
		t.Errorf("a node with no dependencies registered %v, without %s",
			names, builtin.RecallIterationTool)
	}
}

// Annotated as a read. Unannotated counts as NOT a known read, so leaving it
// out would make every recall look like a delivery to the gate — a turn that
// only looked at its own record would report that it reached somebody.
func TestRecallIsAnnotatedAsAnIdempotentRead(t *testing.T) {
	t.Parallel()
	got := builtin.AnnotationsFor(builtin.RecallIterationTool)
	if got.ReadOnly != mcp.Yes || got.Idempotent != mcp.Yes {
		t.Errorf("recall_iteration is annotated %+v, want a read that is safe "+
			"to repeat", got)
	}
}
