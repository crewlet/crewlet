package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// THE EPOCH EVERY APPLIER READS IS THE PUBLISHED COMPANY'S, AS IT STANDS NOW.
// The state log is built once, at boot (with no company on a node started
// unconfigured), and each runner asks its epoch source per batch
// ([statelog.RunnerDeps.Epoch]); a source capturing the boot epoch would apply
// a revision's inbox retention only after a restart. statelog certifies the
// per-batch ask; this certifies the answer is the published company, through
// two activations and no restart.
//
// Mutation: hand the log `company.Epoch()` captured at boot, and the first
// activation's value is never read.
func TestTheLogsEpochFollowsThePublishedCompany(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	e, err := New(t.Context(), Options{Bootstrap: &b})
	if err != nil {
		t.Fatalf("New with no company: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	const key = inbox.RetentionKey
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
		cfg.Inbox = config.Inbox{RetentionDays: days}
		status, stages, err := e.Apply(t.Context(), cfg, time.Now())
		if err != nil || status != configplane.StatusOK {
			t.Fatalf("Apply (%d days) = (%s, %v, %v), want ok", days, status, stages, err)
		}
		if got := epochOf()[key]; got != days {
			t.Errorf("after activating %d days the appliers read %s = %v — "+
				"the epoch reaches them only at a restart", days, key, got)
		}
	}
}
