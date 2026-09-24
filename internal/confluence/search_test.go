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
//     returned when its chain came back — under another page, and at the
//     top of its space, where the chain comes back as an empty list: moving
//     is the gesture that means reviewed, and the prefix is only the
//     backstop for an answer that carried no chain at all.
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
			 "body":{"storage":{"value":"<p>old</p>"}}},
			{"id":"6","title":"[Auto-draft] Moved to the top","space":{"key":"ENG"},
			 "ancestors":[],
			 "body":{"storage":{"value":"<p>reviewed</p>"}}}]}`
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
		// did not come back are hidden; the drafts whose chain came back
		// clean are published, the one at the top of its space included.
		{"the default exclusion", nil, []string{"3", "4", "5", "6"}},
		// A DIFFERENT QUESTION: drafts are not being hidden, so neither
		// the chain nor the prefix hides one; the page under the excluded
		// ancestor is.
		{"an unrelated exclusion", []string{"Archive"}, []string{"1", "2", "3", "4", "6"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hits := searcher.Search(context.Background(), knowledge.Query{
				Text: "deploy", Org: o, Seat: seat, ExcludeAncestors: tc.excluded,
			}).Hits
			var ids []string
			for _, hit := range hits {
				ids = append(ids, hit.PageID)
				// KNOWN WHERE THE ANSWER CARRIED THE CHAIN, an empty
				// list included, and not where it carried no key.
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

// A PAGE SAYS WHETHER ITS CHAIN CAME BACK, which the list alone cannot.
//
// `"ancestors": []` is a page at the top of its space, and an answer with no
// `ancestors` key lost the expand; both decode to an empty list. The first is
// under nothing and the second might be under the draft parent, so the
// exclusion needs them apart — told only the list, a draft a lead moved to the
// top of its space would stay hidden for as long as its title kept the prefix.
func TestAPageSaysWhetherItsChainCameBack(t *testing.T) {
	t.Parallel()
	inst := newInstance(t, func(string) (int, string) {
		return 200, `{"results":[
			{"id":"absent","title":"a","space":{"key":"ENG"}},
			{"id":"null","title":"b","space":{"key":"ENG"},"ancestors":null},
			{"id":"top","title":"c","space":{"key":"ENG"},"ancestors":[]},
			{"id":"under","title":"d","space":{"key":"ENG"},
			 "ancestors":[{"title":"Engineering"},{"title":"Runbooks"}]}]}`
	})
	got, err := client(t, inst).Search(context.Background(), `text ~ "x"`, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, want := range []struct {
		id    string
		known bool
		chain []string
	}{
		{"absent", false, nil},
		{"null", false, nil},
		{"top", true, nil},
		{"under", true, []string{"Engineering", "Runbooks"}},
	} {
		i := slices.IndexFunc(got, func(p confluence.Page) bool { return p.ID == want.id })
		if i < 0 {
			t.Errorf("page %q did not decode", want.id)
			continue
		}
		if got[i].AncestorsKnown != want.known {
			t.Errorf("page %q reports its chain known=%v, want %v", want.id,
				got[i].AncestorsKnown, want.known)
		}
		if !slices.Equal(got[i].Ancestors, want.chain) {
			t.Errorf("page %q decoded the chain %v, want %v", want.id,
				got[i].Ancestors, want.chain)
		}
	}
}

// A LIVE SEARCH IS NEVER BUILDING: it keeps no index of this node's own, so a
// seat searching through it is never told the knowledge base is still being
// indexed.
func TestALiveSearchIsNeverBuilding(t *testing.T) {
	t.Parallel()
	searcher := confluence.NewSearcher(confluence.SearcherOptions{})
	if searcher.Building(context.Background()) {
		t.Error("the Confluence searcher reports an index still building")
	}
}
