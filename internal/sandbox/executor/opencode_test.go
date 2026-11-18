package executor

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A transcript line stays within its bound in BYTES, marker included, and is
// never cut through a rune: a byte slice plus the three-byte ellipsis overshoots
// the bound and can split a character, putting invalid UTF-8 into the event store.
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

	// A short line is untouched; only the FIRST line is taken (the transcript
	// is line-structured), but the lines after it are COUNTED, so a heredoc
	// does not read as the one command on its first line.
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

	// Degenerate limits must not panic: `limit - len("…")` is negative under
	// 3, on a path that runs only while a run is already failing.
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

// Unparseable output is carried WHOLE, as the failure's detail rather than a
// report: its actual error comes at the END, after the banner, and the
// coordinator condenses an oversized failure keeping its cause, so a head or
// tail cut here would decide by size what survives.
func TestUnparseableCodingOutputIsCarriedWholeAsTheFailure(t *testing.T) {
	t.Parallel()
	out := strings.Repeat("banner\n", sandbox.MaxRunTextBytes) + "Error: token expired"
	res := ClaudeCode{}.Parse(out)
	if !strings.HasPrefix(res.Error, "the harness's output could not be parsed") {
		t.Errorf("unparseable output did not report itself as unparseable: %.80q", res.Error)
	}
	if !strings.HasSuffix(res.Error, "\n"+out) {
		t.Error("the output was not carried whole")
	}
	if res.Text != "" || res.Success {
		t.Errorf("unparseable output read as a report (text %d bytes, success %v)", len(res.Text), res.Success)
	}
}

// OPENCODE'S OWN NAME FOR A CONVERSATION PAST ITS CONTEXT is what tells such a
// run from another failed one, never the sentence, which a tool's output can
// repeat; and a later error that names another cause replaces it.
func TestAContextOverflowIsToldByTheErrorsName(t *testing.T) {
	t.Parallel()
	overflow := `{"type":"error","error":{"name":"ContextOverflowError","data":{"message":"too long"}}}`
	other := `{"type":"error","error":{"name":"UnknownError","data":{"message":"ContextOverflowError in a tool"}}}`
	for _, c := range []struct {
		name, stream string
		want         bool
	}{
		{"its name", overflow, true},
		{"its words under another name", other, false},
		{"the last error decides", overflow + "\n" + other, false},
		{"a text error", `{"type":"error","error":"ContextOverflowError"}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := (OpenCode{}).Parse(c.stream); got.ContextExhausted != c.want {
				t.Errorf("ContextExhausted = %v, want %v, on %s", got.ContextExhausted, c.want, c.stream)
			}
		})
	}
}
