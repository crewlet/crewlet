package sandbox

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A STREAM'S END IS SHOWN IN WHOLE LINES: the window starts on a line, keeps a
// line its byte count landed exactly at the start of, and only a single line
// longer than the window keeps its own end — on a character, marked, with the
// mark not counted in the offset.
func TestTheEndOfATextIsKeptInWholeLines(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		text   string
		keep   int
		want   string
		wantAt int
	}{
		"fits":                {"a\nb\n", 10, "a\nb\n", 0},
		"opens mid-line":      {"first\nsecond\nthird\n", 10, "third\n", 13},
		"lands on a line":     {"first\nsecond\n", 7, "second\n", 6},
		"one line too long":   {"x" + strings.Repeat("é", 10), 6, "…ééé", 15},
		"a last line too big": {"ok\n" + strings.Repeat("z", 20), 5, "…zzzzz", 18},
	} {
		got, at := KeepEnd(c.text, c.keep)
		if got != c.want || at != c.wantAt {
			t.Errorf("%s: KeepEnd = %q at %d; want %q at %d", name, got, at, c.want, c.wantAt)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: kept %q, which is not valid UTF-8", name, got)
		}
		if kept := strings.TrimPrefix(got, "…"); c.text[at:] != kept {
			t.Errorf("%s: the offset %d does not say where %q begins", name, at, kept)
		}
	}
}
