package backup_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/store"
)

// stored is an object in a test's store: what a row names, and its bytes.
type stored struct {
	object objstore.Object
	data   []byte
}

// filesStore is an object store over the in-memory twin, its keys minted at
// the backup suite's clock.
func filesStore(t *testing.T) (*objstore.Store, *memobj.Backend) {
	t.Helper()
	backend := memobj.New()
	s, err := objstore.NewStoreAt(backend, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	return s, backend
}

// upload puts each body into s as an object of its own.
func upload(t *testing.T, s *objstore.Store, bodies ...string) []stored {
	t.Helper()
	var out []stored
	for _, body := range bodies {
		o, err := s.Put(t.Context(), bytes.NewReader([]byte(body)), 1<<20, objstore.PutMeta{})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, stored{object: o, data: []byte(body)})
	}
	return out
}

// filesNaming puts one file row per object straight into the replicated
// estate, as the applier would have — `ENG/file-<i>` naming the i-th — and a
// removed row naming none, which no backup may count.
func filesNaming(t *testing.T, db *store.DB, objects ...stored) {
	t.Helper()
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for i, o := range objects {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO tracker_files
				(id, project_key, path, hash, size, object, created_at, updated_at, version, document)
				VALUES (?, 'ENG', ?, ?, ?, ?, 0, 0, 1, x'7b7d')`,
				fmt.Sprintf("ENG.%d", i), fmt.Sprintf("file-%d", i), string(o.object.Hash),
				o.object.Size, o.object.Key.String()); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(t.Context(), `INSERT INTO tracker_files
			(id, project_key, path, hash, size, object, created_at, updated_at, removed_at, version, document)
			VALUES ('ENG.gone', 'ENG', 'gone', '', 0, NULL, 0, 0, 1, 2, x'7b7d')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// retiredRefs declares test_retired_objects, the table [retiredNaming] writes:
// one retired reference per object, which keeps the object from the collector
// and names nothing the company is owed (ADR-0033). No migration creates it,
// so it is never in references.All; a case hands it to the backup through
// [backup.SetReferenceTables].
var retiredRefs = objstore.ReferenceTable{Domain: "tracker", Table: "test_retired_objects",
	Key: "object", Hash: "hash", Size: "size", Owner: []string{"owner"}, Standing: objstore.Retired}

// retiredNaming creates retiredRefs' table in the replicated estate, indexed on
// its key as the references gate requires of a declared table, and writes one
// retired row per object, owned by `retired-<i>`.
func retiredNaming(t *testing.T, db *store.DB, objects ...stored) {
	t.Helper()
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`CREATE TABLE test_retired_objects (object TEXT, hash TEXT NOT NULL,
				size INTEGER NOT NULL, owner TEXT NOT NULL)`,
			`CREATE INDEX test_retired_objects_object_idx ON test_retired_objects (object)`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
				return err
			}
		}
		for i, o := range objects {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO test_retired_objects
				(object, hash, size, owner) VALUES (?, ?, ?, ?)`,
				o.object.Key.String(), string(o.object.Hash), o.object.Size,
				fmt.Sprintf("retired-%d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A BACKUP CARRIES NO RETIRED REFERENCE (ADR-0033). A retired row keeps a
// replaced object from the collector for a grace and names nothing the company
// is owed, so the backup neither copies it nor asks after it nor records it
// lost — on S3, a GET per retired object per backup for bytes no restore wants;
// on the broker's bucket, a retired object the store already let go of
// reported as a lost file. The copy's retired rows restore without their
// objects, which nothing owes them.
func TestABackupCarriesNoRetiredReference(t *testing.T) {
	t.Parallel()
	// fixture is a copy naming one live file and two retired objects — one
	// the store still holds, one the collector has already taken.
	fixture := func(t *testing.T) (*store.DB, *objstore.Store, []stored) {
		t.Helper()
		db := openStore(t)
		files, _ := filesStore(t)
		held := upload(t, files, "a live file", "a replaced object",
			"a replaced object the collector took")
		filesNaming(t, db, held[0])
		retiredNaming(t, db, held[1], held[2])
		if err := files.Delete(t.Context(), held[2].object.Key); err != nil {
			t.Fatal(err)
		}
		return db, files, held
	}
	declared := append(slices.Clone(references.All), retiredRefs)

	t.Run("copied", func(t *testing.T) {
		t.Parallel()
		db, files, held := fixture(t)
		fleet := memory.NewFleet()
		svc := build(t, backup.Options{
			Store: db, Estate: backup.HoldsReplicated, NodeID: "n", Holds: fleet, Backups: fleet,
			Now: func() time.Time { return clock },
			Objects: &backup.Objects{Open: func(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
				if o.Key != held[0].object.Key {
					t.Errorf("the backup read %s, which only a retired row names", o.Key)
				}
				return files.Open(ctx, o)
			}},
		})
		backup.SetReferenceTables(svc, declared)
		dir := filepath.Join(t.TempDir(), "with-retired")
		manifest, err := svc.Take(t.Context(), dir)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		o := manifest.Objects
		if o == nil || o.Objects != 1 || o.Bytes != int64(len("a live file")) || len(o.Lost) != 0 {
			t.Fatalf("manifest objects = %+v, want the live file alone carried and nothing lost", o)
		}
		entries, err := os.ReadDir(filepath.Join(dir, o.Dir, "files"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != held[0].object.Key.String() {
			t.Fatalf("the object directory holds %v, want the live file's object alone", entries)
		}
		restoresWhole(t, filepath.Join(dir, o.Dir), held[:1])
	})

	t.Run("in a stream", func(t *testing.T) {
		t.Parallel()
		db, files, held := fixture(t)
		fleet := memory.NewFleet()
		svc := build(t, backup.Options{
			Store: db, Estate: backup.HoldsReplicated, NodeID: "n", Holds: fleet, Backups: fleet,
			Now: func() time.Time { return clock },
			Objects: &backup.Objects{Stream: "OBJ_crewlet_files",
				Stat: func(ctx context.Context, k objstore.Key) (objstore.Info, error) {
					if k != held[0].object.Key {
						t.Errorf("the backup asked after %s, which only a retired row names", k)
					}
					return files.Stat(ctx, k)
				},
				Open: func(context.Context, objstore.Object) (io.ReadCloser, error) {
					t.Error("an object was read although the stream snapshot carries it")
					return nil, errors.New("not expected")
				}},
		})
		backup.SetReferenceTables(svc, declared)
		dir := filepath.Join(t.TempDir(), "in-stream-with-retired")
		manifest, err := svc.Take(t.Context(), dir)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		o := manifest.Objects
		if o == nil || o.Stream != "OBJ_crewlet_files" || o.Objects != 1 || len(o.Lost) != 0 {
			t.Fatalf("manifest objects = %+v, want the live file alone counted and nothing lost", o)
		}
		if _, err := os.Stat(filepath.Join(dir, "objects")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("an object directory was written beside a stream that carries the objects")
		}
	})
}

// A DECLARATION THAT STATES NO STANDING REFUSES THE BACKUP (ADR-0033). Sorted
// as though it were retired, a live table's objects would be left out of the
// backup with nothing failing — the very default a standing has none of — so
// every declaration the backup is handed is validated before any is left out,
// as the collector validates every one at boot. A standing nobody defined is
// refused for the same reason.
func TestABackupRefusesADeclarationThatStatesNoStanding(t *testing.T) {
	t.Parallel()
	for name, standing := range map[string]objstore.Standing{
		"none":    "",
		"unknown": "live",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := openStore(t)
			files, _ := filesStore(t)
			held := upload(t, files, "a live file", "an object only the undeclared table names")
			filesNaming(t, db, held[0])
			retiredNaming(t, db, held[1])
			undeclared := retiredRefs
			undeclared.Standing = standing
			fleet := memory.NewFleet()
			svc := build(t, backup.Options{
				Store: db, Estate: backup.HoldsReplicated, NodeID: "n", Holds: fleet, Backups: fleet,
				Now:     func() time.Time { return clock },
				Objects: &backup.Objects{Open: files.Open},
			})
			backup.SetReferenceTables(svc, append(slices.Clone(references.All), undeclared))
			dir := filepath.Join(t.TempDir(), "undeclared")
			if _, err := svc.Take(t.Context(), dir); err == nil ||
				!strings.Contains(err.Error(), "Standing") ||
				!strings.Contains(err.Error(), undeclared.Table) {
				t.Fatalf("Take = %v; want it refused for %s's standing", err, undeclared.Table)
			}
			if _, err := os.Stat(filepath.Join(dir, backup.ManifestName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused backup wrote its manifest (stat: %v)", err)
			}
		})
	}
}

// counting wraps s's Open, counting every object it is asked for.
func counting(s *objstore.Store, n *atomic.Int32) func(context.Context, objstore.Object) (io.ReadCloser, error) {
	return func(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
		n.Add(1)
		return s.Open(ctx, o)
	}
}

// A BACKUP CARRIES EVERY OBJECT THE STORE COPY NAMES, each under the name the
// store keeps it under — the key layout an S3 backend's bucket has, so a
// restore is one sync of the directory — verified against the row that names
// it, and says how many.
func TestABackupCarriesTheObjectsItsCopyNames(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	files, _ := filesStore(t)
	held := upload(t, files, "one file", "another file")
	filesNaming(t, db, held...)
	fleet := memory.NewFleet()
	svc := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "n", Holds: fleet, Backups: fleet,
		Now:     func() time.Time { return clock },
		Objects: &backup.Objects{Open: files.Open},
	})
	dir := filepath.Join(t.TempDir(), "with-files")
	manifest, err := svc.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if manifest.Objects == nil || manifest.Objects.Objects != 2 || len(manifest.Objects.Lost) != 0 ||
		manifest.Objects.Bytes != int64(len("one file")+len("another file")) {
		t.Fatalf("manifest objects = %+v", manifest.Objects)
	}
	restoresWhole(t, filepath.Join(dir, manifest.Objects.Dir), held)
}

// AN OBJECT WHOSE BYTES ARE NOT THE ROW'S IS RECORDED LOST AND NOT CARRIED: the
// store's read ends short of the object rather than handing over its last
// byte, and the backup leaves no file under the object's name — a restore
// that synced a damaged copy back would overwrite nothing better, but it would
// certify bytes the row says are wrong.
func TestABackupDoesNotCarryBytesThatAreNotTheRows(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	files, backend := filesStore(t)
	held := upload(t, files, "intact", "this one rots")
	backend.Corrupt(held[1].object.Key.Name(), []byte("this one rot!"))
	filesNaming(t, db, held...)
	fleet := memory.NewFleet()
	svc := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "n", Holds: fleet, Backups: fleet,
		Now:     func() time.Time { return clock },
		Objects: &backup.Objects{Open: files.Open},
	})
	dir := filepath.Join(t.TempDir(), "with-a-damaged-file")
	manifest, err := svc.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	o := manifest.Objects
	if o == nil || o.Objects != 1 || len(o.Lost) != 1 || o.Lost[0].Object != held[1].object.Key ||
		o.Lost[0].NamedBy != "ENG/file-1" {
		t.Fatalf("manifest objects = %+v, want the damaged file named lost", o)
	}
	entries, err := os.ReadDir(filepath.Join(dir, o.Dir, "files"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != held[0].object.Key.String() {
		t.Fatalf("the object directory holds %v, want the intact object alone — no "+
			"damaged copy, no partial file", entries)
	}
	restoresWhole(t, filepath.Join(dir, o.Dir), held[:1])
}

// A BACKUP WHOSE OBJECTS LIVE IN A STREAM IT SNAPSHOTS COPIES NONE BESIDE IT:
// the NATS backend's bucket is a stream, and its snapshot is the copy. What it
// does instead is ASK THE STORE for every object the copy names once the
// snapshot is taken, recording what the store does not hold — or holds as
// other bytes — as lost: a NATS backup used to count the references and check
// none of them, so an object the collector deleted before the snapshot was
// simply absent from it, and the manifest said nothing.
func TestABackupOfObjectsInAStreamAsksTheStoreForEach(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	files, backend := filesStore(t)
	held := upload(t, files, "one file", "another", "a third")
	filesNaming(t, db, held...)
	if err := files.Delete(t.Context(), held[1].object.Key); err != nil {
		t.Fatal(err)
	}
	// A DIFFERENT SIZE, which a stat sees: what a stat cannot see — the same
	// number of other bytes — is the audit's and a restore's to find.
	backend.Corrupt(held[2].object.Key.Name(), []byte("a third, longer"))
	fleet := memory.NewFleet()
	svc := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "n", Holds: fleet, Backups: fleet,
		Now: func() time.Time { return clock },
		Objects: &backup.Objects{Stream: "OBJ_crewlet_files", Stat: files.Stat,
			Open: func(context.Context, objstore.Object) (io.ReadCloser, error) {
				t.Error("an object was read although the stream snapshot carries it")
				return nil, errors.New("not expected")
			}},
	})
	dir := filepath.Join(t.TempDir(), "in-stream")
	manifest, err := svc.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	o := manifest.Objects
	if o == nil || o.Stream != "OBJ_crewlet_files" || o.Objects != 1 || o.Dir != "" ||
		len(o.Lost) != 2 {
		t.Fatalf("manifest objects = %+v, want the stream named, one object held and two lost", o)
	}
	for _, lost := range o.Lost {
		if lost.Object != held[1].object.Key && lost.Object != held[2].object.Key {
			t.Errorf("%s was recorded lost and the store holds it whole", lost.Object)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "objects")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an object directory was written beside a stream that carries the objects")
	}
}

// A COPY NAMING AN OBJECT NOBODY COULD SUPPLY — AND WHOSE ABSENCE IS NOT
// DEFINITE — IS NOT A BACKUP: no manifest is written, because the object may
// be intact in a store that did not answer, and a restore from it would bring
// back files whose bytes the store still had.
func TestABackupMissingAnObjectWritesNoManifest(t *testing.T) {
	t.Parallel()
	unanswered := errors.New("the store did not answer")
	for name, objects := range map[string]*backup.Objects{
		"an object nobody answered for": {Open: func(context.Context, objstore.Object) (io.ReadCloser, error) {
			return nil, unanswered
		}},
		"a stream nobody answered for": {Stream: "OBJ_crewlet_files",
			Open: func(context.Context, objstore.Object) (io.ReadCloser, error) { return nil, unanswered },
			Stat: func(context.Context, objstore.Key) (objstore.Info, error) {
				return objstore.Info{}, unanswered
			}},
		"no object client at all": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := openStore(t)
			files, _ := filesStore(t)
			filesNaming(t, db, upload(t, files, "unreachable")...)
			fleet := memory.NewFleet()
			svc := build(t, backup.Options{
				Store: db, Estate: backup.HoldsReplicated, NodeID: "n", Holds: fleet, Backups: fleet,
				Now: func() time.Time { return clock }, Objects: objects,
			})
			dir := filepath.Join(t.TempDir(), "incomplete")
			if _, err := svc.Take(t.Context(), dir); !errors.Is(err, backup.ErrObjectsUnreachable) {
				t.Fatalf("Take = %v, want ErrObjectsUnreachable", err)
			}
			if _, err := os.Stat(filepath.Join(dir, backup.ManifestName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a backup missing an object wrote its manifest")
			}
		})
	}
}

// AN OBJECT THE STORE HAS LOST IS RECORDED, NOT REFUSED. Refused, it refused
// every later backup too, and the trim's backup term — which waits on a backup
// it can see — stopped every log in the fleet from being trimmed over one lost
// file. An object the store ANSWERED it does not hold is gone whatever the
// backup does; the backup carries the rest, names the lost one by the file it
// belonged to, and is announced like any other.
func TestABackupRecordsAnObjectTheStoreHasLostAndCompletes(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	files, _ := filesStore(t)
	held := upload(t, files, "still held", "held by nobody")
	if err := files.Delete(t.Context(), held[1].object.Key); err != nil {
		t.Fatal(err)
	}
	filesNaming(t, db, held...)
	// A TRACKER CHECKPOINT, as any data node with files has: a copy with no
	// log position has nothing to announce.
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 1, 7)
	fleet := memory.NewFleet()
	svc := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "n", Holds: fleet, Backups: fleet,
		Now:     func() time.Time { return clock },
		Objects: &backup.Objects{Open: files.Open},
	})
	dir := filepath.Join(t.TempDir(), "with-a-lost-object")
	manifest, err := svc.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("Take = %v, want a backup naming the lost object", err)
	}
	if o := manifest.Objects; o == nil || o.Objects != 1 || len(o.Lost) != 1 ||
		o.Lost[0] != (backup.LostObject{Object: held[1].object.Key, NamedBy: "ENG/file-1"}) {
		t.Fatalf("manifest objects = %+v, want one object carried and the other named lost", o)
	}
	if _, err := os.Stat(filepath.Join(dir, backup.ManifestName)); err != nil {
		t.Fatalf("the backup wrote no manifest: %v", err)
	}
	points, err := fleet.BackupPoints(t.Context())
	if err != nil || len(points) != 1 || points[0].Owner != "n" {
		t.Fatalf("announced %+v, %v — a backup the trim cannot see does not unblock it", points, err)
	}
	// THE WHOLE ARTEFACT, objects included.
	var whole int64
	for _, st := range manifest.Stores {
		whole += st.Bytes
	}
	for _, st := range manifest.Streams {
		whole += st.Bytes
	}
	whole += manifest.Objects.Bytes
	if points[0].Bytes != whole {
		t.Errorf("announced %d bytes, want the whole artefact's %d", points[0].Bytes, whole)
	}
}

// A BACKUP TAKES WHAT ITS PREVIOUS ONE HOLDS, from this host's disk rather
// than from the store: a key is minted for one upload and never names other
// bytes, so an object an earlier backup already holds is the same object. A
// copy that rotted in the earlier artefact is read back against its row — by
// a streaming hash, never a whole read into memory — and read from the store
// again rather than carried forward, and the earlier file is left exactly as
// it was.
func TestABackupTakesWhatItsPreviousBackupHolds(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	files, _ := filesStore(t)
	held := upload(t, files, "first file", "second file")
	filesNaming(t, db, held...)
	// The previous backup is found through this node's own announcement,
	// which needs a log position to make.
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 1, 7)
	var read atomic.Int32
	fleet := memory.NewFleet()
	svc := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "n", Holds: fleet, Backups: fleet,
		Now:     func() time.Time { return clock },
		Objects: &backup.Objects{Open: counting(files, &read)},
	})
	root := t.TempDir()
	first := filepath.Join(root, "first")
	if _, err := svc.Take(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if got := read.Load(); got != 2 {
		t.Fatalf("the first backup read %d objects, want 2", got)
	}

	second := filepath.Join(root, "second")
	manifest, err := svc.Take(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if got := read.Load(); got != 2 {
		t.Errorf("the second backup read %d more objects its predecessor already held", got-2)
	}
	if o := manifest.Objects; o == nil || o.Objects != 2 || o.Reused != 2 ||
		o.ReusedFrom != filepath.Join(first, "objects") {
		t.Fatalf("manifest objects = %+v, want both objects reused from the first backup", o)
	}
	restoresWhole(t, filepath.Join(second, manifest.Objects.Dir), held)

	// A ROTTEN COPY IN THE EARLIER ARTEFACT IS READ AGAIN — one of the
	// same size, so only the hash can tell. The second backup linked the
	// first's files, so a third is the one that finds the rot, written
	// over the second's copy, the newest previous backup.
	rotten := filepath.Join(second, manifest.Objects.Dir, "files", held[0].object.Key.String())
	if err := os.Remove(rotten); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rotten, []byte("first fil3"), 0o600); err != nil {
		t.Fatal(err)
	}
	third := filepath.Join(root, "third")
	manifest, err = svc.Take(t.Context(), third)
	if err != nil {
		t.Fatal(err)
	}
	if got := read.Load(); got != 3 {
		t.Errorf("the third backup read %d objects, want only the rotten one", got-2)
	}
	if o := manifest.Objects; o.Reused != 1 || o.Objects != 2 {
		t.Fatalf("manifest objects = %+v, want one reused and one read", o)
	}
	restoresWhole(t, filepath.Join(third, manifest.Objects.Dir), held)
	if b, err := os.ReadFile(rotten); err != nil || string(b) != "first fil3" {
		t.Errorf("the earlier artefact's file was changed: %q, %v", b, err)
	}
}

// restoresWhole fails unless dir holds every object of held under the name the
// store keeps it under — the layout a sync into an S3 bucket's prefix
// restores.
func restoresWhole(t *testing.T, dir string, held []stored) {
	t.Helper()
	for _, o := range held {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(o.object.Key.Name())))
		if err != nil || !bytes.Equal(got, o.data) {
			t.Fatalf("object %s from %s = %q, %v", o.object.Key, dir, got, err)
		}
	}
}
