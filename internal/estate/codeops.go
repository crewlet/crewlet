package estate

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// Gitlet's operations. Each names the CODE domain, so the code copy's own
// verdict, never the tracker's, decides whether a node answers it (ADR-0079),
// and its floor is on the code log — except a read of a repository's content
// by object id and an engine write, which are asked of the repository's home
// Gitlet node ([GitCopy], ADR-0089). A push and the other committer writes are
// keyed on their repository's data nodes (package doc, "A push is asked of its
// repository's committer"). An operation that judges a grant from the serving
// node's chart declares it ([op.judgesGrant]), and a node behind the company's
// configuration answers none of it.

// CodeServer is the code half a data node serves, declared here by its
// consumer with exactly the methods the operations below call. [gitlet.Served]
// satisfies it.
type CodeServer interface {
	Propose(ctx context.Context, p gitlet.Proposal) (gitlet.ProposeResult, error)
	ProposeCompaction(ctx context.Context, p gitlet.CompactionProposal) (gitlet.CompactResult, error)
	ProposeExpunge(ctx context.Context, p gitlet.ExpungeProposal) (gitlet.ExpungeResult, error)
	ProposeImport(ctx context.Context, p gitlet.ImportProposal) (gitlet.SyncResult, error)
	ProposeMirrorSync(ctx context.Context, p gitlet.MirrorSyncProposal) (gitlet.SyncResult, error)
	ProposeChange(ctx context.Context, p gitlet.ChangeProposal) (gitlet.ChangeResult, error)
	ProposeQueue(ctx context.Context, p gitlet.QueueProposal) (gitlet.QueueResult, error)
	ProposeMerge(ctx context.Context, p gitlet.MergeProposal) (gitlet.MergeResult, error)
	Converse(ctx context.Context, w gitlet.ConversationWrite) (gitlet.ConversationResult, error)
	Mark(ctx context.Context, w gitlet.MarkWrite) (gitlet.MarkResult, error)
	PublishChecks(ctx context.Context, r gitlet.ChecksRequest) (gitlet.ChecksResult, error)
	Resolve(ctx context.Context, q gitlet.ResolveQuery) (gitlet.RepoRef, error)
	Snapshot(ctx context.Context, q gitlet.StateQuery) (gitlet.RepoSnapshot, error)
	PushOutcome(ctx context.Context, q gitlet.OutcomeQuery) (gitlet.PushOutcome, error)
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	QueueDue(ctx context.Context, q gitlet.QueueDueQuery) (gitlet.QueueDueAnswer, error)
	TurnsMovedCode(ctx context.Context, q gitlet.TurnsMovedQuery) (map[string]bool, error)
	ResolveRef(ctx context.Context, q gitlet.RefQuery) (gitlet.RefAnswer, error)
	Lifecycle(ctx context.Context, r gitlet.LifecycleRequest) (gitlet.LifecycleResult, error)
}

// ranNothing turns a code half's [gitlet.ErrNotServing] — a refusal before
// anything was appended (a committer stopping, a node that publishes nothing,
// or this node evicted, behind, below its floor, unable to decode a record or
// read its log's usage) or a half with no reader — into a [declined] half: it
// ran nothing, so the walk moves on with the same arguments, keeping the cause
// for its account. Every other error is the operation's answer.
func ranNothing(err error) error {
	if errors.Is(err, gitlet.ErrNotServing) {
		return declined{cause: err}
	}
	return err
}

