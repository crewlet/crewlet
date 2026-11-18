package engine_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// These cases assert the ENGINE arms `internal/schedule`'s loop: that
// package's own suite cannot notice that nothing constructs one.

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
			brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		}),
		Company: c,
	})
}

// A COMPANY THAT DECLARES A SCHEDULE GETS A RUNNING TICK LOOP.
func TestACompanyWithASchedulePlansToFireIt(t *testing.T) {
	t.Parallel()
	e := scheduledEngine(t, scheduledCompany(t))
	if !e.SchedulerRunning() {
		t.Fatal("a company declaring a schedule armed no scheduler, so none of " +
			"its cron work will ever fire")
	}
}

// AND A COMPANY WITH NONE DOES NOT: an idle tick would claim a fleet duty
// every ten seconds for work that does not exist.
func TestACompanyWithNoSchedulesArmsNothing(t *testing.T) {
	t.Parallel()
	e := scheduledEngine(t, parsedCompany(t, companyDoc))
	if e.SchedulerRunning() {
		t.Fatal("a company with no schedules armed a scheduler")
	}
}

// THE OPERATOR'S OFF SWITCH IS HONOURED even when schedules exist, or
// `scheduling.enabled: false` would validate and do nothing.
func TestTheSchedulerRespectsItsOffSwitch(t *testing.T) {
	t.Parallel()
	c := scheduledCompany(t)
	c.Scheduling.Enabled = org.Off()
	if e := scheduledEngine(t, c); e.SchedulerRunning() {
		t.Fatal("scheduling.enabled: false still armed the scheduler")
	}
}

// THE FIRST SCHEDULE ADDED TO A LIVE COMPANY STARTS THE LOOP, and the last
// one removed stops it, with no restart.
func TestAddingTheFirstScheduleLiveArmsTheLoop(t *testing.T) {
	t.Parallel()
	e := scheduledEngine(t, parsedCompany(t, companyDoc))
	if e.SchedulerRunning() {
		t.Fatal("armed before any schedule existed")
	}

	withSchedule := scheduledCompany(t)
	if _, _, err := e.Apply(t.Context(), withSchedule, time.Now()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !e.SchedulerRunning() {
		t.Fatal("adding the first schedule to a live company did not arm the " +
			"loop, so it fires nothing until the process restarts")
	}

	// ...and back again.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc), time.Now()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if e.SchedulerRunning() {
		t.Fatal("removing the last schedule left the loop running and its " +
			"fleet duty claimed")
	}
}

// A COMPANY WITH NO MODEL ARMS NO LOOP, AND ITS FIRST PROVIDER ARMS ONE. Its
// inboxes hold their work until a provider exists, so a loop ticking through
// the wait would replay the whole backlog when one arrived, the replay the
// catchup window refuses.
func TestSchedulesWaitForTheCompanysFirstModel(t *testing.T) {
	t.Parallel()
	const noModels = `
name: Acme
roles:
  - name: CTO
    handle: cto
    schedules:
      - name: standup
        cron: "30 9 * * *"
        task: "Post the standup thread"
`
	e := scheduledEngine(t, parsedCompany(t, noModels))
	if e.SchedulerRunning() {
		t.Fatal("a company with no model provider armed its scheduler, so its " +
			"fires pile up behind inboxes that cannot take a turn")
	}

	if _, _, err := e.Apply(t.Context(), scheduledCompany(t), time.Now()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !e.SchedulerRunning() {
		t.Fatal("the apply that added a provider left the schedules idle until " +
			"the process restarts")
	}

	// ...and removing every provider stops the loop again.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, noModels), time.Now()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if e.SchedulerRunning() {
		t.Fatal("a revision that removed every provider left the loop firing " +
			"into inboxes that hold their work")
	}
}

// STOPPING THE ENGINE STOPS THE LOOP. A tick outliving its engine would
// publish into a closing queue, and its ledger row would stop any peer firing
// that run.
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

// A SCHEDULED TURN'S DEFAULT CAP OUTLASTS WHAT THE TURN SPENDS BEFORE ITS RUN.
//
// [org.DefaultScheduleTimeout] runs from the turn's start, and before the
// executor's run can act the turn prefetches (an auxiliary call's bound),
// starts its box (a container start, cold image pull included) and applies
// setup (one command's default allowance). A cap at or under their sum ends a
// ritual's run before it does anything.
//
// Three constants in three packages, so only the ORDER is asserted, as
// TestAnIdleAskOutlastsTheSilenceOfOneStillOwed does: the run's own need is no
// constant, and a demanded multiple would fail a sound change. The rest of the
// derivation is at the definition.
func TestADefaultScheduleCapOutlastsTheTurnBeforeItsRun(t *testing.T) {
	t.Parallel()
	setup := time.Duration(org.DefaultSetupTimeoutSeconds * float64(time.Second))
	before := prefetch.AuxTimeout + sandbox.ControlTimeout + setup
	if org.DefaultScheduleTimeout <= before {
		t.Errorf("a schedule with no timeout_seconds is capped at %v, and its turn can spend %v "+
			"before its executor's run starts (an auxiliary prefetch call %v, a container's "+
			"start %v, one setup command's default allowance %v).\nThe cap would end a "+
			"ritual's run before it had done anything. Raise DefaultScheduleTimeout, or "+
			"shorten what it outlasts.",
			org.DefaultScheduleTimeout, before, prefetch.AuxTimeout, sandbox.ControlTimeout, setup)
	}
}
