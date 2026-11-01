// Package collect deletes the object store's garbage and counts what it has
// lost: the one fleet duty the object store needs (ADR-0026).
//
// # Why there is so little here
//
// The bytes live in ONE backend the whole fleet shares — the broker's own
// object store, replicated as every stream is, or an S3-compatible bucket
// that keeps its own copies. So nothing here decides where an object lives,
// fetches a missing copy or reads a disk for rot: the backend does all of
// that. What no backend can know is which objects the company still REFERS
// to, because that is a fact about the replicated estate — so the two jobs
// left are the two that need it, run by one data node at a time.
//
// # The inventory is the estate
//
// No list of objects is kept beside the estate. Every row that refers to an
// object names its key, and the declared tables (internal/objstore/
// references) are the whole inventory ([Sources]). A list kept beside it
// would be a second answer to which objects exist, and the two would drift
// the first time a write landed in one and not the other.
//
// # Collection: deletion is the only dangerous thing, and it needs no lock (ADR-0027)
//
// What a name in the store MEANS is read here, never by a backend: a backend
// lists every name it holds, verbatim, and this package sorts each into one
// of two kinds.
//
//   - AN OBJECT'S KEY under the engine's namespace (objstore.KeyOfName) is
//     the collector's to judge, by the rule below.
//   - ANYTHING ELSE is left where it is, and never counted: another
//     application's object under a shared prefix, something an operator
//     put in the bucket.
//
// A key's object is deleted only when ALL of these hold:
//
//   - It was stored more than [PendingGrace] ago, by the backend's own
//     clock (objstore.Info.Written). Bytes are uploaded BEFORE the record
//     naming them is written, so every object is unnamed for a while at the
//     start of its life; the grace outlasts the slowest upload from its key
//     to its record.
//   - Its key was MINTED more than [PendingGrace] ago, by its own instant
//     (objstore.Key.Minted). This is the half that makes the rule safe: a
//     write naming a key is refused once the key is older than
//     objstore.RecordWithin, which is shorter than the grace by more than
//     any two nodes' clocks disagree — so a key past the grace is one no
//     write can name any more, however late a record arrives.
//   - No declared table names it in an estate that is CURRENT and COMPLETE.
//     Current: before it judges its first object, the pass waits for
//     everything each domain's log had committed by then, and so by the
//     moment it started ([Source.Barrier]). A pass with nothing past the
//     grace takes no barrier and writes nothing to any log. Complete: a
//     record this node could not decode might be the one naming the
//     object, so a pass that meets one stops judging. What it deleted
//     before is reported beside why it stopped.
//   - Nothing this node CANNOT SEE may name it ([Options.Unseen]). Every
//     declared table a build reads is its own build's declaration, so a
//     domain it has never heard of — a state log a newer build runs on
//     another data node during a rolling upgrade, or tables a newer build
//     migrated this node's own copy to before a rollback — may name any
//     object in the store, and no record of it is one this node met and
//     failed to decode. So the pass asks first, and judges nothing while
//     anything is unseen: this build is the older peer of the next one,
//     and the next one's objects are not this build's to delete.
//
// THE GRACE IS AN UPLOAD'S, NEVER A REPLACED VERSION'S. Both instants date
// the object's upload, and nothing records when a row stopped naming it: an
// object a file was rewritten off, or removed from, goes at the first pass
// after it stops being named once both are past the grace — within the hour
// for a file older than a day. The store is not where a replaced version is
// kept.
//
// NO LOCK, NO SECOND LOOK. A key is minted for one upload and named only by
// the write that uploaded it, so there is no writer re-using an object the
// collector could be deleting, and so nothing for a lock to guard. Two
// collectors at once are safe too: deleting an object that is gone is not an
// error.
//
// # The sweep: what no listing shows
//
// An upload that died part of the way through — a process killed mid-put, a
// delete interrupted between its halves — leaves bytes no name reaches: an S3
// multipart upload nobody completed or aborted, broker pieces no metadata
// names. A listing of the names never shows them, and each can be a gibibyte,
// so every collection also asks the backend for them (objstore.Backend's
// Pending) and abandons the ones begun more than [PendingGrace] ago — every
// upload still in flight is pending too, and none takes that long — that are
// named by no name or by a key also minted past the grace; never one under
// somebody else's name. It needs no estate: an upload that never finished is
// named by no row. A sweep the backend refuses — an S3 identity without the right to list or abort
// uploads — is reported beside the collection rather than failing it.
//
// # Audit: what the store has lost
//
// Every object the estate names should be in the backend, whole — a file's
// record is written only after its object is stored. The audit holds every
// reference against what the backend LISTS and finds two things: an object it
// does not hold (MISSING, asked about once more, by name, at the end of the
// pass before it is called so) and one it holds at another size, or under
// another digest where the backend keeps one (DAMAGED).
//
// A LISTING, NEVER A QUESTION PER OBJECT. One stat per reference was a
// request per file a day, which on a bucket that bills per request is the
// audit's whole cost, and on the broker a round trip to the stream's leader
// per file; a listing answers a thousand names a request on S3 and streams
// the broker's metadata in one consumer. It is read a WINDOW at a time
// ([auditWindow] keys, the smallest above the last window's), and the
// references in that window walked in key order beside it, so the audit holds
// a window of the inventory in memory and never the whole of it: a store of
// more objects than a window is listed once per window, still a small
// fraction of the requests a stat apiece cost. Both are files the company cannot read: the
// `objects_missing` alarm and the list `crewlet objects status` prints, each
// named as the file that holds it, since a person restores files rather
// than keys. A durable backend loses nothing, so a non-zero count is the
// backend failing at the one thing it is for.
//
// THE ESTATE IS READ A PAGE AT A TIME AND THE BACKEND IS ASKED BETWEEN PAGES,
// never inside a read: a read of the estate holds one of the node's few
// reader connections and the snapshot under it, and a listing or a question
// to the backend is a network round trip that connection must not wait on.
//
// WHAT AN AUDIT FOUND OUTLIVES THE AUDIT THAT FAILED AFTER IT, and the duty
// moving: the report keeps the last audit that ran to its end beside the
// last attempt ([AuditReport.Found]), and a node taking the duty picks both
// up from the fleet's record ([Collector.Restore]), so a broker that stopped
// answering mid-audit, or a holder that went away, never clears an alarm about
// files the store has lost.
package collect

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
)

