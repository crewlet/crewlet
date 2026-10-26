package runner_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/tools"
)

// scriptedProvider replays completions PER PHASE, choosing the script from the
// tools the request offers: the phases the engine drives with its own loop,
// which the executor is not — its run is played by a [playLauncher].
//
// Not a flat sequence: each phase runs its own loop and a phase that calls a
// tool takes another round, so a shared index drifts as soon as any phase
// makes one more call than the fixture author counted. Dispatching on the
// surface is both robust and closer to the truth — the offered tools ARE what
// distinguishes the phases.
type scriptedProvider struct {
	review, onboarding []llm.Completion
	seen               []llm.Request
	n                  map[string]int
}

func (p *scriptedProvider) Model() string { return "scripted" }

func (p *scriptedProvider) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	p.seen = append(p.seen, req)
	which, script := p.scriptFor(req)
	if p.n == nil {
		p.n = map[string]int{}
	}
	if len(script) == 0 {
		return &llm.Completion{Content: "(no script)"}, nil
	}
	i := min(p.n[which], len(script)-1)
	p.n[which]++
	c := script[i]
	return &c, nil
}

func (p *scriptedProvider) scriptFor(req llm.Request) (string, []llm.Completion) {
	offered := toolNames(req.Tools)
	switch {
	case slices.Contains(offered, runner.SubmitReviewTool):
		return "review", p.review
	case slices.Contains(offered, runner.MarkOnboardedTool):
		return "onboarding", p.onboarding
	default:
		// Nothing else calls a model through the engine's loop: the
		// executor is its CLI's own run. A request landing here is a phase
		// this fixture does not know, and it is answered with nothing a
		// phase could mistake for a decision.
		return "other", nil
	}
}

// requestsFor returns the requests one phase received, in order.
func (p *scriptedProvider) requestsFor(which string) []llm.Request {
	var out []llm.Request
	for _, req := range p.seen {
		if got, _ := p.scriptFor(req); got == which {
			out = append(out, req)
		}
	}
	return out
}

// submitCall builds a completion whose only content is a call to a submission
// tool with the given JSON arguments.
func submitCall(t *testing.T, name, argsJSON string) llm.Completion {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return llm.Completion{
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: name, Arguments: args}},
	}
}

// submitWork is the ordinary ending for a run that delivered nothing: the
// executor's starting surface carries no MCP tool, so a run that has not
// discovered one cannot honestly cite a delivery.
func submitWork(t *testing.T) llm.ToolCall {
	t.Helper()
	return call(t, runner.SubmitWorkTool,
		`{"outcome":"blocked","summary":"nothing to do here","evidence":"no write tool yet"}`)
}

// activate is the discovery half of a delivery. MCP TOOLS ARE NOT ON THE
// STARTING SURFACE — that is the whole point of discovery — so a run that
// posts has to promote the tool first, exactly as a real one does over the
// bridge.
func activate(name string) llm.ToolCall {
	return llm.ToolCall{ID: "act", Name: "activate_tool", Arguments: map[string]any{"name": name}}
}

// activating is [activate] as a round of an engine-driven phase.
func activating(name string) llm.Completion {
	return llm.Completion{ToolCalls: []llm.ToolCall{activate(name)}}
}

