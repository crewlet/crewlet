package backup_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// THE COPY'S OWN POSITION IS IN THE MANIFEST, read from the file rather than
// from the live database.
//
// A restore replays from that number, so it has to describe the file. The
// checkpoint commits in the same transaction as the rows it covers, and the
// applier runs throughout the copy — so the live cursor names where the node
// was when the copy STARTED, which on a large store is minutes of records the
// restore would then skip.
func TestTheManifestNamesThePositionInsideTheCopy(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 1, 41)

	dir := filepath.Join(t.TempDir(), "b")
	manifest, err := service(t, db, nil).Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	at, ok := manifest.Domains["CREWLET_TRACKER_LOG"]
	if !ok {
		t.Fatalf("the manifest names no position for the tracker's log: %+v",
			manifest.Domains)
	}
	if at.Seq != 41 || at.Generation != 1 || at.Stream != "CREWLET_TRACKER_LOG" {
		t.Fatalf("the manifest names %+v", at)
	}

	// AND THE ARTEFACT IS STILL A SET OF NAMED FILES. Reading a database
	// creates a -wal and a -shm beside it even for a read, and both are
	// debris carrying the reader's own umask rather than this directory's
	// deliberate 0700.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) == ".db" || name == backup.ManifestName || entry.IsDir() {
			continue
		}
		t.Errorf("the artefact holds %q, which is neither a copy, the "+
			"manifest, nor the stream directory", name)
	}

	// AND THE DIGEST DESCRIBES THE FILE THAT IS THERE, which is the one
	// question a shipped artefact raises. It was retaken after this
	// package opened the copy to read its checkpoints.
	for _, artifact := range manifest.Stores {
		if artifact.Estate != store.EstateReplicated {
			continue
		}
		digest, err := store.FileDigest(filepath.Join(dir, artifact.File))
		if err != nil {
			t.Fatal(err)
		}
		if digest != artifact.SHA256 {
			t.Fatalf("the manifest claims %s and the file on disk digests to "+
				"%s — a checksum that named the bytes before this package "+
				"opened the copy would never match what was shipped",
				artifact.SHA256, digest)
		}
	}
}

// THE HOLD IS TAKEN BEFORE THE COPY AND RELEASED WITH THE MANIFEST.
//
// It is what stops the trim deleting exactly the records the artefact's gap
// needs, and it is HEARTBEATED because a pin that outlived its owner would
// stop the trim for ever and the log would grow to its ceiling.
func TestTheBackupPinsTheLogWhileItCopiesAndReleasesIt(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 2, 900)
	fleet := memory.NewFleet()
	held := &watchedHolds{HoldRegister: fleet}

	s := backup.New(backup.Options{
		Store: db, NodeID: "node-0", Holds: held,
		Now: func() time.Time { return clock },
	})
	if _, err := s.Take(t.Context(), filepath.Join(t.TempDir(), "b")); err != nil {
		t.Fatalf("take: %v", err)
	}

	if len(held.put) != 1 {
		t.Fatalf("the backup took %d hold(s)", len(held.put))
	}
	pinned := held.put[0]
	if pinned.Owner != coord.HoldOwner("node-0", backup.HoldPurpose) {
		t.Fatalf("the hold is owned by %q — an operator reading a stalled trim "+
			"needs both which node and what it was doing", pinned.Owner)
	}
	// AT THE LIVE POSITION, deliberately: the pin has to cover everything
	// the copy is about to include, and the copy has not happened yet. A
	// pin at the copy's own position would be taken after the window it
	// exists to protect.
	if at := pinned.Domains["CREWLET_TRACKER_LOG"]; at.Seq != 900 || at.Generation != 2 {
		t.Fatalf("the hold pins %+v", at)
	}
	if pinned.Reason == "" {
		t.Error("the hold names no reason, which is the one thing the register " +
			"cannot derive and the only thing an operator can act on")
	}
	if held.released != 1 {
		t.Fatalf("the hold was released %d time(s) — a pin that outlives its "+
			"owner stops the trim until the stale bound expires it",
			held.released)
	}
	if remaining, err := fleet.Holds(t.Context()); err != nil || len(remaining) != 0 {
		t.Fatalf("%d hold(s) survive the backup (%v)", len(remaining), err)
	}
}

// A REGISTER THAT REFUSES THE PIN REFUSES THE BACKUP.
//
// Carrying on without one would produce an artefact whose own gap may be
// trimmed away while it is being written, and it would report success either
// way — which is the shape this whole step exists to remove.
func TestABackupThatCannotPinTheLogIsRefused(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 1, 5)
	s := backup.New(backup.Options{
		Store: db, NodeID: "node-0", Holds: refusingHolds{},
		Now: func() time.Time { return clock },
	})
	dir := filepath.Join(t.TempDir(), "b")
	if _, err := s.Take(t.Context(), dir); err == nil {
		t.Fatal("a backup that could not pin the log reported success")
	}
	if _, err := os.Stat(filepath.Join(dir, backup.ManifestName)); err == nil {
		t.Fatal("a manifest was written, so a reader cannot tell this from a " +
			"complete backup")
	}
}

