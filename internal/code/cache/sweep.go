package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
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
//	objects/info/   nothing fixed: an alternates file here would serve
//	                another repository's objects by id, and the pool refuses
//	                it (gitproc.ShapeAlternates)
//	objects/pack/   pack-<cs>.{pack,idx,rev} for every pack the rows name
//	declared        every file a declaration of derived.go owns, in the
//	                directory it names — the commit-graph in objects/info, the
//	                multi-pack-index's files in objects/pack — unless a change
//	                it is built over drops it (dropFor)
//
// Every FILE named here is a REGULAR file, and every directory a directory:
// a symbolic link, or anything else, at one of their names is outside the
// image (image.imaged), whatever it points at.
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
// they name — and that only in an incarnation NOBODY may hold: in one a
// reader may hold, a pack file no row names leaves by the SWAP into the next
// incarnation instead (swap.go), since git opens a pack lazily, part way
// through a run, and no pin of a file could say which a running process will
// touch.
//
// A DELTA SWEEPS NOTHING: between two rounds of one process nothing but this
// package has written the directory, and every writer here leaves only image
// entries — so the steady-state work of a sync is proportional to what moved,
// never to the directory.

// THE GRAMMAR, one predicate an entry: what the sweep keeps and Verify
// admits, written once so the two never disagree about what a cache is — the
// image's FIXED entries below, and every file a DECLARATION owns (derived.go),
// read from the declarations rather than spelled beside them, so a derived
// file is added to the image by adding its declaration.

// rootFiles are the root's fixed files: the repository's HEAD, config and
// packed-refs, and the cache's state — each a regular file (image.imaged).
var rootFiles = []string{"HEAD", "config", "packed-refs", stateName}

// requiredDirs are the image's directories below its root that every
// incarnation has: the skeleton makes them, a swap's staging too (swap.go),
// and the sweep makes them again.
// refs/heads and refs/tags stay EMPTY: there are never loose refs, which
// outrank packed-refs for every reader.
var requiredDirs = []string{"refs/heads", "refs/tags", "objects/pack", "objects/info"}

// fixedDirs are the image's directories whatever the declarations say,
// parents first, in the order Verify lists them: the root, refs/ and its two
// empty directories, objects/ and its two. Every one is required.
var fixedDirs = []string{".", "refs", "refs/heads", "refs/tags", "objects", "objects/info", "objects/pack"}

// packFileName matches a pack's three files, its checksum the first group:
// which of them the image keeps is the rows' to say.
var packFileName = regexp.MustCompile(`^pack-([0-9a-f]{40})\.(pack|idx|rev)$`)

// packRel is the directory of an incarnation its packs live in, relative to
// it; where each derived file lives is its declaration's (derived.go).
var packRel = filepath.Join("objects", "pack")

// entryKind is what a directory entry is: a regular file, a directory, or
// anything else (a symbolic link, a device, a socket).
type entryKind uint8

const (
	fileEntry entryKind = iota + 1
	dirEntry
	otherEntry
)

// kindOf is what d is, as its directory listing says: a link is never
// followed.
func kindOf(d fs.DirEntry) entryKind {
	switch {
	case d.Type().IsRegular():
		return fileEntry
	case d.IsDir():
		return dirEntry
	}
	return otherEntry
}

// image is the grammar of an incarnation's directory, built ONCE at Open from
// the cache's declarations (newImage) — DerivedFiles in production — so no
// reader spells a derived file's name and none rebuilds the grammar per
// entry. Read-only once built, so every goroutine of the cache shares it.
type image struct {
	// decls is the declarations, in DerivedFiles' order: a derivedSet's bit i
	// is decls[i].
	decls []Derived
	// dirs is every directory of the image, parents first: fixedDirs, then
	// every directory a declaration owns or holds a file in, with its
	// parents, each as the filepath of the incarnation's.
	dirs []string
	// dir is dirs as a set, and required those of them every incarnation
	// has: fixedDirs. A declared directory git has not yet written is no
	// directory missing.
	dir, required map[string]bool
	// stating, when set, is told the path relative to the incarnation of
	// every entry sizes is about to stat: the suite's count of what a
	// measure reads (knobs.stating), set before the image is shared.
	stating func(rel string)
}

// newImage is the grammar of the declarations decls, which it holds to
// validateAll first: a build whose declarations leave anything unsaid opens
// no cache.
func newImage(decls []Derived) (*image, error) {
	if err := validateAll(decls); err != nil {
		return nil, err
	}
	if len(decls) > maxDerived {
		return nil, fmt.Errorf("%d derived files are declared, and a set of them holds %d", len(decls), maxDerived)
	}
	im := &image{decls: slices.Clone(decls), dir: map[string]bool{}, required: map[string]bool{}}
	add := func(rel string) {
		if !im.dir[rel] {
			im.dir[rel] = true
			im.dirs = append(im.dirs, rel)
		}
	}
	for _, rel := range fixedDirs {
		add(filepath.FromSlash(rel))
		im.required[filepath.FromSlash(rel)] = true
	}
	for i := range im.decls {
		for _, p := range im.decls[i].patterns() {
			var parents []string
			for rel := filepath.FromSlash(p.Dir); rel != "."; rel = filepath.Dir(rel) {
				parents = append(parents, rel)
			}
			for j := len(parents) - 1; j >= 0; j-- {
				add(parents[j])
			}
		}
	}
	return im, nil
}

