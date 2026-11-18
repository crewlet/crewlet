package engine_test

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// A START WHOSE CONTEXT ENDS BEGINS THE DRAIN: the seat host's first placement
// pass runs on a context detached from Start's
// ([TestCancellingTheStartContextDoesNotStopTheEngine]), so otherwise it would
// claim the node's whole share onto a stopping node and run every hook out.
//
// The pass is caught at its presence claim, the one store call the host's
// Start always makes first (the claim gate may open only after Start returns).
// Held there, the case cancels and waits for the drain before the claim
// answers: the pass must claim nothing, and the presence is given back. The
// lease TTL is [startedLeaseTTL], the floor every started engine is held to.
//
// Mutation: arm no AfterFunc in [engine.Engine.Start]; no drain during start.
func TestAStartWhoseContextEndsBeginsTheDrain(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		b.Coordination.LeaseTTLSeconds = startedLeaseTTL.Seconds()
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc), nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	leases := &heldPresence{coordBackend: back.Coord, at: make(chan struct{}), proceed: make(chan struct{})}
	back.Coord = leases

	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, companyDoc), Backends: back,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	host := e.Node().Host()

	leases.armed.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var startErr error
	started := make(chan struct{})
	go func() {
		defer close(started)
		startErr = e.Start(ctx)
	}()
	// Registered after the stop's cleanup so it runs first: a failing case
	// lets the start finish rather than leaving the stop waiting on it.
	t.Cleanup(func() { leases.release(); <-started })

	select {
	case <-leases.at:
	case <-started:
		t.Fatalf("the premise: Start returned (%v) without the seat host claiming its "+
			"presence", startErr)
	}
	cancel()
	waitFor(t, "the seat host to begin draining once Start's context ended", host.Draining)
	leases.release()
	<-started
	if startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}

	if !host.Draining() {
		t.Error("the host is not draining after a Start whose context ended")
	}
	if held := host.Held(); len(held) != 0 {
		t.Errorf("the first pass of a start whose context had ended claimed %v onto a "+
			"node already stopping", held)
	}
	if n := leases.seatClaims.Load(); n != 0 {
		t.Errorf("the first pass asked the store for %d seat(s) after the start's context "+
			"ended", n)
	}
	lease, err := leases.coordBackend.Get(t.Context(), coord.NodeResource(e.Node().ID()))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if lease != nil {
		t.Errorf("the presence a draining host was granted is still held by %s: its "+
			"peers keep reserving a share for a node that is leaving", lease.Owner)
	}
}

// heldPresence is the lease store with the first presence claim made once
// armed held until release, at closed when that claim has begun; it counts the
// seat claims asked of it once armed.
type heldPresence struct {
	coordBackend
	armed      atomic.Bool
	at         chan struct{}
	proceed    chan struct{}
	once       sync.Once
	seatClaims atomic.Int64
}

func (l *heldPresence) TryAcquire(ctx context.Context, resource string,
	opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {

	switch {
	case coord.ClassNode.Holds(resource) && l.armed.CompareAndSwap(true, false):
		close(l.at)
		select {
		case <-l.proceed:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	case coord.IsSeatResource(resource):
		select {
		case <-l.at:
			l.seatClaims.Add(1)
		default:
		}
	}
	return l.coordBackend.TryAcquire(ctx, resource, opts)
}

// release lets the held presence claim answer.
func (l *heldPresence) release() { l.once.Do(func() { close(l.proceed) }) }
