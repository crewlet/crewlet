package coordtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// BrowserControlStore is a lease store and the browser controls it keeps: one
// store, two surfaces, as [RunNodeStatusRecords] pairs a lease store with its
// records. A control is a lease, so the cases hold the two to each other.
type BrowserControlStore interface {
	coord.Backend
	coord.BrowserControls
}

// RunBrowserControls is the contract suite every [coord.BrowserControls] must
// pass. open returns at least one handle on one EMPTY store, for [Run]'s
// reason, and a replicated store's on different members, for [RunShared]'s: a
// case of one handle uses the first, and the shared cases spread their calls
// across them all. Lapsing is the harness's, as in [Run].
func RunBrowserControls(t *testing.T, open func(t *testing.T) []BrowserControlStore) {
	t.Helper()
	for _, c := range browserControlCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var handles []BrowserControlStore
			h := newHarness(t, func(t *testing.T) coord.Backend {
				if handles = open(t); len(handles) == 0 {
					t.Fatal("RunBrowserControls: open returned no handle")
				}
				return handles[0]
			})
			if live := h.listLive(coord.ClassBrowserControl); len(live) != 0 {
				t.Fatalf("open must return an empty store, got %d live browser control(s): %v",
					len(live), resources(live))
			}
			c.fn(&controlHarness{harness: h, handles: handles})
		})
	}
}

// controlHarness is a lease harness over a store's first handle, and every
// handle open returned.
type controlHarness struct {
	*harness
	handles []BrowserControlStore
}

type controlCase struct {
	name string
	fn   func(h *controlHarness)
}

// aLaunch is one executor launch a case takes the browser of.
type aLaunch struct{ launch, turn, seat uuid.UUID }

func newLaunch() aLaunch { return aLaunch{uuid.New(), uuid.New(), uuid.New()} }

// take is p taking the launch's browser.
func (l aLaunch) take(p aPerson) coord.BrowserControlTake {
	return coord.BrowserControlTake{Launch: l.launch, Turn: l.turn, Seat: l.seat, Owner: p.owner, Holder: p.holder}
}

// aPerson is one person taking a browser: who owns the take, and how they are
// shown to everybody watching.
type aPerson struct {
	owner  coord.BrowserControlOwner
	holder coord.BrowserControlHolder
}

func newPerson(name string) aPerson {
	return aPerson{
		owner: coord.BrowserControlOwner{Person: uuid.New(), Credential: iam.SessionName(uuid.NewString()),
			Take: uuid.New()},
		holder: coord.BrowserControlHolder{Seat: uuid.New(), Name: name},
	}
}

// retake is the same person in the same session under a new take gesture.
func (p aPerson) retake() aPerson {
	p.owner.Take = uuid.New()
	return p
}

// elsewhere is the same person, under the same take id, in another session.
func (p aPerson) elsewhere() aPerson {
	p.owner.Credential = iam.SessionName(uuid.NewString())
	return p
}

// handle is the case's i-th handle, wrapping round the ones open returned.
func (h *controlHarness) handle(i int) BrowserControlStore { return h.handles[i%len(h.handles)] }

// takeOn takes t's launch through b, failing the case on an error.
func (h *controlHarness) takeOn(b BrowserControlStore, t coord.BrowserControlTake,
	ttl time.Duration) (coord.BrowserControl, coord.BrowserControlTaken) {

	h.t.Helper()
	h.lastClaimAt, h.lastClaimTTL, h.travelled = time.Now(), ttl, false
	control, taken, err := b.TakeBrowserControl(h.ctx, t, ttl)
	switch {
	case err != nil:
		h.t.Fatalf("TakeBrowserControl(%s by %s): %v", t.Launch, t.Owner.Key(), err)
	case !taken.Valid():
		h.t.Fatalf("TakeBrowserControl(%s by %s) answered %q, which is no outcome", t.Launch,
			t.Owner.Key(), taken)
	case control.Launch != t.Launch:
		h.t.Fatalf("TakeBrowserControl(%s) answered launch %s's control", t.Launch, control.Launch)
	}
	return control, taken
}

// taken takes t's launch through the first handle and fails the case unless a
// new tenure began for t's owner, which it answers.
func (h *controlHarness) taken(t coord.BrowserControlTake, ttl time.Duration) coord.BrowserControlTenure {
	h.t.Helper()
	control, taken := h.takeOn(h.handle(0), t, ttl)
	held, ok := control.Held()
	switch {
	case taken != coord.ControlTaken:
		h.t.Fatalf("a take by %s answered %q, want %q", t.Owner.Key(), taken, coord.ControlTaken)
	case !ok || held.Owner != t.Owner || held.Holder != t.Holder || held.Epoch < 1:
		h.t.Fatalf("a take by %s answered %+v as the tenure it began", t.Owner.Key(), control.Current)
	case held.TakenAt.IsZero() || held.TakenAt.Location() != time.UTC:
		h.t.Fatalf("a take's tenure began at %v, want an instant the store stamped, in UTC", held.TakenAt)
	}
	return held
}

