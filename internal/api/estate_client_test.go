package api_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/membership"
)

// The estate screen's closed sets the dashboard keeps its own copy of, held
// against the engine's in both directions — see internal/clientsource for why a
// copy exists at all and why this side checks it.
//
// A state the engine sends and the dashboard does not name falls to a
// renderer's default arm — a holder the screen draws as neither serving nor
// joining, an estate state it draws as an empty map — and one the dashboard
// names and the engine never sends is a branch nothing reaches.
func TestTheDashboardKnowsEveryEstateState(t *testing.T) {
	t.Parallel()
	var states, holders, parts []string
	for _, s := range queries.EstateMapStates() {
		states = append(states, string(s))
	}
	for _, s := range partmap.HolderStates {
		holders = append(holders, string(s))
	}
	for _, s := range partmap.PartitionStates {
		parts = append(parts, string(s))
	}
	tree := clientsource.Tree(t)
	holdStrings(t, tree, "ESTATE_MAP_STATES", states, "estate state")
	holdStrings(t, tree, "HOLDER_STATES", holders, "holder state")
	holdStrings(t, tree, "PARTITION_STATES", parts, "partition state")
}

// EVERY HOLD LENGTH THE DASHBOARD OFFERS IS ONE THE ENGINE ACCEPTS, AND THE
// LONGEST IS ITS CEILING.
//
// A length past membership.MaxHold is a choice the estate route answers `invalid_hold`
// on every press; a list that stopped short of it hides the day-long hold a
// long maintenance needs. The copy exists because the dashboard is its own
// build — see internal/clientsource — so this side holds it.
func TestTheDashboardOffersHoldLengthsTheEngineAccepts(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "HOLD_LENGTHS")
	if err != nil {
		t.Fatal(err)
	}
	lengths := clientsource.Strings(body)
	if len(lengths) == 0 {
		t.Fatal("the dashboard declares no hold lengths at all, so this gate certifies nothing")
	}
	longest := time.Duration(0)
	for _, l := range lengths {
		d, err := time.ParseDuration(l)
		if err != nil || d <= 0 || d > membership.MaxHold {
			t.Errorf("the dashboard offers a hold of %q, which the engine refuses: it "+
				"takes more than nothing and at most %s", l, membership.MaxHold)
		}
		longest = max(longest, d)
	}
	if longest != membership.MaxHold {
		t.Errorf("the dashboard's longest hold is %s and the engine's ceiling %s",
			longest, membership.MaxHold)
	}
}
