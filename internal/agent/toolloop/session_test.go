package toolloop_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcprelay"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolsession"
)

func TestMain(m *testing.M) {
	mcprelay.DispatchIfRelay()
	os.Exit(m.Run())
}

// --- doubles ---------------------------------------------------------------

// surface answers tool calls from a table, as the phase's tool surface does.
type surface struct {
	mu       sync.Mutex
	tools    []llm.ToolDef
	results  map[string]toolloop.ToolResult
	ran      []string
	detached map[string]bool
	revision uint64
	// activates adds a tool when called.
	activates string
	adds      llm.ToolDef
	// hold blocks a call to holdTool until closed; entered says it began.
	holdTool string
	hold     chan struct{}
	entered  chan struct{}
	// panics names a tool whose call panics.
	panics string
}

func newSurface(names ...string) *surface {
	s := &surface{results: map[string]toolloop.ToolResult{}, detached: map[string]bool{}}
	for _, n := range names {
		s.tools = append(s.tools, llm.ToolDef{Name: n, Description: n,
			Parameters: map[string]any{"type": "object"}})
	}
	return s
}

func (s *surface) ToolDefs() []llm.ToolDef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.tools)
}
func (s *surface) Revision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}
func (s *surface) AnnotationsOf(string) mcp.Annotations { return mcp.Annotations{} }
func (s *surface) Detached(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.detached[name]
}
func (s *surface) Phase() string { return "execute" }

func (s *surface) Execute(_ context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	s.mu.Lock()
	s.ran = append(s.ran, call.Name)
	if call.Name == s.activates {
		s.tools = append(s.tools, s.adds)
		s.revision++
	}
	res, ok := s.results[call.Name]
	hold, entered, holdTool, panics := s.hold, s.entered, s.holdTool, s.panics
	s.mu.Unlock()
	if call.Name == panics {
		panic("a tool fell over")
	}
	if call.Name == holdTool && hold != nil {
		close(entered)
		<-hold
	}
	if !ok {
		res = toolloop.ToolResult{Output: call.Name + " ok"}
	}
	return res, nil
}

func (s *surface) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ran)
}

func host(t *testing.T) *toolsession.Host {
	t.Helper()
	h, err := toolsession.NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func opening() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleSystem, Content: "you are a seat"},
		{Role: llm.RoleUser, Content: "do the task"},
	}
}

func run(t *testing.T, cfg toolloop.Config) (*toolloop.Result, error) {
	t.Helper()
	if cfg.Host == nil {
		cfg.Host = host(t)
	}
	if cfg.Messages == nil {
		cfg.Messages = opening()
	}
	if cfg.MaxRounds == 0 {
		cfg.MaxRounds = 8
	}
	return toolloop.Run(t.Context(), cfg)
}

// --- the cases -------------------------------------------------------------

// A PHASE ENDS AT ITS TERMINATOR, and the CLI makes no request after it: the
// submission's answer is withheld until the run is stopped, so the model never
// reads it and asks again.
//
// Mutation: answer the terminator's call instead of withholding it, and the
// script's response after it is played — a third round on the record.
func TestThePhaseEndsAtItsTerminatorWithNoFurtherRequest(t *testing.T) {
	t.Parallel()
	surf := newSurface("post", "submit_work")
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "posting", Calls: []llmtest.Call{{Name: "post", Args: `{"text":"hi"}`}}},
		{Text: "submitting", Calls: []llmtest.Call{{Name: "submit_work", Args: `{}`}}},
		{Text: "a response made after the phase was decided"},
	}}}}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); !slices.Equal(got, []string{"post", "submit_work"}) {
		t.Errorf("the surface ran %v", got)
	}
	if res.RoundsUsed != 2 || len(res.Rounds) != 2 {
		t.Errorf("rounds used %d (%d recorded), want the two the phase made", res.RoundsUsed, len(res.Rounds))
	}
	if res.ExhaustedRounds {
		t.Error("a phase its terminator ended reads as exhausted")
	}
	if strings.Contains(res.Text, "after the phase was decided") {
		t.Errorf("the text carries a response made after the terminator: %q", res.Text)
	}
	if late := prov.Late(); late != 0 {
		t.Errorf("the CLI made %d model request(s) after the phase was decided", late)
	}
	if len(res.Invocations) != 1 || res.Invocations[0].Outcome != toolloop.OutcomeStopped {
		t.Errorf("invocations %+v, want one the engine stopped", res.Invocations)
	}
	// The conversation pairs each call with its result, in the order made.
	var tools []string
	for _, m := range res.Messages {
		if m.Role == llm.RoleTool {
			tools = append(tools, m.Name)
		}
	}
	if !slices.Equal(tools, []string{"post", "submit_work"}) {
		t.Errorf("the conversation's tool results are %v", tools)
	}
}

