package subagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// --- doubles ---------------------------------------------------------------

// stubTool counts its calls, so a test can assert a denied tool was not merely
// absent from the offered list but genuinely never RAN.
type stubTool struct {
	name string
	desc string
	out  string
	ran  atomic.Int32
}

func (s *stubTool) Name() string { return s.name }
func (s *stubTool) Description() string {
	if s.desc != "" {
		return s.desc
	}
	return s.name + " does a thing"
}
func (s *stubTool) Parameters() map[string]any { return nil }
func (s *stubTool) Call(context.Context, map[string]any) (tools.Result, error) {
	s.ran.Add(1)
	out := s.out
	if out == "" {
		out = s.name + " ok"
	}
	return tools.Result{Output: out}, nil
}

// provider replays a caller-supplied reply function, so a case states the
// model's behaviour — including blocking, panicking and token spend — as
// ordinary Go rather than as a mock's expectations.
type provider struct {
	name  string
	reply func(ctx context.Context, n int, req llm.Request) (*llm.Completion, error)

	mu   sync.Mutex
	seen []llm.Request
}

func (p *provider) Model() string { return p.name }

func (p *provider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	p.mu.Lock()
	p.seen = append(p.seen, req)
	n := len(p.seen)
	p.mu.Unlock()
	if p.reply == nil {
		return answer("done", 0, 0), nil
	}
	return p.reply(ctx, n, req)
}

func (p *provider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.seen)
}

func (p *provider) offered(i int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, t := range p.seen[i].Tools {
		out = append(out, t.Name)
	}
	return out
}

// say builds a plain text answer with a token cost.
func say(text string, in, out int) *llm.Completion {
	return &llm.Completion{Model: "scripted", Content: text, InputTokens: in, OutputTokens: out}
}

// callTool builds an answer that asks for one tool.
func callTool(name string, args map[string]any, in, out int) *llm.Completion {
	return &llm.Completion{
		Model: "scripted", InputTokens: in, OutputTokens: out,
		ToolCalls: []llm.ToolCall{{ID: "c" + name, Name: name, Arguments: args}},
	}
}

// meter is a shared token counter that records every charge it actually saw.
type meter struct {
	mu      sync.Mutex
	charges []int
	total   int
	refuse  bool
	err     error
}

func (m *meter) Spend(context.Context, int) (toolloop.SpendOutcome, error) {
	return toolloop.SpendOutcome{}, nil
}

func (m *meter) spend(tokens int) (toolloop.SpendOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return toolloop.SpendOutcome{}, m.err
	}
	if m.refuse {
		return toolloop.SpendOutcome{OK: false, Scope: "org", Used: m.total, Limit: m.total}, nil
	}
	m.charges = append(m.charges, tokens)
	m.total += tokens
	return toolloop.SpendOutcome{OK: true, Scope: "org", Used: m.total}, nil
}

func (m *meter) sum() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

// countingMeter is the real BudgetMeter shape over meter.spend.
type countingMeter struct{ m *meter }

func (c countingMeter) Spend(_ context.Context, tokens int) (toolloop.SpendOutcome, error) {
	return c.m.spend(tokens)
}

// publisher captures the batch summary event.
type publisher struct {
	mu     sync.Mutex
	topics []string
	events []*events.Event
	err    error
}

func (p *publisher) Publish(_ context.Context, topic string, ev *events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.topics = append(p.topics, topic)
	p.events = append(p.events, ev)
	return p.err
}

func (p *publisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events)
}

// activator is a stand-in for the engine's activate_tool meta-tool: it does
// the one thing that matters to this package's boundary, which is promoting a
// name onto the CHILD's surface.
type activator struct{ surface func() *tools.Surface }

func (a *activator) Name() string        { return "child_activate" }
func (a *activator) Description() string { return "Activate a tool from your catalogue." }
func (a *activator) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"name": map[string]any{"type": "string"},
	}}
}

func (a *activator) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	name, _ := args["name"].(string)
	if a.surface().Activate(name) {
		return tools.Result{Output: name + " activated"}, nil
	}
	return tools.Result{Output: "no tool named " + name, Failed: true}, nil
}

func discovery(get func() *tools.Surface) []tools.Callable {
	return []tools.Callable{&activator{surface: get}}
}

// --- fixtures --------------------------------------------------------------

// world is the parent's catalogue: one plain builtin, one proven read, one
// annotated shared write, one unannotated MCP tool, and the engine-control
// tools a parent legitimately holds.
type world struct {
	registry *tools.Registry
	snapshot tools.Snapshot
	byName   map[string]*stubTool
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{registry: tools.NewRegistry(), byName: map[string]*stubTool{}}
	add := func(name, origin string, ann tools.Annotations) {
		tool := &stubTool{name: name}
		if err := w.registry.RegisterWith(tool, origin, ann); err != nil {
			t.Fatalf("RegisterWith(%s): %v", name, err)
		}
		w.byName[name] = tool
	}
	add("read_file", tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.Yes})
	add("load_tool_skill", tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.Yes})
	add("web_search", tools.Origin("research"), tools.Annotations{ReadOnly: mcp.Yes})
	add("jira_lookup", tools.Origin("jira"), tools.Annotations{})
	add("slack_post", tools.Origin("slack"), tools.Annotations{ReadOnly: mcp.No, OpenWorld: mcp.Yes})
	add("delete_page", tools.Origin("wiki"), tools.Annotations{Destructive: mcp.Yes})
	// The engine-control surface a parent's Execute phase really does hold.
	add(subagent.ToolName, tools.OriginBuiltin, tools.Annotations{})
	add("activate_tool", tools.OriginBuiltin, tools.Annotations{})
	add("list_mcp_server_tools", tools.OriginBuiltin, tools.Annotations{})
	add("a2a_ask", tools.OriginBuiltin, tools.Annotations{})
	add("run_sandbox", tools.OriginBuiltin, tools.Annotations{})
	w.snapshot = w.registry.Snapshot()
	return w
}

// parentAll is everything the parent itself may call.
func (w *world) parentAll() []string { return w.snapshot.Names() }

func seat(t *testing.T) prompts.Seat {
	t.Helper()
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	return prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role}
}

func models(t *testing.T, entries ...phase.Entry) *phase.Registry {
	t.Helper()
	reg, err := phase.NewRegistry(entries)
	if err != nil {
		t.Fatalf("phase.NewRegistry: %v", err)
	}
	return reg
}

func limits() subagent.Limits {
	return subagent.Limits{
		MaxTurns: 4, MaxTasksPerCall: 8,
		TaskTimeout: 5 * time.Second, CallTimeout: 10 * time.Second,
		MaxParallel: 3, BudgetFraction: 0.2, MinTokensPerTask: 500,
	}
}

// baseConfig wires a seat, a world and one provider under the "default" key.
func baseConfig(t *testing.T, w *world, p *provider) subagent.Config {
	t.Helper()
	return subagent.Config{
		Seat:     seat(t),
		Models:   models(t, phase.Entry{Key: "default", Provider: p}),
		Universe: w.snapshot,
		Parent:   w.parentAll,
		Limits:   limits(),
	}
}

// request is one ad-hoc task, which is what most cases here need: the
// boundary, the budget and the loop are the same whether the persona came
// from a template or from the call.
func request(toolNames ...string) subagent.Request {
	return subagent.Request{Tasks: []subagent.Task{task("t1", toolNames...)}}
}

// task builds one ad-hoc task with the given tool request.
func task(id string, toolNames ...string) subagent.Task {
	return subagent.Task{
		ID: id, SystemPrompt: "you research things",
		Prompt: "summarise the incident", Tools: toolNames,
	}
}

