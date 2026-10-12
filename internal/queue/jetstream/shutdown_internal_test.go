package jetstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
)

// stopRecorder is a server that records what is done to it, and whether its
// scratch directory was still there when the shutdown was waited for.
type stopRecorder struct {
	scratch string

	mu            sync.Mutex
	calls         []string
	scratchAtWait bool
}

func (r *stopRecorder) Shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "Shutdown")
}

func (r *stopRecorder) WaitForShutdown() {
	_, err := os.Stat(r.scratch)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "WaitForShutdown")
	r.scratchAtWait = err == nil
}

func (r *stopRecorder) seen() ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls), r.scratchAtWait
}

// A SHUTDOWN WAITS FOR THE START IT OVERLAPS, and only then stops the server
// and removes its scratch store.
//
// nats-server's Start checks for no shutdown: run after one, it lets the
// server start goroutines again and enables JetStream, so a server shut down
// mid-start comes up behind its own WaitForShutdown — store open, and a
// scratch directory just removed made again. Only the two failure paths of a
// boot can overlap a Start, and nothing else would notice.
//
// In a synctest bubble, so "the teardown did what it would do while Start was
// still running" is [synctest.Wait] rather than a sleep: shutdownAndClean is
// either blocked on the Start or has already called Shutdown, and the case
// sees which on every run.
func TestAShutdownWaitsForTheStartItOverlaps(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		scratch := filepath.Join(t.TempDir(), "scratch")
		if err := os.Mkdir(scratch, 0o700); err != nil {
			t.Fatal(err)
		}
		srv := &stopRecorder{scratch: scratch}
		started := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			shutdownAndClean(srv, started, scratch)
		}()

		synctest.Wait()
		if calls, _ := srv.seen(); len(calls) > 0 {
			t.Fatalf("the shutdown began while Start was still running: %v", calls)
		}
		if _, err := os.Stat(scratch); err != nil {
			t.Fatalf("the scratch store was removed while Start was still running: %v", err)
		}

		close(started)
		<-done
		calls, scratchAtWait := srv.seen()
		if want := []string{"Shutdown", "WaitForShutdown"}; !slices.Equal(calls, want) {
			t.Fatalf("once Start returned the server saw %v, want %v", calls, want)
		}
		if !scratchAtWait {
			t.Error("the scratch store was removed before the shutdown was waited for")
		}
		if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the scratch store is still there after the shutdown: %v", err)
		}
	})
}

// A CANCELLED START LEAVES NO STORE BEHIND.
//
// The real server, cancelled before it could become ready: the path a Ctrl-C
// during a boot takes. Its scratch store lives under TMPDIR, which this case
// points at a directory of its own, so anything a start left there — the
// scratch directory itself, or one made again by a Start that outlived the
// shutdown — is found by name.
//
// THE CANCEL MAY LOSE, and that is not a failure: a fresh scratch store is
// ready in milliseconds, so on a loaded machine the readiness answer can
// arrive before the start reaches its select, and the select picks between
// two ready cases. The server it returns then is up and the caller's to stop,
// and stopping it must leave nothing either. Which path ran is the one thing
// this does not pin; [TestAShutdownWaitsForTheStartItOverlaps] pins the order
// the cancel path depends on.
func TestACancelledStartLeavesNoStoreBehind(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e, err := startEmbedded(ctx, Config{ServerName: "cancelled"})
	switch {
	case err == nil:
		e.shutdown()
	case !errors.Is(err, context.Canceled):
		t.Fatalf("a cancelled start failed with %v, want the cancellation", err)
	}
	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range left {
		t.Errorf("a cancelled start left %s in its TMPDIR", entry.Name())
	}
}