// A RUN THAT ENDS IN PROSE WITH ITS TERMINATOR OWED IS ASKED AGAIN, in a new
// run carrying the conversation so far — and the corrective names the call
// that finishes it.
func TestARunEndingInProseIsCorrectedInTheNextRun(t *testing.T) {
	t.Parallel()
	surf := newSurface("submit_work")
	prov := &llmtest.Script{Runs: []llmtest.Run{
		{Responses: []llmtest.Response{{Text: "here is my report: all done"}}},
		{Responses: []llmtest.Response{{Text: "submitting", Calls: []llmtest.Call{{Name: "submit_work"}}}}},
	}}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); !slices.Equal(got, []string{"submit_work"}) {
		t.Fatalf("the surface ran %v, want the submission the corrective asked for", got)
	}
	seen := prov.Seen()
	if len(seen) != 2 {
		t.Fatalf("%d runs, want the declined one and the corrected one", len(seen))
	}
	lastMsg := seen[1].Messages[len(seen[1].Messages)-1]
	if lastMsg.Role != llm.RoleUser || !strings.Contains(lastMsg.Content, "`submit_work`") {
		t.Errorf("the second run opened with %+v, want the corrective naming submit_work", lastMsg)
	}
	if len(res.Narration) == 0 || !res.Narration[0].Declined {
		t.Errorf("the declined response is not marked: %+v", res.Narration)
	}
}

// A DETACHED TOOL PARKS THE PHASE WITH ITS CALL UNANSWERED: exactly one
// dangling call in the conversation, which is what a resume checks.
func TestADetachedToolParksThePhaseWithOneDanglingCall(t *testing.T) {
	t.Parallel()
	surf := newSurface("run_sandbox", "submit_work")
	surf.detached["run_sandbox"] = true
	surf.results["run_sandbox"] = toolloop.ToolResult{Output: "started", Suspend: true,
		SuspendPayload: map[string]any{"job": "j1"}}
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "coding it", Calls: []llmtest.Call{{Name: "run_sandbox", Args: `{"task":"x"}`}}},
		{Text: "never reached"},
	}}}}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, AllowSuspend: true,
		TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Suspended || res.PendingToolName != "run_sandbox" || res.SuspendPayload["job"] != "j1" {
		t.Fatalf("result %+v, want parked on run_sandbox with its payload", res)
	}
	answered := map[string]bool{}
	var calls []string
	for _, m := range res.Messages {
		for _, c := range m.ToolCalls {
			calls = append(calls, c.ID)
		}
		if m.Role == llm.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	dangling := 0
	for _, id := range calls {
		if !answered[id] {
			dangling++
			if id != res.PendingToolCallID {
				t.Errorf("call %s dangles, want only the parked one", id)
			}
		}
	}
	if dangling != 1 {
		t.Errorf("%d dangling calls, want exactly the parked one", dangling)
	}
}

// THE FENCE STOPS THE NEXT CALL: a node whose lease moved runs nothing more,
// and the loop returns the fence's own error.
func TestAClosedFenceStopsTheNextCall(t *testing.T) {
	t.Parallel()
	moved := errors.New("the seat moved")
	surf := newSurface("first", "second")
	var closed sync.Once
	fenced := false
	var mu sync.Mutex
	prov := &llmtest.Script{
		Runs: []llmtest.Run{{Responses: []llmtest.Response{
			{Calls: []llmtest.Call{{Name: "first"}}},
			{Calls: []llmtest.Call{{Name: "second"}}},
		}}},
		Before: func(name string) {
			if name == "second" {
				closed.Do(func() { mu.Lock(); fenced = true; mu.Unlock() })
			}
		},
	}
	fence := func() error {
		mu.Lock()
		defer mu.Unlock()
		if fenced {
			return moved
		}
		return nil
	}
	_, err := run(t, toolloop.Config{Provider: prov, Surface: surf, Fence: fence})
	if !errors.Is(err, moved) {
		t.Fatalf("Run returned %v, want the fence's error", err)
	}
	if got := surf.calls(); !slices.Equal(got, []string{"first"}) {
		t.Errorf("the surface ran %v after the fence closed", got)
	}
}

