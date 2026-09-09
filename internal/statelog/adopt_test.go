package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/nats-io/nats.go"
)

// joinHarness is a donor holding a real replicated estate and a joiner about
// to replace its own with it.
type joinHarness struct {
	t        *testing.T
	nc       *nats.Conn
	donorDB  *store.DB
	joiner   *store.DB
	joinPath string
	manifest statelog.Manifest
	snapPath string

	held     atomic.Int64
	released atomic.Int64
	phases   []statelog.AdoptionPhase
	closes   atomic.Int64
	reopens  atomic.Int64

	// stillUsable is step 7's re-check, nil by default. A case sets it to
	// stage the one thing the hold is a belt against: a fleet that
	// trimmed past the artefact while it was in flight.
	stillUsable func(context.Context, statelog.Manifest) error
}

func newJoinHarness(t *testing.T) *joinHarness {
	t.Helper()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})

	// The DONOR, with a snapshot of its own replicated estate.
	donorDir := t.TempDir()
	donorDB, err := store.Open(t.Context(), filepath.Join(donorDir, "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open the donor's store: %v", err)
	}
	t.Cleanup(func() {
		if err := donorDB.Close(); err != nil {
			t.Errorf("close the donor's store: %v", err)
		}
	})
	if err := donorDB.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		// A statement BATCH takes no arguments on this driver, so the
		// schema and the parameterised row go separately.
		if _, err := tx.ExecContext(t.Context(), probeDDL+`
			INSERT INTO probe_rows (position, kind, stored_at) VALUES (1, 'edit', 0);
			INSERT INTO probe_ops (op_id, subject, position, applied_at)
				VALUES ('op-1', 's', 1, 0);`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at)
				VALUES (?, 1, 4200, 0, 0)`, probeStream)
		return err
	}); err != nil {
		t.Fatalf("seed the donor: %v", err)
	}

	h := &joinHarness{t: t, nc: q.Conn()}
	snapDir := filepath.Join(donorDir, "snapshots")
	lag := uint64(0)
	snapper, err := statelog.NewSnapshotter(statelog.SnapshotDeps{
		Domains: []statelog.Registered{{
			Domain: probeDomain{},
			Health: func() statelog.Health {
				return statelog.Health{
					Position: statelog.Position{Stream: probeStream, Generation: 1, Seq: 4_200},
					CaughtUp: true,
					Lag:      &lag,
				}
			},
			StreamCreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		}},
		DB:            donorDB,
		Dir:           snapDir,
		NodeID:        "donor",
		EngineVersion: "v0.0.0-test",
		Counted:       func(context.Context) (int, error) { return 3, nil },
		Interval:      24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	m, err := snapper.Take(t.Context())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	h.manifest = m
	h.snapPath = filepath.Join(snapDir, "snapshot-4200.db")
	h.donorDB = donorDB

	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: "donor",
		Dial:   func(context.Context) (*nats.Conn, error) { return q.Conn(), nil },
		Newest: func() (statelog.Manifest, bool) { return h.manifest, true },
		Path:   func(statelog.Manifest) string { return h.snapPath },
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan struct{})
	go func() { defer close(served); _ = donor.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served })
	waitForSubject(t, h.nc, statelog.SubjectOffer)

	// The JOINER, whose own replicated estate is about to be replaced.
	joinDir := t.TempDir()
	joiner, err := store.Open(t.Context(), filepath.Join(joinDir, "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open the joiner's store: %v", err)
	}
	h.joiner = joiner
	h.joinPath = joiner.ReplicatedPath()
	t.Cleanup(func() { _ = h.joiner.Close() })
	return h
}

func (h *joinHarness) adopter(t *testing.T) *statelog.Adopter {
	t.Helper()
	a, err := statelog.NewAdopter(statelog.AdoptDeps{
		Domains:  map[string]statelog.Registered{"probe": {Domain: probeDomain{}}},
		LivePath: h.joinPath,
		NodeID:   "joiner",
		Conn:     h.nc,
		Need: func(context.Context) (map[string]uint64, map[string]uint32, error) {
			return map[string]uint64{"probe": 4_000}, map[string]uint32{"probe": 1}, nil
		},
		Hold: func(context.Context, map[string]uint64) (func(), error) {
			h.held.Add(1)
			return func() { h.released.Add(1) }, nil
		},
		Close: func(ctx context.Context) error {
			h.closes.Add(1)
			return h.joiner.Close()
		},
		Reopen: func(ctx context.Context) error {
			h.reopens.Add(1)
			db, err := store.Open(ctx, filepath.Join(filepath.Dir(h.joinPath), "node.db"),
				store.Options{})
			if err != nil {
				return err
			}
			h.joiner = db
			return nil
		},
		Record: func(_ context.Context, _ string, _ statelog.Manifest, phase statelog.AdoptionPhase) error {
			h.phases = append(h.phases, phase)
			return nil
		},
		StillUsable: h.stillUsable,
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}
	return a
}

// A NODE BELOW THE FLOOR ADOPTS A PEER'S SNAPSHOT WHOLESALE, and every claim
// the artefact makes is checked before it is installed.
//
// A node that has fallen below the trim floor cannot replay its way back: the
// records it is missing are gone. The transfer is what replaces the replay,
// and the verifications are what make accepting somebody else's database file
// safe at all.
func TestANodeBelowTheFloorAdoptsAVerifiedArtefact(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)

	m, err := h.adopter(t).Join(t.Context())
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if m.NodeID != "donor" {
		t.Fatalf("adopted an artefact from %q, want the donor's", m.NodeID)
	}

	// THE ROWS ARRIVED and the checkpoint with them, which is the whole
	// point: the position inside the file is what describes the file.
	var rows, seq int64
	if err := h.joiner.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM probe_rows`).Scan(&rows); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(),
			`SELECT seq FROM statelog_cursor WHERE stream = ?`, probeStream).Scan(&seq)
	}); err != nil {
		t.Fatalf("read the adopted estate: %v", err)
	}
	if rows != 1 || seq != 4_200 {
		t.Fatalf("the adopted estate holds %d row(s) at position %d, want 1 at 4200",
			rows, seq)
	}

	// THE DONOR'S OWN TABLES DID NOT COME WITH IT.
	var ops int64
	if err := h.joiner.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM probe_ops`).Scan(&ops)
	}); err != nil {
		t.Fatalf("read the operation ledger: %v", err)
	}
	if ops != 0 {
		t.Fatalf("the adopted estate holds %d row(s) of the DONOR's operation "+
			"ledger — a writer here would resolve its own ambiguous publish "+
			"against a peer's history", ops)
	}

	// THE HOLD WAS TAKEN BEFORE THE FETCH AND RELEASED AFTER.
	if h.held.Load() != 1 || h.released.Load() != 1 {
		t.Fatalf("the replay tail was held %d time(s) and released %d — without "+
			"the hold the trim can pass the artefact while it is in flight, and "+
			"this node installs a snapshot whose tail is already gone",
			h.held.Load(), h.released.Load())
	}

	// AND THE ADOPTION WAS RECORDED THROUGH ITS PHASES. A node that
	// crashed mid-adoption looks, from its checkpoint alone, exactly like
	// one that is caught up — the checkpoint came from the artefact.
	want := []statelog.AdoptionPhase{
		statelog.AdoptionScrubbed, statelog.AdoptionInstalled, statelog.AdoptionComplete,
	}
	if len(h.phases) != len(want) {
		t.Fatalf("the adoption recorded %v, want %v", h.phases, want)
	}
	for i := range want {
		if h.phases[i] != want[i] {
			t.Fatalf("the adoption recorded %v, want %v", h.phases, want)
		}
	}
	// AND NO PART FILE SURVIVES.
	if _, err := os.Stat(h.joinPath + statelog.AdoptPartSuffix); err == nil {
		t.Error("the part file survives beside the live database")
	}
}

// A CORRUPTED TRANSFER IS REFUSED AND NOTHING IS INSTALLED.
//
// A transfer that dropped or reordered a chunk arrives looking exactly like
// one that did not, so the checksum is the only thing between that and an
// installed database with a hole in it.
func TestACorruptedArtefactIsRefusedAndTheLiveDatabaseSurvives(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)
	// The manifest claims a checksum the bytes do not have, which is what
	// a corrupted transfer looks like from the recipient's side.
	h.manifest.SHA256 = strings.Repeat("0", 64)

	_, err := h.adopter(t).Join(t.Context())
	if !errors.Is(err, statelog.ErrNoOffer) {
		t.Fatalf("Join = %v, want ErrNoOffer", err)
	}
	if !strings.Contains(err.Error(), "hashes to") {
		t.Errorf("the refusal does not name the checksum: %v", err)
	}
	if h.closes.Load() != 0 {
		t.Fatal("the live database was closed for an artefact that never passed " +
			"verification — the install is the one place a live database is " +
			"replaced, and it must not be reached by a refused offer")
	}
	if _, err := os.Stat(h.joinPath + statelog.AdoptPartSuffix); err == nil {
		t.Error("a refused artefact was left beside the live database")
	}
	// AND THE HOLD WAS RELEASED, so a refused join does not pin the log.
	if h.held.Load() != h.released.Load() {
		t.Fatalf("the tail was held %d time(s) and released %d — a refused join "+
			"that keeps its hold stops the whole fleet trimming",
			h.held.Load(), h.released.Load())
	}
}

// AN ARTEFACT WHOSE MANIFEST DOES NOT MATCH THE FILE IS REFUSED.
//
// The checkpoint commits in the same transaction as the rows, so the position
// inside the file is the only one that describes the file — a manifest naming
// a different one is describing a different artefact, and adopting it would
// leave this node resuming above rows it does not have.
func TestAManifestTheFileDoesNotKeepIsACorruptSnapshot(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)
	pos := h.manifest.Domains["probe"]
	pos.Seq = 9_999
	h.manifest.Domains["probe"] = pos

	_, err := h.adopter(t).Join(t.Context())
	if err == nil {
		t.Fatal("an artefact whose manifest the file does not keep was adopted")
	}
	if !strings.Contains(err.Error(), "corrupt snapshot") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
	if h.closes.Load() != 0 {
		t.Fatal("the live database was closed for an artefact that failed " +
			"verification")
	}
}

// A DONOR THAT CLAIMS A SCRUB IT DID NOT DO IS REFUSED.
//
// The safety argument for accepting somebody else's database file at all is
// that everything still in it is fleet-visible. A claim nobody checks is a
// claim.
func TestADonorsScrubClaimIsCheckedRatherThanTrusted(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)
	// The manifest says a table was emptied that the artefact still holds.
	h.manifest.Scrubbed = append(h.manifest.Scrubbed, "probe_rows")

	_, err := h.adopter(t).Join(t.Context())
	if err == nil {
		t.Fatal("an artefact claiming a scrub it did not do was adopted")
	}
	if !strings.Contains(err.Error(), "fleet-visible") {
		t.Errorf("the refusal does not say why the claim matters: %v", err)
	}
}
