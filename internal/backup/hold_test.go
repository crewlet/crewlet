package backup_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
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
		digest, err := store.FileDigest(t.Context(), filepath.Join(dir, artifact.File))
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

	s := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "node-0", Holds: held, Backups: fleet,
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
	if at := pinned.Streams["CREWLET_TRACKER_LOG"]; at.Seq != 900 || at.Generation != 2 {
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

// THE COPY HOLDS EVERY COMMIT THAT RETURNED BEFORE IT BEGAN, the position the
// hold pinned included.
//
// The applier commits throughout a backup, and the hold is taken at the
// position the node stood at just before the copies — so a replicated copy
// older than that position would leave every record between the two pinned by
// nothing, for the trim to take before a restore replays them. Here the
// applier's commit lands while the hold is being put: after the read the hold
// took its position from, before either estate is copied, and on the pinned
// writer's own connection, which no copy draws. The connection the copy does
// draw last read before that commit, and a copy that took its connection's
// last view instead of its own holds the position the hold pinned and nothing
// after it.
func TestTheCopyHoldsEveryCommitThatReturnedBeforeItBegan(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 1, 41)
	w, err := storetest.ReplicatedDB(t, db.Replicated()).Writer(t.Context())
	if err != nil {
		t.Fatalf("pin the applier's writer: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	fleet := memory.NewFleet()
	held := &committingHolds{HoldRegister: fleet, commit: func(ctx context.Context) error {
		return w.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE statelog_cursor SET seq = 42
				WHERE stream = 'CREWLET_TRACKER_LOG'`)
			return err
		})
	}}

	manifest, err := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "node-0", Holds: held, Backups: fleet,
		Now: func() time.Time { return clock },
	}).Take(t.Context(), filepath.Join(t.TempDir(), "b"))
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if held.err != nil {
		t.Fatalf("the applier's commit during the hold: %v", held.err)
	}
	if at := held.pinned["CREWLET_TRACKER_LOG"]; at.Seq != 41 {
		t.Fatalf("the hold pinned %+v, want the 41 the node stood at before the commit", at)
	}
	if at := manifest.Domains["CREWLET_TRACKER_LOG"]; at.Seq != 42 {
		t.Fatalf("the copy holds %+v, want the 42 whose commit returned before the copy "+
			"began — the copy took the view its connection last read", at)
	}
}

// committingHolds is a register whose first put commits the applier's next
// write before it answers, which is where a backup's copy has not begun yet.
type committingHolds struct {
	coord.HoldRegister
	commit func(context.Context) error
	once   sync.Once
	pinned map[string]coord.Position
	err    error
}

func (c *committingHolds) PutHold(ctx context.Context, h coord.TrimHold) error {
	if err := c.HoldRegister.PutHold(ctx, h); err != nil {
		return err
	}
	c.once.Do(func() {
		c.pinned = h.Streams
		c.err = c.commit(ctx)
	})
	return nil
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
	s := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "node-0", Holds: refusingHolds{}, Backups: memory.NewFleet(),
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

// A BACKUP IS REFUSED WHERE A DATABASE'S OWN VOLUME HAS NO ROOM FOR THE LOG ITS
// COPY HOLDS BACK — before it pins the log or copies a byte.
//
// A copy holds its file's write-ahead log for its whole run, so the log grows
// by what the node writes meanwhile, on the volume the node commits to, and a
// node whose volume fills stops committing for the rest of the copy. Here the
// replicated estate's last copy was held while its applier wrote beside it, so
// its handle measured what a copy of it holds back. The backup then finds the
// estate's volume a byte short of that and is refused, naming the file and the
// log, with no hold put and nothing in the directory; and with exactly that
// room it runs, and pins the log as every backup does.
func TestABackupIsRefusedWhereTheLogItsCopyHoldsBackHasNoRoom(t *testing.T) {
	t.Parallel()
	hold := storetest.NewCopyHold()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "store.db"),
		store.Options{WrapDriver: hold.Wrap}, 1)
	t.Cleanup(func() { _ = db.Close() })
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 1, 41)
	replicated := storetest.ReplicatedDB(t, estate)
	ctx := t.Context()
	if err := estate.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`CREATE TABLE crewlet_copy_held (id INTEGER PRIMARY KEY, pad BLOB NOT NULL)`)
		return err
	}); err != nil {
		t.Fatalf("create the table written beside the copy: %v", err)
	}
	storetest.HoldCopy(t, replicated, hold, func() {
		for i := range 64 {
			if err := estate.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO crewlet_copy_held (id, pad)
					VALUES (?, zeroblob(4096))`, i)
				return err
			}); err != nil {
				t.Fatalf("write row %d beside the copy: %v", i, err)
			}
		}
	})
	info, err := os.Stat(replicated.Path())
	if err != nil {
		t.Fatalf("measure the replicated estate: %v", err)
	}
	holds := replicated.CopyHoldsBack(info.Size())
	if holds <= 0 {
		t.Fatalf("the estate measured a copy holding back %d after one held its log "+
			"under 64 commits", holds)
	}
	fleet := memory.NewFleet()
	held := &watchedHolds{HoldRegister: fleet}
	s := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "node-0", Holds: held, Backups: fleet,
		Now: func() time.Time { return clock },
	})
	live := filepath.Dir(replicated.Path())
	backup.SetVolumeFree(s, func(dir string) (int64, error) {
		if dir == live {
			return holds - 1, nil
		}
		return 1 << 40, nil
	})
	dir := filepath.Join(t.TempDir(), "b")
	_, err = s.Take(ctx, dir)
	if err == nil || !strings.Contains(err.Error(), replicated.Path()) ||
		!strings.Contains(err.Error(), "write-ahead log") {
		t.Fatalf("a backup whose estate's volume is a byte short of the log its copy holds "+
			"back answered %v, want a refusal naming the estate and its log", err)
	}
	if len(held.put) != 0 {
		t.Errorf("a backup refused for room pinned the log %d time(s) first", len(held.put))
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("a backup refused for room left %d entries in its directory (%v)",
			len(entries), err)
	}

	// AND EXACTLY THAT ROOM IS ROOM.
	backup.SetVolumeFree(s, func(string) (int64, error) { return holds, nil })
	if _, err := s.Take(ctx, filepath.Join(t.TempDir(), "b2")); err != nil {
		t.Fatalf("a backup with room for the log its copy holds back was refused: %v", err)
	}
	if len(held.put) != 1 {
		t.Errorf("the backup that ran pinned the log %d time(s), want 1", len(held.put))
	}
}

