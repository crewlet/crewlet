package store_test

import (
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

	got, err := log.AgentPhases(t.Context(), "", "Lead", nil)
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
	company, err := log.Phases(t.Context(), "Lead", 0, nil)
	if err != nil {
		t.Fatalf("Phases: %v", err)
	}
	if len(company) != len(ids) {
		t.Errorf("company-wide read returned %d phases and the seat's returned %d",
			len(company), len(ids))
	}
}
