package executor

import (
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"slices"

	"github.com/crewlet/crewlet/internal/redact"
)

// A TEXT THE FIRST READING STAGES BEFORE ITS SHAPE PASS ([firstPieces]) is
// looked up in the process's memo ([scrubMemo]) by its first chunk: one the
// memo knows the shape pass leaves unchanged is written as it came and checked
// at its end, and every other is redacted, and kept in the memo where its
// redaction left it unchanged. The package doc's "A text a take found
// unchanged is not redacted again" has the argument.

// errMemoDiverged is a text whose first chunk the memo knew and whose whole it
// did not: the first reading is read again without that chunk's lookups.
var errMemoDiverged = errors.New("executor: a text the memo knew by its start came out otherwise")

// The ways a first reading reads a line or a `.json` file at once: as text,
// and as JSON, each staging its own texts until the line's way is known.
const (
	textWay = iota
	jsonWay
)

// pieceSink is where a text's by-value pass writes: the shape pass, or a
// stage before it.
type pieceSink interface {
	io.Writer
	Close() error
}

// memoUse is a take's way into the process's memo, and what its readings did
// with their texts, which the take logs.
type memoUse struct {
	memo  *scrubMemo
	scope string
	n     pieceCounts
}

// pieceCounts is what a take's readings did with texts of a chunk or more:
// hits, the memo answered for; misses, it did not; diverged, its answer came
// out otherwise.
type pieceCounts struct {
	hits, misses, diverged int
}

// firstPieces is the first reading's stage of a take: what of the memo it may
// still use, and each way's room for a text's first chunk.
type firstPieces struct {
	use *memoUse
	// denied is every first chunk whose answer diverged in this take, never
	// looked up again; lookups is false once a reading read again diverged
	// too ([firstReadings]).
	denied  map[memoKey]bool
	lookups bool
	ways    [2]firstWay
}

// firstWay is one way of the first reading: the buffer its texts' first
// chunks are held in, one at a time.
type firstWay struct {
	r   *firstPieces
	buf []byte
}

// newFirstPieces is a take's first-reading stage, using use's memo where it
// has one.
func newFirstPieces(use *memoUse) *firstPieces {
	if use == nil {
		use = &memoUse{}
	}
	r := &firstPieces{use: use, denied: map[memoKey]bool{}, lookups: use.memo != nil}
	for i := range r.ways {
		r.ways[i].r = r
	}
	return r
}

// divergedAt records that the memo's answer for key came out otherwise.
func (r *firstPieces) divergedAt(key memoKey) {
	r.denied[key] = true
	r.use.n.diverged++
}

// open is the stage of a text, written onto out.
func (w *firstWay) open(out *textOut) pieceSink {
	if w.buf == nil {
		w.buf = make([]byte, 0, scrubPieceBaseBytes)
	}
	w.buf = w.buf[:0]
	return &firstPiece{way: w, out: out}
}

// firstPiece is one text of the first reading. Its first chunk is held: a
// text ending sooner is redacted whole; one that reaches it is looked up, and
// is either the memo's (written as it is, hashed, and checked at its end) or
// redacted, hashed on both sides to learn whether the redaction changed it.
type firstPiece struct {
	way     *firstWay
	out     *textOut
	decided bool
	key     memoKey
	known   []pieceWhole
	shape   *redact.Stream
	in      *pieceDigests
	kept    *pieceDigests
}

func (p *firstPiece) Write(b []byte) (int, error) {
	n := len(b)
	if !p.decided {
		take := min(len(b), scrubPieceBaseBytes-len(p.way.buf))
		p.way.buf = append(p.way.buf, b[:take]...)
		if b = b[take:]; len(p.way.buf) < scrubPieceBaseBytes {
			return n, nil
		}
		if err := p.decide(); err != nil {
			return 0, err
		}
	}
	if len(b) == 0 {
		return n, nil
	}
	_, _ = p.in.Write(b)
	var err error
	if p.shape != nil {
		_, err = p.shape.Write(b)
	} else {
		_, err = p.out.Write(b)
	}
	if err != nil {
		return 0, err
	}
	return n, nil
}

// decide looks the held first chunk up and hands it on.
func (p *firstPiece) decide() error {
	p.decided = true
	r := p.way.r
	p.in = newPieceDigests()
	_, _ = p.in.Write(p.way.buf)
	p.key = memoKeyOf(r.use.scope, p.in.first)
	if r.lookups && !r.denied[p.key] {
		p.known = r.use.memo.candidates(p.key)
	}
	if len(p.known) > 0 {
		_, err := p.out.Write(p.way.buf)
		return err
	}
	if r.use.memo != nil {
		r.use.n.misses++
	}
	p.kept = newPieceDigests()
	p.shape = redact.NewStream(io.MultiWriter(p.kept, p.out))
	_, err := p.shape.Write(p.way.buf)
	return err
}

func (p *firstPiece) Close() error {
	if !p.decided {
		shape := redact.NewStream(p.out)
		if _, err := shape.Write(p.way.buf); err != nil {
			return err
		}
		return shape.Close()
	}
	r := p.way.r
	whole := p.in.finish()
	if p.shape == nil {
		if !slices.Contains(p.known, whole) {
			r.divergedAt(p.key)
			return errMemoDiverged
		}
		r.use.n.hits++
	} else {
		if err := p.shape.Close(); err != nil {
			return err
		}
		if p.kept.finish() != whole {
			return nil
		}
	}
	if r.use.memo != nil {
		r.use.memo.keep(r.use.scope, p.key, whole)
	}
	return nil
}

// pieceDigests hashes a text in chunks of [scrubPieceBaseBytes]: its first
// chunk's digest, and the digest of every chunk's in order.
type pieceDigests struct {
	chunk, whole hash.Hash
	inChunk      int
	length       int64
	chunks       int
	first        [sha256.Size]byte
}

// newPieceDigests is a text's digests.
func newPieceDigests() *pieceDigests {
	return &pieceDigests{chunk: sha256.New(), whole: sha256.New()}
}

func (d *pieceDigests) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		take := min(len(p), scrubPieceBaseBytes-d.inChunk)
		_, _ = d.chunk.Write(p[:take])
		d.inChunk += take
		d.length += int64(take)
		if p = p[take:]; d.inChunk == scrubPieceBaseBytes {
			d.endChunk()
		}
	}
	return n, nil
}

// endChunk ends the chunk being hashed.
func (d *pieceDigests) endChunk() {
	var s [sha256.Size]byte
	d.chunk.Sum(s[:0])
	d.chunk.Reset()
	d.inChunk = 0
	_, _ = d.whole.Write(s[:])
	if d.chunks == 0 {
		d.first = s
	}
	d.chunks++
}

// finish ends the text, and is it as the memo knows it.
func (d *pieceDigests) finish() pieceWhole {
	if d.inChunk > 0 {
		d.endChunk()
	}
	w := pieceWhole{length: d.length}
	d.whole.Sum(w.digest[:0])
	return w
}
