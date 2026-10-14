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
// request past its in-memory part, or one that finds the allowance spent, has
// the rest SPOOLED to an unnamed file on the cache's volume, so a partial
// clone's lazy fetch of a million wants is served where git's own 10 MiB
// buffer refused it, and read back as a stream wherever it is needed — never
// held whole. And the body is GIVEN BACK the moment upload-pack has read it to
// its end and written its answer's first byte, since nothing reads it again
// then: a transfer paced by its client holds neither memory nor a spool.

// The refusals reading a request makes, each answered in its own status.
var (
	// errRequestTooLarge is a request past its repository's bound (413).
	errRequestTooLarge = errors.New("host: the request is past its repository's bound")
	// errSpoolPastShare is a request that would spool more than one
	// credential's share of the node's spool allowance by itself — one this
	// node can never take, however idle (413).
	errSpoolPastShare = errors.New("host: the request is past what one credential may spool")
	// errSpoolFull is a request that found the node's spool allowance
	// spent, or its credential holding its share of it or as many spools as
	// it may (503).
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
	// head is the body's first bytes, held in memory; heads is the
	// allowance it is charged to, held how much of it (its capacity).
	head  []byte
	heads *allowance
	held  int64
	// spool holds what followed head, size bytes of it; nil when the
	// request fitted in memory.
	spool *os.File
	size  int64
	// spools is the allowance the spool draws on — credential's count and
	// taken bytes of it — set once the request outgrew memory.
	spools     *spools
	credential string
	taken      int64
	counted    bool
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
	return io.MultiReader(bytes.NewReader(b.head), io.NewSectionReader(b.spool, 0, b.size))
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
// removes, as it has no name — its bytes and its count returned to the spool
// allowance, and the head's memory to its own. Idempotent, and safe on a body
// that never spooled.
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
		b.spools.give(b.credential, b.taken, b.counted)
		b.taken, b.counted = 0, false
	}
	b.head = nil
	if b.held > 0 {
		b.heads.give(b.held)
		b.held = 0
	}
}

// spoolPiece is how much of a request is copied into its spool at a time, and
// taken from the allowance before it is written.
const spoolPiece = 64 << 10

// readRequest reads src to its end: up to s.requestMemory bytes into memory,
// in a buffer grown with the request from its declared length (declared, -1
// when the client did not say or encoded the body) or from firstHead, each
// growth charged to the node's request memory; the rest — past the in-memory
// part, or past what the allowance had left — into a spool the cache makes.
// bound answers the most the request may carry: asked once with fresher false,
// and once more with fresher true before a request past the first answer is
// refused, so a bound read from a stale row never refuses a request a current
// one admits. A request past the bound is errRequestTooLarge; one past its
// credential's whole share of the spool allowance errSpoolPastShare; one that
// finds the allowance or its credential's share spent, or its credential at
// its count, errSpoolFull; src's own failure is returned as itself.
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
	piece := make([]byte, spoolPiece)
	// ONE BYTE FIRST: a request the in-memory part held exactly opens no
	// spool, and one that goes on is counted as spooling at once, not a
	// piece later.
	k, end, err := objstore.Fill(src, piece[:1])
	switch {
	case err != nil:
		b.close()
		return nil, err
	case k == 0 && end:
		return b, nil
	}
	limit, err := bound(ctx, false)
	if err != nil {
		b.close()
		return nil, err
	}
	if err = s.spools.open(credential); err != nil {
		b.close()
		return nil, err
	}
	b.spools, b.credential, b.counted = s.spools, credential, true
	if b.spool, err = s.o.Cache.Spool(); err != nil {
		b.close()
		return nil, err
	}
	fresher := false
	for {
		if total := b.length() + int64(k); total > limit {
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
		if k > 0 {
			if err = s.spools.take(credential, b.taken, int64(k)); err != nil {
				b.close()
				return nil, err
			}
			b.taken += int64(k)
			if _, err = b.spool.Write(piece[:k]); err != nil {
				b.close()
				return nil, fmt.Errorf("host: spool a request: %w", err)
			}
			b.size += int64(k)
		}
		if end {
			return b, nil
		}
		if k, end, err = objstore.Fill(src, piece); err != nil {
			b.close()
			return nil, err
		}
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

// spools is the node's SPOOL ALLOWANCE: the bytes every spooled request holds
// at once, against limit (SpoolAllowance); the bytes each credential's spools
// hold, against share (SpoolCredentialBytes); and how many spools each
// credential holds, against perCredential (SpoolsPerCredential). Never the
// cache's budget: a spool has no name the cache counts (cache.Spool), and
// counted there one reader's large request would make the budget evict warm
// repositories. A request that finds any of them spent is refused at once,
// never queued: a queued request holds its connection reading at the client's
// pace.
type spools struct {
	limit, share  int64
	perCredential int

	mu    sync.Mutex
	used  int64
	bytes map[string]int64
	held  map[string]int
}

func newSpools(limit, share int64, perCredential int) *spools {
	return &spools{limit: limit, share: share, perCredential: perCredential,
		bytes: map[string]int64{}, held: map[string]int{}}
}

// open counts a spool against credential's count.
func (a *spools) open(credential string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held[credential] >= a.perCredential {
		return fmt.Errorf("%w: this credential already holds %d spooled requests, as many as one "+
			"credential may hold at once", errSpoolFull, a.held[credential])
	}
	a.held[credential]++
	return nil
}

// take takes n more bytes for a spool of credential's already holding own:
// refused past the credential's share by that spool alone, past the share
// with the credential's other spools, and past the node's allowance.
func (a *spools) take(credential string, own, n int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case own+n > a.share:
		return fmt.Errorf("%w: past the %d bytes one credential's requests may hold spooled on this node",
			errSpoolPastShare, a.share)
	case a.bytes[credential]+n > a.share:
		return fmt.Errorf("%w: this credential's spooled requests already hold %d of the %d bytes one "+
			"credential may hold", errSpoolFull, a.bytes[credential], a.share)
	case a.used+n > a.limit:
		return fmt.Errorf("%w: the node's %d bytes of spool are in use", errSpoolFull, a.limit)
	}
	a.used += n
	a.bytes[credential] += n
	return nil
}

// give gives back n bytes credential's spool held, and its count when
// counted.
func (a *spools) give(credential string, n int64, counted bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.used -= n
	if a.bytes[credential] -= n; a.bytes[credential] <= 0 {
		delete(a.bytes, credential)
	}
	if counted {
		if a.held[credential]--; a.held[credential] <= 0 {
			delete(a.held, credential)
		}
	}
}

// inUse is the bytes and the spools held right now.
func (a *spools) inUse() (int64, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, k := range a.held {
		n += k
	}
	return a.used, n
}
