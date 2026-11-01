package runner_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The golden-turn suite: whole turns driven through the REAL loop, the real
// runner and the real tool surfaces, with only the models scripted — the
// reviewer's replies, and what each executor run does over the bridge.
//
// It is the gate for the turn engine because none of the pieces below is
// individually wrong in any of these scenarios — each one is a
// misunderstanding BETWEEN two of them, which is exactly what a unit test of
// either cannot see.

func settings() turn.Settings {
	return turn.Settings{MaxIterations: 4, SkipNames: []string{"activate_tool"}}
}

// played is one executor run of a golden turn: what it was launched with and
// what the bridge recorded of the calls it made.
type played struct {
	req     runner.AgentRunRequest
	bridged []runner.BridgedCall
}

// results is every answer the run's calls were given, in order — what its
// CLI read back over the bridge.
func (p played) results() []string {
	out := make([]string, 0, len(p.bridged))
	for _, c := range p.bridged {
		out = append(out, c.Result)
	}
	return out
}

// goldenTurn runs a whole turn as the engine runs one. turn.Run drives a
// runner whose executor launches a run and parks the turn; the run plays its
// script against the surface it was launched with; a FRESH runner — as the
// process that collects it builds one — resumes from the parked state with
// what the bridge recorded, and the loop re-enters the parked round; and so
// on until the turn ends. The k-th launch plays runs[k], the last repeating.
func goldenTurn(t *testing.T, entries []phase.Entry, opts buildOpts, reply turn.Reply,
	runs ...run,
) (turn.Result, []played) {
	t.Helper()
	launcher := &playLauncher{}
	opts.launcher = launcher
	r, _ := buildWith(t, entries, opts)
	res, err := turn.Run(context.Background(), r, settings(),
		turn.Input{RunID: "t-golden", Reply: reply})
	var plays []played
	for err == nil && res.Suspended {
		req := launcher.last(t)
		script := runs[min(len(plays), len(runs)-1)]
		bridged := play(t, req, script)
		plays = append(plays, played{req: req, bridged: bridged})
		parked, ok := r.Suspended()
		if !ok {
			t.Fatal("the turn parked with no state to collect it from")
		}
		opts.resume = &runner.Resume{State: parked.State, Answer: script.text, Bridged: bridged}
		r, _ = buildWith(t, entries, opts)
		res, err = turn.Run(context.Background(), r, settings(), turn.Input{
			RunID: "t-golden", Reply: reply, Resume: true,
			Round: parked.State.Round, History: parked.State.Iterations,
		})
	}
	if err != nil {
		t.Fatalf("turn.Run: %v", err)
	}
	return res, plays
}

// runTurn is a golden turn somebody is waiting on, on the fixture's one
// provider: prov answers the reviewer, and runs are the executor's.
func runTurn(t *testing.T, prov *scriptedProvider, runs ...run) (turn.Result, []played) {
	t.Helper()
	return goldenTurn(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{},
		turn.ToolReply(""), runs...)
}

// sawResult reports whether any call of any run was answered with text
// carrying want — what its CLI read back over the bridge.
func sawResult(plays []played, want string) bool {
	for _, p := range plays {
		for _, out := range p.results() {
			if strings.Contains(out, want) {
				return true
			}
		}
	}
	return false
}

