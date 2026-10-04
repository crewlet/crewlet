package backup_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// fileWithChunks puts a file row and its chunk rows straight into the
// replicated estate, as an applier would have.
func fileWithChunks(t *testing.T, db *store.DB, chunks ...[]byte) {
	t.Helper()
	if err := storetest.EstateOf(db).Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO tracker_files
			(id, project_key, path, created_at, updated_at, version, document)
			VALUES ('ENG.x', 'ENG', 'x', 0, 0, 1, x'7b7d')`); err != nil {
			return err
		}
		for i, c := range chunks {
			h := objstore.HashOf(c)
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO tracker_file_chunks
				(file_id, seq, chunk, size) VALUES ('ENG.x', ?, ?, ?)`,
				i, string(h), len(c)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A BACKUP CARRIES EVERY CHUNK THE STORE COPY NAMES, one file per chunk named
// by its hash — the key layout an S3 backend's bucket has, so a restore is one
// sync of the directory — and says how many.
func TestABackupCarriesTheChunksItsCopyNames(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	held := map[objstore.Hash][]byte{}
	var chunks [][]byte
	for _, body := range []string{"one chunk", "another chunk"} {
		held[objstore.HashOf([]byte(body))] = []byte(body)
		chunks = append(chunks, []byte(body))
	}
	fileWithChunks(t, db, chunks...)
	fleet := memory.NewFleet()
	svc := build(t, backup.Options{
		Store: db, Partitions: holdsTheEstate, NodeID: "n", Holds: fleet, Backups: fleet,
		Now: func() time.Time { return clock },
		Objects: &backup.Objects{Get: func(_ context.Context, h objstore.Hash) ([]byte, error) {
			if b, ok := held[h]; ok {
				return b, nil
			}
			return nil, errors.New("nobody holds it")
		}},
	})
	dir := filepath.Join(t.TempDir(), "with-files")
	manifest, err := svc.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if manifest.Objects == nil || manifest.Objects.Chunks != 2 ||
		manifest.Objects.Bytes != int64(len("one chunk")+len("another chunk")) {
		t.Fatalf("manifest objects = %+v", manifest.Objects)
	}
	restoresWhole(t, filepath.Join(dir, manifest.Objects.Dir), held)
}

// A BACKUP WHOSE CHUNKS LIVE IN A STREAM IT SNAPSHOTS COPIES NONE BESIDE IT:
// the NATS backend's bucket is a stream, and its snapshot is the copy.
func TestABackupOfChunksInAStreamNamesTheStream(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	fileWithChunks(t, db, []byte("one chunk"), []byte("another"))
	fleet := memory.NewFleet()
	svc := build(t, backup.Options{
		Store: db, Partitions: holdsTheEstate, NodeID: "n", Holds: fleet, Backups: fleet,
		Now: func() time.Time { return clock },
		Objects: &backup.Objects{Stream: "OBJ_crewlet_files",
			Get: func(context.Context, objstore.Hash) ([]byte, error) {
				t.Error("a chunk was fetched although the stream snapshot carries it")
				return nil, errors.New("not expected")
			}},
	})
	dir := filepath.Join(t.TempDir(), "in-stream")
	manifest, err := svc.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if o := manifest.Objects; o == nil || o.Stream != "OBJ_crewlet_files" || o.Chunks != 2 || o.Dir != "" {
		t.Fatalf("manifest objects = %+v, want the stream named and no directory", o)
	}
	if _, err := os.Stat(filepath.Join(dir, "objects")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a chunk directory was written beside a stream that carries the chunks")
	}
}

// A COPY NAMING A CHUNK NOBODY COULD SUPPLY — AND WHOSE ABSENCE IS NOT
// DEFINITE — IS NOT A BACKUP: no manifest is written, because the chunk may be
// intact in a store that did not answer, and a restore from it would bring
// back files whose bytes the store still had.
func TestABackupMissingAChunkWritesNoManifest(t *testing.T) {
	t.Parallel()
	for name, objects := range map[string]*backup.Objects{
		"a chunk nobody holds": {Get: func(context.Context, objstore.Hash) ([]byte, error) {
			return nil, errors.New("nobody holds it")
		}},
		"no object client at all": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := openStore(t)
			fileWithChunks(t, db, []byte("lost"))
			fleet := memory.NewFleet()
			svc := build(t, backup.Options{
				Store: db, Partitions: holdsTheEstate, NodeID: "n", Holds: fleet, Backups: fleet,
				Now: func() time.Time { return clock }, Objects: objects,
			})
			dir := filepath.Join(t.TempDir(), "incomplete")
			if _, err := svc.Take(t.Context(), dir); !errors.Is(err, backup.ErrObjectsUnreachable) {
				t.Fatalf("Take = %v, want ErrObjectsUnreachable", err)
			}
			if _, err := os.Stat(filepath.Join(dir, backup.ManifestName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a backup missing a chunk wrote its manifest")
			}
		})
	}
}

