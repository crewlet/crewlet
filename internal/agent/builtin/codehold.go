package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE SCRATCH HOLD'S ONE SEAM (M3-decisions M3-O61, M3-O65): a scratch
// repository idle `code.scratch.idle_days` is expired, and a turn still using
// it — running, or parked on a detached run's row — would then be refused
// `repo_deleted`. So a turn's request about a scratch repository or a seat's
// branch is sent only after [HoldRepo] or [HoldRef] answered for it, and only
// the one resolution of a code tool's `repo` ([CodeDeps.repoRead]) and the
// create (code_create_repo) call HoldRepo: TestOnlyTheSeamResolvesARepositoryForATurn
// and TestEveryCodeToolHoldsItsScratchRepositoryBeforeItsRequest hold a new tool
// to it. A hold lives in the turn's [turnctx.Holds] (memory, and the run row,
// written first, once the turn has one), which Gitlet's expiry reads before it
// decides (crewlet.code.holds); none is released before the turn ends. A key
// confirmed once read and ordered ([turnctx.Holds.Confirm]) sends nothing more.

// CodeHolder records a turn's hold — on a scratch repository, or on a seat's
// branch — on the repository's subject: the routed `code.lifecycle`, declared
// here by its consumer (estate.CodeSeam satisfies it).
type CodeHolder interface {
	Lifecycle(ctx context.Context, r gitlet.LifecycleRequest) (gitlet.LifecycleResult, error)
}

// HoldRepo takes turn's hold on a scratch repo BEFORE the request that named
// it is sent (the seam above); a project repository, a call with no turn or by
// no seat holds nothing, and a turn that keeps no holds is refused
// ([turnctx.ErrNoHolds]). A create holds the id its decide will derive
// (gitlet.RepoIDOf) and records nothing: a new repository is not idle. An
// active one idle [gitlet.HoldRecordAfter] or more by the clock read AFTER the
// Take may be past the cutoff of an expiry pass already in flight, so its hold
// is also RECORDED: a lifecycle hold on its subject, which the broker orders
// against the delete, decided at the turn's read ([gitlet.LifecycleRequest.Seen]) so no lagging
// node answers it from older rows. It records on every resolution that needs
// it, since a retry after an unknown outcome must ask again ([holdOpID] makes
// that one record). Every error refuses the request ([holdFailure]).
func HoldRepo(ctx context.Context, turn *turnctx.Turn, holder CodeHolder, actor Actor, repo gitlet.RepoRef,
	now func() time.Time) error {

	if turn == nil || repo.Kind != gitlet.RepoScratch {
		return nil
	}
	seat := CodeAskerOf(actor)
	if seat.Kind != gitlet.PrincipalAgent {
		// A TURN IS A SEAT'S: a person's call has no turn, and an actor
		// that is no seat inside one names nobody to hold for.
		return nil
	}
	holds := turn.Holding()
	if holds.Confirmed(repo.ID) {
		// CONFIRMED in this turn and process (M3-decisions M3-O65): its Take
		// and its order are done.
		return nil
	}
	if err := holds.Take(ctx, repo.ID); err != nil {
		return fmt.Errorf("builtin: hold scratch repository %s for turn %s: %w", repo.Name, turn.RunID, err)
	}
	// Only a READ of the repository (its Position set) confirms a hold: a
	// create's hold read no row, so the turn's next read confirms it. Every error
	// after the Take clears the mark.
	read := !repo.Position.IsZero()
	if repo.Status != gitlet.RepoStatusActive || wallClock(now).Sub(repo.LastActivityAt) < gitlet.HoldRecordAfter {
		if read {
			holds.Confirm(repo.ID)
		}
		return nil
	}
	if holder == nil {
		holds.Unconfirm(repo.ID)
		return errNoHolder
	}
	res, err := holder.Lifecycle(ctx, gitlet.LifecycleRequest{
		Kind: gitlet.LifecycleHold, OpID: holdOpID(turn, repo.ID), Actor: seat,
		RepoID: repo.ID, TurnID: turn.RunID, Seen: repo.Position,
	})
	switch {
	case err != nil:
		holds.Unconfirm(repo.ID)
		return fmt.Errorf("builtin: record turn %s's hold on scratch repository %s: %w", turn.RunID, repo.Name, err)
	case res.Refusal != "":
		holds.Unconfirm(repo.ID)
		return res.Err()
	case res.Outcome != statelog.OutcomeApplied && res.Outcome != statelog.OutcomePending:
		holds.Unconfirm(repo.ID)
		return fmt.Errorf("%w: Gitlet answered turn %s's hold on scratch repository %s with outcome %q",
			errHoldUnconfirmed, turn.RunID, repo.Name, res.Outcome)
	}
	// APPLIED OR PENDING: durable on the repository's subject, so the broker
	// has ordered it against the expiry's delete on every node.
	holds.Confirm(repo.ID)
	return nil
}

