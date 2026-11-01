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

// What the dispatcher holds that a harness's own loop could otherwise break: calls
// that arrive together, calls the model never made as such, a harness that gives
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

// TWO DETACHED CALLS IN ONE RESPONSE PARK THE PHASE ON ONE. Each starts work a
// parked phase answers for only one of — two changes, each waiting on a
// person's approval — so they run alone, the first parks, and the second is
// never run — answered as not run, so the conversation holds exactly one
// dangling call, which is the invariant a resume checks.
//
// Issued at once, the two race for the gate, so that case is tried repeatedly:
// a park decided only after the gate is let go admits the second now and then.
func TestTwoDetachedCallsInOneResponseParkOnOne(t *testing.T) {
	t.Parallel()
	cases := []bool{false}
	for range 20 {
		cases = append(cases, true)
	}
	for _, parallel := range cases {
		surf := newSurface("propose_change")
		surf.detached["propose_change"] = true
		surf.results["propose_change"] = toolloop.ToolResult{Output: "proposed", Suspend: true}
		prov := script(runOf(llmtest.Response{Parallel: parallel, Calls: []llmtest.Call{
			{ID: "a", Name: "propose_change", Args: `{"change":"one"}`},
			{ID: "b", Name: "propose_change", Args: `{"change":"two"}`},
		}}))
		res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, AllowSuspend: true})
		if err != nil {
			t.Fatalf("parallel %v: Run: %v", parallel, err)
		}
		if !res.Suspended || res.PendingToolName != "propose_change" {
			t.Fatalf("parallel %v: result %+v, want parked on a propose_change", parallel, res)
		}
		if got := surf.calls(); len(got) != 1 {
			t.Errorf("parallel %v: the surface ran %v, want one detached call", parallel, got)
		}
		if d := dangling(res.Messages); len(d) != 1 || d[0] != res.PendingToolCallID {
			t.Errorf("parallel %v: dangling %v, want exactly the parked call %s", parallel, d, res.PendingToolCallID)
		}
		for _, m := range toolResults(res.Messages) {
			if !m.Failed || !strings.Contains(strings.ToLower(m.Content), "not run") {
				t.Errorf("parallel %v: the other call is answered %+v, want not run", parallel, m)
			}
		}
	}
}

// A CALL NAMING A TOOL THE SESSION NEVER OFFERED never runs, and the response it
// is part of is not held for it: the harness refuses it itself, so nothing waits for
// it to arrive. On the last round the budget allows, the response's other call
// is the last to come, and the run ends after it — the harness asks nothing more.
// The record answers the call as the model's, not run, saying why.
func TestACallNamingAToolNeverOfferedIsNotWaitedFor(t *testing.T) {
	t.Parallel()
	surf := newSurface("post")
	prov := script(runOf(
		llmtest.Response{Calls: []llmtest.Call{{ID: "x", Name: "nonexistent"}, {ID: "p", Name: "post"}}},
		calling("", "post"),
	))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); !slices.Equal(got, []string{"post"}) {
		t.Errorf("the surface ran %v, want only the offered call", got)
	}
	if late := prov.Late(); late != 0 {
		t.Errorf("the harness asked its model %d time(s) past the last round", late)
	}
	if !res.ExhaustedRounds || res.RoundsUsed != 1 {
		t.Errorf("rounds %d exhausted %v, want the one round the budget allows, exhausted",
			res.RoundsUsed, res.ExhaustedRounds)
	}
	x := toolResults(res.Messages)["x"]
	if !x.Failed || !strings.Contains(x.Content, "nonexistent") || !strings.Contains(x.Content, "not one of") {
		t.Errorf("the unknown call is answered %+v, want not run because nothing of that name is offered", x)
	}
}

// A CALL NAMING A TOOL NO LONGER ACTIVE IS REFUSED, NOT RUN: a session that
// listed the tool before the surface stopped offering it still routes the name
// to the engine, and the engine runs only what it offers now.
func TestACallNamingAnInactiveToolIsRefused(t *testing.T) {
	t.Parallel()
	surf := newSurface("post", "retired")
	prov := script(runOf(calling("", "retired"), says("done")))
	prov.Before = func(name string) {
		if name == "retired" {
			surf.remove("retired")
		}
	}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); len(got) != 0 {
		t.Errorf("the surface ran %v, a tool it no longer offers", got)
	}
	got := prov.Results()
	if len(got) != 1 || !got[0].Failed || !strings.Contains(got[0].Output, "not one of") {
		t.Errorf("the harness was answered %+v, want the call refused as not offered", got)
	}
	if len(res.Executions) != 1 || !res.Executions[0].Failed || !res.Executions[0].StartedAt.IsZero() {
		t.Errorf("executions %+v, want the refusal recorded, untimed", res.Executions)
	}
}

