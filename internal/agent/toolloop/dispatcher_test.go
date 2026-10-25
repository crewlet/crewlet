package toolloop_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// What the dispatcher holds that a CLI's own loop could otherwise break: calls
// that arrive together, calls the model never made as such, a CLI that gives
// up or dies under a call, answers withheld from a run that is ending, and the
// round budget's last round.

// toolResults is every tool message of a conversation, by the call it answers.
func toolResults(msgs []llm.Message) map[string]llm.Message {
	out := map[string]llm.Message{}
	for _, m := range msgs {
		if m.Role == llm.RoleTool {
			out[m.ToolCallID] = m
		}
	}
	return out
}

// dangling is every call of a conversation no tool message answers.
func dangling(msgs []llm.Message) []string {
	answered := toolResults(msgs)
	var out []string
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if _, ok := answered[c.ID]; !ok {
				out = append(out, c.ID)
			}
		}
	}
	return out
}

// A CALL THE CLI GAVE UP ON STILL RUNS OUT UNDER THE PHASE, AND IS RECORDED. The
// CLI's cancel abandons only the delivery: the tool is not cut short half-way
// through a write — its context is the phase's, which the CLI cannot cancel —
// and its real result is on the record the next run reads.
func TestACallTheCLIGaveUpOnRunsOutUnderThePhase(t *testing.T) {
	t.Parallel()
	surf := newSurface("slow")
	surf.holdTool, surf.hold, surf.entered = "slow", make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var ctxErr error = errors.New("never read")
	surf.hook = func(ctx context.Context, call llm.ToolCall) {
		go func() {
			<-surf.entered
			time.Sleep(150 * time.Millisecond) // well past the CLI's give-up
			mu.Lock()
			ctxErr = ctx.Err()
			mu.Unlock()
			close(surf.hold)
		}()
	}
	prov := script(runOf(
		llmtest.Response{Calls: []llmtest.Call{{Name: "slow", GiveUp: 30 * time.Millisecond}}},
		says("it timed out"),
	))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := prov.Results(); len(got) != 1 || got[0].Err == nil {
		t.Fatalf("the CLI was answered %+v, want it to have given up", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if ctxErr != nil {
		t.Errorf("the tool's context ended with the CLI's cancel: %v", ctxErr)
	}
	if len(res.Executions) != 1 || res.Executions[0].Output != "slow ok" || res.Executions[0].Failed {
		t.Errorf("executions %+v, want the call's real result recorded", res.Executions)
	}
	var result string
	for _, m := range res.Messages {
		if m.Role == llm.RoleTool {
			result = m.Content
		}
	}
	if result != "slow ok" {
		t.Errorf("the conversation answers the call %q, want its real result", result)
	}
}

// A CLI THAT DIES UNDER A CALL FAILS ITS RUN, AND THE CALL STILL RUNS OUT AND IS
// RECORDED: the EOF abandons the delivery and nothing else. The phase reports
// the run's failure with the call's real result on its failure record.
func TestACallTheCLIDiedUnderRunsOutAndIsRecorded(t *testing.T) {
	t.Parallel()
	surf := newSurface("slow")
	surf.holdTool, surf.hold, surf.entered = "slow", make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var ctxErr error = errors.New("never read")
	surf.hook = func(ctx context.Context, call llm.ToolCall) {
		go func() {
			<-surf.entered
			time.Sleep(150 * time.Millisecond)
			mu.Lock()
			ctxErr = ctx.Err()
			mu.Unlock()
			close(surf.hold)
		}()
	}
	prog := &toolloop.Progress{}
	prov := script(runOf(
		llmtest.Response{Calls: []llmtest.Call{{Name: "slow", GiveUp: 30 * time.Millisecond, Exit: true}}},
		says("never"),
	))
	_, err := run(t, toolloop.Config{Provider: prov, Surface: surf, Progress: prog})
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("Run returned %v, want the run's failure", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if ctxErr != nil {
		t.Errorf("the tool's context ended with the CLI: %v", ctxErr)
	}
	snap := prog.Snapshot()
	if len(snap.Executions) != 1 || snap.Executions[0].Output != "slow ok" {
		t.Errorf("the failure record holds %+v, want the call's real result", snap.Executions)
	}
	if inv := snap.Invocations; len(inv) != 1 || inv[0].Outcome != toolloop.OutcomeFailed {
		t.Errorf("invocations %+v, want the one run, failed", inv)
	}
}

// AN ANSWER WITHHELD FROM AN ENDING RUN REACHES THE NEXT RUN. A run ended for a
// reason the next one answers — a person's note, a tool its CLI could not see —
// never handed its CLI the result of the call that ended it, so the model never
// read it there; the next run is handed it, as that call's answer.
func TestAWithheldAnswerReachesTheNextRun(t *testing.T) {
	t.Parallel()
	t.Run("a note", func(t *testing.T) {
		t.Parallel()
		box := &stepBox{log: &stepLog{}}
		surf := newSurface("look")
		surf.results["look"] = toolloop.ToolResult{Output: "the real result"}
		surf.hook = offerDuring(box, "look", "the note")
		prov := script(runOf(calling("", "look"), says("never")), runOf(says("done")))
		if _, err := run(t, toolloop.Config{Provider: prov, Surface: surf, Steer: box}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		withheldAndHanded(t, prov, "the real result")
	})
	t.Run("an activation the CLI cannot see", func(t *testing.T) {
		t.Parallel()
		surf := newSurface("activate_tool")
		surf.activates = "activate_tool"
		surf.adds = llm.ToolDef{Name: "post", Parameters: map[string]any{"type": "object"}}
		surf.results["activate_tool"] = toolloop.ToolResult{Output: "the real result"}
		prov := script(runOf(calling("", "activate_tool"), says("never")), runOf(says("done")))
		prov.NoRelist = true
		if _, err := run(t, toolloop.Config{Provider: prov, Surface: surf}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		withheldAndHanded(t, prov, "the real result")
	})
}

// withheldAndHanded checks the first run's CLI never read result and the second
// run was handed it.
func withheldAndHanded(t *testing.T, prov *llmtest.Script, result string) {
	t.Helper()
	for _, r := range prov.Results() {
		if r.Output == result {
			t.Errorf("the ending run's CLI read the result: %+v", r)
		}
	}
	if late := prov.Late(); late != 0 {
		t.Errorf("the CLI asked its model %d time(s) after its run was ended", late)
	}
	seen := prov.Seen()
	if len(seen) != 2 {
		t.Fatalf("%d runs, want the ended one and the next", len(seen))
	}
	found := false
	for _, m := range seen[1].Messages {
		if m.Role == llm.RoleTool && m.Content == result && !m.Failed {
			found = true
		}
	}
	if !found {
		t.Errorf("the next run was not handed the call's result: %+v", seen[1].Messages)
	}
}

// THE LAST ROUND THE BUDGET ALLOWS, WITH ITS CALLS RUN AT ONCE: every call of it
// runs and is recorded on it, and the run ends once the last of them returns,
// before the CLI can ask its model again — exhausted, for the caller's judge.
func TestTheLastRoundsParallelCallsAllRunAndTheRunEnds(t *testing.T) {
	t.Parallel()
	surf := newSurface("a", "b", "c", "d")
	prov := script(runOf(
		calling("", "a"),
		llmtest.Response{Parallel: true, Calls: []llmtest.Call{{Name: "b"}, {Name: "c"}}},
		calling("", "d"),
	))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ran := surf.calls()
	slices.Sort(ran)
	if !slices.Equal(ran, []string{"a", "b", "c"}) {
		t.Errorf("the surface ran %v, want the two rounds' calls and nothing of a third", ran)
	}
	if late := prov.Late(); late != 0 {
		t.Errorf("the CLI asked its model %d time(s) past the budget", late)
	}
	if !res.ExhaustedRounds || res.RoundsUsed != 2 {
		t.Errorf("rounds %d exhausted %v, want 2 and exhausted", res.RoundsUsed, res.ExhaustedRounds)
	}
	onLast := 0
	for _, e := range res.Executions {
		if e.Round == 2 {
			onLast++
		}
	}
	if onLast != 2 {
		t.Errorf("executions %+v, want both of round 2's calls on it", res.Executions)
	}
	if d := dangling(res.Messages); len(d) != 0 {
		t.Errorf("calls %v left unanswered on the record", d)
	}
}

// lagging reports the run's records in order, but only once a lag has passed
// from the first — a backend reading a session file the CLI flushes late — and
// every one of them before the run is over.
type lagging struct {
	*llmtest.Script
	by time.Duration
}

type laggingHost struct {
	llm.ToolHost
	queue chan llm.Turn
}

func (h laggingHost) Observe(t llm.Turn) { h.queue <- t }

func (l *lagging) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	h := laggingHost{ToolHost: req.Tools, queue: make(chan llm.Turn, 64)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(l.by)
		for t := range h.queue {
			h.ToolHost.Observe(t)
		}
	}()
	req.Tools = h
	comp, err := l.Script.Complete(ctx, req)
	close(h.queue)
	<-done
	return comp, err
}

// A SUSPEND IS REFUSED WHERE IT CANNOT BE RESUMED. Honouring it in a phase that
// never persists a partial conversation strands the turn with a call nothing
// will ever answer, so the model is answered a refusal — and the record says
// exactly what the CLI was told, failed, so the two never disagree about it.
func TestASuspendIsRefusedWhereItCannotBeResumed(t *testing.T) {
	t.Parallel()
	surf := newSurface("run_sandbox")
	surf.detached["run_sandbox"] = true
	surf.results["run_sandbox"] = toolloop.ToolResult{Output: "launched", Suspend: true}
	prov := script(runOf(
		llmtest.Response{Calls: []llmtest.Call{{ID: "call-1", Name: "run_sandbox"}}},
		says("understood"),
	))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, AllowSuspend: false})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Suspended {
		t.Fatal("a phase that cannot resume was allowed to suspend")
	}
	got := prov.Results()
	if len(got) != 1 || !got[0].Failed || !strings.Contains(got[0].Output, "cannot suspend") {
		t.Fatalf("the CLI was answered %+v, want the refusal", got)
	}
	answer, ok := toolResults(res.Messages)["call-1"]
	if !ok || answer.Content != got[0].Output || !answer.Failed {
		t.Errorf("the conversation answers the call %+v, want what the CLI was told: %+v", answer, got[0])
	}
	if len(res.Executions) != 1 || !res.Executions[0].Failed || res.Executions[0].Output != got[0].Output {
		t.Errorf("executions = %+v, want the refusal the CLI was told, failed", res.Executions)
	}
}

// halting is a backend that, as a fallback chain does, asks the host after its
// run whether another member may continue it.
type halting struct {
	*llmtest.Script
	mu     sync.Mutex
	halted []bool
}

func (h *halting) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	comp, err := h.Script.Complete(ctx, req)
	h.mu.Lock()
	h.halted = append(h.halted, req.Tools.Halted())
	h.mu.Unlock()
	return comp, err
}

// A RUN THAT FAILED AFTER IT DECIDED THE PHASE'S END IS HALTED. A submission
// that ran finished the phase even while the rest of its response was still to
// come; a CLI that died before making it leaves that end standing, and no other
// member of a chain may continue the run — it would submit again. A run that
// failed with nothing decided is not halted: the next member continues it.
func TestARunThatFailedAfterDecidingIsHalted(t *testing.T) {
	t.Parallel()
	died := errors.New("the CLI died")
	t.Run("decided", func(t *testing.T) {
		t.Parallel()
		surf := newSurface("post", "submit_work")
		// The model asks for a delivery beside its submission, and the CLI
		// dies before the delivery reaches the engine.
		prov := &halting{Script: script(llmtest.Run{Responses: []llmtest.Response{{Calls: []llmtest.Call{
			{Name: "submit_work"}, {Name: "post", Sent: "unreachable"},
		}}}, Err: died})}
		res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, TerminateAfter: []string{"submit_work"}})
		if err != nil {
			t.Fatalf("Run: %v — the phase finished before its CLI died", err)
		}
		if !slices.Equal(prov.halted, []bool{true}) {
			t.Errorf("halted %v, want the run halted once the submission ran", prov.halted)
		}
		if res.ExhaustedRounds || !slices.Contains(surf.calls(), "submit_work") {
			t.Errorf("result %+v, want the phase finished", res)
		}
	})
	t.Run("undecided", func(t *testing.T) {
		t.Parallel()
		prov := &halting{Script: script(llmtest.Run{Responses: []llmtest.Response{calling("", "post")}, Err: died})}
		_, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("post")})
		if !errors.Is(err, died) {
			t.Fatalf("Run returned %v, want the run's failure", err)
		}
		if !slices.Equal(prov.halted, []bool{false}) {
			t.Errorf("halted %v, want a failed run with nothing decided left to the chain", prov.halted)
		}
	})
}

