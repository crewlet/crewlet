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
// lists every name it holds, verbatim, and only a name that is an object's
// key under the engine's namespace (objstore.KeyOfName) is the collector's to
// judge. A name an earlier build stored a chunk under — sixty-four hex digits
// — is left where it is, and so is anything else: another application's
// object under a shared prefix, something an operator put in the bucket.
//
// An object is deleted only when ALL of these hold:
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
//   - No declared table names it in an estate that is CURRENT — the pass
//     first waits for everything each domain's log had committed when it
//     started ([Source.Barrier]) — and COMPLETE: a record this node could not
//     decode might be the one naming the object, so a pass that meets one
//     deletes nothing further.
//
// NO LOCK, NO SECOND LOOK. A key is minted for one upload and named only by
// the write that uploaded it, so there is no writer re-using an object the
// collector could be deleting — the race the chunk-era lock existed for has
// nothing left to race. Two collectors at once are safe too: deleting an
// object that is gone is not an error.
//
// # Audit: what the store has lost
//
// Every object the estate names should be in the backend — a file's record
// is written only after its object is stored. The audit asks the backend for
// each one and counts what is not there: the `objects_missing` alarm, and the
// list `crewlet objects status` prints. A durable backend loses nothing, so a
// non-zero count is the backend failing at the one thing it is for.
package collect