// HoldRef takes turn's hold on ref of repo BEFORE a request that depends on
// what it names is sent — a read of its commit, a write against it, a launch's
// clone — as [HoldRepo] does for a repository. Its only callers are the ref read
// ([CodeDeps.refsOf]), a write's branch ([CodeDeps.branch]) and a launch's new
// branch ([CodeDeps.ResolveRunRepos]): TestOnlyTheSeamResolvesARefForATurn and
// TestEveryCodeToolHoldsTheSeatRefsItNamesBeforeItsRequest. A ref
// [gitlet.Holdable] refuses — main, a tag, a person's branch — takes no hold.
// A read moves no ref and a write that moves nothing appends no record, so
// neither orders itself against a deletion: a seat's branch (seen.Seat set)
// unset [gitlet.RefHoldRecordAfter] or more is RECORDED, decided at read. seen is nil
// only for a ref not made yet, which no deletion can have listed; the turn's
// next read confirms it.
func HoldRef(ctx context.Context, turn *turnctx.Turn, holder CodeHolder, actor Actor, repo gitlet.RepoRef,
	ref string, seen *gitlet.ResolvedRef, read statelog.Position, now func() time.Time) error {

	if turn == nil || !gitlet.Holdable(ref) {
		return nil
	}
	seat := CodeAskerOf(actor)
	if seat.Kind != gitlet.PrincipalAgent {
		// A TURN IS A SEAT'S, as HoldRepo says.
		return nil
	}
	key := gitlet.RefHoldKey(repo.ID, ref)
	holds := turn.Holding()
	if holds.Confirmed(key) {
		return nil
	}
	if err := holds.Take(ctx, key); err != nil {
		return fmt.Errorf("builtin: hold %s of %s for turn %s: %w", ref, repo.Name, turn.RunID, err)
	}
	if seen == nil {
		return nil
	}
	if seen.Seat == "" || wallClock(now).Sub(seen.At) < gitlet.RefHoldRecordAfter {
		// NO RECORD NEEDED: a branch no seat set is no seat's to lose, and
		// one set within the threshold is later than the cutoff of any
		// deletion's listing in flight at this Take.
		holds.Confirm(key)
		return nil
	}
	if holder == nil {
		holds.Unconfirm(key)
		return errNoHolder
	}
	res, err := holder.Lifecycle(ctx, gitlet.LifecycleRequest{
		Kind: gitlet.LifecycleHold, OpID: holdOpID(turn, key), Actor: seat,
		RepoID: repo.ID, TurnID: turn.RunID, Ref: ref, Seen: read,
	})
	switch {
	case err != nil:
		holds.Unconfirm(key)
		return fmt.Errorf("builtin: record turn %s's hold on %s of %s: %w", turn.RunID, ref, repo.Name, err)
	case res.Refusal == gitlet.LifecycleRefDeleted:
		holds.Unconfirm(key)
		return &refDeletedError{ref: ref, oid: seen.OID, err: res.Err()}
	case res.Refusal != "":
		holds.Unconfirm(key)
		return res.Err()
	case res.Outcome != statelog.OutcomeApplied && res.Outcome != statelog.OutcomePending:
		holds.Unconfirm(key)
		return fmt.Errorf("%w: Gitlet answered turn %s's hold on %s of %s with outcome %q",
			errHoldUnconfirmed, turn.RunID, ref, repo.Name, res.Outcome)
	}
	// APPLIED OR PENDING: durable on the repository's subject, so the broker
	// has ordered it against any deletion of the ref.
	holds.Confirm(key)
	return nil
}

