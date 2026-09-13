package statelog

import (
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// AN ABSENT CHECKPOINT ROW VERIFIES AS THE ZERO POSITION, and as nothing else.
//
// The donor stamps a domain that has applied nothing at zero, because that is
// what the file says about it. A recipient that refused the absent row would
// refuse every artefact a fleet produced before its newest domain's first
// record; one that accepted any position for it would install a manifest the
// file does not keep.
func TestAnAbsentCheckpointVerifiesOnlyAsTheZeroPosition(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	path := db.ReplicatedPath()
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	a := &Adopter{}
	zero := Manifest{Domains: map[string]DomainPosition{
		"probe": {Stream: "CREWLET_PROBE_LOG"},
	}}
	if err := a.verifyPositions(t.Context(), path, zero); err != nil {
		t.Fatalf("a manifest naming zero for a domain with no checkpoint row was "+
			"refused: %v", err)
	}
	ahead := Manifest{Domains: map[string]DomainPosition{
		"probe": {Stream: "CREWLET_PROBE_LOG", Generation: 1, Seq: 5},
	}}
	if err := a.verifyPositions(t.Context(), path, ahead); err == nil {
		t.Fatal("a manifest naming 1/5 for a domain with no checkpoint row was " +
			"accepted — a metadata claim the file does not keep is a corrupt snapshot")
	}
}
