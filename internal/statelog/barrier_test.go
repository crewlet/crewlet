package statelog_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

func probeEncode(env statelog.Envelope) ([]byte, error) { return json.Marshal(env) }

// A BARRIER CARRIES NO MESSAGE ID, AND THAT IS THE PROOF ITSELF.
//
// A linearizable read needs to know the log's end at or after the read
// arrived, and a barrier's acknowledgement is what establishes it: the server
// proposes rather than stores, and the acknowledgement is written from the
// apply path, so a PubAck at N proves N is committed by a majority.
//
// A repeated message id inside the duplicate window is served straight out of
// that window with NO quorum round trip at all. So a barrier carrying one
// would hand back a sequence nothing confirmed — a stale position with a
// quorum's authority behind it, which is the worst shape a read can have.
func TestBarrierNeverAcceptsADuplicateAck(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	idx, err := statelog.NewReadIndex(probeDomain{}, h.log, probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}

	// TWICE, INSIDE THE DUPLICATE WINDOW. The window is two minutes and
	// these are milliseconds apart, so an id would collapse them.
	first, err := idx.Read(t.Context())
	if err != nil {
		t.Fatalf("the first barrier: %v", err)
	}
	second, err := idx.Read(t.Context())
	if err != nil {
		t.Fatalf("the second barrier: %v", err)
	}

	if first.Seq == 0 || second.Seq == 0 {
		t.Fatalf("a barrier answered sequence 0: %s then %s", first, second)
	}
	if second.Seq <= first.Seq {
		t.Fatalf("the second barrier answered %s, at or below the first's %s — "+
			"which is what a duplicate acknowledgement served out of the dedupe "+
			"window looks like, and it proves nothing about the log's end",
			second, first)
	}
	if first.Stream != probeStream || second.Stream != probeStream {
		t.Errorf("a barrier's position names stream %q/%q, want %q",
			first.Stream, second.Stream, probeStream)
	}
	if first.Generation != h.gen.Load() {
		t.Errorf("a barrier's position is at generation %d, want %d",
			first.Generation, h.gen.Load())
	}
}

// AND A DUPLICATE ACKNOWLEDGEMENT IS A REFUSAL RATHER THAN A POSITION.
//
// If the broker ever serves one — a client that started sending an id, a
// server that collapses on something else — the read index must refuse rather
// than hand back the sequence, because the sequence is the one thing the
// duplicate did not establish.
func TestABarrierServedFromTheDedupeWindowIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	idx, err := statelog.NewReadIndex(probeDomain{}, duplicating{h.log}, probeEncode,
		h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	_, err = idx.Read(t.Context())
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("a duplicate barrier acknowledgement = %v, want an Unavailable", err)
	}
}

// duplicating answers every append as a duplicate, which is what the dedupe
// window does for a repeated message id.
type duplicating struct{ inner statelog.Appender }

func (d duplicating) Append(ctx context.Context, subject, msgID string, expect *uint64, body []byte) (uint64, bool, error) {
	seq, _, err := d.inner.Append(ctx, subject, msgID, expect, body)
	return seq, true, err
}

func (d duplicating) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	return d.inner.LastSeq(ctx, subject)
}

