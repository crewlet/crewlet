package mcpbridge_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A call longer than its harness waits (the package doc's "A call longer than
// its harness waits").

// kimiCeiling is a ceiling a runner states: Kimi Code's, which no setting moves.
const kimiCeiling = 300 * time.Second

// parks is what a session's job was parked on ([mcpbridge.Session.Park]).
type parks struct {
	mu     sync.Mutex
	parked []sandbox.CallWait
}

func (p *parks) all() []sandbox.CallWait {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.parked)
}

// withCeiling readies a session whose box's harness gives up on a call at
// ceiling, as its runner states it (sandbox.OpenedJob.CallCeiling), its job
// parked on such a call as the run's row records it, onto p.
func withCeiling(ceiling time.Duration, p *parks) func(*mcpbridge.Session) {
	return withCeilingParking(ceiling, p, func() (sandbox.AskOutcome, error) { return sandbox.AskRecorded, nil })
}

// withCeilingParking is [withCeiling] whose park, asked onto p, answers what
// outcome answers once it returns.
func withCeilingParking(ceiling time.Duration, p *parks, outcome func() (sandbox.AskOutcome, error),
) func(*mcpbridge.Session) {
	return func(s *mcpbridge.Session) {
		s.CallCeiling = ceiling
		s.Park = func(_ context.Context, call sandbox.CallWait) (sandbox.AskOutcome, error) {
			p.mu.Lock()
			p.parked = append(p.parked, call)
			p.mu.Unlock()
			return outcome()
		}
	}
}

// answeredNow is the answer on answered at this moment, or nil.
func answeredNow(answered <-chan *mcp.CallToolResult) *mcp.CallToolResult {
	select {
	case res := <-answered:
		return res
	default:
		return nil
	}
}

// A CALL LONGER THAN ITS HARNESS WAITS PARKS ITS JOB BEFORE IT, by the tool
// loop's clock and not before: the job parked on the call on the run's row,
// then the call answered still running (end your turn, you will be resumed),
// not as a failure; its tool runs on, recorded as one nobody waited for. The
// package doc's "A call longer than its harness waits" has why.
//
// Mutation: no clock (parkClock answering nil), and nothing answers before the
// ceiling; Park not asked, and nothing resumes the run; cutFirst unset, and the
// run's end answers the call a second time.
func TestACallLongerThanItsHarnessWaitsParksItsJobBeforeIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		held := newHeldTool("write")
		l := &ledger{}
		p := &parks{}
		b := openBubbledWith(t, l, l, withCeiling(kimiCeiling, p), held)
		box := b.connect(t, nil)
		t.Cleanup(held.free)

		answered := callFromBox(context.WithoutCancel(t.Context()), box, "write", map[string]any{"value": "hold"}, "")
		<-held.entered
		parkAt := toolloop.ParkBefore(kimiCeiling)
		time.Sleep(parkAt - time.Second)
		synctest.Wait()
		if res := answeredNow(answered); res != nil {
			t.Fatalf("the call was answered %v %q a second before the tool loop's clock, want it still "+
				"waited for", res.IsError, text(res))
		}
		time.Sleep(time.Second)
		synctest.Wait()
		res := answeredNow(answered)
		switch {
		case res == nil:
			t.Fatalf("the call was not answered at %v, before its harness's ceiling of %v", parkAt, kimiCeiling)
		case res.IsError || text(res) != mcpbridge.ParkedNote:
			t.Fatalf("the call was answered %v %q, want %q, and not as failed", res.IsError, text(res),
				mcpbridge.ParkedNote)
		}
		if parked := p.all(); len(parked) != 1 || parked[0].Tool != "write" || parked[0].Seq != 1 {
			t.Fatalf("the job is parked on %+v, want once, on its call to write, the first", parked)
		}
		if rows := l.rows(); len(rows) != 0 {
			t.Fatalf("the call is recorded before its tool returned: %+v", rows)
		}

		// THE RUN ENDS, as its harness ends its turn: its opening's end finds the
		// call answered already and answers it no second time.
		b.bridge.Close("run-1")
		held.free()
		synctest.Wait()
		if rows := l.rows(); len(rows) != 1 || rows[0].Output != "write done" || !rows[0].Abandoned {
			t.Fatalf("the call is recorded %+v, want it once, with what its tool did, as one nobody "+
				"waited for", rows)
		}
	})
}

