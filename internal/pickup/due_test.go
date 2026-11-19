package pickup_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/pickup"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/worklet"
)

// due is an item in progress whose start passed with nothing of the seat's on
// it since, as the ready order marks it: after urgent work, before the rest.
func due(id string, moved uint64, start time.Time, reason string) worklet.ReadyItem {
	it := item(id, worklet.StatusInProgress, moved)
	it.Start, it.Why = &start, types.PickupDue
	it.WaitReason, it.WaitBy = reason, &provenance.Author{Kind: iam.ActorAgent, Seat: "engineer"}
	return it
}

// busyHolder is a seat a turn runs on with mail waiting behind it.
func busyHolder() *holder {
	h := idleHolder()
	h.c.Dispatching, h.c.MailWaiting, h.c.IdleBefore = true, true, false
	return h
}

// A DUE ITEM IS HANDED TO A BUSY SEAT ON ITS LANE (ADR-0162, situation 4). A
// check parked until a release's health check came due while its seat drained a
// backlog, and ran hours late: a passed start reached a seat only through the
// idle pickup. A due item is handed as urgent work is, right after it, on the
// lane, and its brief names the date and the wait's words, fenced under their
// writer; started work that is not due still waits for idle.
//
// Mutation: leave due off the lane walk, and the busy seat is handed nothing
// after its urgent item; publish it off the lane, and it queues behind the
// backlog; drop the wait from the event, and the brief cannot say what it was.
func TestADueItemIsHandedToABusySeatOnItsLane(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	start := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	const reason = "the release's health check runs then"
	tr := &tracker{items: []worklet.ReadyItem{urgent("fire", worklet.StatusTodo, 40),
		due("check", 41, start, reason), item("started", worklet.StatusInProgress, 42),
		item("later", worklet.StatusTodo, 43)}}
	tr.items[2].Why = types.PickupInProgress
	h := busyHolder()
	p := node(f, tr, in, h)

	if out := pass(p); !out.Woken || out.Item.ID != "fire" || !queue.Prioritized(in.got[0]) {
		t.Fatalf("a busy seat's first lane pickup = %+v, want the urgent item on the lane", out)
	}
	// ITS TURN FINISHES THE URGENT ITEM, and the seat is still busy.
	f.turnOn(in.got[0])
	tr.items = tr.items[1:]
	out := pass(p)
	if !out.Woken || out.Item.ID != "check" {
		t.Fatalf("a busy seat holding a due item after its urgent one = %+v, want the due item handed", out)
	}
	ev := in.got[1]
	if !queue.Prioritized(ev) {
		t.Error("the due pickup is off the priority lane, behind the seat's background mail")
	}
	handed, _ := events.DataAs[*types.WorkPickup](ev)
	if handed.Why != types.PickupDue || handed.Waited == nil || !handed.Waited.Equal(start) ||
		handed.WaitReason != reason {
		t.Fatalf("the due pickup carries why %q, waited %v, reason %q; want due, %s and %q",
			handed.Why, handed.Waited, handed.WaitReason, start, reason)
	}
	b := provenance.NewBoundary()
	brief := handed.FencedBrief(b)
	for _, want := range []string{"set aside until a date that has passed", "2026-11-08T06:00:00Z",
		"`wait_until` and `wait_reason`"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the due pickup's brief lacks %q:\n%s", want, brief)
		}
	}
	fences := provenance.Fences(brief, b)
	if len(fences) != 1 || !strings.Contains(fences[0], reason) ||
		!strings.Contains(fences[0], "authors=agent:engineer") {
		t.Errorf("the wait's words are not fenced once under the seat that set them: %q\n%s", fences, brief)
	}

	// STARTED WORK THAT IS NOT DUE waits for the seat to be idle.
	f.turnOn(ev)
	tr.items = tr.items[1:]
	if out := pass(p); out.Woken || out.Skip != pickup.SkipBusy {
		t.Fatalf("a busy seat with only started work left = %+v, want it busy", out)
	}
	h.c.Dispatching, h.c.MailWaiting = false, false
	pass(p)
	if out := pass(p); !out.Woken || out.Item.ID != "started" || queue.Prioritized(in.got[2]) {
		t.Errorf("the idle seat's next pickup = %+v, want its started item off the lane", out)
	}
}

// A LANE PICKUP WAITS FOR AN EMPTY LANE (ADR-0162). The lane is how a person's
// message reaches a busy seat first, so an urgent or due item goes on it only
// while nothing waits there: at most one engine pickup does, and none is
// claimed meanwhile, so the pass after the lane empties hands it. A seat whose
// inbox was read empty has an empty lane, which is not read again.
//
// Mutation: publish without asking the lane, and the urgent item queues beside
// the person's message; take the item's claim while the lane waits, and the
// pass after it empties finds the item claimed.
func TestALanePickupWaitsForAnEmptyLane(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	start := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	tr := &tracker{items: []worklet.ReadyItem{urgent("fire", worklet.StatusTodo, 40),
		due("check", 41, start, "the health check")}}
	h := busyHolder()
	p := node(f, tr, in, h)

	// A PERSON'S MESSAGE WAITS ON THE LANE.
	h.lane = true
	if out := pass(p); out.Woken || out.Skip != pickup.SkipLaneWaiting || in.count() != 0 {
		t.Fatalf("a pass while the lane holds a delivery = %+v with %d published, want it waiting",
			out, in.count())
	}
	h.lane = false
	if out := pass(p); !out.Woken || out.Item.ID != "fire" {
		t.Fatalf("the pass after the lane emptied = %+v, want the urgent item handed", out)
	}

	// THE PICKUP ITSELF WAITS THERE NOW, until the seat's next turn takes it.
	h.lane = true
	if out := pass(p); out.Woken || out.Skip != pickup.SkipLaneWaiting {
		t.Fatalf("a pass while the urgent pickup waits on the lane = %+v, want the due item held", out)
	}
	f.turnOn(in.got[0])
	h.lane = false
	if out := pass(p); !out.Woken || out.Item.ID != "check" || in.count() != 2 {
		t.Fatalf("the pass after the seat took its urgent pickup = %+v, want the due item handed", out)
	}

	// A LANE THAT CANNOT BE READ is not empty: nothing is published.
	f2, in2 := newFleet(), &inbox{}
	broken := busyHolder()
	broken.laneErr = errors.New("the broker did not answer")
	if out := pass(node(f2, &tracker{items: []worklet.ReadyItem{urgent("fire", worklet.StatusTodo, 40)}},
		in2, broken)); out.Woken || out.Skip != pickup.SkipUnknown || out.Err == nil || in2.count() != 0 {
		t.Errorf("a pass whose lane read failed = %+v with %d published, want nothing sent", out, in2.count())
	}

	// A SEAT WITH AN EMPTY INBOX, idle or settling, has an empty lane by
	// the read that found it so, and its lane is not read again.
	f3, in3 := newFleet(), &inbox{}
	quiet := idleHolder()
	if out := pass(node(f3, &tracker{items: []worklet.ReadyItem{urgent("fire", worklet.StatusTodo, 40)}},
		in3, quiet)); !out.Woken || quiet.laneAsked != 0 {
		t.Errorf("a settling seat's urgent pickup = %+v after %d lane reads, want it handed with none",
			out, quiet.laneAsked)
	}
}
