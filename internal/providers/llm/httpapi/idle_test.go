package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// slowBody yields one byte per read after a fixed pause, for a fixed number of
// reads: a stream that is slow but never silent for longer than its gap.
type slowBody struct {
	gap   time.Duration
	reads int
}

func (b *slowBody) Read(p []byte) (int, error) {
	if b.reads == 0 {
		return 0, io.EOF
	}
	time.Sleep(b.gap)
	b.reads--
	p[0] = 'x'
	return 1, nil
}

func (*slowBody) Close() error { return nil }

// A STREAM IS BOUNDED BY ITS SILENCE, NOT ITS LENGTH: a body that keeps
// arriving runs well past the idle bound in total and is never cut off,
// because each read that returns data restarts the clock.
//
// The bound is wide enough that the test measures the watchdog and not the
// machine: what the clock sees between two reads is the body's gap PLUS
// however long the reading goroutine waits to be scheduled, and at 80 ms with
// a 20 ms gap a race-instrumented suite on four busy CPUs stalled it past the
// remaining 60 ms and cancelled a stream that never went silent. A 300 ms
// margin is a stall nothing healthy produces.
func TestALongStreamThatKeepsTalkingIsNeverCutOff(t *testing.T) {
	t.Parallel()
	const idle = 400 * time.Millisecond
	ctx, w := WatchIdle(context.Background(), idle)
	defer w.Stop()
	body := w.Body(&slowBody{gap: idle / 4, reads: 12}) // ~3x idle in total
	start := time.Now()
	got, err := io.ReadAll(body)
	if err != nil || len(got) != 12 {
		t.Fatalf("read %d bytes, err %v", len(got), err)
	}
	if elapsed := time.Since(start); elapsed < 2*idle {
		t.Fatalf("the body took %v, want well past the %v bound to prove it is not a total", elapsed, idle)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("a stream that never went silent was cancelled: %v", context.Cause(ctx))
	}
}

// SILENCE ENDS THE ATTEMPT, as a TIMEOUT: retryable on the next member of the
// chain and never a reason to bench the key. Silent before its first byte, a
// request fails with the bare cancellation the watchdog caused, which on its
// own reads as the CALLER giving up — fatal, and wrong — so Err must swap it
// for the stall.
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

// A STALL THE SDK ALREADY REPORTED IS NAMED ONCE. Part-way through a stream
// the failure is a read of the body, which the watched body reports as the
// stall itself on either protocol ([TestABodyReadTheStallFailedReportsTheStall])
// and which the SDK hands back wrapped in its own words. Swapped for the stall
// again, the message a person reads said "stream went silent" twice over.
//
// Mutation: drop the errors.Is(err, ErrStalled) pass-through from Err.
func TestAStallTheSDKAlreadyReportedIsNamedOnce(t *testing.T) {
	t.Parallel()
	ctx, w := WatchIdle(context.Background(), 30*time.Millisecond)
	defer w.Stop()
	<-ctx.Done()

	read := fmt.Errorf("reading the stream: %w", context.Cause(ctx))
	err := w.Err(read)
	if !errors.Is(err, read) {
		t.Fatalf("err = %v, want the SDK's own report of the stall unchanged", err)
	}
	if n := strings.Count(err.Error(), "stream went silent"); n != 1 {
		t.Fatalf("err = %q names the stall %d times, want once", err, n)
	}
	if kind := FromTransport(err, "p", "m").Kind; kind != llm.KindTimeout {
		t.Fatalf("a stall classified %s, want timeout", kind)
	}
}

// cancelledBody blocks a read until ctx is done and then fails it with what
// the transport under test reports for a cancelled body read — after the
// first read, which returns data, so the stream had begun.
type cancelledBody struct {
	ctx     context.Context
	started bool
	fail    func(ctx context.Context) error
}

func (b *cancelledBody) Read(p []byte) (int, error) {
	if !b.started {
		b.started = true
		p[0] = 'x'
		return 1, nil
	}
	<-b.ctx.Done()
	return 0, b.fail(b.ctx)
}

func (*cancelledBody) Close() error { return nil }

// A BODY READ THE STALL FAILED REPORTS THE STALL, whichever protocol carried
// it. Go's HTTP/1.1 client fails a cancelled body read with the context's
// cause, which is the stall already; its HTTP/2 client — what the real API
// speaks — fails it with the bare context.Canceled, which the SDK's span reads
// as the application stopping and leaves without a status, and which Err could
// only wrap, naming a cancellation beside the stall. Both read the same
// through the watched body, and Err names the stall once.
//
// Mutation: return the wrapped body's error unchanged and the HTTP/2 case
// reads Canceled.
func TestABodyReadTheStallFailedReportsTheStall(t *testing.T) {
	t.Parallel()
	for name, fail := range map[string]func(context.Context) error{
		"HTTP/1.1, the cause": context.Cause,
		"HTTP/2, bare Err":    func(ctx context.Context) error { return ctx.Err() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, w := WatchIdle(context.Background(), 30*time.Millisecond)
			defer w.Stop()
			_, err := io.ReadAll(w.Body(&cancelledBody{ctx: ctx, fail: fail}))
			if !errors.Is(err, ErrStalled) || errors.Is(err, context.Canceled) {
				t.Fatalf("the read failed with %v, want the stall and not a cancellation", err)
			}
			reported := w.Err(fmt.Errorf("reading the stream: %w", err))
			if n := strings.Count(reported.Error(), "stream went silent"); n != 1 ||
				strings.Contains(reported.Error(), "canceled") {
				t.Fatalf("err = %q, want the stall named once and no cancellation", reported)
			}
			if kind := FromTransport(reported, "p", "m").Kind; kind != llm.KindTimeout {
				t.Fatalf("a stall classified %s, want timeout", kind)
			}
		})
	}
}

// ONLY THE STALL IS TRANSLATED: a body read the CALLER's cancellation failed
// stays the caller's, and a stream that reached its end stays ended whatever
// the timer did afterwards.
func TestABodyReadNotFailedByTheStallIsLeftAlone(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	ctx, w := WatchIdle(parent, time.Hour)
	defer w.Stop()
	cancel()
	if _, err := io.ReadAll(w.Body(&cancelledBody{ctx: ctx,
		fail: func(ctx context.Context) error { return ctx.Err() }})); !errors.Is(err, context.Canceled) ||
		errors.Is(err, ErrStalled) {
		t.Fatalf("the caller's cancellation read as %v, want it unchanged", err)
	}

	stalled, w2 := WatchIdle(context.Background(), time.Millisecond)
	defer w2.Stop()
	<-stalled.Done()
	n, err := w2.Body(io.NopCloser(strings.NewReader(""))).Read(make([]byte, 1))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("a finished body read as %d, %v after the bound passed, want its EOF", n, err)
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
