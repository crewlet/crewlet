package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/store"
)

// objectsDirName holds the objects a backup carries, each under the name the
// store keeps it under ([objstore.Key.Name]) — the layout an S3 backend's
// bucket has under its prefix, so restoring them is one sync of the directory
// into the bucket (docs/guides/backup.md).
const objectsDirName = "objects"

// Objects is how a backup reaches the company's files' bytes.
//
// # Why a backup carries every object a live row of the copy names
//
// A tracker restored without the bytes of its files names files nobody can
// open, and the collector deletes a removed file's object at its next pass
// once the object is more than a day old — within the hour of the removal for
// any file older than that — so a restore from a week-old backup brings back
// rows whose objects a live store may no longer hold. The backup is a copy of
// the COMPANY, so it carries every object a REQUIRED table of the store copy
// names.
//
// A RETIRED REFERENCE IS NOT CARRIED (ADR-0033). A retired row keeps a
// replaced object from the collector for a grace and names nothing the
// company is owed, so it is never copied, never asked after and never
// recorded lost: on S3 its object stays out of objects/, and on the broker's
// bucket the stream snapshot holds whatever the bucket holds without this
// package counting it. A restore brings the row back without its object,
// which nothing owes it, and its domain deletes the row once its grace has
// passed.
//
// WHICH OBJECTS is not an option: the copy is read against
// internal/objstore/references, the one list its schema gate holds, and walks
// exactly the Required tables the collector's audit walks — so a backup can
// never be built knowing fewer required tables than the audit does.
type Objects struct {
	// Open streams one object back, checked against the row naming it
	// ([objstore.Store.Open]): a stream of the wrong bytes ends in
	// [objstore.ErrCorrupt] short of the object's size, never whole.
	Open func(ctx context.Context, o objstore.Object) (io.ReadCloser, error)

	// Stat asks the store what it holds under a key — the check a backup
	// whose objects ride a stream snapshot makes once the snapshot is
	// taken ([Objects.Stream]).
	Stat func(ctx context.Context, k objstore.Key) (objstore.Info, error)

	// Stream, when set, is the broker stream the objects already live in —
	// the NATS backend's bucket — which the backup snapshots with every
	// other stream, so it copies no object beside it: the snapshot is the
	// copy, restored with the streams it was taken with.
	Stream string
}

