package prompts

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// rounds is more loops than any phase's cap, so a prompt that drifted per
// round would have drifted several times over by the last one.
const rounds = 9

// stableAcrossRounds requires a phase's system prompt to be byte-identical on
// every round while the per-round state moves (package doc, "Byte-stability
// across rounds"): a drift shows up only on the bill. An absence passes when
// the test is broken too, so two guards: the prompt must be substantial, and
// `sensitive` (the builder with ONE input changed) must differ, proving the
// builder reads its inputs.
func stableAcrossRounds(t *testing.T, name string, build func(round int) string, sensitive string) {
	t.Helper()
	first := build(1)
	if n := len(first); n < 500 {
		t.Fatalf("%s: prompt is %d bytes — too small to be a real prompt, so "+
			"equality across rounds proves nothing", name, n)
	}
	if sensitive == first {
		t.Fatalf("%s: a changed input produced identical bytes — this builder "+
			"is ignoring its inputs, so the stability check below is vacuous", name)
	}
	for round := 2; round <= rounds; round++ {
		got := build(round)
		if got == first {
			continue
		}
		t.Errorf("%s: system prompt changed on round %d — the provider's prefix "+
			"cache misses every remaining round\n%s", name, round, firstDiff(first, got))
	}
}

// firstDiff points at the byte where two prompts part company; printing two
// multi-thousand-character prompts would bury the finding.
func firstDiff(a, b string) string {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			lo := max(0, i-60)
			return fmt.Sprintf("  first diff at byte %d\n  round 1: %q\n  later:   %q",
				i, a[lo:min(len(a), i+60)], b[lo:min(len(b), i+60)])
		}
	}
	return fmt.Sprintf("  identical for %d bytes, then lengths differ: %d vs %d", n, len(a), len(b))
}

// A turn sent back launches another run, often from another process after a
// park; none of that reaches the system prompt: the prefetch blocks are frozen
// at turn start and the ledger rides the brief.
func TestExecutorPromptIsByteStableAcrossRounds(t *testing.T) {
	t.Parallel()
	seat := lead()
	frozen := ExecutorInput{
		// comment_on_work_item and the code tools, so the escalation and
		// code guidance they gate are inside the bytes held stable.
		AvailableTools: append([]string{"post_message", "refresh_memory", "comment_on_work_item"},
			slices.Concat(CodeHistoryTools(), CodeSearchTools(), CodeWriteTools(), CodeMergeTools())...),
		ThreadContext:     "- **Ana Ruiz (ana)**: staging redirects in a loop",
		PersonalMemory:    "- prefers short replies",
		RelevantKnowledge: "- **Runbook**: steps",
	}
	// What a round actually changes, and where it goes: the user message.
	var userMessages []string
	stableAcrossRounds(t, "executor", func(round int) string {
		userMessages = append(userMessages, BuildPhaseUserMessage(UserMessage{
			TaskDescription: "post the summary",
			PriorWork:       ledgerAfter(round),
		}).Text)
		return BuildExecutor(seat, frozen).Text
	}, BuildExecutor(seat, ExecutorInput{AvailableTools: frozen.AvailableTools}).Text)

	// Guard the guard: unmoved per-round state tests nothing.
	if userMessages[0] == userMessages[len(userMessages)-1] {
		t.Fatal("the per-round state never changed — this test would pass on a " +
			"prompt that drifted every round")
	}
}

func ledgerAfter(round int) string {
	var b strings.Builder
	for i := 1; i < round; i++ {
		fmt.Fprintf(&b, "- round %d: post_message(...) → success\n", i)
	}
	return b.String()
}

