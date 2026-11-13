package estate

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// Gitlet's operations: a push's proposal, a compaction's, an expunge
// record's and a change's, the reads the host's admission, a cache's sync, the
// REST routes and a seat's tools make of the rows, and the listings a Gitlet node
// finds the repositories a compaction, a publication or an expunge may be due
// in by — each naming the CODE domain, so the code copy's own verdict — never
// the tracker's — decides whether a node answers it (ADR-0079), and its floor
// is on the code log — and a read of a repository's CONTENT by object id and
// an engine write built beside it, which name no domain's copy and are asked
// of the repository's home GITLET node ([GitCopy], ADR-0089). A push, a
// compaction, an expunge record and a change's write are keyed on their
// repository's data nodes: see the package doc's "A push is asked of its
// repository's committer". An operation that JUDGES A GRANT from the serving
// node's chart — a read a seat asks, and a seat's write of a change — says so
// ([op.judgesGrant]), and a node behind the company's configuration answers
// none of it.

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

// ranNothing is a code half's error as the walk reads it: a half that refused
// before anything was appended ([gitlet.ErrNotServing] — a committer stopping, a
// node that publishes nothing, a refusal about THIS node: evicted, behind, a
// record it cannot decode, below its floor, a log whose usage it could not
// read — or a half with no reader) RAN
// NOTHING, which every class moves on from with the same arguments. So it
// becomes a [declined] half, which the router and the server read as a node
// with no backend for the operation, its cause kept for the walk's account.
// Every other error is the operation's answer.
func ranNothing(err error) error {
	if errors.Is(err, gitlet.ErrNotServing) {
		return declined{cause: err}
	}
	return err
}

