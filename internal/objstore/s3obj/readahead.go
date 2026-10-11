package s3obj

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/crewlet/crewlet/internal/objstore"
)

// piecesPerRange is how many pieces a range read ahead is handed to the reader
// in: a range is filled a piece at a time, and every piece is the reader's the
// moment it lands rather than once the whole range has.
//
// SIXTY-FOUR, a piece of 128 KiB of a production part, so a read a provider
// is moving is a read [objstore.Store]'s stall watchdog sees moving. A range is
// a part, eight mebibytes, and a read keeps up to [partsInFlight] of them
// crossing one link together; handed over only whole, the range the reader
// waits on would cross at a quarter of the link with nothing handed out — over
// four minutes at [objstore.MiBPace], where [objstore.ReadStall] ends a read
// that hears nothing for one. A piece at a quarter of the floor's 35 KB/s
// lands every fifteen seconds; at the 80–90 MB/s one S3 connection carries,
// every millisecond and a half — a mutex and a wake each, which the reader's
// copy of the piece dwarfs. A share of the PART rather than a size of its
// own, so a case running the backend at a small part reads its ranges in
// pieces too.
const piecesPerRange = 64

// askAhead is how many first bytes ahead of the reader a range's request is
// sent: the time the bucket takes to answer a request, at the rate the reader
// is handed bytes, twice over.
//
// TWO, so an answer that takes twice the read's usual first byte still
// arrives before the reader needs it, and a reader handed bytes at a steady
// rate never waits on a first byte at all. What asking early costs is an
// answer that waits on its connection, unread, for about one first byte — tens
// of milliseconds a range, with the bucket's bytes held by the connection
// rather than by a buffer of the node's. Asked only when its buffer was free,
// every range cost its first byte in turn: at 50 ms, a 128 MiB read four
// ranges wide moved at 196–199 MB/s where its four connections carry 320, and
// one with no buffer at all at 54 against one stream's 78; asked for ahead,
// at 219–246 and 102 (BenchmarkBackend's `bw=80MB` arms).
const askAhead = 2

// firstByteWeight is the share of the read's estimate of its first byte each
// new answer moves it by: a quarter, so a slow answer — a fresh connection's
// handshake — moves the estimate a quarter of the way rather than becoming
// it, and a bucket that has grown slower is followed within a few ranges.
const firstByteWeight = 4

// getWide reads n bytes of name from off — to the end when n is negative — as
// consecutive [Backend.partBytes] ranges handed out strictly in order
// ([wideRead]).
//
// THE FIRST RANGE IS STREAMED STRAIGHT THROUGH, never buffered, and its answer
// says how large the object is: an object the first range holds whole — every
// object of a part or less, most of what a company keeps — costs the one
// request and the memory a read cost before there were ranges.
//
// EVERY RANGE AFTER IT IS HELD TO THE FIRST ONE'S ETAG (If-Match). Keys are
// never reused ([objstore.Key]), so a later range answered 412 is a bucket
// somebody wrote into while it was read — a failed read, never two objects
// stitched together. An object the bucket gives no tag is refused past its
// first range for the same reason: there would be nothing to hold the rest to.
func (b *Backend) getWide(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	// THE READ'S CONTEXT, which every range's descends from: Close, a
	// failure the reader is handed and the caller giving up all end every
	// request of the read at once, the one being streamed too.
	ranges, stop := context.WithCancel(ctx)
	asked := time.Now()
	out, err := b.client.GetObject(ranges, b.rangeInput(name, off, int64(b.partBytes), nil))
	switch {
	case missing(err):
		stop()
		return nil, objstore.ErrNotFound
	case status(err) == http.StatusRequestedRangeNotSatisfiable:
		// A READ THAT BEGINS AT OR PAST THE END — an empty object's
		// whole read included, since S3 refuses every range of one:
		// nothing to read, which the contract says is an empty stream.
		stop()
		return io.NopCloser(bytes.NewReader(nil)), nil
	case err != nil:
		stop()
		return nil, fmt.Errorf("s3obj: get %s: %w", name, err)
	}
	answered := time.Since(asked)
	got, size, err := spanOf(aws.ToString(out.ContentRange), off, int64(b.partBytes), -1)
	if err != nil {
		_ = out.Body.Close()
		stop()
		return nil, fmt.Errorf("s3obj: get %s: %w", name, err)
	}
	end := size
	if n > 0 {
		end = min(off+n, size)
	}
	if off+got >= end {
		return &oneRange{Reader: objstore.ContextReader(ctx, out.Body), body: out.Body, stop: stop}, nil
	}
	if aws.ToString(out.ETag) == "" {
		_ = out.Body.Close()
		stop()
		return nil, fmt.Errorf("s3obj: get %s: the bucket answered its first range with no ETag, "+
			"and a read of more than one range holds every later range to the first one's tag "+
			"so it can never stitch two objects together; S3 tags every object, so reach the "+
			"bucket through a gateway that does", name)
	}
	first := &rangeRead{off: off, n: int(got), ctx: ranges, streamed: true, body: out.Body,
		moved: make(chan struct{}, 1)}
	w := &wideRead{b: b, ctx: ranges, stop: stop, name: name, etag: out.ETag, size: size,
		ranges: []*rangeRead{first}, last: ranges, next: off + got, end: end, pos: off,
		frontAt: time.Now()}
	w.firstByte.Store(int64(max(answered, time.Nanosecond)))
	w.plan()
	return w, nil
}

