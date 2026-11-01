package executordomain

import (
	"database/sql"
	"slices"
	"strings"
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

// AN END ABSORBS EXACTLY WHAT ITS TURN'S FIRST READ HANDED OUT: the turns its
// captures carry as absorbed go, each only where it ended at or before that
// read; a turn the read left out (past its bound) stays for the next turn to be
// shown, and so does a turn named in error that ended after the read, which the
// read could not have shown.
//
// Mutation: absorb every ended row at or below the read, as the rule once was,
// and the turn the read left out is absorbed although nobody was shown it; drop
// the end_seq bound from absorbStatement and the turn that ended after the
// read goes.
func TestAnEndAbsorbsExactlyWhatItsFirstReadHandedOut(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	leftOut, shown, head, concurrent, current := newTurn(), newTurn(), newTurn(), newTurn(), newTurn()
	for _, run := range []string{leftOut, shown, head} {
		e.capture(capturing("ana", "chat:room", run, 1, 1, 100, budget))
		e.end(ending("ana", "chat:room", run, "", Ending{Outcome: OutcomeCompleted}))
	}
	read := capturing("ana", "chat:room", current, 1, 1, 100, budget)
	read.ReadThrough = e.at().Packed()
	read.Absorbs = []string{head, shown, concurrent}
	e.capture(read)
	// A TURN THAT RAN BESIDE IT and ended after the current turn read the
	// head: not shown to it, so not absorbed by it, whatever its list says.
	e.capture(capturing("ana", "chat:room", concurrent, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", concurrent, "", Ending{Outcome: OutcomeCompleted}))

	e.end(ending("ana", "chat:room", current, "", Ending{Outcome: OutcomeCompleted}))
	for _, gone := range []string{shown, head} {
		if _, found := e.session("ana", gone); found {
			t.Errorf("turn %s was handed out by the read and was not absorbed", gone)
		}
		if _, tombed := e.tombed("ana", gone); !tombed {
			t.Errorf("absorbed turn %s lost its tombstone, so a late capture could "+
				"recreate it", gone)
		}
	}
	for name, kept := range map[string]string{
		"the turn the read left out": leftOut, "the turn that ended after the read": concurrent,
		"the ending turn": current,
	} {
		if _, found := e.session("ana", kept); !found {
			t.Errorf("%s was absorbed", name)
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
	// EVEN NAMED, as a writer's mistake would name it.
	c.ReadThrough, c.Absorbs = readAt.Packed(), []string{earlier}
	e.capture(c)
	e.end(ending("ana", "", current, "", Ending{Outcome: OutcomeCompleted}))
	if _, found := e.session("ana", earlier); !found {
		t.Fatal("a keyless turn's end absorbed another keyless turn of the seat")
	}
}

// AN END DEDUPES ONLY ON A NON-EMPTY WORK KEY, AND KEEPS WHAT WAS DELIVERED: an
// older attempt at the same unit of work goes whether its captures carried the
// key or only its end did, and the surviving row says the work's reply went out
// if any attempt delivered it; two turns that share no work key leave each
// other alone.
//
// THE END IS AUTHORITATIVE FOR THE KEY: an attempt whose captures carried none
// takes its end's, which is what a later attempt's dedupe finds it by.
//
// Mutation: leave the end's work key off endRowStatement and the first
// attempt, whose capture carried no key, survives its retry; drop the
// work-key condition, or the merge of `delivered`, and a keyless sibling is
// erased, or the account loses the reply the first attempt sent.
func TestAnEndDedupesOnlyOnANonEmptyWorkKeyAndKeepsWhatWasDelivered(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	first, keyed, retry, other := newTurn(), newTurn(), newTurn(), newTurn()
	// AN ATTEMPT WHOSE CAPTURE CARRIED NO KEY, and whose end did.
	e.capture(capturing("ana", "task:ENG-7", first, 1, 1, 100, budget))
	e.end(ending("ana", "task:ENG-7", first, "work-1", Ending{Outcome: OutcomeCompleted,
		Delivered: true, DeliveredTo: "chatlet"}))
	if s, _ := e.session("ana", first); s.WorkKey != "work-1" {
		t.Fatalf("the first attempt's row holds work key %q after its end named %q",
			s.WorkKey, "work-1")
	}
	// AN ATTEMPT WHOSE CAPTURE CARRIED THE KEY.
	withKey := capturing("ana", "task:ENG-7", keyed, 1, 1, 100, budget)
	withKey.WorkKey = "work-1"
	e.capture(withKey)
	e.end(ending("ana", "task:ENG-7", keyed, "work-1", Ending{Outcome: OutcomeFailed}))
	e.capture(capturing("ana", "task:ENG-7", other, 1, 1, 100, budget))
	e.end(ending("ana", "task:ENG-7", other, "", Ending{Outcome: OutcomeCompleted}))

	e.capture(capturing("ana", "task:ENG-7", retry, 1, 1, 100, budget))
	e.end(ending("ana", "task:ENG-7", retry, "work-1", Ending{Outcome: OutcomeCompleted}))
	for name, attempt := range map[string]string{
		"the attempt whose end alone carried the key": first,
		"the attempt whose capture carried the key":   keyed,
	} {
		if _, found := e.session("ana", attempt); found {
			t.Errorf("%s survived its retry's end", name)
		}
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

// AN END NAMING ANOTHER UNIT OF WORK THAN ITS TURN'S WRITES NOTHING, as the
// decide refuses it: a turn has one unit of work, and an end that disagrees
// with what the turn's capture said is not this turn's account.
//
// Mutation: drop the sameWork guard from applyEnd and the turn is ended under
// a key that dedupes another unit of work's attempts.
func TestAnEndNamingAnotherUnitOfWorkWritesNothing(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := newTurn()
	keyed := capturing("ana", "task:ENG-7", run, 1, 1, 100, budget)
	keyed.WorkKey = "work-1"
	e.capture(keyed)
	if _, n := e.end(ending("ana", "task:ENG-7", run, "work-2", Ending{Outcome: OutcomeCompleted})); n != 0 {
		t.Errorf("an end naming another unit of work wrote %d rows", n)
	}
	if s, _ := e.session("ana", run); s.Ended() || s.WorkKey != "work-1" {
		t.Errorf("the turn is %+v after an end naming another unit of work", s)
	}
}

// A CAPTURE KEEPS ITS TURN'S FIRST READ AND ITS WORK: the position the turn's
// first launch read the head at, the turns that read handed out, and the first
// work key a record carried, decided from the row in Go rather than inside the
// statement; a later capture carrying another read changes none of them, and
// one naming another unit of work writes nothing.
//
// Mutation: take read_through and absorbs from the newest capture and the
// relaunch's capture replaces what the turn's end will absorb.
func TestACaptureKeepsItsTurnsFirstReadAndWork(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run, shown, later := newTurn(), newTurn(), newTurn()
	first := capturing("ana", "chat:room", run, 1, 1, 100, budget)
	first.ReadThrough, first.Absorbs, first.WorkKey = 7, []string{shown}, "work-1"
	e.capture(first)
	relaunch := capturing("ana", "chat:room", run, 2, 1, 100, budget)
	relaunch.ReadThrough, relaunch.Absorbs = 9, []string{later}
	if _, n := e.capture(relaunch); n == 0 {
		t.Fatal("the relaunch's capture wrote nothing")
	}
	s, _ := e.session("ana", run)
	if s.ReadThrough.Packed() != 7 || !slices.Equal(s.Absorbs, []string{shown}) ||
		s.WorkKey != "work-1" || s.SessionID != relaunch.SessionID {
		t.Errorf("the row is (read %d, absorbs %v, work %q, session %s), want the first "+
			"read and work beside the relaunch's session", s.ReadThrough.Packed(), s.Absorbs,
			s.WorkKey, s.SessionID)
	}
	elsewhere := capturing("ana", "chat:room", run, 3, 1, 100, budget)
	elsewhere.WorkKey = "work-2"
	if _, n := e.capture(elsewhere); n != 0 {
		t.Errorf("a capture naming another unit of work wrote %d rows", n)
	}
}

// A CAPTURE NAMING WHAT NO READER CAN PARSE WRITES NOTHING: an object key the
// store does not spell, a digest that is not one, and a list of absorbed turns
// no read hands out. Each would be a row that fails the collector, its audit,
// the backup and every read of the seat on every node, for as long as it stays.
//
// Mutation: drop nameable from applyCapture and the malformed key is recorded.
func TestACaptureNamingWhatNoReaderCanParseWritesNothing(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	for name, spoil := range map[string]func(*Capture){
		"an object key the store does not spell": func(c *Capture) { c.Object = "session-7" },
		"an uppercase object key":                func(c *Capture) { c.Object = strings.ToUpper(c.Object) },
		"a digest that is not one":               func(c *Capture) { c.ObjectHash = "abc" },
		"an absorbed turn in another spelling": func(c *Capture) {
			c.ReadThrough, c.Absorbs = 7, []string{strings.ToUpper(newTurn())}
		},
		"the turn absorbing itself": func(c *Capture) { c.ReadThrough, c.Absorbs = 7, []string{c.Turn} },
		"a turn absorbed twice": func(c *Capture) {
			twice := newTurn()
			c.ReadThrough, c.Absorbs = 7, []string{twice, twice}
		},
		"absorbed turns with no read": func(c *Capture) { c.Absorbs = []string{newTurn()} },
	} {
		run := newTurn()
		c := capturing("ana", "chat:room", run, 1, 1, 100, budget)
		spoil(&c)
		if _, n := e.capture(c); n != 0 {
			t.Errorf("a capture with %s wrote %d rows", name, n)
		}
		if _, found := e.session("ana", run); found {
			t.Errorf("a capture with %s left a row", name)
		}
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
// its tombstone (by the retention no record decided before it can still land,
// so a tombstone kept would only be a second row to age out).
//
// Mutation: drop the last_at comparison from expiredIn and the freshly
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

// A RETENTION DELETE TAKES A TURN THAT NEVER ENDED: the duty never nominates a
// turn whose run is live, so a row with no end and no run is one whose end was
// lost, and the retention is what bounds it.
//
// Mutation: refuse an unended row in expiredIn and a lost turn's session is
// kept for ever.
func TestARetentionDeleteTakesATurnThatNeverEnded(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := newTurn()
	captured, _ := e.capture(capturing("ana", "chat:room", run, 1, 1, 100, budget))
	e.apply(ConversationSubject("ana", "chat:room", run), OpDelete, ScopeSet{Subject: true},
		Delete{V: 1, Handle: "ana", Turn: run, ConversationKey: "chat:room",
			Cutoff: stamped(captured.Seq)})
	if _, found := e.session("ana", run); found {
		t.Error("a turn that never ended outlived the retention")
	}
}

// A REMOVED SEAT STAYS REMOVED: its rows go, its live turns are tombstoned,
// and a capture or an end decided before the removal and landing after it
// writes nothing — the turn that had a row by its tombstone, and the turn that
// had none (no tombstone could name it) by the removal's position, which every
// capture and end carries the one its decide saw. A capture of the seat's next
// tenure, decided once the removal was applied, lands; and the removal's
// record goes with the retention.
//
// Mutation: drop the removal check from applyCapture (or applyEnd) and the
// turn that had no row comes back for a removed seat; drop the tombstones from
// removeSeat and the live turn does.
func TestARemovedSeatStaysRemoved(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	done, live, rowless, endless := newTurn(), newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", "chat:a", done, 1, 1, 100, budget))
	e.end(ending("ana", "chat:a", done, "", Ending{Outcome: OutcomeCompleted}))
	e.capture(capturing("ana", "chat:b", live, 1, 1, 100, budget))
	e.capture(capturing("ben", "chat:b", newTurn(), 1, 1, 100, budget))

	removed := e.removeSeat("ana")
	for _, run := range []string{done, live} {
		if _, found := e.session("ana", run); found {
			t.Errorf("the removed seat's turn %s kept its row", run)
		}
	}
	if reason, ok := e.tombed("ana", live); !ok || reason != tombSeatRemoved {
		t.Errorf("the removed seat's live turn's tombstone is (%q, %v), want %q",
			reason, ok, tombSeatRemoved)
	}
	// THE LIVE TURN IS REFUSED BY ITS TOMBSTONE, even decided after the
	// removal: its seat's removal ended it.
	lateLive := capturing("ana", "chat:b", live, 1, 2, 100, budget)
	lateLive.SeenRemoval = removed.Packed()
	if _, n := e.capture(lateLive); n != 0 {
		t.Errorf("a capture of the live turn landing after the removal wrote %d rows", n)
	}
	// DECIDED BEFORE THE REMOVAL, so carrying none it saw.
	if _, n := e.capture(capturing("ana", "chat:c", rowless, 1, 1, 100, budget)); n != 0 {
		t.Errorf("the first capture of a turn with no row, decided before the removal, "+
			"wrote %d rows", n)
	}
	if _, n := e.end(ending("ana", "chat:c", endless, "", Ending{Outcome: OutcomeFailed})); n != 0 {
		t.Errorf("the end of a turn that captured nothing, decided before the removal, "+
			"wrote %d rows", n)
	}
	// THE SEAT'S NEXT TENURE, decided by a node that applied the removal.
	next := capturing("ana", "chat:c", newTurn(), 1, 1, 100, budget)
	next.SeenRemoval = removed.Packed()
	if _, n := e.capture(next); n == 0 {
		t.Error("a capture decided after the removal, carrying it, wrote nothing")
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

	// THE REMOVAL'S RECORD GOES WITH THE RETENTION: not before its cutoff,
	// and at it.
	forget := func(cutoff time.Time) int {
		_, n := e.apply(SeatSubject("ana"), OpDelete, ScopeSet{Subject: true},
			Delete{V: 1, Handle: "ana", Cutoff: cutoff})
		return n
	}
	if n := forget(stamped(removed.Seq).Add(-time.Second)); n != 0 {
		t.Errorf("the removal's record was forgotten before its cutoff (%d rows)", n)
	}
	if n := forget(stamped(removed.Seq)); n != 1 {
		t.Errorf("the removal's record at its cutoff: %d rows went, want 1", n)
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
