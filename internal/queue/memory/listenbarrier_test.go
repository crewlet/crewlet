package memory_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/crewlet/crewlet/internal/queue/memory"
)

// A LISTEN BARRIER PASSES ONLY ONCE EVERY LISTENER HAS HANDLED WHAT IT WAS
// HANDED: it is what the conformance suite reads a withdrawn listener's silence
// behind (queuetest's a_withdrawn_listener_hears_nothing_more), so a barrier
// that passed early would let a registration the withdrawal left standing hear
// the broadcast after the case had looked, and the case would pass with it.
//
// In a bubble: the listener's handler is held inside the first of two
// broadcasts, and the barrier not having passed is read once every goroutine
// of the case is durably blocked — a barrier that was going to pass without
// the handler has passed by then.
//
// Mutation: close the barrier's channel where it is queued rather than where
// the listener reaches it, and it passes with the handler still held.
func TestAListenBarrierPassesOnceItsListenersHaveHandledWhatTheyWereHanded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		q := memory.New()
		if err := q.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = q.Stop(context.WithoutCancel(ctx)) }()

		release := make(chan struct{})
		var mu sync.Mutex
		var heard []string
		stop, err := q.Listen(ctx, "crewlet.room", func(_ string, payload []byte) {
			if string(payload) == "first" {
				<-release
			}
			mu.Lock()
			heard = append(heard, string(payload))
			mu.Unlock()
		})
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		for _, payload := range []string{"first", "second"} {
			if err := q.Broadcast(ctx, "crewlet.room", []byte(payload)); err != nil {
				t.Fatalf("Broadcast %s: %v", payload, err)
			}
		}

		passed := make(chan error, 1)
		go func() { passed <- q.ListenBarrier(ctx) }()
		synctest.Wait()
		select {
		case err := <-passed:
			t.Fatalf("the barrier passed (%v) with the listener's handler still held "+
				"inside the first of two broadcasts", err)
		default:
		}

		close(release)
		if err := <-passed; err != nil {
			t.Fatalf("ListenBarrier: %v", err)
		}
		mu.Lock()
		got := slices.Clone(heard)
		mu.Unlock()
		if !slices.Equal(got, []string{"first", "second"}) {
			t.Fatalf("past the barrier the listener had heard %v, want both broadcasts "+
				"handed to it before the barrier was asked for", got)
		}
		if err := stop(ctx); err != nil {
			t.Fatalf("stop listening: %v", err)
		}
	})
}