// rangeInput is the request for size bytes of name from off, held to etag
// where it is not nil.
func (b *Backend) rangeInput(name string, off, size int64, etag *string) *s3.GetObjectInput {
	return &s3.GetObjectInput{
		Bucket:  aws.String(b.bucket),
		Key:     aws.String(b.key(name)),
		Range:   aws.String(fmt.Sprintf("bytes=%d-%d", off, off+size-1)),
		IfMatch: etag,
	}
}

// spanOf reads a ranged get's Content-Range — `bytes first-last/size` — and
// holds it to what was asked: a range beginning at from, of at most most
// bytes, of an object of size bytes where size is known (not negative). It
// answers how many bytes the range holds and how large the object is.
//
// A RANGE THAT IS NOT THE ONE ASKED FOR IS REFUSED rather than handed out at
// the offset it was asked for — a gateway that ignored the range (and so
// answered no Content-Range), one answering from elsewhere or more than was
// asked, or an object of another size than the first range found, which is
// another object.
func spanOf(contentRange string, from, most, size int64) (n, total int64, err error) {
	span, ok := strings.CutPrefix(contentRange, "bytes ")
	first, rest, okDash := strings.Cut(span, "-")
	last, sizeText, okSlash := strings.Cut(rest, "/")
	if !ok || !okDash || !okSlash {
		return 0, 0, fmt.Errorf("the bucket answered the range at %d with the Content-Range %q, "+
			"not the `bytes first-last/size` a range is answered with", from, contentRange)
	}
	a, errA := strconv.ParseInt(first, 10, 64)
	z, errZ := strconv.ParseInt(last, 10, 64)
	total, errT := strconv.ParseInt(sizeText, 10, 64)
	switch {
	case errA != nil || errZ != nil || errT != nil || a < 0 || z < a || z >= total:
		return 0, 0, fmt.Errorf("the bucket answered the range at %d with the Content-Range %q, "+
			"which is no range of an object", from, contentRange)
	case a != from:
		return 0, 0, fmt.Errorf("the bucket answered the range at %d with bytes %d-%d", from, a, z)
	case z-a+1 > most:
		return 0, 0, fmt.Errorf("the bucket answered the range at %d with %d bytes, and at most %d "+
			"were asked for", from, z-a+1, most)
	case size >= 0 && total != size:
		return 0, 0, fmt.Errorf("the bucket answered the range at %d from an object of %d bytes, "+
			"and the read began on one of %d", from, total, size)
	}
	return z - a + 1, total, nil
}

// oneRange is a read the first range held whole: its body, and the end of the
// context its request was made under, which Close ends with it.
type oneRange struct {
	io.Reader
	body io.Closer
	stop context.CancelFunc
}

func (o *oneRange) Close() error {
	err := o.body.Close()
	o.stop()
	return err
}

