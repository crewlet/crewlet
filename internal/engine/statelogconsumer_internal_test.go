package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// A NODE WHOSE ROWS WENT BACKWARDS REPLAYS WHAT ITS READER HAD ALREADY
// ACKNOWLEDGED, and hydrates.
//
// # The node this is
//
// Its replicated database was deleted and rebuilt — or restored from a copy
// older than the broker estate beside it, which is what a backup restore is,
// since a backup copies the store first — under the SAME node id, on a broker
// that kept the node's consumer of every log. The rows resume at nothing; the
// consumers had delivered and acknowledged everything the last run applied.
// The broker never hands an acknowledged record over again, so the node sat
// unhydrated for ever: `applying: 2 record(s) behind the log's head` at every
// sample to forty-five seconds, the knowledge base's boot refusing on a home
// container it could not see, and nothing below the floor for the heartbeat to
// notice, because the records were all still on the log.
//
// # The bound
//
// Far inside the consumer's thirty-second ack window, so a fix that merely
// waited for a redelivery would fail it as surely as the node that never got
// one. Measured against the unfixed build it fails on the hydration wait.
func TestANodeWhoseRowsWentBackwardsReplaysWhatItsReaderAcknowledged(t *testing.T) {
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

	// THE FIRST RUN applies everything its own boot wrote, which is what
	// leaves each consumer acknowledged through the head of its log.
	// AN ACTIVATED COMPANY, so its boot writes its chart and its knowledge
	// spaces: those are the records this case needs on every log it can.
	first, err := New(t.Context(), Options{
		Bootstrap: &b, Company: cfg, Backends: back, ActivatedAt: testActivation,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	heads := map[string]uint64{}
	waitUntil(t, 20*time.Second, "the first run to apply every log", func() bool {
		for _, domain := range registeredDomains() {
			running := first.native.Load().log.Domain(domain.Name())
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
	path := back.Store.ReplicatedPath()
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
	if err := back.Store.ReopenReplicated(t.Context()); err != nil {
		t.Fatalf("reopen the replicated estate: %v", err)
	}

	// THE SECOND RUN, the same node on the same broker.
	second, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New again: %v", err)
	}
	t.Cleanup(func() { second.Stop(context.Background()) })
	waitUntil(t, 10*time.Second, "the node whose rows went backwards to hydrate "+
		"again — its consumers had acknowledged every record it is missing, and "+
		"a consumer the broker was told is done never hands them over",
		hydrated(t, second))
	for name, head := range heads {
		running := second.native.Load().log.Domain(name)
		if got := running.runner.Committed().Seq; got < head {
			t.Errorf("%s's rows are at %d after the replay, below the %d the "+
				"first run had applied", name, got, head)
		}
	}
}
