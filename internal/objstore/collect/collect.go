// Package collect deletes the object store's garbage and counts what it has
// lost: the one fleet duty the object store needs (ADR-0026).
//
// # Why there is so little here
//
// The bytes live in ONE backend the whole fleet shares — the broker's own
// object store, replicated as every stream is, or an S3-compatible bucket
// that keeps its own copies. So nothing here decides where a chunk lives,
// fetches a missing copy or reads a disk for rot: the backend does all of
// that. What no backend can know is which chunks the company still REFERS to,
// because that is a fact about the replicated estate — so the two jobs left
// are the two that need it, run by one data node at a time.
//
// # The inventory is the estate
//
// No list of chunks is kept beside the estate. Every row that refers to an
// object names its chunks, and the declared tables (internal/objstore/
// references) are the whole inventory ([Sources]). A list kept beside it
// would be a second answer to which chunks exist, and the two would drift the
// first time a write landed in one and not the other.
//
// # Collection: deletion is the only dangerous thing, and it has three rules (ADR-0027)
//
// A chunk is deleted only when ALL of these hold:
//
//   - It is older than [PendingGrace]. Bytes are uploaded BEFORE the record
//     naming them is written, so every chunk is unreferenced for a while at
//     the start of its life; the grace outlasts the slowest upload from its
//     first chunk to its record.
//   - No declared table names it in an estate that is CURRENT — the pass
//     first waits for everything each domain's log had committed when it
//     started ([Source.Barrier]) — and COMPLETE: a record this node could not
//     decode might be the one naming the chunk, so a pass that meets one
//     deletes nothing.
//   - Its age, read again UNDER THE CHUNK'S LOCK (objstore.Locks), is still
//     past the grace. A file re-using an old chunk re-puts it under the same
//     lock, so either the re-put came first and the chunk is young, or the
//     delete did and the re-put stores it again.
//
// # Audit: what the store has lost
//
// Every chunk the estate names should be in the backend — a file's record is
// written only after its chunks are stored. The audit asks the backend for
// each one and counts what is not there: the `objects_missing` alarm, and the
// list `crewlet objects status` prints. A durable backend loses nothing, so a
// non-zero count is the backend failing at the one thing it is for.
package collect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
)

var log = logging.Get("objstore")

// PendingGrace is how long a chunk nothing refers to is kept.
//
// A DAY. The grace has to outlast the slowest upload from its first chunk to
// the record naming it, which is minutes, and it costs nothing but a day's
// worth of abandoned uploads in the store. It is not what covers a node whose
// estate is behind — the pass's barrier is.
const PendingGrace = 24 * time.Hour

// CollectInterval is how often garbage is collected.
//
// AN HOUR: a pass waits on a barrier in every domain that refers to chunks and
// lists the whole store, and the only cost of running it less often is
// garbage kept a little longer beside a grace that is already a day.
const CollectInterval = time.Hour

// AuditInterval is how often the store is asked for every chunk the estate
// names.
//
// A DAY: an audit is one stat per referenced chunk, and what it looks for is a
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

// judgeBatch is how many listed chunks one reference read judges.
//
// FIVE HUNDRED: one statement's arguments, inside every SQL engine's variable
// ceiling with room to spare, and what the pass holds in memory between the
// listing and the read — never the store's whole inventory.
const judgeBatch = 500

