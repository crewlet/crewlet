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
// nats-server's Start checks for no shutdown, so a server shut down mid-start
// comes up behind its own WaitForShutdown: JetStream on, store open, and the
// removed scratch directory made again. Only a boot's two failure paths can
// overlap a Start, and nothing else would notice.
//
// In a synctest bubble, so [synctest.Wait], not a sleep, shows whether
// shutdownAndClean is blocked on the Start or has already called Shutdown.
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

// A CANCELLED START LEAVES NO STORE BEHIND: the real server, cancelled before
// ready, as by a Ctrl-C during a boot. TMPDIR is the case's own directory, so
// anything a start left there — its scratch directory, or one a Start that
// outlived the shutdown made again — is found by name.
//
// THE CANCEL MAY LOSE, and that is no failure: a fresh scratch store is ready
// in milliseconds, so the start's select may pick readiness; the server it
// returns is the caller's to stop, and stopping it must leave nothing either.
// [TestAShutdownWaitsForTheStartItOverlaps] pins the order the cancel path
// depends on.
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
