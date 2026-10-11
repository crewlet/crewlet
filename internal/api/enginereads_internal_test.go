package api

import (
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
)

// THE READ SURFACE'S VIEW HOLDS ONLY THE HALVES THE RUNTIME RUNS, each absent
// one a genuinely nil interface. A field holding a zero router facade would
// pass the question's guard ([queries.Sources.Native]) and fail on the first
// read — and the backends are absent independently, so each half is held to
// its own flag: the tracker's reader carries the file rows, the knowledge
// base's carries the backlinks, and the ranked search is a half of its own.
func TestTheNativeViewHoldsOnlyTheHalvesTheRuntimeRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                  string
		reads                 engine.OperatorReads
		tracker, wiki, search bool
	}{
		{"no runtime", engine.OperatorReads{}, false, false, false},
		{"both halves and the index",
			engine.OperatorReads{Tracker: true, Wiki: true, Search: true}, true, true, true},
		{"the tracker without its searcher",
			engine.OperatorReads{Tracker: true}, true, false, false},
		{"the knowledge base on a vendor tracker",
			engine.OperatorReads{Wiki: true}, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			view := nativeView(tc.reads)
			for _, half := range []struct {
				name string
				got  any
				want bool
			}{
				{"Work", view.Work, tc.tracker},
				{"Files", view.Files, tc.tracker},
				{"Pages", view.Pages, tc.wiki},
				{"Backlinks", view.Backlinks, tc.wiki},
				{"WorkSearch", view.WorkSearch, tc.search},
			} {
				if (half.got != nil) != half.want {
					t.Errorf("%s = %#v, want held=%v", half.name, half.got, half.want)
				}
			}
			if tc.tracker {
				if _, ok := view.Work.(estate.Work); !ok {
					t.Errorf("Work is %T, want the router's estate.Work", view.Work)
				}
			}
			if tc.wiki {
				if _, ok := view.Pages.(estate.Pages); !ok {
					t.Errorf("Pages is %T, want the router's estate.Pages", view.Pages)
				}
			}
		})
	}
}
