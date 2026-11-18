package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// A NODE CLAIMS ITS SEATS THE MOMENT ITS CLAIM GATE OPENS, NOT AT THE SWEEP'S
// NEXT TICK.
//
// [seat.Host.Start]'s first pass is the host's only synchronous claim, and on
// the commonest boot the gate does not admit it yet. Here the gate is held
// shut by the fleet's view never having listed (a node whose company runs a
// native half routes no seat's calls before it has) and opened after; the
// sweep's next tick is an HOUR away, so only the gate's opening can claim.
//
// The shipped lease TTL is kept and the view relisted ([engine.RelistFleetView]):
// a TTL is also the seat host's watchdog threshold, and a short one ended the
// whole test binary (exit 75) on a loaded runner.
func TestSeatsAreClaimedTheMomentTheClaimGateOpens(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc), nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	roster := &unlistedRoster{coordBackend: back.Coord}
	back.Coord = roster

	e, err := engine.New(t.Context(), engine.WithSweepEvery(engine.Options{
		Bootstrap: b, Company: parsedCompany(t, companyDoc), Backends: back,
	}, time.Hour))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	host := e.Node().Host()
	// THE PREMISE: the gate refused the start's own pass, so the seats are
	// the gate's opening's to claim or nobody's for an hour.
	if last, ok := host.LastSweep(); !ok || !last.Withheld {
		t.Fatalf("the premise: the claim gate refused the start's pass, got %+v", last)
	}
	if held := host.Held(); len(held) != 0 {
		t.Fatalf("the premise: the node claimed %v past a closed gate", held)
	}

	roster.answer()
	engine.RelistFleetView(e)
	waitFor(t, "both seats to be claimed once the gate opened", func() bool {
		return len(host.Held()) == 2
	})
}

// unlistedRoster is the coordination store with its listing of the fleet's
// presence leases refused until [unlistedRoster.answer]: a node's view of the
// fleet never lists while it is, and a node whose company runs a native half
// claims no seat until its view has listed.
type unlistedRoster struct {
	coordBackend
	mu       sync.Mutex
	answered bool
}

func (r *unlistedRoster) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	r.mu.Lock()
	answered := r.answered
	r.mu.Unlock()
	if class == coord.ClassNode && !answered {
		return nil, errors.New("the fleet's presence leases cannot be listed yet")
	}
	return r.coordBackend.ListLive(ctx, class)
}

// answer lets the listing through from now on.
func (r *unlistedRoster) answer() {
	r.mu.Lock()
	r.answered = true
	r.mu.Unlock()
}
