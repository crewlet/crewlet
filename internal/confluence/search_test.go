package confluence_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
)

// A CONFLUENCE SEARCH EXCLUDES BY THE SEAM'S ONE RULE.
//
// [knowledge.Excludes] is the rule the native backend applies too, so a page
// is kept or dropped the same way whichever backend a company runs. Two
// consequences are visible only here:
//
//   - A draft moved out from under the auto-draft parent, prefix kept, is
//     returned when its chain came back: moving is the gesture that means
//     reviewed, and the prefix is only the backstop for a chain the site did
//     not answer with.
//   - The prefix hides a draft only while the caller excludes the auto-draft
//     parent. A caller excluding some other page asked a different question,
//     and one that hid every prefixed title whatever it asked would hide
//     exactly the drafts a lead went looking for.
func TestAConfluenceSearchExcludesByTheSeamsOneRule(t *testing.T) {
	t.Parallel()
	inst := newInstance(t, func(string) (int, string) {
		return 200, `{"results":[
			{"id":"1","title":"Draft by ancestor","space":{"key":"ENG"},
			 "ancestors":[{"title":"Auto-Drafted Skills"}],
			 "body":{"storage":{"value":"<p>unreviewed</p>"}}},
			{"id":"2","title":"[Auto-draft] No chain","space":{"key":"ENG"},
			 "body":{"storage":{"value":"<p>unreviewed</p>"}}},
			{"id":"3","title":"[Auto-draft] Moved under Runbooks","space":{"key":"ENG"},
			 "ancestors":[{"title":"Runbooks"}],
			 "body":{"storage":{"value":"<p>reviewed</p>"}}},
			{"id":"4","title":"Real page","space":{"key":"ENG"},
			 "ancestors":[{"title":"Runbooks"}],
			 "body":{"storage":{"value":"<p>reviewed</p>"}}},
			{"id":"5","title":"Archived page","space":{"key":"ENG"},
			 "ancestors":[{"title":"Archive"}],
			 "body":{"storage":{"value":"<p>old</p>"}}}]}`
	})
	searcher := confluence.NewSearcher(confluence.SearcherOptions{
		Org: client(t, inst),
		ForSeat: func(*org.Role) (*confluence.Client, bool) {
			return client(t, inst), true
		},
	})
	o := &org.Organization{Name: "nimbus"}
	o.Normalize()
	seat := &org.Role{Name: "SWE"}

	for _, tc := range []struct {
		name     string
		excluded []string
		want     []string
	}{
		// THE DEFAULT: the draft under the parent and the one whose chain
		// did not come back are hidden; the draft whose chain came back
		// clean is published.
		{"the default exclusion", nil, []string{"3", "4", "5"}},
		// A DIFFERENT QUESTION: drafts are not being hidden, so neither
		// the chain nor the prefix hides one; the page under the excluded
		// ancestor is.
		{"an unrelated exclusion", []string{"Archive"}, []string{"1", "2", "3", "4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hits := searcher.Search(context.Background(), knowledge.Query{
				Text: "deploy", Org: o, Seat: seat, ExcludeAncestors: tc.excluded,
			})
			var ids []string
			for _, hit := range hits {
				ids = append(ids, hit.PageID)
				// KNOWN WHERE THE SITE ANSWERED WITH A CHAIN, and not
				// where it answered with none: an empty chain here is
				// either the top of a space or a lost expand.
				if want := hit.PageID != "2"; hit.AncestorsKnown != want {
					t.Errorf("page %s reports its chain known=%v, want %v",
						hit.PageID, hit.AncestorsKnown, want)
				}
			}
			if !slices.Equal(ids, tc.want) {
				t.Errorf("returned pages %v, want %v", ids, tc.want)
			}
		})
	}
}
