package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	"github.com/crewlet/crewlet/internal/objstore/transfer"
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

	// Chunks and Bytes are how many and how large — every chunk the
	// artefact holds, a reused one included, since shipping the directory
	// ships it.
	Chunks int   `json:"chunks"`
	Bytes  int64 `json:"bytes"`

	// Reused is how many of them were taken from this node's previous
	// backup on the same host (ReusedFrom) rather than fetched across the
	// fleet: chunks are named by their content, so one an earlier backup
	// already holds is the same bytes — linked where the filesystem allows
	// and copied where it does not, and read back against its name either
	// way. Each is a complete file of this artefact, never a reference into
	// the other one.
	Reused     int    `json:"reused,omitempty"`
	ReusedFrom string `json:"reused_from,omitempty"`

	// Lost is every chunk the copy names that the fleet no longer holds:
	// every member of the placement map ANSWERED that it has no copy.
	//
	// RECORDED RATHER THAN REFUSED. Refusing would not bring the chunk
	// back, and every later backup would be refused for the same chunk —
	// which stops the trim's backup term from ever advancing, so every log
	// in the fleet grows towards its ceiling over one file's lost
	// mebibyte. The artefact restores everything else, the files these
	// chunks belong to restore with their bytes missing exactly as they are
	// missing now, and the `objects_missing` alarm is what sends somebody
	// to replace them. A chunk a member could not ANSWER about is another
	// matter — it may be intact there — and still refuses the backup
	// ([ErrObjectsUnreachable]).
	Lost []objstore.Hash `json:"lost,omitempty"`
}

// ErrObjectsUnreachable is a copy that names chunks this backup could not
// read and whose absence is not definite — a member that may hold them did
// not answer, refused, or could not read its own copy. The backup is refused
// rather than written without them: they may well exist, a later attempt may
// reach them, and a restore missing them would bring back files whose bytes
// the fleet still had.
var ErrObjectsUnreachable = errors.New("backup: the copy names chunks the backup could not read")

// backupFetchConcurrency is how many chunks a backup fetches at once.
//
// FOUR, the repair's own recovery throttle (objstore/upkeep's
// fetchConcurrency) and for its reason: a backup is background work riding
// the broker every upload and read also rides, so it may overlap round trips
// but must not saturate it. One at a time made a backup's duration the corpus
// times a round trip — about an hour and a half for a million chunks at five
// milliseconds each, while the trim waited on it.
const backupFetchConcurrency = 4

