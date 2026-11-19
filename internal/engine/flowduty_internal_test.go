package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fixedPosture is a live projection answering one posture.
type fixedPosture worklet.Posture

func (p fixedPosture) SeatPostures() worklet.Posture { return worklet.Posture(p) }

// THE FLOW DUTY IS HELD WHERE THE POSTURE IS LIVE, WHATEVER THE NODE'S ROLES
// (ADR-0162). A worker node folds no live projection, so it reads every
// stopped seat's work as moving and never asks for the lease; a node serving
// the API with a seeded projection takes it, though it runs no workers; and a
// holder whose projection stops being live gives the lease back at once, so a
// live peer takes it at its next tick. Mutation: gate the lease on the workers
// role, or keep it while not live, and it fails.
func TestTheFlowDutyIsHeldWhereThePostureIsLive(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	backend := memory.New()
	worker := &Engine{profile: placement.NodeProfile{ID: "w", Roles: placement.Roles(placement.RoleWorkers)}}
	api := &Engine{profile: placement.NodeProfile{ID: "i", Roles: placement.Roles(placement.RoleIngress)}}
	api.UsePostures(fixedPosture{Live: true})
	live := func(e *Engine) bool { return livePostures{engine: e}.SeatPostures().Live }
	workerLease := newLiveLease(backend, flowDutyName, "owner-w", "w", flowDutyTTL)
	apiLease := newLiveLease(backend, flowDutyName, "owner-i", "i", flowDutyTTL)
	holder := func() string {
		t.Helper()
		lease, err := backend.Get(ctx, coord.WorkerResource(flowDutyName))
		if err != nil {
			t.Fatalf("read the lease: %v", err)
		}
		if lease == nil {
			return ""
		}
		return lease.Owner
	}

	if held, err := workerLease.holds(ctx, live(worker)); err != nil || held || holder() != "" {
		t.Fatalf("a worker node with no live projection held the duty %v (%v), the lease is %q",
			held, err, holder())
	}
	if held, err := apiLease.holds(ctx, live(api)); err != nil || !held || holder() != "owner-i" {
		t.Fatalf("a node serving the API with a live projection held the duty %v (%v), "+
			"the lease is %q", held, err, holder())
	}
	api.UsePostures(fixedPosture{})
	if held, err := apiLease.holds(ctx, live(api)); err != nil || held || holder() != "" {
		t.Fatalf("a holder whose projection stopped being live held the duty %v (%v), "+
			"the lease is %q", held, err, holder())
	}
	if held, err := workerLease.holds(ctx, live(worker)); err != nil || held {
		t.Fatalf("the worker node took the duty the API node gave back: %v (%v)", held, err)
	}
}

// A LONE NODE HOLDS THE DUTY WHENEVER ITS POSTURE IS LIVE, with no lease to
// take: the single-node company's API and tracker are one node.
func TestALoneNodeHoldsTheFlowDutyWhileLive(t *testing.T) {
	t.Parallel()
	lone := (&Engine{}).liveDuty(flowDutyName, flowDutyTTL)
	if held, err := lone.holds(t.Context(), true); err != nil || !held {
		t.Fatalf("a lone live node held the duty %v (%v)", held, err)
	}
	if held, err := lone.holds(t.Context(), false); err != nil || held {
		t.Fatalf("a lone node that is not live held the duty %v (%v)", held, err)
	}
}

// A DUE ITEM'S GRACE IS THE INBOX'S ACKNOWLEDGEMENT WINDOW, borrowed (ADR-0015,
// ADR-0162): the window a wake plus a worst-case turn is sized to, so a lane
// pickup handed at the start has had every turn the engine expects first.
func TestADueItemsGraceIsTheInboxsAcknowledgementWindow(t *testing.T) {
	t.Parallel()
	if worklet.FlowDueGrace != jetstream.DefaultAckWait {
		t.Fatalf("a due item's grace is %s and the inbox acknowledges within %s",
			worklet.FlowDueGrace, jetstream.DefaultAckWait)
	}
}