// A CALL THE HARNESS REWROTE IS REFUSED: a harness that maps a name the model wrote,
// and its list does not hold, to one it does, then runs it with the model's
// arguments, makes a call the model never asked for. Joined to the model's own
// call — by the harness's id where it sends one, by the arguments where it does
// not — its name is not the one the model asked for, and it is not run.
func TestACallTheHarnessRewroteIsRefused(t *testing.T) {
	t.Parallel()
	for _, noIDs := range []bool{false, true} {
		surf := newSurface("post")
		prov := script(runOf(
			llmtest.Response{Calls: []llmtest.Call{{ID: "c1", Name: "pots", Sent: "post", Args: `{"text":"hi"}`}}},
			says("done"),
		))
		prov.NoCallIDs = noIDs
		res, err := run(t, toolloop.Config{Provider: prov, Surface: surf})
		if err != nil {
			t.Fatalf("no ids %v: Run: %v", noIDs, err)
		}
		if got := surf.calls(); len(got) != 0 {
			t.Errorf("no ids %v: the surface ran %v, a call the model never made", noIDs, got)
		}
		got := prov.Results()
		if len(got) != 1 || !got[0].Failed || !strings.Contains(got[0].Output, "pots") {
			t.Errorf("no ids %v: the harness was answered %+v, want the rewrite refused naming what the model asked",
				noIDs, got)
		}
		var asked []string
		for _, m := range res.Messages {
			for _, c := range m.ToolCalls {
				asked = append(asked, c.Name)
			}
		}
		if !slices.Equal(asked, []string{"pots"}) {
			t.Errorf("no ids %v: the record says the model asked for %v, want what it wrote", noIDs, asked)
		}
	}
}

