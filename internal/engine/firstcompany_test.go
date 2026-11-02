package engine_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A NODE THAT BOOTED WITH NO COMPANY BRINGS ITS NATIVE BACKENDS UP WITH THE
// FIRST ONE IT IS HANDED — the tracker, the knowledge base, their tools, the
// chart's projects and spaces, and the loops a boot arms beside them — with no
// restart.
//
// The quickstart's own path: start a node, then `PUT /config` or create the
// company from the dashboard. The runtime was built by boot alone, so a node
// started that way had none of it until the process restarted: every tool
// said the backend was not wired, and no project existed to file work into.
func TestAFirstCompanyBringsUpTheNativeBackendsWithoutARestart(t *testing.T) {
	t.Parallel()
	e := unconfiguredEngine(t)
	// RUNNING, as a node waiting for its first company is: the seat host is
	// already asking whether this node may take seats, which reads the very
	// log the apply below brings its halves up over.
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if e.TrackerWriter() != nil || e.PagesStore() != nil || e.NativeStarted() {
		t.Fatal("the premise: a node with no company runs no native backend")
	}

	activated := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	status, applied, err := e.Apply(t.Context(),
		parsedCompany(t, chartCompany("builds it")), activated)
	if err != nil || status != configplane.StatusOK {
		t.Fatalf("Apply = (%s, %v, %v), want ok", status, applied, err)
	}
	for _, stage := range []string{"native", "maintenance"} {
		if !slices.Contains(applied, stage) {
			t.Errorf("the apply got through %v, which does not name %q", applied, stage)
		}
	}

	// THE RUNTIME AND ITS TOOLS.
	writer := e.TrackerWriter()
	if writer == nil || e.PagesStore() == nil || e.Pages() == nil {
		t.Fatal("the first company's apply left the node with no Worklet " +
			"or knowledge base")
	}
	for _, tool := range []string{builtin.CreateWorkItemTool, builtin.WritePageTool} {
		if _, ok := e.Company().Tools.Lookup(tool); !ok {
			t.Errorf("the applied company has no %s tool — equip ran before the "+
				"runtime it registers against", tool)
		}
	}

	// THE CHART, stamped with the activation that brought it.
	purpose, epoch := eventuallyProject(t, e, "ENG")
	if purpose != "builds it" || epoch != configplane.ActivationStamp(activated) {
		t.Errorf("the chart's project is (%q, %d), want (\"builds it\", %d)",
			purpose, epoch, configplane.ActivationStamp(activated))
	}
	if space := eventuallyContainer(t, e, "ENGDOCS"); space.Purpose != "builds it" {
		t.Errorf("the unit's knowledge space reads %q", space.Purpose)
	}

	// A WORKING TRACKER: work filed into the chart's project lands.
	deadline := time.Now().Add(20 * time.Second)
	for !e.StateLogHydrated(t.Context()) {
		if time.Now().After(deadline) {
			t.Fatal("the node never admitted seats on its new runtime")
		}
		time.Sleep(20 * time.Millisecond)
	}
	filed, err := writer.CreateTask(t.Context(), statelog.NewOpID(time.Now(), "create"),
		worklet.Task{
			V: worklet.DocumentVersion, ID: "t-first", Project: "ENG",
			Title: "the first task", Status: worklet.StatusTodo,
			Priority: worklet.PriorityNormal,
		}, nil)
	if err != nil {
		t.Fatalf("a task filed into the chart's project: %v", err)
	}
	if filed.Key != "ENG-1" {
		t.Errorf("the first task was keyed %q, want ENG-1", filed.Key)
	}

	// AND THE SWEEP, rebuilt with the tracker's repairs beside the operation
	// ledgers the core contributed from boot.
	jobs := e.Maintenance().Jobs()
	for _, job := range []string{"tracker_respread", "tracker_ops", "pages_ops", "code_ops"} {
		if !slices.Contains(jobs, job) {
			t.Errorf("the sweep runs %v, which does not include %s", jobs, job)
		}
	}
}

// A NODE'S FIRST COMPANY IS CLAIMED AT ONCE, NOT AT THE SWEEP'S NEXT TICK.
//
// An apply is the one thing that changes the seat set and the one that knows
// it has, and nothing passed that on: a node started with no company, then
// handed one, ran none of its seats until placement's next tick noticed them,
// up to [seat.SweepInterval] later — every time a company was created from the
// dashboard.
//
// THE SWEEP'S NEXT TICK IS AN HOUR AWAY here, so nothing but the apply's own
// ask can claim, and the bound below is only how long a claim takes once the
// apply has returned. A deadline raced against the shipped five-second tick
// measured the APPLY instead: a node's first apply brings up the native
// backends over the logs the core started at boot, which takes most of five
// seconds on a loaded runner, and the case failed with the ask working exactly
// as it should.
func TestAFirstCompanysSeatsAreClaimedWithoutWaitingForTheSweep(t *testing.T) {
	t.Parallel()
	e, err := engine.New(t.Context(), engine.WithSweepEvery(engine.Options{
		Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
		}),
	}, time.Hour))
	if err != nil {
		t.Fatalf("an engine with no company was refused: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if status, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc),
		time.Now()); err != nil || status != configplane.StatusOK {
		t.Fatalf("Apply = (%s, %v)", status, err)
	}
	applied := time.Now()
	want := len(e.Company().Seats())
	if want == 0 {
		t.Fatal("the premise: the company has seats to claim")
	}
	// GENEROUS, because a claim attaches a mailbox and prepares the seat:
	// whatever it takes on a loaded machine, it is nowhere near the hour.
	deadline := applied.Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if len(e.Node().Host().Held()) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("held %v of %d seats %v after the apply returned, with the sweep's "+
		"next tick an hour away — nothing claimed the seats the apply added",
		e.Node().Host().Held(), want, time.Since(applied))
}
