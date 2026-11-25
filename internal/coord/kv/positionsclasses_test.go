package kv

import (
	"slices"
	"testing"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// coveredEarlier are the positions classes whose widest records their own
// tests hold (coord.TestEveryPositionsClassesWidestRecordFitsItsBound and the
// writers' own), beside coordtest.WidestPositionsRecords.
var coveredEarlier = []string{coord.MCPListingClass, coord.MCPDefinitionClass, coord.MCPCheckClass,
	coord.AccessReviewClass, coord.SeatAccessClass, coord.HomeBoardClass, coord.AtlasWakeClass,
	coord.DeviceSignInClass}

// EVERY POSITIONS FAMILY IS DECLARED AT THE BOUND ITS WIDEST RECORD FITS: the
// register's holding gives each family's keys a class of their own, never its
// rest, held to the bound its record's widest is proved against
// (coord.TestEveryPositionsFamilysWidestRecordFitsItsClass), and declares no
// class nothing writes.
//
// Mutation: drop the seat pause's entry from positionsHolding, or hold the
// OAuth grants to coord.SmallValueBytes, and this is red.
func TestEveryPositionsFamilyIsDeclaredAtTheBoundItsWidestRecordFits(t *testing.T) {
	t.Parallel()
	widest := coordtest.WidestPositionsRecords()
	for _, w := range widest {
		k, declared := declaredClass(positionsHolding, w.Key)
		switch {
		case !declared:
			t.Errorf("%s's key %q falls to the register's rest", w.Class, w.Key)
		case k.class.bound != w.Bound:
			t.Errorf("%s is declared at %d bytes, want the %d its widest record is proved against",
				w.Class, k.class.bound, w.Bound)
		}
	}
	for _, k := range positionsHolding.classes {
		if !slices.ContainsFunc(widest, func(w coordtest.PositionsWidest) bool {
			return server.SubjectMatchesFilter(w.Key, k.keys)
		}) && !slices.ContainsFunc(coveredEarlier, func(class string) bool {
			return k.keys == coord.DocumentFilter(class)
		}) {
			t.Errorf("the register declares %q, which no widest record is written under", k.keys)
		}
	}
}

// declaredClass is the keyClass of h key matches, false where it falls to
// h's rest.
func declaredClass(h holding, key string) (keyClass, bool) {
	for _, k := range h.classes {
		if server.SubjectMatchesFilter(key, k.keys) {
			return k, true
		}
	}
	return keyClass{}, false
}
