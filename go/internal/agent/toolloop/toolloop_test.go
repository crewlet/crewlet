package toolloop_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// --- doubles ---------------------------------------------------------------

// scriptedProvider answers with a fixed sequence, so a case states the model's
// behaviour as data rather than as a mock's expectations.
type scriptedProvider struct {
	turns  []llm.Completion
	calls  int
	seen   []llm.Request
	failAt int // 1-based; 0 never fails
	err    error
}

func (p *scriptedProvider) Model() string { return "scripted" }

func (p *scriptedProvider) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	p.calls++
	p.seen = append(p.seen, req)
	if p.failAt == p.calls {
		if p.err != nil {
			return nil, p.err
		}
		return nil, errors.New("provider exploded")
	}
	if p.calls > len(p.turns) {
		return &llm.Completion{Content: "done"}, nil
	}
	c := p.turns[p.calls-1]
	return &c, nil
}

// fakeSurface answers tool calls from a table.
type fakeSurface struct {
	tools    []llm.ToolDef
	results  map[string]toolloop.ToolResult
	ran      []string
	surfErr  error
	phaseVal string
}

func (s *fakeSurface) ToolDefs() []llm.ToolDef { return s.tools }
func (s *fakeSurface) Phase() string {
	if s.phaseVal == "" {
		return "execute"
	}
	return s.phaseVal
}

func (s *fakeSurface) Execute(_ context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	s.ran = append(s.ran, call.Name)
	if s.surfErr != nil {
		return toolloop.ToolResult{}, s.surfErr
	}
	if r, ok := s.results[call.Name]; ok {
		return r, nil
	}
	return toolloop.ToolResult{Output: "ok"}, nil
}

// meter records spends and can refuse or fail.
type meter struct {
	spent    int
	refuseAt int // refuse once cumulative spend would exceed this; 0 never
	err      error
}

func (m *meter) Spend(_ context.Context, tokens int) (toolloop.SpendOutcome, error) {
	if m.err != nil {
		return toolloop.SpendOutcome{}, m.err
	}
	if m.refuseAt > 0 && m.spent+tokens > m.refuseAt {
		return toolloop.SpendOutcome{
			Scope: "role", Used: m.spent, Limit: m.refuseAt,
		}, nil
	}
	m.spent += tokens
	return toolloop.SpendOutcome{OK: true}, nil
}

func toolCall(id, name string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: map[string]any{}}
}

func def(name string) llm.ToolDef { return llm.ToolDef{Name: name} }

// --- the loop's shape ------------------------------------------------------

func TestALoopEndsWhenTheModelStopsAskingForTools(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{
		{Content: "thinking", ToolCalls: []llm.ToolCall{toolCall("1", "read")}},
		{Content: "finished"},
	}}
	s := &fakeSurface{tools: []llm.ToolDef{def("read")}}

	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 2 {
		t.Errorf("rounds = %d, want 2", res.RoundsUsed)
	}
	if res.ExhaustedRounds {
		t.Error("a loop the model ended reports exhausted rounds")
	}
	if len(res.Executions) != 1 || res.Executions[0].Name != "read" {
		t.Errorf("executions = %+v, want one read", res.Executions)
	}
}

// The per-model token breakdown is built from completions, so the loop bills
// against the model the ANSWER named, and falls back to the provider's
// configured identity only for a backend that named nothing. Both directions
// matter: a chain that fell through mid-phase reports the member that served,
// and a bare backend that fills nothing in still reports something billable.
func TestTheResultBillsAgainstTheModelTheCompletionNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		first llm.Completion
		want  string
	}{
		{"the completion names one", llm.Completion{Model: "served-model", Content: "done"}, "served-model"},
		{"the completion names none", llm.Completion{Content: "done"}, "scripted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &scriptedProvider{turns: []llm.Completion{tc.first}}
			res, err := toolloop.Run(t.Context(), toolloop.Config{
				Provider: p, Surface: &fakeSurface{}, MaxRounds: 3,
				Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Model != tc.want {
				t.Fatalf("Model = %q, want %q", res.Model, tc.want)
			}
		})
	}
}

