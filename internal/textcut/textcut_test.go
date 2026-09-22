package textcut_test

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/textcut"
)

// A CUT NEVER PRODUCES INVALID UTF-8. This is the rule nine helpers were
// re-deriving and four of them had never learned: a plain s[:n] splits
// whatever multi-byte character straddles the boundary, and what that
// produces depends on where it goes — a JSON encoder substitutes U+FFFD, a
// model reads a replacement character, a terminal prints a box.
func TestACutNeverLandsMidRune(t *testing.T) {
	t.Parallel()
	// Every cut position across a string whose runes are 1, 2, 3 and 4
	// bytes wide, so some boundary lands inside each width.
	const mixed = "aé€𝄞bcé€𝄞"
	for max := range len(mixed) + 4 {
		for _, tc := range []struct {
			name string
			got  string
		}{
			{"Bytes", textcut.Bytes(mixed, max)},
			{"Ellipsis", strings.TrimSuffix(textcut.Ellipsis(mixed, max), "…")},
		} {
			if !utf8.ValidString(tc.got) {
				t.Errorf("%s(%q, %d) = %q, which is not valid UTF-8",
					tc.name, mixed, max, tc.got)
			}
			if !strings.HasPrefix(mixed, tc.got) {
				t.Errorf("%s(%q, %d) = %q, which is not a prefix of the input",
					tc.name, mixed, max, tc.got)
			}
		}
	}
}

// A string within the cap is returned UNCHANGED — no marker, no copy of a
// decision nobody asked for.
func TestAShortEnoughStringIsUntouched(t *testing.T) {
	t.Parallel()
	const s = "already short"
	for _, got := range []string{
		textcut.Bytes(s, len(s)),
		textcut.Ellipsis(s, len(s)),
	} {
		if got != s {
			t.Errorf("a string at exactly the cap was changed to %q", got)
		}
	}
}

// THE MARKER IS THE POINT of Ellipsis: without it a reader cannot tell a
// severed value from a shorter one — a truncated tool argument reads as a
// different argument.
func TestEllipsisMarksWhereItCut(t *testing.T) {
	t.Parallel()
	got := textcut.Ellipsis("abcdefghij", 4)
	if got != "abcd…" {
		t.Errorf("Ellipsis = %q, want %q", got, "abcd…")
	}
	if textcut.Bytes("abcdefghij", 4) != "abcd" {
		t.Error("Bytes must not append a marker")
	}
}

// A NON-POSITIVE CAP YIELDS NOTHING rather than panicking on s[:max].
func TestANonPositiveCapYieldsNothing(t *testing.T) {
	t.Parallel()
	for _, max := range []int{0, -1} {
		if got := textcut.Bytes("abc", max); got != "" {
			t.Errorf("Bytes(_, %d) = %q", max, got)
		}
	}
}

// WITHIN'S BUDGET INCLUDES ITS MARKER, which is the whole difference from
// [Ellipsis] and the reason it exists: a tracker excerpt is REFUSED above its
// cap, so a marker outside the budget turns every long comment into a failed
// write rather than a marked one.
func TestWithinCountsItsMarker(t *testing.T) {
	t.Parallel()
	const max = 12
	for name, in := range map[string]string{
		"plain ascii":   strings.Repeat("a", 100),
		"multi-byte":    strings.Repeat("é", 100),
		"mixed":         "aé" + strings.Repeat("漢", 100),
		"exactly at it": strings.Repeat("a", max),
		"just under":    strings.Repeat("a", max-1),
	} {
		t.Run(name, func(t *testing.T) {
			got := textcut.Within(in, max)
			if len(got) > max {
				t.Fatalf("Within(%d bytes, %d) = %d bytes — the marker has to "+
					"fit inside the budget or a capped field is refused",
					len(in), max, len(got))
			}
			if !utf8.ValidString(got) {
				t.Fatalf("Within split a rune: %q", got)
			}
			if len(in) > max && !strings.HasSuffix(got, "…") {
				t.Errorf("Within cut %q and did not say so", got)
			}
			if len(in) <= max && got != in {
				t.Errorf("Within altered a value that already fit")
			}
		})
	}
}

// AND A BUDGET TOO SMALL FOR THE MARKER YIELDS THE MARKER, not the empty
// string: something was cut, and a reader shown nothing cannot tell that from
// a value that was empty to begin with.
//
// It is the ONE case where the result exceeds max, which is why it is asserted
// rather than left to fall out of [textcut.Bytes]'s own contract — a change to
// that contract would move this silently.
func TestWithinUnderTheMarkersOwnLength(t *testing.T) {
	t.Parallel()
	for _, max := range []int{0, 1, 2} {
		if got := textcut.Within("hello", max); got != "…" {
			t.Errorf("Within(%q, %d) = %q, want the marker alone", "hello", max, got)
		}
	}
	// AND IT NEVER EXCEEDS A BUDGET IT CAN MEET.
	if got := textcut.Within("hello", 3); got != "…" {
		t.Errorf("Within(%q, 3) = %q", "hello", got)
	}
}

