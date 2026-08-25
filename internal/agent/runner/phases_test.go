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
	plan, execute, review, onboarding []llm.Completion
	seen                              []llm.Request
	n                                 map[string]int
}

func (p *scriptedProvider) Model() string { return "scripted" }

func (p *scriptedProvider) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	p.seen = append(p.seen, req)
	which, script := p.scriptFor(req)
	if p.n == nil {
		p.n = map[string]int{}
	}
	i := min(p.n[which], len(script)-1)
	p.n[which]++
	if len(script) == 0 {
		return &llm.Completion{Content: "(no script)"}, nil
	}
	c := script[i]
	return &c, nil
}

func (p *scriptedProvider) scriptFor(req llm.Request) (string, []llm.Completion) {
	offered := toolNames(req.Tools)
	switch {
	case slices.Contains(offered, runner.SubmitPlanTool):
		return "plan", p.plan
	case slices.Contains(offered, runner.SubmitReviewTool):
		return "review", p.review
	case slices.Contains(offered, runner.MarkOnboardedTool):
		// Checked AFTER the submit tools, because it is the pass that
		// offers mark_onboarded and NEITHER of them: Plan used to offer
		// mark_onboarded too, and keying on it first answered a plan with
		// an onboarding mark. See runner.phaseScoped.
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

func text(s string) llm.Completion { return llm.Completion{Content: s} }

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

// runnerWithModels builds a runner over an explicit provider set, so a test
// can give each phase its own model.
func runnerWithModels(t *testing.T, entries []phase.Entry) *runner.Runner {
	t.Helper()
	r, _ := build(t, entries)
	return r
}

func build(t *testing.T, entries []phase.Entry) (*runner.Runner, *tools.Registry) {
	t.Helper()
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
	// Each phase names its own key when one is configured. With only a
	// default present these all miss and resolution falls back to it, which
	// is the counterfactual the golden suite asserts.
	role.LLMPlan = org.ProviderKeys{"planner"}
	role.LLMExecute = org.ProviderKeys{"executor"}
	role.LLMReview = org.ProviderKeys{"reviewer"}
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{role}}

	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: organization, Role: role},
		Registry: reg,
		Models:   models,
		Caps: runner.Caps{
			PlanRounds: 4, ExecuteRounds: 6, ReviewRounds: 3,
		},
		Task:     "post the weekly summary",
		AlwaysOn: []string{"reflect"},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r, reg
}

func TestPlanReturnsWhatTheModelSubmitted(t *testing.T) {
	t.Parallel()
	r, _, _ := fixture(t, &scriptedProvider{plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool, `{
		"decision":"plan","reasoning":"post it",
		"tools_needed":["slack_post"],
		"steps":[{"intent":"post","approach":"Weekly: three PRs."}],
		"success_criteria":["the post exists"]}`)}})

	p, surface, err := r.Plan(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if p.Decision != turn.PlanRun {
		t.Errorf("decision = %s", p.Decision)
	}
	if !slices.Equal(p.ToolsNeeded, []string{"slack_post"}) {
		t.Errorf("tools_needed = %v", p.ToolsNeeded)
	}
	if !strings.Contains(p.Summary, "Weekly: three PRs.") {
		t.Errorf("the step's approach was lost:\n%s", p.Summary)
	}
	// The surface handed back is what the delivery gate judges against, so
	// it must describe the whole catalogue and not just what was offered.
	if !slices.Contains(surface.MCPTools, "slack_post") {
		t.Errorf("surface MCP tools = %v", surface.MCPTools)
	}
	if !slices.Equal(surface.KnownReads, []string{"slack_history"}) {
		t.Errorf("surface known reads = %v", surface.KnownReads)
	}
}

func TestThePlannerIsNotHandedEveryMCPTool(t *testing.T) {
	t.Parallel()
	// A real server publishes dozens and a planner shown all of them plans
	// against a wall of text. Discovery is a tool call, which also keeps the
	// prompt prefix stable while a server's catalogue changes underneath.
	r, prov, _ := fixture(t, &scriptedProvider{plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool, `{"decision":"plan","tools_needed":["slack_post"]}`)}})
	if _, _, err := r.Plan(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	offered := toolNames(prov.requestsFor("plan")[0].Tools)
	if slices.Contains(offered, "slack_post") {
		t.Errorf("the planner was offered an MCP tool directly: %v", offered)
	}
	if !slices.Contains(offered, runner.SubmitPlanTool) {
		t.Errorf("the planner was not offered its own submission tool: %v", offered)
	}
	if !slices.Contains(offered, "lookup_colleague") {
		t.Errorf("the planner was not offered the first-party tools: %v", offered)
	}
	// But the CATALOGUE names the server, or it cannot plan to discover it.
	if !strings.Contains(prov.requestsFor("plan")[0].Messages[0].Content, "slack") {
		t.Error("the planner's prompt does not name the MCP server")
	}
}

