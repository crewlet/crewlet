package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// liveStreamCreatedAt is the instant the probe stream was created, as both
// this node's live registration and its checkpoint row report it. A case that
// needs them to DISAGREE moves the file's copy, which is what a donor
// restarted onto a rebuilt stream holds.
var liveStreamCreatedAt = time.Unix(1_700_000_000, 0).UTC()

// snapHarness is a node with a replicated estate, a domain and somewhere to
// write snapshots.
type snapHarness struct {
	t      *testing.T
	db     *store.DB
	dir    string
	health statelog.Health
	nodes  int
	snap   *statelog.Snapshotter

	// clock is what the snapshotter reads as "now", pinned so a take's own
	// instant — which is part of its file name — is a value a case can
	// compute. A case taking two snapshots moves it, exactly as the wall
	// clock would.
	clock time.Time

	// created is the stream instance the FILE says its checkpoint was
	// applying, which is a different value from the one the registration
	// below reports live — they agree by default and a case that pulls
	// them apart is a donor that was restarted onto a rebuilt stream.
	created time.Time
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
		t:       t,
		db:      db,
		dir:     filepath.Join(dir, "snapshots"),
		nodes:   3,
		created: liveStreamCreatedAt,
		clock:   liveStreamCreatedAt.Add(24 * time.Hour),
	}
	// THE FILE HOLDS THE POSITION. The manifest is stamped from the
	// checkpoint the copy keeps, so the harness seeds one; the health below
	// is what the GATE reads, and the two are deliberately separate values.
	h.cursor(4_200)
	lag := uint64(0)
	first := uint64(1)
	floor := uint64(1)
	h.health = statelog.Health{
		Position:  statelog.Position{Stream: probeStream, Generation: 1, Seq: 4_200},
		Drained:   true,
		Floor:     statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()},
		Lag:       &lag,
		FirstSeq:  &first,
		TrimFloor: &floor,
	}
	h.rebuild(24 * time.Hour)
	return h
}

