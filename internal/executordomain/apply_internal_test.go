package executordomain

import (
	"database/sql"
	"testing"
	"time"
)

// budget is a seat budget no case here reaches unless it means to.
const budget = 1 << 30

// A LATE CAPTURE NEVER ROLLS A SESSION BACK. A collection and a periodic save
// of one launch, or a retried read whose first attempt records late, land in
// any order; the row keeps the newest (launch, capture) pair, and a later
// launch's first capture is newer than every capture of the launch before it.
//
// Mutation: drop the newer() guard from applyCapture and the late (1, 1)
// capture rolls the row back to an older session.
func TestALateCaptureNeverRollsASessionBack(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := newTurn()
	second := capturing("ana", "chat:room", run, 1, 2, 100, budget)
	if _, n := e.capture(second); n == 0 {
		t.Fatal("the premise: the first capture wrote nothing")
	}
	late := capturing("ana", "chat:room", run, 1, 1, 100, budget)
	if _, n := e.capture(late); n != 0 {
		t.Errorf("a capture older than the row's wrote %d rows", n)
	}
	if s, _ := e.session("ana", run); s.SessionID != second.SessionID || s.CaptureSeq != 2 {
		t.Errorf("the row holds (%d, %d) %s after a late capture, want the (1, 2) one",
			s.LaunchSeq, s.CaptureSeq, s.SessionID)
	}
	relaunch := capturing("ana", "chat:room", run, 2, 1, 100, budget)
	if _, n := e.capture(relaunch); n == 0 {
		t.Fatal("a relaunch's first capture wrote nothing")
	}
	if s, _ := e.session("ana", run); s.SessionID != relaunch.SessionID {
		t.Errorf("the row names %s, want the relaunch's %s", s.SessionID, relaunch.SessionID)
	}
}

// AN ENDED TURN TAKES NO FURTHER CAPTURE, and its end is terminal: a capture
// decided before the end and landing after it writes nothing, and a second end
// changes nothing, because the end's tombstone and the row's end both refuse.
//
// Mutation: drop the tombstone and the ended checks from applyCapture and the
// late capture reopens the ended row with a session the turn's ending does not
// describe.
func TestAnEndedTurnTakesNoFurtherCapture(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := newTurn()
	e.capture(capturing("ana", "chat:room", run, 1, 1, 100, budget))
	at, _ := e.end(ending("ana", "chat:room", run, "", Ending{Outcome: OutcomeCompleted,
		Decision: "done"}))
	if reason, ok := e.tombed("ana", run); !ok || reason != tombEnded {
		t.Fatalf("the ended turn's tombstone is (%q, %v), want %q", reason, ok, tombEnded)
	}
	if _, n := e.capture(capturing("ana", "chat:room", run, 2, 1, 100, budget)); n != 0 {
		t.Errorf("a capture after the end wrote %d rows", n)
	}
	if _, n := e.end(ending("ana", "chat:room", run, "", Ending{Outcome: OutcomeFailed})); n != 0 {
		t.Errorf("a second end wrote %d rows", n)
	}
	s, _ := e.session("ana", run)
	if !s.Ended() || s.Ending.Outcome != OutcomeCompleted || s.EndedAtPosition != at ||
		s.LaunchSeq != 1 {
		t.Errorf("the row after a late capture and a second end is %+v, want the first "+
			"end's, at %s", s, at)
	}
}

