package cache

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // git's hashfile trailer IS a SHA-1: the check compares what git wrote, it trusts no hash of its own to it
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The check: nothing an index file says is served from a file this process
// did not write or check (package doc, "What this process did not write is
// checked before it serves, and every byte is scrubbed behind it"). A
// checkable file (the commit-graph, the multi-pack-index with its bitmap and
// reverse index, every pack's .idx and .rev) is one git reads unchecked and
// can answer wrong from. Each is a git hashfile: its last twenty bytes are the
// SHA-1 of every byte before them; a .idx and a .rev carry their pack's
// checksum in the twenty before, and a .pack's trailer is its name. The rule's
// one implementation, gitlet.HashTrailer, is gitlet.CheckLayer's too
// (ADR-0008). No git process runs: git's verifiers cost twenty times a
// trailer for the graph and miss the bitmap.

// fileKind is which file of an incarnation a check hashed: the `file`
// attribute of crewlet.code.cache.checks. A pack's own files are named here;
// a derived file is named by its declaration (Owned.File, derived.go), so a
// derived file is checked by declaring it.
type fileKind string

const (
	fileIdx fileKind = "idx"
	fileRev fileKind = "rev"
	// filePack is a pack's own bytes, held to the trailer that is its name:
	// only the scrub reads one (scrub.go), behind the serving.
	filePack fileKind = "pack"
)

// checkOutcome is what hashing one file concluded: the `outcome` attribute of
// crewlet.code.cache.checks.
type checkOutcome string

const (
	// checkSound: its trailer is the SHA-1 of the bytes before it, and its
	// name or row says nothing else of it.
	checkSound checkOutcome = "sound"
	// checkDamaged: it is not.
	checkDamaged checkOutcome = "damaged"
	// checkUnreadable: a read of it failed. Repaired as damaged, since a
	// rename over it or a drop replaces a bad sector. A refused read slot or
	// an ended context is no outcome (noVerdict).
	checkUnreadable checkOutcome = "unreadable"
	// checkReplaced: it was replaced or removed while it was hashed — no
	// verdict: its successor is unvouched, and the next check reads it.
	checkReplaced checkOutcome = "replaced"
)

// checkTrigger is why a check hashed what it hashed: the `trigger` attribute
// of crewlet.code.cache.checks and crewlet.code.cache.check.duration.
type checkTrigger string

const (
	// triggerFirst: the incarnation's first full round in this process,
	// which holds every file a previous process left to its trailer.
	triggerFirst checkTrigger = "first"
	// triggerUnvouched: a later full round, holding the files this process
	// has neither written nor checked since.
	triggerUnvouched checkTrigger = "unvouched"
	// triggerScrub: a scrub its ScrubInterval made due (scrub.go). A scrub
	// after a copy's first full round is `first`, as that round's check is.
	triggerScrub checkTrigger = "scrub"
	// triggerReport: a scrub a git process's report made due — a pack or an
	// object it could not read, a pack it named in words, or a crash.
	triggerReport checkTrigger = "report"
	// triggerForced: the in-place round of a pack's or an object's repair
	// (Cache.Rebuild, repairDamaged), holding every index file to its
	// trailer whatever its vouch.
	triggerForced checkTrigger = "forced"
	// triggerRequest: the scrub a repair runs after that round
	// (Cache.escalate), and a pack a confirmed hint named, installed again
	// without a hash of the cache's own.
	triggerRequest checkTrigger = "request"
)

// hashVerdict is what hashing one file concluded: sound, damaged, or none (the
// file was replaced or removed while it was read).
type hashVerdict uint8

const (
	hashNone hashVerdict = iota
	hashSound
	hashDamaged
)

// hashTrailer is the length of a git hashfile's trailer: a SHA-1, since every
// repository the engine serves is SHA-1 (gitlet.PackRef.GitChecksum is forty
// hex digits).
const hashTrailer = sha1.Size

// fileID is the identity a verdict about a file's bytes is kept by: device,
// inode, size and modification time, or absence. No ctime: an adoption and a
// seeded rebuild hard-link files, which moves ctime without changing a byte.
// A pack's modification time is no part of it (packModified): git freshens a
// .pack whenever a process writes an object it holds (object-file.c's
// freshen_packed_object; an engine write's build over a copy does), which
// would spoil the scrub's verdict and the repair mark's and a confirmed
// hint's naming of it for as long as an agent kept writing. Nothing writes a
// pack through its own inode, so inode and size catch every change of it but
// media damage, which is the scrub's.
type fileID struct {
	present        bool
	dev, ino       uint64
	size, modified int64
}

