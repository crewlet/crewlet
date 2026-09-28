package store_test

import (
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

// A PAGE SAYS WHETHER IT IS THE LAST — ASKED, NEVER GUESSED FROM A FULL PAGE.
//
// Each paged read of this log asks one row past its page and reports whether
// it came back. "The page filled, so there may be more" is wrong exactly when
// the history is a multiple of the page: a seat with fifty phases, or fifty
// turns, was offered "older" onto an empty page.
//
// Mutation: report `len(rows) >= limit` instead, and the exactly-a-page cases
// say there is more.
func TestAPageOfExactlyTheHistorySaysItIsTheLast(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC()
	add := func(i int) {
		t.Helper()
		id := fmt.Sprintf("p-%03d", i)
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: "agent_phase_completed", Source: "Lead",
			Category: "agent", Time: now.Add(-time.Duration(i+1) * time.Second), Actor: "Lead",
			Tags:    map[string]string{"agent_role": "Lead", "turn_id": id},
			Payload: []byte(`{"turn_id":"` + id + `","phase":"execute","role":"Lead"}`),
		}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	for i := range store.AgentPhaseLimit {
		add(i)
	}

	seat, more, err := log.AgentPhases(t.Context(), "", "Lead", nil)
	if err != nil {
		t.Fatalf("AgentPhases: %v", err)
	}
	if len(seat) != store.AgentPhaseLimit || more {
		t.Errorf("a seat with exactly a page of phases: %d rows, more=%v; want %d and false",
			len(seat), more, store.AgentPhaseLimit)
	}
	company, more, err := log.Phases(t.Context(), "", store.AgentPhaseLimit, nil)
	if err != nil {
		t.Fatalf("Phases: %v", err)
	}
	if len(company) != store.AgentPhaseLimit || more {
		t.Errorf("a company page of exactly the phases there are: %d rows, more=%v; want %d and false",
			len(company), more, store.AgentPhaseLimit)
	}
	turns, more, err := log.TurnPartials(t.Context(), store.TurnQuery{Limit: store.AgentPhaseLimit})
	if err != nil {
		t.Fatalf("TurnPartials: %v", err)
	}
	if len(turns) != store.AgentPhaseLimit || more {
		t.Errorf("a turn page of exactly the turns there are: %d rows, more=%v; want %d and false",
			len(turns), more, store.AgentPhaseLimit)
	}

	// ONE MORE, and every read says so — and still answers only its page.
	add(store.AgentPhaseLimit)
	seat, more, err = log.AgentPhases(t.Context(), "", "Lead", nil)
	if err != nil || len(seat) != store.AgentPhaseLimit || !more {
		t.Errorf("a seat with a page and one: %d rows, more=%v, err %v; want %d and true",
			len(seat), more, err, store.AgentPhaseLimit)
	}
	company, more, err = log.Phases(t.Context(), "", store.AgentPhaseLimit, nil)
	if err != nil || len(company) != store.AgentPhaseLimit || !more {
		t.Errorf("a company page short of one: %d rows, more=%v, err %v; want %d and true",
			len(company), more, err, store.AgentPhaseLimit)
	}
	turns, more, err = log.TurnPartials(t.Context(), store.TurnQuery{Limit: store.AgentPhaseLimit})
	if err != nil || len(turns) != store.AgentPhaseLimit || !more {
		t.Errorf("a turn page short of one: %d rows, more=%v, err %v; want %d and true",
			len(turns), more, err, store.AgentPhaseLimit)
	}

	// A SHARE IS NOT A PAGE: every turn it names, and never more.
	ids := []string{"p-000", "p-001"}
	share, more, err := log.TurnPartials(t.Context(), store.TurnQuery{IDs: ids, Limit: 1})
	if err != nil || len(share) != len(ids) || more {
		t.Errorf("a share of two named turns: %d, more=%v, err %v; want 2 and false", len(share), more, err)
	}
}