// deliver is the ordinary delivery a run makes: discover, call, report.
func deliver(t *testing.T, summary string) run {
	t.Helper()
	return run{calls: []llm.ToolCall{
		activate("slack_post"),
		{ID: "post", Name: "slack_post"},
		call(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"`+summary+`","deliveries":["slack_post"]}`),
	}, text: summary}
}

func text(s string) llm.Completion { return llm.Completion{Content: s} }

// workFor is a plain delivered submission, for tests that drive Review
// directly and do not care what the executor did.
func workFor(summary string) turn.Work {
	return turn.Work{Outcome: turn.OutcomeDelivered, Summary: summary}
}

type stubTool struct {
	name string
	out  string
	desc string
}

func (s stubTool) Name() string { return s.name }
func (s stubTool) Description() string {
	if s.desc != "" {
		return s.desc
	}
	return s.name + " does a thing"
}
func (s stubTool) Parameters() map[string]any { return nil }
func (s stubTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: s.out}, nil
}

// fixture is the shared runner on one provider, for a turn somebody is
// waiting on.
func fixture(t *testing.T, prov *scriptedProvider) (*runner.Runner, *scriptedProvider, *tools.Registry) {
	t.Helper()
	r, reg := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{})
	return r, prov, reg
}

// buildOpts are the knobs a test varies on the shared fixture. Zero is a turn
// somebody is waiting on, whose executor's launches start nothing.
type buildOpts struct {
	reply turn.Reply
	// launcher is the executor's launch seam. Nil is a [playLauncher] of
	// the fixture's own, which records the launch and starts nothing.
	launcher runner.AgentLauncher
	resume   *runner.Resume
	pub      queue.Publisher
	// reviewChain overrides the reviewer's provider keys (`llm_review`), so
	// a test can give the phase a real fallback chain rather than the
	// single key the fixture's seat runs it on.
	reviewChain org.ProviderKeys
	// task overrides the brief, which is the one piece of the prompt a test
	// can put arbitrary text into — so a case about how the prompt is
	// MEASURED can make it carry multi-byte runes.
	task string

	// context is the turn's frozen prefetch, and conversation its
	// prior-turns block, for the cases about what reaches a prompt rather
	// than what a phase decides. A collection is handed neither, as the
	// engine's is: its runner takes them from the park ([collect]).
	context      prefetch.Blocks
	conversation string
	// onPhase is the working indicator's seam. See [runner.Config.OnPhase].
	onPhase func(phase.Phase)
	// register adds tools beyond the fixture's standard catalogue, for a
	// case whose behaviour depends on a first-party tool the seat holds.
	register func(t *testing.T, reg *tools.Registry)
	// steer is the turn's note box, for the cases about a person's note.
	steer *steer.Box
	// subagent arms delegate on the executor's surface.
	subagent *runner.SubagentConfig

	// calls, when set, is the run's call log, on a turn context the
	// surfaces are bound to — for the cases about what a run has called.
	calls *turnctx.CallLog

	// budget is the turn's meter, for the cases about what a refusal stops.
	budget toolloop.BudgetMeter

	// compact is the seat's compactor, for the cases about what a rewrite
	// the prompt's assembly asks for costs.
	compact compact.Bound

	// fence is the seat fence, for the cases about what a closed one stops.
	fence func() error
}

func buildWith(t *testing.T, entries []phase.Entry, opts buildOpts) (*runner.Runner, *tools.Registry) {
	t.Helper()
	waiting := opts.reply
	if waiting.Kind == turn.ReplyUnset {
		waiting = turn.ToolReply("")
	}
	reg := tools.NewRegistry()
	for _, tl := range []stubTool{{name: "lookup_colleague"}, {name: "reflect"}} {
		if err := reg.Register(tl, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	if err := reg.RegisterWith(stubTool{name: "slack_post", out: "posted"},
		tools.Origin("slack"), tools.Annotations{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// A MULTI-LINE description, because a real MCP server publishes
	// paragraphs and a listing is a list: an entry that spilled onto a
	// second line would break the one-tool-per-line shape a model reads it
	// as.
	if err := reg.RegisterWith(stubTool{
		name: "slack_history", out: "history",
		desc: "Read a channel's history.\n\nAccepts a cursor for paging, and\nreturns up to 200 messages.",
	}, tools.Origin("slack"), tools.Annotations{ReadOnly: mcp.Yes}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// A SECOND server, so "list one server's tools" is distinguishable from
	// "list every MCP tool". With one server the filter is unobservable.
	if err := reg.RegisterWith(stubTool{name: "jira_create", out: "created"},
		tools.Origin("jira"), tools.Annotations{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if opts.register != nil {
		opts.register(t, reg)
	}

	models, err := phase.NewRegistry(entries)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	// The reviewer names its own key when one is configured; the executor
	// runs on `llm`, which is what makes it the seat's model rather than a
	// second spelling of one. With only a default present both miss and
	// resolution falls back to it, which is the counterfactual the golden
	// suite asserts.
	role.LLM = org.ProviderKeys{"executor"}
	role.LLMReview = org.ProviderKeys{"reviewer"}
	if len(opts.reviewChain) > 0 {
		role.LLMReview = opts.reviewChain
	}
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{role}}

	task := opts.task
	if task == "" {
		task = "post the weekly summary"
	}

	runTurn := runner.Turn{RunID: "t-1", WorkKey: "wk-1", AgentID: "a-1"}
	if opts.calls != nil {
		runTurn.Context = &turnctx.Turn{RunID: "t-1", WorkKey: "wk-1", Calls: opts.calls}
	}
	launcher := opts.launcher
	if launcher == nil {
		launcher = &playLauncher{}
	}
	r, err := runner.New(runner.Config{
		Seat:         prompts.Seat{Org: organization, Role: role},
		Registry:     reg,
		Models:       models,
		Context:      opts.context,
		Conversation: opts.conversation,
		Task:         task,
		Reply:        waiting,
		Launcher:     launcher,
		Resume:       opts.resume,
		Fence:        opts.fence,
		Publisher:    opts.pub,
		OnPhase:      opts.onPhase,
		Steer:        opts.steer,
		Subagent:     opts.subagent,
		Turn:         runTurn,
		Budget:       opts.budget,
		Compact:      opts.compact,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r, reg
}

func TestTheExecutorReturnsWhatTheModelSubmitted(t *testing.T) {
	t.Parallel()
	got := executeRound(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{}, 1, "", nil, run{calls: []llm.ToolCall{
			activate("slack_post"),
			{ID: "a", Name: "slack_post", Arguments: map[string]any{"channel": "C1"}},
			call(t, runner.SubmitWorkTool, `{
				"outcome":"delivered","summary":"posted the weekly summary",
				"deliveries":["slack_post"],"open_questions":"which channel next week?"}`),
		}, text: "posted it"}, runner.RunRecord{})

	w, surface := got.work, got.surface
	if w.Outcome != turn.OutcomeDelivered {
		t.Errorf("outcome = %s", w.Outcome)
	}
	if w.Summary != "posted the weekly summary" {
		t.Errorf("summary = %q", w.Summary)
	}
	if !slices.Equal(w.Deliveries, []string{"slack_post"}) {
		t.Errorf("deliveries = %v", w.Deliveries)
	}
	if w.OpenQuestions == "" {
		t.Error("the executor's open questions were dropped")
	}
	if w.Rescued {
		t.Error("a submitted outcome was marked as the engine's own")
	}
	// What the run CALLED, arguments and all: the delivery check, the
	// citations and the ledger read it.
	if len(w.Calls) != 3 || w.Calls[1].Name != "slack_post" || w.Calls[1].Args["channel"] != "C1" {
		t.Errorf("calls = %+v, want the run's own three", w.Calls)
	}
	// The surface handed back is what the delivery check judges against, so
	// it must describe the whole catalogue and not just what was offered.
	// And WHERE each one lands, which is what the gate compares against the
	// surface the ask arrived on.
	if surface.Deliveries["slack_post"] != "slack" {
		t.Errorf("surface deliveries = %v, want slack_post on its own server",
			surface.Deliveries)
	}
	if !slices.Equal(surface.KnownReads, []string{"slack_history"}) {
		t.Errorf("surface known reads = %v", surface.KnownReads)
	}
}

// EVERY FIRST-PARTY TOOL, and the MCP surface behind discovery. Choosing in
// advance is what the planner used to do — against a catalogue it was never
// shown — and every wrong guess became a tool the actor did not have when it
// turned out to need it.
func TestTheExecutorGetsTheWholeFirstPartySurfaceAndDiscovery(t *testing.T) {
	t.Parallel()
	launcher := &playLauncher{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{launcher: launcher})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	req := launcher.last(t)
	offered := toolNames(req.Surface.ToolDefs())
	slices.Sort(offered)
	want := []string{
		"activate_tool", "list_mcp_server_tools", "lookup_colleague", "reflect",
		runner.SubmitWorkTool,
	}
	slices.Sort(want)
	if !slices.Equal(offered, want) {
		t.Errorf("offered %v, want %v", offered, want)
	}
	// A real server publishes dozens and a model shown all of them acts
	// against a wall of text. Discovery is a tool call, which also keeps the
	// prompt prefix stable while a server's catalogue changes underneath.
	if slices.Contains(offered, "slack_post") {
		t.Errorf("an MCP tool was offered directly: %v", offered)
	}
	// But the CATALOGUE names the server, or discovery has nothing to aim
	// at.
	if !strings.Contains(req.System, "slack") {
		t.Error("the prompt does not name the MCP server")
	}
}

// isFinishingCorrective reports whether a message is the tool loop's re-prompt
// for a round that ended without the phase's submission.
func isFinishingCorrective(m llm.Message, tool string) bool {
	return m.Role == llm.RoleUser && strings.Contains(m.Content, "so this phase has not finished") &&
		strings.Contains(m.Content, "`"+tool+"`")
}

// A DELIVERY AN EARLIER ROUND MADE IS CITABLE. The prior-work ledger tells a
// round sent back for a re-read not to comment again, so a check reading only
// the round refused the one citation that was true — eleven times, in the
// measured turn, until the round cap ended it.
func TestAnEarlierRoundsDeliveryIsCitableInALaterOne(t *testing.T) {
	t.Parallel()
	tracker := func(t *testing.T, reg *tools.Registry) {
		t.Helper()
		if err := reg.RegisterWith(stubTool{name: "comment_on_work_item", out: "commented"},
			tools.OriginBuiltin, tools.Annotations{}, tools.DeliversTo("work")); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	cite := run{calls: []llm.ToolCall{call(t, runner.SubmitWorkTool,
		`{"outcome":"delivered","summary":"re-read the item","deliveries":["comment_on_work_item"]}`)}}
	earlier := []ledger.Iteration{{Iteration: 1,
		Calls: []ledger.Call{{Name: "comment_on_work_item", Result: "commented"}}}}
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	opts := buildOpts{reply: turn.ToolReply("work"), register: tracker}

	w := executeRound(t, entries, opts, 2, "read the item first", earlier, cite, runner.RunRecord{}).work
	if w.Rescued || w.Outcome != turn.OutcomeDelivered {
		t.Errorf("outcome = %s (rescued %v), want the citation of round one's comment "+
			"accepted", w.Outcome, w.Rescued)
	}

	// The counterfactual: with no such round behind it, the same citation
	// is refused when the run makes it, and the phase ends in the rescue.
	w = executeRound(t, entries, opts, 1, "", nil, cite, runner.RunRecord{}).work
	if !w.Rescued {
		t.Errorf("outcome = %s, want a citation of a comment nobody made refused", w.Outcome)
	}
}

func TestTheReviewersCorrectionReachesTheNextRoundWithoutRewritingTheAsk(t *testing.T) {
	t.Parallel()
	// The task text also feeds knowledge search and the episode record,
	// both of which want the requester's actual ask and not the engine's
	// running commentary on it. So the correction is prefixed to the BRIEF,
	// not merged into the task.
	launcher := &playLauncher{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{launcher: launcher})
	if _, _, err := r.Execute(context.Background(), 2, "the link was wrong", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	brief := launcher.last(t).Brief
	if !strings.Contains(brief, "the link was wrong") {
		t.Errorf("the correction did not reach the executor:\n%s", brief)
	}
	if !strings.Contains(brief, "post the weekly summary") {
		t.Errorf("the original ask was lost:\n%s", brief)
	}
	// And with no correction the brief is byte-identical to its
	// pre-correction form, which is what keeps the prompt prefix cacheable.
	plain := &playLauncher{}
	r2, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{launcher: plain})
	if _, _, err := r2.Execute(context.Background(), 1, "   ", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := plain.last(t).Brief; strings.Contains(got, "reviewed") {
		t.Errorf("a blank correction left scaffolding behind:\n%s", got)
	}
}

// mark_onboarded is the whole phase-scoped list, and it earns its place:
// onboarding is its own pass, and a seat that could mark itself from inside
// the executor would permanently skip orientation. Registered here, so the
// case is about the narrowing and not about a registry that never held it.
func TestTheExecutorIsNotOfferedTheOnboardingMarker(t *testing.T) {
	t.Parallel()
	launcher := &playLauncher{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{launcher: launcher, register: func(t *testing.T, reg *tools.Registry) {
			if err := reg.Register(stubTool{name: runner.MarkOnboardedTool}, tools.OriginBuiltin); err != nil {
				t.Fatalf("Register: %v", err)
			}
		}})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if slices.Contains(toolNames(launcher.last(t).Surface.ToolDefs()), runner.MarkOnboardedTool) {
		t.Error("the executor's run was offered mark_onboarded")
	}
}

func TestReviewJudgesAgainstTheToolLogVerbatim(t *testing.T) {
	t.Parallel()
	// The header points at the log as the primary evidence. A reviewer
	// judging an ELIDED log is judging a summary and calling it evidence —
	// the budgets belong to the cross-round ledger, not here.
	body := strings.Repeat("x", 3000)
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	w := turn.Work{
		Outcome: turn.OutcomeDelivered, Summary: "post it", Text: "posted",
		Calls: []ledger.Call{{Name: "slack_post", Args: map[string]any{"text": body}}},
	}
	if _, err := r.Review(context.Background(), 1, w, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	system := prov.requestsFor("review")[0].Messages[0].Content
	if !strings.Contains(system, body) {
		t.Error("the review's evidence log was elided")
	}
	if !strings.Contains(system, "post it") {
		t.Error("the executor's own account did not reach the reviewer")
	}
}

// An empty log renders as "(none)" rather than being omitted: a missing
// heading reads as "log unavailable", not "no calls were made".
func TestAnEmptyToolLogStillLeavesATrace(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	if _, err := r.Review(context.Background(), 1, turn.Work{Summary: "s"}, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !strings.Contains(prov.requestsFor("review")[0].Messages[0].Content, "(none)") {
		t.Error("an empty tool log left no trace")
	}
}

func TestAReviewerThatNeverDecidedDoesNotSilentlyPassTheTurn(t *testing.T) {
	t.Parallel()
	// Defaulting to done here is the difference between "the work was
	// judged good" and "nothing judged it", and those look identical
	// downstream.
	r, _, _ := fixture(t, &scriptedProvider{review: []llm.Completion{text("looks fine to me")}})
	got, err := r.Review(context.Background(), 1, turn.Work{}, nil)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got.Decision != phase.SelfIterate {
		t.Errorf("decision = %s, want self_iterate", got.Decision)
	}
	if !strings.Contains(got.Notes, runner.SubmitReviewTool) {
		t.Errorf("the correction does not say what to do: %q", got.Notes)
	}
}

// THE REVIEWER IS NEVER FORCED, its corrective round included — a request has
// no way to force a call. Its only tool is its submission, so a forced choice
// once looked free — but Claude Opus 5.5, Sonnet 5.5, Fable 5.1 and Mythos 5.1
// answer one with a 400, which the chain does not retry, so on those models
// every review died before the corrective that actually enforces the call
// could run. The submission it names is what re-asks a reviewer that answered
// in prose.
func TestTheReviewerAsksOnAutoAndIsReAskedForItsSubmission(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{review: []llm.Completion{
		text("Looks done to me."),
		submitCall(t, runner.SubmitReviewTool, `{"decision":"done","final_artifact":"a"}`),
	}})
	got, err := r.Review(context.Background(), 1, turn.Work{Summary: "s"}, nil)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got.Decision != phase.Done {
		t.Fatalf("decision = %s, want the submitted done", got.Decision)
	}
	reqs := prov.requestsFor("review")
	if len(reqs) != 2 {
		t.Fatalf("the reviewer was asked %d times, want 2", len(reqs))
	}
	if last := reqs[1].Messages[len(reqs[1].Messages)-1]; !isFinishingCorrective(last, runner.SubmitReviewTool) {
		t.Errorf("the second review round opened on %q, want the finishing corrective", last.Content)
	}
}

func TestEachPhaseGetsItsOwnSubmissionToolAndNoneLeaks(t *testing.T) {
	t.Parallel()
	// The submission tool is per-phase state. Registering it into the
	// shared registry would leave one phase's answer visible to the next —
	// or to the next turn, still holding the last one's decision.
	prov := &scriptedProvider{
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}
	// The launching segment's registry and the collecting one's: the run
	// is launched in one process and collected and reviewed in another.
	var regs []*tools.Registry
	got := executeRound(t, []phase.Entry{{Key: "default", Provider: prov}},
		buildOpts{register: func(_ *testing.T, r *tools.Registry) { regs = append(regs, r) }},
		1, "", nil, run{calls: []llm.ToolCall{submitWork(t)}}, runner.RunRecord{})
	if _, err := got.collector.Review(context.Background(), 1, got.work, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}

	// Neither name ever reaches a shared registry.
	for _, reg := range regs {
		for _, name := range []string{runner.SubmitWorkTool, runner.SubmitReviewTool} {
			if _, ok := reg.Lookup(name); ok {
				t.Errorf("%s leaked into the shared registry", name)
			}
		}
	}
	// The executor is offered its own and not the reviewer's.
	execOffered := toolNames(got.launch.Surface.ToolDefs())
	if !slices.Contains(execOffered, runner.SubmitWorkTool) {
		t.Errorf("the executor was not offered its submission tool: %v", execOffered)
	}
	if slices.Contains(execOffered, runner.SubmitReviewTool) {
		t.Errorf("the executor was offered the reviewer's tool: %v", execOffered)
	}
	// And the reviewer is offered its own, not the executor's.
	reviewOffered := toolNames(prov.requestsFor("review")[0].Tools)
	if !slices.Contains(reviewOffered, runner.SubmitReviewTool) {
		t.Errorf("the reviewer was not offered its submission tool: %v", reviewOffered)
	}
	if slices.Contains(reviewOffered, runner.SubmitWorkTool) {
		t.Errorf("the reviewer was offered the executor's tool: %v", reviewOffered)
	}
}

// THE REVIEWER HAS NO KNOB. It holds one submission tool, so its budget is a
// structural fact rather than an operator preference — it used to be silently
// borrowed from the executor's cap, which gave a phase that calls one tool
// twenty rounds.
func TestTheReviewerRunsOnItsOwnBudgetNotTheExecutors(t *testing.T) {
	t.Parallel()
	// A reviewer that never submits burns its whole budget and then
	// rescues, so the round count is observable.
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "x", Name: "lookup_colleague"}}},
		},
	})
	if _, err := r.Review(context.Background(), 1, turn.Work{Summary: "s"}, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	// The executor's cap in this fixture is 6; the reviewer's is its own
	// unexported constant, and must be neither that nor unbounded.
	if got := len(prov.requestsFor("review")); got == 0 || got > 5 {
		t.Errorf("the reviewer ran %d rounds, want its own small budget", got)
	}
}

// A RUNNER NEEDS ITS REGISTRIES AND ITS LAUNCHER, and refuses at assembly
// without one: every executor is a launched run, so a runner with no launcher
// cannot run the one phase every turn has, and finding that out at the first
// Execute fails a turn on a mistake made where the runner was built.
func TestARunnerNeedsItsRegistriesAndItsLauncher(t *testing.T) {
	t.Parallel()
	launcher := &playLauncher{}
	if _, err := runner.New(runner.Config{Models: &phase.Registry{}, Launcher: launcher}); err == nil {
		t.Error("a runner built with no tool registry")
	}
	if _, err := runner.New(runner.Config{Registry: tools.NewRegistry(), Launcher: launcher}); err == nil {
		t.Error("a runner built with no provider registry")
	}
	_, err := runner.New(runner.Config{Registry: tools.NewRegistry(), Models: &phase.Registry{}})
	if err == nil || !strings.Contains(err.Error(), "Launcher") {
		t.Errorf("a runner built with no launcher = %v, want a refusal naming the field", err)
	}
}

// A COLLECTION HANDED A SECOND CONTEXT IS REFUSED where it is assembled. The
// park froze what the turn started with, and a context fetched again for the
// resume is a second answer to the same question: taking either silently
// would launch the turn's next run on a prompt nobody chose.
func TestACollectionHandedASecondContextIsRefused(t *testing.T) {
	t.Parallel()
	resume := &runner.Resume{State: execstate.State{Version: execstate.Version, Round: 1}}
	for name, cfg := range map[string]runner.Config{
		"prefetched blocks": {Context: prefetch.Blocks{PersonalMemory: "- fetched again"}},
		"conversation":      {Conversation: "### Turn 1\nfetched again"},
	} {
		cfg.Registry, cfg.Models, cfg.Launcher, cfg.Resume =
			tools.NewRegistry(), &phase.Registry{}, &playLauncher{}, resume
		if _, err := runner.New(cfg); err == nil || !strings.Contains(err.Error(), "Config.Resume") {
			t.Errorf("%s: a resume handed its own context = %v, want a refusal naming the fields", name, err)
		}
	}
}

func toolNames(defs []llm.ToolDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}