func TestGoldenATurnThatDeliversCompletes(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool,
			`{"decision":"done","final_artifact":"Posted the weekly summary."}`)},
	}
	res, plays := runTurn(t, prov, run{calls: []llm.ToolCall{
		activate("slack_post"),
		{ID: "a", Name: "slack_post",
			Arguments: map[string]any{"channel": "C0ENG", "text": "Weekly: three PRs merged."}},
		call(t, runner.SubmitWorkTool, `{
			"outcome":"delivered","summary":"posted the weekly summary to #eng",
			"deliveries":["slack_post"]}`),
	}, text: "Posted it."})

	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if res.Artifact != "Posted the weekly summary." {
		t.Errorf("artifact = %q", res.Artifact)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d, want one", res.Rounds)
	}
	// Both phases ran exactly once: one run, one review.
	if len(plays) != 1 {
		t.Errorf("%d runs were launched, want one", len(plays))
	}
	if n := len(prov.requestsFor("review")); n == 0 {
		t.Error("the reviewer never ran")
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
	res, plays := runTurn(t, &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}, run{calls: []llm.ToolCall{
		// The run read through a tool that is positively annotated
		// read-only, then claimed the read as the delivery.
		activate("slack_history"),
		{ID: "a", Name: "slack_history"},
		call(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"answered","deliveries":["slack_history"]}`),
		// Told what is citable, it discovers the real tool, posts, and
		// submits honestly.
		activate("slack_post"),
		{ID: "b", Name: "slack_post"},
		call(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"posted","deliveries":["slack_post"]}`),
	}})
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d — the correction should have stayed inside the round", res.Rounds)
	}
	// The refusal reached the run as the call's answer, listing what IS
	// citable rather than a bare no.
	if !sawResult(plays, "nothing has been delivered yet") {
		t.Error("the run was not told its delivery claim did not match the record")
	}
}

// The end-to-end shape of the engine's most important override: the reviewer
// judged from the produced text and said done while nothing had sent it. Only
// the loop, the check and the surface TOGETHER can catch this.
func TestGoldenAnUndeliveredDoneIsOverturnedAndTheSecondRoundDelivers(t *testing.T) {
	t.Parallel()
	res, plays := runTurn(t, &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	},
		// Round 1: reads, composes, reports honestly that it is blocked —
		// which passes the pre-review check and reaches a reviewer that
		// likes the prose.
		run{calls: []llm.ToolCall{
			activate("slack_history"),
			{ID: "a", Name: "slack_history"},
			call(t, runner.SubmitWorkTool, `{
				"outcome":"blocked","summary":"drafted the summary",
				"evidence":"I could not find the channel's post tool"}`),
		}},
		// Round 2, after the correction: discovers the tool and posts.
		run{calls: []llm.ToolCall{
			activate("slack_post"),
			{ID: "b", Name: "slack_post", Arguments: map[string]any{"channel": "C0ENG"}},
			call(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"posted","deliveries":["slack_post"]}`),
		}},
	)

	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if res.Rounds != 2 {
		t.Fatalf("rounds = %d, want the override to have forced a second", res.Rounds)
	}
	// The engine's correction reached the second round's brief, naming
	// what was missing.
	if len(plays) != 2 {
		t.Fatalf("%d runs were launched, want two", len(plays))
	}
	second := plays[1].req.Brief
	if !strings.Contains(second, "requester will never see it") {
		t.Fatalf("the override's correction never reached the second run:\n%s", second)
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
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}, run{calls: []llm.ToolCall{
		activate("slack_history"),
		{ID: "a", Name: "slack_history"},
		call(t, runner.SubmitWorkTool,
			`{"outcome":"blocked","summary":"read it","evidence":"no write tool"}`),
	}})
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
	prov := &scriptedProvider{}
	res, _ := goldenTurn(t, []phase.Entry{{Key: "default", Provider: prov}},
		buildOpts{reply: turn.NoReply()}, turn.NoReply(),
		run{calls: []llm.ToolCall{call(t, runner.SubmitWorkTool,
			`{"outcome":"no_action","summary":"this was addressed to the CTO"}`)}})
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
	res, plays := runTurn(t, &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}, run{calls: []llm.ToolCall{
		call(t, runner.SubmitWorkTool, `{"outcome":"no_action","summary":"not my area"}`),
		activate("slack_post"),
		{ID: "a", Name: "slack_post"},
		call(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"declined in the thread","deliveries":["slack_post"]}`),
	}})
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d — the refusal should have stayed inside the round", res.Rounds)
	}
	if !sawResult(plays, "even to decline") {
		t.Error("the run was not told that silence is not an option here")
	}
}

