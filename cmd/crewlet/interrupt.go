package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// interrupts is `crewlet run`'s hold on SIGINT and SIGTERM, in TWO TIERS: the
// first signal cancels the run's context, and a second ends the process at
// once.
//
// From the first moment of the run rather than once the node is serving,
// because a serving node's drain is not the only unbounded wait. A boot has
// them too: a boot that fails once the engine is up runs the same drain, and
// an embedded broker still recovering a large store cannot be interrupted
// (internal/queue/jetstream's startEmbedded waits the recovery out). While a
// handler that only cancels is still installed, every further press is
// swallowed, which left an operator with no way to abort from the terminal
// they started the node in short of SIGKILL from somewhere else. The second
// tier is what makes those unbounded waits safe to offer: the bound belongs
// to whoever supervises the process, and for an operator at a terminal the
// second press IS their bound.
//
// THE SECOND TIER IS THE OPERATING SYSTEM'S WHEREVER IT CAN BE. The first
// signal HANDS BOTH BACK before it cancels anything ([interrupts.handBack]),
// so the second takes the signal's default action and nothing in this
// process has to be well enough to perform it. A press that reached the
// handler between the first and the hand-back is raised again once the
// default is back ([release]), rather than swallowed by a handler that no
// longer reads it.
//
// EXCEPT A SIGINT THE PROCESS STARTED WITH IGNORED. A background job of a
// non-interactive shell starts that way (with job control off, POSIX has the
// shell ignore SIGINT and SIGQUIT for an asynchronous command), and so does
// anything a parent that ignored SIGINT starts, nohup-style launchers among
// them. Notify overrides an inherited ignore, so the first SIGINT is caught
// and starts the drain like any other — but handing SIGINT back restores the
// disposition the process INHERITED, because the runtime keeps an inherited
// SIG_IGN for SIGINT and SIGHUP alone, and the second press, which the drain
// line had just promised "exits at once", did nothing. No Go program can set
// SIGINT to its default once it inherited it ignored: os/signal offers
// Notify, Ignore and a Reset to whatever was inherited, and sigaction is the
// runtime's. So for that one signal the hold keeps its own handler past the
// first, and the second SIGINT ends the process from it, with
// [interruptedStatus] and every deferred cleanup skipped, as the kill would
// have. Keeping a handler for SIGINT in every case, so both dispositions took
// one path, was the alternative, and it is worse where it does not have to
// be paid: a default disposition ends the process whatever state the Go
// scheduler is in. SIGTERM needs no such care, because the runtime installs
// its own handler for SIGTERM whatever the process inherited, so handing it
// back always leaves the default action.
//
// A HANGUP THE PROCESS STARTED IGNORING (`nohup crewlet run`) STAYS IGNORED
// HERE, AND IS NOT HANDED ON. The runtime keeps an inherited SIG_IGN for
// SIGHUP and every process the engine starts inherits it, and a
// non-interactive sh may not trap a signal it started ignoring, so a box's run
// script under nohup could neither trap a SIGHUP nor die of one: the export a
// hangup gives a run (and its exit 129) never happened. Caught on a channel
// nothing reads, SIGHUP still does nothing to the engine, which is what nohup
// asked for, and the runtime starts every child with its default, as it does
// any signal it handles. One the process did not start ignoring keeps its
// default, which ends the engine as it always has.
type interrupts struct {
	// sigintIgnored says SIGINT was ignored when the process started. Read
	// BEFORE the first Notify, which clears what signal.Ignored reports.
	sigintIgnored bool

	// One channel per signal, so each is handed back on its own: the hand
	// back releases SIGTERM, and SIGINT unless the process inherited it
	// ignored, in which case it stays on its channel for [interrupts.caught]
	// to end the process from.
	sigint, sigterm chan os.Signal

	// sighup holds a SIGHUP the process started ignoring, read by nothing,
	// so its children start with the default; nil where it did not.
	sighup chan os.Signal

	cancel context.CancelFunc

	handBackOnce sync.Once
	endOnce      sync.Once
	ended        chan struct{}
	watched      chan struct{}
}

