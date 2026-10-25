package estate

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/code"
)

// The code host's operations: a push's proposal, and the four reads the
// host's admission, a cache's sync and the REST routes make. Every one names
// the CODE domain, so the code copy's own verdict — never the tracker's —
// decides whether a node answers it (ADR-0046), and its floor is on the code
// log. A push is the one KEYED operation: see the package doc's "A push is
// asked of its repository's committer".

// CodeServer is the code half a data node serves, declared here by its
// consumer with exactly the methods the operations below call. [code.Served]
// satisfies it.
type CodeServer interface {
	Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error)
	Resolve(ctx context.Context, q code.ResolveQuery) (code.RepoRef, error)
	Snapshot(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error)
	PushOutcome(ctx context.Context, q code.OutcomeQuery) (code.PushOutcome, error)
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
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

// proposalAnswers is how many proposals a data node answers at once, on the
// lane they have to themselves ([laneProposals]): 1 088, THE MOST ONE
// REPOSITORY'S COMMITTER HOLDS — a full queue (code.CommitterInbox) and the
// batch in flight ahead of it (code.MaxPushesPerRecord). A proposal holds its
// slot while it waits in its repository's queue and its batch runs, so a
// lane as narrow as [queue.ConcurrentAnswers] would cut a burst to one
// repository off at sixty-four, the rest waiting unseen in the broker's
// buffer behind them; as wide as a whole queue, the bound a burst meets is
// the committer's own, which answers at once — busy past a repository's count
// or bytes or the node's (code.CommitterNodeBytes), and busy the moment a
// queued push's attempt runs short — so a slot is held for long only by a
// push the committer is deciding or will decide inside its asker's attempt.
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

// opCodeRead is a person's or a surface's read of the code host: the REST
// routes' repository listing, one repository's detail and its push history,
// which the operator's CLI reads too.
var opCodeRead = define("code.read", opRead, codeDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q code.ReadQuery) (code.ReadAnswer, error) {
		if b.Code == nil {
			return code.ReadAnswer{}, errNoHalf
		}
		answer, err := b.Code.Read(ctx, q)
		return answer, ranNothing(err)
	})
