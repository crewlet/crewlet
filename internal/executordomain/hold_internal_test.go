package executordomain

import (
	"maps"
	"slices"
	"testing"
)

// THE HOLD: a turn's begin names the base its first read froze, and while the
// turn's row is live and has landed no capture, no release takes that base.
// These cases apply the records through the shipped applier in the orders a
// fleet's log can hold them, and read the rows back.

// A BEGIN HOLDS THE BASE ITS READ NAMED, AND ITS FIRST READ STANDS: the turn's
// row records where it read its conversation and what that read handed out,
// names the head as its base and holds it; a second begin of the turn — a
// retry, or another read after a newer turn ended — writes nothing; and a
// begin no read hands out (a base outside what the read handed out, the turn
// as its own base, no read, no conversation, a session no reader can parse)
// writes nothing at all.
//
// Mutation: drop the `read_through > 0` refusal from applyBegin and the second
// begin replaces the turn's first read and its base; drop baseShaped and a
// begin naming a base its read never handed out holds it.
func TestABeginHoldsTheBaseItsReadNamed(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	base, newer, turn := newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", "chat:room", base, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", base, "", Ending{Outcome: OutcomeCompleted}))
	h := e.head("ana", "chat:room")
	if h.Session == nil || h.Session.Turn != base {
		t.Fatalf("the premise: the head is %+v, want the base", h.Session)
	}
	if _, n := e.begin(beginning(h, "ana", "chat:room", turn, "work-1")); n == 0 {
		t.Fatal("the begin wrote nothing")
	}
	s, found := e.session("ana", turn)
	if !found || s.Ended() || s.HasObject() || s.LaunchSeq != 0 || s.BaseTurn != base ||
		s.ReadThrough.Packed() != h.Position.Packed() || !slices.Equal(s.Absorbs, h.Absorbs()) || s.WorkKey != "work-1" {
		t.Fatalf("the begun turn's row is (%+v, %v), want a live row naming no session, with its "+
			"first read, its work and the base", s, found)
	}
	if !e.holds("ana", base) {
		t.Fatal("the base the begin named is not held")
	}

	// ANOTHER READ, after a newer turn ended: the first read stands.
	e.capture(capturing("ana", "chat:room", newer, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", newer, "", Ending{Outcome: OutcomeCompleted}))
	later := e.head("ana", "chat:room")
	if _, n := e.begin(beginning(later, "ana", "chat:room", turn, "work-1")); n != 0 {
		t.Errorf("a second begin of the turn wrote %d rows", n)
	}
	if s, _ := e.session("ana", turn); s.BaseTurn != base || s.ReadThrough.Packed() != h.Position.Packed() {
		t.Errorf("after a second begin the row names base %s read at %s, want the first read's %s "+
			"at %s", s.BaseTurn, s.ReadThrough, base, h.Position)
	}
	if e.holds("ana", newer) {
		t.Error("the second begin's base is held")
	}

	for name, spoil := range map[string]func(*Begin){
		"a base the read did not hand out": func(b *Begin) {
			b.Absorbs = slices.DeleteFunc(slices.Clone(b.Absorbs), func(x string) bool { return x == b.BaseTurn })
		},
		"the turn as its own base":           func(b *Begin) { b.BaseTurn = b.Turn },
		"no read":                            func(b *Begin) { b.ReadThrough = 0 },
		"no conversation":                    func(b *Begin) { b.ConversationKey = "" },
		"a base session no reader can parse": func(b *Begin) { b.BaseObject = "session-7" },
	} {
		fresh := newTurn()
		b := beginning(later, "ana", "chat:room", fresh, "")
		spoil(&b)
		if _, n := e.begin(b); n != 0 {
			t.Errorf("a begin with %s wrote %d rows", name, n)
		}
		if _, found := e.session("ana", fresh); found {
			t.Errorf("a begin with %s left a row", name)
		}
	}
}

// A BEGIN AND AN END OF ITS BASE ARE ORDERED BY THE LOG. Two turns read the
// same head; the other one landed a capture. Where its end lands first, it
// absorbs the head, and the begin landing after finds no base: it records its
// read and holds nothing. Where the begin lands first, the end skips the head
// the begin holds, and the head stays, held.
//
// Mutation: drop the hold from absorb's delete and the end absorbs the head
// under its holder; write the base without asking baseStands and the late
// begin names a turn whose row is gone.
func TestABeginAndAnEndOfItsBaseAreOrderedByTheLog(t *testing.T) {
	t.Parallel()
	for name, beginFirst := range map[string]bool{"the end first": false, "the begin first": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEstate(t)
			head, sibling, holder := newTurn(), newTurn(), newTurn()
			e.capture(capturing("ana", "chat:room", head, 1, 1, 100, budget))
			e.end(ending("ana", "chat:room", head, "", Ending{Outcome: OutcomeCompleted}))
			h := e.head("ana", "chat:room")
			e.capture(launching(h, capturing("ana", "chat:room", sibling, 1, 1, 100, budget)))
			begin := beginning(h, "ana", "chat:room", holder, "")
			if beginFirst {
				e.begin(begin)
			}
			e.end(ending("ana", "chat:room", sibling, "", Ending{Outcome: OutcomeCompleted}))
			if !beginFirst {
				e.begin(begin)
			}
			row, found := e.session("ana", holder)
			if !found || row.ReadThrough.Packed() != h.Position.Packed() {
				t.Fatalf("the holder's row is (%+v, %v), want its read recorded whichever landed first",
					row, found)
			}
			_, kept := e.session("ana", head)
			switch {
			case beginFirst && (!kept || row.BaseTurn != head || !e.holds("ana", head)):
				t.Errorf("the begin landed first: the head kept %v, the holder names %q, held %v; "+
					"want the head kept, named and held", kept, row.BaseTurn, e.holds("ana", head))
			case !beginFirst && (kept || row.BaseTurn != ""):
				t.Errorf("the end landed first: the head kept %v and the holder names %q; want the "+
					"head absorbed and nothing named", kept, row.BaseTurn)
			}
		})
	}
}

