package prompts

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// rounds is more loops than any phase's cap, so a prompt that drifted per
// round would have drifted several times over by the last one.
const rounds = 9

// stableAcrossRounds is the byte-stability check: build the same phase's
// system prompt on every round of a loop whose per-round state is genuinely
// moving, and require identical bytes.
//
// Why it matters: the provider's prompt cache keys on the prefix. One changed
// byte between round 1 and round 9 costs the full uncached rate for every
// remaining round of every turn — and nothing reports it except the bill.
//
// A test asserting an ABSENCE (nothing drifted) passes both when the property
// holds and when the test is broken, so this carries two guards:
//
//   - the prompt must be substantial, so an empty or truncated build cannot
//     satisfy "all rounds equal";
//   - `sensitive` — the same builder with ONE input changed — must produce
//     DIFFERENT bytes, proving the builder reads its inputs at all. Without
//     it, a builder that ignored everything it was given would pass.
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

// The executor loops: it runs recon, results come back, it acts, it goes round
// again — and it additionally suspends and resumes around a detached sandbox
// run. None of that may reach the system prompt: the prefetch blocks are
// frozen at turn start, and the ledger of what has already run rides the user
// message.
func TestExecutorPromptIsByteStableAcrossRounds(t *testing.T) {
	t.Parallel()
	seat := lead()
	frozen := ExecutorInput{
		ToolCatalogue:     "- post_message: Post to a channel.",
		AvailableTools:    []string{"post_message", "mark_onboarded", "refresh_memory"},
		PersonalMemory:    "- prefers short replies",
		RelevantKnowledge: "- **Runbook**: steps",
		OnboardingHint:    "Read the onboarding pages.",
		Skills:            allSkills(),
	}
	// What a round actually changes, and where it goes: the user message.
	var userMessages []string
	stableAcrossRounds(t, "executor", func(round int) string {
		userMessages = append(userMessages, BuildPhaseUserMessage(UserMessage{
			TaskDescription: "post the summary",
			PriorWork:       ledgerAfter(round),
		}))
		return BuildExecutor(seat, frozen)
	}, BuildExecutor(seat, ExecutorInput{
		ToolCatalogue: "- post_message: Post to a channel.", AvailableTools: frozen.AvailableTools,
	}))

	// Guard the guard: if the per-round state never moved, the stability
	// assertion above tested nothing at all.
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

// Review's evidence is assembled once when the phase starts and is fixed for
// every round of it. It legitimately DIFFERS between round 1 and round 2 of a
// turn, because by then the evidence genuinely differs — a tool round is one
// model turn inside a phase, a turn round is a whole executor/reviewer pass.
func TestReviewPromptIsByteStableAcrossRounds(t *testing.T) {
	t.Parallel()
	seat := lead()
	frozen := ReviewInput{
		Intent:   "Post the summary to #eng.",
		Outcome:  "delivered",
		Produced: "Posted the summary.",
		ToolLog:  "- post_message(...) → success",
		Skills:   allSkills(),
	}
	stableAcrossRounds(t, "review",
		func(int) string { return BuildReview(seat, frozen) },
		BuildReview(seat, ReviewInput{Intent: "Post the summary to #eng."}))

	// And the cross-round case, stated so it is not mistaken for drift: a
	// second pass carries the ledger the first one did not have.
	second := frozen
	second.EarlierIterations = "### Iteration 1\nCalled:\n- post_message(...) → success"
	if BuildReview(seat, second) == BuildReview(seat, frozen) {
		t.Error("the earlier-rounds ledger did not reach the review prompt")
	}
}

// THE WHOLE TURN is byte-stable too, which is the claim that actually bills:
// a per-prompt assertion cannot catch a section moving from the executor's
// prompt into the reviewer's, and with two prompts instead of three there is
// one less place for a section to hide.
func TestTheWholeTurnIsByteStableAcrossRounds(t *testing.T) {
	t.Parallel()
	seat := lead()
	exec := ExecutorInput{
		ToolCatalogue:  "- post_message: Post to a channel.",
		AvailableTools: []string{"post_message"},
		Skills:         allSkills(),
	}
	review := ReviewInput{Intent: "Post.", Outcome: "delivered", Skills: allSkills()}
	stableAcrossRounds(t, "turn",
		func(int) string { return BuildExecutor(seat, exec) + BuildReview(seat, review) },
		BuildExecutor(seat, ExecutorInput{})+BuildReview(seat, ReviewInput{}))
}

// Assembly is deterministic for the same inputs, which is a stronger claim
// than "stable across rounds": the seat's MCP servers live in a map, and Go
// randomises map iteration on every range. Unsorted, that order would reach
// the skill catalogue and could reorder a prompt between two builds in the
// same process.
func TestAssemblyIsDeterministic(t *testing.T) {
	t.Parallel()
	// A wide MCP surface, so a map-order leak has plenty of room to show.
	o := acme()
	o.Role("Engineer").MCPEnv = map[string]map[string]string{
		"atlassian": {}, "github": {}, "gitlab": {},
		"slack": {}, "mattermost": {}, "custom-a": {}, "custom-b": {},
	}
	seat := seatIn(o, "Engineer")
	cat := &fakeCatalogue{skills: []fakeSkill{
		{key: "mcp:slack", mcpServer: "slack", summary: "S"},
		{key: "mcp:github", mcpServer: "github", summary: "G"},
		{key: "mcp:atlassian", mcpServer: "atlassian", summary: "A"},
		{key: "mcp:mattermost", mcpServer: "mattermost", summary: "M"},
	}}
	in := ExecutorInput{Skills: cat, ToolCatalogue: "- x: does x"}

	const builds = 200
	first := BuildExecutor(seat, in)
	for i := 2; i <= builds; i++ {
		if got := BuildExecutor(seat, in); got != first {
			t.Fatalf("build %d of %d differs from the first\n%s", i, builds, firstDiff(first, got))
		}
	}
	// Guard: the catalogue really did render, so the loop compared prompts
	// that had something to disagree about.
	contains(t, first, "- `mcp:atlassian` — A", "- `mcp:slack` — S")
	order(t, first, "mcp:atlassian", "mcp:github", "mcp:mattermost", "mcp:slack")
}

// A system prompt is a pure function of turn-start inputs. A round counter
// among those inputs is the one way that stops being true, and it would not
// fail any assertion here — the builder would simply start rendering it.
//
// "Iteration" is deliberately NOT on this list: ReviewInput.EarlierIterations
// is a per-TURN-ROUND input, which is a different thing from a per-tool-round
// one and is allowed to move between passes of a turn.
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
