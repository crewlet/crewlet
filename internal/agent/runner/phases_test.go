package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

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
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// scriptedProvider is the model behind every phase's CLI runs, scripted PER
// PHASE: each phase reads its own stream of responses, one per model request,
// and a run is told which phase it is by the tools its CLI was offered.
//
// Not a flat sequence: each phase runs its own loop and a phase that calls a
// tool takes another round, so a shared index drifts as soon as any phase
// makes one more call than the fixture author counted. Dispatching on the
// surface is both robust and closer to the truth — the offered tools ARE what
// distinguishes the phases.
//
// A STREAM OF RESPONSES, NOT A LIST OF RUNS, because the runs are not the
// model's to choose: its CLI asks it again after every call it makes, and the
// engine ends a run when the phase finishes, parks or is corrected. So a run
// plays the phase's next responses until one calls nothing — the model's
// answer — or the engine ends it, and the next run of the phase picks the
// stream up where that one left it. Past the end, the last response repeats: a
// model that keeps answering the same way.
//
// THROUGH THE REAL SESSION: every response reaches the phase's dispatcher as a
// CLI's record does, and every call reaches the phase's tools over MCP
// (llmtest.Conn).
type scriptedProvider struct {
	execute, review, onboarding []llmtest.Response

	mu   sync.Mutex
	runs []*scriptedRun
	n    map[string]int
}

// scriptedRun is one CLI run the provider played.
type scriptedRun struct {
	which string
	req   llm.Request
	conn  *llmtest.Conn

	// offered is the tool list the CLI read before each response it played
	// — one per model request, an activation's effect included.
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

// phaseOffering names the phase a run is from the tools its CLI was offered.
func phaseOffering(offered []string) string {
	switch {
	case slices.Contains(offered, runner.SubmitReviewTool):
		return "review"
	case slices.Contains(offered, toolname.MarkOnboarded):
		// Checked BEFORE the executor, because onboarding is the pass that
		// offers mark_onboarded and the executor is deliberately denied it:
		// a seat that could mark itself from inside the executor would
		// permanently skip orientation. See runner.phaseScoped.
		return "onboarding"
	default:
		return "execute"
	}
}

func (p *scriptedProvider) scriptOf(which string) []llmtest.Response {
	switch which {
	case "review":
		return p.review
	case "onboarding":
		return p.onboarding
	default:
		return p.execute
	}
}

// runsFor is every CLI run one phase made, in order.
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

// allRuns is every CLI run, of every phase, in order.
func (p *scriptedProvider) allRuns() []*scriptedRun {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.runs)
}

// requestsFor returns the request each of one phase's CLI runs was handed, in
// order: the conversation a run opens on.
func (p *scriptedProvider) requestsFor(which string) []llm.Request {
	var out []llm.Request
	for _, run := range p.runsFor(which) {
		out = append(out, run.req)
	}
	return out
}

// roundsOf is how many model requests — responses — one phase was asked for.
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

// answered is what every call one phase's CLI made was answered, in order:
// what the model read back from its tools.
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

