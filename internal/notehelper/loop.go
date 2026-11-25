package main

import (
	"context"
	"errors"
	"log"
	"time"
)

// harness is the one way the helper hands a note to the harness beside it,
// which an adapter (-adapter) implements. Declared here, by the loop.
type harness interface {
	// ready returns once the harness takes a note now, with ctx's error once
	// ctx ends, or [errGone] once the connection to the harness has ended.
	ready(ctx context.Context) error

	// deliver hands text to the harness whole. [errNotNow] where it stopped
	// taking notes since ready, [errGone] where the connection ended, and any
	// other error for a note the harness refused.
	deliver(ctx context.Context, text string) error

	// interrupt ends turn ("" for the turn in flight) at once. [errNotNow]
	// where no turn is in flight.
	interrupt(ctx context.Context, turn string) error

	// gone is closed once the connection to the harness has ended.
	gone() <-chan struct{}

	// close lets go of the connection.
	close()
}

// events is what an adapter reports of the harness besides taking notes,
// each from the harness's reader and so never blocking: every turn started
// ([stepCap.begin]), every model response of a turn ([stepCap.step]) and every
// load of one of the run's skills ([reporter.loaded]). A nil func is a report
// nobody asked for.
type events struct {
	begin func(turn string)
	step  func(turn string)
	skill func(name string)
}

var (
	// errGone is a harness whose connection ended: the run is over.
	errGone = errors.New("the harness's connection closed")
	// errNotNow is a harness that takes no note now (no turn in flight).
	errNotNow = errors.New("the harness takes no note now")
)

// pacing is every pause the helper takes, a value so its suite can run the
// same loop on pauses of microseconds.
type pacing struct {
	// backoffFirst and backoffMost bound the pause after a failed poll or a
	// refused note: the first, doubling to the most, reset by a success.
	backoffFirst, backoffMost time.Duration
	// pollFloor is the least time from one poll's start to the next one's,
	// after a poll that handed nothing over.
	pollFloor time.Duration
	// pollBound bounds one poll of the notes route.
	pollBound time.Duration
	// waitFirst and waitMost bound the pause between two looks for the
	// thread file, and between two dials of a socket not yet answering.
	waitFirst, waitMost time.Duration
	// dialBudget bounds the dials of the harness's socket, all told, and as
	// much again the joins of a thread not yet joinable ([appServer.rejoin]).
	dialBudget time.Duration
	// callBound bounds one request to the harness and its answer.
	callBound time.Duration
}

// defaultPacing is the helper's pacing in a box.
//
// The backoff, the floor and the poll's bound are OpenCode's plugin's, for
// its reasons (opencode_steer.js): a second doubling to thirty delays a note
// by at most thirty seconds once a down bridge is back; twenty seconds is the
// bridge's ten-second hold (mcpbridge's steerHold) and the same again. The
// thread file lands milliseconds after the harness starts, so a look every
// 50 ms rising to a second costs nothing and adds at most a second. A socket
// not answering, or a thread refusing its join, ten seconds on is broken (the
// refusals end within 137 ms). A steer took at most 2.8 ms and a join's resume
// 41 ms (measured, Codex 0.171.0): thirty seconds per call is only ever
// reached by a wedged server.
var defaultPacing = pacing{
	backoffFirst: time.Second, backoffMost: 30 * time.Second,
	pollFloor: time.Second, pollBound: 20 * time.Second,
	waitFirst: 50 * time.Millisecond, waitMost: time.Second,
	dialBudget: 10 * time.Second, callBound: 30 * time.Second,
}

// next is the pause after a failure that followed a pause of wait.
func next(wait, first, most time.Duration) time.Duration {
	if wait == 0 {
		return first
	}
	return min(wait*2, most)
}

// sleep waits d, false where ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// loop polls the notes route and hands each note to the harness.
type loop struct {
	poll *poller
	h    harness
	log  *log.Logger
	pace pacing
}

// run polls and hands notes over until ctx ends (its error), the harness's
// connection ends ([errGone]) or the route refuses the helper ([errRefused]).
func (l *loop) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A POLL IN FLIGHT ENDS WITH THE HARNESS: the run is over, and the
	// helper exits at once rather than at the poll's bound.
	go func() {
		select {
		case <-l.h.gone():
			cancel()
		case <-ctx.Done():
		}
	}()
	ended := func() error {
		select {
		case <-l.h.gone():
			return errGone
		default:
			return ctx.Err()
		}
	}
	cursor := 0
	var wait time.Duration
	for {
		if err := l.h.ready(ctx); err != nil {
			if ctx.Err() != nil {
				return ended()
			}
			return err
		}
		began := time.Now()
		past := cursor
		notes, err := l.poll.poll(ctx, cursor, l.pace.pollBound)
		switch {
		case ctx.Err() != nil:
			return ended()
		case errors.Is(err, errRefused):
			return err
		case err == nil:
			err = l.hand(ctx, notes, &cursor)
		}
		switch {
		case ctx.Err() != nil:
			return ended()
		case errors.Is(err, errGone):
			return errGone
		case errors.Is(err, errNotNow):
			// The turn ended: the rest is asked for once the next one runs.
			continue
		case err != nil:
			l.log.Print(err)
			wait = next(wait, l.pace.backoffFirst, l.pace.backoffMost)
			if !sleep(ctx, wait) {
				return ended()
			}
			continue
		}
		wait = 0
		// A POLL THAT HANDED NOTHING OVER waits out the floor; one that
		// handed a note over asks at once for the notes after it.
		if early := l.pace.pollFloor - time.Since(began); cursor == past && early > 0 {
			if !sleep(ctx, early) {
				return ended()
			}
		}
	}
}

// hand delivers each note past the cursor, oldest first, moving the cursor
// past each the harness took. A note it refused stops the hand-over: the
// cursor stays before it, and the next poll is served it again.
func (l *loop) hand(ctx context.Context, notes []note, cursor *int) error {
	for _, n := range notes {
		if n.Seq <= *cursor {
			continue
		}
		if err := l.h.deliver(ctx, n.Message); err != nil {
			if errors.Is(err, errNotNow) || errors.Is(err, errGone) {
				return err
			}
			return &refusedNote{id: n.ID, err: err}
		}
		*cursor = n.Seq
	}
	return nil
}

// refusedNote is a note the harness did not take, named by its id alone:
// its text is somebody's words, which the run's error stream does not carry.
type refusedNote struct {
	id  string
	err error
}

func (e *refusedNote) Error() string {
	return "the harness refused note " + e.id + ", which the next poll hands over again: " + e.err.Error()
}

func (e *refusedNote) Unwrap() error { return e.err }
