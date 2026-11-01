package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
//
// # A seat's branch, by the same rule
//
// A seat's branch — a ref under refs/heads/seats/<handle>/, the only refs whose
// row names the seat that set it — is held the same way, through its own seam
// ([HoldRef]) and its own key ([code.RefHoldKey]) in the same set and on the
// same row: every ref a turn READS through a code tool, every branch it
// WRITES, whether or not the write moves it, and every ref a coding run's
// launch resolves. A read moves no ref, and a write that moves nothing appends
// no record, so neither orders itself against a deletion of the branch; so a
// hold on a branch unset [code.RefHoldRecordAfter] or more is RECORDED, as an
// idle scratch repository's is, and decided at the turn's read of the ref.
//
// # Confirmed once a turn
//
// A seam that took a hold having READ what it holds, and found it ordered —
// no record needed, or the record applied — CONFIRMS its key in the turn's
// set ([turnctx.Holds.Confirm], M3-decisions M3-O65), and sends nothing more
// for it in this turn and process: a write naming a branch the turn's read
// already confirmed reads nothing more before it is sent ([CodeDeps.branch]).

// CodeHolder records a turn's hold — on a scratch repository, or on a seat's
// branch — on the repository's subject: the routed `code.lifecycle`, declared
// here by its consumer (estate.CodeSeam satisfies it).
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
// CONFIRMED ONCE A TURN AND PROCESS (M3-decisions M3-O65), so a resolution
// made again — the turn's lookups forgotten — sends nothing either: a key the
// turn has confirmed answers nil at once, with no Take and no record. HoldRepo
// confirms the repository's id when it returns nil having been handed a READ
// of it (a resolution's RepoRef, its Position set), its Take done, and either
// no record needed (archived, or active within [code.HoldRecordAfter]) or the
// record applied; a create's hold, on an id no row was read for, confirms
// nothing. Every error after the Take clears the mark.
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
	holds := turn.Holding()
	if holds.Confirmed(repo.ID) {
		// CONFIRMED IN THIS TURN AND PROCESS (M3-decisions M3-O65): its Take
		// and its order are done, and nothing more is sent.
		return nil
	}
	if err := holds.Take(ctx, repo.ID); err != nil {
		return fmt.Errorf("builtin: hold scratch repository %s for turn %s: %w", repo.Name, turn.RunID, err)
	}
	// A READ OF THE REPOSITORY is what a hold is confirmed from: a
	// resolution's RepoRef, at the position it was read at. A create's hold,
	// on the id its decide will derive, read no row — nothing could have
	// listed what does not exist yet — so it confirms nothing, and the turn's
	// next read of the repository confirms it.
	read := !repo.Position.IsZero()
	if repo.Status != code.RepoStatusActive || wallClock(now).Sub(repo.LastActivityAt) < code.HoldRecordAfter {
		if read {
			holds.Confirm(repo.ID)
		}
		return nil
	}
	if holder == nil {
		holds.Unconfirm(repo.ID)
		return errNoHolder
	}
	res, err := holder.Lifecycle(ctx, code.LifecycleRequest{
		Kind: code.LifecycleHold, OpID: holdOpID(turn, repo.ID), Actor: seat,
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
		return fmt.Errorf("%w: the code host answered turn %s's hold on scratch repository %s with outcome %q",
			errHoldUnconfirmed, turn.RunID, repo.Name, res.Outcome)
	}
	// APPLIED OR PENDING: the record is durable on the repository's subject,
	// so the broker has ordered it against the expiry's delete whichever
	// node applies the two.
	holds.Confirm(repo.ID)
	return nil
}