// idOf is info's identity: a .pack's without its modification time (fileID).
func idOf(info fs.FileInfo) fileID {
	f := fileID{present: true, size: info.Size()}
	if !packModified(info.Name()) {
		f.modified = info.ModTime().UnixNano()
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		f.dev, f.ino = uint64(st.Dev), st.Ino //nolint:unconvert // Dev is int32 on darwin
	}
	return f
}

// packModified reports whether name is a .pack, whose modification time git's
// freshening moves (fileID). A .idx or .rev is never freshened, so its
// modification time stays part of its identity.
func packModified(name string) bool { return strings.HasSuffix(name, ".pack") }

// lstatID is the identity of what is at path now, or the zero (absent) one.
func lstatID(path string) fileID {
	info, err := os.Lstat(path)
	if err != nil {
		return fileID{}
	}
	return idOf(info)
}

// checkedFile is one checkable file of an incarnation: its kind, its path
// relative to the incarnation, the identity it was hashed at, and — for one
// found damaged — why it was hashed.
type checkedFile struct {
	kind    fileKind
	rel     string
	id      fileID
	trigger checkTrigger
	// scrubbed: a scrub found it (scrub.go), not a round's check — the two
	// share the trigger `first`.
	scrubbed bool
	// hinted: a pack a confirmed hint named (Failure.Packs) — its bytes held
	// to its row by ConfirmHint's hash, not to its trailer by the cache's.
	hinted bool
}

// by is what a derived file's drop for this finding is logged as asked by:
// the round's check, or a scrub (code_cache_derived_repaired's trigger).
func (f checkedFile) by() string {
	if f.scrubbed {
		return dropScrub
	}
	return dropCheck
}

// expect is what a file's name or row says of its last bytes beside the
// trailer rule: prior is the twenty before the trailer (a .idx's and .rev's
// pack checksum), trailer the trailer itself (a .pack's name, a published
// layer's .midx's and .graph's); nil says nothing. run is the provenance
// index's SHA-256 trailer (CheckRunTrailer). whole is a chain file's rule
// (CheckLayers' main file): no trailer, every byte held to content, the ids
// installed a line each.
type expect struct {
	prior, trailer []byte
	run            bool
	whole          bool
	content        []byte
}

// hasher is the trailer's hash and its length under e's rule.
func (e expect) hasher() (hash.Hash, int) {
	if e.run {
		return sha256.New(), sha256.Size
	}
	return sha1.New(), hashTrailer //nolint:gosec // git's hashfile trailer is a SHA-1
}

// pieces is how a hash reads a file: through buf, size bytes at a time
// (CheckPieceBytes unless a case lowers it), each piece in a slot hold takes
// (gitproc.Pool.HoldRead; nil holds nothing). read is a case's hook, told the
// path after each piece with its slot held and again once it is given back.
type pieces struct {
	buf  []byte
	size int64
	hold func(context.Context) (func(), error)
	read func(path string, held bool)
	// fault, when set, is asked before each piece is read, and an error it
	// answers is that read's: a case's disk failing under one file.
	fault func(path string) error
}

// pieces is the way this cache's hashes read a file, holding a slot with hold.
func (c *Cache) pieces(hold func(context.Context) (func(), error)) pieces {
	p := pieces{buf: make([]byte, copyBuffer), size: CheckPieceBytes, hold: hold}
	if c.knobs != nil {
		if c.knobs.checkPiece > 0 {
			p.size = c.knobs.checkPiece
		}
		p.read, p.fault = c.knobs.pieceRead, c.knobs.readFault
	}
	return p
}

// of is p as gitlet.HashTrailer reads the file at path by, a case's hooks
// run inside the hold: the fault once the slot is held and before the read,
// the read hook with the slot held and again once it is given back.
func (p pieces) of(path string) gitlet.Pieces {
	if p.read == nil && p.fault == nil {
		return gitlet.Pieces{Buf: p.buf, Size: p.size, Hold: p.hold}
	}
	return gitlet.Pieces{Buf: p.buf, Size: p.size, Hold: func(ctx context.Context) (func(), error) {
		release := func() {}
		if p.hold != nil {
			var err error
			if release, err = p.hold(ctx); err != nil {
				return nil, err
			}
		}
		if p.fault != nil {
			if err := p.fault(path); err != nil {
				release()
				return nil, err
			}
		}
		return func() {
			if p.read != nil {
				p.read(path, true)
			}
			release()
			if p.read != nil {
				p.read(path, false)
			}
		}, nil
	}}
}

