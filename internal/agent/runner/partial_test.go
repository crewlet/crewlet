package runner

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The accumulated text is republished five times a second for the life of a
// round, so its cost is quadratic in the round's length. Deltas cannot be sent
// instead — the socket hub drops the OLDEST frame when a client falls behind,
// and a consumer that missed one would splice the rest into nonsense — so the
// accumulation is bounded instead.
func TestALongRoundDoesNotSendItsWholeSelfEveryTime(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", partialTail*3)
	got := tail(long)
	if len(got) > partialTail+len("…") {
		t.Errorf("sent %d bytes for a %d-byte round; want it bounded", len(got), len(long))
	}
	if !strings.HasPrefix(got, "…") {
		t.Error("an elided partial does not say it was elided")
	}
	// The TAIL, because that is where text appears.
	if !strings.HasSuffix(got, "a") {
		t.Error("the elision kept the head; a reader watches the end")
	}
}

func TestAShortRoundIsSentWhole(t *testing.T) {
	t.Parallel()
	if got := tail("hello"); got != "hello" {
		t.Errorf("tail(%q) = %q — a round under the cap must not be touched", "hello", got)
	}
}

// Slicing UTF-8 by bytes can cut a character in half, and the replacement
// glyph would be the first thing on screen every time the cut landed
// mid-character — which, on a reasoning trace full of em dashes and quotes,
// is most of the time.
func TestTheElisionCutsOnACharacterNotAByte(t *testing.T) {
	t.Parallel()
	// Three-byte runes, so most byte offsets land mid-character.
	long := strings.Repeat("あ", partialTail)
	got := tail(long)
	if !utf8.ValidString(got) {
		t.Errorf("the elided partial is not valid UTF-8: %q", got[:16])
	}
}
