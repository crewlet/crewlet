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
)

// THE IDENTITY DUTIES ARE ARMED WHERE THEIR INPUTS ARE, and nowhere else.
//
// Each of the two had no loop behind it until it was armed here — a sweep
// record nothing published, three indexes nobody read — and each of those
// looked, from every other vantage point, exactly like a duty quietly finding
// nothing to do. So the arming decision is held three ways:
//
//  1. A node running the identity domain arms both, at their stated
//     intervals.
//  2. A seats-only satellite RUNS the domain, as every node does, and arms
//     nothing, for the reason the next one does.
//  3. An ingress-only node RUNS the domain and still arms nothing: every duty
//     is a worker singleton its roles gate would refuse on every tick, and
//     loops that never run reported as armed duties are exactly the
//     look-alike this case exists to rule out.
func TestTheIdentityDutiesAreArmedWhereTheirInputsAre(t *testing.T) {
	t.Parallel()
	storeDir := func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
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
		"a seats-only satellite": {"seats"},
		"an ingress-only node":   {"ingress"},
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

// EVERY IDENTITY DUTY IS A LEASE-CLAIMED FLEET SINGLETON.
//
// Each duty's claim is the node's own `worker:` lease on the duty's name, so a
// fleet runs each pass on exactly one node per tick — the sweep publisher and
// the probe above all, which write records and ask somebody else's identity
// provider. A duty armed with no claim runs on EVERY node, and nothing else in
// this package would notice: the pass still runs, the records it writes are
// still accepted. So the case reads the coordination store after the first
// tick, which every duty takes at arming: each armed duty's lease is held, by
// this node.
func TestEveryIdentityDutyIsALeaseClaimedSingleton(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
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

// THE DUTY ROSTER IS READ WHILE THE NODE STOPS, and never raced.
//
// `/health` serves [engine.Engine.IdentityDuties] from the socket's health
// tick, which keeps running while a node is torn down under an open dashboard
// — and the stop cleared the roster through a plain pointer that tick was
// reading. The fleet suite's race detector caught it on a restart. Here a
// reader loops over the roster while the node stops, which the detector
// reports against a plain pointer, and a stopped node names no duties.
//
// Mutation: store the roster in a plain pointer again and -race fails this.
func TestTheDutyRosterIsReadableWhileTheNodeStops(t *testing.T) {
	t.Parallel()
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
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
