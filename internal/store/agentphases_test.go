package store_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A SEAT'S HISTORY STOPS WHERE EVERY OTHER READ OF THIS TABLE STOPS.
//
// EventHistory is documented as the hard bottom of paging, and List, Trace,
// Turn and the company-wide Phases all apply it. AgentPhases was the one read
// that did not, so it answered below the floor: the day of slack EventRetention
// deliberately leaves, and on a node whose maintenance singleton is not
// sweeping, rows of any age. The seat's own page then carried turns the Model
// screen excluded — the exact comparison an operator makes to decide whether a
// seat has gone quiet.
func TestAgentPhasesStopAtTheSameFloorEveryOtherReadDoes(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC()

	phase := func(id string, at time.Time) store.EventRecord {
		return store.EventRecord{
			ID: id, Type: "agent_phase_completed", Source: "Lead",
			Category: "agent", Time: at, Actor: "Lead",
			Tags:    map[string]string{"agent_role": "Lead", "turn_id": id},
			Payload: []byte(`{"turn_id":"` + id + `","phase":"execute","role":"Lead"}`),
		}
	}
	for _, rec := range []store.EventRecord{
		phase("recent", now.Add(-time.Hour)),
		// Inside EventRetention's day of slack, so the sweep has not taken
		// it — and below EventHistory, so no read may answer with it.
		phase("below-floor", now.Add(-store.EventHistory-time.Hour)),
	} {
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}

	got, _, err := log.AgentPhases(t.Context(), "", "Lead", nil)
	if err != nil {
		t.Fatalf("AgentPhases: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if len(ids) != 1 || ids[0] != "recent" {
		t.Errorf("seat phases = %v, want only the row inside the read floor", ids)
	}

	// And the company-wide read agrees, which is the point: the two answers
	// disagreeing about where history stops is what an operator sees.
	company, _, err := log.Phases(t.Context(), "Lead", 0, nil)
	if err != nil {
		t.Fatalf("Phases: %v", err)
	}
	if len(company) != len(ids) {
		t.Errorf("company-wide read returned %d phases and the seat's returned %d",
			len(company), len(ids))
	}
}

// A SEAT'S PHASE PAGE SAYS WHEN THE RECORD HOLDS MORE, and says nothing when it
// does not.
//
// The boundary is the case: a seat with exactly a page of history is COMPLETE,
// so a caller inferring "more" from a full page offers an older page that holds
// nothing, and one inferring "complete" from it shows a page as the seat's
// whole record. The rest of the record is where the doc says it is: the next
// page, from the last row as the cursor.
func TestASeatsPhasePageSaysWhetherTheRecordHoldsMore(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC()
	for i := range store.AgentPhaseLimit {
		appendPhase(t, log, fmt.Sprintf("p-%03d", i), now.Add(-time.Duration(i+1)*time.Minute))
	}

	page, more, err := log.AgentPhases(t.Context(), "", "Lead", nil)
	if err != nil {
		t.Fatalf("AgentPhases: %v", err)
	}
	if len(page) != store.AgentPhaseLimit || more {
		t.Fatalf("a record of exactly %d phases answered %d with more=%v, want all "+
			"of them and more=false", store.AgentPhaseLimit, len(page), more)
	}

	oldest := now.Add(-time.Duration(store.AgentPhaseLimit+1) * time.Minute)
	appendPhase(t, log, "p-oldest", oldest)
	page, more, err = log.AgentPhases(t.Context(), "", "Lead", nil)
	if err != nil {
		t.Fatalf("AgentPhases: %v", err)
	}
	if len(page) != store.AgentPhaseLimit || !more {
		t.Fatalf("a record one past the page answered %d with more=%v, want a full "+
			"page and more=true", len(page), more)
	}
	last := page[len(page)-1]
	rest, more, err := log.AgentPhases(t.Context(), "", "Lead",
		&store.Cursor{Time: last.Time, ID: last.ID})
	if err != nil {
		t.Fatalf("AgentPhases, next page: %v", err)
	}
	if len(rest) != 1 || rest[0].ID != "p-oldest" || more {
		t.Fatalf("the next page is %d rows (more=%v), want exactly the oldest phase",
			len(rest), more)
	}
}

// AND THE COMPANY-WIDE PAGE, which shares the seat read's ordering and cursor
// and so shares its boundary: a full page is not evidence of a next one.
func TestTheCompanyPhasePageSaysWhetherTheRecordHoldsMore(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC()
	for i := range 3 {
		appendPhase(t, log, fmt.Sprintf("p-%d", i), now.Add(-time.Duration(i+1)*time.Minute))
	}
	for _, c := range []struct {
		limit int
		want  int
		more  bool
	}{
		{limit: 2, want: 2, more: true},
		{limit: 3, want: 3, more: false},
		{limit: 4, want: 3, more: false},
	} {
		page, more, err := log.Phases(t.Context(), "", c.limit, nil)
		if err != nil {
			t.Fatalf("Phases(%d): %v", c.limit, err)
		}
		if len(page) != c.want || more != c.more {
			t.Errorf("Phases(limit %d) = %d rows, more=%v; want %d rows, more=%v",
				c.limit, len(page), more, c.want, c.more)
		}
	}
}

// appendPhase writes one of Lead's completed phases at the given instant.
func appendPhase(t *testing.T, log *store.EventLog, id string, at time.Time) {
	t.Helper()
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id, Type: "agent_phase_completed", Source: "Lead",
		Category: "agent", Time: at, Actor: "Lead",
		Tags:    map[string]string{"agent_role": "Lead", "turn_id": "t-" + id},
		Payload: []byte(`{"phase":"execute","role":"Lead"}`),
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// HALF A CURSOR IS REFUSED BY EVERY PAGED READ OF THE LOG, not served. Read as
// its time alone it steps over every row sharing the boundary's instant; read
// as no cursor it hands a pager its first page again, for ever.
func TestEveryPagedEventReadRefusesACursorWithoutAnID(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	half := &store.Cursor{Time: time.Now().UTC()}

	if _, err := log.List(t.Context(), store.ListQuery{Before: half}); !errors.Is(err, store.ErrHalfCursor) {
		t.Errorf("List answered %v, want ErrHalfCursor", err)
	}
	if _, _, err := log.AgentPhases(t.Context(), "", "Lead", half); !errors.Is(err, store.ErrHalfCursor) {
		t.Errorf("AgentPhases answered %v, want ErrHalfCursor", err)
	}
	if _, _, err := log.Phases(t.Context(), "", 0, half); !errors.Is(err, store.ErrHalfCursor) {
		t.Errorf("Phases answered %v, want ErrHalfCursor", err)
	}
}
