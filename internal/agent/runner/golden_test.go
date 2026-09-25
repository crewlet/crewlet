package runner_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
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
	res, err := turn.Run(context.Background(), r, settings(),
		turn.Input{RunID: "t-golden", Reply: turn.ToolReply("")})
	if err != nil {
		t.Fatalf("turn.Run: %v", err)
	}
	return res, p
}

func TestGoldenATurnThatDeliversCompletes(t *testing.T) {
	t.Parallel()
	res, prov := runTurn(t, &scriptedProvider{
		execute: []llm.Completion{
			activate("slack_post"),
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post",
				Arguments: map[string]any{"channel": "C0ENG", "text": "Weekly: three PRs merged."}}}},
			submitCall(t, runner.SubmitWorkTool, `{
				"outcome":"delivered","summary":"posted the weekly summary to #eng",
				"deliveries":["slack_post"]}`),
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
	// Both phases ran exactly once.
	for _, which := range []string{"execute", "review"} {
		if n := len(prov.requestsFor(which)); n == 0 {
			t.Errorf("%s never ran", which)
		}
	}
	// The delivery reached the tool, which is the whole point.
	if !strings.Contains(prov.requestsFor("review")[0].Messages[0].Content, "slack_post") {
		t.Error("the reviewer was not shown the delivery")
	}
}

// A CLAIM THE RECORD REFUTES COSTS ONE BOUNCED TOOL CALL, not a round. The
// decoder, the surface's MCP classification and the tool loop all have to
// agree for this to happen inside the phase.
func TestGoldenAFalseDeliveryClaimIsCorrectedInsideTheRound(t *testing.T) {
	t.Parallel()
	res, prov := runTurn(t, &scriptedProvider{
		execute: []llm.Completion{
			// The model read through a tool that is positively annotated
			// read-only, then claimed the read as the delivery.
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_history"}}},
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"answered","deliveries":["slack_history"]}`),
			// Told what is citable, it discovers the real tool, posts,
			// and submits honestly.
			activate("slack_post"),
			{ToolCalls: []llm.ToolCall{{ID: "b", Name: "slack_post"}}},
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"posted","deliveries":["slack_post"]}`),
		},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d — the correction should have stayed inside the round", res.Rounds)
	}
	// The refusal reached the model as a tool result, listing what IS
	// citable rather than a bare no.
	var sawRefusal bool
	for _, req := range prov.requestsFor("execute") {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool && strings.Contains(m.Content, "nothing has been delivered yet") {
				sawRefusal = true
			}
		}
	}
	if !sawRefusal {
		t.Error("the model was not told its delivery claim did not match the record")
	}
}

