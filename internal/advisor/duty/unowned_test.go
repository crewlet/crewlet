package duty_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
)

// recordOf is the record l holds for run, nil where it holds none.
func recordOf(l *fakeLog, run string) *duty.Run {
	if i := slices.IndexFunc(l.runs, func(r duty.Run) bool { return r.ID == run }); i >= 0 {
		return &l.runs[i]
	}
	return nil
}

// heardAt writes run's lines at each instant, as a holder that stopped left them.
func heardAt(t *testing.T, l *fakeLog, run string, at ...time.Time) {
	t.Helper()
	for _, a := range at {
		if err := l.Progress(t.Context(), run, duty.Line{At: a, Step: advisordomain.ProgressNote,
			Text: "reading capacity"}); err != nil {
			t.Fatal(err)
		}
	}
}

// AN ORPHANED SLOT RUN IS RECORDED INTERRUPTED ON THE NEXT TICK: a deep run of
// an earlier slot that left lines and no record is closed as interrupted, from
// its first line to its last, saying why, whether or not a model is assigned,
// and the slot's own run starts fresh. Mutation: leave it, and it reads as
// running until its lines' retention.
func TestAnOrphanedSlotRunIsRecordedInterruptedOnTheNextTick(t *testing.T) {
	t.Parallel()
	began := time.Date(2030, 1, 6, 3, 0, 0, 0, time.UTC)
	for _, assigned := range []bool{true, false} {
		l := &fakeLog{}
		heardAt(t, l, "orphan", began, began.Add(7*time.Minute))
		d := newDuty(l, &fakeReader{signature: "r1"}, nil, &clock{now: start})
		deep := &fakeDeep{assigned: assigned, release: make(chan struct{})}
		close(deep.release)
		d.Deep = deep
		step(t, d)
		d.WaitDeep()
		got := recordOf(l, "orphan")
		if got == nil || got.Status != duty.RunInterrupted || got.Kind != advisordomain.RunKindDeep ||
			got.Trigger != duty.TriggerSchedule || !got.StartedAt.Equal(began) ||
			!got.EndedAt.Equal(began.Add(7*time.Minute)) {
			t.Fatalf("assigned %v: the orphan's record = %+v, want it interrupted from its first line to its last",
				assigned, got)
		}
		if model, _ := got.Model.(map[string]any); model["reason"] != duty.InterruptedSaid {
			t.Errorf("assigned %v: the record says %v, want why in plain words", assigned, got.Model)
		}
		if assigned && (deep.runs != 1 || deep.resumed[0] != "" || recordOf(l, "run-1") == nil) {
			t.Errorf("the slot's own run ran %d times, handed %q; want it once, fresh, under a new id",
				deep.runs, deep.resumed)
		}
	}
}

// AN ORPHAN OF THE CURRENT SLOT IS RESUMED UNDER ITS OWN ID: a deep run begun
// within the slot whose holder stopped, while no deep run has concluded since,
// goes on from its checkpoint under its id, recorded once as begun when it
// began, and no second run starts. Mutation: start the slot's run under a new
// id, and the company pays for the answered items again while the orphan reads
// as running.
func TestAnOrphanOfTheCurrentSlotIsResumedUnderItsOwnId(t *testing.T) {
	t.Parallel()
	slot := time.Date(2030, 1, 9, 3, 0, 0, 0, time.UTC)
	l := &fakeLog{checkpoints: map[string]json.RawMessage{"orphan": json.RawMessage(`{"agenda":[],"run":0}`)}}
	heardAt(t, l, "orphan", slot, slot.Add(10*time.Minute))
	c := &clock{now: slot.Add(30 * time.Minute)}
	d := newDuty(l, &fakeReader{signature: "r1"}, nil, c)
	deep := &fakeDeep{assigned: true, release: make(chan struct{})}
	close(deep.release)
	d.Deep = deep
	step(t, d)
	d.WaitDeep()
	if deep.runs != 1 || deep.resumed[0] != `{"agenda":[],"run":0}` {
		t.Fatalf("the deep run ran %d times, handed %q; want once, from the orphan's checkpoint", deep.runs, deep.resumed)
	}
	got := recordOf(l, "orphan")
	if got == nil || got.Status != duty.RunDone || !got.StartedAt.Equal(slot) || len(l.runs) != 1 {
		t.Errorf("runs = %+v, want the orphan recorded once as done, begun at its first line", l.runs)
	}
	step(t, d)
	d.WaitDeep()
	if deep.runs != 1 {
		t.Errorf("the slot ran %d deep runs, want the one taken over", deep.runs)
	}
}

