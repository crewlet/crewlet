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

	got, _, err := log.AgentPhases(t.Context(), "a-lead", nil)
	if err != nil {
		t.Fatalf("AgentPhases: %v", err)
	}
	ids := idsOf(got)
	if len(ids) != 1 || ids[0] != "recent" {
		t.Errorf("seat phases = %v, want only the row inside the read floor", ids)
	}

	// And the company-wide read agrees, which is the point: the two answers
	// disagreeing about where history stops is what an operator sees.
	company, _, err := log.Phases(t.Context(), "a-lead", 0, nil)
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
// among its own: a name is prose, and two "Engineer"s is an ordinary company
// — two unit seats stamped from one template share one as a matter of course,
// and the Live screen's `seat=` handed over as a role listed the twin's rounds.
//
// Mutation: filter on `agent_role` again, and the twin's phase is listed.
func TestASeatsPhasesAreItsOwnWhenItsNameIsShared(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC()

	for i, agentID := range []string{"a-ada", "a-bob"} {
		id := "phase-" + agentID
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: "agent_phase_completed", Source: "Engineer",
			Category: "agent", Time: now.Add(-time.Duration(i+1) * time.Minute), Actor: "Engineer",
			// ONE ROLE NAME, TWO SEATS.
			Tags:    map[string]string{"agent_id": agentID, "agent_role": "Engineer", "turn_id": id},
			Payload: []byte(`{"turn_id":"` + id + `","phase":"execute","role":"Engineer"}`),
		}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}

	seat, more, err := log.AgentPhases(t.Context(), "a-ada", nil)
	if err != nil {
		t.Fatalf("AgentPhases: %v", err)
	}
	if len(seat) != 1 || seat[0].ID != "phase-a-ada" || more {
		t.Errorf("a-ada's phases = %v (more=%v), want only its own", idsOf(seat), more)
	}
	company, more, err := log.Phases(t.Context(), "a-bob", 0, nil)
	if err != nil {
		t.Fatalf("Phases: %v", err)
	}
	if len(company) != 1 || company[0].ID != "phase-a-bob" || more {
		t.Errorf("a-bob's phases = %v (more=%v), want only its own", idsOf(company), more)
	}
	if all, _, err := log.Phases(t.Context(), "", 0, nil); err != nil || len(all) != 2 {
		t.Errorf("the company's phases = %d (err %v), want both", len(all), err)
	}
	if none, more, err := log.AgentPhases(t.Context(), "", nil); err != nil || len(none) != 0 || more {
		t.Errorf("no id = %v (more=%v), %v; want nothing rather than every row that carries none",
			idsOf(none), more, err)
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
			Tags:    map[string]string{"agent_id": "a-lead", "agent_role": "Lead", "turn_id": id},
			Payload: []byte(`{"turn_id":"` + id + `","phase":"execute","role":"Lead"}`),
		}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	for i := range store.AgentPhaseLimit {
		add(i)
	}

	seat, more, err := log.AgentPhases(t.Context(), "a-lead", nil)
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
	turns, more, err := log.TurnPartials(t.Context(), store.TurnQuery{Limit: store.AgentPhaseLimit}, time.Now())
	if err != nil {
		t.Fatalf("TurnPartials: %v", err)
	}
	if len(turns) != store.AgentPhaseLimit || more {
		t.Errorf("a turn page of exactly the turns there are: %d rows, more=%v; want %d and false",
			len(turns), more, store.AgentPhaseLimit)
	}

	// ONE MORE, and every read says so — and still answers only its page.
	add(store.AgentPhaseLimit)
	seat, more, err = log.AgentPhases(t.Context(), "a-lead", nil)
	if err != nil || len(seat) != store.AgentPhaseLimit || !more {
		t.Errorf("a seat with a page and one: %d rows, more=%v, err %v; want %d and true",
			len(seat), more, err, store.AgentPhaseLimit)
	}
	company, more, err = log.Phases(t.Context(), "", store.AgentPhaseLimit, nil)
	if err != nil || len(company) != store.AgentPhaseLimit || !more {
		t.Errorf("a company page short of one: %d rows, more=%v, err %v; want %d and true",
			len(company), more, err, store.AgentPhaseLimit)
	}
	turns, more, err = log.TurnPartials(t.Context(), store.TurnQuery{Limit: store.AgentPhaseLimit}, time.Now())
	if err != nil || len(turns) != store.AgentPhaseLimit || !more {
		t.Errorf("a turn page short of one: %d rows, more=%v, err %v; want %d and true",
			len(turns), more, err, store.AgentPhaseLimit)
	}

	// A SHARE IS NOT A PAGE: every turn it names, and never more.
	ids := []string{"p-000", "p-001"}
	share, more, err := log.TurnPartials(t.Context(), store.TurnQuery{IDs: ids, Limit: 1}, time.Now())
	if err != nil || len(share) != len(ids) || more {
		t.Errorf("a share of two named turns: %d, more=%v, err %v; want 2 and false", len(share), more, err)
	}
}

