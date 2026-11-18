package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// A NODE WHOSE ROWS WENT BACKWARDS REPLAYS WHAT ITS READER HAD ALREADY
// ACKNOWLEDGED, and hydrates.
//
// Its replicated database was deleted, or restored from a copy older than the
// broker beside it (a backup copies the store first), under the SAME node id,
// on a broker that kept the node's consumer of every log. Those consumers had
// acknowledged everything the last run applied, and the broker never hands an
// acknowledged record over again, and nothing falls below the floor for the
// heartbeat to notice, so without a replay the node never hydrates. The waits
// are far inside the consumer's thirty-second ack window, so a fix that merely
// waited for a redelivery fails as surely as no fix.
func TestANodeWhoseRowsWentBackwardsReplaysWhatItsReaderAcknowledged(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })

	// THE FIRST RUN applies everything its own boot wrote, leaving each
	// consumer acknowledged through its log's head. A company with units,
	// booted at an activation, writes their projects and knowledge spaces:
	// records on every log it can.
	activated := time.Now().UTC()
	first, err := New(t.Context(), Options{
		Bootstrap: &b, Company: cfg, Backends: back, ActivatedAt: activated,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	heads := map[string]uint64{}
	waitUntil(t, 20*time.Second, "the first run to apply every log", func() bool {
		for _, domain := range registeredDomains() {
			running := first.core.Load().log.Domain(domain.Name())
			if running == nil {
				return false
			}
			_, end, err := running.log.Bounds(t.Context())
			if err != nil || running.runner.Committed().Seq != end {
				return false
			}
			heads[domain.Name()] = end
		}
		return true
	})
	written := uint64(0)
	for _, end := range heads {
		written += end
	}
	if written == 0 {
		t.Fatal("the first run wrote nothing to any log, so there is nothing " +
			"for the second to be missing and this case proves nothing — if " +
			"the boot stopped writing its own records, stage some here")
	}
	first.Stop(context.Background())

	// THE ROWS GO. The broker, and every consumer on it, stays.
	path := back.Store.ReplicatedFile()
	if err := back.Store.CloseReplicated(); err != nil {
		t.Fatalf("close the replicated estate: %v", err)
	}
	files, err := filepath.Glob(path + "*")
	if err != nil || len(files) == 0 {
		t.Fatalf("found no replicated estate at %s to delete (%v)", path, err)
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			t.Fatalf("delete %s: %v", f, err)
		}
	}
	if _, err := back.Store.OpenReplicated(t.Context(), estateLogs()); err != nil {
		t.Fatalf("reopen the replicated estate: %v", err)
	}

	// THE SECOND RUN, the same node on the same broker.
	second, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back,
		ActivatedAt: activated})
	if err != nil {
		t.Fatalf("New again: %v", err)
	}
	t.Cleanup(func() { second.Stop(context.Background()) })
	waitUntil(t, 10*time.Second, "the node whose rows went backwards to hydrate "+
		"again — its consumers had acknowledged every record it is missing, and "+
		"a consumer the broker was told is done never hands them over",
		hydrated(t, second))

	// EVERY LOG REPLAYS, not only the ones hydration waits for: hydration
	// reads only the domains whose health gates seat admission, so the
	// others (artifacts, IAM, usage, search) may still be applying when it
	// reports true and are waited on to the first run's head.
	behind := map[string]uint64{}
	waitUntil(t, 10*time.Second, "every log to replay to the head the first run "+
		"had applied", func() bool {
		clear(behind)
		for name, head := range heads {
			running := second.core.Load().log.Domain(name)
			if got := running.runner.Committed().Seq; got < head {
				behind[name] = got
			}
		}
		return len(behind) == 0
	})
}
