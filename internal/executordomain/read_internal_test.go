package executordomain

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// turnsOf is the turn ids of sessions, in order.
func turnsOf(sessions []Session) []string {
	out := make([]string, len(sessions))
	for i, s := range sessions {
		out[i] = s.Turn
	}
	return out
}

// A HEAD SHOWS EVERY TURN NO ENDED TURN WAS SHOWN, whether it ended before the
// head or after it: here a turn parked on a person reads the head, a second
// turn runs beside it and fails before it captures anything, and the parked
// turn resumes and ends after it. The next turn's head is the parked turn's,
// and the failed turn — older than that head, naming no session — is still
// shown to it, and absorbed only by the end of the turn it was shown to.
//
// Mutation: read the endings as the turns newer than the head alone, as the
// read once did, and the failed turn is shown to nobody.
func TestAHeadShowsEveryTurnNoEndedTurnWasShown(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	earlier, parked, failed, next := newTurn(), newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", "chat:room", earlier, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", earlier, "", Ending{Outcome: OutcomeCompleted}))

	// THE PARKED TURN READS THE HEAD, captures and waits on a person.
	first := e.head("ana", "chat:room")
	if first.Session == nil || first.Session.Turn != earlier {
		t.Fatalf("the first read's head is %+v, want the earlier turn", first.Session)
	}
	e.capture(launching(first, capturing("ana", "chat:room", parked, 1, 1, 100, budget)))
	// A TURN BESIDE IT fails before its first capture.
	e.end(ending("ana", "chat:room", failed, "", Ending{Outcome: OutcomeFailed,
		Failure: "the box was lost before its first collection"}))
	// THE PARKED TURN resumes and ends after it.
	e.end(ending("ana", "chat:room", parked, "", Ending{Outcome: OutcomeCompleted}))

	h := e.head("ana", "chat:room")
	if h.Session == nil || h.Session.Turn != parked {
		t.Fatalf("the head is %+v, want the parked turn's session", h.Session)
	}
	if !slices.Equal(turnsOf(h.Endings), []string{failed}) {
		t.Fatalf("the next turn is told of %v, want the failed turn that ended beside the "+
			"parked one", turnsOf(h.Endings))
	}
	if _, found := e.session("ana", earlier); found {
		t.Error("the turn the parked one continued was not absorbed by its end")
	}
	if got := h.Absorbs(); !slices.Equal(got, []string{parked, failed}) {
		t.Errorf("the read hands out %v, want the head and the failed turn", got)
	}

	e.capture(launching(h, capturing("ana", "chat:room", next, 1, 1, 100, budget)))
	e.end(ending("ana", "chat:room", next, "", Ending{Outcome: OutcomeCompleted}))
	for _, gone := range []string{parked, failed} {
		if _, found := e.session("ana", gone); found {
			t.Errorf("turn %s was shown to the next turn and survived its end", gone)
		}
	}
}

