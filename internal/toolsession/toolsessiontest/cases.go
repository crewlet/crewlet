package toolsessiontest

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// A SEAT THIS NODE NO LONGER SPEAKS FOR CALLS NOTHING. The fence is asked
// before every call: the seat may have moved to a peer, or a person asked it to
// stop, since the last one. The call that finds it closed is refused, and so is
// every call after it, whatever the fence says by then — a seat that moved and
// came back is a grant this run was never admitted under.
func testAClosedFenceRefusesTheCallAndEveryOneAfter(t *testing.T, factory Factory) {
	f := newFixture(t, factory)
	cs := f.dial(0)
	if res := f.call(cs, readTool, nil); res.IsError || text(res) != readAnswer {
		t.Fatalf("a call on an open fence was answered %v %q", res.IsError, text(res))
	}

	f.closeFence()
	fenced := f.send(cs, readTool, nil)
	f.recorded(2)
	f.openFence()
	after := f.send(cs, readTool, nil)
	rec := f.recorded(3)
	f.served.End(t)
	fenced.over(t)
	after.over(t)

	if n := f.reads.Load(); n != 1 {
		t.Errorf("the tool ran %d times, want only the call before the fence closed", n)
	}
	for _, r := range rec[1:] {
		if !r.Failed || r.Output == readAnswer {
			t.Errorf("a call made once the fence had closed is recorded %s, want it refused",
				describe([]Recorded{r}))
		}
	}
}

// A FENCE THAT CLOSES DURING A CALL STOPS EVERY CALL AFTER IT. The fence is
// asked again once a call has returned: the call ran, and what it did is done
// and recorded — but nothing after it runs as a seat this node no longer holds,
// even once the fence would let it.
func testAFenceThatClosesDuringACallRefusesEveryCallAfterIt(t *testing.T, factory Factory) {
	f := newFixture(t, factory)
	cs := f.dial(0)
	running := f.send(cs, writeTool, map[string]any{"value": holdValue})
	f.held()
	f.closeFence()
	f.free()
	if rec := f.recorded(1); rec[0].Failed || rec[0].Output != "0" {
		t.Errorf("the call the fence closed under is recorded %s, want what it did",
			describe(rec))
	}

	f.openFence()
	after := f.send(cs, readTool, nil)
	rec := f.recorded(2)
	f.served.End(t)
	running.over(t)
	after.over(t)

	if n := f.reads.Load(); n != 0 {
		t.Errorf("a call ran %d times after the fence closed under the run", n)
	}
	if n := len(f.written()); n != 1 {
		t.Errorf("the write ran %d times, want the one call made on an open fence", n)
	}
	if r := rec[1]; !r.Failed || r.Output == readAnswer {
		t.Errorf("the call after the fence closed is recorded %s, want it refused",
			describe([]Recorded{r}))
	}
}

// A PANIC IS CONTAINED, AND IT ENDS THE RUN. A tool with a defect is answered
// as a failure that does not carry the panic, the process stays up, and the run
// is over: a defect does not run again on the run's next call, nor does any
// other tool.
func testAToolThatPanicsFailsAndEndsTheRun(t *testing.T, factory Factory) {
	f := newFixture(t, factory)
	cs := f.dial(0)
	broke := f.send(cs, brokenTool, nil)
	if r := f.recorded(1)[0]; !r.Failed || strings.Contains(r.Output, panicValue) {
		t.Errorf("the call that panicked is recorded %s, want a failure that does not "+
			"carry the panic", describe([]Recorded{r}))
	}
	after := f.send(cs, readTool, nil)
	rec := f.recorded(2)
	f.served.End(t)
	if res, err := broke.answer(t); err == nil && strings.Contains(text(res), panicValue) {
		t.Errorf("the panic reached the client: %q", text(res))
	}
	after.over(t)

	if n := f.breaks.Load(); n != 1 {
		t.Errorf("the broken tool ran %d times", n)
	}
	if n := f.reads.Load(); n != 0 {
		t.Errorf("a call ran %d times on a run a panic ended", n)
	}
	if r := rec[1]; !r.Failed || r.Output == readAnswer {
		t.Errorf("the call after the panic is recorded %s, want it refused",
			describe([]Recorded{r}))
	}
}

