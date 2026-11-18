package query

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
)

// A STOP WAITS FOR EVERY READ IN FLIGHT: once Stop has returned, no read this
// server admitted is still being answered or recorded (Server.Stop). Read off
// the admission itself, begin and the end it hands back: a read admitted is
// cut by the stop, which returns only once the read has ended and admits none
// after it. In a bubble, synctest.Wait returns once every other goroutine is
// durably blocked, so a Stop that would not wait has returned by then.
//
// Mutation: return from the stop without waiting for the reads in flight, red.
func TestAStopWaitsForEveryReadInFlight(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := &Server{o: Options{NodeID: "git-a"}}
		s.life, s.end = context.WithCancel(context.Background())
		read, end, ok := s.begin(t.Context())
		if !ok {
			t.Fatal("a running server refused a read")
		}
		// ENDED HOWEVER THE CASE ENDS, so a failing case leaves no stop
		// parked in the bubble.
		end = sync.OnceFunc(end)
		t.Cleanup(end)
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			s.Stop(context.Background())
		}()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("Stop returned while a read it admitted was still in flight")
		default:
		}
		if cause := context.Cause(read); !errors.Is(cause, errStopped) {
			t.Fatalf("the read in flight ended with %v, want the stop's cut", cause)
		}
		if _, _, ok := s.begin(t.Context()); ok {
			t.Fatal("a stopping server admitted a read")
		}

		end()
		synctest.Wait()
		select {
		case <-stopped:
		default:
			t.Fatal("Stop had not returned once the only read in flight had ended")
		}
	})
}