// The end-to-end shape of the engine's most important override: the reviewer
// judged from the produced text and said done while nothing had sent it. Only
// the loop, the check and the surface TOGETHER can catch this.
func TestGoldenAnUndeliveredDoneIsOverturnedAndTheSecondRoundDelivers(t *testing.T) {
	t.Parallel()
	res, prov := runTurn(t, &scriptedProvider{
		execute: []llm.Completion{
			// Round 1: reads, composes, reports honestly that it is
			// blocked — which passes the pre-review check and reaches a
			// reviewer that likes the prose.
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_history"}}},
			submitCall(t, runner.SubmitWorkTool, `{
				"outcome":"blocked","summary":"drafted the summary",
				"evidence":"I could not find the channel's post tool"}`),
			// Round 2, after the correction: discovers the tool and posts.
			activate("slack_post"),
			{ToolCalls: []llm.ToolCall{{ID: "b", Name: "slack_post",
				Arguments: map[string]any{"channel": "C0ENG"}}}},
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"posted","deliveries":["slack_post"]}`),
		},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})

	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if res.Rounds != 2 {
		t.Fatalf("rounds = %d, want the override to have forced a second", res.Rounds)
	}
	// The engine's correction reached the second round, naming what was
	// missing.
	rounds := prov.requestsFor("execute")
	var second string
	for _, req := range rounds {
		if strings.Contains(req.Messages[1].Content, "requester will never see it") {
			second = req.Messages[1].Content
		}
	}
	if second == "" {
		t.Fatalf("the override's correction never reached a round")
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
	// through the runner's surface description, and is applied by the
	// check. Three components; the property exists in none of them alone.
	res, _ := runTurn(t, &scriptedProvider{
		execute: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_history"}}},
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"blocked","summary":"read it","evidence":"no write tool"}`),
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

// NOBODY ASKED. An unaddressed turn may end having done nothing, and it must
// not spend a review call to say so.
func TestGoldenNoActionOnAnUnaddressedTurnEndsItSilently(t *testing.T) {
	t.Parallel()
	r, prov := unaddressedFixture(t, &scriptedProvider{
		execute: []llm.Completion{submitCall(t, runner.SubmitWorkTool,
			`{"outcome":"no_action","summary":"this was addressed to the CTO"}`)},
	})
	res, err := turn.Run(context.Background(), r, settings(),
		turn.Input{RunID: "t-golden", Reply: turn.NoReply()})
	if err != nil {
		t.Fatalf("turn.Run: %v", err)
	}
	if res.Decision != phase.Skipped {
		t.Fatalf("decision = %s", res.Decision)
	}
	if !strings.Contains(res.Artifact, "addressed to the CTO") {
		t.Errorf("artifact = %q, want the executor's own reason", res.Artifact)
	}
	if n := len(prov.requestsFor("review")); n != 0 {
		t.Errorf("the reviewer ran %d times on a skip", n)
	}
}

// SILENCE IS NOT A DECLINE, and the refusal reaches the model inside the round
// where it can still act on it.
func TestGoldenNoActionOnAnAwaitedTurnIsRefusedInsideTheRound(t *testing.T) {
	t.Parallel()
	res, prov := runTurn(t, &scriptedProvider{
		execute: []llm.Completion{
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"no_action","summary":"not my area"}`),
			activate("slack_post"),
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post"}}},
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"declined in the thread","deliveries":["slack_post"]}`),
		},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d — the refusal should have stayed inside the round", res.Rounds)
	}
	var sawRefusal bool
	for _, req := range prov.requestsFor("execute") {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool && strings.Contains(m.Content, "even to decline") {
				sawRefusal = true
			}
		}
	}
	if !sawRefusal {
		t.Error("the model was not told that silence is not an option here")
	}
}

func TestGoldenAStalledTurnFailsRatherThanSpinning(t *testing.T) {
	t.Parallel()
	// Every round produces the same artifact and the reviewer keeps asking
	// for another. The stall guard has to end it before the round budget
	// does, and say which guard fired.
	res, _ := runTurn(t, &scriptedProvider{
		execute: []llm.Completion{
			activate("slack_post"),
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post"}}},
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"the same output every time","deliveries":["slack_post"]}`),
		},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool,
			`{"decision":"self_iterate","notes":"still not right"}`)},
	})
	if res.Decision != phase.Failed {
		t.Fatalf("decision = %s", res.Decision)
	}
	if res.Breach == nil || res.Breach.Kind != types.GuardStall {
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
	// read by the next executor round. This is the only place all three
	// meet.
	body := strings.Repeat("prose ", 400)
	res, prov := runTurn(t, &scriptedProvider{
		execute: []llm.Completion{
			activate("slack_post"),
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post",
				Arguments: map[string]any{"channel": "C0ENGINEERING", "text": body}}}},
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"v1","deliveries":["slack_post"]}`),
			activate("slack_post"),
			{ToolCalls: []llm.ToolCall{{ID: "b", Name: "slack_post",
				Arguments: map[string]any{"channel": "C0ENGINEERING"}}}},
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"v2","deliveries":["slack_post"]}`),
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

	var second string
	for _, req := range prov.requestsFor("execute") {
		if strings.Contains(req.Messages[1].Content, "Already done earlier in this turn") {
			second = req.Messages[1].Content
			break
		}
	}
	if second == "" {
		t.Fatal("the prior-work ledger never reached a round")
	}
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
	// The last round survives a `done`, which appends no ledger entry —
	// without these the conversation ledger loses both.
	if res.LastReview == nil || res.LastWork == nil {
		t.Error("the last round was not carried out of the turn")
	}
}

func TestGoldenAToolFailureIsOrdinaryAndTheTurnContinues(t *testing.T) {
	t.Parallel()
	// A failing tool is not an engine failure: its message goes back to the
	// model, which is expected to react. The turn must reach a decision.
	res, _ := runTurn(t, &scriptedProvider{
		execute: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "ghost_tool"}}},
			activate("slack_post"),
			{ToolCalls: []llm.ToolCall{{ID: "b", Name: "slack_post"}}},
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"recovered and posted","deliveries":["slack_post"]}`),
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
		execute: []llm.Completion{
			activate("slack_post"),
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post"}}},
			submitCall(t, runner.SubmitWorkTool, `{"outcome":"wat","summary":"s"}`),
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"posted","deliveries":["slack_post"]}`),
		},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d — the retry should have stayed inside the phase", res.Rounds)
	}
	// The failure text reached the model as a tool result.
	var sawFailure bool
	for _, req := range prov.requestsFor("execute") {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool && strings.Contains(m.Content, "delivered, no_action or blocked") {
				sawFailure = true
			}
		}
	}
	if !sawFailure {
		t.Error("the model was not told why its submission was refused")
	}
}

// The executor never named its tools in advance, so there is nothing to
// reconcile: it discovers the real name and the check reads that as a genuine
// delivery, or the turn loops re-posting.
func TestGoldenADiscoveredToolIsAGenuineDelivery(t *testing.T) {
	t.Parallel()
	res, _ := runTurn(t, &scriptedProvider{
		execute: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "list_mcp_server_tools",
				Arguments: map[string]any{"server": "slack"}}}},
			{ToolCalls: []llm.ToolCall{{ID: "b", Name: "activate_tool",
				Arguments: map[string]any{"name": "slack_post"}}}},
			{ToolCalls: []llm.ToolCall{{ID: "c", Name: "slack_post"}}},
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"found the real tool and posted",
				  "deliveries":["slack_post"]}`),
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
}

