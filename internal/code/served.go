package code

import (
	"context"
	"fmt"
)

// Served is a data node's code half as the estate serves it: the committer's
// writes and the reader's reads, so the engine and the multi-node suite hand
// the router ONE adapter rather than each writing its own (ADR-0008).
//
// Committer is nil on a node that does not publish — a node in a maintenance
// mode, which may append nothing — and a proposal, a push's, a compaction's,
// an expunge record's or a change's, is then refused [ErrNotServing], which the router reads
// as a node that ran nothing and asks the repository's runner-up with the same
// operation id. Handing a maintenance-mode node's committer to peers would be
// the publish the mode exists to rule out.
//
// Reader is what every read is answered from. A Served with none — its zero
// value — runs NOTHING rather than panicking on the goroutine that serves the
// estate: every operation is refused [ErrNotServing], and the router asks the
// next data node.
//
// Store is the lifecycle store a tool's lifecycle request is decided by, nil
// on a node that publishes nothing — refused [ErrNotServing] like a
// proposal there, and asked of the next data node.
type Served struct {
	Committer *Committer
	// ChangeWriter is the writer of changes' conversations, beside the
	// committer on every node that publishes, and nil where the committer is:
	// a write of a change's conversation is then refused [ErrNotServing], as
	// a proposal is.
	ChangeWriter *ChangeWriter
	// ChecksWriter is the writer of repositories' checks, beside the
	// committer on every node that publishes, and nil where the committer
	// is: a write of a repository's checks is then refused [ErrNotServing],
	// as a proposal is.
	ChecksWriter *ChecksWriter
	Reader       *Reader
	Store        *Store
}

// Lifecycle decides one lifecycle request on this node's lifecycle store —
// see [Store.Lifecycle].
func (s Served) Lifecycle(ctx context.Context, req LifecycleRequest) (LifecycleResult, error) {
	if s.Store == nil {
		return LifecycleResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides no "+
			"lifecycle request", ErrNotServing)
	}
	return s.Store.Lifecycle(ctx, req)
}

// Propose decides one push on this node's committer — see [Committer.Propose].
func (s Served) Propose(ctx context.Context, p Proposal) (ProposeResult, error) {
	if s.Committer == nil {
		return ProposeResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides "+
			"no push", ErrNotServing)
	}
	return s.Committer.Propose(ctx, p)
}

// ProposeCompaction decides one compaction on this node's committer — see
// [Committer.ProposeCompaction] — and, on a node that publishes nothing,
// refuses it as a push is refused there: the router asks the repository's
// runner-up with the same compaction id.
func (s Served) ProposeCompaction(ctx context.Context, p CompactionProposal) (CompactResult, error) {
	if s.Committer == nil {
		return CompactResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides "+
			"no compaction", ErrNotServing)
	}
	return s.Committer.ProposeCompaction(ctx, p)
}

// ProposeExpunge decides one expunge record — a person's request, or a
// maintainer's begin or commit — on this node's committer — see
// [Committer.ProposeExpunge] — and, on a node that publishes nothing, refuses
// it as a push is refused there: the router asks the repository's runner-up
// with the same operation id.
func (s Served) ProposeExpunge(ctx context.Context, p ExpungeProposal) (ExpungeResult, error) {
	if s.Committer == nil {
		return ExpungeResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides "+
			"no expunge", ErrNotServing)
	}
	return s.Committer.ProposeExpunge(ctx, p)
}

// ProposeChange decides one write of a change's lifecycle on this node's
// committer — see [Committer.ProposeChange] — and, on a node that publishes
// nothing, refuses it as a push is refused there: the router asks the
// repository's runner-up with the same operation id.
func (s Served) ProposeChange(ctx context.Context, p ChangeProposal) (ChangeResult, error) {
	if s.Committer == nil {
		return ChangeResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides "+
			"no change", ErrNotServing)
	}
	return s.Committer.ProposeChange(ctx, p)
}

// Converse decides one write of a change's conversation on this node's change
// writer — see [ChangeWriter.Converse] — and, on a node that publishes
// nothing, refuses it as a push is refused there: the router asks the
// repository's runner-up with the same operation id.
func (s Served) Converse(ctx context.Context, w ConversationWrite) (ConversationResult, error) {
	if s.ChangeWriter == nil {
		return ConversationResult{}, fmt.Errorf("%w: this node publishes nothing, so it writes no change's "+
			"conversation", ErrNotServing)
	}
	return s.ChangeWriter.Converse(ctx, w)
}

// PublishChecks decides one write of a repository's checks on this node's
// checks writer — see [ChecksWriter.Publish] — and, on a node that publishes
// nothing, refuses it as a push is refused there: the router asks the
// repository's runner-up with the same operation id.
func (s Served) PublishChecks(ctx context.Context, r ChecksRequest) (ChecksResult, error) {
	if s.ChecksWriter == nil {
		return ChecksResult{}, fmt.Errorf("%w: this node publishes nothing, so it writes no repository's "+
			"checks", ErrNotServing)
	}
	return s.ChecksWriter.Publish(ctx, r)
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

// Read answers a person's, a seat's or a surface's read — see [Reader.Read].
func (s Served) Read(ctx context.Context, q ReadQuery) (ReadAnswer, error) {
	if s.Reader == nil {
		return ReadAnswer{}, errNoReader
	}
	return s.Reader.Read(ctx, q)
}

// ResolveRef answers one ref of one repository — see [Reader.ResolveRef].
func (s Served) ResolveRef(ctx context.Context, q RefQuery) (RefAnswer, error) {
	if s.Reader == nil {
		return RefAnswer{}, errNoReader
	}
	return s.Reader.ResolveRef(ctx, q)
}

// errNoReader is a read of a code half that holds no reader of the code log.
var errNoReader = fmt.Errorf("%w: this node holds no reader of the code log", ErrNotServing)