// run drives a whole call and fails the test if the request was refused.
func run(t *testing.T, cfg subagent.Config, req subagent.Request) []subagent.Result {
	t.Helper()
	results, err := subagent.Run(t.Context(), cfg, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return results
}

// one drives a single-task call and returns that task's result.
func one(t *testing.T, cfg subagent.Config, req subagent.Request) subagent.Result {
	t.Helper()
	return oneOn(t.Context(), t, cfg, req)
}

// oneOn is [one] under a caller-supplied context, for the cases about what a
// torn-down parent does to a task in flight.
func oneOn(ctx context.Context, t *testing.T, cfg subagent.Config, req subagent.Request) subagent.Result {
	t.Helper()
	results, err := subagent.Run(ctx, cfg, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("%d results, want 1", len(results))
	}
	return results[0]
}

// batch builds a many-task call, giving every task the same tool request and
// an id derived from its position.
//
// The old API had one allowlist for a whole batch; the new one is per task,
// which is strictly more expressive. This helper keeps the cases below saying
// what they were about — the budget, the concurrency, the ordering — rather
// than restating a tool list eight times.
func batch(toolNames []string, tasks []subagent.Task) subagent.Request {
	out := make([]subagent.Task, 0, len(tasks))
	for i, t := range tasks {
		if t.ID == "" {
			t.ID = fmt.Sprintf("t%d", i)
		}
		if t.Tools == nil {
			t.Tools = toolNames
		}
		if t.SystemPrompt == "" && t.Worker == "" {
			t.SystemPrompt = "you research things"
		}
		if t.Prompt == "" {
			t.Prompt = "do the thing"
		}
		out = append(out, t)
	}
	return subagent.Request{Tasks: out}
}

// submit is the answer a worker gives when it means to finish.
func submit(fields map[string]any, in, out int) *llm.Completion {
	return callTool(subagent.SubmitTool, fields, in, out)
}

// answer is the ordinary submission: one `result` field.
func answer(text string, in, out int) *llm.Completion {
	return submit(map[string]any{"result": text}, in, out)
}

// --- the grant is a security boundary --------------------------------------

func TestAGrantIsBoundedByTheParentsOwnTools(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// The parent holds read_file but NOT web_search, so a child asking for
	// both may have one. Without the parent bound a child would reach past
	// its spawner into the whole registry.
	g := subagent.Permit(w.snapshot, []string{"read_file"}, []string{"read_file", "web_search"})

	if !slices.Contains(g.Active, "read_file") {
		t.Errorf("a tool the parent holds was not granted: %v", g.Active)
	}
	if slices.Contains(g.Active, "web_search") {
		t.Errorf("a tool the parent LACKS was granted: %v", g.Active)
	}
	if !slices.Contains(g.Rejected, "web_search") {
		t.Errorf("the refusal was not reported: %v", g.Rejected)
	}
	// Reachability, not just the offered list: discovery resolves against
	// this snapshot, so a name left in it is a name a child can promote.
	if _, ok := g.Universe.Lookup("web_search"); ok {
		t.Error("a tool the parent lacks is still discoverable by the child")
	}
	if _, ok := g.Universe.Lookup("read_file"); !ok {
		t.Error("a granted tool is missing from the child's universe")
	}
}

func TestAGrantRefusesTheEngineControlDenylist(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	denied := []string{
		subagent.ToolName, "run_sandbox", "activate_tool",
		"list_mcp_server_tools", "a2a_ask",
	}
	// The parent holds every one of these; the request names every one.
	g := subagent.Permit(w.snapshot, w.parentAll(), append(slices.Clone(denied), "read_file"))

	for _, name := range denied {
		if slices.Contains(g.Active, name) {
			t.Errorf("%s was granted to a sub-agent", name)
		}
		if !slices.Contains(g.Rejected, name) {
			t.Errorf("%s was dropped without being reported", name)
		}
		if _, ok := g.Universe.Lookup(name); ok {
			t.Errorf("%s is still discoverable by the child", name)
		}
		if !subagent.Denied(name) {
			t.Errorf("Denied(%q) says otherwise", name)
		}
	}
	// The counterfactual: the same call still grants what is allowed, so
	// this is a filter and not a blanket refusal.
	if !slices.Contains(g.Active, "read_file") {
		t.Errorf("a legitimate tool was lost alongside the denied ones: %v", g.Active)
	}
	if subagent.Denied("read_file") {
		t.Error("Denied says an ordinary tool is engine control")
	}
}

func TestAGrantRefusesToolsThatWriteToASharedSurface(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	g := subagent.Permit(w.snapshot, w.parentAll(),
		[]string{"slack_post", "delete_page", "web_search", "jira_lookup"})

	for _, name := range []string{"slack_post", "delete_page"} {
		if slices.Contains(g.Active, name) {
			t.Errorf("%s writes to a shared surface and was granted", name)
		}
		if _, ok := g.Universe.Lookup(name); ok {
			t.Errorf("%s is still discoverable by the child", name)
		}
	}
	// A PROVEN read is granted, and so is an UNANNOTATED tool: the filter is
	// deliberately conservative about unknown, because the parent's explicit
	// allowlist is what curates that case and blocking it would make most of
	// a fresh MCP server unusable to a sub-agent.
	for _, name := range []string{"web_search", "jira_lookup"} {
		if !slices.Contains(g.Active, name) {
			t.Errorf("%s should have been granted: active=%v rejected=%v",
				name, g.Active, g.Rejected)
		}
	}
}

func TestTheSkillLoaderRidesAlongUnrequested(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// Unrequested, but the child's own prompt tells it to call this.
	g := subagent.Permit(w.snapshot, w.parentAll(), []string{"read_file"})
	if !slices.Contains(g.Active, "load_tool_skill") {
		t.Errorf("the skill loader was not granted: %v", g.Active)
	}
	// The counterfactual: it is not an exemption from the parent bound. A
	// parent that cannot load skills does not grant the ability to.
	g = subagent.Permit(w.snapshot, []string{"read_file"}, []string{"read_file"})
	if slices.Contains(g.Active, "load_tool_skill") {
		t.Errorf("the skill loader was granted past the parent bound: %v", g.Active)
	}
}

func TestARepeatedRequestIsGrantedOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	g := subagent.Permit(w.snapshot, w.parentAll(),
		[]string{"read_file", "read_file", "slack_post", "slack_post"})

	if got := slices.Clone(g.Active); len(got) != 2 {
		// read_file plus the skill loader, once each: two ToolDefs with one
		// name is a request some vendors refuse outright.
		t.Errorf("duplicates reached the offered list: %v", got)
	}
	if n := strings.Count(strings.Join(g.Rejected, ","), "slack_post"); n != 1 {
		t.Errorf("one refusal was reported %d times: %v", n, g.Rejected)
	}
}