// A CALL IS THE RUN'S, NOT THE REQUEST'S. A harness that gives up on a call — its
// own timeout — or dies under it cancels its request, and nothing a tool is
// half-way through writing: the tool runs to its end under the run's own
// context, with that context's values, and its call is recorded.
func testACallRunsUnderTheRunsContextNotTheRequests(t *testing.T, factory Factory) {
	for _, tc := range []struct {
		name string
		// leave is how the client walks away from the call: it cancels
		// the call's request, or hangs up under it.
		leave func(cancel context.CancelFunc, hangUp func())
	}{
		{"the client cancels its request", func(cancel context.CancelFunc, _ func()) { cancel() }},
		{"the client hangs up", func(_ context.CancelFunc, hangUp func()) { hangUp() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, factory)
			cs, hangUp := f.connect(0)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			gone := make(chan struct{})
			go func() {
				defer close(gone)
				_, _ = cs.CallTool(ctx, &mcp.CallToolParams{
					Name: writeTool, Arguments: map[string]any{"value": holdValue}})
			}()
			f.held()
			tc.leave(cancel, hangUp)
			select {
			case <-gone:
			case <-time.After(answerWait):
				t.Fatal("the client never gave up on its call")
			}
			// The session's own time to act on the departure — a
			// dispatcher that let it reach the call would have by now.
			time.Sleep(departureGrace)
			f.free()

			if rec := f.recorded(1); rec[0].Name != writeTool || rec[0].Failed || rec[0].Output != "0" {
				t.Errorf("the abandoned call is recorded %s, want what it did", describe(rec))
			}
			seen := f.written()
			if len(seen) != 1 {
				t.Fatalf("the write returned %d times, want once", len(seen))
			}
			if seen[0].cancelled {
				t.Error("the tool's context ended with the client's request")
			}
			if seen[0].carried != runValue {
				t.Errorf("the tool ran under a context without the run's values: %v", seen[0].carried)
			}
		})
	}
}

// ARGUMENTS THAT DO NOT PARSE ARE ANSWERED, NOT RUN: running the call on an
// empty object is a write the model never asked for. The answer says why in
// the one set of words both places use — a model would otherwise learn to fix
// the same mistake differently depending on where its harness ran — and the run
// goes on, so the model's corrected call runs.
func testArgumentsThatDoNotParseAreAnsweredNotRun(t *testing.T, factory Factory) {
	f := newFixture(t, factory)
	cs := f.dial(0)
	res := f.call(cs, readTool, json.RawMessage(`["not", "an object"]`))
	if !res.IsError || !shaped(text(res), f.served.UnparsedArguments, readTool) {
		t.Errorf("a call whose arguments did not parse was answered %v %q, want the shared "+
			"answer naming why", res.IsError, text(res))
	}
	if n := f.reads.Load(); n != 0 {
		t.Fatalf("the tool ran %d times on arguments that did not parse", n)
	}
	if r := f.recorded(1)[0]; !r.Failed || r.Output != text(res) {
		t.Errorf("the call is recorded %s, want the failure it was answered with",
			describe([]Recorded{r}))
	}

	if again := f.call(cs, readTool, map[string]any{}); again.IsError || text(again) != readAnswer {
		t.Errorf("the corrected call was answered %v %q, want it run", again.IsError, text(again))
	}
}