// A RUNNER THAT STATES NO CEILING HAS NO CALL ANSWERED EARLY. Every runner
// whose harness's wait is a setting writes the bridge's own bound there, so
// the call's own answer is the one its model reads, however long its tool
// runs: a session stating no ceiling starts no clock, and a call held a day is
// answered with what its tool did, recorded as one its harness was handed.
//
// Mutation: park a session stating none at the tool loop's clock of the
// bridge's floor, and the held call is answered still running.
func TestARunnerThatStatesNoCeilingHasNoCallAnsweredEarly(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		held := newHeldTool("write")
		l := &ledger{}
		p := &parks{}
		b := openBubbledWith(t, l, l, withCeiling(0, p), held)
		box := b.connect(t, nil)
		t.Cleanup(held.free)

		answered := callFromBox(context.WithoutCancel(t.Context()), box, "write", map[string]any{"value": "hold"}, "")
		<-held.entered
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		if res := answeredNow(answered); res != nil {
			t.Fatalf("a call of a session stating no ceiling was answered %v %q while its tool ran, want "+
				"its own answer alone", res.IsError, text(res))
		}
		held.free()
		if res := <-answered; res.IsError || text(res) != "write done" {
			t.Errorf("the call was answered %v %q, want its tool's own answer", res.IsError, text(res))
		}
		if rows := l.rows(); len(rows) != 1 || rows[0].Abandoned {
			t.Errorf("the call is recorded %+v, want it once, as one its harness was handed", rows)
		}
		if parked := p.all(); len(parked) != 0 {
			t.Errorf("the job is parked on %+v, want on nothing", parked)
		}
	})
}

// A PARK NOT MARKED ON THE ROW PROMISES NO RESUME. Only a call on the job's row
// (AskRecorded) is told to end its turn and wait for its result: where the
// store did not take the mark, or the row holds another job or an ask that
// stands, nothing waits on the call, and a run that ended its turn believing
// it would be resumed would wait on nothing. So a park that fails is answered
// still running, one on a run no longer running that the run has ended, and
// neither as failed.
//
// Mutation: answer ParkedNote whatever Park answered, and every case goes red.
func TestAParkNotMarkedOnTheRowPromisesNoResume(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		outcome sandbox.AskOutcome
		err     error
		want    string
	}{
		{"the store refused the mark", "", errors.New("the store is unreachable"), mcpbridge.StillRunningNote},
		{"the row holds no running job", sandbox.AskNotRunning, nil, mcpbridge.EndedRunningNote},
		{"an ask on the row stands", sandbox.AskAlreadyAsked, nil, mcpbridge.StillRunningNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				held := newHeldTool("write")
				l := &ledger{}
				p := &parks{}
				b := openBubbledWith(t, l, l, withCeilingParking(kimiCeiling, p,
					func() (sandbox.AskOutcome, error) { return tc.outcome, tc.err }), held)
				box := b.connect(t, nil)
				t.Cleanup(held.free)

				answered := callFromBox(context.WithoutCancel(t.Context()), box, "write",
					map[string]any{"value": "hold"}, "")
				<-held.entered
				time.Sleep(toolloop.ParkBefore(kimiCeiling))
				synctest.Wait()
				res := answeredNow(answered)
				switch {
				case res == nil:
					t.Fatal("the call was not answered at the tool loop's clock")
				case res.IsError || text(res) != tc.want:
					t.Fatalf("the call was answered %v %q, want %q, and not as failed", res.IsError, text(res),
						tc.want)
				}
				if parked := p.all(); len(parked) != 1 {
					t.Errorf("the job's park was asked %+v, want once", parked)
				}
				b.bridge.Close("run-1")
				held.free()
				synctest.Wait()
			})
		})
	}
}