// wideRead is a read of more than one range. Its reader is handed the range at
// the FRONT and only that one, a piece at a time — from the range's answer
// when it is STREAMED, from its buffer when it was READ AHEAD — while the
// ranges behind it are asked for and read ahead by goroutines of their own.
//
// THE READ IS AS WIDE AS ITS READER. A range is read ahead into a buffer from
// the node's spare allowance ([spareParts]), taken only while one is free and
// never waited for, and a read holds as many as its reader keeps busy, up to
// [partsInFlight]: one to begin with — the second range read ahead while the
// first streams — and then what the reader's ranges say, once it has been
// handed half the first and each time it has been handed one whole
// ([wideRead.fit]). A reader that waited on the bucket for a share f of its
// time on a range would have kept 1 / (1 − f) times the connections busy, and
// the read takes that many — one more only when the reader would keep at
// least half of it busy — and a reader that never waited on a range, and then
// reaches one read ahead which landed whole before it got there, had more in
// hand than it could use: the read gives a buffer back. So a reader slower than
// one connection — a download paced to a person's link, for hours — holds one
// buffer, and one faster than its connections — the git cache's install, a
// verified read, a backup's copy — holds as many as its connections need. A
// read that finds no buffer free streams every range, and takes one as soon
// as one is free.
//
// A RANGE IS ASKED FOR BEFORE IT IS NEEDED, buffer or not: a request goes out
// [askAhead] first bytes before the reader will need its answer — the moment
// a buffer frees for it, or the reader reaches it — and its answer waits on its
// connection, costing the node no buffer, until then. Asked only once a
// buffer was free, every range cost a whole first byte with its connection
// idle, which held a read four ranges wide well under its connections' rate,
// and one streaming its ranges under the rate of a single stream. At most one
// range waits so for each buffer the read holds — one when it holds none — and
// a read holds at most twice [partsInFlight] ranges, the one it streams among
// them ([wideRead.due]), so it has at most twice [partsInFlight] requests at
// the bucket.
//
// STRICTLY IN ORDER: the reader hands out the front range and only that one,
// as it lands, however far the ranges behind it have got. A range that fails
// ends EVERY RANGE AFTER IT at once — its context is every later range's
// ancestor — while every range before it lands and is handed out; the read
// then asks for nothing more, and its failure is handed out at the Read that
// would have handed out the range's first byte not yet handed out.
//
// ONE READER: Read and Close are the caller's, in turn. Close may still be
// called while a Read waits, and ends it.
type wideRead struct {
	b    *Backend
	ctx  context.Context // the read's, every range's ancestor
	stop context.CancelFunc
	name string
	etag *string
	size int64 // the object's size, as the first range's answer gave it

	// broken is set by the first range that fails of its own accord: the
	// read asks for nothing more, and puts no buffer to a new range.
	broken atomic.Bool
	// firstByte is how long, in nanoseconds, a request of the read takes to
	// be answered — the first's, then each answer folded in
	// ([firstByteWeight]).
	firstByte atomic.Int64

	// mu is held by a Read for all of it and by Close once it has stopped
	// the ranges — which is what ends a Read waiting on one.
	mu sync.Mutex

	ranges []*rangeRead    // asked for and not yet handed out, the front first
	last   context.Context // the last range asked for's: the next one's parent
	next   int64           // the first byte no range has been asked for yet
	end    int64           // where the read ends
	pos    int64           // the next byte the reader is handed

	held int // buffers held, each one of the node's spares
	debt int // buffers to give back to the node rather than put to a new range

	frontAt     time.Time     // when the front range became the front
	frontWaited time.Duration // how long the reader has waited on the bucket since
	rate        float64       // bytes a second the reader took the last range at

	// fitted is whether the read's width has been fitted to its reader yet.
	// It is at the end of every range, and once in the middle of the FIRST,
	// when its reader has been handed half of it, because a range read
	// ahead takes a range's transfer to land: fitted only once the first
	// range was handed out whole, the ranges a fast reader's read then asked
	// for were a whole transfer behind it, and a verified 128 MiB read lost
	// about a tenth of its time to the wait. HALF, four mebibytes of a
	// production range — four of the API's one-mebibyte sends to a person's
	// client — because a reader is judged on what it did over them: one
	// fast for its first send and slow after must not be taken for a fast
	// one, and a judgement any later leaves the read's next ranges behind.
	fitted bool

	running sync.WaitGroup
	err     error
}

