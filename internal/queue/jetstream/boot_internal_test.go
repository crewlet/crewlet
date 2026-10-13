package jetstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// A STORE RECOVERY LONGER THAN THE LISTENER BUDGET STILL BOOTS.
//
// # The failure this pins
//
// A start's budget used to bound nats-server's whole Start, and on a store
// worth the name Start is its recovery — linear in what the store holds, and
// the object store has no ceiling. A recovery that outran the budget failed
// the boot, after waiting for it anyway (nats-server cannot stop a recovery),
// and the next boot recovered the same store again: a store whose recovery
// was longer than the budget could never boot. The budget now bounds the
// listeners alone, which open after Start has returned.
//
// # The input
//
// A real store whose recovery is HELD: one stream's metadata file replaced by
// a named pipe, which nats-server's recovery opens and reads, and which blocks
// it until this test opens the other end. Held for five listener budgets, then
// let go — the pipe gives up nothing, so nats-server skips that stream as
// unreadable and finishes the start.
func TestARecoveryLongerThanTheListenerBudgetStillBoots(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	seedAStream(t, store)
	metas, err := filepath.Glob(filepath.Join(store, "jetstream", "*", "streams", "*", "meta.inf"))
	if err != nil || len(metas) != 1 {
		t.Fatalf("the seeded stream's metadata file: %v, %v", metas, err)
	}
	if err := os.Remove(metas[0]); err != nil {
		t.Fatalf("remove the metadata file: %v", err)
	}
	if err := syscall.Mkfifo(metas[0], 0o600); err != nil {
		t.Fatalf("plant a pipe where the metadata file was: %v", err)
	}
	// OPENED READ-WRITE TO LET GO, which never blocks and is a writer: the
	// recovery's read then ends. Also on the way out, so a case that fails
	// with the recovery still held does not leave it blocked.
	var once sync.Once
	release := func() {
		once.Do(func() {
			if f, err := os.OpenFile(metas[0], os.O_RDWR, 0); err == nil {
				f.Close()
			}
		})
	}
	t.Cleanup(release)

	timing := startTiming{listen: 200 * time.Millisecond, report: 50 * time.Millisecond}
	type outcome struct {
		srv *Server
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		srv, err := StartServer(t.Context(), Config{StoreDir: store, start: timing})
		done <- outcome{srv, err}
	}()

	select {
	case got := <-done:
		if got.srv != nil {
			got.srv.Shutdown()
		}
		t.Fatalf("the start ended while its recovery was still held: %v", got.err)
	case <-time.After(5 * timing.listen):
	}
	release()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("a recovery longer than the listener budget failed the boot: %v",
				got.err)
		}
		got.srv.Shutdown()
	case <-time.After(time.Minute):
		t.Fatal("the start never finished after its recovery was let go")
	}
}

// seedAStream leaves a file store in dir holding one stream, so the next start
// on it has something to recover.
func seedAStream(t *testing.T, dir string) {
	t.Helper()
	srv, err := StartServer(t.Context(), Config{StoreDir: dir})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Shutdown()
	nc, err := srv.Conn()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{
		Name: "HELD", Subjects: []string{"held.>"}, Storage: jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("create a stream: %v", err)
	}
}

// EVERY UNSET START TIMING IS PRODUCTION'S, FIELD BY FIELD.
//
// Resolved as a whole struct, a case that set only the listener budget kept a
// report interval of zero, and the boot's ticker panics on that rather than
// failing the case.
func TestAStartTimingFillsEachUnsetFieldOnItsOwn(t *testing.T) {
	t.Parallel()
	production := startTiming{listen: listenTimeout, report: startReportEvery}
	for _, c := range []struct {
		name     string
		set, got startTiming
	}{
		{"nothing set", startTiming{}, production},
		{"only the listener budget", startTiming{listen: time.Second},
			startTiming{listen: time.Second, report: startReportEvery}},
		{"only the report interval", startTiming{report: time.Second},
			startTiming{listen: listenTimeout, report: time.Second}},
		{"both", startTiming{listen: 2 * time.Second, report: time.Second},
			startTiming{listen: 2 * time.Second, report: time.Second}},
	} {
		if got := c.set.resolved(); got != c.got {
			t.Errorf("%s: %+v resolved to %+v, want %+v", c.name, c.set, got, c.got)
		}
	}
}

// startedAfter is a Start that returns after d.
func startedAfter(d time.Duration) <-chan struct{} {
	started := make(chan struct{})
	time.AfterFunc(d, func() { close(started) })
	return started
}