var log = logging.Get("objstore")

// PendingGrace is how long an object nothing refers to is kept, measured
// both from when the backend stored it and from when its key was minted.
//
// A DAY. The grace has to outlast the longest a key can go unnamed and still
// be named: objstore.RecordWithin, twelve hours, after which a write naming it
// is refused — and the twelve hours left over are what the clocks of the node
// deciding a write and the node running the collector may disagree by before
// the rule stops holding, which is hours more than any clock an engine runs
// on is ever off. It costs nothing but a day's worth of abandoned uploads in
// the store. It is not what covers a node whose estate is behind — the pass's
// barrier is.
const PendingGrace = 24 * time.Hour

// CollectInterval is how often garbage is collected.
//
// AN HOUR: a pass waits on a barrier in every domain that refers to objects
// and lists the whole store, and the only cost of running it less often is
// garbage kept a little longer beside a grace that is already a day.
const CollectInterval = time.Hour

// AuditInterval is how often the store is asked for every object the estate
// names.
//
// A DAY: an audit lists the store once per window, and what it looks for is a
// backend losing acknowledged bytes (an event, not a drift), so a day is soon
// enough to page somebody and a fraction of the requests a tighter cadence
// would cost on a store that bills per request.
const AuditInterval = 24 * time.Hour

// auditWindow is how many listed objects one window of the audit holds in
// memory, matched against the references whose keys fall in it.
//
// A HUNDRED THOUSAND: about a hundred bytes an entry with the map and the heap
// around it, so ten megabytes or so, on a data node whose own estate is far
// larger; and the million objects the broker's bucket is sized for are ten
// listings, against the million stats the audit used to make.
const auditWindow = 100_000