// Review's evidence is fixed for every tool round of the phase, yet DIFFERS
// between turn rounds (whole executor/reviewer passes), since the evidence
// does.
func TestReviewPromptIsByteStableAcrossRounds(t *testing.T) {
	t.Parallel()
	seat := lead()
	frozen := ReviewInput{
		Intent:   "Post the summary to #eng.",
		Outcome:  "delivered",
		Produced: "Posted the summary.",
		ToolLog:  "- post_message(...) → success",
		// The gated rule ON and delivered refs set, so their bytes are held
		// stable too.
		Delivered: DeliveredRefs([]string{"acme/api#41"}, 0),
		OffersA2A: true,
		Skills:    allSkills(),
	}
	stableAcrossRounds(t, "review",
		func(int) string { return BuildReview(seat, frozen).Text },
		BuildReview(seat, ReviewInput{Intent: "Post the summary to #eng."}).Text)

	// Not drift: a second pass carries the ledger the first lacked.
	second := frozen
	second.EarlierIterations = "### Iteration 1\nCalled:\n- post_message(...) → success"
	if BuildReview(seat, second).Text == BuildReview(seat, frozen).Text {
		t.Error("the earlier-rounds ledger did not reach the review prompt")
	}
}

// THE WHOLE TURN is byte-stable, the claim that bills: a per-prompt assertion
// misses a section moving from the executor's prompt into the reviewer's.
func TestTheWholeTurnIsByteStableAcrossRounds(t *testing.T) {
	t.Parallel()
	seat := lead()
	exec := ExecutorInput{AvailableTools: []string{"post_message"}}
	review := ReviewInput{Intent: "Post.", Outcome: "delivered", Skills: allSkills()}
	stableAcrossRounds(t, "turn",
		func(int) string { return BuildExecutor(seat, exec).Text + BuildReview(seat, review).Text },
		BuildExecutor(seat, ExecutorInput{}).Text+BuildReview(seat, ReviewInput{}).Text)
}

// Assembly is deterministic for the same inputs: a catalogue may answer in
// map order, and a leaked order would reorder two builds in one process.
func TestAssemblyIsDeterministic(t *testing.T) {
	t.Parallel()
	// A wide MCP surface, out of order, so a leaked order shows.
	seat := seatIn(acme(), "eng")
	worker := []Phase{PhaseSubagent}
	cat := &fakeCatalogue{skills: []fakeSkill{
		{key: "mcp:slack", mcpServer: "slack", summary: "S", phases: worker},
		{key: "mcp:github", mcpServer: "github", summary: "G", phases: worker},
		{key: "mcp:atlassian", mcpServer: "atlassian", summary: "A", phases: worker},
		{key: "mcp:mattermost", mcpServer: "mattermost", summary: "M", phases: worker},
	}}
	in := SubagentInput{ParentSystemPrompt: "P", Skills: cat, MCPServers: []string{
		"slack", "custom-b", "github", "mattermost", "gitlab", "custom-a", "atlassian",
	}}

	const builds = 200
	first := BuildSubagent(seat, in).Text
	for i := 2; i <= builds; i++ {
		if got := BuildSubagent(seat, in).Text; got != first {
			t.Fatalf("build %d of %d differs from the first\n%s", i, builds, firstDiff(first, got))
		}
	}
	// Guard: the catalogue rendered, so the builds had something to differ on.
	contains(t, first, "- `mcp:atlassian` — A", "- `mcp:slack` — S")
	order(t, first, "mcp:atlassian", "mcp:github", "mcp:mattermost", "mcp:slack")
}

// A system prompt is a pure function of turn-start inputs, and a round
// counter among them would break that without failing any other assertion.
// "Iteration" is NOT banned: ReviewInput.EarlierIterations is per TURN round,
// allowed to move between passes.
func TestNoPhaseInputCarriesARoundCounter(t *testing.T) {
	t.Parallel()
	banned := []string{"round", "attempt", "retry"}
	for _, in := range []any{ExecutorInput{}, ReviewInput{}, OnboardingInput{}, SubagentInput{}} {
		typ := reflect.TypeOf(in)
		for i := range typ.NumField() {
			name := strings.ToLower(typ.Field(i).Name)
			for _, b := range banned {
				if strings.Contains(name, b) {
					t.Errorf("%s.%s looks like per-round state; a system prompt "+
						"that carries one misses the prefix cache every round",
						typ.Name(), typ.Field(i).Name)
				}
			}
		}
	}
}