// cursor commits the probe domain's checkpoint in the replicated estate, which
// is what a real applier does with every batch.
func (h *snapHarness) cursor(seq uint64) {
	h.t.Helper()
	if err := h.db.Replicated().Tx(h.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, 1, ?, ?, 0)
			ON CONFLICT (stream) DO UPDATE SET
				seq = excluded.seq, stream_created_at = excluded.stream_created_at`,
			probeStream, int64(seq), store.EncodeTime(h.created))
		return err
	}); err != nil {
		h.t.Fatalf("commit the checkpoint at %d: %v", seq, err)
	}
}

func (h *snapHarness) rebuild(interval time.Duration) {
	h.t.Helper()
	s, err := statelog.NewSnapshotter(statelog.SnapshotDeps{
		Domains: []statelog.Registered{{
			Domain: probeDomain{},
			Health: func() statelog.Health { return h.health },
		}},
		DB:            h.db,
		Dir:           h.dir,
		NodeID:        "node-a",
		EngineVersion: "v0.0.0-test",
		Counted:       func(context.Context) (int, error) { return h.nodes, nil },
		Interval:      interval,
		Now:           func() time.Time { return h.clock },
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
	if got.Seq != 4_200 || got.Generation != 1 {
		t.Errorf("the manifest names position %d/%d, want 1/4200", got.Generation, got.Seq)
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

	// THE PAIR IS ON DISK, at the name the manifest itself carries, and
	// the manifest reads back.
	if m.Artifact == "" {
		t.Fatal("the manifest does not name its own file, so nothing can find it")
	}
	if !strings.HasPrefix(m.Artifact, "snapshot-4200-") {
		t.Errorf("the artefact is called %q — the position belongs in the name "+
			"an operator reads, and the take's own instant is what makes it "+
			"unique", m.Artifact)
	}
	base := filepath.Join(h.dir, strings.TrimSuffix(m.Artifact, ".db"))
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
	copyPath := filepath.Join(h.dir, m.Artifact)
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
			arrange: func(h *snapHarness) { h.health.Drained = false },
			want:    statelog.SkipUnhydrated,
		},
		// THE ONE THE OTHER TERMS CANNOT SEE. Lag is clamped at zero, so
		// a checkpoint PAST the log's end reports zero records behind and
		// caught up, and every other precondition here passes a node
		// whose rows are keyed to a sequence space the stream no longer
		// has — which is exactly what a deleted-and-recreated stream
		// leaves behind, and exactly the artefact a joiner must never be
		// handed.
		"its checkpoint is past the log's end": {
			arrange: func(h *snapHarness) {
				end := uint64(100)
				h.health.LastSeq = &end
			},
			want: statelog.SkipAheadOfLog,
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
	// A second take at a later position, with the interval out of the way
	// and the clock moved on — a take's own instant is part of its name,
	// so two takes at one instant are one take.
	h.cursor(9_000)
	h.health.Position.Seq = 9_000
	h.clock = h.clock.Add(time.Hour)
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
		if !strings.HasPrefix(n, "snapshot-9000-") {
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
	h.cursor(9_000)
	h.health.Position.Seq = 9_000
	h.clock = h.clock.Add(time.Hour)
	h.rebuild(time.Nanosecond)
	blocked := filepath.Join(h.dir, fmt.Sprintf("snapshot-9000-%d.json",
		h.clock.UTC().UnixNano()))
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
	for _, n := range after {
		if strings.HasPrefix(n, "snapshot.part") {
			t.Fatalf("a failed take left %q behind — a part file is debris, and "+
				"its sidecars would be applied to the next copy written under "+
				"that name", n)
		}
	}
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

// THE MANIFEST NAMES THE POSITION THE FILE KEEPS, not the one the node was
// at when it decided to take a snapshot.
//
// A recipient verifies the manifest against the checkpoint inside the copy and
// refuses one that differs, because a metadata claim the file does not keep is
// a corrupt snapshot. The checkpoint commits with the rows, so on a node that
// is applying, the file the copy captures is ahead of any health reading
// taken before it — by however many records landed in between. Stamped from
// health, every snapshot taken under write load was refused by the joiner that
// needed it, and only an idle fleet's snapshots were ever adoptable.
//
// The applier's commit between the health read and the copy is staged
// deterministically: the health the gate reads says 4 200 while the file the
// copy is taken from already holds 4 207.
func TestASnapshotNamesThePositionTheFileKeeps(t *testing.T) {
	t.Parallel()
	h := newSnapHarness(t)
	h.cursor(4_207)

	m, err := h.snap.Take(t.Context())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	copyPath := filepath.Join(h.dir, m.Artifact)
	// THE ARTEFACT IS ONE FILE, checked BEFORE this test opens it: the
	// donor read the copy's checkpoint, which grows a -wal and a lock
	// beside it, and neither may travel or be digested around.
	for _, n := range entryNames(t, h.dir) {
		if strings.Contains(n, ".db-") || strings.HasSuffix(n, ".lock") {
			t.Fatalf("the snapshot directory holds %q beside the artefact", n)
		}
	}
	digest, err := store.FileDigest(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if digest != m.SHA256 {
		t.Fatal("the manifest's checksum does not cover the bytes on disk")
	}

	inFile, err := statelog.CursorsInFile(t.Context(), copyPath)
	if err != nil {
		t.Fatalf("read the copy's checkpoint the way a recipient does: %v", err)
	}
	kept, ok := inFile[probeStream]
	if !ok {
		t.Fatalf("the copy holds no checkpoint for %s", probeStream)
	}
	named := m.Domains["probe"]
	if named.Seq != kept.Position.Seq || named.Generation != kept.Position.Generation {
		t.Fatalf("the manifest names %d/%d and the file keeps %d/%d — a recipient "+
			"refuses exactly this, so a snapshot taken while the applier commits "+
			"would be unadoptable", named.Generation, named.Seq,
			kept.Position.Generation, kept.Position.Seq)
	}
	if named.Seq != 4_207 {
		t.Fatalf("the manifest names %d, want the file's 4207", named.Seq)
	}
}

// A DOMAIN THAT HAS APPLIED NOTHING IS AT THE ZERO POSITION, in the manifest
// and in the file alike.
//
// A fleet's newest domain has no checkpoint row until its first record. Its
// snapshots are still real artefacts — a joiner adopting one is at zero on
// that domain exactly as a fresh node is — and a manifest that named it as
// anything else, or a recipient that refused the absent row, would leave every
// snapshot unadoptable until somebody wrote to that log.
func TestADomainWithNoCheckpointIsSnapshottedAtZero(t *testing.T) {
	t.Parallel()
	h := newSnapHarness(t)
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM statelog_cursor`)
		return err
	}); err != nil {
		t.Fatalf("clear the checkpoint: %v", err)
	}
	m, err := h.snap.Take(t.Context())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	got := m.Domains["probe"]
	if got.Seq != 0 || got.Generation != 0 {
		t.Fatalf("the manifest names %d/%d for a domain that applied nothing, "+
			"want 0/0", got.Generation, got.Seq)
	}
	if !strings.HasPrefix(m.Artifact, "snapshot-0-") {
		t.Fatalf("the artefact is called %q, want a name carrying its zero "+
			"position", m.Artifact)
	}
	if _, err := os.Stat(filepath.Join(h.dir, m.Artifact)); err != nil {
		t.Fatalf("the artefact the manifest names is not there: %v", err)
	}
}