// patterns is every file pattern d owns, or a write of it holds.
func (d *Derived) patterns() []FilePattern {
	out := make([]FilePattern, 0, len(d.Owns))
	for _, o := range d.Owns {
		out = append(out, o.FilePattern)
	}
	return append(out, d.holds()...)
}

// holds is every file pattern a write of d holds while it runs
// (Write.Holds), each once.
func (d *Derived) holds() []FilePattern {
	var out []FilePattern
	for _, w := range d.Writes {
		for _, p := range w.Holds {
			if !slices.ContainsFunc(out, func(q FilePattern) bool {
				return q.Dir == p.Dir && q.Name.String() == p.Name.String()
			}) {
				out = append(out, p)
			}
		}
	}
	return out
}

// imaged reports whether name, an entry of the image directory rel, is part
// of the image whatever the rows say: one of the image's directories as a
// directory, or — as a REGULAR file, and as nothing else — one of the root's
// fixed files or a file a declaration owns. The rows' pack files are judged
// apart (packFileName): against the snapshot by the sweep, against the
// listing by Verify, and there too only a regular file is one.
//
// EVERY FILE OF THE IMAGE IS A REGULAR FILE (ADR-0056). git reads through a
// link at a derived file's name, at config or at packed-refs: a symbolic link
// named commit-graph had git read the graph it pointed at, outside the
// incarnation, and one named config over a file holding gitproc.RepoConfig
// passed the sweep's replace and the pool's shape check alike — both read
// through it — so the repository's configuration lived wherever the link
// pointed, outside the budget and outside what a rebuild replaces. A link at
// HEAD git does not read through: it refuses the whole repository unless the
// link's text begins refs/ (setup.c's validate_headref checks only that
// prefix, so text that climbs back out of refs/ has git read the file it
// leads to), and with a target holding a HEAD tidy's replace accepts — it
// reads through the link — every git process in the copy failed, a request's
// and the round's own alike, through every full round. Admitted as anything
// but a directory, as the root and objects/info each admitted before the
// grammar was one predicate, each was kept by the sweep and admitted by
// Verify. Outside the image, it is moved into trash/ by a full round — never
// followed: trash/ takes the link itself — whose tidy then writes config and
// HEAD afresh, writeRefs packed-refs and persist the state, and named by
// Verify.
func (im *image) imaged(rel, name string, kind entryKind) bool {
	if im.dir[filepath.Join(rel, name)] {
		return kind == dirEntry
	}
	if rel == "." && slices.Contains(rootFiles, name) {
		return kind == fileEntry
	}
	if i, o := im.ownerOf(rel, name); i >= 0 {
		// A tree a declaration owns whole (Owned.Tree) is a DIRECTORY, kept
		// without a look inside: what it holds is its writer's.
		if o.Tree {
			return kind == dirEntry
		}
		return kind == fileEntry
	}
	return false
}

// tree reports whether name, an entry of the image directory rel, is at the
// name of a tree a declaration owns whole (Owned.Tree), whatever it is on
// disk.
func (im *image) tree(rel, name string) bool {
	_, o := im.ownerOf(rel, name)
	return o != nil && o.Tree
}

// ownerOf is the declaration, by its index, and the owned pattern whose file
// name is in the image directory rel — or -1 and nil.
func (im *image) ownerOf(rel, name string) (int, *Owned) {
	for i := range im.decls {
		for j := range im.decls[i].Owns {
			o := &im.decls[i].Owns[j]
			if filepath.FromSlash(o.Dir) == rel && o.Name.MatchString(name) {
				return i, o
			}
		}
	}
	return -1, nil
}

// owner is ownerOf for a path relative to the incarnation.
func (im *image) owner(rel string) (int, *Owned) {
	dir, name := filepath.Split(rel)
	return im.ownerOf(filepath.Clean(dir), name)
}

// holderOf is the declaration, by its index, that holds name in the image
// directory rel while a write of it runs (Write.Holds) — or -1.
func (im *image) holderOf(rel, name string) int {
	for i := range im.decls {
		for _, p := range im.decls[i].holds() {
			if filepath.FromSlash(p.Dir) == rel && p.Name.MatchString(name) {
				return i
			}
		}
	}
	return -1
}