// referencedIn is every chunk the replicated copy at path names.
func referencedIn(ctx context.Context, path string, tables []objstore.ReferenceTable) ([]objstore.Hash, error) {
	if len(tables) == 0 {
		return nil, nil
	}
	db, err := store.OpenEstate(ctx, store.EstatePartition, path, store.Options{})
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

// copyObjects writes every chunk the copy names into the backup, taking each
// from prev — the chunk directory of this node's previous backup, empty when
// there is none — where it is there and intact, and fetching the rest.
func copyObjects(ctx context.Context, dir string, hashes []objstore.Hash, objs *Objects,
	prev string) (*ObjectArtifact, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	if objs == nil || objs.Get == nil {
		return nil, fmt.Errorf("%w: %d chunks, and this node runs no object client",
			ErrObjectsUnreachable, len(hashes))
	}
	root := filepath.Join(dir, objectsDirName)
	out := &ObjectArtifact{Dir: objectsDirName}
	if prev != "" {
		out.ReusedFrom = prev
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu          sync.Mutex
		unreachable []string
		failed      error
	)
	work := make(chan objstore.Hash)
	var wg sync.WaitGroup
	for range min(backupFetchConcurrency, len(hashes)) {
		wg.Go(func() {
			for h := range work {
				size, reused, err := placeChunk(ctx, root, prev, h, objs)
				mu.Lock()
				switch {
				case err == nil:
					out.Chunks++
					out.Bytes += size
					if reused {
						out.Reused++
					}
				case errors.Is(err, transfer.ErrNotFound):
					out.Lost = append(out.Lost, h)
				case errors.Is(err, errDestination):
					// THE DESTINATION, not the fleet: every other chunk
					// would fail the same way, so the backup stops here.
					if failed == nil {
						failed = err
					}
					cancel()
				default:
					unreachable = append(unreachable, fmt.Sprintf("%s: %v", h, err))
				}
				mu.Unlock()
			}
		})
	}
feed:
	for _, h := range hashes {
		select {
		case work <- h:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()

	if failed != nil {
		return nil, failed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(unreachable) > 0 {
		sort.Strings(unreachable)
		shown := unreachable
		if len(shown) > 5 {
			shown = append(shown[:5:5], fmt.Sprintf("and %d more", len(unreachable)-5))
		}
		return nil, fmt.Errorf("%w: %d of %d (%v) — the objects_degraded alarm fires on "+
			"the node that could not reach a member that may hold one; take the backup "+
			"again once it answers", ErrObjectsUnreachable, len(unreachable),
			len(hashes), shown)
	}
	sort.Slice(out.Lost, func(i, j int) bool { return out.Lost[i] < out.Lost[j] })
	return out, nil
}

// errDestination is a failure writing the backup itself rather than reading a
// chunk — the directory, not the fleet.
var errDestination = errors.New("backup: the destination")

// placeChunk puts one chunk into the backup: from prev when it holds an intact
// copy, else from the fleet. It answers the chunk's size and whether it was
// reused.
func placeChunk(ctx context.Context, root, prev string, h objstore.Hash,
	objs *Objects) (int64, bool, error) {
	if prev != "" {
		if size, ok, err := reuseChunk(root, prev, h); err != nil || ok {
			return size, ok, err
		}
	}
	data, err := objs.Get(ctx, h)
	if err != nil {
		return 0, false, err
	}
	if err := writeChunk(root, h, data); err != nil {
		return 0, false, err
	}
	return int64(len(data)), false, nil
}

// reuseChunk takes one chunk from the previous backup's directory, reporting
// false — and leaving nothing behind — when that copy is absent or no longer
// hashes to its name, so the caller fetches it.
//
// LINKED WHERE IT CAN BE, because a chunk never changes once written and a
// second name for the same file costs no disk at all; COPIED where the link is
// refused, a previous backup on another filesystem among them. Either way the
// bytes are READ BACK against the name before they count: a copy that rotted
// in the earlier artefact would otherwise be carried into every later one, and
// reading a local file is still a fraction of fetching it across the fleet.
func reuseChunk(root, prev string, h objstore.Hash) (int64, bool, error) {
	src := filepath.Join(prev, disk.Layout(h))
	dst := filepath.Join(root, disk.Layout(h))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, false, fmt.Errorf("%w: create %s: %w", errDestination, filepath.Dir(dst), err)
	}
	if linkErr := os.Link(src, dst); linkErr != nil {
		if errors.Is(linkErr, os.ErrNotExist) {
			return 0, false, nil
		}
		data, ok := intactAt(src, h)
		if !ok {
			return 0, false, nil
		}
		if err := writeChunk(root, h, data); err != nil {
			return 0, false, err
		}
		return int64(len(data)), true, nil
	}
	data, ok := intactAt(dst, h)
	if !ok {
		// THE LINK GOES, never the earlier artefact's file: that one is
		// somebody's backup, and judging it is not this backup's place.
		if err := os.Remove(dst); err != nil {
			return 0, false, fmt.Errorf("%w: remove %s: %w", errDestination, dst, err)
		}
		return 0, false, nil
	}
	return int64(len(data)), true, nil
}

// intactAt reads the chunk at path and reports whether it is h's bytes. A file
// that will not read is no copy, for the reason one that does not hash to its
// name is none: either way the chunk is fetched instead.
func intactAt(path string, h objstore.Hash) ([]byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil || objstore.HashOf(data) != h {
		return nil, false
	}
	return data, true
}

// writeChunk writes one chunk, synced, at its place in the layout. Every
// failure is the destination's ([errDestination]).
func writeChunk(root string, h objstore.Hash, data []byte) error {
	path := filepath.Join(root, disk.Layout(h))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%w: create %s: %w", errDestination, filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("%w: write chunk %s: %w", errDestination, h, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("%w: write chunk %s: %w", errDestination, h, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("%w: sync chunk %s: %w", errDestination, h, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%w: close chunk %s: %w", errDestination, h, err)
	}
	return nil
}

// objectBytes is the size of every chunk the artefact holds, or 0.
func objectBytes(m Manifest) int64 {
	if m.Objects == nil {
		return 0
	}
	return m.Objects.Bytes
}
