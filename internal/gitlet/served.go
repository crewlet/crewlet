package gitlet

import (
	"context"
	"fmt"
)

// Served is a data node's code half as the estate serves it: the committer's
// writes and the reader's reads, one adapter the engine and the multi-node
// suite both hand the router (ADR-0008). Store decides a tool's lifecycle
// requests.
//
// Every writer (Committer, ChangeWriter, ChecksWriter, Marks, Store) is nil on
// a node that does not publish: a maintenance mode may append nothing, and
// handing peers its committer would be the publish the mode rules out. Each
// write there is refused [ErrNotServing], which the router reads as a node that
// ran nothing and asks the next data node (a repository's runner-up) with the
// same operation id. With no Reader (the zero value) every operation is refused
// so, rather than panicking on the estate's serving goroutine.
type Served struct {
	Committer *Committer
	// ChangeWriter writes changes' conversations.
	ChangeWriter *ChangeWriter
	// ChecksWriter writes repositories' checks.
	ChecksWriter *ChecksWriter
	Reader       *Reader
	Store        *Store
	// Marks writes persons' own marks on the code log: their inbox's and the
	// files they viewed.
	Marks *Marks
}

// Mark decides one gesture over a person's code notices — see [Marks.Mark].
func (s Served) Mark(ctx context.Context, w MarkWrite) (MarkResult, error) {
	if s.Marks == nil {
		return MarkResult{}, fmt.Errorf("%w: this node publishes nothing, so it writes no person's marks",
			ErrNotServing)
	}
	return s.Marks.Mark(ctx, w)
}

// Lifecycle decides one lifecycle request — see [Store.Lifecycle].
func (s Served) Lifecycle(ctx context.Context, req LifecycleRequest) (LifecycleResult, error) {
	if s.Store == nil {
		return LifecycleResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides no "+
			"lifecycle request", ErrNotServing)
	}
	return s.Store.Lifecycle(ctx, req)
}

// Propose decides one push — see [Committer.Propose].
func (s Served) Propose(ctx context.Context, p Proposal) (ProposeResult, error) {
	if s.Committer == nil {
		return ProposeResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides "+
			"no push", ErrNotServing)
	}
	return s.Committer.Propose(ctx, p)
}

// ProposeCompaction decides one compaction — see [Committer.ProposeCompaction].
func (s Served) ProposeCompaction(ctx context.Context, p CompactionProposal) (CompactResult, error) {
	if s.Committer == nil {
		return CompactResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides "+
			"no compaction", ErrNotServing)
	}
	return s.Committer.ProposeCompaction(ctx, p)
}

// ProposeImport decides one import record, a chunk or the import's end — see
// [Committer.ProposeImport].
func (s Served) ProposeImport(ctx context.Context, p ImportProposal) (SyncResult, error) {
	if s.Committer == nil {
		return SyncResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides no import", ErrNotServing)
	}
	return s.Committer.ProposeImport(ctx, p)
}

// ProposeMirrorSync decides one pull mirror's sync — see
// [Committer.ProposeMirrorSync].
func (s Served) ProposeMirrorSync(ctx context.Context, p MirrorSyncProposal) (SyncResult, error) {
	if s.Committer == nil {
		return SyncResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides no mirror sync",
			ErrNotServing)
	}
	return s.Committer.ProposeMirrorSync(ctx, p)
}

// ProposeExpunge decides one expunge record, a person's request or a
// maintainer's begin or commit — see [Committer.ProposeExpunge].
func (s Served) ProposeExpunge(ctx context.Context, p ExpungeProposal) (ExpungeResult, error) {
	if s.Committer == nil {
		return ExpungeResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides "+
			"no expunge", ErrNotServing)
	}
	return s.Committer.ProposeExpunge(ctx, p)
}

// ProposeChange decides one write of a change's lifecycle — see
// [Committer.ProposeChange].
func (s Served) ProposeChange(ctx context.Context, p ChangeProposal) (ChangeResult, error) {
	if s.Committer == nil {
		return ChangeResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides "+
			"no change", ErrNotServing)
	}
	return s.Committer.ProposeChange(ctx, p)
}

// ProposeQueue decides one write of a target's merge queue — see
// [Committer.ProposeQueue].
func (s Served) ProposeQueue(ctx context.Context, p QueueProposal) (QueueResult, error) {
	if s.Committer == nil {
		return QueueResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides "+
			"no queue's write", ErrNotServing)
	}
	return s.Committer.ProposeQueue(ctx, p)
}

// ProposeMerge decides one merge of a change — see [Committer.ProposeMerge].
func (s Served) ProposeMerge(ctx context.Context, p MergeProposal) (MergeResult, error) {
	if s.Committer == nil {
		return MergeResult{}, fmt.Errorf("%w: this node publishes nothing, so it decides "+
			"no merge", ErrNotServing)
	}
	return s.Committer.ProposeMerge(ctx, p)
}

// Converse decides one write of a change's conversation — see
// [ChangeWriter.Converse].
func (s Served) Converse(ctx context.Context, w ConversationWrite) (ConversationResult, error) {
	if s.ChangeWriter == nil {
		return ConversationResult{}, fmt.Errorf("%w: this node publishes nothing, so it writes no change's "+
			"conversation", ErrNotServing)
	}
	return s.ChangeWriter.Converse(ctx, w)
}

// PublishChecks decides one write of a repository's checks — see
// [ChecksWriter.Publish].
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

// QueueDue answers one page of the queue work a repository's home owes — see
// [Reader.QueueDue].
func (s Served) QueueDue(ctx context.Context, q QueueDueQuery) (QueueDueAnswer, error) {
	if s.Reader == nil {
		return QueueDueAnswer{}, errNoReader
	}
	return s.Reader.QueueDue(ctx, q)
}

// TurnsMovedCode answers which of a set of turns moved work in code: see
// [Reader.TurnsMovedCode].
func (s Served) TurnsMovedCode(ctx context.Context, q TurnsMovedQuery) (map[string]bool, error) {
	if s.Reader == nil {
		return nil, errNoReader
	}
	return s.Reader.TurnsMovedCode(ctx, q)
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
