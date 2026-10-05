package codingagent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox"
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
	// transcript is line-structured and a spilled entry reads as two — but
	// the lines after it are COUNTED, so a heredoc does not read as the one
	// command on its first line.
	if got := firstLine("git status", transcriptDetailLimit); got != "git status" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("cat <<EOF\nline\nEOF", transcriptDetailLimit); got != "cat <<EOF (+2 more line(s))" {
		t.Errorf("firstLine = %q, want the later lines counted", got)
	}
	if got := firstLine(long+"\nmore", transcriptDetailLimit); len(got) > transcriptDetailLimit ||
		!strings.HasSuffix(got, "… (+1 more line(s))") {
		t.Errorf("a long first line with more after it = %q (%d bytes)", got, len(got))
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

// Unparseable output is the case where the text IS the account: there is no
// structured field to fall back to, and the useful part of it — the actual
// error, after the banner — is at the END. It is carried WHOLE, as the
// failure's detail rather than as a report, because the coordinator condenses
// a failure past the record's bound keeping its cause, and a head cut or a
// tail cut here would decide by size what survives.
func TestUnparseableCodingOutputIsCarriedWholeAsTheFailure(t *testing.T) {
	t.Parallel()
	out := strings.Repeat("banner\n", sandbox.MaxRunTextBytes) + "Error: token expired"
	res := ClaudeCode{}.Parse(out)
	if !strings.HasPrefix(res.Error, "the coding agent's output could not be parsed") {
		t.Errorf("unparseable output did not report itself as unparseable: %.80q", res.Error)
	}
	if !strings.HasSuffix(res.Error, "\n"+out) {
		t.Error("the output was not carried whole")
	}
	if res.Text != "" || res.Success {
		t.Errorf("unparseable output read as a report (text %d bytes, success %v)", len(res.Text), res.Success)
	}
}