// A REFUSED SPEND STOPS WHAT FOLLOWS: the response's tokens are recorded, and
// no call it asked for runs.
func TestARefusedSpendRunsNoFurtherCall(t *testing.T) {
	t.Parallel()
	surf := newSurface("write")
	m := &refusingMeter{refuseOver: 150}
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Calls: []llmtest.Call{{Name: "write"}}},
		{Calls: []llmtest.Call{{Name: "write"}}},
	}}}}
	_, err := run(t, toolloop.Config{Provider: prov, Surface: surf, Budget: m})
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("Run returned %v, want the budget's refusal", err)
	}
	if got := surf.calls(); len(got) != 1 {
		t.Errorf("the surface ran %v, want only the call the first response's charge admitted", got)
	}
}

// refusingMeter admits spend up to refuseOver and refuses everything after.
type refusingMeter struct {
	mu         sync.Mutex
	spent      int
	refuseOver int
	refused    bool
}

func (m *refusingMeter) Spend(_ context.Context, tokens int) (toolloop.SpendOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spent += tokens
	if m.spent > m.refuseOver {
		m.refused = true
		return toolloop.SpendOutcome{Scope: "org", Used: m.spent, Limit: m.refuseOver}, nil
	}
	return toolloop.SpendOutcome{OK: true}, nil
}

func (m *refusingMeter) Refused(context.Context) (toolloop.SpendOutcome, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return toolloop.SpendOutcome{Scope: "org", Used: m.spent, Limit: m.refuseOver}, m.refused
}

// THE ROUND BUDGET IS THE MODEL'S REQUESTS: on the last round it allows, the
// calls run and the run ends before the CLI can ask again — exhausted, for the
// caller's judge to extend or rescue.
//
// Mutation: deliver the last round's answers instead of ending the run, and
// the third response's call reaches the surface.
func TestTheRoundBudgetEndsTheRunAfterItsLastRoundsCalls(t *testing.T) {
	t.Parallel()
	surf := newSurface("look")
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Calls: []llmtest.Call{{Name: "look", Args: `{"n":1}`}}},
		{Calls: []llmtest.Call{{Name: "look", Args: `{"n":2}`}}},
		{Calls: []llmtest.Call{{Name: "look", Args: `{"n":3}`}}},
	}}}}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.ExhaustedRounds {
		t.Error("a phase out of rounds with the model still asking is not exhausted")
	}
	if got := surf.calls(); len(got) != 2 {
		t.Errorf("the surface ran %v, want the two rounds the budget allowed", got)
	}
	if res.RoundsUsed != 2 {
		t.Errorf("rounds used %d, want 2", res.RoundsUsed)
	}
	// The last round's result is on the conversation, for the extension the
	// caller may grant to read.
	if last := res.Messages[len(res.Messages)-1]; last.Role != llm.RoleTool {
		t.Errorf("the conversation ends with %+v, want the last round's result", last)
	}
}

// ARGUMENTS THAT DO NOT PARSE ARE ANSWERED, NOT RUN: running the call on an
// empty object is a write the model never asked for.
func TestArgumentsThatDoNotParseAreAnsweredNotRun(t *testing.T) {
	t.Parallel()
	surf := newSurface("post")
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Calls: []llmtest.Call{{Name: "post", Args: `["not", "an object"]`}}},
		{Text: "I will stop"},
	}}}}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); len(got) != 0 {
		t.Fatalf("the surface ran %v on arguments that did not parse", got)
	}
	got := prov.Results()
	if len(got) != 1 || !got[0].Failed || !strings.Contains(got[0].Output, "was NOT run") {
		t.Errorf("the CLI was answered %+v, want the refusal saying why", got)
	}
	if len(res.Executions) != 1 || !res.Executions[0].Failed {
		t.Errorf("executions %+v, want the refused call recorded as failed", res.Executions)
	}
}

