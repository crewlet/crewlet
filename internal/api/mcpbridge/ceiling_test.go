package mcpbridge_test

import (
	"context"
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
	return func(s *mcpbridge.Session) {
		s.CallCeiling = ceiling
		s.Park = func(_ context.Context, call sandbox.CallWait) (sandbox.AskOutcome, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.parked = append(p.parked, call)
			return sandbox.AskRecorded, nil
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