// layer is p as gitlet.CheckLayer reads by: one gitlet.Pieces for the layer's
// three files, so without the cases' hooks, which each name one file.
func (p pieces) layer() gitlet.Pieces {
	return gitlet.Pieces{Buf: p.buf, Size: p.size, Hold: p.hold}
}

// repairAction is what a repair does about the checkable file at rel found
// failing: a derived file is dropped, a pack's file installed again, a file
// of the installed chain written or fetched again (planChain) — but the
// node's own tip (tip, recordedTip), which it writes as it writes its graph,
// is dropped and written again at once.
func (im *image) repairAction(rel, tip string) string {
	switch i, o := im.owner(rel); {
	case i < 0:
		return "reinstalled"
	case tip != "" && rel == tipRel(tip):
		return "dropped"
	case im.decls[i].chained() && o.Main:
		// A chain file is written again from the ids installed (planChain).
		return "rewritten"
	case im.decls[i].chained():
		// A published layer's file is fetched again, the chain cut at it
		// meanwhile (planChain).
		return "refetched"
	}
	return "dropped"
}

// noVerdict reports whether a hash's error says nothing of the file: its
// context ended, or no read slot was free within gitproc.SlotWait. Any other
// error is a failed read of it.
func noVerdict(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, gitproc.ErrBusy)
}

// trailerSound holds the file at path to its hashfile trailer (git's SHA-1,
// or a provenance run's SHA-256, expect.run) by gitlet.HashTrailer, a piece
// at a time as p says, and to want. A non-regular file, or one shorter than
// its trailer and want's prior, is damaged. A verdict holds only for the file
// it hashed: the path is Lstat'ed before the open, and the open file and the
// path again after, so a file written in place, renamed over or removed
// meanwhile gets hashNone. It answers the identity hashed and the bytes read;
// an error is a failed read, never a verdict.
func trailerSound(ctx context.Context, path string, p pieces, want expect) (hashVerdict, fileID, int64, error) {

	before, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return hashNone, fileID{}, 0, nil
	case err != nil:
		return hashNone, fileID{}, 0, err
	case !before.Mode().IsRegular():
		return hashDamaged, idOf(before), 0, nil
	case want.whole:
		return contentSound(ctx, path, idOf(before), want.content)
	}
	id := idOf(before)
	sum, trailerLen := want.hasher()
	need := int64(trailerLen + len(want.prior))
	if id.size < need {
		return hashDamaged, id, 0, nil
	}
	f, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return hashNone, id, 0, nil
	case err != nil:
		return hashNone, id, 0, err
	}
	defer func() { _ = f.Close() }() // read-only: what was read is the answer

	t, err := gitlet.HashTrailer(ctx, f, id.size, sum, p.of(path))
	read := t.Read
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		// Shorter than when it was Lstat'ed: rewritten while it was read.
		return hashNone, id, read, nil
	case err != nil:
		return hashNone, id, read, err
	}
	prior := make([]byte, len(want.prior))
	if len(prior) > 0 {
		n, priorErr := f.ReadAt(prior, id.size-need)
		read += int64(n)
		switch {
		case n == len(prior):
		case errors.Is(priorErr, io.EOF):
			// Shorter than when it was Lstat'ed, as above.
			return hashNone, id, read, nil
		default:
			return hashNone, id, read, priorErr
		}
	}
	after, err := f.Stat()
	if err != nil {
		return hashNone, id, read, err
	}
	// Both: the open file written through its own inode moves the first,
	// and another file renamed in over the path — or the path removed —
	// moves the second while the open file keeps every byte it had.
	if idOf(after) != id || lstatID(path) != id {
		return hashNone, id, read, nil
	}
	switch {
	case !t.Sound(),
		want.prior != nil && !bytes.Equal(prior, want.prior),
		want.trailer != nil && !bytes.Equal(t.Held, want.trailer):
		return hashDamaged, id, read, nil
	}
	return hashSound, id, read, nil
}

