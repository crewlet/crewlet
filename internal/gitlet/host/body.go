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

// A FETCH REQUEST'S BODY IS READ TO ITS END BEFORE ANYTHING ANSWERS IT: its
// level and whether this node lacks a want are in it, and a fetch over a
// damaged cache is retried once over the same bytes (package doc, "A fetch
// request is spooled, and bounded by the repository it names"). It is held in
// memory up to RequestMemoryBytes, charged to RequestMemoryAllowance, and
// otherwise SPOOLED whole, waiting its turn for room (spool.go). It is GIVEN
// BACK at the answer's FIRST BYTE (sink.onStart), since a retry reads it until
// then: for a fetch that is its PACK's, after pack-objects' enumeration (at
// most a progress line or one 5 s uploadpack.keepAlive with side-band, until
// the raw pack without); for ls-refs or a push, its first output. Past it, a
// transfer its client paces holds neither memory nor a spool.

// The refusals reading a request makes, each answered in its own status.
var (
	// errRequestTooLarge is a request past its repository's bound (413).
	errRequestTooLarge = errors.New("host: the request is past its repository's bound")
	// errSpoolPastCeiling is a request past SpoolRequestBytes: refused at
	// once, since no wait would ever fit it (413).
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

// firstHead is the first buffer an undeclared request is read into: a fetch's
// negotiation is a few hundred bytes to a few kilobytes, so most fit it, and a
// larger one doubles toward RequestMemoryBytes, every copy together under its
// final size.
const firstHead = 4 << 10

// requestBody is one fetch request's decoded body, read to its end.
type requestBody struct {
	mu sync.Mutex
	// head is the body in memory, or a spooling request's first bytes until
	// they are written to the spool; held is its capacity charged to heads,
	// zero while a place in the spool's waiting room holds it instead.
	head  []byte
	heads *allowance
	held  int64
	// spool holds the whole body, size bytes, once spooled; nil in memory.
	spool *os.File
	size  int64
	// spools is set once the request starts spooling: turn is its body there
	// (the room taken while arriving, what it spooled once ended), and placed
	// is true while it holds a place in credential's waiting room.
	spools     *spools
	turn       *spoolBody
	credential string
	placed     bool
	// drained: upload-pack's feed reached the end; answered: the response's
	// first byte was written. Both together, nothing reads the body again.
	drained, answered bool
	closed            bool
}

// reader is the whole body from its start, fresh each call: the parse, the
// lack check and each upload-pack run read it again. A body given back fails
// rather than read a closed spool.
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

// errReleased is a read of a body already given back, which nothing does: it
// is given back only once read and answered, and only an unanswered run is
// retried.
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

// feed is the body as upload-pack reads it: at its end it drops its bytes and
// marks the body DRAINED, given back once answered too. One feed an attempt:
// only the latest run's end counts.
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
		// holds this reader through the transfer, which would pin the head.
		f.r = nil
		f.b.mu.Lock()
		f.b.drained = true
		f.b.releaseLocked()
		f.b.mu.Unlock()
	}
	return n, err
}

// started marks the response's first byte written, giving back a drained
// body.
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

// close gives everything back: the unnamed spool (the kernel removes it), its
// body and place to the spool allowance, the head's memory to its own.
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

// readRequest reads src to its end: into memory up to s.requestMemory, the
// buffer grown from declared (-1 when undeclared or encoded) or firstHead,
// each growth charged to s.heads; past that, or past what s.heads has left,
// whole into a spool once spools.start admits it, its room taken as bytes
// arrive (spools.more), waiting its turn for either. bound is asked with
// fresher false, then true before refusing, so a stale row never refuses what
// a current one admits. Errors: errRequestTooLarge past the bound,
// errSpoolPastCeiling past SpoolRequestBytes, errPastClaim past the declared
// length, errSpoolFull for a wait not granted, the context's error if it ended
// while waiting, src's own failure as itself.
func (s *Server) readRequest(ctx context.Context, src io.Reader, declared int64, credential string,
	bound func(ctx context.Context, fresher bool) (int64, error)) (*requestBody, error) {

	b := &requestBody{heads: s.heads}
	memory := int64(s.requestMemory)
	want := int64(firstHead)
	if declared >= 0 {
		// One byte past the declared length, so its end is read with it,
		// capped below memory FIRST: it is the client's word, net/http admits
		// math.MaxInt64, and one past it wraps negative.
		want = min(declared, memory-1) + 1
	}
	for {
		want = min(want, memory)
		if grow := want - int64(cap(b.head)); grow > 0 {
			if !s.heads.take(grow) {
				// The node's request memory is spent: the rest spools.
				break
			}
			b.held += grow
			next := make([]byte, len(b.head), want)
			copy(next, b.head)
			b.head = next
		}
		if len(b.head) == cap(b.head) {
			// A HEAD WITH NO ROOM AND NONE GIVEN spools: a fill of nothing
			// reports no end, and asking again would spin.
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
		// Not at the end, so the buffer is full (objstore.Fill): double it.
		want = 2 * int64(cap(b.head))
	}
	// ONE BYTE FIRST: a request memory held exactly opens no spool and waits
	// for no turn.
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
	// AT LEAST what arrived and what the client declared: a request past its
	// bound or the spool ceiling is refused before it waits for room.
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
	// ONE ALLOWANCE for the whole request, read once: every later room and
	// its end go to the allowance its body started in.
	sp := s.spools
	ceiling := sp.request
	if atLeast > ceiling {
		b.close()
		return nil, sp.pastCeiling()
	}
	// IT STARTS, claiming the most it can grow to (the ceiling, or the
	// declared length, where net/http ends an unencoded body) with room for
	// the head and the next byte; while it waits its turn, the head moves to
	// a place in the spool's waiting room, out of the shared request memory.
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
	// THE HEAD GOES INTO THE SPOOL and its memory or place is let go, so a
	// spooled request waiting for its sync and transfer slot holds only disk,
	// and a queue of them never pushes the requests behind them to spool.
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
		// ROOM AS THE BYTES ARRIVE: the spool holds what its client sent,
		// never what it might yet send.
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
	// ITS BODY HAS ENDED: unwritten room and its claim go back now, not with
	// the request, which may wait a cold sync of minutes before upload-pack
	// reads it.
	sp.ended(b.turn, b.size)
	return b, nil
}

// spoolWaited records a request's turn at the spool allowance: a wait is
// counted and timed by outcome, a refused place only counted, and an
// immediate grant neither.
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

// allowance is a node-wide count of bytes against a limit, taken before they
// are held and refused rather than waited for.
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
