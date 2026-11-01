// Package migrate copies every object the estate names from the fleet's object
// store into another backend, verified, so the fleet can move its files from
// the broker's own bucket to an S3-compatible one (`crewlet objects migrate
// --to s3`).
//
// # What a move carries, and why it is more than a backup carries
//
// Every object a Required row names — a file's bytes, what the company holds
// — and every object a Derived row names, but no object a Retired row keeps:
// ADR-0074's table of obligations, whose store-move column is
// [objstore.PassMove], read through [collect.Source.Moved]. A derived object
// is one the engine can build again, which is why no backup carries it: a
// backup is taken again and again and restored rarely, so carrying one would
// cost its bytes at every backup to spare a rebuild after a restore. A move
// is made once, and a derived object it left behind would be built again by
// every node of the moved fleet, at a cost that grows with what it was built
// from rather than with anything that changed, where a copy costs its bytes
// once, through the same verified read as a file's. So the move copies it;
// and since nothing is lost without it, one the source cannot give back whole
// is counted apart and left behind, never named as a file and never holding
// the move up ([Report.Derived]). A retired object stays behind: the move
// ends with every node restarted onto the target, so no reader is left that
// has not caught up with what replaced it.
//
// # Why a copy by key is safe to repeat
//
// A key is minted for one upload and never names other bytes (ADR-0026), so an
// object the target already holds at the right size and digest is the object,
// and a pass that stopped part of the way through is resumed by running it
// again: it copies what is missing and skips what is there. Nothing here
// records progress beside the target itself, because the target IS the record
// of what has been copied.
//
// # Every byte is checked on the way out and on the way in
//
// An object is read through [objstore.Store.Open], which hashes every byte
// against the row naming it and holds back the last one until the whole
// verifies, so a damaged source object reaches the target as a FAILED READ and
// the target stores nothing ([objstore.Backend.Put]). What the target stored
// is then asked about: its size, and its digest where the provider keeps one
// (the s3 backend's probe decides that).
//
// # What this package does not do
//
// It moves no record and decides nothing about the fleet. Which backend the
// fleet's files are in is the coordination store's record, moved by the
// engine once a final pass, run where nothing publishes, has copied
// everything (internal/engine, and docs/concepts/object-store.md for the
// procedure). The source is left whole: the operator deletes it once the
// fleet runs on the target.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Copies is how many objects a pass copies at once.
//
// FOUR. A copy is one read from the source and one streamed put into the
// target, and the s3 backend holds one part (8 MiB) in memory per put, so four
// is 32 MiB of buffers; it is also four reads at once against the broker's
// stream leader, which serves every other read the fleet makes. Past four the
// copy is bound by the link to the bucket rather than by round trips, and the
// pass runs beside a live fleet.
const Copies = 4

// UnreadableShown is how many unreadable files a report names; the count is
// always whole.
const UnreadableShown = 100

// Options is one pass.
type Options struct {
	// Source is the fleet's object store, read verified.
	Source *objstore.Store

	// Target is the backend the objects are copied into.
	Target objstore.Backend

	// References is every domain whose rows name objects.
	References collect.References

	// Pin takes a barrier on every domain's log before the walk, so the
	// pass reads an estate at least as new as its start. A node in a
	// maintenance mode appends no barrier, and nothing publishes there, so
	// it reads what it holds instead.
	Pin bool
}

// Counts is what one pass found and did for one kind of object: those the
// estate's Required rows name ([Report]'s own), or those its Derived rows name
// ([Report.Derived]).
type Counts struct {
	// Referenced is how many such objects the estate names.
	Referenced int `json:"referenced"`

	// Present is how many the target already held, verified; Copied how
	// many this pass copied, and CopiedBytes their bytes.
	Present     int   `json:"present"`
	Copied      int   `json:"copied"`
	CopiedBytes int64 `json:"copied_bytes"`

	// Unreadable is how many the SOURCE could not give back whole: missing
	// from it, or damaged in it.
	Unreadable int `json:"unreadable"`
}

// settled reports whether every object c counts ended present, copied or
// unreadable.
func (c Counts) settled() bool { return c.Present+c.Copied+c.Unreadable == c.Referenced }

// Report is what one pass found and did.
type Report struct {
	// Counts is for the objects Required rows name: the company's files.
	// One the source could not give back whole is a file no copy can bring
	// back; the objects_missing alarm already counts it, and Files names
	// the first [UnreadableShown].
	Counts
	Files []UnreadableFile `json:"unreadable_files,omitempty"`

	// Derived is for the objects Derived rows name, which the engine can
	// build again (ADR-0074): copied as a file is, so the moved fleet has
	// them without building them. One the source could not give back whole
	// is counted here and left behind — named nowhere and holding nothing
	// up, since nothing is lost without it: the engine builds it again.
	Derived Counts `json:"derived"`

	// Complete is whether every domain's estate was complete: false while
	// this node holds a record it could not apply, which may name an
	// object the walk did not see.
	Complete bool `json:"complete"`
}

// UnreadableFile is one file whose bytes the source cannot give back.
type UnreadableFile struct {
	Object  objstore.Key `json:"object"`
	NamedBy string       `json:"named_by"`
	Damaged bool         `json:"damaged,omitempty"`
}

// Done reports whether the target holds every object the source can give
// back, from an estate this node read whole: what the move of the fleet's
// record waits for.
func (r Report) Done() bool { return r.Complete && r.Counts.settled() && r.Derived.settled() }

