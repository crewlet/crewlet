package code_test

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // git's hashfile trailer is a SHA-1; the case writes one as git would
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// oneSlot is a read lane of one slot, as a git node's read lane is to a hash
// (gitproc.Pool.HoldRead): hold takes the slot, and the function it answers
// gives it back. It keeps the bytes read in each hold (a reader through it
// counts them, read), counts a hold asked while the slot is still held — a
// hash that held two at once, or never gave one back — and, where set,
// answers the n-th hold with refuse's error or calls taken as the n-th is
// taken.
type oneSlot struct {
	refuse func(n int) error
	taken  func(n int)

	mu     sync.Mutex
	held   bool
	twice  int
	pieces []int64
}

func (l *oneSlot) hold(context.Context) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.pieces) + 1
	if l.refuse != nil {
		if err := l.refuse(n); err != nil {
			return nil, err
		}
	}
	if l.held {
		l.twice++
	}
	l.held = true
	l.pieces = append(l.pieces, 0)
	if l.taken != nil {
		l.taken(n)
	}
	return sync.OnceFunc(func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.held = false
	}), nil
}

// read counts n bytes read to the hold the slot is in, answering whether it
// is held.
func (l *oneSlot) read(n int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		l.pieces[len(l.pieces)-1] += n
	}
	return l.held
}

// state is the slot as a case reads it once a hash has returned.
func (l *oneSlot) state() (held bool, twice int, pieces []int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held, l.twice, slices.Clone(l.pieces)
}

// slotted is a file read through a lane: every read counted to the hold the
// slot is in as it is made, and the offset of every read made with none kept
// apart.
type slotted struct {
	r    *bytes.Reader
	lane *oneSlot

	mu     sync.Mutex
	held   int64
	unheld []int64
}

func (s *slotted) ReadAt(p []byte, off int64) (int, error) {
	n, err := s.r.ReadAt(p, off)
	held := s.lane.read(int64(n))
	s.mu.Lock()
	defer s.mu.Unlock()
	if held {
		s.held += int64(n)
	} else {
		s.unheld = append(s.unheld, off)
	}
	return n, err
}

// sealed is body with the trailer h writes after it, as git's hashfile does.
func sealed(body []byte, h hash.Hash) []byte {
	h.Write(body)
	return h.Sum(slices.Clone(body))
}