func TestGoldenEveryPhaseRunsOnItsOwnConfiguredModel(t *testing.T) {
	t.Parallel()
	// Per-phase resolution, the runner's chain construction and the loop
	// all have to agree, and a crossed wire is invisible: the seat still
	// works, on the wrong model.
	byPhase := map[string]*scriptedProvider{}
	entries := []phase.Entry{}
	for _, key := range []string{"executor", "reviewer"} {
		p := &scriptedProvider{}
		byPhase[key] = p
		entries = append(entries, phase.Entry{Key: key, Provider: p})
	}
	byPhase["executor"].execute = deliver(t, "posted")
	byPhase["reviewer"].review = []llm.Completion{
		submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)}

	r := runnerWithModels(t, entries)
	res, err := turn.Run(context.Background(), r, settings(), turn.Input{Reply: turn.ToolReply("")})
	if err != nil {
		t.Fatalf("turn.Run: %v", err)
	}
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	for key, want := range map[string]string{"executor": "execute", "reviewer": "review"} {
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
}

func TestGoldenPhaseModelsAreNotSilentlyCrossed(t *testing.T) {
	t.Parallel()
	// The counterfactual for the test above: with only a default provider
	// configured, every phase lands on it. Without this, that test also
	// passes for a resolver that ignores the per-phase keys entirely.
	only := &scriptedProvider{
		execute: deliver(t, "posted"),
		review:  []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}
	r := runnerWithModels(t, []phase.Entry{{Key: "default", Provider: only}})
	if _, err := turn.Run(context.Background(), r, settings(),
		turn.Input{Reply: turn.ToolReply("")}); err != nil {
		t.Fatalf("turn.Run: %v", err)
	}
	seen := map[string]bool{}
	for _, req := range only.seen {
		which, _ := only.scriptFor(req)
		seen[which] = true
	}
	if !slices.Equal(sortedKeys(seen), []string{"execute", "review"}) {
		t.Errorf("one provider served %v, want both phases", sortedKeys(seen))
	}
}

func sortedKeys(m map[string]bool) []string {
	out := slices.Sorted(maps.Keys(m))
	return out
}

// askingTool stands in for `comment_on_work_item`: a first-party tool that
// DELIVERS on the tracker, recording what it was called with. The tool's own
// write — the decision validated, the record published — is certified in
// `builtin` and `tracker`; what this suite owns is the turn around it.
type askingTool struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (a *askingTool) Name() string               { return "comment_on_work_item" }
func (a *askingTool) Description() string        { return "Comment on a work item, or ask on it." }
func (a *askingTool) Parameters() map[string]any { return nil }
func (a *askingTool) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, args)
	return tools.Result{Output: "commented (id c-9)"}, nil
}

func (a *askingTool) recorded() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

