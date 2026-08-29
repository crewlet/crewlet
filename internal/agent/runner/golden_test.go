package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The golden-turn suite: whole turns driven through the REAL loop, the real
// runner and the real tool surfaces, with only the model scripted.
//
// It is the gate for the turn engine because none of the pieces below is
// individually wrong in any of these scenarios — each one is a
// misunderstanding BETWEEN two of them, which is exactly what a unit test of
// either cannot see.

func settings() turn.Settings {
	return turn.Settings{MaxIterations: 4, SkipNames: []string{"activate_tool"}}
}

func runTurn(t *testing.T, prov *scriptedProvider) (turn.Result, *scriptedProvider) {
	t.Helper()
	r, p, _ := fixture(t, prov)
	res, err := turn.Run(context.Background(), r, settings(), turn.Input{TurnID: "t-golden"})
	if err != nil {
		t.Fatalf("turn.Run: %v", err)
	}
	return res, p
}

func TestGoldenAPlannedTurnThatDeliversCompletes(t *testing.T) {
	t.Parallel()
	res, prov := runTurn(t, &scriptedProvider{
		plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool, `{
			"decision":"plan","reasoning":"post the summary",
			"tools_needed":["slack_post"],
			"steps":[{"intent":"post it","approach":"Weekly: three PRs merged."}],
			"success_criteria":["the post exists"]}`)},
		execute: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post",
				Arguments: map[string]any{"channel": "C0ENG", "text": "Weekly: three PRs merged."}}}},
			text("Posted the weekly summary to #eng."),
		},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool,
			`{"decision":"done","final_artifact":"Posted the weekly summary."}`)},
	})

	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if res.Artifact != "Posted the weekly summary." {
		t.Errorf("artifact = %q", res.Artifact)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d, want one", res.Rounds)
	}
	// Every phase ran exactly once.
	for _, which := range []string{"plan", "execute", "review"} {
		if n := len(prov.requestsFor(which)); n == 0 {
			t.Errorf("%s never ran", which)
		}
	}
	// The delivery reached the tool, which is the whole point.
	if !strings.Contains(prov.requestsFor("review")[0].Messages[0].Content, "slack_post") {
		t.Error("the reviewer was not shown the delivery")
	}
}

func TestGoldenAnUndeliveredDoneIsOverturnedAndTheSecondRoundDelivers(t *testing.T) {
	t.Parallel()
	// The end-to-end shape of the engine's most important override: Review
	// judged from the produced text and said done while nothing had sent
	// it. Only the loop, the gate and the surface TOGETHER can catch this —
	// the gate needs the surface's MCP classification, and the loop needs
	// the gate's correction to get a second round that actually posts.
	res, prov := runTurn(t, &scriptedProvider{
		plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool,
			`{"decision":"plan","tools_needed":["slack_post","slack_history"],"steps":[{"intent":"post"}]}`)},
		execute: []llm.Completion{
			// Round 1: reads through a tool the plan DID name, composes,
			// never posts. slack_history is positively annotated read-only,
			// so calling it is recon and not delivery.
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_history"}}},
			text("Here is the summary: three PRs merged."),
			// Round 2, after the correction: actually posts.
			{ToolCalls: []llm.ToolCall{{ID: "b", Name: "slack_post",
				Arguments: map[string]any{"channel": "C0ENG"}}}},
			text("Posted."),
		},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})

	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if res.Rounds != 2 {
		t.Fatalf("rounds = %d, want the override to have forced a second", res.Rounds)
	}
	// The engine's correction reached the second Plan round, naming the
	// tool that was never called.
	planRounds := prov.requestsFor("plan")
	if len(planRounds) < 2 {
		t.Fatalf("Plan ran %d times", len(planRounds))
	}
	second := planRounds[1].Messages[1].Content
	if !strings.Contains(second, "slack_post") {
		t.Errorf("the correction did not name the missing delivery tool:\n%s", second)
	}
	// And the ledger told the second round what round one already did, so
	// it adds rather than repeats.
	if !strings.Contains(second, "slack_history") {
		t.Errorf("the second round was not shown round one's calls:\n%s", second)
	}
}

