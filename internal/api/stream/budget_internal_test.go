package stream

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// The principals the cases below hold budgets for, by the id a budget is
// keyed on.
var (
	founder        = uuid.MustParse("018f3a9c-0000-7000-8000-0000000000f1")
	secondOperator = uuid.MustParse("018f3a9c-0000-7000-8000-0000000000f2")
)

// ONE PRINCIPAL GETS ONE CHANNEL, however many sockets they hold.
//
// This is the whole of the per-principal property, asserted at the unit rather
// than only through a socket: two sockets that were handed DIFFERENT channels
// would each admit a full burst, and the socket-level case would still pass on
// a timing accident if the second tab's queries happened to arrive late.
func TestOnePrincipalGetsOneBudgetHoweverManySockets(t *testing.T) {
	t.Parallel()
	b := newBudgets()
	first, releaseFirst := b.acquire(founder)
	second, releaseSecond := b.acquire(founder)
	if first != second {
		t.Error("a principal's second socket got its own channel, so their " +
			"tabs each admit a full burst")
	}
	// And a different principal does NOT share it, which is the control:
	// one channel for everybody would pass the assertion above and make
	// one person's burst everybody else's outage.
	other, releaseOther := b.acquire(secondOperator)
	if other == first {
		t.Error("two principals share one budget")
	}
	releaseFirst()
	releaseSecond()
	releaseOther()
}

// AND AN ENTRY DOES NOT OUTLIVE THE SOCKETS THAT MADE IT.
//
// The key is a principal, so entries are created by anybody who can
// authenticate. Kept for the life of the process, that is one channel per
// person who has ever opened a tab, held alive by nothing — a leak that grows
// with the company and never shrinks, and one nothing else here would report.
func TestABudgetIsReleasedWithTheLastSocketHoldingIt(t *testing.T) {
	t.Parallel()
	b := newBudgets()
	_, releaseFirst := b.acquire(founder)
	_, releaseSecond := b.acquire(founder)
	if got := b.tracked(); got != 1 {
		t.Fatalf("tracked = %d after two sockets for one principal, want 1", got)
	}
	releaseFirst()
	// STILL HELD, because a socket is still open. Released here, the
	// remaining socket's queries would run against a budget the next
	// acquire replaces — a second full burst beside the one in flight.
	if got := b.tracked(); got != 1 {
		t.Fatalf("tracked = %d while a socket is still open, want 1", got)
	}
	releaseSecond()
	if got := b.tracked(); got != 0 {
		t.Errorf("tracked = %d after the last socket closed, want 0: an entry "+
			"per principal who has ever connected grows for the life of the "+
			"process", got)
	}
}

// A RELEASE THAT ARRIVES TWICE DOES NOT EVICT A BUDGET SOMEBODY ELSE ACQUIRED.
//
// serveSocket defers its release, and a second path unwinding through the same
// socket is exactly where one runs twice. The interleaving that costs is the
// one below: between the two calls another tab of the SAME person acquires a
// fresh entry, and a second decrement takes that live one out — after which
// the next tab builds a budget beside the queries already running under it,
// which is the per-socket allowance back by a longer route.
//
// Guarding the decrement against zero does not reach this: the entry the
// second release finds is a legitimate one with a legitimate count. Only a
// release that cannot happen twice does.
func TestAReleaseThatArrivesTwiceDoesNotEvictALiveBudget(t *testing.T) {
	t.Parallel()
	b := newBudgets()
	slots, release := b.acquire(founder)
	release()

	// The person's next tab, arriving between the two releases.
	live, releaseLive := b.acquire(founder)
	defer releaseLive()

	release() // the duplicate
	if got := b.tracked(); got != 1 {
		t.Fatalf("tracked = %d, want the live socket's budget still held", got)
	}
	// AND IT IS STILL THE SAME ONE. An entry evicted and rebuilt has the
	// right COUNT and the wrong channel, so counting alone would pass on
	// exactly the bug this is about.
	again, releaseAgain := b.acquire(founder)
	defer releaseAgain()
	if again != live {
		t.Error("the live socket's budget was evicted by a duplicate release, " +
			"so the next tab runs a second full burst beside it")
	}
	// The control: the first socket's own budget is gone, or the
	// assertions above would pass on a release that did nothing at all.
	if again == slots {
		t.Error("the first release freed nothing")
	}
}

// TWO PEOPLE ENROLLED BY ADDRESS ALONE GET TWO BUDGETS.
//
// A login is optional — an invitation redeemed without one, a person an
// administrator created with only an email — so it is EMPTY for exactly the
// people most likely to be many. The budget was keyed on it, which put every
// one of them behind one four-slot budget: the second to open a dashboard
// queued behind the first, and one person's burst was everybody's outage. The
// key is the principal's id, which a resolved principal always carries and no
// two share.
func TestTwoPeopleWithNoLoginGetTwoBudgets(t *testing.T) {
	t.Parallel()
	b := newBudgets()
	dana := iam.Principal{ID: uuid.MustParse("018f3a9c-0000-7000-8000-0000000000d1"),
		Kind: iam.KindPerson, Stage: iam.StageActive}
	eli := iam.Principal{ID: uuid.MustParse("018f3a9c-0000-7000-8000-0000000000e1"),
		Kind: iam.KindPerson, Stage: iam.StageActive}
	if dana.Login != "" || eli.Login != "" {
		t.Fatal("the case is about two people with no login")
	}
	first, releaseFirst := b.acquire(budgetKeyOf(dana))
	defer releaseFirst()
	second, releaseSecond := b.acquire(budgetKeyOf(eli))
	defer releaseSecond()
	if first == second {
		t.Error("two people with no login share one in-flight budget, so the " +
			"second to open a dashboard queues behind the first")
	}
	// THE CONTROL: the same person's second tab still shares theirs, or the
	// assertion above would pass on a key that was unique per socket.
	again, releaseAgain := b.acquire(budgetKeyOf(dana))
	defer releaseAgain()
	if again != first {
		t.Error("one person's two tabs got two budgets")
	}
}

// AND A PRINCIPAL WITH NO ID GETS A BUDGET OF ITS OWN, which is what the doc
// promised for an empty key and the map did not do: every socket that reached
// it with nothing to key on shared one entry. It is unreachable for a resolved
// principal, and if it is ever reached the sockets must bound themselves one
// at a time rather than be pooled behind four slots. Nothing enters the map,
// so there is nothing to leak.
func TestAPrincipalWithNoIDIsNotPooledWithEveryOther(t *testing.T) {
	t.Parallel()
	b := newBudgets()
	first, releaseFirst := b.acquire(uuid.Nil)
	second, releaseSecond := b.acquire(uuid.Nil)
	if first == second {
		t.Error("two sockets with no principal id share one budget")
	}
	if cap(first) != MaxInFlightQueries {
		t.Errorf("an id-less socket's budget holds %d slots, want %d",
			cap(first), MaxInFlightQueries)
	}
	if got := b.tracked(); got != 0 {
		t.Errorf("tracked = %d, want 0: an id-less budget has no key to be "+
			"released under", got)
	}
	releaseFirst()
	releaseSecond()
}
