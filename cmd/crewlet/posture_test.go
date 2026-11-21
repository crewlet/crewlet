package main

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// A CLIENT POLLING /health THROUGH A CONFIG WRITE NEVER SEES A DIVERGENCE, on
// the node's first company and on a later one: the API and reconcile loop as
// serveNodeWith wires them, the write through PUT /config, the apply by the
// running loop, with the company-published hook taken over as the observer.
//
// The reconciler counted an attempt before making it, so the posture read an
// attempt at an unreached epoch as failed and every successful apply briefly
// reported `isolated` (`unconfigured` on the first).
//
// TWO READERS, because one could not fail: a poller samples the run as a
// client does and could miss a short apply; the hook reads /health from INSIDE
// each apply, after the epoch swap and before the outcome is recorded.
func TestHealthNeverReportsASuccessfulApplyAsADivergence(t *testing.T) {
	t.Parallel()
	n := bootUnconfigured(t, "a-test-token-long-enough-for-tier-a")
	if first, err := n.health(t.Context()); err != nil || first.Configured {
		t.Fatalf("before any write: %+v, %v — want a reachable, unconfigured node", first, err)
	}

	var (
		mu     sync.Mutex
		inside []healthSample
		failed []error
	)
	n.e.SetOnCompanyPublished(func(ctx context.Context) {
		sample, err := n.health(ctx)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failed = append(failed, err)
			return
		}
		inside = append(inside, sample)
	})
	n.reconcile()

	ctx, cancel := context.WithCancel(t.Context())
	var poller sync.WaitGroup
	var polled []healthSample
	poller.Go(func() {
		for ctx.Err() == nil {
			if sample, err := n.health(ctx); err == nil {
				polled = append(polled, sample)
			}
		}
	})

	// Until the node serves the epoch each write activated, which it reaches
	// only once the apply has recorded its outcome — after the hook read
	// /health inside it.
	if err := n.settle(n.put(companyYAML)); err != nil {
		t.Fatalf("the first company: %v", err)
	}
	if err := n.settle(n.put(strings.Replace(companyYAML, "name: Acme", "name: Acme Two", 1))); err != nil {
		t.Fatalf("a later company: %v", err)
	}
	cancel()
	poller.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(failed) > 0 {
		t.Fatalf("reading /health inside an apply failed: %v", failed)
	}
	if len(inside) != 2 {
		t.Fatalf("%d readings inside an apply, want one for each of the two", len(inside))
	}
	for i, sample := range inside {
		// After the swap, before the record: the node serves the new
		// company and has not yet taken its epoch as applied, which is
		// propagation — wait, and an ok status.
		if sample.Posture != "wait" || sample.Status != "ok" || !sample.Configured {
			t.Errorf("apply %d, read from inside it: %+v, want posture wait, status ok, configured",
				i+1, sample)
		}
	}
	if len(polled) == 0 {
		t.Fatal("the poller read /health not once, so it observed nothing")
	}
	for _, sample := range polled {
		switch {
		case sample.Posture != "serve" && sample.Posture != "wait":
			t.Fatalf("a client polling through two successful applies read posture %q: %+v",
				sample.Posture, sample)
		case sample.Status != "ok" && sample.Status != "unconfigured":
			t.Fatalf("a client polling through two successful applies read status %q: %+v",
				sample.Status, sample)
		case sample.Status == "unconfigured" && sample.Configured:
			t.Fatalf("status unconfigured on a configured node: %+v", sample)
		}
	}
}