// NO RELEASE TAKES A HELD BASE: the seat budget's, on a capture of another
// conversation; an end's absorption; an end's dedupe on the base's unit of
// work, which still merges what the base delivered; the retention's delete;
// and a lost end of a turn handed the same head. Each case holds the base by a
// begin, runs one release that would take it, and finds it named still.
//
// Mutation: drop the hold from any one path — seatPlan's skip, dropUnheld in
// the absorption or the dedupe, expiredIn's — and that case goes red.
func TestNoReleaseTakesAHeldBase(t *testing.T) {
	t.Parallel()
	// holding is an estate whose seat ana has an ended base of size bytes
	// in chat:room, ended with work and delivery as given, which a live
	// turn's begin holds; and the head that turn read.
	holding := func(t *testing.T, size, seatBudget int64, work string, delivered bool) (*estate, string, Head) {
		t.Helper()
		e := newEstate(t)
		base := newTurn()
		e.capture(capturing("ana", "chat:room", base, 1, 1, size, seatBudget))
		e.end(ending("ana", "chat:room", base, work, Ending{Outcome: OutcomeCompleted,
			Delivered: delivered, DeliveredTo: "chatlet"}))
		h := e.head("ana", "chat:room")
		e.begin(beginning(h, "ana", "chat:room", newTurn(), ""))
		if !e.holds("ana", base) {
			t.Fatal("the premise: the base is not held")
		}
		return e, base, h
	}
	named := func(t *testing.T, e *estate, base, path string) {
		t.Helper()
		if s, found := e.session("ana", base); !found || !s.HasObject() {
			t.Errorf("%s took the held base: its row is (%+v, %v)", path, s, found)
		}
	}

	t.Run("the seat budget, on a capture of another conversation", func(t *testing.T) {
		t.Parallel()
		const tight = 100
		e, base, _ := holding(t, 40, tight, "", false)
		other, next := newTurn(), newTurn()
		e.capture(capturing("ana", "chat:other", other, 1, 1, 40, tight))
		e.end(ending("ana", "chat:other", other, "", Ending{Outcome: OutcomeCompleted}))
		// ROOM FOR IT IS ONE RELEASE AWAY, and the oldest ended session is the
		// held base: the release passes it over.
		if _, n := e.capture(capturing("ana", "chat:third", next, 1, 1, 30, tight)); n == 0 {
			t.Fatal("a capture the budget holds once the unheld session goes wrote nothing")
		}
		named(t, e, base, "the budget's release")
		if s, _ := e.session("ana", other); s.HasObject() {
			t.Error("the unheld ended session was not released in the held base's place")
		}
		// ROOM ONLY THE HELD BASE COULD MAKE: the capture is refused whole.
		if _, n := e.capture(capturing("ana", "chat:fourth", newTurn(), 1, 1, 60, tight)); n != 0 {
			t.Errorf("a capture only the held base's release could fit wrote %d rows", n)
		}
		named(t, e, base, "a refused capture's release")
	})

	t.Run("an end's absorption", func(t *testing.T) {
		t.Parallel()
		e, base, h := holding(t, 100, budget, "", false)
		sibling := newTurn()
		e.capture(launching(h, capturing("ana", "chat:room", sibling, 1, 1, 100, budget)))
		e.end(ending("ana", "chat:room", sibling, "", Ending{Outcome: OutcomeCompleted}))
		named(t, e, base, "an end that was handed it")
	})

	t.Run("an end's dedupe", func(t *testing.T) {
		t.Parallel()
		e, base, _ := holding(t, 100, budget, "work-1", true)
		retry := newTurn()
		e.capture(capturing("ana", "chat:room", retry, 1, 1, 100, budget))
		e.end(ending("ana", "chat:room", retry, "work-1", Ending{Outcome: OutcomeCompleted}))
		named(t, e, base, "an end of the same unit of work")
		if s, _ := e.session("ana", retry); !s.Ended() || !s.Ending.Delivered {
			t.Error("the retry's row lost what the held attempt delivered")
		}
	})

	t.Run("the retention's delete", func(t *testing.T) {
		t.Parallel()
		e, base, _ := holding(t, 100, budget, "", false)
		e.apply(ConversationSubject("ana", "chat:room", base), OpDelete, ScopeSet{Subject: true},
			Delete{V: 1, Handle: "ana", Turn: base, ConversationKey: "chat:room", Cutoff: stamped(e.seq)})
		named(t, e, base, "a delete past its cutoff")
	})

	t.Run("a lost end", func(t *testing.T) {
		t.Parallel()
		e, base, h := holding(t, 100, budget, "", false)
		lost := newTurn()
		e.capture(launching(h, capturing("ana", "chat:room", lost, 1, 1, 100, budget)))
		e.end(End{V: 1, Handle: "ana", Turn: lost, ConversationKey: "chat:room",
			Ending: Ending{Outcome: OutcomeLost, Failure: lostFailure}})
		named(t, e, base, "the lost end of a turn that was handed it")
	})
}