// A READER MAY ONLY USE A BARRIER THAT STARTED AT OR AFTER IT ARRIVED.
//
// Coalescing concurrent readers onto one append is what keeps a company's
// read rate off the raft log, and readers that arrive while nothing is in
// flight legitimately share the append one of them starts. But an append
// ALREADY IN FLIGHT was proposed before this reader existed, so its sequence
// is not a bound on anything this reader asked about — joining it would
// answer a stale position with a quorum's authority behind it, which is the
// worst shape a read can have.
//
// The staging is exact rather than timed: the first append is held open in a
// gate, so the second reader provably arrives after it started.
func TestAReaderNeverJoinsABarrierThatWasAlreadyInFlight(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	gate := &gatedBarrier{inner: h.log, started: make(chan struct{}), release: make(chan struct{})}
	idx, err := statelog.NewReadIndex(probeDomain{}, gate, probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}

	firstDone := make(chan statelog.Position, 1)
	firstErr := make(chan error, 1)
	go func() {
		p, err := idx.Read(t.Context())
		firstErr <- err
		firstDone <- p
	}()

	// The first append is now in flight and cannot complete until this
	// test lets it, so the second reader below arrives strictly after it
	// started.
	<-gate.started

	secondDone := make(chan statelog.Position, 1)
	secondErr := make(chan error, 1)
	go func() {
		p, err := idx.Read(t.Context())
		secondErr <- err
		secondDone <- p
	}()

	// Let both appends through: the first, and the one the second reader
	// must start for itself.
	go func() {
		for range 2 {
			gate.release <- struct{}{}
			<-gate.started
		}
	}()

	if err := <-firstErr; err != nil {
		t.Fatalf("the first read: %v", err)
	}
	first := <-firstDone
	if err := <-secondErr; err != nil {
		t.Fatalf("the second read: %v", err)
	}
	second := <-secondDone

	if second.Seq <= first.Seq {
		t.Fatalf("the second reader was served %s, at or below the barrier that "+
			"was already in flight when it arrived (%s) — that append was "+
			"proposed before the read existed, so its sequence bounds nothing "+
			"the read asked about", second, first)
	}
}

// AND CONCURRENT READERS ARE ALL SERVED ABOVE A BARRIER THAT PREDATES THEM.
//
// The sequential case is the easy half. This is the one that matters: sixteen
// readers arriving at once, sharing whatever appends they legitimately can,
// and not one of them handed a position from before it existed.
func TestConcurrentReadersAreAllServedAboveTheirOwnArrival(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	idx, err := statelog.NewReadIndex(probeDomain{}, h.log, probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	base, err := idx.Read(t.Context())
	if err != nil {
		t.Fatalf("the baseline barrier: %v", err)
	}

	const readers = 16
	var wg sync.WaitGroup
	out := make([]statelog.Position, readers)
	errs := make([]error, readers)
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], errs[i] = idx.Read(t.Context())
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("reader %d: %v", i, err)
		}
		if out[i].Seq <= base.Seq {
			t.Fatalf("reader %d was served %s, at or below a barrier that "+
				"completed before it started (%s)", i, out[i], base)
		}
		if out[i].Stream != probeStream {
			t.Errorf("reader %d was served a position on stream %q", i, out[i].Stream)
		}
	}
}

// gatedBarrier lets a test hold an append open, so a staging that depends on
// one being in flight is exact rather than a sleep.
type gatedBarrier struct {
	inner   statelog.Appender
	started chan struct{}
	release chan struct{}
}

func (g *gatedBarrier) Append(ctx context.Context, subject, msgID string, expect *uint64, body []byte) (uint64, bool, error) {
	select {
	case g.started <- struct{}{}:
		<-g.release
	case <-ctx.Done():
		return 0, false, ctx.Err()
	}
	return g.inner.Append(ctx, subject, msgID, expect, body)
}

func (g *gatedBarrier) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	return g.inner.LastSeq(ctx, subject)
}

// A BARRIER'S RECORD VERSION IS ONE, FOR EVER.
//
// A record version is normally free to move. This one is not: a barrier a
// build cannot decode would be DEFERRED, and a deferred barrier is a
// self-inflicted permanent read outage on the node that deferred it. The
// record has no payload to extend, so nothing a later version could carry is
// worth that.
func TestABarriersVersionIsPinnedAtOne(t *testing.T) {
	t.Parallel()
	if statelog.BarrierVersion != 1 {
		t.Fatalf("BarrierVersion = %d, want 1 — a barrier above a build's own "+
			"record version is deferred, and a deferred barrier is a permanent "+
			"read outage on that node", statelog.BarrierVersion)
	}
	if statelog.BarrierScope == "" {
		t.Fatal("a barrier declares no scope — an empty scope is refused everywhere " +
			"else and a barrier must not be the exception")
	}
}
