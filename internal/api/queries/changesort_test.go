package queries_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// A CHANGES LISTING ASKS THE SORT ITS CALLER NAMED: `sort` reaches the reader as the read's
// ChangeSort, as given, an order the reader does not know among them (the reader's to refuse), and
// none named asks the walk's own order.
//
// Mutation: read `sort` on another kind, or drop it.
func TestAChangesListingAsksTheSortItsCallerNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		params map[string]any
		want   gitlet.ChangeSort
	}{
		{map[string]any{"repo": "web"}, ""},
		{map[string]any{"repo": "web", "sort": "updated"}, gitlet.ChangeSortUpdated},
		{map[string]any{"repo": "web", "sort": "opened"}, gitlet.ChangeSortOpened},
		{map[string]any{"repo": "web", "sort": "sideways"}, "sideways"},
	} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_changes", tc.params); err != nil {
			t.Fatalf("code_changes %v: %v", tc.params, err)
		}
		if asked := stub.last(); asked.Kind != gitlet.ReadChanges || asked.ChangeSort != tc.want {
			t.Errorf("code_changes %v asked %+v, want a changes read asking sort %q", tc.params, asked, tc.want)
		}
	}
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_reviews", map[string]any{"reviewer": "cto",
		"sort": "updated"}); err != nil {
		t.Fatalf("code_reviews with a sort: %v", err)
	}
	if asked := stub.last(); asked.ChangeSort != "" {
		t.Errorf("code_reviews with a sort asked %+v, want no sort: only a listing of changes is ordered so", asked)
	}
}
