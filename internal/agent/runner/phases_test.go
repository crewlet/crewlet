package runner_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// scriptedProvider replays completions PER PHASE, choosing the script from the
// tools the request offers.
//
// Not a flat sequence: each phase runs its own loop and a phase that calls a
// tool takes another round, so a shared index drifts as soon as any phase
// makes one more call than the fixture author counted. Dispatching on the
// surface is both robust and closer to the truth — the offered tools ARE what
// distinguishes the phases.
type scriptedProvider struct {
	execute, review, onboarding []llm.Completion
	seen                        []llm.Request
	n                           map[string]int
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
		// Checked BEFORE the executor, because onboarding is the pass that
		// offers mark_onboarded and the executor is deliberately denied it:
		// a seat that could mark itself from inside the executor would
		// permanently skip orientation. See runner.phaseScoped.
		return "onboarding", p.onboarding
	default:
		return "execute", p.execute
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

// submitWork is the ordinary ending for a round that delivered nothing: the
// executor's starting surface carries no MCP tool, so a fixture that has not
// discovered one cannot honestly cite a delivery.
func submitWork(t *testing.T) llm.Completion {
	t.Helper()
	return submitCall(t, runner.SubmitWorkTool,
		`{"outcome":"blocked","summary":"nothing to do here","evidence":"no write tool yet"}`)
}

// activate is the discovery half of a delivery. MCP TOOLS ARE NOT ON THE
// STARTING SURFACE — that is the whole point of discovery — so a fixture that
// posts has to promote the tool first, exactly as a real turn does.
func activate(name string) llm.Completion {
	return llm.Completion{ToolCalls: []llm.ToolCall{
		{ID: "act", Name: "activate_tool", Arguments: map[string]any{"name": name}},
	}}
}

// deliver is the ordinary three-round delivery: discover, call, report.
func deliver(t *testing.T, summary string) []llm.Completion {
	t.Helper()
	return []llm.Completion{
		activate("slack_post"),
		{ToolCalls: []llm.ToolCall{{ID: "post", Name: "slack_post"}}},
		submitCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"`+summary+`","deliveries":["slack_post"]}`),
	}
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

func fixture(t *testing.T, prov *scriptedProvider) (*runner.Runner, *scriptedProvider, *tools.Registry) {
	t.Helper()
	r, reg := build(t, []phase.Entry{{Key: "default", Provider: prov}})
	return r, prov, reg
}

// unaddressedFixture is the same runner for a turn nobody asked for, so
// no_action is a legitimate submission rather than one the decoder refuses.
func unaddressedFixture(t *testing.T, prov *scriptedProvider) (*runner.Runner, *scriptedProvider) {
	t.Helper()
	r, _ := build(t, []phase.Entry{{Key: "default", Provider: prov}}, turn.ReplyNone)
	return r, prov
}

// runnerWithModels builds a runner over an explicit provider set, so a test
// can give each phase its own model.
func runnerWithModels(t *testing.T, entries []phase.Entry) *runner.Runner {
	t.Helper()
	r, _ := build(t, entries)
	return r
}

func build(t *testing.T, entries []phase.Entry, reply ...turn.Reply) (*runner.Runner, *tools.Registry) {
	t.Helper()
	waiting := turn.ReplyTool
	if len(reply) > 0 {
		waiting = reply[0]
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
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{role}}

	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: organization, Role: role},
		Registry: reg,
		Models:   models,
		Caps:     runner.Caps{ExecutorRounds: 6},
		Task:     "post the weekly summary",
		Reply:    waiting,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r, reg
}

func TestTheExecutorReturnsWhatTheModelSubmitted(t *testing.T) {
	t.Parallel()
	r, _, _ := fixture(t, &scriptedProvider{execute: []llm.Completion{
		activate("slack_post"),
		{ToolCalls: []llm.ToolCall{{ID: "a", Name: "slack_post", Arguments: map[string]any{"channel": "C1"}}}},
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
	if !slices.Contains(surface.MCPTools, "slack_post") {
		t.Errorf("surface MCP tools = %v", surface.MCPTools)
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
	r, prov, _ := fixture(t, &scriptedProvider{execute: []llm.Completion{submitWork(t)}})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	offered := toolNames(prov.requestsFor("execute")[0].Tools)
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

// THE RESCUE PATH. An executor that ran out of rounds, or simply stopped, has
// produced text and no account of itself. Discarding the turn wastes
// everything it did; calling it delivered puts words in its mouth on the one
// question that matters.
func TestAnExecutorThatNeverSubmittedIsRescuedAsIncomplete(t *testing.T) {
	t.Parallel()
	r, _, _ := fixture(t, &scriptedProvider{execute: []llm.Completion{text("I posted something.")}})
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if w.Outcome != turn.OutcomeIncomplete {
		t.Errorf("outcome = %s, want incomplete", w.Outcome)
	}
	if !strings.Contains(w.Summary, "posted something") {
		t.Errorf("the phase's own text was discarded: %q", w.Summary)
	}
	// AND IT SAYS SO. Without the mark, the word is indistinguishable from
	// one the executor chose — and every fast path in the loop turns on
	// telling "the executor decided this" from "nothing decided anything".
	if !w.Rescued {
		t.Error("a synthesised outcome was returned as if the executor had made it")
	}
}

func TestTheReviewersCorrectionReachesTheNextRoundWithoutRewritingTheAsk(t *testing.T) {
	t.Parallel()
	// The task text also feeds knowledge search, the sandbox brief and the
	// episode record, all of which want the requester's actual ask and not
	// the engine's running commentary on it. So the correction is prefixed
	// to the user MESSAGE, not merged into the task.
	r, prov, _ := fixture(t, &scriptedProvider{execute: []llm.Completion{submitWork(t)}})
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
	r2, prov2, _ := fixture(t, &scriptedProvider{execute: []llm.Completion{submitWork(t)}})
	if _, _, err := r2.Execute(context.Background(), 1, "   ", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := prov2.requestsFor("execute")[0].Messages[1].Content; strings.Contains(got, "reviewed") {
		t.Errorf("a blank correction left scaffolding behind:\n%s", got)
	}
}

func TestTheExecutorReportsWhatItCalledAndWhatWasMissing(t *testing.T) {
	t.Parallel()
	r, _, _ := fixture(t, &scriptedProvider{execute: []llm.Completion{
		activate("slack_post"),
		{ToolCalls: []llm.ToolCall{
			{ID: "a", Name: "slack_post", Arguments: map[string]any{"channel": "C1"}},
			{ID: "b", Name: "ghost_tool"},
		}},
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
	r, prov, _ := fixture(t, &scriptedProvider{execute: []llm.Completion{submitWork(t)}})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if slices.Contains(toolNames(prov.requestsFor("execute")[0].Tools), runner.MarkOnboardedTool) {
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

func TestEachPhaseGetsItsOwnSubmissionToolAndNoneLeaks(t *testing.T) {
	t.Parallel()
	// The submission tool is per-phase state. Registering it into the
	// shared registry would leave one phase's answer visible to the next —
	// or to the next turn, still holding the last one's decision.
	r, prov, reg := fixture(t, &scriptedProvider{
		execute: []llm.Completion{submitWork(t)},
		review:  []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
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
	execOffered := toolNames(prov.requestsFor("execute")[0].Tools)
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

func TestARunnerNeedsItsRegistries(t *testing.T) {
	t.Parallel()
	if _, err := runner.New(runner.Config{Models: &phase.Registry{}}); err == nil {
		t.Error("a runner built with no tool registry")
	}
	if _, err := runner.New(runner.Config{Registry: tools.NewRegistry()}); err == nil {
		t.Error("a runner built with no provider registry")
	}
}

func toolNames(defs []llm.ToolDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}
