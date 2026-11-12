package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A FETCH REQUEST'S BODY, READ TO ITS END BEFORE ANYTHING ANSWERS IT: the
// level a fetch is answered at and whether this node lacks a want are both in
// it, and a fetch that fails on a damaged cache is retried once over the same
// bytes without the client sending them again.
//
// It is held in memory AS LARGE AS IT IS, up to RequestMemoryBytes — a
// clone's negotiation is a few hundred bytes, and that is what it costs — and
// the memory every request holds at once is charged to a node-wide allowance
// (RequestMemoryAllowance), so what requests waiting for their sync and their
// transfer slot hold is bounded by the node rather than by how many arrive. A
// request past its in-memory part, or one that finds the allowance spent, is
// SPOOLED — the whole of it, its head written beside the rest and its memory
// let go — to an unnamed file on the cache's volume, so a partial clone's
// lazy fetch of a million wants is served where git's own 10 MiB buffer
// refused it, and read back as a stream wherever it is needed — never held
// whole. A request that finds no room in the spool allowance WAITS ITS TURN
// for it, holding its head in a place of the waiting room and nothing more
// (spool.go). And the body is GIVEN BACK when the answer's FIRST BYTE is
// written (sink.onStart), because a retry over a repaired cache reads it until
// then and no further. For a FETCH that first byte is its PACK's: upload-pack
// writes its answer's opening line, then runs pack-objects, and only its pack
// is the first byte — so the body (and its spool) is held through
// pack-objects' enumeration, at most a progress line (about a second) or one
// keepalive (5 s, uploadpack.keepAlive) for a client using side-band, and
// until the raw pack for one that is not. An ls-refs' or a push's first byte is
// its first output, so its body is given back at once. Past the first byte a
// transfer paced by its client holds neither memory nor a spool.

// The refusals reading a request makes, each answered in its own status.
var (
	// errRequestTooLarge is a request past its repository's bound (413).
	errRequestTooLarge = errors.New("host: the request is past its repository's bound")
	// errSpoolPastCeiling is a request that would spool more than this node
	// spools for one request (SpoolRequestBytes) — one it can never take,
	// however idle, so it is refused at once rather than made to wait for
	// room it would never fit (413).
	errSpoolPastCeiling = errors.New("host: the request is past what this node spools for one request")
	// errSpoolFull is a request that waited for the spool allowance and was
	// not served: its wait reached its bound, or it had to wait and found no
	// place to (503). Always a *spoolUnserved.
	errSpoolFull = errors.New("host: the spool allowance is spent")
)

// tooLarge is errRequestTooLarge naming the bytes read and the bound.
func tooLarge(read, bound int64) error {
	return fmt.Errorf("%w: at least %d bytes, against a bound of %d", errRequestTooLarge, read, bound)
}

// firstHead is the first buffer a request that does not declare its length is
// read into: a clone's or an ordinary fetch's negotiation is a few hundred
// bytes to a few kilobytes, so most requests fit it, and one past it doubles
// its buffer toward RequestMemoryBytes — the copies of every doubling together
// under its final size.
const firstHead = 4 << 10

// requestBody is one fetch request's decoded body, read to its end.
type requestBody struct {
	mu sync.Mutex
	// head is the body held in memory, when it fitted there — and, for a
	// request that spools, its first bytes until they are written into the
	// spool; heads is the allowance it is charged to, held how much of it
	// (its capacity), and nothing while a place of the spool's waiting room
	// holds it instead.
	head  []byte
	heads *allowance
	held  int64
	// spool holds the whole body, size bytes of it, once the request
	// spooled; nil when it fitted in memory.
	spool *os.File
	size  int64
	// spools is the allowance the spool draws on, set once the request
	// started spooling: turn is its body there — the room it took while
	// arriving, what it spooled once it ended — and placed a place of the
	// waiting room of credential's while it holds one.
	spools     *spools
	turn       *spoolBody
	credential string
	placed     bool
	// drained: the reader upload-pack is reading (feed) has returned the
	// body's end; answered: the response's first byte was written. Both
	// together, nothing reads the body again (release).
	drained, answered bool
	closed            bool
}

// reader is the whole body, from its first byte: a fresh reader each call, so
// the parse, the check of what this node lacks and upload-pack — once, or
// again after a rebuild — each read it from the start. A body already given
// back is no body: its reader fails rather than read a closed spool.
func (b *requestBody) reader() io.Reader {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.closed:
		return errReader{errReleased}
	case b.spool == nil:
		return bytes.NewReader(b.head)
	}
	return io.NewSectionReader(b.spool, 0, b.size)
}

// errReleased is a read of a body already given back — which nothing does:
// the body is given back only once upload-pack has read it and answered, and
// only a run that answered nothing is retried over it.
var errReleased = errors.New("host: the request's body was already given back")

// errReader is a reader that fails with its error.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// length is the body's decoded size.
func (b *requestBody) length() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(len(b.head)) + b.size
}

