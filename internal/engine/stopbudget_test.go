package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/seat"
)

// A STOP AGAINST A COORDINATION STORE THAT HAS GONE AWAY SPENDS ONE ALLOWANCE.
//
// A member that has lost quorum still stops in order: it gives its presence
// and its seats back, withdraws its admission and releases its duties — and
// every one of those fails, each falling back to a lease lapsing on its TTL.
// Each used to wait out a bound of its own, so the stop spent their sum (the
// e2e fleet measured 25 s for one member), close to an orchestrator's kill
// grace and past it once the flushes behind them ran. Here every round trip to
// the lease and fleet stores stalls for five seconds once the stop begins,
// against a lease TTL of three — an allowance of one second — so the old sum
// is past half a minute and one allowance is a second. (The stream stays up:
// the native backends this company runs are reached through the broker's own
// client, which a stand-in cannot be.)
//
// A RECONCILE PASS IS IN FLIGHT WHEN THE STOP BEGINS, under the hold on its
// surface: the integration loop's first grant is held back until then. The
// teardown waits for that pass, and the pass gives its hold back on the
// context the hold was taken on — long before the stop — so that give-back
// sat out a whole stalled round trip beside the allowance until the engine
// bound it to the stop. Held rather than left to chance, because the loop's
// first pass runs as it starts and is over by the stop on an idle machine, so
// a case that waited for luck exercised the give-back only under load.
//
// THE STORE GOES AWAY AS THE STOP BEGINS — the moment its drain is decided
// ([engine.Engine.ShuttingDown]) — rather than a moment before it, because the
// subject is the round trips the STOP makes. One begun earlier ends on its own
// client's bound like any round trip in flight when a stop begins, and a case
// that took the store away first measured whether one of those happened to
// start in between.
func TestAStopAgainstAnUnreachableStoreSpendsOneAllowance(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		b.Coordination.LeaseTTLSeconds = 3
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc))
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	var stopped atomic.Pointer[engine.Engine]
	gone := func() bool {
		e := stopped.Load()
		return e != nil && e.ShuttingDown()
	}
	pass := &heldPass{granted: make(chan struct{})}
	back.Coord = stallingLeases{coordBackend: back.Coord, gone: gone, pass: pass}
	back.Fleet = stallingFleet{fleet: back.Fleet, gone: gone}

	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, companyDoc), Backends: back,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	stopped.Store(e)
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(e.Node().Host().Held()) == 0 {
		t.Fatal("the premise: the node holds seats for its stop to give back")
	}
	select {
	case <-pass.granted:
	case <-time.After(30 * time.Second):
		t.Fatal("the premise: the integration loop never took a surface's hold, " +
			"so no pass is in flight for the stop to wait on")
	}

	started := time.Now()
	e.Stop(context.Background())
	took := time.Since(started)
	allowance := seat.StopAllowance(3 * time.Second)
	// THE ALLOWANCE, and room for the local teardown around it: one stalled
	// round trip alone is five seconds, so a stop that left even one of its
	// steps to a bound of its own would be past this.
	if limit := allowance + 3*time.Second; took > limit {
		t.Fatalf("the stop took %v against a store that answers nothing, past "+
			"its %v allowance and %v of local teardown: its round trips each "+
			"waited out a bound of their own", took, allowance, limit-allowance)
	}
}

// errGone is what the stalled store answers once its stall is over.
var errGone = errors.New("the coordination store has lost quorum")

// stall waits out a round trip to a store that has gone away: until the
// request's context ends, or five seconds — a client's own timeout.
func stall(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errGone
	}
}

type coordBackend = coord.Backend

// stallingLeases is a lease store whose reads and releases stall once gone
// answers true, and which holds back its first grant of an integration
// surface's hold until then ([heldPass]).
type stallingLeases struct {
	coordBackend
	gone func() bool
	pass *heldPass
}

// heldPass is the reconcile pass a stop begins in the middle of: granted is
// closed when the first surface hold is taken, and that hold reaches its pass
// only once the stop has begun.
type heldPass struct {
	once    sync.Once
	granted chan struct{}
}

func (l stallingLeases) TryAcquire(ctx context.Context, resource string,
	opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {
	lease, refusal, err := l.coordBackend.TryAcquire(ctx, resource, opts)
	// EVERY SURFACE IS HELD UNDER `setup-provision-<kind>`, the one name a
	// reconcile pass and an operator's pass both take.
	if err != nil || lease == nil ||
		!strings.HasPrefix(resource, coord.WorkerResource("setup-provision-")) {
		return lease, refusal, err
	}
	first := false
	l.pass.once.Do(func() { first = true; close(l.pass.granted) })
	for first && !l.gone() {
		select {
		case <-ctx.Done():
			return lease, refusal, err
		case <-time.After(5 * time.Millisecond):
		}
	}
	return lease, refusal, err
}

func (l stallingLeases) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	if l.gone() {
		return false, stall(ctx)
	}
	return l.coordBackend.Release(ctx, resource, owner, epoch)
}

func (l stallingLeases) Get(ctx context.Context, resource string) (*coord.Lease, error) {
	if l.gone() {
		return nil, stall(ctx)
	}
	return l.coordBackend.Get(ctx, resource)
}

// stallingFleet is a fleet store whose admission withdrawal stalls once
// gone answers true.
type stallingFleet struct {
	fleet
	gone func() bool
}

func (f stallingFleet) ForgetAdmission(ctx context.Context, nodeID, incarnation string) error {
	if f.gone() {
		return stall(ctx)
	}
	return f.fleet.ForgetAdmission(ctx, nodeID, incarnation)
}
