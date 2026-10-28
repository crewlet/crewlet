package catalog

import "testing"

// THE REFERENCE SAYS WHICH LIST ROUTES PAGE BY LIMIT, and a generic read of
// one is asked for a page: the event log and the turns do; the company
// document does not. Mutation: read the wrong column of the question table
// and the event log is not paged.
func TestTheReferenceSaysWhichRoutesPageByLimit(t *testing.T) {
	t.Parallel()
	for pattern, want := range map[string]bool{
		"GET /events": true, "GET /turns": true, "GET /worklet/inbox": true,
		"GET /config": false, "GET /agents/{id}": false,
	} {
		if got := pagedByLimit(pattern); got != want {
			t.Errorf("pagedByLimit(%q) = %v, want %v", pattern, got, want)
		}
	}
}