// AN END ABSORBS WHAT ITS TURN'S FIRST LAUNCH READ, and nothing after it: every
// ended row of the conversation at or below the position the head was read at
// goes (the launch continued the newest of them or was shown them), and a row
// that ended after that read stays, as a sibling the next turn is told of.
//
// Mutation: compare end_seq < read_through rather than <= in absorb, and the
// head the turn continued survives its absorption.
func TestAnEndAbsorbsWhatItsTurnsFirstLaunchRead(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	earlier, head, concurrent, current := newTurn(), newTurn(), newTurn(), newTurn()
	for _, run := range []string{earlier, head} {
		e.capture(capturing("ana", "chat:room", run, 1, 1, 100, budget))
	}
	e.end(ending("ana", "chat:room", earlier, "", Ending{Outcome: OutcomeCompleted}))
	readAt, _ := e.end(ending("ana", "chat:room", head, "", Ending{Outcome: OutcomeCompleted}))

	read := capturing("ana", "chat:room", current, 1, 1, 100, budget)
	read.ReadThrough = readAt.Packed()
	e.capture(read)
	// A TURN THAT RAN BESIDE IT and ended after the current turn read the
	// head: not shown to it, so not absorbed by it.
	e.capture(capturing("ana", "chat:room", concurrent, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", concurrent, "", Ending{Outcome: OutcomeCompleted}))

	e.end(ending("ana", "chat:room", current, "", Ending{Outcome: OutcomeCompleted}))
	for _, gone := range []string{earlier, head} {
		if _, found := e.session("ana", gone); found {
			t.Errorf("turn %s ended at or below the read and was not absorbed", gone)
		}
		if _, tombed := e.tombed("ana", gone); !tombed {
			t.Errorf("absorbed turn %s lost its tombstone, so a late capture could "+
				"recreate it", gone)
		}
	}
	for _, kept := range []string{concurrent, current} {
		if _, found := e.session("ana", kept); !found {
			t.Errorf("turn %s ended after the read (or is the ending turn) and was absorbed", kept)
		}
	}
}

// A TURN WITH NO CONVERSATION ABSORBS NOTHING, because it is alone in its own:
// it keeps its row until the retention takes it, and every other keyless turn
// of the seat keeps its own, so each stays a session the seat can find again.
//
// Mutation: absorb on an empty conversation key and the earlier keyless turn
// is deleted by an end that never saw it.
func TestATurnWithNoConversationAbsorbsNothing(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	earlier, current := newTurn(), newTurn()
	e.capture(capturing("ana", "", earlier, 1, 1, 100, budget))
	readAt, _ := e.end(ending("ana", "", earlier, "", Ending{Outcome: OutcomeCompleted}))
	c := capturing("ana", "", current, 1, 1, 100, budget)
	c.ReadThrough = readAt.Packed()
	e.capture(c)
	e.end(ending("ana", "", current, "", Ending{Outcome: OutcomeCompleted}))
	if _, found := e.session("ana", earlier); !found {
		t.Fatal("a keyless turn's end absorbed another keyless turn of the seat")
	}
}

// AN END DEDUPES ONLY ON A NON-EMPTY WORK KEY, AND KEEPS WHAT WAS DELIVERED: an
// older attempt at the same unit of work goes, and the surviving row says the
// work's reply went out if any attempt delivered it; two turns that share no
// work key leave each other alone.
//
// Mutation: drop the work-key condition, or the merge of `delivered`, and a
// keyless sibling is erased, or the account loses the reply the first attempt
// sent.
func TestAnEndDedupesOnlyOnANonEmptyWorkKeyAndKeepsWhatWasDelivered(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	first, retry, other := newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", "task:ENG-7", first, 1, 1, 100, budget))
	e.end(ending("ana", "task:ENG-7", first, "work-1", Ending{Outcome: OutcomeCompleted,
		Delivered: true, DeliveredTo: "chatlet"}))
	e.capture(capturing("ana", "task:ENG-7", other, 1, 1, 100, budget))
	e.end(ending("ana", "task:ENG-7", other, "", Ending{Outcome: OutcomeCompleted}))

	e.capture(capturing("ana", "task:ENG-7", retry, 1, 1, 100, budget))
	e.end(ending("ana", "task:ENG-7", retry, "work-1", Ending{Outcome: OutcomeCompleted}))
	if _, found := e.session("ana", first); found {
		t.Error("the earlier attempt at the same work survived its retry's end")
	}
	if _, found := e.session("ana", other); !found {
		t.Error("a turn with no work key was deduped away")
	}
	survivor, _ := e.session("ana", retry)
	if !survivor.Ended() || !survivor.Ending.Delivered {
		t.Errorf("the surviving attempt says delivered=%v, and the first attempt "+
			"delivered the work's reply", survivor.Ending != nil && survivor.Ending.Delivered)
	}
}

// AN END WITH NOTHING CAPTURED KEEPS THE ENDING ALONE: an ended row naming no
// object, so the next turn is told how this one ended, and it absorbs nothing,
// because it read nothing it could stand in for.
//
// Mutation: return early from applyEnd when the turn has no row, and the
// conversation forgets the turn ever ended.
func TestAnEndWithNothingCapturedKeepsTheEndingAlone(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	head, lost := newTurn(), newTurn()
	e.capture(capturing("ana", "chat:room", head, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", head, "", Ending{Outcome: OutcomeCompleted}))
	e.end(ending("ana", "chat:room", lost, "", Ending{Outcome: OutcomeLost,
		Failure: "the box was lost before its first collection"}))
	s, found := e.session("ana", lost)
	if !found || !s.Ended() || s.HasObject() || s.Ending.Outcome != OutcomeLost {
		t.Fatalf("the uncaptured turn's row is (%+v, %v), want an ended row with no "+
			"object holding its ending", s, found)
	}
	if _, found := e.session("ana", head); !found {
		t.Error("an end with nothing captured absorbed the conversation's head")
	}
}

// THE SEAT'S BUDGET RELEASES THE OLDEST ENDED SESSIONS FIRST, keeping their
// endings, and never a live turn's: and a capture that cannot fit even with
// every ended session released writes nothing and releases nothing.
//
// Mutation: order the release by end_seq DESC and the newer ending's session
// goes first; release before checking the plan fits and the oversized capture
// strips every ended session for nothing.
func TestTheSeatBudgetReleasesTheOldestEndedSessionsFirst(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	oldest, newer, live, next := newTurn(), newTurn(), newTurn(), newTurn()
	const tight = 100
	e.capture(capturing("ana", "chat:a", oldest, 1, 1, 40, tight))
	e.end(ending("ana", "chat:a", oldest, "", Ending{Outcome: OutcomeCompleted}))
	e.capture(capturing("ana", "chat:b", newer, 1, 1, 40, tight))
	e.end(ending("ana", "chat:b", newer, "", Ending{Outcome: OutcomeCompleted}))
	e.capture(capturing("ana", "chat:c", live, 1, 1, 10, tight))

	if _, n := e.capture(capturing("ana", "chat:d", next, 1, 1, 30, tight)); n == 0 {
		t.Fatal("a capture the budget holds once the oldest ending is released wrote nothing")
	}
	released, _ := e.session("ana", oldest)
	if released.HasObject() || !released.Ended() {
		t.Errorf("the oldest ended turn is %+v, want its session released and its ending kept",
			released)
	}
	if kept, _ := e.session("ana", newer); !kept.HasObject() {
		t.Error("the newer ending's session was released before the older one's")
	}

	huge := newTurn()
	if _, n := e.capture(capturing("ana", "chat:e", huge, 1, 1, tight+1, tight)); n != 0 {
		t.Errorf("a capture larger than the whole budget wrote %d rows", n)
	}
	if kept, _ := e.session("ana", newer); !kept.HasObject() {
		t.Error("a capture that could never fit released an ended session anyway")
	}
	if l, _ := e.session("ana", live); !l.HasObject() {
		t.Error("a live turn's session was released")
	}
}

// A RETENTION DELETE SPARES A TURN CAPTURED SINCE ITS CUTOFF: the apply holds
// the row's own last instant to the cutoff the record carries, so a turn
// nominated and then captured again stays, and one past the cutoff goes with
// its tombstone.
//
// Mutation: drop the last_at comparison from applyDelete and the freshly
// captured turn is deleted under a running conversation.
func TestARetentionDeleteSparesATurnCapturedSinceItsCutoff(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := newTurn()
	captured, _ := e.capture(capturing("ana", "chat:room", run, 1, 1, 100, budget))
	early := stamped(captured.Seq).Add(-time.Second)
	e.apply(ConversationSubject("ana", "chat:room", run), OpDelete, ScopeSet{Subject: true},
		Delete{V: 1, Handle: "ana", Turn: run, ConversationKey: "chat:room", Cutoff: early})
	if _, found := e.session("ana", run); !found {
		t.Fatal("a delete whose cutoff is before the row's last capture removed it")
	}
	ended, _ := e.end(ending("ana", "chat:room", run, "", Ending{Outcome: OutcomeCompleted}))
	e.apply(ConversationSubject("ana", "chat:room", run), OpDelete, ScopeSet{Subject: true},
		Delete{V: 1, Handle: "ana", Turn: run, ConversationKey: "chat:room",
			Cutoff: stamped(ended.Seq)})
	if _, found := e.session("ana", run); found {
		t.Error("a delete whose cutoff is at the row's end left it")
	}
	if _, tombed := e.tombed("ana", run); tombed {
		t.Error("a retention delete left the turn's tombstone, which then lives for ever")
	}
}

// A REMOVED SEAT'S ROWS GO, AND ITS LIVE TURNS ARE TOMBSTONED: a capture decided
// before the removal and landing after it recreates nothing.
//
// Mutation: drop the tombstones from applySeat and the late capture brings a
// removed seat's turn back.
func TestARemovedSeatsLiveTurnsAreTombstoned(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	done, live := newTurn(), newTurn()
	e.capture(capturing("ana", "chat:a", done, 1, 1, 100, budget))
	e.end(ending("ana", "chat:a", done, "", Ending{Outcome: OutcomeCompleted}))
	e.capture(capturing("ana", "chat:b", live, 1, 1, 100, budget))
	e.capture(capturing("ben", "chat:b", newTurn(), 1, 1, 100, budget))

	e.apply(SeatSubject("ana"), OpSeatDelete, ScopeSet{Subject: true},
		SeatDelete{V: 1, Handle: "ana"})
	for _, run := range []string{done, live} {
		if _, found := e.session("ana", run); found {
			t.Errorf("the removed seat's turn %s kept its row", run)
		}
	}
	if reason, ok := e.tombed("ana", live); !ok || reason != tombSeatRemoved {
		t.Errorf("the removed seat's live turn's tombstone is (%q, %v), want %q",
			reason, ok, tombSeatRemoved)
	}
	if _, n := e.capture(capturing("ana", "chat:b", live, 1, 2, 100, budget)); n != 0 {
		t.Errorf("a capture landing after the seat's removal wrote %d rows", n)
	}
	var others int
	if err := e.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM executor_sessions
			WHERE agent_handle = 'ben'`).Scan(&others)
	}); err != nil {
		t.Fatal(err)
	}
	if others != 1 {
		t.Errorf("another seat holds %d rows after ana's removal, want its one", others)
	}
}

// A RECORD ON ANOTHER CONVERSATION'S SUBJECT WRITES NOTHING: the arbitration it
// passed was about a different object, so the rules it would apply were never
// decided from that conversation's snapshot.
//
// Mutation: drop the subject check in applyConversation and the capture lands
// on a conversation nobody arbitrated it against.
func TestARecordOnAnotherConversationsSubjectWritesNothing(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := newTurn()
	c := capturing("ana", "chat:room", run, 1, 1, 100, budget)
	if _, n := e.apply(ConversationSubject("ana", "chat:elsewhere", run), OpCapture,
		seatScope("ana"), c); n != 0 {
		t.Errorf("a capture published on another conversation's subject wrote %d rows", n)
	}
}
