package textcut_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/textcut"
)

// A CUT NEVER PRODUCES INVALID UTF-8. This is the rule nine helpers were
// re-deriving and four of them had never learned: a plain s[:n] splits
// whatever multi-byte character straddles the boundary, and what that
// produces depends on where it goes — a JSON encoder substitutes U+FFFD, a
// model reads a replacement character, a vendor rejects the field.
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