// ownedDirs is every directory, relative to the incarnation, a declaration
// of s owns a file in, in declaration order.
func (im *image) ownedDirs(s derivedSet) []string {
	var out []string
	im.each(s, func(_ int, d *Derived) {
		for _, o := range d.Owns {
			if rel := filepath.FromSlash(o.Dir); !slices.Contains(out, rel) {
				out = append(out, rel)
			}
		}
	})
	return out
}

// listed is the entries of the image directory rel of the incarnation at dir,
// and none for a declared directory git has not written; an absent required
// directory, or one that cannot be read, is an error.
func (im *image) listed(dir, rel string) ([]fs.DirEntry, error) {
	list, err := os.ReadDir(filepath.Join(dir, rel))
	if errors.Is(err, fs.ErrNotExist) && !im.required[rel] {
		return nil, nil
	}
	return list, err
}

// ownedFile is a file of an incarnation a declaration owns: its path
// relative to the incarnation, and the declaration — by its index, a
// derivedSet's bit — and owned pattern it is a file of, which say what is
// done with it: how a check and the scrub hold it to its bytes (held, by
// Derived.Check), and what a check of it is counted as (Owned.File).
type ownedFile struct {
	rel string
	i   int
	of  *Derived
	as  *Owned
}

// owned is the file at rel, a path relative to an incarnation, as the
// declaration that owns it says — false for a file no declaration owns.
func (im *image) owned(rel string) (ownedFile, bool) {
	i, o := im.owner(rel)
	if i < 0 {
		return ownedFile{}, false
	}
	return ownedFile{rel: rel, i: i, of: &im.decls[i], as: o}, true
}

// present is every derived file in the incarnation at dir — every REGULAR
// file a declaration owns, since the sweep moves anything else — with its
// declaration, sorted by its path relative to the incarnation.
func (im *image) present(dir string) ([]ownedFile, error) {
	var out []ownedFile
	for _, rel := range im.ownedDirs(im.all()) {
		list, err := im.listed(dir, rel)
		if err != nil {
			return nil, err
		}
		for _, d := range list {
			if !d.Type().IsRegular() {
				continue
			}
			if f, ok := im.owned(filepath.Join(rel, d.Name())); ok {
				out = append(out, f)
			}
		}
	}
	slices.SortFunc(out, func(a, b ownedFile) int { return strings.Compare(a.rel, b.rel) })
	return out, nil
}

// derivedMeasure is one measurement of an incarnation's derived files: the
// bytes each declaration's owned files hold, keyed by its name for every
// declaration with a file on disk — none for one with none — and the
// declarations whose main file (Owned.Main) is there. What
// Cache.recordDerived sets an incarnation's record from; a declaration with no
// main file is present where its record says it is, so no bit of mains is
// about it, and a tree's bytes are none of a measure's (sizes): its bit says
// only that it is there.
type derivedMeasure struct {
	bytes map[DerivedName]int64
	mains derivedSet
}

// derivedSizes measures every file of the incarnation at dir a declaration
// owns, whatever its kind: what the budget counts of them, declaration by
// declaration. Each owned directory is listed once, and ONLY THE FILES A
// DECLARATION OWNS GET A STAT: objects/pack holds three files a pack beside
// the index's, with no cap on the packs, and derivedSizes runs at the end of
// every maintenance run — which a full round's stopMaintenance and every
// dropFor that stops a run wait for, so the request that round answers waits
// on it too — and on the full round that writes its own index. A stat of
// every entry there would put three stats a pack on those waits: 12.9–13.3 ms
// at 1 000 packs and 145–148 ms at 10 000, against 3.0–3.3 ms and 49–57 ms for
// the listing and the owned files alone (measured on four cores over a warm
// page cache, three runs each), the listing being what is left. A caller that
// has listed a directory already — measure, objects/pack — reads it from its
// own listing (ownedIn).
func (im *image) derivedSizes(dir string) (derivedMeasure, error) {
	m := derivedMeasure{bytes: map[DerivedName]int64{}}
	for _, rel := range im.ownedDirs(im.all()) {
		sizes, err := im.sizes(dir, rel, im.owns(rel))
		if err != nil {
			return derivedMeasure{}, err
		}
		im.ownedIn(rel, sizes, &m)
	}
	return m, nil
}

// owns is whether a declaration owns name in the image directory rel: what a
// listing that counts only the derived files keeps (sizes).
func (im *image) owns(rel string) func(name string) bool {
	return func(name string) bool {
		i, _ := im.ownerOf(rel, name)
		return i >= 0
	}
}

