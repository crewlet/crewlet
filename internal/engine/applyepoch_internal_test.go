package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
)

// THE EPOCH EVERY APPLIER READS IS THE PUBLISHED COMPANY'S, AS IT STANDS NOW.
//
// The state log is the engine's core and is built once, at boot — with no
// company at all on a node started unconfigured — and every domain's runner
// asks this source per batch ([statelog.RunnerDeps.Epoch]). It was handed the
// boot company's epoch as a value, so a node that booted with none applied
// every record under none for the life of the process, and a revision that
// moved the tracker's inbox retention reached no applier until a restart.
// statelog certifies that a runner asks per batch; this certifies that what it
// asks is the company this node has published, through two activations and no
// restart.
//
// Mutation: hand the log `company.Epoch()` captured at boot, and the first
// activation's value is never read.
func TestTheLogsEpochFollowsThePublishedCompany(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	e, err := New(t.Context(), Options{Bootstrap: &b})
	if err != nil {
		t.Fatalf("New with no company: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	const key = "tracker.native.inbox_retention_days"
	epochOf := func() map[string]any {
		t.Helper()
		c := e.core.Load()
		if c == nil || c.log == nil || c.log.epoch == nil {
			t.Fatal("the node's state log holds no epoch source")
		}
		return c.log.epoch()
	}
	if got, held := epochOf()[key]; held {
		t.Fatalf("a node with no company hands its appliers %s = %v", key, got)
	}

	for _, days := range []int{90, 45} {
		cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
		if err != nil {
			t.Fatalf("parse the company: %v", err)
		}
		cfg.Tracker.Native = &config.TrackerNativeConfig{InboxRetentionDays: days}
		status, stages, err := e.Apply(t.Context(), cfg)
		if err != nil || status != configplane.StatusOK {
			t.Fatalf("Apply (%d days) = (%s, %v, %v), want ok", days, status, stages, err)
		}
		if got := epochOf()[key]; got != days {
			t.Errorf("after activating %d days the appliers read %s = %v — "+
				"the epoch reaches them only at a restart", days, key, got)
		}
	}
}
