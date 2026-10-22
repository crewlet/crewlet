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
	// failOnce names tools whose FIRST call fails and every later one
	// succeeds.
	failOnce map[string]bool
	// broken makes Execute itself fail — a torn-down turn, not a tool.
	broken error
	// delay is how long every call takes, and origin and server who
	// answers it.
	delay          time.Duration
	origin, server string
	// hook, when set, is called as each call begins, outside the lock.
	hook func(ctx context.Context, call llm.ToolCall)
	// readOnly names tools annotated read-only.
	readOnly map[string]bool
}

// remove takes a tool off what the surface offers, as a surface whose active
// set moved does.
func (s *surface) remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = slices.DeleteFunc(s.tools, func(d llm.ToolDef) bool { return d.Name == name })
	s.revision++
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
func (s *surface) AnnotationsOf(name string) mcp.Annotations {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readOnly[name] {
		return mcp.Annotations{ReadOnly: mcp.Yes}
	}
	return mcp.Annotations{}
}
func (s *surface) Detached(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.detached[name]
}
func (s *surface) Phase() string { return "execute" }

// Admit admits a call to a surface that keeps no call log: it runs as it is.
func (s *surface) Admit(string, map[string]any) toolloop.Admitted { return s.execute }

func (s *surface) execute(ctx context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	s.mu.Lock()
	hook := s.hook
	s.mu.Unlock()
	if hook != nil {
		hook(ctx, call)
	}
	s.mu.Lock()
	s.ran = append(s.ran, call.Name)
	if call.Name == s.activates {
		s.tools = append(s.tools, s.adds)
		s.revision++
	}
	res, ok := s.results[call.Name]
	if s.failOnce[call.Name] {
		delete(s.failOnce, call.Name)
		res, ok = toolloop.ToolResult{Output: "Invalid submission: outcome is required", Failed: true}, true
	}
	hold, entered, holdTool, panics := s.hold, s.entered, s.holdTool, s.panics
	broken, delay, origin, server := s.broken, s.delay, s.origin, s.server
	s.mu.Unlock()
	if broken != nil {
		return toolloop.ToolResult{}, broken
	}
	if call.Name == panics {
		panic("a tool fell over")
	}
	if call.Name == holdTool && hold != nil {
		close(entered)
		<-hold
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if !ok {
		res = toolloop.ToolResult{Output: call.Name + " ok"}
	}
	if res.Origin == "" {
		res.Origin, res.Server = origin, server
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
// reads it and asks again — and a second submission, which is what a model that
// read it would make, never runs.
//
// Mutation: answer the terminator's call instead of withholding it, and the
// script's response after it is played — a third round on the record.
func TestThePhaseEndsAtItsTerminatorWithNoFurtherRequest(t *testing.T) {
	t.Parallel()
	surf := newSurface("post", "submit_work")
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "posting", Calls: []llmtest.Call{{Name: "post", Args: `{"text":"hi"}`}}},
		{Text: "submitting", Calls: []llmtest.Call{{Name: "submit_work", Args: `{}`}}},
		{Text: "a response made after the phase was decided", Calls: []llmtest.Call{{Name: "submit_work"}}},
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
// that finishes it, by a message, never by forcing one. The measured case is a
// submission's ARGUMENTS typed out as a fenced JSON block, which a model
// reasonably takes for a structured answer: the corrective says JSON is not a
// submission either, and the round is marked declined on the record.
func TestARunEndingInProseIsCorrectedInTheNextRun(t *testing.T) {
	t.Parallel()
	surf := newSurface("comment_on_work_item", "submit_work")
	prov := &llmtest.Script{Runs: []llmtest.Run{
		{Responses: []llmtest.Response{{Text: writtenSubmission}}},
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
	if lastMsg.Role != llm.RoleUser || !strings.Contains(lastMsg.Content, "`submit_work`") ||
		!strings.Contains(lastMsg.Content, "JSON included") {
		t.Errorf("the second run opened with %+v, want the corrective naming submit_work", lastMsg)
	}
	if res.RoundsUsed != 2 {
		t.Errorf("rounds = %d, want 2 — the written submission ended the phase", res.RoundsUsed)
	}
	if len(res.Narration) == 0 || res.Narration[0].Round != 1 || !res.Narration[0].Declined {
		t.Errorf("the declined response is not marked: %+v", res.Narration)
	}
	if res.EmptyAnswers != 0 {
		t.Errorf("EmptyAnswers = %d, want 0 — the response said something", res.EmptyAnswers)
	}
}

// A DETACHED TOOL PARKS THE PHASE WITH ITS CALL UNANSWERED: exactly one
// dangling call in the conversation, which is what a resume checks — zero is
// nothing to resume into, two a model that answers one and strands the other —
// and it is the call the CLI named, under the id the resume answers.
func TestADetachedToolParksThePhaseWithOneDanglingCall(t *testing.T) {
	t.Parallel()
	surf := newSurface("run_sandbox", "submit_work")
	surf.detached["run_sandbox"] = true
	surf.results["run_sandbox"] = toolloop.ToolResult{Output: "started", Suspend: true,
		SuspendPayload: map[string]any{"job": "j1"}}
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "coding it", Calls: []llmtest.Call{{ID: "call-1", Name: "run_sandbox", Args: `{"task":"x"}`}}},
		{Text: "never reached"},
	}}}}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, AllowSuspend: true,
		TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Suspended || res.PendingToolName != "run_sandbox" || res.PendingToolCallID != "call-1" ||
		res.SuspendPayload["job"] != "j1" {
		t.Fatalf("result %+v, want parked on run_sandbox call-1 with its payload", res)
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
	// THE CALL THAT PARKED THE PHASE RAN, so it is on the record like any
	// other call — only its answer is left open. A record without it says
	// nothing started the run the resume collects.
	var parked *toolloop.Execution
	for i := range res.Executions {
		if res.Executions[i].CallID == res.PendingToolCallID {
			parked = &res.Executions[i]
		}
	}
	if parked == nil || parked.Name != "run_sandbox" || parked.Output != "started" {
		t.Errorf("executions %+v, want the parked run_sandbox call with what it returned", res.Executions)
	}
}

// THE FENCE STOPS THE NEXT CALL: a node whose lease moved runs nothing more,
// and the loop returns the fence's own error. A response is ONE model request
// and MANY calls, and the calls are what reach outside the engine, so the
// fence closing between two calls of one response stops the rest of it too —
// the chat post, the tracker write and the sandbox launch the node would
// otherwise run on a seat a peer now holds.
func TestAClosedFenceStopsTheNextCall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		responses []llmtest.Response
	}{
		{"between responses", []llmtest.Response{
			{Calls: []llmtest.Call{{Name: "first"}}},
			{Calls: []llmtest.Call{{Name: "second"}}},
		}},
		{"between one response's calls", []llmtest.Response{
			{Calls: []llmtest.Call{{Name: "first"}, {Name: "second"}, {Name: "second", Args: `{"n":2}`}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			moved := errors.New("the seat moved")
			surf := newSurface("first", "second")
			var mu sync.Mutex
			fenced := false
			// The lease moves while the first call runs.
			surf.hook = func(_ context.Context, call llm.ToolCall) {
				if call.Name == "first" {
					mu.Lock()
					fenced = true
					mu.Unlock()
				}
			}
			fence := func() error {
				mu.Lock()
				defer mu.Unlock()
				if fenced {
					return moved
				}
				return nil
			}
			prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: tc.responses}}}
			_, err := run(t, toolloop.Config{Provider: prov, Surface: surf, Fence: fence})
			if !errors.Is(err, moved) {
				t.Fatalf("Run returned %v, want the fence's error unwrapped", err)
			}
			if got := surf.calls(); !slices.Equal(got, []string{"first"}) {
				t.Errorf("the surface ran %v after the fence closed", got)
			}
			if late := prov.Late(); late != 0 {
				t.Errorf("the CLI asked its model %d time(s) after the fence closed", late)
			}
		})
	}
}

