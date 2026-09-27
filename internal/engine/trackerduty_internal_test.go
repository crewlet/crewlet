package engine

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// THE ENGINE HANDS THE TRACKER DUTY EVERY DEPENDENCY IT NEEDS, THE READ
// AUTHORITY AMONG THEM.
//
// The merge repair decides from a linearizable read, and the tracker's own
// tests build the duty with a read authority of their own — so nothing in them
// can see whether the engine hands one over. [tracker.Jobs] refuses a missing
// dependency by name when the jobs are built, so asking the engine's own
// wiring for them is asking exactly that.
//
// Mutation: drop `Reader:` from trackerJobs and this fails naming
// DutyDeps.Reader.
func TestTheEngineHandsTheTrackerDutyItsReadAuthority(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if e.native == nil || e.native.writer == nil {
		t.Fatal("the engine runs no native tracker, so it arms no tracker duty " +
			"and this case is not the shape it names")
	}

	jobs, err := e.trackerJobs()
	if err != nil {
		t.Fatalf("the engine's own wiring of the tracker duty was refused: %v", err)
	}
	names := make([]string, 0, len(jobs))
	for _, job := range jobs {
		names = append(names, job.Name)
	}
	if !slices.Contains(names, "tracker_abandoned_merges") {
		t.Errorf("the tracker duty the engine arms is %v, with no merge repair", names)
	}
}
