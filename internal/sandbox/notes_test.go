package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
)

// A person's notes as a run's ending meets them: what nothing will read now is
// handed to the engine to record expired — before the record is deleted, once
// per attempt — and an ending whose notes could not be recorded is kept for its
// retry, as one whose announcement could not be published is.

// expirySpy records what an ending hands [CoordinatorOptions.SteersExpired],
// and whether the run's record still existed when it did.
type expirySpy struct {
	mu      sync.Mutex
	pending PendingStore
	calls   []expiryCall
	err     error
}

type expiryCall struct {
	waiting  []string
	recorded bool
}

func (s *expirySpy) expired(ctx context.Context, run PendingRun) error {
	_, recorded, _ := s.pending.Get(ctx, run.TurnID)
	var waiting []string
	for _, n := range run.Steer.Notes {
		if n.Waiting() {
			waiting = append(waiting, n.ID)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, expiryCall{waiting: waiting, recorded: recorded})
	return s.err
}

func (s *expirySpy) seen() []expiryCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// watchExpiry installs the spy on the rig's coordinator.
func watchExpiry(rig *coordRig) *expirySpy {
	spy := &expirySpy{pending: rig.pending}
	rig.coordinator.steersExpired = spy.expired
	return spy
}

// fileNote files a sealed note on a run's record, under the turn's bounds.
func fileNote(t *testing.T, s PendingStore, turnID, id string) {
	t.Helper()
	filed, _, err := s.FileNote(t.Context(), turnID, NoteFiling{
		Note: RunNote{ID: id, Sealed: []byte("sealed:" + id)}, MaxWaiting: 5, MaxNotes: 10,
	})
	if err != nil || filed != NoteAccepted {
		t.Fatalf("file %s = %s, %v", id, filed, err)
	}
}

// AN ENDING RECORDS WHAT NOTHING WILL READ, BEFORE IT DELETES THE RECORD: the
// record is the only thing that holds a waiting note, so a step left for after
// the delete is a note nobody ever hears the end of. The note already read is
// not handed over — it was delivered, and its record says so.
func TestAnEndingRecordsItsWaitingNotesBeforeItsDelete(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spy := watchExpiry(rig)
	watchStops(rig)
	running := rig.launch("t1")
	fileNote(t, rig.pending, "t1", "read")
	fileNote(t, rig.pending, "t1", "waiting")
	if _, err := rig.pending.TakeNotes(t.Context(), "t1", NoteTake{IDs: []string{"read"},
		Launch: running.LaunchID, Statuses: []string{StatusRunning},
		Read: NoteRead{Phase: "execute", Round: 1, Boundary: "tool_batch"}}); err != nil {
		t.Fatal(err)
	}

	rig.stop(t)

	calls := spy.seen()
	if len(calls) != 1 {
		t.Fatalf("the ending handed its notes over %d times, want once", len(calls))
	}
	if !calls[0].recorded {
		t.Error("the notes were handed over after the record was deleted")
	}
	if !slices.Equal(calls[0].waiting, []string{"waiting"}) {
		t.Errorf("the ending handed over waiting notes %v, want only the one nothing read", calls[0].waiting)
	}
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Error("the ending did not delete the record")
	}
}

// AN ENDING WITH NOTHING WAITING HANDS NOTHING OVER: the engine is not asked to
// open and publish anything for a turn whose every note was read.
func TestAnEndingWithNothingWaitingRecordsNothing(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spy := watchExpiry(rig)
	watchStops(rig)
	rig.launch("t1")

	rig.stop(t)

	if calls := spy.seen(); len(calls) != 0 {
		t.Errorf("an ending with no waiting note handed notes over: %+v", calls)
	}
}

// AN ENDING WHOSE NOTES COULD NOT BE RECORDED IS KEPT, its record with it, for
// the attempt that can — as an ending whose announcement could not be published
// is. Deleted anyway, the waiting notes would be lost with the only thing that
// held them.
func TestAnEndingWhoseNotesCouldNotBeRecordedKeepsItsRecord(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spy := watchExpiry(rig)
	watchStops(rig)
	rig.launch("t1")
	fileNote(t, rig.pending, "t1", "waiting")
	spy.mu.Lock()
	spy.err = errors.New("the event bus refused the record")
	spy.mu.Unlock()

	rig.stop(t)

	run, found, err := rig.pending.Get(t.Context(), "t1")
	if err != nil || !found || run.Ending == nil {
		t.Fatalf("the record went (found=%v, err=%v) with its notes unrecorded", found, err)
	}
	spy.mu.Lock()
	spy.err = nil
	spy.mu.Unlock()
	if err := rig.recoverSeat(t.Context(), "node-b:1", 9); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the ending was never finished")
	}
	calls := spy.seen()
	if len(calls) != 2 || !slices.Equal(calls[1].waiting, []string{"waiting"}) || !calls[1].recorded {
		t.Errorf("the attempts handed over %+v, want the waiting note again, before the delete", calls)
	}
}