func TestGoldenAKnownReadDoesNotCountAsDelivery(t *testing.T) {
	t.Parallel()
	// The classification comes from the registry's annotations, travels
	// through the runner's surface description, and is applied by the gate.
	// Three components; the property exists in none of them alone.
	res, _ := runTurn(t, &scriptedProvider{
		plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool,
			`{"decision":"plan","tools_needed":["slack_post","slack_history"],"steps":[{"intent":"post"}]}`)},
		execute: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_history"}}},
			text("read it, here is my answer"),
		},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	// Every round reads and never posts, so the turn exhausts rather than
	// completing — which is the correct failure. A silent "done" would be
	// the bug.
	if res.Decision == phase.Done {
		t.Fatal("a turn that only ever read completed as done")
	}
}

func TestGoldenADirectPlanThatDeliversSkipsReviewEntirely(t *testing.T) {
	t.Parallel()
	res, prov := runTurn(t, &scriptedProvider{
		plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool,
			`{"decision":"direct","tools_needed":["slack_post"],"reasoning":"one-liner"}`)},
		execute: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post"}}},
			text("Posted."),
		},
	})
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if n := len(prov.requestsFor("review")); n != 0 {
		t.Errorf("Review ran %d times on a delivered direct plan", n)
	}
	if res.Artifact != "Posted." {
		t.Errorf("artifact = %q", res.Artifact)
	}
}

func TestGoldenADirectPlanThatDeliversNothingIsReviewedAnyway(t *testing.T) {
	t.Parallel()
	// The safety net, end to end. Without it the turn completes as a silent
	// no-op: the seat appears to have answered and nothing reached Slack.
	res, prov := runTurn(t, &scriptedProvider{
		plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool,
			`{"decision":"direct","tools_needed":["slack_post"],"reasoning":"one-liner"}`)},
		execute: []llm.Completion{text("I would say: three PRs merged.")},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool,
			`{"decision":"failed","notes":"nothing was posted and I cannot post it"}`)},
	})
	if n := len(prov.requestsFor("review")); n == 0 {
		t.Fatal("Review was skipped on a direct plan that delivered nothing")
	}
	if res.Decision == phase.Done {
		t.Error("the turn completed as done having delivered nothing")
	}
}

func TestGoldenASkipEndsTheTurnWithoutTouchingAnything(t *testing.T) {
	t.Parallel()
	res, prov := runTurn(t, &scriptedProvider{
		plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool,
			`{"decision":"skip","reasoning":"this was addressed to the CTO"}`)},
	})
	if res.Decision != phase.Skipped {
		t.Fatalf("decision = %s", res.Decision)
	}
	if !strings.Contains(res.Artifact, "addressed to the CTO") {
		t.Errorf("artifact = %q, want the planner's reasoning", res.Artifact)
	}
	for _, which := range []string{"execute", "review"} {
		if n := len(prov.requestsFor(which)); n != 0 {
			t.Errorf("%s ran %d times on a skip", which, n)
		}
	}
}

func TestGoldenAStalledTurnFailsRatherThanSpinning(t *testing.T) {
	t.Parallel()
	// Every round produces the same artifact and the reviewer keeps asking
	// for another. The stall guard has to end it before the round budget
	// does, and say which guard fired.
	res, _ := runTurn(t, &scriptedProvider{
		plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool,
			`{"decision":"plan","tools_needed":["slack_post"],"steps":[{"intent":"post"}]}`)},
		execute: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post"}}},
			text("the same output every time"),
		},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool,
			`{"decision":"self_iterate","notes":"still not right"}`)},
	})
	if res.Decision != phase.Failed {
		t.Fatalf("decision = %s", res.Decision)
	}
	if res.Breach == nil || res.Breach.Kind != turn.BreachStall {
		t.Fatalf("breach = %+v, want a stall", res.Breach)
	}
	if res.Rounds >= settings().MaxIterations {
		t.Errorf("the stall guard took %d rounds; the budget alone would have "+
			"ended it at %d", res.Rounds, settings().MaxIterations)
	}
}

