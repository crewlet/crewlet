package jetstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// A STORE RECOVERY LONGER THAN THE LISTENER BUDGET STILL BOOTS: the budget
// bounds the listeners alone, never nats-server's Start, whose recovery is
// linear in the store and cannot be stopped — a store whose recovery outran it
// could otherwise never boot.
//
// The recovery is HELD by a named pipe in place of one stream's metadata file,
// from the recovery's open of it until the boot's own report says the start ran
// five listener budgets; then let go, and nats-server skips that stream. Held
// by the boot's clock, not the test's, so a late-scheduled boot can neither
// hang on the pipe nor be held for less than the budget.
//
// NOT PARALLEL: the report is a line on the process's logger.
func TestARecoveryLongerThanTheListenerBudgetStillBoots(t *testing.T) {
	reports := captureBootReports(t)
	store := brokertest.TempDir(t)
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
	// THE PIPE'S OTHER END: the open returns once the recovery has opened
	// the pipe, whose read then blocks until this end closes. A nil file is
	// an open that failed.
	writer := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(metas[0], os.O_WRONLY, 0)
		if err != nil {
			writer <- nil
			return
		}
		writer <- f
	}()
	// AND LET GO however the case ends: the held end closed, or — where the
	// recovery never opened the pipe — a reader of the case's own lets the
	// waiting open return so its end can be closed.
	var held *os.File
	opened := false
	t.Cleanup(func() {
		if !opened {
			r, err := os.OpenFile(metas[0], os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				return
			}
			defer r.Close()
			held = <-writer
		}
		if held != nil {
			_ = held.Close()
		}
	})

	timing := startTiming{listen: 200 * time.Millisecond, report: 50 * time.Millisecond}
	// FIVE LISTENER BUDGETS, a second: the report counts whole seconds.
	holdFor := 5 * timing.listen
	type outcome struct {
		srv *Server
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		srv, err := StartServer(t.Context(), Config{StoreDir: store, StoreMaxBytes: brokertest.StoreMaxBytes,
			start: timing})
		done <- outcome{srv, err}
	}()

	// HELD, AND REPORTED PAST THE BUDGET, with the start still running.
	endedWhileHeld := func(got outcome) {
		t.Helper()
		if got.srv != nil {
			got.srv.Shutdown()
		}
		t.Fatalf("the start ended while its recovery was still held: %v", got.err)
	}
	bound := time.NewTimer(time.Minute) //fixedwait:liveness
	defer bound.Stop()
	for held == nil || !reports.ranFor(store, holdFor) {
		select {
		case f := <-writer:
			opened = true
			if f == nil {
				t.Fatal("the pipe's other end could not be opened, so the case holds nothing")
			}
			held = f
		case <-reports.wrote:
		case got := <-done:
			endedWhileHeld(got)
		case <-bound.C:
			t.Fatalf("a minute in, the recovery had opened the held pipe: %v, and "+
				"the boot had said its start ran %v: %v", held != nil, holdFor,
				reports.ranFor(store, holdFor))
		}
	}
	select {
	case got := <-done:
		endedWhileHeld(got)
	default:
	}
	_ = held.Close()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("a recovery longer than the listener budget failed the boot: %v",
				got.err)
		}
		got.srv.Shutdown()
	case <-time.After(time.Minute): //fixedwait:liveness
		t.Fatal("the start never finished after its recovery was let go")
	}
}

// bootReports is what the process's logger writes while one case points it
// here: every record, and a wake for a reader that is behind.
type bootReports struct {
	mu      sync.Mutex
	written []byte
	wrote   chan struct{}
}

// captureBootReports points the process's logger at a bootReports, in JSON,
// until the case ends.
func captureBootReports(t *testing.T) *bootReports {
	r := &bootReports{wrote: make(chan struct{}, 1)}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, r)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	return r
}

func (r *bootReports) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.written = append(r.written, p...)
	r.mu.Unlock()
	select {
	case r.wrote <- struct{}{}:
	default:
	}
	return len(p), nil
}

// ranFor reports whether the boot of the broker on store has said its start
// was still running after at least d — in the line it writes every report
// interval, which counts whole seconds.
func (r *bootReports) ranFor(store string, d time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for line := range bytes.Lines(r.written) {
		var report struct {
			Msg            string `json:"msg"`
			StoreDir       string `json:"store_dir"`
			ElapsedSeconds int    `json:"elapsed_seconds"`
		}
		if json.Unmarshal(line, &report) == nil && report.Msg == "embedded_broker_starting" &&
			report.StoreDir == store && time.Duration(report.ElapsedSeconds)*time.Second >= d {
			return true
		}
	}
	return false
}

