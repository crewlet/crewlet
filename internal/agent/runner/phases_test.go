package runner_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// scriptedProvider is the model behind the harness runs the engine drives (the
// executor's is played by a [playLauncher]), scripted PER PHASE and told the
// phase by the tools its harness was offered: a shared index would drift once a
// phase made one more call than the fixture counted.
//
// Each phase is a stream of responses, not a list of runs: the harness re-asks
// after every call and the engine ends a run, so a run plays responses until one
// calls nothing or the engine ends it, the next run resuming there; past the
// end the last repeats. Responses go through the real session: each reaches
// the dispatcher as a harness record, each call the tools over MCP
// (llmtest.Conn).
type scriptedProvider struct {
	review, onboarding []llmtest.Response

	mu   sync.Mutex
	runs []*scriptedRun
	n    map[string]int
}

// scriptedRun is one harness run the provider played.
type scriptedRun struct {
	which string
	req   llm.Request
	conn  *llmtest.Conn

	// offered is the tool list read before each response, one per request,
	// an activation's effect included.
	offered [][]llm.ToolDef
}

func (p *scriptedProvider) Model() string { return "scripted" }

func (p *scriptedProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if req.Tools == nil {
		return &llm.Completion{Content: "(no script)"}, nil
	}
	var run *scriptedRun
	return llmtest.Drive(ctx, req, llmtest.ConnOptions{}, func(c *llmtest.Conn) (llmtest.Response, error) {
		offered, err := c.Relist(ctx)
		if err != nil {
			return llmtest.Response{}, err
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if run == nil {
			run = &scriptedRun{which: phaseOffering(toolNames(offered)), req: req, conn: c}
			p.runs = append(p.runs, run)
		}
		run.offered = append(run.offered, offered)
		script := p.scriptOf(run.which)
		if len(script) == 0 {
			return llmtest.Response{Text: "(no script)"}, nil
		}
		if p.n == nil {
			p.n = map[string]int{}
		}
		i := min(p.n[run.which], len(script)-1)
		p.n[run.which]++
		return script[i], nil
	})
}

// phaseOffering names the phase a run is from the tools its harness was offered.
func phaseOffering(offered []string) string {
	switch {
	case slices.Contains(offered, runner.SubmitReviewTool):
		return "review"
	case slices.Contains(offered, toolname.MarkOnboarded):
		return "onboarding"
	default:
		// A phase this fixture does not know (the executor is its harness's
		// own run): answered with nothing a phase could take for a decision.
		return "other"
	}
}

func (p *scriptedProvider) scriptOf(which string) []llmtest.Response {
	switch which {
	case "review":
		return p.review
	case "onboarding":
		return p.onboarding
	default:
		return nil
	}
}

// runsFor is every harness run one phase made, in order.
func (p *scriptedProvider) runsFor(which string) []*scriptedRun {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*scriptedRun
	for _, run := range p.runs {
		if run.which == which {
			out = append(out, run)
		}
	}
	return out
}

// allRuns is every harness run, of every phase, in order.
func (p *scriptedProvider) allRuns() []*scriptedRun {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.runs)
}

// requestsFor returns the request each of one phase's runs was handed, in
// order: the conversation a run opens on.
func (p *scriptedProvider) requestsFor(which string) []llm.Request {
	var out []llm.Request
	for _, run := range p.runsFor(which) {
		out = append(out, run.req)
	}
	return out
}

// roundsOf is how many model requests (responses) one phase was asked for.
func (p *scriptedProvider) roundsOf(which string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n[which]
}

// offeredFor is what the model was offered at each of one phase's requests,
// across its runs, by name.
func (p *scriptedProvider) offeredFor(which string) [][]string {
	var out [][]string
	for _, run := range p.runsFor(which) {
		p.mu.Lock()
		lists := slices.Clone(run.offered)
		p.mu.Unlock()
		for _, defs := range lists {
			out = append(out, toolNames(defs))
		}
	}
	return out
}

// answered is what each call one phase's harness made was answered, in order.
func (p *scriptedProvider) answered(which string) []llmtest.Result {
	var out []llmtest.Result
	for _, run := range p.runsFor(which) {
		out = append(out, run.conn.Results()...)
	}
	return out
}

