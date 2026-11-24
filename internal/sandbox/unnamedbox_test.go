package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// A BOX WHOSE CREATE ANSWERED NOBODY IS RECLAIMED WITH ITS RUN: a remote box
// carries the launch it is made for ([Spec.LaunchID]), and the ending of that
// launch's run asks its backend for the launch's boxes, so a box the row never
// named is killed rather than billed to its TTL. Two ways a row misses its box:
// the create's answer lost, which ends the launch's own row, and the engine
// stopping before the attach, whose row the seat's next holder reaps.
//
// Mutation: stamp the box with anything but the row's launch, and the first
// row goes red; end a row without asking its backend for the launch's boxes,
// and both do.
func TestABoxTheRowNeverNamedIsReclaimedWithItsRun(t *testing.T) {
	t.Parallel()
	t.Run("its create's answer lost", func(t *testing.T) {
		t.Parallel()
		rig := newWaiterRig(t)
		backend, manager := stampingBackend(t, rig)
		backend.lose = true
		req := launchReq("t1")
		req.Spec.Placement = E2B
		var launch string
		req.Recorded = func(id string) { launch = id }

		if _, err := rig.launchVia(t.Context(), manager, req); err == nil {
			t.Fatal("a launch whose create answered nobody started")
		}
		made := backend.made()
		if len(made) != 1 || made[0].launch != launch || launch == "" {
			t.Fatalf("boxes made %+v, want one stamped with the row's launch %q", made, launch)
		}
		if killed := rig.provider.KilledIDs(); !slices.Contains(killed, made[0].id) {
			t.Errorf("the box the row never named was left running: killed %v", killed)
		}
		rig.finished("t1")
	})
	t.Run("its engine stopped before the attach", func(t *testing.T) {
		t.Parallel()
		rig := newWaiterRig(t)
		backend, manager := stampingBackend(t, rig)
		ctx := t.Context()
		opened, err := rig.pending.BeginLaunch(ctx, PendingRun{
			TurnID: "t1", AgentHandle: "swe", AgentID: sweID, Role: "SWE",
			Harness: "claude-code", Placement: string(E2B),
			PartitionKey: "chat:D1:root-1", ConversationKey: "chat:D1",
			WorkKey: rigWorkKey, CreatedAt: rig.now,
		}, rigLease)
		if err != nil {
			t.Fatalf("BeginLaunch: %v", err)
		}
		box, err := backend.Create(ctx, Spec{Placement: E2B, TurnID: "t1", LaunchID: opened.LaunchID})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		successor := Fence{Owner: "node-2", Epoch: 7}
		c, err := NewCoordinator(CoordinatorOptions{
			Pushes:  &pushSpy{},
			Answers: rigAnswers(), Leads: rigLeads(),
			Queue: rig.queue, Pending: rig.pending, Manager: manager,
			Resume: &resumeSpy{}, Audience: &audienceSpy{},
			Lease: leased(successor),
			Now:   func() time.Time { return rig.now },
		})
		if err != nil {
			t.Fatalf("NewCoordinator: %v", err)
		}
		t.Cleanup(c.Stop)

		if err := c.RecoverSeat(ctx, "swe", successor.Owner, successor.Epoch, rigInbox, nil); err != nil {
			t.Fatalf("RecoverSeat: %v", err)
		}
		if killed := rig.provider.KilledIDs(); !slices.Contains(killed, box.ID()) {
			t.Errorf("the box the row never named was left running: killed %v", killed)
		}
		rig.finished("t1")
	})
}

// stamping is a remote backend's way with its boxes ([E2BProvider]): each is
// stamped with the launch it is made for, and the launch's boxes are killed by
// that stamp. While lose is set, a create makes its box and answers an error,
// as one whose answer never reached its caller.
type stamping struct {
	*FakeProvider
	mu      sync.Mutex
	lose    bool
	stamped []stampedBox
}

// stampedBox is one box a [stamping] backend made, and its stamp.
type stampedBox struct{ id, launch string }

var _ launchReclaimer = (*stamping)(nil)

// stampingBackend is a [stamping] backend over rig's fake, and a manager
// serving it as the e2b cell.
func stampingBackend(t *testing.T, rig *waiterRig) (*stamping, *Manager) {
	t.Helper()
	backend := &stamping{FakeProvider: rig.provider}
	manager, err := NewManager(ManagerOptions{
		Providers:        map[Placement]Provider{E2B: backend},
		DefaultPlacement: E2B,
		Runners:          map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return backend, manager
}

func (p *stamping) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	box, err := p.FakeProvider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stamped = append(p.stamped, stampedBox{id: box.ID(), launch: spec.LaunchID})
	if p.lose {
		return nil, Because(WhyProviderUnavailable, errors.New("e2b: the create's answer was lost"))
	}
	return box, nil
}

// KillLaunch kills every box stamped with launch.
func (p *stamping) KillLaunch(ctx context.Context, launch string) error {
	for _, box := range p.made() {
		if box.launch != launch {
			continue
		}
		if err := p.Kill(ctx, box.id); err != nil {
			return err
		}
	}
	return nil
}

func (p *stamping) made() []stampedBox {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.stamped)
}
