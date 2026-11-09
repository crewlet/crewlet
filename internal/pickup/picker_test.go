package pickup_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/pickup"
	"github.com/crewlet/crewlet/internal/queue/topics"
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
}

func newFleet() *fleet {
	return &fleet{now: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC),
		worked: map[string]bool{}, claims: map[string]time.Time{}}
}

func (f *fleet) Worked(_ context.Context, _ string, keys []string) map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func pickupWorkKey(ev *events.Event) string {
	assigned, _ := events.DataAs[*types.TaskAssigned](ev)
	return pickup.Key{Seat: seat, Item: assigned.Pickup.Item, Moved: assigned.Pickup.Version,
		Status: assigned.Pickup.Status}.WorkKey()
}

// tracker is the seat's ready work, as the read answers it.
type tracker struct {
	mu    sync.Mutex
	items []worklet.ReadyItem
}

func (t *tracker) ReadyWork(context.Context, worklet.ReadyWorkQuery, time.Time) (worklet.ReadyWork, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return worklet.ReadyWork{Items: append([]worklet.ReadyItem(nil), t.items...)}, nil
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

	// THE SAME NODE ASKING AGAIN, inside the cooldown.
	if out := pass(a); out.Woken || out.Skip != pickup.SkipCooldown {
		t.Errorf("a pass inside the cooldown = %+v, want the cooldown", out)
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

// A SEAT THAT DECLINES ONE ITEM IS HANDED THE NEXT, one per cooldown.
func TestADeclinedItemGivesWayToTheNext(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	tr := &tracker{items: []worklet.ReadyItem{item("a", worklet.StatusInProgress, 40),
		item("b", worklet.StatusTodo, 7)}}
	p := node(f, tr, in, idleHolder())
	pass(p)
	if out := pass(p); out.Item.ID != "a" {
		t.Fatalf("first pickup = %q, want a", out.Item.ID)
	}
	f.turnOn(in.got[0])
	if out := pass(p); out.Skip != pickup.SkipCooldown {
		t.Fatalf("inside the cooldown = %+v", out)
	}
	f.advance(10 * time.Minute)
	if out := pass(p); !out.Woken || out.Item.ID != "b" || out.Ready != 2 {
		t.Errorf("after the cooldown = %+v, want b of 2 ready", out)
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
