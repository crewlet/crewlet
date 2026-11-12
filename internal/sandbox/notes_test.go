package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A person's notes as a run's ending meets them: what no step reported reading
// is handed to the engine to record as the record knows it, before the record
// is deleted, once per attempt; and an ending whose notes could not be
// recorded is kept for its retry, as one whose announcement could not be
// published is.

// endSpy records what an ending hands [CoordinatorOptions.SteersEnded], and
// whether the run's record still existed when it did.
type endSpy struct {
	mu      sync.Mutex
	pending PendingStore
	calls   []endCall
	err     error
}

// endCall is one ending's notes, each as "id:outcome", and whether the record
// still existed.
type endCall struct {
	ended    []string
	recorded bool
}

func (s *endSpy) ended(ctx context.Context, run PendingRun) error {
	_, recorded, _ := s.pending.Get(ctx, run.TurnID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, endCall{ended: endsOf(run), recorded: recorded})
	return s.err
}

func (s *endSpy) seen() []endCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// endsOf is run's ended notes, each as "id:outcome".
func endsOf(run PendingRun) []string {
	var out []string
	for _, n := range run.EndedNotes() {
		out = append(out, n.Note.ID+":"+string(n.Outcome))
	}
	return out
}

// watchEnds installs the spy on the rig's coordinator.
func watchEnds(rig *coordRig) *endSpy {
	spy := &endSpy{pending: rig.pending}
	rig.coordinator.steersEnded = spy.ended
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

// AN ENDING RECORDS WHAT NO STEP REPORTED, BEFORE IT DELETES THE RECORD: the
// record is the only thing that holds a waiting note, a take's lost answer and
// a harness's unconfirmed note, so a step left for after the delete is a note
// nobody ever hears the end of. The note already read is not handed over (it
// was delivered, and its record says so), and an ending whose only notes were
// missed or unconfirmed hands them over all the same.
//
// Mutation: hand the engine the waiting notes alone, and the missed and the
// unconfirmed note have no end.
func TestAnEndingRecordsItsWaitingNotesBeforeItsDelete(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spy := watchEnds(rig)
	watchStops(rig)
	running := rig.launch("t1")
	for _, id := range []string{"read", "waiting", "missed", "handed"} {
		fileNote(t, rig.pending, "t1", id)
	}
	take := func(id, name string) {
		t.Helper()
		if _, err := rig.pending.TakeNotes(t.Context(), "t1", NoteTake{IDs: []string{id},
			Launch: running.LaunchID, Statuses: []string{StatusRunning},
			Read: NoteRead{Phase: "execute", Round: 1, Boundary: "tool_batch", Take: name}}); err != nil {
			t.Fatal(err)
		}
	}
	take("read", "answered")
	take("missed", "lost")
	if _, err := rig.pending.SettleTake(t.Context(), "t1", NoteSettle{Take: "lost", Unanswered: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.pending.ServeNotes(t.Context(), "t1", NoteServe{IDs: []string{"handed"},
		Launch: running.LaunchID, Statuses: []string{StatusRunning}}); err != nil {
		t.Fatal(err)
	}

	rig.stop(t)
	rig.fireRetries()

	calls := spy.seen()
	if len(calls) != 1 {
		t.Fatalf("the ending handed its notes over %d times, want once", len(calls))
	}
	if !calls[0].recorded {
		t.Error("the notes were handed over after the record was deleted")
	}
	if want := []string{"waiting:expired", "missed:missed", "handed:unconfirmed"}; !slices.Equal(calls[0].ended,
		want) {
		t.Errorf("the ending handed over %v, want %v: every note no step reported, as its record knows it",
			calls[0].ended, want)
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
	spy := watchEnds(rig)
	watchStops(rig)
	rig.launch("t1")

	rig.stop(t)
	rig.fireRetries()

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
	spy := watchEnds(rig)
	watchStops(rig)
	rig.launch("t1")
	fileNote(t, rig.pending, "t1", "waiting")
	spy.mu.Lock()
	spy.err = errors.New("the event bus refused the record")
	spy.mu.Unlock()

	rig.stop(t)
	rig.fireRetries()

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
	if len(calls) != 2 || !slices.Equal(calls[1].ended, []string{"waiting:expired"}) || !calls[1].recorded {
		t.Errorf("the attempts handed over %+v, want the waiting note again, before the delete", calls)
	}
}

// EACH NOTE ENDS AS ITS RECORD KNOWS IT, in one order of precedence. The ending
// reports what no step reported: a note a later launch's brief told again
// after its take was marked unanswered, DELIVERED there where that job
// started, ahead of every other end, since its harness was handed it; a note a
// job's harness was handed and never confirmed, UNCONFIRMED, ahead of the two
// below, since its session may hold it; one a take whose answer was lost
// recorded read and no later launch told, MISSED; and a note still waiting, or
// one the brief of the record's job was the first to read while that job
// never started, EXPIRED: its record never took its command. A note that
// brief told again is MISSED all the same, at the take whose read the brief
// kept: the brief handed nothing over, and the take's reader never had it.
// Each is reported at the step the record names, a missed note at its take
// and a note told again at the brief, and an unconfirmed or expired one at
// none the record keeps. A read by a job that started, a read by an earlier
// job, a take inside the job, a review's round and a confirmation are reads,
// and their steps reported them; and a job that started still started once
// its box, and the row's command with it, is forgotten.
//
// A NOTE A HOOK WAS SERVED is reported at that serve: UNCONFIRMED at its live
// serve, the step the hook answered, whether or not a brief whose job never
// started read it after; at the job a later poll served it to, where one did,
// the hook's serve no longer live; and MISSED at the last of its serves where
// every one was lost, a cancel or its session proving it never took it, and
// no step read it since, a brief whose job never started included. And an ITEM
// FOR ONE JOB is reported as nothing at all, waiting, served or read: what
// becomes of a job's item is its filer's to say.
//
// Mutation: count a brief read unread whatever the job's record, read the
// witness off the row's command, leave the never-started job's out, report a
// marked read as nothing or as expired, a served note as expired, a note told
// again as nothing, as missed or as delivered where its job started, as
// expired or as delivered where its job never started, report it at the brief
// rather than the take it kept, or put the served or the marked ahead of a
// note told again, and this goes red. And: report a hook's live serve at no
// step, take a serve as live once a later poll moved Served on, report a note
// whose serves were all lost as expired, or report an item, and this goes red.
func TestEachNoteEndsAsItsRecordKnowsIt(t *testing.T) {
	t.Parallel()
	read := func(launch, boundary string) *NoteRead {
		return &NoteRead{Phase: "execute", Round: 1, Boundary: boundary, Launch: launch}
	}
	serve := func(launch string, lost ServeLoss) []HookServed {
		return []HookServed{{Take: "take-" + launch, Launch: launch, Boundary: "tool_batch", Iteration: 3,
			Lost: lost}}
	}
	marked := func(launch, boundary string) *NoteRead {
		r := read(launch, boundary)
		r.Unanswered = true
		return r
	}
	retold := func(launch, took string) *NoteRead {
		r := read(launch, "brief")
		r.Retold = marked(took, "tool_batch")
		return r
	}
	notes := []RunNote{
		{ID: "waiting"},
		{ID: "briefed", Read: read("job-2", "brief")},
		{ID: "earlier-brief", Read: read("job-1", "brief")},
		{ID: "taken", Read: read("job-2", "tool_batch")},
		{ID: "reviewed", Read: read("", "round")},
		{ID: "missed", Read: marked("job-1", "tool_batch")},
		{ID: "review-missed", Read: marked("", "round")},
		{ID: "handed", Served: "job-1"},
		{ID: "confirmed", Served: "job-1", Read: read("job-1", "step")},
		{ID: "handed-missed", Served: "job-1", Read: marked("", "round")},
		{ID: "handed-briefed", Served: "job-1", Read: read("job-2", "brief")},
		{ID: "retold", Read: retold("job-2", "job-1")},
		{ID: "retold-earlier", Read: retold("job-1", "job-0")},
		{ID: "handed-retold", Served: "job-1", Read: retold("job-2", "job-1")},
		{ID: "hook-live", Served: "job-1", Place: 7, Serves: serve("job-1", "")},
		{ID: "hook-polled-later", Served: "job-2", Place: 7, Serves: serve("job-1", "")},
		{ID: "hook-lost", Place: 7, Serves: serve("job-1", ServeCancelled)},
		{ID: "hook-live-briefed", Served: "job-1", Place: 7, Serves: serve("job-1", ""),
			Read: read("job-2", "brief")},
		{ID: "hook-lost-briefed", Place: 7, Serves: serve("job-1", ServeAbsent), Read: read("job-2", "brief")},
		{ID: "item", For: "job-2"},
		{ID: "item-served", For: "job-2", Served: "job-2", Place: 7, Serves: serve("job-2", "")},
		{ID: "item-lost", For: "job-2", Place: 7, Serves: serve("job-2", ServeCancelled)},
	}
	ends := func(want ...string) []string {
		out := append([]string{}, want...)
		out = append(out, "missed:missed", "review-missed:missed", "handed:unconfirmed",
			"handed-missed:unconfirmed", "retold-earlier:delivered", "hook-live:unconfirmed",
			"hook-polled-later:unconfirmed", "hook-lost:missed")
		return out
	}
	sorted := func(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }
	// A NOTE SERVED AND THEN READ INTO THE BRIEF OF A JOB THAT NEVER STARTED is
	// unconfirmed, the session it was served into maybe holding it, whether
	// that brief was the first to tell it or told it again; and a note that
	// brief told again unserved is missed at the take it kept, as it was
	// before that brief.
	neverStarted := PendingRun{LaunchID: "job-2", Steer: RunSteer{Notes: notes}}
	if got, want := endsOf(neverStarted), ends("waiting:expired", "briefed:expired",
		"handed-briefed:unconfirmed", "retold:missed", "handed-retold:unconfirmed",
		"hook-live-briefed:unconfirmed", "hook-lost-briefed:missed"); !slices.Equal(sorted(got),
		sorted(want)) {
		t.Errorf("a record whose job never started ends %v, want %v", got, want)
	}
	// EACH AT THE STEP THE RECORD NAMES.
	at := func(run PendingRun) map[string]string {
		out := map[string]string{}
		for _, n := range run.EndedNotes() {
			out[n.Note.ID] = string(n.Outcome) + "@"
			switch {
			case n.Read != nil:
				out[n.Note.ID] += n.Read.Launch + "/" + n.Read.Boundary
			case n.Serve != nil:
				out[n.Note.ID] += "serve:" + n.Serve.Launch + "/" + n.Serve.Boundary
			}
		}
		return out
	}
	for id, want := range map[string]string{"retold": "missed@job-1/tool_batch",
		"missed": "missed@job-1/tool_batch", "review-missed": "missed@/round",
		"retold-earlier": "delivered@job-1/brief", "handed-retold": "unconfirmed@", "waiting": "expired@",
		"briefed": "expired@", "hook-live": "unconfirmed@serve:job-1/tool_batch",
		"hook-polled-later": "unconfirmed@", "hook-lost": "missed@serve:job-1/tool_batch",
		"hook-live-briefed": "unconfirmed@serve:job-1/tool_batch",
		"hook-lost-briefed": "missed@serve:job-1/tool_batch"} {
		if got := at(neverStarted)[id]; got != want {
			t.Errorf("a record whose job never started reports note %s %s, want %s", id, got, want)
		}
	}
	// ONE READ INTO THE BRIEF OF A JOB THAT STARTED was delivered there: by
	// the runner that briefed it where the brief was the first to tell it, and
	// by the ending where it told the note again, served or not.
	started := neverStarted
	started.CommandID = "4242"
	started.Launch.Started = true
	wantStarted := ends("waiting:expired", "retold:delivered", "handed-retold:delivered")
	if got := endsOf(started); !slices.Equal(sorted(got), sorted(wantStarted)) {
		t.Errorf("a record whose job started ends %v, want %v", got, wantStarted)
	}
	if got := at(started)["retold"]; got != "delivered@job-2/brief" {
		t.Errorf("a record whose job started reports the note it told again %s, want it delivered at its brief",
			got)
	}
	boxForgotten := started
	boxForgotten.CommandID = ""
	if got := endsOf(boxForgotten); !slices.Equal(sorted(got), sorted(wantStarted)) {
		t.Errorf("a record whose started job's box was forgotten ends %v, want %v", got, wantStarted)
	}
	attachedNoStart := neverStarted
	attachedNoStart.CommandID = "4242"
	if got := endsOf(attachedNoStart); !slices.Contains(got, "briefed:expired") ||
		!slices.Contains(got, "retold:missed") {
		t.Errorf("a record naming a command its job's record never took ends %v, want its brief's notes "+
			"expired, the one it told again missed", got)
	}
	allRead := PendingRun{LaunchID: "job-2", Launch: LaunchRecord{Started: true},
		Steer: RunSteer{Notes: []RunNote{notes[3], notes[4], notes[8]}}}
	if !neverStarted.endsNotes() || allRead.endsNotes() {
		t.Error("endsNotes disagrees with EndedNotes")
	}
	// AN ENDING WHOSE ONLY NOTE WAS MISSED, handed over and never confirmed,
	// or told again, has notes to report all the same.
	for _, only := range []RunNote{notes[5], notes[7], notes[12]} {
		if alone := (PendingRun{Steer: RunSteer{Notes: []RunNote{only}}}); !alone.endsNotes() {
			t.Errorf("a record whose only note is %s has nothing to report", only.ID)
		}
	}
	for _, run := range []PendingRun{neverStarted, started} {
		for _, n := range run.EndedNotes() {
			missedAt := (n.Read != nil && n.Read.Unanswered) || (n.Serve != nil && n.Serve.Lost != "")
			if !n.Outcome.Valid() || (n.Outcome == types.SteerDelivered && (n.Read == nil || n.Read.Retold == nil)) ||
				(n.Outcome == types.SteerMissed && !missedAt) ||
				(n.Outcome == types.SteerUnconfirmed && n.Serve != nil && n.Serve.Lost != "") || n.Note.For != "" {
				t.Errorf("note %s ends %q at %+v (serve %+v), which an ending never reports of it", n.Note.ID,
					n.Outcome, n.Read, n.Serve)
			}
		}
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
	spy := &endSpy{pending: rig.pending}
	c, err := NewCoordinator(CoordinatorOptions{
		Answers: rigAnswers(),
		Queue:   rig.queue, Pending: rig.pending, Manager: rig.manager,
		Resume: &resumeSpy{}, Audience: &audienceSpy{}, Pushes: &pushSpy{},
		Lease:       leased(rigLease),
		SteersEnded: spy.ended,
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
	if len(calls) != 1 || !calls[0].recorded ||
		!slices.Equal(calls[0].ended, []string{"briefed:expired", "held:expired"}) {
		t.Errorf("the ending handed over %+v, want the brief's note and the box's, before the delete", calls)
	}
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Error("the failed launch's record was not deleted")
	}
}

// A PARKED RUN'S ENDING NEVER REPORTS EXPIRED THE NOTES ITS JOB'S BRIEF CARRIED,
// whatever forgot its box first. A launch whose job started records the job
// started ([LaunchRecord.Started]), and its turn's runner has recorded the
// brief's notes delivered by then; a run parked past its pause, or one whose box
// a park's teardown released, no longer names that job's command, and its
// ending — the seat leaving the company here — must hand over the note still
// waiting and nothing its job ran with. Reported again, one note would read
// `delivered` and then `expired`.
//
// Mutation: read the job's start off the row's command (briefNeverRan), or
// clear it with the box, and the ending hands over the brief's note.
func TestAParkedRunsEndingNeverExpiresTheNotesItsJobsBriefCarried(t *testing.T) {
	t.Parallel()
	for name, forget := range map[string]func(t *testing.T, s PendingStore){
		"its pause expired": func(t *testing.T, s PendingStore) {
			if won, err := s.ExpirePause(t.Context(), "t1"); err != nil || !won {
				t.Fatalf("ExpirePause = %v, %v", won, err)
			}
		},
		"its box was released at the park": func(t *testing.T, s PendingStore) {
			if err := s.ReleaseBox(t.Context(), "t1"); err != nil {
				t.Fatalf("ReleaseBox: %v", err)
			}
			if err := s.SetStatus(t.Context(), "t1", StatusReseed, rigLease); err != nil {
				t.Fatalf("SetStatus: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			spy := watchEnds(rig)
			req := launchReq("t1")
			req.BriefNotes = []RunNote{{ID: "briefed", Sealed: []byte("sealed:briefed"),
				Read: &NoteRead{Phase: "execute", Iteration: 1, Round: 1, Boundary: "brief"}}}
			if _, err := rig.coordinator.Launch(t.Context(), rig.manager, req); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			rig.suspend("t1")
			rig.ask("t1", Ask{Question: "which branch?", To: AskToRequester, Decision: choosing("which branch?"), Branch: "wip/t1"})
			if err := rig.pending.MarkAwaiting(t.Context(), "t1", Clarification{AskedAt: rig.now}); err != nil {
				t.Fatalf("MarkAwaiting: %v", err)
			}
			forget(t, rig.pending)
			if run := rig.get("t1"); run.CommandID != "" {
				t.Fatalf("the park left the job's command %q on the row", run.CommandID)
			}
			fileNote(t, rig.pending, "t1", "waiting")

			if _, err := rig.coordinator.RetireSeat(t.Context(), sweSeat(), "swe", "retirement:1", 12); err != nil {
				t.Fatalf("RetireSeat: %v", err)
			}

			calls := spy.seen()
			if len(calls) != 1 || !calls[0].recorded || !slices.Equal(calls[0].ended, []string{"waiting:expired"}) {
				t.Errorf("the ending handed over %+v, want the waiting note alone, before the delete", calls)
			}
			if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
				t.Error("the ending did not delete the record")
			}
		})
	}
}
