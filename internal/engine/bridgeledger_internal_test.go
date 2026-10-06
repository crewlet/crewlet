package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// appendingStore is a pending store that records each bridged append and
// answers it as a row holding job "launch-1" would — or, once ended, as a row
// that has moved on.
type appendingStore struct {
	sandbox.PendingStore
	mu      sync.Mutex
	appends []sandbox.BridgeAppend
	ended   bool
}

func (s *appendingStore) AppendBridgeCall(_ context.Context, _ string, a sandbox.BridgeAppend) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appends = append(s.appends, a)
	if s.ended || (a.Launch != "" && a.Launch != "launch-1") {
		return "", nil
	}
	return "launch-1", nil
}

// steppingMeter is a bridged meter whose total grows by one call each read.
type steppingMeter struct {
	mu sync.Mutex
	n  int
}

func (m *steppingMeter) Total() runner.Bridged {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	return runner.Bridged{Aux: auxspend.Spent{Calls: m.n, Input: 100 * m.n, Output: 10 * m.n,
		CacheRead: 30 * m.n, CacheWrite: 2 * m.n}, Workers: m.n, WorkerInput: 500 * m.n,
		WorkerOutput: 40 * m.n}
}

// A SESSION'S CALLS ARE PINNED TO ITS JOB AND CARRY ITS METER.
//
// The first append names no job and learns the one the row holds; every later
// append names it, so a call that outlives its job is refused by the store
// rather than landing on the next job's record. And every append carries the
// meter's running total as the row stores it — every figure, the cache shares
// and the workers included — for the segment that resumes from the run to pay.
//
// Mutations: never pin, keep re-sending the empty job, or send an empty total,
// and this goes red.
func TestABridgeSessionsCallsArePinnedToItsJobAndCarryItsSpend(t *testing.T) {
	t.Parallel()
	store := &appendingStore{}
	meter := &steppingMeter{}
	l := &bridgeLedger{store: store, spend: meter}
	for range 3 {
		if err := l.Append(t.Context(), "run-1", tools.Call{Name: "query_episodes"}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if len(store.appends) != 3 {
		t.Fatalf("%d appends, want 3", len(store.appends))
	}
	if got := store.appends[0].Launch; got != "" {
		t.Errorf("the first append named job %q before any append had landed", got)
	}
	for i, a := range store.appends[1:] {
		if a.Launch != "launch-1" {
			t.Errorf("append %d named job %q, want the one the first append landed under", i+2, a.Launch)
		}
	}
	n := 3
	want := sandbox.EngineSpend{
		Aux:     sandbox.AuxTokens{Input: 100 * n, Output: 10 * n, CacheRead: 30 * n, CacheWrite: 2 * n},
		Workers: n, WorkerInput: 500 * n, WorkerOutput: 40 * n,
	}
	if got := store.appends[2].Spent; got != want {
		t.Errorf("the third append carried %+v, want the meter's running total %+v", got, want)
	}
}

// A LEDGER WHOSE FIRST APPEND LANDED NOWHERE LEARNS NOTHING. A row that is gone
// or has moved on answers no job, and pinning to that answer would leave the
// session's every later call naming no job — so it keeps asking, and a session
// with no meter sends no spend at all.
func TestABridgeSessionPinsOnlyOnAnAppendThatLanded(t *testing.T) {
	t.Parallel()
	store := &appendingStore{ended: true}
	l := newBridgeLedger(store, nil)
	for range 2 {
		if err := l.Append(t.Context(), "run-1", tools.Call{Name: "read_page"}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	for i, a := range store.appends {
		if a.Launch != "" {
			t.Errorf("append %d named job %q, though no append ever landed", i+1, a.Launch)
		}
		if a.Spent != (sandbox.EngineSpend{}) {
			t.Errorf("append %d carried spend %+v from a session with no meter", i+1, a.Spent)
		}
	}
}