// read reads launch's record through b, failing the case unless it has one.
func (h *controlHarness) read(b BrowserControlStore, launch uuid.UUID) coord.BrowserControl {
	h.t.Helper()
	control, found, err := b.BrowserControl(h.ctx, launch)
	switch {
	case err != nil:
		h.t.Fatalf("BrowserControl(%s): %v", launch, err)
	case !found || control.Launch != launch:
		h.t.Fatalf("BrowserControl(%s) = (%+v, %v), want its record", launch, control, found)
	}
	return control
}

// renewOn renews through b, failing the case on an error.
func (h *controlHarness) renewOn(b BrowserControlStore, launch uuid.UUID, owner coord.BrowserControlOwner,
	epoch int64, ttl time.Duration) (coord.BrowserControl, bool) {

	h.t.Helper()
	control, renewed, err := b.RenewBrowserControl(h.ctx, launch, owner, epoch, ttl)
	if err != nil {
		h.t.Fatalf("RenewBrowserControl(%s by %s at %d): %v", launch, owner.Key(), epoch, err)
	}
	return control, renewed
}

// endOn ends through b, failing the case on an error.
func (h *controlHarness) endOn(b BrowserControlStore, launch uuid.UUID, owner coord.BrowserControlOwner,
	epoch int64, by coord.BrowserControlEnd) (coord.BrowserControl, bool) {

	h.t.Helper()
	control, ended, err := b.EndBrowserControl(h.ctx, launch, owner, epoch, by)
	if err != nil {
		h.t.Fatalf("EndBrowserControl(%s by %s at %d as %s): %v", launch, owner.Key(), epoch, by, err)
	}
	return control, ended
}

// ended ends a live tenure through the first handle, failing the case unless
// it ended.
func (h *controlHarness) ended(l aLaunch, p aPerson, epoch int64, by coord.BrowserControlEnd) {
	h.t.Helper()
	if _, ended := h.endOn(h.handle(0), l.launch, p.owner, epoch, by); !ended {
		h.t.Fatalf("the owner's end of epoch %d as %s was refused", epoch, by)
	}
}

// requireEnded fails the case unless control has epoch ended as by, at an
// instant the store stamped, in UTC; it answers the tenure.
func (h *controlHarness) requireEnded(what string, control coord.BrowserControl, epoch int64,
	by coord.BrowserControlEnd) coord.BrowserControlTenure {

	h.t.Helper()
	t, ok := control.Of(epoch)
	switch {
	case !ok:
		h.t.Fatalf("%s: epoch %d is not on the record, which holds %+v and carries %+v", what, epoch,
			control.Current, control.Earlier)
	case t.EndedBy != by:
		h.t.Fatalf("%s: epoch %d ended as %q, want %q", what, epoch, t.EndedBy, by)
	case t.EndedAt.IsZero() || t.EndedAt.Location() != time.UTC:
		h.t.Fatalf("%s: epoch %d ended at %v, want an instant the store stamped, in UTC", what, epoch, t.EndedAt)
	}
	return t
}

// requireHeldBy fails the case unless control is held by p at epoch.
func (h *controlHarness) requireHeldBy(what string, control coord.BrowserControl, p aPerson, epoch int64) {
	h.t.Helper()
	held, ok := control.Held()
	if !ok || held.Owner != p.owner || held.Holder != p.holder || held.Epoch != epoch {
		h.t.Fatalf("%s: held by %+v, want %s at epoch %d", what, control.Current, p.owner.Key(), epoch)
	}
}

// takeUntilDefinite takes t's launch through b, asking again on an unknown as
// a person's panel does: another owner's claim still minting, or a stampede
// outrunning a compare-and-swap's rounds, is answered unknown, and asked again
// once it settles. The case's own deadline bounds it.
func (h *controlHarness) takeUntilDefinite(b BrowserControlStore,
	t coord.BrowserControlTake) (coord.BrowserControl, coord.BrowserControlTaken, error) {

	for {
		control, taken, err := b.TakeBrowserControl(h.ctx, t, LongTTL)
		if err == nil || !errors.Is(err, coord.ErrUnavailable) || h.ctx.Err() != nil {
			return control, taken, err
		}
		time.Sleep(time.Millisecond) //fixedwait:liveness
	}
}