func TestAPlannerThatNeverSubmittedFallsBackWithoutInventingAPlan(t *testing.T) {
	t.Parallel()
	// Discarding the turn wastes everything the phase did; inventing a full
	// plan puts words in its mouth. A direct plan carrying its own text is
	// the honest middle — and the engine's forced-Review net still catches
	// a non-delivery.
	r, _, _ := fixture(t, &scriptedProvider{plan: []llm.Completion{text("I think we should post something.")}})
	p, _, err := r.Plan(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if p.Decision != turn.PlanDirect {
		t.Errorf("decision = %s, want direct", p.Decision)
	}
	if !strings.Contains(p.Reasoning, "post something") {
		t.Errorf("the phase's own text was discarded: %q", p.Reasoning)
	}
	if len(p.ToolsNeeded) != 0 {
		t.Errorf("tools_needed = %v, want nothing invented", p.ToolsNeeded)
	}
}

func TestTheReviewersCorrectionReachesTheNextPlanWithoutRewritingTheAsk(t *testing.T) {
	t.Parallel()
	// The task text also feeds knowledge search, the sandbox brief and the
	// episode record, all of which want the requester's actual ask and not
	// the engine's running commentary on it. So the correction is prefixed
	// to the user MESSAGE, not merged into the task.
	r, prov, _ := fixture(t, &scriptedProvider{plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool, `{"decision":"plan","tools_needed":["slack_post"]}`)}})
	if _, _, err := r.Plan(context.Background(), 2, "the link was wrong", nil); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	user := prov.requestsFor("plan")[0].Messages[1].Content
	if !strings.Contains(user, "the link was wrong") {
		t.Errorf("the correction did not reach the planner:\n%s", user)
	}
	if !strings.Contains(user, "post the weekly summary") {
		t.Errorf("the original ask was lost:\n%s", user)
	}
	// And with no correction the message is byte-identical to its
	// pre-correction form, which is what keeps the prompt prefix cacheable.
	r2, prov2, _ := fixture(t, &scriptedProvider{plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool, `{"decision":"plan","tools_needed":["slack_post"]}`)}})
	if _, _, err := r2.Plan(context.Background(), 1, "   ", nil); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := prov2.requestsFor("plan")[0].Messages[1].Content; strings.Contains(got, "reviewed") {
		t.Errorf("a blank correction left scaffolding behind:\n%s", got)
	}
}

func TestExecuteGetsWhatThePlanNamedPlusTheAlwaysOnSet(t *testing.T) {
	t.Parallel()
	// A plan that named its delivery tool should be executing it, not
	// re-deciding against the full catalogue.
	r, prov, _ := fixture(t, &scriptedProvider{execute: []llm.Completion{text("posted")}})
	p := turn.Plan{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}, Summary: "post it"}
	if _, _, err := r.Execute(context.Background(), 1, p, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	offered := toolNames(prov.requestsFor("execute")[0].Tools)
	slices.Sort(offered)
	// The discovery pair is always present: a phase that cannot discover a
	// tool cannot recover from a planner that guessed a name wrong, and the
	// delivery gate's own correction tells it to use exactly these two.
	want := []string{"activate_tool", "list_mcp_server_tools", "reflect", "slack_post"}
	if !slices.Equal(offered, want) {
		t.Errorf("offered %v, want the plan's tool, the always-on set and discovery", offered)
	}
}

