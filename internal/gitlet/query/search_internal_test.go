package query

import (
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/textcut"
)

// hitTexts are lines whose answered text hitText must spell as the search
// always has: ASCII, multi-byte runes at the cut, runs of invalid bytes, a NUL,
// an empty line and a line of a mebibyte.
var hitTexts = [][]byte{
	[]byte("status is ok"),
	nil,
	[]byte("a\x00b"),
	[]byte("\xff\xfe status \xc3"),
	[]byte("\xef\xbf\xbd\xff valid replacement beside an invalid byte"),
	[]byte(strings.Repeat("é", 200)),
	[]byte(strings.Repeat("x", 252) + "日本語"),
	[]byte(strings.Repeat("x", 253) + "日本"),
	[]byte(strings.Repeat("x", 256)),
	[]byte(strings.Repeat("x", 257)),
	[]byte(strings.Repeat("\xff", 300) + "status"),
	[]byte(strings.Repeat("\xffa", 200)),
	[]byte(strings.Repeat("y", 1<<20)),
}

// A HIT'S TEXT IS THE SAME AS BEFORE: hitText, which maps match offsets into
// the text it answers, answers the very text the search answered before it
// (strings.ToValidUTF8 with U+FFFD, then textcut.Within at
// gitlet.MaxSearchLineBytes), so offsets index the text a reader holds.
//
// Mutation: replace each invalid byte rather than each run, red.
func TestAHitsTextIsTheSameAsBefore(t *testing.T) {
	t.Parallel()
	for _, raw := range hitTexts {
		if got, want := hitTextBefore(raw); got != want {
			t.Errorf("hitText(%.40q) = %.60q, want %.60q", raw, got, want)
		}
	}
}

// hitTextBefore is hitText's text beside the rule it replaced.
func hitTextBefore(raw []byte) (string, string) {
	got, _ := hitText(raw, nil)
	return got, textcut.Within(strings.ToValidUTF8(string(raw), "�"), gitlet.MaxSearchLineBytes)
}

// FuzzHitText holds hitText's text to the rule it replaced, and its offsets
// inside it, on any line.
func FuzzHitText(f *testing.F) {
	for _, raw := range hitTexts {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if got, want := hitTextBefore(raw); got != want {
			t.Fatalf("hitText(%q) = %q, want %q", raw, got, want)
		}
		text, ms := hitText(raw, func(within int) [][2]int {
			var all [][2]int
			for i := 0; i+1 <= within && i+1 <= len(raw); i += 2 {
				all = append(all, [2]int{i, i + 1})
			}
			return all
		})
		end := 0
		for _, m := range ms {
			if m[0] < end || m[1] <= m[0] || m[1] > len(text) {
				t.Fatalf("hitText(%q) answered %v over %q: out of order, empty or past the text", raw, ms, text)
			}
			end = m[1]
		}
	})
}

// A HIT NAMES WHERE ITS LINE MATCHED, IN ITS TEXT AS ANSWERED: offsets of the
// raw line move with the replacement of a run of invalid bytes, are clipped
// at the text's cut, dropped past it or where empty, and stop at
// gitlet.MaxSearchHitMatches.
//
// Mutation: map no offsets through the invalid run (answer the raw ones),
// or keep the matches past the cut: red.
func TestAHitsMatchesAreOffsetsIntoItsText(t *testing.T) {
	t.Parallel()
	ranges := func(rs ...[2]int) func(int) [][2]int {
		return func(within int) [][2]int {
			var out [][2]int
			for _, r := range rs {
				if r[0] < within {
					out = append(out, r)
				}
			}
			return out
		}
	}
	long := strings.Repeat("x", 251) + "abcdefgh"
	many := make([][2]int, 40)
	for i := range many {
		many[i] = [2]int{3 * i, 3*i + 3}
	}
	for _, tc := range []struct {
		name string
		raw  string
		ms   func(int) [][2]int
		want [][2]int
	}{
		{"twice", "abc xyz abc", ranges([2]int{0, 3}, [2]int{8, 11}), [][2]int{{0, 3}, {8, 11}}},
		{"after an invalid run", "\xff\xfe abc", ranges([2]int{3, 6}), [][2]int{{4, 7}}},
		{"inside an invalid run", "a\xff\xfe\xfdb", ranges([2]int{2, 3}), [][2]int{{1, 4}}},
		{"straddling the cut", long, ranges([2]int{251, 254}), [][2]int{{251, 253}}},
		{"past the cut", long, ranges([2]int{255, 258}), nil},
		{"empty", "abc", ranges([2]int{1, 1}), nil},
		{"forty", strings.Repeat("abc", 40), ranges(many...), many[:gitlet.MaxSearchHitMatches]},
	} {
		_, got := hitText([]byte(tc.raw), tc.ms)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: matches %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A FILE THE SEARCH STOPPED IN NAMES NO TOTAL: a per-file search answers each
// hit's FileMatches only of a file it confirmed to its end, so a file its
// budget or its limit cut part way names none rather than a count short of
// the file's.
//
// Mutation: set FileMatches whatever the file's cut (flush), red.
func TestAFileTheSearchStoppedInNamesNoTotal(t *testing.T) {
	t.Parallel()
	for _, cut := range []bool{false, true} {
		s := &searching{perFile: 2, limit: 10, room: 1 << 20, answer: gitlet.SearchAnswer{Hits: []gitlet.SearchHit{}}}
		s.file = pendingFile{path: "a", matched: 5, cut: cut,
			kept: []gitlet.SearchHit{{Path: "a", Line: 1, Text: "x"}, {Path: "a", Line: 2, Text: "x"}}}
		s.flush(nil)
		want := 5
		if cut {
			want = 0
		}
		for _, h := range s.answer.Hits {
			if h.FileMatches != want {
				t.Errorf("a file cut %t answered file_matches %d, want %d", cut, h.FileMatches, want)
			}
		}
		if len(s.answer.Hits) != 2 {
			t.Errorf("a file cut %t answered %d hits, want its 2", cut, len(s.answer.Hits))
		}
	}
}
