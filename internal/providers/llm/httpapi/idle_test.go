package httpapi

import (
	"context"
	"errors"
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
func TestALongStreamThatKeepsTalkingIsNeverCutOff(t *testing.T) {
	t.Parallel()
	const idle = 80 * time.Millisecond
	ctx, w := WatchIdle(context.Background(), idle)
	defer w.Stop()
	body := w.Body(&slowBody{gap: idle / 4, reads: 16}) // ~4x idle in total
	start := time.Now()
	got, err := io.ReadAll(body)
	if err != nil || len(got) != 16 {
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