// A PERSON'S NOTE ENDS THE RUN AND OPENS THE NEXT AS A USER MESSAGE — never
// inside a tool's result, where any tool's output could imitate one.
func TestANoteEndsTheRunAndOpensTheNextAsAUserMessage(t *testing.T) {
	t.Parallel()
	surf := newSurface("look", "submit_work")
	box := &noteBox{}
	prov := &llmtest.Script{
		Runs: []llmtest.Run{
			{Responses: []llmtest.Response{
				{Calls: []llmtest.Call{{Name: "look"}}},
				{Calls: []llmtest.Call{{Name: "look"}}},
			}},
			{Responses: []llmtest.Response{{Calls: []llmtest.Call{{Name: "submit_work"}}}}},
		},
		Before: func(name string) {
			if name == "look" {
				box.offer("use the staging cluster")
			}
		},
	}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, Steer: box,
		TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := prov.Seen()
	if len(seen) != 2 {
		t.Fatalf("%d runs, want the one the note ended and the one that read it", len(seen))
	}
	var note llm.Message
	for _, m := range seen[1].Messages {
		if strings.Contains(m.Content, "staging cluster") {
			note = m
		}
	}
	if note.Role != llm.RoleUser {
		t.Errorf("the note reached the next run as %+v, want a user message", note)
	}
	for _, r := range prov.Results() {
		if strings.Contains(r.Output, "staging cluster") {
			t.Errorf("the note was put inside a tool's result: %q", r.Output)
		}
	}
	if len(res.Steers) != 1 {
		t.Errorf("steers %+v, want the one note marked", res.Steers)
	}
	if got := surf.calls(); !slices.Equal(got, []string{"look", "submit_work"}) {
		t.Errorf("the surface ran %v, want the run to end after the call the note arrived during", got)
	}
}

type noteBox struct {
	mu      sync.Mutex
	pending []string
	taken   int
}

func (b *noteBox) offer(text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.taken == 0 && len(b.pending) == 0 {
		b.pending = append(b.pending, text)
	}
}

func (b *noteBox) Drain(int) []toolloop.SteerNote {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []toolloop.SteerNote
	for _, p := range b.pending {
		out = append(out, toolloop.SteerNote{ID: "n1", Message: "A note from the founder: " + p})
	}
	b.taken += len(b.pending)
	b.pending = nil
	return out
}

func (b *noteBox) Waiting() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending) > 0
}

// A SUBMISSION ISSUED BESIDE A DELIVERY RUNS AFTER IT: a CLI may issue a
// response's calls at once and in any order, and the submission is judged
// against what the phase delivered.
//
// Mutation: run a terminator as an ordinary call and, issued at once, it can
// run first — the delivery is then refused as after the phase ended.
func TestASubmissionBesideADeliveryRunsAfterIt(t *testing.T) {
	t.Parallel()
	for i := range 20 {
		surf := newSurface("post", "submit_work")
		prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{{
			Parallel: true,
			Calls:    []llmtest.Call{{Name: "submit_work"}, {Name: "post"}},
		}}}}}
		_, err := run(t, toolloop.Config{Provider: prov, Surface: surf, TerminateAfter: []string{"submit_work"}})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got := surf.calls(); !slices.Equal(got, []string{"post", "submit_work"}) {
			t.Fatalf("attempt %d: the surface ran %v, want the delivery before the submission", i, got)
		}
	}
}

// A TOOL THAT PANICS ENDS THE PHASE AS A PANIC, and the process lives: the
// session's call goroutine is no recovery point of the turn's own.
func TestAToolThatPanicsEndsThePhaseAsAPanic(t *testing.T) {
	t.Parallel()
	surf := newSurface("boom")
	surf.panics = "boom"
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Calls: []llmtest.Call{{Name: "boom"}}},
		{Text: "never"},
	}}}}
	_, err := run(t, toolloop.Config{Provider: prov, Surface: surf})
	var perr *turn.PanicError
	if !errors.As(err, &perr) {
		t.Fatalf("Run returned %v, want the panic", err)
	}
}

