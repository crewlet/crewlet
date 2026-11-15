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

// A START WHOSE CONTEXT ENDS BEGINS THE DRAIN.
//
// The node's loops run on a context detached from Start's
// ([TestCancellingTheStartContextDoesNotStopTheEngine] says why), and so does
// the seat host's first placement pass, which runs each seat's acquire hook
// inline. A signal that arrives while that pass runs used to cut nothing: the
// caller's shutdown, and with it the drain, begins only once Start returns, so
// the pass went on claiming the node's whole share onto a node already
// stopping and ran every claimed seat's hook to its end, however long a hook
// takes. Now the caller's context ending while the node starts is a stop
// already decided: the host begins its drain at once, so the pass claims
// nothing more and cuts what it is establishing.
//
// THE PASS IS CAUGHT AT ITS PRESENCE CLAIM, the one store call the seat host's
// Start always makes before its first pass: whether that pass gets to claim a
// seat depends on the claim gate, which on a fresh node opens when its logs
// are level and so sometimes after Start has returned. Held there, the case
// cancels Start's context and waits for the drain to begin before letting the
// claim answer. The pass that follows must claim nothing, and the presence the
// host was granted meanwhile is given straight back.
//
// The lease TTL is [startedLeaseTTL], the floor every started engine is held
// to.
//
// Mutation: arm no AfterFunc in [engine.Engine.Start], and the drain never
// begins while the start runs.
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
	// BEFORE THE STOP, so a case that fails below lets the start finish
	// rather than leaving it held for the stop to wait on.
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
