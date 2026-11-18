package statelog

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// AN ABSENT CHECKPOINT ROW VERIFIES AS THE ZERO POSITION, and as nothing else.
// The donor stamps a domain that applied nothing at zero: refusing the absent
// row would refuse every artefact made before the newest domain's first
// record, and accepting any position would install a claim the file lacks.
func TestAnAbsentCheckpointVerifiesOnlyAsTheZeroPosition(t *testing.T) {
	t.Parallel()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	path := storetest.ReplicatedDB(t, estate).Path()
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
// as one naming the wrong sequence is: this is the recipient's last check,
// after the transfer and before the install, on the file itself.
func TestAManifestNamingAnotherStreamInstanceIsRefused(t *testing.T) {
	t.Parallel()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	applying := time.Unix(1_700_000_000, 0).UTC()
	if err := estate.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at, applied_version)
			VALUES (?, 1, 9, ?, 0, 0)`,
			"CREWLET_PROBE_LOG", store.EncodeTime(applying))
		return err
	}); err != nil {
		t.Fatalf("seed the checkpoint: %v", err)
	}
	path := storetest.ReplicatedDB(t, estate).Path()
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
	// post-dates some rows.
	silent := Manifest{Domains: map[string]DomainPosition{"probe": {
		Stream: "CREWLET_PROBE_LOG", Generation: 1, Seq: 9,
	}}}
	if err := a.verifyPositions(t.Context(), path, silent); err != nil {
		t.Fatalf("a manifest naming no instant was refused: %v", err)
	}
}