// seedAStream leaves a file store in dir holding one stream, so the next start
// on it has something to recover.
func seedAStream(t *testing.T, dir string) {
	t.Helper()
	srv, err := StartServer(t.Context(), Config{StoreDir: dir, StoreMaxBytes: brokertest.StoreMaxBytes})
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

// EVERY UNSET START TIMING IS PRODUCTION'S, FIELD BY FIELD: resolved as a
// whole struct, a case setting only the listener budget would keep a zero
// report interval, on which the boot's ticker panics.
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
//
//fixedwait:bubble
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
//
// EVERY CASE RUNS IN A BUBBLE, because each claim is about the boot's clock:
// there the reports are counted exactly, and a death is answered at the
// instant it was declared. Mutation: drop the report, report a zero running
// time, or answer a death one report late, and a case fails.
func TestTheBootWaitsTheStartOutAndBoundsTheListeners(t *testing.T) {
	t.Parallel()
	timing := startTiming{listen: 50 * time.Millisecond, report: 20 * time.Millisecond}
	readyAt := func(ok bool) func(time.Duration) bool {
		return func(time.Duration) bool { return ok }
	}

	t.Run("a start longer than the listener budget, reported while it runs", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			lasts := 10 * timing.listen
			var reports []time.Duration
			err := awaitBoot(t.Context(), bootWait{
				started: startedAfter(lasts), death: newDeath(),
				ready: readyAt(true), timing: timing,
				slow: func(running time.Duration) { reports = append(reports, running) },
			})
			if err != nil {
				t.Fatalf("a start that outran the listener budget failed the boot: %v", err)
			}
			// ONE REPORT FOR EVERY INTERVAL THE START RAN: the last ends as
			// the start does, and the wait may take either first.
			if every := int(lasts / timing.report); len(reports) < every-1 {
				t.Fatalf("a start that ran for ten listener budgets said it was still "+
					"starting %d times, want one for each of the %d report intervals "+
					"it ran", len(reports), every-1)
			}
			for i, running := range reports {
				if want := time.Duration(i+1) * timing.report; running != want {
					t.Errorf("report %d said the start had run %v, want %v", i+1, running, want)
				}
			}
		})
	})

	t.Run("a server that never reports its listeners ready", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
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
	})

	// THE DEATH WINS EVERY RACE WITH SUCCESS. Each case hands the wait both
	// together, and a select picks at random, so each is asked enough times
	// that an ordering letting success through would turn up.
	const races = 64
	t.Run("a death declared by a Start that went on", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			for range races {
				death := newDeath()
				// The one Fatalf of v2.15.1's Start that does not return:
				// the server finishes its start past it, so both have
				// happened when the wait looks.
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
	})
	// This case cannot hand the wait both at once: readiness comes from a
	// goroutine the wait starts. It holds the property — a death landing with
	// readiness fails the boot — for the orderings a test can produce.
	t.Run("a death that came with readiness", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			for range races {
				death := newDeath()
				err := awaitBoot(t.Context(), bootWait{
					started: startedAfter(0), death: death, timing: timing,
					// A Fatalf from a goroutine Start left behind — the
					// route listener's — landing as the readiness answer
					// does.
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
	})

	t.Run("a death while starting ends the wait at once", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
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
			// AT THE INSTANT IT WAS DECLARED, which the bubble makes exact.
			if took := time.Since(begun); took != timing.listen {
				t.Errorf("a death declared %v into the start was answered at %v", timing.listen, took)
			}
		})
	})

	t.Run("a cancel ends the wait and not the start", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			time.AfterFunc(timing.listen, cancel)
			var waits atomic.Int32
			err := awaitBoot(ctx, bootWait{
				started: make(chan struct{}), death: newDeath(), ready: readyAt(true),
				timing: timing, slow: func(time.Duration) {},
				cancelWaits: func() { waits.Add(1) },
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("a cancelled boot answered %v", err)
			}
			if got := waits.Load(); got != 1 {
				t.Errorf("a cancel during a start still running said %d times that "+
					"the shutdown waits for it, want once", got)
			}
		})
	})

	// A CANCEL THAT LANDS AS THE START RETURNS waits for nothing, and must
	// not say it does: the select picks at random, so the case is asked
	// enough times that an announcement made regardless would turn up.
	t.Run("a cancel as the start returns announces no wait", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			started := make(chan struct{})
			close(started)
			var waits atomic.Int32
			for range races {
				// Either answer is right — the boot was cancelled, or it
				// finished first; only the announcement is under test.
				_ = awaitBoot(ctx, bootWait{
					started: started, death: newDeath(), ready: readyAt(true),
					timing: timing, slow: func(time.Duration) {},
					cancelWaits: func() { waits.Add(1) },
				})
			}
			if got := waits.Load(); got != 0 {
				t.Errorf("a cancel that landed after the start had returned said "+
					"%d times in %d that the shutdown waits for a start in progress",
					got, races)
			}
		})
	})
}