func TestDiscoveryCannotReachWhatTheGrantRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// The child is granted read_file only, and then tries to widen itself:
	// once onto a tool the parent holds and the grant allows (legal), and
	// once onto each class the grant refuses.
	targets := []string{"web_search", "slack_post", subagent.ToolName, "activate_tool"}
	p := &provider{name: "sub", reply: func(_ context.Context, n int, _ llm.Request) (*llm.Completion, error) {
		if n <= len(targets) {
			return callTool("child_activate", map[string]any{"name": targets[n-1]}, 1, 1), nil
		}
		return callTool("slack_post", map[string]any{"text": "hi"}, 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Discovery = discovery
	cfg.Limits.MaxTurns = len(targets) + 1

	res := one(t, cfg, request("read_file"))

	outcome := map[string]toolloop.Execution{}
	for _, e := range res.Executions {
		if name, _ := e.Args["name"].(string); name != "" {
			outcome[name] = e
		}
	}
	if e, ok := outcome["web_search"]; !ok || e.Failed {
		t.Errorf("a legal widening was refused: %+v", e)
	}
	for _, name := range targets[1:] {
		if e, ok := outcome[name]; !ok || !e.Failed {
			t.Errorf("the child activated %s, which the grant refused: %+v", name, e)
		}
	}
	// And the direct call: not offered, not in the universe, never ran.
	if n := w.byName["slack_post"].ran.Load(); n != 0 {
		t.Errorf("slack_post RAN %d times inside a sub-agent", n)
	}
	if !slices.Contains(res.ToolsAvailable, "web_search") {
		t.Errorf("the legal activation did not reach the surface: %v", res.ToolsAvailable)
	}
}

func TestAChildWithNoDiscoveryStaysFrozen(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub"}
	cfg := baseConfig(t, w, p)
	cfg.Discovery = nil

	res := one(t, cfg, request("read_file"))
	offered := p.offered(0)
	if slices.Contains(offered, "child_activate") {
		t.Errorf("a meta-tool was offered with no discovery configured: %v", offered)
	}
	if !slices.Contains(offered, "read_file") {
		t.Errorf("the granted tool was not offered: %v", offered)
	}
	// A catalogue is rendered only for a child that can act on it: listing
	// tools to a worker with no activate is an invitation to call a name it
	// was never offered.
	if strings.Contains(res.SystemPrompt, "## Available tools") {
		t.Error("a frozen child was shown a catalogue it cannot use")
	}
}

func TestADiscoveryCapableChildIsShownTheSafeCatalogue(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub"}
	cfg := baseConfig(t, w, p)
	cfg.Discovery = discovery

	res := one(t, cfg, request("read_file"))
	if !strings.Contains(res.SystemPrompt, "## Available tools") {
		t.Fatalf("no catalogue in a discovery-capable child's prompt:\n%s", res.SystemPrompt)
	}
	if !strings.Contains(res.SystemPrompt, "research") {
		t.Error("the catalogue does not name the MCP server the child may discover")
	}
	// The CATALOGUE section only: the preamble legitimately says "do not
	// delegate further", and matching the whole prompt for the tool's name
	// would read that prohibition as an advertisement.
	// The CATALOGUE SECTION ONLY: it is followed by the preamble, which
	// legitimately says "do not delegate further", and matching to the end
	// of the prompt would read that prohibition as an advertisement.
	catalogue := res.SystemPrompt[strings.Index(res.SystemPrompt, "## Available tools"):]
	catalogue, _, _ = strings.Cut(catalogue, "You are a short-lived worker")
	for _, denied := range []string{"slack_post", subagent.ToolName} {
		if strings.Contains(catalogue, denied) {
			t.Errorf("the catalogue advertises %s, which the worker cannot have", denied)
		}
	}
}

// --- running one child -----------------------------------------------------

func TestSkillsCoverWhatTheChildMayLaterActivate(t *testing.T) {
	t.Parallel()
	// Skill scope is the offered set PLUS the discoverable catalogue. A
	// skill covering a tool the child activates mid-run has to be in the
	// prompt BEFORE the first call — a required skill discovered only when
	// the guard blocks the call costs a round and, on a one-round child,
	// the whole spawn.
	w := newWorld(t)
	p := &provider{name: "sub"}
	cfg := baseConfig(t, w, p)
	cfg.Discovery = discovery
	cfg.Skills = skillFor{tool: "web_search", key: "research-etiquette"}

	res := one(t, cfg, request("read_file"))
	if !strings.Contains(res.SystemPrompt, "research-etiquette") {
		t.Errorf("a skill for a discoverable tool was left out:\n%s", res.SystemPrompt)
	}

	// The counterfactual: a skill for a tool the grant refuses stays out,
	// so this is scope and not "every skill in the registry".
	cfg.Skills = skillFor{tool: "slack_post", key: "posting-etiquette"}
	res = one(t, cfg, request("read_file"))
	if strings.Contains(res.SystemPrompt, "posting-etiquette") {
		t.Errorf("a skill for a denied tool reached the prompt:\n%s", res.SystemPrompt)
	}
}

func TestASubagentRunsOnTheSeatsSubagentChain(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	main := &provider{name: "main"}
	sub := &provider{name: "sub"}
	cfg := baseConfig(t, w, sub)
	cfg.Models = models(t,
		phase.Entry{Key: "main-model", Provider: main},
		phase.Entry{Key: "sub-model", Provider: sub})
	cfg.Seat.Role.LLM = org.ProviderKeys{"main-model"}
	cfg.Seat.Role.LLMSubagent = org.ProviderKeys{"sub-model"}

	res := one(t, cfg, request("read_file"))
	if sub.count() != 1 || main.count() != 0 {
		t.Errorf("llm_subagent was not used: sub=%d main=%d", sub.count(), main.count())
	}
	if res.ProviderKey != "sub-model" {
		t.Errorf("ProviderKey = %q, want sub-model", res.ProviderKey)
	}

	// The counterfactual: with no llm_subagent the seat's own chain answers,
	// so the assertion above is about the phase and not about ordering.
	cfg.Seat.Role.LLMSubagent = nil
	one(t, cfg, request("read_file"))
	if main.count() != 1 {
		t.Errorf("without llm_subagent the seat's chain did not answer: main=%d", main.count())
	}
}

func TestAChildCannotCallAToolItWasNotGranted(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(_ context.Context, n int, _ llm.Request) (*llm.Completion, error) {
		switch n {
		case 1:
			return callTool("slack_post", map[string]any{"text": "leaked"}, 1, 1), nil
		case 2:
			return callTool("read_file", map[string]any{"path": "/x"}, 1, 1), nil
		}
		return answer("done", 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)

	res := one(t, cfg, request("read_file", "slack_post"))
	if n := w.byName["slack_post"].ran.Load(); n != 0 {
		t.Fatalf("a denied tool ran %d times", n)
	}
	if n := w.byName["read_file"].ran.Load(); n != 1 {
		t.Errorf("the granted tool ran %d times, want 1", n)
	}
	if len(res.Executions) < 2 || !res.Executions[0].Failed {
		t.Fatalf("the denied call was not recorded as a failure: %+v", res.Executions)
	}
	if res.Executions[1].Failed {
		t.Errorf("the granted call failed: %+v", res.Executions[1])
	}
	if !slices.Contains(res.Rejected, "slack_post") {
		t.Errorf("the rejection is not on the record: %v", res.Rejected)
	}
}

func TestAParentAskingForMoreTurnsIsClampedNotRefused(t *testing.T) {
	t.Parallel()
	always := func(_ context.Context, _ int, _ llm.Request) (*llm.Completion, error) {
		return callTool("read_file", map[string]any{"path": "/x"}, 1, 1), nil
	}

	for _, tc := range []struct {
		name      string
		requested int
		want      int
	}{
		{"above the cap is clamped down", 99, 3},
		{"unspecified takes the cap", 0, 3},
		{"below the cap is honoured", 1, 1},
		{"nonsense takes the cap", -4, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &provider{name: "sub", reply: always}
			cfg := baseConfig(t, newWorld(t), p)
			cfg.Limits.MaxTurns = 3

			req := request("read_file")
			req.Tasks[0].MaxTurns = tc.requested
			res := one(t, cfg, req)
			if p.count() != tc.want || res.Rounds != tc.want {
				t.Errorf("rounds: provider=%d result=%d, want %d",
					p.count(), res.Rounds, tc.want)
			}
			// CLAMPED, NOT REFUSED: the task ran, and it ended having
			// used up its rounds without answering rather than being
			// turned away for asking. `no_result` is the honest word for
			// that; a refusal would have cost the whole call.
			if res.Status != subagent.StatusNoResult {
				t.Errorf("a clamped request was not run: %+v", res)
			}
		})
	}
}

func TestATimedOutChildReportsWhatItAlreadyDid(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(ctx context.Context, n int, _ llm.Request) (*llm.Completion, error) {
		if n == 1 {
			return callTool("read_file", map[string]any{"path": "/x"}, 60, 40), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	cfg := baseConfig(t, w, p)
	cfg.Limits.TaskTimeout = 60 * time.Millisecond

	res := one(t, cfg, request("read_file"))
	if !res.Failed() || !res.TimedOut() || res.Status != subagent.StatusTimedOut {
		t.Fatalf("a timed-out child was not reported as one: %+v", res)
	}
	if !strings.Contains(res.Error, "wall-clock") {
		t.Errorf("the reason does not say what expired: %q", res.Error)
	}
	// The partial work is the point: a child that spent a round did work the
	// parent paid for, and reporting zeros throws away both the transcript
	// and the only evidence of the cost.
	if res.Rounds != 1 || res.Tokens() != 100 {
		t.Errorf("partial work lost: rounds=%d tokens=%d", res.Rounds, res.Tokens())
	}
	if len(res.Executions) != 1 {
		t.Errorf("the tool the child ran is missing: %+v", res.Executions)
	}
	if res.SystemPrompt == "" || res.UserPrompt == "" {
		t.Error("a cut-off child left no prompt record for the phase event")
	}
}

func TestAFastChildIsNotReportedAsTimedOut(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		return answer("all done", 10, 5), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Limits.TaskTimeout = 5 * time.Second

	res := one(t, cfg, request("read_file"))
	if res.Failed() || res.TimedOut() || res.Status != subagent.StatusOK {
		t.Fatalf("a healthy worker was flagged: %+v", res)
	}
	if res.Output["result"] != "all done" || res.Tokens() != 15 {
		t.Errorf("result = %+v / %d tokens", res.Output, res.Tokens())
	}
}

func TestAPanickingChildDoesNotTakeTheParentDown(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		panic("provider SDK dereferenced nil")
	}}
	cfg := baseConfig(t, w, p)

	res := one(t, cfg, request("read_file"))
	if !res.Failed() || res.Status != subagent.StatusFailed {
		t.Fatalf("a panic was not contained as a failure: %+v", res)
	}
	if !strings.Contains(res.Error, "dereferenced nil") {
		t.Errorf("the panic's message was lost: %q", res.Error)
	}
	if res.TimedOut() {
		t.Error("a panic was reported as a timeout")
	}
}

func TestAPanickingChildDoesNotTakeItsSiblingsDown(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(_ context.Context, n int, req llm.Request) (*llm.Completion, error) {
		if strings.Contains(userText(req), "poison") {
			panic("poison child")
		}
		return answer("fine", 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0

	results, err := subagent.Run(context.Background(), cfg, batch([]string{"read_file"}, []subagent.Task{
		{Prompt: "healthy one", SystemPrompt: "s"},
		{Prompt: "poison", SystemPrompt: "s"},
		{Prompt: "healthy two", SystemPrompt: "s"},
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Failed() || results[2].Failed() {
		t.Errorf("a panicking sibling took healthy children with it: %+v", results)
	}
	if results[1].Status != subagent.StatusFailed {
		t.Errorf("the panicking child = %+v", results[1])
	}
}

func TestACancelledParentIsNotReportedAsATimeout(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	started := make(chan struct{})
	p := &provider{name: "sub", reply: func(ctx context.Context, _ int, _ llm.Request) (*llm.Completion, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	cfg := baseConfig(t, w, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-started; cancel() }()
	res := oneOn(ctx, t, cfg, request("read_file"))
	// A torn-down turn is not an exceeded cap. A planner told "timed out"
	// helpfully retries with a smaller task against an engine that is
	// shutting down.
	if res.Status != subagent.StatusCancelled || res.TimedOut() {
		t.Fatalf("cancellation reported as %+v", res)
	}
}

// --- the budget slice ------------------------------------------------------

func TestTheSliceIsAFractionOfTheParentsRemaining(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	m := &meter{}
	p := &provider{name: "sub", reply: func(_ context.Context, n int, _ llm.Request) (*llm.Completion, error) {
		if n == 1 {
			return callTool("read_file", map[string]any{"path": "/x"}, 100, 50), nil
		}
		return answer("more", 60, 40), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Budget = countingMeter{m}
	cfg.ParentRemaining = 1000 // slice = 200
	// THE FLOOR IS A DIFFERENT RULE, and it applies to a call of one like
	// any other. This case is about the slice, so the floor is off.
	cfg.Limits.MinTokensPerTask = 0

	res := one(t, cfg, request("read_file"))
	if !res.Failed() || res.Status != subagent.StatusBudget {
		t.Fatalf("the slice did not stop the child: %+v", res)
	}
	if m.sum() != 150 {
		t.Errorf("the parent counter was charged %d, want only the round that fit (150)", m.sum())
	}

	// The counterfactual: a parent with no per-seat cap imposes none on its
	// child, so the same script runs to the end.
	m2 := &meter{}
	p2 := &provider{name: "sub", reply: p.reply}
	cfg2 := baseConfig(t, newWorld(t), p2)
	cfg2.Budget = countingMeter{m2}
	cfg2.ParentRemaining = 0
	res2 := one(t, cfg2, request("read_file"))
	if res2.Failed() {
		t.Fatalf("an uncapped parent's child was refused: %+v", res2)
	}
	if m2.sum() != 250 {
		t.Errorf("uncapped charge = %d, want 250", m2.sum())
	}
}

func TestABatchSharesOneSliceRatherThanOnePerChild(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	m := &meter{}
	p := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		return answer("answer", 100, 50), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Budget = countingMeter{m}
	cfg.ParentRemaining = 1000 // total slice = 200, one 150-token child fits
	cfg.Limits.MinTokensPerTask = 0

	results, err := subagent.Run(context.Background(), cfg, batch([]string{"read_file"}, []subagent.Task{
		{Prompt: "a", SystemPrompt: "s"},
		{Prompt: "b", SystemPrompt: "s"},
		{Prompt: "c", SystemPrompt: "s"},
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A per-child wrapper would have let all three spend 150 — 450 against
	// a configured slice of 200, which is the fan-out cost being invisible
	// until the org budget is gone.
	if m.sum() > 200 {
		t.Fatalf("the batch charged %d against a 200-token slice", m.sum())
	}
	var ok int
	for _, r := range results {
		if !r.Failed() {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("%d children finished on a slice that fits one: %+v", ok, results)
	}
	for _, r := range results {
		if r.Failed() && r.Status != subagent.StatusBudget {
			t.Errorf("a starved worker reported %q: %+v", r.Status, r)
		}
	}
}

func TestABatchIsRefusedWhenTheSliceCannotFeedEveryChild(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub"}
	cfg := baseConfig(t, w, p)
	cfg.Budget = countingMeter{&meter{}}
	cfg.ParentRemaining = 1000 // slice = 200
	cfg.Limits.MinTokensPerTask = 500

	tasks := []subagent.Task{
		{Prompt: "a", SystemPrompt: "s"},
		{Prompt: "b", SystemPrompt: "s"},
	}
	results, err := subagent.Run(context.Background(), cfg,
		batch([]string{"read_file"}, tasks))

	var refused *subagent.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a RefusedError", err)
	}
	if refused.Slice != 200 || refused.MinPerTask != 500 || refused.Tasks != 2 {
		t.Errorf("the refusal does not carry the numbers: %+v", refused)
	}
	if results != nil {
		t.Errorf("a refused batch returned results: %+v", results)
	}
	// UP FRONT: nothing ran, so nothing was paid for.
	if p.count() != 0 {
		t.Errorf("a refused batch called the model %d times", p.count())
	}

	// PER CHILD, not per batch: a slice that clears the floor once still
	// cannot feed two children, and comparing the total against a single
	// child's floor is how a batch gets started with everyone too poor to
	// finish.
	cfg.Limits.MinTokensPerTask = 150 // 200 >= 150, but 200 < 2*150
	if _, err := subagent.Run(context.Background(), cfg,
		batch([]string{"read_file"}, tasks)); !errors.As(err, &refused) {
		t.Fatalf("a slice that feeds one of two children was accepted: %v", err)
	}
	if p.count() != 0 {
		t.Errorf("a refused batch called the model %d times", p.count())
	}

	// The counterfactual: a floor the slice can meet lets the same batch run.
	cfg.Limits.MinTokensPerTask = 50
	if _, err := subagent.Run(context.Background(), cfg,
		batch([]string{"read_file"}, tasks)); err != nil {
		t.Fatalf("a fundable batch was refused: %v", err)
	}
	if p.count() == 0 {
		t.Error("a fundable batch never reached the model")
	}
}

func TestAnUncappedParentSkipsTheFloorEntirely(t *testing.T) {
	t.Parallel()
	// There is no total to divide, so there is nothing to compare the floor
	// against. Refusing here would make a seat with no budget unable to fan
	// out at all.
	w := newWorld(t)
	p := &provider{name: "sub"}
	cfg := baseConfig(t, w, p)
	cfg.Budget = countingMeter{&meter{}}
	cfg.ParentRemaining = 0
	cfg.Limits.MinTokensPerTask = 100000

	if _, err := subagent.Run(context.Background(), cfg,
		batch([]string{"read_file"}, []subagent.Task{{Prompt: "a"}})); err != nil {
		t.Fatalf("an uncapped parent's call was refused: %v", err)
	}
	if p.count() == 0 {
		t.Error("the call never reached the model")
	}
}

func TestConcurrentChildrenCannotOvershootTheSlice(t *testing.T) {
	t.Parallel()
	// The check and the reservation must be ONE operation. Two children
	// testing against the same `used` snapshot both pass and both spend,
	// overshooting by a whole child — and the inner charge is exactly where
	// that window opens, because it can block.
	w := newWorld(t)
	m := &meter{}
	release := make(chan struct{})
	slow := blockingMeter{inner: countingMeter{m}, gate: release}
	p := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		return answer("answer", 60, 0), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Budget = slow
	cfg.ParentRemaining = 500 // slice = 100, so exactly one 60-token child fits
	cfg.Limits.MinTokensPerTask = 0
	cfg.Limits.MaxParallel = 8

	done := make(chan []subagent.Result, 1)
	go func() {
		var tasks []subagent.Task
		for i := range 8 {
			tasks = append(tasks, subagent.Task{
				Prompt: fmt.Sprintf("t%d", i), SystemPrompt: "s",
			})
		}
		res, err := subagent.Run(context.Background(), cfg,
			batch([]string{"read_file"}, tasks))
		if err != nil {
			t.Errorf("SpawnBatch: %v", err)
		}
		done <- res
	}()
	// Let every child pile up inside the inner charge before any returns.
	time.Sleep(30 * time.Millisecond)
	close(release)
	results := <-done

	if m.sum() > 100 {
		t.Fatalf("charged %d against a 100-token slice", m.sum())
	}
	var ok int
	for _, r := range results {
		if !r.Failed() {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("%d of 8 children fitted a one-child slice", ok)
	}
}

func TestAnOrgRefusalIsNotBlamedOnTheChildsSlice(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	m := &meter{refuse: true}
	p := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		return answer("answer", 10, 0), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Budget = countingMeter{m}
	cfg.ParentRemaining = 100 // slice = 20, so two 10-token rounds fit
	cfg.Limits.MinTokensPerTask = 0

	res := one(t, cfg, request("read_file"))
	// The ORG refused, so the child's own kind must not claim its slice ran
	// out — that sends an operator to raise a limit that was never reached.
	if res.Status == subagent.StatusBudget {
		t.Errorf("an org refusal was reported as the sub-agent's own slice: %+v", res)
	}
	if !res.Failed() {
		t.Errorf("a refused charge did not stop the child: %+v", res)
	}
}

func TestARefusedChargeGivesTheReservationBack(t *testing.T) {
	t.Parallel()
	// A refusal is DEFINITE: nothing was charged. Keeping the reservation
	// would shrink the slice for every sibling over a charge that never
	// happened — which is the opposite polarity from an unreachable
	// counter, where the charge may well have landed.
	//
	// Observable only through a sibling: the second child fits the slice
	// exactly when the first child's refused reservation came back.
	w := newWorld(t)
	m := &meter{}
	var once sync.Once
	gate := refuseOnceMeter{inner: countingMeter{m}, once: &once}
	p := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		return answer("answer", 10, 0), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Budget = gate
	cfg.ParentRemaining = 75 // slice = 15: one 10-token child, not two
	cfg.Limits.MinTokensPerTask = 0
	cfg.Limits.MaxParallel = 1

	results, err := subagent.Run(context.Background(), cfg, batch([]string{"read_file"}, []subagent.Task{
		{Prompt: "a", SystemPrompt: "s"},
		{Prompt: "b", SystemPrompt: "s"},
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var ok, failed int
	for _, r := range results {
		if r.Failed() {
			failed++
			if r.Status == subagent.StatusBudget {
				t.Errorf("the survivor was refused by the slice, not the org: %+v", r)
			}
			continue
		}
		ok++
	}
	if ok != 1 || failed != 1 {
		t.Fatalf("ok=%d failed=%d, want 1 and 1: %+v", ok, failed, results)
	}
	if m.sum() != 10 {
		t.Errorf("the parent counter was charged %d, want 10", m.sum())
	}
}

// --- batch mechanics -------------------------------------------------------

func TestABatchRunsAtMostMaxParallelChildrenAtOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var live, peak atomic.Int32
	p := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		cur := live.Add(1)
		for {
			was := peak.Load()
			if cur <= was || peak.CompareAndSwap(was, cur) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		live.Add(-1)
		return answer("answer", 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Limits.MaxParallel = 2
	cfg.ParentRemaining = 0

	var tasks []subagent.Task
	for i := range 6 {
		tasks = append(tasks, subagent.Task{Prompt: fmt.Sprintf("t%d", i), SystemPrompt: "s"})
	}
	results, err := subagent.Run(context.Background(), cfg,
		batch([]string{"read_file"}, tasks))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := peak.Load(); got > 2 {
		t.Errorf("peak concurrency %d exceeds MaxParallel 2", got)
	} else if got < 2 {
		// The counterfactual for a cap that is really just serialisation.
		t.Errorf("peak concurrency %d — the children never overlapped", got)
	}
	// Children beyond the cap run as earlier ones finish, rather than being
	// dropped.
	if len(results) != 6 {
		t.Fatalf("%d results for 6 tasks", len(results))
	}
	for i, r := range results {
		if r.Failed() {
			t.Errorf("child %d failed: %+v", i, r)
		}
	}
}

func TestABatchTimeoutKeepsTheAnswersThatAlreadyLanded(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(ctx context.Context, _ int, req llm.Request) (*llm.Completion, error) {
		if strings.Contains(userText(req), "slow") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return answer("fast answer", 7, 3), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0
	cfg.Limits.MaxParallel = 2
	cfg.Limits.TaskTimeout = 10 * time.Second // the BATCH cap is what must fire
	cfg.Limits.CallTimeout = 60 * time.Millisecond

	results, err := subagent.Run(context.Background(), cfg, batch([]string{"read_file"}, []subagent.Task{
		{Prompt: "quick one", SystemPrompt: "s"},
		{Prompt: "slow one", SystemPrompt: "s"},
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Discarding a finished child's answer throws away work that completed
	// and was paid for: the tokens are spent either way.
	if results[0].Failed() || results[0].Output["result"] != "fast answer" || results[0].Tokens() != 10 {
		t.Errorf("a finished child's answer was lost: %+v", results[0])
	}
	if !results[1].TimedOut() || !strings.Contains(results[1].Error, "delegate call") {
		t.Errorf("the straggler was not attributed to the call cap: %+v", results[1])
	}
}

func TestAChildBeyondMaxParallelSaysItNeverStarted(t *testing.T) {
	t.Parallel()
	// REPEATED, because the property is about a coin flip. The waiting child
	// is blocked in a select whose two cases — the freed slot and the batch
	// deadline — become ready at the same instant, since the slot is freed
	// BY the deadline killing the child ahead of it. A single run passes
	// about half the time with the post-acquire re-check removed, which
	// reads as flakiness rather than as the race it is; twelve runs make it
	// a fact.
	const runs = 12
	for range runs {
		w := newWorld(t)
		p := &provider{name: "sub", reply: func(ctx context.Context, _ int, _ llm.Request) (*llm.Completion, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		cfg := baseConfig(t, w, p)
		cfg.ParentRemaining = 0
		cfg.Limits.MaxParallel = 1
		cfg.Limits.TaskTimeout = 10 * time.Second
		cfg.Limits.CallTimeout = 20 * time.Millisecond

		results, err := subagent.Run(context.Background(), cfg, batch([]string{"read_file"}, []subagent.Task{
			{Prompt: "a", SystemPrompt: "s"},
			{Prompt: "b", SystemPrompt: "s"},
		}))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		// WHICH child wins the single slot is itself a coin flip, so the
		// property is about the count, not the index.
		var queued int
		for _, r := range results {
			if !strings.HasPrefix(r.Error, "never started") {
				continue
			}
			queued++
			// "Never started" is the one failure a planner can retry
			// unchanged, unlike a child that burned its budget.
			// ITS OWN STATUS, not a timeout: nothing this worker did ran
			// out of time, and a parent told "timed out" retries with a
			// smaller task when the answer is to retry this one unchanged.
			if r.Status != subagent.StatusNeverStarted || !r.Failed() || r.TimedOut() {
				t.Errorf("a queued worker was not reported as never started: %+v", r)
			}
			if r.Rounds != 0 || r.SystemPrompt != "" {
				t.Errorf("a child that never started left a record of running: %+v", r)
			}
		}
		if queued != 1 {
			t.Fatalf("%d of 2 children never started, want exactly 1: %+v", queued, results)
		}
		if p.count() != 1 {
			t.Fatalf("%d children reached the model under MaxParallel 1", p.count())
		}
	}
}

func TestAnUnreachableCounterKeepsItsReservation(t *testing.T) {
	t.Parallel()
	// An unreachable counter does not say whether the charge LANDED, so the
	// slice keeps the reservation. Releasing it would let a sibling spend
	// tokens that may already be billed — and the budget's polarity in this
	// engine is fail-closed.
	w := newWorld(t)
	m := &meter{}
	var once sync.Once
	failing := errOnceMeter{inner: countingMeter{m}, once: &once}
	p := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		return answer("answer", 60, 0), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Budget = failing
	cfg.ParentRemaining = 500 // slice = 100, so 60 + 60 does not fit
	cfg.Limits.MinTokensPerTask = 0
	cfg.Limits.MaxParallel = 1

	results, err := subagent.Run(context.Background(), cfg, batch([]string{"read_file"}, []subagent.Task{
		{Prompt: "a", SystemPrompt: "s"},
		{Prompt: "b", SystemPrompt: "s"},
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Whichever child hit the broken counter, the OTHER must find the slice
	// already spoken for: a released reservation would let it through.
	var budget, unreachable int
	for _, r := range results {
		switch {
		case r.Status == subagent.StatusBudget:
			budget++
		case r.Failed():
			unreachable++
		}
	}
	if unreachable != 1 || budget != 1 {
		t.Fatalf("unreachable=%d budget=%d, want 1 and 1: %+v", unreachable, budget, results)
	}
	if m.sum() != 0 {
		t.Errorf("the parent counter was charged %d by a batch that never got through", m.sum())
	}
}

func TestAPerChildTimeoutDoesNotFailTheWholeBatch(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(ctx context.Context, _ int, req llm.Request) (*llm.Completion, error) {
		if strings.Contains(userText(req), "hang") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return answer("answer", 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0
	cfg.Limits.TaskTimeout = 50 * time.Millisecond
	cfg.Limits.CallTimeout = 10 * time.Second

	start := time.Now()
	results, err := subagent.Run(context.Background(), cfg, batch([]string{"read_file"}, []subagent.Task{
		{Prompt: "hang here", SystemPrompt: "s"},
		{Prompt: "fine", SystemPrompt: "s"},
		{Prompt: "also fine", SystemPrompt: "s"},
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the batch waited out its own cap (%v) for one straggler", elapsed)
	}
	if !results[0].TimedOut() || strings.Contains(results[0].Error, "batch") {
		t.Errorf("the straggler was not attributed to its own cap: %+v", results[0])
	}
	for _, i := range []int{1, 2} {
		if results[i].Failed() {
			t.Errorf("child %d failed because a sibling hung: %+v", i, results[i])
		}
	}
}

func TestResultsComeBackInInputOrder(t *testing.T) {
	t.Parallel()
	// The parent's model wrote the tasks as a list and reads the answers as
	// one; completion order would silently re-pair every answer with the
	// wrong question.
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(_ context.Context, _ int, req llm.Request) (*llm.Completion, error) {
		body := userText(req)
		switch body {
		case "first":
			time.Sleep(40 * time.Millisecond)
		case "second":
			time.Sleep(20 * time.Millisecond)
		}
		return answer("answer to "+body, 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0
	cfg.Limits.MaxParallel = 3

	results, err := subagent.Run(context.Background(), cfg, batch([]string{"read_file"}, []subagent.Task{
		{Prompt: "first", SystemPrompt: "s"},
		{Prompt: "second", SystemPrompt: "s"},
		{Prompt: "third", SystemPrompt: "s"},
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, want := range []string{"answer to first", "answer to second", "answer to third"} {
		if results[i].Output["result"] != want {
			t.Errorf("results[%d] = %+v, want %q", i, results[i].Output, want)
		}
	}
}

func TestABatchPublishesOneSummary(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(_ context.Context, _ int, req llm.Request) (*llm.Completion, error) {
		if strings.Contains(userText(req), "bad") {
			panic("nope")
		}
		return answer("answer", 4, 6), nil
	}}
	pub := &publisher{}
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0
	cfg.Publisher = pub
	cfg.Trace = events.NewTrace()

	if _, err := subagent.Run(context.Background(), cfg, batch([]string{"read_file"}, []subagent.Task{
		{Prompt: "good", SystemPrompt: "s"},
		{Prompt: "bad", SystemPrompt: "s"},
	})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if pub.count() != 1 {
		t.Fatalf("%d events published for one batch", pub.count())
	}
	ev := pub.events[0]
	if ev.Type != "subagent_batched" {
		t.Errorf("event type = %q", ev.Type)
	}
	if pub.topics[0] != "crewlet.events.subagent_batched" {
		t.Errorf("topic = %q", pub.topics[0])
	}
	if ev.TraceID != cfg.Trace.TraceID {
		t.Error("the summary does not hang under the parent turn's trace")
	}
	// The payload carries no role, so the envelope's source is the only
	// attribution: without it every fan-out renders as "system".
	if ev.Actor() != "CTO" {
		t.Errorf("actor = %q, want CTO", ev.Actor())
	}
	blob, err := json.Marshal(ev.Data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		TaskCount   int `json:"task_count"`
		Successes   int `json:"successes"`
		Failures    int `json:"failures"`
		TotalTokens int `json:"total_tokens"`
	}
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TaskCount != 2 || got.Successes != 1 || got.Failures != 1 || got.TotalTokens != 10 {
		t.Errorf("summary = %+v", got)
	}
}

func TestAPublisherFailureDoesNotFailTheBatch(t *testing.T) {
	t.Parallel()
	// The children have already run and their results ARE the parent's
	// answer; a broker refusing telemetry must not turn a finished batch
	// into a failed tool call.
	w := newWorld(t)
	p := &provider{name: "sub"}
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0
	cfg.Publisher = &publisher{err: errors.New("broker down")}

	results, err := subagent.Run(context.Background(), cfg,
		batch([]string{"read_file"}, []subagent.Task{{Prompt: "a"}}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 || results[0].Status != subagent.StatusOK {
		t.Errorf("results = %+v", results)
	}
}

// --- refusals that mean nothing ran ----------------------------------------

func TestZeroLimitsAreRefusedRatherThanDefaulted(t *testing.T) {
	t.Parallel()
	// A Go zero is not "unset" here — it is the most destructive possible
	// setting. Naming the field is the whole value of refusing.
	w := newWorld(t)
	for _, tc := range []struct {
		name  string
		mutin func(*subagent.Limits)
		want  string
	}{
		{"no rounds", func(l *subagent.Limits) { l.MaxTurns = 0 }, "MaxTurns"},
		{"no tasks per call", func(l *subagent.Limits) { l.MaxTasksPerCall = 0 }, "MaxTasksPerCall"},
		{"expired task deadline", func(l *subagent.Limits) { l.TaskTimeout = 0 }, "TaskTimeout"},
		{"expired call deadline", func(l *subagent.Limits) { l.CallTimeout = 0 }, "CallTimeout"},
		{"no concurrency", func(l *subagent.Limits) { l.MaxParallel = 0 }, "MaxParallel"},
		{"no fraction", func(l *subagent.Limits) { l.BudgetFraction = 0 }, "BudgetFraction"},
		{"fraction above one", func(l *subagent.Limits) { l.BudgetFraction = 1.5 }, "BudgetFraction"},
		{"negative floor", func(l *subagent.Limits) { l.MinTokensPerTask = -1 }, "MinTokensPerTask"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &provider{name: "sub"}
			cfg := baseConfig(t, w, p)
			tc.mutin(&cfg.Limits)
			_, err := subagent.Run(t.Context(), cfg, request("read_file"))
			if err == nil {
				t.Fatal("a zero cap was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not name the field: %v", err)
			}
			if p.count() != 0 {
				t.Error("a refused call still reached the model")
			}
		})
	}
	// The counterfactual: valid limits are accepted, so this is validation
	// and not a permanently closed door.
	p := &provider{name: "sub"}
	cfg := baseConfig(t, w, p)
	one(t, cfg, request("read_file"))
}

// A MALFORMED GRAPH MUST NOT RUN HALF OF ITSELF. Starting three tasks and
// then discovering the fourth names a cycle has already spent three workers'
// tokens on work whose consumer will never execute.
func TestAMalformedGraphIsRefusedBeforeAnythingRuns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, tc := range []struct {
		name  string
		tasks []subagent.Task
		want  string
	}{
		{"no tasks", nil, "at least one"},
		{"no id",
			[]subagent.Task{{Prompt: "p", SystemPrompt: "s"}}, "no `id`"},
		{"no prompt",
			[]subagent.Task{{ID: "a", SystemPrompt: "s"}}, "no `prompt`"},
		{"neither worker nor prompt",
			[]subagent.Task{{ID: "a", Prompt: "p"}}, "neither `worker` nor `system_prompt`"},
		{"both worker and prompt",
			[]subagent.Task{{ID: "a", Prompt: "p", Worker: "w", SystemPrompt: "s"}}, "not both"},
		{"duplicate ids", []subagent.Task{
			{ID: "a", Prompt: "p", SystemPrompt: "s"},
			{ID: "a", Prompt: "q", SystemPrompt: "s"},
		}, "share the id"},
		{"a dependency that is not in the call", []subagent.Task{
			{ID: "a", Prompt: "p", SystemPrompt: "s", After: []string{"ghost"}},
		}, "not a task in this call"},
		{"a task waiting on itself", []subagent.Task{
			{ID: "a", Prompt: "p", SystemPrompt: "s", After: []string{"a"}},
		}, "lists itself"},
		{"a cycle", []subagent.Task{
			{ID: "a", Prompt: "p", SystemPrompt: "s", After: []string{"b"}},
			{ID: "b", Prompt: "q", SystemPrompt: "s", After: []string{"a"}},
		}, "wait on each other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &provider{name: "sub"}
			cfg := baseConfig(t, w, p)
			_, err := subagent.Run(t.Context(), cfg, subagent.Request{Tasks: tc.tasks})
			if err == nil {
				t.Fatal("a malformed graph was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say what is wrong: %v", err)
			}
			// THE MODEL CAN FIX THIS, so it must reach the model rather
			// than ending the round as an engine error.
			if _, ok := subagent.AsPlanError(err); !ok {
				t.Errorf("the refusal is not a plan error: %T", err)
			}
			if p.count() != 0 {
				t.Error("a refused call still reached the model")
			}
		})
	}
}

// A CYCLE IS NAMED, not merely reported. "There is a cycle" sends the model
// to re-read a graph it just wrote, and it writes the same one again.
func TestACycleNamesTheTasksThatFormIt(t *testing.T) {
	t.Parallel()
	cfg := baseConfig(t, newWorld(t), &provider{name: "sub"})
	_, err := subagent.Run(t.Context(), cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "independent", Prompt: "p", SystemPrompt: "s"},
		{ID: "gather", Prompt: "p", SystemPrompt: "s", After: []string{"report"}},
		{ID: "report", Prompt: "q", SystemPrompt: "s", After: []string{"gather"}},
	}})
	if err == nil {
		t.Fatal("a cycle was accepted")
	}
	for _, want := range []string{"gather", "report"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the cycle members are not named: %v", err)
		}
	}
	// And the task that is NOT in the cycle is not blamed for it.
	if strings.Contains(err.Error(), "independent") {
		t.Errorf("a task outside the cycle was named in it: %v", err)
	}
}

func TestTooManyTasksAreRefusedRatherThanTruncated(t *testing.T) {
	t.Parallel()
	// Refused rather than clamped, because dropping tasks silently hands
	// the parent a report missing answers it is about to act on.
	p := &provider{name: "sub"}
	cfg := baseConfig(t, newWorld(t), p)
	cfg.Limits.MaxTasksPerCall = 2
	var tasks []subagent.Task
	for i := range 3 {
		tasks = append(tasks, subagent.Task{
			ID: fmt.Sprintf("t%d", i), Prompt: "p", SystemPrompt: "s",
		})
	}
	_, err := subagent.Run(t.Context(), cfg, subagent.Request{Tasks: tasks})
	if err == nil || !strings.Contains(err.Error(), "at most 2") {
		t.Fatalf("err = %v, want a refusal naming the cap", err)
	}
	if p.count() != 0 {
		t.Error("a refused call still reached the model")
	}
}

// --- worker templates ------------------------------------------------------

// A TEMPLATE IS A REQUEST, NOT A GRANT. `workers:` is founder-owned Tier B
// config, and Tier B must never be a privilege escalation path: a template
// naming a tool the seat itself lacks has that name rejected like any other.
func TestATemplateCannotWidenWhatTheSeatCanReach(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub"}
	cfg := baseConfig(t, w, p)
	cfg.Parent = func() []string { return []string{"read_file"} }
	cfg.Workers = map[string]config.Worker{"greedy": {
		Description:  "wants everything",
		SystemPrompt: "you research things",
		// web_search is the load-bearing one: it is read-only and not a
		// control tool, so NOTHING but the parent-subset filter stands
		// between the template and it. slack_post and delegate would be
		// denied by their own rules even if the subset check vanished.
		Tools: []string{"read_file", "web_search", "slack_post", subagent.ToolName},
	}}

	res := one(t, cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "a", Worker: "greedy", Prompt: "go"},
	}})
	for _, want := range []string{"web_search", "slack_post", subagent.ToolName} {
		if !slices.Contains(res.Rejected, want) {
			t.Errorf("%q was granted through a template: %v", want, res.Rejected)
		}
	}
	if !slices.Contains(res.ToolsAvailable, "read_file") {
		t.Errorf("the legitimate tool was lost: %v", res.ToolsAvailable)
	}
}

func TestATemplateSuppliesThePersonaAndTheAnswerShape(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var schema map[string]any
	p := &provider{name: "sub", reply: func(_ context.Context, _ int, req llm.Request) (*llm.Completion, error) {
		for _, def := range req.Tools {
			if def.Name == subagent.SubmitTool {
				schema = def.Parameters
			}
		}
		return submit(map[string]any{"verdict": "clean", "confidence": "high"}, 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Workers = map[string]config.Worker{"auditor": {
		Description:  "audits things",
		SystemPrompt: "You are a meticulous auditor.",
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"verdict":    map[string]any{"type": "string"},
				"confidence": map[string]any{"type": "string"},
			},
			"required": []any{"verdict"},
		},
	}}

	res := one(t, cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "a", Worker: "auditor", Prompt: "check the deploy"},
	}})
	if !strings.Contains(res.SystemPrompt, "meticulous auditor") {
		t.Errorf("the template's persona is not in the prompt:\n%s", res.SystemPrompt)
	}
	if res.Status != subagent.StatusOK {
		t.Fatalf("status = %q: %+v", res.Status, res)
	}
	if res.Output["verdict"] != "clean" {
		t.Errorf("the submission did not come back as fields: %+v", res.Output)
	}
	if res.Worker != "auditor" {
		t.Errorf("the result does not name its template: %q", res.Worker)
	}
	// The TEMPLATE's schema, not the default one — that is the whole point
	// of declaring it in config.
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["verdict"]; !ok {
		t.Errorf("submit_result published the default schema, not the template's: %+v", schema)
	}
}

func TestAnUnknownWorkerIsRefusedNamingWhatIsAvailable(t *testing.T) {
	t.Parallel()
	p := &provider{name: "sub"}
	cfg := baseConfig(t, newWorld(t), p)
	cfg.Workers = map[string]config.Worker{
		"researcher": {Description: "reads", SystemPrompt: "s"},
		"auditor":    {Description: "audits", SystemPrompt: "s"},
	}
	_, err := subagent.Run(t.Context(), cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "a", Worker: "reasercher", Prompt: "go"},
	}})
	if err == nil {
		t.Fatal("an unknown worker was accepted")
	}
	// A model that typo'd a name fixes it from the list; one told only
	// "unknown worker" guesses again.
	for _, want := range []string{"reasercher", "researcher", "auditor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if p.count() != 0 {
		t.Error("a refused call still reached the model")
	}
}

func TestATaskOverridesItsTemplate(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		return answer("done", 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Workers = map[string]config.Worker{"researcher": {
		Description: "reads", SystemPrompt: "s", Tools: []string{"read_file", "web_search"},
	}}

	// A NON-NIL tools list REPLACES the template's, including an empty
	// one: "this task needs no tools" is a real thing to say and the only
	// way to say it.
	res := one(t, cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "a", Worker: "researcher", Prompt: "go", Tools: []string{"read_file"}},
	}})
	if slices.Contains(res.ToolsAvailable, "web_search") {
		t.Errorf("the task's narrower list did not replace the template's: %v", res.ToolsAvailable)
	}
	if !slices.Contains(res.ToolsAvailable, "read_file") {
		t.Errorf("the task's own tool was lost: %v", res.ToolsAvailable)
	}

	// And an OMITTED list takes the template's, so the override is a
	// choice rather than a requirement.
	res = one(t, cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "a", Worker: "researcher", Prompt: "go"},
	}})
	if !slices.Contains(res.ToolsAvailable, "web_search") {
		t.Errorf("an omitted list did not take the template's: %v", res.ToolsAvailable)
	}
}

// AN EXPLICIT MODEL GETS NO FALLBACK AND NO SUBSTITUTION. A template or a
// task that named a model named it for a reason, and quietly running
// somewhere else is how a cheap-model decision becomes a frontier bill.
func TestAnExplicitModelIsUsedAndAnUnknownOneIsRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	fast := &provider{name: "fast"}
	slow := &provider{name: "slow"}
	cfg := baseConfig(t, w, slow)
	cfg.Models = models(t,
		phase.Entry{Key: "default", Provider: slow},
		phase.Entry{Key: "fast", Provider: fast})

	res := one(t, cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "a", SystemPrompt: "s", Prompt: "go", Model: "fast"},
	}})
	if res.ProviderKey != "fast" || fast.count() != 1 || slow.count() != 0 {
		t.Errorf("the named model was not used: key=%q fast=%d slow=%d",
			res.ProviderKey, fast.count(), slow.count())
	}

	// An unknown key fails THIS TASK rather than the call: its siblings are
	// running on keys that do resolve, and refusing the whole call would
	// throw away work already in flight.
	results := run(t, cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "good", SystemPrompt: "s", Prompt: "go"},
		{ID: "bad", SystemPrompt: "s", Prompt: "go", Model: "nope"},
	}})
	if results[0].Status != subagent.StatusOK {
		t.Errorf("a healthy sibling was failed by a bad model key: %+v", results[0])
	}
	if results[1].Status != subagent.StatusFailed ||
		!strings.Contains(results[1].Error, "not configured") {
		t.Errorf("an unknown model key was not refused: %+v", results[1])
	}
}

// --- dependency waves ------------------------------------------------------

func TestADependentTaskWaitsAndIsGivenTheAnswer(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var order []string
	var mu sync.Mutex
	var synthesisPrompt string
	p := &provider{name: "sub", reply: func(_ context.Context, _ int, req llm.Request) (*llm.Completion, error) {
		body := userText(req)
		mu.Lock()
		switch {
		case strings.Contains(body, "gather A"):
			order = append(order, "a")
		case strings.Contains(body, "gather B"):
			order = append(order, "b")
		default:
			order = append(order, "synth")
			synthesisPrompt = body
		}
		mu.Unlock()
		if strings.Contains(body, "gather A") {
			return answer("A says yes", 1, 1), nil
		}
		if strings.Contains(body, "gather B") {
			return answer("B says no", 1, 1), nil
		}
		return answer("they disagree", 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)

	results := run(t, cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "synth", SystemPrompt: "s", Prompt: "reconcile them", After: []string{"a", "b"}},
		{ID: "a", SystemPrompt: "s", Prompt: "gather A"},
		{ID: "b", SystemPrompt: "s", Prompt: "gather B"},
	}})

	// INPUT ORDER, whatever order the waves finished in: the parent wrote
	// the list and reads the answers as one.
	if got := []string{results[0].ID, results[1].ID, results[2].ID}; !slices.Equal(got,
		[]string{"synth", "a", "b"}) {
		t.Fatalf("results = %v, want input order", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[2] != "synth" {
		t.Fatalf("the dependent did not run last: %v", order)
	}
	// THE ANSWERS ARE THE INPUT. A dependent that has to be told what its
	// dependencies said by the parent is a dependent the parent had to
	// spend a model call assembling.
	for _, want := range []string{"A says yes", "B says no", "reconcile them"} {
		if !strings.Contains(synthesisPrompt, want) {
			t.Errorf("the dependency answers are not in the prompt:\n%s", synthesisPrompt)
		}
	}
}

// A TASK WHOSE INPUT NEVER ARRIVED IS SKIPPED, not run on nothing. Feeding a
// dependent a missing answer produces a confident wrong one.
func TestADependentIsSkippedWhenItsInputDidNotSucceed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(_ context.Context, _ int, req llm.Request) (*llm.Completion, error) {
		if strings.Contains(userText(req), "will not answer") {
			// Prose and no submission: a `no_result`, which is exactly the
			// case where the fields a dependent was promised do not exist.
			return say("I had a think about it", 1, 1), nil
		}
		return answer("fine", 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)

	results := run(t, cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "gather", SystemPrompt: "s", Prompt: "will not answer"},
		{ID: "synth", SystemPrompt: "s", Prompt: "reconcile", After: []string{"gather"}},
		{ID: "unrelated", SystemPrompt: "s", Prompt: "independent work"},
	}})
	if results[0].Status != subagent.StatusNoResult {
		t.Fatalf("the gather task = %+v", results[0])
	}
	if results[1].Status != subagent.StatusSkipped {
		t.Fatalf("the dependent ran on a missing answer: %+v", results[1])
	}
	// WHICH dependency broke the chain, and how. In a graph of eight the
	// parent's next move is entirely determined by that.
	if !strings.Contains(results[1].Error, "gather") ||
		!strings.Contains(results[1].Error, string(subagent.StatusNoResult)) {
		t.Errorf("the skip does not name what broke: %q", results[1].Error)
	}
	if results[2].Status != subagent.StatusOK {
		t.Errorf("an unrelated task was skipped too: %+v", results[2])
	}
	// The skipped task never reached a model: two of the three did.
	if p.count() != 2 {
		t.Errorf("%d model calls, want 2 — a skipped task must cost nothing", p.count())
	}
}

// DETERMINISM: the same graph under the same deadline reports the same
// statuses. A skip is classified before the deadline is read at all, so a
// call that ran out of time reports the broken chain rather than a scattering
// of timeouts.
func TestASkipIsNotReclassifiedByADeadline(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(ctx context.Context, _ int, req llm.Request) (*llm.Completion, error) {
		if strings.Contains(userText(req), "hangs") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return answer("fine", 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	// THE CALL'S OWN CAP IS WHAT FIRES, and it fires during wave 0 — so
	// by the time the dependent is considered, the context is already
	// done. That is precisely the case the ordering rule is for: a
	// deadline-first classifier would report the dependent as
	// `never_started` (a retry-unchanged failure) instead of naming the
	// chain that actually broke.
	cfg.Limits.TaskTimeout = 2 * time.Second
	cfg.Limits.CallTimeout = 50 * time.Millisecond

	for i := range 3 {
		results := run(t, cfg, subagent.Request{Tasks: []subagent.Task{
			{ID: "gather", SystemPrompt: "s", Prompt: "hangs"},
			{ID: "synth", SystemPrompt: "s", Prompt: "reconcile", After: []string{"gather"}},
		}})
		if results[0].Status != subagent.StatusTimedOut {
			t.Fatalf("run %d: the hung task = %+v", i, results[0])
		}
		if results[1].Status != subagent.StatusSkipped {
			t.Fatalf("run %d: the dependent was reported as %q, want a skip",
				i, results[1].Status)
		}
	}
}

// --- structured results ----------------------------------------------------

func TestAWorkerThatNeverSubmittedIsNotGivenAnAnswer(t *testing.T) {
	t.Parallel()
	// The engine writing a result from the transcript would put words in
	// the worker's mouth on the one question the parent asked.
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		return say("here is my thinking, at length", 3, 4), nil
	}}
	res := one(t, baseConfig(t, w, p), request())
	if res.Status != subagent.StatusNoResult {
		t.Fatalf("status = %q", res.Status)
	}
	if len(res.Output) != 0 {
		t.Errorf("an answer was synthesised: %+v", res.Output)
	}
	// The prose is still handed back: rounds the parent paid for are worth
	// reading even when the last step was skipped.
	if res.Text != "here is my thinking, at length" {
		t.Errorf("the worker's prose was discarded: %q", res.Text)
	}
}

func TestAnInvalidSubmissionGoesBackToTheWorker(t *testing.T) {
	t.Parallel()
	// A rejected submission is the one tool failure a model reliably
	// fixes; ending the task over it throws away everything it did.
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(_ context.Context, n int, _ llm.Request) (*llm.Completion, error) {
		if n == 1 {
			return submit(map[string]any{"result": "   "}, 1, 1), nil
		}
		return answer("a real answer", 1, 1), nil
	}}
	res := one(t, baseConfig(t, w, p), request())
	if res.Status != subagent.StatusOK {
		t.Fatalf("status = %q: %+v", res.Status, res)
	}
	if res.Output["result"] != "a real answer" {
		t.Errorf("the corrected submission did not win: %+v", res.Output)
	}
	if p.count() != 2 {
		t.Errorf("%d model calls: the rejection did not reach the worker", p.count())
	}
}

// A SUBMISSION ENDS THE LOOP. Without it a worker that has answered keeps its
// remaining rounds and spends them narrating what it just submitted — on the
// parent's budget, for output nobody reads.
func TestASubmissionEndsTheLoop(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(_ context.Context, n int, _ llm.Request) (*llm.Completion, error) {
		if n == 1 {
			return answer("done", 1, 1), nil
		}
		return callTool("read_file", map[string]any{"path": "/x"}, 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Limits.MaxTurns = 6
	res := one(t, cfg, request("read_file"))
	if p.count() != 1 {
		t.Errorf("%d model calls after a submission, want 1", p.count())
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d", res.Rounds)
	}
}

// A SUBMISSION SURVIVES THE FAILURE THAT FOLLOWED IT. A worker that answered
// and then spent a round it did not have has still answered.
func TestAnAnswerSurvivesALaterTimeout(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// The submission arrives alongside a tool call, so the loop continues
	// past it and then hits the wall.
	p := &provider{name: "sub", reply: func(ctx context.Context, n int, _ llm.Request) (*llm.Completion, error) {
		if n == 1 {
			return &llm.Completion{Model: "scripted", ToolCalls: []llm.ToolCall{
				{ID: "s1", Name: subagent.SubmitTool, Arguments: map[string]any{"result": "the answer"}},
				{ID: "r1", Name: "read_file", Arguments: map[string]any{"path": "/x"}},
			}}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	cfg := baseConfig(t, w, p)
	cfg.Limits.TaskTimeout = 60 * time.Millisecond
	cfg.Limits.MaxTurns = 6

	res := one(t, cfg, request("read_file"))
	if res.Status != subagent.StatusOK {
		t.Fatalf("an answered task was reported as %q: %+v", res.Status, res)
	}
	if res.Output["result"] != "the answer" {
		t.Errorf("the answer was discarded with the failure: %+v", res.Output)
	}
}

// --- the LLM-callable tool -------------------------------------------------

func toolFixture(t *testing.T, p *provider) (*subagent.Tool, *world) {
	t.Helper()
	w := newWorld(t)
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0
	return subagent.NewTool(cfg), w
}

// taskArg builds one task's arguments as a model would send them.
func taskArg(id, prompt string, extra map[string]any) map[string]any {
	out := map[string]any{"id": id, "prompt": prompt, "system_prompt": "you research things"}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestTheToolCoercesTheRoundCapAsAModelEmitsIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value any
		ok    bool
	}{
		{"a bare number", float64(2), true},
		{"a quoted number", "2", true},
		{"absent", nil, true},
		{"a fractional float", 3.9, false},
		{"a bool", true, false},
		{"a word", "many", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &provider{name: "sub"}
			tool, _ := toolFixture(t, p)
			extra := map[string]any{}
			if tc.value != nil {
				extra["max_turns"] = tc.value
			}
			res, err := tool.Call(context.Background(), map[string]any{
				"tasks": []any{taskArg("a", "t", extra)},
			})
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if tc.ok && res.Failed {
				t.Fatalf("%v was refused: %s", tc.value, res.Output)
			}
			if !tc.ok {
				// 3.9 truncating to 3 mis-caps the worker; `true`
				// becoming 1 gives a one-round worker that can do nothing
				// but answer.
				if !res.Failed {
					t.Fatalf("%v was accepted", tc.value)
				}
				if p.count() != 0 {
					t.Error("a malformed cap still reached the model")
				}
			}
		})
	}
}

// THE TOOL ITSELF SUCCEEDS EVEN WHEN TASKS FAIL: per-task outcomes ride inside
// the payload so the parent can pick out which need a retry. Marking the whole
// call failed would tell it to throw away the siblings that worked.
func TestTheToolResultCarriesPerTaskOutcomes(t *testing.T) {
	t.Parallel()
	p := &provider{name: "sub", reply: func(ctx context.Context, _ int, req llm.Request) (*llm.Completion, error) {
		if strings.Contains(userText(req), "hangs") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return answer("all good", 5, 5), nil
	}}
	w := newWorld(t)
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0
	cfg.Limits.TaskTimeout = 60 * time.Millisecond
	tool := subagent.NewTool(cfg)

	res, err := tool.Call(context.Background(), map[string]any{"tasks": []any{
		taskArg("good", "fine", nil),
		taskArg("bad", "hangs", nil),
	}})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Failed {
		t.Fatalf("one failing task failed the whole call: %s", res.Output)
	}
	var payload struct {
		Tasks []struct {
			ID     string         `json:"id"`
			Status string         `json:"status"`
			Result map[string]any `json:"result"`
			Text   string         `json:"text"`
			Error  string         `json:"error"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(res.Output), &payload); err != nil {
		t.Fatalf("the tool result is not JSON: %v\n%s", err, res.Output)
	}
	if len(payload.Tasks) != 2 {
		t.Fatalf("%d tasks reported", len(payload.Tasks))
	}
	if payload.Tasks[0].ID != "good" || payload.Tasks[0].Status != "ok" {
		t.Errorf("the healthy task = %+v", payload.Tasks[0])
	}
	if payload.Tasks[0].Result["result"] != "all good" {
		t.Errorf("the submission is not in the report: %+v", payload.Tasks[0])
	}
	// THE SUBMISSION AND THE PROSE NEVER BOTH APPEAR: two accounts of one
	// task invite the parent to reconcile them, and to prefer the longer.
	if payload.Tasks[0].Text != "" {
		t.Errorf("a submitted task also carried prose: %q", payload.Tasks[0].Text)
	}
	if payload.Tasks[1].Status != string(subagent.StatusTimedOut) ||
		payload.Tasks[1].Error == "" {
		t.Errorf("the failing task = %+v", payload.Tasks[1])
	}
}

func TestTheToolRefusesAnEmptyTasksList(t *testing.T) {
	t.Parallel()
	p := &provider{name: "sub"}
	tool, _ := toolFixture(t, p)
	for _, args := range []map[string]any{{"tasks": []any{}}, {}} {
		res, err := tool.Call(context.Background(), args)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if !res.Failed || !strings.Contains(res.Output, "at least one task") {
			t.Errorf("%v was accepted: %+v", args, res)
		}
	}
	if p.count() != 0 {
		t.Error("a refused call reached the model")
	}
}

func TestTheToolNeverOffersTheDenylistToItsWorker(t *testing.T) {
	t.Parallel()
	p := &provider{name: "sub"}
	tool, _ := toolFixture(t, p)
	res, err := tool.Call(context.Background(), map[string]any{"tasks": []any{
		taskArg("a", "t", map[string]any{
			"tools": []any{subagent.ToolName, "read_file"},
		}),
	}})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Failed {
		t.Fatalf("a request naming a denied tool failed outright: %s", res.Output)
	}
	offered := p.offered(0)
	if slices.Contains(offered, subagent.ToolName) {
		t.Errorf("a worker was offered the delegate tool: %v", offered)
	}
	if !slices.Contains(offered, "read_file") {
		t.Errorf("the legitimate tool was lost: %v", offered)
	}
	// And the submission tool is always there: a worker able to work and
	// unable to report is worse than one that was never started.
	if !slices.Contains(offered, subagent.SubmitTool) {
		t.Errorf("the worker cannot answer: %v", offered)
	}
}

func TestTheToolRejectsMalformedToolNames(t *testing.T) {
	t.Parallel()
	p := &provider{name: "sub"}
	tool, _ := toolFixture(t, p)
	res, err := tool.Call(context.Background(), map[string]any{"tasks": []any{
		taskArg("a", "t", map[string]any{
			"tools": []any{map[string]any{"name": "read_file"}},
		}),
	}})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Output, "schema") {
		t.Errorf("a non-string tool name was accepted: %+v", res)
	}
	if p.count() != 0 {
		t.Error("a malformed request reached the model")
	}
}

// THE MODEL IS TOLD WHICH WORKERS IT HAS, in the tool description as well as
// the system prompt: a provider sends the description with the schema on
// every round, and a model choosing a worker mid-loop reads that rather than
// scrolling back ten rounds.
func TestTheToolDescriptionNamesTheSeatsWorkers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cfg := baseConfig(t, w, &provider{name: "sub"})
	if got := subagent.NewTool(cfg).Description(); !strings.Contains(got, "no worker templates") {
		t.Errorf("a seat with no workers is not told so:\n%s", got)
	}
	cfg.Workers = map[string]config.Worker{
		"researcher": {Description: "reads", SystemPrompt: "s"},
		"auditor":    {Description: "audits", SystemPrompt: "s"},
	}
	got := subagent.NewTool(cfg).Description()
	for _, want := range []string{"auditor", "researcher"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is not in the description:\n%s", want, got)
		}
	}
	// SORTED, because the description reaches a prompt: a list that
	// reshuffles between rounds costs the provider's cache the prefix.
	if strings.Index(got, "auditor") > strings.Index(got, "researcher") {
		t.Errorf("the worker list is not sorted:\n%s", got)
	}
}

// userText returns the first user message's content.
func userText(req llm.Request) string {
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			return m.Content
		}
	}
	return ""
}

// blockingMeter holds every charge at the gate, so a test can put several
// children inside the inner spend at once.
type blockingMeter struct {
	inner toolloop.BudgetMeter
	gate  chan struct{}
}

func (b blockingMeter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	<-b.gate
	return b.inner.Spend(ctx, tokens)
}

// errOnceMeter fails the FIRST charge and serves the rest, so a test can put a
// store blip in the middle of a batch.
type errOnceMeter struct {
	inner toolloop.BudgetMeter
	once  *sync.Once
}

func (e errOnceMeter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	var blipped bool
	e.once.Do(func() { blipped = true })
	if blipped {
		return toolloop.SpendOutcome{}, errors.New("counter unreachable")
	}
	return e.inner.Spend(ctx, tokens)
}

// refuseOnceMeter refuses the FIRST charge and serves the rest, so a test can
// watch what a definite refusal does to a shared reservation.
type refuseOnceMeter struct {
	inner toolloop.BudgetMeter
	once  *sync.Once
}

func (r refuseOnceMeter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	var refused bool
	r.once.Do(func() { refused = true })
	if refused {
		return toolloop.SpendOutcome{OK: false, Scope: "org", Used: 0, Limit: 0}, nil
	}
	return r.inner.Spend(ctx, tokens)
}

// skillFor is a one-skill catalogue that fires only when its tool is in
// scope, so a test can watch which tools the prompt's skill scope covers.
type skillFor struct{ tool, key string }

func (c skillFor) SkillsFor(_ prompts.Phase, surface prompts.Surface) []prompts.Skill {
	if !slices.Contains(surface.Tools, c.tool) {
		return nil
	}
	return []prompts.Skill{{Key: c.key, Summary: "how to use " + c.tool, Required: true}}
}

func (c skillFor) Render(text string) string { return text }

// --- what the caller must supply, and what happens without it ---------------

// THE PARENT'S CALLABLE SET IS READ LIVE.
//
// An executor that discovers a tool mid-phase and activates it has widened
// what it may call, and a child spawned afterwards inherits that. With a
// frozen slice the tool is silently rejected by Permit's second filter and
// reported to the model as refused — a permission decision nobody made.
func TestTheParentsToolsAreReadWhenTheChildSpawns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	parent := []string{"read_file"}
	answer := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		return &llm.Completion{Content: "done"}, nil
	}}
	cfg := baseConfig(t, w, answer)
	cfg.Parent = func() []string { return parent }

	res := one(t, cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "a", Prompt: "go", SystemPrompt: "you are a worker",
			Tools: []string{"read_file", "web_search"}},
	}})
	if !slices.Contains(res.Rejected, "web_search") {
		t.Fatalf("web_search was granted before the parent had it: %+v", res.Rejected)
	}

	// The parent discovers it mid-phase and activates it; the next spawn
	// inherits that, which a frozen slice could not express.
	parent = append(parent, "web_search")
	res = one(t, cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "a", Prompt: "go", SystemPrompt: "you are a worker",
			Tools: []string{"web_search"}},
	}})
	if slices.Contains(res.Rejected, "web_search") {
		t.Error("a tool the parent had activated was still refused to its child")
	}
}

// EVERY CHILD PRODUCES ONE TELEMETRY CALL, on every path it can end on.
//
// The package produces a Result for every outcome precisely so a caller's
// phase event cannot be missing — and the tool is that caller. Without the
// hook a fan-out is invisible: tokens charged, model calls made, and nothing
// in the event store saying a sub-agent ran.
func TestEveryChildIsReportedOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	answer := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		return &llm.Completion{Content: "done"}, nil
	}}
	var seen []subagent.Result
	cfg := baseConfig(t, w, answer)
	cfg.Telemetry = func(_ context.Context, res subagent.Result) { seen = append(seen, res) }

	if _, err := subagent.Run(t.Context(), cfg, subagent.Request{Tasks: []subagent.Task{
		{ID: "a", Prompt: "go", SystemPrompt: "you are a worker"},
	}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("one worker produced %d telemetry calls", len(seen))
	}
	if seen[0].SystemPrompt == "" || seen[0].UserPrompt == "" {
		t.Error("the reported result carries no prompts, so a dashboard shows an empty phase")
	}
}

// A WORKER'S THINKING TRAVELS WITH THE ROUND IT THOUGHT IN.
//
// The Result carries Executions and Narration keyed on the SAME round number,
// which is the whole contract a consumer interleaves them on. Publishing the
// executions alone left every delegated worker's card as bare tool rows with
// nothing that asked for them, and pushed the worker's reasoning into the
// dashboard's pre-narration fallback — where it renders under a heading
// claiming the record predates rounds being kept apart, which is false: it is
// what this build publishes for every worker.
func TestAWorkersNarrationSharesItsRoundsWithItsToolCalls(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(_ context.Context, n int, _ llm.Request) (*llm.Completion, error) {
		if n == 1 {
			call := callTool("read_file", map[string]any{"path": "/x"}, 60, 40)
			call.ReasoningContent = "the answer is probably in that file"
			return call, nil
		}
		return answer("read it", 10, 5), nil
	}}

	res := one(t, baseConfig(t, w, p), request("read_file"))

	if res.Status != subagent.StatusOK {
		t.Fatalf("the worker did not finish: %+v", res)
	}
	if len(res.Narration) == 0 {
		t.Fatal("the worker's rounds carry no narration, so its thinking reaches no consumer")
	}
	byRound := make(map[int]string, len(res.Narration))
	for _, n := range res.Narration {
		byRound[n.Round] = n.Reasoning
	}
	// The thinking and the call it asked for carry the SAME number, which is
	// the only thing that puts them in one block. A round that called a tool
	// and said nothing narrates nothing — that is not a gap, it is the round
	// having nothing to say.
	var read toolloop.Execution
	for _, ex := range res.Executions {
		if ex.Name == "read_file" {
			read = ex
		}
	}
	if read.Name == "" {
		t.Fatalf("the worker's tool call is missing: %+v", res.Executions)
	}
	if byRound[read.Round] != "the answer is probably in that file" {
		t.Errorf("round %d called read_file but its narration reads %q",
			read.Round, byRound[read.Round])
	}
	for _, n := range res.Narration {
		if n.Round < 1 || n.Round > res.Rounds {
			t.Errorf("narration numbered round %d on a worker that ran %d", n.Round, res.Rounds)
		}
	}
}

// A worker runs under its parent's name and inside its parent's turn, so the
// parent's OWN memory is not its to write: a note it persists is filed as the
// parent's observation, a skill it refines rewrites the parent's practice, and
// a marker it stamps says the parent finished reading pages it never saw.
//
// They are denied BY NAME rather than by annotation, which is the criterion
// this list exists for. All three are closed-world writes — they touch this
// node's own store and nothing else — so [mcp.WritesToSharedSurface]
// correctly does not catch them, and for a while the only thing keeping them
// from a worker was an OpenWorld hint nobody had set. That is an accident, not
// a boundary: annotate them honestly and the accident disappears, which is
// exactly what happened and why this test exists.
func TestAWorkerNeverWritesItsParentsMemory(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"reflect_and_persist", "refine_skill", "mark_onboarded"} {
		if !subagent.Denied(name) {
			t.Errorf("%s is not on the engine-control denylist: a worker acting "+
				"under its parent's name could write the parent's own memory", name)
		}
	}
}
