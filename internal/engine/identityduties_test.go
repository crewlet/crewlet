package engine_test

import (
	"context"
	"maps"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// THE IDENTITY DUTIES ARE ARMED WHERE THEIR INPUTS ARE, and nowhere else: a
// duty with no loop behind it looks, from everywhere else, exactly like one
// finding nothing to do. So:
//
//  1. A node running the identity domain arms them, at their stated intervals.
//  2. A data node holding seats alone RUNS the domain and arms nothing.
//  3. A data node serving ingress alone RUNS the domain and arms nothing too:
//     every duty is a worker singleton its roles gate refuses on every tick,
//     and such loops reported as armed are the look-alike ruled out here.
func TestTheIdentityDutiesAreArmedWhereTheirInputsAre(t *testing.T) {
	t.Parallel()
	storeDir := func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	}
	want := map[string]time.Duration{
		"iam_sweep": engine.IdentitySweepInterval,
	}

	t.Run("a node running the domain", func(t *testing.T) {
		t.Parallel()
		e := newEngine(t, engine.Options{Bootstrap: bootstrap(t, storeDir)})
		if got := e.IdentityDuties(); !maps.Equal(got, want) {
			t.Fatalf("armed %v, want %v", got, want)
		}
	})

	for name, roles := range map[string][]string{
		"a data node holding seats alone":   {"data", "seats"},
		"a data node serving ingress alone": {"data", "ingress"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEngine(t, engine.Options{Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
				storeDir(b)
				b.Node.Roles = roles
			})})
			if e.IAM() == nil {
				t.Fatalf("precondition: a node with roles %v opened no identity "+
					"domain, and this case is about one that runs it", roles)
			}
			if got := e.IdentityDuties(); len(got) != 0 {
				t.Fatalf("a node that runs no workers armed %v", got)
			}
		})
	}
}

// EVERY IDENTITY DUTY IS A LEASE-CLAIMED FLEET SINGLETON: its claim is the
// node's own `worker:` lease on the duty's name, so a fleet runs each pass on
// one node per tick. A duty armed with no claim runs on EVERY node and nothing
// else would notice (the pass runs, its records are accepted), so the case
// reads the coordination store after the first tick, taken at arming: each
// armed duty's lease is held by this node.
func TestEveryIdentityDutyIsALeaseClaimedSingleton(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	})})
	armed := e.IdentityDuties()
	if len(armed) == 0 {
		t.Fatal("precondition: a worker node running the identity domain armed no duty")
	}
	owner := e.Node().Owner()
	deadline := time.Now().Add(30 * time.Second)
	for name := range armed {
		for {
			lease, err := e.Backends().Coord.Get(t.Context(), coord.WorkerResource(name))
			if err != nil {
				t.Fatalf("read the lease of %s: %v", name, err)
			}
			if lease != nil && lease.Owner == owner {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("duty %s ran its first tick with no lease held by this node "+
					"(%+v): it is not a singleton, so every node in a fleet runs it",
					name, lease)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// THE DUTY ROSTER IS READ WHILE THE NODE STOPS, and never raced: `/health`
// serves [engine.Engine.IdentityDuties] from the socket's health tick, which
// keeps running while a node is torn down under an open dashboard. Readers loop
// over the roster while the node stops, which -race reports against a plain
// pointer, and a stopped node names no duties.
//
// Mutation: store the roster in a plain pointer again and -race fails this.
func TestTheDutyRosterIsReadableWhileTheNodeStops(t *testing.T) {
	t.Parallel()
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		}),
		Company: parsedCompany(t, companyDoc),
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if len(e.IdentityDuties()) == 0 {
		t.Fatal("precondition: the node armed no identity duties to take down")
	}
	// SEVERAL READERS, each already reading before the stop begins, as a
	// health tick per open dashboard would be: the detector reports a race
	// only between accesses it saw, so a reader that had not started yet
	// would let the plain pointer pass.
	const readers = 4
	stop := make(chan struct{})
	var started, done sync.WaitGroup
	for range readers {
		started.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			first := true
			for {
				select {
				case <-stop:
					return
				default:
					_ = e.IdentityDuties()
					if first {
						first = false
						started.Done()
					}
				}
			}
		}()
	}
	started.Wait()
	e.Stop(context.Background())
	close(stop)
	done.Wait()
	if got := e.IdentityDuties(); got != nil {
		t.Errorf("a stopped node still names %v as armed", got)
	}
}
