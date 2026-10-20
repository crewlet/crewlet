package httpapi

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// slowBody yields one byte per read after the next of its gaps, and ends once
// they run out: a stream that is slow but never silent for longer than its
// longest gap. With ctx set it honours the request's context the way a
// transport does, so a silence the watchdog ends is a read that fails rather
// than one that finishes late.
type slowBody struct {
	ctx  context.Context
	gaps []time.Duration
}

func (b *slowBody) Read(p []byte) (int, error) {
	if len(b.gaps) == 0 {
		return 0, io.EOF
	}
	gap := b.gaps[0]
	b.gaps = b.gaps[1:]
	if b.ctx == nil {
		time.Sleep(gap)
	} else {
		select {
		case <-time.After(gap):
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
	}
	p[0] = 'x'
	return 1, nil
}

func (*slowBody) Close() error { return nil }

// Every case that waits on the bound runs in a synctest bubble, whose clock
// moves only once every goroutine in it is durably blocked. So the gap the
// watchdog sees between two reads is exactly the body's gap: on the wall
// clock it was the gap PLUS however long the reading goroutine waited to be
// scheduled, which a race-instrumented suite on busy CPUs stretched past the
// bound — and widening the bound, as this file once did, only moved the edge
// it could flake at. On the bubble's clock an assertion can sit on the edge
// itself, a nanosecond either side.

// A STREAM IS BOUNDED BY ITS SILENCE, NOT ITS LENGTH: a body that keeps
// arriving runs well past the idle bound in total and is never cut off,
// because each read that returns data restarts the clock. Every gap is a
// millisecond short of the bound, and the whole read is eleven bounds long.
//
// Mutations, each red: no Reset in watchedBody.Read (the stream is cancelled
// at the first bound), or the bound armed at half its length.
func TestALongStreamThatKeepsTalkingIsNeverCutOff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const idle = time.Minute
		const gap = idle - time.Millisecond
		ctx, w := WatchIdle(context.Background(), idle)
		defer w.Stop()
		start := time.Now()
		got, err := io.ReadAll(w.Body(&slowBody{gaps: slices.Repeat([]time.Duration{gap}, 12)}))
		if err != nil || len(got) != 12 {
			t.Fatalf("read %d bytes, err %v", len(got), err)
		}
		if elapsed := time.Since(start); elapsed != 12*gap {
			t.Fatalf("the body took %v, want its twelve gaps, %v", elapsed, 12*gap)
		}
		synctest.Wait()
		if err := ctx.Err(); err != nil {
			t.Fatalf("a stream that never went silent was cancelled: %v", context.Cause(ctx))
		}
	})
}

// A SILENCE MID-STREAM ENDS THE ATTEMPT ONE BOUND AFTER THE LAST BYTE, as a
// stall: two bytes half a bound apart, then a gap of three bounds, is ended at
// two bounds from the start — the second byte's instant plus the bound — and
// the read that was waiting fails with the context the watchdog cancelled.
//
// Mutation: the Reset arming twice the bound ends it at three bounds, and is
// red.
func TestASilenceMidStreamEndsTheAttemptAtTheBound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const idle = time.Minute
		ctx, w := WatchIdle(context.Background(), idle)
		defer w.Stop()
		start := time.Now()
		got, err := io.ReadAll(w.Body(&slowBody{ctx: ctx,
			gaps: []time.Duration{idle / 2, idle / 2, 3 * idle}}))
		if len(got) != 2 {
			t.Fatalf("read %d bytes before the silence, want the 2 the body sent", len(got))
		}
		if elapsed := time.Since(start); elapsed != 2*idle {
			t.Fatalf("the silence was ended at %v, want one bound after the last byte, %v",
				elapsed, 2*idle)
		}
		err = w.Err(err)
		if !errors.Is(err, ErrStalled) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want ErrStalled and DeadlineExceeded", err)
		}
		if !errors.Is(context.Cause(ctx), ErrStalled) {
			t.Fatalf("the attempt ended with cause %v, want the stall", context.Cause(ctx))
		}
	})
}

// endsAt holds a watchdog that reads nothing to its edge: not ended a
// nanosecond short of its bound, ended at it.
func endsAt(ctx context.Context, t *testing.T, idle time.Duration) {
	t.Helper()
	time.Sleep(idle - time.Nanosecond)
	synctest.Wait()
	if err := ctx.Err(); err != nil {
		t.Fatalf("the attempt ended %v into a %v bound: %v", idle-time.Nanosecond, idle,
			context.Cause(ctx))
	}
	time.Sleep(time.Nanosecond)
	synctest.Wait()
	if ctx.Err() == nil {
		t.Fatalf("the attempt outlived its %v bound with nothing read", idle)
	}
}

// SILENCE ENDS THE ATTEMPT, as a TIMEOUT: retryable on the next member of the
// chain and never a reason to bench the key. The SDK only ever sees the
// cancelled context the watchdog caused, which on its own reads as the CALLER
// giving up — fatal, and wrong — so Err must swap it for the stall.
func TestSilenceEndsTheAttemptAsATimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, w := WatchIdle(context.Background(), 30*time.Millisecond)
		defer w.Stop()
		endsAt(ctx, t, 30*time.Millisecond)

		err := w.Err(ctx.Err())
		if !errors.Is(err, ErrStalled) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want ErrStalled and DeadlineExceeded", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v still answers Canceled, which classifies as the caller's own doing", err)
		}
		if kind := FromTransport(err, "p", "m").Kind; kind != llm.KindTimeout {
			t.Fatalf("a stall classified %s, want timeout", kind)
		}
		if !strings.Contains(err.Error(), "30ms") {
			t.Fatalf("err = %q, want it to name the bound", err)
		}
	})
}

// THE CALLER'S CANCELLATION STAYS THE CALLER'S. A seat fence or a shutdown is
// not a stall, and reporting it as one would make a fenced seat's call look
// like a vendor timeout worth retrying elsewhere.
func TestACallerCancellationIsNotAStall(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	ctx, w := WatchIdle(parent, time.Hour)
	defer w.Stop()
	cancel()
	<-ctx.Done()
	if err := w.Err(ctx.Err()); !errors.Is(err, context.Canceled) || errors.Is(err, ErrStalled) {
		t.Fatalf("err = %v, want the caller's cancellation unchanged", err)
	}
}

// AN ATTEMPT THAT SUCCEEDED HAS NO ERROR, even when the bound passed between
// its last byte and the caller looking: the answer is complete.
func TestASuccessIsNotRewrittenAsAStall(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, w := WatchIdle(context.Background(), time.Millisecond)
		defer w.Stop()
		endsAt(ctx, t, time.Millisecond)
		if err := w.Err(nil); err != nil {
			t.Fatalf("Err(nil) = %v, want nil", err)
		}
	})
}

// A NON-POSITIVE BOUND IS A WIRING BUG — it would declare every stream dead at
// once — so it is refused loudly rather than read as "no bound".
func TestANonPositiveIdleBoundIsRefused(t *testing.T) {
	t.Parallel()
	for _, idle := range []time.Duration{0, -time.Second} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WatchIdle(%v) did not panic", idle)
				}
			}()
			_, w := WatchIdle(context.Background(), idle)
			w.Stop()
		}()
	}
}