// HoldRef takes turn's hold on ref of repo BEFORE the request that depends on
// what ref names is sent — a git read of the commit it resolved to, a write
// proposed against it, a launch's clone of it. THE SEAM FOR A REF, as
// [HoldRepo] is for a repository, and the rule it keeps is one: a request
// that depends on what a seat's branch names is sent only after HoldRef has
// answered for it. Its callers are the ONE ref read of this package
// ([CodeDeps.refsOf], under every ref a code tool resolves and every ref a
// coding run's launch does), a write tool's branch ([CodeDeps.branch]) and the
// launch's branch that does not exist yet ([CodeDeps.ResolveRunRepos]) — and
// nothing else in a turn names a ref (TestOnlyTheSeamResolvesARefForATurn,
// TestEveryCodeToolHoldsTheSeatRefsItNamesBeforeItsRequest).
//
// A ref [code.Holdable] refuses — main, a tag, a person's branch — keeps
// nothing and takes no hold, and an operator's call, which has no turn, holds
// nothing; a turn built outside the engine, which keeps no holds, is refused
// rather than sent through a seat's branch unheld ([turnctx.ErrNoHolds]).
//
// The key ([code.RefHoldKey]) goes into the turn's [turnctx.Holds] — written
// first to its run row where the turn has one; the ref read takes every key of
// its answer in one Take first ([takeRefHolds]), so its HoldRefs find theirs
// held and write nothing. Then, where seen — the ref's
// row as the turn's read answered it, at the checkpoint read — is a SEAT'S
// branch (seen.Seat set) last set at least [code.RefHoldRecordAfter] before
// now, the clock read AFTER the hold was taken, the hold is RECORDED on the
// repository's subject: a lifecycle request of kind hold naming the ref,
// decided at the position the turn read it at, as one operation per run and
// key ([holdOpID]). seen is nil only for a ref that does not exist yet — a
// branch a write creates, or a run's branch its box will create — which no
// deletion can have listed: the write or the push that makes it sets it now.
//
// CONFIRMED ONCE A TURN AND PROCESS (M3-decisions M3-O65): a key the turn has
// confirmed answers nil at once and sends nothing — no Take, no record. HoldRef
// confirms the key when it returns nil having been handed seen, its Take done,
// and either no record needed or the record applied; with seen nil it confirms
// nothing, and the turn's next read of the ref confirms it. Every error after
// the Take clears the key's mark.
//
// Every error refuses the request, which then reads and writes nothing
// through the ref: a hold the set or the run row refused ([turnctx.ErrHoldsFull],
// [sandbox.ErrCodeHoldsFull], a row gone or ending); a record refused — the
// ref deleted after the turn read it ([refDeletedError], naming the commit the
// read answered so the branch can be restored at it), the repository deleted
// ([code.ErrRepoNotFound]) or not readable by the seat ([code.ErrNotPermitted]);
// a record nothing served or whose outcome is unknown.
func HoldRef(ctx context.Context, turn *turnctx.Turn, holder CodeHolder, actor Actor, repo code.RepoRef,
	ref string, seen *code.ResolvedRef, read statelog.Position, now func() time.Time) error {

	if turn == nil || !code.Holdable(ref) {
		return nil
	}
	seat := CodeAskerOf(actor)
	if seat.Kind != code.PrincipalAgent {
		// A TURN IS A SEAT'S, as HoldRepo says.
		return nil
	}
	key := code.RefHoldKey(repo.ID, ref)
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
	if seen.Seat == "" || wallClock(now).Sub(seen.At) < code.RefHoldRecordAfter {
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
	res, err := holder.Lifecycle(ctx, code.LifecycleRequest{
		Kind: code.LifecycleHold, OpID: holdOpID(turn, key), Actor: seat,
		RepoID: repo.ID, TurnID: turn.RunID, Ref: ref, Seen: read,
	})
	switch {
	case err != nil:
		holds.Unconfirm(key)
		return fmt.Errorf("builtin: record turn %s's hold on %s of %s: %w", turn.RunID, ref, repo.Name, err)
	case res.Refusal == code.LifecycleRefDeleted:
		holds.Unconfirm(key)
		return &refDeletedError{ref: ref, oid: seen.OID, err: res.Err()}
	case res.Refusal != "":
		holds.Unconfirm(key)
		return res.Err()
	case res.Outcome != statelog.OutcomeApplied && res.Outcome != statelog.OutcomePending:
		holds.Unconfirm(key)
		return fmt.Errorf("%w: the code host answered turn %s's hold on %s of %s with outcome %q",
			errHoldUnconfirmed, turn.RunID, ref, repo.Name, res.Outcome)
	}
	// APPLIED OR PENDING: durable on the repository's subject, so the broker
	// has ordered it against any deletion of the ref.
	holds.Confirm(key)
	return nil
}

// takeRefHolds takes, in ONE Take, turn's hold on every ref of refs that
// [HoldRef] would take one on — each [code.Holdable] admits and the turn has
// not confirmed — before the HoldRef of each, which then finds its key held
// and goes straight on to its record and its confirmation. So a call that
// depends on several seats' branches — a comparison's base and head, a commit
// and the branch it is asked to have landed in, a change's source and target
// — writes them on the turn's run row in one compare-and-set rather than one
// each. Its callers are the ref read, with every ref its answer holds
// ([CodeDeps.refsOf]), and a write's branches, with those it holds with no
// read ([CodeDeps.branch]). What HoldRef takes nothing for — an operator's
// call, a person's, no turn — it takes nothing for either. It answers the
// refs it was taking, joined, for a refusal to name.
func takeRefHolds(ctx context.Context, turn *turnctx.Turn, actor Actor, repo code.RepoRef,
	refs []string) (string, error) {

	if turn == nil || len(refs) == 0 || CodeAskerOf(actor).Kind != code.PrincipalAgent {
		return "", nil
	}
	holds := turn.Holding()
	var keys, names []string
	for _, ref := range refs {
		if !code.Holdable(ref) {
			continue
		}
		if key := code.RefHoldKey(repo.ID, ref); !holds.Confirmed(key) {
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

// refDeletedError is a hold on a seat's branch the code host answered
// ref_deleted: the branch was deleted after the turn read it at oid, so the
// request that depended on it is refused before it reads or writes anything,
// and the answer names the commit to restore the branch at.
type refDeletedError struct {
	ref, oid string
	err      error
}

func (e *refDeletedError) Error() string {
	return fmt.Sprintf("builtin: %s was deleted after this turn read it at %s: %v", e.ref, e.oid, e.err)
}

func (e *refDeletedError) Unwrap() error { return e.err }

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

// holdOpID is the operation a turn's hold record is: ONE PER RUN AND KEY — a
// scratch repository's id, or a seat branch's [code.RefHoldKey] — so the same
// hold asked again in the run is one record, and the ledger answers the
// repeat. A ref's key is fixed-width, so the id is at most 111 bytes (36, a
// dot, "hold-" and 69), inside statelog.MaxCallerOpIDBytes however long the
// ref.
//
// THE RUN, NEVER THE UNIT OF WORK a write's operation is seeded from
// ([Actor.OperationSeed]): a turn redelivered after a crash runs again under a
// new run id and the same work key, and a hold record derived from the key
// would be answered from the FIRST run's record by the ledger — `applied`, at
// a position every pass begun since that record is past, so the re-run's hold
// would be ordered against no pass in flight at its own Take, which is the
// one thing a record is for. Named after the key, so two repositories or two
// branches one run holds are two operations.
//
// MINTED WHERE THE RUN'S OWN DERIVED IDS ARE ([runIdentity]): at the run's
// start, which its id carries (run ids are time-ordered), so the ledger judges
// it against the run's start rather than against the work's, which may be
// older than its watermark — and on an attempt the engine REBASED
// ([turnctx.Turn.RebasedTo]), at the rebase. A turn resumed longer after its
// run began than the operation ledger remembers (statelog.OpsRetention, the
// code log's too) finds every repository it holds idle past
// [code.HoldRecordAfter], and every seat branch it holds set longer ago than
// [code.RefHoldRecordAfter], so it records its hold on each one it names
// again; minted at the run's start, before what every node's ledger has
// swept, each record is answered `unknown` on every node for good
// ([statelog.MintAt]'s reason), and every call the resumed turn makes about
// the key is refused, the same call made again asking the same record. Minted
// at the rebase, a rebased attempt's record is a second record of the run's
// hold, which is what it must be: the first record's row is gone, and the
// passes it orders against are the ones in flight at THIS attempt's Take.
func holdOpID(turn *turnctx.Turn, key string) string {
	run := runIdentity(turn)
	return statelog.DeriveOpID(run.OperationSince(), "hold-"+key, holdOpNamespace, run.TurnID, key)
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
			"the most repositories and branches one turn may (%d), and it reaches a scratch repository only "+
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

// refHoldFailure explains a call refused because its turn's hold on ref of repo
// could not be taken or recorded ([HoldRef]): nothing that depends on the ref
// was sent. Each refusal takes the class its kind of failure already has
// ([HoldRepo]'s, and the code tools' own): the branch deleted after the turn
// read it is not_found, as a deleted repository is; a hold the code host could
// not confirm is unavailable; a turn at its bound is forbidden.
func refHoldFailure(ctx context.Context, tool string, repo code.RepoRef, ref string, err error) tools.Result {
	var deleted *refDeletedError
	switch {
	case errors.As(err, &deleted):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s did not run: %s of repository %s was "+
			"deleted after this turn read it at %s — by a push, or by the code host's ref lifecycle, which "+
			"deletes a seat's branch nobody has set for %d days — so nothing was sent about it. Restore it "+
			"with %s at %s if the work on it is still needed.", tool, ref, repo.Name, deleted.oid,
			int(code.RefExpiry/(24*time.Hour)), CodeCreateBranchTool, deleted.oid))
	case errors.Is(err, turnctx.ErrHoldsFull), errors.Is(err, sandbox.ErrCodeHoldsFull):
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s did not run: this turn already holds "+
			"the most repositories and branches one turn may (%d), and it reaches a seat's branch only while it "+
			"holds it, so nothing was sent about %s. Finish this work in a later turn.", tool, turnctx.MaxHolds,
			ref))
	case errors.Is(err, code.ErrRepoNotFound), errors.Is(err, code.ErrNotPermitted):
		// THE DOMAIN'S OWN ANSWER to the hold, in its own words.
		return codeFailure(ctx, tool, repo.Name, err)
	case errors.Is(err, turnctx.ErrNoHolds), errors.Is(err, errNoHolder):
		return faulted(ctx, tool, err, fmt.Sprintf("%s could not hold %s of repository %s for this turn, so "+
			"it sent nothing about it: %s.", tool, ref, repo.Name, faultSaid))
	}
	if why, ok := Condition(err); ok || errors.Is(err, errHoldUnconfirmed) || errors.Is(err, code.ErrNotServing) {
		if !ok {
			why = "the record of this turn's hold did not reach the code log in time"
		}
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s did not run: the code host could not "+
			"confirm this turn's hold on %s of repository %s (%s), so nothing was sent about it. Make the same "+
			"call again.", tool, ref, repo.Name, why))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not hold %s of repository %s for this turn, so it "+
		"sent nothing about it: %s.", tool, ref, repo.Name, faultSaid))
}
