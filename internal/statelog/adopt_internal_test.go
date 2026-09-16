package statelog

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

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

// A MANIFEST NAMING AN IDENTITY THE FILE DOES NOT KEEP IS A CORRUPT SNAPSHOT,
// exactly as one naming the wrong sequence is.
//
// The identity is a checkpoint field like the generation and the sequence, and
// this is the recipient's last chance to notice: it runs after the transfer
// and before the install, on the file itself rather than on anything the donor
// says about it.
func TestAManifestNamingAnotherStreamInstanceIsRefused(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	applying := time.Unix(1_700_000_000, 0).UTC()
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, 1, 9, ?, 0)`,
			"CREWLET_PROBE_LOG", store.EncodeTime(applying))
		return err
	}); err != nil {
		t.Fatalf("seed the checkpoint: %v", err)
	}
	path := db.ReplicatedPath()
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	a := &Adopter{}

	honest := Manifest{Domains: map[string]DomainPosition{"probe": {
		Stream: "CREWLET_PROBE_LOG", Generation: 1, Seq: 9,
		StreamCreatedAt: applying,
	}}}
	if err := a.verifyPositions(t.Context(), path, honest); err != nil {
		t.Fatalf("a manifest naming the instant the file keeps was refused: %v", err)
	}

	lying := Manifest{Domains: map[string]DomainPosition{"probe": {
		Stream: "CREWLET_PROBE_LOG", Generation: 1, Seq: 9,
		StreamCreatedAt: applying.Add(72 * time.Hour),
	}}}
	if err := a.verifyPositions(t.Context(), path, lying); err == nil {
		t.Fatal("a manifest claiming its rows were applied against a stream the " +
			"file never names was accepted — that is the shape a donor produced " +
			"for as long as the position came from the file and the identity " +
			"came from its live broker handle")
	}

	// AND A MANIFEST MAKING NO CLAIM IS NOT A MISMATCH: the column
	// post-dates some rows, and a domain at the zero position was
	// applying nothing.
	silent := Manifest{Domains: map[string]DomainPosition{"probe": {
		Stream: "CREWLET_PROBE_LOG", Generation: 1, Seq: 9,
	}}}
	if err := a.verifyPositions(t.Context(), path, silent); err != nil {
		t.Fatalf("a manifest naming no instant was refused: %v", err)
	}
}