// opCodePropose is a push's proposal, asked of the repository's COMMITTER — an
// IDEMPOTENT write keyed by its push id, a UUIDv7 the receiving node minted
// once, which the committer's decide collapses onto an existing push row — so
// an unanswered one is asked of the repository's runner-up with the same id.
// No actor: the principal travels INSIDE the proposal, checked by its Validate
// on the node that decides, and whom the push is made for beside it, which the
// asking node holds to the principal before anything is asked ([codeWrites]).
// Its caller is the host's receiver, once a push's bundle is stored.
var opCodePropose = define("code.propose", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p gitlet.Proposal) (gitlet.ProposeResult, error) {
		if b.Code == nil {
			return gitlet.ProposeResult{}, errNoHalf
		}
		res, err := b.Code.Propose(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(func(p gitlet.Proposal) string { return p.RepoID }).answeredOn(laneProposals).
	refusedBeforeAsking(func(p gitlet.Proposal) error { return codeWrites(p.Principal, p.WrittenFor) })

// opCodeCompact is a compaction's proposal, asked of the repository's
// COMMITTER exactly as a push's is — an IDEMPOTENT write keyed by its
// compaction id, a bare UUIDv7 the node that built it minted once, which the
// domain's operation ledger collapses — so an unanswered one is asked of the
// repository's runner-up with the same id. On the proposals lane, because a
// compaction waits in the repository's one queue beside its pushes and holds
// its answer slot as a push's proposal does. No actor: a compaction is the
// engine's own, recorded as the deciding node's system work.
var opCodeCompact = define("code.compact", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p gitlet.CompactionProposal) (gitlet.CompactResult, error) {
		if b.Code == nil {
			return gitlet.CompactResult{}, errNoHalf
		}
		res, err := b.Code.ProposeCompaction(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(func(p gitlet.CompactionProposal) string { return p.Compaction.RepoID }).answeredOn(laneProposals)

// opCodeExpunge is an expunge record's proposal — a person's purge request, or
// the begin or the commit a repository's maintainer built — asked of the
// repository's COMMITTER exactly as a compaction's is: an IDEMPOTENT write
// keyed by the record's own operation id (a request's, an expunge's or a
// commit's, each minted once by its asker), which the domain's operation
// ledger collapses, so an unanswered one is asked of the repository's
// runner-up with the same id. On the proposals lane, because it waits in the
// repository's one queue beside its pushes. No actor: a request names its
// person inside it, checked by its Validate on the node that decides, and a
// begin and a commit are the engine's own, recorded as the deciding node's
// system work.
var opCodeExpunge = define("code.expunge", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p gitlet.ExpungeProposal) (gitlet.ExpungeResult, error) {
		if b.Code == nil {
			return gitlet.ExpungeResult{}, errNoHalf
		}
		res, err := b.Code.ProposeExpunge(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(gitlet.ExpungeProposal.RepoID).answeredOn(laneProposals)

// opCodeImport is one step of a repository's import — a chunk its home
// fetched, or the import's end — asked of the repository's COMMITTER exactly
// as an expunge record is: an IDEMPOTENT write, by its Seq's precondition
// rather than its id (a chunk landed answers its second asking stale, and its
// home reads the rows), so an unanswered one is asked of the repository's
// runner-up as it stands. On the proposals lane, because it waits in the
// repository's one queue beside its pushes. No actor: an import is the
// engine's own, recorded as the deciding node's system work.
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

// opCodeChange is a write of a change's lifecycle — an open, an edit, a draft's
// state, a retarget, a close or a reopen — asked of the repository's COMMITTER
// exactly as a push's is: an IDEMPOTENT write keyed by its operation id, which
// its asker minted once and the domain's operation ledger collapses (an open's
// change id is derived from it, so no retry opens a second change), so an
// unanswered one is asked of the repository's runner-up with the same id. On
// the proposals lane, because it waits in the repository's one queue beside
// its pushes. No actor: the asker travels inside the proposal, checked by its
// Validate on the node that decides, which records it as the change's owner or
// as who wrote it; and whom the write is made for beside it, which the asking
// node holds to the asker before anything is asked ([codeWrites]).
//
// A SEAT'S WRITE JUDGES ITS GRANT from the deciding node's chart — whether it
// may push the branch an open is from, whether it maintains the repository a
// change it does not own is on — and a write that mentions somebody holds the
// mentions to it, so a node behind the company's configuration answers none of
// either ([op.judgesGrant], gitlet.ChangeProposal.JudgesChart, the committer's
// own rule), even asked last; a person's that mentions nobody consults no
// chart. The committer holds the same line itself, as it does for a seat's
// push (gitlet.CommitterOptions.Current).
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

// opCodeQueue is a write of a target's MERGE QUEUE — an enqueue of a change,
// decided by its merge gate, or a dequeue of an entry, and the queue's own
// records its repository's home asks: a train, an ejection, a landing, a
// green and a broken landing — asked of the repository's COMMITTER exactly as
// a change's write is: an IDEMPOTENT write keyed by its operation id, which
// its asker minted once and the domain's operation ledger collapses (an
// enqueue's entry id, a train's and a landing's ids are derived from it, so no
// retry queues a change or lands a train twice), so an unanswered one is
// asked of the repository's runner-up with the same id. On the proposals
// lane, because it waits in the repository's one queue beside its pushes. No
// actor: the asker travels inside the proposal, checked by its Validate on
// the node that decides, which records it as who enqueued or dequeued — and
// the queue's own records name none, a landing landing each change as its
// enqueuer; and whom the write is made for beside it, which the asking node
// holds to the asker before anything is asked ([codeWrites]).
//
// A WRITE THE CHART JUDGES — every enqueue and every landing, whoever asks,
// since the merge gate each judges counts each approval by who may approve
// the change, each owner's by the lead chain above it and a seat's merge
// authority by its grant; and a seat's dequeue, whose grant says whether it
// may take an entry out — is declared so (gitlet.QueueProposal.JudgesChart, the
// committer's own rule), and a node behind the company's configuration
// answers none of it ([op.judgesGrant]), even asked last; a person's dequeue
// and the queue's other records consult no chart.
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
// from one snapshot (ADR-0096) — asked of the repository's COMMITTER exactly
// as a write of its queue is: an IDEMPOTENT write keyed by its operation id,
// which its builder minted once and the domain's operation ledger collapses
// (the merge's push id is derived from it, so no retry lands a branch twice),
// so an unanswered one is asked of the repository's runner-up with the same
// id. On the proposals lane, because it waits in the repository's one queue
// beside its pushes. No actor: the asker travels inside the proposal, checked
// by its Validate on the node that decides, which records it as who merged;
// and whom the merge is made for beside it, which the asking node holds to the
// asker before anything is asked ([codeWrites]).
//
// A WRITE THE CHART JUDGES, whoever asks — its merge gate counts each approval
// by who may approve the change, each owner's by the lead chain above it and a
// seat's merge authority by its grant — so it is declared so
// (gitlet.MergeProposal.JudgesChart, the committer's own rule), and a node
// behind the company's configuration answers none of it ([op.judgesGrant]),
// even asked last.
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

// opCodeConversation is a write of a change's CONVERSATION — an ask for review,
// a comment or a thread's resolution — asked of the repository's COMMITTING
// data node, keyed by the repository as a change's lifecycle is, and answered
// there by its change writer (gitlet.ChangeWriter), which publishes it on the
// change's own subject, never in the committer's queue: an IDEMPOTENT write
// keyed by its operation id, which its asker minted once and the domain's
// operation ledger collapses (a comment's id is derived from it, so no retry
// says it twice), so an unanswered one is asked of the repository's runner-up
// with the same id. On the proposals lane, because it holds its answer slot
// while it waits for its change's turn on that node — its share of the lane
// the change writer's own node-wide count, past which the writer answers busy
// at once ([proposalAnswers]). No actor: the asker travels inside the write,
// checked by its Validate on the node that decides, which records it as who
// wrote it; and whom the write is made for beside it, which the asking node
// holds to the asker before anything is asked ([codeWrites]).
//
// A WRITE THE CHART JUDGES — a seat's, an ask of reviewers by anybody, since
// each reviewer is judged by the chart, and the home's own record of a
// revision's facts or a change's mergeability, whose path owners and whose
// readiness the chart judges — is declared so
// (gitlet.ConversationWrite.JudgesChart, the writer's own rule), and a node
// behind the company's configuration answers none of it, even asked last; a
// person's comment consults no chart.
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

// opCodeMark is a person's gesture over their own code notices — a notice
// read, unread, snoozed or unsnoozed, their position on the code log read
// through, their primary reasons — published on the person's own subject by
// WHICHEVER DATA NODE IT IS ASKED OF (gitlet.Marks): UNKEYED, since a mark
// contends with nothing but the same person's other marks, which the broker
// arbitrates on that subject, and no repository's committer has a reason to
// decide it. An IDEMPOTENT write keyed by its operation id, which its asker
// minted once and the domain's operation ledger collapses, so an unanswered
// one is asked of the next data node with the same id. No actor: the person
// travels inside the write, checked by its writer, which refuses a mark of
// anybody's record but the asker's own. It judges no chart, and DECLARES so
// ([op.judgesGrant], a predicate false of every write): the writer holds the
// asker to the handle by the principal alone (a person whose seat is the
// handle), so a node behind the company's configuration decides a mark as a
// current one would, and refusing it there would only send a person's
// gesture round the walk for nothing.
var opCodeMark = define("code.mark", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, w gitlet.MarkWrite) (gitlet.MarkResult, error) {
		if b.Code == nil {
			return gitlet.MarkResult{}, errNoHalf
		}
		res, err := b.Code.Mark(ctx, w)
		return res, ranNothing(err)
	}).judgesGrant(func(gitlet.MarkWrite) bool { return false })

// opCodeChecks is a write of a repository's CHECKS — an attempt queued by the
// repository's home, a claim of one by the Gitlet node that runs it, its holder's
// naming of the box it made, an attempt's ending — asked of the repository's
// COMMITTING data node, keyed by the repository as a push is, and answered
// there by its checks writer (gitlet.ChecksWriter), which publishes it on the
// repository's checks subject, never in the committer's queue, one write of a
// repository's checks at a time: so a second claim of an attempt is refused
// before it appends, and a Gitlet node never publishes a check's record. An
// IDEMPOTENT write keyed by its operation id, which its asker minted once and
// the domain's operation ledger collapses, so an unanswered one is asked of the
// repository's runner-up with the same id. On the proposals lane, because it
// holds its answer slot while it waits for its repository's turn on that node
// — its share of the lane the checks writer's own node-wide count, past which
// the writer answers busy at once ([proposalAnswers]). No actor: the engine's
// own writes name no principal, and a principal's — a rerun a seat or a person
// asks for, a person's CI's report — travels inside the request, checked by its
// Validate on the node that decides, which records it as who asked.
//
// A SEAT'S RERUN JUDGES ITS GRANT from the deciding node's chart — whether it
// may write the repository or wrote the change the check judges, and
// whoever its turn works for — so it is declared so
// (gitlet.ChecksRequest.JudgesChart, the writer's own rule), and a node behind
// the company's configuration answers none of it ([op.judgesGrant]), even
// asked last; a person's rerun and report, and the engine's own records,
// consult no chart.
var opCodeChecks = define("code.checks", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, r gitlet.ChecksRequest) (gitlet.ChecksResult, error) {
		if b.Code == nil {
			return gitlet.ChecksResult{}, errNoHalf
		}
		res, err := b.Code.PublishChecks(ctx, r)
		return res, ranNothing(err)
	}).keyedBy(gitlet.ChecksRequest.RepoID).answeredOn(laneProposals).
	judgesGrant(gitlet.ChecksRequest.JudgesChart)

// proposalAnswers is how many proposals a data node answers at once, on the
// lane they have to themselves ([laneProposals]): 3 136, EVERY ANSWER THE
// NODE'S CODE HALF HOLDS AT ONCE, summed over the three that answer on it —
// what its committer holds across every repository
// (gitlet.CommitterNodeProposals: one repository's full queue and the batch in
// flight ahead of it, 1 088), what its change writer holds across every change
// (gitlet.ChangeWriterNodeWrites, 1 024) and what its checks writer holds across
// every repository (gitlet.ChecksWriterNodeWrites, 1 024). A request holds its
// slot for as long as its component holds it — a push while it waits in its
// repository's queue and its batch runs, a write of a change's conversation or
// of a repository's checks while it waits for its key's turn and its publish
// runs — so each component refuses busy AT ONCE past its own node-wide count,
// and past its bytes (gitlet.CommitterNodeBytes, gitlet.ChangeWriterNodeBytes,
// gitlet.ChecksWriterNodeBytes) and its per-key bounds before that, and a slot is
// held for long only by a write its component is deciding or will decide
// inside its asker's attempt. With the lane as wide as the three counts
// together, the bound a burst meets is always its component's own, answered at
// once — never the registration's, past which a request waits unseen in the
// broker's buffer for a slot — and no component's burst holds a slot another's
// request needs: a burst of comments on one change, written faster than the
// change's single flight drains, or queued behind a publish an election holds,
// is refused busy once its writer is full while every push, compaction and
// change routed to the node is still answered. The lane's slots are shared by
// SUMMING the shares rather than by a registration each, so a request finds
// its slot on the one subject its operation is declared on. A request also
// holds a slot while the serving node waits for its asker's floor (bounded by
// statelog.ReadBudget) and while it is refused, each briefly.
//
// What a slot costs is a goroutine and the request it carries, a kilobyte or
// two for an ordinary push or comment: a full lane of them is some tens of
// megabytes of stacks. The largest push the code domain admits weighs about a
// mebibyte (gitlet.EntryRoom: some six hundred commands at the longest ref JSON
// spells six times over — a thousand at the longest plain ref, and a full list
// of the commits it introduced, weigh about 440 KiB), and so does the largest
// write of a conversation (gitlet.MaxConversationBytes, a review with its
// findings), and those their components bound by their bytes, so a full lane
// of large ones holds what each component's node bytes admit and answers the
// rest busy at once. And every proposal is a push some receiving node's
// receive-pack holds open while it waits, so what reaches the lane at once is
// bounded by the fleet's transfer lanes as well.
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

// opCodeRepoState is one page of a repository's snapshot — its packs and refs,
// whole or since a position: a Gitlet node's cache sync. ANSWERED WHOLE
// ([op.answeredWhole]), because a page's cursor (gitlet.RepoSnapshot.Next) is the
// next page's request: a member a newer data node put in it and this build
// would drop is a snapshot paged on by a cursor that no longer says where it
// stands, so it is refused rather than paged on.
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
// routes' repository listing, one repository's detail and its push history,
// which the operator's CLI reads too; an executor run's pushes, which the
// engine reads when it collects the run; a repository's refs; a listing of
// changes — of a repository, a work item, a state, a reviewer or what its
// changes wait on — and one change by its number, a part of it a page at a time
// (its revisions, reviewers, comments, reviews or findings), each held to the
// grant on the change's repository as a read of the repository is; its pushes
// in log order, which a Gitlet node's index of which push introduced each commit
// is built from; the repositories a compaction may be due in, most live packs
// first, which a Gitlet node polls for the ones it is home for; the repositories a
// publication may be due in, least recently published first; the repositories
// an expunge is owed in, an expunge open or a purge requested; a repository's
// checks — a change's, a commit's, the repository's newest ended first, or
// every readable repository's since a moment — and one attempt whole, each held
// to the grant on its repository; one attempt by its key (check_box), the read
// a check's box's admission makes on every request the box sends to /box/git/
// or /box/gitlet/ (internal/gitlet/host's checkadmit.go); the attempts waiting for
// a runner in one cell, oldest queued first (checks_queued), and those runners
// hold (checks_running); the change heads a repository's home owes a dispatch
// of their checks (checks_due); the live entries of a repository's merge queues
// — of one target, or of every readable repository — with their trains, a
// target's landings above its green and a repository's green rows, each held to
// the grant on its repository; and a repository's push mirror with the green
// rows it publishes, read in one snapshot by the home that pushes it
// (mirror_push). A seat's read on a node that holds no chart is that node's
// gitlet.ErrNotServing — it ran nothing — so the walk asks the next. The queue
// work a repository's home owes is no kind of it: its home carries what it
// reads on, so it is read whole or refused, by an operation of its own
// ([opCodeQueueDue]).
//
// A SEAT'S READ JUDGES ITS GRANT from the serving node's chart (which
// repositories it lists, and whether it reads one: gitlet.JudgeRead) — and the
// grant of the seat it reads for, which rides only beside a seat's asker
// (gitlet.ReadQuery.CeilingSeat), so the same declaration covers it — so a node
// behind the company's configuration answers none of it ([op.judgesGrant]); a
// person's or a token's consults no chart, and the engine's own reads name no
// asker at all — a `run_pushes` page, an `introduced` page, a `branch_changes`
// read, a repository's `expunge_tips`, the `compaction_due`, `publication_due`,
// `expunge_due`, `imports_due`, `mirrors_due`, `indexed`, `changes_due`,
// `checks_due`, `checks_queued` and `checks_running` listings, a
// check's box's admission's `check_box` and a push mirror's `mirror_push` — so
// each is answered there as a last resort: a check's box's admission among
// them, which a node behind the company's configuration may therefore answer,
// since the attempt's row it reads is the box's authority and no chart judges
// it. A push mirror's read is the home's, which judges the seat it pushes as
// itself (gitlet.MirrorSeatRefusal).
var opCodeRead = define("code.read", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		if b.Code == nil {
			return gitlet.ReadAnswer{}, errNoHalf
		}
		answer, err := b.Code.Read(ctx, q)
		return answer, ranNothing(err)
	}).judgesGrant(func(q gitlet.ReadQuery) bool { return q.Asker.Kind == gitlet.PrincipalAgent })

// opCodeQueueDue is a page of the queue work a repository's HOME Gitlet node
// owes (gitlet.QueueDueQuery): the repositories owing any, or one repository's
// queue work a target a page, its first page with the given-up trains'
// attempts its home cancels and the closes it owes — the read the home's merge
// queue works from (internal/gitlet/home). The engine's own, as every duty
// listing is: it names no asker and judges no grant, so a node behind the
// company's configuration answers it as a last resort too. And the engine's
// alone: its answer hands over whom each entry's enqueue was made for, so no
// package that serves a person or a model reaches it ([CodeSeam.QueueDue]).
//
// ANSWERED WHOLE ([op.answeredWhole]), because the home carries what it reads
// into the records it asks next: it lands, ejects and settles each entry as the
// entry it read, closes its change's work item as its enqueuer, and states the
// page's position as what it saw. A member a newer data node put in a page
// that this build would drop refuses the page instead, so the home's turn
// fails, stepping on nothing, and its queue waits until the home runs a build
// that declares the member. AN OPERATION OF ITS OWN, never a kind of
// [opCodeRead], whose answers a caller only shows and so reads for what its
// build declares. Across a roll the queue waits rather than step on less than
// it was answered: a home of a build that predates the operation asks
// code.read for `queue_due`, which a data node of this build refuses as a
// member it does not declare (`args.queue_due`), and a data node of such a
// build answers this operation unserved, as one it does not know, so the walk
// asks the next data node and the queue waits for one that serves it.
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

// opCodeResolveRef is one ref of one repository, asked of any data node whose
// code copy serves, at the level the query names — the read a seat's tool
// turns a branch or a tag into the object id every read by oid takes. A
// seat's resolve judges its grant on the repository as a read of it does, so
// it is declared to ([op.judgesGrant]).
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
// IDEMPOTENT BY THE REQUEST'S OP ID: the domain's operation ledger vouches for
// a repeat, and a create derives its repository's id from it
// (gitlet.RepoIDOf), so an unanswered request is asked again of the next node
// under the same id. No actor of the estate's own: the asker travels INSIDE
// the request, checked by its Validate on the node that decides.
//
// EVERY REQUEST JUDGES A GRANT from the serving node's chart — a seat's on
// the repository, and a person's create's owner — so a node behind the
// company's configuration answers none of it, asked first or asked last
// ([op.judgesGrant], a predicate true of every request): the revision it
// failed to apply may be the one that took the grant away. The lifecycle
// store's own gitlet.StoreOptions.Current holds the in-process call to the same
// rule.
var opCodeLifecycle = define("code.lifecycle", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, r gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
		if b.Code == nil {
			return gitlet.LifecycleResult{}, errNoHalf
		}
		res, err := b.Code.Lifecycle(ctx, r)
		return res, ranNothing(err)
	}).judgesGrant(func(gitlet.LifecycleRequest) bool { return true })

// opCodeGitRead is one server-side read of a repository by object id — a tree,
// a file's lines, a diff, a comparison — asked of the repository's HOME Gitlet
// node first, then its runner-up (ADR-0089), on the git lane, each attempt
// stamped with whether the node asked is the walk's effective home
// (gitlet.GitQuery.Home), the one node a walk lets fill a copy. A Gitlet node that
// cannot answer for a reason about itself (gitlet.ErrGitUnready) ran nothing
// about the repository's content, and the walk asks the next without demoting
// it ([gitUnready]). The Gitlet node judges a seat's grant itself, and refuses a
// seat where it is behind its configuration, so the operation declares no
// grant here.
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

// opCodeMergePreview is one MERGE PREVIEW — what merging one commit of a
// repository into another would do, a comparison's preview
// (gitlet.MergePreviewQuery) — asked as a read by object id is: of the
// repository's HOME Gitlet node first, then its runner-up (ADR-0089), on the git
// reads' lane, each attempt stamped with whether the node asked is the walk's
// effective home (gitlet.MergePreviewQuery.Home), the one node a walk lets fill
// a copy. The Gitlet node answers it by THE merge a repository's home records a
// change's mergeability by (internal/gitlet/home's Previews), written into a
// scratch of its own and never its cache, so a preview and the change it would
// open say one thing. A Gitlet node that cannot answer for a reason about itself
// (gitlet.ErrGitUnready) ran nothing about the repository's content, and the walk
// asks the next without demoting it ([gitUnready]). The Gitlet node judges a
// seat's grant itself, and refuses a seat where it is behind its
// configuration, so the operation declares no grant here, as a read's
// declares none.
//
// THE CODE DOMAIN'S RECORD FORMAT IS UNTOUCHED: a preview rides no log, so it
// takes no versionedFields row; the envelope evolves as every operation's
// does, additive only.
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
// cherry-pick, a branch created or deleted, which a seat or a person asks of
// a repository through a tool and the engine builds beside a copy of it
// (ADR-0092) — asked of the repository's HOME Gitlet node first, then its
// runner-up (ADR-0089): the home's copy is the one the repository's reads keep
// warm, so a write is built where its objects already are. Each attempt is
// stamped with whether the node asked is the walk's effective home
// (gitlet.EngineWrite.Home), the one node a walk lets fill a copy, as a read's
// is; on a lane of its own ([laneGitWrites]); held to [GitWriteAttempt] — or,
// for a kind whose build is given longer, its kind's attempt
// ([Timing.GitWriteAttemptFor]: a merge_change's, which lands as ONE MERGE
// RECORD rather than a push, the merge's own build budget in place of a
// push's).
//
// IDEMPOTENT BY ITS PUSH ID (gitlet.WritePushID of the write's operation): a
// home that went silent may have landed it, and the runner-up asked the same
// write builds the same push — the same commit, byte for byte — and is
// answered from the row the silent node's landing wrote, or, racing it,
// collapses into that push at the repository's committer. So it is an
// [opIdempotentWrite], asked of the next Gitlet node after one that went silent
// as after one that ran nothing. A Gitlet node that RAN NOTHING about this node's
// own state — Gitlet stopping there, or holding no epoch or no seat to sign
// the commit by (gitlet.ErrNotServing) — is declined ([ranNothing]) and asked
// last for it for [suspectFor]; one that could not build it inside its budget
// for a reason about itself (gitlet.ErrGitUnready) is moved on from without
// demotion ([gitUnready]). Every other answer, a refusal of the change or of
// the push among them, is the write's. The Gitlet node judges a seat's grant
// itself, and answers a seat `behind_config` where it is behind its
// configuration, so the operation declares no grant here, as a read declares
// none. Whom the write is made for travels inside it beside its principal,
// which the asking node holds it to before anything is asked ([codeWrites]).
//
// THE CODE DOMAIN'S RECORD FORMAT IS UNTOUCHED: nothing here rides the code
// log — what a write leaves is a push, whose proposal and row are the domain's
// own — so this operation takes no versionedFields row; the envelope evolves
// as every operation's does, additive only.
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
