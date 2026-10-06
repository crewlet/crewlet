package iamapi_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE DASHBOARD OFFERS EXACTLY THE ENGINE'S GRANTS, in its order, and withholds
// from a token exactly the grants the engine never lets one carry.
//
// People & access confers grants — an invitation, an edit, a service account,
// a token — by ticking them off `GRANTS`, and greys out of a token's mint the
// ones in `TOKEN_WITHHELD_GRANTS`. A grant the engine grew that the list lacks
// is one no administrator can confer from the dashboard, and one the list kept
// that the engine dropped is a box every write refuses.
//
// Mutation: drop a grant from either list, or reorder `GRANTS`, and this fails.
func TestTheDashboardOffersExactlyTheEnginesGrants(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, tc := range []struct {
		name string
		want []iam.Grant
	}{
		{"GRANTS", iam.AllGrants},
		{"TOKEN_WITHHELD_GRANTS", iam.PersonPresentGrants},
	} {
		body, err := clientsource.Literal(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		got := clientsource.Strings(body)
		if len(got) == 0 {
			t.Fatalf("%s reads as empty, so this gate certifies nothing", tc.name)
		}
		want := make([]string, 0, len(tc.want))
		for _, g := range tc.want {
			want = append(want, string(g))
		}
		if !slices.Equal(got, want) {
			t.Errorf("the dashboard's %s is %v, want the engine's, in order: %v",
				tc.name, got, want)
		}
	}
}
