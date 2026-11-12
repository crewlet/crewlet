package gitlet_test

import (
	"math"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// REANCHOR FOLLOWS HUNKS. Lines are placed by the hunks of the diff between
// the blob they were read in and the file now — moved by what changed above
// them, left be by what changed below — and called outdated where a hunk
// replaced, deleted or split them, never placed on lines that only look the
// same. An insertion right above or right below a range touches none of its
// lines.
//
// Mutations: let a hunk that overlaps a range move it rather than outdate it —
// the replaced and deleted rows red; count an insertion inside a range as
// above it — the split row red; shift by a hunk below the range — the below
// rows red.
func TestReanchorFollowsHunks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		hunks      []gitlet.Hunk
		start, end int
		wantStart  int
		wantEnd    int
		current    bool
	}{
		{"no hunk", nil, 3, 5, 3, 5, true},
		{"two lines inserted at the top", []gitlet.Hunk{hunk(0, 0, 1, 2)}, 3, 5, 5, 7, true},
		{"an insertion right above", []gitlet.Hunk{hunk(2, 0, 3, 4)}, 3, 5, 7, 9, true},
		{"an insertion right below", []gitlet.Hunk{hunk(5, 0, 6, 4)}, 3, 5, 3, 5, true},
		{"an insertion inside splits it", []gitlet.Hunk{hunk(3, 0, 4, 1)}, 3, 5, 3, 5, false},
		{"a line above deleted", []gitlet.Hunk{hunk(1, 1, 0, 0)}, 3, 5, 2, 4, true},
		{"three lines above made one", []gitlet.Hunk{hunk(1, 2, 1, 1)}, 3, 5, 2, 4, true},
		{"a line of it replaced", []gitlet.Hunk{hunk(4, 1, 4, 1)}, 3, 5, 3, 5, false},
		{"its first line deleted", []gitlet.Hunk{hunk(3, 1, 2, 0)}, 3, 5, 3, 5, false},
		{"a hunk across its start", []gitlet.Hunk{hunk(2, 2, 2, 3)}, 3, 5, 3, 5, false},
		{"a hunk across its end", []gitlet.Hunk{hunk(5, 3, 5, 1)}, 3, 5, 3, 5, false},
		{"a hunk below", []gitlet.Hunk{hunk(6, 1, 6, 9)}, 3, 5, 3, 5, true},
		{"above and below", []gitlet.Hunk{hunk(1, 1, 1, 3), hunk(9, 2, 11, 0)}, 3, 5, 5, 7, true},
		{"above, then over it", []gitlet.Hunk{hunk(0, 0, 1, 1), hunk(4, 1, 5, 2)}, 3, 5, 4, 6, false},
		{"one line, moved", []gitlet.Hunk{hunk(1, 0, 2, 1)}, 7, 7, 8, 8, true},
		{"the last line a file holds, moved", []gitlet.Hunk{hunk(0, 0, 1, 2)}, gitlet.MaxAnchorLine, gitlet.MaxAnchorLine,
			gitlet.MaxAnchorLine + 2, gitlet.MaxAnchorLine + 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			start, end, current := gitlet.Reanchor(tc.hunks, tc.start, tc.end)
			if start != tc.wantStart || end != tc.wantEnd || current != tc.current {
				t.Errorf("Reanchor(%v, %d, %d) = %d, %d, %t; want %d, %d, %t", tc.hunks, tc.start, tc.end, start, end,
					current, tc.wantStart, tc.wantEnd, tc.current)
			}
		})
	}
}

// hunk is the hunk replacing oldLines lines from oldStart with newLines from
// newStart.
func hunk(oldStart, oldLines, newStart, newLines int) gitlet.Hunk {
	return gitlet.Hunk{OldStart: oldStart, OldLines: oldLines, NewStart: newStart, NewLines: newLines}
}

// A HUNK HEADER IS READ AS GIT WRITES IT: a count left out for one line, a
// section heading after the closing marks, a pure insertion's and a pure
// deletion's zero — and anything else is no header, never a hunk of zeros: a
// sign, a leading zero, or a number past what an int holds, which would wrap
// into a line no file has.
//
// Mutation: drop hunkNumber's bound on what an int holds — the past-an-int
// row reads a wrapped number, red.
func TestAHunkHeaderIsReadAsGitWritesIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line string
		want gitlet.Hunk
		ok   bool
	}{
		{"@@ -3,2 +3,4 @@", hunk(3, 2, 3, 4), true},
		{"@@ -10 +12 @@", hunk(10, 1, 12, 1), true},
		{"@@ -0,0 +1,2 @@", hunk(0, 0, 1, 2), true},
		{"@@ -5,3 +4,0 @@ func main() {", hunk(5, 3, 4, 0), true},
		{"@@ -1 +1 @@\n", hunk(1, 1, 1, 1), true},
		{"@@ -1,2 +1,2 @@@", gitlet.Hunk{}, false},
		{"@@@ -1 -1 +1 @@@", gitlet.Hunk{}, false},
		{"@@ -01 +1 @@", gitlet.Hunk{}, false},
		{"@@ --1 +1 @@", gitlet.Hunk{}, false},
		{"@@ -1, +1 @@", gitlet.Hunk{}, false},
		{"@@ -1 +1", gitlet.Hunk{}, false},
		{"+@@ -1 +1 @@", gitlet.Hunk{}, false},
		{"@@ -+1 +1 @@", gitlet.Hunk{}, false},
		{"@@ -0 +9223372036854775807 @@", hunk(0, 1, math.MaxInt64, 1), true},
		{"@@ -1 +9223372036854775808 @@", gitlet.Hunk{}, false},
	} {
		got, ok := gitlet.ParseHunk([]byte(tc.line))
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseHunk(%q) = %+v, %t; want %+v, %t", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}