// MissingShown is how many missing chunks a status names: the count is always
// whole, and the list is what an operator restores first.
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
	// every chunk old enough to be judged.
	Completed bool `json:"completed"`

	// Listed is how many chunks the store held; Aged how many of them
	// were past the grace; Deleted how many of those no row named and the
	// pass deleted; Referenced how many a row still named; Refreshed how
	// many were re-put while the pass judged them and so kept.
	Listed     int `json:"listed"`
	Aged       int `json:"aged"`
	Deleted    int `json:"deleted"`
	Referenced int `json:"referenced"`
	Refreshed  int `json:"refreshed"`

	// Skipped says why the pass deleted nothing, empty when it ran in full.
	Skipped string `json:"skipped,omitempty"`

	// At is when the pass ended, and Error what stopped it.
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// AuditReport is what one audit found.
type AuditReport struct {
	// Completed is whether the audit asked about every chunk the estate
	// names, in an estate that was complete. Missing below is what it
	// found either way; an incomplete audit's is a floor.
	Completed bool `json:"completed"`

	// Referenced is how many chunks the estate names, and Missing how
	// many of those the store does not hold. MissingChunks names the first
	// [MissingShown] of them.
	Referenced    int             `json:"referenced"`
	Missing       int             `json:"missing"`
	MissingChunks []objstore.Hash `json:"missing_chunks,omitempty"`

	// At is when the audit ended, and Error what stopped it.
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// Options builds a [Collector].
type Options struct {
	// Store is the object store — the backend and the chunk locks a
	// deletion takes.
	Store *objstore.Store
	// References is every source of chunk references ([Sources]).
	References References
	// Now is the clock the grace is measured on, injected for tests.
	Now func() time.Time
}

// Collector runs collection and audits. ONE node runs it at a time, as a
// fleet duty: two collectors would each be safe — every deletion is judged
// under its chunk's lock — but would pay every request twice.
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
		// reads every chunk in the store as unreferenced and deletes the
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
	out.Audit.MissingChunks = slices.Clone(out.Audit.MissingChunks)
	return out
}

// Collect deletes the chunks nothing refers to past their grace. See the
// package doc for why each of its three rules is needed.
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
			"deleted", r.Deleted, "refreshed", r.Refreshed, "skipped", r.Skipped,
			"completed", r.Completed, "error", r.Error)
	}
	return r, err
}

// errIncomplete stops a pass that met an estate it cannot call complete.
var errIncomplete = errors.New("a record this node could not apply may refer to chunks in the store")

func (c *Collector) collect(ctx context.Context, r *CollectionReport) error {
	v, err := c.opts.References.pin(ctx)
	if err != nil {
		r.Skipped = err.Error()
		return err
	}
	cutoff := c.opts.Now().Add(-PendingGrace)
	var batch []objstore.Hash
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
		for _, h := range batch {
			if _, named := refs[h]; named {
				r.Referenced++
				continue
			}
			deleted, derr := c.delete(ctx, h, cutoff)
			if derr != nil {
				return derr
			}
			if deleted {
				r.Deleted++
			} else {
				r.Refreshed++
			}
		}
		return nil
	}
	err = c.opts.Store.Backend().List(ctx, func(held objstore.Held) error {
		r.Listed++
		if !held.Written.Before(cutoff) {
			return nil
		}
		r.Aged++
		batch = append(batch, held.Hash)
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

// delete removes h if, read under its lock, it is still older than cutoff,
// reporting whether it did.
func (c *Collector) delete(ctx context.Context, h objstore.Hash, cutoff time.Time) (bool, error) {
	deleted := false
	err := c.opts.Store.Locked(ctx, h, func(ctx context.Context) error {
		written, err := c.opts.Store.Backend().Stat(ctx, h)
		switch {
		case errors.Is(err, objstore.ErrNotFound):
			return nil
		case err != nil:
			return err
		case !written.Before(cutoff):
			return nil // re-put since it was listed
		}
		if err := c.opts.Store.Backend().Delete(ctx, h); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	return deleted, err
}

// Audit asks the store for every chunk the estate names and counts the ones
// it does not hold.
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
			"detail", "chunks the company's files are made of are not in the object store")
	}
	return r, err
}

func (c *Collector) audit(ctx context.Context, r *AuditReport) error {
	v, err := c.opts.References.pin(ctx)
	if err != nil {
		return err
	}
	// ONE QUESTION PER CHUNK, across sources: a chunk two tables name is
	// one chunk the store either holds or does not.
	asked := map[objstore.Hash]struct{}{}
	complete := true
	for i, s := range c.opts.References {
		whole, err := s.Each(ctx, v[i], func(h objstore.Hash) error {
			if _, dup := asked[h]; dup {
				return nil
			}
			asked[h] = struct{}{}
			r.Referenced++
			_, err := c.opts.Store.Backend().Stat(ctx, h)
			switch {
			case errors.Is(err, objstore.ErrNotFound):
				r.Missing++
				if len(r.MissingChunks) < MissingShown {
					r.MissingChunks = append(r.MissingChunks, h)
				}
				return nil
			case err != nil:
				return fmt.Errorf("ask the store for %s: %w", h, err)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("objstore/collect: audit %s's references: %w", s.Name(), err)
		}
		complete = complete && whole
	}
	slices.Sort(r.MissingChunks)
	r.Completed = complete
	return nil
}
