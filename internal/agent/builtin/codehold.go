package builtin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE SCRATCH HOLD'S ONE SEAM: a turn's hold on a scratch repository it names,
// taken BEFORE the request that named it is sent (M3-decisions M3-O61).
//
// A seat's scratch repository is expired once it has sat idle for
// `code.scratch.idle_days`, and a turn that has named one may still be using
// it — running, or parked on a detached run's row — long after its last push.
// Expired under such a turn, the turn's next request about it is refused
// `repo_deleted`, whatever a restore could do later. So every request a turn
// makes about a repository is sent only after [HoldRepo] has answered for it,
// and its callers are few by construction: the ONE resolution of a code tool's
// `repo` argument ([CodeDeps.repoRead], which every code tool's repository
// passes through — the reads, the writes and the lifecycle tools alike), and
// the lifecycle create, on the id its decide will derive (code_create_repo).
// A static test refuses a second resolution of a repository name in this
// package (TestOnlyTheSeamResolvesARepositoryForATurn), and a dynamic one
// holds every code tool to its hold before its first request
// (TestEveryCodeToolHoldsItsScratchRepositoryBeforeItsRequest), so a tool added
// later is held to the rule without a line of its own.
//
// # Where the hold is kept, and who reads it
//
// In the turn's own [turnctx.Holds] — the process's memory while the turn
// runs, and the turn's run row once it has one — which the code host's
// expiry asks of every row and every seat holder before it decides (the
// engine's crewlet.code.holds). Nothing here releases a hold: a turn's set
// only grows, and ends with the turn.
//
// # And when it is RECORDED
//
// A hold the expiry could not see — taken after a pass in flight read its
// holds — is safe for a repository idle less than [code.HoldRecordAfter] by
// the clock read after the hold was taken: that pass fixed its cutoff before
// it read its holds, so before this hold, and a day or more before it, and the
// repository's last activity is later than any such cutoff. One idle longer
// could be past that cutoff, so its hold is also RECORDED — a lifecycle
// request of kind hold on the repository's own subject, which the expiry's
// delete arbitrates on too: landed first, it refuses the delete; landed after
// it, its decide answers the repository deleted and the turn's request is
// refused before it reads anything. Decided at the turn's own read of the
// repository ([code.LifecycleRequest.Seen]), so a data node behind that read
// never answers it from older rows.

// CodeHolder records a turn's hold on a scratch repository's subject: the
// routed `code.lifecycle`, declared here by its consumer (estate.CodeSeam
// satisfies it).
type CodeHolder interface {
	Lifecycle(ctx context.Context, r code.LifecycleRequest) (code.LifecycleResult, error)
}

// HoldRepo takes turn's hold on repo BEFORE the request that named it is sent
// — the one seam the package doc describes. A project repository never
// expires and takes no hold, and an operator's call, which has no turn, holds
// nothing; a turn built outside the engine, which keeps no holds, is refused
// rather than sent to a scratch repository unheld ([turnctx.ErrNoHolds]).
//
// The repository's id goes into the turn's [turnctx.Holds] — written first
// to its run row where the turn has one. A create's repository is held on the
// id its decide will derive (code.RepoIDOf), before the create is sent: it
// carries no status, since nothing has been made, so nothing is recorded —
// a new repository is not idle. Then, where the repository is active
// and its last activity is at least [code.HoldRecordAfter] before now — the
// clock read AFTER the hold was taken — the hold is RECORDED on its subject:
// a lifecycle request of kind hold, as actor's seat, decided at the position
// the turn read the repository at (repo.Position, [code.LifecycleRequest.Seen]).
//
// RECORDED ON EVERY RESOLUTION THAT NEEDS ONE, not only on a hold this call
// took new: a record whose outcome the turn could not learn refuses the call,
// the hold it took stays in the set, and the call made again — a resolution
// again, since nothing was kept of the first — must ask for the record again,
// which a hold already in the set could not tell it to. The same record asked
// twice is one record ([holdOpID]); a resolution the turn already keeps takes
// no hold and sends nothing ([CodeDeps.repoRead]), so a turn pays at most one
// record round trip per scratch repository it names.
//
// Every error refuses the request, which then reads and writes nothing: a
// hold the set or the run row refused ([turnctx.ErrHoldsFull],
// [sandbox.ErrCodeHoldsFull], a row gone or ending); a record refused — the
// repository deleted ([code.ErrRepoNotFound], in the deletion's own words) or
// not readable by the seat ([code.ErrNotPermitted]); a record nothing served
// or whose outcome is unknown.
func HoldRepo(ctx context.Context, turn *turnctx.Turn, holder CodeHolder, actor Actor, repo code.RepoRef,
	now func() time.Time) error {

	if turn == nil || repo.Kind != code.RepoScratch {
		return nil
	}
	seat := CodeAskerOf(actor)
	if seat.Kind != code.PrincipalAgent {
		// A TURN IS A SEAT'S, and only a seat's turn holds anything: a
		// person's call has no turn, and an actor that is no seat inside
		// one is a wiring that names nobody to hold for.
		return nil
	}
	if _, err := turn.Holding().Take(ctx, repo.ID); err != nil {
		return fmt.Errorf("builtin: hold scratch repository %s for turn %s: %w", repo.Name, turn.RunID, err)
	}
	if repo.Status != code.RepoStatusActive || wallClock(now).Sub(repo.LastActivityAt) < code.HoldRecordAfter {
		return nil
	}
	if holder == nil {
		return errNoHolder
	}
	res, err := holder.Lifecycle(ctx, code.LifecycleRequest{
		Kind: code.LifecycleHold, OpID: holdOpID(turn.RunID, repo.ID), Actor: seat,
		RepoID: repo.ID, TurnID: turn.RunID, Seen: repo.Position,
	})
	switch {
	case err != nil:
		return fmt.Errorf("builtin: record turn %s's hold on scratch repository %s: %w", turn.RunID, repo.Name, err)
	case res.Refusal != "":
		return res.Err()
	case res.Outcome != statelog.OutcomeApplied && res.Outcome != statelog.OutcomePending:
		return fmt.Errorf("%w: the code host answered turn %s's hold on scratch repository %s with outcome %q",
			errHoldUnconfirmed, turn.RunID, repo.Name, res.Outcome)
	}
	// APPLIED OR PENDING: the record is durable on the repository's subject,
	// so the broker has ordered it against the expiry's delete whichever
	// node applies the two.
	return nil
}