// A CALL THE HARNESS GAVE UP ON STILL RUNS OUT UNDER THE PHASE, AND IS RECORDED. The
// harness's cancel abandons only the delivery: the tool is not cut short half-way
// through a write — its context is the phase's, which the harness cannot cancel —
// and its real result is on the record the next run reads.
func TestACallTheHarnessGaveUpOnRunsOutUnderThePhase(t *testing.T) {
	t.Parallel()
	surf := newSurface("slow")
	surf.holdTool, surf.hold, surf.entered = "slow", make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	ctxErr := errors.New("never read")
	surf.hook = func(ctx context.Context, call llm.ToolCall) {
		go func() {
			<-surf.entered
			time.Sleep(150 * time.Millisecond) // well past the harness's give-up
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
		t.Fatalf("the harness was answered %+v, want it to have given up", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if ctxErr != nil {
		t.Errorf("the tool's context ended with the harness's cancel: %v", ctxErr)
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

// A HARNESS THAT DIES UNDER A CALL FAILS ITS RUN, AND THE CALL STILL RUNS OUT AND IS
// RECORDED: the EOF abandons the delivery and nothing else. The phase reports
// the run's failure with the call's real result on its failure record.
func TestACallTheHarnessDiedUnderRunsOutAndIsRecorded(t *testing.T) {
	t.Parallel()
	surf := newSurface("slow")
	surf.holdTool, surf.hold, surf.entered = "slow", make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	ctxErr := errors.New("never read")
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
		t.Errorf("the tool's context ended with the harness: %v", ctxErr)
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
// reason the next one answers — a person's note, a tool its harness could not see —
// never handed its harness the result of the call that ended it, so the model never
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
	t.Run("an activation the harness cannot see", func(t *testing.T) {
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

// withheldAndHanded checks the first run's harness never read result and the second
// run was handed it.
func withheldAndHanded(t *testing.T, prov *llmtest.Script, result string) {
	t.Helper()
	for _, r := range prov.Results() {
		if r.Output == result {
			t.Errorf("the ending run's harness read the result: %+v", r)
		}
	}
	if late := prov.Late(); late != 0 {
		t.Errorf("the harness asked its model %d time(s) after its run was ended", late)
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
// before the harness can ask its model again — exhausted, for the caller's judge.
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
		t.Errorf("the harness asked its model %d time(s) past the budget", late)
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

// THE LAST ROUND'S OWN TOOLS END THE RUN TOO. A last round whose calls are all
// the harness's own — its web search — has nothing for the host to answer, and the
// harness runs them and asks its model again: the run is ended as soon as the round
// is read, not after a request the budget did not allow.
func TestALastRoundOfTheHarnessOwnToolsEndsTheRun(t *testing.T) {
	t.Parallel()
	surf := newSurface("post")
	prov := script(runOf(
		llmtest.Response{Calls: []llmtest.Call{{Name: "web_search", Native: true}}},
		calling("", "post"),
	))
	prov.Before = func(name string) {
		if name == "web_search" {
			// The harness's own search takes its time, as a real one does.
			time.Sleep(3 * llmtest.KillLatency)
		}
	}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); len(got) != 0 {
		t.Errorf("the surface ran %v past the budget", got)
	}
	if res.RoundsUsed != 1 || !res.ExhaustedRounds {
		t.Errorf("rounds %d exhausted %v, want the one round, exhausted", res.RoundsUsed, res.ExhaustedRounds)
	}
}

// A CALL STILL RUNNING NEAR THE HARNESS'S OWN TOOL TIMEOUT ENDS THE RUN, and the
// next run is handed its real result: a harness that gave up on a call the engine is
// still running would send its model to do it again.
func TestACallNearTheHarnessToolTimeoutEndsTheRun(t *testing.T) {
	t.Parallel()
	surf := newSurface("slow", "submit_work")
	surf.results["slow"] = toolloop.ToolResult{Output: "the real result"}
	surf.hook = func(_ context.Context, call llm.ToolCall) {
		if call.Name == "slow" {
			time.Sleep(600 * time.Millisecond)
		}
	}
	prov := script(
		runOf(calling("", "slow"), says("never")),
		runOf(calling("", "submit_work")),
	)
	// A margin of five seconds — the floor — under a timeout of 5.3: the
	// call is answered at 300ms, while it still runs.
	prov.ToolTimeout = 5300 * time.Millisecond
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); !slices.Equal(got, []string{"slow", "submit_work"}) {
		t.Errorf("the surface ran %v", got)
	}
	if late := prov.Late(); late != 0 {
		t.Errorf("the harness asked its model %d time(s) after its run was ended", late)
	}
	withheldAndHanded(t, prov, "the real result")
	if res.Executions[0].Output != "the real result" {
		t.Errorf("executions %+v, want the call's real result recorded", res.Executions)
	}
}

// A CALL WHOSE RESPONSE THE HARNESS'S RECORD SHOWS LATE IS STILL ITS ROUND. A record
// read off a file the harness writes can lag the call by more than the join waits:
// the call is placed on a round of its own, and when the record does show the
// response, that response IS that round — not one more.
func TestAResponseRecordedAfterItsCallIsTheCallsRound(t *testing.T) {
	t.Parallel()
	surf := newSurface("look")
	prov := script(runOf(calling("", "look"), says("done")))
	res, err := run(t, toolloop.Config{Provider: &lagging{Script: prov, by: 2500 * time.Millisecond},
		Surface: surf})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 2 || len(res.Rounds) != 2 {
		t.Errorf("rounds %d (%d recorded), want the two requests the harness made", res.RoundsUsed, len(res.Rounds))
	}
	if len(res.Executions) != 1 || res.Executions[0].Round != 1 {
		t.Errorf("executions %+v, want the call on round 1", res.Executions)
	}
	// And the record pairs the response's call with what it returned: the
	// call that ran IS the one the response asked for, not a second beside
	// it while the response's own reads as never made.
	var asked []string
	for _, m := range res.Messages {
		for _, c := range m.ToolCalls {
			asked = append(asked, c.ID)
		}
	}
	results := toolResults(res.Messages)
	if len(asked) != 1 || results[asked[0]].Content != "look ok" || results[asked[0]].Failed {
		t.Errorf("the record holds calls %v answered %+v, want the one call and its result", asked, results)
	}
}

// lagging reports the run's records in order, but only once a lag has passed
// from the first — a backend reading a session file the harness flushes late — and
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
// exactly what the harness was told, failed, so the two never disagree about it.
func TestASuspendIsRefusedWhereItCannotBeResumed(t *testing.T) {
	t.Parallel()
	surf := newSurface("propose_change")
	surf.detached["propose_change"] = true
	surf.results["propose_change"] = toolloop.ToolResult{Output: "proposed", Suspend: true}
	prov := script(runOf(
		llmtest.Response{Calls: []llmtest.Call{{ID: "call-1", Name: "propose_change"}}},
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
		t.Fatalf("the harness was answered %+v, want the refusal", got)
	}
	answer, ok := toolResults(res.Messages)["call-1"]
	if !ok || answer.Content != got[0].Output || !answer.Failed {
		t.Errorf("the conversation answers the call %+v, want what the harness was told: %+v", answer, got[0])
	}
	if len(res.Executions) != 1 || !res.Executions[0].Failed || res.Executions[0].Output != got[0].Output {
		t.Errorf("executions = %+v, want the refusal the harness was told, failed", res.Executions)
	}
}

// A LONG CALL BESIDE ANOTHER LETS THE OTHER RUN. A call still running near the
// harness's own tool timeout ends its run — but not before the rest of its
// response: a harness that waits on each answer is told "still running" so it makes
// the next call, and the run ends after that one, whose answer is withheld. The
// next run is handed both results.
func TestALongCallBesideAnotherLetsTheOtherRun(t *testing.T) {
	t.Parallel()
	surf := newSurface("slow", "post", "submit_work")
	surf.results["slow"] = toolloop.ToolResult{Output: "the slow call's answer"}
	surf.hook = func(_ context.Context, call llm.ToolCall) {
		if call.Name == "slow" {
			time.Sleep(600 * time.Millisecond)
		}
	}
	prov := script(
		runOf(calling("", "slow", "post"), says("never reached in this run")),
		runOf(calling("", "submit_work")),
	)
	// Answered "still running" at 300ms, while it runs to 600.
	prov.ToolTimeout = 5300 * time.Millisecond
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf,
		TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ran := surf.calls()
	slices.Sort(ran)
	if !slices.Equal(ran, []string{"post", "slow", "submit_work"}) {
		t.Errorf("the surface ran %v, want the long call, the one beside it, and the submission", ran)
	}
	if late := prov.Late(); late != 0 {
		t.Errorf("the harness asked its model %d time(s) after its run was ended", late)
	}
	seen := prov.Seen()
	if len(seen) != 2 {
		t.Fatalf("%d runs, want two", len(seen))
	}
	results := map[string]string{}
	for _, m := range seen[1].Messages {
		if m.Role == llm.RoleTool {
			results[m.Name] = m.Content
		}
	}
	if results["slow"] != "the slow call's answer" || results["post"] != "post ok" {
		t.Errorf("the next run was handed %v, want both calls' real results", results)
	}
	if strings.Contains(res.Text, "never reached in this run") {
		t.Errorf("the first run went on past its response: %q", res.Text)
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
// come; a harness that died before making it leaves that end standing, and no other
// member of a chain may continue the run — it would submit again. A run that
// failed with nothing decided is not halted: the next member continues it.
func TestARunThatFailedAfterDecidingIsHalted(t *testing.T) {
	t.Parallel()
	died := errors.New("the harness died")
	t.Run("decided", func(t *testing.T) {
		t.Parallel()
		surf := newSurface("post", "submit_work")
		// The model asks for a delivery beside its submission, and the harness
		// dies before the delivery reaches the engine.
		prov := &halting{Script: script(llmtest.Run{Responses: []llmtest.Response{{Calls: []llmtest.Call{
			{Name: "submit_work"}, {Name: "post", Sent: "unreachable"},
		}}}, Err: died})}
		res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, TerminateAfter: []string{"submit_work"}})
		if err != nil {
			t.Fatalf("Run: %v — the phase finished before its harness died", err)
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
	died := errors.New("the harness died")
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

// A SUBMISSION THAT OUTLIVES ITS RUN STILL FINISHES THE PHASE. A call still
// running near the harness's tool timeout is answered "still running" and its run
// ended; a submission that returns after that has finished the phase all the
// same, and no further run is started — it would read the result and submit
// again.
func TestASubmissionThatOutlivesItsRunStillFinishesThePhase(t *testing.T) {
	t.Parallel()
	surf := newSurface("submit_work")
	surf.hook = func(context.Context, llm.ToolCall) { time.Sleep(600 * time.Millisecond) }
	prov := script(runOf(calling("", "submit_work"), says("never")), runOf(calling("", "submit_work")))
	// Answered "still running" at 300ms, while it runs to 600.
	prov.ToolTimeout = 5300 * time.Millisecond
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); len(got) != 1 {
		t.Errorf("the surface ran %v, want the one submission", got)
	}
	if n := len(prov.Seen()); n != 1 {
		t.Errorf("%d runs, want none after the submission finished the phase", n)
	}
	if res.ExhaustedRounds || res.Withheld != "" {
		t.Errorf("result %+v, want a finished phase", res)
	}
}

// A NOTE WAITING AS THE HARNESS RUNS ITS OWN TOOLS ENDS THE RUN. A response whose
// calls are all the harness's own has nothing for the host to answer, so no call's
// end can pause the run: the note is seen as the response is read, and the next
// run reads it before the model asks anything more.
func TestANoteWaitingAsTheHarnessRunsItsOwnToolsEndsTheRun(t *testing.T) {
	t.Parallel()
	box := &stepBox{log: &stepLog{}}
	prov := script(
		runOf(
			llmtest.Response{Writing: func() { box.offer("n-1", "the note") },
				Calls: []llmtest.Call{{Name: "web_search", Native: true}}},
			says("never reached in this run"),
		),
		runOf(says("done")),
	)
	prov.Before = func(name string) {
		if name == "web_search" {
			time.Sleep(3 * llmtest.KillLatency)
		}
	}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("post"), Steer: box})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := prov.Seen()
	if len(seen) != 2 || indexOf(seen[1].Messages, "the note") == -1 {
		t.Fatalf("%d runs, want the one the note ended and the one that read it", len(seen))
	}
	if strings.Contains(res.Text, "never reached in this run") {
		t.Errorf("the first run went on past the note: %q", res.Text)
	}
}
