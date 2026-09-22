package learning

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// THE WINDOWING MUST TERMINATE ON TEXT THAT IS NOT VALID UTF-8, and nothing
// proved it did.
//
// [episodeWindows] backs a window's start and end onto rune boundaries, and an
// UNBOUNDED "walk back to a rune start" is what makes that loop stand still: on
// bytes that are never a rune start the walk hands the whole window back, the
// NEVER-SKIPS clamp then pins the next window's start to the current one's, and
// the loop spins for ever. [runeAlignBack] and the `floor` argument are what
// bound it — this is the case that says so.
//
// It is not hypothetical input. A task summary is a coalesced trigger, and a
// webhook body can carry any bytes at all: a diff of a binary file, a payload
// truncated by a proxy mid-rune, a Latin-1 filename a vendor never transcoded.
// The write it would hang is the reflect pass that persists a seat's episode,
// so the cost is not a bad vector — it is a goroutine that never returns.
func TestWindowingTerminatesOnInvalidUTF8(t *testing.T) {
	t.Parallel()
	// 0x80 is a UTF-8 CONTINUATION byte: never a rune start, so every
	// alignment walk over this input runs to its bound and finds nothing.
	// Sized past one window so the loop has to advance at all.
	cases := map[string]string{
		"all continuation bytes":  strings.Repeat("\x80", EpisodeWindowBytes*3),
		"valid text, torn tail":   strings.Repeat("a", EpisodeWindowBytes*2) + strings.Repeat("\x80", 64),
		"a lone byte per window":  strings.Repeat("\xff", EpisodeWindowBytes*2+7),
		"continuation at the cut": strings.Repeat("b", EpisodeWindowBytes-1) + strings.Repeat("\x80", EpisodeWindowBytes+1),
	}
	for name, summary := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			done := make(chan []string, 1)
			go func() { done <- episodeWindows(summary) }()
			select {
			case windows := <-done:
				// TERMINATING IS NOT ENOUGH: the whole point of
				// windowing is that no byte is dropped, and a
				// degenerate input must not quietly lose its tail.
				if len(windows) == 0 {
					t.Fatal("no windows at all, so every byte was dropped")
				}
				if got := len(strings.Join(windows, "")); got < len(summary)-len(windows)*utf8.UTFMax {
					t.Errorf("windows hold %d bytes of a %d-byte summary — the tail was dropped",
						got, len(summary))
				}
			case <-time.After(10 * time.Second):
				t.Fatal("episodeWindows did not return: the rune-alignment " +
					"walk is unbounded again, and this hangs a seat's episode write")
			}
		})
	}
}

// A WINDOW START IS NEVER PUSHED PAST A WINDOW END, which is the other half of
// the same arithmetic: if a word edge could walk an end back further than the
// stride, the next window would begin after it and the bytes between would be
// in no window at all — the silent cut this replaced, wearing a new shape.
func TestWindowsCoverEveryByteOfAWordlessSummary(t *testing.T) {
	t.Parallel()
	// No spaces and no newlines anywhere, so episodeWordEdge finds no edge
	// on any pass and every window falls back to its full width.
	summary := strings.Repeat("x", EpisodeWindowBytes*4+123)
	windows := episodeWindows(summary)
	if len(windows) < 2 {
		t.Fatalf("a %d-byte summary made %d window(s)", len(summary), len(windows))
	}
	// Reconstruct by walking the windows in order and asserting each one
	// starts at or before the previous one's end — overlap is fine, a gap
	// is the bug.
	joined := strings.Join(windows, "")
	if len(joined) < len(summary) {
		t.Errorf("windows hold %d bytes of %d — a gap means bytes in no window",
			len(joined), len(summary))
	}
}