// PinsPerDay is how many times the collector pins the estate in a day at its
// steady cadence — a collection every [CollectInterval] and an audit every
// [AuditInterval]. Each pin is a linearizable read, which appends one barrier
// to the log of every domain a declared table names; the census those logs'
// read rates are held against counts it (statelog.Census.Background).
const PinsPerDay = int(24*time.Hour/CollectInterval + 24*time.Hour/AuditInterval)

// judgeBatch is how many listed objects one reference read judges.
//
// FIVE HUNDRED: one statement's arguments, inside every SQL engine's variable
// ceiling with room to spare, and what the pass holds in memory between the
// listing and the read — never the store's whole inventory.
const judgeBatch = 500

// MissingShown is how many unreadable files a status names: the counts are
// always whole, and the list is what an operator restores first.
const MissingShown = 100

// Status is what the collector last found, for the alarm, the status surface
// and the next duty holder.
type Status struct {
	// Collect is the last collection pass, and Audit the last audit.
	Collect CollectionReport `json:"collect"`
	Audit   AuditReport      `json:"audit"`
}

// CollectionReport is what one collection pass did.
type CollectionReport struct {
	// Completed is whether the pass listed the whole store and judged
	// every object old enough to be judged.
	Completed bool `json:"completed"`

	// Listed is how many objects the store held under the engine's
	// namespace; Aged how many of them were past the grace; Deleted how
	// many of those no row named and the pass deleted; Referenced how
	// many a row still named.
	Listed     int `json:"listed"`
	Aged       int `json:"aged"`
	Deleted    int `json:"deleted"`
	Referenced int `json:"referenced"`

	// Abandoned is how many uploads begun and never finished the pass
	// abandoned.
	Abandoned int `json:"abandoned"`

	// Skipped says why the pass stopped judging, empty when it ran in
	// full. What it deleted before it stopped is counted above either way.
	Skipped string `json:"skipped,omitempty"`

	// SweepError is what stopped the sweep of unfinished uploads — which
	// the collection does not wait on, and so does not fail with.
	SweepError string `json:"sweep_error,omitempty"`

	// At is when the pass ended, and Error what stopped it.
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// AuditReport is the last audit attempt, and what the last one that ran to its
// end found.
//
// The attempt's fields describe that attempt whatever became of it — a failed
// one's counts are floors — and the findings that must outlive a failed
// attempt are [AuditReport.Found], beside them. The record is the fleet's, so
// it evolves additively: a successor build reads it during a rolling upgrade.
type AuditReport struct {
	// Completed is whether the attempt asked about every object the estate
	// names, in an estate that was complete; Referenced how many it asked
	// about, Missing how many the store does not hold and Damaged how
	// many it holds wrong — floors, for an attempt that did not complete.
	Completed  bool `json:"completed"`
	Referenced int  `json:"referenced"`
	Missing    int  `json:"missing"`
	Damaged    int  `json:"damaged"`

	// At is when the attempt ended, and Error what stopped it.
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`

	// Found is what the last audit that ran to its end found — this one,
	// or the one before it when this one failed — and absent before any
	// has. It is what the alarm counts, so a failed attempt never clears
	// it.
	Found *AuditFindings `json:"found,omitempty"`
}

// AuditFindings is what an audit that ran to its end found.
type AuditFindings struct {
	// At is when that audit ended, and Completed whether its estate was
	// complete — when it was not, Missing and Damaged are floors.
	At        time.Time `json:"at"`
	Completed bool      `json:"completed"`

	// Referenced is how many objects the estate named; Missing how many
	// of them the store does not hold, and Damaged how many it holds at
	// another size, or under another digest where it keeps one.
	Referenced int `json:"referenced"`
	Missing    int `json:"missing"`
	Damaged    int `json:"damaged"`

	// MissingFiles names the first [MissingShown] files that cannot be
	// read, missing and damaged alike.
	MissingFiles []MissingFile `json:"missing_files,omitempty"`
}

// MissingFile is one file whose bytes the store cannot give back.
type MissingFile struct {
	// Object is the object the file's row names.
	Object objstore.Key `json:"object"`
	// NamedBy is the file, as its row's owner columns spell it — a
	// project and a path, `ENG/reports/q3.md`.
	NamedBy string `json:"named_by"`
	// Damaged is set where the store holds the object with the wrong
	// bytes, and unset where it holds nothing at all.
	Damaged bool `json:"damaged,omitempty"`
}

// clone is f with a list of its own.
func (f *AuditFindings) clone() *AuditFindings {
	if f == nil {
		return nil
	}
	out := *f
	out.MissingFiles = slices.Clone(f.MissingFiles)
	return &out
}

// Options builds a [Collector].
type Options struct {
	// Store is the object store the collector lists and deletes from.
	Store *objstore.Store
	// References is every source of object references ([Sources]).
	References References

	// Unseen says what may name an object that this node cannot read beyond
	// [Options.References], in a sentence, or "" where there is nothing: a
	// state log a data node the fleet counts runs and this build does not
	// register (a newer build's, during a rolling upgrade), or tables a newer
	// build migrated this node's copy of the estate to (before it was rolled
	// back). Asked once per collection, at its first judgement, and a pass
	// judges nothing while it answers anything — a sentence or an error —
	// because every declared table a build reads is its own build's
	// declaration, and a domain it has never heard of may name any object in
	// the store. Nil asks nothing, which only a caller that knows the whole
	// fleet is its own build may leave it.
	Unseen func(ctx context.Context) (string, error)

	// Now is the clock the grace is measured on, injected for tests.
	Now func() time.Time
}

// Collector runs collection and audits. ONE node runs it at a time, as a
// fleet duty: two collectors would each be safe — a deletion needs no lock
// (see the package doc), and deleting what another already deleted is not an
// error — but would pay every request twice.
type Collector struct {
	opts Options

	// window is [auditWindow], which New sets and nothing in the engine
	// changes: a field so this package's tests can cross windows with a
	// handful of objects, every use reading it rather than the constant.
	window int

	mu     sync.Mutex
	status Status
}

// New builds a collector.
func New(opts Options) (*Collector, error) {
	if opts.Store == nil {
		return nil, errors.New("objstore/collect: the collector needs the object store")
	}
	if len(opts.References) == 0 {
		// REFUSED, never run with nothing: a collector with no source
		// reads every object in the store as unreferenced and deletes the
		// lot a day later.
		return nil, errors.New("objstore/collect: the collector needs at least one source of references")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Collector{opts: opts, window: auditWindow}, nil
}

// Status is what the last passes found.
func (c *Collector) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.status
	out.Audit.Found = out.Audit.Found.clone()
	return out
}

// Restore seeds the status from a record of passes another node ran — the
// fleet's, read when this node takes the duty — keeping each half that is
// newer than what this node has. Without it, the first pass a new holder
// recorded would carry the other half empty: the last audit's findings gone
// from the fleet's record, and its alarm with them, an hour after the duty
// moved.
func (c *Collector) Restore(s Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.Collect.At.After(c.status.Collect.At) {
		c.status.Collect = s.Collect
	}
	found := c.status.Audit.Found
	if s.Audit.At.After(c.status.Audit.At) {
		c.status.Audit = s.Audit
		c.status.Audit.Found = found
	}
	if s.Audit.Found != nil && (found == nil || s.Audit.Found.At.After(found.At)) {
		c.status.Audit.Found = s.Audit.Found.clone()
	}
}

// Collect deletes the objects nothing refers to past their grace and abandons
// the uploads that never finished. See the package doc for why each of its
// rules is needed, and why none is a lock.
func (c *Collector) Collect(ctx context.Context) (CollectionReport, error) {
	var r CollectionReport
	cutoff := c.opts.Now().Add(-PendingGrace)
	err := c.collect(ctx, &r, cutoff)
	if ctx.Err() == nil {
		c.sweep(ctx, &r, cutoff)
	}
	r.At = c.opts.Now().UTC()
	if err != nil {
		r.Error = err.Error()
	}
	c.mu.Lock()
	c.status.Collect = r
	c.mu.Unlock()
	if r.Deleted > 0 || r.Abandoned > 0 || r.Skipped != "" ||
		r.SweepError != "" || err != nil {
		log.InfoContext(ctx, "objects_collected", "listed", r.Listed, "aged", r.Aged,
			"deleted", r.Deleted, "abandoned", r.Abandoned,
			"skipped", r.Skipped, "sweep_error", r.SweepError,
			"completed", r.Completed, "error", r.Error)
	}
	return r, err
}

// errPin stops a pass whose estate could not be brought current: it judges
// nothing, and reports why beside what it had deleted before.
type errPin struct{ err error }

func (e errPin) Error() string { return e.err.Error() }
func (e errPin) Unwrap() error { return e.err }

// errIncomplete stops a pass that met an estate it cannot call complete.
var errIncomplete = errors.New("a record this node could not apply may refer to objects in the store")

// errUnseen stops a pass that met rows it cannot read at all ([Options.Unseen]):
// it judges nothing, and reports what it could not see.
type errUnseen struct{ why string }

func (e errUnseen) Error() string {
	return "this node cannot read every row that may name an object, so nothing is deleted: " + e.why
}

func (c *Collector) collect(ctx context.Context, r *CollectionReport, cutoff time.Time) error {
	// THE ESTATE IS PINNED AT THE FIRST JUDGEMENT, not before the listing:
	// a pass that finds nothing past the grace decides nothing, and a
	// barrier it took anyway would be a record on every domain's log each
	// hour, and on every log a node boots with, written for no reader. A
	// pin taken after the listing began is still at least as new as the
	// moment the pass began, which is all the rule asks: every name a key
	// past the grace will ever have was written before then.
	var (
		v      view
		pinned bool
		batch  []objstore.Key
	)
	judge := func() error {
		if len(batch) == 0 {
			return nil
		}
		defer func() { batch = batch[:0] }()
		if !pinned {
			// WHAT THIS BUILD CANNOT SEE is asked before the pin, with
			// the pin's own reason to be late: a pass with nothing past
			// the grace asks nothing. An answer that could not be had is
			// a failed pin — tried again in ten minutes — never a pass
			// that went ahead.
			if c.opts.Unseen != nil {
				why, uerr := c.opts.Unseen(ctx)
				if uerr != nil {
					return errPin{fmt.Errorf("reading what this node cannot "+
						"see that may name an object: %w", uerr)}
				}
				if why != "" {
					return errUnseen{why}
				}
			}
			at, perr := c.opts.References.pin(ctx)
			if perr != nil {
				return errPin{perr}
			}
			v, pinned = at, true
		}
		refs, complete, rerr := c.opts.References.referenced(ctx, batch, v)
		if rerr != nil {
			return rerr
		}
		if !complete {
			return errIncomplete
		}
		for _, k := range batch {
			if _, named := refs[k]; named {
				r.Referenced++
				continue
			}
			if derr := c.opts.Store.Delete(ctx, k); derr != nil {
				return derr
			}
			r.Deleted++
		}
		return nil
	}
	backend := c.opts.Store.Backend()
	err := backend.List(ctx, func(info objstore.Info) error {
		// THE GRAMMAR IS READ HERE, not by the backend: see the package
		// doc for the two kinds of name.
		k, ours := keyNamed(info.Name)
		if !ours {
			return nil
		}
		r.Listed++
		// PAST THE GRACE BY BOTH CLOCKS: the backend's, which dates the
		// bytes, and the key's own, which is what the bound on the write
		// naming it is measured from.
		if !info.Written.Before(cutoff) || !k.Minted().Before(cutoff) {
			return nil
		}
		r.Aged++
		batch = append(batch, k)
		if len(batch) < judgeBatch {
			return nil
		}
		return judge()
	})
	// A LISTING THAT LEFT UNDATED NAMES OUT still handed on every name it
	// could date, so the batch it filled is judged before the pass reports
	// what it could not list.
	if err == nil || errors.Is(err, objstore.ErrUndated) {
		if jerr := judge(); jerr != nil {
			err = errors.Join(err, jerr)
		}
	}
	if pe := (errPin{}); errors.As(err, &pe) {
		r.Skipped = pe.err.Error()
		return pe.err
	}
	if errors.Is(err, errIncomplete) {
		r.Skipped = err.Error()
		return nil
	}
	if ue := (errUnseen{}); errors.As(err, &ue) {
		r.Skipped = ue.Error()
		return nil
	}
	if err != nil {
		return fmt.Errorf("objstore/collect: %w", err)
	}
	r.Completed = true
	return nil
}

// sweep abandons the uploads begun more than the grace ago and never
// finished: see the package doc. A failure is the report's, never the pass's.
func (c *Collector) sweep(ctx context.Context, r *CollectionReport, cutoff time.Time) {
	backend := c.opts.Store.Backend()
	err := backend.Pending(ctx, func(p objstore.Pending) error {
		if !p.Started.Before(cutoff) {
			return nil
		}
		k, ours := keyNamed(p.Name)
		switch {
		case p.Name == "":
			// NAMED BY NOTHING, so nothing could ever read or delete
			// it: the pieces of a put that died before naming them.
		case ours:
			// A KEY'S UPLOAD, past the grace by its key's own instant
			// too, for the rule a finished object is judged by.
			if !k.Minted().Before(cutoff) {
				return nil
			}
		default:
			// SOMEBODY ELSE'S upload, under a shared prefix: theirs.
			return nil
		}
		if err := backend.Abandon(ctx, p); err != nil {
			return err
		}
		r.Abandoned++
		return nil
	})
	if err != nil {
		r.SweepError = err.Error()
		log.WarnContext(ctx, "objects_sweep_failed", "error", err, "abandoned", r.Abandoned,
			"detail", "uploads that never finished are kept, and billed, until a sweep can "+
				"list and abandon them — on S3 the identity needs "+
				"s3:ListBucketMultipartUploads and s3:AbortMultipartUpload")
	}
}

// Audit asks the store for every object the estate names, and counts the ones
// it does not hold and the ones it holds wrong.
func (c *Collector) Audit(ctx context.Context) (AuditReport, error) {
	var r AuditReport
	files, err := c.audit(ctx, &r)
	r.At = c.opts.Now().UTC()
	c.mu.Lock()
	if err != nil {
		r.Error = err.Error()
		r.Found = c.status.Audit.Found.clone()
	} else {
		r.Found = &AuditFindings{At: r.At, Completed: r.Completed, Referenced: r.Referenced,
			Missing: r.Missing, Damaged: r.Damaged, MissingFiles: files}
	}
	c.status.Audit = r
	c.mu.Unlock()
	if r.Missing > 0 || r.Damaged > 0 || err != nil {
		log.WarnContext(ctx, "objects_audited", "referenced", r.Referenced,
			"missing", r.Missing, "damaged", r.Damaged, "completed", r.Completed,
			"error", r.Error,
			"detail", "objects the company's files are kept in are missing or damaged in the object store")
	}
	return r, err
}

func (c *Collector) audit(ctx context.Context, r *AuditReport) ([]MissingFile, error) {
	v, err := c.opts.References.pin(ctx)
	if err != nil {
		return nil, err
	}
	var (
		shown []MissingFile
		// suspects is every reference no listing showed, asked about by
		// name once the walk is over: a store that had not caught up with
		// an object a moment ago (a bucket that lists a new object late, a
		// gateway that left its date out) is not one that lost it.
		suspects []Reference
		complete = true
	)
	note := func(ref Reference, damaged bool) {
		if len(shown) < MissingShown {
			shown = append(shown, MissingFile{Object: ref.Object.Key, NamedBy: ref.NamedBy, Damaged: damaged})
		}
	}
	// judge holds one reference against what the store holds for it.
	judge := func(ref Reference, info objstore.Info) {
		if info.Size != ref.Object.Size || (info.Digest != "" && info.Digest != ref.Object.Hash) {
			r.Damaged++
			note(ref, true)
		}
	}
	var after objstore.Key
	for {
		win, err := c.listWindow(ctx, after)
		if err != nil {
			return nil, fmt.Errorf("objstore/collect: audit: %w", err)
		}
		for i, s := range c.opts.References {
			// ONE LOOK PER REFERENCE, and no set of the keys looked at: a
			// key is named only by the write that uploaded it, so no two
			// rows name one.
			whole, err := s.Each(ctx, v[i], after, func(ref Reference) error {
				if !win.last && ref.Object.Key.Compare(win.through) > 0 {
					return ErrSkipTable
				}
				r.Referenced++
				if info, held := win.held[ref.Object.Key]; held {
					judge(ref, info)
				} else {
					suspects = append(suspects, ref)
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("objstore/collect: audit %s's references: %w", s.Name(), err)
			}
			complete = complete && whole
		}
		if win.last {
			break
		}
		after = win.through
	}
	for _, ref := range suspects {
		info, err := c.opts.Store.Stat(ctx, ref.Object.Key)
		switch {
		case errors.Is(err, objstore.ErrNotFound):
			r.Missing++
			note(ref, false)
		case err != nil:
			return nil, fmt.Errorf("objstore/collect: ask the store for %s: %w", ref.Object.Key, err)
		default:
			judge(ref, info)
		}
	}
	slices.SortFunc(shown, func(a, b MissingFile) int {
		if c := strings.Compare(a.NamedBy, b.NamedBy); c != 0 {
			return c
		}
		return strings.Compare(a.Object.String(), b.Object.String())
	})
	r.Completed = complete
	return shown, nil
}

// window is one window of the audit's listing: what the store holds under
// every key in it, the last key in it, and whether it is the last window.
type window struct {
	held    map[objstore.Key]objstore.Info
	through objstore.Key
	last    bool
}

// listWindow lists the store once and keeps the window's smallest keys
// after `after` (the zero key: from the first). The window is the last one
// when the listing held no more keys than that; otherwise it runs through the
// largest key kept, and the next window starts after it.
//
// AN UNDATED LISTING IS NO FAILURE HERE. An entry the backend could not date
// is left out of the listing ([objstore.ErrUndated]), so the reference naming
// it reads as unlisted and is asked about by name at the end of the pass,
// which answers whether the object is there; the collection, which deletes,
// is the pass that must refuse to judge what it cannot date.
func (c *Collector) listWindow(ctx context.Context, after objstore.Key) (window, error) {
	held := map[objstore.Key]objstore.Info{}
	var largest keyHeap
	truncated := false
	err := c.opts.Store.Backend().List(ctx, func(info objstore.Info) error {
		k, ours := keyNamed(info.Name)
		if !ours || (!after.IsZero() && k.Compare(after) <= 0) {
			return nil
		}
		held[k] = info
		heap.Push(&largest, k)
		if largest.Len() > c.window {
			delete(held, heap.Pop(&largest).(objstore.Key))
			truncated = true
		}
		return nil
	})
	if err != nil && !errors.Is(err, objstore.ErrUndated) {
		return window{}, err
	}
	w := window{held: held, last: !truncated}
	if truncated {
		w.through = largest[0]
	}
	return w, nil
}

// keyHeap is a max-heap of keys: the window keeps the smallest keys it has
// seen, so the one it gives up is always the largest.
type keyHeap []objstore.Key

func (h keyHeap) Len() int           { return len(h) }
func (h keyHeap) Less(i, j int) bool { return h[i].Compare(h[j]) > 0 }
func (h keyHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *keyHeap) Push(x any)        { *h = append(*h, x.(objstore.Key)) }
func (h *keyHeap) Pop() any {
	old := *h
	k := old[len(old)-1]
	*h = old[:len(old)-1]
	return k
}

// keyNamed is the key a listed name stores, and whether it is one at all —
// false for every name the collector does not judge as a key
// ([objstore.KeyOfName]).
func keyNamed(name string) (objstore.Key, bool) {
	k, err := objstore.KeyOfName(name)
	return k, err == nil
}
