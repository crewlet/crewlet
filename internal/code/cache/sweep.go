package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
)

// THE SWEEP, on a FULL answer only: the incarnation made exactly its IMAGE,
// and nothing else. The image is a grammar — anything outside it is renamed
// into trash/, one rename per top-level entry, and removed by the janitor:
//
//	the root        HEAD, config, packed-refs, crewlet-state.json, refs/, objects/
//	refs/           heads/ and tags/, each EMPTY: there are never loose refs,
//	                which outrank packed-refs for every reader
//	objects/        pack/ and info/: nothing the engine runs in a cache
//	                creates a loose object
//	objects/info/   commit-graph (the maintainer's); an alternates file here
//	                would serve another repository's objects by id, and the
//	                pool refuses it (gitproc.ShapeAlternates)
//	objects/pack/   pack-<cs>.{pack,idx,rev} for every pack the rows name, and
//	                the multi-pack-index files — unless a pack is removed
//
// It runs in TWO HALVES around the install. Everything but the pack files
// first (tidy), because a git process — the round's peel, its index — runs
// in the directory before the packs' half could: the pool refuses one whose
// config is not gitproc.RepoConfig or that holds a commondir or an alternates
// file — every gitproc.ShapeRule an incarnation can break — and git cannot
// read a repository whose HEAD is not a symbolic ref, so a cache damaged there
// would refuse every round that could repair it. This half is what REPAIRS a
// cache the pool refused for its shape, in place, reusing every pack it holds
// (markWhole); and what it leaves is held to gitproc.CheckShape, the
// pool's own predicate. objects/pack is listed once, after the install (packStrays), and
// whatever there is no pack's file goes at that listing, before the round's
// index: no reader of any ref reads one, and a dead writer's lock among them
// — a maintenance run killed with its node — would refuse the index, since git
// writes none over another writer's lock. The pack files last (sweepPacks),
// once the new refs are in, so a reader of the old refs never loses a pack
// they name.
//
// A DELTA SWEEPS NOTHING: between two rounds of one process nothing but this
// package has written the directory, and every writer here leaves only image
// entries — so the steady-state work of a sync is proportional to what moved,
// never to the directory.

// THE GRAMMAR, one predicate a directory: what the sweep keeps and Verify
// admits, written once so the two never disagree about what a cache is.

// The root's image: each name and whether it is a directory.
var rootImage = map[string]bool{
	"HEAD": false, "config": false, "packed-refs": false, stateName: false,
	"refs": true, "objects": true,
}

// inRoot admits an entry of an incarnation's root.
func inRoot(name string, dir bool) bool {
	isDir, ok := rootImage[name]
	return ok && isDir == dir
}

// inRefs admits an entry of refs/: heads/ and tags/, each to stay empty.
func inRefs(name string, dir bool) bool { return dir && (name == "heads" || name == "tags") }

// inObjects admits an entry of objects/: pack/ and info/.
func inObjects(name string, dir bool) bool { return dir && (name == "pack" || name == "info") }

// inInfo admits an entry of objects/info/: the maintainer's commit-graph.
func inInfo(name string, dir bool) bool { return !dir && name == "commit-graph" }

// midxFile matches the multi-pack-index's files.
var midxFile = regexp.MustCompile(`^multi-pack-index(-[0-9a-f]{40}\.(bitmap|rev))?$`)

// packFileName matches a pack's three files, its checksum the first group:
// which of them the image keeps is the rows' to say.
var packFileName = regexp.MustCompile(`^pack-([0-9a-f]{40})\.(pack|idx|rev)$`)