// takeRefHolds takes, in ONE Take, turn's hold on each of refs [HoldRef] would
// hold and the turn has not confirmed, so a call naming several seats' branches
// (a comparison's base and head, a change's source and target) writes the run
// row in one compare-and-set rather than one each; each HoldRef then finds its
// key held. It holds nothing where HoldRef would not, and answers the refs it
// was taking, joined, for a refusal to name.
func takeRefHolds(ctx context.Context, turn *turnctx.Turn, actor Actor, repo gitlet.RepoRef,
	refs []string) (string, error) {

	if turn == nil || len(refs) == 0 || CodeAskerOf(actor).Kind != gitlet.PrincipalAgent {
		return "", nil
	}
	holds := turn.Holding()
	var keys, names []string
	for _, ref := range refs {
		if !gitlet.Holdable(ref) {
			continue
		}
		if key := gitlet.RefHoldKey(repo.ID, ref); !holds.Confirmed(key) {
			keys, names = append(keys, key), append(names, ref)
		}
	}
	if len(keys) == 0 {
		return "", nil
	}
	taking := strings.Join(names, ", ")
	if err := holds.Take(ctx, keys...); err != nil {
		return taking, fmt.Errorf("builtin: hold %s of %s for turn %s: %w", taking, repo.Name, turn.RunID, err)
	}
	return taking, nil
}

// refDeletedError is a hold on a seat's branch Gitlet answered ref_deleted:
// deleted after the turn read it at oid, the commit to restore it at.
type refDeletedError struct {
	ref, oid string
	err      error
}

func (e *refDeletedError) Error() string {
	return fmt.Sprintf("builtin: %s was deleted after this turn read it at %s: %v", e.ref, e.oid, e.err)
}

func (e *refDeletedError) Unwrap() error { return e.err }

// errHoldUnconfirmed is a hold record whose outcome the turn could not learn.
// NOT ErrOutcomeUnknown: the record is the engine's own ordering, not the
// call's write, and the call sent nothing, so it is refused as unavailable and
// the same call made again finishes it ([holdOpID]).
var errHoldUnconfirmed = errors.New("builtin: Gitlet could not confirm this turn's hold")

// errNoHolder is a hold that needs recording on a surface wired with no
// holder: an assembly fault, refused rather than the request sent unordered
// against an expiry in flight.
var errNoHolder = errors.New("builtin: this surface records no hold on a scratch repository " +
	"(CodeDeps.Holder is nil)")

// holdOpID is a turn's hold record's operation: ONE PER RUN AND KEY (a scratch
// repository's id, or a [gitlet.RefHoldKey]), so the hold asked again in the
// run is one record and two keys are two. At most 111 bytes (36, a dot, "hold-"
// and 69, a ref's key being fixed-width), inside statelog.MaxCallerOpIDBytes.
// THE RUN, never the work key ([Actor.OperationSeed]): a turn redelivered after
// a crash runs under a new run id and the same work key, and would be answered
// from the first run's `applied` record, ordered against no pass in flight at
// its own Take. Minted at the run's start ([runIdentity]), not the work's, which
// may predate the ledger's watermark; an attempt resumed past statelog.OpsRetention
// would be answered `unknown` for good ([statelog.MintAt]), so one the engine
// REBASED ([turnctx.Turn.RebasedTo]) mints at the rebase: a second record,
// ordered against the passes in flight at THIS attempt's Take.
func holdOpID(turn *turnctx.Turn, key string) string {
	run := runIdentity(turn)
	return statelog.DeriveOpID(run.OperationSince(), "hold-"+key, holdOpNamespace, run.TurnID, key)
}

