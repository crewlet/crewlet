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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE CHECK: nothing an index file says is served from a file this process did
// not write or check (doc.go, "What this process did not write is checked
// before it serves"). A CHECKABLE file is one git reads to find an object or a
// parent — the commit-graph, the multi-pack-index with its bitmap and reverse
// index, and every pack's .idx and .rev — and git checks none of them when it
// reads: a flipped byte of any is read with no word, and a damaged one can
// answer WRONG — a parent the force check reads, an object offset a clone
// ships the wrong bytes from, a bitmap that drops objects from every clone.
// Each is a git HASHFILE, whose last twenty bytes are the SHA-1 of every byte
// before them (git's finalize_hashfile), and that is the check: a .idx and a
// .rev also carry their pack's checksum — the pack's name — in the twenty
// before it, and a .pack's trailer IS its name. The rule has ONE
// implementation, code.HashTrailer, which a published layer's check
// (code.CheckLayer) holds the layer's files to as well, so a layer a git node
// fetched and the layer it keeps are judged by one reading (ADR-0008). No git
// process runs: git's own verifiers cost twenty times a trailer for the graph
// and are blind to the bitmap.

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
	// checkUnreadable: a read of it failed — damaged at the identity it had,
	// and repaired as a file that failed its trailer is: a disk's bad sector
	// is what a rename over the file, or a drop, replaces. Never a slot the
	// read lane did not grant, nor a context that ended: those say nothing
	// of the file and are counted under no outcome.
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
	// triggerScrub: a scrub its interval made due — a used copy's every file
	// re-hashed a ScrubInterval after its last (scrub.go). A scrub after a
	// copy's first full round is `first`, as that round's check is.
	triggerScrub checkTrigger = "scrub"
	// triggerReport: a scrub a git process's report made due — a pack or an
	// object it could not read, a pack it named in words, or a crash.
	triggerReport checkTrigger = "report"
	// triggerForced: the in-place round of a pack's or an object's repair —
	// a failed request's (Cache.Rebuild) or a round's own git's
	// (repairDamaged) — holding every index file to its trailer whatever its
	// vouch.
	triggerForced checkTrigger = "forced"
	// triggerRequest: what that in-place round did not end — the scrub the
	// repair runs next (Cache.escalate) — and a pack a confirmed hint named,
	// installed again without a hash of the cache's own.
	triggerRequest checkTrigger = "request"
)

// hashVerdict is what hashing one file concluded: sound, damaged, or NONE — the
// file was replaced or removed while it was read, so no verdict is its.
type hashVerdict uint8

const (
	hashNone hashVerdict = iota
	hashSound
	hashDamaged
)

// hashTrailer is the length of a git hashfile's trailer: a SHA-1, since every
// repository the engine serves is SHA-1 (code.PackRef.GitChecksum is forty
// hex digits). A SHA-256 object format would be a trailer of 32, checked the
// same way, and lands with that format.
const hashTrailer = sha1.Size

// fileID is a file's identity as a verdict about its bytes is kept by: its
// device and inode, its size and its modification time — or its absence. NO
// CTIME: an adoption and a seeded rebuild hard-link files, and every link and
// unlink moves the inode's ctime without changing a byte. A file renamed in
// over it, written through its own inode (which moves its modification time)
// or removed is another identity; a file whose bytes change with NO change of
// identity — media damage — is the scrub's to find, never a vouch's.
type fileID struct {
	present        bool
	dev, ino       uint64
	size, modified int64
}

// idOf is info's identity.
func idOf(info fs.FileInfo) fileID {
	f := fileID{present: true, size: info.Size(), modified: info.ModTime().UnixNano()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		f.dev, f.ino = uint64(st.Dev), st.Ino //nolint:unconvert // Dev is int32 on darwin
	}
	return f
}

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