func TestGoldenAStalledTurnFailsRatherThanSpinning(t *testing.T) {
	t.Parallel()
	// Every round produces the same artifact and the reviewer keeps asking
	// for another. The stall guard has to end it before the round budget
	// does, and say which guard fired — ACROSS the parks every launch makes,
	// since each round is reviewed in a segment of its own.
	res, _ := runTurn(t, &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool,
			`{"decision":"self_iterate","notes":"still not right"}`)},
	}, run{calls: []llm.ToolCall{
		activate("slack_post"),
		{ID: "a", Name: "slack_post"},
		call(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"the same output every time","deliveries":["slack_post"]}`),
	}, text: "the same output every time"})
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
	// One reviewer per segment, as the engine has: each segment's runner is
	// a fresh one, so a shared script would replay its first reply.
	prov := &scriptedProvider{review: []llmtest.Response{
		submitCall(t, runner.SubmitReviewTool,
			`{"decision":"self_iterate","notes":"wrong link","completed_work":"the post landed"}`),
		submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`),
	}}
	res, plays := runTurn(t, prov,
		run{calls: []llm.ToolCall{
			activate("slack_post"),
			{ID: "a", Name: "slack_post",
				Arguments: map[string]any{"channel": "C0ENGINEERING", "text": body}},
			call(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"v1","deliveries":["slack_post"]}`),
		}, text: "v1"},
		run{calls: []llm.ToolCall{
			activate("slack_post"),
			{ID: "b", Name: "slack_post", Arguments: map[string]any{"channel": "C0ENGINEERING"}},
			call(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"v2","deliveries":["slack_post"]}`),
		}, text: "v2"},
	)
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if len(res.Iterations) != 1 {
		t.Fatalf("ledger = %d entries, want the one closed round", len(res.Iterations))
	}

	if len(plays) != 2 {
		t.Fatalf("%d runs were launched, want two", len(plays))
	}
	second := plays[1].req.Brief
	if !strings.Contains(second, "Already done earlier in this turn") {
		t.Fatalf("the prior-work ledger never reached the second run:\n%s", second)
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
	// A failing tool is not an engine failure: its answer goes back to the
	// run, which is expected to react. The turn must reach a decision.
	res, _ := runTurn(t, &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}, run{calls: []llm.ToolCall{
		{ID: "a", Name: "ghost_tool"},
		activate("slack_post"),
		{ID: "b", Name: "slack_post"},
		call(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"recovered and posted","deliveries":["slack_post"]}`),
	}})
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
}

func TestGoldenAMalformedSubmissionIsRetriedInsideThePhase(t *testing.T) {
	t.Parallel()
	// Refusing the turn over a malformed submission throws away everything
	// the run already did. The run is told what was wrong and tries again,
	// inside the same run.
	res, plays := runTurn(t, &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}, run{calls: []llm.ToolCall{
		activate("slack_post"),
		{ID: "a", Name: "slack_post"},
		call(t, runner.SubmitWorkTool, `{"outcome":"wat","summary":"s"}`),
		call(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"posted","deliveries":["slack_post"]}`),
	}})
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d — the retry should have stayed inside the run", res.Rounds)
	}
	// The failure text reached the run as the call's answer.
	if !sawResult(plays, "delivered, no_action or blocked") {
		t.Error("the run was not told why its submission was refused")
	}
}

