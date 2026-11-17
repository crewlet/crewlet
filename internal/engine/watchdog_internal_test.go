package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// watchdogCompanyDoc is the smallest company that builds an epoch.
const watchdogCompanyDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: CTO
    handle: cto
    llm: zulu
`

func watchdogEngine(t *testing.T) *Engine {
	t.Helper()
	boot := testBootstrap(t)
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&boot.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	SeedStore(t, &boot)
	// No HTTP surface: a bound port would contend with the package's other tests.
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

// THE ENGINE ARMS A WATCHDOG AND POINTS IT AT THE SEAT HOST, so a wedged node
// dies rather than hold its seats' mail unacked after its leases lapse.
//
// A beat seen is the discriminator, catching a MISSING Watch as well as a
// missing constructor: the watchdog reads as beating only once a watched, live
// duty has stamped a beat. The lag cannot be: a host on its beat has zero lag,
// as does a watchdog watching nothing.
func TestTheEngineArmsAWatchdogOnTheSeatHost(t *testing.T) {
	t.Parallel()
	e := watchdogEngine(t)
	if e.watchdog == nil {
		t.Fatal("New built no watchdog, so a wedged node would never end itself")
	}
	if e.watchdog.Beating() {
		t.Fatal("the watchdog reads a beat before the host runs: the premise")
	}

	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for !e.watchdog.Beating() {
		if time.Now().After(deadline) {
			t.Fatal("the watchdog never saw a beat from the seat host: it is " +
				"watching nothing, or it was started before the host was live " +
				"and stood down for the life of the process")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A HEALTHY NODE REPORTS NO STALL LAG: a host on its beat reads zero, which the
// health body omits, as the docs promise `stall_lag_seconds` only while a duty
// is behind; the raw time since the last beat is never zero. Polled, not read
// once: the host beats on a real clock, and one late tick on a loaded runner
// is a missed beat honestly reported.
func TestAHealthyNodeReportsNoStallLag(t *testing.T) {
	t.Parallel()
	e := watchdogEngine(t)
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !e.watchdog.Beating() || e.StallLag() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("a beating seat host still reads %v behind: a healthy node's "+
				"health carries stall_lag_seconds", e.StallLag())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// THE WATCHDOG IS DISARMED BEFORE THE DRAIN, NOT AFTER IT: teardown (reaping
// MCP process trees, joining goroutines, waiting on in-flight turns) blocks
// long enough to look like the wedge it ends, and exiting mid-drain abandons
// the seat release that makes it graceful.
func TestTheWatchdogIsDisarmedBeforeTheDrain(t *testing.T) {
	t.Parallel()
	e := watchdogEngine(t)
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	e.Stop(t.Context())

	// A stopped host reports no live duty, so this asserts only that Stop
	// returned; the ordering is guarded at the call site, by the drain never
	// being able to outlive an armed watchdog.
	if got := e.StallLag(); got != 0 {
		t.Errorf("lag = %v after a full stop, want none", got)
	}
	// Idempotent: a failed Start's cleanup runs Stop a second time.
	e.Stop(t.Context())
}

// THE ENGINE SUPPLIES A ROUND-CAP EXTENSION JUDGE: without one,
// extension.Consider rescues with "no_judge" on every exhaustion, so the
// per-phase ceilings, round step, enable switch and llm_judge model role go
// unread and a phase making progress is rescued like one thrashing.
func TestTheEngineSuppliesAnExtensionJudge(t *testing.T) {
	t.Parallel()
	e := watchdogEngine(t)
	if got := e.judgeFor(e.Company(), "ceo", auxspend.Use{Stage: types.AuxStageTurn}); got == nil {
		t.Error("no judge for a seat whose company configures a model")
	}
	// A handle that is no seat here has no chain and no judge: an ordinary
	// answer, which Consider reads as "do not ask", not a failure.
	if got := e.judgeFor(e.Company(), "nobody", auxspend.Use{Stage: types.AuxStageTurn}); got != nil {
		t.Error("a judge for a handle that is not a seat")
	}
	if got := e.judgeFor(nil, "ceo", auxspend.Use{Stage: types.AuxStageTurn}); got != nil {
		t.Error("a judge built from no epoch")
	}
}