// chainVerdict is v once the reading is over: hashNone for a chain file
// (want.whole) whose incarnation's chain files moved between m, the mark
// taken with chainWants, and now (chainStill). An in-place write of a chain
// file moves the file and its record at two moments, so a hash across it may
// compare the two at different moments. Every other verdict stands. e.mu is
// not held.
func chainVerdict(e *entry, inc *incarnation, want expect, m chainMark, v hashVerdict) hashVerdict {
	if !want.whole || v == hashNone {
		return v
	}
	e.mu.Lock()
	still := chainStill(inc, m)
	e.mu.Unlock()
	if !still {
		return hashNone
	}
	return v
}

// contentSound holds a chain file at path, Lstat'ed at id, to want byte for
// byte (CheckLayers), reading at most want's length and a byte, since a longer
// file is damaged whatever it holds. As in trailerSound, a file replaced or
// removed while it was read has no verdict.
func contentSound(ctx context.Context, path string, id fileID, want []byte) (hashVerdict, fileID, int64, error) {
	if err := ctx.Err(); err != nil {
		return hashNone, id, 0, err
	}
	if len(want) == 0 || id.size != int64(len(want)) {
		// An incarnation that installed no layer holds no chain file
		// (writeChains).
		return hashDamaged, id, 0, nil
	}
	f, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return hashNone, id, 0, nil
	case err != nil:
		return hashNone, id, 0, err
	}
	defer func() { _ = f.Close() }() // read-only: what was read is the answer
	got := make([]byte, len(want)+1)
	n, err := io.ReadFull(f, got)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return hashNone, id, int64(n), err
	}
	after, err := f.Stat()
	if err != nil {
		return hashNone, id, int64(n), err
	}
	if idOf(after) != id || lstatID(path) != id {
		return hashNone, id, int64(n), nil
	}
	if !bytes.Equal(got[:n], want) {
		return hashDamaged, id, int64(n), nil
	}
	return hashSound, id, int64(n), nil
}

// checksumBytes is a pack checksum (forty hex digits) as the twenty bytes a
// .idx, a .rev and a .pack carry.
func checksumBytes(cs string) []byte {
	b, err := hex.DecodeString(cs)
	if err != nil || len(b) != hashTrailer {
		return nil
	}
	return b
}

// held is how a check and the scrub hold the derived file f, as its
// declaration's Check says (derived.go), never as its name suggests: the kind
// it is counted as (Owned.File) and what it is hashed against. A Check no case
// here reads is refused as this build's bug, Validate admitting only known
// Checks, never held to a trailer it may not carry; one no hash reads
// (Check.hashed: the memo's) is passed over by every caller (heldFiles,
// Verify's checkDerived) and refused here too. chains is what each chain file
// holds when sound (chainWants), by path relative to the incarnation.
func (f ownedFile) held(chains map[string][]byte) (checkedFile, expect, error) {
	cf := checkedFile{kind: fileKind(f.as.File), rel: f.rel}
	switch f.of.Check {
	case CheckTrailer:
		// git's hashfile trailer, and for a name that carries a checksum
		// (Owned.Prior) that checksum in the twenty bytes before it.
		var want expect
		if f.as.Prior {
			if m := f.as.Name.FindStringSubmatch(filepath.Base(f.rel)); m != nil {
				want.prior = checksumBytes(m[1])
			}
		}
		return cf, want, nil
	case CheckRunTrailer:
		// A provenance run's own SHA-256 trailer, written with it.
		return cf, expect{run: true}, nil
	case CheckLayers:
		// A published layer's file to git's trailer, one named by its
		// checksum (Owned.Named: the .midx, the .graph) to its name as that
		// trailer, and the chain file, which has none, to the ids installed.
		var want expect
		switch {
		case f.as.Main:
			want = expect{whole: true, content: chains[f.rel]}
		case f.as.Named:
			if m := f.as.Name.FindStringSubmatch(filepath.Base(f.rel)); m != nil {
				want.trailer = checksumBytes(m[1])
			}
		}
		return cf, want, nil
	}
	return checkedFile{}, expect{}, fmt.Errorf("cache: %s is a file of the derived file %q, which declares "+
		"the check %q, and no check of this build holds a file to it", filepath.ToSlash(f.rel), f.of.Name,
		f.of.Check)
}

// heldFiles is every derived file of files as held holds it, adding what each
// is hashed against to wants; a file whose Check no hash reads (Check.hashed)
// is passed over.
func heldFiles(files []ownedFile, wants map[string]expect, chains map[string][]byte) ([]checkedFile, error) {
	out := make([]checkedFile, 0, len(files))
	for _, f := range files {
		if !f.of.Check.hashed() {
			continue
		}
		cf, want, err := f.held(chains)
		if err != nil {
			return nil, err
		}
		out = append(out, cf)
		wants[cf.rel] = want
	}
	return out, nil
}