// wasAnswered reports whether any call one phase made was answered with text
// containing part.
func (p *scriptedProvider) wasAnswered(which, part string) bool {
	return slices.ContainsFunc(p.answered(which), func(r llmtest.Result) bool {
		return strings.Contains(r.Output, part)
	})
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

// submitCall builds a response whose only content is a call to a tool with the
// given JSON arguments.
func submitCall(t *testing.T, name, argsJSON string) llmtest.Response {
	t.Helper()
	if !json.Valid([]byte(argsJSON)) {
		t.Fatalf("fixture is not JSON: %s", argsJSON)
	}
	return calling(llmtest.Call{Name: name, Args: argsJSON})
}

// submitWork is the ordinary ending for a round that delivered nothing: the
// executor's starting surface carries no MCP tool, so a fixture that has not
// discovered one cannot honestly cite a delivery.
func submitWork(t *testing.T) llmtest.Response {
	t.Helper()
	return submitCall(t, runner.SubmitWorkTool,
		`{"outcome":"blocked","summary":"nothing to do here","evidence":"no write tool yet"}`)
}

// activate is the discovery half of a delivery. MCP TOOLS ARE NOT ON THE
// STARTING SURFACE — that is the whole point of discovery — so a fixture that
// posts has to promote the tool first, exactly as a real turn does.
func activate(name string) llmtest.Response {
	return calling(llmtest.Call{Name: toolname.Activate, Args: `{"name":"` + name + `"}`})
}

// deliver is the ordinary three-round delivery: discover, call, report.
func deliver(t *testing.T, summary string) []llmtest.Response {
	t.Helper()
	return []llmtest.Response{
		activate("slack_post"),
		calling(llmtest.Call{Name: "slack_post"}),
		submitCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"`+summary+`","deliveries":["slack_post"]}`),
	}
}

func text(s string) llmtest.Response { return llmtest.Response{Text: s} }

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

func fixture(t *testing.T, prov *scriptedProvider) (*runner.Runner, *scriptedProvider, *tools.Registry) {
	t.Helper()
	r, reg := build(t, []phase.Entry{{Key: "default", Provider: prov}})
	return r, prov, reg
}

// unaddressedFixture is the same runner for a turn nobody asked for, so
// no_action is a legitimate submission rather than one the decoder refuses.
func unaddressedFixture(t *testing.T, prov *scriptedProvider) (*runner.Runner, *scriptedProvider) {
	t.Helper()
	r, _ := build(t, []phase.Entry{{Key: "default", Provider: prov}}, turn.NoReply())
	return r, prov
}

// runnerWithModels builds a runner over an explicit provider set, so a test
// can give each phase its own model.
func runnerWithModels(t *testing.T, entries []phase.Entry) *runner.Runner {
	t.Helper()
	r, _ := build(t, entries)
	return r
}

// buildOpts are the knobs a test varies on the shared fixture. Zero is the
// host-mode executor with somebody waiting, which is what most tests want.
type buildOpts struct {
	reply    turn.Reply
	agentRun runner.AgentLauncher
	resume   *runner.Resume
	pub      queue.Publisher
	// execChain overrides the executor's provider keys, so a test can give
	// the phase a real fallback chain rather than the single key the
	// fixture's seat runs on.
	execChain org.ProviderKeys
	// task overrides the brief, which is the one piece of the prompt a test
	// can put arbitrary text into — so a case about how the prompt is
	// MEASURED can make it carry multi-byte runes.
	task string

	// context is the turn's frozen prefetch, for the cases about what
	// reaches a prompt rather than what a phase decides.
	context prefetch.Blocks
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

	// sandbox is the seat's role.sandbox gate, for the cases about what a
	// gate does and does not offer.
	sandbox *org.RoleSandbox
}

func build(t *testing.T, entries []phase.Entry, reply ...turn.Reply) (*runner.Runner, *tools.Registry) {
	t.Helper()
	waiting := turn.ToolReply("")
	if len(reply) > 0 {
		waiting = reply[0]
	}
	return buildWith(t, entries, buildOpts{reply: waiting})
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
	if len(opts.execChain) > 0 {
		role.LLM = opts.execChain
	}
	role.LLMReview = org.ProviderKeys{"reviewer"}
	role.Sandbox = opts.sandbox
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{role}}

	task := opts.task
	if task == "" {
		task = "post the weekly summary"
	}

	runTurn := runner.Turn{RunID: "t-1", WorkKey: "wk-1", AgentID: "a-1"}
	if opts.calls != nil {
		runTurn.Context = &turnctx.Turn{RunID: "t-1", WorkKey: "wk-1", Calls: opts.calls}
	}
	r, err := runner.New(runner.Config{
		Seat:      prompts.Seat{Org: organization, Role: role},
		Registry:  reg,
		Models:    models,
		Caps:      runner.Caps{ExecutorRounds: 6},
		Context:   opts.context,
		Task:      task,
		Reply:     waiting,
		AgentRun:  opts.agentRun,
		Resume:    opts.resume,
		Publisher: opts.pub,
		OnPhase:   opts.onPhase,
		Steer:     opts.steer,
		Subagent:  opts.subagent,
		Turn:      runTurn,
		Budget:    opts.budget,
		Compact:   opts.compact,
		Sessions:  sessions,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r, reg
}

func TestTheExecutorReturnsWhatTheModelSubmitted(t *testing.T) {
	t.Parallel()
	r, _, _ := fixture(t, &scriptedProvider{execute: []llmtest.Response{
		activate("slack_post"),
		calling(callOf(t, "slack_post", map[string]any{"channel": "C1"})),
		submitCall(t, runner.SubmitWorkTool, `{
			"outcome":"delivered","summary":"posted the weekly summary",
			"deliveries":["slack_post"],"open_questions":"which channel next week?"}`),
	}})

	w, surface, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
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
	r, prov, _ := fixture(t, &scriptedProvider{execute: []llmtest.Response{submitWork(t)}})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	offered := slices.Clone(prov.offeredFor("execute")[0])
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
	if !strings.Contains(prov.requestsFor("execute")[0].Messages[0].Content, "slack") {
		t.Error("the prompt does not name the MCP server")
	}
}

// isFinishingCorrective reports whether a message is the tool loop's re-prompt
// for a round that ended without the phase's submission.
func isFinishingCorrective(m llm.Message, tool string) bool {
	return m.Role == llm.RoleUser && strings.Contains(m.Content, "so this phase has not finished") &&
		strings.Contains(m.Content, "`"+tool+"`")
}

// THE RESCUE PATH. An executor that ran out of rounds, or simply stopped, has
// produced text and no account of itself. Discarding the turn wastes
// everything it did; calling it delivered puts words in its mouth on the one
// question that matters.
//
// But only after it was ASKED: its loop finishes by `submit_work`, so a round
// of prose is not a finish, and the loop asks twice more — naming the tool —
// before the phase ends without its submission and lands here.
func TestAnExecutorThatNeverSubmittedIsRescuedAsIncomplete(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{execute: []llmtest.Response{text("I posted something.")}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	exec := prov.requestsFor("execute")
	if len(exec) != 3 {
		t.Fatalf("the executor was asked %d times, want 3 — the first answer and two "+
			"finishing correctives", len(exec))
	}
	for i, req := range exec[1:] {
		last := req.Messages[len(req.Messages)-1]
		if !isFinishingCorrective(last, runner.SubmitWorkTool) {
			t.Errorf("request %d opened on %q, want the finishing corrective", i+2, last.Content)
		}
	}
	// Every round declined, the last included: that is what tells a reader
	// the phase ended without its submission rather than finishing.
	done := completedPhase(t, pub, "execute")
	if !done.RescueFired || len(done.RoundNarration) != 3 {
		t.Fatalf("rescue_fired = %v narration = %v", done.RescueFired, done.RoundNarration)
	}
	for _, n := range done.RoundNarration {
		if n["declined"] != true {
			t.Errorf("round %v is not marked declined: %v", n["round"], n)
		}
	}
	if w.Outcome != turn.OutcomeIncomplete {
		t.Errorf("outcome = %s, want incomplete", w.Outcome)
	}
	// Its answer to the task, once: the two the correctives drew out
	// repeat it, and the reviewer would read the report three times.
	if n := strings.Count(w.Text, "posted something"); n != 1 {
		t.Errorf("the phase's text carries its answer %d times, want once: %q", n, w.Text)
	}
	// AS TEXT, NOT AS INTENT. The executor gave no account of itself, and
	// Summary is the intent line — rendered whole to every later round and
	// kept whole in the conversation ledger — so the transcript copied into
	// it went out unbounded, twice per round, under a heading calling it
	// the executor's own account.
	if w.Summary != "" {
		t.Errorf("a rescue wrote an intent the executor never gave: %q", w.Summary)
	}
	// AND IT SAYS SO. Without the mark, the word is indistinguishable from
	// one the executor chose — and every fast path in the loop turns on
	// telling "the executor decided this" from "nothing decided anything".
	if !w.Rescued {
		t.Error("a synthesised outcome was returned as if the executor had made it")
	}
}

// writtenSubmission is a measured failure: an executor ended its turn with
// submit_work's ARGUMENTS typed out as a fenced JSON block — the report, as
// prose — instead of the call.
func writtenSubmission() llmtest.Response {
	return text("Commented on the work item.\n\n```json\n" +
		`{"summary":"Commented on the work item","outcome":"blocked",` +
		`"evidence":"no write tool yet"}` + "\n```")
}