// sizes is the size of every entry of the image directory rel of the
// incarnation at dir that keep admits — every entry for a nil keep — by name,
// from ONE listing, an entry gone since the listing omitted, and none for a
// declared directory git has not written. An entry keep refuses gets no stat:
// the listing names it, and its size would be a stat of its own.
//
// A TREE A DECLARATION OWNS WHOLE (Owned.Tree) gets no stat either: it is
// there, as a directory, at size zero, and nothing at all otherwise — its
// bytes are its writer's running total (Cache.recordDerived), and its inode's
// size is a few KiB whatever it holds (TestTheHeldBytesCountTheMemo).
func (im *image) sizes(dir, rel string, keep func(name string) bool) (map[string]int64, error) {
	list, err := im.listed(dir, rel)
	if err != nil {
		return nil, err
	}
	sizes := make(map[string]int64, len(list))
	for _, d := range list {
		if keep != nil && !keep(d.Name()) {
			continue
		}
		if im.tree(rel, d.Name()) {
			if d.IsDir() {
				sizes[d.Name()] = 0
			}
			continue
		}
		if im.stating != nil {
			im.stating(filepath.Join(rel, d.Name()))
		}
		info, err := d.Info()
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return nil, err
		}
		sizes[d.Name()] = info.Size()
	}
	return sizes, nil
}

// ownedIn adds to m what the declarations own of one listing of the image
// directory rel, whatever each entry's kind: the bytes of each declaration's
// files there, and the declarations whose main file (Owned.Main) it holds —
// read from a listing a caller already has, never from another.
func (im *image) ownedIn(rel string, sizes map[string]int64, m *derivedMeasure) {
	for name, size := range sizes {
		i, o := im.ownerOf(rel, name)
		if i < 0 {
			continue
		}
		m.bytes[im.decls[i].Name] += size
		if o.Main {
			m.mains |= 1 << i
		}
	}
}

// mainRel is the path, relative to an incarnation, of the file whose
// presence is the declaration name's (Owned.Main) — a literal, which
// Validate holds.
func (im *image) mainRel(name DerivedName) string {
	for _, d := range im.decls {
		if d.Name != name {
			continue
		}
		for _, o := range d.Owns {
			if o.Main {
				literal, _ := o.Name.LiteralPrefix()
				return filepath.Join(filepath.FromSlash(o.Dir), literal)
			}
		}
	}
	return ""
}

// derivedSet is a set of the image's declarations, bit i its decls[i]: what a
// drop was asked and did, and what was written, named in declaration order
// wherever a line names it.
type derivedSet uint32

// maxDerived is how many declarations a derivedSet holds.
const maxDerived = 32

// all is every declaration.
func (im *image) all() derivedSet { return derivedSet(1)<<len(im.decls) - 1 }

// decl is the declaration named name, or nil where the image holds none.
func (im *image) decl(name DerivedName) *Derived {
	for i := range im.decls {
		if im.decls[i].Name == name {
			return &im.decls[i]
		}
	}
	return nil
}

// set is the declarations named names.
func (im *image) set(names ...DerivedName) derivedSet {
	var s derivedSet
	for i, d := range im.decls {
		if slices.Contains(names, d.Name) {
			s |= 1 << i
		}
	}
	return s
}

// reported is the declarations a damage report naming parts names: those
// whose Part is among them. The parts to pass are gitproc.Damage.Drops — a
// chunk-format report names both chunk-format files there.
func (im *image) reported(parts gitproc.Part) derivedSet {
	var s derivedSet
	for i, d := range im.decls {
		if d.Part&parts != 0 {
			s |= 1 << i
		}
	}
	return s
}

// holding is the declarations inc holds a file of: those its record has an
// entry for, which a declaration has exactly while a file of it is on disk
// (setDerived). A REPORT REACHES ONLY THESE: a part names the files git read
// of it, and an incarnation in CHAIN mode holds no own-mode index or graph
// (layers.go), so a multi-pack-index's or a commit-graph's report there names
// the installed chain alone — which keeps its files on a report, the scrub
// deciding — and never a drop of files that are not there, which would read
// the copy whole for nothing. e.mu is held.
func (im *image) holding(inc *incarnation) derivedSet {
	var s derivedSet
	for i, d := range im.decls {
		if _, ok := inc.derived[d.Name]; ok {
			s |= 1 << i
		}
	}
	return s
}

// naming is the parts of derived — a report's derived parts — that name a
// declaration of s (gitproc.Damage.Drops, the one map from a part to the
// files it names): what a report is counted by under what became of the
// declarations of s. A chunk-format part names both files, so it is counted
// under each route its declarations take.
func (im *image) naming(derived gitproc.Part, s derivedSet) gitproc.Part {
	var out gitproc.Part
	for _, p := range []gitproc.Part{gitproc.PartMultiPackIndex, gitproc.PartCommitGraph, gitproc.PartChunk} {
		if derived&p != 0 && im.reported((&gitproc.Damage{Parts: p}).Drops())&s != 0 {
			out |= p
		}
	}
	return out
}

