package runner

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The accumulated text is republished five times a second for a round's life,
// so unbounded it costs quadratic in the round's length. Not deltas: the
// socket hub drops the OLDEST frame when a client falls behind, and a
// consumer missing one would splice the rest into nonsense.
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

// A byte cut can split a character, putting a replacement glyph first on
// screen; on prose full of em dashes and quotes that is most cuts.
func TestTheElisionCutsOnACharacterNotAByte(t *testing.T) {
	t.Parallel()
	// Three-byte runes, so most byte offsets land mid-character.
	long := strings.Repeat("あ", partialTail)
	got := tail(long)
	if !utf8.ValidString(got) {
		t.Errorf("the elided partial is not valid UTF-8: %q", got[:16])
	}
}