// packFileRel names one of a pack's files relative to its incarnation, in the
// directory its place puts it in (packPlace).
func packFileRel(place packPlace, cs, ext string) string {
	return filepath.Join(place.rel(), "pack-"+cs+ext)
}

// vouches is which of an incarnation's files this process wrote or checked,
// at which identity (incarnation.vouched): a pack's .idx and .rev in one entry
// keyed by its checksum's twenty bytes, the derived files (a handful) by path.
type vouches struct {
	packs   map[[hashTrailer]byte]packVouch
	derived map[string]fileStamp
}

// packVouch is a pack's .idx and .rev as vouched for; a zero stamp is no
// vouch.
type packVouch struct {
	idx, rev fileStamp
}

// fileStamp is a fileID as a vouch keeps it: always present, so presence is
// not stored, and a zero stamp is no file (no inode is numbered zero).
type fileStamp struct {
	dev, ino       uint64
	size, modified int64
}

// stampOf is id as a vouch keeps it.
func stampOf(id fileID) fileStamp {
	return fileStamp{dev: id.dev, ino: id.ino, size: id.size, modified: id.modified}
}

// packIndexOf is the checksum bytes and extension of the pack index file at
// rel (pack-<forty hex>.idx or .rev, in objects/pack or the dropping view), or
// false for any other path, a .pack among them. One vouch a checksum, wherever
// its files lie: a pack the rows move has files in both places only while a
// round installs it, and the vouch, held to one identity (vouchedAt), carries
// no verdict to the other copy.
func packIndexOf(rel string) ([hashTrailer]byte, string, bool) {
	var key [hashTrailer]byte
	dir, name := filepath.Split(rel)
	if !slices.Contains(packDirs, filepath.Clean(dir)) {
		return key, "", false
	}
	m := packFileName().FindStringSubmatch(name)
	if m == nil || m[2] == "pack" {
		return key, "", false
	}
	if _, err := hex.Decode(key[:], []byte(m[1])); err != nil {
		return key, "", false
	}
	return key, m[2], true
}

// vouch records that inc's file at rel, at identity id, is one this process
// wrote or checked; e.mu is held. A .pack is never vouched for: the check
// reads none, and the scrub reads every one.
func vouch(inc *incarnation, rel string, id fileID) {
	if !id.present {
		return
	}
	if key, ext, ok := packIndexOf(rel); ok {
		if inc.vouched.packs == nil {
			inc.vouched.packs = map[[hashTrailer]byte]packVouch{}
		}
		v := inc.vouched.packs[key]
		if ext == "idx" {
			v.idx = stampOf(id)
		} else {
			v.rev = stampOf(id)
		}
		inc.vouched.packs[key] = v
		return
	}
	if packFileName().MatchString(filepath.Base(rel)) {
		return // a .pack
	}
	if inc.vouched.derived == nil {
		inc.vouched.derived = map[string]fileStamp{}
	}
	inc.vouched.derived[rel] = stampOf(id)
}

