package configapi_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
)

// THE DOCUMENTS ACTIVE OVER A SPAN ARE THE ONE ACTIVE AT ITS START AND EVERY ONE
// ACTIVATED SINCE, oldest first, so a reader can tell whether anything changed
// over it; a span the kept history does not reach back over is refused rather
// than read as unchanged. Mutation: leave out the one active at the start, and
// the span reads as one document.
func TestTheDocumentsActiveOverASpanAreEveryOneItHeld(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	now := pinned
	s := newSurfaceWith(t, func(o *configapi.Options) {
		o.Now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}
	})
	base := s.seed(t, orgDoc)
	mu.Lock()
	now = pinned.Add(time.Hour)
	mu.Unlock()
	if _, err := s.service().ApplySeats(t.Context(), configapi.ApplySeatsRequest{
		Seats: []configapi.SeatEdit{goalEdit("ceo", "set direction")}, Summary: "a goal", By: byEngine, Expect: base,
	}); err != nil {
		t.Fatalf("ApplySeats: %v", err)
	}
	docs, err := s.service().CompaniesActiveSince(t.Context(), pinned.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	goals := func() []string {
		var out []string
		for _, d := range docs {
			for role := range d.EachRole() {
				if role.Seat().Handle() == "ceo" {
					out = append(out, role.Goal)
				}
			}
		}
		return out
	}
	if got := goals(); len(got) != 2 || got[0] == "set direction" || got[1] != "set direction" {
		t.Errorf("the span holds the CEO's goals %v, want the seed's then the edit's", got)
	}
	if docs, err = s.service().CompaniesActiveSince(t.Context(), pinned.Add(2*time.Hour)); err != nil || len(docs) != 1 {
		t.Errorf("a span after the last activation holds %d documents (%v), want the active one", len(docs), err)
	}
	if _, err := s.service().CompaniesActiveSince(t.Context(), pinned.Add(-time.Hour)); !errors.Is(err, configapi.ErrHistoryShort) {
		t.Errorf("a span from before the first revision answered %v, want ErrHistoryShort", err)
	}
}
