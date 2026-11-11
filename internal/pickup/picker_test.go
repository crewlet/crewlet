package pickup_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/pickup"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/workkey"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fleet is what every node shares: the completion ledger and the claim store,
// on a clock the case moves.
type fleet struct {
	mu     sync.Mutex
	now    time.Time
	worked map[string]bool
	claims map[string]time.Time
	down   bool
	reads  int
}

func newFleet() *fleet {
	return &fleet{now: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC),
		worked: map[string]bool{}, claims: map[string]time.Time{}}
}

func (f *fleet) Worked(_ context.Context, _ string, keys []string) map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads += len(keys)
	out := map[string]bool{}
	for _, k := range keys {
		if f.worked[k] {
			out[k] = true
		}
	}
	return out
}

func (f *fleet) Claim(_ context.Context, key string, ttl time.Duration, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return false, errors.New("the coordination store did not answer")
	}
	if until, held := f.claims[key]; held && now.Before(until) {
		return false, nil
	}
	f.claims[key] = now.Add(ttl)
	return true, nil
}

func (f *fleet) Release(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claims, key)
	return nil
}

func (f *fleet) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fleet) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// turnOn records what a turn on the pickup does in the ledger: the work key of
// its one event.
func (f *fleet) turnOn(ev *events.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.worked[pickupWorkKey(ev)] = true
}

// pickupWorkKey is the work key a turn on ev alone records: its event's.
func pickupWorkKey(ev *events.Event) string { return workkey.Derive([]string{ev.ID.String()}) }

// tracker is the seat's ready work, as the read answers it, page by page.
type tracker struct {
	mu    sync.Mutex
	items []worklet.ReadyItem
}

func (t *tracker) ReadyWork(_ context.Context, q worklet.ReadyWorkQuery, _ time.Time) (worklet.ReadyWork, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	from := min(q.Page*worklet.ReadyRows, len(t.items))
	to := min(from+worklet.ReadyRows, len(t.items))
	// A WORD ON AN ITEM IS A NEW VERSION of it, which the real fingerprint
	// reads; here, its quiet instant stands for that.
	fp := ""
	for _, it := range t.items {
		fp += fmt.Sprintf("%s:%s:%d:%d:%d;", it.ID, it.Status, it.Moved, it.Progressed, it.Quiet.Unix())
	}
	return worklet.ReadyWork{Items: append([]worklet.ReadyItem(nil), t.items[from:to]...),
		Total: len(t.items), More: to < len(t.items), Fingerprint: fp}, nil
}

// inbox is what a node published, and whether it refuses.
type inbox struct {
	mu      sync.Mutex
	got     []*events.Event
	refuses bool
}

func (i *inbox) Publish(_ context.Context, topic string, ev *events.Event) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.refuses {
		return errors.New("the broker refused the publish")
	}
	if topic != topics.AgentInbox(seat) {
		return errors.New("published to " + topic + ", not the seat's inbox")
	}
	i.got = append(i.got, ev)
	return nil
}

func (i *inbox) count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.got)
}

// holder is one node's view of the seat.
type holder struct{ c pickup.Conditions }

func (h *holder) Conditions(context.Context, pickup.Seat) pickup.Conditions { return h.c }

// node is one engine's picker over the shared fleet and tracker.
func node(f *fleet, tr *tracker, in *inbox, h *holder) *pickup.Picker {
	return &pickup.Picker{Holder: h, Ready: tr, Ledger: f, Claims: f, Publisher: in,
		Now: f.clock, Cooldown: 10 * time.Minute}
}

func idleHolder() *holder { return &holder{c: idle()} }

var the = pickup.Seat{ID: seat, Handle: "engineer", Role: "Engineer"}

func pass(p *pickup.Picker) pickup.Outcome {
	return p.Pass(context.Background(), []pickup.Seat{the})[0]
}