// vouchFiles vouches for inc's files at rels as they are now: files this
// process has just written there. Called holding no lock but the write's.
func (c *Cache) vouchFiles(e *entry, inc *incarnation, rels ...string) {
	ids := make([]fileID, len(rels))
	for i, rel := range rels {
		ids[i] = lstatID(filepath.Join(inc.dir, rel))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, rel := range rels {
		vouch(inc, rel, ids[i])
	}
}

// vouchPack vouches for the index files of inc's pack cs at place as they are
// now: files this process has just placed there, from a bundle held to its
// row.
func (c *Cache) vouchPack(e *entry, inc *incarnation, place packPlace, cs string) {
	c.vouchFiles(e, inc, packFileRel(place, cs, ".idx"), packFileRel(place, cs, ".rev"))
}

// ids is the identity of every derived file in the incarnation at dir now,
// by its path relative to it; a listing that fails reads as none, which
// vouches nothing afterwards.
func (im *image) ids(dir string) map[string]fileID {
	files, err := im.present(dir)
	if err != nil {
		return nil
	}
	ids := make(map[string]fileID, len(files))
	for _, f := range files {
		ids[f.rel] = lstatID(filepath.Join(dir, f.rel))
	}
	return ids
}

// vouchWritten vouches for every derived file of inc a git process of this
// one has just written (present at an identity other than before's) and
// stamps its declaration written (report.go's wrote). Only what moved: git
// keeps a file it would write unchanged, so a step that kept a damaged bitmap
// vouches nothing of it and the next check still reads it.
func (c *Cache) vouchWritten(e *entry, inc *incarnation, before map[string]fileID) {
	var rels []string
	var written derivedSet
	for rel, id := range c.image.ids(inc.dir) {
		if id.present && before[rel] != id {
			rels = append(rels, rel)
			if i, _ := c.image.owner(rel); i >= 0 {
				written |= 1 << i
			}
		}
	}
	if len(rels) > 0 {
		c.vouchFiles(e, inc, rels...)
		e.mu.Lock()
		c.wrote(inc, written)
		e.mu.Unlock()
	}
}

// unvouch forgets the vouches for inc's files at rels: files dropped or
// moved out of it. e.mu is held.
func unvouch(inc *incarnation, rels ...string) {
	for _, rel := range rels {
		key, ext, ok := packIndexOf(rel)
		if !ok {
			delete(inc.vouched.derived, rel)
			continue
		}
		v, held := inc.vouched.packs[key]
		if !held {
			continue
		}
		if ext == "idx" {
			v.idx = fileStamp{}
		} else {
			v.rev = fileStamp{}
		}
		if v == (packVouch{}) {
			delete(inc.vouched.packs, key)
		} else {
			inc.vouched.packs[key] = v
		}
	}
}

// vouchedAt reports whether inc's file at rel is the one this process wrote
// or checked at identity id; e.mu is held.
func vouchedAt(inc *incarnation, rel string, id fileID) bool {
	if !id.present {
		return false
	}
	if key, ext, ok := packIndexOf(rel); ok {
		v := inc.vouched.packs[key]
		have := v.rev
		if ext == "idx" {
			have = v.idx
		}
		return have != (fileStamp{}) && have == stampOf(id)
	}
	have, ok := inc.vouched.derived[rel]
	return ok && have == stampOf(id)
}

// vouchedRels is every path, relative to inc, a vouch is held for, sorted —
// a pack's at the place its live set puts it; e.mu is held.
func vouchedRels(inc *incarnation) []string {
	var out []string
	for key, v := range inc.vouched.packs {
		cs := hex.EncodeToString(key[:])
		place := placeOf(inc, cs)
		if v.idx != (fileStamp{}) {
			out = append(out, packFileRel(place, cs, ".idx"))
		}
		if v.rev != (fileStamp{}) {
			out = append(out, packFileRel(place, cs, ".rev"))
		}
	}
	for rel := range inc.vouched.derived {
		out = append(out, rel)
	}
	slices.Sort(out)
	return out
}

// checked is what a check found: why it ran, how many files were sound, every
// file found damaged at the identity it was hashed at (still), and the bytes
// read. forced: a repair's in-place round asked it (Cache.Rebuild,
// repairDamaged), so it ignored vouches. at and ended are when it ended, on
// Options.Now's clock and on time.Now(): a forced check's verdict, recorded by
// its round (incarnation.forcedAt).
type checked struct {
	trigger checkTrigger
	forced  bool
	sound   int
	damaged []checkedFile
	// tip is the node's tip the copy held as the check read what it holds
	// (recordedTip): what a finding of its file says was done about it.
	tip       string
	bytes     int64
	at, ended time.Time
}

// checkIndexes is step 2 of a full round (sync.go apply): it holds to its
// trailer every checkable file of inc the snapshot names (the .idx and .rev of
// every pack present at its row's sizes, and every present derived file git
// reads) not vouched for at its identity now, and vouches what passes. A pack
// absent or at the wrong size is the install's; no .pack is hashed before
// serving (scrub.go), nor a provenance run (provenance.go). It holds one read
// slot a piece and none between. The round holds the entry's write lock and
// has stopped the maintainer, so nothing else writes a file it hashes. Forced
// (Cache.Rebuild, repairDamaged), it ignores vouches: the damage may postdate
// a check. Not `check`: verify.go's Verify body has that name.
func (c *Cache) checkIndexes(ctx context.Context, e *entry, inc *incarnation, snap snapshot, forced bool) (checked,
	error) {

	e.mu.Lock()
	trigger := triggerUnvouched
	switch {
	case !inc.checkedOnce:
		trigger = triggerFirst
	case forced:
		trigger = triggerForced
	}
	e.mu.Unlock()
	began := time.Now()
	var (
		ck    = checked{trigger: trigger, forced: forced}
		files []checkedFile
		wants = map[string]expect{}
	)
	// Each pack where the snapshot places it: a pack held at the other place
	// — its rows moved it — is the install's, as one absent is.
	places := snap.places()
	for _, p := range snap.packs {
		place := places[p.GitChecksum]
		held, err := installed(filepath.Join(inc.dir, place.rel()), p, true)
		if err != nil {
			return ck, err
		}
		if held != packWhole {
			continue
		}
		prior := checksumBytes(p.GitChecksum)
		for _, f := range []struct {
			kind fileKind
			ext  string
		}{{fileIdx, ".idx"}, {fileRev, ".rev"}} {
			rel := packFileRel(place, p.GitChecksum, f.ext)
			if _, twice := wants[rel]; twice {
				continue // two rows of one checksum name one file
			}
			files = append(files, checkedFile{kind: f.kind, rel: rel})
			wants[rel] = expect{prior: prior}
		}
	}
	present, err := c.image.present(inc.dir)
	if err != nil {
		return ck, err
	}
	// Only a file GIT reads is the round's to hold before it serves (its
	// Part, derived.go): the installed chain's are, though the cache writes
	// them, and the provenance runs and the memo are not.
	present = slices.DeleteFunc(present, func(f ownedFile) bool { return !f.of.gitReads() })
	e.mu.Lock()
	chains, mark := chainWants(inc), markChain(inc)
	ck.tip = recordedTip(inc)
	e.mu.Unlock()
	derived, err := heldFiles(present, wants, chains)
	if err != nil {
		return ck, err
	}
	files = append(files, derived...)
	slices.SortFunc(files, func(a, b checkedFile) int { return strings.Compare(a.rel, b.rel) })

	p := c.pieces(c.git.HoldRead)
	defer func() {
		if c.metrics != nil && ck.bytes > 0 {
			c.metrics.Observe(metrics.CodeCacheCheckDuration, time.Since(began),
				metrics.Attrs{"trigger": string(trigger)})
		}
	}()
	for _, f := range files {
		path := filepath.Join(inc.dir, f.rel)
		e.mu.Lock()
		skip := !forced && vouchedAt(inc, f.rel, lstatID(path))
		e.mu.Unlock()
		if skip {
			continue
		}
		if c.knobs != nil && c.knobs.hashing != nil {
			c.knobs.hashing(path)
		}
		v, id, n, err := trailerSound(ctx, path, p, wants[f.rel])
		v = chainVerdict(e, inc, wants[f.rel], mark, v)
		ck.bytes += n
		f.id = id
		switch {
		case err != nil && noVerdict(ctx, err):
			// No slot in SlotWait, or the round's context ended: nothing
			// is known of the file. The round fails busy, its callers
			// answered as any read the lane refuses.
			return ck, fmt.Errorf("checking %s against its trailer: %w", f.rel, err)
		case err != nil:
			// Unreadable: repaired as damaged, which replaces the inode the
			// disk cannot read. Failing the round would leave the repository
			// unserved for good: no round could pass the file, and the
			// maintainer runs only on a converged copy.
			c.countCheck(f.kind, checkUnreadable, trigger)
			f.trigger = trigger
			ck.damaged = append(ck.damaged, f)
			c.log.Warn("code_cache_check_damaged", "repo", e.id, "incarnation", inc.n, "file", f.rel,
				"trigger", string(trigger), "action", c.image.repairAction(f.rel, ck.tip), "error", err)
			continue
		case v == hashNone:
			c.countCheck(f.kind, checkReplaced, trigger)
			continue
		case v == hashSound:
			c.countCheck(f.kind, checkSound, trigger)
			ck.sound++
			e.mu.Lock()
			vouch(inc, f.rel, id)
			if c.backfillReads(f.rel) {
				c.backfillDirty(e)
			}
			e.mu.Unlock()
			continue
		}
		c.countCheck(f.kind, checkDamaged, trigger)
		f.trigger = trigger
		ck.damaged = append(ck.damaged, f)
		c.log.Warn("code_cache_check_damaged", "repo", e.id, "incarnation", inc.n, "file", f.rel,
			"trigger", string(trigger), "action", c.image.repairAction(f.rel, ck.tip))
	}
	e.mu.Lock()
	inc.checkedOnce = true
	e.mu.Unlock()
	ck.at, ck.ended = c.now(), time.Now()
	return ck, nil
}

// recordCheck records a full round's check on inc's repair mark, whatever the
// round meets after, and returns the findings the round repairs; e.mu is not
// held. Its forces are spent now: settled after the install, a failing
// install would force every later round. A finding stays on the mark
// (repairMark.found) at the identity it failed at until a round installs or
// drops it (settleFound): damaged in place, a file keeps its vouch. One found
// again at that identity stays as found, a scrub's above all (it answers a
// request's scrub and covers a report, scrubWaits); only another identity
// replaces it (checked.still). While a check is admitted (entry.verifying),
// the round gets only this check's, as the mark holds them (the repair logs
// what found the damage first), so no repair moves a file it admitted; the
// rest wait for its release, which asks the round again.
func (c *Cache) recordCheck(e *entry, inc *incarnation, forces int, damaged []checkedFile) []checkedFile {
	e.mu.Lock()
	defer e.mu.Unlock()
	inc.repair.forces = max(inc.repair.forces-forces, 0)
	for _, f := range damaged {
		if have, ok := inc.repair.found[f.rel]; ok && have.id == f.id {
			continue
		}
		if inc.repair.found == nil {
			inc.repair.found = map[string]checkedFile{}
		}
		inc.repair.found[f.rel] = f
		if c.backfillReads(f.rel) {
			// A run that failed waits for the round's repair, and the
			// backfill's look checks it no more (runMarks).
			c.backfillDirty(e)
		}
	}
	if e.verifying > 0 {
		own := make([]checkedFile, 0, len(damaged))
		for _, f := range damaged {
			own = append(own, inc.repair.found[f.rel])
		}
		return own
	}
	found := make([]checkedFile, 0, len(inc.repair.found))
	for _, f := range inc.repair.found {
		found = append(found, f)
	}
	slices.SortFunc(found, func(a, b checkedFile) int { return strings.Compare(a.rel, b.rel) })
	return found
}

// countCheck counts one file a check hashed.
func (c *Cache) countCheck(kind fileKind, outcome checkOutcome, trigger checkTrigger) {
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheChecks, 1,
			metrics.Attrs{"file": string(kind), "outcome": string(outcome), "trigger": string(trigger)})
	}
}

