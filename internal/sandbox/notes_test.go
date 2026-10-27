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
	unread   []string
	recorded bool
}

func (s *expirySpy) expired(ctx context.Context, run PendingRun) error {
	_, recorded, _ := s.pending.Get(ctx, run.TurnID)
	var unread []string
	for _, n := range run.UnreadNotes() {
		unread = append(unread, n.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, expiryCall{unread: unread, recorded: recorded})
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
	if !slices.Equal(calls[0].unread, []string{"waiting"}) {
		t.Errorf("the ending handed over waiting notes %v, want only the one nothing read", calls[0].unread)
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
	if len(calls) != 2 || !slices.Equal(calls[1].unread, []string{"waiting"}) || !calls[1].recorded {
		t.Errorf("the attempts handed over %+v, want the waiting note again, before the delete", calls)
	}
}

// ONLY A NOTE NO JOB RAN WITH IS UNREAD: one still waiting, or one the brief of
// the record's job recorded read while that job never started — its command
// never attached. A read by a job that started, a read by an earlier job, and a
// take inside the job are reads, and their records say so.
//
// Mutation: count a brief read unread whatever the job's command, or leave the
// never-started job's out, and this goes red.
func TestOnlyANoteNoJobRanWithIsUnread(t *testing.T) {
	t.Parallel()
	read := func(launch, boundary string) *NoteRead {
		return &NoteRead{Phase: "execute", Round: 1, Boundary: boundary, Launch: launch}
	}
	notes := []RunNote{
		{ID: "waiting"},
		{ID: "briefed", Read: read("job-2", "brief")},
		{ID: "earlier-brief", Read: read("job-1", "brief")},
		{ID: "taken", Read: read("job-2", "tool_batch")},
		{ID: "reviewed", Read: read("", "round")},
	}
	unread := func(run PendingRun) []string {
		var out []string
		for _, n := range run.UnreadNotes() {
			out = append(out, n.ID)
		}
		return out
	}
	neverStarted := PendingRun{LaunchID: "job-2", Steer: RunSteer{Notes: notes}}
	if got := unread(neverStarted); !slices.Equal(got, []string{"waiting", "briefed"}) {
		t.Errorf("a record whose job never started has %v unread, want the waiting note and its "+
			"brief's", got)
	}
	started := neverStarted
	started.CommandID = "4242"
	if got := unread(started); !slices.Equal(got, []string{"waiting"}) {
		t.Errorf("a record whose job started has %v unread, want the waiting note alone", got)
	}
	if !neverStarted.unreadNotes() || (PendingRun{LaunchID: "job-2", CommandID: "4242",
		Steer: RunSteer{Notes: notes[1:]}}).unreadNotes() {
		t.Error("unreadNotes disagrees with UnreadNotes")
	}
}

// A FAILED LAUNCH'S ENDING REPORTS THE NOTES ITS WRITE HELD. The write that
// opens a job records the brief's notes read by it and files the box's waiting,
// and says so to the caller ([LaunchRequest.Recorded]) before anything else can
// fail — so the caller hands none of them back. A launch refused after it, by
// its turn's fence here, ends the record, and that ending hands every one of
// them to be reported expired, before the delete: the brief's too, read by a job
// that never started.
//
// Mutation: call Recorded after the fence, or leave the brief's reads out of
// the ending's, and this goes red.
func TestAFailedLaunchsEndingReportsTheNotesItsWriteHeld(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	spy := &expirySpy{pending: rig.pending}
	c, err := NewCoordinator(CoordinatorOptions{
		Answers: rigAnswers(),
		Queue:   rig.queue, Pending: rig.pending, Manager: rig.manager,
		Resume: &resumeSpy{}, Audience: &audienceSpy{},
		Lease:         leased(rigLease),
		SteersExpired: spy.expired,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(c.Stop)
	req := launchReq("t1")
	req.BriefNotes = []RunNote{{ID: "briefed", Sealed: []byte("sealed:briefed"),
		Read: &NoteRead{Phase: "execute", Iteration: 1, Round: 1, Boundary: "brief"}}}
	req.HandedNotes = []RunNote{{ID: "held", Sealed: []byte("sealed:held")}}
	var recorded []string
	req.Recorded = func(launch string) { recorded = append(recorded, launch) }
	req.Fence = func() error { return errors.New("a person stopped the seat") }
	if _, err := c.Launch(t.Context(), rig.manager, req); err == nil {
		t.Fatal("a launch its fence refused went ahead")
	}
	if len(recorded) != 1 || recorded[0] == "" {
		t.Fatalf("the launch told its caller of the write %v, want once, with the job", recorded)
	}
	calls := spy.seen()
	if len(calls) != 1 || !calls[0].recorded || !slices.Equal(calls[0].unread, []string{"briefed", "held"}) {
		t.Errorf("the ending handed over %+v, want the brief's note and the box's, before the delete", calls)
	}
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Error("the failed launch's record was not deleted")
	}
}
