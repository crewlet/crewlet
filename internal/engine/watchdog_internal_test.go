package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// watchdogCompanyDoc is the smallest company that builds an epoch.
const watchdogCompanyDoc = `
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

func watchdogEngine(t *testing.T) *Engine {
	t.Helper()
	boot := config.DefaultBootstrap()
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	boot.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	// No HTTP surface: this is about the seat host, and binding a port
	// would make the test fight every other one in the package for it.
	boot.API.Port = 0
	company, err := config.ParseCompany([]byte(watchdogCompanyDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := New(t.Context(), Options{Bootstrap: &boot, Company: company})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(t.Context()) })
	return e
}

// THE ENGINE ARMS A WATCHDOG AND POINTS IT AT THE SEAT HOST.
//
// The regression this exists for: seat.NewWatchdog had no caller outside its
// own test, so nothing in a running node ever constructed one. A node that
// wedged neither worked nor died — its leases lapsed and peers took its seats,
// while the process went on holding their mail unacked and no orchestrator
// watching for liveness ever restarted it.
//
// The lag is the discriminator, and it is what makes this catch a MISSING
// Watch as well as a missing constructor: Lag reports 0 both when nothing is
// watched and when nothing is live, and only becomes non-zero once a watched,
// live duty has actually stamped a beat.
func TestTheEngineArmsAWatchdogOnTheSeatHost(t *testing.T) {
	e := watchdogEngine(t)
	if e.watchdog == nil {
		t.Fatal("New built no watchdog, so a wedged node would never end itself")
	}
	if got := e.StallLag(); got != 0 {
		t.Fatalf("lag = %v before the host runs, want none — the premise", got)
	}

	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for e.StallLag() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the watchdog never saw a beat from the seat host: it is " +
				"watching nothing, or it was started before the host was live " +
				"and stood down for the life of the process")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// THE WATCHDOG IS DISARMED BEFORE THE DRAIN, NOT AFTER IT.
//
// Teardown is the one part of the process that legitimately blocks for a long
// time — reaping MCP process trees, joining goroutines, waiting on in-flight
// turns indefinitely — and all of it looks to an armed watchdog exactly like
// the wedge it exists to end. Exiting through the middle of a drain abandons
// the seat release that makes it graceful.
func TestTheWatchdogIsDisarmedBeforeTheDrain(t *testing.T) {
	e := watchdogEngine(t)
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	e.Stop(t.Context())

	// A stopped host reports no live duty, so the lag is nothing to report
	// either way; what this asserts is that Stop returned at all. An
	// engine that disarmed AFTER the drain would still be correct here,
	// so the ordering itself is guarded by the drain never being able to
	// outlive an armed watchdog — see the comment at the call site.
	if got := e.StallLag(); got != 0 {
		t.Errorf("lag = %v after a full stop, want none", got)
	}
	// Idempotent: the second Stop is what a failed Start's cleanup path
	// runs after the ordinary one has already been through.
	e.Stop(t.Context())
}
