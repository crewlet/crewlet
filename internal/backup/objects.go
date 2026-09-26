package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	"github.com/crewlet/crewlet/internal/store"
)

// objectsDirName holds the chunks a backup carries, in the layout a node's own
// chunk directory has — so restoring them is copying the directory into
// `store.objects.dir` (docs/guides/backup.md).
const objectsDirName = "objects"

// Objects is how a backup reads a chunk from wherever the fleet holds it.
//
// # Why a backup carries every chunk, and not this node's share
//
// A node holds only the chunks the placement map puts on it, so a copy of its
// disk is a fraction of the company's files — and a restore from it would be
// a tracker naming files whose bytes are nowhere. The backup is a copy of the
// COMPANY, taken from one node: so it reads every chunk the store copy refers
// to, from this node's disk where it can and from a peer where it must.
//
// WHICH CHUNKS is not an option: the copy is read against
// internal/objstore/references, the one list its schema gate holds, so a
// backup can never be built knowing fewer referencing tables than the
// collector does.
type Objects struct {
	// Get reads one chunk from wherever the fleet holds it.
	Get func(ctx context.Context, h objstore.Hash) ([]byte, error)
}

// ObjectArtifact describes the chunks inside a backup.
type ObjectArtifact struct {
	// Dir is where they are, relative to the backup directory.
	Dir string `json:"dir"`

	// Chunks and Bytes are how many and how large.
	Chunks int   `json:"chunks"`
	Bytes  int64 `json:"bytes"`
}

// ErrObjectsUnreachable is a copy that names chunks this backup could not
// read. The backup is refused rather than written without them: a restore
// would bring back files whose bytes are nowhere, and nothing would say so
// until somebody opened one.
var ErrObjectsUnreachable = errors.New("backup: the copy names chunks the backup could not read")

// referencedIn is every chunk the replicated copy at path names.
func referencedIn(ctx context.Context, path string, tables []objstore.ReferenceTable) ([]objstore.Hash, error) {
	if len(tables) == 0 {
		return nil, nil
	}
	db, err := store.OpenEstate(ctx, store.EstateReplicated, path, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("backup: open the copy to read its chunks: %w", err)
	}
	defer func() { _ = db.Close() }()
	seen := map[objstore.Hash]bool{}
	for _, t := range tables {
		query, err := t.Chunks()
		if err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		if err := db.Read(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, query)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var raw string
				if err := rows.Scan(&raw); err != nil {
					return err
				}
				h, err := objstore.ParseHash(raw)
				if err != nil {
					return fmt.Errorf("%s.%s: %w", t.Table, t.Column, err)
				}
				seen[h] = true
			}
			return rows.Err()
		}); err != nil {
			return nil, fmt.Errorf("backup: read the chunks the copy names: %w", err)
		}
	}
	out := make([]objstore.Hash, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// copyObjects writes every chunk the copy names into the backup.
func copyObjects(ctx context.Context, dir string, hashes []objstore.Hash, objs *Objects) (*ObjectArtifact, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	if objs == nil || objs.Get == nil {
		return nil, fmt.Errorf("%w: %d chunks, and this node runs no object client",
			ErrObjectsUnreachable, len(hashes))
	}
	root := filepath.Join(dir, objectsDirName)
	out := &ObjectArtifact{Dir: objectsDirName}
	var missing []string
	for _, h := range hashes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := objs.Get(ctx, h)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s: %v", h, err))
			continue
		}
		if err := writeChunk(root, h, data); err != nil {
			return nil, err
		}
		out.Chunks++
		out.Bytes += int64(len(data))
	}
	if len(missing) > 0 {
		shown := missing
		if len(shown) > 5 {
			shown = append(shown[:5:5], fmt.Sprintf("and %d more", len(missing)-5))
		}
		return nil, fmt.Errorf("%w: %d of %d (%v) — the objects_missing alarm names "+
			"which node should hold them", ErrObjectsUnreachable, len(missing), len(hashes), shown)
	}
	return out, nil
}

// writeChunk writes one chunk, synced, at its place in the layout.
func writeChunk(root string, h objstore.Hash, data []byte) error {
	path := filepath.Join(root, disk.Layout(h))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("backup: create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("backup: write chunk %s: %w", h, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("backup: write chunk %s: %w", h, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("backup: sync chunk %s: %w", h, err)
	}
	return f.Close()
}