// tidy is the sweep's first half: every directory of the image made, every
// entry outside it but the pack files moved into trash/, config made
// gitproc.RepoConfig and a HEAD git cannot read made the snapshot's — and the
// directory it leaves held to gitproc.CheckShape, whose refusal it answers. It
// reports whether it moved anything.
func (c *Cache) tidy(inc *incarnation, snap snapshot) (bool, error) {
	// The root first: a symbolic link named objects or refs is outside the
	// image, and a directory made through it would be made wherever it
	// points.
	t := &trashing{c: c}
	t.outside(inc.dir, inRoot)
	if t.err != nil {
		return t.moved, t.err
	}
	for _, d := range []string{"refs/heads", "refs/tags", "objects/pack", "objects/info"} {
		if err := c.ensureDir(inc.dir, d); err != nil {
			return t.moved, err
		}
	}
	t.outside(filepath.Join(inc.dir, "refs"), inRefs)
	for _, d := range []string{"refs/heads", "refs/tags"} {
		t.outside(filepath.Join(inc.dir, d), func(string, bool) bool { return false })
	}
	t.outside(filepath.Join(inc.dir, "objects"), inObjects)
	t.outside(filepath.Join(inc.dir, "objects", "info"), inInfo)
	if t.err != nil {
		return t.moved, t.err
	}

	replace := func(name string, want []byte, ok func([]byte) bool) error {
		on, err := os.ReadFile(filepath.Join(inc.dir, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err == nil && ok(on) {
			return nil
		}
		staged, err := c.stage(hostbox.FileMode, func(f *os.File) error {
			_, werr := f.Write(want)
			return werr
		})
		if err != nil {
			return err
		}
		return c.place(staged, filepath.Join(inc.dir, name))
	}
	if err := replace("config", []byte(gitproc.RepoConfig), func(on []byte) bool {
		return string(on) == gitproc.RepoConfig
	}); err != nil {
		return t.moved, err
	}
	if err := replace("HEAD", headFor(snap.repo.DefaultBranch), headValid); err != nil {
		return t.moved, err
	}
	for _, d := range t.dirs(inc.dir) {
		if err := syncDir(d); err != nil {
			return t.moved, err
		}
	}
	if c.knobs != nil && c.knobs.swept != nil {
		c.knobs.swept(inc.dir)
	}
	// HELD TO THE POOL'S OWN PREDICATE, not a copy of it: what the sweep
	// leaves is exactly what every git process in the incarnation is checked
	// against, so a full round never answers a directory the pool refuses —
	// the image grammar already moved a commondir and an alternates file
	// out, and config is RepoConfig, so a refusal here is a shape the sweep
	// could not put right (a filesystem that does not keep what it is
	// given), which Cache.run answers with a rebuild.
	return t.moved, gitproc.CheckShape(inc.dir, "")
}

// ensureDir makes rel under dir when it is missing; something else at its
// name — a file where a directory belongs — is moved into trash/ first.
func (c *Cache) ensureDir(dir, rel string) error {
	path := filepath.Join(dir, rel)
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		if err = toTrash(c.dir, path, newV7()+"-"+filepath.Base(path)); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return os.MkdirAll(path, hostbox.DirMode)
}

// trashing moves what lies outside the image into trash/, remembering which
// directories it touched.
type trashing struct {
	c       *Cache
	moved   bool
	touched map[string]bool
	err     error
}

// outside moves every entry of dir that keep refuses into trash/.
func (t *trashing) outside(dir string, keep func(name string, dir bool) bool) {
	if t.err != nil {
		return
	}
	list, err := os.ReadDir(dir)
	if err != nil {
		t.err = fmt.Errorf("listing %s: %w", dir, err)
		return
	}
	for _, d := range list {
		if keep(d.Name(), d.IsDir()) {
			continue
		}
		t.move(filepath.Join(dir, d.Name()))
		if t.err != nil {
			return
		}
	}
}

// move moves path into trash/.
func (t *trashing) move(path string) {
	if err := toTrash(t.c.dir, path, newV7()+"-"+filepath.Base(path)); err != nil {
		t.err = fmt.Errorf("moving %s into trash/: %w", path, err)
		return
	}
	if t.touched == nil {
		t.touched = map[string]bool{}
	}
	t.touched[filepath.Dir(path)] = true
	t.moved = true
}

// dirs is every directory the moves touched, and inc's own (where config
// and HEAD may have been replaced).
func (t *trashing) dirs(inc string) []string {
	out := []string{inc}
	for d := range t.touched {
		if d != inc {
			out = append(out, d)
		}
	}
	return out
}

// packStrays decides the sweep's second half on a full answer, before the
// round's index and its refs: every pack file of objects/pack the snapshot
// does not name — a stray, a pack no row names any more (a log gone back
// under the cache) — answered for sweepPacks to move once the new refs are
// in. When there is one, the multi-pack-index and the commit-graph go NOW,
// first: an index naming a missing pack fails every read of the repository,
// and the round's own index is then written over exactly the snapshot's
// packs.
//
// Every other entry the image does not admit goes now too, at this one
// listing: it is no pack's file, so no reader of the old refs reads it, and
// what such entries are — a lock or a temporary file a maintenance run
// killed with its node left, since no run is in flight beside a full round —
// is what would refuse the round's index below. Kept for sweepPacks, the lock
// was refused the index, whose failure removed it from under the sweep that
// had listed it, and the round failed on the move. It reports whether it
// moved anything.
func (c *Cache) packStrays(inc *incarnation, snap snapshot) ([]string, bool, error) {
	packDir := filepath.Join(inc.dir, "objects", "pack")
	want := map[string]bool{}
	for _, p := range snap.packs {
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			want["pack-"+p.GitChecksum+ext] = true
		}
	}
	list, err := os.ReadDir(packDir)
	if err != nil {
		return nil, false, err
	}
	var strays []string
	now := &trashing{c: c}
	for _, d := range list {
		name := d.Name()
		switch {
		case (want[name] || midxFile.MatchString(name)) && d.Type().IsRegular():
		case strings.HasPrefix(name, "pack-"):
			strays = append(strays, name)
		default:
			if now.move(filepath.Join(packDir, name)); now.err != nil {
				return nil, now.moved, now.err
			}
		}
	}
	if now.moved {
		if err = syncDir(packDir); err != nil {
			return nil, true, err
		}
	}
	if len(strays) > 0 {
		if err = c.dropIndexes(inc); err != nil {
			return nil, true, err
		}
	}
	return strays, now.moved || len(strays) > 0, nil
}

// sweepPacks is the sweep's second half, once the refs are in: every stray
// packStrays found is moved into trash/, so a reader of the old refs never
// loses a pack they name — and then the incarnation's counts are MEASURED,
// whatever they had drifted to.
func (c *Cache) sweepPacks(e *entry, inc *incarnation, strays []string) (bool, error) {
	packDir := filepath.Join(inc.dir, "objects", "pack")
	t := &trashing{c: c}
	for _, name := range strays {
		if t.move(filepath.Join(packDir, name)); t.err != nil {
			return true, t.err
		}
	}
	if t.moved {
		if err := syncDir(packDir); err != nil {
			return true, err
		}
	}
	if err := c.measure(e, inc); err != nil {
		return t.moved, err
	}
	return t.moved, nil
}

// dropIndexes moves the multi-pack-index files and the commit-graph into
// trash/ — before a pack they may name is removed.
func (c *Cache) dropIndexes(inc *incarnation) error {
	return c.dropDerived(inc, gitproc.PartMultiPackIndex|gitproc.PartCommitGraph)
}

// dropDerived moves the derived files drop names into trash/: the
// multi-pack-index's (gitproc.PartMultiPackIndex) and the commit-graph
// (gitproc.PartCommitGraph) — what the repair of damaged derived files drops
// (apply), by the part the damage named, and both when a pack they may name
// is removed (dropIndexes).
func (c *Cache) dropDerived(inc *incarnation, drop gitproc.Part) error {
	if drop&gitproc.PartMultiPackIndex != 0 {
		if err := c.dropMultiPackIndex(inc); err != nil {
			return err
		}
	}
	if drop&gitproc.PartCommitGraph != 0 {
		return c.dropCommitGraph(inc)
	}
	return nil
}

// derivedFiles names the files a drop of drop moves, for its log line.
func derivedFiles(drop gitproc.Part) []string {
	var files []string
	if drop&gitproc.PartMultiPackIndex != 0 {
		files = append(files, "multi-pack-index")
	}
	if drop&gitproc.PartCommitGraph != 0 {
		files = append(files, "commit-graph")
	}
	return files
}

// dropCommitGraph moves the commit-graph into trash/ and nothing else: what
// the repair of a damaged commit-graph drops. The multi-pack-index beside it
// names no commit-graph position, so it stays — dropped too, a repair of the
// graph would also throw away a bitmap that took the maintainer minutes to
// write. measure then finds the graph missing, and the maintainer writes it
// first (its graph-first rule), so the window in which walks go by date is
// that write's.
func (c *Cache) dropCommitGraph(inc *incarnation) error {
	graph := filepath.Join(inc.dir, "objects", "info", "commit-graph")
	if err := toTrash(c.dir, graph, newV7()+"-"+filepath.Base(graph)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(graph))
}

// dropMultiPackIndex moves the multi-pack-index files — the index, its bitmap
// and its reverse index — into trash/, and nothing else: what the repair of a
// damaged multi-pack-index drops (apply). The commit-graph beside it is a file
// of its own that names no position in the index, so it stays: dropped too, a
// repair of the index would also put every walk of the repository back on
// commit dates until the maintainer had written the graph again.
func (c *Cache) dropMultiPackIndex(inc *incarnation) error {
	packDir := filepath.Join(inc.dir, "objects", "pack")
	list, err := os.ReadDir(packDir)
	if err != nil {
		return err
	}
	for _, d := range list {
		if !midxFile.MatchString(d.Name()) {
			continue
		}
		path := filepath.Join(packDir, d.Name())
		if err = toTrash(c.dir, path, newV7()+"-"+d.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return syncDir(packDir)
}

// measure sets inc's counts from its directory: the packs installed (an .idx
// with its .pack), the bytes of their files and packed-refs, and the
// maintainer's files. A multi-pack-index no longer on disk is no longer
// covering anything, and a commit-graph no longer on disk is due again.
func (c *Cache) measure(e *entry, inc *incarnation) error {
	packDir := filepath.Join(inc.dir, "objects", "pack")
	list, err := os.ReadDir(packDir)
	if err != nil {
		return err
	}
	sizes := map[string]int64{}
	for _, d := range list {
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		sizes[d.Name()] = info.Size()
	}
	var packs int
	var disk, meta int64
	midx := false
	for name, size := range sizes {
		switch {
		case midxFile.MatchString(name):
			meta += size
			midx = midx || name == "multi-pack-index"
		case strings.HasSuffix(name, ".idx"):
			if _, ok := sizes[strings.TrimSuffix(name, ".idx")+".pack"]; ok {
				packs++
			}
			disk += size
		default:
			disk += size
		}
	}
	graph := false
	for _, f := range []struct {
		path  string
		into  *int64
		there *bool
	}{
		{filepath.Join(inc.dir, "packed-refs"), &disk, nil},
		{filepath.Join(inc.dir, "objects", "info", "commit-graph"), &meta, &graph},
	} {
		info, err := os.Lstat(f.path)
		switch {
		case err == nil:
			*f.into += info.Size()
			if f.there != nil {
				*f.there = true
			}
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
	}
	e.mu.Lock()
	inc.packs = packs
	c.setSize(inc, disk, meta)
	if !midx {
		inc.midx.Packs, inc.midx.Bitmap = 0, false
	}
	if !graph {
		inc.midx.Graph = false
	}
	e.mu.Unlock()
	return nil
}