// ObjectArtifact describes the objects inside a backup.
type ObjectArtifact struct {
	// Dir is where they are, relative to the backup directory — or, when
	// Stream is set, empty: the objects are in that stream's snapshot.
	Dir    string `json:"dir,omitempty"`
	Stream string `json:"stream,omitempty"`

	// Objects and Bytes are how many and how large — every object the
	// artefact holds, a reused one included, since shipping the directory
	// ships it. On a stream, every object a required table of the copy
	// names and the store was found holding once the snapshot was taken,
	// whose bytes that stream's snapshot already counts.
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`

	// Reused is how many of them were taken from this node's previous
	// backup on the same host (ReusedFrom) rather than read from the
	// store: a key is minted for one upload and never names other bytes,
	// so one an earlier backup already holds is the same object — linked
	// where the filesystem allows and copied where it does not, and read
	// back against its row either way. Each is a complete file of this
	// artefact, never a reference into the other one.
	Reused     int    `json:"reused,omitempty"`
	ReusedFrom string `json:"reused_from,omitempty"`

	// Lost is every object a required table of the copy names that the
	// object store answered it does not hold, or holds only as bytes that
	// are not the ones the row records — each with the file that named it,
	// which is what a person restores.
	//
	// RECORDED RATHER THAN REFUSED. Refusing would not bring the object
	// back, and every later backup would be refused for the same one —
	// which stops the trim's backup term from ever advancing, so every log
	// in the fleet grows towards its ceiling over one lost file. The
	// artefact restores everything else, the files these objects belong to
	// restore with their bytes missing exactly as they are missing now, and
	// the `objects_missing` alarm is what sends somebody to replace them.
	// An object the store could not ANSWER about is another matter — it may
	// be intact there — and still refuses the backup
	// ([ErrObjectsUnreachable]).
	Lost []LostObject `json:"lost,omitempty"`
}

// LostObject is one object a backup could not carry, and the file that names
// it: a key alone is a name nobody can look up.
type LostObject struct {
	Object  objstore.Key `json:"object"`
	NamedBy string       `json:"named_by"`
}

// ErrObjectsUnreachable is a copy that names objects this backup could not
// read and whose absence is not definite — the object store did not answer
// for them. The backup is refused rather than written without them: they may
// well exist, a later attempt may reach them, and a restore missing them
// would bring back files whose bytes the fleet still had.
var ErrObjectsUnreachable = errors.New("backup: the copy names objects the backup could not read")

// backupFetchConcurrency is how many objects a backup reads from the store at
// once, on a store whose objects it copies (`s3`).
//
// SIXTEEN. S3 documents at least 5,500 GET and HEAD requests a second per
// partitioned prefix, and every object the engine writes is under one
// (`files/`). A backup's GETs are whole objects, tens to hundreds of
// milliseconds each, so sixteen in flight are a few hundred requests a second —
// under a tenth of the prefix's rate, leaving the rest to the company's own
// downloads on the same prefix — while the bytes come sixteen streams wide,
// which is what a FIRST copy is bound by: a company of the design's large
// model holds about 1.15 TB of objects, which four streams carry in about 1.6
// hours on a link that can take it and sixteen in about 25 minutes (the
// design's arithmetic, not a measurement of this tree), and the trim hold
// covers that whole copy. A later backup takes what its predecessor holds from
// this host's disk ([reuseObject]) and reads only the objects written since.
//
// UNDER httpx.MaxIdleConnsPerHost (32), which a test holds, so every GET rides
// a connection the shared transport keeps warm rather than paying a TLS
// handshake of its own. Each worker streams one object through io.Copy's
// buffer rather than holding it: sixteen buffers, never sixteen objects.
//
// A CLOSED LOOP, like the collector's bounds: an object goes out only when one
// in flight has finished, so a store that slows down is read more slowly by
// itself, and the first object it does not answer for ends the copy with at
// most sixteen out — the backup is refused for that object anyway, and a store
// that stopped answering is not sent the rest of a terabyte.
const backupFetchConcurrency = 16

// streamCheckConcurrency is how many objects a backup asks the store about at
// once, on a store whose objects ride a stream snapshot (`nats`).
//
// THIRTY-TWO, EQUAL TO THE COLLECTOR'S AUDIT ON PURPOSE: the question is the
// same one — a stat, on `nats` one read from the bucket stream's leader — asked
// of the same objects, and the audit's bound (collect.auditConcurrency) is
// where its measurement is recorded: natsobj's benchmark answered 4,800–8,600
// stats a second one at a time on a member and 15,000–19,000 with thirty-two
// out, so two million objects are about two minutes rather than four to seven.
// Asked one at a time, as the backup used to, they were the latter end to end.
// A closed loop for the reason [backupFetchConcurrency] is.
const streamCheckConcurrency = 32

// reference is one object a backup owes: what it must be, the row naming it
// and that row's table.
type reference = objstore.Reference

// objectPass is one backup's objects: the ones the store copy's Required
// tables name, read from the copy a page at a time and carried (`s3`) or asked
// after (`nats`) as they are read — never gathered first.
//
// WHY STREAMED. A company of two million objects is two million references,
// and the backup used to hold every one twice — a map to de-duplicate them and
// a sorted slice to copy them from — half a gibibyte before the first object
// was read. The copy is now walked in ONE KEY ORDER across every Required
// table ([objstore.MergeWalks]), holding a page of each: a key two rows name
// comes out twice in a row, so de-duplicating it is a comparison with the one
// before, and a key named as two different objects is caught at the same
// comparison.
type objectPass struct {
	objs     *Objects
	required []objstore.ReferenceTable
	// copyPath is the replicated copy the references are read from, opened
	// for each walk and quiesced only once every walk is done.
	copyPath string
	// root is <dir>/objects, where a copied object is written (`s3`), and
	// prev the previous backup's objects directory on this host, or empty.
	root, prev string
	// pageRead is called after each read of the copy — nil but in a test,
	// which counts them to see that the copy is walked a page at a time.
	pageRead func()

	// mu guards art, written by up to a bound's visits at once.
	mu sync.Mutex
	// art is the artefact, nil until the first object is owed — a copy
	// naming none carries no objects section at all.
	art *ObjectArtifact
}

// newObjectPass is dir's objects, read from the replicated copy at copyPath.
//
// EVERY DECLARATION IS VALIDATED BEFORE ANY IS LEFT OUT: sorted by
// `Standing == Required` alone, one stating no standing would be skipped as
// though it were retired — a live table missing from the backup with nothing
// failing, the default the standing exists to forbid. A RETIRED TABLE IS LEFT
// OUT — see [Objects] — and its walk refuses to be read, so a pass that forgot
// to leave it out fails rather than carrying it.
func (s *Service) newObjectPass(ctx context.Context, dir, copyPath string) (*objectPass, error) {
	p := &objectPass{objs: s.objects, copyPath: copyPath,
		root: filepath.Join(dir, objectsDirName), pageRead: s.pageRead}
	for _, t := range s.tables {
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		// AFTER Validate, which has refused a standing that is neither.
		if t.Standing == objstore.Required {
			p.required = append(p.required, t)
		}
	}
	if len(p.required) > 0 && !p.inStream() {
		p.prev = s.previousObjects(ctx, dir)
	}
	return p, nil
}

// inStream reports whether the objects ride a stream the backup snapshots.
func (p *objectPass) inStream() bool { return p.objs != nil && p.objs.Stream != "" }

// artifact is the pass's artefact, made on the first object owed. The caller
// holds mu.
func (p *objectPass) artifact() *ObjectArtifact {
	if p.art == nil {
		if p.inStream() {
			p.art = &ObjectArtifact{Stream: p.objs.Stream}
		} else {
			p.art = &ObjectArtifact{Dir: objectsDirName, ReusedFrom: p.prev}
		}
	}
	return p.art
}

// carryCopy copies every object the copy names into the backup, up to
// [backupFetchConcurrency] at once, taking each from the previous backup on
// this host where it is there and intact and reading the rest from the store.
//
// Where the objects ride a stream the backup snapshots anyway it does nothing:
// the snapshot is the copy, and [objectPass.finish] asks after each object
// once it is taken.
func (p *objectPass) carryCopy(ctx context.Context) error {
	if len(p.required) == 0 || p.inStream() {
		return nil
	}
	return p.walkCopy(ctx, backupFetchConcurrency, func(ctx context.Context, ref reference) error {
		if p.objs == nil || p.objs.Open == nil {
			return fmt.Errorf("%w: the copy names objects — %s (%s) the first of them — and "+
				"this node runs no object store", ErrObjectsUnreachable, ref.NamedBy, ref.Object.Key)
		}
		reused, err := placeObject(ctx, p.root, p.prev, ref.Object, p.objs)
		p.mu.Lock()
		defer p.mu.Unlock()
		art := p.artifact()
		switch {
		case err == nil:
			art.Objects++
			art.Bytes += ref.Object.Size
			if reused {
				art.Reused++
			}
			return nil
		case errors.Is(err, objstore.ErrNotFound), errors.Is(err, objstore.ErrCorrupt):
			art.Lost = append(art.Lost, LostObject{Object: ref.Object.Key, NamedBy: ref.NamedBy})
			return nil
		case errors.Is(err, errDestination):
			// THE DESTINATION, not the store: every other object would
			// fail the same way, so the backup stops here.
			return err
		}
		return unanswered(ref, err)
	})
}

// finish completes the pass once the streams are snapshotted and answers its
// artefact — nil when the copy names no object.
//
// On a store whose objects ride a stream, it asks the store about every object
// the copy names, up to [streamCheckConcurrency] at once, and records as lost
// what the store answers it does not hold — or holds as other bytes than the
// row records, by the size and the digest the backend keeps, or as no object
// it can read at all ([objstore.ErrCorrupt]).
//
// AFTER THE SNAPSHOT, and that order is what makes a present answer mean the
// snapshot holds it: the copy is taken before the streams, and the collector
// may delete an object a removed row named in between — but a key is never
// reused, so an object the store holds after the snapshot finished is one it
// held throughout, snapshot included. Asked before, a deletion landing between
// the question and the snapshot would be carried as present and restore as a
// file with no bytes.
func (p *objectPass) finish(ctx context.Context) (*ObjectArtifact, error) {
	if len(p.required) > 0 && p.inStream() {
		if err := p.walkCopy(ctx, streamCheckConcurrency, p.askAfter); err != nil {
			return nil, err
		}
	}
	if p.art != nil {
		sortLost(p.art.Lost)
	}
	return p.art, nil
}

// askAfter asks the store whether it holds one object of a stream backup —
// see [objectPass.finish].
func (p *objectPass) askAfter(ctx context.Context, ref reference) error {
	if p.objs.Stat == nil {
		return fmt.Errorf("%w: the copy names objects in %s — %s (%s) the first of "+
			"them — and this node cannot ask the store about them",
			ErrObjectsUnreachable, p.objs.Stream, ref.NamedBy, ref.Object.Key)
	}
	info, err := p.objs.Stat(ctx, ref.Object.Key)
	p.mu.Lock()
	defer p.mu.Unlock()
	art := p.artifact()
	switch {
	case errors.Is(err, objstore.ErrNotFound), errors.Is(err, objstore.ErrCorrupt),
		err == nil && (info.Size != ref.Object.Size ||
			(info.Digest != "" && info.Digest != ref.Object.Hash)):
		art.Lost = append(art.Lost, LostObject{Object: ref.Object.Key, NamedBy: ref.NamedBy})
		return nil
	case err != nil:
		return unanswered(ref, err)
	}
	art.Objects++
	art.Bytes += ref.Object.Size
	return nil
}

// walkCopy hands every object the copy's Required tables name to visit, once
// each, with up to limit visits running at once.
//
// THE COPY IS READ ON THIS GOROUTINE ALONE, a page at a time, and a page is
// read only once the visits have caught up with the one before: a hand-off
// waits for a free slot. A VISIT THAT FAILS ENDS THE WALK — its context ends
// the others' and the walk reads no further — and walkCopy returns only once
// every visit it started has ended, answering the first failure: nothing a
// visit started outlives the pass.
func (p *objectPass) walkCopy(ctx context.Context, limit int,
	visit func(context.Context, reference) error) error {

	db, err := store.OpenEstate(ctx, store.EstateReplicated, p.copyPath, store.Options{})
	if err != nil {
		return fmt.Errorf("backup: open the copy to read the objects it names: %w", err)
	}
	defer func() { _ = db.Close() }()
	read := func(ctx context.Context, fn func(*sql.Tx) error) (bool, error) {
		// COMPLETE, always: a copy is a file, and no record it could
		// not apply is pending against it.
		rerr := db.Read(ctx, fn)
		if p.pageRead != nil {
			p.pageRead()
		}
		return true, rerr
	}
	walks := make([]objstore.NamedWalk, 0, len(p.required))
	for _, t := range p.required {
		walks = append(walks, objstore.NamedWalk{Name: t.Table, Walk: t.WalkOf(read)})
	}
	m := objstore.MergeWalks(walks...)

	visits, vctx := errgroup.WithContext(ctx)
	visits.SetLimit(limit)
	var (
		walkErr error
		prior   reference
		begun   bool
	)
	for vctx.Err() == nil {
		ref, ok, nerr := m.Next(vctx)
		if nerr != nil {
			walkErr = fmt.Errorf("backup: %w", nerr)
			break
		}
		if !ok {
			break
		}
		// THE MERGE HANDS A KEY TWO ROWS NAME ON ADJACENT, so the one
		// before is the only one it can repeat.
		if begun && ref.Object.Key == prior.Object.Key {
			if ref.Object != prior.Object {
				walkErr = contradiction(prior, ref)
				break
			}
			continue
		}
		prior, begun = ref, true
		visits.Go(func() error { return visit(vctx, ref) })
	}
	// ON EVERY PATH, so no visit outlives the pass. A visit that failed is
	// the root of a walk that stopped for it, so it is the one reported —
	// unless the backup itself was stopped, whose visits all fail for that
	// reason and none of them for the store's.
	err = visits.Wait()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return err
	}
	return walkErr
}

// contradiction refuses a key two references name as two different objects.
//
// A KEY IS MINTED FOR ONE UPLOAD, so two rows giving it two digests or two
// sizes are a backup that contradicts itself, and one that verified the object
// against either would be certifying the other wrong.
func contradiction(a, b reference) error {
	return fmt.Errorf("backup: object %s is named as %s (%d bytes) by %s and as %s "+
		"(%d bytes) by %s — one key is one upload's bytes", a.Object.Key,
		a.Object.Hash, a.Object.Size, a.NamedBy, b.Object.Hash, b.Object.Size, b.NamedBy)
}

// unanswered is the refusal for an object the store did not answer for.
//
// THE FIRST ENDS THE PASS: the backup is refused for it whatever happens to
// the rest — it may well be intact in a store that did not answer, so a
// manifest without it would be a restore missing a file the fleet still has —
// and a store that stopped answering is not sent the rest of the inventory.
func unanswered(ref reference, err error) error {
	return fmt.Errorf("%w: the object store did not answer for %s (%s): %v — the "+
		"backup stopped asking; take it again once it answers",
		ErrObjectsUnreachable, ref.NamedBy, ref.Object.Key, err)
}

func sortLost(lost []LostObject) {
	sort.Slice(lost, func(i, j int) bool {
		return lost[i].Object.String() < lost[j].Object.String()
	})
}

// errDestination is a failure writing the backup itself rather than reading an
// object — the directory, not the store.
var errDestination = errors.New("backup: the destination")

// placeObject puts one object into the backup: from prev when it holds an
// intact copy, else from the object store. It answers whether it was reused.
func placeObject(ctx context.Context, root, prev string, o objstore.Object,
	objs *Objects) (bool, error) {
	if prev != "" {
		if ok, err := reuseObject(ctx, root, prev, o); err != nil || ok {
			return ok, err
		}
	}
	body, err := objs.Open(ctx, o)
	if err != nil {
		return false, err
	}
	defer func() { _ = body.Close() }()
	return false, writeObject(root, o, body)
}

// reuseObject takes one object from the previous backup's directory, reporting
// false — and leaving nothing behind — when that copy is absent or is not the
// object its row records, so the caller reads it from the store.
//
// LINKED WHERE IT CAN BE, because an object never changes once written and a
// second name for the same file costs no disk at all; COPIED where the link is
// refused, a previous backup on another filesystem among them. Either way the
// bytes are READ BACK against the row before they count: a copy that rotted in
// the earlier artefact would otherwise be carried into every later one, and
// reading a local file is still a fraction of reading it from the store.
func reuseObject(ctx context.Context, root, prev string, o objstore.Object) (bool, error) {
	src := filepath.Join(prev, filepath.FromSlash(o.Key.Name()))
	dst := filepath.Join(root, filepath.FromSlash(o.Key.Name()))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return false, fmt.Errorf("%w: create %s: %w", errDestination, filepath.Dir(dst), err)
	}
	if linkErr := os.Link(src, dst); linkErr != nil {
		if errors.Is(linkErr, os.ErrNotExist) || !intactAt(ctx, src, o) {
			return false, nil
		}
		f, err := os.Open(src)
		if err != nil {
			return false, nil //nolint:nilerr // A copy that will not open is no copy: it is read from the store instead.
		}
		defer func() { _ = f.Close() }()
		// CHECKED AGAIN AS IT IS COPIED: the file was intact a moment
		// ago, and a copy is the one that counts. A copy that fails the
		// check is read from the store instead; only the destination's
		// own failure stops the backup.
		err = writeObject(root, o, &checked{r: f, o: o, digest: sha256.New()})
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, errDestination):
			return false, err
		}
		return false, nil
	}
	if !intactAt(ctx, dst, o) {
		// THE LINK GOES, never the earlier artefact's file: that one is
		// somebody's backup, and judging it is not this backup's place.
		if err := os.Remove(dst); err != nil {
			return false, fmt.Errorf("%w: remove %s: %w", errDestination, dst, err)
		}
		return false, nil
	}
	return true, nil
}

// intactAt reports whether the file at path is o's bytes, hashing it as it is
// read rather than holding it. A file that will not read is no copy, for the
// reason one that is not o is none: either way the object is read from the
// store instead.
func intactAt(ctx context.Context, path string, o objstore.Object) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	c := &checked{r: f, o: o, digest: sha256.New()}
	_, err = io.Copy(io.Discard, contextReader{ctx: ctx, r: c})
	return err == nil
}

// checked is a local copy of an object read back against its row: it ends in
// io.EOF only where exactly the row's size arrived and hashes to its digest.
type checked struct {
	r      io.Reader
	o      objstore.Object
	digest hash.Hash
	n      int64
}

func (c *checked) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	_, _ = c.digest.Write(p[:n])
	switch {
	case c.n > c.o.Size:
		return n, fmt.Errorf("%w: %s holds more than %d bytes", objstore.ErrCorrupt, c.o.Key, c.o.Size)
	case err == io.EOF && c.n != c.o.Size:
		return n, fmt.Errorf("%w: %s holds %d of %d bytes", objstore.ErrCorrupt, c.o.Key, c.n, c.o.Size)
	case err == io.EOF && objstore.Hash(hex.EncodeToString(c.digest.Sum(nil))) != c.o.Hash:
		return n, fmt.Errorf("%w: %s does not hash to its row's digest", objstore.ErrCorrupt, c.o.Key)
	}
	return n, err
}

// contextReader ends a read once ctx is done, so a backup that was cancelled
// stops hashing a gibibyte it no longer needs.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// writeObject streams one object into the backup under its own name, synced:
// written to a temporary name first and renamed once whole, so a file under
// an object's name is always the whole object. A read that fails leaves
// nothing behind and is answered as the read's own error; every failure of
// the backup's own disk is the destination's ([errDestination]).
func writeObject(root string, o objstore.Object, body io.Reader) error {
	path := filepath.Join(root, filepath.FromSlash(o.Key.Name()))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%w: create %s: %w", errDestination, filepath.Dir(path), err)
	}
	partial := path + ".partial"
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("%w: write object %s: %w", errDestination, o.Key, err)
	}
	written, err := io.Copy(destWriter{f}, body)
	if err == nil && written != o.Size {
		err = fmt.Errorf("%w: %s read as %d of %d bytes", objstore.ErrCorrupt, o.Key, written, o.Size)
	}
	if err == nil {
		if serr := f.Sync(); serr != nil {
			err = fmt.Errorf("%w: sync object %s: %w", errDestination, o.Key, serr)
		}
	}
	if cerr := f.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("%w: close object %s: %w", errDestination, o.Key, cerr)
	}
	if err == nil {
		if rerr := os.Rename(partial, path); rerr != nil {
			err = fmt.Errorf("%w: name object %s: %w", errDestination, o.Key, rerr)
		}
	}
	if err != nil {
		_ = os.Remove(partial)
		return err
	}
	return nil
}

// destWriter tags a failure writing the backup's own file as the
// destination's, so io.Copy's error says which side failed.
type destWriter struct{ f *os.File }

func (d destWriter) Write(p []byte) (int, error) {
	n, err := d.f.Write(p)
	if err != nil {
		return n, fmt.Errorf("%w: write %s: %w", errDestination, d.f.Name(), err)
	}
	return n, nil
}

// objectBytes is the size of every object the artefact holds beside its
// stream snapshots, or 0 — none where the objects ride a stream, whose
// snapshot's size already counts them.
func objectBytes(m Manifest) int64 {
	if m.Objects == nil || m.Objects.Stream != "" {
		return 0
	}
	return m.Objects.Bytes
}
