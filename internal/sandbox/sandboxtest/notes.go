package sandboxtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A person's notes on a run's record: the cases [Run] drives for them.
//
// EVERY ONE IS A PROPERTY OF THE STORE'S CONDITIONAL WRITES: a note waits until
// the write that hands it over records it read, a take is pinned to its job,
// its statuses, an undecided ending and its lease, a retry is one note, and the
// turn's bounds hold under writers racing for one record. The engine above it
// opens the seals and decides who may file; none of that is here, because none
// of it is the store's.

// noteCases are the note cases, in the shape [Run] drives.
var noteCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"ANoteWaitsUntilATakeReadsIt", testANoteWaitsUntilATakeReadsIt},
	{"TwoTakesOfOneNoteReadItOnce", testTwoTakesOfOneNoteReadItOnce},
	{"ATakeIsPinnedToItsJobItsStatusesAndAnUndecidedEnding",
		testATakeIsPinnedToItsJobItsStatusesAndAnUndecidedEnding},
	{"ADuplicateNoteIsOneNote", testADuplicateNoteIsOneNote},
	{"TheSixthWaitingNoteIsFull", testTheSixthWaitingNoteIsFull},
	{"TheEleventhNoteOfATurnIsSpent", testTheEleventhNoteOfATurnIsSpent},
	{"ANoteToARunWhoseEndingIsDecidedIsClosed", testANoteToARunWhoseEndingIsDecidedIsClosed},
	{"AParkKeepsEveryWaitingNote", testAParkKeepsEveryWaitingNote},
	{"ALaunchAnchorsTheSeatAndReadsItsBriefUnderTheNewJob",
		testALaunchAnchorsTheSeatAndReadsItsBriefUnderTheNewJob},
	{"AResetKeepsTheTurnsNotesAndClearsPulled", testAResetKeepsTheTurnsNotesAndClearsPulled},
	{"ALaunchFilesTheBoxsNotesWaitingAfterItsBrief", testALaunchFilesTheBoxsNotesWaitingAfterItsBrief},
	{"EveryWriteThatRecordsAReadTakesTheNextPlace", testEveryWriteThatRecordsAReadTakesTheNextPlace},
	{"APullIsRecordedOnceAndOnlyForItsJob", testAPullIsRecordedOnceAndOnlyForItsJob},
	{"AStartedJobsBriefStaysReadThroughEveryWriteThatForgetsItsBox",
		testAStartedJobsBriefStaysReadThroughEveryWriteThatForgetsItsBox},
	{"NotesRideABridgedCallStorm", testNotesRideABridgedCallStorm},
	{"AWatchSeesEveryNoteChange", testAWatchSeesEveryNoteChange},
	{"ATakeWhoseAnswerWasLostIsSettledByItsIdentity", testATakeWhoseAnswerWasLostIsSettledByItsIdentity},
}

// noteRawCases are the note cases that read the raw record.
var noteRawCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore, coord.SandboxRuns)
}{
	{"NotesSurviveEveryWriteThatIsNotTheirs", testNotesSurviveEveryWriteThatIsNotTheirs},
	{"AKeyALaterBuildAddsInsideASectionSurvivesEveryWrite",
		testAKeyALaterBuildAddsInsideASectionSurvivesEveryWrite},
	{"AStateTheNoteViewCannotReadIsPassedOver", testAStateTheNoteViewCannotReadIsPassedOver},
	{"ASettleOfATakeThatRecordedNothingFencesItsWrite", testASettleOfATakeThatRecordedNothingFencesItsWrite},
}

// The turn's bounds every filing here is held to: internal/agent/steer's,
// written out because this suite must not import the agent layer either.
const (
	maxWaiting = 5
	maxNotes   = 10
)

// sealedNote is a note as the engine files it: its id, and a seal the store
// keeps as bytes and never opens.
func sealedNote(id string) sandbox.RunNote {
	return sandbox.RunNote{ID: id, Sealed: []byte("sealed:" + id), At: base}
}

// file files a note under the turn's bounds.
func file(ctx context.Context, s sandbox.PendingStore, turnID, id string) (sandbox.NoteFiled, sandbox.PendingRun, error) {
	return s.FileNote(ctx, turnID, sandbox.NoteFiling{
		Note: sealedNote(id), MaxWaiting: maxWaiting, MaxNotes: maxNotes,
	})
}

// mustFile files a note and fails the case unless it is accepted.
func mustFile(t *testing.T, s sandbox.PendingStore, turnID, id string) {
	t.Helper()
	filed, _, err := file(t.Context(), s, turnID, id)
	if err != nil || filed != sandbox.NoteAccepted {
		t.Fatalf("file %s on %s = %s, %v, want accepted", id, turnID, filed, err)
	}
}

// bridgeTake is a take the run's tool bridge makes: pinned to its job, while
// the run is launching or running, at a boundary of the harness's own loop.
func bridgeTake(launch string, ids ...string) sandbox.NoteTake {
	return sandbox.NoteTake{
		IDs: ids, Launch: launch,
		Statuses: []string{sandbox.StatusLaunching, sandbox.StatusRunning},
		Read: sandbox.NoteRead{Phase: "execute", Iteration: 1, Round: 1,
			Boundary: "tool_batch", Launch: launch},
	}
}

// namedTake is [bridgeTake] under the identity take, which a settle finds its
// reads by.
func namedTake(launch, take string, ids ...string) sandbox.NoteTake {
	t := bridgeTake(launch, ids...)
	t.Read.Take = take
	return t
}

// waitingIDs is the ids of the notes on turnID's record nothing has read.
func waitingIDs(t *testing.T, s sandbox.PendingStore, turnID string) []string {
	t.Helper()
	var out []string
	for _, n := range mustGet(t, s, turnID).Steer.Notes {
		if n.Waiting() {
			out = append(out, n.ID)
		}
	}
	return out
}

// A NOTE WAITS UNTIL A TAKE READS IT, and the take is the handover: the write
// that records it read answers it — with where it was read — and a second take
// of it takes nothing, so no note is handed to a model twice.
func testANoteWaitsUntilATakeReadsIt(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-wait"))
	filed, written, err := file(ctx, s, "t-wait", "n-1")
	if err != nil || filed != sandbox.NoteAccepted {
		t.Fatalf("file = %s, %v", filed, err)
	}
	if len(written.Steer.Notes) != 1 || written.Steer.Notes[0].Seq != 1 ||
		!bytes.Equal(written.Steer.Notes[0].Sealed, []byte("sealed:n-1")) {
		t.Fatalf("the filing answered the record %+v, want the note numbered 1 with its seal", written.Steer)
	}
	if got := waitingIDs(t, s, "t-wait"); !slices.Equal(got, []string{"n-1"}) {
		t.Fatalf("waiting = %v, want the note", got)
	}
	taken, err := s.TakeNotes(ctx, "t-wait", bridgeTake(launch, "n-1"))
	if err != nil || len(taken) != 1 || taken[0].ID != "n-1" {
		t.Fatalf("take = %+v, %v, want the note", taken, err)
	}
	read := taken[0].Read
	if read == nil || read.Boundary != "tool_batch" || read.Launch != launch || read.Phase != "execute" ||
		read.Round != 1 || read.At.IsZero() {
		t.Errorf("the take recorded the read %+v", read)
	}
	if got := waitingIDs(t, s, "t-wait"); len(got) != 0 {
		t.Errorf("a note the take read still waits: %v", got)
	}
	stored := mustGet(t, s, "t-wait").Steer.Notes[0].Read
	if stored == nil || !reflect.DeepEqual(*stored, *read) {
		t.Errorf("the record holds the read %+v, the take answered %+v", stored, read)
	}
	again, err := s.TakeNotes(ctx, "t-wait", bridgeTake(launch, "n-1"))
	if err != nil || len(again) != 0 {
		t.Errorf("a second take of a read note = %+v, %v, want nothing", again, err)
	}
	// A TAKE NAMING NOTHING TAKES NOTHING: the taker names what it can
	// deliver, and "everything" is never what a taker that could open none
	// of it means.
	mustFile(t, s, "t-wait", "n-2")
	if none, err := s.TakeNotes(ctx, "t-wait", bridgeTake(launch)); err != nil || len(none) != 0 {
		t.Errorf("a take naming no note took %+v, %v", none, err)
	}
	if got := waitingIDs(t, s, "t-wait"); !slices.Equal(got, []string{"n-2"}) {
		t.Errorf("waiting = %v after a take naming nothing", got)
	}
}

