package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
)

// A COMPANY THAT KEEPS NO NATIVE TRACKER HAS NO WORK SEARCH, NOT AN EMPTY ONE.
//
// The lexical index is built under either native backend, because it covers
// both corpora, and the tracker's ranked search was built over it whenever the
// index was — so a company whose tracker is a vendor's, or none, still had a
// work search, over a tracker corpus that is empty by construction. Every
// other work question is one such a node does not have (`unknown_query`);
// `work_search` answered with nothing, which reads to a screen as "no item
// matches" rather than "there is no tracker here" — the confusion the
// accessor's untyped nil exists to prevent. The control is a company on the
// engine's own tracker, which searches.
//
// Mutation: build the searcher whatever the tracker backend and the first
// case holds one.
func TestACompanyWithNoNativeTrackerHasNoWorkSearch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		doc    string
		search bool
	}{
		{"no tracker, the native knowledge base", companyDoc + `
tracker:
  backend: none
`, false},
		{"the engine's own tracker, the control", companyDoc, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEngine(t, engine.Options{Company: parsedCompany(t, tc.doc)})
			if !e.NativeStarted() {
				t.Fatal("the premise: this node met its company at boot and " +
					"brought its native halves up")
			}
			if got := e.WorkSearch() != nil; got != tc.search {
				t.Errorf("WorkSearch held = %v, want %v", got, tc.search)
			}
			if got := engine.WorkSearcher(e) != nil; got != tc.search {
				t.Errorf("WorkSearcher held = %v, want %v — a tool seam over a "+
					"search with no tracker behind it", got, tc.search)
			}
		})
	}
}