// expect is what a file's name or its row says of its last bytes beside the
// trailer rule: prior is the twenty before the trailer (a .idx's and a .rev's
// pack checksum), trailer the trailer itself (a .pack's name, and a published
// layer's .midx's and .graph's); nil says nothing. run is the provenance
// index's rule (CheckRunTrailer) in place of git's: a SHA-256 trailer, the
// last 32 bytes, rather than git's SHA-1. whole is a CHAIN FILE's rule
// (CheckLayers' main file), which carries no trailer: its every byte is
// content, the ids the incarnation installed a line each.
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

// pieces is how a hash reads a file: through buf, size bytes at a time —
// CheckPieceBytes, unless a case lowers it — each piece in a slot hold takes
// and gives back once the piece is read (gitproc.Pool.HoldRead; nil holds
// nothing), and read, when set, told the file's path after each piece twice:
// with its slot still held, and once it is given back (a case's hook).
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

// of is p as the one hash of git's trailer rule reads the file at path by
// (code.HashTrailer): through p's buffer, a piece of p.size in each slot
// p.hold takes — and, where a case set them, its hooks told of each piece
// through the hold itself: the fault asked once the piece's slot is held and
// before a byte of it is read, its error that read's; the read hook once the
// piece is read with its slot still held, and again once the slot is given
// back.
func (p pieces) of(path string) code.Pieces {
	if p.read == nil && p.fault == nil {
		return code.Pieces{Buf: p.buf, Size: p.size, Hold: p.hold}
	}
	return code.Pieces{Buf: p.buf, Size: p.size, Hold: func(ctx context.Context) (func(), error) {
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

// layer is p as a published layer's check reads by (code.CheckLayer), which
// hashes the layer's three files through one code.Pieces: p's buffer, its
// piece and its hold, without the cases' hooks, each of which names the one
// file a piece is of.
func (p pieces) layer() code.Pieces {
	return code.Pieces{Buf: p.buf, Size: p.size, Hold: p.hold}
}

// repairAction is what a repair does about the checkable file at rel found
// failing: a derived file is dropped, a pack's file installed again — and a
// file of the installed chain written again or fetched again, but for the
// node's tip (tip, its id where the copy holds one, recordedTip), which is a
// file the node writes as it writes its own graph: dropped, the chain file
// written again without it (planChain), and written again at once.
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

// noVerdict reports whether a hash's error is no fault of the file's: its
// context ended, or no read slot was free within gitproc.SlotWait. Such a
// hash says nothing of the file, and is counted under no outcome; any other
// error is a read of it that failed.
func noVerdict(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, gitproc.ErrBusy)
}

// trailerSound holds the file at path to its hashfile trailer — git's, the
// SHA-1 of every byte before its last twenty against those twenty, or a
// provenance run's SHA-256 over 32 (expect.run) — by the rule's one
// implementation, code.HashTrailer, which a published layer's check shares
// (code.CheckLayer), reading it one PIECE at a time as p says and checking ctx
// before every read; and to want. A file that is not a regular file, or
// shorter than its trailer and what want names, is damaged.
//
// A VERDICT HOLDS ONLY FOR THE FILE IT HASHED: the path is Lstat'ed before it
// is opened, and after the hash both the open file and the path again, and
// when an identity differs — the file written in place, or another renamed
// in over it, while it was read — or the path is gone, there is no verdict
// (hashNone): its successor is no file this hash read. Answered beside the verdict: the
// identity hashed, and the bytes read. An error is a read that failed, never
// a verdict.
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

	t, err := code.HashTrailer(ctx, f, id.size, sum, p.of(path))
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

// chainVerdict is v, a reader's verdict of a file held to want, as it stands
// once its reading is over: no verdict (hashNone) for a CHAIN FILE (want.whole)
// whose incarnation's chain files did not stand still from m — the mark the
// reader took with what it held the file to (chainWants) — to now
// (chainStill). A chain file is held to what the incarnation's record names,
// and an in-place write of it — the node's tip, a cut that drops the tip —
// moves the file and the record at two moments, so a hash across one may
// compare a file of one moment with a record of another: neither sound nor
// damaged. Every other verdict is the file's own. e.mu is NOT held.
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

// contentSound holds the file at path, Lstat'ed at id, to want byte for byte —
// a chain file, which carries no trailer, against the ids its incarnation
// installed (CheckLayers): one read, never more than want's length and a
// byte, since a file longer than want is damaged whatever it holds. As
// trailerSound, a file replaced or removed while it was read has no verdict.
func contentSound(ctx context.Context, path string, id fileID, want []byte) (hashVerdict, fileID, int64, error) {
	if err := ctx.Err(); err != nil {
		return hashNone, id, 0, err
	}
	if len(want) == 0 || id.size != int64(len(want)) {
		// Of no layer, a chain file is none: an incarnation that installed
		// none holds no chain file (writeChains).
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

// held is how a check and the scrub hold the derived file f to its bytes, as
// ITS DECLARATION'S Check says (derived.go) — never as a reader would guess
// from its name: the file as a check of it is counted (its Owned.File), and
// what it is hashed against. A Check no case here reads is refused: Validate
// admits only the Checks this build knows, so a reader not taught one a
// declaration names is this build's bug, never a file to hold to a trailer it
// may not carry. A Check the check and the scrub do not hash at all
// (Check.hashed: the memo's, whose every read holds an entry) is passed over by
// every caller before it asks (heldFiles, Verify's checkDerived), and refused
// here like one this build does not know.
//
// chains is what each CHAIN FILE of the incarnation holds when it is sound
// (chainWants), by its path relative to the incarnation: the one file of a
// declaration that carries no trailer, held to the ids installed.
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
		// A published layer's file to git's hashfile trailer — and one named
		// by its checksum (Owned.Named: the .midx and the .graph) to the
		// forty hex digits of its name AS that trailer — and the chain file,
		// which the cache writes from the rows and which carries no trailer,
		// to the ids installed, line for line.
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

// heldFiles is every derived file of files as a check and the scrub hold it
// (held, against chains for a chain file), with what each is hashed against
// added to wants — passing over a file whose declaration's Check no hash
// holds (Check.hashed).
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

// packRel names one of a pack's files relative to its incarnation.
func packFileRel(cs, ext string) string { return filepath.Join(packRel, "pack-"+cs+ext) }

// vouches is what of an incarnation's files this process wrote or checked, at
// which identity (incarnation.vouched): a pack's .idx and .rev in ONE entry
// keyed by the twenty bytes of the pack's checksum, and the derived files —
// a handful at most — by their path relative to the incarnation.
type vouches struct {
	packs   map[[hashTrailer]byte]packVouch
	derived map[string]fileStamp
}

// packVouch is a pack's .idx and .rev as vouched for; a zero stamp is no
// vouch.
type packVouch struct {
	idx, rev fileStamp
}

// fileStamp is a fileID as a vouch keeps it: always a file that was present,
// so its presence is not stored, and a zero stamp is no file — no inode is
// numbered zero.
type fileStamp struct {
	dev, ino       uint64
	size, modified int64
}

// stampOf is id as a vouch keeps it.
func stampOf(id fileID) fileStamp {
	return fileStamp{dev: id.dev, ino: id.ino, size: id.size, modified: id.modified}
}

// packIndexOf is the checksum, as its twenty bytes, and the extension of the
// pack index file at rel — objects/pack/pack-<forty hex>.idx or .rev — or
// false for any other path, a .pack among them.
func packIndexOf(rel string) ([hashTrailer]byte, string, bool) {
	var key [hashTrailer]byte
	dir, name := filepath.Split(rel)
	if filepath.Clean(dir) != packRel {
		return key, "", false
	}
	m := packFileName.FindStringSubmatch(name)
	if m == nil || m[2] == "pack" {
		return key, "", false
	}
	if _, err := hex.Decode(key[:], []byte(m[1])); err != nil {
		return key, "", false
	}
	return key, m[2], true
}

// vouch records that inc's file at rel, at identity id, is one this process
// wrote or checked: a pack's .idx or .rev, or a derived file, which is what
// every caller hands it; e.mu is held. A pack's own bytes are never vouched
// for: the check reads no .pack, and the scrub reads every one vouched or
// not.
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
	if packFileName.MatchString(filepath.Base(rel)) {
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

// vouchPack vouches for the index files of inc's pack cs as they are now:
// files this process has just placed there, from a bundle held to its row.
func (c *Cache) vouchPack(e *entry, inc *incarnation, cs string) {
	c.vouchFiles(e, inc, packFileRel(cs, ".idx"), packFileRel(cs, ".rev"))
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

// vouchWritten vouches for every derived file of inc that a git process of
// this one has just written: present now at an identity other than the one
// before held — and stamps its declaration written (report.go's wrote). ONLY
// WHAT MOVED: git keeps a file it would write again unchanged — a bitmap whose
// index's checksum did not move is not rewritten — so a step that ran over a
// damaged bitmap and kept it vouches nothing of it, and the next check still
// reads it.
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

// vouchedRels is every path, relative to inc, a vouch is held for, sorted;
// e.mu is held.
func vouchedRels(inc *incarnation) []string {
	var out []string
	for key, v := range inc.vouched.packs {
		cs := hex.EncodeToString(key[:])
		if v.idx != (fileStamp{}) {
			out = append(out, packFileRel(cs, ".idx"))
		}
		if v.rev != (fileStamp{}) {
			out = append(out, packFileRel(cs, ".rev"))
		}
	}
	for rel := range inc.vouched.derived {
		out = append(out, rel)
	}
	slices.Sort(out)
	return out
}

// checked is what a check found: why it ran, how many files were sound, every
// file found damaged at the identity it was hashed at — a pack's .idx or .rev,
// whose pack the install puts in again, or a derived file, which the round
// drops (still) — and the bytes read. forced: the in-place round of a pack's
// or an object's repair asked it (Cache.Rebuild, repairDamaged), so it held
// every index file to its trailer whatever its vouch. at and ended are when it
// ended, on Options.Now's clock and as a time.Now() reading: a forced check's
// verdict, which its round records once it ends (incarnation.forcedAt).
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

// checkIndexes is step 2 of a full round (sync.go apply): every checkable file
// of inc the snapshot names — the .idx and .rev of every pack present at its
// row's sizes — and every derived file git reads that is present, held to its
// trailer when it is not vouched for at the identity it has now. A derived
// file the cache's own code writes is no file git reads: the provenance
// index's runs are held to their own trailer before the cache maps one this
// process did not write (provenance.go's checks, behind the serving), so a
// copy's first request after a restart waits on none of them. A read slot of the pool is
// held for each piece of hashing and none between (D15 of the check's doc:
// one slot, a piece at a time, none while the round runs git). A pack absent
// or at the wrong size is not its: the install meets it. Nor is any .pack: a
// pack is hashed behind the serving, never before it. What passes is vouched.
// The round holds the entry's write lock and has stopped the maintainer, so
// nothing else writes a file it hashes.
//
// FORCED by a pack's or an object's damage — a request's failure
// (Cache.Rebuild) or a round's own git's (repairDamaged) — it holds every file
// to its trailer whether or not it is vouched for: what failed may be damage
// that arrived after this process checked a file, which no vouch sees.
//
// (Not `check`: verify.go's Verify body has that name.)
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
	packDir := filepath.Join(inc.dir, packRel)
	for _, p := range snap.packs {
		held, err := installed(packDir, p, true)
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
			rel := packFileRel(p.GitChecksum, f.ext)
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
			// UNREADABLE: damaged at the identity it had, and repaired as
			// a file that failed its trailer is — a derived file dropped, a
			// pack's file renamed over, which replaces the inode the disk
			// cannot read. Failing the round over it left the repository
			// unserved for good: no full round could ever pass the file,
			// and the maintainer, whose rewrite would have replaced it,
			// runs only on a copy a full round has converged.
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

// recordCheck is what a full round's check leaves on inc's repair mark once
// it has run, whatever the round meets after it, and answers every finding
// the mark holds that the round repairs; e.mu is not held. forces is how many
// repairs' forces the check took (requested), and damaged what it found.
//
//   - ITS FORCES ARE SPENT. A forced check is asked to hold every index file
//     to its trailer whatever its vouch, and once it has run, it has done
//     that. Settled only once the round's install had acted, the forces
//     outlived a round that failed there — on a pack it ADDS that the object
//     store does not have or fails to hand over — so every later round of the
//     copy was forced again, every index file hashed again under the
//     repository's write lock, until an install succeeded; and a round the
//     log's head sent on to a second apply after a pack's not-found
//     (converge) held them all to their trailers a second time itself.
//   - WHAT IT FOUND GOES ON THE MARK (repairMark.found), at the identity each
//     file failed at, as a scrub's findings do, and comes off once a round
//     has installed or dropped it (settleFound): a round that cannot act on a
//     finding — its install failing on a pack it adds, the finding's own
//     download failing — leaves it to the next. It is the FINDING that has
//     to outlive the round, not the forces: an index file damaged in place
//     keeps its identity and the vouch an earlier check gave it, which no
//     check but a forced one reads past, so the round that can at last
//     install its pack again knows of it only from the mark.
//   - A FINDING FOUND AGAIN STAYS AS IT WAS FOUND. A file the mark already
//     holds at the identity this check found it failing at is the same
//     damage, and the finding the mark holds — what found it, a SCRUB above
//     all — stands; only a finding at ANOTHER identity, newer damage in a
//     file put there since, replaces it, the one the mark holds naming a file
//     no longer there (checked.still). A scrub's finding still waiting is
//     what answers a request's scrub in that scrub's place and covers a
//     report within its cooldown (scrubWaits), which a check's never does —
//     a check reads no pack body. Written over by the forced check of a
//     request failing on that very damage, its pack held off by an object
//     store that will not hand it over, it answered neither: the request's
//     step 3 read the whole copy again, about 1.13 times its pack bytes, as
//     did the report of every clone crashing over it within the hour — once
//     an hour for as long as the store refused the pack. A scrub's own
//     finding replaces whatever the mark holds for its file (runScrub): it
//     read every byte, the pack bodies a check never does among them.
//   - The round repairs every finding on the mark — this check's, and what an
//     earlier round's check or a scrub found that still waits — but only this
//     check's while a check of the repository is admitted (entry.verifying):
//     a repair between that check's listing and its hashing would move a file
//     it admitted, so the rest wait for its release, which asks the round
//     again. This check's own are the round's to act on either way, as they
//     always were — each handed to it as the mark holds it, so the round's
//     repair is logged as what found the damage first.
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

// still is what of ck's findings — what a check or a scrub found that waits on
// the repair mark (repairMark.found: the round's own check's among them, put
// there by recordCheck) and a confirmed hint's (repairMark.reinstall) — the
// incarnation at dir still holds at the identity each was found at: the packs
// to install again, by checksum, each with its first finding, and the derived
// parts to drop, each with what asked. A REPAIR ACTS ONLY ON THE FILE ITS
// FINDING NAMES: one replaced or removed since its hash — rewritten by the
// maintainer, put right by an earlier repair — is no finding any more, and its
// successor, which no hash of this process read, is checked by the next round
// or the next scrub.
func (ck checked) still(im *image, dir string) (map[string]packFinding, derivedDrops) {
	packs := map[string]packFinding{}
	var drops derivedDrops
	for _, f := range ck.damaged {
		if lstatID(filepath.Join(dir, f.rel)) != f.id {
			continue
		}
		if i, _ := im.owner(f.rel); i >= 0 {
			// A file of the installed chain is the round's chain plan's to
			// answer — the chain cut at the layer that holds it, which is
			// fetched again, or its chain file written again (planChain) —
			// never a drop.
			if !im.decls[i].chained() {
				drops.add(im.decls[i].Name, time.Time{}, f.by())
			}
			continue
		}
		m := packFileName.FindStringSubmatch(filepath.Base(f.rel))
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

// packFinding is what of one pack's files the findings still true of it say:
// the trigger of the first that found it, whether a confirmed hint named it
// (its bundle, not a file of it), and whether its .idx failed — the derived
// files may have been built from that index (install).
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
