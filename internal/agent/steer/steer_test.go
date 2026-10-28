package steer_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/events/types"
)

func note(id string) steer.Note { return steer.Note{ID: id, Text: "use staging", By: "ops"} }

// THE TRANSITION TABLE: every state a box can be in, and what an offer is
// answered there. Each answer is a different fact a person acts on, so a box
// that answered `full` where it meant `closed` would tell somebody to wait for
// a turn that has already ended.
func TestTheSteerTransitionTable(t *testing.T) {
	t.Parallel()
	// spent is a box that has taken a turn's worth of notes and read them
	// all, so nothing waits and nothing more is taken.
	spent := func() *steer.Box {
		b := steer.New()
		for i := range steer.MaxTurnNotes {
			if got := b.Offer(note("spent-" + string(rune('a'+i)))); got != steer.StatusAccepted {
				t.Fatalf("spending: offer %d answered %s", i, got)
			}
			b.Drain()
		}
		return b
	}
	full := func() *steer.Box {
		b := steer.New()
		for i := range steer.MaxPendingNotes {
			if got := b.Offer(note(string(rune('a' + i)))); got != steer.StatusAccepted {
				t.Fatalf("filling: offer %d answered %s", i, got)
			}
		}
		return b
	}
	for _, tc := range []struct {
		name string
		box  func() *steer.Box
		want steer.Status
	}{
		{"an open box takes a note", steer.New, steer.StatusAccepted},
		{"a box at its bound refuses the next", full, steer.StatusFull},
		{"a drained box takes notes again", func() *steer.Box {
			b := full()
			b.Drain()
			return b
		}, steer.StatusAccepted},
		{"a closed box takes nothing", func() *steer.Box {
			b := steer.New()
			b.Close()
			return b
		}, steer.StatusClosed},
		{"a box that took a turn's worth takes no more", spent, steer.StatusSpent},
		// SPENT OUTRANKS FULL: a turn that will never take the note again
		// must not tell a person to wait for a read.
		{"a spent box with a full wait says spent", func() *steer.Box {
			b := spent()
			b.Return(t.Context(), []steer.Note{note("back-1"), note("back-2"), note("back-3"),
				note("back-4"), note("back-5")})
			return b
		}, steer.StatusSpent},
		{"a closed spent box says closed", func() *steer.Box {
			b := spent()
			b.Close()
			return b
		}, steer.StatusClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// THE PROBE ANSWERS WHAT THE OFFER WILL, and takes nothing:
			// the offer that follows it is answered the same.
			box := tc.box()
			if got := box.Status(); got != tc.want {
				t.Errorf("probe answered %s, want %s", got, tc.want)
			}
			if got := box.Offer(note("new")); got != tc.want {
				t.Errorf("offer answered %s, want %s", got, tc.want)
			}
		})
	}
}

// A RETRIED NOTE IS ONE NOTE. The id is the person's request id, so a retry
// after an answer lost in transit carries the same one — and a turn that read
// the same instruction twice would act on it twice. The retry is answered
// `accepted` in every later state, the box's end included: the first answer
// was the true one about that note.
func TestARetriedNoteIsOneNote(t *testing.T) {
	t.Parallel()
	b := steer.New()
	for range 3 {
		if got := b.Offer(note("req-1")); got != steer.StatusAccepted {
			t.Fatalf("a retry answered %s", got)
		}
	}
	if got := b.Drain(); len(got) != 1 {
		t.Fatalf("drained %d notes for one request retried three times", len(got))
	}
	if got := b.Offer(note("req-1")); got != steer.StatusAccepted {
		t.Errorf("a retry after the turn read the note answered %s", got)
	}
	if got := b.Drain(); len(got) != 0 {
		t.Errorf("a retry after the turn read the note was read again: %+v", got)
	}
	b.Close()
	if got := b.Offer(note("req-1")); got != steer.StatusAccepted {
		t.Errorf("a retry after the turn ended answered %s, not the note's first answer", got)
	}
}

// CLOSING RETURNS WHAT NOBODY READ, ONCE. Those notes expired, and a caller
// that closes on two paths must not report them twice.
func TestCloseReturnsTheUnreadNotesOnce(t *testing.T) {
	t.Parallel()
	b := steer.New()
	b.Offer(note("read"))
	b.Drain()
	b.Offer(note("missed"))
	expired := b.Close()
	if len(expired) != 1 || expired[0].ID != "missed" {
		t.Fatalf("close returned %+v, want only the note nobody read", expired)
	}
	if again := b.Close(); again != nil {
		t.Errorf("a second close returned %+v", again)
	}
	if got := b.Drain(); got != nil {
		t.Errorf("a closed box drained %+v", got)
	}
}

