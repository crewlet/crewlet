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

// THE IDLE BUDGET IS THE CALLER'S, on every read and not just the first.
//
// `newIdleReader` took an `idle` duration, armed the first timer with it, and
// then reset with the package constant — so a caller asking for anything else
// got its budget once and ten minutes for the rest of the stream. Nothing
// noticed because the only caller passes the constant, which is exactly the
// shape that breaks the day a second one does not.
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

	// Well inside the package constant and well past the budget asked for:
	// only a reset that honoured the parameter fires here.
	time.Sleep(150 * time.Millisecond)
	if !cancelled.Load() {
		t.Errorf("an idle stream was not abandoned %s after its last byte, "+
			"so the reset used something other than the 30ms budget", 150*time.Millisecond)
	}
}

// AND A DELIVERING STREAM SURVIVES PAST ITS OWN BUDGET, which is the other
// half of the same claim: the budget bounds the GAP between bytes, not the
// life of the stream.
//
// Driven through a countdown this case owns rather than a real one, because
// the claim is a NEGATIVE — the stream was not abandoned — and an
// abandonment that has not happened has no signal to wait on. Against wall
// time the only way to make it is to sleep between deliveries and hope the
// budget outlasts the scheduler, which is a claim about the machine: this
// case slept 10ms against a 40ms budget and went red on a loaded CI box with
// the reader behaving perfectly. See newIdleReaderOn.
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

// expire runs the callback the way a real timer's own goroutine would.
func (f *fakeTimer) expire() { f.fire() }
