package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
)

// bodyRefused is a fleet whose every read of a body fails, so a boot that
// reads one is caught by what it does next.
type bodyRefused struct{ coord.Plane }

func (bodyRefused) Body(context.Context, coord.Activation) (json.RawMessage, bool, error) {
	return nil, false, errors.New("the body was read")
}

// A BOOT READS NO BODY OF A REVISION ITS HISTORY HOLDS. A node started with
// -import-company naming the file the fleet runs compares the two at every
// boot. Where its own history holds the fleet's revision it compares against
// that, as every other reader of the pointer does, rather than pay the body's
// crossing: minutes on a leaf's link for a company at its bound.
//
// Mutation: read the body whatever the history holds, and the comparison
// fails, so the override imports the file again and the fleet moves.
func TestABootReadsNoBodyOfARevisionItsHistoryHolds(t *testing.T) {
	t.Parallel()
	file := parse(t, companyYAML)
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := bootSeed(t, db, fleet, seedOf(file), fixtureCipher); err != nil {
		t.Fatalf("the first boot: %v", err)
	}
	before, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := decideSeed(t.Context(), &config.Bootstrap{}, overrideOf(file),
		storeOf(db, fixtureCipher), quiet())
	if err != nil {
		t.Fatal(err)
	}
	if err := seedCompany(t.Context(), db, bodyRefused{fleet}, nil, plan, fixtureCipher,
		testNode, builds, quiet()); err != nil {
		t.Fatalf("a boot overriding with the file the fleet runs: %v", err)
	}
	assertFleetOn(t, fleet, before)
}
