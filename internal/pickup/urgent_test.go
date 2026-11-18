package pickup_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/pickup"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/worklet"
)

// urgent is a ready item marked urgent, first in the order as orderReady puts it.
func urgent(id string, status worklet.Status, moved uint64) worklet.ReadyItem {
	it := item(id, status, moved)
	it.Priority, it.Why = worklet.PriorityUrgent, types.PickupUrgent
	return it
}

// URGENT WORK DOES NOT WAIT FOR A BACKLOG (ADR-0159). A seat carrying hundreds
// of queued wakes runs a turn for each, in arrival order, and is never idle
// with an empty inbox, so its urgent item waited behind all of them. An urgent
// item is handed to a seat that is busy or has mail waiting, on its priority
// lane, so its next turn takes it; anything else still waits for the seat to
// be idle.
//
// Mutation: hand nothing to a seat that is not idle, and the urgent item waits
// for the backlog; publish it off the priority lane, and it queues behind it.
func TestAnUrgentItemIsHandedToABusySeatOnItsPriorityLane(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	tr := &tracker{items: []worklet.ReadyItem{urgent("fire", worklet.StatusTodo, 40),
		item("later", worklet.StatusTodo, 41)}}
	h := idleHolder()
	h.c.Dispatching, h.c.MailWaiting, h.c.IdleBefore = true, true, false
	p := node(f, tr, in, h)

	out := pass(p)
	if !out.Woken || out.Item.ID != "fire" {
		t.Fatalf("a busy seat holding urgent work = %+v, want the urgent item handed", out)
	}
	if !queue.Prioritized(in.got[0]) {
		t.Error("the urgent pickup is off the priority lane, behind the seat's background mail")
	}

	// THE NEXT ITEM IS NOT URGENT, so it waits for the seat to be idle.
	f.turnOn(in.got[0])
	if out := pass(p); out.Woken || out.Skip != pickup.SkipBusy {
		t.Fatalf("a busy seat with no urgent work left = %+v, want it busy", out)
	}
	h.c.Dispatching, h.c.MailWaiting = false, false
	pass(p)
	if out := pass(p); !out.Woken || out.Item.ID != "later" || queue.Prioritized(in.got[1]) {
		t.Errorf("the idle seat's next pickup = %+v, want later in arrival order", out)
	}
}

// A RUN IN ITS BOX IS A TURN IN FLIGHT. A seat whose every turn runs in an
// executor box is held by each run from its launch to its settle, so a pass
// that read the run as a hold found the seat free only between two runs, and
// a minute's pass could miss every such gap while the seat drained a backlog.
// The urgent pickup queues on the lane, which the inbox holds with the rest of
// its mail while the run holds the seat and takes first once it settles.
//
// Mutation: keep the executor's hold in UrgentMayJump, and the urgent item is
// not handed while the run lasts.
func TestAnUrgentItemIsHandedWhileTheSeatsExecutorRuns(t *testing.T) {
	t.Parallel()
	f, in := newFleet(), &inbox{}
	tr := &tracker{items: []worklet.ReadyItem{urgent("fire", worklet.StatusTodo, 40),
		item("later", worklet.StatusTodo, 41)}}
	h := idleHolder()
	h.c.ExecutorHolds, h.c.MailWaiting, h.c.IdleBefore = true, true, false
	p := node(f, tr, in, h)

	out := pass(p)
	if !out.Woken || out.Item.ID != "fire" {
		t.Fatalf("a seat its executor's run holds, with urgent work = %+v, want the urgent item handed", out)
	}
	if !queue.Prioritized(in.got[0]) {
		t.Error("the urgent pickup is off the priority lane, behind the seat's background mail")
	}

	// THE NEXT ITEM IS NOT URGENT, so it waits for the run to settle.
	f.turnOn(in.got[0])
	if out := pass(p); out.Woken || out.Skip != pickup.SkipExecutor {
		t.Fatalf("a seat its run holds, with no urgent work left = %+v, want it held by its executor", out)
	}
}

// ONLY A TURN, ITS RUN, MAIL OR THE SETTLE IS JUMPED. A seat a person paused,
// one whose run waits on a person's answer (the seat's next delivery is offered
// as that answer), one parked or refused at a limit, or one this node does not
// hold is handed nothing, urgent or not.
func TestAnUrgentItemWaitsForWhatHoldsTheSeat(t *testing.T) {
	t.Parallel()
	for name, hold := range map[string]func(*pickup.Conditions){
		"paused":          func(c *pickup.Conditions) { c.Paused = true },
		"awaits a person": func(c *pickup.Conditions) { c.ExecutorAwaits = true },
		"parked":          func(c *pickup.Conditions) { c.Parked = true },
		"refused":         func(c *pickup.Conditions) { c.Refused = true },
		"not held":        func(c *pickup.Conditions) { c.Owned = false },
	} {
		f, in := newFleet(), &inbox{}
		tr := &tracker{items: []worklet.ReadyItem{urgent("fire", worklet.StatusTodo, 40)}}
		h := idleHolder()
		h.c.Dispatching = true
		hold(&h.c)
		if out := pass(node(f, tr, in, h)); out.Woken {
			t.Errorf("%s: an urgent item was handed to a seat held back: %+v", name, out)
		}
	}
}
