package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// A BOX THE ROW NEVER NAMED IS RECLAIMED BY ITS HOST: a box records the turn
// it is made for as it is made ([Spec.TurnID]), before the launch attaches it
// to the run's row, so its host's sweep finds one the row never named and
// reclaims it once the row lets it go. Two ways a row misses its box: a create
// that failed and left its box (a container the runtime would not remove),
// which ends the launch's own row; and the engine stopping before the attach,
// whose row the seat's next holder reaps, the box kept until then since a
// launch may still attach it.
//
// Mutations: make the launch's box record no turn, and the first row goes red;
// reclaim a box whose row is launching, or none whose row is gone, the second.
func TestABoxTheRowNeverNamedIsReclaimedByItsHost(t *testing.T) {
	t.Parallel()
	t.Run("a failed create left it", func(t *testing.T) {
		t.Parallel()
		rig := newWaiterRigIn(t, Container)
		backend := leavingBackend(t, rig)
		backend.leave = true
		req := launchReq("t1")
		req.Spec.Placement = Container

		if _, err := rig.launchVia(t.Context(), rig.manager, req); err == nil {
			t.Fatal("a launch whose create failed started")
		}
		rig.finished("t1")
		left := backend.made()
		if len(left) != 1 {
			t.Fatalf("boxes made %v, want the one the failed create left", left)
		}
		rig.tick()
		if killed := rig.provider.KilledIDs(); !slices.Contains(killed, left[0]) {
			t.Errorf("the box the row never named was left on its host: killed %v", killed)
		}
	})
	t.Run("its engine stopped before the attach", func(t *testing.T) {
		t.Parallel()
		rig := newWaiterRigIn(t, Container)
		ctx := t.Context()
		if _, err := rig.pending.BeginLaunch(ctx, PendingRun{
			TurnID: "t1", AgentHandle: "swe", AgentID: sweID, Role: "SWE",
			Harness: "claude-code", Placement: string(Container),
			PartitionKey: "chat:D1:root-1", ConversationKey: "chat:D1",
			WorkKey: rigWorkKey, CreatedAt: rig.now,
		}, rigLease); err != nil {
			t.Fatalf("BeginLaunch: %v", err)
		}
		box, err := rig.provider.Create(ctx, Spec{Placement: Container, TurnID: "t1"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		rig.tick()
		if killed := rig.provider.KilledIDs(); slices.Contains(killed, box.ID()) {
			t.Fatalf("reclaimed %s while its run was launching, which may still attach it", box.ID())
		}

		successor := Fence{Owner: "node-2", Epoch: 7}
		c, err := NewCoordinator(CoordinatorOptions{
			Pushes:  &pushSpy{},
			Answers: rigAnswers(), Leads: rigLeads(),
			Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
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
		rig.finished("t1")
		rig.tick()
		if killed := rig.provider.KilledIDs(); !slices.Contains(killed, box.ID()) {
			t.Errorf("the box the row never named was left on its host: killed %v", killed)
		}
	})
}

// leaving is a backend whose create, while leave is set, makes its box and
// then fails, as a container the runtime started and then would not remove
// once its mount failed its proof: the box is left, recording its turn.
type leaving struct {
	*FakeProvider
	mu    sync.Mutex
	leave bool
	boxes []string
}

// leavingBackend is a [leaving] backend over rig's fake, serving its
// container cell to the rig's launches and its waiter alike.
func leavingBackend(t *testing.T, rig *waiterRig) *leaving {
	t.Helper()
	backend := &leaving{FakeProvider: rig.provider}
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Container: backend},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rig.manager = manager
	rig.current.Store(manager)
	return backend
}

func (p *leaving) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	box, err := p.FakeProvider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.boxes = append(p.boxes, box.ID())
	if p.leave {
		return nil, Because(WhyConfiguration, errors.New("the box's mount failed its proof, and its container was kept"))
	}
	return box, nil
}

func (p *leaving) made() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.boxes)
}