// feed is the body as upload-pack reads it: a reader that, at the body's
// end, lets go of the bytes it read from and marks the body DRAINED — and a
// drained body whose answer has started is given back (release). One feed an
// attempt: a run retried after a rebuild reads a fresh one, and only that
// run's end counts.
func (b *requestBody) feed() io.Reader {
	b.mu.Lock()
	b.drained = false
	b.mu.Unlock()
	return &feeding{b: b, r: b.reader()}
}

// feeding is one feed.
type feeding struct {
	b *requestBody
	r io.Reader
}

func (f *feeding) Read(p []byte) (int, error) {
	if f.r == nil {
		return 0, io.EOF
	}
	n, err := f.r.Read(p)
	if err == io.EOF {
		// THE BYTES ARE LET GO HERE, not when the run ends: the run's Cmd
		// holds this reader until the transfer is over, and a reference to
		// the head through it would pin it as long.
		f.r = nil
		f.b.mu.Lock()
		f.b.drained = true
		f.b.releaseLocked()
		f.b.mu.Unlock()
	}
	return n, err
}

// started is the response's first byte written: a drained body is given
// back.
func (b *requestBody) started() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answered = true
	b.releaseLocked()
}

// releaseLocked gives the body back once it is drained and answered; b.mu is
// held.
func (b *requestBody) releaseLocked() {
	if b.drained && b.answered {
		b.closeLocked()
	}
}

// close gives everything back: the spool closed — which the kernel then
// removes, as it has no name — its body, still arriving or not, and its place
// returned to the spool allowance, and the head's memory to its own.
// Idempotent, and safe on a body that never spooled.
func (b *requestBody) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeLocked()
}

func (b *requestBody) closeLocked() {
	if b.closed {
		return
	}
	b.closed = true
	if b.spool != nil {
		_ = b.spool.Close()
		b.spool = nil
	}
	if b.spools != nil {
		if b.placed {
			b.spools.unplace(b.credential)
			b.placed = false
		}
		b.spools.release(b.turn)
	}
	b.head = nil
	if b.held > 0 {
		b.heads.give(b.held)
		b.held = 0
	}
}

// spoolPiece is how much of a request is copied into its spool at a time.
const spoolPiece = 64 << 10

