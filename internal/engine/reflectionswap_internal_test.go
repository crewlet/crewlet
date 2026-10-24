package engine

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// A LATER APPLY'S REFLECTION WORKERS GO LIVE ONLY WHEN THE APPLY SWAPS THEM IN.
// The running seats' reflections are served by whatever workers the dispatcher
// holds, so workers swapped in as soon as they were built ran the revision's
// models before its configured credential bundles were restored — a pass on an
// entry the revision signs in from its bundle ran signed out. Built
// by prepareReflection, they wait for the swap it returns, which the apply
// makes after the restore ([TestTheBundleIsRestoredBeforeTheEpochIsCurrent]
// holds that order).
//
// Mutation: swap the workers inside prepareReflection rather than in what it
// returns, and the running workers are gone before the swap here.
func TestALaterApplysReflectionWorkersWaitForTheSwap(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	// A node running a dispatcher over one probe, and a revision whose own
	// workers are none: this node keeps no store to write them to.
	e, probe := holderOn(t, q)
	running := e.reflector.Load()

	install, err := e.prepareReflection(ctx, companyFor(t, reflectingCompany))
	if err != nil {
		t.Fatalf("prepareReflection: %v", err)
	}
	if got := running.Reflect(ctx, reflectedTurn("run-1"), events.TraceContext{}); got.Skip != "" ||
		!slices.Equal(probe.ran(), []string{"run-1"}) {
		t.Fatalf("the revision's workers went live before the apply swapped them in: the pass "+
			"was %+v and the running worker reflected on %v", got, probe.ran())
	}

	install()
	if e.reflector.Load() != running {
		t.Error("the swap rebuilt the dispatcher, emptying its redelivery ring")
	}
	if got := running.Reflect(ctx, reflectedTurn("run-2"), events.TraceContext{}); got.Skip != learning.SkipNoWorkers {
		t.Errorf("after the swap the dispatcher still runs the previous epoch's workers: %+v", got)
	}
}
