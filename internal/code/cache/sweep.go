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
//	                would chain a push's scratch to another repository
//	objects/pack/   pack-<cs>.{pack,idx,rev} for every pack the rows name, and
//	                the multi-pack-index files — unless a pack is removed
//
// It runs in TWO HALVES around the install. Everything but the pack files
// first (tidy), because a git process — the round's peel, its index — runs
// in the directory before the packs' half could: the pool refuses one whose
// config is not gitproc.RepoConfig or that holds a commondir or a foreign
// alternate, and git cannot read a repository whose HEAD is not a symbolic
// ref, so a cache damaged there would refuse every round that could repair
// it. The pack files last (sweepPacks), once the new refs are in, so a
// reader of the old refs never loses a pack they name.
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
// gitproc.RepoConfig and a HEAD git cannot read made the snapshot's. It
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
	return t.moved, nil
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
// round's index and its refs: every file of objects/pack the snapshot does not
// name — a stray, a pack no row names any more (a log gone back under the
// cache) — answered for sweepPacks to move once the new refs are in. When one
// is a pack's, the multi-pack-index and the commit-graph go NOW, first: an
// index naming a missing pack fails every read of the repository, and the
// round's own index is then written over exactly the snapshot's packs. It
// reports whether it dropped them.
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
	dropPack := false
	for _, d := range list {
		name := d.Name()
		if (want[name] || midxFile.MatchString(name)) && d.Type().IsRegular() {
			continue
		}
		strays = append(strays, name)
		dropPack = dropPack || strings.HasPrefix(name, "pack-")
	}
	if dropPack {
		if err = c.dropIndexes(inc); err != nil {
			return nil, false, err
		}
	}
	return strays, dropPack, nil
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
	packDir := filepath.Join(inc.dir, "objects", "pack")
	list, err := os.ReadDir(packDir)
	if err != nil {
		return err
	}
	var paths []string
	for _, d := range list {
		if midxFile.MatchString(d.Name()) {
			paths = append(paths, filepath.Join(packDir, d.Name()))
		}
	}
	paths = append(paths, filepath.Join(inc.dir, "objects", "info", "commit-graph"))
	for _, path := range paths {
		if err = toTrash(c.dir, path, newV7()+"-"+filepath.Base(path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, d := range []string{packDir, filepath.Join(inc.dir, "objects", "info")} {
		if err = syncDir(d); err != nil {
			return err
		}
	}
	return nil
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