// answerTo is what the model read back from its first call of name, or "".
func (p *scriptedProvider) answerTo(which, name string) string {
	for _, r := range p.answered(which) {
		if r.Name == name {
			return r.Output
		}
	}
	return ""
}

// callOf is one call a response makes, its arguments encoded as the model
// writes them.
func callOf(t *testing.T, name string, args map[string]any) llmtest.Call {
	t.Helper()
	if args == nil {
		return llmtest.Call{Name: name}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("fixture arguments are not JSON: %v", err)
	}
	return llmtest.Call{Name: name, Args: string(raw)}
}

// calling is a response whose only content is calls.
func calling(calls ...llmtest.Call) llmtest.Response {
	return llmtest.Response{Calls: calls}
}

// submitCall builds a response whose only content is a call to tool with args.
func submitCall(t *testing.T, name, argsJSON string) llmtest.Response {
	t.Helper()
	if !json.Valid([]byte(argsJSON)) {
		t.Fatalf("fixture is not JSON: %s", argsJSON)
	}
	return calling(llmtest.Call{Name: name, Args: argsJSON})
}

// submitWork is the ordinary ending for a run that delivered nothing, so it
// cites no delivery.
func submitWork(t *testing.T) llm.ToolCall {
	t.Helper()
	return call(t, runner.SubmitWorkTool,
		`{"outcome":"blocked","summary":"nothing to do here","evidence":"no write tool yet"}`)
}

// activating is an engine-driven phase (onboarding) promoting one tool onto its
// surface by discovery; an executor holds every seat tool from launch.
func activating(name string) llmtest.Response {
	return calling(llmtest.Call{Name: toolname.Activate, Args: `{"name":"` + name + `"}`})
}