// A SNAPSHOT NAMES THE STREAM THE FILE WAS APPLYING, NOT THE ONE ITS DONOR IS
// LIVE ON.
//
// Every other checkpoint field is read out of the copy, for the reason the
// package states: the checkpoint commits in the same transaction as the rows,
// so the position inside a file is the only position that describes that file.
// The identity was the exception — taken from the donor's live registration —
// and the two are the same stream only until one is rebuilt. A donor restarted
// onto a recreated stream therefore stamped OLD rows with the NEW stream's
// identity, and the artefact was self-consistent enough to pass every check a
// recipient could make: its position matched the file, its identity matched
// the recipient's live stream, and its history no longer existed.
func TestASnapshotNamesTheStreamItsFileWasApplying(t *testing.T) {
	t.Parallel()
	h := newSnapHarness(t)
	// The checkpoint in the file was committed against the stream this
	// node was applying BEFORE the rebuild; the registration reports the
	// one it is live on now.
	h.created = liveStreamCreatedAt.Add(-72 * time.Hour)
	h.cursor(4_200)

	m, err := h.snap.Take(t.Context())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	got := m.Domains["probe"]
	if !got.StreamCreatedAt.Equal(h.created) {
		t.Errorf("the manifest names the stream created at %s and the file was "+
			"applying the one created at %s — an identity from the donor's live "+
			"handle describes a stream the rows in this copy never saw",
			got.StreamCreatedAt.UTC(), h.created.UTC())
	}
	if got.StreamCreatedAt.Equal(liveStreamCreatedAt) {
		t.Error("the manifest took its identity from the donor's live " +
			"registration, which is the defect: the artefact then matches a " +
			"recipient's live stream while carrying another one's history")
	}
}

// AND AN OFFER FROM ANOTHER STREAM INSTANCE IS REFUSED BEFORE THE TRANSFER.
//
// A rebuilt stream comes back at generation 0 counting from 1, so it matches
// on generation, on record version and on replay protocol. The broker's own
// creation instant is the only term that separates them — which is why it is
// stamped into every checkpoint and carried in every manifest.
func TestAnOfferFromAnotherStreamInstanceIsRefused(t *testing.T) {
	t.Parallel()
	offer := statelog.Offer{Manifest: statelog.Manifest{
		V: statelog.ManifestVersion,
		Domains: map[string]statelog.DomainPosition{"probe": {
			Stream:          probeStream,
			Generation:      1,
			Seq:             9_000,
			StreamCreatedAt: liveStreamCreatedAt.Add(-72 * time.Hour),
			RecordVersion:   probeDomain{}.RecordVersion(),
			Replay:          statelog.ReplayStrict,
		}},
	}}
	build := map[string]statelog.Registered{"probe": {Domain: probeDomain{}}}

	req := statelog.OfferRequest{
		Need:            map[string]uint64{"probe": 1},
		Generations:     map[string]uint32{"probe": 1},
		StreamCreatedAt: map[string]time.Time{"probe": liveStreamCreatedAt},
	}
	err := offer.Usable(req, build)
	if err == nil {
		t.Fatal("an artefact from a stream this node's log is not was accepted " +
			"— its sequences name a history this stream does not have, and " +
			"nothing else in the manifest can say so")
	}
	if !strings.Contains(err.Error(), "rebuilt") {
		t.Errorf("the refusal does not say what happened: %v", err)
	}

	// AND THE SAME OFFER IS USABLE when the instants agree, so this is a
	// refusal of a mismatch rather than of the term's presence.
	req.StreamCreatedAt["probe"] = liveStreamCreatedAt.Add(-72 * time.Hour)
	if err := offer.Usable(req, build); err != nil {
		t.Errorf("an artefact from this node's own stream was refused: %v", err)
	}

	// AND AN INSTANT THAT DIFFERS ONLY BELOW THE STORED RESOLUTION IS THE
	// SAME STREAM.
	//
	// The artefact's copy came back through a microsecond column and the
	// joiner's came straight from the broker in nanoseconds, so an exact
	// comparison calls every honest artefact a recreation — which is the
	// same trap the applier's own identity check documents, and the
	// reason both go through one rule.
	offer.Manifest.Domains["probe"] = statelog.DomainPosition{
		Stream: probeStream, Generation: 1, Seq: 9_000,
		StreamCreatedAt: liveStreamCreatedAt,
		RecordVersion:   probeDomain{}.RecordVersion(),
		Replay:          statelog.ReplayStrict,
	}
	req.StreamCreatedAt["probe"] = liveStreamCreatedAt.Add(37 * time.Nanosecond)
	if err := offer.Usable(req, build); err != nil {
		t.Errorf("an artefact whose instant differs by 37ns was refused: %v — "+
			"the column keeps microseconds and the broker reports "+
			"nanoseconds, so this refuses every real snapshot", err)
	}

	// AND A JOINER THAT COULD NOT READ ITS OWN INSTANT ASKS WITHOUT ONE
	// rather than refusing every donor.
	delete(req.StreamCreatedAt, "probe")
	if err := offer.Usable(req, build); err != nil {
		t.Errorf("a joiner naming no instant refused an otherwise usable "+
			"artefact: %v", err)
	}
}

