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

// backupFetchConcurrency is how many objects a backup reads at once.
//
// FOUR: a backup is background work sharing the object store with every
// upload and read the company's seats make, so it may overlap round trips but
// must not saturate the store. Each worker streams one object through a
// buffer of its own rather than holding it, so four is also four buffers,
// never four gibibytes.
const backupFetchConcurrency = 4

// referencePage is how many rows of a declared table one statement of the
// backup's walk reads: a page of the declaration's keyset
// ([objstore.ReferenceTable.ReferencesAfter]).
//
// FIVE HUNDRED, the page the collector's own walk reads: a row is a few
// hundred bytes, and a smaller page is more statements for nothing.
const referencePage = 500

// reference is one object a required table of the copy names: what it must
// be, and the file that names it.
type reference struct {
	object  objstore.Object
	namedBy string
}

// referencedIn is every object a REQUIRED table of the replicated copy at path
// names, in key order — read from the copy itself, so the objects a backup
// carries are the ones the rows it carries name.
//
// A RETIRED TABLE IS LEFT OUT before the copy is opened — see [Objects] — and
// [objstore.ReferenceTable.ReferencesAfter] refuses one, so a walk that forgot
// to leave it out fails rather than carrying it. EVERY declaration is
// validated before any is left out: sorted by `Standing == Required` alone, one
// stating no standing would be skipped as though it were retired — a live
// table missing from the backup with nothing failing, the default the
// standing exists to forbid — and no statement is ever built for a table that
// is skipped, so ReferencesAfter's own validation would never see it.
//
// A KEY NAMED AS TWO DIFFERENT OBJECTS refuses the backup: a key is minted for
// one upload, so two rows giving it two digests or two sizes are a copy that
// contradicts itself, and a backup that verified the object against either
// would be certifying the other wrong.
func referencedIn(ctx context.Context, path string, tables []objstore.ReferenceTable) ([]reference, error) {
	var required []objstore.ReferenceTable
	for _, t := range tables {
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		// AFTER Validate, which has refused a standing that is neither.
		if t.Standing == objstore.Required {
			required = append(required, t)
		}
	}
	if len(required) == 0 {
		return nil, nil
	}
	db, err := store.OpenEstate(ctx, store.EstateReplicated, path, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("backup: open the copy to read the objects it names: %w", err)
	}
	defer func() { _ = db.Close() }()
	seen := map[objstore.Key]reference{}
	for _, t := range required {
		query, err := t.ReferencesAfter(referencePage)
		if err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		for after := ""; ; {
			var page []reference
			if rerr := db.Read(ctx, func(tx *sql.Tx) error {
				var perr error
				page, perr = readReferences(ctx, tx, t, query, after)
				return perr
			}); rerr != nil {
				return nil, fmt.Errorf("backup: read the objects the copy names: %w", rerr)
			}
			for _, ref := range page {
				if prior, dup := seen[ref.object.Key]; dup && prior.object != ref.object {
					return nil, fmt.Errorf("backup: the copy names object %s as %s (%d bytes) "+
						"for %s and as %s (%d bytes) for %s — one key is one upload's bytes",
						ref.object.Key, prior.object.Hash, prior.object.Size, prior.namedBy,
						ref.object.Hash, ref.object.Size, ref.namedBy)
				}
				if _, dup := seen[ref.object.Key]; !dup {
					seen[ref.object.Key] = ref
				}
			}
			if len(page) < referencePage {
				break
			}
			after = page[len(page)-1].object.Key.String()
		}
	}
	out := make([]reference, 0, len(seen))
	for _, ref := range seen {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].object.Key.String() < out[j].object.Key.String()
	})
	return out, nil
}