// THE FENCE RUNS BEFORE ANYTHING IS SPENT: a node whose lease moved stops before
// it starts a CLI — no tokens, no tools.
func TestTheFenceRunsBeforeAnythingIsSpent(t *testing.T) {
	t.Parallel()
	lost := errors.New("seat lost")
	m := &refusingMeter{refuseOver: 1 << 30}
	prov := &llmtest.Script{}
	surf := newSurface("read")
	_, err := run(t, toolloop.Config{Provider: prov, Surface: surf, Budget: m,
		Fence: func() error { return lost }})
	if !errors.Is(err, lost) {
		t.Fatalf("err = %v, want the caller's own sentinel unwrapped", err)
	}
	if n := len(prov.Seen()); n != 0 {
		t.Errorf("the CLI was started %d times past a closed fence", n)
	}
	if m.spent != 0 || len(surf.calls()) != 0 {
		t.Errorf("%d tokens spent and %v run past a closed fence", m.spent, surf.calls())
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
// empty object is a write the model never asked for. The model reads why, as a
// FAILED result — the flag rides the tool message, which a backend renders as a
// failure — and its corrected call runs; the response's other call runs as
// normal.
func TestArgumentsThatDoNotParseAreAnsweredNotRun(t *testing.T) {
	t.Parallel()
	surf := newSurface("post", "read")
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Calls: []llmtest.Call{{ID: "bad", Name: "post", Args: `["not", "an object"]`}, {Name: "read"}}},
		{Calls: []llmtest.Call{{Name: "post", Args: `{"body":"x"}`}}},
		{Text: "done"},
	}}}}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); !slices.Equal(got, []string{"read", "post"}) {
		t.Fatalf("the surface ran %v, want the unparsed call skipped and its retry run", got)
	}
	got := prov.Results()
	if len(got) != 3 || !got[0].Failed || !strings.Contains(got[0].Output, "was NOT run") {
		t.Errorf("the CLI was answered %+v, want the refusal saying why", got)
	}
	if first := res.Executions[0]; first.Name != "post" || !first.Failed ||
		!strings.Contains(first.Output, "is a list where an object belongs") || !first.StartedAt.IsZero() {
		t.Errorf("first execution %+v, want a failed, untimed row naming why", first)
	}
	answer := toolResults(res.Messages)["bad"]
	if !answer.Failed || !strings.Contains(answer.Content, "NOT run") {
		t.Errorf("the conversation answers the call %+v, want a failed result saying it did not run", answer)
	}
}