// THE HOLD LIFTS AT THE HOLDER'S FIRST CAPTURE, which releases its own base
// where the budget needs the room — and at the seat budget's floor it must. A
// seat given one sealed session at its bound holds a base at that bound; while
// the base is held, another turn's capture as large is refused; the holder's
// own first capture as large releases the base it held, since its own session
// now holds everything the base did, and lands.
//
// Mutation: hold a base against its own holder's capture (no exception in
// seatPlan's read of the holds) and the continued turn's first capture is
// refused at the floor.
func TestTheHoldLiftsAtTheHoldersFirstCapture(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	// THE FLOOR as internal/config states it (config.SessionBytesPerSeatFloor,
	// held to SealedMaxBytes by TestTheSeatBudgetFloorHoldsOneSessionAtItsBound):
	// one sealed session at its bound, rounded up to a whole mebibyte.
	const mib = 1 << 20
	floor := (SealedMaxBytes + mib - 1) / mib * mib
	base, holder, intruder := newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", "chat:room", base, 1, 1, SealedMaxBytes, floor))
	e.end(ending("ana", "chat:room", base, "", Ending{Outcome: OutcomeCompleted}))
	e.begin(beginning(e.head("ana", "chat:room"), "ana", "chat:room", holder, ""))

	if _, n := e.capture(capturing("ana", "chat:other", intruder, 1, 1, SealedMaxBytes, floor)); n != 0 {
		t.Fatalf("another turn's capture took the room of the held base (%d rows)", n)
	}
	if _, n := e.capture(capturing("ana", "chat:room", holder, 1, 1, SealedMaxBytes, floor)); n == 0 {
		t.Fatal("the holder's first capture was refused at the floor")
	}
	if s, _ := e.session("ana", holder); !s.HasObject() || s.LaunchSeq != 1 || s.BaseTurn != base {
		t.Errorf("the holder's row is %+v, want its own session beside the base it was handed", s)
	}
	if s, _ := e.session("ana", base); s.HasObject() || !s.Ended() {
		t.Errorf("the base is %+v, want its session released and its ending kept", s)
	}
	if e.holds("ana", base) {
		t.Error("the base is still held after its holder's first capture")
	}
}

