package api_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/estate/partmap"
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
	holdStrings(t, `export const ESTATE_MAP_STATES = \[([^\]]*)\] as const;`, states, "estate state")
	holdStrings(t, `export const HOLDER_STATES = \[([^\]]*)\] as const;`, holders, "holder state")
	holdStrings(t, `export const PARTITION_STATES = \[([^\]]*)\] as const;`, parts, "partition state")
}