// readRequest reads src to its end: up to s.requestMemory bytes into memory,
// in a buffer grown with the request from its declared length (declared, -1
// when the client did not say or encoded the body) or from firstHead, each
// growth charged to the node's request memory; and a request that goes past
// the in-memory part, or past what the allowance had left, whole into a spool
// the cache makes, once the spool allowance has let it start (spools.start)
// claiming as much as it can grow to, its room taken as its bytes arrive
// (spools.more) — waiting its turn for either if it must. bound answers the most the request may carry: asked once
// with fresher false, and once more with fresher true before a request past
// the first answer is refused, so a bound read from a stale row never refuses
// a request a current one admits. A request past the bound is
// errRequestTooLarge; one past what this node spools for one request
// errSpoolPastCeiling; one longer than its client declared errPastClaim; one
// that waited for room and was not granted it errSpoolFull; one whose context
// ended while it waited that context's error; src's own failure is returned as
// itself.
func (s *Server) readRequest(ctx context.Context, src io.Reader, declared int64, credential string,
	bound func(ctx context.Context, fresher bool) (int64, error)) (*requestBody, error) {

	b := &requestBody{heads: s.heads}
	memory := int64(s.requestMemory)
	want := int64(firstHead)
	if declared >= 0 {
		// One byte past the declared length, so its end is read with it —
		// the length capped below memory FIRST: it is the client's word, and
		// net/http admits one of math.MaxInt64, one past which wraps negative.
		want = min(declared, memory-1) + 1
	}
	for {
		want = min(want, memory)
		if grow := want - int64(cap(b.head)); grow > 0 {
			if !s.heads.take(grow) {
				// THE NODE'S REQUEST MEMORY IS IN USE: what is left goes to
				// the spool, which has an allowance of its own.
				break
			}
			b.held += grow
			next := make([]byte, len(b.head), want)
			copy(next, b.head)
			b.head = next
		}
		if len(b.head) == cap(b.head) {
			// A HEAD WITH NO ROOM AND NONE GIVEN is the spool's from here:
			// asked to fill nothing, a reader answers nothing and no end, and a
			// loop that asked again would spin without reading a byte.
			break
		}
		n, end, err := objstore.Fill(src, b.head[len(b.head):cap(b.head)])
		b.head = b.head[:len(b.head)+n]
		switch {
		case err != nil:
			b.close()
			return nil, err
		case end:
			return b, nil
		}
		// Not at the end, so the buffer is full (objstore.Fill): it doubles,
		// up to memory, where the loop above finds no room and ends.
		want = 2 * int64(cap(b.head))
	}
	// ONE BYTE FIRST, into a byte of its own: a request the in-memory part
	// held exactly opens no spool and waits for no turn, and one that goes on
	// is known to before anything is asked of the spool for it.
	var first [1]byte
	k, end, err := objstore.Fill(src, first[:])
	switch {
	case err != nil:
		b.close()
		return nil, err
	case k == 0 && end:
		return b, nil
	}
	head := int64(len(b.head))
	limit, err := bound(ctx, false)
	if err != nil {
		b.close()
		return nil, err
	}
	// AT LEAST what has arrived, and what the client declared it sends: a
	// request that says it is past its bound, or past what this node spools
	// for one request, is refused before it waits for room it would never use.
	atLeast := max(head+int64(k), declared)
	fresher := false
	if atLeast > limit {
		fresher = true
		if limit, err = bound(ctx, true); err != nil {
			b.close()
			return nil, err
		}
		if atLeast > limit {
			b.close()
			return nil, tooLarge(atLeast, limit)
		}
	}
	// ONE ALLOWANCE for the whole request, read once: the body it starts is
	// that allowance's, and every later room and its end go to it.
	sp := s.spools
	ceiling := sp.request
	if atLeast > ceiling {
		b.close()
		return nil, sp.pastCeiling()
	}
	// IT STARTS, claiming the most it can grow to — what this node spools for
	// one request, or the length its client declared, at which net/http ends
	// an unencoded body — with room for the head and the byte after it: at
	// once, or in its turn, the head waiting in a place of the spool's
	// waiting room rather than in the request memory every request on the
	// node shares.
	claim := ceiling
	if declared >= 0 {
		claim = min(claim, declared)
	}
	need := head + int64(k)
	turn, w, err := sp.start(ctx, credential, claim, need, sp.ask(0, need, claim),
		s.o.Timing.SpoolWait, func() {
			s.heads.give(b.held)
			b.held = 0
		})
	s.spoolWaited(w)
	if err != nil {
		b.close()
		return nil, err
	}
	b.spools, b.turn, b.credential, b.placed = sp, turn.body, credential, turn.placed
	room := turn.got
	if b.spool, err = s.o.Cache.Spool(); err != nil {
		b.close()
		return nil, err
	}
	// THE HEAD GOES INTO THE SPOOL, and its memory is let go — the request
	// memory it was charged to, or its place — so a spooled request that
	// waits for its sync and its transfer slot holds disk and no memory, and
	// a queue of them never sends the requests behind them to the spool.
	for _, p := range [][]byte{b.head, first[:k]} {
		if _, err = b.spool.Write(p); err != nil {
			b.close()
			return nil, fmt.Errorf("host: spool a request: %w", err)
		}
		b.size += int64(len(p))
	}
	b.head = nil
	if b.held > 0 {
		s.heads.give(b.held)
		b.held = 0
	}
	if b.placed {
		sp.unplace(credential)
		b.placed = false
	}
	piece := make([]byte, spoolPiece)
	for !end {
		if k, end, err = objstore.Fill(src, piece); err != nil {
			b.close()
			return nil, err
		}
		if total := b.size + int64(k); total > limit {
			if fresher {
				b.close()
				return nil, tooLarge(total, limit)
			}
			fresher = true
			if limit, err = bound(ctx, true); err != nil {
				b.close()
				return nil, err
			}
			if total > limit {
				b.close()
				return nil, tooLarge(total, limit)
			}
		}
		if b.size+int64(k) > ceiling {
			b.close()
			return nil, sp.pastCeiling()
		}
		// ROOM AS THE BYTES ARRIVE, a grain at a time: what the spool holds
		// is what its client sent, never what its request might yet send.
		if short := b.size + int64(k) - room; short > 0 {
			var got int64
			got, w, err = sp.more(ctx, b.turn, short, sp.ask(room, short, claim),
				s.o.Timing.SpoolWait)
			s.spoolWaited(w)
			if err != nil {
				b.close()
				return nil, err
			}
			room += got
		}
		if _, err = b.spool.Write(piece[:k]); err != nil {
			b.close()
			return nil, fmt.Errorf("host: spool a request: %w", err)
		}
		b.size += int64(k)
	}
	// ITS BODY HAS ENDED: the room it took and did not write goes back, and
	// so does its claim among the bodies arriving — at once, not with the
	// request, which may wait a cold sync of a minute or two before
	// upload-pack reads it, holding only what it spooled.
	sp.ended(b.turn, b.size)
	return b, nil
}

// spoolWaited records how a request's turn at the spool allowance went
// (spoolWait): a wait counted and observed by its outcome, and a request
// refused a place to wait counted; a request granted at once is neither.
func (s *Server) spoolWaited(w spoolWait) {
	if w.outcome == "" || s.o.Metrics == nil {
		return
	}
	attrs := metrics.Attrs{"outcome": w.outcome}
	s.o.Metrics.Add(metrics.CodeHostSpoolWaits, 1, attrs)
	if w.outcome != spoolRefused {
		s.o.Metrics.Observe(metrics.CodeHostSpoolWait, w.took, attrs)
	}
}

// allowance is a node-wide count of bytes against a limit: taken before they
// are held, refused rather than waited for.
type allowance struct {
	limit int64

	mu   sync.Mutex
	used int64
}

func newAllowance(limit int64) *allowance { return &allowance{limit: limit} }

// take takes n bytes, or reports there are not n left.
func (a *allowance) take(n int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.used+n > a.limit {
		return false
	}
	a.used += n
	return true
}

// give gives n bytes back.
func (a *allowance) give(n int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.used -= n
}

// inUse is the bytes taken right now.
func (a *allowance) inUse() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.used
}
