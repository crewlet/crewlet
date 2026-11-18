package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
)

// THE SWEEP, on a FULL answer only (package doc, "Convergence, priced for a
// push on every node"): the incarnation made exactly its IMAGE, anything
// outside it renamed into trash/ for the janitor. The image (image.imaged):
// the root's HEAD, config, packed-refs and state; refs/heads and refs/tags,
// EMPTY, since a loose ref outranks packed-refs; objects/info and
// objects/pack, with no loose object and no alternates file
// (gitproc.ShapeAlternates); the packs the rows place in objects/pack or the
// dropping view (packStrays); and every file a declaration owns (derived.go).
// Every file is a REGULAR file and every directory a directory. TWO HALVES
// around the install: tidy first, since the round's git processes run before
// the packs' half could; pack files last (sweepPacks), once the new refs are
// in, so a reader of the old refs never loses a pack (or by the swap).

// THE GRAMMAR, one predicate an entry, so the sweep and Verify never disagree
// about what a cache is: the FIXED entries below, and every file a
// DECLARATION owns, read from the declarations (derived.go), never spelled
// beside them.

// rootFiles are the root's fixed files, each a regular file (image.imaged).
var rootFiles = []string{"HEAD", "config", "packed-refs", stateName}

// requiredDirs are the directories below the root every incarnation has: the
// skeleton, a swap's staging (swap.go) and the sweep make them. refs/heads
// and refs/tags stay EMPTY: a loose ref outranks packed-refs for every reader.
var requiredDirs = []string{"refs/heads", "refs/tags", "objects/pack", "objects/info"}

// fixedDirs are the image's directories whatever the declarations say,
// parents first, in the order Verify lists them. Every one is required.
var fixedDirs = []string{".", "refs", "refs/heads", "refs/tags", "objects", "objects/info", "objects/pack"}

// packFileName matches a pack's three files, its checksum the first group:
// which of them the image keeps is the rows' to say.
var packFileName = lazyPattern(`^pack-([0-9a-f]{40})\.(pack|idx|rev)$`)

// packRel is where an incarnation's live packs live, relative to it.
var packRel = filepath.Join("objects", "pack")

// droppingRel is where an incarnation's DROPPING packs live, relative to it
// (packPlace, M6-D3): an object directory BESIDE objects/
// (gitproc.DroppingDir) that only a process serving the copy names. Not under
// objects/, where a push's scratch's alternate would reach it; and no
// alternates file names it, which git would follow from every repository
// naming the copy as ITS alternate.
var droppingRel = filepath.Join(gitproc.DroppingDir, "pack")

// packDirs is the directories pack files live in (packPlace.rel):
// objects/pack, then the dropping view's.
var packDirs = []string{packRel, droppingRel}

// droppingDirs is the dropping view's directories, parents first: in the
// image when present, made by whatever places a pack there, and required of
// no incarnation.
var droppingDirs = []string{gitproc.DroppingDir, droppingRel}

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
// the declarations (newImage), so no reader spells a derived file's name or
// rebuilds the grammar per entry. Read-only once built: every goroutine
// shares it.
type image struct {
	// decls is the declarations in DerivedFiles' order: a derivedSet's bit i
	// is decls[i].
	decls []Derived
	// dirs is every image directory, parents first: fixedDirs, droppingDirs,
	// then each a declaration owns or holds a file in, with its parents.
	dirs []string
	// dir is dirs as a set; required is fixedDirs. A declared directory git
	// has not yet written, or a dropping view holding no pack, is not missing.
	dir, required map[string]bool
	// stating, when set, is told every path sizes is about to stat (the
	// suite's count, knobs.stating); set before the image is shared.
	stating func(rel string)
}

