package confluence_test

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"
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

// sentSearch is the CQL and the depth the site was last asked for.
func sentSearch(t *testing.T, inst *instance) (cql string, limit int) {
	t.Helper()
	values, err := url.ParseQuery(inst.lastQuery())
	if err != nil {
		t.Fatalf("parse the query the site was sent: %v", err)
	}
	limit, err = strconv.Atoi(values.Get("limit"))
	if err != nil {
		t.Fatalf("the site was sent no depth: %q", inst.lastQuery())
	}
	return values.Get("cql"), limit
}

// THE TOOL-SKILLS SPACE IS EXCLUDED AT THE SOURCE, whatever the scope.
//
// Dropped from what came back instead, every skill page among the site's top
// rows would take a row of the headroom a knowledge page could have had. And
// the exclusion is a clause of its own rather than a name taken off the scope
// list: a scope that named only that space would then be empty, which on a
// seat with its own credential is the unscoped search of the whole instance.
//
// Mutation: drop the clause from the query and every case fails; take the
// space off the scope list instead and the last one searches unscoped.
func TestTheSkillsSpaceIsExcludedAtTheSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		scope []string
		want  []string
	}{
		{"unscoped", nil, []string{`type = page`}},
		{"scoped", []string{"ENG"}, []string{`space IN ("ENG")`}},
		{"scoped to the skills space alone", []string{"ts"}, []string{`space IN ("TS")`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inst := newInstance(t, func(string) (int, string) { return 200, `{"results":[]}` })
			searcher := confluence.NewSearcher(confluence.SearcherOptions{
				Org: client(t, inst), SkillsSpace: "ts",
				ForSeat: func(*org.Role) (*confluence.Client, bool) {
					return client(t, inst), true
				},
			})
			o := &org.Organization{Name: "nimbus", KnowledgeScope: tc.scope}
			o.Normalize()
			answer := searcher.Search(context.Background(), knowledge.Query{
				Text: "deploy", Org: o, Seat: &org.Role{Name: "SWE"},
			})
			if answer.Failed {
				t.Fatalf("the search failed: %+v", answer)
			}
			cql, _ := sentSearch(t, inst)
			for _, want := range append(tc.want, `space != "TS"`) {
				if !strings.Contains(cql, want) {
					t.Errorf("the site was sent %q, which does not carry %q", cql, want)
				}
			}
		})
	}
}

// THE SITE IS ASKED FOR A MULTIPLE OF THE LIMIT.
//
// The drafts the exclusion drops are a share of what the site ranks, so the
// rows they take grow with the depth asked for: a fixed allowance tolerates a
// smaller share the larger the limit, where a multiple tolerates the same
// share at every limit.
//
// Mutation: ask for the limit plus a fixed number of rows and the two limits
// below are asked for depths that are not the same multiple.
func TestTheSiteIsAskedForAMultipleOfTheLimit(t *testing.T) {
	t.Parallel()
	depths := map[int]int{}
	for _, limit := range []int{4, 8} {
		inst := newInstance(t, func(string) (int, string) { return 200, `{"results":[]}` })
		searcher := confluence.NewSearcher(confluence.SearcherOptions{
			Org: client(t, inst),
			ForSeat: func(*org.Role) (*confluence.Client, bool) {
				return client(t, inst), true
			},
		})
		o := &org.Organization{Name: "nimbus"}
		o.Normalize()
		searcher.Search(context.Background(), knowledge.Query{
			Text: "deploy", Org: o, Seat: &org.Role{Name: "SWE"}, Limit: limit,
		})
		_, depths[limit] = sentSearch(t, inst)
	}
	if depths[4] <= 4 || depths[8] != 2*depths[4] {
		t.Errorf("limits 4 and 8 asked the site for %d and %d rows, want the same "+
			"multiple of each, above the limit", depths[4], depths[8])
	}
}

// A NIL SEARCHER IS A SEARCH THAT DID NOT RUN, and says so — except for a
// query with no text in it, which asked nothing and answers the unmarked
// empty answer every searcher gives it.
//
// Mutation: answer the nil searcher before the blank text and the blank query
// comes back marked failed; answer it unmarked and the search that did not
// run reads as one that matched nothing.
func TestANilSearcherIsASearchThatDidNotRun(t *testing.T) {
	t.Parallel()
	var searcher *confluence.Searcher
	o := &org.Organization{Name: "nimbus", KnowledgeScope: []string{"ENG"}}
	o.Normalize()
	seat := &org.Role{Name: "SWE"}

	if searcher.CanSearch(seat, o) {
		t.Error("a nil searcher passed the pre-gate")
	}
	asked := searcher.Search(context.Background(), knowledge.Query{
		Text: "deploy", Org: o, Seat: seat,
	})
	if !asked.Failed || len(asked.Hits) != 0 || asked.Partial != nil {
		t.Errorf("a search on a nil searcher answered %+v, want an empty answer "+
			"marked failed", asked)
	}
	blank := searcher.Search(context.Background(), knowledge.Query{
		Text: "  ", Org: o, Seat: seat,
	})
	if blank.Failed || len(blank.Hits) != 0 || blank.Partial != nil {
		t.Errorf("a blank query on a nil searcher answered %+v, want the unmarked "+
			"empty answer, because nothing was asked", blank)
	}
}