// dropping is the declarations whose answer to ev is Drop (Derived.On): what
// a change ev drops, and — for EventReported — the only declarations a drop
// can repair, since a report of one that keeps its files moves nothing. For
// eventFailedCheck it is every declaration but the installed chain's: a file
// that fails its check is dropped whatever its declaration says of a report,
// and a chain's is CUT at the layer that holds it (cutting).
func (im *image) dropping(ev Event) derivedSet {
	var s derivedSet
	for i, d := range im.decls {
		if d.On(ev) == Drop || (ev == eventFailedCheck && !d.chained()) {
			s |= 1 << i
		}
	}
	return s
}

// cutting is the installed chain's declarations a change ev cuts to a prefix
// of what they hold (Prefix) — for eventFailedCheck, every one of them: a
// layer's file that fails its check goes with every layer above it.
func (im *image) cutting(ev Event) derivedSet {
	var s derivedSet
	for i, d := range im.decls {
		if d.On(ev) == Prefix || (ev == eventFailedCheck && d.chained()) {
			s |= 1 << i
		}
	}
	return s
}

// parts is the parts of a repository the declarations of s are named by in a
// damage report: what a report's counts are kept by.
func (im *image) parts(s derivedSet) gitproc.Part {
	var p gitproc.Part
	im.each(s, func(_ int, d *Derived) { p |= d.Part })
	return p
}

// each calls f with every declaration of s, in declaration order.
func (im *image) each(s derivedSet, f func(i int, d *Derived)) {
	for i := range im.decls {
		if s&(1<<i) != 0 {
			f(i, &im.decls[i])
		}
	}
}

// files is what a line names each declaration of s by, in declaration order:
// its main file's name — and, for one with no main file (the provenance
// index's runs), what a check of its files is counted as (Owned.File), since
// "." would name nothing.
func (im *image) files(s derivedSet) []string {
	out := []string{}
	im.each(s, func(_ int, d *Derived) {
		if d.hasMain() {
			out = append(out, filepath.Base(im.mainRel(d.Name)))
			return
		}
		out = append(out, d.Owns[0].File)
	})
	return out
}

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
	t.outside(inc.dir, ".")
	if t.err != nil {
		return t.moved, t.err
	}
	for _, d := range requiredDirs {
		if err := c.ensureDir(inc.dir, filepath.FromSlash(d)); err != nil {
			return t.moved, err
		}
	}
	// Every other directory of the image, parents first, so a declared
	// directory that is something else than a directory went with its
	// parent's listing before its own is read. objects/pack is the second
	// half's (packStrays).
	for _, rel := range c.image.dirs {
		if rel != "." && rel != packRel {
			t.outside(inc.dir, rel)
		}
	}
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