// newImage is the grammar of decls, held to validateAll first: a build whose
// declarations leave anything unsaid opens no cache.
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
	for _, rel := range droppingDirs {
		add(rel)
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
// of the image whatever the rows say: an image directory as a directory, or a
// root fixed file or declared file as a REGULAR file only. Pack files are
// judged apart (packFileName): against the snapshot by the sweep, against the
// listing by Verify, and there too only a regular file is one.
//
// A link is outside the image whatever it points at (ADR-0086; package doc,
// "Derived files"): git reads through one at config, packed-refs or a derived
// file's name, and fails every process over one at HEAD. A full round moves
// it into trash/ without following it, then writes the fixed files afresh;
// Verify names it.
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

// ownedFile is a file of an incarnation a declaration owns: its path relative
// to the incarnation, the declaration (by index, a derivedSet's bit) and the
// owned pattern, which say how a check and the scrub hold it to its bytes
// (Derived.Check) and what a check of it counts as (Owned.File).
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

// derivedMeasure is one measurement of an incarnation's derived files, which
// Cache.recordDerived records: each declaration's owned bytes by name (none
// for one with no file on disk), and the declarations whose main file
// (Owned.Main) is there. A declaration with no main file is present where its
// record says, so no bit of mains is about it; a tree's bit says only that it
// is there, its bytes none of a measure's (sizes).
type derivedMeasure struct {
	bytes map[DerivedName]int64
	mains derivedSet
}

// derivedSizes measures every file a declaration owns in the incarnation at
// dir, whatever its kind: what the budget counts of them. Each owned
// directory is listed once and ONLY OWNED FILES GET A STAT: objects/pack holds
// three files a pack, uncapped, and this runs at the end of every maintenance
// run, which a full round (and the request it answers) and dropFor wait on. A
// stat of every entry costs ~145 ms at 10 000 packs against ~50 ms for the
// listing and owned files alone. A caller that has listed a directory already
// reads it from that listing (ownedIn).
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

// sizes is the size, by name, of every entry of the image directory rel of
// the incarnation at dir that keep admits (all for a nil keep), from ONE
// listing: an entry gone since is omitted, and a declared directory git has
// not written has none. An entry keep refuses gets no stat.
//
// A TREE A DECLARATION OWNS WHOLE (Owned.Tree) gets none either: it is there
// at size zero as a directory, its bytes its writer's running total
// (Cache.recordDerived), since its inode's size is a few KiB whatever it holds
// (TestTheHeldBytesCountTheMemo).
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

// ownedIn adds to m what the declarations own in sizes, a listing of the
// image directory rel a caller already has, whatever each entry's kind: each
// declaration's bytes, and those whose main file (Owned.Main) it holds.
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

// derivedSet is a set of the image's declarations, bit i its decls[i], named
// in declaration order wherever a line names it.
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
// entry for, exactly while a file is on disk (setDerived). A REPORT REACHES ONLY THESE: in CHAIN mode an
// incarnation holds no own-mode index or graph (layers.go), so such a report
// names the installed chain alone (which keeps its files, the scrub
// deciding), never a drop of absent files, which would read the copy whole
// for nothing. e.mu is held.
func (im *image) holding(inc *incarnation) derivedSet {
	var s derivedSet
	for i, d := range im.decls {
		if _, ok := inc.derived[d.Name]; ok {
			s |= 1 << i
		}
	}
	return s
}

// naming is the parts of derived, a report's derived parts, that name a
// declaration of s (gitproc.Damage.Drops): what a report is counted by under
// what became of s. A chunk-format part names both files, so it counts under
// each route its declarations take.
func (im *image) naming(derived gitproc.Part, s derivedSet) gitproc.Part {
	var out gitproc.Part
	for _, p := range []gitproc.Part{gitproc.PartMultiPackIndex, gitproc.PartCommitGraph, gitproc.PartChunk} {
		if derived&p != 0 && im.reported((&gitproc.Damage{Parts: p}).Drops())&s != 0 {
			out |= p
		}
	}
	return out
}

// dropping is the declarations whose answer to ev is Drop (Derived.On): for
// EventReported, the only ones a drop can repair. For eventFailedCheck it is
// every declaration but the installed chain's, whatever it says of a report;
// a chain's failing file is CUT at its layer instead (cutting).
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

// tidy is the sweep's first half: every image directory made, every entry
// outside the image but the pack files moved into trash/, config made
// gitproc.RepoConfig and an unreadable HEAD the snapshot's. It runs before
// the install because the round's git processes do, which the pool refuses
// in a directory of the wrong shape: it is what REPAIRS a copy refused for
// its shape, in place (markWhole). It reports whether it moved anything.
func (c *Cache) tidy(inc *incarnation, snap snapshot) (bool, error) {
	// The root first: a directory made through a link named objects or refs
	// would be made wherever it points.
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
	// Parents first, so a non-directory at a declared directory's name went
	// with its parent's listing. The pack directories are packStrays'.
	for _, rel := range c.image.dirs {
		if rel != "." && !slices.Contains(packDirs, rel) {
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
	// HELD TO THE POOL'S OWN PREDICATE, not a copy of it, so a full round
	// never answers a directory the pool refuses. A refusal here is a shape
	// the sweep could not put right (a filesystem that does not keep what it
	// is given), which Cache.run answers with a rebuild.
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

// packStrays decides the sweep's second half, before the round's index and
// refs: each STRAY, a pack file the snapshot does not place in its directory
// (snapshot.places), and what took it out: EventPackRetired where the
// snapshot's retirements name it (history false) or its rows place it in the
// other directory; EventPackExpunged for a dropping-view pack no row names;
// else EventPackRemoved. In an incarnation nobody holds (unconverged and
// unpinned) it drops what each event drops NOW and answers the strays as
// paths for sweepPacks; in any other they leave by the swap (package doc, "A
// pack leaves a copy a reader may hold by a swap"). Any other entry outside
// the image goes now: a dead maintenance run's lock kept for sweepPacks would
// refuse the round's index. It reports whether it moved anything, or will
// once the refs are in.
func (c *Cache) packStrays(e *entry, inc *incarnation, snap snapshot, history bool) ([]string,
	map[leaveKey]Event, bool, error) {

	places := snap.places()
	want := map[string]map[string]bool{packRel: {}, droppingRel: {}}
	viewWanted := false
	for cs, place := range places {
		viewWanted = viewWanted || place == placeDropping
		for _, ext := range bundleOrder {
			want[place.rel()]["pack-"+cs+ext] = true
		}
	}
	// THE CHAIN HOLDS ITS PACKS (layers.go): a pack an installed layer names
	// is no stray, whatever its rows say, until the layer goes.
	e.mu.Lock()
	for cs := range inc.installedPacks {
		for _, ext := range bundleOrder {
			want[packRel]["pack-"+cs+ext] = true
		}
	}
	e.mu.Unlock()
	type stray struct {
		place packPlace
		name  string
	}
	var strays []stray
	now := &trashing{c: c}
	// tidy has held the view's directory to the image, so it is a directory
	// or absent.
	_, err := os.Lstat(filepath.Join(inc.dir, gitproc.DroppingDir))
	viewHeld := err == nil
	for _, place := range []packPlace{placeLive, placeDropping} {
		rel := place.rel()
		list, err := c.image.listed(inc.dir, rel)
		if err != nil {
			return nil, nil, now.moved, err
		}
		dir := filepath.Join(inc.dir, rel)
		moved := false
		for _, d := range list {
			name := d.Name()
			switch {
			case (want[rel][name] && d.Type().IsRegular()) || c.image.imaged(rel, name, kindOf(d)):
			case strings.HasPrefix(name, "pack-"):
				strays = append(strays, stray{place, name})
			default:
				if now.move(filepath.Join(dir, name)); now.err != nil {
					return nil, nil, now.moved, now.err
				}
				moved = true
			}
		}
		if moved {
			if err = syncDir(dir); err != nil {
				return nil, nil, true, err
			}
		}
	}
	e.mu.Lock()
	held := inc.converged || inc.pins > 0
	known := maps.Clone(inc.leaving)
	purged := crossedACommit(inc.dropEpoch, snap.repo.DropEpoch, viewWanted)
	e.mu.Unlock()
	emptied := viewHeld && !viewWanted && !held
	if len(strays) == 0 {
		if emptied {
			// A view the rows place nothing in goes, so no scratch over the
			// copy is paranoid for nothing (gitproc.Cmd.Dropping).
			return []string{gitproc.DroppingDir}, nil, true, nil
		}
		return nil, nil, now.moved, nil
	}
	// This answer's retirements, or what the incarnation already knew: a pack
	// an earlier round retired, whose swap failed, may lie below the floor
	// this answer serves retirements above.
	var retired map[string]bool
	if !history && !snap.retiredFrom.IsZero() {
		retired = snap.retiredChecksums()
	}
	leaving := map[leaveKey]Event{}
	for _, s := range strays {
		key, ev := leaveKey(s), EventPackRemoved
		if m := packFileName().FindStringSubmatch(s.name); m != nil {
			key.name = m[1]
			switch {
			case s.place == placeDropping && places[key.name] == placeNone:
				// A DROPPING PACK NO ROW NAMES left as an expunge however it
				// came about, since nothing else takes a pack out of the view:
				// what it held may be gone from every copy.
				ev = EventPackExpunged
			case places[key.name] != placeNone:
				ev = EventPackRetired
			case !history && (retired[key.name] || known[key] == EventPackRetired):
				// A RETIREMENT, unless the round crossed an expunge's commit
				// whose dropping pack this copy never placed (crossedACommit):
				// then this pack's objects are not all here, and it leaves as
				// the commit's.
				ev = EventPackRetired
				if purged {
					ev = EventPackExpunged
				}
			}
		}
		if had, ok := leaving[key]; !ok || had != EventPackRemoved {
			leaving[key] = ev
		}
	}
	if held {
		return nil, leaving, now.moved, nil
	}
	// IN PLACE, EACH EVENT ITS OWN DROP over the packs it took out (dropFor),
	// before the move: an index naming a missing pack fails every read. The
	// chain is cut below the lowest layer naming any of them (prefixOf).
	byEvent := map[Event]map[string]bool{}
	for key, v := range leaving {
		if byEvent[v] == nil {
			byEvent[v] = map[string]bool{}
		}
		byEvent[v][key.name] = true
	}
	for _, ev := range Events {
		if gone, ok := byEvent[ev]; ok {
			if _, err := c.dropFor(e, inc, ev, eventDetail{packs: gone}); err != nil {
				return nil, nil, true, err
			}
		}
	}
	var rels []string
	for _, s := range strays {
		if !emptied || s.place != placeDropping {
			rels = append(rels, filepath.Join(s.place.rel(), s.name))
		}
	}
	if emptied {
		// The view whole, its strays in it: one rename, and no directory left
		// to make a scratch over the copy paranoid.
		rels = append(rels, gitproc.DroppingDir)
	}
	return rels, nil, true, nil
}

// sweepPacks is the sweep's second half, once the refs are in: every stray
// packStrays answered (a pack's file, or the dropping view whole) is moved
// into trash/, so a reader of the old refs never loses a pack, and then the
// incarnation's counts are MEASURED, strays the swap will take included.
func (c *Cache) sweepPacks(e *entry, inc *incarnation, strays []string) (bool, error) {
	t := &trashing{c: c}
	for _, rel := range strays {
		gone := []string{rel}
		if rel == gitproc.DroppingDir {
			// The view whole: the vouches of the files it takes with it go too.
			list, err := os.ReadDir(filepath.Join(inc.dir, droppingRel))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return t.moved, err
			}
			gone = gone[:0]
			for _, d := range list {
				gone = append(gone, filepath.Join(droppingRel, d.Name()))
			}
		}
		if t.move(filepath.Join(inc.dir, rel)); t.err != nil {
			return true, t.err
		}
		e.mu.Lock()
		unvouch(inc, gone...)
		e.mu.Unlock()
	}
	for d := range t.touched {
		if err := syncDir(d); err != nil {
			return true, err
		}
	}
	if err := c.measure(e, inc); err != nil {
		return t.moved, err
	}
	return t.moved, nil
}

// dropFor is THE ONE REMOVAL OF A DERIVED FILE in place (package doc,
// "Derived files"): it moves into trash/ every file of inc that a declaration
// dropping on ev owns (image.dropping; for EventReported and eventFailedCheck
// only d.named's), each taking its vouch (check.go) and stamping its
// declaration written and the copy repaired (droppedDerived), and answers the
// declarations dropped. The installed chain is CUT, never dropped (cutting,
// prefixOf, cutChain, tipGoes). THE ORDER RULE: every caller calls it BEFORE
// its change is visible to any git process, whatever the declarations say of
// it today. A run writing what it drops or cuts is stopped first; what a
// running write holds (Write.Holds) is never moved. The caller holds the
// repository's write lock (whole for a round, a share for an adoption), not
// entry.mu. With nothing dropping on ev it does nothing.
func (c *Cache) dropFor(e *entry, inc *incarnation, ev Event, d eventDetail) (derivedSet, error) {
	drop := c.image.dropping(ev)
	if ev == EventReported || ev == eventFailedCheck {
		drop &= d.named
	}
	if cut := c.image.cutting(ev); cut != 0 && (ev != eventFailedCheck || cut&d.named != 0) {
		// A RUN WRITING THE NODE'S TIP is stopped first, and waited for: it
		// writes over chain files a cut rewrites and over packs the change
		// takes out, so its tip would name what is gone.
		c.stopWriting(e, inc, cut)
		e.mu.Lock()
		installed, tip := slices.Clone(inc.installed), inc.tip.id
		e.mu.Unlock()
		// THE CHAIN MOVING makes the chain's directories exactly the kept
		// prefix, so files no record names go. A change keeping every layer
		// still takes the tip where it takes the tip's base or commits
		// (tipGoes).
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
	// for: git reads a write's inputs as it starts, so a file renamed in
	// after the drop would be recorded current over inputs it does not
	// describe. A step finished before it stopped is dropped by the listing
	// below. None starts while a round runs or an adoption holds the
	// repository (runnable).
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
			fileRel := filepath.Join(rel, d.Name())
			if c.knobs != nil && c.knobs.dropping != nil {
				c.knobs.dropping(fileRel)
			}
			e.mu.Lock()
			unvouch(inc, fileRel)
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
			if c.image.set(DerivedProvenance)&(1<<i) != 0 {
				// Marked in this hold, which took the index's record and
				// the run's vouch, and never once the drop is done: a look
				// between finds the copy marked (schedule.go).
				c.backfillDirty(e)
			}
			e.mu.Unlock()
			if c.knobs != nil && c.knobs.dropped != nil {
				c.knobs.dropped(fileRel)
			}
		}
		if moved {
			if err = syncDir(filepath.Join(inc.dir, rel)); err != nil {
				return 0, err
			}
		}
	}
	// A TREE GOES WHOLE (Owned.Tree), with its record and index, under its
	// writer's lock (memo.go's dropMemo), so an answer placed into it while it
	// moved is not counted for a directory gone. The memo is the one tree.
	tree := false
	c.image.each(drop, func(_ int, d *Derived) { tree = tree || d.tree() })
	if tree {
		if err := c.dropMemo(e, inc); err != nil {
			return 0, err
		}
	}
	// The record follows the disk whatever kind of round dropped: a delta
	// measures nothing after, and a file recorded present but gone is one the
	// maintainer never writes again.
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
	want    []gitlet.LayerRow
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

// measure sets inc's counts from its directory: the packs installed in
// objects/pack (an .idx with its .pack), the bytes of their files, of the
// dropping view's and of packed-refs, and each derived file's (recordDerived:
// one whose main file is gone is no longer made, and a declaration with no
// file on disk has no entry). EACH DIRECTORY IS LISTED ONCE: objects/pack is
// uncapped and measure runs at every full round, which a boot's first request
// waits on, so its derived files are read from the listing that counts the
// packs (ownedIn).
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
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		other, err := c.image.sizes(inc.dir, rel, c.image.owns(rel))
		if err != nil {
			return err
		}
		c.image.ownedIn(rel, other, &m)
	}
	// The dropping view's packs: on the disk the budget counts, and in the
	// count of none of the packs an index covers, which never covers them.
	view, err := c.image.sizes(inc.dir, droppingRel, nil)
	if err != nil {
		return err
	}
	var dropping int64
	for _, size := range view {
		dropping += size
	}
	disk += dropping
	switch info, lerr := os.Lstat(filepath.Join(inc.dir, "packed-refs")); {
	case lerr == nil:
		disk += info.Size()
	case !errors.Is(lerr, fs.ErrNotExist):
		return lerr
	}
	e.mu.Lock()
	inc.packs = packs
	c.setSize(inc, disk)
	c.setDropping(inc, dropping)
	c.recordDerived(inc, m)
	e.mu.Unlock()
	return nil
}
