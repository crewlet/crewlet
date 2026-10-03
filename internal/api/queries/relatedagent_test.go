package queries_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/store"
)

// ONE AGENT'S RELATED EVENTS, NEVER ITS NAMESAKE'S.
//
// `events?agent=` matched through a party index keyed on NAMES — the actor,
// the `agent_role` tag and the participant tags as they came — so two seats
// both called "Engineer" listed each other's work, and a vendor delivery
// addressed to one was on the other's page too once the second took the same
// name. The index is keyed on agent ids and the parameter, a handle, is
// resolved to one through the chart this node holds, as the event log resolved
// each event's participants when it was written.
//
// Mutation: compare the handle against the index as it came, and ada's own
// events drop out; key the index on `agent_role` again, and bob's phase is
// listed as ada's.
func TestTheRelatedFilterIsOneAgentsNeverItsNamesakes(t *testing.T) {
	t.Parallel()
	cfg, ids := namesakes(t)
	db := openStore(t)
	roster := companySource(t, cfg)
	db.SetEventSeats(func(handle string) (string, bool) {
		_, o := roster()
		id, ok := o.AgentIDFor(o.AgentSeatByHandle(handle))
		if !ok {
			return "", false
		}
		return id.String(), true
	})
	log := db.Events()
	at := time.Now().UTC().Add(-time.Minute)
	for _, rec := range []store.EventRecord{
		{ID: "ada-phase", Type: "agent_phase_completed", Time: at,
			Category: "system", Actor: "Engineer", Tags: map[string]string{
				"agent_id": ids["ada"], "agent_role": "Engineer"}},
		{ID: "bob-phase", Type: "agent_phase_completed", Time: at.Add(time.Second),
			Category: "system", Actor: "Engineer", Tags: map[string]string{
				"agent_id": ids["bob"], "agent_role": "Engineer"}},
		{ID: "to-ada", Type: "webhook:push", Source: "github",
			Time: at.Add(2 * time.Second), Category: "webhook", Actor: "github",
			Tags: map[string]string{"recipient": "ada"}},
	} {
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}

	r := registryOver(t, queries.Sources{State: livestate.New(), Events: fleetOf(log),
		Company: roster})
	got := ask(t, r, "events", map[string]any{"agent": "ada"})
	rows, _ := got["events"].([]store.EventRecord)
	var listed []string
	for _, row := range rows {
		listed = append(listed, row.ID)
	}
	slices.Sort(listed)
	if !slices.Equal(listed, []string{"ada-phase", "to-ada"}) {
		t.Errorf("ada's related events are %v, want her own phase and the "+
			"delivery addressed to her — never bob's", listed)
	}

	// A HANDLE NO AGENT SEAT ANSWERS TO IS REFUSED, never compared: there
	// is no id it could match, and an empty page reads as an agent that
	// has done nothing.
	if _, err := r.Answer(everyGrant(t), "events", map[string]any{"agent": "nobody"}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a handle nobody answers to answered %v, want %v", err,
			queries.ErrBadParams)
	}
}
