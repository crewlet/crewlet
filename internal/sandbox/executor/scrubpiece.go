package executor

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"

	"github.com/crewlet/crewlet/internal/redact"
)

// A TEXT EITHER READING STAGES BEFORE ITS SHAPE PASS: the first reading's
// ([firstPieces]) finds the texts of at least [scrubPieceBaseBytes] the shape
// pass leaves unchanged, by its own full scrub or by the process's memo
// ([scrubMemo]), and plans each by its place and its chunks' digests; the
// second's ([secondPieces]) writes a planned text a chunk at a time, each only
// once it matches its digest, and redacts every other. The package doc's "A
// text a take found unchanged is not redacted again" has the argument.

// errMemoDiverged is a text whose first chunk the memo knew and whose whole it
// did not: the first reading is read again without that chunk's lookups.
var errMemoDiverged = errors.New("executor: a text the memo knew by its start came out otherwise")

// errPieceChanged is a text the second reading found other than the digests
// the first planned it by: a member, or the export, that changed.
var errPieceChanged = fmt.Errorf("%w: a text the first reading planned came out otherwise", errLinesChanged)

// The ways a first reading reads a line or a `.json` file at once: as text,
// and as JSON, each planning its own texts until the line's way is known.
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

// piecePos is where a text begins: the unit it is in (each regular member of
// a tar, and each line of a transcript, from 1; an export's is 0) and its
// offset there (0 for a unit read as text, the scanner's for a JSON token).
type piecePos struct{ unit, offset int64 }

