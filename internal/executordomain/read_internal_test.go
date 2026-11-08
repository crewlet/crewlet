package executordomain

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
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
	e.end(ending("ana", "chat:room", earlier, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))

	// THE PARKED TURN READS THE HEAD, captures and waits on a person.
	first := e.head("ana", "chat:room")
	if first.Session == nil || first.Session.Turn != earlier {
		t.Fatalf("the first read's head is %+v, want the earlier turn", first.Session)
	}
	e.capture(launching(first, capturing("ana", "chat:room", parked, 1, 1, 100, budget)))
	// A TURN BESIDE IT fails before its first capture.
	e.end(ending("ana", "chat:room", failed, "", executorkeep.Ending{Outcome: executorkeep.OutcomeFailed,
		Failure: "the box was lost before its first collection"}))
	// THE PARKED TURN resumes and ends after it.
	e.end(ending("ana", "chat:room", parked, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))

	h := e.head("ana", "chat:room")
	if h.Session == nil || h.Session.Turn != parked {
		t.Fatalf("the head is %+v, want the parked turn's session", h.Session)
	}
	if h.Position != e.at() {
		t.Errorf("the head was read at %s, want the position this node had applied, %s",
			h.Position, e.at())
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
	e.end(ending("ana", "chat:room", next, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	for _, gone := range []string{parked, failed} {
		if _, found := e.session("ana", gone); found {
			t.Errorf("turn %s was shown to the next turn and survived its end", gone)
		}
	}
}

// agreedHead is the head of handle's conversation as all three reads that name
// one answer it — a launch's read ([Reader.Head]), a page of the seat
// ([Page.Heads]) and a read of turn's own row ([SessionRead.Head]) — and fails
// the case where they disagree: a person would read one session as the one
// continued while the next turn continues another.
func (e *estate) agreedHead(handle, conversationKey, turn string) string {
	e.t.Helper()
	var dispatch string
	if h := e.head(handle, conversationKey); h.Session != nil {
		dispatch = h.Session.Turn
	}
	page, err := e.reader().Sessions(e.t.Context(), handle, "", MaxPageSize, stale)
	if err != nil {
		e.t.Fatalf("read %s's page: %v", handle, err)
	}
	read, err := e.reader().Session(e.t.Context(), handle, turn, stale)
	if err != nil {
		e.t.Fatalf("read %s's turn %s: %v", handle, turn, err)
	}
	if page.Heads[conversationKey] != dispatch || read.Head != dispatch {
		e.t.Fatalf("a launch's read names %q the head of %s, a page of the seat %q and a read of "+
			"turn %s %q: the API must name the head the dispatch continues", dispatch,
			conversationKey, page.Heads[conversationKey], turn, read.Head)
	}
	return dispatch
}

// A CONVERSATION'S HEAD IS THE TURN THAT READ IT LAST, never simply the one
// that ended last: of the ended turns whose rows still name a session, the
// newest READ first; among turns that read the conversation at one position,
// the newest END; and a turn that read nothing (its read failed, its
// read_through 0) after every turn that read, however late it ended. A turn
// parked on a person's question frees its seat, so a turn that read the
// conversation later runs beside it and ends first; the parked one, which read
// the conversation before that turn did and continued none of what it left,
// then ends after it — and is told and offered beside the head, never
// continued, and absorbed by the next end. At every step the launch's read, a
// page of the seat and a read of one row name the same head (agreedHead).
// TestAFireThatEndsAfterTheFiresBesideItIsToldNotContinued holds a schedule's
// line of fires beside a parked one.
//
// Mutation: order the head by its end alone, by where its turn first wrote, or
// by its read with the older end first among turns that read alike, or put a
// turn that read nothing before the turns that read, and a check goes red;
// answer a page's or a row's head in another order than a launch's read, and
// the agreement goes red.
func TestAConversationsHeadIsTheTurnThatReadItLast(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	const fires = "schedule/role/0193f0c2-6a1e-7c55-9d1a-3b6f0e2a4c11/health-check"
	earlier, parked, beside, next := newTurn(), newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", fires, earlier, 1, 1, 100, budget))
	e.end(ending("ana", fires, earlier, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))

	// THE PARKED TURN reads the head, captures and waits on a person; THE
	// TURN BESIDE IT reads after it, continues the same head and ends; and
	// the parked one is answered and ends last.
	first := e.head("ana", fires)
	e.capture(launching(first, capturing("ana", fires, parked, 1, 1, 100, budget)))
	second := e.head("ana", fires)
	if second.Position.Seq <= first.Position.Seq {
		t.Fatalf("the premise: the second read at %s is not past the first at %s",
			second.Position, first.Position)
	}
	e.capture(launching(second, capturing("ana", fires, beside, 1, 1, 100, budget)))
	e.end(ending("ana", fires, beside, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	e.end(ending("ana", fires, parked, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))

	h := e.head("ana", fires)
	if h.Session == nil || h.Session.Turn != beside {
		t.Fatalf("the head is %+v, want the turn that read the conversation last, not the one "+
			"that ended last", h.Session)
	}
	if !slices.Equal(turnsOf(h.Endings), []string{parked}) ||
		!slices.Equal(turnsOf(h.Siblings), []string{parked}) {
		t.Fatalf("the next turn is told %v and offered %v, want the parked turn told and offered "+
			"beside the head", turnsOf(h.Endings), turnsOf(h.Siblings))
	}
	e.agreedHead("ana", fires, parked)
	e.capture(launching(h, capturing("ana", fires, next, 1, 1, 100, budget)))
	e.end(ending("ana", fires, next, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	for _, gone := range []string{beside, parked} {
		if _, found := e.session("ana", gone); found {
			t.Errorf("turn %s was handed to the next turn and survived its end", gone)
		}
	}

	// TWO TURNS READ AT ONE POSITION: the one that ended last is the head.
	now := e.head("ana", fires)
	early, late := newTurn(), newTurn()
	e.capture(launching(now, capturing("ana", fires, early, 1, 1, 100, budget)))
	e.capture(launching(now, capturing("ana", fires, late, 1, 1, 100, budget)))
	e.end(ending("ana", fires, early, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	e.end(ending("ana", fires, late, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	if tie := e.agreedHead("ana", fires, early); tie != late {
		t.Fatalf("of two turns that read the conversation alike the head is %q, want %q, the one "+
			"that ended last", tie, late)
	}

	// A TURN THAT READ NOTHING ends last, and the head stays the turn that read.
	blind := newTurn()
	e.capture(capturing("ana", fires, blind, 1, 1, 100, budget))
	e.end(ending("ana", fires, blind, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	h = e.head("ana", fires)
	if h.Session == nil || h.Session.Turn != late {
		t.Fatalf("the head is %+v, want the turn that read the conversation, not one that read "+
			"nothing and ended after it", h.Session)
	}
	e.agreedHead("ana", fires, blind)
	if !slices.Contains(turnsOf(h.Siblings), blind) {
		t.Errorf("the turn that read nothing is not offered beside the head: %v", turnsOf(h.Siblings))
	}
}

// A FIRE THAT ENDS AFTER THE FIRES BESIDE IT IS TOLD, NOT CONTINUED. A fire is
// never the answer to a question, so a schedule's next fires run beside one
// parked on a person's question: F10 continues F09 and parks; F11 fires beside
// it, continues F09 too and ends; F12 continues F11 and ends; and F10,
// answered hours later, ends last of all. The schedule's line is F11 then F12,
// so F13 continues F12 — as a launch's read, a page of the seat and a read of
// F10's own row all name it (agreedHead), and F13's begin holds it as its base
// — is told how F10 ended and is offered its session, and F13's end absorbs
// both. It runs the shipped applier's and reader's own statements over a
// migrated estate.
//
// Mutation: order headStatement by end_seq alone, as the head was ordered
// before, and F13 is handed F10's session, which never saw F11 or F12: the
// case goes red.
func TestAFireThatEndsAfterTheFiresBesideItIsToldNotContinued(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	const fires = "schedule/role/0193f0c2-6a1e-7c55-9d1a-3b6f0e2a4c11/health-check"
	f09, f10, f11, f12, f13 := newTurn(), newTurn(), newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", fires, f09, 1, 1, 100, budget))
	e.end(ending("ana", fires, f09, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))

	// F10 CONTINUES F09 and parks on a person's question, its session kept.
	e.capture(launching(e.head("ana", fires), capturing("ana", fires, f10, 1, 1, 100, budget)))
	// F11 FIRES BESIDE IT, continues F09 too and ends, its end absorbing F09.
	e.capture(launching(e.head("ana", fires), capturing("ana", fires, f11, 1, 1, 100, budget)))
	e.end(ending("ana", fires, f11, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	// F12 CONTINUES F11 and ends, its end absorbing F11.
	at12 := e.head("ana", fires)
	if at12.Session == nil || at12.Session.Turn != f11 {
		t.Fatalf("the premise: F12 is handed %+v, want F11's session", at12.Session)
	}
	e.capture(launching(at12, capturing("ana", fires, f12, 1, 1, 100, budget)))
	e.end(ending("ana", fires, f12, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	// F10 IS ANSWERED and ends last of all.
	e.end(ending("ana", fires, f10, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))

	at13 := e.head("ana", fires)
	if at13.Session == nil || at13.Session.Turn != f12 {
		t.Fatalf("F13 is handed %+v, want F12's session, the newest in the schedule's line, not "+
			"F10's, which ended last", at13.Session)
	}
	if !slices.Equal(turnsOf(at13.Endings), []string{f10}) ||
		!slices.Equal(turnsOf(at13.Siblings), []string{f10}) {
		t.Fatalf("F13 is told %v and offered %v, want F10 told and offered beside F12",
			turnsOf(at13.Endings), turnsOf(at13.Siblings))
	}
	e.agreedHead("ana", fires, f10)

	// F13'S FIRST LAUNCH HOLDS F12 as the base it continues, then captures.
	e.begin(beginning(at13, "ana", fires, f13, ""))
	if !e.holds("ana", f12) {
		t.Fatal("F13's begin holds nothing, want F12's session, the head its read named")
	}
	e.capture(launching(at13, capturing("ana", fires, f13, 1, 1, 100, budget)))
	e.end(ending("ana", fires, f13, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	for _, gone := range []string{f12, f10} {
		if _, found := e.session("ana", gone); found {
			t.Errorf("fire %s was handed to F13 and survived its end", gone)
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
		e.end(ending("ana", "chat:room", run, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
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
	e.end(ending("ana", "chat:room", next, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	if _, found := e.session("ana", siblings[0]); !found {
		t.Fatal("the sibling no launch was offered was absorbed")
	}

	// ENDINGS: two more than a read shows, none of them with a session.
	var endings []string
	for range MaxEndings + 2 {
		run := newTurn()
		e.end(ending("ana", "chat:room", run, "", executorkeep.Ending{Outcome: executorkeep.OutcomeFailed}))
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
	e.end(ending("ana", "chat:room", last, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
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
	e.end(ending("ana", "chat:room", runs[1], "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
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
// Mutation: drop the tombstone query from Expired, or the tombstone arm from
// expiredIn, and an absorbed turn's tombstone lives for ever; drop the
// removals' query and a removed seat's record does.
func TestTheRetentionNominatesRowsTombstonesAndRemovals(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	absorbed, head, fresh := newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", "chat:room", absorbed, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", absorbed, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	h := e.head("ana", "chat:room")
	e.capture(launching(h, capturing("ana", "chat:room", head, 1, 1, 100, budget)))
	e.end(ending("ana", "chat:room", head, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
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
	// AND THE ROW-LESS TOMBSTONE GOES BY ITS OWN INSTANT, which is all an
	// absorbed turn has left.
	e.apply(ConversationSubject("ana", "chat:room", absorbed), OpDelete, ScopeSet{Subject: true},
		Delete{V: 1, Handle: "ana", Turn: absorbed, ConversationKey: "chat:room", Cutoff: cutoff})
	if _, tombed := e.tombed("ana", absorbed); tombed {
		t.Error("the retention of an absorbed turn left its tombstone")
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
	e.end(ending("ana", "chat:b", ended, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
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
	e.end(ending("ana", "chat:a", older, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
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
// rather than answered empty. The row is answered with where it was read: the
// checkpoint the read served, and the read's own word that nothing is missing
// from it.
//
// Mutation: drop the canonical check from Session and a braced spelling reads
// as a turn nobody holds rather than as a mistake; drop the read's position
// from the answer and the row reads as read nowhere.
func TestATurnIsReadByItsOneSpelling(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := newTurn()
	e.capture(capturing("ana", "chat:room", run, 1, 1, 100, budget))
	got, err := e.reader().Session(t.Context(), "ana", run, stale)
	if err != nil || got.Session.Turn != run || !got.Session.HasObject() {
		t.Fatalf("the turn read back as (%+v, %v)", got, err)
	}
	if got.Position != e.at() || !got.Complete {
		t.Errorf("the turn was read at %s, complete %v, want the position this node had applied, %s, "+
			"whole", got.Position, got.Complete, e.at())
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

// A ROW THAT NAMES NO SESSION SAYS WHICH OF THREE REASONS, told apart off the
// row the applier leaves: a turn that captured nothing — so far, while it is
// live, its row its begin's; at all, once it ended; one whose end took its older
// capture off because its newest was not kept, saying which capture and why;
// and one whose session the seat's budget released, whose end named an unkept
// capture that had landed first and so released nothing itself — or whose end
// said the turn went on past the session, which kept it beside a newer capture
// not kept, until the budget took it: said as the budget's, with that newer
// capture. A row that names a session says nothing.
//
// Mutation: read every released row as the budget's, ignore the place the
// unkept capture names, tell a live turn's nothing as an ended one's, or tell
// the session its end kept as one its unkept capture took off, and a case goes
// red.
func TestARowThatNamesNoSessionSaysWhichOfThreeReasons(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	nothing, begun, stale, landed, wentOn, newest := newTurn(), newTurn(), newTurn(), newTurn(), newTurn(),
		newTurn()
	e.end(ending("ana", "chat:a", nothing, "", executorkeep.Ending{Outcome: executorkeep.OutcomeFailed}))
	e.begin(beginning(e.head("ana", "chat:b"), "ana", "chat:b", begun, ""))
	e.capture(capturing("ana", "chat:c", stale, 1, 1, 10, budget))
	e.end(ending("ana", "chat:c", stale, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
		Unkept: &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Reason: "the object store's reservation is spent"}}))
	// THE BUDGET'S RELEASE: an end whose unkept capture is the very one the row
	// names releases nothing, and the next capture past the seat's budget
	// takes the session, the oldest ended one.
	const tight = 100
	e.capture(capturing("ana", "chat:d", landed, 1, 1, 60, tight))
	e.end(ending("ana", "chat:d", landed, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
		Unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 1, Reason: "its record was never answered"}}))
	if s, _ := e.session("ana", landed); !s.HasObject() {
		t.Fatalf("the end released a session whose unkept capture had landed: %+v", s)
	}
	// AN END THAT SAYS ITS TURN WENT ON keeps its session beside the newer
	// capture it names unkept — the capture before it making room by taking
	// the oldest ended session, the one above — and the next capture past the
	// budget takes it.
	e.capture(capturing("ana", "chat:f", wentOn, 1, 1, 60, tight))
	e.end(ending("ana", "chat:f", wentOn, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
		WentOn: true, Unkept: &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Reason: "the seat's budget is spent"}}))
	if s, _ := e.session("ana", wentOn); !s.HasObject() || s.Unkept() == nil {
		t.Fatalf("the premise: the end that said its turn went on is %+v, want it keeping its session "+
			"beside the newer capture not kept", s)
	}
	e.capture(capturing("ana", "chat:e", newest, 1, 1, 60, tight))

	for name, tc := range map[string]struct {
		turn string
		why  string
	}{
		"a live turn that has captured nothing": {turn: begun,
			why: "nothing of this turn's session has been captured yet"},
		"an ended turn that captured nothing": {turn: nothing,
			why: "nothing of this turn's session was captured: its run ended"},
		"a turn whose newest capture was not kept": {turn: stale,
			why: "(launch 2, capture 1) was not kept: the object store's reservation is spent"},
		"a session the seat's budget released": {turn: landed, why: "the seat's session budget released"},
		"a session its turn went on past that the seat's budget released": {turn: wentOn,
			why: "the seat's session budget released this session's object to make room for newer ones — its " +
				"turn had gone on past it, and its newer capture (launch 2, capture 1) was not kept: the " +
				"seat's budget is spent"},
		"a turn that names its session": {turn: newest},
	} {
		s, found := e.session("ana", tc.turn)
		switch {
		case !found:
			t.Errorf("%s: no row", name)
		case tc.why == "" && (!s.HasObject() || s.NotKept() != ""):
			t.Errorf("%s: %+v says %q, want a session named and nothing said", name, s, s.NotKept())
		case tc.why != "" && (s.HasObject() || !strings.Contains(s.NotKept(), tc.why)):
			t.Errorf("%s: %+v says %q, want no session and %q", name, s, s.NotKept(), tc.why)
		}
	}
}

// AN END'S UNKEPT CAPTURE STANDS ONLY WHERE IT IS NEWER THAN THE ROW'S PLACE.
// The end names the turn's newest capture as not kept where the run's record
// says so, and the apply decides on the row's own place, in log order: an
// unkept capture newer than the one the row keeps took that one off, and is
// why the row names none; one at the row's own place landed after all — its
// record, sent before the end was decided, taken before the end — and is the
// session the row keeps. [Session.Unkept] is the one reading every account of
// an end takes, so the landed row is told as kept everywhere and the released
// one says which capture was not kept, and why.
//
// Mutation: answer Session.Unkept off the ending alone, or hold it at or past
// the row's place, and the landed row tells its own session as not kept;
// answer it nil for a newer capture, and the released row no longer says why
// it names none.
func TestAnEndsUnkeptCaptureStandsOnlyWhereItIsNewerThanTheRowsPlace(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	landed, released := newTurn(), newTurn()
	unkept := executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 2, Reason: "its taker node-a stopped before it was kept"}
	e.capture(capturing("ana", "chat:a", landed, 1, 1, 100, budget))
	e.capture(capturing("ana", "chat:a", landed, 1, 2, 100, budget))
	landedUnkept := unkept
	e.end(ending("ana", "chat:a", landed, "", executorkeep.Ending{Outcome: executorkeep.OutcomeLost,
		Unkept: &landedUnkept}))
	e.capture(capturing("ana", "chat:b", released, 1, 1, 100, budget))
	releasedUnkept := unkept
	e.end(ending("ana", "chat:b", released, "", executorkeep.Ending{Outcome: executorkeep.OutcomeLost,
		Unkept: &releasedUnkept}))

	kept, found := e.session("ana", landed)
	if !found || !kept.Ended() || !kept.HasObject() || kept.CaptureSeq != unkept.CaptureSeq {
		t.Fatalf("the premise: the turn whose unkept capture landed is (%+v, %v), want it ended keeping "+
			"that capture", kept, found)
	}
	if got := kept.Unkept(); got != nil || kept.NotKept() != "" {
		t.Errorf("the row keeping the capture its end named unkept tells %+v unkept and says %q, want "+
			"neither", got, kept.NotKept())
	}
	gone, found := e.session("ana", released)
	if !found || !gone.Ended() || gone.HasObject() {
		t.Fatalf("the premise: the turn whose unkept capture never landed is (%+v, %v), want it ended "+
			"naming no session", gone, found)
	}
	switch got := gone.Unkept(); {
	case got == nil || *got != unkept:
		t.Errorf("the released row tells %+v unkept, want %+v", got, unkept)
	case !strings.Contains(gone.NotKept(), "(launch 1, capture 2) was not kept: "+unkept.Reason):
		t.Errorf("the released row says %q, want which capture was not kept and why", gone.NotKept())
	}
}