// outside moves every entry of the image directory rel of the incarnation at
// dir that the grammar does not admit (image.imaged) into trash/. A declared
// directory git has not written has nothing to move.
func (t *trashing) outside(dir, rel string) {
	if t.err != nil {
		return
	}
	list, err := t.c.image.listed(dir, rel)
	if err != nil {
		t.err = fmt.Errorf("listing %s: %w", filepath.Join(dir, rel), err)
		return
	}
	for _, d := range list {
		if t.c.image.imaged(rel, d.Name(), kindOf(d)) {
			continue
		}
		t.move(filepath.Join(dir, rel, d.Name()))
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
// does not name — a stray, a pack no row names any more — and what took each
// out: a COMPACTION'S RETIREMENT (EventPackRetired) where the snapshot's
// retired phase names its checksum and the log under the incarnation is the
// history it was read from (history false), and otherwise a pack REMOVED
// (EventPackRemoved): a log gone back or recreated, or a full answer that
// served no retirements, which leaves no stray accounted for. A stray's
// leaving is what a declaration built over the packs answers (On): an index
// is dropped by either, the commit-graph only by a removal — a merge drops no
// commit (X04).
//
// HOW THEY LEAVE turns on who may hold the incarnation. One NOBODY HOLDS and
// that is not converged — a boot's first round, a rebuild's product before it
// is promoted: no Synced of it was ever handed out, since nothing is answered
// at once before an incarnation's first round, and nothing can pin it while
// this round holds its write lock — has its strays answered for sweepPacks to
// move once the new refs are in, and every derived file built over the packs
// is dropped NOW, first, by the strongest event among them (dropFor): an
// index naming a missing pack fails every read of the repository, and the
// round's own index is then written over exactly the snapshot's packs. Any
// OTHER — converged, so atOnce may hand it to a reader while this round runs,
// or pinned — keeps every file a reader may open, and its strays are answered
// as leaving, by checksum: the round's end swaps the incarnation for the next
// (swap.go), which holds the live packs alone, and nothing of them is dropped
// or moved here.
//
// Every other entry the image does not admit goes now too, at this one
// listing: it is no pack's file, so no reader of the old refs reads it, and
// what such entries are — a lock or a temporary file a maintenance run
// killed with its node left, since no run is in flight beside a full round —
// is what would refuse the round's index below. Kept for sweepPacks, the lock
// was refused the index, whose failure removed it from under the sweep that
// had listed it, and the round failed on the move. It reports whether it
// moved anything, or will once the refs are in.
func (c *Cache) packStrays(e *entry, inc *incarnation, snap snapshot, history bool) ([]string, map[string]Event,
	bool, error) {

	packDir := filepath.Join(inc.dir, packRel)
	want := map[string]bool{}
	for _, p := range snap.packs {
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			want["pack-"+p.GitChecksum+ext] = true
		}
	}
	// THE CHAIN HOLDS ITS PACKS (layers.go): a pack an installed layer names
	// is no stray, whatever its rows say, until the layer goes.
	e.mu.Lock()
	for cs := range inc.installedPacks {
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			want["pack-"+cs+ext] = true
		}
	}
	e.mu.Unlock()
	list, err := os.ReadDir(packDir)
	if err != nil {
		return nil, nil, false, err
	}
	var strays []string
	now := &trashing{c: c}
	for _, d := range list {
		name := d.Name()
		switch {
		case (want[name] && d.Type().IsRegular()) || c.image.imaged(packRel, name, kindOf(d)):
		case strings.HasPrefix(name, "pack-"):
			strays = append(strays, name)
		default:
			if now.move(filepath.Join(packDir, name)); now.err != nil {
				return nil, nil, now.moved, now.err
			}
		}
	}
	if now.moved {
		if err = syncDir(packDir); err != nil {
			return nil, nil, true, err
		}
	}
	if len(strays) == 0 {
		return nil, nil, now.moved, nil
	}
	// What took a stray out is what this answer's retirements say, or what
	// the incarnation already knew of it: a pack an earlier round's
	// retirements took out, whose swap failed, may lie below the floor this
	// answer serves retirements above.
	var retired map[string]bool
	if !history && !snap.retiredFrom.IsZero() {
		retired = snap.retiredChecksums()
	}
	e.mu.Lock()
	held := inc.converged || inc.pins > 0
	known := maps.Clone(inc.leaving)
	e.mu.Unlock()
	leaving := map[string]Event{}
	for _, name := range strays {
		key, ev := name, EventPackRemoved
		if m := packFileName.FindStringSubmatch(name); m != nil {
			key = m[1]
			if !history && (retired[key] || known[key] == EventPackRetired) {
				ev = EventPackRetired
			}
		}
		if had, ok := leaving[key]; !ok || had != EventPackRemoved {
			leaving[key] = ev
		}
	}
	if held {
		return nil, leaving, now.moved, nil
	}
	ev := EventPackRetired
	gone := map[string]bool{}
	for key, v := range leaving {
		if v == EventPackRemoved {
			ev = EventPackRemoved
		}
		gone[key] = true
	}
	if _, err = c.dropFor(e, inc, ev, eventDetail{packs: gone}); err != nil {
		return nil, nil, true, err
	}
	return strays, nil, true, nil
}

