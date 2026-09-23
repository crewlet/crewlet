package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// A NODE'S LOCAL COPY KEEPS THE INSTANT THE POINTER PUBLISHED IT AT.
//
// The pointer publishes an instant later than the activation it replaces
// (coord.ActivationAt), so a node whose clock runs behind the one that
// activated last is published later than it asked. Its local row's
// `activated_at` is what it boots its chart with next time — "one instant for
// both halves" — so the row has to carry the pointer's, or a restart stamps
// the chart with an instant the fleet never applied.
func TestAPublishKeepsThePointersInstantLocally(t *testing.T) {
	t.Parallel()

	t.Run("an import over a fleet whose last activation is ahead", func(t *testing.T) {
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), nil, quiet()); err != nil {
			t.Fatalf("seed: %v", err)
		}
		first, _, err := db.Configs().Active(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// A PEER WITH A CLOCK TWO HOURS AHEAD re-activates the same
		// revision — the credential-rotation gesture.
		if _, err = fleet.Activate(t.Context(), coord.ActivationRequest{
			RevisionID: first.ID, Payload: first.Payload, At: time.Now().Add(2 * time.Hour),
		}); err != nil {
			t.Fatalf("peer activate: %v", err)
		}
		changed := strings.Replace(companyYAML, "name: Acme", "name: Acme Imported", 1)
		if err = seedCompany(t.Context(), db, fleet, nil, overrideOf(parse(t, changed)), nil, quiet()); err != nil {
			t.Fatalf("import: %v", err)
		}
		assertLocalCopyIsThePointers(t, db, fleet)
	})

	t.Run("a boot publish that lands on a pointer it did not read", func(t *testing.T) {
		t.Parallel()
		db := seedStore(t)
		if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seedOf(parse(t, companyYAML)), nil, quiet()); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// THE POINTER MOVES BETWEEN THE BOOT'S READ AND ITS WRITE: a peer's
		// activation, ahead, that the read did not see.
		real := coordmemory.NewFleet()
		if _, err := real.Activate(t.Context(), coord.ActivationRequest{
			RevisionID: "peer-revision", Payload: []byte(`{"name":"Peer"}`),
			At: time.Now().Add(2 * time.Hour),
		}); err != nil {
			t.Fatalf("peer activate: %v", err)
		}
		if err := seedCompany(t.Context(), db, unreadPointer{real}, nil, seedOf(parse(t, companyYAML)), nil, quiet()); err != nil {
			t.Fatalf("restart: %v", err)
		}
		assertLocalCopyIsThePointers(t, db, real)
	})
}

// unreadPointer is a fleet whose pointer a read never sees, standing in for a
// peer's activation landing between a node's read of it and its own write.
type unreadPointer struct{ coord.Plane }

func (unreadPointer) Target(context.Context) (coord.Activation, bool, error) {
	return coord.Activation{}, false, nil
}

func assertLocalCopyIsThePointers(t *testing.T, db *store.DB, fleet *coordmemory.Fleet) {
	t.Helper()
	target, found, err := fleet.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("target: found=%v err=%v", found, err)
	}
	active, _, err := db.Configs().Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != target.RevisionID ||
		store.EncodeTime(active.ActivatedAt) != store.EncodeTime(target.At) {
		t.Fatalf("this node holds %s activated at %s, and the pointer names %s at "+
			"%s — a restart would stamp the chart with an instant the fleet never "+
			"applied", active.ID, active.ActivatedAt, target.RevisionID, target.At)
	}
}
