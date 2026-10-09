package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/logging"
)

// healthSample is the part of a /health body a client reads to decide whether
// a node has diverged from its fleet.
type healthSample struct {
	Status       string `json:"status"`
	Posture      string `json:"posture"`
	Configured   bool   `json:"configured"`
	AppliedEpoch int64  `json:"applied_epoch"`
}

// A CLIENT POLLING /health THROUGH A CONFIG WRITE NEVER SEES A DIVERGENCE, on
// the node's first company and on a later one — the node's API and reconcile
// loop as serveNodeWith wires them, the write through PUT /config, the apply
// by the running loop. One part of that wiring is replaced: the engine's
// applied hook, which the API installs only to re-broadcast the dashboard's
// config-derived surfaces and which nothing /health reports reads, is taken
// over below as the observer inside each apply.
//
// For about a tenth of a second during every apply that then succeeded, the
// body used to say `posture: isolated` — and `status: isolated`, or
// `unconfigured` on the first apply — because the reconciler counted the
// attempt before making it and the posture read a counted attempt at an epoch
// not yet reached as a failed one. A client cannot tell that from a revision
// that genuinely does not apply, so it had to stop trusting the posture.
//
// TWO READERS, because one would not be able to fail: a poller samples the
// whole run as a client does, and could miss a short apply entirely; the
// engine's applied hook reads /health from INSIDE each apply, after the epoch
// is swapped and before the outcome is recorded, every time.
func TestHealthNeverReportsASuccessfulApplyAsADivergence(t *testing.T) {
	t.Parallel()
	const token = "a-test-token"
	boot := bootstrapFor(t, 0)
	boot.API.Port = freePort(t)
	boot.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: token}}
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: boot})
	if err != nil {
		t.Fatalf("an engine with no company was refused: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	surface, reconciler, err := serveNodeWith(t, boot, e, logging.Get("test"))
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	base := "http://127.0.0.1:" + strconv.Itoa(boot.API.Port)
	client := httpxtest.Pool(t)

	read := func(ctx context.Context) (healthSample, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
		if err != nil {
			return healthSample{}, err
		}
		res, err := client.Do(req)
		if err != nil {
			return healthSample{}, err
		}
		defer func() { _ = res.Body.Close() }()
		var sample healthSample
		return sample, json.NewDecoder(res.Body).Decode(&sample)
	}
	if first, err := read(t.Context()); err != nil || first.Configured {
		t.Fatalf("before any write: %+v, %v — want a reachable, unconfigured node", first, err)
	}

	var (
		mu     sync.Mutex
		inside []healthSample
		failed []error
	)
	e.SetOnApplied(func(ctx context.Context) {
		sample, err := read(ctx)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failed = append(failed, err)
			return
		}
		inside = append(inside, sample)
	})

	ctx, cancel := context.WithCancel(t.Context())
	var loops sync.WaitGroup
	loops.Go(func() { reconciler.Run(ctx) })
	var polled []healthSample
	loops.Go(func() {
		for ctx.Err() == nil {
			if sample, err := read(ctx); err == nil {
				polled = append(polled, sample)
			}
		}
	})

	put := func(doc string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, base+"/config",
			strings.NewReader(doc))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Summary", "posture under a successful apply")
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT /config: %v", err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
			t.Fatalf("PUT /config = %d", res.StatusCode)
		}
	}
	// Until the hook has read /health inside the apply the write caused,
	// and the node then serves the epoch it applied.
	settled := func(applies int) error {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			seen := len(inside) + len(failed)
			mu.Unlock()
			if seen >= applies {
				if sample, err := read(t.Context()); err == nil && sample.Posture == "serve" {
					return nil
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		return fmt.Errorf("apply %d never completed", applies)
	}

	put(companyYAML)
	if err := settled(1); err != nil {
		t.Fatalf("the first company: %v", err)
	}
	put(strings.Replace(companyYAML, "name: Acme", "name: Acme Two", 1))
	if err := settled(2); err != nil {
		t.Fatalf("a later company: %v", err)
	}
	cancel()
	loops.Wait()

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