// A HEAD BOUNDS WHAT IT HANDS OUT, AND WHAT IT LEAVES OUT STAYS: past
// MaxEndings the oldest endings are counted rather than shown, past
// MaxSiblings the oldest sessions beside the head are told rather than
// offered, and the next turn's end absorbs neither — the turn after it is
// shown them.
//
// Mutation: hand out every ending in Absorbs, offered or not, and a session no
// launch was offered is absorbed with the turn that was told of it.
func TestAHeadBoundsWhatItHandsOutAndWhatItLeavesStays(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	// SIBLINGS: sessions that ended beside the head (the newest of them),
	// one more than a launch is offered.
	var siblings []string
	for range MaxSiblings + 2 {
		run := newTurn()
		e.capture(capturing("ana", "chat:room", run, 1, 1, 100, budget))
		e.end(ending("ana", "chat:room", run, "", Ending{Outcome: OutcomeCompleted}))
		siblings = append(siblings, run)
	}
	// The newest of them is the head; the rest are its siblings.
	head, siblings := siblings[len(siblings)-1], siblings[:len(siblings)-1]
	h := e.head("ana", "chat:room")
	if h.Session == nil || h.Session.Turn != head {
		t.Fatalf("the head is %+v, want the newest session", h.Session)
	}
	if !slices.Equal(turnsOf(h.Siblings), siblings[1:]) || h.SiblingsOmitted != 1 {
		t.Fatalf("the read offers %v and omits %d, want the newest %d siblings and one "+
			"omitted", turnsOf(h.Siblings), h.SiblingsOmitted, MaxSiblings)
	}
	if slices.Contains(h.Absorbs(), siblings[0]) {
		t.Errorf("the read hands out %s, a session it told of and never offered", siblings[0])
	}
	next := newTurn()
	e.capture(launching(h, capturing("ana", "chat:room", next, 1, 1, 100, budget)))
	e.end(ending("ana", "chat:room", next, "", Ending{Outcome: OutcomeCompleted}))
	if _, found := e.session("ana", siblings[0]); !found {
		t.Fatal("the sibling no launch was offered was absorbed")
	}

	// ENDINGS: two more than a read shows, none of them with a session.
	var endings []string
	for range MaxEndings + 2 {
		run := newTurn()
		e.end(ending("ana", "chat:room", run, "", Ending{Outcome: OutcomeFailed}))
		endings = append(endings, run)
	}
	h = e.head("ana", "chat:room")
	if len(h.Endings) != MaxEndings || h.EndingsOmitted != 3 {
		t.Fatalf("the read shows %d endings and omits %d, want %d shown and three "+
			"omitted (the two oldest failures and the sibling never offered)",
			len(h.Endings), h.EndingsOmitted, MaxEndings)
	}
	if !slices.Equal(turnsOf(h.Endings), endings[2:]) {
		t.Fatalf("the read shows %v, want the newest %d failures in the order they ended",
			turnsOf(h.Endings), MaxEndings)
	}
	last := newTurn()
	e.capture(launching(h, capturing("ana", "chat:room", last, 1, 1, 100, budget)))
	e.end(ending("ana", "chat:room", last, "", Ending{Outcome: OutcomeCompleted}))
	h = e.head("ana", "chat:room")
	if want := []string{siblings[0], endings[0], endings[1]}; !slices.Equal(turnsOf(h.Endings), want) {
		t.Errorf("after the end of a turn shown the newest, the next read shows %v, want "+
			"what was left out: %v", turnsOf(h.Endings), want)
	}
}

// A SEAT'S PAGES KEEP EVERY ROW IN ITS PLACE: a page is cut by where each row
// began, which nothing moves, so a row captured again, ended or released
// between two pages is neither skipped nor repeated.
//
// Mutation: page by the row's version, as the read once did, and the row
// captured again between the pages jumps above the cursor and is never listed.
func TestASeatsPagesKeepEveryRowInItsPlace(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	var runs []string
	for range 4 {
		run := newTurn()
		e.capture(capturing("ana", "chat:room", run, 1, 1, 100, budget))
		runs = append(runs, run)
	}
	first, err := e.reader().Sessions(t.Context(), "ana", "", 2, stale)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(turnsOf(first.Sessions), []string{runs[3], runs[2]}) || first.Next == "" {
		t.Fatalf("the first page is %v (next %q), want the two latest begun",
			turnsOf(first.Sessions), first.Next)
	}
	// BETWEEN THE PAGES the oldest row is captured again and another ends.
	e.capture(capturing("ana", "chat:room", runs[0], 2, 1, 100, budget))
	e.end(ending("ana", "chat:room", runs[1], "", Ending{Outcome: OutcomeCompleted}))
	second, err := e.reader().Sessions(t.Context(), "ana", first.Next, 2, stale)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(turnsOf(second.Sessions), []string{runs[1], runs[0]}) || second.Next != "" {
		t.Errorf("the second page is %v (next %q), want the two earliest begun and no more",
			turnsOf(second.Sessions), second.Next)
	}
	if _, err := e.reader().Sessions(t.Context(), "ana", "7:"+runs[0], 2, stale); !errors.Is(err, ErrInvalid) {
		t.Errorf("a cursor no page handed out: %v, want ErrInvalid", err)
	}
}

// THE RETENTION NOMINATES ROWS, ROW-LESS TOMBSTONES AND REMOVALS, the longest
// untouched first and none touched since the cutoff: a row past it, the
// tombstone an absorbed turn left, and a removed seat's removal.
//
// Mutation: drop the tombstone query from Expired and an absorbed turn's
// tombstone lives for ever; drop the removals' and a removed seat's record
// does.
func TestTheRetentionNominatesRowsTombstonesAndRemovals(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	absorbed, head, fresh := newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", "chat:room", absorbed, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", absorbed, "", Ending{Outcome: OutcomeCompleted}))
	h := e.head("ana", "chat:room")
	e.capture(launching(h, capturing("ana", "chat:room", head, 1, 1, 100, budget)))
	e.end(ending("ana", "chat:room", head, "", Ending{Outcome: OutcomeCompleted}))
	e.removeSeat("ben")
	cutoff := stamped(e.seq)
	e.capture(capturing("ana", "chat:other", fresh, 1, 1, 100, budget))

	got, err := e.reader().Expired(t.Context(), cutoff, 10)
	if err != nil {
		t.Fatal(err)
	}
	var items []string
	for _, x := range got {
		if x.Seat() {
			items = append(items, "removal of "+x.Handle)
			continue
		}
		items = append(items, x.Turn)
	}
	want := []string{absorbed, head, "removal of ben"}
	if !slices.Equal(items, want) {
		t.Errorf("the retention nominates %v, want the absorbed turn's tombstone, the head's "+
			"row and ben's removal, oldest first", items)
	}
	if got, _ := e.reader().Expired(t.Context(), cutoff, 1); len(got) != 1 || got[0].Turn != absorbed {
		t.Errorf("a listing of one nominates %+v, want the oldest", got)
	}
}

