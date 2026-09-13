package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// snapHarness is a node with a replicated estate, a domain and somewhere to
// write snapshots.
type snapHarness struct {
	t      *testing.T
	db     *store.DB
	dir    string
	health statelog.Health
	nodes  int
	snap   *statelog.Snapshotter
}

func newSnapHarness(t *testing.T) *snapHarness {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), probeDDL)
		return err
	}); err != nil {
		t.Fatalf("create the probe domain's tables: %v", err)
	}

	h := &snapHarness{
		t:     t,
		db:    db,
		dir:   filepath.Join(dir, "snapshots"),
		nodes: 3,
	}
	lag := uint64(0)
	first := uint64(1)
	floor := uint64(1)
	h.health = statelog.Health{
		Position:  statelog.Position{Stream: probeStream, Generation: 1, Seq: 4_200},
		CaughtUp:  true,
		Floor:     statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()},
		Lag:       &lag,
		FirstSeq:  &first,
		TrimFloor: &floor,
	}
	h.rebuild(24 * time.Hour)
	return h
}

func (h *snapHarness) rebuild(interval time.Duration) {
	h.t.Helper()
	s, err := statelog.NewSnapshotter(statelog.SnapshotDeps{
		Domains: []statelog.Registered{{
			Domain:          probeDomain{},
			Health:          func() statelog.Health { return h.health },
			StreamCreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		}},
		DB:            h.db,
		Dir:           h.dir,
		NodeID:        "node-a",
		EngineVersion: "v0.0.0-test",
		Counted:       func(context.Context) (int, error) { return h.nodes, nil },
		Interval:      interval,
	})
	if err != nil {
		h.t.Fatalf("NewSnapshotter: %v", err)
	}
	h.snap = s
}

// A SNAPSHOT NAMES A POSITION PER DOMAIN, and the manifest is written LAST.
//
// One artefact serves every registered domain because it is a copy of one
// file — which is exactly why the manifest names each of them, and why a
// recipient refuses one that does not name every domain its own build
// registers. And the manifest's PRESENCE is the claim: a copy with no manifest
// beside it is the debris of a run that did not finish.
func TestASnapshotNamesEveryDomainAndItsManifestIsTheClaim(t *testing.T) {
	t.Parallel()
	h := newSnapHarness(t)

	m, err := h.snap.Take(t.Context())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(m.Domains) != 1 {
		t.Fatalf("the manifest names %d domain(s), want 1", len(m.Domains))
	}
	got, ok := m.Domains["probe"]
	if !ok {
		t.Fatal("the manifest does not name the domain this build registers")
	}
	if got.Seq != h.health.Position.Seq {
		t.Errorf("the manifest names position %d, want %d", got.Seq, h.health.Position.Seq)
	}
	if want := (probeDomain{}).RecordVersion(); got.RecordVersion != want {
		t.Errorf("the manifest names record version %d, want %d — a recipient "+
			"refuses a donor whose build read more than its own",
			got.RecordVersion, want)
	}
	if got.Replay != statelog.ReplayStrict {
		t.Errorf("the manifest names replay %q — a recipient whose build "+
			"declares another refuses, because adopting a compacted position "+
			"into a strict loop is a permanent stall", got.Replay)
	}
	if got.StreamCreatedAt.IsZero() {
		t.Error("the manifest carries no stream creation instant — that is what " +
			"DETECTS a recreated stream, and the generation is only the response")
	}
	if m.SHA256 == "" || m.Bytes == 0 {
		t.Errorf("the manifest carries no checksum or size: %+v", m)
	}

	// THE PAIR IS ON DISK and the manifest reads back.
	base := filepath.Join(h.dir, "snapshot-4200")
	if _, err := os.Stat(base + ".db"); err != nil {
		t.Fatalf("the copy is not at %s: %v", base+".db", err)
	}
	back, err := statelog.ReadManifest(base + ".json")
	if err != nil {
		t.Fatalf("read the manifest back: %v", err)
	}
	if back.SHA256 != m.SHA256 {
		t.Error("the manifest on disk differs from the one returned")
	}
	// AND NO PART FILE SURVIVES: the rename is what makes the presence a
	// claim, so a leftover part would be a claim nobody made.
	if _, err := os.Stat(base + ".json.part"); err == nil {
		t.Error("a part file survives beside the manifest")
	}
}