// before reports whether p comes before q.
func (p piecePos) before(q piecePos) bool {
	return p.unit < q.unit || p.unit == q.unit && p.offset < q.offset
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
// out otherwise; checked, the second reading wrote as planned, unredacted.
type pieceCounts struct {
	hits, misses, diverged, checked int
}

// counts is where a second reading counts its texts: use's, or none.
func (use *memoUse) counts() *pieceCounts {
	if use == nil {
		return nil
	}
	return &use.n
}

// firstPieces is the first reading's stage of a take: what it plans each text
// by, and what of the memo it may still use.
type firstPieces struct {
	use *memoUse
	// denied is every first chunk whose answer diverged in this take, never
	// looked up again; lookups is false once a reading read again diverged
	// too ([firstReadings]).
	denied  map[memoKey]bool
	lookups bool
	plan    *tarPlan
	unit    int64
	ways    [2]firstWay
}

// firstWay is one way of the first reading: the buffer its texts' first
// chunks are held in, one at a time, and the texts it found unchanged, planned
// once the way is the one kept.
type firstWay struct {
	r       *firstPieces
	buf     []byte
	pending []verbatimText
}

// verbatimText is a text the first reading found unchanged: where it begins
// and its chunks' digests.
type verbatimText struct {
	at      piecePos
	digests [][sha256.Size]byte
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

// begin readies r for one first reading, planning on plan.
func (r *firstPieces) begin(plan *tarPlan) *firstPieces {
	r.plan, r.unit = plan, 0
	for i := range r.ways {
		r.ways[i].pending = nil
	}
	return r
}

// nextUnit begins the next unit; what a way left unplanned is no longer kept.
func (r *firstPieces) nextUnit() {
	r.unit++
	r.forget()
}

// keep plans the texts way found unchanged, the unit's kept way, and forgets
// the other's.
func (r *firstPieces) keep(way int) {
	for _, v := range r.ways[way].pending {
		r.plan.addVerbatim(v)
	}
	r.forget()
}

// forget drops every way's texts not yet planned.
func (r *firstPieces) forget() {
	for i := range r.ways {
		clear(r.ways[i].pending)
		r.ways[i].pending = r.ways[i].pending[:0]
	}
}

// divergedAt records that the memo's answer for key came out otherwise.
func (r *firstPieces) divergedAt(key memoKey) {
	r.denied[key] = true
	r.use.n.diverged++
}

// open is the stage of the text beginning at offset in the current unit,
// written onto out.
func (w *firstWay) open(out *textOut, offset int64) pieceSink {
	if w.buf == nil {
		w.buf = make([]byte, 0, scrubPieceBaseBytes)
	}
	w.buf = w.buf[:0]
	return &firstPiece{way: w, at: piecePos{w.r.unit, offset}, out: out}
}

// firstPiece is one text of the first reading. Its first chunk is held: a
// text ending sooner is redacted whole; one that reaches it is looked up, and
// is either the memo's (written as it is, hashed, and checked at its end) or
// redacted, hashed on both sides to learn whether the redaction changed it.
type firstPiece struct {
	way     *firstWay
	at      piecePos
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
	p.in = newPieceDigests(true)
	_, _ = p.in.Write(p.way.buf)
	p.key = memoKeyOf(r.use.scope, p.in.list[0])
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
	p.kept = newPieceDigests(false)
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
	p.way.pending = append(p.way.pending, verbatimText{at: p.at, digests: p.in.list})
	return nil
}

// pieceDigests hashes a text in chunks of [scrubPieceBaseBytes]: each chunk's
// digest, kept in list where asked, and the digest of them all in order.
type pieceDigests struct {
	chunk, whole hash.Hash
	inChunk      int
	length       int64
	keepList     bool
	list         [][sha256.Size]byte
}

// newPieceDigests is a text's digests, keeping each chunk's where list is set.
func newPieceDigests(list bool) *pieceDigests {
	return &pieceDigests{chunk: sha256.New(), whole: sha256.New(), keepList: list}
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
	if d.keepList {
		d.list = append(d.list, s)
	}
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

// secondPieces is the second reading's stage: the texts the plan names, read
// in the order planned. It reads no memo.
type secondPieces struct {
	plan   *tarPlan
	counts *pieceCounts
	unit   int64
	// next is the next planned text, read off the plan ahead of its place
	// (pending); lastUnit is the unit the plan's last text counted from.
	next     verbatimText
	pending  bool
	lastUnit int64
	buf      []byte
}

// nextUnit begins the next unit.
func (s *secondPieces) nextUnit() { s.unit++ }

// open is the stage of the text beginning at offset in the current unit: a
// check of each chunk against the plan where it planned the text there, and
// nil, the shape pass, where it did not. A planned text the reading passed
// without reaching, one of a cut element, is passed over.
func (s *secondPieces) open(out *textOut, offset int64) pieceSink {
	at := piecePos{s.unit, offset}
	for {
		if !s.pending && !s.readNext() {
			return nil
		}
		if s.next.at.before(at) {
			s.pending = false
			continue
		}
		if s.next.at != at {
			return nil
		}
		s.pending = false
		if s.counts != nil {
			s.counts.checked++
		}
		if s.buf == nil {
			s.buf = make([]byte, 0, scrubPieceBaseBytes)
		}
		s.buf = s.buf[:0]
		return &checkedPiece{digests: s.next.digests, out: out, buf: &s.buf}
	}
}

// readNext reads the plan's next text, false where none is left.
func (s *secondPieces) readNext() bool {
	v := &s.plan.verbatim
	if v.done() {
		return false
	}
	gap, ok := v.takeNumber()
	offset, placed := v.takeNumber()
	more, counted := v.takeNumber()
	if !ok || !placed || !counted {
		return false
	}
	s.lastUnit += int64(gap)
	s.next = verbatimText{at: piecePos{s.lastUnit, int64(offset)}, digests: make([][sha256.Size]byte, more+1)}
	for i := range s.next.digests {
		for j := range s.next.digests[i] {
			c, read := v.takeByte()
			if !read {
				return false
			}
			s.next.digests[i][j] = c
		}
	}
	s.pending = true
	return true
}

// checkedPiece writes a planned text a chunk at a time, each once its digest
// is the plan's: a text other than planned fails before a byte of the chunk
// that differs is written.
type checkedPiece struct {
	digests [][sha256.Size]byte
	out     *textOut
	buf     *[]byte
	at      int
}

func (c *checkedPiece) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		take := min(len(b), scrubPieceBaseBytes-len(*c.buf))
		*c.buf = append(*c.buf, b[:take]...)
		if b = b[take:]; len(*c.buf) == scrubPieceBaseBytes {
			if err := c.flush(); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

// flush writes the chunk held once it matches its digest.
func (c *checkedPiece) flush() error {
	if c.at >= len(c.digests) || sha256.Sum256(*c.buf) != c.digests[c.at] {
		return errPieceChanged
	}
	if _, err := c.out.Write(*c.buf); err != nil {
		return err
	}
	c.at++
	*c.buf = (*c.buf)[:0]
	return nil
}

func (c *checkedPiece) Close() error {
	if len(*c.buf) > 0 {
		if err := c.flush(); err != nil {
			return err
		}
	}
	if c.at != len(c.digests) {
		return errPieceChanged
	}
	return nil
}
