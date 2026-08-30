package seat

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
)

var watchLog = logging.Get("seat.watchdog")

// The watchdog's own cadence. Both are CEILINGS, scaled down against the
// threshold — see [Host.beatInterval] for the other half of the same rule,
// and [Watchdog] for why the threshold itself is not a knob.
const (
	// WatchdogBeatInterval is the fastest a watched duty needs to prove it
	// is turning. A stamp is a mutex and a clock read, so its cost is
	// irrelevant next to the resolution it buys.
	WatchdogBeatInterval = time.Second

	// WatchdogPollInterval is how often the watcher compares. Same
	// reasoning: a monotonic subtraction per second.
	WatchdogPollInterval = time.Second

	// WatchdogBeatsPerThreshold is how many beats must fit inside the
	// threshold. Five gives a healthy duty four chances to refresh before
	// the window closes, so ordinary jitter never registers.
	WatchdogBeatsPerThreshold = 5

	// WatchdogExitCode marks a self-terminated wedge, distinct from any
	// ordinary failure so an orchestrator's restart logs say what happened.
	WatchdogExitCode = 75
)

// Pulse is a liveness source the watchdog watches: a duty that proves it is
// still turning by stamping a time.
//
// Beat answers BOTH questions in one call, deliberately. Read separately
// they race — and a duty that has STOPPED, read as one that is WEDGED, is
// exactly the suicide timer this design must never arm.
type Pulse interface {
	// Beat reports when the duty last proved it was turning, and whether it
	// is still expected to turn at all. A duty that is not live is skipped:
	// it stopped on purpose, and there is nothing to shoot.
	Beat() (last time.Time, live bool)
}

// Stall is what the watchdog found when it fired.
type Stall struct {
	// Duty names the pulse that stopped.
	Duty string
	// Lag is how far past its last beat that duty is.
	Lag time.Duration
	// Threshold is the deadline it blew — the seat lease TTL.
	Threshold time.Duration
}

// Watchdog ends the process when a watched duty stops turning past the seat
// lease TTL.
//
// The failure it exists for is the one where a node neither works nor dies.
// Its seat leases lapse, because nothing is renewing them, and peers
// correctly take the seats over — while the process goes on existing, holding
// whatever its stalled handler already fetched and never acked. The successor
// owns the seat within a lease TTL and cannot see those messages, because on
// the shared durable consumer they are still ack-pending for the corpse.
//
// Nothing can be signalled out of that state, because the code that would
// handle the signal is the code that is stuck. What the watchdog CAN do
// unilaterally is end the process, and it is worth being exact about what that
// buys, because the answer is NOT immediate redelivery:
//
//   - It removes the ACTOR. A wedged node that later resumes would act on a
//     seat it no longer owns; ending it bounds that to the fencing window
//     rather than leaving it open indefinitely.
//   - It lets the node come back. A process that neither works nor dies is
//     never restarted by an orchestrator watching for liveness, so the seats
//     it was handed stay dark until a person notices.
//
// What it does not buy is the held mail back. JetStream releases a
// fetched-unacked message when ackWait elapses (30 minutes — see
// internal/queue/jetstream), and closing the connection does not shorten
// that: unlike a broker that returns unacked mail on session close, there is
// no session-scoped release here. So a wedged seat's in-flight batch — bounded
// by the batch cap, not by the seat's whole backlog, because pull consumers
// prefetch nothing — is invisible to the successor until ackWait expires. The
// successor serves everything published after that point normally; it is the
// already-fetched batch that waits.
//
// The threshold is deliberately NOT a config knob. It is the same number the
// lease TTL is: past it the node is provably not the owner, and letting the
// two drift is how a process gets to be simultaneously "not the owner" and
// "still holding the mail".
type Watchdog struct {
	threshold time.Duration
	poll      time.Duration
	clock     func() time.Time

	// onStall is the injection seam. Unexported because the default really
	// does exit and no caller outside this package may weaken that.
	onStall func(Stall)

	mu      sync.Mutex
	pulses  []namedPulse
	started bool
	fired   bool
	cancel  context.CancelFunc
	done    chan struct{}
}

type namedPulse struct {
	name  string
	pulse Pulse
}

// NewWatchdog builds a watchdog at the lease TTL. It watches nothing until
// [Watchdog.Watch] is called, and a watchdog with no live duty stands down
// rather than firing — there is no such thing as an unprovoked exit here.
func NewWatchdog() *Watchdog { return newWatchdog(SeatLeaseTTL, nil, nil) }

// newWatchdog is the constructor with the two seams tests need: a tight
// threshold and an on-stall that does not end the test binary. Unexported so
// that the public surface cannot express either.
func newWatchdog(threshold time.Duration, clock func() time.Time, onStall func(Stall)) *Watchdog {
	if threshold <= 0 {
		threshold = SeatLeaseTTL
	}
	if clock == nil {
		clock = time.Now
	}
	if onStall == nil {
		onStall = hardExit
	}
	poll := WatchdogPollInterval
	if scaled := threshold / WatchdogBeatsPerThreshold; scaled < poll {
		poll = scaled
	}
	if poll <= 0 {
		poll = time.Millisecond
	}
	return &Watchdog{threshold: threshold, poll: poll, clock: clock, onStall: onStall}
}