// rangeRead is one range: where it is, what its goroutine has made of it, and
// what the reader has.
//
// EVERY RANGE BUT THE FIRST IS ONE OF THREE: READ AHEAD — given a buffer,
// which its goroutine fills a piece at a time; WAITING — asked for, its answer
// held on its connection until a buffer is given it or the reader reaches it;
// or STREAMED — reached by the reader with no buffer, its answer handed to the
// reader to read from. The ranges a read holds are always those of them in
// that order, the one streamed at the front, because a buffer goes to the
// first range waiting for one.
type rangeRead struct {
	off int64
	n   int
	// ctx is the range's request's, a child of the range before it's: the
	// range's own failure ends every range after it, and only those.
	ctx    context.Context
	cancel context.CancelFunc
	// assign carries the range its buffer, or nil: the reader streams it.
	// One slot, one value ever, so the reader never waits on it.
	assign chan *[]byte

	// The reader's own.
	buf      *[]byte       // the buffer given it; nil while it waits, or once streamed
	streamed bool          // the reader streams it
	body     io.ReadCloser // a streamed range's answer, once the reader has it
	out      int           // how many of its bytes the reader has been handed

	// Its goroutine's, under mu.
	mu     sync.Mutex
	answer io.ReadCloser // a streamed range's answer, handed to the reader
	filled int
	done   bool
	err    error
	// moved holds a wake once answer, filled, done or err has changed: one
	// slot, so the goroutine never waits on the reader and a wake is never
	// lost.
	moved chan struct{}
}

// landed is what the range's goroutine has made of it so far.
func (r *rangeRead) landed() (filled int, done bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.filled, r.done, r.err
}

// whole is whether the range was read ahead, and has landed whole.
func (r *rangeRead) whole() bool {
	if r.buf == nil {
		return false
	}
	filled, done, err := r.landed()
	return done && err == nil && filled == r.n
}

func (r *rangeRead) wake() {
	select {
	case r.moved <- struct{}{}:
	default:
	}
}

func (w *wideRead) Read(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.err != nil:
		return 0, w.err
	case w.ctx.Err() != nil:
		// THE CALLER'S CONTEXT ENDED — the store's stall watchdog, or a
		// client gone — and the read ends at this Read, with its error,
		// whatever it had read ahead.
		return 0, w.fail(w.ctx.Err())
	case len(p) == 0:
		return 0, nil
	case len(w.ranges) == 0:
		return 0, io.EOF
	}
	r := w.ranges[0]
	var (
		n   int
		err error
	)
	if r.buf != nil {
		n, err = w.readAhead(r, p)
	} else {
		n, err = w.readStreamed(r, p)
	}
	r.out += n
	w.pos += int64(n)
	if err != nil {
		return n, err
	}
	switch {
	case r.out == r.n:
		w.handedOut(r)
	case !w.fitted && r.out >= r.n/2:
		// THE FIRST RANGE, HALF HANDED OUT: see [wideRead.fitted].
		w.fitted = true
		w.fit(time.Since(w.frontAt), w.frontWaited)
	}
	w.plan()
	return n, nil
}

// readAhead hands out what has landed of r, the front range read ahead,
// waiting for its next piece if none has.
func (w *wideRead) readAhead(r *rangeRead, p []byte) (int, error) {
	for {
		filled, done, err := r.landed()
		if filled > r.out {
			return copy(p, (*r.buf)[r.out:filled]), nil
		}
		if done {
			// A RANGE THAT LANDED WHOLE was handed out above; one that
			// is done short of its length failed, and says how.
			return 0, w.fail(err)
		}
		since := time.Now()
		select {
		case <-r.moved:
		case <-w.ctx.Done():
			return 0, w.fail(w.ctx.Err())
		}
		w.frontWaited += time.Since(since)
	}
}