// The executor never named its tools in advance, so there is nothing to
// reconcile: it discovers the real name and the check reads that as a genuine
// delivery, or the turn loops re-posting.
func TestGoldenADiscoveredToolIsAGenuineDelivery(t *testing.T) {
	t.Parallel()
	res, _ := runTurn(t, &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}, run{calls: []llm.ToolCall{
		{ID: "a", Name: "list_mcp_server_tools", Arguments: map[string]any{"server": "slack"}},
		{ID: "b", Name: "activate_tool", Arguments: map[string]any{"name": "slack_post"}},
		{ID: "c", Name: "slack_post"},
		call(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"found the real tool and posted",
			  "deliveries":["slack_post"]}`),
	}})
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
	// works, on the wrong model. The executor's run is its CLI's, so the
	// executor's entry is asked nothing here — the reviewer's entry, and
	// only it, serves the turn's model calls.
	byPhase := map[string]*scriptedProvider{}
	entries := []phase.Entry{}
	for _, key := range []string{"executor", "reviewer"} {
		p := &scriptedProvider{}
		byPhase[key] = p
		entries = append(entries, phase.Entry{Key: key, Provider: p})
	}
	byPhase["reviewer"].review = []llmtest.Response{
		submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)}

	res, _ := goldenTurn(t, entries, buildOpts{}, turn.ToolReply(""), deliver(t, "posted"))
	if res.Decision != phase.Done {
		t.Fatalf("decision = %s (breach %+v)", res.Decision, res.Breach)
	}
	got := byPhase["reviewer"].allRuns()
	if len(got) == 0 {
		t.Error("the reviewer's entry was never called")
	}
	for _, run := range got {
		if run.which != "review" {
			t.Errorf("the reviewer's entry served a %s request", run.which)
		}
	}
	if n := len(byPhase["executor"].allRuns()); n != 0 {
		t.Errorf("the executor's entry served %d engine runs; its run is the CLI's own", n)
	}
}

func TestGoldenPhaseModelsAreNotSilentlyCrossed(t *testing.T) {
	t.Parallel()
	// The counterfactual for the test above: with only a default provider
	// configured, the reviewer lands on it. Without this, that test also
	// passes for a resolver that ignores the per-phase keys entirely.
	only := &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}
	goldenTurn(t, []phase.Entry{{Key: "default", Provider: only}}, buildOpts{},
		turn.ToolReply(""), deliver(t, "posted"))
	if len(only.requestsFor("review")) == 0 {
		t.Error("the one configured provider served no review")
	}
}

// askingTool stands in for `comment_on_work_item`: a first-party tool that
// DELIVERS on Worklet, recording what it was called with. The tool's own
// write (the decision validated, the record published) is certified in
// `builtin` and `worklet`; what this suite owns is the turn around it.
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
// tool, with the reviewer scripted on prov and the executor's run as given.
func askingTurn(t *testing.T, prov *scriptedProvider, r run) (turn.Result, *askingTool, []played) {
	t.Helper()
	asker := &askingTool{}
	res, plays := goldenTurn(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		// Woken on the tracker, where the ask lands — so the gate is
		// the NARROW one, and only a call delivering there satisfies it.
		reply: turn.ToolReply(worklet.Source),
		task:  "decide which region the Q4 launch ships to first",
		register: func(t *testing.T, reg *tools.Registry) {
			if err := reg.RegisterWith(asker, tools.OriginBuiltin, tools.Annotations{},
				tools.DeliversTo(worklet.Source)); err != nil {
				t.Fatalf("Register: %v", err)
			}
		},
	}, turn.ToolReply(worklet.Source), r)
	return res, asker, plays
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
	askCall := llm.ToolCall{ID: "ask", Name: "comment_on_work_item",
		Arguments: map[string]any{
			"item": "LAUNCH-7", "body": "The region is yours to call.",
			"ask": "founder", "decision": decision,
		}}
	blocked := call(t, runner.SubmitWorkTool, `{
		"outcome":"blocked","summary":"asked the founder which region ships first",
		"evidence":"the region is the founder's call; the rollout plan waits on it"}`)
	done := submitCall(t, runner.SubmitReviewTool,
		`{"decision":"done","final_artifact":"Asked the founder to choose the launch region."}`)

	prov := &scriptedProvider{review: []llmtest.Response{done}}
	res, asker, plays := askingTurn(t, prov, run{calls: []llm.ToolCall{askCall, blocked}})

	// The seat was told how to ask, because it holds the tool that asks.
	first := plays[0].req.System
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
	silent, _, _ := askingTurn(t, &scriptedProvider{review: []llmtest.Response{done}},
		run{calls: []llm.ToolCall{blocked}})
	if silent.Decision == phase.Done && silent.Rounds == 1 {
		t.Error("a blocked round that asked nobody closed as done in one round")
	}
}
