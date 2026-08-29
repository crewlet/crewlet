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
		return &llm.Completion{Model: p.name, Content: "done"}, nil
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
		MaxTurns: 4, Timeout: 5 * time.Second, BatchTimeout: 10 * time.Second,
		MaxParallel: 3, BudgetFraction: 0.2, MinPerChildTokens: 500,
	}
}

// baseConfig wires a seat, a world and one provider under the "default" key.
func baseConfig(t *testing.T, w *world, p *provider) subagent.Config {
	t.Helper()
	return subagent.Config{
		Seat:     seat(t),
		Models:   models(t, phase.Entry{Key: "default", Provider: p}),
		Universe: w.snapshot,
		Parent:   w.parentAll(),
		Limits:   limits(),
	}
}

func request(toolNames ...string) subagent.Request {
	return subagent.Request{
		TaskPrompt: "summarise the incident", SystemPrompt: "you research things",
		ToolNames: toolNames,
	}
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

	res, err := subagent.Spawn(context.Background(), cfg, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

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

	res, err := subagent.Spawn(context.Background(), cfg, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
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

	res, err := subagent.Spawn(context.Background(), cfg, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !strings.Contains(res.SystemPrompt, "## Available tools") {
		t.Fatalf("no catalogue in a discovery-capable child's prompt:\n%s", res.SystemPrompt)
	}
	if !strings.Contains(res.SystemPrompt, "research") {
		t.Error("the catalogue does not name the MCP server the child may discover")
	}
	for _, denied := range []string{"slack_post", subagent.ToolName} {
		if strings.Contains(res.SystemPrompt, denied) {
			t.Errorf("the catalogue advertises %s, which the child cannot have", denied)
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

	res, err := subagent.Spawn(context.Background(), cfg, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !strings.Contains(res.SystemPrompt, "research-etiquette") {
		t.Errorf("a skill for a discoverable tool was left out:\n%s", res.SystemPrompt)
	}

	// The counterfactual: a skill for a tool the grant refuses stays out,
	// so this is scope and not "every skill in the registry".
	cfg.Skills = skillFor{tool: "slack_post", key: "posting-etiquette"}
	res, err = subagent.Spawn(context.Background(), cfg, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
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

	res, err := subagent.Spawn(context.Background(), cfg, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if sub.count() != 1 || main.count() != 0 {
		t.Errorf("llm_subagent was not used: sub=%d main=%d", sub.count(), main.count())
	}
	if res.ProviderKey != "sub-model" {
		t.Errorf("ProviderKey = %q, want sub-model", res.ProviderKey)
	}

	// The counterfactual: with no llm_subagent the seat's own chain answers,
	// so the assertion above is about the phase and not about ordering.
	cfg.Seat.Role.LLMSubagent = nil
	if _, err := subagent.Spawn(context.Background(), cfg, request("read_file")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
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
		return say("done", 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)

	res, err := subagent.Spawn(context.Background(), cfg, request("read_file", "slack_post"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
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
			req.MaxTurns = tc.requested
			res, err := subagent.Spawn(context.Background(), cfg, req)
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			if p.count() != tc.want || res.Rounds != tc.want {
				t.Errorf("rounds: provider=%d result=%d, want %d",
					p.count(), res.Rounds, tc.want)
			}
			if res.Failed {
				t.Errorf("a clamped request was reported as a failure: %+v", res)
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
	cfg.Limits.Timeout = 60 * time.Millisecond

	res, err := subagent.Spawn(context.Background(), cfg, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn returned an error for a child that was merely cut off: %v", err)
	}
	if !res.Failed || !res.TimedOut || res.ErrorKind != subagent.KindTimeout {
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
		return say("all done", 10, 5), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Limits.Timeout = 5 * time.Second

	res, err := subagent.Spawn(context.Background(), cfg, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res.Failed || res.TimedOut || res.ErrorKind != "" {
		t.Fatalf("a healthy child was flagged: %+v", res)
	}
	if res.Text != "all done" || res.Tokens() != 15 {
		t.Errorf("result = %q / %d tokens", res.Text, res.Tokens())
	}
}

func TestAPanickingChildDoesNotTakeTheParentDown(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub", reply: func(context.Context, int, llm.Request) (*llm.Completion, error) {
		panic("provider SDK dereferenced nil")
	}}
	cfg := baseConfig(t, w, p)

	res, err := subagent.Spawn(context.Background(), cfg, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !res.Failed || res.ErrorKind != subagent.KindPanic {
		t.Fatalf("a panic was not contained as a failure: %+v", res)
	}
	if !strings.Contains(res.Error, "dereferenced nil") {
		t.Errorf("the panic's message was lost: %q", res.Error)
	}
	if res.TimedOut {
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
		return say("fine", 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0

	results, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{
		ToolNames: []string{"read_file"},
		Tasks: []subagent.Task{
			{TaskPrompt: "healthy one", SystemPrompt: "s"},
			{TaskPrompt: "poison", SystemPrompt: "s"},
			{TaskPrompt: "healthy two", SystemPrompt: "s"},
		},
	})
	if err != nil {
		t.Fatalf("SpawnBatch: %v", err)
	}
	if results[0].Failed || results[2].Failed {
		t.Errorf("a panicking sibling took healthy children with it: %+v", results)
	}
	if results[1].ErrorKind != subagent.KindPanic {
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
	go func() { <-started; cancel() }()
	res, err := subagent.Spawn(ctx, cfg, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// A torn-down turn is not an exceeded cap. A planner told "timed out"
	// helpfully retries with a smaller task against an engine that is
	// shutting down.
	if res.ErrorKind != subagent.KindCancelled || res.TimedOut {
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
		return say("more", 60, 40), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Budget = countingMeter{m}
	cfg.ParentRemaining = 1000 // slice = 200

	res, err := subagent.Spawn(context.Background(), cfg, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !res.Failed || res.ErrorKind != subagent.KindBudget {
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
	res2, err := subagent.Spawn(context.Background(), cfg2, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res2.Failed {
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
		return say("answer", 100, 50), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Budget = countingMeter{m}
	cfg.ParentRemaining = 1000 // total slice = 200, one 150-token child fits
	cfg.Limits.MinPerChildTokens = 0

	results, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{
		ToolNames: []string{"read_file"},
		Tasks: []subagent.Task{
			{TaskPrompt: "a", SystemPrompt: "s"},
			{TaskPrompt: "b", SystemPrompt: "s"},
			{TaskPrompt: "c", SystemPrompt: "s"},
		},
	})
	if err != nil {
		t.Fatalf("SpawnBatch: %v", err)
	}
	// A per-child wrapper would have let all three spend 150 — 450 against
	// a configured slice of 200, which is the fan-out cost being invisible
	// until the org budget is gone.
	if m.sum() > 200 {
		t.Fatalf("the batch charged %d against a 200-token slice", m.sum())
	}
	var ok int
	for _, r := range results {
		if !r.Failed {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("%d children finished on a slice that fits one: %+v", ok, results)
	}
	for _, r := range results {
		if r.Failed && r.ErrorKind != subagent.KindBudget {
			t.Errorf("a starved child reported %q: %+v", r.ErrorKind, r)
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
	cfg.Limits.MinPerChildTokens = 500

	tasks := []subagent.Task{
		{TaskPrompt: "a", SystemPrompt: "s"},
		{TaskPrompt: "b", SystemPrompt: "s"},
	}
	results, err := subagent.SpawnBatch(context.Background(), cfg,
		subagent.BatchRequest{ToolNames: []string{"read_file"}, Tasks: tasks})

	var refused *subagent.BatchRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a BatchRefusedError", err)
	}
	if refused.Slice != 200 || refused.MinPerChild != 500 || refused.Tasks != 2 {
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
	cfg.Limits.MinPerChildTokens = 150 // 200 >= 150, but 200 < 2*150
	if _, err := subagent.SpawnBatch(context.Background(), cfg,
		subagent.BatchRequest{ToolNames: []string{"read_file"}, Tasks: tasks}); !errors.As(err, &refused) {
		t.Fatalf("a slice that feeds one of two children was accepted: %v", err)
	}
	if p.count() != 0 {
		t.Errorf("a refused batch called the model %d times", p.count())
	}

	// The counterfactual: a floor the slice can meet lets the same batch run.
	cfg.Limits.MinPerChildTokens = 50
	if _, err := subagent.SpawnBatch(context.Background(), cfg,
		subagent.BatchRequest{ToolNames: []string{"read_file"}, Tasks: tasks}); err != nil {
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
	cfg.Limits.MinPerChildTokens = 100000

	if _, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{
		ToolNames: []string{"read_file"},
		Tasks:     []subagent.Task{{TaskPrompt: "a", SystemPrompt: "s"}},
	}); err != nil {
		t.Fatalf("an uncapped parent's batch was refused: %v", err)
	}
	if p.count() == 0 {
		t.Error("the batch never reached the model")
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
		return say("answer", 60, 0), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Budget = slow
	cfg.ParentRemaining = 500 // slice = 100, so exactly one 60-token child fits
	cfg.Limits.MinPerChildTokens = 0
	cfg.Limits.MaxParallel = 8

	done := make(chan []subagent.Result, 1)
	go func() {
		var tasks []subagent.Task
		for i := range 8 {
			tasks = append(tasks, subagent.Task{
				TaskPrompt: fmt.Sprintf("t%d", i), SystemPrompt: "s",
			})
		}
		res, err := subagent.SpawnBatch(context.Background(), cfg,
			subagent.BatchRequest{ToolNames: []string{"read_file"}, Tasks: tasks})
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
		if !r.Failed {
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
		return say("answer", 10, 0), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Budget = countingMeter{m}
	cfg.ParentRemaining = 100 // slice = 20, so two 10-token rounds fit

	res, err := subagent.Spawn(context.Background(), cfg, request("read_file"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// The ORG refused, so the child's own kind must not claim its slice ran
	// out — that sends an operator to raise a limit that was never reached.
	if res.ErrorKind == subagent.KindBudget {
		t.Errorf("an org refusal was reported as the sub-agent's own slice: %+v", res)
	}
	if !res.Failed {
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
		return say("answer", 10, 0), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Budget = gate
	cfg.ParentRemaining = 75 // slice = 15: one 10-token child, not two
	cfg.Limits.MinPerChildTokens = 0
	cfg.Limits.MaxParallel = 1

	results, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{
		ToolNames: []string{"read_file"},
		Tasks: []subagent.Task{
			{TaskPrompt: "a", SystemPrompt: "s"},
			{TaskPrompt: "b", SystemPrompt: "s"},
		},
	})
	if err != nil {
		t.Fatalf("SpawnBatch: %v", err)
	}
	var ok, failed int
	for _, r := range results {
		if r.Failed {
			failed++
			if r.ErrorKind == subagent.KindBudget {
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
		return say("answer", 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Limits.MaxParallel = 2
	cfg.ParentRemaining = 0

	var tasks []subagent.Task
	for i := range 6 {
		tasks = append(tasks, subagent.Task{TaskPrompt: fmt.Sprintf("t%d", i), SystemPrompt: "s"})
	}
	results, err := subagent.SpawnBatch(context.Background(), cfg,
		subagent.BatchRequest{ToolNames: []string{"read_file"}, Tasks: tasks})
	if err != nil {
		t.Fatalf("SpawnBatch: %v", err)
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
		if r.Failed {
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
		return say("fast answer", 7, 3), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0
	cfg.Limits.MaxParallel = 2
	cfg.Limits.Timeout = 10 * time.Second // the BATCH cap is what must fire
	cfg.Limits.BatchTimeout = 60 * time.Millisecond

	results, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{
		ToolNames: []string{"read_file"},
		Tasks: []subagent.Task{
			{TaskPrompt: "quick one", SystemPrompt: "s"},
			{TaskPrompt: "slow one", SystemPrompt: "s"},
		},
	})
	if err != nil {
		t.Fatalf("SpawnBatch: %v", err)
	}
	// Discarding a finished child's answer throws away work that completed
	// and was paid for: the tokens are spent either way.
	if results[0].Failed || results[0].Text != "fast answer" || results[0].Tokens() != 10 {
		t.Errorf("a finished child's answer was lost: %+v", results[0])
	}
	if !results[1].TimedOut || !strings.Contains(results[1].Error, "batch") {
		t.Errorf("the straggler was not attributed to the batch cap: %+v", results[1])
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
		cfg.Limits.Timeout = 10 * time.Second
		cfg.Limits.BatchTimeout = 20 * time.Millisecond

		results, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{
			ToolNames: []string{"read_file"},
			Tasks: []subagent.Task{
				{TaskPrompt: "a", SystemPrompt: "s"},
				{TaskPrompt: "b", SystemPrompt: "s"},
			},
		})
		if err != nil {
			t.Fatalf("SpawnBatch: %v", err)
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
			if !r.TimedOut || !r.Failed {
				t.Errorf("a queued child was not marked failed: %+v", r)
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
		return say("answer", 60, 0), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.Budget = failing
	cfg.ParentRemaining = 500 // slice = 100, so 60 + 60 does not fit
	cfg.Limits.MinPerChildTokens = 0
	cfg.Limits.MaxParallel = 1

	results, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{
		ToolNames: []string{"read_file"},
		Tasks: []subagent.Task{
			{TaskPrompt: "a", SystemPrompt: "s"},
			{TaskPrompt: "b", SystemPrompt: "s"},
		},
	})
	if err != nil {
		t.Fatalf("SpawnBatch: %v", err)
	}
	// Whichever child hit the broken counter, the OTHER must find the slice
	// already spoken for: a released reservation would let it through.
	var budget, unreachable int
	for _, r := range results {
		switch {
		case r.ErrorKind == subagent.KindBudget:
			budget++
		case r.Failed:
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
		return say("answer", 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0
	cfg.Limits.Timeout = 50 * time.Millisecond
	cfg.Limits.BatchTimeout = 10 * time.Second

	start := time.Now()
	results, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{
		ToolNames: []string{"read_file"},
		Tasks: []subagent.Task{
			{TaskPrompt: "hang here", SystemPrompt: "s"},
			{TaskPrompt: "fine", SystemPrompt: "s"},
			{TaskPrompt: "also fine", SystemPrompt: "s"},
		},
	})
	if err != nil {
		t.Fatalf("SpawnBatch: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the batch waited out its own cap (%v) for one straggler", elapsed)
	}
	if !results[0].TimedOut || strings.Contains(results[0].Error, "batch") {
		t.Errorf("the straggler was not attributed to its own cap: %+v", results[0])
	}
	for _, i := range []int{1, 2} {
		if results[i].Failed {
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
		return say("answer to "+body, 1, 1), nil
	}}
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0
	cfg.Limits.MaxParallel = 3

	results, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{
		ToolNames: []string{"read_file"},
		Tasks: []subagent.Task{
			{TaskPrompt: "first", SystemPrompt: "s"},
			{TaskPrompt: "second", SystemPrompt: "s"},
			{TaskPrompt: "third", SystemPrompt: "s"},
		},
	})
	if err != nil {
		t.Fatalf("SpawnBatch: %v", err)
	}
	for i, want := range []string{"answer to first", "answer to second", "answer to third"} {
		if results[i].Text != want {
			t.Errorf("results[%d] = %q, want %q", i, results[i].Text, want)
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
		return say("answer", 4, 6), nil
	}}
	pub := &publisher{}
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0
	cfg.Publisher = pub
	cfg.Trace = events.NewTrace()

	if _, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{
		ToolNames: []string{"read_file"},
		Tasks: []subagent.Task{
			{TaskPrompt: "good", SystemPrompt: "s"},
			{TaskPrompt: "bad", SystemPrompt: "s"},
		},
	}); err != nil {
		t.Fatalf("SpawnBatch: %v", err)
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

	results, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{
		ToolNames: []string{"read_file"},
		Tasks:     []subagent.Task{{TaskPrompt: "a", SystemPrompt: "s"}},
	})
	if err != nil {
		t.Fatalf("SpawnBatch: %v", err)
	}
	if len(results) != 1 || results[0].Failed {
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
		{"expired deadline", func(l *subagent.Limits) { l.Timeout = 0 }, "Timeout"},
		{"expired batch deadline", func(l *subagent.Limits) { l.BatchTimeout = 0 }, "BatchTimeout"},
		{"no concurrency", func(l *subagent.Limits) { l.MaxParallel = 0 }, "MaxParallel"},
		{"no fraction", func(l *subagent.Limits) { l.BudgetFraction = 0 }, "BudgetFraction"},
		{"fraction above one", func(l *subagent.Limits) { l.BudgetFraction = 1.5 }, "BudgetFraction"},
		{"negative floor", func(l *subagent.Limits) { l.MinPerChildTokens = -1 }, "MinPerChildTokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &provider{name: "sub"}
			cfg := baseConfig(t, w, p)
			tc.mutin(&cfg.Limits)
			_, err := subagent.Spawn(context.Background(), cfg, request("read_file"))
			if err == nil {
				t.Fatal("a zero cap was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not name the field: %v", err)
			}
			if p.count() != 0 {
				t.Error("a refused spawn still reached the model")
			}
		})
	}
	// The counterfactual: valid limits are accepted, so this is validation
	// and not a permanently closed door.
	p := &provider{name: "sub"}
	cfg := baseConfig(t, w, p)
	if _, err := subagent.Spawn(context.Background(), cfg, request("read_file")); err != nil {
		t.Fatalf("valid limits were refused: %v", err)
	}
}

func TestAPromptlessRequestIsRefusedBeforeAnythingRuns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, tc := range []struct {
		name string
		req  subagent.Request
	}{
		{"no task", subagent.Request{SystemPrompt: "s"}},
		{"no system prompt", subagent.Request{TaskPrompt: "t"}},
		{"blank task", subagent.Request{TaskPrompt: "  \n ", SystemPrompt: "s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &provider{name: "sub"}
			cfg := baseConfig(t, w, p)
			if _, err := subagent.Spawn(context.Background(), cfg, tc.req); err == nil {
				t.Fatal("an empty prompt was accepted")
			}
			if p.count() != 0 {
				t.Error("a refused spawn still reached the model")
			}
		})
	}
}

func TestAnEmptyBatchIsRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "sub"}
	cfg := baseConfig(t, w, p)
	if _, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{}); err == nil {
		t.Fatal("an empty batch was accepted")
	}
	_, err := subagent.SpawnBatch(context.Background(), cfg, subagent.BatchRequest{
		Tasks: []subagent.Task{{TaskPrompt: "a", SystemPrompt: ""}},
	})
	if err == nil || !strings.Contains(err.Error(), "tasks[0]") {
		t.Fatalf("a malformed task was not named: %v", err)
	}
	if p.count() != 0 {
		t.Error("a refused batch reached the model")
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

func TestTheToolRefusesBothShapesAtOnce(t *testing.T) {
	t.Parallel()
	p := &provider{name: "sub"}
	tool, _ := toolFixture(t, p)
	res, err := tool.Call(context.Background(), map[string]any{
		"task_prompt":   "one",
		"system_prompt": "s",
		"tasks":         []any{map[string]any{"task_prompt": "a", "system_prompt": "s"}},
	})
	if err != nil {
		t.Fatalf("Call returned a Go error: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Output, "not both") {
		t.Errorf("a hybrid payload was accepted: %+v", res)
	}
	if p.count() != 0 {
		t.Error("a refused call reached the model")
	}
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
		{"zero", float64(0), false},
		{"a word", "many", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &provider{name: "sub"}
			tool, _ := toolFixture(t, p)
			args := map[string]any{"task_prompt": "t", "system_prompt": "s"}
			if tc.value != nil {
				args["max_turns"] = tc.value
			}
			res, err := tool.Call(context.Background(), args)
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if tc.ok && res.Failed {
				t.Fatalf("%v was refused: %s", tc.value, res.Output)
			}
			if !tc.ok {
				// 3.9 truncating to 3 mis-caps the child; `true` becoming 1
				// gives a one-round worker that can do nothing but answer.
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

func TestTheToolReturnsPartialOutputWithTheReason(t *testing.T) {
	t.Parallel()
	p := &provider{name: "sub", reply: func(ctx context.Context, n int, _ llm.Request) (*llm.Completion, error) {
		if n == 1 {
			c := callTool("read_file", map[string]any{"path": "/x"}, 1, 1)
			c.Content = "half an answer so far"
			return c, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	w := newWorld(t)
	cfg := baseConfig(t, w, p)
	cfg.ParentRemaining = 0
	cfg.Limits.Timeout = 60 * time.Millisecond
	tool := subagent.NewTool(cfg)

	res, err := tool.Call(context.Background(), map[string]any{
		"task_prompt": "t", "system_prompt": "s", "tool_names": []any{"read_file"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Output, "timeout") {
		t.Fatalf("the cut-off was not reported: %+v", res)
	}
	// A child that spent rounds usually produced most of an answer; throwing
	// it away makes the parent re-run the whole task.
	if !strings.Contains(res.Output, "Partial output") ||
		!strings.Contains(res.Output, "half an answer so far") {
		t.Errorf("the partial transcript was discarded: %s", res.Output)
	}
}

func TestTheBatchedToolResultCarriesPerChildErrors(t *testing.T) {
	t.Parallel()
	p := &provider{name: "sub", reply: func(_ context.Context, _ int, req llm.Request) (*llm.Completion, error) {
		if strings.Contains(userText(req), "bad") {
			panic("child fell over")
		}
		return say("child answer", 3, 2), nil
	}}
	tool, _ := toolFixture(t, p)

	res, err := tool.Call(context.Background(), map[string]any{
		"tool_names": []any{"read_file"},
		"tasks": []any{
			map[string]any{"task_prompt": "good", "system_prompt": "s"},
			map[string]any{"task_prompt": "bad", "system_prompt": "s"},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// The tool SUCCEEDED: marking the whole call failed tells the planner to
	// throw away the sibling that worked.
	if res.Failed {
		t.Fatalf("a partly-failed batch failed the tool: %s", res.Output)
	}
	var payload struct {
		Results []struct {
			Index      int    `json:"index"`
			Text       string `json:"text"`
			TokensUsed int    `json:"tokens_used"`
			Error      string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Output), &payload); err != nil {
		t.Fatalf("the batched output is not JSON: %v\n%s", err, res.Output)
	}
	if len(payload.Results) != 2 {
		t.Fatalf("%d results rendered", len(payload.Results))
	}
	if payload.Results[0].Text != "child answer" || payload.Results[0].TokensUsed != 5 {
		t.Errorf("the healthy child = %+v", payload.Results[0])
	}
	if payload.Results[1].Error == "" || payload.Results[1].Index != 1 {
		t.Errorf("the failed child carries no error: %+v", payload.Results[1])
	}
}

func TestTheToolRefusesAnEmptyTasksList(t *testing.T) {
	t.Parallel()
	p := &provider{name: "sub"}
	tool, _ := toolFixture(t, p)
	res, err := tool.Call(context.Background(), map[string]any{"tasks": []any{}})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Output, "not be empty") {
		t.Errorf("an empty batch was accepted: %+v", res)
	}
	// Absent is not the same as empty: with neither shape present the
	// single-shape validation is what must complain.
	res, err = tool.Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.Failed || strings.Contains(res.Output, "not be empty") {
		t.Errorf("an absent shape was read as an empty batch: %+v", res)
	}
}

func TestTheToolNeverOffersTheDenylistToItsChild(t *testing.T) {
	t.Parallel()
	p := &provider{name: "sub"}
	tool, _ := toolFixture(t, p)
	res, err := tool.Call(context.Background(), map[string]any{
		"task_prompt": "t", "system_prompt": "s",
		"tool_names": []any{subagent.ToolName, "read_file"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Failed {
		t.Fatalf("a request naming a denied tool failed outright: %s", res.Output)
	}
	offered := p.offered(0)
	if slices.Contains(offered, subagent.ToolName) {
		t.Errorf("a sub-agent was offered the spawn tool: %v", offered)
	}
	if !slices.Contains(offered, "read_file") {
		t.Errorf("the legitimate tool was lost: %v", offered)
	}
}

func TestTheToolRejectsMalformedToolNames(t *testing.T) {
	t.Parallel()
	p := &provider{name: "sub"}
	tool, _ := toolFixture(t, p)
	res, err := tool.Call(context.Background(), map[string]any{
		"task_prompt": "t", "system_prompt": "s",
		"tool_names": []any{map[string]any{"name": "read_file"}},
	})
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