// ARGUMENTS A CLI REPLACED ARE ANSWERED, NOT RUN, TOO. Claude Code calls its
// server with an array's elements as an object — the model wrote `["a","b"]`
// and the server reads `{"0":"a","1":"b"}` — so the session reads a perfectly
// good object, and only the CLI's record, which keeps the array, shows that
// the model never wrote it. Read from there, the call is refused exactly as
// one the session could not read.
func TestArgumentsTheCLIReplacedAreAnsweredNotRun(t *testing.T) {
	t.Parallel()
	surf := newSurface("post")
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Calls: []llmtest.Call{{Name: "post", Args: `["a", "b"]`, Handed: `{"0":"a","1":"b"}`}}},
		{Calls: []llmtest.Call{{Name: "post", Args: `{"body":"x"}`}}},
		{Text: "done"},
	}}}}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); len(got) != 1 {
		t.Fatalf("the surface ran %v, want only the corrected call", got)
	}
	got := prov.Results()
	if len(got) != 2 || !got[0].Failed || !strings.Contains(got[0].Output, "was NOT run") {
		t.Errorf("the CLI was answered %+v, want the refusal saying why", got)
	}
	if first := res.Executions[0]; !first.Failed || !strings.Contains(first.Output, "not one JSON object") ||
		!first.StartedAt.IsZero() {
		t.Errorf("first execution %+v, want a failed row naming why", first)
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

// A SUBMISSION IS JUDGED AFTER THE DELIVERY BESIDE IT, AND THE DELIVERY RUNS IN
// EITHER ORDER — on a CLI that issues a response's calls at once and on one
// that waits on each answer before its next call. Issued together, the
// submission waits for the delivery, whatever order the model wrote them in, so
// it is judged against what the phase delivered. Issued in turn, the
// submission's answer goes back so the delivery is made — the model asked for
// it in the same breath — and the run ends after it. Either way both are run
// and recorded, the phase is finished, and the CLI asks its model nothing more.
//
// The cases issued at once race, so each is tried repeatedly.
//
// Mutation: run a terminator as an ordinary call and, issued at once, it can
// run first; end the run at the submission and, issued in turn, the delivery
// after it is never made.
func TestASubmissionBesideADeliveryRunsAfterIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		parallel bool
		order    []string
		ran      []string
	}{
		{"at once, submission first", true, []string{"submit_work", "post"}, []string{"post", "submit_work"}},
		{"at once, delivery first", true, []string{"post", "submit_work"}, []string{"post", "submit_work"}},
		{"in turn, delivery first", false, []string{"post", "submit_work"}, []string{"post", "submit_work"}},
		{"in turn, submission first", false, []string{"submit_work", "post"}, []string{"submit_work", "post"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			attempts := 1
			if tc.parallel {
				attempts = 20
			}
			for i := range attempts {
				surf := newSurface("post", "submit_work")
				r := llmtest.Response{Parallel: tc.parallel}
				for _, n := range tc.order {
					r.Calls = append(r.Calls, llmtest.Call{Name: n})
				}
				prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
					r, {Text: "a request made after the phase was decided"},
				}}}}
				res, err := run(t, toolloop.Config{Provider: prov, Surface: surf,
					TerminateAfter: []string{"submit_work"}})
				if err != nil {
					t.Fatalf("attempt %d: Run: %v", i, err)
				}
				if got := surf.calls(); !slices.Equal(got, tc.ran) {
					t.Fatalf("attempt %d: the surface ran %v, want %v", i, got, tc.ran)
				}
				if res.ExhaustedRounds || res.RoundsUsed != 1 || len(prov.Seen()) != 1 {
					t.Errorf("attempt %d: rounds %d over %d runs (exhausted %v), want the one round, finished",
						i, res.RoundsUsed, len(prov.Seen()), res.ExhaustedRounds)
				}
				if late := prov.Late(); late != 0 {
					t.Errorf("attempt %d: the CLI asked its model %d time(s) after the phase was decided", i, late)
				}
				for id, m := range toolResults(res.Messages) {
					if m.Failed {
						t.Errorf("attempt %d: call %s (%s) is recorded as not run: %q", i, id, m.Name, m.Content)
					}
				}
			}
		})
	}
}