func TestADirectPlanGetsEverything(t *testing.T) {
	t.Parallel()
	// It committed to one shot with no multi-step plan, so it gets the
	// whole surface to work with.
	r, prov, _ := fixture(t, &scriptedProvider{execute: []llm.Completion{text("posted")}})
	p := turn.Plan{Decision: turn.PlanDirect}
	if _, _, err := r.Execute(context.Background(), 1, p, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	offered := toolNames(prov.requestsFor("execute")[0].Tools)
	if !slices.Contains(offered, "slack_post") || !slices.Contains(offered, "slack_history") {
		t.Errorf("a direct plan was offered %v", offered)
	}
}

func TestAPhantomToolIsDroppedAndNamedRatherThanFailingThePhase(t *testing.T) {
	t.Parallel()
	// The planner guessed at an MCP surface it could not see. Failing the
	// phase turns a recoverable mis-guess into a lost turn — but saying
	// nothing lets the executor assume the tool exists, fail to call it,
	// and settle for a text reply that delivers nothing.
	r, prov, _ := fixture(t, &scriptedProvider{execute: []llm.Completion{text("posted")}})
	p := turn.Plan{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_send_msg", "slack_post"}}
	if _, _, err := r.Execute(context.Background(), 1, p, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	offered := toolNames(prov.requestsFor("execute")[0].Tools)
	if slices.Contains(offered, "slack_send_msg") {
		t.Errorf("a phantom was offered: %v", offered)
	}
	if !slices.Contains(offered, "slack_post") {
		t.Errorf("the real tool was dropped alongside the phantom: %v", offered)
	}
	if !strings.Contains(prov.requestsFor("execute")[0].Messages[0].Content, "slack_send_msg") {
		t.Error("the executor was not told which name did not resolve")
	}
}

func TestExecuteReportsWhatItCalledAndWhatWasMissing(t *testing.T) {
	t.Parallel()
	r, _, _ := fixture(t, &scriptedProvider{execute: []llm.Completion{
		{ToolCalls: []llm.ToolCall{
			{ID: "a", Name: "slack_post", Arguments: map[string]any{"channel": "C1"}},
			{ID: "b", Name: "ghost_tool"},
		}},
		text("done"),
	}})
	p := turn.Plan{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}
	e, _, err := r.Execute(context.Background(), 1, p, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if e.Text != "done" {
		t.Errorf("text = %q", e.Text)
	}
	if len(e.Calls) != 2 {
		t.Fatalf("calls = %+v", e.Calls)
	}
	if e.Calls[0].Name != "slack_post" || e.Calls[0].Args["channel"] != "C1" {
		t.Errorf("first call = %+v", e.Calls[0])
	}
	// Membership in the snapshot is the single source of truth. Matching on
	// the failure TEXT would flag a false positive the moment a legitimate
	// tool's own output began with the same words.
	if !slices.Equal(e.MissingTools, []string{"ghost_tool"}) {
		t.Errorf("missing tools = %v", e.MissingTools)
	}
}

func TestReviewJudgesAgainstTheToolLogsVerbatim(t *testing.T) {
	t.Parallel()
	// The header points at the logs as the primary evidence. A reviewer
	// judging an ELIDED log is judging a summary and calling it evidence —
	// the budgets belong to the cross-round ledger, not here.
	body := strings.Repeat("x", 3000)
	r, prov, _ := fixture(t, &scriptedProvider{review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)}})
	p := turn.Plan{Decision: turn.PlanRun, Summary: "post it", ToolsNeeded: []string{"slack_post"}}
	e := turn.Execution{
		Text:  "posted",
		Calls: []ledger.Call{{Name: "slack_post", Args: map[string]any{"text": body}}},
	}
	if _, err := r.Review(context.Background(), 1, p, e, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	system := prov.requestsFor("review")[0].Messages[0].Content
	if !strings.Contains(system, body) {
		t.Error("Review's evidence log was elided")
	}
	// Plan's log renders as "(none)" rather than being omitted: a missing
	// heading reads as "log unavailable", not "no calls were made".
	if !strings.Contains(system, "(none)") {
		t.Errorf("an empty Plan log left no trace:\n%s", system[:min(600, len(system))])
	}
}

func TestAReviewerThatNeverDecidedDoesNotSilentlyPassTheTurn(t *testing.T) {
	t.Parallel()
	// Defaulting to done here is the difference between "the work was
	// judged good" and "nothing judged it", and those look identical
	// downstream.
	r, _, _ := fixture(t, &scriptedProvider{review: []llm.Completion{text("looks fine to me")}})
	got, err := r.Review(context.Background(), 1, turn.Plan{}, turn.Execution{}, nil)
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
		plan:    []llm.Completion{submitCall(t, runner.SubmitPlanTool, `{"decision":"plan","tools_needed":["slack_post"]}`)},
		execute: []llm.Completion{text("posted")},
		review:  []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})

	p, _, err := r.Plan(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	e, _, err := r.Execute(context.Background(), 1, p, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := r.Review(context.Background(), 1, p, e, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}

	// Neither name ever reaches the shared registry.
	for _, name := range []string{runner.SubmitPlanTool, runner.SubmitReviewTool} {
		if _, ok := reg.Lookup(name); ok {
			t.Errorf("%s leaked into the shared registry", name)
		}
	}
	// Execute is offered neither.
	execOffered := toolNames(prov.requestsFor("execute")[0].Tools)
	for _, name := range []string{runner.SubmitPlanTool, runner.SubmitReviewTool} {
		if slices.Contains(execOffered, name) {
			t.Errorf("Execute was offered %s: %v", name, execOffered)
		}
	}
	// And Review is offered its own, not Plan's.
	reviewOffered := toolNames(prov.requestsFor("review")[0].Tools)
	if !slices.Contains(reviewOffered, runner.SubmitReviewTool) {
		t.Errorf("Review was not offered its submission tool: %v", reviewOffered)
	}
	if slices.Contains(reviewOffered, runner.SubmitPlanTool) {
		t.Errorf("Review was offered Plan's submission tool: %v", reviewOffered)
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