// A CALL THAT WAITS ON OTHER MODEL RUNS ENDS THE RUN AT ONCE, and the next run
// is handed its real result: the CLI is not held idle, with its slot, for as
// long as the workers take.
func TestALongCallEndsTheRunAndTheNextIsHandedItsResult(t *testing.T) {
	t.Parallel()
	surf := newSurface("delegate", "submit_work")
	surf.results["delegate"] = toolloop.ToolResult{Output: "the workers' answer"}
	prov := &llmtest.Script{Runs: []llmtest.Run{
		{Responses: []llmtest.Response{
			{Calls: []llmtest.Call{{Name: "delegate"}}},
			{Text: "never reached in this run"},
		}},
		{Responses: []llmtest.Response{{Calls: []llmtest.Call{{Name: "submit_work"}}}}},
	}}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf,
		TerminateAfter: []string{"submit_work"}, ParkLocally: []string{"delegate"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if late := prov.Late(); late != 0 {
		t.Errorf("the CLI asked its model again %d time(s) after the long call ended its run", late)
	}
	if strings.Contains(res.Text, "never reached in this run") {
		t.Errorf("the first run went on past the long call: %q", res.Text)
	}
	seen := prov.Seen()
	if len(seen) != 2 {
		t.Fatalf("%d runs, want two", len(seen))
	}
	found := false
	for _, m := range seen[1].Messages {
		if m.Role == llm.RoleTool && m.Content == "the workers' answer" {
			found = true
		}
	}
	if !found {
		t.Errorf("the next run was not handed the long call's real result: %+v", seen[1].Messages)
	}
}

// AN ACTIVATION ON A CLI THAT NEVER RE-LISTS ENDS THE RUN, and the next one
// starts with the new tool listed — it would otherwise never see it.
func TestAnActivationOnACLIThatNeverRelistsEndsTheRun(t *testing.T) {
	t.Parallel()
	surf := newSurface("activate_tool")
	surf.activates = "activate_tool"
	surf.adds = llm.ToolDef{Name: "post", Parameters: map[string]any{"type": "object"}}
	prov := &llmtest.Script{NoRelist: true, Runs: []llmtest.Run{
		{Responses: []llmtest.Response{
			{Calls: []llmtest.Call{{Name: "activate_tool", Args: `{"name":"post"}`}}},
			{Text: "never reached"},
		}},
		{Responses: []llmtest.Response{{Calls: []llmtest.Call{{Name: "post"}}}, {Text: "done"}}},
	}}
	if _, err := run(t, toolloop.Config{Provider: prov, Surface: surf}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); !slices.Equal(got, []string{"activate_tool", "post"}) {
		t.Errorf("the surface ran %v", got)
	}
	if n := len(prov.Seen()); n != 2 {
		t.Errorf("%d runs, want the activation's and the next", n)
	}
}

// A CLI WHOSE RECORD SHOWS NO REQUESTS IS COUNTED A ROUND PER RUN, with the
// run's whole cost on it.
func TestAnUnobservedRunIsOneRound(t *testing.T) {
	t.Parallel()
	surf := newSurface("look")
	prov := &llmtest.Script{Unobserved: true, Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Calls: []llmtest.Call{{Name: "look"}}},
		{Calls: []llmtest.Call{{Name: "look"}}},
		{Text: "all looked"},
	}}}}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 1 || len(res.Rounds) != 1 {
		t.Fatalf("rounds %d (%d recorded), want one for the run", res.RoundsUsed, len(res.Rounds))
	}
	if r := res.Rounds[0]; r.ToolCalls != 2 || r.InputTokens != 300 {
		t.Errorf("round %+v, want both calls and the run's whole input", r)
	}
	if res.Text != "all looked" {
		t.Errorf("text %q, want the run's answer", res.Text)
	}
	if inv := res.Invocations; len(inv) != 1 || inv[0].RequestsObserved {
		t.Errorf("invocations %+v, want one unobserved run", inv)
	}
}

// A RUNNING CALL IS NAMED ON THE LIVE VIEW while it runs, beside any other —
// a CLI may run a response's calls at once.
func TestRunningCallsAreNamedWhileTheyRun(t *testing.T) {
	t.Parallel()
	surf := newSurface("slow", "submit_work")
	surf.holdTool, surf.hold, surf.entered = "slow", make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var frames []toolloop.Result
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Calls: []llmtest.Call{{Name: "slow"}}},
		{Calls: []llmtest.Call{{Name: "submit_work"}}},
	}}}}
	go func() {
		<-surf.entered
		time.Sleep(50 * time.Millisecond)
		close(surf.hold)
	}()
	_, err := run(t, toolloop.Config{Provider: prov, Surface: surf,
		TerminateAfter: []string{"submit_work"},
		OnProgress: func(r toolloop.Result) {
			mu.Lock()
			frames = append(frames, r)
			mu.Unlock()
		}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	named := false
	for _, f := range frames {
		for _, c := range f.Running {
			if c.Name == "slow" && !c.StartedAt.IsZero() {
				named = true
			}
		}
	}
	if !named {
		t.Error("no frame named the call while it ran")
	}
}