func TestExhaustedRoundsMeansTheModelWasStillAsking(t *testing.T) {
	t.Parallel()
	// The distinction matters because the caller may EXTEND the cap. A
	// loop that stopped because the model stopped asking used its last
	// round legitimately and must not be extended; one still asking was
	// truncated.
	asking := llm.Completion{ToolCalls: []llm.ToolCall{toolCall("1", "read")}}
	p := &scriptedProvider{turns: []llm.Completion{asking, asking}}
	s := &fakeSurface{tools: []llm.ToolDef{def("read")}}

	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.ExhaustedRounds {
		t.Error("a loop cut off mid-conversation did not report exhausted rounds")
	}

	// And the counterfactual: the same cap, but the model stops on the
	// last round. Without this the assertion above passes for a backend
	// that always reports exhausted.
	p2 := &scriptedProvider{turns: []llm.Completion{asking, {Content: "done"}}}
	res2, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p2, Surface: &fakeSurface{tools: []llm.ToolDef{def("read")}}, MaxRounds: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res2.ExhaustedRounds {
		t.Error("a loop that used its last round legitimately reports exhausted")
	}
}

func TestATerminatingToolEndsTheLoop(t *testing.T) {
	t.Parallel()
	// A phase whose delivery tool has fired is finished. Letting it keep
	// going spends rounds re-deciding something already done — and, worse,
	// invites a second delivery.
	p := &scriptedProvider{turns: []llm.Completion{
		{ToolCalls: []llm.ToolCall{toolCall("1", "submit_plan")}},
		{ToolCalls: []llm.ToolCall{toolCall("2", "submit_plan")}},
	}}
	s := &fakeSurface{tools: []llm.ToolDef{def("submit_plan")}}

	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5,
		TerminateAfter: []string{"submit_plan"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 1 {
		t.Errorf("rounds = %d, want 1 — the terminator did not stop the loop", res.RoundsUsed)
	}
	if len(s.ran) != 1 {
		t.Errorf("the tool ran %d times, want 1", len(s.ran))
	}
}

// --- the forced tool call --------------------------------------------------

func TestARequiredToolCallIsEnforcedNotRequested(t *testing.T) {
	t.Parallel()
	// Some endpoints ignore tool_choice and some models think-then-stop.
	// Accepting prose as a clean finish is how a forced round silently
	// produces nothing at all.
	p := &scriptedProvider{turns: []llm.Completion{
		{Content: "I think the plan should be..."}, // no tool call
		{ToolCalls: []llm.ToolCall{toolCall("1", "submit_plan")}},
	}}
	s := &fakeSurface{tools: []llm.ToolDef{def("submit_plan")}}

	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5, ToolChoice: "required",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Executions) != 1 {
		t.Fatalf("executions = %+v, want the tool to have run after the correction",
			res.Executions)
	}
	// The correction must name the tools: a model answering with prose has
	// usually misread the surface rather than refused it.
	var corrected bool
	for _, m := range res.Messages {
		if m.Role == llm.RoleUser && strings.Contains(m.Content, "submit_plan") {
			corrected = true
		}
	}
	if !corrected {
		t.Error("the corrective re-prompt did not name the available tools")
	}
}

func TestTheForcedRetryIsBounded(t *testing.T) {
	t.Parallel()
	// A model that can never emit the call must not burn the whole round
	// budget on re-prompts.
	prose := llm.Completion{Content: "still prose"}
	p := &scriptedProvider{turns: []llm.Completion{prose, prose, prose, prose, prose}}
	s := &fakeSurface{tools: []llm.ToolDef{def("submit_plan")}}

	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 10, ToolChoice: "required",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// One initial round plus at most two corrections.
	if res.RoundsUsed > 3 {
		t.Errorf("rounds = %d, want at most 3 — the retry is unbounded", res.RoundsUsed)
	}
	if len(res.Executions) != 0 {
		t.Errorf("executions = %+v, want none", res.Executions)
	}
}

func TestProseWithoutARequiredToolCallIsACleanFinish(t *testing.T) {
	t.Parallel()
	// The counterfactual for the two above: with no forced choice, prose
	// IS the answer and must not be re-prompted.
	p := &scriptedProvider{turns: []llm.Completion{{Content: "the answer"}}}
	s := &fakeSurface{tools: []llm.ToolDef{def("read")}}

	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 1 {
		t.Errorf("rounds = %d, want 1 — an unforced prose answer was re-prompted", res.RoundsUsed)
	}
}

// --- the budget ------------------------------------------------------------