// askingTurn runs one tracker-woken turn for a seat that holds the asking
// tool, with the executor and reviewer scripted as given.
func askingTurn(t *testing.T, prov *scriptedProvider) (turn.Result, *askingTool) {
	t.Helper()
	asker := &askingTool{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		// Woken on the tracker, where the ask lands — so the gate is
		// the NARROW one, and only a call delivering there satisfies it.
		reply: turn.ToolReply(tracker.Source),
		task:  "decide which region the Q4 launch ships to first",
		register: func(t *testing.T, reg *tools.Registry) {
			if err := reg.RegisterWith(asker, tools.OriginBuiltin, tools.Annotations{},
				tools.DeliversTo(tracker.Source)); err != nil {
				t.Fatalf("Register: %v", err)
			}
		},
	})
	res, err := turn.Run(context.Background(), r, settings(),
		turn.Input{RunID: "t-golden", Reply: turn.ToolReply(tracker.Source)})
	if err != nil {
		t.Fatalf("turn.Run: %v", err)
	}
	return res, asker
}

// A DECISION ABOVE THE SEAT ENDS THE TURN BLOCKED ON THAT BRANCH, AND THAT IS
// A FINISHED TURN. Four components have to agree for it: the prompt builder
// tells a seat holding the asking tool to ask with options and stop, the
// surface carries the decision to the tool unchanged, the pre-review check
// reads a blocked round that asked as having reached somebody, and the
// done-override accepts the ask as the delivery on the tracker. Any one of
// them reading "blocked" as "gave up" sends the seat back round after round to
// make the choice it was told was not its own.
func TestGoldenAnAskWithADecisionEndsTheTurnBlockedOnThatBranch(t *testing.T) {
	t.Parallel()
	decision := map[string]any{
		"question": "Which region ships first?",
		"role":     "approver",
		"options": []any{
			map[string]any{"id": "eu", "label": "EU first", "detail": "GDPR review gates the date"},
			map[string]any{"id": "us", "label": "US first", "detail": "ships two weeks sooner"},
		},
		"recommended": "us",
		"rationale":   "the EU review has no date yet",
		"evidence":    []any{map[string]any{"kind": "task", "ref": "LAUNCH-12"}},
	}
	askCall := llm.Completion{ToolCalls: []llm.ToolCall{{ID: "ask", Name: "comment_on_work_item",
		Arguments: map[string]any{
			"item": "LAUNCH-7", "body": "The region is yours to call.",
			"ask": "founder", "decision": decision,
		}}}}
	blocked := submitCall(t, runner.SubmitWorkTool, `{
		"outcome":"blocked","summary":"asked the founder which region ships first",
		"evidence":"the region is the founder's call; the rollout plan waits on it"}`)
	done := submitCall(t, runner.SubmitReviewTool,
		`{"decision":"done","final_artifact":"Asked the founder to choose the launch region."}`)

	prov := &scriptedProvider{execute: []llm.Completion{askCall, blocked}, review: []llm.Completion{done}}
	res, asker := askingTurn(t, prov)

	// The seat was told how to ask, because it holds the tool that asks.
	first := prov.requestsFor("execute")[0].Messages[0].Content
	for _, want := range []string{
		"## When a decision is not yours to make",
		"END THIS TURN BLOCKED ON THAT BRANCH",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the executor's prompt lacks %q", want)
		}
	}

	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v) — a turn that asked and stopped was "+
			"not accepted as finished", res.Decision, res.Breach)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d — the seat was sent back to make a choice that is "+
			"not its own", res.Rounds)
	}
	calls := asker.recorded()
	if len(calls) != 1 {
		t.Fatalf("the asking tool ran %d times, want exactly the one ask", len(calls))
	}
	got, _ := calls[0]["decision"].(map[string]any)
	if got["recommended"] != "us" || len(got["options"].([]any)) != 2 || calls[0]["ask"] != "founder" {
		t.Errorf("the decision did not reach the tool intact: %v", calls[0])
	}
	// The reviewer judged the round with the ask in front of it.
	if !strings.Contains(prov.requestsFor("review")[0].Messages[0].Content, "comment_on_work_item") {
		t.Error("the reviewer was not shown the ask")
	}

	// THE COUNTERFACTUAL: the same blocked submission and the same reviewer,
	// with NO ask. Nobody was reached, so `blocked` is a round that gave up —
	// and it must not close as a finished turn. Without this the assertion
	// above also holds for a loop that accepts every `blocked` as done.
	silent, _ := askingTurn(t, &scriptedProvider{
		execute: []llm.Completion{blocked},
		review:  []llm.Completion{done},
	})
	if silent.Decision == phase.Done && silent.Rounds == 1 {
		t.Error("a blocked round that asked nobody closed as done in one round")
	}
}