import (
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
// A DAY: an audit is one stat per referenced object, and what it looks for is a
// backend losing acknowledged bytes — an event, not a drift — so a day is
// soon enough to page somebody and a fraction of the requests a tighter
// cadence would cost on a store that bills per request.
const AuditInterval = 24 * time.Hour

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

// MissingShown is how many missing objects a status names: the count is
// always whole, and the list is what an operator restores first.
const MissingShown = 100

// Status is what the collector last found, for the alarm, the status surface
// and the next duty holder's log.
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

	// Skipped says why the pass deleted nothing, empty when it ran in full.
	Skipped string `json:"skipped,omitempty"`

	// At is when the pass ended, and Error what stopped it.
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// AuditReport is what one audit found.
type AuditReport struct {
	// Completed is whether the audit asked about every object the estate
	// names, in an estate that was complete. Missing below is what it
	// found either way; an incomplete audit's is a floor.
	Completed bool `json:"completed"`

	// Referenced is how many objects the estate names, and Missing how
	// many of those the store does not hold. MissingObjects names the
	// first [MissingShown] of them.
	//
	// A NAME OF ITS OWN, never the `missing_chunks` the build before gave
	// its list of digests: this record is the fleet's, read by both builds
	// during a rolling upgrade, and a key decodes only in its own spelling
	// — one build's list under the other's name would fail the whole
	// record. Under two names each build reads the other's counts and
	// simply finds no list.
	Referenced     int            `json:"referenced"`
	Missing        int            `json:"missing"`
	MissingObjects []objstore.Key `json:"missing_objects,omitempty"`

	// At is when the audit ended, and Error what stopped it.
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// Options builds a [Collector].
type Options struct {
	// Store is the object store the collector lists and deletes from.
	Store *objstore.Store
	// References is every source of object references ([Sources]).
	References References
	// Now is the clock the grace is measured on, injected for tests.
	Now func() time.Time
}

// Collector runs collection and audits. ONE node runs it at a time, as a
// fleet duty: two collectors would each be safe — a deletion needs no lock
// (see the package doc), and deleting what another already deleted is not an
// error — but would pay every request twice.
type Collector struct {
	opts Options

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
	return &Collector{opts: opts}, nil
}

// Status is what the last passes found.
func (c *Collector) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.status
	out.Audit.MissingObjects = slices.Clone(out.Audit.MissingObjects)
	return out
}

// Collect deletes the objects nothing refers to past their grace. See the
// package doc for why each of its rules is needed, and why none is a lock.
func (c *Collector) Collect(ctx context.Context) (CollectionReport, error) {
	var r CollectionReport
	err := c.collect(ctx, &r)
	r.At = c.opts.Now().UTC()
	if err != nil {
		r.Error = err.Error()
	}
	c.mu.Lock()
	c.status.Collect = r
	c.mu.Unlock()
	if r.Deleted > 0 || r.Skipped != "" || err != nil {
		log.InfoContext(ctx, "objects_collected", "listed", r.Listed, "aged", r.Aged,
			"deleted", r.Deleted, "skipped", r.Skipped,
			"completed", r.Completed, "error", r.Error)
	}
	return r, err
}

// errIncomplete stops a pass that met an estate it cannot call complete.
var errIncomplete = errors.New("a record this node could not apply may refer to objects in the store")

func (c *Collector) collect(ctx context.Context, r *CollectionReport) error {
	v, err := c.opts.References.pin(ctx)
	if err != nil {
		r.Skipped = err.Error()
		return err
	}
	cutoff := c.opts.Now().Add(-PendingGrace)
	var batch []objstore.Key
	judge := func() error {
		if len(batch) == 0 {
			return nil
		}
		defer func() { batch = batch[:0] }()
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
	err = c.opts.Store.Backend().List(ctx, func(info objstore.Info) error {
		// THE GRAMMAR IS READ HERE, not by the backend: a backend lists
		// every name it holds, and a name that is not an object's key
		// under the engine's namespace — a chunk an earlier build stored,
		// somebody else's object — is never counted, never judged and
		// never deleted.
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
	if err == nil {
		err = judge()
	}
	if errors.Is(err, errIncomplete) {
		r.Skipped = err.Error()
		return nil
	}
	if err != nil {
		return fmt.Errorf("objstore/collect: %w", err)
	}
	r.Completed = true
	return nil
}

// Audit asks the store for every object the estate names and counts the
// ones it does not hold.
func (c *Collector) Audit(ctx context.Context) (AuditReport, error) {
	var r AuditReport
	err := c.audit(ctx, &r)
	r.At = c.opts.Now().UTC()
	if err != nil {
		r.Error = err.Error()
	}
	c.mu.Lock()
	c.status.Audit = r
	c.mu.Unlock()
	if r.Missing > 0 || err != nil {
		log.WarnContext(ctx, "objects_audited", "referenced", r.Referenced,
			"missing", r.Missing, "completed", r.Completed, "error", r.Error,
			"detail", "objects the company's files are kept in are not in the object store")
	}
	return r, err
}

func (c *Collector) audit(ctx context.Context, r *AuditReport) error {
	v, err := c.opts.References.pin(ctx)
	if err != nil {
		return err
	}
	// ONE QUESTION PER OBJECT, across sources: an object two tables name is
	// one object the store either holds or does not.
	asked := map[objstore.Key]struct{}{}
	complete := true
	for i, s := range c.opts.References {
		whole, err := s.Each(ctx, v[i], func(k objstore.Key) error {
			if _, dup := asked[k]; dup {
				return nil
			}
			asked[k] = struct{}{}
			r.Referenced++
			_, err := c.opts.Store.Stat(ctx, k)
			switch {
			case errors.Is(err, objstore.ErrNotFound):
				r.Missing++
				if len(r.MissingObjects) < MissingShown {
					r.MissingObjects = append(r.MissingObjects, k)
				}
				return nil
			case err != nil:
				return fmt.Errorf("ask the store for %s: %w", k, err)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("objstore/collect: audit %s's references: %w", s.Name(), err)
		}
		complete = complete && whole
	}
	slices.SortFunc(r.MissingObjects, func(a, b objstore.Key) int {
		return strings.Compare(a.String(), b.String())
	})
	r.Completed = complete
	return nil
}

// keyNamed is the key a listed name stores, and whether it is one at all —
// false for every name the collector leaves alone ([objstore.KeyOfName]).
func keyNamed(name string) (objstore.Key, bool) {
	k, err := objstore.KeyOfName(name)
	return k, err == nil
}
