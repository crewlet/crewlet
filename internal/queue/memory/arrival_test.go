package memory_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// A REQUEST HELD FOR A SLOT IS COUNTED FROM ITS ARRIVAL: its answer is handed
// the instant it reached the registration (queue.Arrived), stamped before it
// waited ten seconds for the one slot, so an answerer bounding its work by its
// asker's wait counts those seconds. The real backend stamps in its delivery
// callback, which the conformance suite holds to never parking. In a bubble,
// so the wait is exact.
//
// Mutation: stamp the arrival as the answer starts, and the held request reads
// as having waited nothing, red.
func TestARequestHeldForASlotIsCountedFromItsArrival(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := memory.NewBroker().Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
		release := make(chan struct{})
		waited := make(chan time.Duration, 1)
		stop, err := q.Serve(t.Context(), "held", 1, func(ctx context.Context, req []byte) ([]byte, error) {
			if string(req) == "first" {
				<-release
			} else {
				waited <- time.Since(queue.Arrived(ctx))
			}
			return req, nil
		})
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
		for _, req := range []string{"first", "second"} {
			go func() { _, _ = q.Ask(t.Context(), "held", []byte(req), 1) }()
			synctest.Wait()
		}
		time.Sleep(10 * time.Second)
		close(release)
		if got := <-waited; got != 10*time.Second {
			t.Fatalf("a request held 10s for a slot reads as having waited %v since it arrived", got)
		}
	})
}

// AN ASK ACROSS A WITHDRAWAL RETURNS, with no deadline of its own: the request
// the withdrawal drops from its registration's queue never runs, and the twin
// stops waiting on it rather than leaking the collection and the ask with it.
// In a bubble, so "returned" is read once everything has settled.
//
// Mutation: drop the queued request without telling the scatter, and the ask
// never returns, red.
func TestAnAskAcrossAWithdrawalReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := memory.NewBroker().Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
		release := make(chan struct{})
		stop, err := q.Serve(t.Context(), "held", 1, func(_ context.Context, req []byte) ([]byte, error) {
			if string(req) == "first" {
				<-release
			}
			return req, nil
		})
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		go func() { _, _ = q.Ask(t.Context(), "held", []byte("first"), 1) }()
		synctest.Wait()
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			_, _ = q.Ask(context.Background(), "held", []byte("second"), 1)
		}()
		synctest.Wait()
		stopped := make(chan error, 1)
		go func() { stopped <- stop(context.Background()) }()
		synctest.Wait()
		close(release)
		if err := <-stopped; err != nil {
			t.Fatalf("stop serving: %v", err)
		}
		synctest.Wait()
		select {
		case <-returned:
		default:
			t.Fatal("an ask with no deadline, whose request a withdrawal dropped, never returned")
		}
	})
}