func TestARefusedSpendNamesItsScopeAndStopsTheLoop(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{
		{InputTokens: 60, OutputTokens: 60, ToolCalls: []llm.ToolCall{toolCall("1", "write")}},
	}}
	s := &fakeSurface{tools: []llm.ToolDef{def("write")}}
	m := &meter{refuseAt: 100}

	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5, Budget: m,
	})
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	var be *toolloop.BudgetError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v, want a *BudgetError carrying the refusing scope", err)
	}
	if be.Scope != "role" {
		t.Errorf("scope = %q, want role — the refusal did not name itself", be.Scope)
	}
}

func TestARefusedRoundDoesNotRunItsTools(t *testing.T) {
	t.Parallel()
	// The refusal is the whole point, and tools are where the irreversible
	// things happen. Charging after the side effects makes the cap
	// advisory.
	p := &scriptedProvider{turns: []llm.Completion{
		{InputTokens: 500, ToolCalls: []llm.ToolCall{toolCall("1", "send_email")}},
	}}
	s := &fakeSurface{tools: []llm.ToolDef{def("send_email")}}

	if _, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5, Budget: &meter{refuseAt: 10},
	}); !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if len(s.ran) != 0 {
		t.Errorf("a refused round ran its tools: %v", s.ran)
	}
}

func TestAnUnreachableCounterIsNotARefusal(t *testing.T) {
	t.Parallel()
	// Different answers with opposite consequences: treating unreachable
	// as refused stops every turn in the company on a store blip; treating
	// refused as unreachable spends past the cap.
	p := &scriptedProvider{turns: []llm.Completion{{InputTokens: 10, Content: "hi"}}}
	s := &fakeSurface{}

	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 2,
		Budget: &meter{err: errors.New("store unreachable")},
	})
	if err == nil {
		t.Fatal("an unreachable counter was ignored")
	}
	if errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Error("an unreachable counter was reported as a budget refusal")
	}
}

// --- suspend ---------------------------------------------------------------

func TestASuspendLeavesExactlyOneDanglingToolCall(t *testing.T) {
	t.Parallel()
	// The invariant a resume checks: zero means nothing to resume into,
	// two means the model answers one and strands the other.
	p := &scriptedProvider{turns: []llm.Completion{
		{ToolCalls: []llm.ToolCall{toolCall("call-1", "run_sandbox")}},
	}}
	s := &fakeSurface{
		tools: []llm.ToolDef{def("run_sandbox")},
		results: map[string]toolloop.ToolResult{
			"run_sandbox": {Suspend: true, SuspendPayload: map[string]any{"run": "r1"}},
		},
	}

	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5, AllowSuspend: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Suspended {
		t.Fatal("the loop did not report a suspend")
	}
	if res.PendingToolCallID != "call-1" || res.PendingToolName != "run_sandbox" {
		t.Errorf("pending call = %q/%q, want call-1/run_sandbox",
			res.PendingToolCallID, res.PendingToolName)
	}
	if res.SuspendPayload["run"] != "r1" {
		t.Errorf("payload = %v, want the tool's own", res.SuspendPayload)
	}

	// Exactly one dangling call: the assistant asked and no tool message
	// answered it.
	answered := map[string]bool{}
	asked := map[string]bool{}
	for _, m := range res.Messages {
		for _, c := range m.ToolCalls {
			asked[c.ID] = true
		}
		if m.Role == llm.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	var dangling []string
	for id := range asked {
		if !answered[id] {
			dangling = append(dangling, id)
		}
	}
	if len(dangling) != 1 || dangling[0] != "call-1" {
		t.Errorf("dangling calls = %v, want exactly [call-1]", dangling)
	}
}

func TestASuspendIsRefusedWhereItCannotBeResumed(t *testing.T) {
	t.Parallel()
	// Honouring it in a phase that never persists a partial conversation
	// strands the turn with a call nothing will ever answer. The model
	// still gets an answer, so it sees a refusal rather than silence.
	p := &scriptedProvider{turns: []llm.Completion{
		{ToolCalls: []llm.ToolCall{toolCall("call-1", "run_sandbox")}},
		{Content: "understood"},
	}}
	s := &fakeSurface{
		tools: []llm.ToolDef{def("run_sandbox")},
		results: map[string]toolloop.ToolResult{
			"run_sandbox": {Suspend: true, Output: "launched"},
		},
	}

	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5, AllowSuspend: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Suspended {
		t.Fatal("a phase that cannot resume was allowed to suspend")
	}
	var answered bool
	for _, m := range res.Messages {
		if m.Role == llm.RoleTool && m.ToolCallID == "call-1" {
			answered = true
		}
	}
	if !answered {
		t.Error("the refused suspend left its call unanswered")
	}
	if len(res.Executions) != 1 || !res.Executions[0].Failed {
		t.Errorf("executions = %+v, want one marked failed", res.Executions)
	}
}

// --- the fence -------------------------------------------------------------

func TestTheFenceRunsBeforeAnythingIsSpent(t *testing.T) {
	t.Parallel()
	// A node whose lease moved must stop before it spends tokens and
	// before it fires tools, not after.
	sentinel := errors.New("seat lost")
	p := &scriptedProvider{turns: []llm.Completion{{Content: "hi"}}}
	s := &fakeSurface{}
	m := &meter{}

	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5, Budget: m,
		Fence: func() error { return sentinel },
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the caller's own sentinel unwrapped", err)
	}
	if p.calls != 0 {
		t.Errorf("the provider was called %d times past a closed fence", p.calls)
	}
	if m.spent != 0 {
		t.Errorf("%d tokens were spent past a closed fence", m.spent)
	}
	if len(s.ran) != 0 {
		t.Errorf("tools ran past a closed fence: %v", s.ran)
	}
}