// A RUN THAT IS OVER RUNS NOTHING. Once a run has ended, a call its client
// still makes never reaches a tool — and neither does one that was already
// past the session's transport when it ended, which its dispatcher refuses.
func testACallAfterTheRunEndedIsRefused(t *testing.T, factory Factory) {
	f := newFixture(t, factory)
	cs := f.dial(0)
	if res := f.call(cs, readTool, nil); res.IsError {
		t.Fatalf("a call on a live run failed: %q", text(res))
	}
	f.served.End(t)

	ctx, cancel := context.WithTimeout(t.Context(), answerWait)
	defer cancel()
	if res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: readTool}); err == nil && !res.IsError {
		t.Errorf("a call on an ended run was answered as one that ran: %q", text(res))
	}
	if late := f.served.Dispatcher.Call(toolsession.Call{
		Name: readTool, Arguments: map[string]any{},
	}); !late.Failed {
		t.Errorf("a call that reached the dispatcher after its run ended was answered %q, "+
			"want it refused", late.Output)
	}
	if n := f.reads.Load(); n != 1 {
		t.Errorf("the tool ran %d times, want only the call made while the run was live", n)
	}
}

// CALLS MADE AT ONCE COUNT IN THE ORDER THEY ARRIVED. A harness makes a response's
// calls together, and they return in whatever order their tools take; the
// repeat count an operation id carries is read against the calls ADMITTED
// before, so the second call counts the first though the first is still
// running, the first counts nothing after it, the record places each where
// it arrived, and a call after them counts both — so a re-run, making the same
// calls, derives the same ids.
func testCallsCountInTheOrderTheyArrived(t *testing.T, factory Factory) {
	f := newFixture(t, factory)
	cs := f.dial(0)
	first := f.send(cs, writeTool, map[string]any{"value": holdValue})
	f.held()
	if second := f.call(cs, writeTool, map[string]any{"value": "other"}); text(second) != "1" {
		t.Errorf("the second call counts %q different calls before it, want the first, "+
			"admitted before it and still running", text(second))
	}
	f.free()
	if res, err := first.answer(t); err != nil || text(res) != "0" {
		t.Errorf("the first call answered %q (%v), want 0: it counted a call that arrived "+
			"after it", text(res), err)
	}
	rec := f.recorded(2)
	if rec[0].Output != "0" || rec[1].Output != "1" {
		t.Errorf("the record places the calls %s, want each where it arrived", describe(rec))
	}
	// AND BOTH STAY IN THE RUN'S LOG once they have returned, so a call
	// after them counts both.
	if third := f.call(cs, writeTool, map[string]any{"value": "third"}); text(third) != "2" {
		t.Errorf("a later call counts %q calls before it, want both of the earlier two", text(third))
	}
}

// AN ACTIVATION IS ANSWERED ONCE THE CLIENT HAS RE-LISTED. A client that
// re-reads its tool list when told it changed — every harness that runs in a box,
// and every host harness that re-lists — asks its model again the moment the
// answer lands; answered before its re-list, it offers the tool it just
// activated a request late. So the answer waits for the re-list (at most two
// seconds), and the tool is then listed and callable.
func testAnActivationIsAnsweredOnceTheClientHasRelisted(t *testing.T, factory Factory) {
	const relistDelay = 300 * time.Millisecond
	f := newFixture(t, factory)
	cs := f.dial(relistDelay)
	if slices.Contains(f.listed(cs), postTool) {
		t.Fatalf("%s is listed before anything activated it", postTool)
	}

	began := time.Now()
	if res := f.call(cs, activateTool, nil); res.IsError {
		t.Fatalf("the activation failed: %q", text(res))
	}
	// The client re-lists no sooner than relistDelay after the notice, so an
	// answer that waited for it took at least that — and was let go once it
	// came, well before the hold's own two-second bound.
	if took := time.Since(began); took < relistDelay {
		t.Errorf("the activation was answered after %s, before the client could have re-listed", took)
	} else if took > 1500*time.Millisecond {
		t.Errorf("the activation was answered after %s: its hold outlasted the client's re-list", took)
	}

	if !slices.Contains(f.listed(cs), postTool) {
		t.Fatalf("the activated %s is not listed", postTool)
	}
	if res := f.call(cs, postTool, nil); res.IsError || text(res) != postAnswer {
		t.Errorf("the activated %s is listed but not callable: %v %q", postTool, res.IsError, text(res))
	}
	if n := f.posts.Load(); n != 1 {
		t.Errorf("%s ran %d times, want 1", postTool, n)
	}
}

