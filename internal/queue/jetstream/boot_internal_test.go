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
// it for as long as this test holds the pipe's other end — from the moment the
// recovery opened the pipe, which is when the test's own open of that end
// returns, until the boot has said in its own report of a start still running
// that the start has run for a second: five listener budgets. Then let go —
// the pipe gives up nothing, so nats-server skips that stream as unreadable and
// finishes the start.
//
// # Held by the boot's own clock
//
// The hold used to be a second of the test's clock from when it started the
// boot, with the start asserted still running at its end. A boot the scheduler
// started late reached the pipe after the test had let go of it, and waited on
// it for ever: a healthy build failed. One that reached it late, but in time,
// was held for less than the budget, so a boot whose budget bounded the start
// passed. Read from the boot's report, the start has run past the budget by
// the boot's own measure, with the recovery held from its open of the pipe to
// the end of the hold: a budget that bounded the start has run out by then,
// and a boot that cut the start short has already ended.
//
// NOT PARALLEL, because the report is a log line and the logger is the
// process's: a case that points it somewhere has to be the only case running,
// as the engine's own cases that read a log line are.
func TestARecoveryLongerThanTheListenerBudgetStillBoots(t *testing.T) {
	reports := captureBootReports(t)
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
	// THE PIPE'S OTHER END, opened for writing on a goroutine: the open
	// returns only once the recovery has opened the pipe to read it, and the
	// recovery's read then blocks until that end is closed. A nil file is an
	// open that failed.
	writer := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(metas[0], os.O_WRONLY, 0)
		if err != nil {
			writer <- nil
			return
		}
		writer <- f
	}()
	// AND LET GO on the way out, however the case ends, so a case that fails
	// with the recovery still held does not leave it blocked: the held end
	// closed, or — where the recovery never opened the pipe — a reader of the
	// case's own, which lets the waiting open return so its end can be closed.
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
	// FIVE LISTENER BUDGETS, a second: a whole number of the seconds the
	// boot's report counts in.
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
// EVERY CASE RUNS IN A BUBBLE, because what each claims is about the boot's
// clock: a start that runs ten listener budgets and the reports a running start
// makes, a death or a cancel that lands a listener budget in. On the machine's
// clock the first was a bet that a 20 ms ticker fired inside a 500 ms start,
// and a runner that held the waiter past the start's end lost it on a healthy
// build: the wait found the start over and took it before any report. In the
// bubble every instant is exact, so the reports are counted, each with how long
// the start had run, and a death is answered at the instant it was declared.
//
// Mutation: drop the report from the wait and the long start says nothing;
// report a running time of nothing and its reports say so; answer a death one
// report late and it is answered after it was declared.
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
			// ONE REPORT FOR EVERY INTERVAL THE START RAN, each saying how
			// long it had: the last interval ends as the start does, and
			// the wait may take either first.
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

	// THE DEATH WINS EVERY RACE WITH SUCCESS. Each case below hands the wait
	// a death and a success together, and a select picks between ready cases
	// at random, so each is asked enough times that an ordering which let
	// success through would turn up.
	const races = 64
	t.Run("a death declared by a Start that went on", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			for range races {
				death := newDeath()
				// The one Fatalf of v2.15.1's Start that does not return:
				// the server goes on to finish its start past a failure it
				// called fatal — so both have happened when the wait looks.
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
	// Unlike the case above, this one cannot hand the wait both at once: the
	// readiness answer comes from a goroutine the wait starts, and a wait
	// already parked on its select wakes to whichever lands first. It holds
	// the property — a death that lands with readiness fails the boot — for
	// the orderings a test can produce.
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
	// not say it does: both are ready when the wait looks, and the select
	// picks between them at random, so the case is asked enough times that
	// an announcement made regardless would turn up.
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