// AN IDLE SEAT IS HANDED ITS READY ITEM ONCE.
//
// The defect, reproduced: an item assigned to an agent, ready, whose
// assignment wake the seat's turn did not act on, sat for days, because
// nothing but a change wakes a seat. With the pickup the seat, once idle for
// a full pass, is handed the item; and it is handed it once, whatever pass,
// node or restart asks again, until somebody else changes the item.
func TestAnIdleSeatIsHandedItsReadyItemOnce(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	tr := &tracker{items: []worklet.ReadyItem{item("a", worklet.StatusTodo, 40)}}
	a := node(f, tr, in, idleHolder())

	// THE SETTLE: the first pass that finds the seat idle hands it nothing.
	if out := pass(a); out.Woken || out.Skip != pickup.SkipSettling {
		t.Fatalf("the first idle pass = %+v, want it settling", out)
	}
	out := pass(a)
	if !out.Woken || in.count() != 1 || out.Item.ID != "a" {
		t.Fatalf("the second idle pass = %+v with %d published, want item a handed over",
			out, in.count())
	}

	// THE SAME NODE ASKING AGAIN while the copy waits: the pickup's claim.
	if out := pass(a); out.Woken || out.Skip != pickup.SkipClaimed {
		t.Errorf("a pass while the pickup waits = %+v, want it claimed", out)
	}

	// A SECOND NODE that believes it holds the seat, as during a move: the
	// pickup's own claim holds even past the seat's cooldown, while the
	// copy waits for its turn.
	f.advance(11 * time.Minute)
	b := node(f, tr, in, idleHolder())
	pass(b)
	if out := pass(b); out.Woken || out.Skip != pickup.SkipClaimed {
		t.Errorf("a second node's pass before the turn = %+v, want the pickup claimed", out)
	}

	// THE TURN RUNS on the pickup and the seat declines it: nothing about
	// the item changes. A restarted node, with no memory of its own, asks
	// after every claim has lapsed: the fleet's ledger refuses it.
	f.turnOn(in.got[0])
	f.advance(31 * time.Minute)
	restarted := node(f, tr, in, idleHolder())
	pass(restarted)
	if out := pass(restarted); out.Woken || out.Skip != pickup.SkipOffered {
		t.Errorf("a restarted node's pass after the turn = %+v, want it already offered", out)
	}
	if in.count() != 1 {
		t.Fatalf("%d pickups published for one item version, want 1", in.count())
	}

	// SOMEBODY ELSE CHANGES THE ITEM: it is handed over again, once.
	tr.mu.Lock()
	tr.items[0].Moved = 41
	tr.mu.Unlock()
	if out := pass(restarted); !out.Woken || in.count() != 2 {
		t.Errorf("after somebody else changed the item = %+v, want it handed over again", out)
	}
}

// A DECLINE IS SPACED BY THE COOLDOWN, WORK IS NOT: a seat that declines one
// item is handed the next, and the one after only once the cooldown lapses;
// a seat that acted on its item is handed the next at once.
func TestADeclinedItemGivesWayToTheNext(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	tr := &tracker{items: []worklet.ReadyItem{item("a", worklet.StatusInProgress, 40),
		item("b", worklet.StatusTodo, 7), item("c", worklet.StatusTodo, 9)}}
	p := node(f, tr, in, idleHolder())
	pass(p)
	if out := pass(p); out.Item.ID != "a" {
		t.Fatalf("first pickup = %q, want a", out.Item.ID)
	}
	f.turnOn(in.got[0])
	if out := pass(p); !out.Woken || out.Item.ID != "b" || out.Ready != 3 {
		t.Fatalf("after declining a = %+v, want b of 3 ready", out)
	}
	f.turnOn(in.got[1])
	if out := pass(p); out.Woken || out.Skip != pickup.SkipCooldown {
		t.Fatalf("after declining b inside the cooldown = %+v, want the cooldown", out)
	}
	f.advance(10 * time.Minute)
	if out := pass(p); !out.Woken || out.Item.ID != "c" {
		t.Errorf("after the cooldown = %+v, want c", out)
	}

	// WORK: the seat finished its item, which left its ready work, so the
	// next is handed over at once, with no cooldown taken.
	g, out := newFleet(), &inbox{}
	tr2 := &tracker{items: []worklet.ReadyItem{item("a", worklet.StatusTodo, 1),
		item("b", worklet.StatusTodo, 2), item("c", worklet.StatusTodo, 3)}}
	q := node(g, tr2, out, idleHolder())
	pass(q)
	for _, want := range []string{"a", "b", "c"} {
		got := pass(q)
		if !got.Woken || got.Item.ID != want {
			t.Fatalf("the seat that finished its last item was handed %+v, want %s at once", got, want)
		}
		g.turnOn(out.got[len(out.got)-1])
		tr2.mu.Lock()
		tr2.items = tr2.items[1:]
		tr2.mu.Unlock()
	}
}