// readStreamed hands out what r, the front range with no buffer, has next from
// its answer — taking the answer over first, and so the range, from its
// goroutine.
//
// THE WHOLE OF THE CALL IS COUNTED AS WAITED: a body hands out what has
// arrived without telling whether it waited for it, and copying what had
// arrived is as nothing beside waiting for what had not.
func (w *wideRead) readStreamed(r *rangeRead, p []byte) (int, error) {
	since := time.Now()
	defer func() { w.frontWaited += time.Since(since) }()
	if !r.streamed {
		r.streamed = true
		r.assign <- nil
	}
	for r.body == nil {
		r.mu.Lock()
		answer, done, err := r.answer, r.done, r.err
		r.mu.Unlock()
		switch {
		case answer != nil:
			r.body = answer
		case done:
			return 0, w.fail(err)
		default:
			select {
			case <-r.moved:
			case <-w.ctx.Done():
				return 0, w.fail(w.ctx.Err())
			}
		}
	}
	n, err := r.body.Read(p[:min(len(p), r.n-r.out)])
	if r.out+n == r.n {
		// WHOLE, whatever the body says next: the range was its length.
		return n, nil
	}
	switch at, left := r.off+int64(r.out+n), r.n-r.out-n; {
	case w.ctx.Err() != nil:
		return n, w.fail(w.ctx.Err())
	case err == io.EOF: //nolint:errorlint // the end is io.EOF itself, as objstore.Fill reads it
		return n, w.fail(fmt.Errorf("s3obj: get %s: the range ended at %d, %d bytes short: %w",
			w.name, at, left, io.ErrUnexpectedEOF))
	case err != nil:
		return n, w.fail(fmt.Errorf("s3obj: get %s at %d: %w", w.name, at, err))
	}
	return n, nil
}

// handedOut moves the reader on from r, the front range, now handed out
// whole: its answer closed, its buffer put to the next range, and the read's
// width fitted to how the reader took it.
func (w *wideRead) handedOut(r *rangeRead) {
	now := time.Now()
	spent, waited := now.Sub(w.frontAt), w.frontWaited
	w.ranges = w.ranges[1:]
	w.frontAt, w.frontWaited, w.fitted = now, 0, true
	if spent > 0 {
		w.rate = float64(r.n) / spent.Seconds()
	}
	if r.body != nil {
		_ = r.body.Close()
		r.body = nil
	}
	w.fit(spent, waited)
	if buf := r.buf; buf != nil {
		r.buf = nil
		w.give(buf)
	}
}

// fit sets how many buffers the read holds from how its reader took the range
// it has just been handed: spent on it in all, and waited on the bucket of
// that.
//
// AS MANY AS THE READER KEEPS BUSY. A reader that waited a share f of its time
// was handed bytes 1 − f as fast as it takes them, so the read's n buffers —
// one connection when it holds none — are n / (1 − f) of what it needed,
// rounded: one more is taken only when the reader would keep at least half of
// it busy. One that never waited on the range it finished may have more than
// it needs, and says so by reaching a range read ahead that had landed whole
// before it got there: the read then gives one buffer back — the next it frees
// — and withdraws the ranges asked for at the old pace ([wideRead.withdraw]).
// NEVER WAITED, because a fast reader can reach a range that happened to land
// before it got there too — the second, read ahead beside the first — and one
// that waited at all on the range before was keeping the read busy.
//
// FROM WHAT THE READER DID, NEVER FROM WHAT THE BUCKET COULD: a reader slower
// than one connection — a download paced to a person's link, for hours —
// never waits, and holds the one buffer it began with. Widened whenever a
// buffer was free and the connections in flight were fewer than
// [partsInFlight], three such downloads held a quarter of the node's
// allowance each for as long as they lasted, and every upload and wide read
// on the node went a part at a time meanwhile.
func (w *wideRead) fit(spent, waited time.Duration) {
	if w.broken.Load() {
		return
	}
	have := max(w.held, 1)
	want := have
	switch own := spent - waited; {
	case waited <= 0:
	case own <= 0:
		want = partsInFlight
	default:
		want = min(partsInFlight, int(math.Round(float64(have)*float64(spent)/float64(own))))
	}
	if want > w.held {
		w.debt = 0
		// ONLY WHILE A RANGE IS LEFT TO PUT IT TO: a buffer taken with
		// none would go straight back.
		for w.held < want && (w.next < w.end || w.waiting() != nil) && w.b.takeSpare() {
			w.held++
			w.give(w.b.parts.Get().(*[]byte))
		}
		return
	}
	if w.held > 1 && waited == 0 && len(w.ranges) > 0 && w.ranges[0].whole() {
		w.debt = 1
		if spent > askAhead*time.Duration(w.firstByte.Load()) {
			w.withdraw()
		}
	}
}

