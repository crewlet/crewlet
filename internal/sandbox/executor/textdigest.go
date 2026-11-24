package executor

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"slices"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/redact"
)

// textDigestBytes is the piece a digested string is read in: a run the
// scanner copies in one step and SHA-256 hashes in one call.
const textDigestBytes = 32 << 10

// textDigestHeadBytes is how much of a text's opening a digest keeps as
// read ([textDigest.opens]): room for the longest opening a reading compares,
// a harness's stub of a spilled output or its wrapper's opening tag.
const textDigestHeadBytes = 32

// textDigest is what a reading by content reads of a string it digests (the
// export walk's [meansDigest]), which passes through it a piece at a time and
// is never held: its decoded text's length, its SHA-256, the sum of each
// opening the reading names ([newTextDigest]), whether it holds
// [redact.Marker], whether it is blank (white space alone, as
// strings.TrimSpace reads it), and its first [textDigestHeadBytes] bytes.
type textDigest struct {
	h hash.Hash
	// openings are the opening lengths a sum is kept at, ascending, and sums
	// each one's, where the text reached it; next is the next to reach.
	openings []int
	sums     [][32]byte
	next     int
	// n is the text's length so far, whole its sum once it ended.
	n     int
	whole [32]byte
	// marker and blank are what the text read so far holds; tail is its last
	// bytes short of a marker's length, and edge where a piece's edge is read.
	marker, blank bool
	tail, edge    []byte
	buf           []byte
	// head is the text's opening, its first headN bytes read.
	head  [textDigestHeadBytes]byte
	headN int
}

// newTextDigest is a digest keeping the sums of openings.
func newTextDigest(openings []int) *textDigest {
	openings = slices.Compact(slices.Sorted(slices.Values(openings)))
	return &textDigest{h: sha256.New(), openings: openings, sums: make([][32]byte, len(openings)),
		tail: make([]byte, 0, len(redact.Marker)-1), edge: make([]byte, 0, 2*len(redact.Marker)),
		buf: make([]byte, textDigestBytes)}
}

// reset starts a text.
func (d *textDigest) reset() {
	d.h.Reset()
	d.n, d.next, d.marker, d.blank, d.tail, d.headN = 0, 0, false, true, d.tail[:0], 0
	d.sum(nil)
}

// write reads the next piece of the text, whole characters.
func (d *textDigest) write(p []byte) {
	d.headN += copy(d.head[d.headN:], p)
	d.blank = d.blank && blank(p)
	if !d.marker {
		// A MARKER ACROSS TWO PIECES is read off the edge between them: the
		// last bytes of the text so far and the first of the piece.
		d.edge = append(append(d.edge[:0], d.tail...), p[:min(len(p), len(redact.Marker)-1)]...)
		d.marker = bytes.Contains(d.edge, []byte(redact.Marker)) || bytes.Contains(p, []byte(redact.Marker))
		keep := len(redact.Marker) - 1
		if len(p) >= keep {
			d.tail = append(d.tail[:0], p[len(p)-keep:]...)
		} else {
			d.tail = append(d.tail[:0], d.edge[max(0, len(d.edge)-keep):]...)
		}
	}
	d.sum(p)
}

// sum hashes p, keeping the sum at each opening it reaches.
func (d *textDigest) sum(p []byte) {
	for d.next < len(d.openings) && d.n+len(p) >= d.openings[d.next] {
		cut := d.openings[d.next] - d.n
		d.h.Write(p[:cut])
		d.n, p = d.n+cut, p[cut:]
		d.h.Sum(d.sums[d.next][:0])
		d.next++
	}
	d.h.Write(p)
	d.n += len(p)
}

// end ends the text.
func (d *textDigest) end() { d.h.Sum(d.whole[:0]) }

// scan reads sc's current string into d, a piece at a time, from its start
// to its end, holding none of it.
func (d *textDigest) scan(sc *jsonscan.Scanner) error {
	d.reset()
	for {
		n, err := sc.Read(d.buf)
		d.write(d.buf[:n])
		switch {
		case errors.Is(err, io.EOF):
			d.end()
			return nil
		case err != nil:
			return err
		}
	}
}

// opens reports whether the text read opens with prefix, at most
// [textDigestHeadBytes] long: a longer one is a reading's own mistake, which
// no answer fits (false holds a stub, true passes a missing opening), so it
// panics on the first case that compares it.
func (d *textDigest) opens(prefix string) bool {
	if len(prefix) > len(d.head) {
		panic("executor: a text digest keeps no opening as long as " + strconv.Quote(prefix))
	}
	return d.headN >= len(prefix) && string(d.head[:len(prefix)]) == prefix
}

// blank reports whether p, whole characters, is white space alone, as
// strings.TrimSpace reads it.
func blank(p []byte) bool {
	for len(p) > 0 {
		if c := p[0]; c < utf8.RuneSelf {
			if c != ' ' && (c < '\t' || c > '\r') {
				return false
			}
			p = p[1:]
			continue
		}
		r, size := utf8.DecodeRune(p)
		if !unicode.IsSpace(r) {
			return false
		}
		p = p[size:]
	}
	return true
}
