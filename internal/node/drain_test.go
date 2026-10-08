package node_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// A DRAIN'S GIVE-BACKS SPEND ONE ALLOWANCE, and never one longer than the
// lease they are racing.
//
// The store here answers a release only when the context it was asked on ends
// — a member that has lost quorum — so nothing but the allowance decides how
// long the drain spends learning that every give-back failed: the presence and
// both seats. In both cases the caller's own context has ALREADY ENDED, the
// deadline Drain's doc invites; a drain that handed it to the give-backs
// would return nothing at all, which fleet_test.go's expired-deadline case
// holds against a live store.
//
// The presence goes first and is the one give-back the allowance is still
// whole for, so it must be asked on a LIVE context: one asked on the caller's
// ended context fails before reaching any store, and this node goes on being
// counted by its peers for a TTL. The seats after it are rightly asked on
// contexts the presence's wait has already spent.
//
//   - A caller that is not an engine's stop carries no allowance, and the
//     drain begins one from this node's own TTL: one heartbeat interval, a
//     third of the lease. With none, the give-backs would wait on the store
//     for ever.
//   - A caller that IS one — an engine's stop, whose allowance its other
//     steps draw on too — is held to what it carries, not handed a fresh
//     allowance here; the TTL is long enough that this node's own could not
//     pass for it.
func TestADrainsGiveBacksSpendOneAllowance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		ttl       time.Duration
		carried   time.Duration // the caller's stop allowance, 0 for none
		allowance time.Duration
	}{
		{name: "begun_from_its_own_lease", ttl: 3 * time.Second,
			allowance: 3 * time.Second / seat.HeartbeatRatio},
		{name: "the_callers_stop", ttl: 30 * time.Second, carried: 300 * time.Millisecond,
			allowance: 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &stuckReleases{Backend: &coordmem.Backend{}}
			n := drainingNode(t, store, tc.ttl, "ceo", "cto")

			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if tc.carried > 0 {
				ctx = seat.WithStopBudget(ctx, seat.NewStopBudget(tc.carried))
			}
			store.block()
			started := time.Now()
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				n.Drain(ctx)
			}()
			select {
			case <-drained:
			case <-time.After(tc.ttl + 10*time.Second):
				t.Fatal("a drain against a store that never answers did not return: " +
					"its give-backs are bounded by no allowance")
			}
			// SLACK FOR THE TEARDOWNS AND THE SCHEDULER, which the allowance
			// does not charge. A drain that took an allowance of its own in
			// place of the caller's takes ten seconds in the second case,
			// and one with none never returns, which the guard above meets.
			if took := time.Since(started); took > tc.allowance+time.Second {
				t.Errorf("the drain took %v against an allowance of %v", took, tc.allowance)
			}
			if got := store.releases(); got != 3 {
				t.Errorf("%d release(s) asked for, want the presence and both seats", got)
			}
			if store.askedEnded(coord.NodeResource("node-a")) {
				t.Error("the presence was given back on a context that had already ended: " +
					"the drain handed its caller's context to the give-back")
			}
			if held := n.Host().Held(); len(held) != 0 {
				t.Errorf("the drain returned still holding %v", held)
			}
		})
	}
}

// drainingNode starts a node over the in-memory twins and store, holding
// seats, with the lease TTL a case is measuring against.
func drainingNode(t *testing.T, store coord.Backend, ttl time.Duration, seats ...string) *node.Node {
	t.Helper()
	broker := qmem.NewBroker()
	client := func() queue.EventQueue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("queue.Start: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
		return q
	}
	nodeQueue, publisher := client(), client()
	for _, h := range seats {
		if _, err := publisher.EnsureSubscription(t.Context(),
			topics.AgentInbox(h), topics.AgentInboxGroup(h)); err != nil {
			t.Fatalf("EnsureSubscription(%s): %v", h, err)
		}
	}
	n, err := node.New(node.Config{
		Queue: nodeQueue, Coord: store,
		NodeID: "node-a", Owner: "node-a:1",
		Seats: func() []placement.Seat {
			out := make([]placement.Seat, len(seats))
			for i, h := range seats {
				out[i] = placement.Seat{Handle: h}
			}
			return out
		},
		LeaseTTL: ttl, HeartbeatInterval: ttl / 4, SweepInterval: ttl / 8,
		Turn: func(context.Context, string, []*events.Event) queue.Result {
			return queue.Ack()
		},
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	if err := n.Start(t.Context()); err != nil {
		t.Fatalf("node.Start: %v", err)
	}
	t.Cleanup(func() { n.Stop(context.WithoutCancel(t.Context())) })
	within(t, "the node to hold every seat", func() bool {
		return len(n.Host().Held()) == len(seats)
	})
	return n
}

// stuckReleases is a store whose releases, once blocked, answer only when the
// context they were asked on ends — a member without quorum. Everything else
// it answers as the twin does, so the seats are claimed and renewed.
type stuckReleases struct {
	*coordmem.Backend
	mu      sync.Mutex
	blocked bool
	asked   int
	ended   map[string]bool // resources a release was asked for on an ended context
}

// askedEnded reports whether a release of resource was asked for on a context
// that had already ended.
func (s *stuckReleases) askedEnded(resource string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended[resource]
}

func (s *stuckReleases) block() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked = true
}

func (s *stuckReleases) releases() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

func (s *stuckReleases) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	s.mu.Lock()
	blocked := s.blocked
	if blocked {
		s.asked++
		if ctx.Err() != nil {
			if s.ended == nil {
				s.ended = map[string]bool{}
			}
			s.ended[resource] = true
		}
	}
	s.mu.Unlock()
	if !blocked {
		return s.Backend.Release(ctx, resource, owner, epoch)
	}
	<-ctx.Done()
	return false, coord.ErrUnavailable
}