// A SEAT'S ACTIVITY FEED IS ITS OWN WHEN ITS NAME IS SHARED.
//
// The seat page's "all activity" asked the feed by `actor`, which is the seat's
// NAME: a namesake's events came back as this seat's, and the axis above them
// counted both. The listing and the axis narrow by the promoted agent id, and
// the row names it, so the dashboard filters the live rows it merges in the
// same way.
func TestASeatsFeedIsItsOwnWhenItsNameIsShared(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC().Truncate(time.Hour)

	for i, agentID := range []string{"a-ada", "a-bob"} {
		id := "turn-" + agentID
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: "agent_turn_completed", Source: "Engineer",
			Category: "lifecycle", Time: now.Add(-time.Duration(i+1) * time.Minute), Actor: "Engineer",
			Tags:    map[string]string{"agent_id": agentID, "agent_role": "Engineer", "turn_id": id},
			Payload: []byte(`{}`),
		}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}

	q := store.ListQuery{AgentID: "a-bob", Since: now.Add(-time.Hour), Until: now.Add(time.Hour)}
	rows, err := log.List(t.Context(), q)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "turn-a-bob" {
		t.Fatalf("a-bob's feed = %v, want only its own", idsOf(rows))
	}
	if rows[0].AgentID != "a-bob" {
		t.Errorf("the row names agent %q, want the seat it is about", rows[0].AgentID)
	}
	bars, err := log.Histogram(t.Context(), store.HistogramQuery{ListQuery: q, Bucket: store.BucketHour})
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}
	if bars.Total != 1 {
		t.Errorf("the axis counts %d, want the one row the listing shows", bars.Total)
	}
}

// A TURN THAT PARKED AND RESUMED ENDED ONCE.
//
// It writes two completion records — the segment that launched a coding run
// (`suspended`) and the one that ended the turn — so counting completions
// draws it twice beside a turns list that shows it once. `Suspended` tells the
// two apart, on the listing and on its axis alike, and absent it answers both.
//
// Mutation: drop the clause from the predicate, and the axis counts three.
func TestTheSuspendedFilterTellsAnEndFromAParking(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-10 * time.Minute)
	for i, payload := range []string{
		`{"turn_id":"t-1","suspended":true}`,
		`{"turn_id":"t-1","failed":true}`,
		`{"turn_id":"t-2"}`,
	} {
		if err := log.Append(t.Context(), store.EventRecord{
			ID: fmt.Sprintf("c-%d", i), Type: "agent_turn_completed", Category: "system",
			Time: at.Add(time.Duration(i) * time.Second), Payload: []byte(payload),
		}); err != nil {
			t.Fatal(err)
		}
	}
	ended, parked := false, true
	q := store.ListQuery{Type: "agent_turn_completed", Suspended: &ended, Limit: 10}
	rows, err := log.List(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(rows); len(got) != 2 || got[0] != "c-2" || got[1] != "c-1" {
		t.Errorf("the turns that ended = %v, want c-2 and c-1 — never the parking record", got)
	}
	axis, err := log.Histogram(t.Context(), store.HistogramQuery{ListQuery: q, Bucket: store.BucketHour})
	if err != nil {
		t.Fatal(err)
	}
	if axis.Total != 2 {
		t.Errorf("the axis counts %d turns ended, want 2 — one turn parked and resumed is one end", axis.Total)
	}
	q.Suspended = &parked
	rows, err = log.List(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(rows); len(got) != 1 || got[0] != "c-0" {
		t.Errorf("the parkings = %v, want only c-0", got)
	}
	q.Suspended = nil
	rows, err = log.List(t.Context(), q)
	if err != nil || len(rows) != 3 {
		t.Errorf("unfiltered: %d rows (err %v), want every completion", len(rows), err)
	}
}

// idsOf is the ids of a page, in its order.
func idsOf(rows []store.EventRecord) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}
