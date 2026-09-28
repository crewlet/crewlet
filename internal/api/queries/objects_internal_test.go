package queries

import (
	"testing"

	"github.com/google/uuid"

	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/placement"
)

// A LAYOUT IS COMPUTED ONCE PER MAP, and a map that places differently is
// never answered from the last one's layout — whatever its epoch says.
//
// The first half is what keeps the fleet poll from spending groups × members
// draws every fifteen seconds per open tab; the second is what stops the cache
// rendering every share on the screen wrong after a writer changed the map
// without moving its epoch.
func TestTheFleetComputesALayoutOncePerMap(t *testing.T) {
	t.Parallel()
	m := objplacement.Map{
		Generation: uuid.MustParse("5b0c1f7e-9c1d-4f5e-8a3b-2d7c6e1f0a9b"),
		Epoch:      4, Replicas: 2, PGBits: objplacement.MinPGBits,
		Members: []placement.Member{
			{Node: "a", Weight: 1, Share: placement.DefaultShare(1)},
			{Node: "b", Weight: 1, Share: placement.DefaultShare(1)},
			{Node: "c", Weight: 1, Share: placement.DefaultShare(1)},
		},
	}
	cache := &objectLayouts{}
	first := cache.of(m)
	if again := cache.of(m); again != first {
		t.Error("the same map computed a second layout")
	}

	// A CALLER'S SLICE IS NOT THE CACHE'S: a map decoded afresh for the
	// next request is equal, not identical, and the cache must not be
	// changed by a caller writing into the one it was handed.
	decoded := m
	decoded.Members = append([]placement.Member(nil), m.Members...)
	if again := cache.of(decoded); again != first {
		t.Error("an equal map decoded afresh computed a second layout")
	}
	m.Members[0].Share *= 3
	if again := cache.of(decoded); again != first {
		t.Error("writing into a caller's members changed what the cache holds")
	}

	moved := decoded
	moved.Members = append([]placement.Member(nil), decoded.Members...)
	moved.Members[2].Out = true
	if again := cache.of(moved); again == first {
		t.Error("a map with a member taken out was answered from the last map's layout " +
			"because its epoch did not move")
	}
	if cache.of(moved).Map().Members[2].Out != true {
		t.Error("the cache answered a layout for a different map")
	}
}
