package cache

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// THE MULTI-PACK-INDEX's writers, in one file so the writer of an index is one
// file (TestNothingButSyncWritesACache's allowance holds them to it).
//
// A FULL ROUND THAT LEAVES MANY PACKS INDEXES THEM BEFORE IT ANSWERS. One push
// is one pack and nothing compacts them, so a repository reaches thousands;
// without a multi-pack-index every object lookup binary-searches each pack's
// own .idx in turn, and the request a cold cache was synced for — a clone on a
// node that just joined, the first fetch after a restart — would be served by
// pack-objects searching every pack for every object. The round's index has
// NO BITMAP: it is a merge of the packs' own indexes, proportional to their
// objects, where a bitmap is a walk of the whole history no request should
// wait for. It runs in the READ lane, because a request waits on it.

// indexRound writes a multi-pack-index without a bitmap over exactly the
// snapshot's packs, for a full round, when the snapshot names MidxPacks or
// more packs and the incarnation holds NO multi-pack-index file — one stat,
// never the state's count, which reads zero beside an index on disk when the
// state was unreadable. A round never writes over an index that exists: git
// deletes a bitmap that does not match the index it writes.
//
// IT RUNS BEFORE THE REFS, because the refs' peel reads every tag the round
// sets out of the packs, and without an index each lookup searches every
// pack's .idx in turn — the very cost the index removes: at 5 000 packs of one
// commit and one annotated tag each (git 2.43, the cat-file the peel runs over
// all 5 000 tags, six runs each on four shared vCPUs) the peel took 1.0–1.3 s
// without the index and 0.23–0.31 s with it, for a 0.16 s write. Over
// exactly the snapshot's packs (--stdin-packs), so a stray the sweep is about
// to remove is never in it.
//
// Never a failure the round fails on: a busy lane or a failed write is
// logged, the lock file a killed write leaves removed, and the round answers
// without the index — slower, never wrong. It reports whether it wrote.
func (c *Cache) indexRound(ctx context.Context, e *entry, inc *incarnation, snap snapshot) bool {
	packs := len(snap.packs)
	if packs < MidxPacks {
		return false
	}
	packDir := filepath.Join(inc.dir, "objects", "pack")
	if _, err := os.Lstat(filepath.Join(packDir, "multi-pack-index")); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	var names bytes.Buffer
	var indexBytes int64
	for _, p := range snap.packs {
		names.WriteString("pack-" + p.GitChecksum + ".idx\n")
		indexBytes += p.IndexBytes
	}
	_, err := c.git.Run(ctx, gitproc.Cmd{
		Dir: inc.dir, Args: []string{"multi-pack-index", "write", "--stdin-packs", "--no-progress"},
		Config: c.ServeConfig(), Class: gitproc.ClassRead, Stdin: &names,
		Deadline: gitproc.PackWorkDeadline(indexBytes),
	})
	if err != nil {
		c.log.Warn("code_cache_round_index_skipped", "repo", e.id, "packs", packs, "error", err)
		lock := filepath.Join(packDir, "multi-pack-index.lock")
		if rerr := os.Remove(lock); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			c.log.Warn("code_cache_trash_failed", "path", lock, "error", rerr)
		}
		return false
	}
	meta, err := metaBytes(inc.dir)
	e.mu.Lock()
	defer e.mu.Unlock()
	// LastRun and Took stay the maintainer's: the round's index is no
	// maintenance run, and the bitmap the maintainer adds is due at once.
	inc.midx.Packs, inc.midx.Bitmap = packs, false
	if err == nil {
		c.setSize(inc, inc.diskBytes, meta)
	}
	return true
}

// metaBytes is the size of the maintainer's files in the incarnation at dir:
// the multi-pack-index's files and the commit-graph.
func metaBytes(dir string) (int64, error) {
	packDir := filepath.Join(dir, "objects", "pack")
	list, err := os.ReadDir(packDir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, d := range list {
		if !midxFile.MatchString(d.Name()) {
			continue
		}
		info, err := d.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	if info, err := os.Lstat(filepath.Join(dir, "objects", "info", "commit-graph")); err == nil {
		total += info.Size()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	return total, nil
}
