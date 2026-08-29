package engine_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/org"
)

// WHAT THIS FILE EXISTS FOR.
//
// `internal/schedule` shipped complete and certified — cron grammar,
// at-most-once ledger, catchup, fleet duty, its own contract suite over two
// ledger implementations — and nothing ever CONSTRUCTED one. Every unit test
// in that package passed against a scheduler the engine never built, so a
// founder's schedules were parsed, validated, shown in the dashboard, and
// silently never fired.
//
// A suite that certifies a component cannot notice that nobody uses it. These
// cases sit on the other side of the seam: they assert the ENGINE arms the
// loop, so the subsystem cannot go unreachable again without going red.

// scheduledCompany is the ordinary test company plus one seat schedule.
func scheduledCompany(t *testing.T) *config.Company {
	t.Helper()
	return parsedCompany(t, strings.Replace(companyDoc, `  - name: CTO
    handle: cto
    llm: alpha`, `  - name: CTO
    handle: cto
    llm: alpha
    schedules:
      - name: standup
        cron: "30 9 * * *"
        task: "Post the standup thread"`, 1))
}

func scheduledEngine(t *testing.T, c *config.Company) *engine.Engine {
	t.Helper()
	return newEngine(t, engine.Options{
		Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		}),
		Company: c,
	})
}

// A COMPANY THAT DECLARES A SCHEDULE GETS A RUNNING TICK LOOP. The one case
// whose absence made the whole subsystem dead code.
func TestACompanyWithASchedulePlansToFireIt(t *testing.T) {
	t.Parallel()
	e := scheduledEngine(t, scheduledCompany(t))
	if !e.SchedulerRunning() {
		t.Fatal("a company declaring a schedule armed no scheduler, so none of " +
			"its cron work will ever fire")
	}
}

// AND A COMPANY WITH NONE DOES NOT, which is the other half of the config
// block's promise: an idle tick would claim a fleet duty every ten seconds
// on behalf of work that does not exist.
func TestACompanyWithNoSchedulesArmsNothing(t *testing.T) {
	t.Parallel()
	e := scheduledEngine(t, parsedCompany(t, companyDoc))
	if e.SchedulerRunning() {
		t.Fatal("a company with no schedules armed a scheduler")
	}
}

// THE OPERATOR'S OFF SWITCH IS HONOURED even when schedules exist — otherwise
// `scheduling.enabled: false` would be a field that validates and does
// nothing, which is the shape of every bug this audit found.
func TestTheSchedulerRespectsItsOffSwitch(t *testing.T) {
	t.Parallel()
	c := scheduledCompany(t)
	c.Scheduling.Enabled = org.Off()
	if e := scheduledEngine(t, c); e.SchedulerRunning() {
		t.Fatal("scheduling.enabled: false still armed the scheduler")
	}
}

// THE FIRST SCHEDULE ADDED TO A LIVE COMPANY STARTS THE LOOP, and the last
// one removed stops it. Without this an operator's first schedule does
// nothing until the next restart — and the config plane exists precisely so
// that a company can be edited without one.
func TestAddingTheFirstScheduleLiveArmsTheLoop(t *testing.T) {
	t.Parallel()
	e := scheduledEngine(t, parsedCompany(t, companyDoc))
	if e.SchedulerRunning() {
		t.Fatal("armed before any schedule existed")
	}

	withSchedule := scheduledCompany(t)
	if _, err := e.Apply(t.Context(), withSchedule); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !e.SchedulerRunning() {
		t.Fatal("adding the first schedule to a live company did not arm the " +
			"loop, so it fires nothing until the process restarts")
	}

	// ...and back again.
	if _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if e.SchedulerRunning() {
		t.Fatal("removing the last schedule left the loop running and its " +
			"fleet duty claimed")
	}
}

// STOPPING THE ENGINE STOPS THE LOOP. A tick that outlived its engine would
// publish into a queue that is closing, and its ledger row would mean no peer
// ever fires that run.
func TestStoppingTheEngineStopsTheScheduler(t *testing.T) {
	t.Parallel()
	e := scheduledEngine(t, scheduledCompany(t))
	if !e.SchedulerRunning() {
		t.Fatal("nothing to stop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.Stop(ctx)
	if e.SchedulerRunning() {
		t.Fatal("the tick loop outlived the engine that owns it")
	}
}