// A HASH OF GIT'S TRAILER READS A PIECE A SLOT AND STOPS WITH ITS CALLER — the
// one rule every hashfile a git node holds is checked by, a layer's and the
// cache's alike. A file of a thousand bytes and its trailer, hashed through a
// buffer of seven in pieces of 64 under a lane of one slot, is sound by the
// trailer it holds, every byte before the trailer read inside a slot and no
// more than a piece in one, each slot given back before the next and the
// last before the hash returns, and only the trailer read with none; a byte
// changed is a file that is not sound, and no error; a context ended as a
// slot is taken reads nothing more and answers its cause; a lane that
// refuses a slot is answered as it refused; a file that ends before its size
// is io.ErrUnexpectedEOF, inside its bytes and inside its trailer; a hold
// over pieces of nothing is refused before it is asked. The same rule holds a
// SHA-256 trailer (a provenance run's), and a hash with no hold reads the
// file in one pass.
//
// Mutations: no context check before a read (the ended context's case reads
// its piece — red); the whole file read in one hold (the pieces — red); the
// slot given back only once the hash ends (two holds at once — red).
func TestAHashOfGitsTrailerReadsAPieceASlotAndStopsWithItsCaller(t *testing.T) {
	t.Parallel()
	body := make([]byte, 1000)
	for i := range body {
		body[i] = byte(i*7 + 3)
	}
	file := sealed(body, sha1.New())
	size := int64(len(file))
	pieces := func(l *oneSlot) code.Pieces { return code.Pieces{Buf: make([]byte, 7), Size: 64, Hold: l.hold} }
	through := func(b []byte, l *oneSlot) *slotted { return &slotted{r: bytes.NewReader(b), lane: l} }

	lane := &oneSlot{}
	r := through(file, lane)
	got, err := code.HashTrailer(t.Context(), r, size, sha1.New(), pieces(lane))
	held, twice, read := lane.state()
	want := append(slices.Repeat([]int64{64}, 15), 40)
	switch {
	case err != nil || !got.Sound() || !bytes.Equal(got.Held, file[1000:]) || got.Read != size:
		t.Errorf("a sound file: %+v, %v; want it sound, its trailer held and all %d bytes read", got, err, size)
	case !slices.Equal(read, want) || r.held != 1000:
		t.Errorf("the body was read %v a slot (%d bytes in all), want %v", read, r.held, want)
	case held || twice != 0:
		t.Errorf("the slot is held after the hash (%t), or was asked while held %d times", held, twice)
	case !slices.Equal(r.unheld, []int64{1000}):
		t.Errorf("reads with no slot held began at %v, want the trailer's alone, at 1000", r.unheld)
	}

	damaged := slices.Clone(file)
	damaged[500] ^= 1
	if got, err := code.HashTrailer(t.Context(), bytes.NewReader(damaged), size, sha1.New(),
		pieces(&oneSlot{})); err != nil || got.Sound() {
		t.Errorf("a byte changed: %+v, %v; want a file that is not sound, and no error", got, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lane = &oneSlot{taken: func(n int) {
		if n == 3 {
			cancel()
		}
	}}
	got, err = code.HashTrailer(ctx, through(file, lane), size, sha1.New(), pieces(lane))
	_, _, read = lane.state()
	if !errors.Is(err, context.Canceled) || got.Sound() || got.Read != 128 ||
		!slices.Equal(read, []int64{64, 64, 0}) {
		t.Errorf("a context ended as the third slot is taken: %+v, %v, read %v a slot; want context.Canceled and "+
			"nothing read past the second piece", got, err, read)
	}

	refused := errors.New("no read slot was free in time")
	lane = &oneSlot{refuse: func(n int) error {
		if n == 2 {
			return refused
		}
		return nil
	}}
	got, err = code.HashTrailer(t.Context(), through(file, lane), size, sha1.New(), pieces(lane))
	if !errors.Is(err, refused) || got.Sound() || got.Read != 64 {
		t.Errorf("a slot refused: %+v, %v; want the lane's refusal as it came, one piece read", got, err)
	}

	for _, c := range []struct {
		name string
		size int64
		read int64
	}{
		{"past its bytes", size + 980, size},  // the bytes before its trailer end inside them
		{"past its trailer", size + 10, size}, // the trailer ends inside it
	} {
		got, err := code.HashTrailer(t.Context(), bytes.NewReader(file), c.size, sha1.New(), code.Pieces{})
		if !errors.Is(err, io.ErrUnexpectedEOF) || got.Sound() || got.Read != c.read {
			t.Errorf("a file read %s: %+v, %v; want io.ErrUnexpectedEOF after its %d bytes", c.name, got, err, c.read)
		}
	}

	lane = &oneSlot{}
	if _, err := code.HashTrailer(t.Context(), bytes.NewReader(file), size, sha1.New(),
		code.Pieces{Hold: lane.hold}); err == nil {
		t.Error("a hold over pieces of no bytes hashed")
	}
	if _, _, read := lane.state(); len(read) != 0 {
		t.Errorf("a hold over pieces of no bytes was asked %d times", len(read))
	}
	if _, err := code.HashTrailer(t.Context(), bytes.NewReader(file[:10]), 10, sha1.New(), code.Pieces{}); err == nil {
		t.Error("a file shorter than its trailer hashed")
	}

	run := sealed(body, sha256.New())
	if got, err := code.HashTrailer(t.Context(), bytes.NewReader(run), int64(len(run)), sha256.New(),
		code.Pieces{}); err != nil || !got.Sound() || !bytes.Equal(got.Held, run[1000:]) {
		t.Errorf("a SHA-256 trailer, in one pass: %+v, %v; want it sound", got, err)
	}
	if (code.Trailer{}).Sound() {
		t.Error("the zero Trailer, of no trailer read, is sound")
	}
}

// THE PIECE LOOP UNDER IT reads what it was asked for and no more, stopping
// where its reader ends: over a reader of 200 bytes, in pieces of 64, 150
// bytes are three slots, the last of 22; 250 are the reader's 200, the last
// slot of 8, and no error — the caller judges a short file; 64 bytes are one
// slot, never a second for nothing; and none is no slot at all.
//
// Mutation: read a whole piece whatever is left to read (more than it was
// asked for — red).
func TestTheHashPieceLoopReadsWhatItWasAskedFor(t *testing.T) {
	t.Parallel()
	src := bytes.Repeat([]byte{'x'}, 200)
	for _, c := range []struct {
		name   string
		n      int64
		added  int64
		pieces []int64
	}{
		{"fewer than its reader holds", 150, 150, []int64{64, 64, 22}},
		{"past its reader's end", 250, 200, []int64{64, 64, 64, 8}},
		{"exactly a piece", 64, 64, []int64{64}},
		{"nothing", 0, 0, nil},
	} {
		lane := &oneSlot{}
		var sum bytes.Buffer
		r := io.TeeReader(bytes.NewReader(src), countTo(lane))
		added, err := code.HashPieces(t.Context(), &sum, r, c.n, code.Pieces{Size: 64, Hold: lane.hold})
		_, _, pieces := lane.state()
		if err != nil || added != c.added || int64(sum.Len()) != c.added || !slices.Equal(pieces, c.pieces) {
			t.Errorf("%s: added %d (%d written), %v, read %v a slot; want %d in %v", c.name, added, sum.Len(), err,
				pieces, c.added, c.pieces)
		}
	}
}

// countTo is a writer counting what passes through it to lane's slot.
func countTo(lane *oneSlot) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		lane.read(int64(len(p)))
		return len(p), nil
	})
}

// writerFunc is a func as an io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