// --- progress --------------------------------------------------------------

func TestProgressIsPublishedTwicePerRound(t *testing.T) {
	t.Parallel()
	// Once the model has spoken — so its reasoning reaches the live view
	// before the round's tools run — and again once they return.
	p := &scriptedProvider{turns: []llm.Completion{
		{Content: "working", ToolCalls: []llm.ToolCall{toolCall("1", "read")}},
		{Content: "done"},
	}}
	s := &fakeSurface{tools: []llm.ToolDef{def("read")}}

	var seen []int // executions visible at each publish
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5,
		OnProgress: func(r toolloop.Result) { seen = append(seen, len(r.Executions)) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Round 1 publishes twice (0 executions, then 1); round 2 publishes
	// once, since it asked for no tools.
	if len(seen) != 3 {
		t.Fatalf("published %d times (%v), want 3", len(seen), seen)
	}
	if seen[0] != 0 {
		t.Errorf("the first publish saw %d executions, want 0 — it did not "+
			"happen before the round's tools ran", seen[0])
	}
	if seen[1] != 1 {
		t.Errorf("the second publish saw %d executions, want 1", seen[1])
	}
	_ = res
}

func TestTheFailureViewCarriesWhatThePhaseManaged(t *testing.T) {
	t.Parallel()
	// A phase that died used to leave only its "started" event, so the
	// dashboard showed an in-flight call with no response and no reason.
	p := &scriptedProvider{
		turns: []llm.Completion{
			{Content: "first", InputTokens: 7, ToolCalls: []llm.ToolCall{toolCall("1", "read")}},
		},
		failAt: 2,
	}
	s := &fakeSurface{tools: []llm.ToolDef{def("read")}}
	prog := &toolloop.Progress{}

	if _, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5, Progress: prog,
	}); err == nil {
		t.Fatal("Run succeeded against a failing provider")
	}

	snap := prog.Snapshot()
	if snap.RoundsUsed != 1 {
		t.Errorf("rounds = %d, want the round it died on", snap.RoundsUsed)
	}
	if len(snap.Executions) != 1 {
		t.Errorf("executions = %+v, want the call that ran before the failure",
			snap.Executions)
	}
	if snap.InputTokens != 7 {
		t.Errorf("input tokens = %d, want the 7 already billed", snap.InputTokens)
	}
	if !strings.Contains(snap.Text, "first") {
		t.Errorf("text = %q, want the conversation so far", snap.Text)
	}
	if snap.ExhaustedRounds {
		t.Error("a phase that DIED reports exhausted rounds — it did not run out, it failed")
	}
}

// --- the response grammar --------------------------------------------------

