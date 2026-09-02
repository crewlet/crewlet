package sandbox

import (
	"errors"
	"io"
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
func TestDeliveringBytesPostponesTheAbandonment(t *testing.T) {
	t.Parallel()
	var cancelled atomic.Bool
	body := &blockingReader{deliveries: 20, block: make(chan struct{})}
	defer close(body.block)

	r := newIdleReader(body, func() { cancelled.Store(true) }, 40*time.Millisecond)
	defer r.timer.Stop()

	// Twenty deliveries at 10ms apart is 200ms of stream against a 40ms
	// idle budget: it survives only because each one re-arms.
	buf := make([]byte, 1)
	for range 20 {
		if _, err := r.Read(buf); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Read: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cancelled.Load() {
		t.Error("a stream delivering every 10ms was abandoned on a 40ms idle budget")
	}
}