// THE DONOR SCRUBS, ON A LIST DERIVED FROM THE DOMAIN'S DECLARATION.
//
// An offered artefact carries no authority a peer does not already hold only
// once every table still in it is fleet-visible — so a table this node owns
// must be empty before the artefact exists, not after it arrives. And the list
// comes from what the domain says about its own tables: a hardcoded one would
// silently omit whatever a deployment actually has.
func TestADonorScrubsItsOwnTablesBeforeItOffersAnything(t *testing.T) {
	t.Parallel()
	h := newSnapHarness(t)
	// A row in the replicated table that travels, and rows in the two the
	// domain classes Local.
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO probe_rows (position, kind, stored_at) VALUES (1, 'edit', 0);
			INSERT INTO probe_ops (op_id, subject, position, applied_at)
				VALUES ('op-1', 's', 1, 0);`)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m, err := h.snap.Take(t.Context())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(m.Scrubbed) == 0 {
		t.Fatal("the manifest claims nothing was scrubbed")
	}
	for _, want := range []string{"probe_ops"} {
		if !slicesContains(m.Scrubbed, want) {
			t.Errorf("the manifest does not list %s as scrubbed: %v", want, m.Scrubbed)
		}
	}
	// THE CLAIM IS TRUE, checked the way a recipient checks it.
	copyPath := filepath.Join(h.dir, "snapshot-4200.db")
	empty, err := store.EmptyTables(t.Context(), copyPath, m.Scrubbed)
	if err != nil {
		t.Fatalf("verify the scrub: %v", err)
	}
	if len(empty) != len(m.Scrubbed) {
		t.Fatalf("the artefact claims %v scrubbed and %v are actually empty — a "+
			"claim nobody checks is a claim", m.Scrubbed, empty)
	}
	// AND WHAT TRAVELS IS STILL THERE.
	remaining, err := store.EmptyTables(t.Context(), copyPath, []string{"probe_rows"})
	if err != nil {
		t.Fatalf("check the replicated table: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatal("the replicated table was scrubbed too — the artefact would " +
			"carry a checkpoint and none of the rows it covers")
	}
}

// EVERY PRECONDITION PUBLISHES ITS OWN REASON.
//
// A fleet that has silently stopped snapshotting is invisible until a node
// needs one, and three of these are routine rather than exotic: a rolling
// upgrade produces `deferred`, a full volume produces `insufficient_space`,
// and a two-node fleet after one eviction produces `sole_node`.
func TestEverySnapshotPreconditionSaysWhyItSkipped(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		arrange func(*snapHarness)
		want    statelog.SkipReason
	}{
		"nobody to donate to": {
			arrange: func(h *snapHarness) { h.nodes = 1 },
			want:    statelog.SkipSoleNode,
		},
		"holding a record it cannot decode": {
			arrange: func(h *snapHarness) {
				h.health.Deferred = 2
				h.health.DeferredFrom = 7
			},
			want: statelog.SkipDeferred,
		},
		"never drained the log": {
			arrange: func(h *snapHarness) { h.health.CaughtUp = false },
			want:    statelog.SkipUnhydrated,
		},
		"too far behind to be worth transferring": {
			arrange: func(h *snapHarness) {
				lag := uint64(statelog.SnapshotLagSlack + 1)
				h.health.Lag = &lag
			},
			want: statelog.SkipLagging,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newSnapHarness(t)
			tc.arrange(h)
			_, err := h.snap.Take(t.Context())
			reason, ok := statelog.Skipped(err)
			if !ok {
				t.Fatalf("Take = %v, want a skip", err)
			}
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			var skip *statelog.ErrSkipped
			if errors.As(err, &skip) && skip.Detail == "" {
				t.Error("the skip says nothing an operator could act on")
			}
			if _, err := os.Stat(h.dir); err == nil {
				entries, _ := os.ReadDir(h.dir)
				if len(entries) != 0 {
					t.Errorf("a skipped tick left %d file(s) behind", len(entries))
				}
			}
		})
	}

	// AND THE INTERVAL, which needs a snapshot to already exist.
	t.Run("one was taken recently", func(t *testing.T) {
		t.Parallel()
		h := newSnapHarness(t)
		if _, err := h.snap.Take(t.Context()); err != nil {
			t.Fatalf("the first take: %v", err)
		}
		_, err := h.snap.Take(t.Context())
		reason, ok := statelog.Skipped(err)
		if !ok || reason != statelog.SkipRecent {
			t.Fatalf("a second take inside the interval = %v, want a recent skip", err)
		}
	})
}

// THE PREVIOUS SNAPSHOT GOES LAST.
//
// Deleting first leaves a window in which this node can donate nothing at all,
// which costs the fleet a donor for the length of a full copy. The price is
// transiently twice the store, and it is the right way round.
func TestTheOldSnapshotIsRemovedOnlyAfterTheNewOneIsComplete(t *testing.T) {
	t.Parallel()
	h := newSnapHarness(t)
	if _, err := h.snap.Take(t.Context()); err != nil {
		t.Fatalf("the first take: %v", err)
	}
	// A second take at a later position, with the interval out of the way.
	h.health.Position.Seq = 9_000
	h.rebuild(time.Nanosecond)
	if _, err := h.snap.Take(t.Context()); err != nil {
		t.Fatalf("the second take: %v", err)
	}

	names := entryNames(t, h.dir)
	if len(names) != 2*statelog.SnapshotsKept {
		t.Fatalf("the directory holds %v, want one pair — the fleet is the "+
			"redundancy, so a second copy on this disk protects against nothing "+
			"the first does not", names)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "snapshot-9000") {
			t.Fatalf("the directory holds %q, which is not the newest pair", n)
		}
	}
}

// AND A TAKE THAT FAILS LEAVES THE PREVIOUS SNAPSHOT INTACT.
//
// This is the half the end state cannot show. Rotating first would cost the
// fleet a donor for the whole length of a copy — and for ever, if the copy
// then fails. The staging makes the manifest's own publish fail, which is the
// last step: everything before it succeeded, so what survives is decided
// entirely by the order.
func TestAFailedTakeLeavesThePreviousSnapshotWhereItIs(t *testing.T) {
	t.Parallel()
	h := newSnapHarness(t)
	if _, err := h.snap.Take(t.Context()); err != nil {
		t.Fatalf("the first take: %v", err)
	}
	before := entryNames(t, h.dir)

	// A directory where the next manifest has to go, so its rename fails
	// after the copy and the scrub have both succeeded.
	h.health.Position.Seq = 9_000
	h.rebuild(time.Nanosecond)
	blocked := filepath.Join(h.dir, "snapshot-9000.json")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatalf("stage the failure: %v", err)
	}
	// Not empty, so the take's own cleanup of a previous part file cannot
	// remove it and the rename genuinely has nowhere to go.
	if err := os.WriteFile(filepath.Join(blocked, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatalf("stage the failure: %v", err)
	}

	if _, err := h.snap.Take(t.Context()); err == nil {
		t.Fatal("a take whose manifest could not be published reported success")
	}
	after := entryNames(t, h.dir)
	for _, want := range before {
		if !slicesContains(after, want) {
			t.Fatalf("%s is gone after a failed take — the previous snapshot is "+
				"this node's only donation until a new one is COMPLETE, and "+
				"removing it first costs the fleet a donor for the whole length "+
				"of a copy", want)
		}
	}
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A COPY WITH NO MANIFEST IS DEBRIS, not a snapshot.
//
// Treating one as a snapshot would let a single crashed attempt suppress every
// later one through the interval gate — a node that silently stops donating
// because one run died.
func TestACopyWithNoManifestDoesNotCountAsASnapshot(t *testing.T) {
	t.Parallel()
	h := newSnapHarness(t)
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		t.Fatalf("create the snapshot directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "snapshot-1.db"), []byte("debris"), 0o600); err != nil {
		t.Fatalf("stage the debris: %v", err)
	}

	if _, err := h.snap.Take(t.Context()); err != nil {
		t.Fatalf("Take with a manifest-less copy present = %v, want a snapshot — "+
			"a crashed attempt must not suppress every later one", err)
	}
}

func slicesContains(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}
