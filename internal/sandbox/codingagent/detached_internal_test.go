package codingagent

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// A coding run's error and its unparseable output are the run's ONLY account
// of itself, and both come from files nothing bounds — the CLI's own stderr
// and stdout redirects. They are TAILED, not head-cut: a crash explains itself
// at the bottom, which is exactly what the 500- and 2000-byte head cuts these
// replaced threw away.
func TestACodingRunsFailureTextKeepsItsEndAndSaysItCut(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("noise\n", OutputTailBytes) + "FATAL: migrations/0007.sql is missing"

	got := tail(huge)
	if len(got) > OutputTailBytes+len(outputCutMarker) {
		t.Errorf("a %d-byte tail, past its %d-byte bound", len(got), OutputTailBytes)
	}
	if !strings.HasPrefix(got, outputCutMarker) {
		t.Error("the cut is silent")
	}
	if !strings.HasSuffix(got, "FATAL: migrations/0007.sql is missing") {
		t.Error("the tail was not kept, so the line naming the failure is gone")
	}
	// Within the bound, untouched — the bound is for pathological input, and
	// an output of exactly the bound is not one.
	for _, in := range []string{"short", strings.Repeat("x", OutputTailBytes)} {
		if got := tail(in); got != in {
			t.Errorf("a %d-byte output, within the %d-byte bound, was altered",
				len(in), OutputTailBytes)
		}
	}
}

// The cut lands wherever the text happens to be long, so it must leave a
// whole character at the front. A byte offset does not: it opens the text with
// part of one, which a JSON encoder turns into U+FFFD — so whoever reads a
// failed run sees mojibake where the run's own output began.
func TestATailedOutputStartsOnAWholeCharacter(t *testing.T) {
	t.Parallel()
	// Three bytes a character and 0, 1 or 2 ASCII bytes after them, so the
	// cut's offset takes all three alignments — two of them inside a
	// character.
	for pad := range 3 {
		t.Run(fmt.Sprintf("%d ASCII bytes at the end", pad), func(t *testing.T) {
			t.Parallel()
			end := strings.Repeat("z", pad)
			got := tail(strings.Repeat("日", OutputTailBytes) + end)

			body, cut := strings.CutPrefix(got, outputCutMarker)
			if !cut {
				t.Fatal("the cut is silent")
			}
			if !utf8.ValidString(body) {
				t.Fatalf("the kept text opens with part of a character: %q", body[:utf8.UTFMax])
			}
			// Only the interrupted character goes, never a whole one.
			if len(body) < OutputTailBytes-(utf8.UTFMax-1) {
				t.Errorf("kept %d bytes of a %d-byte bound: more than the "+
					"interrupted character was dropped", len(body), OutputTailBytes)
			}
			if !strings.HasSuffix(body, "日"+end) {
				t.Error("the end of the output was not kept")
			}
		})
	}
}

// Unparseable output is the case where the text IS the result: there is no
// structured field to fall back to, and Collect's crash detail is skipped
// because it is guarded on Text being empty.
func TestUnparseableCodingOutputIsTailedNotDropped(t *testing.T) {
	t.Parallel()
	res := ClaudeCode{}.Parse(strings.Repeat("banner\n", OutputTailBytes) + "Error: token expired")
	if res.Error == "" {
		t.Error("unparseable output did not report itself as unparseable")
	}
	if !strings.HasSuffix(res.Text, "Error: token expired") {
		t.Error("the end of the output — where the error is — was cut away")
	}
	if len(res.Text) > OutputTailBytes+len(outputCutMarker) {
		t.Errorf("the carried text is %d bytes, past the %d-byte bound",
			len(res.Text), OutputTailBytes)
	}
	if !strings.HasPrefix(res.Text, outputCutMarker) {
		t.Error("the cut is silent")
	}
}
