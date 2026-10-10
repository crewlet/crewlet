package httpapi

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// manualClock is a clock only the test moves, for the watchdog's timers: a
// silence bound asserted on the wall clock is a claim about how promptly the
// scheduler runs a goroutine, which no loaded runner keeps.
type manualClock struct {
	mu     sync.Mutex
	now    time.Duration
	timers []*manualTimer
}

type manualTimer struct {
	clock *manualClock
	at    time.Duration
	f     func()
	armed bool
}

func (c *manualClock) after(d time.Duration, f func()) stallTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTimer{clock: c, at: c.now + d, f: f, armed: true}
	c.timers = append(c.timers, t)
	return t
}

// advance moves the clock by d and runs every armed timer whose time came.
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now += d
	var due []func()
	for _, t := range c.timers {
		if t.armed && t.at <= c.now {
			t.armed = false
			due = append(due, t.f)
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

func (t *manualTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.armed
	t.at, t.armed = t.clock.now+d, true
	return was
}

func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.armed
	t.armed = false
	return was
}

// talkingBody yields one byte per read, the clock moving by gap before each:
// a stream that is slow but never silent for longer than its gap. Silent after
// silentAfter reads, when that is set, for the clock's whole remaining run.
type talkingBody struct {
	clock       *manualClock
	gap         time.Duration
	reads       int
	silentAfter int
	served      int
}

func (b *talkingBody) Read(p []byte) (int, error) {
	if b.served == b.reads {
		return 0, io.EOF
	}
	b.clock.advance(b.gap)
	if b.silentAfter > 0 && b.served >= b.silentAfter {
		// THE CONNECTION GOES QUIET: time passes and nothing arrives.
		b.clock.advance(time.Hour)
		return 0, io.ErrUnexpectedEOF
	}
	b.served++
	p[0] = 'x'
	return 1, nil
}

func (*talkingBody) Close() error { return nil }

// A STREAM IS BOUNDED BY ITS SILENCE, NOT ITS LENGTH: a body that keeps
// arriving runs to four times the idle bound in total and is never cut off,
// because each read that returns data restarts the clock.
func TestALongStreamThatKeepsTalkingIsNeverCutOff(t *testing.T) {
	t.Parallel()
	const idle = 80 * time.Millisecond
	clock := &manualClock{}
	ctx, w := watchIdle(context.Background(), idle, clock.after)
	defer w.Stop()
	body := w.Body(&talkingBody{clock: clock, gap: idle / 4, reads: 16})
	got, err := io.ReadAll(body)
	if err != nil || len(got) != 16 {
		t.Fatalf("read %d bytes, err %v", len(got), err)
	}
	if clock.now < 2*idle {
		t.Fatalf("the body took %v, want well past the %v bound to prove it is not a total", clock.now, idle)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("a stream that never went silent was cancelled: %v", context.Cause(ctx))
	}
}

// AND A STREAM THAT STOPS TALKING IS CUT OFF, however long it talked first:
// the bound is the gap since the last byte, not since the request.
//
// Mutation: drop the Reset in watchedBody.Read, and the stream above is cut
// off a quarter of the way through instead.
func TestAStreamThatGoesSilentIsCutOffAsAStall(t *testing.T) {
	t.Parallel()
	const idle = 80 * time.Millisecond
	clock := &manualClock{}
	ctx, w := watchIdle(context.Background(), idle, clock.after)
	defer w.Stop()
	body := w.Body(&talkingBody{clock: clock, gap: idle / 4, reads: 16, silentAfter: 12})
	if _, err := io.ReadAll(body); err == nil {
		t.Fatal("a body that went silent read to the end")
	}
	if !errors.Is(context.Cause(ctx), ErrStalled) {
		t.Fatalf("cause = %v, want the stall", context.Cause(ctx))
	}
}

// SILENCE ENDS THE ATTEMPT, as a TIMEOUT: retryable on the next member of the
// chain and never a reason to bench the key. The SDK only ever sees the
// cancelled context the watchdog caused, which on its own reads as the CALLER
// giving up — fatal, and wrong — so Err must swap it for the stall.
func TestSilenceEndsTheAttemptAsATimeout(t *testing.T) {
	t.Parallel()
	ctx, w := WatchIdle(context.Background(), 30*time.Millisecond)
	defer w.Stop()
	<-ctx.Done()

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
	ctx, w := WatchIdle(context.Background(), time.Millisecond)
	defer w.Stop()
	<-ctx.Done()
	if err := w.Err(nil); err != nil {
		t.Fatalf("Err(nil) = %v, want nil", err)
	}
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
