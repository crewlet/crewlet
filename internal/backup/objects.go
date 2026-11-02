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
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// objectsDirName holds the objects a backup carries, each under the name the
// store keeps it under ([objstore.Key.Name]) — the layout an S3 backend's
// bucket has under its prefix, so restoring them is one sync of the directory
// into the bucket (docs/guides/backup.md).
const objectsDirName = "objects"

// Objects is how a backup reaches the company's files' bytes.
//
// # Why a backup carries every object a restore's rows name
//
// A tracker restored without the bytes of its files names files nobody can
// open, and the collector deletes a removed file's object at its next pass
// once the object is more than a day old — within the hour of the removal for
// any file older than that — so a restore from a week-old backup brings back
// rows whose objects a live store may no longer hold. The backup is a copy of
// the COMPANY, so it carries every object a REQUIRED table of the store copy
// names — and every object a record the restore replays from the copy's
// position names, since those records write rows too (the package doc's "The
// objects follow the log").
//
// A RETIRED REFERENCE IS NOT CARRIED, NOR IS A DERIVED ONE (ADR-0074). A
// retired row keeps a replaced object from the collector for a grace, and a
// derived row an object the engine can build again; neither names anything
// the company is owed, so its object is never copied, never asked after and
// never recorded lost: on S3 it stays out of objects/, and on the broker's
// bucket the stream snapshot holds whatever the bucket holds without this
// package counting it. A restore brings the row back without its object,
// which nothing owes it: a retired row's domain deletes it once its grace has
// passed, and a derived row's domain builds the object again.
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
	// copy's Required rows or a replayed record names that the artefact
	// holds, a reused one included, since shipping the directory ships it.
	// On a stream, every such object the store was found holding once the
	// snapshot was taken, whose bytes that stream's snapshot already
	// counts.
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`

	// Replayed is how many of Objects no row of the copy names: each named
	// by a record a restore replays from the copy's position — a file
	// written while the backup ran, whose row the restored log writes.
	Replayed int `json:"replayed,omitempty"`

	// Reused is how many of them were taken from this node's previous
	// backup on the same host (ReusedFrom) rather than read from the
	// store: a key is minted for one upload and never names other bytes,
	// so one an earlier backup already holds is the same object — linked
	// where the filesystem allows and copied where it does not, and read
	// back against its row either way. Each is a complete file of this
	// artefact, never a reference into the other one.
	Reused     int    `json:"reused,omitempty"`
	ReusedFrom string `json:"reused_from,omitempty"`

	// Lost is every object the restored rows name that the object store
	// answered it does not hold, or holds only as bytes that are not the
	// ones the row records — each with the file that named it, which is what
	// a person restores. ONLY WHERE THE RESTORED ROWS NAME IT: an object the
	// copy names whose file a record past the copy replaced or removed — and
	// the collector has since deleted — is owed by nothing a restore brings
	// back, and is not listed. One residual: a record a restore's applier
	// reads past (an evicted writer's) that rewrote a file is read here as
	// having rewritten it, so an object the store lost whose file such a
	// record touched reads as replaced rather than lost — it needs an
	// eviction and a store loss on one file inside one backup, and the
	// collector's daily audit reports it either way.
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

// ErrObjectsUnreachable is a backup owing objects it could not read and whose
// absence is not definite — the object store did not answer for them. The
// backup is refused rather than written without them: they may well exist, a
// later attempt may reach them, and a restore missing them would bring back
// files whose bytes the fleet still had.
var ErrObjectsUnreachable = errors.New("backup: the backup owes objects it could not read")

// backupFetchConcurrency is how many objects a backup reads from the store at
// once, on a store whose objects it copies (`s3`).
//
// THIRTY-TWO, because a first copy is bound by REQUEST LATENCY, not by how many
// streams are open (Little's law: what is in flight is the rate times the time
// each request takes). The design's large model holds about 2.1 million
// objects in about 1.15 TB — a mean near 550 KB — and a GET of one from S3
// spends about 100 to 200 ms before its first byte (AWS's performance
// guidelines). Filling a 1 Gbit/s link at that mean is about 230 GETs a
// second, each holding its slot for its first byte and its body — about 130
// ms at a 100 ms first byte — so about thirty in flight: sixteen leave the
// link a third idle and a first copy most of twice as long as it need be.
// BenchmarkCarry measures exactly this at the first byte's lower end, over the
// S3 backend and the model's mix of sizes with every request answered a first
// byte late (two runs on a four-core host): at 100 ms sixteen carried 107 to
// 117 objects a second (59–65 MB/s; the model's first copy about five hours)
// and thirty-two 193 to 199 (107–111 MB/s — a 1 Gbit/s link's worth; about
// three hours). At its upper end a 550 KB object holds its slot about 230 ms,
// so thirty-two carry about 140 objects a second, three fifths of a 1 Gbit/s
// link, and the model's first copy takes about four hours (arithmetic, not a
// measurement). At 20 ms, under any first byte S3 answers in, both reached
// what the host's own writing, verifying and syncing of each object allows,
// 200 to 290 a second. A later backup takes what its predecessor holds from
// this host's disk ([reuseObject]) and reads only the objects written since.
//
// NOT MORE, although sixty-four carried more again at 100 ms where the host
// had room past a 1 Gbit/s link (203 to 295 a second): thirty-two fill the
// link a modest host has — the one the CLI's wait for a first copy is sized
// on — and past it the link, not the bound, sets how long the copy and the
// trim hold it spans take. A faster link to a slower bucket is where a deeper
// bound would pay, and it would take the warm-connection bound below with it.
//
// UNDER THE PREFIX'S RATE: S3 serves at least 5,500 GET and HEAD requests a
// second per partitioned prefix, and every object the engine writes is under
// one (`files/`); thirty-two out are about 140 to 250 requests a second at the
// first bytes above — under a twentieth of it, the rest left to the company's
// own downloads, the git nodes' syncs and the collector on the same prefix.
//
// AT httpx.MaxIdleConnsPerHost (32), which a test holds, so every GET rides a
// connection the bucket's own transport keeps warm — s3obj's clone of httpx's,
// over HTTP/1.1 alone, which keeps httpx's pool of that many to a host —
// rather than paying a TLS handshake of its own; past it, a GET opens a
// connection the pool then closes.
// Each worker streams one object through io.Copy's buffer rather than holding
// it: thirty-two buffers, never thirty-two objects.
//
// A CLOSED LOOP, like the collector's bounds: an object goes out only when one
// in flight has finished, so a store that slows down is read more slowly by
// itself, and the first object it still does not answer for once it has been
// asked again ([fetchAttempts]) ends the copy with at most the bound out — the
// backup is refused for that object anyway, and a store that stopped answering
// is not sent the rest of a terabyte.
const backupFetchConcurrency = 32

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
// after (`nats`) as they are read — never gathered first — and the ones the
// records a restore replays past the copy name ([window]).
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
	// for each read and quiesced only once every read is done.
	copyPath string
	// root is <dir>/objects, where a copied object is written (`s3`), and
	// prev the previous backup's objects directory on this host, or empty.
	root, prev string
	// pageRead is called after each read of the copy — nil but in a test,
	// which counts them to see that the copy is walked a page at a time.
	pageRead func()
	// deferredRead is called after each read of the records the copy
	// deferred, with how many it answered — nil but in a test.
	deferredRead func(rows int)

	// verifiers opens each declaring log's signed frames, by domain
	// ([verifiers]), under the service's keyring.
	verifiers map[string]*statelog.Verifier

	// win is what the records past the copy name, read once the streams
	// are snapshotted ([objectPass.readWindow]) and only then.
	win window

	// mu guards everything below, written by up to a bound's visits at
	// once.
	mu sync.Mutex
	// art is the artefact, nil until the first object is owed — a backup
	// owing none carries no objects section at all.
	art *ObjectArtifact
	// unheld is the copy's objects the store answered it does not hold
	// (`s3`): read before the window is, so whether a restore names each is
	// judged once it has been ([objectPass.finish]). As many as the store
	// has lost — what the manifest lists anyway — never the inventory.
	unheld map[objstore.Key]reference
	// removed is how many objects the copy names that the store does not
	// hold and no restored row names: a record past the copy replaced or
	// removed the file.
	removed int

	// retried is how many objects the store failed to answer for at least
	// once and then answered ([objectPass.retrying]) — logged when the pass
	// ends, so a store that is answering a backup only on its second try is
	// seen before it stops answering it at all.
	retried atomic.Int64
}

// newObjectPass is dir's objects, read from the replicated copy at copyPath.
//
// EVERY DECLARATION IS VALIDATED BEFORE ANY IS LEFT OUT: sorted by
// [objstore.PassBackup]'s question alone, one stating no standing would be
// skipped as though it were retired — a live table missing from the backup
// with nothing failing, the default the standing exists to forbid. A RETIRED
// OR DERIVED TABLE IS LEFT OUT — see [Objects] — and its walk, built for the
// backup's pass, refuses to be read, so a pass that forgot to leave it out
// fails rather than carrying it. And every
// Required one is held to say how its log names objects
// ([objstore.ReferenceTable.ValidateLog]) before a byte is read, rather than
// once the copy has been carried.
func (s *Service) newObjectPass(ctx context.Context, dir, copyPath string) (*objectPass, error) {
	p := &objectPass{objs: s.objects, copyPath: copyPath,
		root: filepath.Join(dir, objectsDirName), pageRead: s.pageRead,
		deferredRead: s.deferredRead, win: newWindow(), unheld: map[objstore.Key]reference{}}
	for _, t := range s.tables {
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		// AFTER Validate, which has refused a standing that is none of the
		// three: only a Required table's objects are the company's to carry.
		if objstore.PassBackup.Reads(t.Standing) {
			p.required = append(p.required, t)
		}
	}
	for _, t := range p.required {
		if err := t.ValidateLog(); err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
	}
	// EVERY DECLARING LOG'S VERIFIER BEFORE A BYTE IS READ, for the reason
	// each declaration is held to name its log here: a keyring that cannot
	// open one is found now, not once the copy has been carried.
	var err error
	if p.verifiers, err = verifiers(p.required, s.ring); err != nil {
		return nil, fmt.Errorf("backup: open the declaring logs' records: %w", err)
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
// BEFORE THE STREAMS ARE SNAPSHOTTED, which is what makes an object the store
// no longer holds judgeable: the collector deletes an object only once a
// complete read of its estate finds no declared table naming it, so an object
// of the copy it has deleted was un-named by a record its estate had applied
// before the deletion — before this read, and so before the snapshot began. The
// record lies in (P, L], which the window reads, and says whether a restore
// names the object ([objectPass.finish]). Not because the object went unnamed
// for any length of time: the collector's grace runs from the upload, so an
// old object goes within the hour of the record that un-names it.
//
// Where the objects ride a stream the backup snapshots anyway it does nothing:
// the snapshot is the copy, and [objectPass.finish] asks after each object
// once it is taken.
func (p *objectPass) carryCopy(ctx context.Context) error {
	if len(p.required) == 0 || p.inStream() {
		return nil
	}
	return p.walkCopy(ctx, backupFetchConcurrency, func(ctx context.Context, ref reference) error {
		return p.place(ctx, ref, false)
	})
}

// place carries one object into the backup (`s3`): counted when it lands —
// as replayed where only a record past the copy names it — and, when the store
// answers it does not hold it, set aside (an object of the copy, judged once
// the window is read) or judged at once (an object of the window, which is
// read by then).
func (p *objectPass) place(ctx context.Context, ref reference, replayed bool) error {
	if p.objs == nil || p.objs.Open == nil {
		return fmt.Errorf("%w: the backup owes objects — %s (%s) the first of them — and "+
			"this node runs no object store", ErrObjectsUnreachable, ref.NamedBy, ref.Object.Key)
	}
	reused, err := p.carry(ctx, ref)
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
		if replayed {
			art.Replayed++
		}
		return nil
	case errors.Is(err, objstore.ErrNotFound), errors.Is(err, objstore.ErrCorrupt):
		if replayed {
			p.judgeReplayedLocked(ref)
		} else {
			p.unheld[ref.Object.Key] = ref
		}
		return nil
	case errors.Is(err, errDestination):
		// THE DESTINATION, not the store: every other object would fail
		// the same way, so the backup stops here.
		return err
	}
	return unanswered(ref, err)
}

// judgeCopiedLocked records an object of the copy the store does not hold:
// LOST where a restore names it, and otherwise removed — a record past the copy
// replaced or removed its file, and nothing restored names it. The caller
// holds mu.
//
// EXACT ON `s3`, where the copy's objects are read before the snapshot: one
// the collector had deleted was un-named before the snapshot began, by a record
// the window holds. On `nats` the question comes after the snapshot, so an
// object the collector deleted between the object stream's snapshot and the
// question is the one case judged lost although the snapshot holds it — the
// conservative direction.
func (p *objectPass) judgeCopiedLocked(ref reference) {
	if p.win.owesCopied(ref) {
		p.artifact().Lost = append(p.artifact().Lost, LostObject{Object: ref.Object.Key, NamedBy: ref.NamedBy})
		return
	}
	p.removed++
}

// judgeReplayedLocked records an object only the window names that the store
// does not hold: LOST where the row's last record leaves the restore naming it
// — a young object, which the collector never takes, so a store fault — and
// nothing where a later record replaced it again. The caller holds mu.
func (p *objectPass) judgeReplayedLocked(ref reference) {
	if p.win.owesReplayed(ref) {
		p.artifact().Lost = append(p.artifact().Lost, LostObject{Object: ref.Object.Key, NamedBy: ref.NamedBy})
	}
}

// finish completes the pass once the streams are snapshotted and the window
// read, and answers its artefact — nil when the backup owes no object.
//
// ON `s3` it judges the copy's objects the store did not hold, then carries
// every object only the window names, up to [backupFetchConcurrency] at once.
//
// ON `nats` it asks the store about every object the copy names and then every
// object only the window names, up to [streamCheckConcurrency] at once, and
// counts what the store holds; what it answers it does not hold — or holds as
// other bytes than the row records, by the size and the digest the backend
// keeps, or as no object it can read at all ([objstore.ErrCorrupt]) — is judged
// as on `s3`. AFTER THE SNAPSHOT, and that order is what makes a present answer
// mean the snapshot holds it: a key is never reused, so an object the store
// holds after the snapshot finished is one it held throughout, snapshot
// included. Asked before, a deletion landing between the question and the
// snapshot would be carried as present and restore as a file with no bytes.
// And the object stream is snapshotted after every log ([snapshotStreams]), so
// an object a record of a log's snapshot names was in the store — it was
// uploaded before its record was written — before the object stream was
// snapshotted.
func (p *objectPass) finish(ctx context.Context) (*ObjectArtifact, error) {
	if len(p.required) == 0 {
		return nil, nil
	}
	if p.inStream() {
		if err := p.walkCopy(ctx, streamCheckConcurrency, p.askCopied); err != nil {
			return nil, err
		}
	} else {
		p.mu.Lock()
		for _, k := range sortedKeys(p.unheld) {
			p.judgeCopiedLocked(p.unheld[k])
		}
		p.mu.Unlock()
	}
	keys := p.windowKeys()
	copied, err := p.copyNames(ctx, keys)
	if err != nil {
		return nil, err
	}
	if p.inStream() {
		err = bounded(ctx, streamCheckConcurrency, p.windowOnly(keys, copied), p.askReplayed)
	} else {
		err = bounded(ctx, backupFetchConcurrency, p.windowOnly(keys, copied),
			func(ctx context.Context, ref reference) error { return p.place(ctx, ref, true) })
	}
	if err != nil {
		return nil, err
	}
	if p.art != nil {
		sortLost(p.art.Lost)
	}
	return p.art, nil
}

// askCopied asks the store whether it holds one object the copy names, on a
// stream backup — see [objectPass.finish].
func (p *objectPass) askCopied(ctx context.Context, ref reference) error {
	held, err := p.ask(ctx, ref)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !held {
		p.judgeCopiedLocked(ref)
		return nil
	}
	art := p.artifact()
	art.Objects++
	art.Bytes += ref.Object.Size
	return nil
}

// askReplayed asks the store whether it holds one object only the window
// names, on a stream backup.
func (p *objectPass) askReplayed(ctx context.Context, ref reference) error {
	held, err := p.ask(ctx, ref)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !held {
		p.judgeReplayedLocked(ref)
		return nil
	}
	art := p.artifact()
	art.Objects++
	art.Bytes += ref.Object.Size
	art.Replayed++
	return nil
}

// ask is the store's answer about one object of a stream backup: held as the
// row records it, or definitely not — or the refusal of a store that did not
// answer.
func (p *objectPass) ask(ctx context.Context, ref reference) (bool, error) {
	if p.objs.Stat == nil {
		return false, fmt.Errorf("%w: the backup owes objects in %s — %s (%s) the first of "+
			"them — and this node cannot ask the store about them",
			ErrObjectsUnreachable, p.objs.Stream, ref.NamedBy, ref.Object.Key)
	}
	var info objstore.Info
	err := p.retrying(ctx, objstore.ReadBase, func(ctx context.Context) error {
		var serr error
		info, serr = p.objs.Stat(ctx, ref.Object.Key)
		return serr
	})
	switch {
	case errors.Is(err, objstore.ErrNotFound), errors.Is(err, objstore.ErrCorrupt):
		return false, nil
	case err != nil:
		return false, unanswered(ref, err)
	}
	return info.Size == ref.Object.Size && (info.Digest == "" || info.Digest == ref.Object.Hash), nil
}

// windowKeys is every object the window names, in key order — the order a
// store lists in and a refusal names the same object by every time.
func (p *objectPass) windowKeys() []objstore.Key { return sortedKeys(p.win.objects) }

// windowOnly yields the window's objects at keys the copy does not name, in
// key order; copied is the ones it does ([objectPass.copyNames]), whose object
// the copy's pass has carried or judged already — and a key the copy names as
// another object than the window does is refused, as two rows disagreeing are.
//
// NO I/O, because [bounded] calls it on its one feeding goroutine: what the
// copy names was looked up before the hand-off began, so a large object is
// never re-read on the goroutine every slot waits on.
func (p *objectPass) windowOnly(keys []objstore.Key,
	copied map[objstore.Key]reference) func(context.Context) (reference, bool, error) {

	next := 0
	return func(context.Context) (reference, bool, error) {
		for next < len(keys) {
			ref := p.win.objects[keys[next]]
			next++
			if c, also := copied[ref.Object.Key]; also {
				if c.Object != ref.Object {
					return reference{}, false, contradiction(c, ref)
				}
				continue
			}
			return ref, true, nil
		}
		return reference{}, false, nil
	}
}

// copyNames answers which of keys — the window's — the copy's Required tables
// name, and as what: a batch of [objstore.WalkPage] keys a statement, each a
// seek of the key column's index.
//
// ASKED OF THE COPY, never of the bytes the backup carried: whether a window
// object is one the copy's pass already handled, and whether the two name its
// key as one object, are both questions about the copy's ROWS. Answered by
// re-reading the carried file instead, they cost a read and a digest of every
// such object — a gibibyte apiece at the tracker's largest file — on the one
// goroutine handing objects to the store's readers.
//
// BOUNDED BY THE WINDOW, which is bounded by how much was written while the
// backup ran, never by the inventory.
func (p *objectPass) copyNames(ctx context.Context, keys []objstore.Key) (map[objstore.Key]reference, error) {
	copied := map[objstore.Key]reference{}
	if len(keys) == 0 {
		return copied, nil
	}
	db, err := store.OpenEstate(ctx, store.EstateReplicated, p.copyPath, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("backup: open the copy to ask which objects it names: %w", err)
	}
	defer func() { _ = db.Close() }()
	for _, t := range p.required {
		for from := 0; from < len(keys); from += objstore.WalkPage {
			batch := keys[from:min(from+objstore.WalkPage, len(keys))]
			query, err := t.ReferencesAmong(len(batch))
			if err != nil {
				return nil, fmt.Errorf("backup: %w", err)
			}
			args := make([]any, len(batch))
			for i, k := range batch {
				args[i] = k.String()
			}
			var found []reference
			if err := db.Read(ctx, func(tx *sql.Tx) error {
				// RESET ON EVERY RUN, for the reason the walk's pages are.
				found = found[:0]
				rows, qerr := tx.QueryContext(ctx, query, args...)
				if qerr != nil {
					return qerr
				}
				defer func() { _ = rows.Close() }()
				return t.ScanReferences(rows, func(obj objstore.Object, namedBy string) error {
					found = append(found, reference{Object: obj, NamedBy: namedBy, Table: t.Table})
					return nil
				})
			}); err != nil {
				return nil, fmt.Errorf("backup: ask the copy which of the objects past it %s "+
					"names: %w", t.Table, err)
			}
			for _, ref := range found {
				copied[ref.Object.Key] = ref
			}
		}
	}
	return copied, nil
}

// walkCopy hands every object the copy's Required tables name to visit, once
// each, in key order, up to limit at once ([bounded]) — read a page at a time,
// a key two rows name de-duplicated, and refused where they name it as two
// objects. The copy is open for the walk and closed when it ends, however it
// ends.
func (p *objectPass) walkCopy(ctx context.Context, limit int,
	visit func(context.Context, reference) error) error {

	db, err := store.OpenEstate(ctx, store.EstateReplicated, p.copyPath, store.Options{})
	if err != nil {
		return fmt.Errorf("backup: open the copy to read the objects it names: %w", err)
	}
	defer func() { _ = db.Close() }()
	read := func(ctx context.Context, fn func(*sql.Tx) error) (bool, error) {
		// COMPLETE, always: a copy is a file, and no record it could not
		// apply is pending against it.
		rerr := db.Read(ctx, fn)
		if p.pageRead != nil {
			p.pageRead()
		}
		return true, rerr
	}
	walks := make([]objstore.NamedWalk, 0, len(p.required))
	for _, t := range p.required {
		walks = append(walks, objstore.NamedWalk{Name: t.Table,
			Walk: t.WalkOf(read, objstore.PassBackup)})
	}
	m := objstore.MergeWalks(walks...)
	var (
		prior reference
		begun bool
	)
	return bounded(ctx, limit, func(ctx context.Context) (reference, bool, error) {
		for {
			ref, ok, err := m.Next(ctx)
			switch {
			case err != nil:
				return reference{}, false, fmt.Errorf("backup: %w", err)
			case !ok:
				return reference{}, false, nil
			}
			// THE MERGE HANDS A KEY TWO ROWS NAME ON ADJACENT, so the
			// one before is the only one it can repeat.
			if begun && ref.Object.Key == prior.Object.Key {
				if ref.Object != prior.Object {
					return reference{}, false, contradiction(prior, ref)
				}
				continue
			}
			prior, begun = ref, true
			return ref, true, nil
		}
	}, visit)
}

// bounded hands every reference next yields to visit, with up to limit visits
// running at once.
//
// NEXT IS CALLED ON THIS GOROUTINE ALONE — for the copy, a page of it read at a
// time — and only once a visit has a free slot: a hand-off waits for one. A
// VISIT THAT FAILS ENDS THE PASS — its context ends the others' and nothing more
// is handed on — and bounded returns only once every visit it started has
// ended, answering the first failure: nothing a visit started outlives the
// pass.
func bounded(ctx context.Context, limit int, next func(context.Context) (reference, bool, error),
	visit func(context.Context, reference) error) error {

	visits, vctx := errgroup.WithContext(ctx)
	visits.SetLimit(limit)
	var nextErr error
	for vctx.Err() == nil {
		ref, ok, err := next(vctx)
		if err != nil {
			nextErr = err
			break
		}
		if !ok {
			break
		}
		visits.Go(func() error { return visit(vctx, ref) })
	}
	// ON EVERY PATH, so no visit outlives the pass. A visit that failed is
	// the root of a walk that stopped for it, so it is the one reported —
	// unless the backup itself was stopped, whose visits all fail for that
	// reason and none of them for the store's.
	err := visits.Wait()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return err
	}
	return nextErr
}

// sortedKeys is the keys of m in key order.
func sortedKeys(m map[objstore.Key]reference) []objstore.Key {
	out := make([]objstore.Key, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, objstore.Key.Compare)
	return out
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

// carry puts one object into the backup: from the previous backup when it
// holds an intact copy, else from the object store, whose read is retried
// ([objectPass.retrying]). It answers whether it was reused.
func (p *objectPass) carry(ctx context.Context, ref reference) (bool, error) {
	o := ref.Object
	if p.prev != "" {
		if ok, err := reuseObject(ctx, p.root, p.prev, o); err != nil || ok {
			return ok, err
		}
	}
	return false, p.retrying(ctx, objstore.ReadBudget(o.Size), func(ctx context.Context) error {
		body, err := p.objs.Open(ctx, o)
		if err != nil {
			return err
		}
		defer func() { _ = body.Close() }()
		return writeObject(p.root, o, body)
	})
}

// fetchAttempts is how many times a backup asks the store about one object —
// a read on `s3`, a stat on `nats` — before a failure that is not a definite
// answer refuses the backup ([unanswered]).
//
// FOUR, paced at [fetchRetryBase] doubling to [fetchRetryCeiling]: a second,
// two and four between them, seven in all with a fifth of each spread either
// way — longer than a broker election's five seconds, which is what a stat on
// the bucket stream fails across, and than the connection reset or the stall
// a GET from a bucket ends in now and then. What these attempts answer is a
// read that ENDED: the bucket's own client asks a request it was refused again
// until the request's deadline — which for a read is the store's stall on its
// open ([objstore.Stall], a minute) — and a stall or a reset part way
// through a body is final, so each attempt here is a read begun again, from
// the object's first byte. And it is worth retrying at a backup's scale where
// it would not be at a request's: a first copy is two million reads over
// hours, refused whole — with no manifest, and every object read again by the
// next attempt — for the first one that fails.
//
// NO LONGER THAN ONE BUDGET IN ALL: every attempt runs inside the object's
// own read budget ([objstore.ReadBudget]; a stat's is [objstore.ReadBase]), so
// a retry never lets one object hold its slot longer than a single read could.
// So against a bucket that keeps failing one object's GET — each attempt
// asked again by the client until the stall ends it — the four are fewer: a
// small object's two-minute budget holds two opens the stall ends, and the
// copy is refused about two minutes after that object's first GET. IN THE
// SAME SLOT, so the bound on what is out at once still holds; a store that
// has stopped answering ends the pass once the first object out has spent its
// budget.
const fetchAttempts = 4

// fetchRetryBase and fetchRetryCeiling pace [fetchAttempts] — see there.
const (
	fetchRetryBase    = time.Second
	fetchRetryCeiling = 5 * time.Second
)

// retrying runs try — one question about one object — until it answers, it
// fails definitely, or [fetchAttempts] are spent, all inside budget, and
// answers its last error. Definite is a store's own answer about the object
// ([objstore.ErrNotFound], [objstore.ErrCorrupt]) and a failure of the
// backup's own disk ([errDestination]), each of which asking again cannot
// change; so is the pass ending.
func (p *objectPass) retrying(ctx context.Context, budget time.Duration,
	try func(context.Context) error) error {

	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for attempt := 1; ; attempt++ {
		err := try(ctx)
		switch {
		case err == nil:
			if attempt > 1 {
				p.retried.Add(1)
			}
			return nil
		case errors.Is(err, objstore.ErrNotFound), errors.Is(err, objstore.ErrCorrupt),
			errors.Is(err, errDestination), attempt == fetchAttempts, ctx.Err() != nil:
			return err
		}
		wait := time.NewTimer(backoff.Jitter(
			backoff.Doubling(attempt, fetchRetryBase, fetchRetryCeiling), 0.2))
		select {
		case <-wait.C:
		case <-ctx.Done():
			wait.Stop()
			return err
		}
	}
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