// pending is a backend that, as a fallback chain does when a member fails, reads
// what the run did so the next member can be shown it.
type pending struct {
	*llmtest.Script
	mu   sync.Mutex
	read [][]llm.Message
}

func (p *pending) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	comp, err := p.Script.Complete(ctx, req)
	p.mu.Lock()
	p.read = append(p.read, req.Tools.Pending())
	p.mu.Unlock()
	return comp, err
}

// PENDING IS WHAT THE RUN DID, for a fallback member to be shown rather than to
// do again: each response the record showed, with the calls it made and their
// results — and nothing the loop already folded into the conversation.
func TestPendingIsWhatTheRunDid(t *testing.T) {
	t.Parallel()
	died := errors.New("the CLI died")
	prov := &pending{Script: script(
		runOf(says(writtenSubmission)),
		llmtest.Run{Responses: []llmtest.Response{
			{Text: "posting", Calls: []llmtest.Call{{ID: "p1", Name: "post", Args: `{"text":"hi"}`}}},
		}, Err: died},
	)}
	_, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("post", "submit_work"),
		TerminateAfter: []string{"submit_work"}})
	if !errors.Is(err, died) {
		t.Fatalf("Run returned %v, want the second run's failure", err)
	}
	if len(prov.read) != 2 {
		t.Fatalf("read %d times, want once per run", len(prov.read))
	}
	got := prov.read[1]
	if len(got) != 2 || got[0].Role != llm.RoleAssistant || got[0].Content != "posting" ||
		len(got[0].ToolCalls) != 1 || got[0].ToolCalls[0].ID != "p1" ||
		got[1].Role != llm.RoleTool || got[1].ToolCallID != "p1" || got[1].Content != "post ok" {
		t.Errorf("pending = %+v, want the second run's response and its call's result alone", got)
	}
}