func TestGoldenTheLedgerCrossesRoundsWithItsBudgetsApplied(t *testing.T) {
	t.Parallel()
	// The ledger is built by the loop, rendered by the prompt builder and
	// read by the next Plan round. This is the only place all three meet.
	body := strings.Repeat("prose ", 400)
	res, prov := runTurn(t, &scriptedProvider{
		plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool,
			`{"decision":"plan","tools_needed":["slack_post"],"steps":[{"intent":"post"}]}`)},
		execute: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post",
				Arguments: map[string]any{"channel": "C0ENGINEERING", "text": body}}}},
			text("v1"),
			{ToolCalls: []llm.ToolCall{{ID: "b", Name: "slack_post",
				Arguments: map[string]any{"channel": "C0ENGINEERING"}}}},
			text("v2"),
		},
		review: []llm.Completion{
			submitCall(t, runner.SubmitReviewTool,
				`{"decision":"self_iterate","notes":"wrong link","completed_work":"the post landed"}`),
			submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`),
		},
	})
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if len(res.Iterations) != 1 {
		t.Fatalf("ledger = %d entries, want the one closed round", len(res.Iterations))
	}

	second := prov.requestsFor("plan")[1].Messages[1].Content
	// The discriminator survives; the payload does not.
	if !strings.Contains(second, "C0ENGINEERING") {
		t.Errorf("the ledger lost the channel that says WHICH delivery fired:\n%s", second)
	}
	if strings.Contains(second, body) {
		t.Error("the ledger carried the whole payload into the next round's prompt")
	}
	// The reviewer's own prose about what landed rides along, because the
	// mechanical log cannot express it.
	if !strings.Contains(second, "the post landed") {
		t.Errorf("the reviewer's gloss did not reach the next round:\n%s", second)
	}
	if !strings.Contains(second, "wrong link") {
		t.Errorf("the correction did not reach the next round:\n%s", second)
	}
	// The reviewer's last word survives a `done` round, which appends no
	// ledger entry — without it the conversation ledger loses it.
	if res.LastReview == nil {
		t.Error("the last review was not carried out of the turn")
	}
}

func TestGoldenAToolFailureIsOrdinaryAndTheTurnContinues(t *testing.T) {
	t.Parallel()
	// A failing tool is not an engine failure: its message goes back to the
	// model, which is expected to react. The turn must reach a decision.
	res, _ := runTurn(t, &scriptedProvider{
		plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool,
			`{"decision":"plan","tools_needed":["slack_post"],"steps":[{"intent":"post"}]}`)},
		execute: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "ghost_tool"}}},
			{ToolCalls: []llm.ToolCall{{ID: "b", Name: "slack_post"}}},
			text("recovered and posted"),
		},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
}

func TestGoldenAMalformedSubmissionIsRetriedInsideThePhase(t *testing.T) {
	t.Parallel()
	// Refusing the turn over a malformed submission throws away everything
	// the phase already did. The model gets told what was wrong and tries
	// again, inside the same phase.
	res, prov := runTurn(t, &scriptedProvider{
		plan: []llm.Completion{
			submitCall(t, runner.SubmitPlanTool, `{"decision":"wat"}`),
			submitCall(t, runner.SubmitPlanTool,
				`{"decision":"plan","tools_needed":["slack_post"],"steps":[{"intent":"post"}]}`),
		},
		execute: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post"}}},
			text("posted"),
		},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d — the retry should have stayed inside Plan", res.Rounds)
	}
	// The failure text reached the model as a tool result.
	planReq := prov.requestsFor("plan")
	if len(planReq) < 2 {
		t.Fatalf("Plan made %d model calls, want a retry", len(planReq))
	}
	var sawFailure bool
	for _, m := range planReq[1].Messages {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, "plan, direct or skip") {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Error("the model was not told why its submission was refused")
	}
}

func TestGoldenAPhantomPlanThatDiscoversTheRealToolStillDelivers(t *testing.T) {
	t.Parallel()
	// The planner guessed the MCP tool's name wrong. Execute is told which
	// name did not resolve, finds the real one, and uses it — and the gate
	// has to read that as a genuine delivery, or the turn loops re-posting.
	res, prov := runTurn(t, &scriptedProvider{
		plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool,
			`{"decision":"plan","tools_needed":["slack_send_msg"],"steps":[{"intent":"post"}]}`)},
		execute: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post"}}},
			text("found the real tool and posted"),
		},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v) — a discovered delivery was not "+
			"read as one", res.Decision, res.Breach)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d, want one", res.Rounds)
	}
	if !strings.Contains(prov.requestsFor("execute")[0].Messages[0].Content, "slack_send_msg") {
		t.Error("Execute was not told which planned name did not resolve")
	}
}

func TestGoldenEveryPhaseRunsOnItsOwnConfiguredModel(t *testing.T) {
	t.Parallel()
	// Per-phase resolution, the runner's chain construction and the loop
	// all have to agree, and a crossed wire is invisible: the seat still
	// works, on the wrong model.
	byPhase := map[string]*scriptedProvider{}
	entries := []phase.Entry{}
	for _, key := range []string{"planner", "executor", "reviewer"} {
		p := &scriptedProvider{}
		byPhase[key] = p
		entries = append(entries, phase.Entry{Key: key, Provider: p})
	}
	byPhase["planner"].plan = []llm.Completion{submitCall(t, runner.SubmitPlanTool,
		`{"decision":"plan","tools_needed":["slack_post"],"steps":[{"intent":"post"}]}`)}
	byPhase["executor"].execute = []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post"}}},
		text("posted"),
	}
	byPhase["reviewer"].review = []llm.Completion{
		submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)}

	r := runnerWithModels(t, entries)
	res, err := turn.Run(context.Background(), r, settings(), turn.Input{})
	if err != nil {
		t.Fatalf("turn.Run: %v", err)
	}
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	for key, want := range map[string]string{
		"planner": "plan", "executor": "execute", "reviewer": "review",
	} {
		got := byPhase[key].seen
		if len(got) == 0 {
			t.Errorf("%s was never called", key)
			continue
		}
		for _, req := range got {
			if which, _ := byPhase[key].scriptFor(req); which != want {
				t.Errorf("%s served a %s request", key, which)
			}
		}
	}
	// And no provider served a phase that is not its own.
	if n := len(byPhase["planner"].seen); n != len(byPhase["planner"].requestsFor("plan")) {
		t.Errorf("the planner served %d requests, %d of them Plan's", n,
			len(byPhase["planner"].requestsFor("plan")))
	}
}

func TestGoldenPhaseModelsAreNotSilentlyCrossed(t *testing.T) {
	t.Parallel()
	// The counterfactual for the test above: with only a default provider
	// configured, every phase lands on it. Without this, that test also
	// passes for a resolver that ignores the per-phase keys entirely.
	only := &scriptedProvider{
		plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool,
			`{"decision":"plan","tools_needed":["slack_post"],"steps":[{"intent":"post"}]}`)},
		execute: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post"}}}, text("posted")},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}
	r := runnerWithModels(t, []phase.Entry{{Key: "default", Provider: only}})
	if _, err := turn.Run(context.Background(), r, settings(), turn.Input{}); err != nil {
		t.Fatalf("turn.Run: %v", err)
	}
	seen := map[string]bool{}
	for _, req := range only.seen {
		which, _ := only.scriptFor(req)
		seen[which] = true
	}
	if !slices.Equal(sortedKeys(seen), []string{"execute", "plan", "review"}) {
		t.Errorf("one provider served %v, want all three phases", sortedKeys(seen))
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
