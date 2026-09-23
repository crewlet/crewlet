package tracker_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN ANSWER'S LIST IS EMPTY, NEVER NULL — asserted on the ENCODED answer,
// because the defect it protects against is a wire shape rather than a Go one.
//
// `ProjectListing.Projects` carries no `omitempty`, so a nil slice reaches a
// client as `"projects": null` — which is neither "some" nor "none" but a
// third state, and the dashboard's landing screen read it as neither: it asks
// `projects?.length === 0` to decide whether to draw the panel that says where
// a project comes from, and `undefined === 0` is false. So a company on its
// very first day — the one state that panel exists for — got the ordinary work
// list instead. Every fixture in the client's own suite said `[]`, which is
// what a fixture written by somebody who already knows the answer says.
//
// THE EMPTY COMPANY IS THE HARNESS WITHOUT ITS PROJECT, which is the state a
// company is actually in the moment it boots.
func TestAnEmptyProjectListingEncodesAnEmptyArray(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)

	// THE SEGMENT THE DASHBOARD LANDS ON, which is the active set.
	listing := r.projects(tracker.ProjectQuery{
		Archived: tracker.ArchivedExclude, Level: statelog.ReadStale,
	})
	if listing.Projects == nil {
		t.Fatal("the listing's Projects is nil, so it encodes as null")
	}
	if len(listing.Projects) != 0 {
		t.Fatalf("a company with no projects listed %d", len(listing.Projects))
	}

	encoded, err := json.Marshal(listing)
	if err != nil {
		t.Fatalf("encode the listing: %v", err)
	}
	if !strings.Contains(string(encoded), `"projects":[]`) {
		t.Fatalf("an empty listing does not carry `\"projects\":[]`: %s", encoded)
	}
	// AND THE CENSUS SAYS IT IS THE COMPANY rather than the segment, which
	// is what stops a reader of the empty answer guessing — see
	// [tracker.ProjectCensus].
	if listing.Census.Active != 0 || listing.Census.Archived != 0 {
		t.Fatalf("an empty company's census is %+v", listing.Census)
	}
}

// THE SAME RULE ON THE DETAIL'S OWN LISTS, asserted on the Go value because
// `history` and `links` carry `omitempty` and an absent field is the same
// bytes whether the slice was nil or empty.
//
// That tag is the ONLY thing between these two readers and the defect above,
// and it is a formatting choice somebody could reasonably drop — so the
// invariant is held where it is actually decided, in the reader.
func TestATaskDetailsListsAreEmptyRatherThanNil(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("Rate limits in the GitLab client")

	detail, err := r.reader.Task(t.Context(), created.ID,
		tracker.DetailWants{History: true, Links: true},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	// A TASK NOBODY HAS LINKED, which is most of them.
	if detail.Links == nil {
		t.Fatal("a task with no relations answers a nil Links")
	}
	if len(detail.Links) != 0 {
		t.Fatalf("a freshly created task has %d links", len(detail.Links))
	}
	// THE HISTORY IS NOT ASSERTED, and that is a statement rather than an
	// omission: a create writes a history row, so every task this reader
	// can find has at least one and there is no empty answer to catch a nil
	// with. `readHistory` holds the same rule — and sizes its slice to the
	// page, which is why the line is there at all — but nothing here can
	// go red over it, and a case that cannot fail is worse than none.
}