func TestOneBuilderRendersReasoningAndContentTogether(t *testing.T) {
	t.Parallel()
	// The live row and the record you expand afterwards must be the same
	// text. They were assembled separately once, so a reasoning model
	// streamed its tool calls against an empty response.
	cases := []struct{ reasoning, content, want string }{
		{"", "answer", "answer"},
		{"pondering", "", "<think>pondering</think>"},
		{"pondering", "answer", "<think>pondering</think>\nanswer"},
		{"  ", "  ", ""},
	}
	for _, c := range cases {
		if got := toolloop.FormatReasoningAndContent(c.reasoning, c.content); got != c.want {
			t.Errorf("FormatReasoningAndContent(%q,%q) = %q, want %q",
				c.reasoning, c.content, got, c.want)
		}
	}

	p := &scriptedProvider{turns: []llm.Completion{
		{ReasoningContent: "let me think", Content: "hello"},
	}}
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "<think>let me think</think>\nhello" {
		t.Errorf("text = %q — the loop does not use the shared builder", res.Text)
	}
}

// --- validation ------------------------------------------------------------

func TestRunRefusesAConfigThatCannotWork(t *testing.T) {
	t.Parallel()
	base := toolloop.Config{
		Provider: &scriptedProvider{}, Surface: &fakeSurface{}, MaxRounds: 1,
	}
	for name, mangle := range map[string]func(*toolloop.Config){
		"no provider":  func(c *toolloop.Config) { c.Provider = nil },
		"no surface":   func(c *toolloop.Config) { c.Surface = nil },
		"no rounds":    func(c *toolloop.Config) { c.MaxRounds = 0 },
		"negative cap": func(c *toolloop.Config) { c.MaxRounds = -1 },
	} {
		cfg := base
		mangle(&cfg)
		if _, err := toolloop.Run(t.Context(), cfg); err == nil {
			t.Errorf("%s: Run accepted it", name)
		}
	}
}

func TestASurfaceFailureIsAnEngineFailure(t *testing.T) {
	t.Parallel()
	// A failing TOOL is ordinary — its message goes back to the model. A
	// failing SURFACE is not: the loop cannot form the next round.
	p := &scriptedProvider{turns: []llm.Completion{
		{ToolCalls: []llm.ToolCall{toolCall("1", "read")}},
	}}
	s := &fakeSurface{tools: []llm.ToolDef{def("read")}, surfErr: errors.New("registry gone")}

	if _, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 2,
	}); err == nil {
		t.Fatal("a broken surface was treated as a tool failure")
	}
}

func TestAFailingToolGoesBackToTheModel(t *testing.T) {
	t.Parallel()
	// The counterfactual for the case above, and the behaviour that makes
	// a tool error recoverable: the model is expected to react to it.
	p := &scriptedProvider{turns: []llm.Completion{
		{ToolCalls: []llm.ToolCall{toolCall("1", "read")}},
		{Content: "I see it failed"},
	}}
	s := &fakeSurface{
		tools:   []llm.ToolDef{def("read")},
		results: map[string]toolloop.ToolResult{"read": {Output: "permission denied", Failed: true}},
	}

	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Executions) != 1 || !res.Executions[0].Failed {
		t.Fatalf("executions = %+v, want one marked failed", res.Executions)
	}
	var delivered bool
	for _, m := range res.Messages {
		if m.Role == llm.RoleTool && m.Content == "permission denied" {
			delivered = true
		}
	}
	if !delivered {
		t.Error("the failing tool's output never reached the model")
	}
}

func TestTheSurfaceIsReReadEveryRound(t *testing.T) {
	t.Parallel()
	// A meta-tool that activates another tool must have it visible on the
	// NEXT provider call, not the one after.
	p := &scriptedProvider{turns: []llm.Completion{
		{ToolCalls: []llm.ToolCall{toolCall("1", "activate")}},
		{Content: "done"},
	}}
	s := &growingSurface{fakeSurface: fakeSurface{tools: []llm.ToolDef{def("activate")}}}

	if _, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.seen) < 2 {
		t.Fatalf("only %d provider calls", len(p.seen))
	}
	if len(p.seen[1].Tools) != 2 {
		t.Errorf("round 2 was offered %d tools, want 2 — the surface was not re-read",
			len(p.seen[1].Tools))
	}
}

// growingSurface adds a tool the first time one runs.
type growingSurface struct{ fakeSurface }

func (g *growingSurface) Execute(ctx context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	res, err := g.fakeSurface.Execute(ctx, call)
	if len(g.tools) == 1 {
		g.tools = append(g.tools, def("newly_activated"))
	}
	return res, err
}
