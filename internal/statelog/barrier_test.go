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
// A PubAck at N proves N committed by a majority, but a repeated message id
// inside the duplicate window is answered from the window with NO quorum round
// trip: a barrier carrying one would hand back a sequence nothing confirmed.
func TestBarrierNeverAcceptsADuplicateAck(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	idx, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), h.log, h.log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
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

// AND A DUPLICATE ACKNOWLEDGEMENT IS A REFUSAL RATHER THAN A POSITION, however
// the broker came to serve one: its sequence is the one thing it did not
// establish.
func TestABarrierServedFromTheDedupeWindowIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	idx, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), duplicating{h.log}, h.log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode,
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

// A READER MAY ONLY USE A BARRIER THAT STARTED AT OR AFTER IT ARRIVED: one
// already in flight was proposed before the reader existed, so its sequence
// bounds nothing the reader asked about ([statelog.ReadIndex.Read]).
//
// The first append is held open in a gate, so the second reader provably
// arrives after it started.
func TestAReaderNeverJoinsABarrierThatWasAlreadyInFlight(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	gate := &gatedBarrier{inner: h.log, started: make(chan struct{}), release: make(chan struct{})}
	idx, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), gate, members(1), testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
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

	// The first append is in flight and held.
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

// AND CONCURRENT READERS ARE ALL SERVED ABOVE A BARRIER THAT PREDATES THEM:
// sixteen arriving at once share what appends they legitimately can, and not
// one is handed a position from before it existed.
func TestConcurrentReadersAreAllServedAboveTheirOwnArrival(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	idx, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), h.log, h.log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
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

// READERS THAT ARRIVE DURING ONE BARRIER ARE ALL SERVED BY THE NEXT ONE: the
// coalescing [statelog.ReadIndex.Read] rests on. A reader that retook its
// arrival on waking would be held to a barrier started after IT WOKE, which
// the first to wake has already claimed, and go round again.
//
// The first barrier is held in a gate while fifteen readers wait on it, each
// reader's context reporting every settle to wait ([waitReporter]), and the
// second is held until all fifteen wait on it. Released, it must serve them
// all, and no third barrier may start.
func TestReadersThatArriveDuringOneBarrierAreServedByTheNext(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	gate := &gatedBarrier{inner: h.log, started: make(chan struct{}), release: make(chan struct{})}
	idx, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), gate, members(1), testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}

	firstErr := make(chan error, 1)
	var first statelog.Position
	go func() {
		var err error
		first, err = idx.Read(t.Context())
		firstErr <- err
	}()
	<-gate.started // the first barrier is in flight, and held

	const readers = 15
	waits := make(chan struct{}, 4*readers)
	out := make([]statelog.Position, readers)
	errs := make([]error, readers)
	var wg sync.WaitGroup
	for i := range readers {
		wg.Go(func() {
			out[i], errs[i] = idx.Read(&waitReporter{Context: t.Context(), waits: waits})
		})
	}
	for range readers {
		<-waits // every reader is waiting on the first barrier
	}
	gate.release <- struct{}{}
	<-gate.started // the second barrier is in flight, and held
	for range readers {
		<-waits // every reader is waiting on the second barrier
	}
	gate.release <- struct{}{}

	// ANY FURTHER BARRIER is let through and counted rather than left to
	// hang its readers, so a failure names how many there were.
	stop, more := make(chan struct{}), make(chan int)
	go func() {
		n := 0
		for {
			select {
			case <-gate.started:
				n++
				gate.release <- struct{}{}
			case <-stop:
				more <- n
				return
			}
		}
	}()
	wg.Wait()
	close(stop)
	if n := <-more; n != 0 {
		t.Fatalf("%d readers that arrived during one barrier took %d more after the next — "+
			"each must be served by the first barrier that started after it arrived", readers, n)
	}
	if err := <-firstErr; err != nil {
		t.Fatalf("the first read: %v", err)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("reader %d: %v", i, err)
		}
		if out[i] != out[0] || out[i].Seq <= first.Seq {
			t.Fatalf("reader %d was served %s and reader 0 %s, after a first barrier at %s; "+
				"want every one served by the one barrier after the first", i, out[i], out[0], first)
		}
	}
}

// waitReporter is a reader's context that reports each time the reader settles
// to wait on a barrier: [statelog.ReadIndex.Read] asks its context for Done
// only once it has claimed the barrier it will wait on, so the n-th report is
// the reader waiting on its n-th.
//
// A report nobody has room for is DROPPED, not waited on, so a reader that
// goes round more often than staged (the failure under test) still finishes.
type waitReporter struct {
	context.Context
	waits chan<- struct{}
}

func (w *waitReporter) Done() <-chan struct{} {
	select {
	case w.waits <- struct{}{}:
	default:
	}
	return w.Context.Done()
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

// A BARRIER'S RECORD VERSION IS ONE, FOR EVER: a barrier a build cannot decode
// is deferred, a permanent read outage on that node ([statelog.BarrierVersion]).
func TestABarriersVersionIsPinnedAtOne(t *testing.T) {
	t.Parallel()
	if statelog.BarrierVersion != 1 {
		t.Fatalf("BarrierVersion = %d, want 1 — a barrier above a build's own "+
			"record version is deferred, and a deferred barrier is a permanent "+
			"read outage on that node", statelog.BarrierVersion)
	}
	if statelog.FrameworkScope == "" {
		t.Fatal("a barrier declares no scope — an empty scope is refused everywhere " +
			"else and a barrier must not be the exception")
	}
}