// holdOpNamespace keeps a hold record's operation apart from any other derived
// from the same run. FIXED: changing it makes a run straddling the change record
// its holds twice.
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
func holdFailure(ctx context.Context, tool string, repo gitlet.RepoRef, err error) tools.Result {
	switch {
	case errors.Is(err, turnctx.ErrHoldsFull), errors.Is(err, sandbox.ErrCodeHoldsFull):
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s did not run: this turn already holds "+
			"the most repositories and branches one turn may (%d), and it reaches a scratch repository only "+
			"while it holds it, so nothing was sent about %s. Finish this work in a later turn.", tool,
			turnctx.MaxHolds, repo.Name))
	case errors.Is(err, gitlet.ErrRepoNotFound), errors.Is(err, gitlet.ErrNotPermitted):
		// THE DOMAIN'S OWN ANSWER, in its words: deleted by the expiry first,
		// or not readable by the seat.
		return codeFailure(ctx, tool, repo.Name, err)
	case errors.Is(err, turnctx.ErrNoHolds), errors.Is(err, errNoHolder):
		return faulted(ctx, tool, err, fmt.Sprintf("%s could not keep scratch repository %s from expiring "+
			"while this turn uses it, so it sent nothing about it: %s.", tool, repo.Name, faultSaid))
	}
	if why, ok := Condition(err); ok || errors.Is(err, errHoldUnconfirmed) || errors.Is(err, gitlet.ErrNotServing) {
		if !ok {
			why = "the record of this turn's hold did not reach the code log in time"
		}
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s did not run: Gitlet could not "+
			"confirm this turn's hold on scratch repository %s (%s), and a repository is reached only while "+
			"the turn holds it, so nothing was sent about it. Make the same call again.", tool, repo.Name, why))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not keep scratch repository %s from expiring while "+
		"this turn uses it, so it sent nothing about it: %s.", tool, repo.Name, faultSaid))
}

// refHoldFailure explains a call refused because its turn's hold on ref of repo
// could not be taken or recorded ([HoldRef]): nothing that depends on the ref
// was sent. Each failure keeps the class [holdFailure] gives it; a branch
// deleted after the read is not_found, as a deleted repository is.
func refHoldFailure(ctx context.Context, tool string, repo gitlet.RepoRef, ref string, err error) tools.Result {
	var deleted *refDeletedError
	switch {
	case errors.As(err, &deleted):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s did not run: %s of repository %s was "+
			"deleted after this turn read it at %s — by a push, or by Gitlet's ref lifecycle, which "+
			"deletes a seat's branch nobody has set for %d days — so nothing was sent about it. Restore it "+
			"with %s at %s if the work on it is still needed.", tool, ref, repo.Name, deleted.oid,
			int(gitlet.RefExpiry/(24*time.Hour)), CodeCreateBranchTool, deleted.oid))
	case errors.Is(err, turnctx.ErrHoldsFull), errors.Is(err, sandbox.ErrCodeHoldsFull):
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s did not run: this turn already holds "+
			"the most repositories and branches one turn may (%d), and it reaches a seat's branch only while it "+
			"holds it, so nothing was sent about %s. Finish this work in a later turn.", tool, turnctx.MaxHolds,
			ref))
	case errors.Is(err, gitlet.ErrRepoNotFound), errors.Is(err, gitlet.ErrNotPermitted):
		// THE DOMAIN'S OWN ANSWER to the hold, in its own words.
		return codeFailure(ctx, tool, repo.Name, err)
	case errors.Is(err, turnctx.ErrNoHolds), errors.Is(err, errNoHolder):
		return faulted(ctx, tool, err, fmt.Sprintf("%s could not hold %s of repository %s for this turn, so "+
			"it sent nothing about it: %s.", tool, ref, repo.Name, faultSaid))
	}
	if why, ok := Condition(err); ok || errors.Is(err, errHoldUnconfirmed) || errors.Is(err, gitlet.ErrNotServing) {
		if !ok {
			why = "the record of this turn's hold did not reach the code log in time"
		}
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s did not run: Gitlet could not "+
			"confirm this turn's hold on %s of repository %s (%s), so nothing was sent about it. Make the same "+
			"call again.", tool, ref, repo.Name, why))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not hold %s of repository %s for this turn, so it "+
		"sent nothing about it: %s.", tool, ref, repo.Name, faultSaid))
}
