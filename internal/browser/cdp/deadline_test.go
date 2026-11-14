package cdp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// EVERY COMMAND FAILS AT ITS OWN DEADLINE, CommandTimeout from its sending,
// naming its method, while a command sent later and answered is untouched;
// and a caller's deadline sooner than that wins. In a bubble, so each instant
// is exact: one unanswered command sent at 0 s fails at 60 s, one sent at
// 10 s fails at 70 s, one sent at 20 s and answered returns at 20 s, and one
// sent at 20 s under a 5 s context fails at 25 s.
//
// Mutation: one timer per connection, started by its first command, and the
// command sent at 10 s fails at 60 s with the first — red.
func TestEveryCommandFailsAtItsOwnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, p := pipePair(t, nil)
		start := time.Now()
		type ended struct {
			err error
			at  time.Duration
		}
		send := func(ctx context.Context) <-chan ended {
			out := make(chan ended, 1)
			go func() {
				_, err := Call(ctx, c.Browser(), testCall, testParams{})
				out <- ended{err: err, at: time.Since(start)}
			}()
			p.next()
			return out
		}

		first := send(t.Context())
		time.Sleep(10 * time.Second)
		second := send(t.Context())
		time.Sleep(10 * time.Second)

		answered := make(chan ended, 1)
		go func() {
			_, err := Call(t.Context(), c.Browser(), testCall, testParams{})
			answered <- ended{err: err, at: time.Since(start)}
		}()
		if err := p.answer(p.next(), testResult{}); err != nil {
			t.Fatal(err)
		}
		if got := <-answered; got.err != nil || got.at != 20*time.Second {
			t.Fatalf("the answered command returned %v at %s, want nothing wrong at 20s", got.err, got.at)
		}

		short, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		sooner := send(short)

		for _, want := range []struct {
			name string
			end  <-chan ended
			at   time.Duration
		}{
			{"the command under a 5s context", sooner, 25 * time.Second},
			{"the command sent at 0s", first, CommandTimeout},
			{"the command sent at 10s", second, 10*time.Second + CommandTimeout},
		} {
			got := <-want.end
			if got.at != want.at || !errors.Is(got.err, context.DeadlineExceeded) || !strings.Contains(got.err.Error(), testCall.name) {
				t.Errorf("%s failed at %s with %v; want at %s, a deadline naming %s", want.name, got.at, got.err, want.at, testCall.name)
			}
		}
	})
}

// A BROWSER THAT TAKES NO FRAME ENDS ITS CONNECTION AT COMMANDTIMEOUT: the
// write of a frame the browser never takes is bounded by CommandTimeout of its
// own, by the connection's writer whichever its transport, and at that bound
// the connection ends with ErrClosed saying why, so a hung browser fails every
// call on the connection rather than each call alone at its deadline while the
// connection stays open. On the pipe, in a bubble, so the instant is exact.
//
// Mutation: drop the writer's bound, and the connection is still open at
// twice CommandTimeout — red.
func TestABrowserThatTakesNoFrameEndsItsConnectionAtCommandTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := pipePair(t, nil) // its browser never reads
		start := time.Now()
		calls := make(chan error, 1)
		go func() {
			_, err := Call(t.Context(), c.Browser(), testCall, testParams{})
			calls <- err
		}()
		select {
		case <-c.Done():
		case <-time.After(2 * CommandTimeout):
			t.Fatalf("at %s the connection whose browser took no frame is still open", 2*CommandTimeout)
		}
		if at := time.Since(start); at != CommandTimeout {
			t.Errorf("the connection ended at %s, want %s", at, CommandTimeout)
		}
		if err := c.Err(); !errors.Is(err, ErrClosed) || !errors.Is(err, errNoFrameTaken) {
			t.Errorf("the connection ended with %v, want ErrClosed saying the browser took no frame", err)
		}
		if err := <-calls; err == nil {
			t.Error("the call whose frame was never taken succeeded")
		}
	})
}

// A SENT COMMAND FAILS AT ITS OWN DEADLINE, WHETHER ANYBODY WAITS OR NOT: a
// command Send sent that the browser never answers fails at CommandTimeout
// from its sending, though nobody waits for it: at twice that the connection
// no longer holds it, so an answer that came now would be unmatched, and a
// Wait then reads the failure at once, naming the method. In a bubble, so
// each instant is exact.
//
// Mutations: give up on the command only in a Wait, as a call does, and the
// connection still holds it at twice CommandTimeout — red; run its deadline
// from the first Wait rather than from its sending, and that Wait returns at
// three times CommandTimeout — red.
func TestASentCommandFailsAtItsOwnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, p := pipePair(t, nil)
		start := time.Now()
		read := make(chan struct{})
		go func() {
			_, _ = p.recv()
			close(read)
		}()
		sent := Send(t.Context(), c.Browser(), testCall, testParams{})
		<-read
		time.Sleep(2 * CommandTimeout)
		c.mu.Lock()
		held := len(c.pending)
		c.mu.Unlock()
		if held != 0 {
			t.Errorf("at %s, with nobody waiting, the connection still holds %d commands sent at 0s", 2*CommandTimeout, held)
		}
		_, err := sent.Wait(t.Context())
		if at := time.Since(start); at != 2*CommandTimeout {
			t.Errorf("the Wait at %s returned at %s, want at once", 2*CommandTimeout, at)
		}
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), testCall.name) {
			t.Errorf("the command sent at 0s failed with %v, want its deadline, naming %s", err, testCall.name)
		}
	})
}