// A LOG TRIMMED PAST THE COPY'S POSITION REFUSES THE MANIFEST.
//
// This is the assertion that makes "restorable" checkable. A restore replays
// from the store's position to the log's head, which is only possible while
// the log still holds `position + 1`; below that the records are gone, and
// applying what remains writes state derived from a prefix with a hole in it —
// silently, because every remaining record applies cleanly.
//
// THE BOUNDARY IS PINNED ON BOTH SIDES, against a log whose first record is
// 40: a copy at 39 has missed nothing and is accepted, and a copy at 38 is
// short exactly record 39 and is refused. A copy far below the log proves
// only that the check exists; one record either way is where a call site that
// passes the wrong operand to the shared predicate goes wrong, and it goes
// wrong in the direction that writes a manifest over a hole.
func TestATrimmedLogRefusesTheManifest(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	// The store stands at 10 and the log's oldest surviving record is 40:
	// everything between was trimmed while the copy ran.
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 1, 10)
	nc := embeddedNATS(t)
	// RECORDS OF THE TRACKER'S OWN FORMAT — barriers, which write no row:
	// a backup reads the log past its copy for the files it names, and
	// bytes no build reads would refuse it for that rather than for the
	// hole this case is about.
	trackerLog(t, nc)
	appendBarriers(t, nc, 60)
	trimTo(t, nc, "CREWLET_TRACKER_LOG", 40)

	dir := filepath.Join(t.TempDir(), "b")
	_, err := service(t, db, nc).Take(t.Context(), dir)
	if !errors.Is(err, backup.ErrLogGap) {
		t.Fatalf("a backup whose store copy names a position the captured log "+
			"can no longer reach answered %v; want ErrLogGap", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, backup.ManifestName)); statErr == nil {
		t.Fatal("a manifest was written over the hole")
	}
	// AND IT SAYS WHERE TO LOOK. The hold is the thing that failed to stop
	// the trim, and the one line that records a hold going stale is its
	// renewal warning, so the refusal names it rather than a node shape.
	if !strings.Contains(err.Error(), "statelog_hold_not_renewed") {
		t.Errorf("the refusal does not say where to look: %v", err)
	}

	// AND THE BOUNDARY CASE PASSES: a log whose first sequence is exactly
	// one past the copy's position has nothing missing at all.
	db2 := openStore(t)
	seedCursor(t, db2, "CREWLET_TRACKER_LOG", 1, 39)
	if _, err := service(t, db2, nc).Take(t.Context(), filepath.Join(t.TempDir(), "b2")); err != nil {
		t.Fatalf("a log starting exactly one past the copy was refused: %v", err)
	}

	// AND ONE RECORD SHORT OF IT IS REFUSED: a copy at 38 against a log
	// whose first record is 40 never applied 39, and the log no longer
	// holds it.
	db3 := openStore(t)
	seedCursor(t, db3, "CREWLET_TRACKER_LOG", 1, 38)
	short := filepath.Join(t.TempDir(), "b3")
	_, err = service(t, db3, nc).Take(t.Context(), short)
	if err == nil {
		t.Fatal("a copy one record short of a log starting at 40 was accepted — " +
			"a restore from it applies 40 onward over the missing 39 and reports " +
			"nothing")
	}
	if !strings.Contains(err.Error(), "statelog_hold_not_renewed") {
		t.Errorf("the refusal one record short does not say where to look: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(short, backup.ManifestName)); statErr == nil {
		t.Fatal("a manifest was written for a copy one record short of the log")
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

// seedCursor writes a checkpoint naming no stream instance — the zero instant,
// which a reader of the copy takes as a claim about nothing.
func seedCursor(t *testing.T, db *store.DB, stream string, generation uint32, seq uint64) {
	t.Helper()
	seedCursorCreated(t, db, stream, generation, seq, time.Time{})
}

// seedCursorCreated writes a checkpoint keyed to the instance of the stream
// created at created.
func seedCursorCreated(t *testing.T, db *store.DB, stream string, generation uint32, seq uint64,
	created time.Time) {
	t.Helper()
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor (stream, generation, seq, stream_created_at, updated_at, applied_version)
			VALUES (?, ?, ?, ?, 0, 0)
			ON CONFLICT (stream) DO UPDATE SET generation = excluded.generation,
			                                   seq = excluded.seq,
			                                   stream_created_at = excluded.stream_created_at`,
			stream, int64(generation), int64(seq), store.EncodeTime(created))
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

// TestAFinishedBackupAnnouncesWhatItCovers is the input the trim's backup term
// has and nothing else can supply.
//
// The term refuses to let the log delete anything the newest backup does not
// hold, and the node evaluating it is whichever one holds the trim duty — not
// necessarily the node that took the copy, and never a node that can see a
// directory on another host. A backup that ran and announced nothing leaves a
// fleet with a working nightly schedule whose log grows for ever, which is the
// most confusing shape this gate has.
func TestAFinishedBackupAnnouncesWhatItCovers(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 2, 900)
	fleet := memory.NewFleet()

	dir := filepath.Join(t.TempDir(), "b")
	service := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "node-0", Holds: fleet, Backups: fleet,
		Now: func() time.Time { return clock },
	})
	manifest, err := service.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("take: %v", err)
	}

	points, err := fleet.BackupPoints(t.Context())
	if err != nil {
		t.Fatalf("BackupPoints: %v", err)
	}
	newest, ok := coord.NewestBackupOf(points, "CREWLET_TRACKER_LOG")
	if !ok {
		t.Fatal("a finished backup announced nothing — the trim's backup term " +
			"has no input at all and refuses for ever, so this fleet's log " +
			"grows with a working backup schedule behind it")
	}
	if newest.Owner != "node-0" || !newest.Verified {
		t.Fatalf("the point is %+v, want node-0's own verified copy", newest)
	}
	if !newest.At.Equal(clock) {
		// THE INSTANT THE COPY STARTED, not the one it finished:
		// nothing in the artefact is older than that, and the term's
		// age comparison is about the state the copy describes rather
		// than about how long writing it took.
		t.Fatalf("the point is stamped %s, want the copy's own start %s",
			newest.At, clock)
	}
	at, covers := newest.Streams["CREWLET_TRACKER_LOG"]
	if !covers || at.Seq != 900 || at.Generation != 2 {
		t.Fatalf("the point reaches %+v, want the log at generation 2 sequence "+
			"900 — a bare sequence from before a reanchor names a dead "+
			"number space", at)
	}
	if newest.Dir != dir {
		t.Fatalf("the point names %q as its directory, want %q — \"the newest "+
			"backup is three days old\" is an answer somebody then has to go "+
			"and find the directory for", newest.Dir, dir)
	}
	// THE WHOLE ARTEFACT'S SIZE, so a screen can say what the copy weighs
	// without reading a directory on another host.
	var want int64
	for _, st := range manifest.Stores {
		want += st.Bytes
	}
	for _, s := range manifest.Streams {
		want += s.Bytes
	}
	if want == 0 || newest.Bytes != want {
		t.Fatalf("the point says the copy is %d bytes, want the manifest's %d",
			newest.Bytes, want)
	}
}