// ITEM 21 ONWARDS IS OFFERED once every item of the first page was, and an
// unchanged set already handed over whole costs no ledger read.
func TestThePickupReadsPastTheFirstPageAndRemembersAnAllOfferedSet(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	tr := &tracker{}
	for i := range worklet.ReadyRows + 5 {
		tr.items = append(tr.items, item(fmt.Sprintf("t%02d", i), worklet.StatusTodo, uint64(i+1)))
	}
	for _, it := range tr.items[:worklet.ReadyRows] {
		f.worked[pickup.KeyOf(seat, it, f.clock()).WorkKey()] = true
	}
	p := node(f, tr, in, idleHolder())
	pass(p)
	if out := pass(p); !out.Woken || out.Item.ID != fmt.Sprintf("t%02d", worklet.ReadyRows) {
		t.Fatalf("with the first page offered = %+v, want the first item of the second", out)
	}
	for _, it := range tr.items {
		f.worked[pickup.KeyOf(seat, it, f.clock()).WorkKey()] = true
	}
	f.advance(31 * time.Minute)
	if out := pass(p); out.Skip != pickup.SkipOffered {
		t.Fatalf("with every item offered = %+v", out)
	}
	before := f.reads
	if out := pass(p); out.Skip != pickup.SkipOffered || f.reads != before {
		t.Errorf("an unchanged set handed over whole read the ledger %d more times", f.reads-before)
	}
	tr.mu.Lock()
	tr.items[3].Moved = 999
	tr.mu.Unlock()
	if out := pass(p); !out.Woken || out.Item.ID != "t03" {
		t.Errorf("after somebody changed t03 = %+v, want it handed over", out)
	}
}

// THE LEDGER IS ASKED IN ORDER AND NO FURTHER than the first item the seat
// has not had.
func TestTheLedgerIsReadOnlyUpToTheItemChosen(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	tr := &tracker{}
	for i := range 30 {
		tr.items = append(tr.items, item(fmt.Sprintf("t%02d", i), worklet.StatusTodo, uint64(i+1)))
	}
	p := node(f, tr, in, idleHolder())
	pass(p)
	if out := pass(p); !out.Woken || f.reads != 1 {
		t.Errorf("handing over the first item read the ledger %d times, want 1", f.reads)
	}
}

// A BUSY SEAT IS NOT HANDED WORK, and settles again once it is free.
func TestABusySeatSettlesBeforeItIsHandedWork(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	tr := &tracker{items: []worklet.ReadyItem{item("a", worklet.StatusTodo, 40)}}
	h := idleHolder()
	p := node(f, tr, in, h)
	pass(p)
	h.c.Dispatching = true
	if out := pass(p); out.Woken || out.Skip != pickup.SkipBusy {
		t.Fatalf("a busy seat = %+v", out)
	}
	h.c.Dispatching = false
	if out := pass(p); out.Woken || out.Skip != pickup.SkipSettling {
		t.Errorf("the first pass after a turn = %+v, want it settling", out)
	}
	if out := pass(p); !out.Woken {
		t.Errorf("the second idle pass = %+v, want a pickup", out)
	}
}

// A FAILED PUBLISH GIVES ITS CLAIMS BACK, so the next pass tries again rather
// than the seat waiting out a cooldown for a pickup nobody received; and a
// claim store that does not answer takes nothing and publishes nothing.
func TestAPickupThatCannotBeSentIsTriedAgain(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{refuses: true}
	tr := &tracker{items: []worklet.ReadyItem{item("a", worklet.StatusTodo, 40)}}
	p := node(f, tr, in, idleHolder())
	pass(p)
	if out := pass(p); out.Woken || out.Skip != pickup.SkipUnknown || out.Err == nil {
		t.Fatalf("a refused publish = %+v, want unknown with its error", out)
	}
	in.refuses = false
	if out := pass(p); !out.Woken {
		t.Fatalf("the pass after a refused publish = %+v, want the pickup sent", out)
	}

	down, quiet := newFleet(), &inbox{}
	down.down = true
	q := node(down, tr, quiet, idleHolder())
	pass(q)
	if out := pass(q); out.Woken || out.Skip != pickup.SkipUnknown || quiet.count() != 0 {
		t.Errorf("an unanswering claim store = %+v with %d sent, want nothing sent", out, quiet.count())
	}
}