// LOOKING DOES NOT TAKE. A run is ended when a note waits, and the run after
// it is the one that reads the note — so the check that ends the first must
// leave the note for the second, and a box that drained or closed has nothing
// waiting.
func TestWaitingLeavesTheNoteForTheNextRun(t *testing.T) {
	t.Parallel()
	b := steer.New()
	if b.Waiting() {
		t.Fatal("an empty box reported a note waiting")
	}
	b.Offer(note("n"))
	for range 2 {
		if !b.Waiting() {
			t.Fatal("a box holding a note did not report it waiting, twice")
		}
	}
	if got := b.Drain(); len(got) != 1 {
		t.Fatalf("drain after a look took %+v, want the one note", got)
	}
	if b.Waiting() {
		t.Error("a drained box still reported a note waiting")
	}
	b.Offer(note("late"))
	b.Close()
	if b.Waiting() {
		t.Error("a closed box reported a note waiting")
	}
	var none *steer.Box
	if none.Waiting() {
		t.Error("a nil box reported a note waiting")
	}
}

func TestANoteIsBounded(t *testing.T) {
	t.Parallel()
	if err := steer.Validate("  "); err == nil {
		t.Error("an empty note is valid")
	}
	if err := steer.Validate(strings.Repeat("é", steer.MaxNoteRunes)); err != nil {
		t.Errorf("a note at the bound, in multi-byte runes, is refused: %v", err)
	}
	var long *steer.TooLongError
	if err := steer.Validate(strings.Repeat("a", steer.MaxNoteRunes+1)); !errors.As(err, &long) {
		t.Errorf("a note past the bound is %v", err)
	}
}

func TestStatusesAreClosed(t *testing.T) {
	t.Parallel()
	for _, s := range steer.Statuses {
		if !s.Valid() {
			t.Errorf("%s is not valid", s)
		}
	}
	if steer.Status("maybe").Valid() || steer.Status("unsupported").Valid() {
		t.Error("a status this build does not answer is valid off the wire")
	}
	for _, r := range steer.ReadsAll {
		if !r.Valid() {
			t.Errorf("%s is not valid", r)
		}
	}
	if steer.Reads("later").Valid() {
		t.Error("an unknown reads value off the wire is valid")
	}
}

// A LAUNCH THAT FAILED GIVES ITS NOTES BACK, FIRST. They were taken for a brief
// nothing ran, so the next step reads them before anything that arrived since —
// or the box's close reports them expired — and a retry of one is still the
// note it was, answered accepted and not taken twice.
func TestReturnedNotesAreReadFirstAndStayOneNote(t *testing.T) {
	t.Parallel()
	b := steer.New()
	b.Offer(note("first"))
	taken := b.Brief(t.Context())
	b.Offer(note("later"))
	b.Return(t.Context(), taken)
	if got := b.Offer(note("first")); got != steer.StatusAccepted {
		t.Fatalf("a retry of a returned note answered %s", got)
	}
	got := b.Take(t.Context(), steer.Reading{})
	ids := make([]string, 0, len(got))
	for _, n := range got {
		ids = append(ids, n.ID)
	}
	if !slices.Equal(ids, []string{"first", "later"}) {
		t.Fatalf("read %v after a return, want the returned note first and once", ids)
	}
	// A CLOSED BOX KEEPS NOTHING: what the caller hands back after the close
	// is its own to report.
	b.Close()
	b.Return(t.Context(), got)
	if b.Waiting() {
		t.Error("a closed box took notes back")
	}
}