// withdraw takes back every range asked for that waits for a buffer — its
// request cancelled, and the range asked for again by [wideRead.plan] at the
// reader's pace as it is now. A read narrowing under a reader that took
// longer over its last range than the lead a range is asked with
// ([askAhead] first bytes) withdraws them.
//
// A NARROWER READ FREES ITS BUFFERS LATER, and each range waiting was asked
// for to be answered by the moment the buffer it was to be given would free
// at the old width: given the buffers the read now frees instead, each waits
// one more of the reader's ranges — for a reader that has slowed to a
// person's link, minutes with an answer unread on its connection — and the
// read keeps its old width until the last of them has had one.
//
// ONLY THEN, because a range's request withdrawn is a first byte paid again:
// a reader a range of which takes no longer than the lead — one just slower
// than the read's connections, a verified read on a CPU hashing at about
// their rate — holds an answer no longer than the lead it was asked with, and
// withdrawn at every narrowing such a read moved a tenth slower.
func (w *wideRead) withdraw() {
	for i, r := range w.ranges {
		if r.buf != nil || r.streamed {
			continue
		}
		// EVERY RANGE FROM HERE ON WAITS, since a buffer goes to the
		// first range waiting for one, and every one descends from this
		// one's context.
		r.cancel()
		w.next, w.ranges, w.last = r.off, w.ranges[:i], w.ctx
		if i > 0 {
			w.last = w.ranges[i-1].ctx
		}
		return
	}
}

// give puts a buffer the read holds, and nothing is filling, to use: the first
// range waiting for one, else a range not yet asked for — or, where the read
// owes the node a buffer, has failed or has asked for its every range, back to
// the node.
func (w *wideRead) give(buf *[]byte) {
	if !w.broken.Load() {
		if r := w.waiting(); r != nil {
			r.buf = buf
			r.assign <- buf
			return
		}
		if w.debt == 0 && w.next < w.end {
			w.ask(buf)
			return
		}
	}
	w.debt = 0
	w.release(buf)
}

// waiting is the first range asked for that has neither a buffer nor the
// reader — nil when there is none.
func (w *wideRead) waiting() *rangeRead {
	for _, r := range w.ranges {
		if r.buf == nil && !r.streamed {
			return r
		}
	}
	return nil
}

// plan puts the ranges the read may have on their way: a buffer taken for the
// next range when the read holds none and one is free, and every range due
// within [wideRead.lead] of the reader asked for.
func (w *wideRead) plan() {
	if w.broken.Load() {
		return
	}
	if w.held == 0 && (w.next < w.end || w.waiting() != nil) && w.b.takeSpare() {
		w.held++
		w.give(w.b.parts.Get().(*[]byte))
	}
	if w.debt > 0 {
		return
	}
	lead := w.lead()
	for w.next < w.end {
		due, ok := w.due()
		if !ok || due-w.pos > lead {
			return
		}
		w.ask(nil)
	}
}

// due is the byte the reader will have reached when the next range asked for
// is needed — when the buffer it will be given frees, or, with no buffer to
// free, when the reader reaches it — and false when no more may wait: one for
// each buffer the read holds, or one when it holds none, and none that would
// make the read hold more than twice [partsInFlight] ranges.
//
// TWICE [partsInFlight] RANGES IN ALL, the one streamed among them, so a read
// has at most twice [partsInFlight] requests at the bucket. A range waiting
// for each buffer keeps a read within that but in ONE case, its FIRST range:
// a read streams it beside the buffers it reads ahead into, and four wide by
// half of it, a range waiting for each of the four beside the range still
// streaming was one past the bound. There the cap holds back the range that
// would wait for the LAST buffer — the buffer of a range asked for at that
// half — and costs the read nothing, whatever the bucket's first byte: the
// range held back is asked for once the first range is handed out, at most
// half a range later, while the range whose buffer it waits for cannot be
// handed out before its own first byte and its whole transfer, which takes
// longer than that half. The first range streams at one connection's rate
// with the reader waiting on it, or the read would not be four wide.
//
// THE STREAMED RANGE IS NOT ONE OF THE WAITING: it frees no buffer, so a range
// waiting for one beside it waits for a buffer of its own. Counted against
// them instead, a read that took a spare while it streamed a later range — one
// that held none when its reader reached it — would withhold the next range's
// request until that range was handed out, and its reader could then wait up
// to a first byte on the range after.
func (w *wideRead) due() (int64, bool) {
	buffered, waiting := 0, 0
	for _, r := range w.ranges {
		switch {
		case r.buf != nil:
			buffered++
		case !r.streamed:
			waiting++
		}
	}
	switch {
	case waiting >= max(buffered, 1), len(w.ranges) >= 2*partsInFlight:
		return 0, false
	case buffered == 0:
		// NO BUFFER TO FREE: the reader reaches it once it has been
		// handed every range before it.
		return w.next, true
	}
	// THE BUFFERS FREE IN ORDER, and go to the ranges waiting in order: the
	// range asked for now is given the buffer of the waiting-th range read
	// ahead, when the reader has been handed it whole.
	k := 0
	for _, r := range w.ranges {
		if r.buf == nil {
			continue
		}
		if k == waiting {
			return r.off + int64(r.n), true
		}
		k++
	}
	return 0, false
}