// A SECOND TAKE AT THE SAME POSITION PUBLISHES ITS OWN PAIR, NOT OVER THE
// FIRST.
//
// The pair used to be named for the highest sequence it covered, and nothing
// required that sequence to have MOVED: a company quiet for a whole snapshot
// interval takes its next snapshot at the same position, which resolved to the
// previous pair's name. Publishing then went THROUGH the only artefact this
// node had — remove the old manifest, rename over the old database, and, if
// writing the new manifest failed there, remove the replacement as well. A
// disk that filled between the copy and the manifest destroyed the last usable
// snapshot, at the one moment a node most needs to still have one.
//
// The names must therefore differ, which is what lets the new pair be written
// beside the old one and the old one rotated away only once the new manifest
// is durable.
func TestASecondTakeAtTheSamePositionDoesNotPublishOverTheFirst(t *testing.T) {
	t.Parallel()
	h := newSnapHarness(t)
	first, err := h.snap.Take(t.Context())
	if err != nil {
		t.Fatalf("the first take: %v", err)
	}

	// NO NEW RECORDS: the checkpoint has not moved, which is the ordinary
	// state of a quiet company at its next interval.
	h.clock = h.clock.Add(time.Hour)
	h.rebuild(time.Nanosecond)
	second, err := h.snap.Take(t.Context())
	if err != nil {
		t.Fatalf("the second take: %v", err)
	}

	if second.Artifact == first.Artifact {
		t.Fatalf("both takes published as %q — publishing over the only "+
			"artefact this node has means removing its manifest and renaming "+
			"over its database before the replacement is durable, so a disk "+
			"that fills in between leaves the node with neither", second.Artifact)
	}
	if second.Domains["probe"].Seq != first.Domains["probe"].Seq {
		t.Fatalf("the case did not exercise a re-take at one position: %d then %d",
			first.Domains["probe"].Seq, second.Domains["probe"].Seq)
	}

	// AND WHAT SURVIVES IS ONE COMPLETE PAIR: the new one, with its
	// manifest readable and its bytes where the manifest says.
	names := entryNames(t, h.dir)
	if len(names) != 2*statelog.SnapshotsKept {
		t.Fatalf("the directory holds %v, want one complete pair", names)
	}
	if _, err := os.Stat(filepath.Join(h.dir, second.Artifact)); err != nil {
		t.Fatalf("the artefact the newest manifest names is not there: %v", err)
	}
	back, err := statelog.ReadManifest(filepath.Join(h.dir,
		strings.TrimSuffix(second.Artifact, ".db")+".json"))
	if err != nil {
		t.Fatalf("read the newest manifest back: %v", err)
	}
	if back.Artifact != second.Artifact {
		t.Errorf("the manifest on disk names %q and the take reported %q",
			back.Artifact, second.Artifact)
	}
}
