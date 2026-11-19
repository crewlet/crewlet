package redact

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// StreamedPrefix is [Prefix] read through a stream at any length, for the
// external test that holds it to [Secrets] over the stream's corpus.
func StreamedPrefix(text string, n int) (string, int) { return prefix(text, n, 0, holdBytes()) }

// AN OPENING OF A TEXT THE STREAM REFUSES IS SECRETS' OPENING: at a hold that
// makes the stream refuse a shape, asserted refused first, [prefix] answers
// the first n bytes of [Secrets] and its whole length, never what the stream
// wrote before it stopped.
//
// Mutation: answer what the opening kept when the stream refuses, and the
// whole length here is short of Secrets'.
func TestAnOpeningOfATextTheStreamRefusesIsSecretsOpening(t *testing.T) {
	t.Parallel()
	const hold = 256 << 10
	tried := 0
	for _, s := range holdShapes(768 << 10) {
		name := s.name[:strings.IndexByte(s.name, ' ')]
		if name != "c1" && name != "pb1" {
			continue
		}
		tried++
		text := s.text()
		if got := readHolding(text, hold, nil, 0); !errors.Is(got.err, ErrDecidedEarly) {
			t.Fatalf("%s at a hold of 256 KiB: Close answered %v, where the test needs a text the stream refuses", s.name, got.err)
		}
		want := Secrets(text)
		for _, n := range []int{1, len(want) / 2, len(want) + 1} {
			opening, whole := prefix(text, n, 0, hold)
			if whole != len(want) || opening != want[:min(n, len(want))] {
				t.Errorf("%s, an opening of %d bytes: %d bytes of a whole of %d, want %d of %d (equal: %t)",
					s.name, n, len(opening), whole, min(n, len(want)), len(want), opening == want[:min(n, len(want))])
			}
		}
	}
	if tried != 2 {
		t.Fatalf("found %d of the two refused shapes by name", tried)
	}
}

// recordedWrites is every write made to it, as written.
type recordedWrites struct{ writes [][]byte }

func (r *recordedWrites) Write(p []byte) (int, error) {
	r.writes = append(r.writes, bytes.Clone(p))
	return len(p), nil
}

// AN OPENING HANDS THE STREAM ITS TEXT IN PIECES: [writePieces] writes every
// byte of the text in order, none of its writes longer than [pieceBytes], so a
// text held as a string is never copied whole to be written.
//
// Mutation: write the text as one []byte, and the one write is the text's
// length.
func TestAnOpeningHandsTheStreamItsTextInPieces(t *testing.T) {
	t.Parallel()
	var text strings.Builder
	for i := range 3*pieceBytes + 17 {
		text.WriteByte(byte('a' + i%23))
	}
	var w recordedWrites
	if err := writePieces(&w, text.String()); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for i, p := range w.writes {
		if len(p) > pieceBytes {
			t.Errorf("write %d is %d bytes, past the %d a piece is", i, len(p), pieceBytes)
		}
		got = append(got, p...)
	}
	if string(got) != text.String() {
		t.Errorf("the writes put together are %d bytes, not the %d-byte text in order", len(got), text.Len())
	}
	want := errors.New("the writer's own")
	if err := writePieces(failingWrites{want}, "x"); !errors.Is(err, want) {
		t.Errorf("a writer's error: writePieces answered %v, want %v", err, want)
	}
}

type failingWrites struct{ err error }

func (f failingWrites) Write([]byte) (int, error) { return 0, f.err }

// AN OPENING KEEPS NO MORE THAN IT IS ASKED FOR: an [opening] written 32 MiB
// keeps the first n bytes, in a buffer within a small multiple of n, and counts
// every byte, so a stream into it holds what its stages hold and n.
//
// Mutation: append every write, and the opening holds 32 MiB.
func TestAnOpeningKeepsNoMoreThanItIsAskedFor(t *testing.T) {
	t.Parallel()
	const n, total = 100 << 10, 32 << 20
	o := &opening{keep: n}
	piece := make([]byte, pieceBytes)
	var want []byte
	for i := range total / pieceBytes {
		for j := range piece {
			piece[j] = byte(i + j%7)
		}
		if len(want) < n {
			want = append(want, piece[:min(len(piece), n-len(want))]...)
		}
		if k, err := o.Write(piece); k != len(piece) || err != nil {
			t.Fatalf("write %d answered %d, %v", i, k, err)
		}
	}
	if o.whole != total {
		t.Errorf("counted %d bytes of %d written", o.whole, total)
	}
	if !bytes.Equal(o.kept, want) {
		t.Errorf("kept %d bytes, not the first %d written", len(o.kept), n)
	}
	if cap(o.kept) > 3*n {
		t.Errorf("holds a buffer of %d bytes to keep %d", cap(o.kept), n)
	}
}

// LONGEST MARKER IS THE LONGEST THE PASS WRITES: [LongestMarker] is at least
// every rule's marker and a key's block's, and is one of them, so a reader
// that reads that far past a cut sees whether any marker crosses it.
//
// Mutation: answer the key's marker alone, shorter than a rule's.
func TestLongestMarkerIsTheLongestThePassWrites(t *testing.T) {
	t.Parallel()
	markers := append(slices.Clone(ruleMarkers()), keyMarker)
	got := LongestMarker()
	for _, m := range markers {
		if len(m) > got {
			t.Errorf("%q is %d bytes, longer than the %d LongestMarker answers", m, len(m), got)
		}
	}
	if !slices.ContainsFunc(markers, func(m string) bool { return len(m) == got }) {
		t.Errorf("LongestMarker answers %d, the length of no marker the pass writes", got)
	}
}