// errHoldUnconfirmed is a hold record whose outcome the turn could not learn.
// NOT the tool contract's unknown outcome (ErrOutcomeUnknown): the record is
// the engine's own ordering, never the call's write, and the call sent
// nothing — so the call is refused as a node that could not serve it, which
// the same call made again finishes, the record being one per run and
// repository ([holdOpID]).
var errHoldUnconfirmed = errors.New("builtin: the code host could not confirm this turn's hold")

// errNoHolder is a hold that needs recording on a surface wired with no
// holder: an assembly fault, refused rather than the request sent unordered
// against an expiry in flight.
var errNoHolder = errors.New("builtin: this surface records no hold on a scratch repository " +
	"(CodeDeps.Holder is nil)")

// holdOpID is the operation a turn's hold record on a repository is: ONE PER
// RUN AND REPOSITORY, so the same hold asked again in the run is one record,
// and the ledger answers the repeat.
//
// THE RUN, NEVER THE UNIT OF WORK a write's operation is seeded from
// ([Actor.OperationSeed]): a turn redelivered after a crash runs again under a
// new run id and the same work key, and a hold record derived from the key
// would be answered from the FIRST run's record by the ledger — `applied`, at
// a position every pass begun since that record is past, so the re-run's hold
// would be ordered against no pass in flight at its own Take, which is the
// one thing a record is for. Minted at the run's own instant, which its id
// carries (run ids are time-ordered), so the ledger judges it against the
// run's start rather than against the work's, which may be older than its
// watermark; and named after the repository, so two repositories one run
// holds are two operations.
func holdOpID(runID, repoID string) string {
	at, _ := statelog.OpMintedAt(runID)
	return statelog.DeriveOpID(at, "hold-"+repoID, holdOpNamespace, runID, repoID)
}

// holdOpNamespace keeps a hold record's operation from colliding with one any
// other package derives from the same run. FIXED for the life of the format:
// changing it makes a run straddling the change record its holds twice.
const holdOpNamespace = "crewlet.builtin.code.hold"

// wallClock is now, or the wall clock where now is nil.
func wallClock(now func() time.Time) time.Time {
	if now == nil {
		return time.Now()
	}
	return now()
}

// holdFailure explains a call refused because its turn's hold on repo could not
// be taken or recorded: nothing about the repository was sent.
func holdFailure(ctx context.Context, tool string, repo code.RepoRef, err error) tools.Result {
	switch {
	case errors.Is(err, turnctx.ErrHoldsFull), errors.Is(err, sandbox.ErrCodeHoldsFull):
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s did not run: this turn already holds "+
			"the most scratch repositories one turn may (%d), and it reaches a scratch repository only "+
			"while it holds it, so nothing was sent about %s. Finish this work in a later turn.", tool,
			turnctx.MaxHolds, repo.Name))
	case errors.Is(err, code.ErrRepoNotFound), errors.Is(err, code.ErrNotPermitted):
		// THE DOMAIN'S OWN ANSWER to the hold — a repository the expiry
		// deleted first, or one the seat may not read — in its own words.
		return codeFailure(ctx, tool, repo.Name, err)
	case errors.Is(err, turnctx.ErrNoHolds), errors.Is(err, errNoHolder):
		return faulted(ctx, tool, err, fmt.Sprintf("%s could not keep scratch repository %s from expiring "+
			"while this turn uses it, so it sent nothing about it: %s.", tool, repo.Name, faultSaid))
	}
	if why, ok := Condition(err); ok || errors.Is(err, errHoldUnconfirmed) || errors.Is(err, code.ErrNotServing) {
		if !ok {
			why = "the record of this turn's hold did not reach the code log in time"
		}
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s did not run: the code host could not "+
			"confirm this turn's hold on scratch repository %s (%s), and a repository is reached only while "+
			"the turn holds it, so nothing was sent about it. Make the same call again.", tool, repo.Name, why))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not keep scratch repository %s from expiring while "+
		"this turn uses it, so it sent nothing about it: %s.", tool, repo.Name, faultSaid))
}