// opCodePropose is a push's proposal, asked of the repository's COMMITTER. An
// IDEMPOTENT write: its push id is a UUIDv7 the receiving node minted once,
// which the committer's decide collapses onto an existing push row, so an
// unanswered one is asked of the runner-up with the same id. No actor: the
// principal travels inside the proposal, checked by its Validate on the
// deciding node, and whom the push is made for beside it, held to the
// principal before anything is asked ([codeWrites]). Asked by the host's
// receiver once a push's bundle is stored.
var opCodePropose = define("code.propose", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p gitlet.Proposal) (gitlet.ProposeResult, error) {
		if b.Code == nil {
			return gitlet.ProposeResult{}, errNoHalf
		}
		res, err := b.Code.Propose(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(func(p gitlet.Proposal) string { return p.RepoID }).answeredOn(laneProposals).
	refusedBeforeAsking(func(p gitlet.Proposal) error { return codeWrites(p.Principal, p.WrittenFor) })

// opCodeCompact is a compaction's proposal, asked of the COMMITTER as a push's
// is: idempotent by its compaction id, a UUIDv7 its builder minted once, which
// the domain's operation ledger collapses. On the proposals lane, because it
// waits in the repository's one queue beside its pushes and holds its answer
// slot as a push does. No actor: a compaction is the engine's own, recorded as
// the deciding node's system work.
var opCodeCompact = define("code.compact", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p gitlet.CompactionProposal) (gitlet.CompactResult, error) {
		if b.Code == nil {
			return gitlet.CompactResult{}, errNoHalf
		}
		res, err := b.Code.ProposeCompaction(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(func(p gitlet.CompactionProposal) string { return p.Compaction.RepoID }).answeredOn(laneProposals)

// opCodeExpunge is an expunge record's proposal — a person's purge request, or
// a maintainer's begin or commit — asked of the COMMITTER on the proposals lane
// as a compaction's is, idempotent by the record's own operation id, minted
// once by its asker. No actor: a request names its person inside it, checked
// by its Validate on the deciding node; a begin and a commit are the engine's
// own system work.
var opCodeExpunge = define("code.expunge", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p gitlet.ExpungeProposal) (gitlet.ExpungeResult, error) {
		if b.Code == nil {
			return gitlet.ExpungeResult{}, errNoHalf
		}
		res, err := b.Code.ProposeExpunge(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(gitlet.ExpungeProposal.RepoID).answeredOn(laneProposals)

// opCodeImport is one step of a repository's import — a chunk its home
// fetched, or the import's end — asked of the COMMITTER on the proposals lane
// as an expunge record is. Idempotent by its Seq's precondition rather than an
// id: a chunk that landed answers its second asking stale, and its home reads
// the rows. No actor: an import is the engine's own system work.
var opCodeImport = define("code.import", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p gitlet.ImportProposal) (gitlet.SyncResult, error) {
		if b.Code == nil {
			return gitlet.SyncResult{}, errNoHalf
		}
		res, err := b.Code.ProposeImport(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(gitlet.ImportProposal.RepoID).answeredOn(laneProposals)

// opCodeMirrorSync is one sync of a pull mirror its home fetched, asked of the
// repository's COMMITTER as an import's step is: idempotent by its commands'
// old values, which a sync landed falsifies, so its second asking is answered
// stale. No actor: a sync is the engine's own.
var opCodeMirrorSync = define("code.mirror_sync", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p gitlet.MirrorSyncProposal) (gitlet.SyncResult, error) {
		if b.Code == nil {
			return gitlet.SyncResult{}, errNoHalf
		}
		res, err := b.Code.ProposeMirrorSync(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(gitlet.MirrorSyncProposal.RepoID).answeredOn(laneProposals)

// opCodeChange is a write of a change's lifecycle (open, edit, draft state,
// retarget, close, reopen), asked of the COMMITTER on the proposals lane as a
// push is, idempotent by its operation id (an open's change id derives from
// it, so no retry opens a second change). No actor, as for a push: the asker
// travels inside, recorded as owner or writer. A seat's write judges its grant
// from the deciding node's chart (may it push the open's branch, does it
// maintain the repository), and a write that mentions somebody holds the
// mentions to the chart, so a node behind the company's configuration answers
// neither, even asked last ([op.judgesGrant],
// gitlet.ChangeProposal.JudgesChart; the committer holds the same line,
// gitlet.CommitterOptions.Current). A person's write that mentions nobody
// consults no chart.
var opCodeChange = define("code.change", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p gitlet.ChangeProposal) (gitlet.ChangeResult, error) {
		if b.Code == nil {
			return gitlet.ChangeResult{}, errNoHalf
		}
		res, err := b.Code.ProposeChange(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(gitlet.ChangeProposal.RepoID).answeredOn(laneProposals).
	judgesGrant(gitlet.ChangeProposal.JudgesChart).
	refusedBeforeAsking(func(p gitlet.ChangeProposal) error { return codeWrites(p.Asker, p.WrittenFor) })

// opCodeQueue is a write of a target's MERGE QUEUE — an enqueue (decided by
// its merge gate), a dequeue, or a record its repository's home asks: a train,
// an ejection, a landing, a green or a broken landing. Asked as a change's
// write is, idempotent by its operation id (entry, train and landing ids
// derive from it, so no retry queues a change or lands a train twice); no
// actor, as [opCodeChange], the asker recorded as who enqueued or dequeued
// (the home's records name none: a landing lands each change as its
// enqueuer). Every enqueue and landing judges the chart whoever asks (its
// merge gate counts approvals, owners and merge authority by it), as does a
// seat's dequeue, so a node behind the company's configuration answers none of
// them, even asked last ([op.judgesGrant], gitlet.QueueProposal.JudgesChart).
// A person's dequeue and the home's other records consult no chart.
var opCodeQueue = define("code.queue", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p gitlet.QueueProposal) (gitlet.QueueResult, error) {
		if b.Code == nil {
			return gitlet.QueueResult{}, errNoHalf
		}
		res, err := b.Code.ProposeQueue(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(gitlet.QueueProposal.RepoID).answeredOn(laneProposals).
	judgesGrant(gitlet.QueueProposal.JudgesChart).
	refusedBeforeAsking(func(p gitlet.QueueProposal) error { return codeWrites(p.Asker, p.WrittenFor) })

// opCodeMerge is a change's MERGE — one record that merges the change and
// moves its target onto the commit its merge built, decided by its merge gate
// from one snapshot (ADR-0096) — asked as a queue write is, idempotent by its
// operation id (the merge's push id derives from it, so no retry lands a
// branch twice); no actor, as [opCodeChange], the asker recorded as who
// merged. It judges the chart whoever asks, as a landing does
// (gitlet.MergeProposal.JudgesChart), so a node behind the company's
// configuration answers none of it, even asked last ([op.judgesGrant]).
var opCodeMerge = define("code.merge", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p gitlet.MergeProposal) (gitlet.MergeResult, error) {
		if b.Code == nil {
			return gitlet.MergeResult{}, errNoHalf
		}
		res, err := b.Code.ProposeMerge(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(gitlet.MergeProposal.RepoID).answeredOn(laneProposals).
	judgesGrant(gitlet.MergeProposal.JudgesChart).
	refusedBeforeAsking(func(p gitlet.MergeProposal) error { return codeWrites(p.Asker, p.WrittenFor) })

// opCodeConversation is a write of a change's CONVERSATION — a review ask, a
// comment, a thread's resolution — keyed by the repository and answered by
// the committing data node's change writer (gitlet.ChangeWriter), which
// publishes it on the change's own subject, never in the committer's queue.
// Idempotent by its operation id (a comment's id derives from it). On the
// proposals lane, holding its slot while it waits for its change's turn,
// bounded by the writer's node-wide count ([proposalAnswers]). No actor, as
// [opCodeChange]. A seat's write, an ask of reviewers by anybody and the
// home's record of a revision's facts or a change's mergeability judge the
// chart (gitlet.ConversationWrite.JudgesChart), so a node behind the company's
// configuration answers none of them, even asked last; a person's comment
// consults no chart.
var opCodeConversation = define("code.conversation", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, w gitlet.ConversationWrite) (gitlet.ConversationResult, error) {
		if b.Code == nil {
			return gitlet.ConversationResult{}, errNoHalf
		}
		res, err := b.Code.Converse(ctx, w)
		return res, ranNothing(err)
	}).keyedBy(gitlet.ConversationWrite.RepoID).answeredOn(laneProposals).
	judgesGrant(gitlet.ConversationWrite.JudgesChart).
	refusedBeforeAsking(func(w gitlet.ConversationWrite) error { return codeWrites(w.Asker, w.WrittenFor) })

// opCodeMark is a person's gesture over their own code notices (read, unread,
// snoozed, unsnoozed, read through a log position, primary reasons) or over
// the files of one change they read (gitlet.MarkWrite.Files), published on the
// person's own subject by whichever data node is asked (gitlet.Marks).
// UNKEYED: a mark contends only with the same person's marks, which the broker
// arbitrates on that subject, so no committer has a reason to decide it.
// Idempotent by its operation id. No actor: the writer refuses a mark of
// anybody's record but the asker's own. It DECLARES it judges no chart
// ([op.judgesGrant]): the writer holds the asker to the handle by the
// principal alone, so a node behind the company's configuration decides it as
// a current one would, and refusing it would only send it round the walk.
var opCodeMark = define("code.mark", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, w gitlet.MarkWrite) (gitlet.MarkResult, error) {
		if b.Code == nil {
			return gitlet.MarkResult{}, errNoHalf
		}
		res, err := b.Code.Mark(ctx, w)
		return res, ranNothing(err)
	}).judgesGrant(func(gitlet.MarkWrite) bool { return false })

// opCodeChecks is a write of a repository's CHECKS — an attempt queued by its
// home, claimed by the Gitlet node that runs it, the box its holder made, an
// attempt's ending — keyed by the repository and answered by the committing
// data node's checks writer (gitlet.ChecksWriter), one write per repository at
// a time on its checks subject: so a second claim is refused before it
// appends, and a Gitlet node never publishes a check's record. Idempotent by
// its operation id; on the proposals lane as [opCodeConversation] is. No
// actor: the engine's writes name no principal, and a rerun or a person's CI
// report carries its own inside, checked by its Validate. A seat's rerun
// judges its grant from the chart (gitlet.ChecksRequest.JudgesChart), so a
// node behind the company's configuration answers none of it, even asked last
// ([op.judgesGrant]); every other write consults no chart.
var opCodeChecks = define("code.checks", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, r gitlet.ChecksRequest) (gitlet.ChecksResult, error) {
		if b.Code == nil {
			return gitlet.ChecksResult{}, errNoHalf
		}
		res, err := b.Code.PublishChecks(ctx, r)
		return res, ranNothing(err)
	}).keyedBy(gitlet.ChecksRequest.RepoID).answeredOn(laneProposals).
	judgesGrant(gitlet.ChecksRequest.JudgesChart)

// proposalAnswers is how many proposals a data node answers at once on
// [laneProposals]: the sum of the node-wide counts of the three components
// answering there. A request holds its slot while its component holds it (and
// briefly while refused, or waiting for its asker's floor, statelog.ReadBudget);
// each component refuses busy AT ONCE past its own count, bytes and per-key
// bounds, so with the lane as wide as the three together a burst meets its
// component's bound, never the registration's (past which a request waits
// unseen in the broker's buffer), and holds no slot another's request needs.
// Summed rather than registered per component, so each operation keeps its one
// subject. A slot costs a goroutine and its request: a full lane of ordinary
// ones is tens of MiB of stacks, large ones bounded by each component's node
// bytes (gitlet.CommitterNodeBytes and its peers).
const proposalAnswers = gitlet.CommitterNodeProposals + gitlet.ChangeWriterNodeWrites + gitlet.ChecksWriterNodeWrites

// opCodeResolve is a repository by its name: the host's admission of every git
// request.
var opCodeResolve = define("code.resolve", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q gitlet.ResolveQuery) (gitlet.RepoRef, error) {
		if b.Code == nil {
			return gitlet.RepoRef{}, errNoHalf
		}
		ref, err := b.Code.Resolve(ctx, q)
		return ref, ranNothing(err)
	})

// opCodeRepoState is one page of a repository's snapshot (packs and refs,
// whole or since a position): a Gitlet node's cache sync. ANSWERED WHOLE
// ([op.answeredWhole]): a page's cursor (gitlet.RepoSnapshot.Next) is the next
// page's request, and a member this build would drop leaves a cursor that no
// longer says where it stands.
var opCodeRepoState = define("code.repo_state", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q gitlet.StateQuery) (gitlet.RepoSnapshot, error) {
		if b.Code == nil {
			return gitlet.RepoSnapshot{}, errNoHalf
		}
		snap, err := b.Code.Snapshot(ctx, q)
		return snap, ranNothing(err)
	}).answeredWhole()

// opCodePushOutcome is what became of one push: the receiver's wait on a push
// whose record resolved pending or unknown.
var opCodePushOutcome = define("code.push_outcome", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q gitlet.OutcomeQuery) (gitlet.PushOutcome, error) {
		if b.Code == nil {
			return gitlet.PushOutcome{}, errNoHalf
		}
		out, err := b.Code.PushOutcome(ctx, q)
		return out, ranNothing(err)
	})

// opCodeRead is a read of Gitlet by its kind (gitlet.ReadKinds): the REST
// routes', the CLI's, a seat's tools' and the engine's own listings and
// duties, a change's or a check's held to the grant on its repository. A
// seat's read judges its grant, and that of the seat it reads for
// (gitlet.ReadQuery.CeilingSeat), from the serving node's chart
// (gitlet.JudgeRead), so a node behind the company's configuration answers
// none of it ([op.judgesGrant]) and one with no chart answers
// gitlet.ErrNotServing. A person's or a token's consults no chart, and the
// engine's name no asker, so a node behind answers them as a last resort:
// `check_box` too, since the attempt's row is the box's authority, and
// `mirror_push`, whose home judges its seat itself (gitlet.MirrorSeatRefusal).
// The queue work a home owes is read whole, by [opCodeQueueDue].
var opCodeRead = define("code.read", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		if b.Code == nil {
			return gitlet.ReadAnswer{}, errNoHalf
		}
		answer, err := b.Code.Read(ctx, q)
		return answer, ranNothing(err)
	}).judgesGrant(func(q gitlet.ReadQuery) bool { return q.Asker.Kind == gitlet.PrincipalAgent })

// opCodeQueueDue is a page of the queue work a repository's HOME Gitlet node
// owes (gitlet.QueueDueQuery): the repositories owing any, or one target's,
// its first page with the given-up trains' attempts to cancel and the closes
// owed; the home's merge queue works from it (internal/gitlet/home). The
// engine's own, naming no asker and judging no grant, so a node behind the
// configuration answers it as a last resort; and the engine's alone, since it
// hands over whom each enqueue was made for ([CodeSeam.QueueDue]). ANSWERED
// WHOLE ([op.answeredWhole]), never a kind of [opCodeRead], whose answers are
// only shown: the home writes what it reads into its next records, so a page
// holding a member this build would drop fails the home's turn and its queue
// waits for a build that declares it. Across a roll the queue waits too: each
// build refuses, or leaves unserved, the other's way of asking.
var opCodeQueueDue = define("code.queue_due", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q gitlet.QueueDueQuery) (gitlet.QueueDueAnswer, error) {
		if b.Code == nil {
			return gitlet.QueueDueAnswer{}, errNoHalf
		}
		answer, err := b.Code.QueueDue(ctx, q)
		return answer, ranNothing(err)
	}).answeredWhole()

// opCodeTurnsMoved is which of a set of turns pushed, opened a change or gave
// a review: the advisor's read, which names no asker.
var opCodeTurnsMoved = define("code.turns_moved", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q gitlet.TurnsMovedQuery) (map[string]bool, error) {
		if b.Code == nil {
			return nil, errNoHalf
		}
		moved, err := b.Code.TurnsMovedCode(ctx, q)
		return moved, ranNothing(err)
	})

// opCodeResolveRef is one ref of one repository at the level the query names,
// asked of any data node whose code copy serves: how a seat's tool turns a
// branch or a tag into the object id every read by oid takes. A seat's resolve
// judges its grant as a read does, so it declares so ([op.judgesGrant]).
var opCodeResolveRef = define("code.resolve_ref", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q gitlet.RefQuery) (gitlet.RefAnswer, error) {
		if b.Code == nil {
			return gitlet.RefAnswer{}, errNoHalf
		}
		answer, err := b.Code.ResolveRef(ctx, q)
		return answer, ranNothing(err)
	}).judgesGrant(func(q gitlet.RefQuery) bool { return q.Asker.Kind == gitlet.PrincipalAgent })

// opCodeLifecycle is one repository-lifecycle request — a tool's create,
// rename, settings change, archive, unarchive, delete, restore or protection,
// or a live turn's hold — decided by any data node whose code copy serves.
// Idempotent by the request's op id: the domain's operation ledger vouches for
// a repeat, and a create derives its repository's id from it
// (gitlet.RepoIDOf). No actor: the asker travels inside, checked by its
// Validate. EVERY request judges a grant (a seat's on the repository, a
// person's create's owner), so a node behind the company's configuration
// answers none, asked first or last ([op.judgesGrant]): the revision it failed
// to apply may be the one that took the grant away.
// gitlet.StoreOptions.Current holds the in-process call to the same rule.
var opCodeLifecycle = define("code.lifecycle", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, r gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
		if b.Code == nil {
			return gitlet.LifecycleResult{}, errNoHalf
		}
		res, err := b.Code.Lifecycle(ctx, r)
		return res, ranNothing(err)
	}).judgesGrant(func(gitlet.LifecycleRequest) bool { return true })

// opCodeGitRead is one server-side read of a repository by object id (a tree,
// a file's lines, a diff, a comparison), asked of its HOME Gitlet node, then
// its runner-up (ADR-0089), on the git reads' lane, each attempt stamped with
// whether it asks the walk's effective home (gitlet.GitQuery.Home), the one
// node a walk lets fill a copy. A node unready for a reason about itself
// (gitlet.ErrGitUnready) is moved on from without demotion ([gitUnready]). It
// declares no grant: the Gitlet node judges a seat's itself, refusing one
// where it is behind its configuration.
var opCodeGitRead = define("code.git_read", opRead, GitCopy, false,
	func(ctx context.Context, b Backend, _ *Actor, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
		if b.Git == nil {
			return gitlet.GitAnswer{}, errNoHalf
		}
		answer, err := b.Git.Read(ctx, q)
		return answer, gitUnready(err)
	}).keyedOnGit(func(q gitlet.GitQuery) string { return q.RepoID },
	func(q gitlet.GitQuery, home bool) gitlet.GitQuery {
		q.Home = home
		return q
	}).answeredOn(laneGitReads)

// opCodeMergePreview is one MERGE PREVIEW — what merging one commit into
// another would do (gitlet.MergePreviewQuery) — asked and stamped as
// [opCodeGitRead] is, and declaring no grant for the same reason. The Gitlet
// node answers it by THE merge a home records a change's mergeability by
// (internal/gitlet/home's Previews), in a scratch of its own and never its
// cache, so a preview and the change it would open say one thing. It rides no
// log, so it takes no versionedFields row; its envelope evolves additive-only.
var opCodeMergePreview = define("code.merge_preview", opRead, GitCopy, false,
	func(ctx context.Context, b Backend, _ *Actor, q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
		if b.Git == nil {
			return gitlet.MergePreviewAnswer{}, errNoHalf
		}
		answer, err := b.Git.MergePreview(ctx, q)
		return answer, gitUnready(err)
	}).keyedOnGit(func(q gitlet.MergePreviewQuery) string { return q.RepoID },
	func(q gitlet.MergePreviewQuery, home bool) gitlet.MergePreviewQuery {
		q.Home = home
		return q
	}).answeredOn(laneGitReads)

// opCodeGitWrite is one ENGINE WRITE — a commit of files, a revert, a
// cherry-pick, a branch created or deleted, built beside a copy of the
// repository (ADR-0092) — asked as [opCodeGitRead] is, since the home's copy
// is the one reads keep warm; on [laneGitWrites], within
// [Timing.GitWriteAttemptFor] (a merge_change, landed as one merge record,
// gets the merge's budget). IDEMPOTENT by its push id (gitlet.WritePushID):
// the runner-up builds the same commit byte for byte and is answered from a
// silent home's row or collapses into its push at the committer. A node that
// ran nothing about its own state is declined ([ranNothing]), one unready
// moved on from ([gitUnready]). No grant declared, as a read's; whom it is
// made for is held to its principal ([codeWrites]). No versionedFields row:
// what it leaves is a push, the domain's own record.
var opCodeGitWrite = define("code.git_write", opIdempotentWrite, GitCopy, false,
	func(ctx context.Context, b Backend, _ *Actor, w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
		if b.Git == nil {
			return gitlet.EngineWriteResult{}, errNoHalf
		}
		res, err := b.Git.Write(ctx, w)
		return res, gitUnready(ranNothing(err))
	}).keyedOnGit(func(w gitlet.EngineWrite) string { return w.RepoID },
	func(w gitlet.EngineWrite, home bool) gitlet.EngineWrite {
		w.Home = home
		return w
	}).attemptBy(func(w gitlet.EngineWrite, t Timing) time.Duration {
	return t.GitWriteAttemptFor(w.Kind)
}).answeredOn(laneGitWrites).
	refusedBeforeAsking(func(w gitlet.EngineWrite) error { return codeWrites(w.Principal, w.WrittenFor) })
