package worklet_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN ANSWER'S LIST IS EMPTY, NEVER NULL, asserted on the ENCODED answer since
// the defect is a wire shape. `ProjectListing.Projects` carries no
// `omitempty`, so a nil slice reaches a client as `"projects": null`, and the
// dashboard's landing screen decides on `projects?.length === 0`, false for
// `undefined`: a company on its first day, the state its
// where-a-project-comes-from panel exists for, would get the work list
// instead.
//
// THE EMPTY COMPANY IS THE HARNESS WITHOUT ITS PROJECT, the state a company
// boots in.
func TestAnEmptyProjectListingEncodesAnEmptyArray(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)

	// THE SEGMENT THE DASHBOARD LANDS ON, which is the active set.
	listing := r.projects(worklet.ProjectQuery{
		Archived: worklet.ArchivedExclude, Level: statelog.ReadStale,
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
	// AND THE CENSUS SAYS IT IS THE COMPANY, not the segment, so a reader of
	// the empty answer need not guess ([worklet.ProjectCensus]).
	if listing.Census.Active != 0 || listing.Census.Archived != 0 {
		t.Fatalf("an empty company's census is %+v", listing.Census)
	}
}

// THE SAME RULE ON THE DETAIL'S OWN LISTS, asserted on the Go value: `history`
// and `links` carry `omitempty`, so nil and empty encode alike. That tag is
// all that stands between these readers and the defect above, and somebody
// could drop it, so the invariant is held where it is decided, in the reader.
func TestATaskDetailsListsAreEmptyRatherThanNil(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("Rate limits in the GitLab client")

	detail, err := r.reader.Task(t.Context(), created.ID,
		worklet.DetailWants{History: true, Links: true},
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
	// THE HISTORY IS NOT ASSERTED, deliberately: a create writes a history
	// row, so no findable task has an empty history to catch a nil with.
	// `readHistory` holds the same rule, but no case here could go red over it.
}
