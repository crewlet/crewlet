package codingagent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The transcript line's bound keeps its cap and its marker; what it used not
// to keep is its own arithmetic. `line[:limit-1] + "…"` emits limit+2 BYTES —
// limit-1 of content plus a three-byte ellipsis — so the constant bounded
// nothing it named, and the byte slice split whatever multi-byte character
// straddled the cut, putting invalid UTF-8 into the event store.
func TestATranscriptLineHonoursItsOwnBound(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 400)
	got := firstLine(long, transcriptDetailLimit)
	if len(got) > transcriptDetailLimit {
		t.Errorf("a %d-byte line, past the %d-byte bound it names",
			len(got), transcriptDetailLimit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("the cut is unmarked: %q", got)
	}

	// A short line is untouched, and only the FIRST line is taken — the
	// transcript is line-structured and a spilled entry reads as two.
	if got := firstLine("git status\nand more", transcriptDetailLimit); got != "git status" {
		t.Errorf("firstLine = %q", got)
	}

	// Never through a rune.
	if got := firstLine(strings.Repeat("日本語", 200), transcriptDetailLimit); !utf8.ValidString(got) {
		t.Errorf("a non-ASCII command was cut through a rune")
	}

	// Degenerate limits must not panic: `limit - len("…")` is negative for
	// anything under 3, and a negative slice index is a crash on a path
	// that only ever runs while a coding run is already failing.
	for _, limit := range []int{-1, 0, 1, 2, 3} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("firstLine panicked at limit %d: %v", limit, r)
				}
			}()
			firstLine(long, limit)
		}()
	}
}

// Unparseable output is the case where the text IS the result: there is no
// structured field to fall back to, and Collect's own tailing fallback is
// skipped because it is guarded on Text being empty.
func TestUnparseableCodingOutputIsTailedNotDropped(t *testing.T) {
	t.Parallel()
	res := ClaudeCode{}.Parse(strings.Repeat("banner\n", MaxTranscriptBytes) + "Error: token expired")
	if res.Error == "" {
		t.Error("unparseable output did not report itself as unparseable")
	}
	if !strings.HasSuffix(res.Text, "Error: token expired") {
		t.Error("the end of the output — where the error is — was cut away")
	}
	if len(res.Text) > MaxTranscriptBytes+len("…") {
		t.Errorf("the carried text is %d bytes, unbounded", len(res.Text))
	}
}