// AN EXECUTOR THAT WROTE ITS SUBMISSION AS TEXT IS RE-ASKED RATHER THAN
// RESCUED. Before, the phase ended on that round: the outcome was the engine's
// `incomplete`, and the reviewer read the fenced JSON as "what the agent
// produced". The loop now asks again with a message naming submit_work — never
// a forced call, which a request cannot express — and the model's call is the
// outcome.
func TestAnExecutorThatWroteItsSubmissionAsTextIsReAskedRatherThanRescued(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{execute: []llmtest.Response{writtenSubmission(), submitWork(t)}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{pub: pub})
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if w.Rescued || w.Outcome != turn.OutcomeBlocked {
		t.Fatalf("outcome = %s rescued = %v, want the submitted blocked", w.Outcome, w.Rescued)
	}
	exec := prov.requestsFor("execute")
	if len(exec) != 2 {
		t.Fatalf("the executor was asked %d times, want 2", len(exec))
	}
	if last := exec[1].Messages[len(exec[1].Messages)-1]; !isFinishingCorrective(last, runner.SubmitWorkTool) {
		t.Errorf("the second round opened on %q, want the finishing corrective", last.Content)
	}
	done := completedPhase(t, pub, "execute")
	if done.RescueFired || done.RoundsUsed != 2 {
		t.Errorf("rescue_fired = %v rounds_used = %d, want a submitted phase of 2 rounds",
			done.RescueFired, done.RoundsUsed)
	}
	if len(done.RoundNarration) != 1 || done.RoundNarration[0]["round"] != 1 ||
		done.RoundNarration[0]["declined"] != true {
		t.Errorf("round_narration = %v, want round 1 marked declined", done.RoundNarration)
	}
}

