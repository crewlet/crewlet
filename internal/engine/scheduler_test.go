package engine_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
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
// nothing until the next restart.
//
// # It is added through the CHART, because that is where a schedule lives
//
// A seat's `schedules:` are part of the seat, and a seat is the org chart's.
// So the gesture that gives somebody their first standup is a chart write, not
// a config apply — and a loop armed only on an apply would fire nothing until
// the next one, which on a company nobody is reconfiguring is never. The
// company a chart write publishes is a company like any other, so every loop
// that follows the epoch has to follow that too.
func TestAddingTheFirstScheduleLiveArmsTheLoop(t *testing.T) {
	t.Parallel()
	e := scheduledEngine(t, parsedCompany(t, companyDoc))
	if e.SchedulerRunning() {
		t.Fatal("armed before any schedule existed")
	}

	setSeatSchedules(t, e, "cto", []org.Schedule{{
		Name: "standup", Cron: "30 9 * * *", Task: "Post the standup thread",
	}})
	if !e.SchedulerRunning() {
		t.Fatal("adding the first schedule to a live company did not arm the " +
			"loop, so it fires nothing until the process restarts")
	}

	// ...and back again.
	setSeatSchedules(t, e, "cto", nil)
	if e.SchedulerRunning() {
		t.Fatal("removing the last schedule left the loop running and its " +
			"fleet duty claimed")
	}
}

// A COMPANY WITH NO MODEL ARMS NO LOOP, AND ITS FIRST PROVIDER ARMS ONE. Every
// fire is work on a seat's inbox, and that company's inboxes hold their work
// until a provider exists. A loop ticking through the wait would stack every
// missed standup behind the hold and run the whole backlog the moment the
// provider arrived, which is the replay the catchup window exists to refuse.
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

	if _, _, err := e.Apply(t.Context(), scheduledCompany(t)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !e.SchedulerRunning() {
		t.Fatal("the apply that added a provider left the schedules idle until " +
			"the process restarts")
	}

	// ...and removing every provider stops the loop again.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, noModels)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if e.SchedulerRunning() {
		t.Fatal("a revision that removed every provider left the loop firing " +
			"into inboxes that hold their work")
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

// setSeatSchedules rewrites one seat's schedules on the chart and waits for
// the company view to carry the change.
//
// THROUGH THE SEAT'S RUNTIME DOCUMENT, which is where everything the chart has
// no column for lives — a model chain, a credential, a schedule. The row's own
// fields are restated because a content write is a FULL POST-STATE: a field
// left empty is a field set to empty.
func setSeatSchedules(t *testing.T, e *engine.Engine, handle string, each []org.Schedule) {
	t.Helper()
	writer := e.ChartWriter()
	if writer == nil {
		t.Fatal("this engine runs no chart writer")
	}
	seat := e.Company().Org.Role(handle)
	if seat == nil {
		t.Fatalf("%s is not a seat in this company", handle)
	}
	edited := *seat
	edited.Schedules = each
	runtime, err := org.SeatRuntime(&edited)
	if err != nil {
		t.Fatalf("encode the seat's runtime: %v", err)
	}
	if _, err := writer.WriteSeat(t.Context(),
		fmt.Sprintf("test:schedules:%s:%d", handle, len(each)),
		chart.SeatContent{
			Handle: handle, Kind: chart.SeatKind(edited.EffectiveKind()),
			Name: edited.Name, Email: edited.Email, Runtime: runtime,
		}); err != nil {
		t.Fatalf("write %s: %v", handle, err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if got := e.Company().Org.Role(handle); got != nil &&
			len(got.Schedules) == len(each) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the schedule change on %s never reached the view", handle)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