// NOTHING READY IS NOTHING HANDED, and a pass that is not handed the seat
// forgets its settle.
func TestASeatWithNothingReadyIsHandedNothing(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	p := node(f, &tracker{}, in, idleHolder())
	pass(p)
	if out := pass(p); out.Woken || out.Skip != pickup.SkipNothing || !out.Settled() {
		t.Errorf("nothing ready = %+v", out)
	}
	p.Pass(context.Background(), nil)
	if out := pass(p); out.Skip != pickup.SkipSettling {
		t.Errorf("a seat handed back and held again = %+v, want it settling", out)
	}
}

// THE COOLDOWN IS A FLEET CLAIM, so the most it may be configured to is the
// longest claim the fleet holds.
func TestTheCooldownCeilingIsTheFleetsLongestClaim(t *testing.T) {
	t.Parallel()
	if got := time.Duration(config.MaxWorkPickupCooldownSeconds) * time.Second; got != coord.MaxClaimTTL {
		t.Errorf("the cooldown ceiling is %s and the longest fleet claim %s", got, coord.MaxClaimTTL)
	}
}

// THE SEAT'S OWN PROGRESS HANDS A STARTED ITEM BACK ONCE PER COOLDOWN: a new
// key for progress alone waits out the cooldown, so a turn answering every
// pickup with a write cannot be handed the item every two minutes.
//
// Mutation: drop the progress claim, and the item is handed back on the next
// pass after every write.
func TestProgressHandsAStartedItemBackOncePerCooldown(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	tr := &tracker{items: []worklet.ReadyItem{item("a", worklet.StatusInProgress, 40)}}
	p := node(f, tr, in, idleHolder())
	pass(p)
	if out := pass(p); !out.Woken {
		t.Fatalf("the started item = %+v, want it handed over", out)
	}
	f.turnOn(in.got[0])
	tr.mu.Lock()
	tr.items[0].Progressed = 55
	tr.mu.Unlock()
	if out := pass(p); out.Woken || out.Skip != pickup.SkipCooldown {
		t.Fatalf("after progress inside the cooldown = %+v, want the cooldown", out)
	}
	f.advance(10 * time.Minute)
	if out := pass(p); !out.Woken || in.count() != 2 {
		t.Errorf("after progress past the cooldown = %+v, want the item handed back", out)
	}
}

// A COOLING ITEM GIVES WAY TO THE NEXT: a started item whose hand-back waits
// out the cooldown does not keep an idle seat from its other ready work.
func TestACoolingItemGivesWayToTheNext(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	tr := &tracker{items: []worklet.ReadyItem{item("a", worklet.StatusInProgress, 40),
		item("b", worklet.StatusTodo, 41)}}
	p := node(f, tr, in, idleHolder())
	pass(p)
	if out := pass(p); !out.Woken || out.Item.ID != "a" {
		t.Fatalf("the first pass = %+v, want the started item", out)
	}
	f.turnOn(in.got[0])
	tr.mu.Lock()
	tr.items[0].Progressed = 55
	tr.mu.Unlock()
	if out := pass(p); !out.Woken || out.Item.ID != "b" {
		t.Fatalf("with the started item cooling = %+v, want the next item handed over", out)
	}
	f.turnOn(in.got[1])
	if out := pass(p); out.Woken || out.Skip != pickup.SkipCooldown {
		t.Fatalf("with nothing but a cooling item left = %+v, want the cooldown", out)
	}
	f.advance(10 * time.Minute)
	if out := pass(p); !out.Woken || out.Item.ID != "a" || in.count() != 3 {
		t.Errorf("past the cooldown = %+v, want the started item handed back", out)
	}
}

