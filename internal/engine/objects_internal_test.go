package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY DECLARED TABLE IS READ BY THE PASSES THIS ENGINE STARTS.
//
// The declarations are one list (internal/objstore/references) and the
// estates the passes read them through are this engine's. A consumer
// declaring a table in a domain the engine hands no estate for would have the
// passes refuse to start at boot, logged and nothing more, and every data
// node would stop repairing and collecting — so the mismatch fails here, at
// build time, instead.
func TestThePassesReadEveryDeclaredTable(t *testing.T) {
	t.Parallel()
	n := &native{trackerReader: &tracker.Reader{}}
	if _, err := upkeep.Sources(references.All, n.objectEstates()...); err != nil {
		t.Fatalf("the passes cannot be built from the declared tables: %v", err)
	}
}

// A runtime without a tracker has no files, so no estate the passes read — and
// the engine starts no passes rather than passes over nothing.
func TestARuntimeWithNoTrackerOffersTheObjectStoreNoEstate(t *testing.T) {
	t.Parallel()
	if got := (&native{}).objectEstates(); len(got) != 0 {
		t.Fatalf("a runtime with no tracker offered %d estates", len(got))
	}
}