// A CHUNK THE STORE HAS LOST IS RECORDED, NOT REFUSED. Refused, it refused
// every later backup too, and the trim's backup term — which waits on a backup
// it can see — stopped every log in the fleet from being trimmed over one
// file's missing mebibyte. A chunk the store ANSWERED it does not hold is gone
// whatever the backup does; the backup carries the rest, names the lost one,
// and is announced like any other.
func TestABackupRecordsAChunkTheStoreHasLostAndCompletes(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	kept, lost := []byte("still held"), []byte("held by nobody")
	fileWithChunks(t, db, kept, lost)
	// A TRACKER CHECKPOINT, as any data node with files has: a copy with no
	// log position has nothing to announce.
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 1, 7)
	fleet := memory.NewFleet()
	svc := build(t, backup.Options{
		Store: db, Partitions: holdsTheEstate, NodeID: "n", Holds: fleet, Backups: fleet,
		Now: func() time.Time { return clock },
		Objects: &backup.Objects{Get: func(_ context.Context, h objstore.Hash) ([]byte, error) {
			if h == objstore.HashOf(kept) {
				return kept, nil
			}
			return nil, fmt.Errorf("%w: %s", objstore.ErrNotFound, h)
		}},
	})
	dir := filepath.Join(t.TempDir(), "with-a-lost-chunk")
	manifest, err := svc.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("Take = %v, want a backup naming the lost chunk", err)
	}
	if o := manifest.Objects; o == nil || o.Chunks != 1 ||
		len(o.Lost) != 1 || o.Lost[0] != objstore.HashOf(lost) {
		t.Fatalf("manifest objects = %+v, want one chunk carried and the other named lost", o)
	}
	if _, err := os.Stat(filepath.Join(dir, backup.ManifestName)); err != nil {
		t.Fatalf("the backup wrote no manifest: %v", err)
	}
	points, err := fleet.BackupPoints(t.Context())
	if err != nil || len(points) != 1 || points[0].Owner != "n" {
		t.Fatalf("announced %+v, %v — a backup the trim cannot see does not unblock it", points, err)
	}
	// THE WHOLE ARTEFACT, chunks included: the announced size left them out.
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

// A BACKUP TAKES WHAT ITS PREVIOUS ONE HOLDS, from this host's disk rather than
// across the fleet: a chunk is named by its content, so one an earlier backup
// already holds is the same bytes. Every backup used to fetch every chunk the
// company has, one round trip at a time — the corpus over the broker on every
// run. A copy that rotted in the earlier artefact is read back against its
// name and fetched again rather than carried forward, and the earlier file is
// left exactly as it was.
func TestABackupTakesWhatItsPreviousBackupHolds(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	one, two := []byte("first chunk"), []byte("second chunk")
	fileWithChunks(t, db, one, two)
	// The previous backup is found through this node's own announcement,
	// which needs a log position to make.
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 1, 7)
	held := map[objstore.Hash][]byte{objstore.HashOf(one): one, objstore.HashOf(two): two}
	var fetched atomic.Int32
	fleet := memory.NewFleet()
	svc := build(t, backup.Options{
		Store: db, Partitions: holdsTheEstate, NodeID: "n", Holds: fleet, Backups: fleet,
		Now: func() time.Time { return clock },
		Objects: &backup.Objects{Get: func(_ context.Context, h objstore.Hash) ([]byte, error) {
			fetched.Add(1)
			if b, ok := held[h]; ok {
				return b, nil
			}
			return nil, errors.New("nobody holds it")
		}},
	})
	root := t.TempDir()
	first := filepath.Join(root, "first")
	if _, err := svc.Take(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if got := fetched.Load(); got != 2 {
		t.Fatalf("the first backup fetched %d chunks, want 2", got)
	}

	second := filepath.Join(root, "second")
	manifest, err := svc.Take(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if got := fetched.Load(); got != 2 {
		t.Errorf("the second backup fetched %d more chunks its predecessor already held", got-2)
	}
	if o := manifest.Objects; o == nil || o.Chunks != 2 || o.Reused != 2 ||
		o.ReusedFrom != filepath.Join(first, "objects") {
		t.Fatalf("manifest objects = %+v, want both chunks reused from the first backup", o)
	}
	restoresWhole(t, filepath.Join(second, manifest.Objects.Dir), held)

	// A ROTTEN COPY IN THE EARLIER ARTEFACT IS FETCHED AGAIN. The second
	// backup linked the first's files, so a third is the one that finds the
	// rot — written over the second's copy, the newest previous backup.
	rotten := filepath.Join(second, manifest.Objects.Dir, string(objstore.HashOf(one)))
	if err := os.Remove(rotten); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rotten, []byte("not the first chunk"), 0o600); err != nil {
		t.Fatal(err)
	}
	third := filepath.Join(root, "third")
	manifest, err = svc.Take(t.Context(), third)
	if err != nil {
		t.Fatal(err)
	}
	if got := fetched.Load(); got != 3 {
		t.Errorf("the third backup fetched %d chunks, want only the rotten one", got-2)
	}
	if o := manifest.Objects; o.Reused != 1 || o.Chunks != 2 {
		t.Fatalf("manifest objects = %+v, want one reused and one fetched", o)
	}
	restoresWhole(t, filepath.Join(third, manifest.Objects.Dir), held)
	if b, err := os.ReadFile(rotten); err != nil || string(b) != "not the first chunk" {
		t.Errorf("the earlier artefact's file was changed: %q, %v", b, err)
	}
}

// restoresWhole fails unless dir holds every chunk of held as a file named by
// its hash — the layout a sync into an S3 bucket's prefix restores.
func restoresWhole(t *testing.T, dir string, held map[objstore.Hash][]byte) {
	t.Helper()
	for h, want := range held {
		got, err := os.ReadFile(filepath.Join(dir, string(h)))
		if err != nil || string(got) != string(want) {
			t.Fatalf("chunk %s from %s = %q, %v", h, dir, got, err)
		}
	}
}
