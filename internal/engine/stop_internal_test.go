package engine

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// stopCompanyDoc is the smallest company that builds an epoch.
const stopCompanyDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`

// A SECOND STOP TEARS NOTHING DOWN.
//
// The first teardown closes the broker and the coordination store that its
// own steps write through — the admission withdrawn, the duties released, the
// node stopped — so a second pass would run each of them again against closed
// ones. Two callers reaching Stop is ordinary — this package's own tests stop
// an engine explicitly and again in their cleanup — so a later caller must
// wait for the first and then do nothing. Counted on the teardown itself
// rather than read off the log, whose sink is the process's and shared with
// every parallel test.
func TestASecondStopTearsNothingDown(t *testing.T) {
	boot := config.DefaultBootstrap()
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	boot.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	// No HTTP surface: nothing here is about the API, and binding a port
	// would make this test fight every other one in the package for it.
	boot.API.Port = 0
	company, err := config.ParseCompany([]byte(stopCompanyDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := New(t.Context(), Options{Bootstrap: &boot, Company: company})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := e.Start(t.Context()); err != nil {
		e.Stop(context.Background())
		t.Fatalf("Start: %v", err)
	}

	// Concurrently as well as in sequence: a caller that arrives while the
	// first teardown is still running must not start one of its own.
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() { e.Stop(context.Background()) })
	}
	wg.Wait()
	e.Stop(context.Background())

	if got := e.teardowns.Load(); got != 1 {
		t.Errorf("four Stops ran the teardown %d times, want once", got)
	}
}