// still is what of ck's findings (repairMark.found, and a confirmed hint's
// repairMark.reinstall) the incarnation at dir still holds at the identity
// each was found at: the packs to install again by checksum, each with its
// first finding, and the derived parts to drop. A repair acts only on the
// file its finding names: a successor (the maintainer's rewrite, an earlier
// repair) is unhashed, and left to the next round or scrub.
func (ck checked) still(im *image, dir string) (map[string]packFinding, derivedDrops) {
	packs := map[string]packFinding{}
	var drops derivedDrops
	for _, f := range ck.damaged {
		if lstatID(filepath.Join(dir, f.rel)) != f.id {
			continue
		}
		if i, _ := im.owner(f.rel); i >= 0 {
			// A file of the installed chain is the chain plan's to answer
			// (planChain), never a drop.
			if !im.decls[i].chained() {
				drops.add(im.decls[i].Name, time.Time{}, f.by())
			}
			continue
		}
		m := packFileName().FindStringSubmatch(filepath.Base(f.rel))
		if m == nil {
			continue
		}
		p, ok := packs[m[1]]
		if !ok {
			p.trigger = f.trigger
		}
		p.hinted = p.hinted || f.hinted
		p.index = p.index || f.kind == fileIdx
		packs[m[1]] = p
	}
	return packs, drops
}

// packFinding is what the findings still true of one pack say: the first's
// trigger, whether a confirmed hint named it, and whether its .idx failed, so
// the derived files built from it go too (install).
type packFinding struct {
	trigger       checkTrigger
	hinted, index bool
}

// findings is the sentences a check's damaged files are reported to a
// verification as, the sync before it having repaired them in place.
func (ck checked) findings(im *image) []string {
	out := make([]string, 0, len(ck.damaged))
	for _, f := range ck.damaged {
		if f.hinted {
			// No check's finding: a request's, handed on by its repair.
			continue
		}
		action := "dropped it to be written again"
		switch im.repairAction(f.rel, ck.tip) {
		case "reinstalled":
			action = "fetched its pack again"
		case "refetched":
			action = "cut the published chain there to fetch it again"
		case "rewritten":
			action = "wrote it again from the installed chain"
		}
		out = append(out, fmt.Sprintf("the sync before the check found %s failing its checksum and %s",
			filepath.ToSlash(f.rel), action))
	}
	return out
}