// A KEPT UNRECORDED SLOT RUN IS RECORDED AGAIN, never closed or run again: a
// slot's deep run whose record the log could not take is this holder's, so
// while the log is down it is neither taken over nor closed, and the first
// tick the log takes records writes it. Mutation: record kept runs only for a
// press, and a slot's run reads as running until the next holder closes it.
func TestAKeptUnrecordedSlotRunIsRecordedAgain(t *testing.T) {
	t.Parallel()
	l, c := &fakeLog{}, &clock{now: time.Date(2030, 1, 9, 3, 30, 0, 0, time.UTC)}
	d := newDuty(l, &fakeReader{signature: "r1"}, nil, c)
	down := &refusingLog{fakeLog: l, down: true}
	d.Log = down
	deep := &fakeDeep{assigned: true, release: make(chan struct{})}
	close(deep.release)
	d.Deep = deep
	step(t, d)
	d.WaitDeep()
	if recordOf(l, "run-1") != nil {
		t.Fatalf("with the log down the slot's run was recorded: %+v", l.runs)
	}
	if err := d.Step(t.Context()); err == nil {
		t.Error("a tick that could not record the kept run said nothing")
	}
	d.WaitDeep()
	down.mu.Lock()
	down.down = false
	down.mu.Unlock()
	step(t, d)
	d.WaitDeep()
	got := recordOf(l, "run-1")
	if deep.runs != 1 || got == nil || got.Status != duty.RunFailed || got.Kind != advisordomain.RunKindDeep {
		t.Errorf("the slot ran %d deep runs and recorded %+v, want it run once and its kept record written",
			deep.runs, got)
	}
}

// A PRESSED RUN IS RESUMED, NEVER CLOSED: a press whose holder stopped mid-run
// keeps its request, and the next holder runs it from the checkpoint it left,
// recording it once as the press. Mutation: close it as interrupted, and the
// person's press is answered with nothing.
func TestAPressedRunIsResumedNeverClosed(t *testing.T) {
	t.Parallel()
	l := &fakeLog{checkpoints: map[string]json.RawMessage{"pressed": json.RawMessage(`{"agenda":[],"run":0}`)},
		requested: []duty.Request{{Run: "pressed", By: "owner", Kind: advisordomain.RunKindDeep}}}
	heardAt(t, l, "pressed", start.Add(-10*time.Minute))
	d := newDuty(l, &fakeReader{signature: "r1"}, nil, &clock{now: start})
	deep := &fakeDeep{assigned: true, release: make(chan struct{})}
	close(deep.release)
	d.Deep = deep
	step(t, d)
	d.WaitDeep()
	got := recordOf(l, "pressed")
	if deep.runs != 1 || deep.resumed[0] != `{"agenda":[],"run":0}` || got == nil ||
		got.Status == duty.RunInterrupted || got.Trigger != duty.TriggerPerson || len(l.runs) != 1 {
		t.Errorf("runs = %+v (deep runs %d, handed %q), want the press resumed and recorded once as the press",
			l.runs, deep.runs, deep.resumed)
	}
}

// A HOLDER WHOSE RENEWALS FAIL FOR THE LEASE'S TTL CANCELS ITS DEEP RUN, and
// one whose run ends past its last claim's TTL records nothing: a peer may hold
// the lease by then and close or resume the run. A renewal failing within the
// TTL leaves the run going. Mutation: keep the run going while renewals fail,
// and two holders write one run.
func TestAHolderWhoseRenewalsFailForTheLeaseTTLCancelsItsDeepRun(t *testing.T) {
	t.Parallel()
	slot := time.Date(2030, 1, 9, 3, 30, 0, 0, time.UTC)
	l, c := &fakeLog{}, &clock{now: slot}
	d := newDuty(l, &fakeReader{signature: "r1"}, nil, c)
	deep := &fakeDeep{assigned: true, release: make(chan struct{}), wrote: make(chan struct{}, 1)}
	d.Deep = deep
	var renew error
	d.Claim = func(context.Context) (bool, error) { return renew == nil, renew }
	step(t, d)
	<-deep.wrote
	renew = errors.New("coord: the store is unreachable")
	c.now = slot.Add(duty.LeaseTTL - time.Second)
	err := d.Step(t.Context())
	deep.mu.Lock()
	canceled := deep.canceled
	deep.mu.Unlock()
	if err == nil || canceled {
		t.Fatalf("a renewal failing within the TTL: %v, canceled %v; want the error and the run going", err, canceled)
	}
	c.now = slot.Add(duty.LeaseTTL)
	if err := d.Step(t.Context()); err == nil {
		t.Fatal("a failed renewal answered no error")
	}
	d.WaitDeep()
	deep.mu.Lock()
	canceled = deep.canceled
	deep.mu.Unlock()
	if !canceled || len(l.runs) != 0 {
		t.Errorf("after renewals failed for the TTL: canceled %v, runs %+v; want it stopped, nothing recorded",
			canceled, l.runs)
	}

	// A RUN ENDING PAST ITS LAST CLAIM'S TTL, before any tick noticed.
	l2, c2 := &fakeLog{}, &clock{now: slot}
	d2 := newDuty(l2, &fakeReader{signature: "r1"}, nil, c2)
	deep2 := &fakeDeep{assigned: true, release: make(chan struct{}), wrote: make(chan struct{}, 1)}
	d2.Deep = deep2
	d2.Claim = func(context.Context) (bool, error) { return true, nil }
	step(t, d2)
	<-deep2.wrote
	c2.now = slot.Add(duty.LeaseTTL + time.Minute)
	close(deep2.release)
	d2.WaitDeep()
	if len(l2.runs) != 0 || len(l2.recorded) != 0 {
		t.Errorf("a run ending past its lease wrote runs %+v and steps %+v, want nothing", l2.runs, l2.recorded)
	}
}