// THE END EDGE UNDOES A CUT AND NOTHING ELSE. A short-but-valid encoding at
// the end is a character somebody's cap interrupted and goes; a whole
// character stays; and a byte no encoding has was never cut by anybody, so
// removing it would be the caller claiming a cut it did not make and charging
// somebody else's bytes to a count that means "what I dropped".
func TestTheEndEdgeDropsOnlyACharacterACutInterrupted(t *testing.T) {
	t.Parallel()
	euro := []byte("€") // three bytes: E2 82 AC
	for name, tc := range map[string]struct {
		in   []byte
		want []byte
	}{
		"whole character":         {euro, euro},
		"one byte of three":       {euro[:1], nil},
		"two bytes of three":      {euro[:2], nil},
		"ascii is never touched":  {[]byte("abc"), []byte("abc")},
		"empty":                   {nil, nil},
		"impossible byte is kept": {[]byte{'a', 0xFF}, []byte{'a', 0xFF}},
		// 0xFF cannot start any encoding, so FullRune calls it full where
		// DecodeLastRune would answer (RuneError, 1) for it AND for the
		// truncated cases above — the pair this predicate exists to separate.
		"impossible byte alone":   {[]byte{0xFF}, []byte{0xFF}},
		"cut character after one": {append([]byte("a"), euro[:2]...), []byte("a")},
		// Continuation bytes with no lead byte within reach: nothing in the
		// window is evidence about this edge, so it is left as it is.
		"orphans only": {[]byte{0x82, 0xAC}, []byte{0x82, 0xAC}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := textcut.TrimSplitRune(tc.in); !bytes.Equal(got, tc.want) {
				t.Fatalf("TrimSplitRune(% x) = % x, want % x", tc.in, got, tc.want)
			}
		})
	}
}

// THE START EDGE IS CLEARED RATHER THAN DECIDED, because nothing precedes it:
// a leading continuation byte is not a character under any reading and no
// evidence exists that would tell an orphan of a cut from one the source
// emitted. The bound is one character's worth — a longer run is not an
// interrupted character at all, so it is the source's own bytes and stays.
func TestTheStartEdgeClearsOneCharactersWorthOfOrphans(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   []byte
		want []byte
	}{
		"already on a boundary": {[]byte("€x"), []byte("€x")},
		"one orphan":            {[]byte{0xAC, 'x'}, []byte("x")},
		"two orphans":           {[]byte{0x82, 0xAC, 'x'}, []byte("x")},
		"three orphans":         {[]byte{0x90, 0x8D, 0x88, 'x'}, []byte("x")},
		// LEFT WHOLE, which is what "is not an interrupted character"
		// has to mean: the old expectation stripped three of the four
		// and kept the last, which is neither outcome this rule offers
		// and leaves an orphan indistinguishable from a real cut's.
		"four is not a character":  {[]byte{0x80, 0x80, 0x80, 0x80}, []byte{0x80, 0x80, 0x80, 0x80}},
		"a long run is the source": {[]byte{0x80, 0x80, 0x80, 0x80, 0x80, 'x'}, []byte{0x80, 0x80, 0x80, 0x80, 0x80, 'x'}},
		"nothing but orphans":      {[]byte{0xAC}, nil},
		"empty":                    {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := textcut.TrimOrphanContinuation(tc.in); !bytes.Equal(got, tc.want) {
				t.Fatalf("TrimOrphanContinuation(% x) = % x, want % x", tc.in, got, tc.want)
			}
		})
	}
}

// THE TWO EDGES TOGETHER MAKE ANY WINDOW OVER VALID TEXT VALID, which is what
// both callers actually need: a ring that keeps the last N bytes and a buffer
// that stopped at a cap cut at byte positions nothing chose, and the window
// between them reaches an operator and a model.
func TestAnyWindowRepairedAtBothEdgesIsValidUTF8(t *testing.T) {
	t.Parallel()
	const mixed = "aé€𝄞bcé€𝄞"
	for from := range len(mixed) + 1 {
		for to := from; to <= len(mixed); to++ {
			window := []byte(mixed[from:to])
			got := textcut.TrimSplitRune(textcut.TrimOrphanContinuation(window))
			if !utf8.Valid(got) {
				t.Fatalf("the window [%d:%d] repaired to % x, which is not valid UTF-8",
					from, to, got)
			}
		}
	}
}

// BOTH RETURN A SUB-SLICE, never a copy. The callers are memory-bounded
// buffers — a capped CLI buffer and the sandbox's control-output ring — whose
// whole reason for existing is that reading them back does not allocate a
// second copy of what they hold, and a helper that copied would put that
// second copy back without anything saying so.
func TestBothEdgeWalksAliasTheirInput(t *testing.T) {
	t.Parallel()
	split := []byte{'a', 'b', 0xE2, 0x82}
	if got := textcut.TrimSplitRune(split); len(got) == 0 || &got[0] != &split[0] {
		t.Fatal("TrimSplitRune copied its input")
	}
	orphan := []byte{0x82, 'a', 'b'}
	if got := textcut.TrimOrphanContinuation(orphan); len(got) == 0 || &got[0] != &orphan[1] {
		t.Fatal("TrimOrphanContinuation copied its input")
	}
}