// A RESUMED EXECUTOR IS ASKED AGAIN TOO. It re-enters the same loop, with the
// same submission, so a coding run's result followed by a written-out report is
// the same failure and gets the same corrective — on the phase's round scale.
func TestAResumedExecutorThatWroteItsSubmissionAsTextIsReAsked(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{execute: []llmtest.Response{writtenSubmission(), submitWork(t)}}
	// A round the pre-suspend half was corrected on stays marked across the
	// suspend: the parked row carries the mark and the resumed record keeps
	// it, or round 1 would read as a finish nothing finished.
	state := suspendedAfterTwoRounds()
	state.RoundNarration[0]["declined"] = true
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		pub:    pub,
		resume: &runner.Resume{State: state, Answer: "the run succeeded"},
	})
	w, _, err := r.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if w.Rescued || w.Outcome != turn.OutcomeBlocked {
		t.Fatalf("outcome = %s rescued = %v, want the submitted blocked", w.Outcome, w.Rescued)
	}
	exec := prov.requestsFor("execute")
	if len(exec) != 2 {
		t.Fatalf("the resumed executor was asked %d times, want 2", len(exec))
	}
	if last := exec[1].Messages[len(exec[1].Messages)-1]; !isFinishingCorrective(last, runner.SubmitWorkTool) {
		t.Errorf("the second round opened on %q, want the finishing corrective", last.Content)
	}
	// Round 3 of the phase: the two it ran before it parked come first, and
	// the pre-suspend round 1 keeps its mark.
	done := completedPhase(t, pub, "execute")
	var declined []any
	for _, n := range done.RoundNarration {
		if n["declined"] == true {
			declined = append(declined, n["round"])
		}
	}
	if len(declined) != 2 || declined[0] != 1 || declined[1] != 3 {
		t.Errorf("declined rounds = %v, want [1 3] on the phase's scale", declined)
	}
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
	cite := submitCall(t, runner.SubmitWorkTool,
		`{"outcome":"delivered","summary":"re-read the item","deliveries":["comment_on_work_item"]}`)
	earlier := []ledger.Iteration{{Iteration: 1,
		Calls: []ledger.Call{{Name: "comment_on_work_item", Result: "commented"}}}}

	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{
		execute: []llmtest.Response{cite},
	}}}, buildOpts{reply: turn.ToolReply("work"), register: tracker})
	w, _, err := r.Execute(context.Background(), 2, "read the item first", earlier)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if w.Rescued || w.Outcome != turn.OutcomeDelivered {
		t.Errorf("outcome = %s (rescued %v), want the citation of round one's comment "+
			"accepted", w.Outcome, w.Rescued)
	}

	// The counterfactual: with no such round behind it, the same citation
	// is refused every time it is made, and the phase ends in the rescue.
	r, _ = buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{
		execute: []llmtest.Response{cite},
	}}}, buildOpts{reply: turn.ToolReply("work"), register: tracker})
	w, _, err = r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !w.Rescued {
		t.Errorf("outcome = %s, want a citation of a comment nobody made refused", w.Outcome)
	}
}

