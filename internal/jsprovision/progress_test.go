package jsprovision

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A READ ACROSS A LINK, HELD WHILE THE LINK MOVES.
//
// The cases here run on a bubble's clock at the production timing, where a
// ceiling of thirty seconds and four minutes beyond it pass at once, over a
// link whose movement a case scripts: still, moving for good, or moving until
// an instant and still after it.

// scriptedLink is a link whose movement a case scripts, read off a closure
// the case writes inside its bubble, on the bubble's clock.
type scriptedLink struct{ moving func() bool }

func (l scriptedLink) Moving(time.Duration) bool { return l.moving() }

// stillLink is a link that never moves.
var stillLink = scriptedLink{moving: func() bool { return false }}

// unanswered is a read nobody answers: a request that waits for its context.
func unanswered(asks *atomic.Int32) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		asks.Add(1)
		<-ctx.Done()
		return 0, ctx.Err()
	}
}

// heldRead is one read at t on its own goroutine, read once it returns.
type heldRead struct {
	done chan struct{}
	v    int
	err  error
}

func reading(ctx context.Context, t Timing, one func(context.Context) (int, error)) *heldRead {
	r := &heldRead{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.v, r.err = ReadWith(ctx, t, one, nil)
	}()
	return r
}

func (r *heldRead) returned() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// A READ'S CEILING RUNS ONLY WHILE ITS LINK IS STILL.
//
// A read nobody answers, at the production timing, across a scripted link: a
// still link ends it at its ceiling; one that moves for good holds it to the
// ceiling and beyond, never later; one that moves past the ceiling then stops
// ends it at the meter's next judgment. Each ending is [Unanswered], with no
// request asked past the ceiling.
//
// Mutant: Timing.Ceiling as a plain timeout, and moving links' reads end at
// the bare ceiling. Mutant: no cap, and a link moving for good runs past the
// ceiling and beyond. Mutant: asked on past the ceiling, and more requests are
// out at its end than at the ceiling.
func TestACeilingRunsOnlyWhileItsLinkIsStill(t *testing.T) {
	t.Parallel()
	production := Clustered(true).Timing()
	beyond := 248 * time.Second
	ceiling := production.Lookup
	for _, c := range []struct {
		name string
		// stopsAfter is how long the link moves from the read's start; 0
		// for a link that never does, and -1 for one that always does.
		stopsAfter time.Duration
		// ends is when the read ends, from its start.
		ends time.Duration
	}{
		{"a still link", 0, ceiling},
		{"a link that moves for good", -1, ceiling + beyond},
		{"a link that falls still past the ceiling", ceiling + 7*time.Second + stillPeriod/2,
			ceiling + 7*time.Second + stillPeriod},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				link := scriptedLink{moving: func() bool {
					return c.stopsAfter < 0 || time.Now().Before(start.Add(c.stopsAfter))
				}}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var asks atomic.Int32
				r := reading(ctx, production.Following(link, beyond), unanswered(&asks))

				time.Sleep(ceiling - time.Nanosecond)
				synctest.Wait()
				atCeiling := asks.Load()
				time.Sleep(c.ends - ceiling)
				synctest.Wait()
				if r.returned() {
					t.Fatalf("the read returned (%d, %v) a nanosecond before %v, want it running", r.v,
						r.err, c.ends)
				}
				if c.ends == ceiling {
					atCeiling = asks.Load()
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if !r.returned() {
					cancel()
					<-r.done
					t.Fatalf("the read was still running at %v, want it ended there", c.ends)
				}
				if !errors.Is(r.err, context.DeadlineExceeded) || !Unanswered(ctx, r.err) {
					t.Errorf("the read ended with %v, want the silence a ceiling's ending is", r.err)
				}
				if n := asks.Load(); n != atCeiling || n < 2 {
					t.Errorf("the read asked %d requests by its ceiling and %d by its end, want the "+
						"same, and more than one: re-asked inside the ceiling, never past it", atCeiling, n)
				}
			})
		})
	}
}

// A REPLY HELD BEHIND A MOVING LINK PAST THE CEILING IS TAKEN.
//
// The one request's reply lands half of beyond past the ceiling. Across a
// link that moves meanwhile it is the answer; across a still one the read
// ended at the ceiling with nothing, as every read did.
//
// Mutant: a ceiling that reads no link — the reply is never taken.
func TestAReplyHeldBehindAMovingLinkPastTheCeilingIsTaken(t *testing.T) {
	t.Parallel()
	production := Clustered(true).Timing()
	beyond := 248 * time.Second
	lands := production.Lookup + beyond/2
	for _, c := range []struct {
		name   string
		moving bool
		taken  bool
	}{
		{"across a moving link", true, true},
		{"across a still one", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				link := scriptedLink{moving: func() bool { return c.moving }}
				var asks atomic.Int32
				got, err := ReadWith(context.Background(), production.Following(link, beyond),
					func(ctx context.Context) (int, error) {
						if asks.Add(1) > 1 {
							<-ctx.Done()
							return 0, ctx.Err()
						}
						landed := time.NewTimer(lands)
						defer landed.Stop()
						select {
						case <-landed.C:
							return 7, nil
						case <-ctx.Done():
							return 0, ctx.Err()
						}
					}, nil)
				if taken := err == nil && got == 7; taken != c.taken {
					t.Errorf("a reply landing %v in = (%d, %v): taken %t, want %t", lands, got, err,
						taken, c.taken)
				}
			})
		})
	}
}