// race has racers take one launch at once across the case's handles, each
// until it is answered definitely, and holds the answers to one tenure:
// exactly one taken, every other held naming its owner, holder and epoch.
func (h *controlHarness) race(handles int) {
	h.t.Helper()
	const racers = 16
	type answer struct {
		p       aPerson
		control coord.BrowserControl
		taken   coord.BrowserControlTaken
		err     error
	}
	l := newLaunch()
	answers := make([]answer, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range racers {
		answers[i].p = newPerson(fmt.Sprintf("Racer %d", i))
		b := h.handle(i % handles)
		wg.Go(func() {
			<-start
			answers[i].control, answers[i].taken, answers[i].err = h.takeUntilDefinite(b, l.take(answers[i].p))
		})
	}
	close(start)
	h.await(&wg, fmt.Sprintf("%d people taking one browser at once", racers))

	winner := -1
	for i, a := range answers {
		switch {
		case a.err != nil:
			h.t.Fatalf("racer %d was never answered definitely: %v", i, a.err)
		case a.taken == coord.ControlTaken && winner >= 0:
			h.t.Fatalf("racers %d and %d both took one browser", winner, i)
		case a.taken == coord.ControlTaken:
			winner = i
		case a.taken != coord.ControlHeld:
			h.t.Fatalf("racer %d was answered %q, want %q or %q", i, a.taken, coord.ControlTaken, coord.ControlHeld)
		}
	}
	if winner < 0 {
		h.t.Fatalf("%d racers took one browser and none was answered %q", racers, coord.ControlTaken)
	}
	won, _ := answers[winner].control.Held()
	for i, a := range answers {
		if i != winner {
			h.requireHeldBy(fmt.Sprintf("racer %d's answer", i), a.control, answers[winner].p, won.Epoch)
		}
	}
	h.requireHeldBy("the record after the race", h.read(h.handle(0), l.launch), answers[winner].p, won.Epoch)
}

// watchControls starts a watch of b's browser controls that ends with the case.
func (h *controlHarness) watchControls(b BrowserControlStore) <-chan coord.BrowserControlWrite {
	h.t.Helper()
	ctx, cancel := context.WithCancel(h.ctx)
	h.t.Cleanup(cancel)
	writes, err := b.WatchBrowserControls(ctx)
	if err != nil {
		h.t.Fatalf("WatchBrowserControls: %v", err)
	}
	return writes
}

// requireControlWrite fails the case unless the watch's next write is want.
func (h *controlHarness) requireControlWrite(writes <-chan coord.BrowserControlWrite, what string,
	want coord.BrowserControlWrite) {

	h.t.Helper()
	select {
	case got, open := <-writes:
		switch {
		case !open:
			h.t.Fatalf("the watch closed before %s was heard", what)
		case got != want:
			h.t.Fatalf("%s was heard as %+v, want %+v", what, got, want)
		}
	case <-time.After(watchBound): //fixedwait:liveness
		h.t.Fatalf("%s was not heard within %v", what, watchBound)
	}
}

// nameOf is a holder's name of n characters: ASCII, a letter of two bytes and
// U+2028, which encoding/json writes as a six-byte escape.
func nameOf(n int) string {
	return string([]rune(strings.Repeat("Ada é ", n))[:n])
}

// wrongOwners are the owners and epochs a renewal or an end of p's tenure at
// epoch must be refused for: another person, another session, another take,
// and p at another epoch.
func wrongOwners(p aPerson, epoch int64) []struct {
	what  string
	owner coord.BrowserControlOwner
	epoch int64
} {
	return []struct {
		what  string
		owner coord.BrowserControlOwner
		epoch int64
	}{
		{"another person", newPerson("Grace Hopper").owner, epoch},
		{"the owner in another session", p.elsewhere().owner, epoch},
		{"the owner under another take", p.retake().owner, epoch},
		{"the owner at another epoch", p.owner, epoch + 1},
	}
}

// browserControlCases certify [coord.BrowserControls]: that a control is one
// owner's at a time, fenced by an epoch from the class's counter, judged on
// the store's clock, and that how each tenure ended reads back.
var browserControlCases = []controlCase{
	{"one_racer_takes_a_browser_and_the_other_is_told_who_holds_it", func(h *controlHarness) {
		h.race(1)
	}},

	{"a_retried_take_by_its_owner_is_the_same_tenure", func(h *controlHarness) {
		// A retried PUT is one take: the panel's retry after a lost answer
		// keeps the epoch its first attempt began, and its start.
		l, ada := newLaunch(), newPerson("Ada Lovelace")
		first := h.taken(l.take(ada), LongTTL)
		control, taken := h.takeOn(h.handle(0), l.take(ada), LongTTL)
		again, _ := control.Held()
		switch {
		case taken != coord.ControlKept:
			h.t.Fatalf("the owner's retried take answered %q, want %q", taken, coord.ControlKept)
		case again.Epoch != first.Epoch || again.Owner != ada.owner:
			h.t.Fatalf("the retried take holds %+v, want the first tenure at epoch %d", control.Current, first.Epoch)
		case !again.TakenAt.Equal(first.TakenAt):
			h.t.Fatalf("the retried take began at %v, want the first tenure's %v", again.TakenAt, first.TakenAt)
		}
	}},

	{"the_same_person_under_another_take_or_session_is_held_against_them", func(h *controlHarness) {
		// TWO TABS ARE TWO OWNERS: one cookie, two takes. Answered kept, the
		// second tab would hold the first's epoch and its input be answered
		// from the record of the first's.
		l, ada := newLaunch(), newPerson("Ada Lovelace")
		first := h.taken(l.take(ada), LongTTL)
		for what, other := range map[string]aPerson{
			"another take in the same session": ada.retake(),
			"the same take in another session": ada.elsewhere(),
		} {
			control, taken := h.takeOn(h.handle(0), l.take(other), LongTTL)
			if taken != coord.ControlHeld {
				h.t.Fatalf("%s answered %q, want %q", what, taken, coord.ControlHeld)
			}
			h.requireHeldBy(what, control, ada, first.Epoch)
		}
	}},

	{"a_lapsed_control_is_taken_over_at_a_higher_epoch_and_reads_lapsed", func(h *controlHarness) {
		l, ada, grace := newLaunch(), newPerson("Ada Lovelace"), newPerson("Grace Hopper")
		first := h.taken(l.take(ada), ShortTTL)
		h.lapse()
		second := h.taken(l.take(grace), LongTTL)
		if second.Epoch <= first.Epoch {
			h.t.Fatalf("the takeover holds epoch %d, not past the lapsed tenure's %d", second.Epoch, first.Epoch)
		}
		lapsed := h.requireEnded("the lapsed tenure", h.read(h.handle(0), l.launch), first.Epoch, coord.ControlLapsed)
		if lapsed.Owner != ada.owner || lapsed.Holder != ada.holder {
			h.t.Fatalf("the lapsed tenure reads as %+v, want Ada's", lapsed)
		}
	}},

	{"only_the_owner_renews_and_only_at_its_epoch", func(h *controlHarness) {
		l, ada := newLaunch(), newPerson("Ada Lovelace")
		resource := coord.BrowserControlResource(l.launch)
		first := h.taken(l.take(ada), WindowTTL)
		before := h.mustHold(resource, ada.owner.Key())
		for _, wrong := range wrongOwners(ada, first.Epoch) {
			control, renewed := h.renewOn(h.handle(0), l.launch, wrong.owner, wrong.epoch, LongTTL)
			if renewed {
				h.t.Fatalf("a renewal by %s was granted", wrong.what)
			}
			h.requireHeldBy("a refused renewal's answer", control, ada, first.Epoch)
			h.stillLive(resource)
			h.requireUnchanged("a renewal by "+wrong.what, before, h.get(resource))
		}
		control, renewed := h.renewOn(h.handle(0), l.launch, ada.owner, first.Epoch, LongTTL)
		if !renewed {
			h.t.Fatal("the owner's renewal of its live tenure was refused")
		}
		h.requireHeldBy("the owner's renewal, as written", control, ada, first.Epoch)
		h.lapsePast(WindowTTL)
		h.requireHeldBy("the renewed tenure past its first TTL", h.read(h.handle(0), l.launch), ada, first.Epoch)
	}},

	{"a_lapsed_tenure_cannot_be_renewed_and_says_it_lapsed", func(h *controlHarness) {
		// A renewal across a lapse would re-cover a gap nothing held: only a
		// take, at a new epoch, comes back from one.
		l, ada := newLaunch(), newPerson("Ada Lovelace")
		first := h.taken(l.take(ada), ShortTTL)
		h.lapse()
		control, renewed := h.renewOn(h.handle(0), l.launch, ada.owner, first.Epoch, LongTTL)
		if renewed {
			h.t.Fatal("a lapsed tenure was renewed")
		}
		h.requireEnded("a refused renewal's answer", control, first.Epoch, coord.ControlLapsed)
		h.mustBeUnheld(coord.BrowserControlResource(l.launch))
	}},

	{"only_the_owner_ends_its_tenure_and_the_end_keeps_its_cause", func(h *controlHarness) {
		// A hand-back, since a tombstone that names no cause reads as released.
		l, ada := newLaunch(), newPerson("Ada Lovelace")
		resource := coord.BrowserControlResource(l.launch)
		first := h.taken(l.take(ada), LongTTL)
		before := h.mustHold(resource, ada.owner.Key())
		for _, wrong := range wrongOwners(ada, first.Epoch) {
			control, ended := h.endOn(h.handle(0), l.launch, wrong.owner, wrong.epoch, coord.ControlHandedBack)
			if ended {
				h.t.Fatalf("an end by %s was granted", wrong.what)
			}
			h.requireHeldBy("a refused end's answer", control, ada, first.Epoch)
			h.requireUnchanged("an end by "+wrong.what, before, h.get(resource))
		}
		control, ended := h.endOn(h.handle(0), l.launch, ada.owner, first.Epoch, coord.ControlHandedBack)
		if !ended {
			h.t.Fatal("the owner's end of its live tenure was refused")
		}
		answered := h.requireEnded("the owner's end, as written", control, first.Epoch, coord.ControlHandedBack)
		read := h.requireEnded("the owner's end, read back", h.read(h.handle(0), l.launch), first.Epoch,
			coord.ControlHandedBack)
		if !read.EndedAt.Equal(answered.EndedAt) {
			h.t.Fatalf("the end was answered at %v and reads back at %v", answered.EndedAt, read.EndedAt)
		}
		h.mustBeUnheld(resource)
		control, ended = h.endOn(h.handle(0), l.launch, ada.owner, first.Epoch, coord.ControlReleased)
		if ended {
			h.t.Fatal("an ended tenure was ended again")
		}
		h.requireEnded("a second end's answer", control, first.Epoch, coord.ControlHandedBack)
	}},

	{"every_written_cause_reads_back_and_a_lapse_is_never_written", func(h *controlHarness) {
		for _, by := range coord.BrowserControlEnds {
			if !by.Written() {
				continue
			}
			l, ada := newLaunch(), newPerson("Ada Lovelace")
			first := h.taken(l.take(ada), LongTTL)
			h.ended(l, ada, first.Epoch, by)
			h.requireEnded("an end written as "+string(by), h.read(h.handle(0), l.launch), first.Epoch, by)
		}
		// A LAPSE IS THE STORE'S TO READ: written by a node, it would say a
		// tenure ran out that the store still holds.
		l, ada := newLaunch(), newPerson("Ada Lovelace")
		first := h.taken(l.take(ada), LongTTL)
		for _, by := range []coord.BrowserControlEnd{coord.ControlLapsed, "expired", ""} {
			_, ended, err := h.handle(0).EndBrowserControl(h.ctx, l.launch, ada.owner, first.Epoch, by)
			if err == nil || ended || errors.Is(err, coord.ErrUnavailable) {
				h.t.Fatalf("an end as %q answered (%v, %v), want an error naming a caller's mistake", by, ended, err)
			}
			h.requireHeldBy("the tenure after an end as "+string(by), h.read(h.handle(0), l.launch), ada, first.Epoch)
		}
	}},

	{"an_end_stays_readable_after_a_ttl_and_through_four_later_tenures", func(h *controlHarness) {
		type end struct {
			p     aPerson
			epoch int64
			by    coord.BrowserControlEnd
		}
		l := newLaunch()
		var ends []end
		endAs := func(name string, by coord.BrowserControlEnd, ttl time.Duration) {
			p := newPerson(name)
			t := h.taken(l.take(p), ttl)
			h.ended(l, p, t.Epoch, by)
			ends = append(ends, end{p, t.Epoch, by})
		}
		endAs("One", coord.ControlReleased, LongTTL)
		endAs("Two", coord.ControlHandedBack, LongTTL)
		three := newPerson("Three")
		lapsed := h.taken(l.take(three), ShortTTL)
		h.lapse()
		ends = append(ends, end{three, lapsed.Epoch, coord.ControlLapsed})
		endAs("Four", coord.ControlIdle, LongTTL)
		endAs("Five", coord.ControlRevoked, WindowTTL)
		// AFTER A TTL the newest end is still the record's own.
		h.lapsePast(WindowTTL)
		h.requireEnded("the newest end, a TTL later", h.read(h.handle(0), l.launch), ends[4].epoch,
			coord.ControlRevoked)

		six := newPerson("Six")
		live := h.taken(l.take(six), LongTTL)
		control := h.read(h.handle(0), l.launch)
		h.requireHeldBy("the sixth take", control, six, live.Epoch)
		if len(control.Earlier) != coord.MaxBrowserControlEnds {
			h.t.Fatalf("the record carries %d ends, want the %d newest", len(control.Earlier),
				coord.MaxBrowserControlEnds)
		}
		for i, got := range control.Earlier {
			want := ends[len(ends)-1-i]
			if got.Epoch != want.epoch || got.Owner != want.p.owner {
				h.t.Fatalf("carried end %d is epoch %d of %s, want %d of %s, newest first", i, got.Epoch,
					got.Owner.Key(), want.epoch, want.p.owner.Key())
			}
			h.requireEnded(fmt.Sprintf("carried end %d", i), control, want.epoch, want.by)
		}
		if t, ok := control.Of(ends[0].epoch); ok {
			h.t.Fatalf("the fifth end back is still carried: %+v", t)
		}
	}},

	{"every_browser_control_mints_from_its_class_counter", func(h *controlHarness) {
		// A COUNTER PER LAUNCH would leave a record per launch for ever, and
		// start every launch's tokens at 1.
		first, second := newLaunch(), newLaunch()
		var highest int64
		for i := range 3 {
			p := newPerson(fmt.Sprintf("Person %d", i))
			t := h.taken(first.take(p), LongTTL)
			h.ended(first, p, t.Epoch, coord.ControlReleased)
			highest = max(highest, t.Epoch)
		}
		next := h.taken(second.take(newPerson("Grace Hopper")), LongTTL)
		if next.Epoch <= highest {
			h.t.Fatalf("a second launch's first take holds epoch %d, not past the first launch's %d", next.Epoch,
				highest)
		}
	}},

	{"a_write_to_a_control_wakes_the_watch_with_its_launch", func(h *controlHarness) {
		writes := h.watchControls(h.handle(0))
		h.claim(coord.SeatResource(seatID("ceo")), coord.AcquireOptions{Owner: "node-a/1", TTL: LongTTL})
		l, ada := newLaunch(), newPerson("Ada Lovelace")
		first := h.taken(l.take(ada), LongTTL)
		write := coord.BrowserControlWrite{Launch: l.launch, Epoch: first.Epoch}
		h.requireControlWrite(writes, "the take", write)
		if _, renewed := h.renewOn(h.handle(0), l.launch, ada.owner, first.Epoch, LongTTL); !renewed {
			h.t.Fatal("the owner's renewal was refused")
		}
		h.requireControlWrite(writes, "the renewal", write)
		h.ended(l, ada, first.Epoch, coord.ControlHandedBack)
		h.requireControlWrite(writes, "the end", write)
	}},

	{"a_control_keeps_its_launch_turn_seat_owner_and_holder_whole", func(h *controlHarness) {
		l := newLaunch()
		ada := newPerson(nameOf(coord.MaxBrowserControlHolderName))
		first := h.taken(l.take(ada), LongTTL)
		control := h.read(h.handle(0), l.launch)
		if control.Turn != l.turn || control.Seat != l.seat {
			h.t.Fatalf("the record names turn %s and seat %s, want %s and %s", control.Turn, control.Seat,
				l.turn, l.seat)
		}
		h.requireHeldBy("the live tenure, read back", control, ada, first.Epoch)
		h.ended(l, ada, first.Epoch, coord.ControlHandedBack)
		h.taken(l.take(newPerson("Grace Hopper")), LongTTL)
		carried, ok := h.read(h.handle(0), l.launch).Of(first.Epoch)
		if !ok || carried.Owner != ada.owner || carried.Holder != ada.holder {
			h.t.Fatalf("the carried end reads as %+v, want Ada's whole", carried)
		}
	}},

	{"a_take_that_breaks_the_records_rules_writes_nothing", func(h *controlHarness) {
		l, ada := newLaunch(), newPerson("Ada Lovelace")
		for _, bad := range []struct {
			what string
			edit func(*coord.BrowserControlTake)
			ttl  time.Duration
		}{
			{"a nil launch", func(t *coord.BrowserControlTake) { t.Launch = uuid.Nil }, LongTTL},
			{"a nil turn", func(t *coord.BrowserControlTake) { t.Turn = uuid.Nil }, LongTTL},
			{"a nil seat", func(t *coord.BrowserControlTake) { t.Seat = uuid.Nil }, LongTTL},
			{"a nil person", func(t *coord.BrowserControlTake) { t.Owner.Person = uuid.Nil }, LongTTL},
			{"a nil take", func(t *coord.BrowserControlTake) { t.Owner.Take = uuid.Nil }, LongTTL},
			{"a machine token", func(t *coord.BrowserControlTake) {
				t.Owner.Credential = iam.MachineTokenName(uuid.NewString())
			}, LongTTL},
			{"no credential", func(t *coord.BrowserControlTake) { t.Owner.Credential = "" }, LongTTL},
			{"a nil holder seat", func(t *coord.BrowserControlTake) { t.Holder.Seat = uuid.Nil }, LongTTL},
			{"no name", func(t *coord.BrowserControlTake) { t.Holder.Name = "" }, LongTTL},
			{"a name of 201 characters", func(t *coord.BrowserControlTake) {
				t.Holder.Name = nameOf(coord.MaxBrowserControlHolderName + 1)
			}, LongTTL},
			{"a name holding a C0 control", func(t *coord.BrowserControlTake) { t.Holder.Name = "Ada\aLovelace" },
				LongTTL},
			{"a name holding a C1 control", func(t *coord.BrowserControlTake) {
				t.Holder.Name = "Ada\u0085Lovelace"
			}, LongTTL},
			{"a name that is not UTF-8", func(t *coord.BrowserControlTake) { t.Holder.Name = "Ada \xff" }, LongTTL},
			{"a TTL of zero", nil, 0},
			{"a TTL past coord.MaxDutyTTL", nil, coord.MaxDutyTTL + time.Second},
		} {
			take := l.take(ada)
			if bad.edit != nil {
				bad.edit(&take)
			}
			_, taken, err := h.handle(0).TakeBrowserControl(h.ctx, take, bad.ttl)
			switch {
			case err == nil:
				h.t.Fatalf("a take with %s was answered %q, want an error", bad.what, taken)
			case errors.Is(err, coord.ErrUnavailable):
				h.t.Fatalf("a take with %s answered %v: a caller's mistake read as an outage", bad.what, err)
			}
			if live := h.listLive(coord.ClassBrowserControl); len(live) != 0 {
				h.t.Fatalf("a take with %s left %v held", bad.what, resources(live))
			}
			if _, found, err := h.handle(0).BrowserControl(h.ctx, l.launch); err != nil || found {
				h.t.Fatalf("a take with %s left a record: (%v, %v)", bad.what, found, err)
			}
		}
	}},

	{"a_renewal_end_or_read_that_breaks_the_records_rules_is_an_error_and_writes_nothing", func(h *controlHarness) {
		// A CALLER'S MISTAKE IS AN ERROR, NEVER "NOT LIVE FOR THAT OWNER": sent
		// on, a renewal at epoch 0 or by a machine token finds no such tenure
		// and is answered a definite false, which its caller reads as control
		// lost rather than as its own bug.
		l, ada := newLaunch(), newPerson("Ada Lovelace")
		resource := coord.BrowserControlResource(l.launch)
		first := h.taken(l.take(ada), LongTTL)
		before := h.mustHold(resource, ada.owner.Key())
		mistaken := func(what string, answered bool, err error) {
			h.t.Helper()
			switch {
			case err == nil || answered:
				h.t.Fatalf("%s answered (%v, %v), want an error", what, answered, err)
			case errors.Is(err, coord.ErrUnavailable):
				h.t.Fatalf("%s answered %v: a caller's mistake read as an outage", what, err)
			}
			h.requireUnchanged(what, before, h.get(resource))
		}
		owner := func(edit func(*coord.BrowserControlOwner)) coord.BrowserControlOwner {
			o := ada.owner
			edit(&o)
			return o
		}
		for _, bad := range []struct {
			what   string
			launch uuid.UUID
			owner  coord.BrowserControlOwner
			epoch  int64
		}{
			{"a nil launch", uuid.Nil, ada.owner, first.Epoch},
			{"epoch 0", l.launch, ada.owner, 0},
			{"a nil person", l.launch, owner(func(o *coord.BrowserControlOwner) { o.Person = uuid.Nil }), first.Epoch},
			{"a nil take", l.launch, owner(func(o *coord.BrowserControlOwner) { o.Take = uuid.Nil }), first.Epoch},
			{"a machine token", l.launch, owner(func(o *coord.BrowserControlOwner) {
				o.Credential = iam.MachineTokenName(uuid.NewString())
			}), first.Epoch},
			{"no credential", l.launch, owner(func(o *coord.BrowserControlOwner) { o.Credential = "" }), first.Epoch},
		} {
			_, renewed, err := h.handle(0).RenewBrowserControl(h.ctx, bad.launch, bad.owner, bad.epoch, LongTTL)
			mistaken("a renewal with "+bad.what, renewed, err)
			_, ended, err := h.handle(0).EndBrowserControl(h.ctx, bad.launch, bad.owner, bad.epoch,
				coord.ControlReleased)
			mistaken("an end with "+bad.what, ended, err)
		}
		for _, ttl := range []time.Duration{0, coord.MaxDutyTTL + time.Second} {
			_, renewed, err := h.handle(0).RenewBrowserControl(h.ctx, l.launch, ada.owner, first.Epoch, ttl)
			mistaken(fmt.Sprintf("a renewal for %v", ttl), renewed, err)
		}
		_, found, err := h.handle(0).BrowserControl(h.ctx, uuid.Nil)
		mistaken("a read of a nil launch", found, err)
		h.requireHeldBy("the tenure after every mistaken call", h.read(h.handle(0), l.launch), ada, first.Epoch)
	}},

	{"a_control_is_a_lease_the_lease_surface_agrees_with", func(h *controlHarness) {
		l, ada := newLaunch(), newPerson("Ada Lovelace")
		resource := coord.BrowserControlResource(l.launch)
		first := h.taken(l.take(ada), LongTTL)
		lease := h.mustHold(resource, ada.owner.Key())
		if lease.Epoch != first.Epoch {
			h.t.Fatalf("the lease is at epoch %d and the control at %d", lease.Epoch, first.Epoch)
		}
		h.requireResources("the live browser controls", h.listLive(coord.ClassBrowserControl), resource)
		if owner, err := coord.Holder(h.ctx, h.b, resource); err != nil || owner != ada.owner.Key() {
			h.t.Fatalf("coord.Holder = (%q, %v), want %q", owner, err, ada.owner.Key())
		}
		left, err := coord.TimeLeft(h.ctx, h.b, coord.ClassBrowserControl)
		if err != nil {
			h.t.Fatalf("coord.TimeLeft(%s): %v", coord.ClassBrowserControl, err)
		}
		if d := left(*lease); d <= 0 || d > LongTTL {
			h.t.Fatalf("a control taken for %v has %v left on the store's clock", LongTTL, d)
		}
		h.ended(l, ada, first.Epoch, coord.ControlReleased)
		h.requireResources("the live browser controls after the end", h.listLive(coord.ClassBrowserControl))
		if owner, err := coord.Holder(h.ctx, h.b, resource); err != nil || owner != "" {
			h.t.Fatalf("coord.Holder after the end = (%q, %v), want nobody", owner, err)
		}
	}},

	{"a_take_on_one_handle_is_held_on_another_and_its_end_reads_whole_there", func(h *controlHarness) {
		// SHARED: each verb through another handle, which on a replicated
		// store is another member.
		l, ada, grace := newLaunch(), newPerson("Ada Lovelace"), newPerson("Grace Hopper")
		control, taken := h.takeOn(h.handle(0), l.take(ada), LongTTL)
		first, _ := control.Held()
		if taken != coord.ControlTaken {
			h.t.Fatalf("the first take answered %q", taken)
		}
		control, taken = h.takeOn(h.handle(1), l.take(grace), LongTTL)
		if taken != coord.ControlHeld {
			h.t.Fatalf("a take through another handle answered %q, want %q", taken, coord.ControlHeld)
		}
		h.requireHeldBy("a take through another handle", control, ada, first.Epoch)
		h.requireHeldBy("a read through a third", h.read(h.handle(2), l.launch), ada, first.Epoch)
		if _, ended := h.endOn(h.handle(1), l.launch, ada.owner, first.Epoch, coord.ControlHandedBack); !ended {
			h.t.Fatal("the owner's end through another handle was refused")
		}
		read := h.read(h.handle(2), l.launch)
		h.requireEnded("the end, read through a third handle", read, first.Epoch, coord.ControlHandedBack)
		if _, held := read.Held(); held {
			h.t.Fatalf("an ended control reads as held: %+v", read.Current)
		}
	}},

	{"two_handles_racing_one_launch_never_both_hold_it", func(h *controlHarness) {
		h.race(len(h.handles))
	}},

	{"every_verb_answers_a_store_it_cannot_reach_as_unknown", func(h *controlHarness) {
		// A context that has ended stands in for a store the call could not
		// reach. No verb may answer definitely: false, nothing found and no
		// write are facts about who holds a browser, which it does not know.
		l, ada := newLaunch(), newPerson("Ada Lovelace")
		first := h.taken(l.take(ada), LongTTL)
		ctx, cancel := context.WithCancel(h.ctx)
		cancel()
		b := h.handle(0)
		for _, v := range []struct {
			name string
			call func() (bool, error)
		}{
			{"TakeBrowserControl", func() (bool, error) {
				_, taken, err := b.TakeBrowserControl(ctx, l.take(ada), LongTTL)
				return taken != "", err
			}},
			{"RenewBrowserControl", func() (bool, error) {
				_, renewed, err := b.RenewBrowserControl(ctx, l.launch, ada.owner, first.Epoch, LongTTL)
				return renewed, err
			}},
			{"EndBrowserControl", func() (bool, error) {
				_, ended, err := b.EndBrowserControl(ctx, l.launch, ada.owner, first.Epoch, coord.ControlReleased)
				return ended, err
			}},
			{"BrowserControl", func() (bool, error) {
				_, found, err := b.BrowserControl(ctx, l.launch)
				return found, err
			}},
			{"WatchBrowserControls", func() (bool, error) {
				writes, err := b.WatchBrowserControls(ctx)
				return writes != nil, err
			}},
		} {
			answered, err := v.call()
			if err == nil || !errors.Is(err, coord.ErrUnavailable) || answered {
				h.t.Fatalf("%s on a store it could not reach answered (%v, %v), want only an error wrapping "+
					"coord.ErrUnavailable", v.name, answered, err)
			}
		}
		h.requireHeldBy("the tenure after every verb failed", h.read(b, l.launch), ada, first.Epoch)
	}},
}