// interruptedStatus is what the process exits with when the second SIGINT
// ends it from a handler rather than by the signal itself: 128 plus SIGINT's
// number, the status a shell reports for a process SIGINT killed, so `$?` and
// `wait` read the two exits alike.
const interruptedStatus = 128 + int(syscall.SIGINT)

// interruptContext is the context `crewlet run` lives under, and the hold that
// cancels it: by the first SIGINT or SIGTERM, which moves both signals to the
// second tier first, or by [interrupts.handBack] with no signal behind it.
func interruptContext() (context.Context, *interrupts) {
	held := &interrupts{
		sigintIgnored: signal.Ignored(syscall.SIGINT),
		sigint:        make(chan os.Signal, 1),
		sigterm:       make(chan os.Signal, 1),
		ended:         make(chan struct{}),
		watched:       make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	held.cancel = cancel
	if signal.Ignored(syscall.SIGHUP) {
		held.sighup = make(chan os.Signal, 1)
		signal.Notify(held.sighup, syscall.SIGHUP)
	}
	signal.Notify(held.sigint, syscall.SIGINT)
	signal.Notify(held.sigterm, syscall.SIGTERM)
	go held.watch()
	return ctx, held
}

// watch is the hold's one goroutine, and every signal the hold catches is
// answered here, one at a time, until [interrupts.end] says the run is over.
func (i *interrupts) watch() {
	defer close(i.watched)
	for {
		select {
		case s := <-i.sigint:
			i.caught(s)
		case s := <-i.sigterm:
			i.caught(s)
		case <-i.ended:
			return
		}
	}
}

// caught answers one signal the hold caught. The one that hands back is the
// first, and every one after it is a second press, which ends the process —
// one after a node that stopped itself handed back with no signal behind it
// included, and one that reached the hold while the hand-back was under way.
// It ends it by the signal's default action, raised again now that it is
// back, or, for a SIGINT the process inherited ignored, which has no default
// to go back to, by exiting with [interruptedStatus].
func (i *interrupts) caught(s os.Signal) {
	if i.handBack() {
		return
	}
	if s == syscall.SIGINT && i.sigintIgnored {
		os.Exit(interruptedStatus)
	}
	raise(s)
}

// handBack moves the run to the second tier and cancels its context, in that
// order: a caller that has seen the context end can rely on the next signal
// ending the process. The first signal calls it; so does a node that stops
// itself ([engine.Engine.Fatal]), as its drain starts, because it had no
// first signal and an operator's FIRST press during that drain is the one
// that must exit. Calling it again is harmless, and it reports whether this
// call was the one that handed back — once another has, a signal in hand is
// a second press.
func (i *interrupts) handBack() (handedBack bool) {
	i.handBackOnce.Do(func() {
		release(i.sigterm, raise)
		if !i.sigintIgnored {
			release(i.sigint, raise)
		}
		handedBack = true
	})
	i.cancel()
	return handedBack
}

// end releases both signals for good and waits for the hold's goroutine: what
// a run defers, because a process that runs another in-process (a test does)
// must not keep the handler of one that has returned. It raises nothing: a
// press that lands as the run returns is answered by the process ending
// anyway. Calling it again is harmless.
func (i *interrupts) end() {
	i.endOnce.Do(func() {
		close(i.ended)
		<-i.watched
		signal.Stop(i.sigint)
		signal.Stop(i.sigterm)
		if i.sighup != nil {
			signal.Stop(i.sighup)
		}
		i.cancel()
	})
}

// release stops c receiving signals and raises again every signal that
// reached c and was never read: a press that landed after the one being
// answered and before c stopped, which the default action, back in place
// now, answers instead. signal.Stop guarantees what makes that complete: a
// signal is either delivered to c before Stop returns or takes the default
// action.
func release(c chan os.Signal, raise func(os.Signal)) {
	signal.Stop(c)
	for {
		select {
		case s := <-c:
			raise(s)
		default:
			return
		}
	}
}

// raise sends s to this process again. Sending a signal to itself cannot fail
// for a process that holds a valid one, and if it ever did the press would
// still be owed an exit, so it exits with the status a shell would report.
func raise(s os.Signal) {
	n, ok := s.(syscall.Signal)
	if !ok {
		return
	}
	if err := syscall.Kill(syscall.Getpid(), n); err != nil {
		os.Exit(128 + int(n))
	}
}