// TWO TAKES OF ONE NOTE READ IT ONCE: a harness's hook and the reviewer — or two
// boxes a seat move left serving one run — racing for the same note get it
// exactly once between them.
func testTwoTakesOfOneNoteReadItOnce(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-race"))
	mustFile(t, s, "t-race", "n-1")
	const takers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		got  int
		errs []error
	)
	for range takers {
		wg.Go(func() {
			taken, err := s.TakeNotes(ctx, "t-race", bridgeTake(launch, "n-1"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			got += len(taken)
		})
	}
	wg.Wait()
	for _, err := range errs {
		t.Errorf("a take failed: %v", err)
	}
	if got != 1 {
		t.Fatalf("%d concurrent takes handed the one note over %d times, want once", takers, got)
	}
}

// A TAKE IS PINNED, every way it can be stale: the job it serves, the statuses
// its taker may act in, an ending not decided, and its lease. Each case here is
// a taker that is no longer the turn's, and takes nothing — the note stays for
// the step that is.
func testATakeIsPinnedToItsJobItsStatusesAndAnUndecidedEnding(t *testing.T, s sandbox.PendingStore) {
	// EACH CASE ON ITS OWN CONTEXT, the subtest's: a case's writes belong to
	// it, and end with it.
	notTaken := func(t *testing.T, turnID string, take sandbox.NoteTake, why string) {
		t.Helper()
		taken, err := s.TakeNotes(t.Context(), turnID, take)
		if err != nil {
			t.Fatalf("%s: %v", why, err)
		}
		if len(taken) != 0 {
			t.Fatalf("%s took %+v", why, taken)
		}
		if got := waitingIDs(t, s, turnID); !slices.Contains(got, "n-1") {
			t.Fatalf("%s: the note no longer waits (%v)", why, got)
		}
	}
	t.Run("a replaced job", func(t *testing.T) {
		previous := mustLaunched(t, s, run("t-pin-job"))
		mustFile(t, s, "t-pin-job", "n-1")
		mustClaim(t, s, "t-pin-job")
		current := mustBeginLaunch(t, s, run("t-pin-job"))
		notTaken(t, "t-pin-job", bridgeTake(previous, "n-1"),
			"the previous job's session, after a relaunch replaced it,")
		if taken, err := s.TakeNotes(t.Context(), "t-pin-job", bridgeTake(current, "n-1")); err != nil || len(taken) != 1 {
			t.Errorf("the current job's take = %+v, %v, want the note", taken, err)
		}
	})
	t.Run("a resumed run, for the bridge", func(t *testing.T) {
		launch := mustLaunched(t, s, run("t-pin-resumed"))
		mustFile(t, s, "t-pin-resumed", "n-1")
		claimed := mustClaim(t, s, "t-pin-resumed")
		notTaken(t, "t-pin-resumed", bridgeTake(launch, "n-1"),
			"the bridge's take, once the run's completion was claimed,")
		reviewer := sandbox.NoteTake{IDs: []string{"n-1"}, Launch: launch,
			Statuses: []string{sandbox.StatusResumed},
			Fence:    sandbox.Fence{Owner: claimed.Owner, Epoch: claimed.OwnerEpoch},
			Read:     sandbox.NoteRead{Phase: "review", Iteration: 1, Round: 1, Boundary: "round"}}
		taken, err := s.TakeNotes(t.Context(), "t-pin-resumed", reviewer)
		if err != nil || len(taken) != 1 || taken[0].Read.Boundary != "round" || taken[0].Read.Launch != "" {
			t.Errorf("the resumed segment's take = %+v, %v, want the note read at a round", taken, err)
		}
	})
	t.Run("a decided ending", func(t *testing.T) {
		launch := mustLaunched(t, s, run("t-pin-ending"))
		mustFile(t, s, "t-pin-ending", "n-1")
		decide(t, s, "t-pin-ending", everyJob(sandbox.Fence{}, sandbox.Active))
		notTaken(t, "t-pin-ending", bridgeTake(launch, "n-1"), "a take after the ending was decided")
	})
	t.Run("a parked run", func(t *testing.T) {
		launch := mustLaunched(t, s, run("t-pin-park"))
		mustFile(t, s, "t-pin-park", "n-1")
		park(t, s, "t-pin-park")
		notTaken(t, "t-pin-park", bridgeTake(launch, "n-1"), "the bridge's take of a parked run")
	})
	t.Run("an outranked lease", func(t *testing.T) {
		launch := mustLaunched(t, s, run("t-pin-lease"))
		mustFile(t, s, "t-pin-lease", "n-1")
		if ok, err := s.ClaimOwnership(t.Context(), "t-pin-lease", "node-b", 5); err != nil || !ok {
			t.Fatalf("ClaimOwnership = %v, %v", ok, err)
		}
		stale := bridgeTake(launch, "n-1")
		stale.Fence = sandbox.Fence{Owner: "node-a", Epoch: 4}
		notTaken(t, "t-pin-lease", stale, "a take under a lease a newer one outranks")
	})
	t.Run("a run with no record", func(t *testing.T) {
		taken, err := s.TakeNotes(t.Context(), "t-pin-gone", bridgeTake("job-gone", "n-1"))
		if err != nil || len(taken) != 0 {
			t.Errorf("a take on no record = %+v, %v, want nothing and no error", taken, err)
		}
	})
}

// A RETRY IS ONE NOTE: a person's request retried after an answer lost in
// transit carries the same id, and the record holds it once — answered
// duplicate, never a second note the turn would act on twice.
func testADuplicateNoteIsOneNote(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-dup"))
	mustFile(t, s, "t-dup", "n-1")
	filed, _, err := file(ctx, s, "t-dup", "n-1")
	if err != nil || filed != sandbox.NoteDuplicate {
		t.Fatalf("a retry was filed %s, %v, want duplicate", filed, err)
	}
	// AND AFTER IT WAS READ: a retry is still the same note.
	if _, takeErr := s.TakeNotes(ctx, "t-dup", bridgeTake(launch, "n-1")); takeErr != nil {
		t.Fatal(takeErr)
	}
	if filed, _, err = file(ctx, s, "t-dup", "n-1"); err != nil || filed != sandbox.NoteDuplicate {
		t.Fatalf("a retry after the read was filed %s, %v, want duplicate", filed, err)
	}
	if notes := mustGet(t, s, "t-dup").Steer.Notes; len(notes) != 1 {
		t.Errorf("the record holds %d notes for one request", len(notes))
	}
}

// THE SIXTH WAITING NOTE IS FULL, and a read makes room: "waiting" is not yet
// read, which is what the bound protects — one step's reading.
func testTheSixthWaitingNoteIsFull(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-full"))
	for i := range maxWaiting {
		mustFile(t, s, "t-full", fmt.Sprintf("n-%d", i+1))
	}
	filed, _, err := file(ctx, s, "t-full", "n-6")
	if err != nil || filed != sandbox.NoteFull {
		t.Fatalf("the sixth waiting note was filed %s, %v, want full", filed, err)
	}
	if _, err := s.TakeNotes(ctx, "t-full", bridgeTake(launch, "n-1")); err != nil {
		t.Fatal(err)
	}
	mustFile(t, s, "t-full", "n-6")
	// A FILING WITH NO BOUNDS IS REFUSED, never one with none.
	if _, _, err := s.FileNote(ctx, "t-full", sandbox.NoteFiling{Note: sealedNote("n-7")}); err == nil {
		t.Error("a filing naming no bounds was taken")
	}
}

// THE ELEVENTH NOTE OF A TURN IS SPENT, read or not — and spent outranks full,
// because a turn that will never take the note again must not tell a person to
// wait for a read.
func testTheEleventhNoteOfATurnIsSpent(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-spent"))
	for i := range maxNotes {
		id := fmt.Sprintf("n-%02d", i+1)
		mustFile(t, s, "t-spent", id)
		if i < maxNotes-maxWaiting {
			if _, err := s.TakeNotes(ctx, "t-spent", bridgeTake(launch, id)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := len(waitingIDs(t, s, "t-spent")); got != maxWaiting {
		t.Fatalf("%d notes wait, want the bound so spent and full both hold", got)
	}
	filed, _, err := file(ctx, s, "t-spent", "n-11")
	if err != nil || filed != sandbox.NoteSpent {
		t.Fatalf("the eleventh note was filed %s, %v, want spent", filed, err)
	}
}

// A NOTE TO A RUN WHOSE ENDING IS DECIDED IS CLOSED, and so is one to a run with
// no record: nothing will read it. Neither is an error, and neither is filed —
// the ending publishes what was waiting when it was decided, and nothing lands
// after that.
func testANoteToARunWhoseEndingIsDecidedIsClosed(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t-closed"))
	decide(t, s, "t-closed", everyJob(sandbox.Fence{}, sandbox.Active))
	filed, _, err := file(ctx, s, "t-closed", "n-1")
	if err != nil || filed != sandbox.NoteClosed {
		t.Fatalf("a note to a run being ended was filed %s, %v, want closed", filed, err)
	}
	if notes := mustGet(t, s, "t-closed").Steer.Notes; len(notes) != 0 {
		t.Errorf("a refused note landed on the record: %+v", notes)
	}
	if filed, _, err = file(ctx, s, "t-never", "n-1"); err != nil || filed != sandbox.NoteClosed {
		t.Errorf("a note to a run with no record was filed %s, %v, want closed", filed, err)
	}
}

// A PARK KEEPS EVERY WAITING NOTE, and takes more: a question can wait days,
// and a note held for whatever resumes the turn is what "it stands for the rest
// of the turn" means. The filing answers the record as written, which says the
// run is parked — what the person is told about when it will be read.
func testAParkKeepsEveryWaitingNote(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t-park"))
	mustFile(t, s, "t-park", "n-1")
	park(t, s, "t-park")
	filed, written, err := file(ctx, s, "t-park", "n-2")
	if err != nil || filed != sandbox.NoteAccepted || written.Status != sandbox.StatusAwaiting {
		t.Fatalf("a note to a parked run was filed %s on a %q record, %v", filed, written.Status, err)
	}
	if got := waitingIDs(t, s, "t-park"); !slices.Equal(got, []string{"n-1", "n-2"}) {
		t.Errorf("the parked run holds waiting %v, want both notes", got)
	}
}

// A LAUNCH ANCHORS THE SEAT AND READS ITS BRIEF UNDER THE JOB IT OPENS, in the
// one write that opens it: a note the brief carries is never one the job's own
// harness could take again, and the seat a person's authority is decided on is on
// the record from its first byte.
func testALaunchAnchorsTheSeatAndReadsItsBriefUnderTheNewJob(t *testing.T, s sandbox.PendingStore) {
	r := run("t-brief")
	r.Steer = sandbox.RunSteer{Seat: []byte("sealed-seat"),
		// Nothing of the caller's but the anchor: a note or a pull it
		// hands in is not the store's to believe.
		Notes: []sandbox.RunNote{sealedNote("smuggled")}, Pulled: "job-x", Seq: 40}
	r.BriefNotes = []sandbox.RunNote{
		{ID: "box-1", Sealed: []byte("sealed:box-1"), At: base,
			Read: &sandbox.NoteRead{Phase: "execute", Iteration: 1, Round: 1, Boundary: "brief"}},
		{ID: "box-2", Sealed: []byte("sealed:box-2"), At: base.Add(time.Second),
			Read: &sandbox.NoteRead{Phase: "execute", Iteration: 1, Round: 1, Boundary: "brief"}},
	}
	opened, err := s.BeginLaunch(t.Context(), r, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	got := mustGet(t, s, "t-brief")
	for name, row := range map[string]sandbox.PendingRun{"the launch's answer": opened, "the record": got} {
		steer := row.Steer
		if string(steer.Seat) != "sealed-seat" || steer.Pulled != "" || steer.Seq != 2 || len(steer.Notes) != 2 {
			t.Fatalf("%s holds notes %+v, want the anchor and the brief's two", name, steer)
		}
		for i, n := range steer.Notes {
			if n.Seq != i+1 || n.Read == nil || n.Read.Launch != opened.LaunchID ||
				n.Read.Boundary != "brief" || n.Read.At.IsZero() || n.Read.Phase != "execute" {
				t.Errorf("%s: note %s is %+v (read %+v), want it read by job %s in its brief",
					name, n.ID, n, n.Read, opened.LaunchID)
			}
		}
	}
}

// A RESET KEEPS THE TURN'S NOTES — every one, read or waiting, their numbering
// and the anchor — and the next job's brief reads what it carries: a note the
// record held waiting is read under the NEW job, one the brief brings from
// elsewhere is added read. Whether a harness asked for notes is the job's, and goes.
func testAResetKeepsTheTurnsNotesAndClearsPulled(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	first := run("t-reset")
	first.Steer = sandbox.RunSteer{Seat: []byte("sealed-seat")}
	launch := mustLaunched(t, s, first)
	mustFile(t, s, "t-reset", "n-1")
	mustFile(t, s, "t-reset", "n-2")
	mustFile(t, s, "t-reset", "n-3")
	if _, err := s.TakeNotes(ctx, "t-reset", bridgeTake(launch, "n-1")); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MarkPulled(ctx, "t-reset", launch); err != nil || !ok {
		t.Fatalf("MarkPulled = %v, %v", ok, err)
	}
	mustClaim(t, s, "t-reset")

	second := run("t-reset")
	second.Steer = sandbox.RunSteer{Seat: []byte("another-seat")}
	brief := &sandbox.NoteRead{Phase: "execute", Iteration: 2, Round: 1, Boundary: "brief"}
	second.BriefNotes = []sandbox.RunNote{
		{ID: "n-2", Read: brief},
		{ID: "n-1", Read: brief},
		{ID: "late", Sealed: []byte("sealed:late"), At: base, Read: brief},
	}
	relaunched, err := s.BeginLaunch(ctx, second, sandbox.Fence{})
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	steer := mustGet(t, s, "t-reset").Steer
	if string(steer.Seat) != "sealed-seat" {
		t.Errorf("the reset replaced the anchor with %q", steer.Seat)
	}
	if steer.Pulled != "" {
		t.Errorf("the previous job's pull survived the reset: %q", steer.Pulled)
	}
	if steer.Seq != 4 || len(steer.Notes) != 4 {
		t.Fatalf("the record holds %+v, want the turn's three notes and the brief's new one", steer)
	}
	byID := map[string]sandbox.RunNote{}
	for _, n := range steer.Notes {
		byID[n.ID] = n
	}
	if read := byID["n-1"].Read; read == nil || read.Launch != launch || read.Boundary != "tool_batch" {
		t.Errorf("a note the previous job read was read again: %+v", read)
	}
	if read := byID["n-2"].Read; read == nil || read.Launch != relaunched.LaunchID ||
		read.Boundary != "brief" || read.Iteration != 2 {
		t.Errorf("a waiting note the brief carried is %+v, want read by job %s", read, relaunched.LaunchID)
	}
	if !byID["n-3"].Waiting() {
		t.Errorf("a note the brief did not carry was read: %+v", byID["n-3"].Read)
	}
	if late := byID["late"]; late.Seq != 4 || late.Read == nil || late.Read.Launch != relaunched.LaunchID ||
		!bytes.Equal(late.Sealed, []byte("sealed:late")) {
		t.Errorf("the brief's own note is %+v, want it added read under the new job", late)
	}
}

// A LAUNCH FILES THE BOX'S NOTES WAITING AFTER ITS BRIEF, in the write that
// creates the record: the brief's notes read by the job it opens, then every
// note the turn's box still held, waiting, numbered after them — ahead of
// anything a person files on the record once it exists, and counted against the
// turn's bounds before it is: a box that held as many notes as may wait leaves
// the next filing full, never one of the box's refused. A reset files them too,
// and leaves a note the record already holds as it is.
func testALaunchFilesTheBoxsNotesWaitingAfterItsBrief(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-handed")
	r.Steer = sandbox.RunSteer{Seat: []byte("sealed-seat")}
	r.BriefNotes = []sandbox.RunNote{{ID: "briefed", Sealed: []byte("sealed:briefed"), At: base,
		Read: &sandbox.NoteRead{Phase: "execute", Iteration: 1, Round: 1, Boundary: "brief"}}}
	for i := range maxWaiting {
		r.HandedNotes = append(r.HandedNotes, sealedNote(fmt.Sprintf("held-%d", i+1)))
	}
	opened, err := s.BeginLaunch(ctx, r, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	notes := mustGet(t, s, "t-handed").Steer.Notes
	if len(notes) != 1+maxWaiting {
		t.Fatalf("the record holds %d notes, want the brief's and the box's %d", len(notes), maxWaiting)
	}
	for i, n := range notes {
		switch {
		case n.Seq != i+1:
			t.Errorf("note %s is numbered %d, want %d", n.ID, n.Seq, i+1)
		case i == 0 && (n.ID != "briefed" || n.Read == nil || n.Read.Launch != opened.LaunchID):
			t.Errorf("the first note is %+v, want the brief's, read by job %s", n, opened.LaunchID)
		case i > 0 && (n.ID != fmt.Sprintf("held-%d", i) || !n.Waiting() ||
			!bytes.Equal(n.Sealed, []byte("sealed:"+n.ID))):
			t.Errorf("note %d is %+v, want held-%d waiting with its seal", i+1, n, i)
		}
	}
	if filed, _, err := file(ctx, s, "t-handed", "direct"); err != nil || filed != sandbox.NoteFull {
		t.Errorf("a note filed behind a full hand-over = %s, %v, want full", filed, err)
	}

	// A RESET, which a first segment's launch can meet: a note the record
	// holds is left as it is, and a new one is filed waiting after the rest.
	if ok, err := s.MarkSuspended(ctx, "t-handed", suspension()); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}
	mustClaim(t, s, "t-handed")
	again := run("t-handed")
	again.HandedNotes = []sandbox.RunNote{sealedNote("held-1"), sealedNote("held-new")}
	if _, err := s.BeginLaunch(ctx, again, sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	steer := mustGet(t, s, "t-handed").Steer
	last := steer.Notes[len(steer.Notes)-1]
	if len(steer.Notes) != 2+maxWaiting || last.ID != "held-new" || last.Seq != steer.Seq || !last.Waiting() {
		t.Errorf("the reset left notes %+v, want the record's and held-new waiting last", steer.Notes)
	}
}

// EVERY WRITE THAT RECORDS A READ TAKES THE NEXT PLACE, stamped on each read it
// records: the order a turn read its notes in is the record's own count, never
// the clock of whichever node made the write. A write that records nothing
// takes no place.
func testEveryWriteThatRecordsAReadTakesTheNextPlace(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-order")
	r.BriefNotes = []sandbox.RunNote{
		{ID: "b-1", Sealed: []byte("sealed:b-1"), Read: &sandbox.NoteRead{Phase: "execute", Boundary: "brief"}},
		{ID: "b-2", Sealed: []byte("sealed:b-2"), Read: &sandbox.NoteRead{Phase: "execute", Boundary: "brief"}},
	}
	launch := mustLaunched(t, s, r)
	mustFile(t, s, "t-order", "n-1")
	mustFile(t, s, "t-order", "n-2")
	if _, err := s.TakeNotes(ctx, "t-order", bridgeTake(launch, "n-2")); err != nil {
		t.Fatal(err)
	}
	if taken, err := s.TakeNotes(ctx, "t-order", bridgeTake(launch, "n-2")); err != nil || len(taken) != 0 {
		t.Fatalf("a take of a note already read = %v, %v", taken, err)
	}
	taken, err := s.TakeNotes(ctx, "t-order", bridgeTake(launch, "n-1"))
	if err != nil || len(taken) != 1 || taken[0].Read.Order != 3 {
		t.Fatalf("the third read's take answered %+v, %v, want it read third", taken, err)
	}
	steer := mustGet(t, s, "t-order").Steer
	want := map[string]int{"b-1": 1, "b-2": 1, "n-2": 2, "n-1": 3}
	for _, n := range steer.Notes {
		if n.Read == nil || n.Read.Order != want[n.ID] {
			t.Errorf("note %s was read %+v, want in the record's read %d", n.ID, n.Read, want[n.ID])
		}
	}
	if steer.Reads != 3 {
		t.Errorf("the record counts %d reads, want 3", steer.Reads)
	}
}

// A PULL IS THE JOB'S: recorded once, for the job the record holds, and never
// for a job it has moved on from.
func testAPullIsRecordedOnceAndOnlyForItsJob(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-pull"))
	if ok, err := s.MarkPulled(ctx, "t-pull", "another-job"); err != nil || ok {
		t.Errorf("a pull for a job the record does not hold = %v, %v", ok, err)
	}
	if ok, err := s.MarkPulled(ctx, "t-pull", launch); err != nil || !ok {
		t.Fatalf("MarkPulled = %v, %v", ok, err)
	}
	if ok, err := s.MarkPulled(ctx, "t-pull", launch); err != nil || ok {
		t.Errorf("a second pull wrote again: %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t-pull").Steer.Pulled; got != launch {
		t.Errorf("pulled = %q, want %q", got, launch)
	}
}

// A STARTED JOB'S BRIEF STAYS READ THROUGH EVERY WRITE THAT FORGETS ITS BOX.
// What tells the notes a job's brief carried from ones read into the brief of a
// launch that never started its job is the job's own record: the attach naming
// the job's command records it started ([sandbox.LaunchRecord.Started]), and
// nothing that forgets the box — the teardown of a park, an expired pause —
// unsays it, though both clear the row's command. Only the next launch, whose
// job is new, starts unstarted.
//
// Mutation: read the witness off the row's command, clear it with the box, or
// let an attach naming no command unsay it, and the parked runs here report
// their brief's note unread.
func testAStartedJobsBriefStaysReadThroughEveryWriteThatForgetsItsBox(t *testing.T, s sandbox.PendingStore) {
	briefed := func(turnID string) sandbox.PendingRun {
		r := run(turnID)
		r.BriefNotes = []sandbox.RunNote{{ID: "briefed", Sealed: []byte("sealed:briefed"), At: base,
			Read: &sandbox.NoteRead{Phase: "execute", Iteration: 1, Round: 1, Boundary: "brief"}}}
		return r
	}
	unread := func(turnID string) []string {
		var out []string
		for _, n := range mustGet(t, s, turnID).UnreadNotes() {
			out = append(out, n.ID)
		}
		return out
	}
	// started opens a job with a brief note, attaches its box and then its
	// command, as a launch does, and parks it on a question.
	started := func(turnID string) {
		ctx := t.Context()
		mustBeginLaunch(t, s, briefed(turnID))
		if err := s.AttachSandbox(ctx, turnID, sandbox.BoxRef{SandboxID: "box-" + turnID,
			Harness: "claude-code", PauseTTLSec: 1800}, sandbox.Fence{}); err != nil {
			t.Fatalf("AttachSandbox: %v", err)
		}
		if got := unread(turnID); !slices.Equal(got, []string{"briefed"}) || mustGet(t, s, turnID).Launch.Started {
			t.Fatalf("a job whose command is not attached has %v unread, want its brief's note", got)
		}
		if err := s.AttachSandbox(ctx, turnID, sandbox.BoxRef{SandboxID: "box-" + turnID, CommandID: "cmd-1",
			Harness: "claude-code", PauseTTLSec: 1800}, sandbox.Fence{}); err != nil {
			t.Fatalf("AttachSandbox: %v", err)
		}
		// AN ATTACH NAMING NO COMMAND UNSAYS NOTHING: the job's record
		// keeps what the attach before it recorded.
		if err := s.AttachSandbox(ctx, turnID, sandbox.BoxRef{SandboxID: "box-" + turnID,
			Harness: "claude-code", PauseTTLSec: 1800}, sandbox.Fence{}); err != nil {
			t.Fatalf("AttachSandbox: %v", err)
		}
		if ok, err := s.MarkSuspended(ctx, turnID, suspension()); err != nil || !ok {
			t.Fatalf("MarkSuspended = %v, %v", ok, err)
		}
		if err := s.MarkAwaiting(ctx, turnID, sandbox.Clarification{AskedAt: base}); err != nil {
			t.Fatalf("MarkAwaiting: %v", err)
		}
		if run := mustGet(t, s, turnID); !run.Launch.Started || len(unread(turnID)) != 0 {
			t.Fatalf("a parked job that started reads started=%v with %v unread", run.Launch.Started,
				unread(turnID))
		}
	}

	ctx := t.Context()
	started("t-expired")
	if won, err := s.ExpirePause(ctx, "t-expired"); err != nil || !won {
		t.Fatalf("ExpirePause = %v, %v", won, err)
	}
	if run := mustGet(t, s, "t-expired"); run.CommandID != "" || !run.Launch.Started {
		t.Errorf("an expired pause left command %q and started=%v, want the box forgotten and the job "+
			"still started", run.CommandID, run.Launch.Started)
	}
	if got := unread("t-expired"); len(got) != 0 {
		t.Errorf("after its pause expired the run has %v unread, want nothing: its job ran with its brief", got)
	}

	started("t-released")
	if err := s.ReleaseBox(ctx, "t-released"); err != nil {
		t.Fatalf("ReleaseBox: %v", err)
	}
	if err := s.SetStatus(ctx, "t-released", sandbox.StatusReseed, sandbox.Fence{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if run := mustGet(t, s, "t-released"); run.CommandID != "" || !run.Launch.Started {
		t.Errorf("a released box left command %q and started=%v, want the box forgotten and the job "+
			"still started", run.CommandID, run.Launch.Started)
	}
	if got := unread("t-released"); len(got) != 0 {
		t.Errorf("after its box was released the run has %v unread, want nothing", got)
	}

	// THE NEXT JOB IS NEW: a relaunch's record starts unstarted.
	if _, err := s.BeginLaunch(ctx, run("t-released"), sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if mustGet(t, s, "t-released").Launch.Started {
		t.Error("a relaunch kept the previous job's start")
	}
}

// NOTES RIDE A BRIDGED-CALL STORM: a run's record takes every call its box
// makes — from up to twenty subagents at once — and a person's notes, the
// bridge's takes and its pulls join them. Every write lands, none is lost to
// another, and none answers that the record kept changing.
func testNotesRideABridgedCallStorm(t *testing.T, s sandbox.PendingStore) {
	const writers, each, notes = 20, 3, maxWaiting
	ctx := t.Context()
	r := run("t-note-storm")
	launch := mustLaunched(t, s, r)
	var wg sync.WaitGroup
	errs := make(chan error, writers*each+4*notes+1)
	for w := range writers {
		wg.Go(func() {
			for n := range each {
				recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
					Launch: launch, Call: sandbox.BridgeCall{Name: fmt.Sprintf("agent-%02d-call-%d", w, n)}})
				switch {
				case err != nil:
					errs <- err
				case !recorded:
					errs <- fmt.Errorf("an append was not recorded")
				}
			}
		})
	}
	for i := range notes {
		id := fmt.Sprintf("n-%d", i+1)
		wg.Go(func() {
			if filed, _, err := file(ctx, s, r.TurnID, id); err != nil || filed != sandbox.NoteAccepted {
				errs <- fmt.Errorf("file %s = %s, %w", id, filed, err)
			}
		})
	}
	wg.Go(func() {
		if _, err := s.MarkPulled(ctx, r.TurnID, launch); err != nil {
			errs <- err
		}
	})
	wg.Wait()
	// THE TAKES RACE THE NEXT APPENDS, each for every note.
	var takers sync.WaitGroup
	taken := make(chan string, notes*2)
	for i := range notes {
		id := fmt.Sprintf("n-%d", i+1)
		for range 2 {
			takers.Go(func() {
				got, err := s.TakeNotes(ctx, r.TurnID, bridgeTake(launch, id))
				if err != nil {
					errs <- err
				}
				for _, n := range got {
					taken <- n.ID
				}
			})
		}
		takers.Go(func() {
			if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
				Launch: launch, Call: sandbox.BridgeCall{Name: "late-" + id}}); err != nil {
				errs <- err
			}
		})
	}
	takers.Wait()
	close(errs)
	close(taken)
	for err := range errs {
		t.Errorf("a write in the storm failed: %v", err)
	}
	var ids []string
	for id := range taken {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if want := []string{"n-1", "n-2", "n-3", "n-4", "n-5"}; !slices.Equal(ids, want) {
		t.Errorf("the takes handed over %v, want every note exactly once", ids)
	}
	got := mustLogged(t, s, r.TurnID)
	if len(got.Log.Calls) != writers*each+notes {
		t.Errorf("the record holds %d calls, want %d", len(got.Log.Calls), writers*each+notes)
	}
	if len(got.Steer.Notes) != notes || got.Steer.Seq != notes || got.Steer.Pulled != launch {
		t.Errorf("the record holds notes %+v, want %d numbered 1 to %d, pulled by %s",
			got.Steer, notes, notes, launch)
	}
	for _, n := range got.Steer.Notes {
		if n.Waiting() {
			t.Errorf("note %s still waits after a take of it", n.ID)
		}
	}
}

// A WATCH SEES EVERY NOTE CHANGE: the run's record as it begins, a note filed,
// the note read, the ending decided and the record gone — the newest state each
// time, which is what a reader following the notes holds.
func testAWatchSeesEveryNoteChange(t *testing.T, s sandbox.PendingStore) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	views, err := s.WatchNotes(ctx, "t-watch")
	if err != nil {
		t.Fatalf("WatchNotes: %v", err)
	}
	until := func(what string, ok func(sandbox.NotesView) bool) sandbox.NotesView {
		t.Helper()
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case view, open := <-views:
				if !open {
					t.Fatalf("the watch closed before %s", what)
				}
				if ok(view) {
					return view
				}
			case <-timer.C:
				t.Fatalf("the watch never saw %s", what)
			}
		}
	}
	until("the run with no record", func(v sandbox.NotesView) bool { return !v.Found })
	launch := mustLaunched(t, s, run("t-watch"))
	mustFile(t, s, "t-watch", "n-1")
	filed := until("the note filed", func(v sandbox.NotesView) bool {
		return v.Found && len(v.Steer.Notes) == 1 && v.Steer.Notes[0].Waiting()
	})
	if filed.LaunchID != launch || filed.Status != sandbox.StatusRunning || filed.EndingDecided {
		t.Errorf("the view of the filed note is %+v", filed)
	}
	if _, err := s.TakeNotes(ctx, "t-watch", bridgeTake(launch, "n-1")); err != nil {
		t.Fatal(err)
	}
	until("the note read", func(v sandbox.NotesView) bool {
		return v.Found && len(v.Steer.Notes) == 1 && !v.Steer.Notes[0].Waiting()
	})
	decided := decide(t, s, "t-watch", everyJob(sandbox.Fence{}, sandbox.Active))
	until("the ending decided", func(v sandbox.NotesView) bool { return v.Found && v.EndingDecided })
	if _, ok, err := s.Finish(ctx, "t-watch", decided.Ending.ID); err != nil || !ok {
		t.Fatalf("Finish = %v, %v", ok, err)
	}
	until("the record gone", func(v sandbox.NotesView) bool { return !v.Found })
}

// NOTES SURVIVE EVERY WRITE THAT IS NOT THEIRS, on the raw record: a status
// flip, a lease, a bridged call, a park, a claim and its release each rewrite
// the run's record whole, and each must hand the notes back — the seals as the
// bytes they were, the reads as recorded — or the first write after a note
// loses it. A key this build does not know rides beside them, as every row's
// does ([sandbox.PendingRun.Extra]).
func testNotesSurviveEveryWriteThatIsNotTheirs(t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns) {
	ctx := t.Context()
	r := run("t-raw-notes")
	r.Steer = sandbox.RunSteer{Seat: []byte("sealed-seat")}
	launch := mustLaunched(t, s, r)
	mustFile(t, s, "t-raw-notes", "n-1")
	mustFile(t, s, "t-raw-notes", "n-2")
	if _, err := s.TakeNotes(ctx, "t-raw-notes", bridgeTake(launch, "n-1")); err != nil {
		t.Fatal(err)
	}
	record, found, err := runs.SandboxRun(ctx, "t-raw-notes")
	if err != nil || !found {
		t.Fatalf("read the raw row: %v %v", found, err)
	}
	fields := map[string]json.RawMessage{}
	if decodeErr := json.Unmarshal(record.Value, &fields); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	want := string(fields["steer"])
	if want == "" {
		t.Fatal("the record carries no notes section to follow")
	}
	fields["steer_quorum"] = json.RawMessage(`{"of":2}`)
	seeded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := runs.MutateSandboxRun(ctx, "t-raw-notes", func(coord.Record) (coord.SandboxRunChange, error) {
		return coord.SandboxRunChange{Put: seeded}, nil
	}); err != nil || !ok {
		t.Fatalf("seed the unknown key: %v %v", ok, err)
	}
	check := func(step string) {
		t.Helper()
		record, found, err := runs.SandboxRun(ctx, "t-raw-notes")
		if err != nil || !found {
			t.Fatalf("%s: read the raw row: %v %v", step, found, err)
		}
		got := map[string]json.RawMessage{}
		if err := json.Unmarshal(record.Value, &got); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if string(got["steer"]) != want {
			t.Errorf("%s rewrote the notes:\n got %s\nwant %s", step, got["steer"], want)
		}
		if string(got["steer_quorum"]) != `{"of":2}` {
			t.Errorf("%s dropped a key this build does not know: %s", step, got["steer_quorum"])
		}
	}
	if err := s.SetStatus(ctx, "t-raw-notes", sandbox.StatusRunning, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	check("a status write")
	if _, err := s.ClaimOwnership(ctx, "t-raw-notes", "node-b", 2); err != nil {
		t.Fatal(err)
	}
	check("a lease")
	if _, err := s.AppendBridgeCall(ctx, "t-raw-notes", sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "read_page"}}); err != nil {
		t.Fatal(err)
	}
	check("a bridged call")
	claimed := mustClaim(t, s, "t-raw-notes")
	check("the claim")
	mustRelease(t, s, claimed)
	check("its release")
	park(t, s, "t-raw-notes")
	check("a park")
}

// A KEY A LATER BUILD ADDS INSIDE A SECTION SURVIVES EVERY WRITE THIS BUILD
// MAKES. The record is rewritten whole by every node that touches it, so a
// field a newer build adds to the notes section, to one note, to where a note
// was read, to a bridged call, to the identity of a call the log dropped or to
// the job's own record is gone the moment this build writes the record back —
// unless every level that can grow keeps what it does not know. And the log
// beside the record ([sandbox.BridgeLog]) is rewritten whole by every append,
// so the same holds at its top, in each of its calls and in each identity it
// keeps. Seeded at each of those seven levels at once, as a newer build would
// write them, and read back off the raw record and the raw log after
// every kind of write this build makes: a status write, a lease, a bridged
// call, a filing, a take, a pull, a claim and its release, a park, and a
// relaunch's reset that carries the round on — which replaces the job's record
// whole, so the key a later build added to it goes with the job it was about.
//
// Mutation: drop any one section's Extra from its decode or its encode, and the
// key at that level is gone after the first write; carry the job's record's
// over a relaunch, and the next job reads the last one's fact as its own.
func testAKeyALaterBuildAddsInsideASectionSurvivesEveryWrite(t *testing.T, s sandbox.PendingStore,
	runs coord.SandboxRuns,
) {
	ctx := t.Context()
	const turn = "t-nested-keys"
	r := run(turn)
	r.Steer = sandbox.RunSteer{Seat: []byte("sealed-seat")}
	launch := mustLaunched(t, s, r)
	mustFile(t, s, turn, "n-1")
	mustFile(t, s, turn, "n-2")
	if _, err := s.TakeNotes(ctx, turn, bridgeTake(launch, "n-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "read_page"}}); err != nil {
		t.Fatal(err)
	}

	// THE NEWER BUILD'S WRITE: one key of its own at every level.
	seeded := map[string]string{
		"the notes section": `{"quorum":2}`,
		"a note":            `"high"`,
		"a note's read":     `"hook"`,
		"a bridged call":    `{"span":"s-1"}`,
		"a dropped call":    `{"span":"s-3"}`,
		"the log":           `{"kept":"all"}`,
		"the job's record":  `{"span":"s-2"}`,
	}
	if _, ok, err := runs.MutateSandboxRun(ctx, turn, func(current coord.Record) (coord.SandboxRunChange, error) {
		var row map[string]any
		if err := json.Unmarshal(current.Value, &row); err != nil {
			return coord.SandboxRunChange{}, err
		}
		section := row["steer"].(map[string]any)
		section["quorum_rule"] = json.RawMessage(seeded["the notes section"])
		first := section["notes"].([]any)[0].(map[string]any)
		first["priority"] = json.RawMessage(seeded["a note"])
		first["read"].(map[string]any)["carrier"] = json.RawMessage(seeded["a note's read"])
		row["launch_record"].(map[string]any)["trace"] = json.RawMessage(seeded["the job's record"])
		raw, err := json.Marshal(row)
		return coord.SandboxRunChange{Put: raw}, err
	}); err != nil || !ok {
		t.Fatalf("seed the newer build's keys: %v %v", ok, err)
	}
	if _, ok, err := runs.MutateSandboxRunPart(ctx, turn, coord.RunLog, func(current coord.Record, found bool) ([]byte, error) {
		var log map[string]any
		if err := json.Unmarshal(current.Value, &log); err != nil {
			return nil, err
		}
		log["retention"] = json.RawMessage(seeded["the log"])
		log["calls"].([]any)[0].(map[string]any)["trace"] = json.RawMessage(seeded["a bridged call"])
		// A DROPPED CALL'S IDENTITY as a later build writes one: the log's
		// count says one call was dropped, and its tally names it.
		log["elided"] = 1
		log["elided_calls"] = []any{map[string]any{"name": "read_page", "digest": "d1", "calls": 1,
			"trace": json.RawMessage(seeded["a dropped call"])}}
		return json.Marshal(log)
	}); err != nil || !ok {
		t.Fatalf("seed the newer build's keys in the log: %v %v", ok, err)
	}

	// sameJob says the step leaves the job the seed was written under on the
	// record, whose own record then keeps the later build's key.
	check := func(step string, sameJob bool) {
		t.Helper()
		record, found, err := runs.SandboxRun(ctx, turn)
		if err != nil || !found {
			t.Fatalf("%s: read the raw row: %v %v", step, found, err)
		}
		var row struct {
			Steer struct {
				Quorum json.RawMessage `json:"quorum_rule"`
				Notes  []struct {
					ID       string          `json:"id"`
					Priority json.RawMessage `json:"priority"`
					Read     *struct {
						Carrier json.RawMessage `json:"carrier"`
					} `json:"read"`
				} `json:"notes"`
			} `json:"steer"`
			Job struct {
				Trace json.RawMessage `json:"trace"`
			} `json:"launch_record"`
		}
		if derr := json.Unmarshal(record.Value, &row); derr != nil {
			t.Fatalf("%s: %v", step, derr)
		}
		logged, found, err := runs.SandboxRunPart(ctx, turn, coord.RunLog)
		if err != nil || !found {
			t.Fatalf("%s: read the raw log: %v %v", step, found, err)
		}
		var log struct {
			Retention json.RawMessage `json:"retention"`
			Calls     []struct {
				Name  string          `json:"name"`
				Trace json.RawMessage `json:"trace"`
			} `json:"calls"`
			Dropped []struct {
				Name  string          `json:"name"`
				Trace json.RawMessage `json:"trace"`
			} `json:"elided_calls"`
		}
		if err := json.Unmarshal(logged.Value, &log); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		got := map[string]string{"the notes section": string(row.Steer.Quorum)}
		for _, n := range row.Steer.Notes {
			if n.ID != "n-1" {
				continue
			}
			got["a note"] = string(n.Priority)
			if n.Read != nil {
				got["a note's read"] = string(n.Read.Carrier)
			}
		}
		got["the log"] = string(log.Retention)
		if len(log.Calls) > 0 && log.Calls[0].Name == "read_page" {
			got["a bridged call"] = string(log.Calls[0].Trace)
		}
		if len(log.Dropped) > 0 && log.Dropped[0].Name == "read_page" {
			got["a dropped call"] = string(log.Dropped[0].Trace)
		}
		got["the job's record"] = string(row.Job.Trace)
		for level, want := range seeded {
			if level == "the job's record" && !sameJob {
				if got[level] != "" {
					t.Errorf("%s carried the key a later build added to the job's record over to the "+
						"next job: %s", step, got[level])
				}
				continue
			}
			if compact(t, got[level]) != compact(t, want) {
				t.Errorf("%s dropped the key a later build added to %s: got %q, want %s",
					step, level, got[level], want)
			}
		}
	}

	check("the seed", true)
	if err := s.SetStatus(ctx, turn, sandbox.StatusRunning, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	check("a status write", true)
	if _, err := s.ClaimOwnership(ctx, turn, "node-b", 2); err != nil {
		t.Fatal(err)
	}
	check("a lease", true)
	if _, err := s.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "write_page"}}); err != nil {
		t.Fatal(err)
	}
	check("a bridged call", true)
	mustFile(t, s, turn, "n-3")
	check("a filing", true)
	if _, err := s.TakeNotes(ctx, turn, bridgeTake(launch, "n-2")); err != nil {
		t.Fatal(err)
	}
	check("a take", true)
	if _, err := s.MarkPulled(ctx, turn, launch); err != nil {
		t.Fatal(err)
	}
	check("a pull", true)
	claimed := mustClaim(t, s, turn)
	check("the claim", true)
	mustRelease(t, s, claimed)
	check("its release", true)
	park(t, s, turn)
	check("a park", true)
	again := run(turn)
	again.Continues = true
	again.BriefNotes = []sandbox.RunNote{{ID: "n-3",
		Read: &sandbox.NoteRead{Phase: "execute", Iteration: 1, Round: 1, Boundary: "brief"}}}
	if _, err := s.BeginLaunch(ctx, again, sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	check("a relaunch's reset", false)
}

// compact is JSON text without its insignificant space, so a value is compared
// as a value and not as the bytes one encoder happened to write.
func compact(t *testing.T, text string) string {
	t.Helper()
	if text == "" {
		return ""
	}
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(text)); err != nil {
		t.Fatalf("compact %q: %v", text, err)
	}
	return b.String()
}

// A STATE THE NOTE VIEW CANNOT READ IS PASSED OVER, never delivered as an empty
// record: a reader following a run's notes — the bridge serving its harness, a
// resumed segment's source — that were handed "no record" would answer every
// step with nothing until the next write it could read. It keeps what it last
// knew, and the next state it can read replaces it.
func testAStateTheNoteViewCannotReadIsPassedOver(t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const turn = "t-unread-view"
	mustLaunched(t, s, run(turn))
	mustFile(t, s, turn, "n-1")
	views, err := s.WatchNotes(ctx, turn)
	if err != nil {
		t.Fatalf("WatchNotes: %v", err)
	}
	next := func(within time.Duration) (sandbox.NotesView, bool) {
		t.Helper()
		timer := time.NewTimer(within)
		defer timer.Stop()
		select {
		case view, open := <-views:
			if !open {
				t.Fatal("the watch closed")
			}
			return view, true
		case <-timer.C:
			return sandbox.NotesView{}, false
		}
	}
	for {
		view, ok := next(30 * time.Second)
		if !ok {
			t.Fatal("the watch never saw the filed note")
		}
		if view.Found && len(view.Steer.Notes) == 1 {
			break
		}
	}
	readable, found, err := runs.SandboxRun(ctx, turn)
	if err != nil || !found {
		t.Fatalf("read the raw row: %v %v", found, err)
	}
	put := func(value func(row map[string]any)) {
		t.Helper()
		if _, ok, err := runs.MutateSandboxRun(ctx, turn, func(current coord.Record) (coord.SandboxRunChange, error) {
			var row map[string]any
			if err := json.Unmarshal(readable.Value, &row); err != nil {
				return coord.SandboxRunChange{}, err
			}
			value(row)
			raw, err := json.Marshal(row)
			return coord.SandboxRunChange{Put: raw}, err
		}); err != nil || !ok {
			t.Fatalf("write the raw row: %v %v", ok, err)
		}
	}
	// A STATE NO VIEW CAN READ: its notes are not a notes section at all.
	put(func(row map[string]any) { row["steer"] = 5 })
	if view, ok := next(500 * time.Millisecond); ok {
		t.Fatalf("a state the view cannot read was delivered as %+v", view)
	}
	// THE NEXT READABLE STATE replaces what the view knew.
	put(func(map[string]any) {})
	mustFile(t, s, turn, "n-2")
	for {
		view, ok := next(30 * time.Second)
		switch {
		case !ok:
			t.Fatal("the watch never saw the state after the unreadable one")
		case !view.Found || len(view.Steer.Notes) == 0:
			t.Fatalf("the watch delivered %+v after a state it could not read", view)
		case len(view.Steer.Notes) == 2:
			return
		}
	}
}

// A TAKE WHOSE ANSWER WAS LOST IS SETTLED BY ITS IDENTITY. A take's write can
// land and its answer not reach the harness that asked — a hook given five
// seconds, a store that answered after them — and the record then says the note
// was read by a job whose harness never had it. The settle answers exactly the
// notes that take's write recorded, found by the identity its read named, and
// where its reader never had them marks each unanswered, keeping where it was
// read: never another take's read, never a note still waiting. Asked again, it
// answers the same notes and marks nothing twice. A take that recorded nothing
// is answered nothing, a run with no record is answered nothing, a settle that
// names no take is refused, and a run whose ending is decided is settled all the
// same: what a take recorded is a fact about the take.
//
// Mutation: mark every read of the take's job rather than the take's own, mark
// the reads of a settle that does not ask it, clear the read rather than mark
// it, refuse a run whose ending is decided, or answer a settle naming no take,
// and a check goes red.
func testATakeWhoseAnswerWasLostIsSettledByItsIdentity(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-settle"))
	for _, id := range []string{"n-1", "n-2", "n-3", "n-4"} {
		mustFile(t, s, "t-settle", id)
	}
	if _, err := s.TakeNotes(ctx, "t-settle", namedTake(launch, "answered", "n-1")); err != nil {
		t.Fatal(err)
	}
	// THE TAKE WHOSE ANSWER WAS LOST, its write landed: two notes.
	lost, takeErr := s.TakeNotes(ctx, "t-settle", namedTake(launch, "lost", "n-2", "n-3"))
	if takeErr != nil || len(lost) != 2 {
		t.Fatalf("the lost take took %d (%v), want two", len(lost), takeErr)
	}
	// AS THEY STAND, for a reader that hands them over itself: nothing marked.
	stand, standErr := s.SettleTake(ctx, "t-settle", sandbox.NoteSettle{Take: "lost"})
	if standErr != nil || len(stand) != 2 || stand[0].Read.Unanswered || stand[1].Read.Unanswered {
		t.Fatalf("a settle answering the take's reads as they stand = %+v, %v, want both, unmarked", stand,
			standErr)
	}
	for range 2 {
		settled, settleErr := s.SettleTake(ctx, "t-settle", sandbox.NoteSettle{Take: "lost", Unanswered: true})
		if settleErr != nil || len(settled) != 2 {
			t.Fatalf("the settle answered %d (%v), want the two notes the take recorded", len(settled), settleErr)
		}
		for _, n := range settled {
			if n.Read == nil || !n.Read.Unanswered || n.Read.Take != "lost" {
				t.Errorf("the settle answered %s read %+v, want it marked unanswered", n.ID, n.Read)
			}
		}
	}
	byID := map[string]sandbox.RunNote{}
	for _, n := range mustGet(t, s, "t-settle").Steer.Notes {
		byID[n.ID] = n
	}
	for _, id := range []string{"n-2", "n-3"} {
		read := byID[id].Read
		if read == nil || !read.Unanswered || read.Order != lost[0].Read.Order || read.Launch != launch ||
			read.Boundary != "tool_batch" {
			t.Errorf("note %s is read %+v on the record, want the lost take's read, kept and marked unanswered",
				id, read)
		}
	}
	if read := byID["n-1"].Read; read == nil || read.Unanswered {
		t.Errorf("the answered take's read is %+v, want it read and unmarked", read)
	}
	if !byID["n-4"].Waiting() {
		t.Errorf("a note no take recorded is read %+v, want it waiting", byID["n-4"].Read)
	}
	if none, err := s.SettleTake(ctx, "t-settle", sandbox.NoteSettle{Take: "never-landed",
		Unanswered: true}); err != nil || len(none) != 0 {
		t.Errorf("a settle of a take that recorded nothing = %+v, %v, want nothing", none, err)
	}
	if none, err := s.SettleTake(ctx, "t-settle-gone", sandbox.NoteSettle{Take: "lost",
		Unanswered: true}); err != nil || len(none) != 0 {
		t.Errorf("a settle of a run with no record = %+v, %v, want nothing", none, err)
	}
	if _, err := s.SettleTake(ctx, "t-settle", sandbox.NoteSettle{Unanswered: true}); err == nil {
		t.Error("a settle that names no take was answered")
	}

	// A RUN WHOSE ENDING IS DECIDED is settled all the same.
	ending := mustLaunched(t, s, run("t-settle-ending"))
	mustFile(t, s, "t-settle-ending", "n-1")
	if _, err := s.TakeNotes(ctx, "t-settle-ending", namedTake(ending, "lost", "n-1")); err != nil {
		t.Fatal(err)
	}
	decide(t, s, "t-settle-ending", everyJob(sandbox.Fence{}, sandbox.Active))
	settled, endingErr := s.SettleTake(ctx, "t-settle-ending", sandbox.NoteSettle{Take: "lost", Unanswered: true})
	if endingErr != nil || len(settled) != 1 || !settled[0].Read.Unanswered {
		t.Errorf("a settle of a run whose ending is decided = %+v, %v, want its note marked", settled, endingErr)
	}
}

// A SETTLE OF A TAKE THAT RECORDED NOTHING FENCES ITS WRITE, IN EITHER MODE. A
// take whose caller gave up on its answer can still have its write in flight —
// sent, and taken by the store only after the settle read the record — and that
// write records the note read by a take nobody answered. So a settle that finds
// no read of the take writes the record all the same, whether it marks what it
// finds or answers it as it stands, and the take's write, conditioned on the
// version it read, cannot land after it: decided again on the record the settle
// wrote, the take is a lost race, and the note still waits. A settle that finds
// the take's reads — marked already, where it marks — writes nothing, nothing
// of the take being left in flight.
//
// Mutation: write nothing where the take recorded nothing, in either mode, and
// the take's write lands after the settle with its note read; or write where
// the reads are there and need nothing, and the record moves for nothing.
func testASettleOfATakeThatRecordedNothingFencesItsWrite(t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns) {
	ctx := t.Context()
	for name, unanswered := range map[string]bool{"marking": true, "answering": false} {
		turnID := "t-fence-" + name
		launch := mustLaunched(t, s, run(turnID))
		mustFile(t, s, turnID, "n-1")
		// THE TAKE'S WRITE, as the store makes it: its decision on the record
		// it read, conditioned on that version — and between its read and its
		// write, THE SETTLE, made by a caller that gave up on the take's
		// answer.
		attempts := 0
		_, wrote, raceErr := runs.MutateSandboxRun(ctx, turnID, func(read coord.Record) (coord.SandboxRunChange, error) {
			attempts++
			if attempts > 1 {
				// DECIDED AGAIN on the record the settle wrote: the take's
				// caller is gone, and its write is no more.
				return coord.SandboxRunChange{}, nil
			}
			settled, settleErr := s.SettleTake(ctx, turnID, sandbox.NoteSettle{Take: "in-flight",
				Unanswered: unanswered})
			if settleErr != nil || len(settled) != 0 {
				return coord.SandboxRunChange{}, fmt.Errorf("the settle beside the take's write = %+v, %v, "+
					"want nothing recorded", settled, settleErr)
			}
			var row sandbox.PendingRun
			if decodeErr := json.Unmarshal(read.Value, &row); decodeErr != nil {
				return coord.SandboxRunChange{}, decodeErr
			}
			row.Steer.Reads++
			row.Steer.Notes[0].Read = &sandbox.NoteRead{At: base, Order: row.Steer.Reads, Phase: "execute",
				Boundary: "tool_batch", Launch: launch, Take: "in-flight"}
			raw, encodeErr := json.Marshal(row)
			return coord.SandboxRunChange{Put: raw}, encodeErr
		})
		if raceErr != nil {
			t.Fatal(raceErr)
		}
		if wrote || attempts != 2 {
			t.Errorf("%s, the take's write landed (%v) in %d attempt(s), want it lost to the settle's write",
				name, wrote, attempts)
		}
		if got := waitingIDs(t, s, turnID); !slices.Equal(got, []string{"n-1"}) {
			t.Errorf("%s, the notes waiting after the settle are %v, want n-1: the take's write landed", name, got)
		}

		// THE TAKE'S READS THERE, marked already where the settle marks:
		// nothing is left in flight, and nothing is written.
		if _, err := s.TakeNotes(ctx, turnID, namedTake(launch, "lost", "n-1")); err != nil {
			t.Fatal(err)
		}
		if _, settleErr := s.SettleTake(ctx, turnID, sandbox.NoteSettle{Take: "lost",
			Unanswered: unanswered}); settleErr != nil {
			t.Fatal(settleErr)
		}
		before, _, beforeErr := runs.SandboxRun(ctx, turnID)
		if beforeErr != nil {
			t.Fatal(beforeErr)
		}
		if _, settleErr := s.SettleTake(ctx, turnID, sandbox.NoteSettle{Take: "lost",
			Unanswered: unanswered}); settleErr != nil {
			t.Fatal(settleErr)
		}
		after, _, afterErr := runs.SandboxRun(ctx, turnID)
		if afterErr != nil {
			t.Fatal(afterErr)
		}
		if after.Version != before.Version {
			t.Errorf("%s, a settle of reads that need nothing wrote the record (version %d to %d)", name,
				before.Version, after.Version)
		}
	}
}

// noteStagedCases are the note cases whose subject is a write another party
// lands under one of the store's own decisions, which only the records it is
// built on can stage ([vanishingRuns]).
var noteStagedCases = []struct {
	name string
	fn   func(*testing.T, *vanishingRuns, sandbox.PendingStore)
}{
	{"ASettleOfARecordRemovedUnderItAnswersNothing", testASettleOfARecordRemovedUnderItAnswersNothing},
}

// vanishingRuns is a coordination store's run records that, once armed, remove
// a run's record between one decision of the store's and its write — the
// run's ending deleting it under a write it lost to.
type vanishingRuns struct {
	records
	armed atomic.Bool
}

// MutateSandboxRun makes the read-decide-write as the records would, the
// record removed under the first decision once armed.
func (r *vanishingRuns) MutateSandboxRun(ctx context.Context, turnID string,
	decide func(coord.Record) (coord.SandboxRunChange, error)) (coord.Record, bool, error) {

	return r.records.MutateSandboxRun(ctx, turnID, func(read coord.Record) (coord.SandboxRunChange, error) {
		change, err := decide(read)
		if r.armed.CompareAndSwap(true, false) {
			if _, _, removeErr := r.records.MutateSandboxRun(ctx, turnID,
				func(coord.Record) (coord.SandboxRunChange, error) {
					return coord.SandboxRunChange{Delete: true}, nil
				}); removeErr != nil {
				return coord.SandboxRunChange{}, removeErr
			}
		}
		return change, err
	})
}

// A SETTLE OF A RECORD REMOVED UNDER IT ANSWERS NOTHING. A settle that marks
// decides to write: its attempt finds the take's reads, and can then lose its
// race to the record's removal — the run's ending deleting it — and the store
// asks again and finds no record. That attempt's decision recorded nothing, so
// it is no answer, as a record gone before the settle is none. (A settle that
// answers as the reads stand writes nothing where it finds them, so it races
// nobody: what it read is its answer.)
//
// Mutation: answer what an earlier attempt decided where the last found no
// record, and the settle answers notes marked by a write that never landed.
func testASettleOfARecordRemovedUnderItAnswersNothing(t *testing.T, runs *vanishingRuns, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-vanish"))
	mustFile(t, s, "t-vanish", "n-1")
	if _, err := s.TakeNotes(ctx, "t-vanish", namedTake(launch, "lost", "n-1")); err != nil {
		t.Fatal(err)
	}
	runs.armed.Store(true)
	settled, err := s.SettleTake(ctx, "t-vanish", sandbox.NoteSettle{Take: "lost", Unanswered: true})
	if err != nil || len(settled) != 0 {
		t.Errorf("a settle of a record removed under it = %+v, %v, want nothing", settled, err)
	}
	if _, found, getErr := s.Get(ctx, "t-vanish"); getErr != nil || found {
		t.Errorf("the record is found (%v, %v) after its removal", found, getErr)
	}
}
