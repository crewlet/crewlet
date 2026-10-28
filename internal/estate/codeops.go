package estate

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/code"
)

// The code host's operations: a push's proposal, a compaction's, an expunge
// record's and a change's, the reads the host's admission, a cache's sync, the
// REST routes and a seat's tools make of the rows, and the listings a git node
// finds the repositories a compaction, a publication or an expunge may be due
// in by — each naming the CODE domain, so the code copy's own verdict — never
// the tracker's — decides whether a node answers it (ADR-0049), and its floor
// is on the code log — and a read of a repository's CONTENT by object id,
// which names no domain's copy and is asked of the repository's home GIT node
// ([GitCopy], ADR-0059). A push, a compaction, an expunge record and a
// change's write are keyed on their repository's data nodes: see the package
// doc's "A push is asked of its repository's committer". An operation that
// JUDGES A GRANT from the serving node's chart — a read a seat asks, and a
// seat's write of a change — says so ([op.judgesGrant]), and a node behind the
// company's configuration answers none of it.

// CodeServer is the code half a data node serves, declared here by its
// consumer with exactly the methods the operations below call. [code.Served]
// satisfies it.
type CodeServer interface {
	Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error)
	ProposeCompaction(ctx context.Context, p code.CompactionProposal) (code.CompactResult, error)
	ProposeExpunge(ctx context.Context, p code.ExpungeProposal) (code.ExpungeResult, error)
	ProposeChange(ctx context.Context, p code.ChangeProposal) (code.ChangeResult, error)
	Resolve(ctx context.Context, q code.ResolveQuery) (code.RepoRef, error)
	Snapshot(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error)
	PushOutcome(ctx context.Context, q code.OutcomeQuery) (code.PushOutcome, error)
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
	ResolveRef(ctx context.Context, q code.RefQuery) (code.RefAnswer, error)
}

// ranNothing is a code half's error as the walk reads it: a half that refused
// before anything was appended ([code.ErrNotServing] — a committer stopping, a
// node that publishes nothing, a refusal about THIS node: evicted, behind, a
// record it cannot decode, below its floor — or a half with no reader) RAN
// NOTHING, which every class moves on from with the same arguments. So it
// becomes a [declined] half, which the router and the server read as a node
// with no backend for the operation, its cause kept for the walk's account.
// Every other error is the operation's answer.
func ranNothing(err error) error {
	if errors.Is(err, code.ErrNotServing) {
		return declined{cause: err}
	}
	return err
}

