package sandbox

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingReader delivers one byte per Read and then blocks forever, so a
// case can watch what the idle timer does between deliveries.
type blockingReader struct {
	deliveries int
	block      chan struct{}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if b.deliveries > 0 {
		b.deliveries--
		p[0] = 'x'
		return 1, nil
	}
	<-b.block
	return 0, io.EOF
}

// THE IDLE BUDGET IS THE CALLER'S, on every read and not just the first: a
// reset that reached for the package constant would give a caller its budget
// once and the constant for the rest of the stream.
func TestTheIdleReaderKeepsTheBudgetItWasGiven(t *testing.T) {
	t.Parallel()
	var cancelled atomic.Bool
	body := &blockingReader{deliveries: 1, block: make(chan struct{})}
	defer close(body.block)

	// A budget nothing in this package names, so a reset that reached for
	// the constant would be off by minutes rather than by nothing.
	r := newIdleReader(body, func() { cancelled.Store(true) }, 30*time.Millisecond)
	defer r.timer.Stop()

	// The first read delivers, which re-arms the timer with the SAME
	// budget. Then nothing arrives.
	if n, err := r.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("first Read = (%d, %v), want (1, nil)", n, err)
	}
	if cancelled.Load() {
		t.Fatal("the request was cancelled while the stream was still delivering")
	}

	// WAITED FOR rather than sampled: firing is the direction a slow machine
	// helps, and one look after a fixed sleep claims the timer's goroutine
	// was scheduled by then. The deadline need only sit far inside the
	// package constant (minutes) for a reset to the constant to fail here.
	const deadline = 10 * time.Second
	waited := time.Now()
	//fixedwait:measure a poll's own deadline: the cancel is asserted after it, and a reset to the package constant fires minutes past it
	for !cancelled.Load() && time.Since(waited) < deadline {
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
	if !cancelled.Load() {
		t.Errorf("an idle stream was not abandoned %s after its last byte, "+
			"so the reset used something other than the 30ms budget — the "+
			"package constant is %s", deadline, e2bStreamIdleTimeout)
	}
}

// AND A DELIVERING STREAM SURVIVES PAST ITS OWN BUDGET: the budget bounds the
// GAP between bytes, not the life of the stream.
//
// Driven through a countdown the case owns (newIdleReaderOn), because the
// claim is a NEGATIVE: an abandonment that has not happened has no signal to
// wait on, and against wall time it would rest on the scheduler.
func TestDeliveringBytesPostponesTheAbandonment(t *testing.T) {
	t.Parallel()
	var cancelled atomic.Bool
	body := &blockingReader{deliveries: 20, block: make(chan struct{})}
	defer close(body.block)

	const budget = 40 * time.Millisecond
	var countdown *fakeTimer
	r := newIdleReaderOn(body, func() { cancelled.Store(true) }, budget,
		func(d time.Duration, fire func()) idleTimer {
			countdown = &fakeTimer{armed: d, fire: fire}
			return countdown
		})
	defer r.timer.Stop()

	buf := make([]byte, 1)
	for range 20 {
		if _, err := r.Read(buf); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Read: %v", err)
		}
	}

	// Every delivery re-armed, and re-armed with the CALLER's budget — the
	// two halves that together mean a stream is bounded by its gaps.
	if got := countdown.resets(); got != 20 {
		t.Errorf("20 deliveries re-armed the budget %d times, want 20", got)
	}
	if got := countdown.last(); got != budget {
		t.Errorf("a delivery re-armed the budget with %s, want the caller's %s", got, budget)
	}
	if cancelled.Load() {
		t.Error("a stream that never stopped delivering was abandoned")
	}

	// And the countdown the reader armed is a real one: when it expires,
	// the request is cancelled rather than merely failing the read.
	countdown.expire()
	if !cancelled.Load() {
		t.Error("the armed countdown expired without cancelling the request")
	}
}

// fakeTimer is a countdown the test expires by hand, recording what the
// reader did to it.
type fakeTimer struct {
	mu    sync.Mutex
	armed time.Duration
	count int
	fire  func()
}

func (f *fakeTimer) Reset(d time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = d
	f.count++
	return true
}

func (f *fakeTimer) Stop() bool { return true }

func (f *fakeTimer) resets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

func (f *fakeTimer) last() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.armed
}

// expire fires the countdown synchronously, in the CALLER's goroutine: a
// timer goroutine would bring back the scheduling race the fake removes, and
// the reader does not care which goroutine calls the callback.
func (f *fakeTimer) expire() { f.fire() }