func TestTheReviewersCorrectionReachesTheNextRoundWithoutRewritingTheAsk(t *testing.T) {
	t.Parallel()
	// The task text also feeds knowledge search, the sandbox brief and the
	// episode record, all of which want the requester's actual ask and not
	// the engine's running commentary on it. So the correction is prefixed
	// to the user MESSAGE, not merged into the task.
	r, prov, _ := fixture(t, &scriptedProvider{execute: []llmtest.Response{submitWork(t)}})
	if _, _, err := r.Execute(context.Background(), 2, "the link was wrong", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	user := prov.requestsFor("execute")[0].Messages[1].Content
	if !strings.Contains(user, "the link was wrong") {
		t.Errorf("the correction did not reach the executor:\n%s", user)
	}
	if !strings.Contains(user, "post the weekly summary") {
		t.Errorf("the original ask was lost:\n%s", user)
	}
	// And with no correction the message is byte-identical to its
	// pre-correction form, which is what keeps the prompt prefix cacheable.
	r2, prov2, _ := fixture(t, &scriptedProvider{execute: []llmtest.Response{submitWork(t)}})
	if _, _, err := r2.Execute(context.Background(), 1, "   ", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := prov2.requestsFor("execute")[0].Messages[1].Content; strings.Contains(got, "reviewed") {
		t.Errorf("a blank correction left scaffolding behind:\n%s", got)
	}
}

func TestTheExecutorReportsWhatItCalledAndWhatWasMissing(t *testing.T) {
	t.Parallel()
	r, _, _ := fixture(t, &scriptedProvider{execute: []llmtest.Response{
		activate("slack_post"),
		calling(callOf(t, "slack_post", map[string]any{"channel": "C1"}),
			llmtest.Call{Name: "ghost_tool"}),
		submitWork(t),
	}})
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(w.Calls) < 3 {
		t.Fatalf("calls = %+v", w.Calls)
	}
	if w.Calls[1].Name != "slack_post" || w.Calls[1].Args["channel"] != "C1" {
		t.Errorf("the delivery call = %+v", w.Calls[1])
	}
	// Membership in the snapshot is the single source of truth. Matching on
	// the failure TEXT would flag a false positive the moment a legitimate
	// tool's own output began with the same words.
	if !slices.Equal(w.MissingTools, []string{"ghost_tool"}) {
		t.Errorf("missing tools = %v", w.MissingTools)
	}
}

// mark_onboarded is the whole phase-scoped list, and it earns its place:
// onboarding is its own pass, and a seat that could mark itself from inside
// the executor would permanently skip orientation.
func TestTheExecutorIsNotOfferedTheOnboardingMarker(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{execute: []llmtest.Response{submitWork(t)}})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if slices.Contains(prov.offeredFor("execute")[0], toolname.MarkOnboarded) {
		t.Error("the executor was offered mark_onboarded")
	}
}

func TestReviewJudgesAgainstTheToolLogVerbatim(t *testing.T) {
	t.Parallel()
	// The header points at the log as the primary evidence. A reviewer
	// judging an ELIDED log is judging a summary and calling it evidence —
	// the budgets belong to the cross-round ledger, not here.
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

// An empty log renders as "(none)" rather than being omitted: a missing
// heading reads as "log unavailable", not "no calls were made".
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
	// Defaulting to done here is the difference between "the work was
	// judged good" and "nothing judged it", and those look identical
	// downstream.
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

// THE REVIEWER IS NEVER FORCED, its corrective round included — a request has
// no way to force a call. Its only tool is its submission, so a forced choice
// once looked free — but Claude Opus 5.5, Sonnet 5.5, Fable 5.1 and Mythos 5.1
// answer one with a 400, which the chain does not retry, so on those models
// every review died before the corrective that actually enforces the call
// could run. The submission it names is what re-asks a reviewer that answered
// in prose.
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
	// The submission tool is per-phase state. Registering it into the
	// shared registry would leave one phase's answer visible to the next —
	// or to the next turn, still holding the last one's decision.
	r, prov, reg := fixture(t, &scriptedProvider{
		execute: []llmtest.Response{submitWork(t)},
		review:  []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})

	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := r.Review(context.Background(), 1, w, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}

	// Neither name ever reaches the shared registry.
	for _, name := range []string{runner.SubmitWorkTool, runner.SubmitReviewTool} {
		if _, ok := reg.Lookup(name); ok {
			t.Errorf("%s leaked into the shared registry", name)
		}
	}
	// The executor is offered its own and not the reviewer's.
	execOffered := prov.offeredFor("execute")[0]
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

// THE REVIEWER HAS NO KNOB. It holds one submission tool, so its budget is a
// structural fact rather than an operator preference — it used to be silently
// borrowed from the executor's cap, which gave a phase that calls one tool
// twenty rounds.
func TestTheReviewerRunsOnItsOwnBudgetNotTheExecutors(t *testing.T) {
	t.Parallel()
	// A reviewer that never submits burns its whole budget and then
	// rescues, so the round count is observable. Its submission is the one
	// tool it holds, so it keeps CALLING it — a self_iterate with no
	// correction, which the decoder refuses — and every refused call is a
	// round the budget counts.
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
	// The executor's cap in this fixture is 6; the reviewer's is its own
	// unexported constant, and must be neither that nor unbounded.
	if rounds := prov.roundsOf("review"); rounds == 0 || rounds > 5 {
		t.Errorf("the reviewer ran %d rounds, want its own small budget", rounds)
	}
}

func TestARunnerNeedsItsRegistries(t *testing.T) {
	t.Parallel()
	if _, err := runner.New(runner.Config{Models: &phase.Registry{}}); err == nil {
		t.Error("a runner built with no tool registry")
	}
	if _, err := runner.New(runner.Config{Registry: tools.NewRegistry()}); err == nil {
		t.Error("a runner built with no provider registry")
	}
	// AND ITS SESSION HOST, which every phase's CLI reaches its tools
	// through: without one the first phase fails, after the turn's prompts
	// were built and its prefetch spent.
	_, err := runner.New(runner.Config{Registry: tools.NewRegistry(), Models: &phase.Registry{}})
	if err == nil || !strings.Contains(err.Error(), "Config.Sessions") {
		t.Errorf("a runner built with no session host answered %v, want a refusal naming the field", err)
	}
}

func toolNames(defs []llm.ToolDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

// breakingProvider plays a fixed script and then fails its CLI run, so a phase
// can be stopped at a chosen point WITH tool calls already behind it. The
// scripted provider above cannot fail at all, and "the phase broke after it had
// already written" is not reachable any other way.
type breakingProvider struct {
	script []llmtest.Response
	err    error

	mu sync.Mutex
	n  int
}

func (p *breakingProvider) Model() string { return "breaking" }

func (p *breakingProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	return llmtest.Drive(ctx, req, llmtest.ConnOptions{}, func(*llmtest.Conn) (llmtest.Response, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.n >= len(p.script) {
			return llmtest.Response{}, p.err
		}
		r := p.script[p.n]
		p.n++
		return r, nil
	})
}

// A BROKEN PHASE HANDS BACK WHAT IT ALREADY DID.
//
// The seam this whole fix rests on, and the one a fake cannot vouch for: the
// dispatcher gives up a trigger when the turn PROVES it wrote outside the
// engine, and the proof is the record the executor carries out with its error.
// This path used to `return turn.Work{}, turn.Surface{}, err` — throwing that
// record away on the one path where a caller most needs it — so a turn that
// had already commented on an issue looked exactly like one that had done
// nothing, and its trigger was replayed until the broker gave up.
func TestABrokenExecutorReturnsTheCallsItAlreadyMade(t *testing.T) {
	t.Parallel()
	prov := &breakingProvider{
		script: []llmtest.Response{
			activate("jira_create"),
			calling(llmtest.Call{Name: "jira_create"}),
		},
		err: errors.New("the provider went away mid-loop"),
	}
	r, _ := build(t, []phase.Entry{{Key: "default", Provider: prov}})

	work, surface, err := r.Execute(t.Context(), 1, "", nil)
	if err == nil {
		t.Fatal("the phase did not break")
	}
	if !slices.ContainsFunc(work.Calls, func(c ledger.Call) bool { return c.Name == "jira_create" }) {
		t.Fatalf("calls = %+v, want the issue it created before it broke", work.Calls)
	}
	// AND THE SURFACE COMES WITH IT, because the record alone cannot be
	// judged: whether a call reached outside the engine is a fact about the
	// tool's origin and annotations, which only the surface carries.
	if !turn.Acted(work.Calls, surface) {
		t.Errorf("the executor created an issue and then broke, and the turn "+
			"reads as having done nothing (surface = %+v)", surface)
	}
}

// The counterfactual, kept beside it: a phase that broke before any call still
// reports nothing, so a transient failure keeps its retry.
func TestAnExecutorThatBrokeBeforeCallingAnythingReportsNothing(t *testing.T) {
	t.Parallel()
	prov := &breakingProvider{err: errors.New("no route to the provider")}
	r, _ := build(t, []phase.Entry{{Key: "default", Provider: prov}})

	work, surface, err := r.Execute(t.Context(), 1, "", nil)
	if err == nil {
		t.Fatal("the phase did not break")
	}
	if turn.Acted(work.Calls, surface) {
		t.Errorf("a phase that never called anything reads as having acted: %+v", work.Calls)
	}
}
