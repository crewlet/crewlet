package engine_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A NODE THAT BOOTED WITH NO COMPANY BRINGS ITS NATIVE BACKENDS UP WITH THE
// FIRST ONE IT IS HANDED — the tracker, the knowledge base, their tools, the
// chart, and the loops a boot arms beside them — with no restart.
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
	// runtime the apply below brings up.
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if e.TrackerWriter() != nil || e.PagesStore() != nil {
		t.Fatal("the premise: a node with no company runs no native backend")
	}
	if _, ok := e.RetentionReport(t.Context()); ok {
		t.Fatal("the premise: a node with no state log has no trim")
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
		t.Fatal("the first company's apply left the node with no native tracker " +
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
	for !e.NativeHydrated(t.Context()) {
		if time.Now().After(deadline) {
			t.Fatal("the node never admitted seats on its new runtime")
		}
		time.Sleep(20 * time.Millisecond)
	}
	filed, err := writer.CreateTask(t.Context(), statelog.NewOpID(time.Now(), "create"),
		tracker.Task{
			V: tracker.DocumentVersion, ID: "t-first", Project: "ENG",
			Title: "the first task", Status: tracker.StatusTodo,
			Priority: tracker.PriorityNormal,
		}, nil)
	if err != nil {
		t.Fatalf("a task filed into the chart's project: %v", err)
	}
	if filed.Key != "ENG-1" {
		t.Errorf("the first task was keyed %q, want ENG-1", filed.Key)
	}

	// THE LOOPS A BOOT ARMS BESIDE THE RUNTIME: the trim, and the sweep
	// rebuilt with the tracker's repairs and every operation ledger.
	if _, ok := e.RetentionReport(t.Context()); !ok {
		t.Error("the node's new state log has no trim — its logs only grow")
	}
	jobs := e.Maintenance().Jobs()
	for _, job := range []string{"tracker_respread", "tracker_ops", "pages_ops"} {
		if !slices.Contains(jobs, job) {
			t.Errorf("the sweep runs %v, which does not include %s", jobs, job)
		}
	}
}

// unconfiguredEngineOn is an engine with no company over a bootstrap the case
// shapes.
func unconfiguredEngineOn(t *testing.T, mutate func(*config.Bootstrap)) *engine.Engine {
	t.Helper()
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: bootstrap(t, mutate)})
	if err != nil {
		t.Fatalf("an engine with no company was refused: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}

// A REVISION WHOSE NATIVE BACKEND WOULD LOG TO AN IN-MEMORY STREAM IS REFUSED
// AT THE APPLY, as it is at boot — before anything is started.
//
// The check ran at boot alone, so a node that booted unconfigured accepted
// such a company; now that an apply brings the native runtime up, it would
// start the log whose first restart leaves the node unable to serve for good.
func TestAnApplyRefusesANativeBackendOnAnInMemoryStream(t *testing.T) {
	t.Parallel()
	e := unconfiguredEngineOn(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = ""
	})
	status, _, err := e.Apply(t.Context(), parsedCompany(t, chartCompany("builds it")),
		time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC))
	if err == nil || status != configplane.StatusError {
		t.Fatalf("Apply = (%s, %v), want a refusal", status, err)
	}
	if e.TrackerWriter() != nil {
		t.Error("the refused revision started a native tracker on an in-memory log")
	}
	if e.Company() != nil {
		t.Error("the refused revision was installed")
	}
}