// A PARKED CALL IS RECORDED ONLY ONCE ITS PARK IS MARKED. The mark on the run's
// row (WaitsOnCall) tells the engine the job's park, not a late item, is to
// tell the call's result: a record landing before it would be filed as a late
// item and then told again by the park's relaunch. So a tool returning while
// its park is still being marked leaves nothing recorded until the mark is
// decided, then is recorded once, as the park's, one nobody waited for.
//
// Mutation: drop claim's wait for the park's mark (<-marked), and the call is
// recorded while its park is still being marked.
func TestAParkedCallIsRecordedOnlyOnceItsParkIsMarked(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		held := newHeldTool("write")
		l := &ledger{}
		p := &parks{}
		release := make(chan struct{})
		b := openBubbledWith(t, l, l, withCeilingParking(kimiCeiling, p, func() (sandbox.AskOutcome, error) {
			<-release
			return sandbox.AskRecorded, nil
		}), held)
		box := b.connect(t, nil)
		t.Cleanup(held.free)
		releasePark := sync.OnceFunc(func() { close(release) })
		t.Cleanup(releasePark)

		answered := callFromBox(context.WithoutCancel(t.Context()), box, "write", map[string]any{"value": "hold"}, "")
		<-held.entered
		time.Sleep(toolloop.ParkBefore(kimiCeiling))
		synctest.Wait()
		if parked := p.all(); len(parked) != 1 {
			t.Fatalf("the job's park was asked %+v, want once", parked)
		}
		// THE TOOL RETURNS WHILE ITS PARK IS STILL BEING MARKED.
		held.free()
		synctest.Wait()
		if rows := l.rows(); len(rows) != 0 {
			t.Fatalf("the call is recorded %+v while its park is still being marked, want nothing yet", rows)
		}
		releasePark()
		synctest.Wait()
		if res := answeredNow(answered); res == nil || res.IsError || text(res) != mcpbridge.ParkedNote {
			t.Fatalf("the call was answered %+v, want the park's, %q", res, mcpbridge.ParkedNote)
		}
		if rows := l.rows(); len(rows) != 1 || rows[0].Output != "write done" || !rows[0].Abandoned {
			t.Fatalf("the call is recorded %+v, want once, with what it did, as one nobody waited for", rows)
		}
		b.bridge.Close("run-1")
		synctest.Wait()
	})
}

// A CALL THAT COULD PARK ITS JOB PAST ITS CAP IS ANSWERED AT ONCE, NOT RUN. The
// row takes no park past sandbox.MaxParkedCalls, and a call answered still
// running with no park reaches the run only as one nobody waited for. So with
// the job parked on as many calls as it may, or as many still running that
// could park, the next is answered as failed before its tool runs, no entry
// written ahead of it and nothing recorded: told to end its turn where the job
// is parked, and to call again where nothing promises a resume. A session
// opened again after its seat moved counts the parks its row holds already.
//
// Mutation: drop binding.Call's parksFullLocked check, and the call past the
// cap runs; open the binding at no parks whatever Session.ParkedCalls says, and
// the reopened job's call past the cap runs.
func TestACallThatCouldParkItsJobPastItsCapIsAnsweredAtOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// already is the parks the job's row holds as the session opens.
		already int
		// running is what the call past the cap is told while the calls it
		// would join still run, none of them parked by this opening.
		running string
	}{
		{"a job parked on none yet", 0, mcpbridge.CallsFullNote},
		{"a job opened again parked on some", sandbox.MaxParkedCalls - 3, mcpbridge.ParksFullNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				held := &heldTool{name: "write", entered: make(chan struct{}, sandbox.MaxParkedCalls+1),
					release: make(chan struct{})}
				l := &ledger{}
				p := &parks{}
				b := openBubbledWith(t, l, l, func(s *mcpbridge.Session) {
					withCeiling(kimiCeiling, p)(s)
					s.ParkedCalls = tc.already
				}, held)
				box := b.connect(t, nil)
				t.Cleanup(held.free)
				call := func() <-chan *mcp.CallToolResult {
					return callFromBox(context.WithoutCancel(t.Context()), box, "write",
						map[string]any{"value": "hold"}, "")
				}
				past := func(when, want string) {
					t.Helper()
					answered := call()
					synctest.Wait()
					res := answeredNow(answered)
					switch {
					case res == nil:
						t.Fatalf("%s, the call past the cap is not answered at once", when)
					case !res.IsError || text(res) != want:
						t.Fatalf("%s, the call past the cap is answered %v %q, want %q, as failed", when,
							res.IsError, text(res), want)
					}
				}

				room := sandbox.MaxParkedCalls - tc.already
				for range room {
					call()
					<-held.entered
				}
				past("with the calls it would join still running", tc.running)
				time.Sleep(toolloop.ParkBefore(kimiCeiling))
				synctest.Wait()
				if parked := p.all(); len(parked) != room {
					t.Fatalf("the job's park was asked for %d calls, want %d", len(parked), room)
				}
				past("with the job parked on as many calls as it may", mcpbridge.ParksFullNote)

				if entered := len(held.entered); entered != 0 {
					t.Errorf("the tool was entered %d more times, want never by a call past the cap", entered)
				}
				if entries := l.entries(); len(entries) != room {
					t.Errorf("%d entries were written ahead of a call, want %d, none for a call past the cap",
						len(entries), room)
				}
				b.bridge.Close("run-1")
				held.free()
				synctest.Wait()
				if rows := l.rows(); len(rows) != room {
					t.Errorf("%d calls are recorded, want the %d that ran, none past the cap", len(rows), room)
				}
			})
		})
	}
}
