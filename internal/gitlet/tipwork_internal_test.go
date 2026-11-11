package gitlet

import (
	"slices"
	"testing"
)

// THE TWO REPOSITORY LISTINGS WALK AS ONE (mergeRepoPages): a `checks_due`
// listing of repositories is two keyset pages after one cursor — the
// repositories owing their changes check work and those owing tip work — and
// merging them must lose no repository whichever of them a bound cut: walked
// page by page from the cursor each merge answers, it names every repository
// of the union exactly once, in order, and stops. Each case is a pair of whole
// listings, each paged after the cursor at the limit as its own statement
// pages it: either cut at the limit while the other is short, the union cut
// while both are short, both cut, ids the two share, and none at all.
//
// Mutation: end the walk wherever the union holds all both pages read,
// whether or not a page was cut at its bound — the repositories past a cut
// page are never listed, red; or set no cursor where only the union was cut
// — the same, red.
func TestTwoRepositoryListingsWalkAsOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		changes, tips   []string
		limit           int
		wantPagesAtMost int
	}{
		{"none at all", nil, nil, 3, 1},
		{"the changes' page cut, the tips' short", []string{"a", "c", "e", "g", "i"}, []string{"b"}, 2, 4},
		{"the tips' page cut, the changes' short", []string{"b"}, []string{"a", "c", "e", "g", "i"}, 2, 4},
		{"the union cut, both pages short", []string{"a", "c"}, []string{"b", "d"}, 3, 2},
		{"both pages cut", []string{"a", "b", "c", "d"}, []string{"a", "e", "f", "g"}, 2, 6},
		{"ids both listings hold", []string{"a", "b", "c"}, []string{"a", "b", "c"}, 3, 2},
		{"the tips alone", nil, []string{"a", "b", "c", "d"}, 2, 3},
		{"the changes alone, a page exactly full", []string{"a", "b"}, nil, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			page := func(all []string, after string) []string {
				var out []string
				for _, id := range all {
					if id > after && len(out) < tc.limit {
						out = append(out, id)
					}
				}
				return out
			}
			want := slices.Compact(slices.Sorted(slices.Values(slices.Concat(tc.changes, tc.tips))))
			var got []string
			cursor := ""
			for pages := 1; ; pages++ {
				if pages > tc.wantPagesAtMost {
					t.Fatalf("the walk was still paging after %d pages, listing %v of %v", tc.wantPagesAtMost,
						got, want)
				}
				ids, next := mergeRepoPages(page(tc.changes, cursor), page(tc.tips, cursor), tc.limit)
				if len(ids) > tc.limit {
					t.Fatalf("a page of %d ids, past the limit %d: %v", len(ids), tc.limit, ids)
				}
				got = append(got, ids...)
				if next == "" {
					break
				}
				cursor = next
			}
			if !slices.Equal(got, want) {
				t.Fatalf("the walk listed %v, want every repository of the union once, in order: %v", got, want)
			}
		})
	}
}
