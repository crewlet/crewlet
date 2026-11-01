package query

import (
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// A RE-ANCHORING KEEPS NO HUNK. Its diff's hunks are placed as they stream,
// so what the read holds is its anchors and never the diff: a hunk above an
// anchor moves it and is dropped — reading one allocates nothing, so a
// million of them grow no heap — and once every anchor of a pair is settled
// the pair's further lines are not read at all, so a line no header could be
// is passed over rather than refused. Each anchor lands where code.Reanchor
// puts it over the same hunks.
//
// NOT PARALLEL: testing.AllocsPerRun counts every allocation of the process
// while it runs, and a case running beside it would count its own.
//
// Mutations: keep each hunk the placer reads (append it to a slice) — the
// allocation is no longer nothing, red; read a pair's lines after its anchors
// are settled — the line past them is refused, red.
func TestAReanchoringKeepsNoHunk(t *testing.T) {
	pl := &placer{cur: -1, placings: []code.Placing{code.PlaceLines(1<<20, 1<<20), code.PlaceLines(3, 4)},
		settled: []bool{false, false}, anchors: [][]int{{0}, {1}}, open: []int{1, 1}, binary: []bool{false, false}}
	feed := func(line string) {
		t.Helper()
		if err := pl.line([]byte(line), false); err != nil {
			t.Fatalf("the placer refused %q: %v", line, err)
		}
	}
	feed("diff --git a/0 b/0")
	above := []byte("@@ -5,1 +5,2 @@ func f() {")
	const runs = 1000
	if allocs := testing.AllocsPerRun(runs, func() {
		if err := pl.line(above, false); err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Errorf("reading a hunk above an anchor allocated %v times a hunk, want none", allocs)
	}
	feed("@@ -2000000 +2000000,0 @@")
	start, end, kept := pl.placings[0].Placed()
	// AllocsPerRun runs its function once more than it measures.
	wantStart, wantEnd, wantKept := code.Reanchor(append(repeat(code.Hunk{OldStart: 5, OldLines: 1, NewStart: 5,
		NewLines: 2}, runs+1), code.Hunk{OldStart: 2000000, OldLines: 1, NewStart: 2000000}), 1<<20, 1<<20)
	if start != wantStart || end != wantEnd || kept != wantKept || pl.open[0] != 0 {
		t.Errorf("the anchor below %d hunks is at %d to %d, kept %t, its pair open %d; want %d to %d, kept %t, "+
			"settled", runs+1, start, end, kept, pl.open[0], wantStart, wantEnd, wantKept)
	}

	feed("@@ this line is past the pair's last anchor and never read")
	feed("diff --git a/1 b/1")
	feed("@@ -1,0 +1,2 @@")
	feed("@@ -4 +6 @@")
	if start, end, kept := pl.placings[1].Placed(); start != 5 || end != 6 || kept || pl.open[1] != 0 {
		t.Errorf("the second pair's anchor is at %d to %d, kept %t, open %d; want lines 5 to 6, outdated, settled",
			start, end, kept, pl.open[1])
	}
	if err := pl.line([]byte("@@ not a header"), false); err != nil {
		t.Errorf("a line of a pair whose anchors are settled was read: %v", err)
	}
}

// repeat is n copies of h.
func repeat(h code.Hunk, n int) []code.Hunk {
	out := make([]code.Hunk, n)
	for i := range out {
		out[i] = h
	}
	return out
}