// A LOG TRIMMED PAST THE COPY'S POSITION REFUSES THE MANIFEST.
//
// This is the assertion that makes "restorable" checkable. A restore replays
// from the store's position to the log's head, which is only possible while
// the log still holds `position + 1`; below that the records are gone, and
// applying what remains writes state derived from a prefix with a hole in it —
// silently, because every remaining record applies cleanly.
func TestATrimmedLogRefusesTheManifest(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	// The store stands at 10 and the log's oldest surviving record is 40:
	// everything between was trimmed while the copy ran.
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 1, 10)
	nc := embeddedNATS(t)
	seedStream(t, nc, "CREWLET_TRACKER_LOG", "crewlet.tracker.log.>", 60)
	trimTo(t, nc, "CREWLET_TRACKER_LOG", 40)

	dir := filepath.Join(t.TempDir(), "b")
	_, err := service(t, db, nc).Take(t.Context(), dir)
	if err == nil {
		t.Fatal("a backup whose store copy names a position the captured log " +
			"can no longer reach reported success")
	}
	if _, statErr := os.Stat(filepath.Join(dir, backup.ManifestName)); statErr == nil {
		t.Fatal("a manifest was written over the hole")
	}

	// AND THE BOUNDARY CASE PASSES: a log whose first sequence is exactly
	// one past the copy's position has nothing missing at all.
	db2 := openStore(t)
	seedCursor(t, db2, "CREWLET_TRACKER_LOG", 1, 39)
	if _, err := service(t, db2, nc).Take(t.Context(), filepath.Join(t.TempDir(), "b2")); err != nil {
		t.Fatalf("a log starting exactly one past the copy was refused: %v", err)
	}
}

// THE AGE COMES FROM THE ARTEFACTS, not from a counter this process keeps.
//
// A counter records that a process BELIEVED it took a backup; the directory
// records that one exists. They differ in exactly the cases the alarm is for —
// a copy deleted, a volume never mounted, a schedule pointing somewhere nobody
// ships from — and in every one the counter says the fleet is protected.
func TestTheBackupAgeIsReadFromTheNewestManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	if _, ok, err := backup.Newest(root); err != nil || ok {
		t.Fatalf("an empty directory reports a backup (%v, %v)", ok, err)
	}
	if _, ok, err := backup.Newest(filepath.Join(root, "never-mounted")); err != nil || ok {
		t.Fatalf("a path that does not exist is a fault rather than a fleet "+
			"with no backup (%v, %v)", ok, err)
	}

	// TWO DATED RUNS UNDER A PARENT, which is the shape a schedule
	// produces, plus the debris of one that did not finish.
	older := clock.Add(-48 * time.Hour)
	newer := clock.Add(-6 * time.Hour)
	writeManifestAt(t, filepath.Join(root, "2026-08-28"), older)
	writeManifestAt(t, filepath.Join(root, "2026-08-30"), newer)
	if err := os.MkdirAll(filepath.Join(root, "2026-08-31"), 0o700); err != nil {
		t.Fatal(err)
	}

	at, ok, err := backup.Newest(root)
	if err != nil || !ok {
		t.Fatalf("Newest = (%v, %v, %v)", at, ok, err)
	}
	if !at.Equal(newer) {
		t.Fatalf("the newest backup is reported as %v, want %v — a directory "+
			"with no manifest is the debris of a run that did not finish, and "+
			"reading it as a backup is the alarm silenced by a failure",
			at, newer)
	}
	age, ok, err := backup.Age(root, clock)
	if err != nil || !ok || age != 6*time.Hour {
		t.Fatalf("Age = (%v, %v, %v)", age, ok, err)
	}

	// A MANIFEST FROM THE FUTURE IS AGE ZERO. Clocks differ between the
	// host that took a backup and the host reading it, and a negative age
	// would compare below every threshold and read as the freshest backup
	// imaginable.
	writeManifestAt(t, filepath.Join(root, "2026-09-05"), clock.Add(time.Hour))
	if age, ok, err := backup.Age(root, clock); err != nil || !ok || age != 0 {
		t.Fatalf("a manifest from the future gives age (%v, %v, %v)", age, ok, err)
	}
}

// ---- the fixtures ----------------------------------------------------- //

// trimTo deletes every message below seq, which is what the fleet's own trim
// does when every counted node has committed past it.
func trimTo(t *testing.T, nc *nats.Conn, name string, seq uint64) {
	t.Helper()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	stream, err := js.Stream(t.Context(), name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	if err := stream.Purge(t.Context(), jetstream.WithPurgeSequence(seq)); err != nil {
		t.Fatalf("trim %s to %d: %v", name, seq, err)
	}
}

func seedCursor(t *testing.T, db *store.DB, stream string, generation uint32, seq uint64) {
	t.Helper()
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor (stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, ?, ?, 0, 0)
			ON CONFLICT (stream) DO UPDATE SET generation = excluded.generation,
			                                   seq = excluded.seq`,
			stream, int64(generation), int64(seq))
		return err
	}); err != nil {
		t.Fatalf("seed the cursor: %v", err)
	}
}

func writeManifestAt(t *testing.T, dir string, finished time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(backup.Manifest{
		TakenAt: finished.Add(-time.Minute), FinishedAt: finished, NodeID: "node-0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestName), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// watchedHolds records what a backup did to the register.
type watchedHolds struct {
	coord.HoldRegister
	put      []coord.TrimHold
	released int
}

func (w *watchedHolds) PutHold(ctx context.Context, h coord.TrimHold) error {
	if err := w.HoldRegister.PutHold(ctx, h); err != nil {
		return err
	}
	w.put = append(w.put, h)
	return nil
}

func (w *watchedHolds) ReleaseHold(ctx context.Context, owner string) error {
	w.released++
	return w.HoldRegister.ReleaseHold(ctx, owner)
}

// refusingHolds is a register that cannot be written to, which is what a
// coordination outage looks like from here.
type refusingHolds struct{}

func (refusingHolds) PutHold(context.Context, coord.TrimHold) error {
	return errRefused
}
func (refusingHolds) Holds(context.Context) ([]coord.TrimHold, error) {
	return nil, errRefused
}
func (refusingHolds) ReleaseHold(context.Context, string) error { return nil }

var errRefused = errorString("the coordination store cannot be reached")

type errorString string

func (e errorString) Error() string { return string(e) }