// A LOST END IS NOMINATED ONLY FOR A TURN THAT NEVER ENDED, untouched since the
// instant asked: an ended turn and one captured since are not.
//
// Mutation: drop `end_seq IS NULL` from unendedStatement and an ended turn is
// ended again as lost.
func TestALostEndIsNominatedOnlyForATurnThatNeverEnded(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	lost, ended, fresh := newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", "chat:a", lost, 1, 1, 100, budget))
	e.capture(capturing("ana", "chat:b", ended, 1, 1, 100, budget))
	e.end(ending("ana", "chat:b", ended, "", Ending{Outcome: OutcomeCompleted}))
	before := stamped(e.seq)
	e.capture(capturing("ana", "chat:c", fresh, 1, 1, 100, budget))
	got, err := e.reader().Unended(t.Context(), before, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Turn != lost || got[0].ConversationKey != "chat:a" {
		t.Errorf("the lost ends nominated are %+v, want the one turn that never ended", got)
	}
}

// THE SEALED CENSUS COUNTS WHAT THE ROWS NAME, by the key each says sealed it:
// a session released past the seat's budget names no object, and a rotation
// waits for nothing of it.
//
// Mutation: count every row rather than the ones naming an object and a
// rotation waits on sessions that are gone.
func TestTheSealedCensusCountsWhatTheRowsName(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	const tight = 100
	older, newer := newTurn(), newTurn()
	e.capture(capturing("ana", "chat:a", older, 1, 1, 60, tight))
	e.end(ending("ana", "chat:a", older, "", Ending{Outcome: OutcomeCompleted}))
	rotated := capturing("ana", "chat:b", newer, 1, 1, 60, tight)
	rotated.ContentKey = "k2"
	// THE OLDER SESSION IS RELEASED to make room for this one.
	e.capture(rotated)
	e.capture(capturing("ben", "chat:a", newTurn(), 1, 1, 60, tight))
	got, err := e.reader().SealedKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.Values["k1"] != 1 || got.Values["k2"] != 1 || got.Outside("k2") != 1 {
		t.Errorf("the census is %v, want one session under each key and one outside k2",
			got.Values)
	}
}

// A TURN IS READ BY ITS ONE SPELLING, and a turn with no row is not found
// rather than answered empty.
//
// Mutation: drop the canonical check from Session and a braced spelling reads
// as a turn nobody holds rather than as a mistake.
func TestATurnIsReadByItsOneSpelling(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := newTurn()
	e.capture(capturing("ana", "chat:room", run, 1, 1, 100, budget))
	got, err := e.reader().Session(t.Context(), "ana", run, stale)
	if err != nil || got.Turn != run || !got.HasObject() {
		t.Fatalf("the turn read back as (%+v, %v)", got, err)
	}
	if _, err := e.reader().Session(t.Context(), "ana", newTurn(), stale); !errors.Is(err, ErrNotFound) {
		t.Errorf("a turn with no row: %v, want ErrNotFound", err)
	}
	if _, err := e.reader().Session(t.Context(), "ana", strings.ToUpper(run), stale); !errors.Is(err,
		ErrInvalid) {
		t.Errorf("an uppercase spelling of the turn: %v, want ErrInvalid", err)
	}
}

// THE SEATS HOLDING ROWS ARE LISTED IN ORDER, each once, and a seat whose rows
// are gone is not.
func TestTheSeatsHoldingRowsAreListed(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	for _, seat := range []string{"cat", "ana", "ana", "ben"} {
		e.capture(capturing(seat, "chat:room", newTurn(), 1, 1, 100, budget))
	}
	e.removeSeat("ben")
	got, err := e.reader().Handles(t.Context(), stale)
	if err != nil || !slices.Equal(got, []string{"ana", "cat"}) {
		t.Errorf("the seats holding rows are (%v, %v), want ana and cat", got, err)
	}
}