// deliver is the ordinary delivery a run makes: call, report.
func deliver(t *testing.T, summary string) run {
	t.Helper()
	return run{calls: []llm.ToolCall{
		{ID: "post", Name: "slack_post"},
		call(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"`+summary+`","deliveries":["slack_post"]}`),
	}, text: summary}
}

func text(s string) llmtest.Response { return llmtest.Response{Text: s} }

// workFor is a plain delivered submission, for tests that drive Review directly.
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

// fixture is the shared runner on one provider, for a turn somebody waits on.
func fixture(t *testing.T, prov *scriptedProvider) (*runner.Runner, *scriptedProvider, *tools.Registry) {
	t.Helper()
	r, reg := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{})
	return r, prov, reg
}

// buildOpts are the knobs a test varies on the shared fixture. Zero is a turn
// somebody is waiting on, whose executor's launches start nothing.
type buildOpts struct {
	reply turn.Reply
	// launcher is the executor's launch seam; nil is the fixture's own
	// [playLauncher], which records the launch and starts nothing.
	launcher runner.AgentLauncher
	// reads is what a relaunch's [readingLauncher] answers a cut-off
	// session holds, for rigs that build it ([lostRelaunchWith]); nil reads none.
	reads  *reading
	resume *runner.Resume
	pub    queue.Publisher
	// reviewChain overrides the reviewer's provider keys (`llm_review`), for
	// a real fallback chain.
	reviewChain org.ProviderKeys
	// fleets, with balance, makes the registry resolve LLM fleets.
	fleets  map[string]phase.Fleet
	balance phase.Balance
	// task overrides the brief, the one prompt piece a test can fill freely
	// (e.g. multi-byte runes for how the prompt is measured).
	task string

	// context is the turn's frozen prefetch and endings how its
	// conversation's earlier turns ended. A collection is handed neither, as
	// the engine's is: it takes them from the park ([collect]).
	context prefetch.Blocks
	endings string
	// access and changes are the seat's "Your access" section and what
	// changed in it, frozen at dispatch.
	access  string
	changes string
	// onPhase is the working indicator's seam. See [runner.Config.OnPhase].
	onPhase func(phase.Phase)
	// register adds tools beyond the fixture's standard catalogue.
	register func(t *testing.T, reg *tools.Registry)
	// steer is where the turn's notes wait: a box, or a source a case plays.
	steer runner.SteerSource
	// subagent arms delegate on the executor's surface.
	subagent *runner.SubagentConfig
	// ask is the tool the executor asks a person through, from outside the
	// seat's registry ([runner.Config.Ask]). Nil offers none.
	ask runner.RunAsk
	// checkout is the tool the executor checks a Gitlet repository out with
	// ([runner.Config.Checkout]). Nil offers none: a company hosting no code.
	checkout tools.Callable

	// calls, when set, is the run's call log, on a turn context the
	// surfaces are bound to.
	calls *turnctx.CallLog

	// boundary pins the token every fence of calls' turn context carries
	// ([turnctx.Turn.Boundary]); unset, each fence mints its own
	// ([turnctx.Turn.FenceBoundary]) and no case could tell the turn's token
	// from another. Set only beside calls, whose context carries it.
	boundary provenance.Boundary

	// budget is the turn's meter, for the cases about what a refusal stops.
	budget toolloop.BudgetMeter

	// compact is the seat's compactor, for what an assembly's rewrite costs.
	compact compact.Bound

	// fence is the seat fence, for the cases about what a closed one stops.
	fence func() error

	// withheld is what the seat's surfaces leave out (runner.Config.Withheld).
	withheld []string

	// boxServers are the seat's MCP servers that run in its executor's box
	// (role.sandbox.mcp.servers).
	boxServers []string

	// conversation is the conversation the turn serves; none unless set.
	conversation string

	// answer is the engine's read of a pickup's answer
	// (runner.Config.PickupAnswer).
	answer func(context.Context) (types.PickupAnswer, error)

	// skip is the meta-tools no record renders (runner.Config.SkipNames).
	skip []string
	// runs is the runs in flight around the run ([runner.Config.Runs]).
	runs runner.RunsAround
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
	// A MULTI-LINE description, as real MCP servers publish: an entry
	// spilling onto a second line would break one tool per line.
	if err := reg.RegisterWith(stubTool{
		name: "slack_history", out: "history",
		desc: "Read a channel's history.\n\nAccepts a cursor for paging, and\nreturns up to 200 messages.",
	}, tools.Origin("slack"), tools.Annotations{ReadOnly: mcp.Yes}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// A SECOND server, so listing one server's tools differs observably from
	// listing every MCP tool.
	if err := reg.RegisterWith(stubTool{name: "jira_create", out: "created"},
		tools.Origin("jira"), tools.Annotations{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if opts.register != nil {
		opts.register(t, reg)
	}

	models, err := phase.NewRegistry(entries)
	if opts.fleets != nil {
		models, err = phase.NewFleetRegistry(entries, opts.fleets, opts.balance)
	}
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	if len(opts.boxServers) > 0 {
		role.Sandbox = &org.RoleSandbox{MCP: org.RoleSandboxMCP{Servers: opts.boxServers}}
	}
	// The reviewer names its own key when configured; the executor runs on
	// `llm`, the seat's model. With only a default both miss and fall back
	// to it, the counterfactual the golden suite asserts.
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

	runTurn := runner.Turn{RunID: "t-1", WorkKey: "wk-1", AgentID: "a-1", ConversationKey: opts.conversation}
	if opts.calls != nil {
		runTurn.Context = &turnctx.Turn{RunID: "t-1", WorkKey: "wk-1", Calls: opts.calls, Boundary: opts.boundary}
	} else if opts.boundary != "" {
		t.Fatal("buildOpts.boundary pins the token of the turn context calls builds: set calls beside it")
	}
	launcher := opts.launcher
	if launcher == nil {
		launcher = &playLauncher{}
	}
	r, err := runner.New(runner.Config{
		Seat:              prompts.Seat{Org: organization, Role: role},
		Registry:          reg,
		Models:            models,
		Context:           opts.context,
		Endings:           opts.endings,
		Access:            opts.access,
		CapabilityChanges: opts.changes,
		Task:              task,
		Reply:             waiting,
		PickupAnswer:      opts.answer,
		Launcher:          launcher,
		Resume:            opts.resume,
		Fence:             opts.fence,
		Publisher:         opts.pub,
		OnPhase:           opts.onPhase,
		Steer:             opts.steer,
		Subagent:          opts.subagent,
		Ask:               opts.ask,
		Checkout:          opts.checkout,
		Runs:              opts.runs,
		Turn:              runTurn,
		Budget:            opts.budget,
		Compact:           opts.compact,
		Sessions:          sessions,
		Withheld:          opts.withheld,
		SkipNames:         opts.skip,
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
	if len(w.Calls) != 2 || w.Calls[0].Name != "slack_post" || w.Calls[0].Args["channel"] != "C1" {
		t.Errorf("calls = %+v, want the run's own two", w.Calls)
	}
	// The delivery check judges against this surface, so it describes the
	// whole catalogue, not just what was offered, and where each one lands,
	// which the gate compares with the surface the ask arrived on.
	if surface.Deliveries["slack_post"] != "slack" {
		t.Errorf("surface deliveries = %v, want slack_post on its own server",
			surface.Deliveries)
	}
	// The executor holds no discovery pair ([TestTheExecutorHoldsEveryToolItsSeatDoes]),
	// so none of it is among the reads.
	if !slices.Equal(surface.KnownReads, []string{"slack_history"}) {
		t.Errorf("surface known reads = %v", surface.KnownReads)
	}
}

// THE EXECUTOR HOLDS EVERY TOOL ITS SEAT DOES, from its launch: first-party and
// every MCP server's, with no discovery pair, since its harness keeps a large
// server's tools out of requests itself (Claude Code's tool search, OpenCode's
// Code Mode). The prompt names none: each definition reaches the run over the
// bridge, under its server.
//
// Mutation: put the discovery pair back on the executor's surface, or offer the
// first-party tools alone, and this goes red.
func TestTheExecutorHoldsEveryToolItsSeatDoes(t *testing.T) {
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
		"jira_create", "lookup_colleague", "reflect", "slack_history", "slack_post",
		runner.SubmitWorkTool,
	}
	slices.Sort(want)
	if !slices.Equal(offered, want) {
		t.Errorf("offered %v, want %v", offered, want)
	}
	if strings.Contains(req.System, "slack_post") || strings.Contains(req.System, "## Tools to discover") {
		t.Errorf("the prompt lists what the surface already offers:\n%s", req.System)
	}
}

// A SERVER THAT RUNS IN THE BOX IS NOT OFFERED OVER THE BRIDGE: the harness
// reaches it in the box under the same name, so offering it too would show
// every tool twice. The seat's other servers are still offered.
//
// Mutation: drop the box's servers from executorActive's filter, and slack_post
// is offered over the bridge too.
func TestAServerThatRunsInTheBoxIsNotOfferedOverTheBridge(t *testing.T) {
	t.Parallel()
	launcher := &playLauncher{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{launcher: launcher, boxServers: []string{"slack"}})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	offered := toolNames(launcher.last(t).Surface.ToolDefs())
	for _, inBox := range []string{"slack_post", "slack_history"} {
		if slices.Contains(offered, inBox) {
			t.Errorf("%s, of a server that runs in the box, is offered over the bridge too: %v",
				inBox, offered)
		}
	}
	if !slices.Contains(offered, "jira_create") {
		t.Errorf("the seat's other server is not offered: %v", offered)
	}
}

// isFinishingCorrective reports whether a message is the tool loop's re-prompt
// for a round that ended without the phase's submission.
func isFinishingCorrective(m llm.Message, tool string) bool {
	return m.Role == llm.RoleUser && strings.Contains(m.Content, "so this phase has not finished") &&
		strings.Contains(m.Content, "`"+tool+"`")
}

// A DELIVERY AN EARLIER ROUND MADE IS CITABLE: the prior-work ledger tells a
// round sent back not to comment again, so a check reading only this round
// would refuse the one true citation until the round cap ended the turn.
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

	// Counterfactual: with no such round, the same citation is refused and
	// the phase ends in the rescue.
	w = executeRound(t, entries, opts, 1, "", nil, cite, runner.RunRecord{}).work
	if !w.Rescued {
		t.Errorf("outcome = %s, want a citation of a comment nobody made refused", w.Outcome)
	}
}

func TestTheReviewersCorrectionReachesTheNextRoundWithoutRewritingTheAsk(t *testing.T) {
	t.Parallel()
	// The correction is prefixed to the BRIEF, not merged into the task:
	// knowledge search and the episode record want the requester's own ask.
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
	// With no correction the brief is byte-identical to its plain form,
	// which keeps the prompt prefix cacheable.
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

// mark_onboarded is the whole phase-scoped list: a seat that could mark itself
// from the executor would skip orientation for good. Registered here, so the
// case tests the narrowing, not a registry that never held it.
func TestTheExecutorIsNotOfferedTheOnboardingMarker(t *testing.T) {
	t.Parallel()
	launcher := &playLauncher{}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{launcher: launcher, register: func(t *testing.T, reg *tools.Registry) {
			if err := reg.Register(stubTool{name: toolname.MarkOnboarded}, tools.OriginBuiltin); err != nil {
				t.Fatalf("Register: %v", err)
			}
		}})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if slices.Contains(toolNames(launcher.last(t).Surface.ToolDefs()), toolname.MarkOnboarded) {
		t.Error("the executor's run was offered mark_onboarded")
	}
}

func TestReviewJudgesAgainstTheToolLogVerbatim(t *testing.T) {
	t.Parallel()
	// The header points at the log as the primary evidence, unelided: the
	// budgets belong to the cross-round ledger, not here.
	body := strings.Repeat("x", 3000)
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
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

// A REVIEWER IS TOLD WHAT THE RUN'S RECORD DROPPED. A long run's record keeps
// its middle only as identities (internal/sandbox's MaxBridgeCalls and
// bridge_elided_calls), so the log ends with a line counting the calls it does
// not show, by tool, and how many failed.
//
// Mutation: hand the reviewer the kept calls alone (reviewLog without its
// ledger.FormatElided line), and it judges an elision as the run.
func TestAReviewerIsToldWhatTheRunsRecordDropped(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	w := turn.Work{
		Outcome: turn.OutcomeDelivered, Summary: "post it", Text: "posted",
		Calls:  []ledger.Call{{Name: "slack_post", Args: map[string]any{"text": "the weekly summary"}}},
		Elided: []ledger.Elided{{Name: "read_file", Digest: "ab", Calls: 120, Failed: 3}, {Calls: 6}},
	}
	if _, err := r.Review(context.Background(), 1, w, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	system := prov.requestsFor("review")[0].Messages[0].Content
	for _, want := range []string{"the weekly summary", "+126 further call(s)", "read_file ×120 (3 failed)",
		"6 to tools the record did not keep either"} {
		if !strings.Contains(system, want) {
			t.Errorf("the reviewer's log does not say %q:\n%s", want, system)
		}
	}
}

// THE REVIEWER READS A LATE CALL AS AN EARLIER RUN'S. The round's record
// marks a call its log took after the job that made it was replaced
// (ledger.Call.Late), and the reviewer's log ends that call's line with
// prompts.ReviewLateCall: read as the reviewed run's own, the run is judged on
// a call it never made. The words go by the call's place, so the skipped name
// before the late posts moves them onto no other line.
//
// Mutation: pass reviewLog no notes, and no late line says so; put the notes
// at each call's shown position, and the first late post's words are lost
// while the second's land on the run's own post.
func TestTheReviewerReadsALateCallAsAnEarlierRunsCall(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{skip: []string{"activate_tool"}})
	post := func(text string, late bool) ledger.Call {
		return ledger.Call{Name: "slack_post", Args: map[string]any{"text": text}, Result: "posted", Late: late}
	}
	w := turn.Work{
		Outcome: turn.OutcomeDelivered, Summary: "post it", Text: "posted",
		Calls: []ledger.Call{{Name: "activate_tool", Result: "activated"},
			post("the earlier run's first", true), post("the run's own", false), post("the earlier run's second", true)},
	}
	if _, err := r.Review(context.Background(), 1, w, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	system := prov.requestsFor("review")[0].Messages[0].Content
	for text, late := range map[string]bool{
		"the earlier run's first": true, "the run's own": false, "the earlier run's second": true,
	} {
		line := `- slack_post({"text":"` + text + `"}) → success`
		if !strings.Contains(system, line) {
			t.Fatalf("the reviewer's log has no line %q:\n%s", line, system)
		}
		marked := strings.Contains(system, line+" "+prompts.ReviewLateCall)
		if marked != late {
			t.Errorf("the line of %q says an earlier run made it: %v, want %v:\n%s", text, marked, late, system)
		}
	}
}

// An empty log renders as "(none)": a missing heading reads as "log
// unavailable", not "no calls were made".
func TestAnEmptyToolLogStillLeavesATrace(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
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
	// Defaulting to done would make "nothing judged it" look like "judged
	// good" downstream.
	r, _, _ := fixture(t, &scriptedProvider{review: []llmtest.Response{text("looks fine to me")}})
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

// THE REVIEWER IS NEVER FORCED, its corrective round included: Claude Opus 5.5,
// Sonnet 5.5, Fable 5.1 and Mythos 5.1 answer a forced tool choice with a 400
// the chain does not retry. The corrective naming the submission is what
// re-asks a reviewer that answered in prose.
func TestTheReviewerAsksOnAutoAndIsReAskedForItsSubmission(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{review: []llmtest.Response{
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
	// The submission tool is per-phase state: in the shared registry one
	// phase's answer would leak to the next phase or turn.
	prov := &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}
	// The launching and collecting segments' registries: launch and
	// collection run in different processes.
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
	reviewOffered := prov.offeredFor("review")[0]
	if !slices.Contains(reviewOffered, runner.SubmitReviewTool) {
		t.Errorf("the reviewer was not offered its submission tool: %v", reviewOffered)
	}
	if slices.Contains(reviewOffered, runner.SubmitWorkTool) {
		t.Errorf("the reviewer was offered the executor's tool: %v", reviewOffered)
	}
}

// THE REVIEWER HAS NO KNOB: it holds one submission tool, so its round budget
// is a structural constant, not an operator preference or another phase's cap.
func TestTheReviewerRunsOnItsOwnBudgetNotTheExecutors(t *testing.T) {
	t.Parallel()
	// A reviewer that never submits burns its budget then rescues, so the
	// round count is observable: it keeps calling its submission with a
	// self_iterate lacking a correction, which the decoder refuses, each
	// refusal a round.
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llmtest.Response{
			submitCall(t, runner.SubmitReviewTool, `{"decision":"self_iterate"}`),
		},
	})
	got, err := r.Review(context.Background(), 1, turn.Work{Summary: "s"}, nil)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got.Decision != phase.SelfIterate || !strings.Contains(got.Notes, "produced no decision") {
		t.Fatalf("review = %+v, want the rescue a reviewer that never decided gets", got)
	}
	// The budget is the reviewer's own constant: bounded, not borrowed.
	if rounds := prov.roundsOf("review"); rounds == 0 || rounds > 5 {
		t.Errorf("the reviewer ran %d rounds, want its own small budget", rounds)
	}
}

// A RUNNER NEEDS ITS REGISTRIES, ITS SESSION HOST AND ITS LAUNCHER, and
// refuses at assembly without one: every turn's phases need the host and the
// launcher, and finding out at the first phase fails a turn on a build mistake.
func TestARunnerNeedsItsRegistriesAndItsLauncher(t *testing.T) {
	t.Parallel()
	launcher := &playLauncher{}
	if _, err := runner.New(runner.Config{Models: &phase.Registry{}, Launcher: launcher,
		Sessions: sessions}); err == nil {
		t.Error("a runner built with no tool registry")
	}
	if _, err := runner.New(runner.Config{Registry: tools.NewRegistry(), Launcher: launcher,
		Sessions: sessions}); err == nil {
		t.Error("a runner built with no provider registry")
	}
	_, err := runner.New(runner.Config{Registry: tools.NewRegistry(), Models: &phase.Registry{},
		Launcher: launcher})
	if err == nil || !strings.Contains(err.Error(), "Config.Sessions") {
		t.Errorf("a runner built with no session host = %v, want a refusal naming the field", err)
	}
	_, err = runner.New(runner.Config{Registry: tools.NewRegistry(), Models: &phase.Registry{},
		Sessions: sessions})
	if err == nil || !strings.Contains(err.Error(), "Launcher") {
		t.Errorf("a runner built with no launcher = %v, want a refusal naming the field", err)
	}
}

// A COLLECTION HANDED A SECOND CONTEXT IS REFUSED where it is assembled: the
// park froze the turn's context, a refetch is a second answer, and taking
// either silently would launch the next run on a prompt nobody chose.
func TestACollectionHandedASecondContextIsRefused(t *testing.T) {
	t.Parallel()
	resume := &runner.Resume{State: execstate.State{Version: execstate.Version, Round: 1}}
	for name, cfg := range map[string]runner.Config{
		"prefetched blocks": {Context: prefetch.Blocks{PersonalMemory: "- fetched again"}},
		"endings":           {Endings: "## How this conversation's earlier turns ended\nread again"},
	} {
		cfg.Registry, cfg.Models, cfg.Launcher, cfg.Resume, cfg.Sessions =
			tools.NewRegistry(), &phase.Registry{}, &playLauncher{}, resume, sessions
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