// THE HOLD LIFTS WHEN THE HOLDER ENDS, by its own end or by the lost end the
// retention duty records once it nominates the holder, and the base stays the
// conversation's head: a holder that never captured absorbs nothing, so the
// next turn continues the session the holder never got to.
//
// Mutation: end a begun row with insertEndingStatement, as a row-less turn is
// ended, and the holder's row stays live, holding the base for good.
func TestTheHoldLiftsWhenTheHolderEnds(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(t *testing.T, e *estate, holder string){
		"its own end": func(_ *testing.T, e *estate, holder string) {
			e.end(ending("ana", "chat:room", holder, "", Ending{Outcome: OutcomeFailed,
				Failure: "the box was lost before its first collection"}))
		},
		"a lost end": func(t *testing.T, e *estate, holder string) {
			lost, err := e.reader().Unended(t.Context(), stamped(e.seq), 10)
			if err != nil || len(lost) != 1 || lost[0].Turn != holder {
				t.Fatalf("the turns whose end may be lost are (%+v, %v), want the holder", lost, err)
			}
			e.end(End{V: 1, Handle: "ana", Turn: holder, ConversationKey: "chat:room",
				Ending: Ending{Outcome: OutcomeLost, Failure: lostFailure}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEstate(t)
			base, holder := newTurn(), newTurn()
			e.capture(capturing("ana", "chat:room", base, 1, 1, 100, budget))
			e.end(ending("ana", "chat:room", base, "", Ending{Outcome: OutcomeCompleted}))
			e.begin(beginning(e.head("ana", "chat:room"), "ana", "chat:room", holder, ""))
			if !e.holds("ana", base) {
				t.Fatal("the premise: the base is not held")
			}
			end(t, e, holder)
			if e.holds("ana", base) {
				t.Error("the base is still held after its holder ended")
			}
			if s, _ := e.session("ana", holder); !s.Ended() || s.HasObject() {
				t.Errorf("the holder's row is %+v, want it ended naming no session", s)
			}
			if h := e.head("ana", "chat:room"); h.Session == nil || h.Session.Turn != base {
				t.Errorf("the conversation's head is %+v after its holder ended, want the base", h.Session)
			}
		})
	}
}

// A TURN THAT NEVER CAPTURED ABSORBS AND DEDUPES NOTHING, though its begin
// recorded what its read handed out: it holds no session that could stand in
// for the head it never continued, the ending it was told or the older attempt
// at its work, as a turn with no row at all does not.
//
// Mutation: absorb wherever the ending turn has a row, as the rule was before a
// begin could write one, and the head, the ending and the older attempt go
// with a turn that never ran.
func TestATurnThatNeverCapturedAbsorbsNothing(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	older, failed, head, turn := newTurn(), newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", "chat:room", older, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", older, "work-1", Ending{Outcome: OutcomeFailed}))
	e.end(ending("ana", "chat:room", failed, "", Ending{Outcome: OutcomeFailed}))
	e.capture(capturing("ana", "chat:room", head, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", head, "", Ending{Outcome: OutcomeCompleted}))
	h := e.head("ana", "chat:room")
	if !slices.Contains(h.Absorbs(), failed) || !slices.Contains(h.Absorbs(), older) {
		t.Fatalf("the premise: the read hands out %v, want the head, the ending and the attempt",
			h.Absorbs())
	}
	e.begin(beginning(h, "ana", "chat:room", turn, "work-1"))
	e.end(ending("ana", "chat:room", turn, "work-1", Ending{Outcome: OutcomeFailed,
		Failure: "the box was lost before its first collection"}))
	for name, kept := range map[string]string{
		"the head": head, "the ending it was told": failed, "the older attempt at its work": older,
	} {
		if _, found := e.session("ana", kept); !found {
			t.Errorf("%s went with the end of a turn that never captured", name)
		}
	}
	if s, _ := e.session("ana", turn); !s.Ended() || s.HasObject() {
		t.Errorf("the turn's row is %+v, want it ended naming no session", s)
	}
}

// THE RETENTION LISTS NO HELD ROW: a base a live turn holds is past the cutoff
// and the oldest row there is, and a listing of one nominates the row behind
// it rather than the held one — every pass, so a hold never stalls the duty —
// until the hold lifts.
//
// Mutation: drop the holds from Expired and the held base fills the page of
// one on every pass.
func TestTheRetentionListsNoHeldRow(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	base, behind, holder := newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", "chat:room", base, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", base, "", Ending{Outcome: OutcomeCompleted}))
	e.capture(capturing("ana", "chat:other", behind, 1, 1, 100, budget))
	e.end(ending("ana", "chat:other", behind, "", Ending{Outcome: OutcomeCompleted}))
	cutoff := stamped(e.seq)
	e.begin(beginning(e.head("ana", "chat:room"), "ana", "chat:room", holder, ""))

	got, err := e.reader().Expired(t.Context(), cutoff, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Turn != behind {
		t.Errorf("a listing of one nominates %+v, want the row behind the held base", got)
	}
	e.end(ending("ana", "chat:room", holder, "", Ending{Outcome: OutcomeFailed}))
	got, err = e.reader().Expired(t.Context(), cutoff, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Turn != base {
		t.Errorf("once its holder ended a listing of one nominates %+v, want the base, the oldest", got)
	}
}

// A PAGE AND A SINGLE READ NAME EACH CONVERSATION'S HEAD FROM THEIR OWN
// SNAPSHOT: every conversation a page's rows name is mapped to its head —
// whether the head's own row is on the page or not — or to nothing where it
// has none; a row with no conversation names none; a single row's read names
// its conversation's head beside it; and a page read after a newer head ended
// names that one.
//
// Mutation: take a conversation's head from the rows in hand rather than a
// seek of its own, and a conversation whose head began before the page's first
// row reads as having none.
func TestAPageNamesEachConversationsHead(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	head, failed, keyless, live := newTurn(), newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", "chat:a", head, 1, 1, 100, budget))
	e.end(ending("ana", "chat:a", head, "", Ending{Outcome: OutcomeCompleted}))
	e.end(ending("ana", "chat:b", failed, "", Ending{Outcome: OutcomeFailed}))
	e.capture(capturing("ana", "", keyless, 1, 1, 100, budget))
	e.capture(capturing("ana", "chat:a", live, 1, 1, 100, budget))

	first, err := e.reader().Sessions(t.Context(), "ana", "", 1, stale)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Sessions) != 1 || first.Sessions[0].Turn != live ||
		!maps.Equal(first.Heads, map[string]string{"chat:a": head}) {
		t.Errorf("a page of the live turn alone names heads %v, want chat:a's head, off the page",
			first.Heads)
	}
	whole, err := e.reader().Sessions(t.Context(), "ana", "", 10, stale)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"chat:a": head, "chat:b": ""}; !maps.Equal(whole.Heads, want) {
		t.Errorf("the whole seat's page names heads %v, want %v", whole.Heads, want)
	}
	for turn, want := range map[string]string{live: head, keyless: "", failed: ""} {
		read, readErr := e.reader().Session(t.Context(), "ana", turn, stale)
		if readErr != nil || read.Head != want {
			t.Errorf("turn %s's read names head %q (%v), want %q", turn, read.Head, readErr, want)
		}
	}

	e.end(ending("ana", "chat:a", live, "", Ending{Outcome: OutcomeCompleted}))
	again, err := e.reader().Sessions(t.Context(), "ana", "", 10, stale)
	if err != nil {
		t.Fatal(err)
	}
	if again.Heads["chat:a"] != live {
		t.Errorf("a page read after the live turn ended names chat:a's head %q, want the turn that "+
			"just ended", again.Heads["chat:a"])
	}
}
