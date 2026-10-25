package code

import (
	"context"
	"fmt"
)

// Served is a data node's code half as the estate serves it: the committer's
// write and the reader's reads, so the engine and the multi-node suite hand
// the router ONE adapter rather than each writing its own (ADR-0008).
//
// Committer is nil on a node that does not publish — a node in a maintenance
// mode, which may append nothing — and a proposal is then refused
// [ErrNotServing], which the router reads as a node that ran nothing and asks
// the repository's runner-up with the same push id. Handing a maintenance-mode
// node's committer to peers would be the publish the mode exists to rule out.
//
// Reader is what every read is answered from. A Served with none — its zero
// value — runs NOTHING rather than panicking on the goroutine that serves the
// estate: every operation is refused [ErrNotServing], and the router asks the
// next data node.
type Served struct {
	Committer *Committer
	Reader    *Reader
}

// Propose decides one push on this node's committer — see [Committer.Propose].
func (s Served) Propose(ctx context.Context, p Proposal) (ProposeResult, error) {
	if s.Committer == nil {
		return ProposeResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides "+
			"no push", ErrNotServing)
	}
	return s.Committer.Propose(ctx, p)
}

// Resolve answers a repository by name — see [Reader.Resolve].
func (s Served) Resolve(ctx context.Context, q ResolveQuery) (RepoRef, error) {
	if s.Reader == nil {
		return RepoRef{}, errNoReader
	}
	return s.Reader.Resolve(ctx, q)
}

// Snapshot answers one page of a repository's snapshot — see [Reader.Snapshot].
func (s Served) Snapshot(ctx context.Context, q StateQuery) (RepoSnapshot, error) {
	if s.Reader == nil {
		return RepoSnapshot{}, errNoReader
	}
	return s.Reader.Snapshot(ctx, q)
}

// PushOutcome answers what became of one push — see [Reader.PushOutcome].
func (s Served) PushOutcome(ctx context.Context, q OutcomeQuery) (PushOutcome, error) {
	if s.Reader == nil {
		return PushOutcome{}, errNoReader
	}
	return s.Reader.PushOutcome(ctx, q)
}

// Read answers a person's or a surface's read — see [Reader.Read].
func (s Served) Read(ctx context.Context, q ReadQuery) (ReadAnswer, error) {
	if s.Reader == nil {
		return ReadAnswer{}, errNoReader
	}
	return s.Reader.Read(ctx, q)
}

// errNoReader is a read of a code half that holds no reader of the code log.
var errNoReader = fmt.Errorf("%w: this node holds no reader of the code log", ErrNotServing)