// A HANDED-OVER BOX TAKES NO NEW NOTE AND KEEPS WHAT A FAILED LAUNCH GIVES
// BACK. The hand-over answers what the box held, for the caller to file on the
// run's record; a new note is answered closed from then, which sends it there
// too, and a retry of one the box took is still accepted. But the launch that
// handed the box over can still fail, and its brief's notes come back: kept,
// read by whatever reads next, and reported expired by the box's close — never
// dropped as a closed box would.
//
// Mutation: hand over by closing the box, and the brief's notes a failed launch
// returns are lost.
func TestAHandedOverBoxKeepsWhatAFailedLaunchGivesBack(t *testing.T) {
	t.Parallel()
	b := steer.New()
	b.Offer(note("briefed"))
	b.Offer(note("late"))
	brief := b.Brief(t.Context())
	b.Offer(note("after-brief"))
	handed := b.HandOver()
	if len(handed) != 1 || handed[0].ID != "after-brief" {
		t.Fatalf("the hand-over answered %+v, want the note the brief did not take", handed)
	}
	if got := b.Offer(note("new")); got != steer.StatusClosed {
		t.Errorf("a new note after the hand-over answered %s, want closed", got)
	}
	if got := b.Offer(note("late")); got != steer.StatusAccepted {
		t.Errorf("a retry of a note the box took answered %s, want accepted", got)
	}
	if again := b.HandOver(); len(again) != 0 {
		t.Errorf("a second hand-over answered %+v", again)
	}
	b.Return(t.Context(), brief)
	if !b.Waiting() {
		t.Fatal("the box dropped the notes a failed launch gave back")
	}
	expired := b.Close()
	ids := make([]string, 0, len(expired))
	for _, n := range expired {
		ids = append(ids, n.ID)
	}
	if !slices.Equal(ids, []string{"briefed", "late"}) {
		t.Errorf("the close reported %v expired, want the brief's notes the failed launch gave back", ids)
	}
}

// A TURN'S NOTES ARE COUNTED, NOT ITS WAITING ONES: reading every note does not
// make room for an eleventh.
func TestATurnTakesAtMostItsNotes(t *testing.T) {
	t.Parallel()
	b := steer.New()
	for i := range steer.MaxTurnNotes {
		if got := b.Offer(note("n-" + string(rune('a'+i)))); got != steer.StatusAccepted {
			t.Fatalf("note %d answered %s", i+1, got)
		}
		b.Drain()
	}
	if got := b.Offer(note("one-more")); got != steer.StatusSpent {
		t.Errorf("note %d answered %s, want spent", steer.MaxTurnNotes+1, got)
	}
	if got := b.Offer(note("n-a")); got != steer.StatusAccepted {
		t.Errorf("a retry of a note the spent turn took answered %s", got)
	}
}

// ONE RECORD PER FACT: a reading makes the note delivered there, and none makes
// it expired — so no publisher can state a delivery without saying where, or an
// expiry that names a step — and the send time is always UTC.
func TestARecordIsDeliveredExactlyWhereItWasRead(t *testing.T) {
	t.Parallel()
	turn := steer.Turn{Agent: "a", AgentHandle: "swe", Role: "Engineer", TurnID: "run-1", WorkKey: "wk"}
	sent := time.Date(2026, 10, 26, 9, 0, 0, 0, time.FixedZone("CET", 3600))
	n := steer.Note{ID: "n-1", Text: "use staging", By: "founder", ByKind: "human",
		OperatorID: "session:f", At: sent}
	read := steer.Record(turn, n, &steer.Reading{Phase: types.PhaseExecute, Iteration: 2, Round: 1,
		Boundary: types.SteerBoundaryToolBatch, Launch: "job-2"})
	if read.Outcome != types.SteerDelivered || read.Boundary != types.SteerBoundaryToolBatch ||
		read.LaunchID != "job-2" || read.Phase != types.PhaseExecute || read.Iteration != 2 ||
		read.Round != 1 || read.TurnID != "run-1" || read.AgentHandle != "swe" || read.Note != "use staging" ||
		read.SteeredBy != "founder" || read.SteeredByKind != "human" || read.OperatorID != "session:f" ||
		read.WorkKey != "wk" || read.NoteID != "n-1" || read.RoleName != "Engineer" || read.Agent != "a" {
		t.Errorf("delivered as %+v", read)
	}
	if !read.SentAt.Equal(sent) || read.SentAt.Location() != time.UTC {
		t.Errorf("sent at %v, want %v in UTC", read.SentAt, sent)
	}
	expired := steer.Record(turn, n, nil)
	if expired.Outcome != types.SteerExpired || expired.Boundary != "" || expired.Phase != "" ||
		expired.LaunchID != "" || expired.Round != 0 {
		t.Errorf("expired as %+v", expired)
	}
}