// THE SEAT'S COOLDOWN IS TAKEN ONCE A PASS, however many items the pass
// offers after passing over one the seat was handed.
func TestAPassTakesTheSeatsCooldownOnce(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	tr := &tracker{items: []worklet.ReadyItem{item("a", worklet.StatusInProgress, 40)}}
	p := node(f, tr, in, idleHolder())
	pass(p)
	if out := pass(p); !out.Woken {
		t.Fatalf("the first pass = %+v, want the started item", out)
	}
	f.turnOn(in.got[0])
	handed := item("w", worklet.StatusInProgress, 39)
	f.turnOn(pickup.Event(seat, "Engineer", handed, pickup.KeyOf(seat, handed, f.clock()), 1, false,
		events.TraceContext{}))
	tr.mu.Lock()
	tr.items[0].Progressed = 55
	tr.items = []worklet.ReadyItem{handed, tr.items[0], item("b", worklet.StatusTodo, 41)}
	tr.mu.Unlock()
	if out := pass(p); !out.Woken || out.Item.ID != "b" {
		t.Errorf("past a handed item and a cooling one = %+v, want the next item", out)
	}
}

// A REQUESTER THE ORGANISATION NO LONGER HOLDS IS WORKED FOR AS A STRANGER,
// never as a seat the turn's every decision would fail to resolve.
func TestARequesterNoLongerHeldIsWorkedForAsAStranger(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	it := item("a", worklet.StatusTodo, 40)
	it.For = &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "gone"}
	p := node(f, &tracker{items: []worklet.ReadyItem{it}}, in, idleHolder())
	p.Holds = func(handle string) bool { return handle != "gone" }
	pass(p)
	if out := pass(p); !out.Woken {
		t.Fatalf("the item = %+v, want it handed over", out)
	}
	assigned, _ := events.DataAs[*types.WorkPickup](in.got[0])
	if w := assigned.WorkingFor; w == nil || w.Kind != types.WorkingForStranger {
		t.Errorf("the pickup is worked for %+v, want a stranger", w)
	}
}

// AN UNTYPED ANSWER HOLDS AN ITEM FOR A DAY, NOT FOR GOOD (ADR-0122): an item
// handed and answered in words only is not handed again before a day has
// passed since those words, is handed once when it has, and not again that day;
// the memory of an all-offered set, which spares the ledger, expires with it;
// and a typed answer holds the item however long it then waits.
//
// Mutation: remember an all-offered set with no expiry, and the recheck is
// never handed until something else about the item changes.
func TestAnUntypedAnswerComesBackOnceADay(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	it := item("a", worklet.StatusInProgress, 40)
	it.Quiet = f.clock()
	tr := &tracker{items: []worklet.ReadyItem{it}}
	p := node(f, tr, in, idleHolder())
	pass(p)
	if out := pass(p); !out.Woken {
		t.Fatalf("the item = %+v, want it handed over", out)
	}
	f.turnOn(in.got[0])

	// THE SEAT ANSWERS IN WORDS: its comment is when the item went quiet.
	f.advance(time.Minute)
	answered := f.clock()
	tr.mu.Lock()
	tr.items[0].Quiet = answered
	tr.mu.Unlock()
	if out := pass(p); out.Woken || out.Skip != pickup.SkipOffered {
		t.Fatalf("right after the words = %+v, want the item already offered", out)
	}
	f.advance(pickup.RecheckAfter - 2*time.Second)
	before := f.reads
	if out := pass(p); out.Woken || out.Skip != pickup.SkipOffered || f.reads != before {
		t.Fatalf("a second short of a day = %+v after %d ledger reads, want it still offered "+
			"from memory", out, f.reads-before)
	}
	f.advance(2 * time.Second)
	out := pass(p)
	if !out.Woken || in.count() != 2 {
		t.Fatalf("a day after the words = %+v with %d sent, want the item handed back once", out, in.count())
	}
	if handed, _ := events.DataAs[*types.WorkPickup](in.got[1]); handed.Quiet == nil ||
		!handed.Quiet.Equal(answered) {
		t.Errorf("the recheck says the item went quiet at %v, want %s", handed.Quiet, answered)
	}
	f.turnOn(in.got[1])
	f.advance(31 * time.Minute)
	if out := pass(p); out.Woken || in.count() != 2 {
		t.Fatalf("the same day after the recheck = %+v, want nothing more", out)
	}

	// A TYPED ANSWER to the recheck holds the item for good.
	tr.mu.Lock()
	tr.items[0].Quiet = time.Time{}
	tr.mu.Unlock()
	f.advance(30 * pickup.RecheckAfter)
	if out := pass(p); out.Woken || out.Skip != pickup.SkipOffered || in.count() != 2 {
		t.Errorf("a month after a typed answer = %+v with %d sent, want the item held", out, in.count())
	}
}