// THE BOOT'S TWO WAITS, EACH ENDED BY WHAT SHOULD END IT AND NOTHING ELSE.
//
// The start is waited for as long as it takes and reported while it runs; the
// listeners after it are bounded; the broker's own death ends either at once,
// even one that arrives with readiness; and a cancel ends the wait, never the
// start, which the caller's shutdown then waits for.
func TestTheBootWaitsTheStartOutAndBoundsTheListeners(t *testing.T) {
	t.Parallel()
	timing := startTiming{listen: 50 * time.Millisecond, report: 20 * time.Millisecond}
	readyAt := func(ok bool) func(time.Duration) bool {
		return func(time.Duration) bool { return ok }
	}

	t.Run("a start longer than the listener budget, reported while it runs", func(t *testing.T) {
		t.Parallel()
		var reports atomic.Int32
		err := awaitBoot(t.Context(), bootWait{
			started: startedAfter(10 * timing.listen), death: newDeath(),
			ready: readyAt(true), timing: timing,
			slow: func(time.Duration) { reports.Add(1) },
		})
		if err != nil {
			t.Fatalf("a start that outran the listener budget failed the boot: %v", err)
		}
		if reports.Load() == 0 {
			t.Error("a start that ran for ten listener budgets never said it was still starting")
		}
	})

	t.Run("a server that never reports its listeners ready", func(t *testing.T) {
		t.Parallel()
		err := awaitBoot(t.Context(), bootWait{
			started: startedAfter(0), death: newDeath(), ready: readyAt(false),
			timing: timing, slow: func(time.Duration) {},
		})
		if err == nil || errors.Is(err, ErrBrokerDied) {
			t.Fatalf("a server that never became ready answered %v", err)
		}
		if !strings.Contains(err.Error(), timing.listen.String()) {
			t.Errorf("the failure does not name the listener budget: %v", err)
		}
	})

	// THE DEATH WINS EVERY RACE WITH SUCCESS. Each case below hands the wait
	// a death and a success together, and a select picks between ready cases
	// at random, so each is asked enough times that an ordering which let
	// success through would turn up.
	const races = 64
	t.Run("a death declared by a Start that went on", func(t *testing.T) {
		t.Parallel()
		for range races {
			death := newDeath()
			// The one Fatalf of v2.15.1's Start that does not return: the
			// server goes on to finish its start past a failure it called
			// fatal — so both have happened when the wait looks.
			death.declare("Not allowed to enable JetStream on the system account")
			started := make(chan struct{})
			close(started)
			err := awaitBoot(t.Context(), bootWait{
				started: started, death: death, ready: readyAt(true),
				timing: timing, slow: func(time.Duration) {},
			})
			if !errors.Is(err, ErrBrokerDied) {
				t.Fatalf("a server that called a failure fatal and went on to "+
					"finish starting booted: %v", err)
			}
		}
	})
	// Unlike the case above, this one cannot hand the wait both at once: the
	// readiness answer comes from a goroutine the wait starts, and a wait
	// already parked on its select wakes to whichever lands first. It holds
	// the property — a death that lands with readiness fails the boot — for
	// the orderings a test can produce.
	t.Run("a death that came with readiness", func(t *testing.T) {
		t.Parallel()
		for range races {
			death := newDeath()
			err := awaitBoot(t.Context(), bootWait{
				started: startedAfter(0), death: death, timing: timing,
				// A Fatalf from a goroutine Start left behind — the route
				// listener's — landing as the readiness answer does.
				ready: func(time.Duration) bool {
					death.declare("Error listening on router port: %d - %v", 6222, "taken")
					return true
				},
				slow: func(time.Duration) {},
			})
			if !errors.Is(err, ErrBrokerDied) {
				t.Fatalf("a server that declared itself dead as it reported "+
					"ready booted: %v", err)
			}
		}
	})

	t.Run("a death while starting ends the wait at once", func(t *testing.T) {
		t.Parallel()
		death := newDeath()
		time.AfterFunc(timing.listen, func() { death.declare("Can't start JetStream: %v", "no disk") })
		begun := time.Now()
		err := awaitBoot(t.Context(), bootWait{
			started: make(chan struct{}), death: death, ready: readyAt(true),
			timing: timing, slow: func(time.Duration) {},
		})
		if !errors.Is(err, ErrBrokerDied) {
			t.Fatalf("a death during the start answered %v", err)
		}
		if took := time.Since(begun); took > 10*time.Second {
			t.Errorf("the death was answered after %s", took)
		}
	})

	t.Run("a cancel ends the wait and not the start", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(timing.listen, cancel)
		err := awaitBoot(ctx, bootWait{
			started: make(chan struct{}), death: newDeath(), ready: readyAt(true),
			timing: timing, slow: func(time.Duration) {},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled boot answered %v", err)
		}
	})
}