// readReferences reads one page of t's references after the key after.
func readReferences(ctx context.Context, tx *sql.Tx, t objstore.ReferenceTable,
	query, after string) ([]reference, error) {

	rows, err := tx.QueryContext(ctx, query, after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var page []reference
	err = t.ScanReferences(rows, func(obj objstore.Object, namedBy string) error {
		page = append(page, reference{object: obj, namedBy: namedBy})
		return nil
	})
	return page, err
}

// copyObjects writes every object a required table of the copy names into the
// backup, taking each from prev — the object directory of this node's previous
// backup, empty when there is none — where it is there and intact, and reading
// the rest from the store. Where the objects live in a stream the backup
// snapshots anyway, it copies none and names the stream; [checkStreamObjects]
// then asks the store for each once the snapshot is taken.
func copyObjects(ctx context.Context, dir string, refs []reference, objs *Objects,
	prev string) (*ObjectArtifact, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if objs == nil || objs.Open == nil {
		return nil, fmt.Errorf("%w: %d objects, and this node runs no object store",
			ErrObjectsUnreachable, len(refs))
	}
	if objs.Stream != "" {
		return &ObjectArtifact{Stream: objs.Stream}, nil
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
	work := make(chan reference)
	var wg sync.WaitGroup
	for range min(backupFetchConcurrency, len(refs)) {
		wg.Go(func() {
			for ref := range work {
				reused, err := placeObject(ctx, root, prev, ref.object, objs)
				mu.Lock()
				switch {
				case err == nil:
					out.Objects++
					out.Bytes += ref.object.Size
					if reused {
						out.Reused++
					}
				case errors.Is(err, objstore.ErrNotFound), errors.Is(err, objstore.ErrCorrupt):
					out.Lost = append(out.Lost, LostObject{Object: ref.object.Key, NamedBy: ref.namedBy})
				case errors.Is(err, errDestination):
					// THE DESTINATION, not the store: every other object
					// would fail the same way, so the backup stops here.
					if failed == nil {
						failed = err
					}
					cancel()
				default:
					unreachable = append(unreachable, fmt.Sprintf("%s (%s): %v",
						ref.namedBy, ref.object.Key, err))
				}
				mu.Unlock()
			}
		})
	}
feed:
	for _, ref := range refs {
		select {
		case work <- ref:
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
	if err := unanswered(unreachable, len(refs)); err != nil {
		return nil, err
	}
	sortLost(out.Lost)
	return out, nil
}

// unanswered is the refusal for the objects the store did not answer for, or
// nil when it answered for every one.
func unanswered(unreachable []string, of int) error {
	if len(unreachable) == 0 {
		return nil
	}
	sort.Strings(unreachable)
	shown := unreachable
	if len(shown) > 5 {
		shown = append(shown[:5:5], fmt.Sprintf("and %d more", len(unreachable)-5))
	}
	return fmt.Errorf("%w: %d of %d (%v) — the object store did not answer for "+
		"them; take the backup again once it does", ErrObjectsUnreachable,
		len(unreachable), of, shown)
}

func sortLost(lost []LostObject) {
	sort.Slice(lost, func(i, j int) bool {
		return lost[i].Object.String() < lost[j].Object.String()
	})
}

// checkStreamObjects asks the store, once the stream snapshot is taken, for
// every object a required table of the copy names, and records as lost what it
// answers it does not hold — or holds as other bytes than the row records, by
// the size and the digest the backend keeps.
//
// AFTER THE SNAPSHOT, and that order is what makes a present answer mean the
// snapshot holds it: the copy is taken before the streams, and the collector
// may delete an object a removed row named in between — but a key is never
// reused, so an object the store holds after the snapshot finished is one it
// held throughout, snapshot included. Asked before, a deletion landing between
// the question and the snapshot would be carried as present and restore as a
// file with no bytes.
func checkStreamObjects(ctx context.Context, art *ObjectArtifact, refs []reference, objs *Objects) error {
	if art == nil || art.Stream == "" {
		return nil
	}
	if objs.Stat == nil {
		return fmt.Errorf("%w: %d objects in %s, and this node cannot ask the store about them",
			ErrObjectsUnreachable, len(refs), art.Stream)
	}
	var unreachable []string
	for _, ref := range refs {
		info, err := objs.Stat(ctx, ref.object.Key)
		switch {
		case errors.Is(err, objstore.ErrNotFound),
			err == nil && (info.Size != ref.object.Size ||
				(info.Digest != "" && info.Digest != ref.object.Hash)):
			art.Lost = append(art.Lost, LostObject{Object: ref.object.Key, NamedBy: ref.namedBy})
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			unreachable = append(unreachable, fmt.Sprintf("%s (%s): %v",
				ref.namedBy, ref.object.Key, err))
		default:
			art.Objects++
			art.Bytes += ref.object.Size
		}
	}
	if err := unanswered(unreachable, len(refs)); err != nil {
		return err
	}
	sortLost(art.Lost)
	return nil
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
