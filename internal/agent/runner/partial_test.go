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

// The window's start clears exactly what a cut left of one character: the
// orphaned bytes go, and not one byte of the whole characters behind them.
func TestTheElisionClearsOnlyTheCharacterItCut(t *testing.T) {
	t.Parallel()
	// The window starts on the last byte of the three-byte rune.
	long := strings.Repeat("a", 10) + "あ" + strings.Repeat("b", partialTail-1)
	got := tail(long)
	if want := "…" + strings.Repeat("b", partialTail-1); got != want {
		t.Errorf("tail kept %d bytes, want %d: it must drop the cut character's orphaned "+
			"byte and nothing else", len(got), len(want))
	}
}

// A run of continuation bytes longer than one interrupted character can leave
// is the text's own, not a cut's residue, so the window keeps it: stripping it
// would claim a cut that never happened. The rule is
// textcut.TrimOrphanContinuation's, and this is what holds tail to it rather
// than to a copy of the walk that strips every continuation byte it meets.
func TestTheElisionKeepsBrokenBytesTheTextCarried(t *testing.T) {
	t.Parallel()
	own := strings.Repeat("\x80", utf8.UTFMax)
	long := strings.Repeat("a", 10) + own + strings.Repeat("b", partialTail-len(own))
	got := tail(long)
	if want := "…" + own + strings.Repeat("b", partialTail-len(own)); got != want {
		t.Errorf("tail kept %d bytes, want %d: the text's own bytes at the window's "+
			"start were stripped as if a cut had left them", len(got), len(want))
	}
}
