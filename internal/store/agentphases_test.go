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
			Tags:    map[string]string{"agent_id": "a-lead", "agent_role": "Lead", "turn_id": id},
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

	got, err := log.AgentPhases(t.Context(), "a-lead", nil)
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
	company, err := log.Phases(t.Context(), "a-lead", 0, nil)
	if err != nil {
		t.Fatalf("Phases: %v", err)
	}
	if len(company) != len(ids) {
		t.Errorf("company-wide read returned %d phases and the seat's returned %d",
			len(company), len(ids))
	}
}

// TWO SEATS THAT SHARE A NAME HAVE TWO HISTORIES.
//
// A seat's phases are matched on its agent id and never on the role name
// beside it. The read used to take the name as well, OR'd with the id, so a
// seat's page and a seat-filtered Model screen listed its namesake's calls
// among its own: a name is prose, and two "Engineer"s is an ordinary company.
func TestASeatsPhasesAreItsOwnWhenItsNameIsShared(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC()

	for i, agentID := range []string{"a-ada", "a-bob"} {
		id := "phase-" + agentID
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: "agent_phase_completed", Source: "Engineer",
			Category: "agent", Time: now.Add(-time.Duration(i+1) * time.Minute), Actor: "Engineer",
			Tags:    map[string]string{"agent_id": agentID, "agent_role": "Engineer", "turn_id": id},
			Payload: []byte(`{"turn_id":"` + id + `","phase":"execute","role":"Engineer"}`),
		}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}

	seat, err := log.AgentPhases(t.Context(), "a-ada", nil)
	if err != nil {
		t.Fatalf("AgentPhases: %v", err)
	}
	if len(seat) != 1 || seat[0].ID != "phase-a-ada" {
		t.Errorf("a-ada's phases = %v, want only its own", phaseIDs(seat))
	}
	company, err := log.Phases(t.Context(), "a-bob", 0, nil)
	if err != nil {
		t.Fatalf("Phases: %v", err)
	}
	if len(company) != 1 || company[0].ID != "phase-a-bob" {
		t.Errorf("a-bob's phases = %v, want only its own", phaseIDs(company))
	}
	if none, err := log.AgentPhases(t.Context(), "", nil); err != nil || len(none) != 0 {
		t.Errorf("no id = %v, %v; want nothing rather than every row that carries none",
			phaseIDs(none), err)
	}
}

func phaseIDs(records []store.EventRecord) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.ID)
	}
	return out
}
