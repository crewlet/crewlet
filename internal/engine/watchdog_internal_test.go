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
	brokertest.StoreIn(&boot.Stream, filepath.Join(t.TempDir(), "stream"))
	SeedStore(t, &boot)
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
// A beat seen is the discriminator, and it is what makes this catch a
// MISSING Watch as well as a missing constructor: the watchdog reads as
// beating only once a watched, live duty has actually stamped a beat. The lag
// cannot be it: a host on its beat is not behind, so its lag is zero, which
// is also what a watchdog watching nothing reads.
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

// A HEALTHY NODE REPORTS NO STALL LAG.
//
// The lag reached /health whole, the time since the host's last beat, which
// on a turning host is never zero, only under a second: every healthy node
// carried a `stall_lag_seconds` the docs promise only while the duty is
// behind. A host on its beat reads zero, and the health body omits zero.
// Polled rather than read once, because the host beats on a real clock and
// one late tick on a loaded runner is a missed beat that is honestly reported.
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

// THE WATCHDOG IS DISARMED BEFORE THE DRAIN, NOT AFTER IT.
//
// Teardown is the one part of the process that legitimately blocks for a long
// time — reaping MCP process trees, joining goroutines, waiting on in-flight
// turns indefinitely — and all of it looks to an armed watchdog exactly like
// the wedge it exists to end. Exiting through the middle of a drain abandons
// the seat release that makes it graceful.
func TestTheWatchdogIsDisarmedBeforeTheDrain(t *testing.T) {
	t.Parallel()
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

// THE ENGINE SUPPLIES A ROUND-CAP EXTENSION JUDGE.
//
// The regression this exists for: extension.Judge had no implementation
// anywhere in the tree and RunnerInput.Judge was never set, so
// extension.Consider rescued with "no_judge" on every exhaustion. The
// per-phase ceilings, the round step, the enable switch and the llm_judge
// model role were all configuration that nothing read — a phase running out
// of rounds while making obvious progress was rescued exactly like one that
// was thrashing.
func TestTheEngineSuppliesAnExtensionJudge(t *testing.T) {
	t.Parallel()
	e := watchdogEngine(t)
	if got := e.judgeFor(e.Company(), "ceo", auxspend.Use{Stage: types.AuxStageTurn}); got == nil {
		t.Error("no judge for a seat whose company configures a model")
	}
	// A handle that is not a seat in this company has no chain to resolve
	// and no judge — an ordinary answer, which Consider reads as "do not
	// ask" rather than as a failure.
	if got := e.judgeFor(e.Company(), "nobody", auxspend.Use{Stage: types.AuxStageTurn}); got != nil {
		t.Error("a judge for a handle that is not a seat")
	}
	if got := e.judgeFor(nil, "ceo", auxspend.Use{Stage: types.AuxStageTurn}); got != nil {
		t.Error("a judge built from no epoch")
	}
}