// lead is how far ahead of the reader a range is due when its request goes
// out: [askAhead] first bytes at the rate the reader is handed bytes — the
// rate it took the last range at, or the one it is taking the front at once
// it has been handed an eighth of it, whichever is slower — and nothing until
// either is known.
//
// THE SLOWER, because a request sent late costs the reader one first byte and
// one sent early holds an answer unread on its connection for as long as it
// was early, which for a reader that has just slowed down is however long it
// has slowed down for. AN EIGHTH, a mebibyte of a production range, before
// the front's own rate counts: a rate taken over its first pieces is the
// rate they happened to land at — a range read ahead hands its landed pieces
// out at memory speed — where a mebibyte is a read's own pace, and still
// leaves most of the range to ask the next one in.
func (w *wideRead) lead() int64 {
	rate := w.rate
	if len(w.ranges) > 0 {
		if front := w.ranges[0]; front.out >= front.n/8 && front.out > 0 {
			if elapsed := time.Since(w.frontAt); elapsed > 0 {
				if now := float64(front.out) / elapsed.Seconds(); rate == 0 || now < rate {
					rate = now
				}
			}
		}
	}
	firstByte := time.Duration(w.firstByte.Load())
	return int64(askAhead * firstByte.Seconds() * rate)
}

// ask asks for the range from next — read ahead into buf, or waiting where buf
// is nil — its context a child of the last range's, and starts its goroutine.
func (w *wideRead) ask(buf *[]byte) {
	ctx, cancel := context.WithCancel(w.last)
	r := &rangeRead{off: w.next, n: int(min(int64(w.b.partBytes), w.end-w.next)), ctx: ctx, cancel: cancel,
		assign: make(chan *[]byte, 1), buf: buf, moved: make(chan struct{}, 1)}
	if buf != nil {
		r.assign <- buf
	}
	w.last = ctx
	w.next += int64(r.n)
	w.ranges = append(w.ranges, r)
	w.running.Add(1)
	go w.fetch(r)
}

// release gives a buffer, and the spare it was taken as, back to the node.
//
// SAFE TO POOL, unlike a failed upload's part: a range's bytes are read into
// its buffer by its own goroutine, which is gone by the time its buffer is
// released — handed out whole, or waited for by Close — and a response body
// hands nothing it reads to the transport.
func (w *wideRead) release(buf *[]byte) {
	w.b.parts.Put(buf)
	w.b.returnSpares(1)
	w.held--
}

// fetch asks for r and makes of it what the reader has it made: read ahead
// into the buffer it is given, or handed over to be streamed — and says how
// it ended.
//
// A FAILURE OF ITS OWN ends every range after it — they descend from its
// context — and the read asks for nothing more. One its context caused is the
// read's end or an earlier range's failure, which the reader hears of first.
func (w *wideRead) fetch(r *rangeRead) {
	defer w.running.Done()
	err := w.fetchRange(r)
	if err != nil && r.ctx.Err() == nil {
		w.broken.Store(true)
		r.cancel()
	}
	r.mu.Lock()
	r.done, r.err = true, err
	r.mu.Unlock()
	r.wake()
}

