package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
// a node has diverged from its fleet, and which epoch it has applied.
type healthSample struct {
	Status       string `json:"status"`
	Posture      string `json:"posture"`
	Configured   bool   `json:"configured"`
	AppliedEpoch int64  `json:"applied_epoch"`
}

// unconfiguredNode is a node booted with NO company and served the way
// runEngine serves one — the API over the engine, the config writer beside it,
// the reconciler that owns the posture — which is the quickstart's live
// bootstrap: its first revision arrives through PUT /config and is applied by
// the running reconcile loop, after every surface the API carries was built.
type unconfiguredNode struct {
	t          *testing.T
	boot       *config.Bootstrap
	e          *engine.Engine
	reconciler *engine.Reconciler
	// base is the API's own address, and token the operator credential
	// `ops` that every write and operator-only read here carries.
	base   string
	token  string
	client *http.Client
}

// bootUnconfigured boots and serves a node with no company, its API guarded by
// one operator token with the id `ops`. The reconcile loop is NOT started:
// a case that observes the apply from inside it installs its observer first,
// then calls [unconfiguredNode.reconcile].
func bootUnconfigured(t *testing.T, token string) *unconfiguredNode {
	t.Helper()
	boot := bootstrapFor(t, 0)
	boot.API.Port = freePort(t)
	boot.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: token}}
	seedEngineStore(t, boot)
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
	return &unconfiguredNode{
		t: t, boot: boot, e: e, reconciler: reconciler,
		base:   "http://127.0.0.1:" + strconv.Itoa(boot.API.Port),
		token:  token,
		client: httpxtest.Pool(t),
	}
}

// reconcile runs the node's reconcile loop as runEngine does, until the test
// ends — stopped, and waited for, before the API and the engine are.
func (n *unconfiguredNode) reconcile() {
	ctx, cancel := context.WithCancel(n.t.Context())
	var loop sync.WaitGroup
	loop.Go(func() { n.reconciler.Run(ctx) })
	n.t.Cleanup(func() {
		cancel()
		loop.Wait()
	})
}

// health reads /health once.
func (n *unconfiguredNode) health(ctx context.Context) (healthSample, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.base+"/health", nil)
	if err != nil {
		return healthSample{}, err
	}
	res, err := n.client.Do(req)
	if err != nil {
		return healthSample{}, err
	}
	defer func() { _ = res.Body.Close() }()
	var sample healthSample
	return sample, json.NewDecoder(res.Body).Decode(&sample)
}

// put writes doc as the company through PUT /config, as the operator, and
// returns the epoch the write activated.
func (n *unconfiguredNode) put(doc string) int64 {
	n.t.Helper()
	req, err := http.NewRequestWithContext(n.t.Context(), http.MethodPut, n.base+"/config",
		strings.NewReader(doc))
	if err != nil {
		n.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	req.Header.Set("X-Summary", "a company applied after boot")
	res, err := n.client.Do(req)
	if err != nil {
		n.t.Fatalf("PUT /config: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		n.t.Fatalf("PUT /config = %d", res.StatusCode)
	}
	var applied struct {
		Epoch int64 `json:"epoch"`
	}
	if err := json.NewDecoder(res.Body).Decode(&applied); err != nil || applied.Epoch <= 0 {
		n.t.Fatalf("PUT /config answered no epoch (%d): %v", applied.Epoch, err)
	}
	return applied.Epoch
}

// settle waits until the node reports epoch applied and serves it: /health's
// `applied_epoch` at or past it, and `posture: serve` — which a node reaches
// only once the apply has recorded its outcome, so everything the apply built
// (a native runtime at the first company that runs a state log among it)
// exists by the time this returns.
func (n *unconfiguredNode) settle(epoch int64) error {
	deadline := time.Now().Add(30 * time.Second)
	var last healthSample
	for time.Now().Before(deadline) {
		sample, err := n.health(n.t.Context())
		if err == nil {
			last = sample
			if sample.AppliedEpoch >= epoch && sample.Posture == "serve" {
				return nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("epoch %d was never applied and served (last /health: %+v)", epoch, last)
}

// get reads path from the node's API as the operator, answering the status
// and the body.
func (n *unconfiguredNode) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, n.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	res, err := n.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	if !json.Valid(body) {
		t.Fatalf("GET %s answered %d with a body that is not JSON: %q", path, res.StatusCode, body)
	}
	return res.StatusCode, string(body)
}
