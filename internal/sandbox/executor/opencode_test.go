package executor

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact/redacttest"
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

// UNPARSEABLE OUTPUT IS ITS FAILURE'S DETAIL, AT MOST ITS END: whole within
// [unparsedShownBytes], since its actual error comes at the END, after the
// banner; past it, that end redacted and the output's size, never the whole,
// which would ride the failure uncut ([failureDetail]); and an end that is one
// line begun before it is none of it shown, never a cut of that line.
//
// Mutations, each red: quote the whole output past the bound; keep its start
// rather than its end; leave the end unredacted; leave the size unsaid; show a
// cut of one long line.
func TestUnparseableOutputIsItsFailuresDetailAtMostItsEnd(t *testing.T) {
	t.Parallel()
	const opening = "the harness's output could not be parsed"
	short := strings.Repeat("banner\n", 1000) + "Error: token expired"
	if got := (ClaudeCode{}).Parse([]byte(short)).Error; got != opening+":\n"+short {
		t.Errorf("output within the bound reads %.80q..., want it whole", got)
	}

	var key string
	for _, s := range redacttest.Shapes() {
		if s.Name == "anthropic-key" {
			key = s.Sample
		}
	}
	long := strings.Repeat("banner\n", 2*unparsedShownBytes/len("banner\n")) + "the key was " + key +
		"\nError: token expired"
	res := (ClaudeCode{}).Parse([]byte(long))
	switch got := res.Error; {
	case len(got) > unparsedShownBytes+len(opening)+64:
		t.Errorf("output of %d bytes left a failure of %d, past the %d its end is shown within",
			len(long), len(got), unparsedShownBytes)
	case !strings.HasPrefix(got, opening+" ("+humanSize(int64(len(long)))+"; its end shown):\n"):
		t.Errorf("the failure opens %.120q, want the output's size said", got)
	case !strings.HasSuffix(got, "\nError: token expired"):
		t.Errorf("the failure ends %q, want the output's end", got[max(0, len(got)-80):])
	case strings.Contains(got, key):
		t.Error("the failure shows a credential the output's end held")
	}
	if res.Text != "" || res.Success {
		t.Errorf("unparseable output read as a report (text %d bytes, success %v)", len(res.Text), res.Success)
	}

	line := strings.Repeat("x", 2*unparsedShownBytes)
	if got := (ClaudeCode{}).Parse([]byte(line)).Error; len(got) > 256 || strings.Contains(got, "xxx") {
		t.Errorf("one long line left the failure %.120q (%d bytes), want only its size said", got, len(got))
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
			if got := (OpenCode{}).Parse([]byte(c.stream)); got.ContextExhausted != c.want {
				t.Errorf("ContextExhausted = %v, want %v, on %s", got.ContextExhausted, c.want, c.stream)
			}
		})
	}
}
