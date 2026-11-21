package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// interrupts is `crewlet run`'s hold on SIGINT and SIGTERM in TWO TIERS: the
// first signal cancels the run's context, the second ends the process at once.
// Held from the start of the run, since a boot has unbounded waits too (an
// embedded broker recovering a large store); the second press IS the operator's
// bound. The first signal HANDS BOTH BACK before cancelling ([interrupts.handBack]),
// so the second takes the OS default; a press in between is raised again ([release]).
// EXCEPT A SIGINT INHERITED IGNORED (background job of a non-interactive shell,
// nohup launchers): no Go program can restore its default, so that signal keeps
// its handler and the second SIGINT exits with [interruptedStatus], skipping
// deferred cleanup, as a kill would. SIGTERM needs no care: the runtime always
// installs its own handler.
type interrupts struct {
	// sigintIgnored says SIGINT was ignored when the process started. Read
	// BEFORE the first Notify, which clears what signal.Ignored reports.
	sigintIgnored bool

	// One channel per signal, so each is handed back on its own: the hand
	// back releases SIGTERM, and SIGINT unless the process inherited it
	// ignored, in which case it stays on its channel for [interrupts.caught]
	// to end the process from.
	sigint, sigterm chan os.Signal

	// sighup holds a SIGHUP the process started ignoring (`nohup crewlet
	// run`), read by nothing: it stays ignored here, but caught rather than
	// inherited as SIG_IGN, so children start with the default and a box's
	// run script can trap or die of a hangup. nil where it did not.
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