// Watch registers a duty. Call before [Watchdog.Start]; registering later is
// allowed and takes effect on the next poll.
//
// The name is supplied HERE rather than by the pulse, so a duty does not
// have to carry a naming method it has no other use for, and so two
// registrations of the same object can be told apart in the stall log.
func (w *Watchdog) Watch(name string, p Pulse) {
	if p == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pulses = append(w.pulses, namedPulse{name: name, pulse: p})
}

// Threshold is the lag at which the watchdog exits — the seat lease TTL.
func (w *Watchdog) Threshold() time.Duration { return w.threshold }

// Start begins watching. Idempotent; a second call is a no-op rather than a
// second goroutine.
func (w *Watchdog) Start(ctx context.Context) {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return
	}
	w.started = true
	loopCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	done := make(chan struct{})
	w.done = done
	w.mu.Unlock()

	go func() {
		defer close(done)
		w.watch(loopCtx)
	}()
	watchLog.Info("watchdog_started", "threshold_seconds", w.threshold.Seconds(),
		"poll_seconds", w.poll.Seconds())
}

// Stop disarms the watchdog and waits for its goroutine.
//
// The engine calls this FIRST in a graceful shutdown, and the ordering is
// not incidental: teardown is the one part of the process that legitimately
// blocks for a long time — reaping MCP subprocesses, joining goroutines,
// tearing sandboxes down — and exiting through the middle of it abandons the
// seat release that makes a drain graceful. A shutdown that hangs is a
// SIGKILL away; a shutdown that exits without releasing costs every peer a
// full TTL of dark seats.
func (w *Watchdog) Stop() {
	w.mu.Lock()
	if !w.started {
		w.mu.Unlock()
		return
	}
	w.started = false
	cancel, done := w.cancel, w.done
	w.cancel, w.done = nil, nil
	w.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	watchLog.Info("watchdog_stopped")
}

// Lag is how far behind the worst LIVE duty is, for /health. Zero when no
// watched duty is live, which is the same reading as "nothing to report".
func (w *Watchdog) Lag() time.Duration {
	_, lag, _ := w.worst(w.clock())
	return lag
}

func (w *Watchdog) watch(ctx context.Context) {
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !w.check() {
			return
		}
	}
}

// check runs one poll, reporting whether watching should continue.
//
// It is the whole decision, factored out so a test can drive it against a
// clock it controls rather than against a sleep.
func (w *Watchdog) check() bool {
	name, lag, anyLive := w.worst(w.clock())
	if !anyLive {
		// Every watched duty stopped on purpose. From here a STOPPED duty
		// and a WEDGED one look identical — the beat simply stops
		// refreshing — and they are opposite situations. A wedged duty is
		// still alive inside a live process holding a peer's mail, which is
		// the entire justification for exiting; a duty that was shut down
		// took its work with it and there is nothing left to hold. Without
		// this check, every engine that is abandoned rather than stopped
		// arms a suicide timer that fires one lease TTL later on a
		// perfectly healthy process.
		watchLog.Info("watchdog_stood_down", "lag_seconds", lag.Seconds(),
			"hint", "no watched duty is running any more, so a frozen beat means shut down "+
				"rather than wedged; there is nothing left holding a peer's mail")
		return false
	}
	if lag <= w.threshold {
		return true
	}

	w.mu.Lock()
	if w.fired {
		w.mu.Unlock()
		return false
	}
	w.fired = true
	w.mu.Unlock()

	w.onStall(Stall{Duty: name, Lag: lag, Threshold: w.threshold})
	return false
}

// worst finds the live duty that is furthest behind.
//
// A live duty with no beat at all reads as lag zero rather than as lag
// forever: it has been registered but has not run yet, and firing on that
// would kill a process for starting up.
func (w *Watchdog) worst(now time.Time) (name string, lag time.Duration, anyLive bool) {
	w.mu.Lock()
	pulses := slices.Clone(w.pulses)
	w.mu.Unlock()

	for _, p := range pulses {
		last, live := p.pulse.Beat()
		if !live {
			continue
		}
		anyLive = true
		if last.IsZero() {
			continue
		}
		if d := now.Sub(last); d > lag {
			name, lag = p.name, d
		}
	}
	return name, lag, anyLive
}

// hardExit leaves, now, without unwinding.
//
// os.Exit rather than a panic, a signal, or a graceful shutdown: the duty
// that stalled is the one that would have to run the shutdown, and anything
// that waits on it hangs. Go's os.Exit already runs no deferred function,
// which is exactly the crudeness this wants.
//
// The line goes straight to stderr rather than through the logger for the
// same reason: a configured handler may batch, format, or ship lines
// somewhere, and that is more machinery than a wedged process has earned.
func hardExit(s Stall) {
	fmt.Fprintf(os.Stderr,
		"crewlet: %s stalled %.1fs past the seat lease TTL; exiting so peers can take over "+
			"cleanly (code %d)\n",
		s.Duty, s.Lag.Seconds(), WatchdogExitCode)
	os.Exit(WatchdogExitCode)
}

var _ Pulse = (*Host)(nil)