// A SUSPEND THE RUN CANNOT RESUME IS REFUSED. A tool whose work outlives its
// call asks the run to park with the call unanswered, so the conversation can
// be persisted and re-entered once the work is done. A run that can never be
// re-entered would strand the harness waiting on an answer nobody will give, so the
// call is answered refused — and the run goes on. The tool ran, so it is on the
// record as a call that ran, failed.
func testASuspendTheRunCannotResumeIsRefused(t *testing.T, factory Factory) {
	f := newFixture(t, factory)
	cs := f.dial(0)
	if res := f.call(cs, launchTool, nil); !res.IsError {
		t.Errorf("a suspend the run cannot resume was answered %q, want it refused", text(res))
	}
	if r := f.recorded(1)[0]; r.Name != launchTool || !r.Failed {
		t.Errorf("the refused suspend is recorded %s, want it failed", describe([]Recorded{r}))
	}
	if n := f.launches.Load(); n != 1 {
		t.Errorf("the tool ran %d times, want once", n)
	}
	if res := f.call(cs, readTool, nil); res.IsError || text(res) != readAnswer {
		t.Errorf("the call after a refused suspend was answered %v %q, want the run to go on",
			res.IsError, text(res))
	}
}

// EVERY REFUSAL CARRIES ITS CLASS. A call the dispatcher answers without the
// tool's own output is on the record with the class a surface that is not a
// model acts on (mcp.Refusal…): the run is not taking calls, the arguments
// are wrong, the engine failed inside the tool, or the tool may not do what it
// asked from here. A model reads the sentence; nothing else can read a
// sentence.
func testEveryRefusalCarriesItsClass(t *testing.T, factory Factory) {
	for _, tc := range []struct {
		name string
		// refuse makes the refusal and returns its place on the record.
		refuse func(f *fixture, cs *mcp.ClientSession) int
		want   crewletmcp.Refusal
	}{
		{"a call on a closed fence", func(f *fixture, cs *mcp.ClientSession) int {
			f.closeFence()
			f.send(cs, readTool, nil)
			return 1
		}, crewletmcp.RefusalNotRunning},
		{"a call after a fence closed during another", func(f *fixture, cs *mcp.ClientSession) int {
			f.send(cs, writeTool, map[string]any{"value": holdValue})
			f.held()
			f.closeFence()
			f.free()
			f.recorded(1)
			f.openFence()
			f.send(cs, readTool, nil)
			return 2
		}, crewletmcp.RefusalNotRunning},
		{"a tool that panicked", func(f *fixture, cs *mcp.ClientSession) int {
			f.send(cs, brokenTool, nil)
			return 1
		}, crewletmcp.RefusalInternalError},
		{"a call after a tool panicked", func(f *fixture, cs *mcp.ClientSession) int {
			f.send(cs, brokenTool, nil)
			f.recorded(1)
			f.send(cs, readTool, nil)
			return 2
		}, crewletmcp.RefusalNotRunning},
		{"arguments that did not parse", func(f *fixture, cs *mcp.ClientSession) int {
			f.send(cs, readTool, json.RawMessage(`"an object it is not"`))
			return 1
		}, crewletmcp.RefusalInvalid},
		{"a suspend the run cannot resume", func(f *fixture, cs *mcp.ClientSession) int {
			f.send(cs, launchTool, nil)
			return 1
		}, crewletmcp.RefusalForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, factory)
			cs := f.dial(0)
			at := tc.refuse(f, cs)
			r := f.recorded(at)[at-1]
			if !r.Failed || r.Class != tc.want {
				t.Errorf("the refusal is recorded %s, want it failed with class %q",
					describe([]Recorded{r}), string(tc.want))
			}
		})
	}
}