// opCodePropose is a push's proposal, asked of the repository's COMMITTER — an
// IDEMPOTENT write keyed by its push id, a UUIDv7 the receiving node minted
// once, which the committer's decide collapses onto an existing push row — so
// an unanswered one is asked of the repository's runner-up with the same id.
// No actor: the principal travels INSIDE the proposal, checked by its Validate
// on the node that decides. Its caller is the host's receiver, once a push's
// bundle is stored.
var opCodePropose = define("code.propose", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p code.Proposal) (code.ProposeResult, error) {
		if b.Code == nil {
			return code.ProposeResult{}, errNoHalf
		}
		res, err := b.Code.Propose(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(func(p code.Proposal) string { return p.RepoID }).answeredOn(laneProposals)

// opCodeCompact is a compaction's proposal, asked of the repository's
// COMMITTER exactly as a push's is — an IDEMPOTENT write keyed by its
// compaction id, a bare UUIDv7 the node that built it minted once, which the
// domain's operation ledger collapses — so an unanswered one is asked of the
// repository's runner-up with the same id. On the proposals lane, because a
// compaction waits in the repository's one queue beside its pushes and holds
// its answer slot as a push's proposal does. No actor: a compaction is the
// engine's own, recorded as the deciding node's system work.
var opCodeCompact = define("code.compact", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p code.CompactionProposal) (code.CompactResult, error) {
		if b.Code == nil {
			return code.CompactResult{}, errNoHalf
		}
		res, err := b.Code.ProposeCompaction(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(func(p code.CompactionProposal) string { return p.Compaction.RepoID }).answeredOn(laneProposals)

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
	func(ctx context.Context, b Backend, _ *Actor, p code.ExpungeProposal) (code.ExpungeResult, error) {
		if b.Code == nil {
			return code.ExpungeResult{}, errNoHalf
		}
		res, err := b.Code.ProposeExpunge(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(code.ExpungeProposal.RepoID).answeredOn(laneProposals)

// opCodeChange is a write of a change's lifecycle — an open, an edit, a draft's
// state, a retarget, a close or a reopen — asked of the repository's COMMITTER
// exactly as a push's is: an IDEMPOTENT write keyed by its operation id, which
// its asker minted once and the domain's operation ledger collapses (an open's
// change id is derived from it, so no retry opens a second change), so an
// unanswered one is asked of the repository's runner-up with the same id. On
// the proposals lane, because it waits in the repository's one queue beside
// its pushes. No actor: the asker travels inside the proposal, checked by its
// Validate on the node that decides, which records it as the change's owner or
// as who wrote it.
//
// A SEAT'S WRITE JUDGES ITS GRANT from the deciding node's chart — whether it
// may push the branch an open is from, whether it maintains the repository a
// change it does not own is on — so a node behind the company's configuration
// answers none of it ([op.judgesGrant]), even asked last; a person's consults
// no chart. The committer holds the same line itself, as it does for a seat's
// push (code.CommitterOptions.Current).
var opCodeChange = define("code.change", opIdempotentWrite, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, p code.ChangeProposal) (code.ChangeResult, error) {
		if b.Code == nil {
			return code.ChangeResult{}, errNoHalf
		}
		res, err := b.Code.ProposeChange(ctx, p)
		return res, ranNothing(err)
	}).keyedBy(code.ChangeProposal.RepoID).answeredOn(laneProposals).
	judgesGrant(func(p code.ChangeProposal) bool { return p.Asker.Kind == code.PrincipalAgent })

// proposalAnswers is how many proposals a data node answers at once, on the
// lane they have to themselves ([laneProposals]): 1 088, THE MOST ONE
// REPOSITORY'S COMMITTER HOLDS — a full queue (code.CommitterInbox) and the
// batch in flight ahead of it (code.MaxPushesPerRecord); a compaction's
// proposal takes a place of that same queue, so the figure counts it already.
// A proposal holds its slot while it waits in its repository's queue and its
// batch runs, so a lane as narrow as [queue.ConcurrentAnswers] would cut a
// burst to one repository off at sixty-four, the rest waiting unseen in the
// broker's buffer behind them; as wide as a whole queue, the bound a burst
// meets is the committer's own, which answers at once — busy past a
// repository's count or bytes or the node's (code.CommitterNodeBytes), and
// busy the moment a queued push's attempt runs short — so a slot is held for
// long only by a push the committer is deciding or will decide inside its
// asker's attempt.
//
// What a slot costs is a goroutine and the proposal it carries, a kilobyte or
// two for an ordinary push: a full lane of them is a few megabytes. The
// largest push the code domain admits weighs about a mebibyte
// (code.EntryRoom: some six hundred commands at the longest ref JSON spells
// six times over — a thousand at the longest plain ref, and a full list of
// the commits it introduced, weigh about 440 KiB), and those the committer
// bounds by their bytes, so a full lane
// of large ones holds what code.CommitterNodeBytes admits and answers the
// rest busy at once. And every proposal is a push some receiving node's
// receive-pack holds open while it waits, so what reaches the lane at once is
// bounded by the fleet's transfer lanes as well.
const proposalAnswers = code.CommitterInbox + code.MaxPushesPerRecord

// opCodeResolve is a repository by its name: the host's admission of every git
// request.
var opCodeResolve = define("code.resolve", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q code.ResolveQuery) (code.RepoRef, error) {
		if b.Code == nil {
			return code.RepoRef{}, errNoHalf
		}
		ref, err := b.Code.Resolve(ctx, q)
		return ref, ranNothing(err)
	})

// opCodeRepoState is one page of a repository's snapshot — its packs and refs,
// whole or since a position: a git node's cache sync.
var opCodeRepoState = define("code.repo_state", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q code.StateQuery) (code.RepoSnapshot, error) {
		if b.Code == nil {
			return code.RepoSnapshot{}, errNoHalf
		}
		snap, err := b.Code.Snapshot(ctx, q)
		return snap, ranNothing(err)
	})

// opCodePushOutcome is what became of one push: the receiver's wait on a push
// whose record resolved pending or unknown.
var opCodePushOutcome = define("code.push_outcome", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q code.OutcomeQuery) (code.PushOutcome, error) {
		if b.Code == nil {
			return code.PushOutcome{}, errNoHalf
		}
		out, err := b.Code.PushOutcome(ctx, q)
		return out, ranNothing(err)
	})

// opCodeRead is a read of the code host by its kind (code.ReadKinds): the REST
// routes' repository listing, one repository's detail and its push history,
// which the operator's CLI reads too; a repository's refs; its pushes in log
// order, which a git node's index of which push introduced each commit is
// built from; the repositories a compaction may be due in, most live packs
// first, which a git node polls for the ones it is home for; the repositories
// a publication may be due in, least recently published first; and the
// repositories an expunge is owed in, an expunge open or a purge requested. A
// seat's read on a node that holds no chart is that node's
// code.ErrNotServing — it ran nothing — so the walk asks the next.
//
// A SEAT'S READ JUDGES ITS GRANT from the serving node's chart (which
// repositories it lists, and whether it reads one: code.JudgeRead) — and the
// grant of the seat it reads for, which rides only beside a seat's asker
// (code.ReadQuery.CeilingSeat), so the same declaration covers it — so a node
// behind the company's configuration answers none of it ([op.judgesGrant]); a
// person's or a token's consults no chart, and an `introduced` page and the
// `compaction_due`, `publication_due` and `expunge_due` listings name no asker
// at all, so each is answered there as a last resort.
var opCodeRead = define("code.read", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q code.ReadQuery) (code.ReadAnswer, error) {
		if b.Code == nil {
			return code.ReadAnswer{}, errNoHalf
		}
		answer, err := b.Code.Read(ctx, q)
		return answer, ranNothing(err)
	}).judgesGrant(func(q code.ReadQuery) bool { return q.Asker.Kind == code.PrincipalAgent })

// opCodeResolveRef is one ref of one repository, asked of any data node whose
// code copy serves, at the level the query names — the read a seat's tool
// turns a branch or a tag into the object id every read by oid takes. A
// seat's resolve judges its grant on the repository as a read of it does, so
// it is declared to ([op.judgesGrant]).
var opCodeResolveRef = define("code.resolve_ref", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q code.RefQuery) (code.RefAnswer, error) {
		if b.Code == nil {
			return code.RefAnswer{}, errNoHalf
		}
		answer, err := b.Code.ResolveRef(ctx, q)
		return answer, ranNothing(err)
	}).judgesGrant(func(q code.RefQuery) bool { return q.Asker.Kind == code.PrincipalAgent })

// opCodeGitRead is one server-side read of a repository by object id — a tree,
// a file's lines, a diff, a comparison — asked of the repository's HOME git
// node first, then its runner-up (ADR-0059), on the git lane, each attempt
// stamped with whether the node asked is the walk's effective home
// (code.GitQuery.Home), the one node a walk lets fill a copy. A git node that
// cannot answer for a reason about itself (code.ErrGitUnready) ran nothing
// about the repository's content, and the walk asks the next without demoting
// it ([gitUnready]). The git node judges a seat's grant itself, and refuses a
// seat where it is behind its configuration, so the operation declares no
// grant here.
var opCodeGitRead = define("code.git_read", opRead, GitCopy, false,
	func(ctx context.Context, b Backend, _ *Actor, q code.GitQuery) (code.GitAnswer, error) {
		if b.Git == nil {
			return code.GitAnswer{}, errNoHalf
		}
		answer, err := b.Git.Read(ctx, q)
		return answer, gitUnready(err)
	}).keyedOnGit(func(q code.GitQuery) string { return q.RepoID },
	func(q code.GitQuery, home bool) code.GitQuery {
		q.Home = home
		return q
	}).answeredOn(laneGitReads)