// counts is the counts ref's object is kept in: [Report.Derived] for one a
// Derived row names, [Report.Counts] for every other.
func (r *Report) counts(ref objstore.Reference) *Counts {
	if ref.Standing == objstore.Derived {
		return &r.Derived
	}
	return &r.Counts
}

// Run copies every object the estate names that the target does not hold, and
// verifies what it copied. A transient failure of either store ends the pass
// with what it did so far in the report; running it again resumes it.
func Run(ctx context.Context, opts Options) (Report, error) {
	if opts.Source == nil || opts.Target == nil || len(opts.References) == 0 {
		return Report{}, errors.New("objstore/migrate: a pass needs a source, a target and the estate's references")
	}
	at := make([]statelog.Position, len(opts.References))
	if opts.Pin {
		for i, s := range opts.References {
			p, err := s.Barrier(ctx)
			if err != nil {
				return Report{}, fmt.Errorf("objstore/migrate: bring %s's view current: %w", s.Name(), err)
			}
			at[i] = p
		}
	}
	p := &pass{opts: opts, refs: make(chan objstore.Reference)}
	p.report.Complete = true
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var workers sync.WaitGroup
	for range Copies {
		workers.Go(func() {
			for ref := range p.refs {
				if err := p.one(ctx, ref); err != nil {
					cancel(err)
				}
			}
		})
	}
	walkErr := p.walk(ctx, at)
	close(p.refs)
	workers.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	slices.SortFunc(p.report.Files, func(a, b UnreadableFile) int {
		return strings.Compare(a.NamedBy, b.NamedBy)
	})
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return p.report, cause
	}
	if walkErr != nil {
		return p.report, walkErr
	}
	return p.report, ctx.Err()
}

// pass is one run's shared state.
type pass struct {
	opts Options
	refs chan objstore.Reference

	mu     sync.Mutex
	report Report
}

// walk hands every reference a table the move copies names to the copiers —
// the Required tables and the Derived ones, as the package doc says — a
// domain and a table at a time, a keyset page of each in a read of its own
// ([collect.Source.Moved]).
func (p *pass) walk(ctx context.Context, at []statelog.Position) error {
	for i, s := range p.opts.References {
		for _, w := range s.Moved(at[i]) {
			for {
				page, whole, err := w.Next(ctx)
				if err != nil {
					return fmt.Errorf("objstore/migrate: walk %s's references: %w", s.Name(), err)
				}
				if !whole {
					p.count(func(r *Report) { r.Complete = false })
				}
				if len(page) == 0 {
					break
				}
				for _, ref := range page {
					p.count(func(r *Report) { r.counts(ref).Referenced++ })
					select {
					case p.refs <- ref:
					case <-ctx.Done():
						return context.Cause(ctx)
					}
				}
			}
		}
	}
	return nil
}

// one makes sure the target holds ref's object.
func (p *pass) one(ctx context.Context, ref objstore.Reference) error {
	name := ref.Object.Key.Name()
	held, err := p.holds(ctx, name, ref.Object)
	if err != nil {
		return err
	}
	if held {
		p.count(func(r *Report) { r.counts(ref).Present++ })
		return nil
	}
	body, err := p.opts.Source.Open(ctx, ref.Object)
	if errors.Is(err, objstore.ErrNotFound) {
		p.unreadable(ref, false)
		return nil
	}
	if err != nil {
		return fmt.Errorf("objstore/migrate: read %s (%s): %w", ref.Object.Key, ref.NamedBy, err)
	}
	err = p.opts.Target.Put(ctx, name, body, objstore.PutMeta{})
	_ = body.Close()
	if errors.Is(err, objstore.ErrCorrupt) {
		// THE SOURCE'S BYTES ARE NOT THE ROW'S, and the target stored
		// nothing: a failed read is the one failure every backend cleans
		// up after.
		p.unreadable(ref, true)
		return nil
	}
	if err != nil {
		return fmt.Errorf("objstore/migrate: store %s (%s) in the target: %w", ref.Object.Key, ref.NamedBy, err)
	}
	if held, err = p.holds(ctx, name, ref.Object); err != nil {
		return err
	}
	if !held {
		return fmt.Errorf("objstore/migrate: the target acknowledged %s (%s) and does not hold "+
			"it at its size and digest", ref.Object.Key, ref.NamedBy)
	}
	p.count(func(r *Report) {
		c := r.counts(ref)
		c.Copied++
		c.CopiedBytes += ref.Object.Size
	})
	return nil
}

// holds reports whether the target holds o under name at its size, and at its
// digest where the target keeps one.
func (p *pass) holds(ctx context.Context, name string, o objstore.Object) (bool, error) {
	info, err := p.opts.Target.Stat(ctx, name)
	switch {
	case errors.Is(err, objstore.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("objstore/migrate: ask the target for %s: %w", o.Key, err)
	}
	return info.Size == o.Size && (info.Digest == "" || info.Digest == o.Hash), nil
}

func (p *pass) count(fn func(*Report)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(&p.report)
}

// unreadable counts ref's object as one the source cannot give back whole,
// and names it among the files where a Required row names it: a derived one
// is counted alone ([Report.Derived]).
func (p *pass) unreadable(ref objstore.Reference, damaged bool) {
	p.count(func(r *Report) {
		c := r.counts(ref)
		c.Unreadable++
		if c == &r.Derived {
			return
		}
		if len(r.Files) < UnreadableShown {
			r.Files = append(r.Files, UnreadableFile{Object: ref.Object.Key, NamedBy: ref.NamedBy, Damaged: damaged})
		}
	})
}