// A CALL THAT MUST RUN ALONE WAITS ONLY FOR CALLS THE CLI COULD BE MAKING
// BESIDE IT. A CLI that makes one call at a time — or, beside anything but a
// read-only tool, one at a time — sends the response's next call only once this
// one is answered, so holding a submission for a sibling listed after it holds
// it for the whole of the join wait and gains nothing; while a read-only
// submission beside a read-only call sent at once still waits for that call to
// run first.
func TestACallThatRunsAloneWaitsOnlyForCallsThatCanArriveBesideIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		concurrency llm.CallConcurrency
		parallel    bool
		readOnly    []string
		ran         []string
		waits       bool
	}{
		{"one at a time", llm.CallsInTurn, false, nil, []string{"submit_work", "post"}, false},
		{"read-only side by side, neither read-only", llm.CallsReadOnlyAtOnce, false, nil,
			[]string{"submit_work", "post"}, false},
		{"read-only side by side, both read-only, sent at once", llm.CallsReadOnlyAtOnce, true,
			[]string{"submit_work", "post"}, []string{"post", "submit_work"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			attempts := 1
			if tc.parallel {
				attempts = 20
			}
			for i := range attempts {
				surf := newSurface("post", "submit_work")
				surf.readOnly = map[string]bool{}
				for _, n := range tc.readOnly {
					surf.readOnly[n] = true
				}
				prov := &llmtest.Script{Concurrency: tc.concurrency, Runs: []llmtest.Run{{Responses: []llmtest.Response{
					{Parallel: tc.parallel, Calls: []llmtest.Call{{Name: "submit_work"}, {Name: "post"}}},
					{Text: "a request made after the phase was decided"},
				}}}}
				began := time.Now()
				if _, err := run(t, toolloop.Config{Provider: prov, Surface: surf,
					TerminateAfter: []string{"submit_work"}}); err != nil {
					t.Fatalf("attempt %d: Run: %v", i, err)
				}
				took := time.Since(began)
				if got := surf.calls(); !slices.Equal(got, tc.ran) {
					t.Fatalf("attempt %d: the surface ran %v, want %v", i, got, tc.ran)
				}
				if !tc.waits && took >= toolloop.JoinWait {
					t.Errorf("attempt %d: the run took %s — the submission was held for a call the "+
						"CLI makes only once it is answered", i, took)
				}
			}
		})
	}
}

