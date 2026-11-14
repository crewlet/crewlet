package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// steppingClock is a coordinator's clock that reads one instant until it is
// stepped, then the instant it was stepped at once more, and the next one for
// every read after that: a bound crossed between the first reading an attempt
// takes and any later one.
type steppingClock struct {
	mu       sync.Mutex
	at, next time.Time
	stepping bool
}

func (s *steppingClock) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := s.at
	if s.stepping {
		s.at, s.stepping = s.next, false
	}
	return at
}

// step has the clock read at once more, and next from then on.
func (s *steppingClock) step(at, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at, s.next, s.stepping = at, next, true
}

// steppedRead is a way of asking a box's host that steps the collecting node's
// clock as the host's answer comes back, once, where step is set.
type steppedRead struct {
	BoxReader
	mu   sync.Mutex
	step func()
}

func (s *steppedRead) ReadBox(ctx context.Context, run PendingRun, read BoxRead) (Result, bool, error) {
	result, ended, err := s.BoxReader.ReadBox(ctx, run, read)
	s.mu.Lock()
	step := s.step
	s.step = nil
	s.mu.Unlock()
	if step != nil {
		step()
	}
	return result, ended, err
}

// A LOST JOB'S READ IS JUDGED ONCE, AT ONE READING OF THE CLOCK. A lost job's
// round is carried on without its read only once the read is past asking again:
// its box has not answered for the give-up window, or its host has answered for
// the box timeout that its coordination store did not answer it. Until then the
// claim is handed back for the read to be asked again. The carry judged the
// failed read at its reading of the clock, and the hand-back judged it again at
// its own, so a bound crossed between the two had the hand-back give the
// collection up and END the lost job's run as unreachable, where the carry had
// just decided to ask again and, the attempt after, would have carried the round
// on. Here the clock crosses the bound between the attempt's first reading after
// its read failed and every later one: the claim is handed back, and the next
// attempt carries the round on without the read.
//
// Mutation: have the hand-back read the clock again rather than the carry's
// verdict, or have the carry hand a failure on for the hand-back to judge afresh;
// each ends the lost job's run as unreachable.
func TestALostJobsReadIsJudgedOnceAtOneReadingOfTheClock(t *testing.T) {
	t.Parallel()

	// handedBackNotEnded holds t1 after a lost job's second failed read to a
	// claim handed back for the read to be asked again: nothing ended,
	// announced or resumed, and the verdict that it was lost still on its row.
	handedBackNotEnded := func(t *testing.T, rig *coordRig, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("the read judged too young to give up was not handed back for a retry")
		}
		if failed := rig.failures(); len(failed) != 0 {
			t.Fatalf("announced %+v: the hand-back judged the read again, past the bound, and ended the lost "+
				"job's run its carry had decided to ask again", failed)
		}
		if got := rig.get("t1"); got.Status != StatusRunning || got.Lost == nil {
			t.Fatalf("the run is %s with loss %+v; want it handed back running, its verdict kept",
				got.Status, got.Lost)
		}
		if resumed := rig.resumer.calls(); len(resumed) != 0 {
			t.Fatalf("the turn resumed %d times before the read was given up", len(resumed))
		}
	}
	// carriedOn holds t1 to a lost job's round carried on once, without its read.
	carriedOn := func(t *testing.T, rig *coordRig, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("the attempt past the bound = %v; want the round carried on", err)
		}
		if resumed := rig.resumer.calls(); len(resumed) != 1 || resumed[0].Lost == nil {
			t.Fatalf("the turn resumed %d times (%+v); want the round carried on once", len(resumed), resumed)
		}
		if failed := rig.failures(); len(failed) != 0 {
			t.Fatalf("announced %+v for a lost job whose round is carried on", failed)
		}
		if charged := rig.accountant.total(); charged != 0 {
			t.Fatalf("charged %d tokens nobody read", charged)
		}
	}

	t.Run("a box that has failed for the give-up window", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		clock := &steppingClock{at: rig.now}
		rig.coordinator.now = clock.now
		start := rig.now
		var (
			mu   sync.Mutex
			step func()
		)
		rig.runner.CollectFunc = func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			if step != nil {
				step()
				step = nil
			}
			return errors.New("connection reset")
		}
		if err := loseAndClaim(t, rig, Result{InputTokens: 500}); err == nil {
			t.Fatal("the lost job's first failed read was not handed back for a retry")
		}
		if got := rig.get("t1").Launch; got.CollectFailures != 1 || !got.CollectFailingSince.Equal(start) {
			t.Fatalf("the premise: the first failed read left %d failures since %v; want one, since %v",
				got.CollectFailures, got.CollectFailingSince, start)
		}

		// THE GIVE-UP WINDOW ENDS between the attempt's reading as its read
		// fails and any later reading.
		mu.Lock()
		step = func() { clock.step(start.Add(ConnectGiveUp-time.Nanosecond), start.Add(ConnectGiveUp)) }
		mu.Unlock()
		handedBackNotEnded(t, rig, collectAgain(t, rig))
		if got := rig.get("t1").Launch.CollectFailures; got != 2 {
			t.Fatalf("the read handed back counted %d failures; want its second", got)
		}

		carriedOn(t, rig, collectAgain(t, rig))
	})

	t.Run("a host whose store alone stayed silent for the box timeout", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		clock := &steppingClock{at: rig.now}
		rig.coordinator.now = clock.now
		start := rig.now
		_, run := unsettledHost(t, rig)
		if marked, err := rig.pending.MarkLost(t.Context(), "t1", run.LaunchID,
			JobLoss{Cause: LossEngineRestarted, Node: "node-a", At: rig.now}); err != nil || !marked {
			t.Fatalf("MarkLost = %v, %v", marked, err)
		}
		rig.runner.Lose(Result{Text: "half way through", InputTokens: 900, OutputTokens: 100})
		handedBackUnsettled(t, rig, "the host's store did not answer")

		// THE BOX TIMEOUT ENDS on the host's clock between the attempt's reading
		// as the host's answer comes back and any later reading.
		timeout := DefaultBoxTimeout
		rig.coordinator.boxes = &steppedRead{BoxReader: rig.coordinator.boxes, step: func() {
			clock.step(start.Add(timeout-time.Nanosecond), start.Add(timeout))
		}}
		err := collectAgain(t, rig)
		handedBackNotEnded(t, rig, err)
		if !errors.Is(err, ErrStoreSilent) {
			t.Fatalf("the read handed back = %v; want the host's store's silence", err)
		}

		carriedOn(t, rig, collectAgain(t, rig))
	})
}