func (w *wideRead) fetchRange(r *rangeRead) error {
	asked := time.Now()
	out, err := w.b.client.GetObject(r.ctx, w.b.rangeInput(w.name, r.off, int64(r.n), w.etag))
	if err != nil {
		return w.rangeFailed(r.off, err)
	}
	w.answeredIn(time.Since(asked))
	if err := w.answered(out, r); err != nil {
		_ = out.Body.Close()
		return err
	}
	var buf *[]byte
	select {
	case buf = <-r.assign:
	case <-r.ctx.Done():
		_ = out.Body.Close()
		return r.ctx.Err()
	}
	if buf == nil {
		// THE READER STREAMS IT: the answer is the reader's now.
		r.mu.Lock()
		r.answer = out.Body
		r.mu.Unlock()
		return nil
	}
	defer func() { _ = out.Body.Close() }()
	return w.fill(r, out.Body, buf)
}

// fill reads r's answer into buf a piece at a time, each the reader's as it
// lands.
func (w *wideRead) fill(r *rangeRead, body io.Reader, buf *[]byte) error {
	into, piece := (*buf)[:r.n], max(w.b.partBytes/piecesPerRange, 1)
	for filled := 0; filled < r.n; {
		m, end, err := objstore.Fill(body, into[filled:min(filled+piece, r.n)])
		filled += m
		if m > 0 {
			r.mu.Lock()
			r.filled = filled
			r.mu.Unlock()
			r.wake()
		}
		switch {
		case err != nil:
			return fmt.Errorf("s3obj: get %s at %d: %w", w.name, r.off+int64(filled), err)
		case end && filled < r.n:
			return fmt.Errorf("s3obj: get %s: the range at %d ended at %d, %d bytes short: %w",
				w.name, r.off, r.off+int64(filled), r.n-filled, io.ErrUnexpectedEOF)
		}
	}
	return nil
}

// answeredIn folds how long a request took to be answered into the read's
// estimate of its first byte.
func (w *wideRead) answeredIn(d time.Duration) {
	for {
		old := w.firstByte.Load()
		folded := old + (int64(d)-old)/firstByteWeight
		if w.firstByte.CompareAndSwap(old, folded) {
			return
		}
	}
}

// answered holds a later range's answer to the range asked for, of the object
// the first range was of.
func (w *wideRead) answered(out *s3.GetObjectOutput, r *rangeRead) error {
	// EXACTLY THE RANGE ASKED FOR: every range after the first lies inside
	// the object the first one measured, so a short one is as wrong as a
	// long one.
	n, _, err := spanOf(aws.ToString(out.ContentRange), r.off, int64(r.n), w.size)
	if err == nil && n != int64(r.n) {
		err = fmt.Errorf("the bucket answered the range at %d with %d bytes, and %d were asked for",
			r.off, n, r.n)
	}
	if err != nil {
		return fmt.Errorf("s3obj: get %s: %w", w.name, err)
	}
	return nil
}

// rangeFailed is why a range after the first was not answered, said as what
// it means for the read.
func (w *wideRead) rangeFailed(off int64, err error) error {
	switch {
	case status(err) == http.StatusPreconditionFailed:
		return fmt.Errorf("s3obj: get %s: the object changed while it was read — its range at %d "+
			"no longer carries the ETag %s the read began with: %w", w.name, off, aws.ToString(w.etag), err)
	case missing(err):
		return fmt.Errorf("s3obj: get %s: the object was removed while it was read, before its "+
			"range at %d: %w", w.name, off, err)
	}
	return fmt.Errorf("s3obj: get %s: the range at %d: %w", w.name, off, err)
}

// fail ends the read with err, every range in flight with it, and answers err.
func (w *wideRead) fail(err error) error {
	w.err = err
	w.stop()
	return err
}

// errClosed is a Read after Close.
var errClosed = errors.New("s3obj: read of a closed object")

// Close ends the read: every range in flight is cancelled, and once each
// range's goroutine has returned, every answer the read holds is closed and
// every buffer goes back to the node.
func (w *wideRead) Close() error {
	// FIRST, outside the lock: a Read waiting on a range holds it, and
	// returns once the ranges are stopped.
	w.stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.running.Wait()
	for _, r := range w.ranges {
		// AFTER THE WAIT, an answer its goroutine handed over and the
		// reader never took up is the read's to close.
		if r.body == nil {
			r.body = r.answer
		}
		if r.body != nil {
			_ = r.body.Close()
		}
		if r.buf != nil {
			w.release(r.buf)
		}
	}
	w.ranges = nil
	if w.err == nil {
		w.err = errClosed
	}
	return nil
}