// A CALL'S ID IS READ UNDER THE KEY ITS CLI DECLARED AND NO OTHER: a CLI that
// puts something else in `_meta` under a key another CLI uses for ids is not
// read as naming a call. Played on a CLI whose record shows no requests, so the
// `_meta` is the only place an id could come from — one joined to a record
// takes the record's.
func TestACallsIDIsReadOnlyUnderTheKeyItsCLIDeclared(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, declared string
		read           bool
	}{
		{"declared", llmtest.CallIDKey, true},
		{"another key declared", "some/otherKey", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			surf := newSurface("post")
			prov := &llmtest.Script{CallIDMeta: tc.declared, Unobserved: true, Runs: []llmtest.Run{{Responses: []llmtest.Response{
				{Calls: []llmtest.Call{{ID: "cli-7", Name: "post"}}},
				{Text: "done"},
			}}}}
			res, err := run(t, toolloop.Config{Provider: prov, Surface: surf})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(res.Executions) != 1 {
				t.Fatalf("executions %+v, want the one call", res.Executions)
			}
			if got := res.Executions[0].CallID == "cli-7"; got != tc.read {
				t.Errorf("the call is recorded as %q; read the CLI's id = %v, want %v",
					res.Executions[0].CallID, got, tc.read)
			}
		})
	}
}

