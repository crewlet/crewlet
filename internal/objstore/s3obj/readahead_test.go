package s3obj_test

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
)

// seeded is n bytes of seed's stream: random, so a range handed out at the
// wrong offset never reads as the right one.
func seeded(n int, seed uint64) []byte {
	data := make([]byte, n)
	r := rand.New(rand.NewPCG(seed, 7))
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	return data
}

// readCtx is a context fit for a read, which every backend refuses without a
// deadline.
func readCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// putBytes stores data under name.
func putBytes(t *testing.T, b *s3obj.Backend, name string, data []byte) {
	t.Helper()
	if err := b.Put(t.Context(), name, bytes.NewReader(data), objstore.PutMeta{}); err != nil {
		t.Fatalf("put %s: %v", name, err)
	}
}

// getBytes reads n bytes of name from off through the backend's own stream —
// everything it handed out, and how the stream ended.
func getBytes(t *testing.T, b *s3obj.Backend, name string, off, n int64) ([]byte, error) {
	t.Helper()
	r, err := b.Get(readCtx(t), name, off, n)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

// waitFor polls cond until it holds, failing the case with what after the
// deadline.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(within); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("after %v: %s", within, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// paced is the rate the cases that need a reader the bucket holds back serve
// an object's bytes at: a mebibyte a second, so a range of the cases' part
// crosses in about 65 ms and a reader copying it waits on the bucket nearly
// all that time.
const paced = 1 << 20

// A LARGE READ RUNS ITS RANGES CONCURRENTLY once its reader shows it waits on
// the bucket: [s3obj.PartsInFlight] ranges read ahead at once, and beside
// them the request for each range that will take a buffer next, sent before
// the buffer is free so its first byte is not waited for — more than
// PartsInFlight requests at the bucket, and never more than twice that. Its
// bytes are handed out strictly IN ORDER, whatever order the ranges arrive
// in. One connection is one connection's throughput from a provider that
// limits each, which is what a round's largest bundle, a large download and a
// backup's copy were held to.
//
// A read the first range holds whole costs ONE request, as it did before
// there were ranges: an object of a byte, and one exactly a part long.
//
// The order is held by a bucket that answers the second range only once the
// fifth has been answered: a reader handing ranges out as they arrive hands
// out the third first.
func TestALargeReadRunsItsRangesConcurrently(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	// A ROUND TRIP LONGER THAN A RANGE TAKES TO CROSS, so a request sent
	// only once its buffer was free would leave every range's first byte
	// to be waited for — and the requests a read sends ahead overlap at the
	// bucket with the ranges it is reading.
	srv.Latency = 100 * time.Millisecond
	srv.ConnBytesPerSecond = paced
	b := open(t, srv, "")
	data := seeded(5*part+17, 1)
	putBytes(t, b, "large", data)

	before := srv.Requests()
	got, err := getBytes(t, b, "large", 0, -1)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("a whole read of %d bytes answered %d, %v; want the object, in order", len(data), len(got), err)
	}
	if most := srv.MostGetsAtOnce(); most <= s3obj.PartsInFlight || most > 2*s3obj.PartsInFlight {
		t.Fatalf("a read of six ranges had %d requests at the bucket at once; want more than the %d "+
			"it reads ahead — a request on its way for the range each buffer takes next — and at "+
			"most %d", most, s3obj.PartsInFlight, 2*s3obj.PartsInFlight)
	}
	if n := srv.Requests() - before; n != 6 {
		t.Fatalf("a read of %d bytes in ranges of %d made %d requests, want 6", len(data), part, n)
	}
	if held := s3obj.SparesHeld(b); held != 0 {
		t.Fatalf("a finished read kept %d of the node's spare part buffers", held)
	}

	got, err = getBytes(t, b, "large", part+100, 3*part)
	if want := data[part+100 : 4*part+100]; err != nil || !bytes.Equal(got, want) {
		t.Fatalf("three parts from inside the second answered %d bytes, %v; want that slice", len(got), err)
	}

	for name, size := range map[string]int{"byte": 1, "part": part} {
		one := seeded(size, 2)
		putBytes(t, b, name, one)
		before := srv.Requests()
		got, err := getBytes(t, b, name, 0, -1)
		if err != nil || !bytes.Equal(got, one) {
			t.Fatalf("%s: a whole read answered %d bytes, %v", name, len(got), err)
		}
		if n := srv.Requests() - before; n != 1 {
			t.Fatalf("%s: a read of an object of %d bytes made %d requests, want one", name, size, n)
		}
	}

	held := s3fake.Start(t, "files")
	held.ConnBytesPerSecond = paced
	fifth := make(chan struct{})
	var once sync.Once
	held.GetHook = func(_ string, start int64) {
		switch start {
		case 4 * part:
			once.Do(func() { close(fifth) })
		case part:
			select {
			case <-fifth:
			case <-time.After(30 * time.Second):
				t.Error("the fifth range was never asked for while the second was held")
				return
			}
			// ANSWERED, NOT JUST ASKED: only this range is left in hand.
			for deadline := time.Now().Add(30 * time.Second); held.GetsInHand() != 1; {
				if time.Now().After(deadline) {
					t.Error("the ranges after the second were never answered")
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}
	hb := open(t, held, "")
	putBytes(t, hb, "large", data)
	got, err = getBytes(t, hb, "large", 0, -1)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("with its second range answered after its fifth, a read answered %d bytes, %v; "+
			"want the object, in order", len(got), err)
	}
}

// A READ HAS AT MOST TWICE [s3obj.PartsInFlight] REQUESTS AT THE BUCKET, the
// range it streams counted among them, and a read from a bucket slow to answer
// has exactly that many: one reading ahead into each of its buffers and one
// asked for ahead of each buffer freeing. The FIRST range is the one a read
// streams beside its buffers: four buffers wide once its reader had been
// handed half of it, a read that also asked a range ahead of each of the four
// had one request more than the bound.
//
// The bucket answers a get 400 ms after it is asked and sends a range in
// about 65 ms, so the read asks for ranges four buffers ahead of its reader
// while the first one still streams — a lead past the four ranges after it —
// and twelve ranges, so the end of the object is not what holds it back.
func TestAReadHasAtMostTwicePartsInFlightRequestsAtTheBucket(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	// THE FIRST BYTE OF A GET ALONE, held in the hook rather than set as
	// [s3fake.Server.Latency], which the put that stores the object would
	// pay on every one of its requests too. A get is in the bucket's
	// hand from its arrival, the hook's wait included.
	srv.GetHook = func(string, int64) { time.Sleep(400 * time.Millisecond) }
	srv.ConnBytesPerSecond = paced
	b := open(t, srv, "")
	data := seeded(12*part, 14)
	putBytes(t, b, "far", data)

	got, err := getBytes(t, b, "far", 0, -1)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("a whole read of %d bytes answered %d, %v; want the object, in order", len(data), len(got), err)
	}
	if most, want := srv.MostGetsAtOnce(), int64(2*s3obj.PartsInFlight); most != want {
		t.Fatalf("a read of twelve ranges from a bucket answering in 400 ms had %d requests at the bucket "+
			"at once; want %d — one reading ahead into each of its %d buffers and one asked for ahead "+
			"of each, the range it streams counted among them — and never more",
			most, want, s3obj.PartsInFlight)
	}
	if held := s3obj.SparesHeld(b); held != 0 {
		t.Fatalf("a finished read kept %d of the node's spare part buffers", held)
	}
}

// A READER SLOWER THAN ONE CONNECTION HOLDS ONE SPARE AT MOST, however long
// it reads: it is what the read waits on, so ranges landed ahead of it would
// buy it nothing, and the node's allowance is every other read's and upload's
// width. A download paced to a person's link runs for hours, and three that
// held a read's full width took the whole allowance from the node for as long
// as they lasted.
//
// The reader takes a quarter of a range every twentieth of a second from a
// bucket that sends it at loopback's rate: every range lands long before the
// reader reaches it.
func TestASlowReaderHoldsOneSpare(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := open(t, srv, "")
	data := seeded(6*part, 8)
	putBytes(t, b, "slow", data)

	r, err := b.Get(readCtx(t), "slow", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	got := make([]byte, 0, len(data))
	step := make([]byte, part/4)
	for len(got) < len(data) {
		n, err := io.ReadFull(r, step[:min(len(step), len(data)-len(got))])
		got = append(got, step[:n]...)
		if err != nil {
			t.Fatalf("after %d bytes: %v", len(got), err)
		}
		time.Sleep(50 * time.Millisecond)
		if held := s3obj.SparesHeld(b); held > 1 {
			t.Fatalf("a reader slower than the bucket held %d of the node's spare part buffers "+
				"after %d bytes, where one is the most it can use", held, len(got))
		}
	}
	if !bytes.Equal(got, data) {
		t.Fatal("the slow read handed out other bytes than the object's")
	}
}

// A READER THAT SLOWS DOWN GIVES ITS SPARES BACK: a read widens while its
// reader waits on the bucket and narrows once a range it read ahead sits
// landed while the reader is still on the one before — so a download that
// began on a fast link and went on at a slow one holds one spare again
// within a few ranges, rather than the four it took while it was fast. And
// it narrows a range at a time: the ranges it asked for at the fast pace,
// to be given buffers it now gives back, are withdrawn rather than each
// keeping the read at its old width until it has been given one.
func TestAReaderThatSlowsDownGivesItsSparesBack(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.ConnBytesPerSecond = paced
	b := open(t, srv, "")
	data := seeded(9*part, 9)
	putBytes(t, b, "slowing", data)

	r, err := b.Get(readCtx(t), "slowing", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	got := make([]byte, part, len(data))
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("the first range: %v", err)
	}
	if held := s3obj.SparesHeld(b); held != s3obj.PartsInFlight {
		t.Fatalf("a reader the bucket held back for a range held %d spares, want the %d of a "+
			"read at full width", held, s3obj.PartsInFlight)
	}
	got = got[:2*part]
	if _, err := io.ReadFull(r, got[part:]); err != nil {
		t.Fatalf("the second range: %v", err)
	}
	step := make([]byte, part/4)
	for len(got) < len(data) {
		n, err := io.ReadFull(r, step[:min(len(step), len(data)-len(got))])
		got = append(got, step[:n]...)
		if err != nil {
			t.Fatalf("after %d bytes: %v", len(got), err)
		}
		time.Sleep(50 * time.Millisecond)
		if held := s3obj.SparesHeld(b); len(got) >= 6*part && held > 1 {
			t.Fatalf("four ranges after its reader slowed down, a read still held %d spares, "+
				"where one is the most a reader slower than the bucket can use", held)
		}
	}
	if !bytes.Equal(got, data) {
		t.Fatal("the read handed out other bytes than the object's")
	}
}

// A RANGE OF AN OBJECT REPLACED MID-READ FAILS THE READ, naming the change,
// and no byte of the other object is handed out: every range after the first
// is held to the first one's tag (If-Match), so a bucket somebody wrote into
// while it was read answers 412 rather than the other object's bytes — never
// two objects stitched together. Keys are never reused, so it is a store
// written behind the engine's back, and it is still the reader's to refuse.
//
// The replacement is as long as the object, so only the tag can tell them
// apart: a reader that asked for its ranges unconditionally would read the
// replacement's bytes at the same offsets and succeed.
func TestARangeOfAnObjectReplacedMidReadFailsTheRead(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b, other := open(t, srv, ""), open(t, srv, "")
	old := seeded(5*part+17, 3)
	replacement := bytes.Repeat([]byte{0xee}, len(old))
	var once sync.Once
	srv.GetHook = func(_ string, start int64) {
		if start == 2*part {
			once.Do(func() { putBytes(t, other, "replaced", replacement) })
		}
	}
	putBytes(t, b, "replaced", old)

	got, err := getBytes(t, b, "replaced", 0, -1)
	if err == nil || !strings.Contains(err.Error(), "changed while it was read") {
		t.Fatalf("a read whose object was replaced after its second range = %d bytes, %v; "+
			"want a failure naming the change", len(got), err)
	}
	if len(got) < part || len(got) > 2*part || !bytes.Equal(got, old[:len(got)]) {
		t.Fatalf("the failed read handed out %d bytes; want the first one or two ranges of the "+
			"object it began reading, and nothing of the replacement", len(got))
	}
}

// A RANGE THAT FAILS ENDS THE RANGES AFTER IT AT ONCE, while every range
// before it still lands and is handed out: the read cannot get past the
// failure, so a range behind it moving its bytes — and holding a connection
// and a spare buffer — is paying for nothing. And once a range has failed
// the read asks for no range more, and puts no buffer to a new one.
//
// The third range is answered 412 — the object replaced under it — while the
// fourth and fifth, asked beside it, are still crossing from the bucket at a
// second a range and the reader has stopped at the start of the second. Both
// must leave the bucket before the reader moves; once it does, it is handed
// the second range and then the failure, and nothing past the third is ever
// asked for again.
func TestAFailedRangeEndsTheRangesAfterIt(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	// A SECOND A RANGE, so the ranges behind the failed one are still on
	// their way unless something ends them.
	srv.ConnBytesPerSecond = part
	b, other := open(t, srv, ""), open(t, srv, "")
	old := seeded(6*part, 10)
	putBytes(t, b, "failing", old)

	var (
		fourth, fifth = make(chan struct{}), make(chan struct{})
		failed        = make(chan struct{})
		settled       = make(chan struct{})
		once          [3]sync.Once
		late          atomic.Int64
	)
	srv.GetHook = func(_ string, start int64) {
		select {
		case <-settled:
			if start > 2*part {
				late.Add(1)
			}
			return
		default:
		}
		switch start {
		case 3 * part:
			once[0].Do(func() { close(fourth) })
		case 4 * part:
			once[1].Do(func() { close(fifth) })
		case 2 * part:
			// THE FOURTH AND FIFTH READ THE OBJECT BEFORE IT IS
			// REPLACED, so they are moving its bytes when the third
			// fails.
			for _, ch := range []chan struct{}{fourth, fifth} {
				select {
				case <-ch:
				case <-time.After(30 * time.Second):
					t.Error("the fourth and fifth ranges were never asked for beside the third")
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
			once[2].Do(func() {
				putBytes(t, other, "failing", bytes.Repeat([]byte{0xee}, len(old)))
				close(failed)
			})
		}
	}

	r, err := b.Get(readCtx(t), "failing", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	first := make([]byte, part)
	if _, err := io.ReadFull(r, first); err != nil {
		t.Fatalf("the first range: %v", err)
	}
	select {
	case <-failed:
	case <-time.After(30 * time.Second):
		t.Fatal("the third range was never asked for")
	}
	waitFor(t, 500*time.Millisecond, "the ranges after a failed one were still at the bucket", func() bool {
		return srv.GetsInHand() == 0
	})
	close(settled)

	rest, err := io.ReadAll(r)
	if err == nil || !strings.Contains(err.Error(), "changed while it was read") {
		t.Fatalf("past its first range the read answered %d bytes, %v; want the second range and "+
			"then the failure", len(rest), err)
	}
	if !bytes.Equal(rest, old[part:2*part]) {
		t.Fatalf("before its failure the read handed out %d bytes; want the second range whole", len(rest))
	}
	if n := late.Load(); n != 0 {
		t.Fatalf("%d ranges past the failed one were asked for after it failed", n)
	}
	// THE THIRD, FOURTH AND FIFTH RANGES' BUFFERS, until Close: the
	// second's went back to the node once it was handed out, rather than
	// to a range the read would never reach.
	if held := s3obj.SparesHeld(b); held != s3obj.PartsInFlight-1 {
		t.Fatalf("a read whose third range failed held %d spares once the second was handed "+
			"out, want %d", held, s3obj.PartsInFlight-1)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if held := s3obj.SparesHeld(b); held != 0 {
		t.Fatalf("a closed read kept %d of the node's spare part buffers", held)
	}
}

// A RANGE READ AHEAD IS HANDED OUT AS IT LANDS, a piece at a time, never only
// once the whole range has: [objstore.Store]'s stall watchdog ends a read that
// hears nothing for a minute, and a part crossing a quarter of a slow link —
// the range the reader waits on, sharing it with the ranges behind — takes
// minutes to land whole. Handed over whole, a read the provider was moving
// would be ended as stalled.
//
// The bucket begins the second range only once the reader has drained the
// first, and sends it at a pace that takes four seconds: the reader's next
// Read must answer with the piece that has landed rather than wait for the
// rest.
func TestARangeReadAheadIsHandedOutAsItLands(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.ConnBytesPerSecond = 16 << 10
	drained := make(chan struct{})
	srv.GetHook = func(_ string, start int64) {
		if start == part {
			select {
			case <-drained:
			case <-time.After(30 * time.Second):
				t.Error("the reader never drained its first range")
			}
		}
	}
	b := open(t, srv, "")
	data := seeded(2*part, 7)
	putBytes(t, b, "paced", data)

	r, err := b.Get(readCtx(t), "paced", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	first := make([]byte, part)
	if _, err := io.ReadFull(r, first); err != nil || !bytes.Equal(first, data[:part]) {
		t.Fatalf("the first range read %v; want its bytes", err)
	}
	close(drained)
	next := make([]byte, part)
	n, err := r.Read(next)
	if err != nil || n == 0 || !bytes.Equal(next[:n], data[part:part+n]) {
		t.Fatalf("the second range's first Read = %d bytes, %v; want the bytes that had landed", n, err)
	}
	if n == part {
		t.Fatalf("the second range was handed out only once all %d of its bytes had landed, "+
			"where a piece of it had landed seconds before", part)
	}
}

// CLOSING A LARGE READ STOPS ITS RANGES: every range in flight is cancelled —
// the bucket holds no get of it within a second — and every spare buffer the
// read held is back with the node by the time Close returns. A download whose
// client went away would otherwise go on paying for up to four ranges, and
// hold their buffers from every other read and upload on the node.
func TestClosingALargeReadStopsItsRanges(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	// A SECOND A RANGE, so every range the read asked for is still on its
	// way when it is closed.
	srv.ConnBytesPerSecond = part
	b := open(t, srv, "")
	putBytes(t, b, "closed", seeded(6*part, 4))

	r, err := b.Get(readCtx(t), "closed", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	// THE FIRST RANGE WHOLE, waited on: the read widens as it ends.
	if _, err := io.ReadFull(r, make([]byte, part)); err != nil {
		t.Fatalf("the first range: %v", err)
	}
	waitFor(t, 10*time.Second, "the read never had its ranges in flight", func() bool {
		return srv.GetsInHand() >= s3obj.PartsInFlight-1
	})
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if held := s3obj.SparesHeld(b); held != 0 {
		t.Fatalf("a closed read kept %d of the node's spare part buffers", held)
	}
	waitFor(t, time.Second, "the bucket still holds gets of a closed read", func() bool {
		return srv.GetsInHand() == 0
	})
	if _, err := r.Read(make([]byte, 1)); err == nil {
		t.Fatal("a closed read answered a read")
	}
}

// NO RANGE IS STILL BEING WRITTEN WHEN CLOSE RETURNS: every buffer a read
// held goes back to the node's pool as Close returns, and the next upload or
// read draws it from there and writes it — so a range's goroutine still
// reading its body into one would be writing under somebody else's bytes.
// Close waits for every range's goroutine before it gives a buffer back.
//
// NOT PARALLEL: it looks for any goroutine of this package still filling a
// range, and a parallel case's reads would be found too. The ranges are held
// mid-body by a bucket sending a second a range, and each takes a third of a
// second to let go of its answer once cancelled ([s3obj.LateToLetGo]); the
// read is closed, and then an upload draws every pooled buffer and writes it
// whole — a write the race detector reports against any range still filling
// one.
func TestAClosedReadFillsNoBufferAfterClose(t *testing.T) {
	srv := s3fake.Start(t, "files")
	srv.ConnBytesPerSecond = part
	b := open(t, srv, "")
	putBytes(t, b, "filling", seeded(6*part, 11))
	s3obj.LateToLetGo(b, 300*time.Millisecond)

	r, err := b.Get(readCtx(t), "filling", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(r, make([]byte, part)); err != nil {
		t.Fatalf("the first range: %v", err)
	}
	waitFor(t, 10*time.Second, "the read never had its ranges in flight", func() bool {
		return srv.GetsInHand() >= s3obj.PartsInFlight-1
	})
	// PART WAY INTO THEIR BODIES: some of every range's buffer written.
	time.Sleep(100 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := goroutinesIn("s3obj.(*wideRead).fill("); n != 0 {
		t.Fatalf("%d ranges were still filling their buffers when Close returned, and those "+
			"buffers were back in the node's pool", n)
	}
	putBytes(t, b, "drawn", seeded((s3obj.PartsInFlight+1)*part, 12))
}

// goroutinesIn is how many goroutines have frame — a function's name as a
// stack trace prints it — on their stack now.
func goroutinesIn(frame string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), frame)
		}
		buf = make([]byte, 2*len(buf))
	}
}

// A READ FINDING NO SPARE STREAMS ITS RANGES ONE AT A TIME, and still
// completes, at one connection's rate rather than below it: a read takes a
// buffer only while the node's allowance has one free and never waits for
// one, so however many reads and uploads are under way the node's part
// memory is that one allowance — and the next range's request is sent while
// the range before it is still being read, so its first byte arrives while
// the reader is busy rather than after. A range asked for only once the
// reader reached it cost a first byte every part, which a read in one stream
// paid once.
//
// The bucket answers a twentieth of a second after it is asked and sends a
// range in about 65 ms: the next range's request must be at the bucket while
// the range before it is still crossing — two at once, and never more.
func TestAReadFindingNoSpareStreamsItsRanges(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.Latency = 50 * time.Millisecond
	srv.ConnBytesPerSecond = paced
	b := open(t, srv, "")
	data := seeded(5*part+17, 5)
	putBytes(t, b, "streamed", data)

	giveBack := s3obj.TakeSpares(b)
	// TEN SECONDS for six ranges a twentieth of a second away: a read that
	// waited for a spare instead of streaming would wait on the case, which
	// gives none back until the read is over.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	r, err := b.Get(ctx, "streamed", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("a read with no spare answered %d bytes, %v; want the object, without waiting for "+
			"a spare", len(got), err)
	}
	if most := srv.MostGetsAtOnce(); most != 2 {
		t.Fatalf("a read with no spare had %d gets at the bucket at once; want two — the range "+
			"being read and the next one's request on its way", most)
	}
	if held := s3obj.SparesHeld(b); held != s3obj.SpareParts {
		t.Fatalf("the node's allowance holds %d spares taken, want the %d the case took", held, s3obj.SpareParts)
	}
	giveBack()
	if held := s3obj.SparesHeld(b); held != 0 {
		t.Fatalf("%d spares are still taken once the case gave its own back", held)
	}
}

// AN OBJECT THE BUCKET GIVES NO TAG IS NOT READ PAST ITS FIRST RANGE: with no
// ETag there is nothing to hold the later ranges to, and ranges read
// regardless could stitch two objects together. S3 tags every object; a
// gateway that does not is refused by name, before a byte is handed out — and
// a read its first range holds whole needs no tag.
func TestAnUntaggedObjectIsNotReadPastItsFirstRange(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.Untagged = true
	b := open(t, srv, "")
	putBytes(t, b, "small", []byte("one range"))
	if got, err := getBytes(t, b, "small", 0, -1); err != nil || string(got) != "one range" {
		t.Fatalf("an untagged object of one range read %q, %v", got, err)
	}
	putBytes(t, b, "large", seeded(2*part, 6))
	r, err := b.Get(readCtx(t), "large", 0, -1)
	if err == nil {
		_ = r.Close()
		t.Fatal("an untagged object of two ranges was read")
	}
	if !strings.Contains(err.Error(), "ETag") {
		t.Fatalf("an untagged object of two ranges = %v; want a refusal naming the missing ETag", err)
	}
	if held := s3obj.SparesHeld(b); held != 0 {
		t.Fatalf("a refused read kept %d of the node's spare part buffers", held)
	}
}

// A RANGE IS READ AS S3 SPELLS ITS ANSWER, and held to the range asked for:
// `bytes first-last/size`, beginning where it was asked to, no longer than
// was asked and — once the first range has said how large the object is —
// describing an object of that size. Anything else is a bucket answering
// another range than the one asked, and is refused rather than handed out at
// the wrong offset or past the read's end.
func TestARangeIsHeldToTheRangeAskedFor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		header           string
		from, most, size int64
		n, total         int64
		refused          bool
		description      string
	}{
		{"bytes 0-9/10", 0, 16, -1, 10, 10, false, "a whole object shorter than asked"},
		{"bytes 5-9/10", 5, 5, -1, 5, 10, false, "the rest from an offset"},
		{"bytes 8-15/100", 8, 8, 100, 8, 100, false, "a range of an object of a known size"},
		{"", 0, 16, -1, 0, 0, true, "no Content-Range: the bucket ignored the range"},
		{"bytes 4-9/10", 0, 16, -1, 0, 0, true, "another start than the one asked"},
		{"bytes 0-15/100", 0, 8, -1, 0, 0, true, "more than was asked"},
		{"bytes 8-15/99", 8, 8, 100, 0, 0, true, "another object's size"},
		{"bytes 0-9/*", 0, 16, -1, 0, 0, true, "no size"},
		{"bytes 9-5/10", 9, 16, -1, 0, 0, true, "a range that ends before it begins"},
		{"bytes 0-10/10", 0, 16, -1, 0, 0, true, "a range past the object's end"},
		{"items 0-9/10", 0, 16, -1, 0, 0, true, "another unit"},
		{"bytes 0-9", 0, 16, -1, 0, 0, true, "no size at all"},
	} {
		n, total, err := s3obj.SpanOf(c.header, c.from, c.most, c.size)
		switch {
		case c.refused && err == nil:
			t.Errorf("%s (%q) was read as %d bytes of %d", c.description, c.header, n, total)
		case !c.refused && (err != nil || n != c.n || total != c.total):
			t.Errorf("%s (%q) = %d bytes of %d, %v; want %d of %d",
				c.description, c.header, n, total, err, c.n, c.total)
		}
	}
}
