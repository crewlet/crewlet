package backup_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	"github.com/crewlet/crewlet/internal/store"
)

// fileWithChunks puts a file row and its chunk rows straight into the
// replicated estate, as an applier would have.
func fileWithChunks(t *testing.T, db *store.DB, chunks ...[]byte) {
	t.Helper()
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO tracker_files
			(id, project_key, path, created_at, updated_at, version, document)
			VALUES ('ENG.x', 'ENG', 'x', 0, 0, 1, x'7b7d')`); err != nil {
			return err
		}
		for i, c := range chunks {
			h := objstore.HashOf(c)
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO tracker_file_chunks
				(file_id, seq, chunk, size, pg) VALUES ('ENG.x', ?, ?, ?, ?)`,
				i, string(h), len(c), h.PG()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A BACKUP CARRIES EVERY CHUNK THE STORE COPY NAMES, in the layout a node's own
// chunk directory has — so a restore is a directory copy — and says how many.
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
		Store: db, NodeID: "n", Holds: fleet, Backups: fleet,
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
	// RESTORABLE AS A DIRECTORY: opened as a chunk store, it serves every
	// chunk under its name.
	restored, err := disk.Open(filepath.Join(dir, manifest.Objects.Dir))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for h, want := range held {
		got, err := restored.Get(h)
		if err != nil || string(got) != string(want) {
			t.Fatalf("chunk %s from the backup = %q, %v", h, got, err)
		}
	}
}

// A COPY NAMING A CHUNK NOBODY COULD SUPPLY IS NOT A BACKUP: no manifest is
// written, because a restore from it would bring back files whose bytes are
// nowhere.
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
				Store: db, NodeID: "n", Holds: fleet, Backups: fleet,
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