// THE CLI'S WAIT ENDS WITH ITS ANSWER: a call answered "still running" goes on,
// but the CLI has its answer and is working — or has been stopped — so the
// backend is told the run's last call ended then, not when the call returns.
// Told late, the clock that bounds the CLI's own work stayed stopped, and its
// login unchecked, for as long as the parked call ran.
func TestARunStopsWaitingOnACallItWasAnsweredStillRunning(t *testing.T) {
	t.Parallel()
	surf := newSurface("delegate")
	surf.holdTool, surf.hold, surf.entered = "delegate", make(chan struct{}), make(chan struct{})
	busy := make(chan bool, 8)
	prov := &llmtest.Script{
		OnCall: func(_ int, b bool) { busy <- b },
		Runs: []llmtest.Run{
			{Responses: []llmtest.Response{{Calls: []llmtest.Call{{Name: "delegate"}}}, {Text: "never"}}},
			{Responses: []llmtest.Response{{Text: "done"}}},
		},
	}
	done := make(chan error, 1)
	go func() {
		_, err := run(t, toolloop.Config{Provider: prov, Surface: surf, ParkLocally: []string{"delegate"}})
		done <- err
	}()
	<-surf.entered
	if b := <-busy; !b {
		t.Fatal("the first call was not announced as started")
	}
	select {
	case b := <-busy:
		if b {
			t.Fatal("a second start was announced with no second call")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run was never told its call ended, though the CLI was answered still running")
	}
	close(surf.hold)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// A REQUEST'S ID NAMES IT WITHIN ITS RUN ONLY: a CLI's ids are its own, unique
// within one process at most, and a backend that numbers a CLI's requests
// itself numbers each run's from one. Read across runs, the next run's first
// request was merged into the last run's, and the phase lost a round.
func TestARequestOfOneRunIsNeverReadAsAnotherRunsRequest(t *testing.T) {
	t.Parallel()
	surf := newSurface("delegate", "post")
	surf.results["delegate"] = toolloop.ToolResult{Output: "the workers' answer"}
	prov := &llmtest.Script{NumberedRequests: true, Runs: []llmtest.Run{
		{Responses: []llmtest.Response{{Text: "delegating", Calls: []llmtest.Call{{Name: "delegate"}}}, {Text: "never"}}},
		{Responses: []llmtest.Response{{Text: "posting", Calls: []llmtest.Call{{Name: "post"}}}, {Text: "done"}}},
	}}
	began := time.Now()
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, ParkLocally: []string{"delegate"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 3 || len(res.Rounds) != 3 {
		t.Fatalf("rounds used %d, rounds %+v; want the three requests of the two runs", res.RoundsUsed, res.Rounds)
	}
	for _, ex := range res.Executions {
		if ex.Name == "post" && ex.Round != 2 {
			t.Errorf("the second run's call is on round %d, want 2 — the request that asked for it", ex.Round)
		}
	}
	if took := time.Since(began); took >= toolloop.JoinWait {
		t.Errorf("the run took %s: the second run's call waited for a request it could not find", took)
	}
}

// A CLI WHOSE RECORD SHOWS CALLS ONLY AFTER THEY RAN IS NEVER WAITED FOR, and
// its calls still land on the request that asked for them. Gemini CLI and
// OpenCode write a response down once its calls are answered, so holding a
// call for its response's record held every call for the whole join wait.
func TestACLIThatRecordsCallsLateIsNotWaitedFor(t *testing.T) {
	t.Parallel()
	surf := newSurface("search", "post")
	prov := &llmtest.Script{LateRecord: true, Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "looking", Calls: []llmtest.Call{{Name: "search"}}},
		{Text: "posting", Calls: []llmtest.Call{{Name: "post"}}},
		{Text: "done"},
	}}}}
	began := time.Now()
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if took := time.Since(began); took >= toolloop.JoinWait {
		t.Errorf("the run took %s: its calls waited for records that come only after them", took)
	}
	if res.RoundsUsed != 3 || len(res.Rounds) != 3 {
		t.Fatalf("rounds used %d, rounds %+v; want the three requests", res.RoundsUsed, res.Rounds)
	}
	want := map[string]int{"search": 1, "post": 2}
	for _, ex := range res.Executions {
		if ex.Round != want[ex.Name] {
			t.Errorf("%s is on round %d, want %d — the request that asked for it", ex.Name, ex.Round, want[ex.Name])
		}
	}
}

// A CONCURRENCY NOTHING CAN WAIT ON IS REFUSED, rather than read as one of the
// three: a profile the backend should have refused reaches no session.
func TestAnUnknownCallConcurrencyIsRefused(t *testing.T) {
	t.Parallel()
	prov := &llmtest.Script{Concurrency: "sometimes"}
	_, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("post")})
	if err == nil || !strings.Contains(err.Error(), "sometimes") {
		t.Fatalf("Run returned %v, want the unknown concurrency named", err)
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