// sweepPacks is the sweep's second half, once the refs are in: every stray
// packStrays answered to move — none where the strays leave by the swap — is
// moved into trash/, so a reader of the old refs never loses a pack they name
// — and then the incarnation's counts are MEASURED, whatever they had drifted
// to: strays the swap is about to take out included.
func (c *Cache) sweepPacks(e *entry, inc *incarnation, strays []string) (bool, error) {
	packDir := filepath.Join(inc.dir, packRel)
	t := &trashing{c: c}
	for _, name := range strays {
		if t.move(filepath.Join(packDir, name)); t.err != nil {
			return true, t.err
		}
		e.mu.Lock()
		unvouch(inc, filepath.Join(packRel, name))
		e.mu.Unlock()
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

// dropFor is THE ONE REMOVAL OF A DERIVED FILE: it moves into trash/ every
// file of inc that a declaration whose On(ev) is Drop owns — for
// EventReported and eventFailedCheck, of the declarations named alone, since
// a report and a check each name which files they found damaged, and a file
// failing its check whatever its declaration says (image.dropping) — syncs
// each directory it moved a file out of once,
// and answers the declarations it dropped, whether or not a file of theirs
// was there. Each file moved takes its vouch with it (check.go): a file
// written there next is vouched as it is written, or checked. And each
// declaration a file of which it moved is stamped written (wrote, the guard of
// a report's drop) and the copy repaired (repaired: git looks an object up
// through the packs' own indexes once a multi-pack-index that placed it
// wrongly is gone); a drop that found nothing to move stamps neither.
//
// With no declaration dropping on ev it does NOTHING — no listing, no sync —
// so a call at every change costs a loop over the declarations.
//
// THE ORDER RULE: a drop precedes the change it answers becoming visible to
// any git process — a pack removed, a pack placed, packed-refs renamed in — so
// every caller calls it BEFORE its change, whatever the declarations say of
// that change today, and a declaration changing its answer needs no new call.
//
// A maintenance run writing what it drops or cuts is STOPPED first, and
// waited for (stopWriting, below). It moves what a declaration OWNS and never what a
// write of it holds while it runs (Write.Holds): that is a running writer's
// own, whose rename it would break, and a writer that does not succeed —
// one it stopped among them — removes it as it ends (clearWriting). Its
// caller holds the repository's write lock — whole for a round, a share for
// an adoption — and not entry.mu.
//
// THE INSTALLED CHAIN IS CUT, NEVER DROPPED (Prefix): every declaration of
// it the change cuts (image.cutting) keeps the longest prefix of the chain
// that d's packs, its failing layers and — for EventLayersMoved — the chain
// it wants leave valid (prefixOf, the one cut both chains share), and the
// rest goes (cutChain), the node's tip with it. A change that keeps the whole
// chain moves nothing of it but the tip, which every change but a retirement
// takes (tipGoes). d names what the change takes out: for EventReported and
// eventFailedCheck the declarations named (d.named), for a pack's leaving its
// checksums (d.packs), for the chain moving the chain wanted (d.want), and
// for a failed check the layers whose files failed (d.failing).
func (c *Cache) dropFor(e *entry, inc *incarnation, ev Event, d eventDetail) (derivedSet, error) {
	drop := c.image.dropping(ev)
	if ev == EventReported || ev == eventFailedCheck {
		drop &= d.named
	}
	if cut := c.image.cutting(ev); cut != 0 && (ev != eventFailedCheck || cut&d.named != 0) {
		// A RUN WRITING THE NODE'S TIP is stopped first, and waited for, as a
		// run writing what a drop takes is (below): it writes over the chain
		// files a cut writes again, and over packs the change takes out —
		// recorded after the change, its tip would name what is gone.
		c.stopWriting(e, inc, cut)
		e.mu.Lock()
		installed, tip := slices.Clone(inc.installed), inc.tip.id
		e.mu.Unlock()
		// THE CHAIN MOVING makes the chain's directories exactly the prefix it
		// keeps, whatever the cut: a placement in place is where a previous
		// process's files no record names, or a crashed placement's, go. And
		// a change that keeps every layer still takes the node's tip where it
		// takes the tip's base or commits the tip names (tipGoes): a pack
		// removed from the tail.
		k := prefixOf(installed, ev, d)
		if k < len(installed) || ev == EventLayersMoved || (tip != "" && tipGoes(installed, k, ev)) {
			if err := c.cutChain(e, inc, k, ev); err != nil {
				return 0, err
			}
		}
	}
	if drop == 0 {
		return 0, nil
	}
	// A RUN IN FLIGHT WRITING WHAT THIS DROPS IS STOPPED FIRST, and waited
	// for: git reads a write's inputs as it starts, so a file it renamed in
	// after this drop would be built over the inputs before the change, and
	// the run would record it current — covering, graphed, vouched — over
	// inputs it does not describe. A step it finished before it stopped is
	// on disk, and the listing below drops it. None starts until the change
	// is visible, since no run starts while a round runs or an adoption
	// holds the repository (runnable); a full round has stopped every run
	// already (apply).
	c.stopWriting(e, inc, drop)
	for _, rel := range c.image.ownedDirs(drop) {
		list, err := c.image.listed(inc.dir, rel)
		if err != nil {
			return 0, err
		}
		moved := false
		for _, d := range list {
			i, o := c.image.ownerOf(rel, d.Name())
			if i < 0 || drop&(1<<i) == 0 || o.Tree {
				// A tree goes whole, below, under its writer's lock.
				continue
			}
			var size int64
			if info, ierr := d.Info(); ierr == nil {
				size = info.Size()
			}
			path := filepath.Join(inc.dir, rel, d.Name())
			err = toTrash(c.dir, path, newV7()+"-"+d.Name())
			switch {
			case errors.Is(err, fs.ErrNotExist):
				continue
			case err != nil:
				return 0, err
			}
			moved = true
			e.mu.Lock()
			unvouch(inc, filepath.Join(rel, d.Name()))
			c.droppedDerived(inc, 1<<i)
			name := c.image.decls[i].Name
			s := inc.derived[name]
			if !c.image.decls[i].hasMain() {
				// A declaration with no main file is there by its record
				// (recordDerived), and a record naming a file gone is no
				// record: the index of pushes goes with its first run.
				s = derivedState{Bytes: s.Bytes}
			}
			s.Bytes = max(s.Bytes-size, 0)
			c.setDerived(inc, name, s, true)
			e.mu.Unlock()
		}
		if moved {
			if err = syncDir(filepath.Join(inc.dir, rel)); err != nil {
				return 0, err
			}
		}
	}
	// A TREE GOES WHOLE (Owned.Tree), its record and its index with it, under
	// its writer's lock (memo.go's dropMemo): an answer placed into the tree
	// while it moved would otherwise be counted for a directory no longer
	// there, and its record reset after it, a directory counted as none. The
	// memo is the one tree there is.
	tree := false
	c.image.each(drop, func(_ int, d *Derived) { tree = tree || d.tree() })
	if tree {
		if err := c.dropMemo(e, inc); err != nil {
			return 0, err
		}
	}
	// What the incarnation records it holds follows what it holds, whatever
	// kind of round dropped it: a delta measures nothing after, and a
	// derived file recorded as present that is gone is one the maintainer
	// never writes again — so each dropped declaration's entry goes, its
	// bytes with it: an index gone covers nothing and has no bitmap, a graph
	// gone is due again.
	e.mu.Lock()
	c.image.each(drop, func(_ int, d *Derived) {
		if !d.tree() {
			c.setDerived(inc, d.Name, derivedState{}, false)
		}
	})
	// The runs of an index of pushes dropped stay mapped only while their
	// readers hold them (provenance.go), and the backfill has nothing of the
	// index to do (schedule.go).
	c.forgetRuns(inc)
	if drop&c.image.set(DerivedProvenance) != 0 {
		c.backfillDirty(e)
	}
	e.mu.Unlock()
	return drop, nil
}

// eventDetail is what a change takes out, as dropFor reads it: the
// declarations a report or a check named (named, for EventReported and
// eventFailedCheck); and, for the installed chain's cut (prefixOf), the pack
// checksums the change removes or retires (packs), the chain the rows want
// (want, for EventLayersMoved) and the layers whose files failed their check,
// by id (failing).
type eventDetail struct {
	named   derivedSet
	packs   map[string]bool
	want    []code.LayerRow
	failing map[string]bool
}

// droppedDerived stamps that this process has just dropped a file of each
// declaration of s from inc: a write of it (wrote, D4's guard) and one that
// can end a pack's or an object's failure (repaired). e.mu is held.
func (c *Cache) droppedDerived(inc *incarnation, s derivedSet) {
	c.wrote(inc, s)
	read := false
	c.image.each(s, func(_ int, d *Derived) { read = read || d.gitReads() })
	if read {
		// Only a file git reads can be what a failed process met: the
		// cache's own (the index of pushes) repairs no read.
		repaired(inc)
	}
}

// measure sets inc's counts from its directory: the packs installed (an .idx
// with its .pack), the bytes of their files and packed-refs, and each derived
// file's (recordDerived). A derived file whose main file is no longer on disk
// is no longer recorded as made — an index gone covers nothing, a graph gone
// is due again — and a declaration with no file on disk has no entry.
//
// EACH DIRECTORY IS LISTED ONCE. objects/pack holds three files a pack until
// compaction, with no cap on the packs, and measure runs at every full round
// — a boot's first among them, which the first request to the repository
// waits on — so the derived files' bytes and presence there are read from the
// one listing that counts the packs (ownedIn), and only the other owned
// directories (objects/info) are listed beside it.
func (c *Cache) measure(e *entry, inc *incarnation) error {
	// Every entry of objects/pack: the packs' files are what it counts.
	sizes, err := c.image.sizes(inc.dir, packRel, nil)
	if err != nil {
		return err
	}
	var packs int
	var disk int64
	for name, size := range sizes {
		if i, _ := c.image.ownerOf(packRel, name); i >= 0 {
			continue // a derived file's: ownedIn counts it
		}
		if c.image.dir[filepath.Join(packRel, name)] {
			// A directory of the image there — the installed chain's — is
			// listed and counted below as the declared directory it is.
			continue
		}
		if strings.HasSuffix(name, ".idx") {
			if _, ok := sizes[strings.TrimSuffix(name, ".idx")+".pack"]; ok {
				packs++
			}
		}
		disk += size
	}
	m := derivedMeasure{bytes: map[DerivedName]int64{}}
	c.image.ownedIn(packRel, sizes, &m)
	for _, rel := range c.image.ownedDirs(c.image.all()) {
		if rel == packRel {
			continue
		}
		other, err := c.image.sizes(inc.dir, rel, c.image.owns(rel))
		if err != nil {
			return err
		}
		c.image.ownedIn(rel, other, &m)
	}
	switch info, lerr := os.Lstat(filepath.Join(inc.dir, "packed-refs")); {
	case lerr == nil:
		disk += info.Size()
	case !errors.Is(lerr, fs.ErrNotExist):
		return lerr
	}
	e.mu.Lock()
	inc.packs = packs
	c.setSize(inc, disk)
	c.recordDerived(inc, m)
	e.mu.Unlock()
	return nil
}
