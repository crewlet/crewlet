package sandbox

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// CoordStore is the pending-run store, on the FLEET's coordination store.
//
// The one implementation, and the node's own database is deliberately not an
// option. A detached run OUTLIVES its turn, its process and sometimes its
// node: when the seat moves — a lease lapse, a drain, a rolling upgrade — the
// node that owns it next is the one whose recovery pass has to find the run.
// On a per-node store that pass found nothing, so the suspended Execute
// conversation was unreachable and the sandbox was neither resumed nor reaped:
// a billed box, running to its own TTL, handing its result to nobody. The
// release path's own comment states the contract this restores — "a detached
// run belongs to its row, not to this process, and the seat's next owner
// recovers it through RecoverSeat" — which was only ever true of a row the
// successor could read.
//
// A single-node company is not a special case: it runs the in-memory
// coordination twin, which is a real implementation of the same certified
// contract rather than a stub.
//
// # A mutex became a compare-and-swap
//
// Every mutation here is a CONDITIONAL FLIP whose condition is one of the
// run's own fields — the at-most-once tail claim on the status, the epoch
// fence on a write, the pause expiry on "is it still parked". Within one
// process a mutex made those atomic; across a fleet the condition has to be
// re-evaluated against what the store actually holds, so each is a
// read-decide-write under the record's version, retried on a lost race. A
// caller that loses re-reads, which is what makes the SECOND writer see the
// first one's decision instead of overwriting it.
type CoordStore struct {
	runs coord.SandboxRuns
	now  func() time.Time
}

// NewCoordStore wraps the fleet's run records.
func NewCoordStore(runs coord.SandboxRuns) *CoordStore {
	return &CoordStore{runs: runs}
}

var _ PendingStore = (*CoordStore)(nil)

// WithClock pins the clock, for a suite that asserts about ages.
func (s *CoordStore) WithClock(now func() time.Time) *CoordStore {
	s.now = now
	return s
}

func (s *CoordStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// casRetries bounds one read-decide-write.
//
// Sixteen, matching the coordination store's own loops. Contention here is a
// seat's own turns plus a recovering peer — two or three writers at the very
// worst — so exhausting this many rounds is a store that is not settling
// rather than a queue of legitimate writers, and it is reported as an error
// instead of as a lost race: a caller told "somebody else got there" stops,
// while a caller told "no answer" retries.
const casRetries = 16

// BeginLaunch opens a launch on this turn's row. See the contract on
// [PendingStore].
//
// A ROW THAT VANISHES BETWEEN THE CREATE AND THE RESET is a row there is none
// of, so the create is tried again: the turn's previous run finishing as this
// one opens ends the row underneath the reset, and a reset that found nothing
// used to report the launch open on a row that did not exist.
func (s *CoordStore) BeginLaunch(ctx context.Context, run PendingRun, fence Fence) (PendingRun, error) {
	if run.TurnID == "" {
		return PendingRun{}, fmt.Errorf("sandbox: a pending run needs a turn id")
	}
	now := s.clock()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	// Not the caller's to choose: a row exists to be launched into, and
	// the only status that can mean is launching.
	run.Status = StatusLaunching
	// OWNED BY THE LEASE THAT LAUNCHED IT — see [PendingStore.BeginLaunch].
	if fence.Fenced() {
		run.Owner, run.OwnerEpoch = fence.Owner, fence.Epoch
	}
	// Nor is the name of the job, and it is new on every launch, the
	// reset below included: a completion claims only the job it names.
	run.LaunchID = uuid.NewString()
	run.UpdatedAt = now
	// And the job's own record starts here, keyed on that name: the
	// instant the launch exists is the instant its phase began, and a
	// previous job's record — its start, its iteration, whether its phase
	// was published — is not this one's. Only the model is the caller's.
	run.Launch = LaunchRecord{StartedAt: now, Model: run.Launch.Model}
	raw, err := encodeRun(run)
	if err != nil {
		return PendingRun{}, err
	}
	for range casRetries {
		created, err := s.runs.CreateSandboxRun(ctx, run.TurnID, raw)
		if err != nil {
			return PendingRun{}, fmt.Errorf("sandbox: create run %s: %w", run.TurnID, err)
		}
		if created {
			return run, nil
		}
		opened, reset, held, err := s.resetLaunch(ctx, run, fence)
		if err != nil {
			return PendingRun{}, err
		}
		if reset {
			return opened, nil
		}
		if held > 0 {
			return PendingRun{}, fmt.Errorf("sandbox: run %s is held by a newer lease "+
				"(epoch %d, this launch's %d), so no job was opened on it",
				run.TurnID, held, fence.Epoch)
		}
		// The row went between the create and the reset: create it again.
	}
	return PendingRun{}, fmt.Errorf(
		"sandbox: begin launch %s: the record kept appearing and vanishing under the launch",
		run.TurnID)
}

// resetLaunch is [CoordStore.BeginLaunch]'s half for a row that was already
// there — a second run_sandbox call in this turn, or a redelivered kick-off —
// reporting the row as it reset it, whether it did and, where a newer lease
// refused it, that lease's epoch. Neither is a row that is gone.
//
// Only the LAUNCH-SCOPED state is reset: the identity fields stay the existing
// row's, and so does the box reference, which the caller is about to reattach
// to.
//
// A RESET THE ROW REFUSES IS NEVER A LAUNCH THAT WENT AHEAD: a newer lease
// owns the run (the epoch reported), or its ending is decided — an error
// wrapping [ErrRunEnding]. Answered as a launch, either would start a job in a
// box the row did not record — killed under it by the ending, or billed and
// named by nothing.
func (s *CoordStore) resetLaunch(ctx context.Context, run PendingRun, fence Fence) (PendingRun, bool, int64, error) {
	var held int64
	opened, reset, err := s.mutateLive(ctx, run.TurnID, func(existing *PendingRun) bool {
		if outranked(*existing, fence) {
			held = existing.OwnerEpoch
			return false
		}
		existing.Status = StatusLaunching
		existing.LaunchID = run.LaunchID
		existing.Launch = run.Launch
		if fence.Fenced() {
			existing.Owner, existing.OwnerEpoch = fence.Owner, fence.Epoch
		}
		// The previous job's suspension is not this job's. Left in place
		// it is worse than absent: a completion claimed before the new
		// suspension lands would resume the conversation the LAST call
		// suspended, splicing this run's findings into a loop that has
		// already moved on.
		existing.ExecuteState = nil
		// And its question is answered, or was never asked — either way a
		// reply arriving now belongs to the new job, not the old one.
		existing.Question, existing.Audience = "", ""
		// And whom it was put to: the audience is the question's, and a
		// question that is gone waits on nobody.
		existing.AudienceHandles, existing.AudienceFallback = nil, false
		// AND EVERYTHING ITS ANSWER WAS MEASURED AGAINST: when it was
		// asked, what answered it, and which replies it let go of. The
		// next job's question is anchored on its own asking, and a reply
		// one question declined may be exactly what the next one is
		// waiting for.
		existing.AskedAt, existing.Answer, existing.DeclinedAnswers = time.Time{}, nil, nil
		// AND ITS COST: a parked job's tokens are paid by the resume its
		// answer drives, which has happened by the time a new job opens.
		existing.ParkedInputTokens, existing.ParkedOutputTokens = 0, 0
		// NOR ARE THE PREVIOUS JOB'S TOOL CALLS THIS JOB'S. The bridged
		// log is the whole record an agent-mode resume rebuilds its phase
		// from, and a second executor round under the same turn id — what
		// a reviewer's self_iterate produces — would otherwise replay the
		// FIRST round's submit_work: a round that in fact submitted
		// nothing would report the previous round's outcome instead of
		// being rescued, and its deliveries would satisfy this round's
		// delivery check.
		existing.BridgeCalls, existing.BridgeCallsElided = nil, 0
		// AND THE PREVIOUS JOB'S CHARGE IS NOT THIS JOB'S. Carried over,
		// it would tell this job's completion that its spend is already
		// counted, and the company would never be billed for it.
		existing.Charged, existing.CompanyCharged = false, false
		return true
	})
	if err != nil {
		return PendingRun{}, false, 0, fmt.Errorf("sandbox: relaunch run %s: %w", run.TurnID, err)
	}
	return opened, reset, held, nil
}

// Get returns one run by turn id.
func (s *CoordStore) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	run, _, found, err := s.read(ctx, turnID)
	return run, found, err
}

// ClaimForResume flips a run holding the tail's launch, in one of the tail's
// statuses, to resumed, reporting the row IFF THIS CALL WON.
//
// The at-most-once tail guard, and the reason the version matters: two nodes
// can be handed the same completion (a redelivery, a zombie finishing between
// fence checks) and exactly one must run the tail. The launch, the status, the
// lease and the write are one compare-and-swap, so the loser sees `resumed` on
// its re-read, or a job that is no longer its own, or a newer lease, and
// reports false. A fenced claim stamps its lease on the row, and a zero one
// claims only a row no lease owns — see the contract on [PendingStore].
func (s *CoordStore) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	var before string
	run, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != tail.Launch || !slices.Contains(tail.From, run.Status) ||
			!slices.Contains(Claimable, run.Status) || superseded(*run, fence) {
			return false
		}
		before = run.Status
		run.Status = StatusResumed
		if fence.Fenced() {
			run.Owner, run.OwnerEpoch = fence.Owner, fence.Epoch
		}
		return true
	})
	if err != nil || !won {
		return PendingRun{}, false, err
	}
	// Carried back so a failed dispatch can put the row exactly where it
	// was. Never persisted — inferring it from the other fields is unsound,
	// because a reused run keeps its old question.
	run.ClaimedFrom = before
	return run, true, nil
}

// ReleaseClaim hands a claimed run back while the claim still stands. See the
// contract on [PendingStore].
func (s *CoordStore) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	if !slices.Contains(Claimable, release.To) {
		return false, fmt.Errorf("sandbox: a claim is never taken out of %q, so it cannot be released to it",
			release.To)
	}
	if release.Collected && !release.CollectFailedAt.IsZero() {
		return false, errors.New("sandbox: a release cannot report a collection that both read its box " +
			"back and failed to")
	}
	_, released, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusResumed || run.LaunchID != release.Launch || outranked(*run, release.Fence) {
			return false
		}
		if release.To == StatusAnswered && run.Answer == nil {
			// AN ENDING LET THE ANSWER GO under the claim: there is no
			// answer left to be owed a resume, and the row is that
			// ending's to finish.
			return false
		}
		run.Status = release.To
		run.Charged = run.Charged || release.Charged
		run.CompanyCharged = run.CompanyCharged || release.CompanyCharged
		if run.Answer != nil && run.Answer.Taken() {
			// The turn that took it gave the claim back as a retry, so
			// the answer is the run's again — see [RecordedAnswer.TakenAt].
			answer := *run.Answer
			answer.TakenAt = time.Time{}
			run.Answer = &answer
		}
		// Onto THIS job's record, every fact below: the release names the
		// job the row holds, or it was refused above.
		if release.Published {
			run.Launch.Published = true
		}
		if at := release.CollectFailedAt; !at.IsZero() {
			run.Launch.CollectFailures++
			if run.Launch.CollectFailingSince.IsZero() {
				run.Launch.CollectFailingSince = at.UTC()
			}
		}
		// THE RUN OF FAILURES ENDS AT A COLLECTION THAT READ THE BOX.
		if release.Collected {
			run.Launch.CollectFailures = 0
			run.Launch.CollectFailingSince = time.Time{}
		}
		return true
	})
	return released, err
}

// MarkAwaiting parks a run until a person answers.
func (s *CoordStore) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	if q.AskedAt.IsZero() {
		return fmt.Errorf("sandbox: parking run %s on a question with no instant it was asked at", turnID)
	}
	_, _, err := s.mutateLive(ctx, turnID, func(run *PendingRun) bool {
		run.Status = StatusAwaiting
		run.Question = q.Question
		run.Audience = q.Audience
		run.AudienceHandles = append([]string(nil), q.Answerers.Handles...)
		run.AudienceFallback = q.Answerers.Fallback
		run.Branch = q.Branch
		run.SessionID = q.SessionID
		run.ParkedInputTokens, run.ParkedOutputTokens = q.InputTokens, q.OutputTokens
		// A NEW QUESTION, measured from its own asking, with nothing yet
		// recorded against it and nothing yet declined: an answer and the
		// replies let go of belong to the question they were matched to.
		run.AskedAt = q.AskedAt
		run.Answer, run.DeclinedAnswers = nil, nil
		// And what condensing the collection that parked it cost, onto the
		// record of the job the row holds — see [LaunchRecord.Condensed].
		run.Launch.Condensed = q.Condensed
		return true
	})
	return err
}

// RecordAnswer records a reply as the answer to a parked run's question, under
// the recording node's lease. See the contract on [PendingStore].
func (s *CoordStore) RecordAnswer(ctx context.Context, turnID, launch string, answer RecordedAnswer,
	fence Fence,
) (PendingRun, bool, error) {
	if len(answer.EventIDs) == 0 {
		return PendingRun{}, false, fmt.Errorf("sandbox: an answer to run %s names no delivery", turnID)
	}
	if !answer.Via.Valid() {
		return PendingRun{}, false, fmt.Errorf("sandbox: an answer to run %s names no route it came by "+
			"(via %q)", turnID, answer.Via)
	}
	if answer.RecordedAt.IsZero() {
		answer.RecordedAt = s.clock()
	}
	ownedBy := int64(0)
	recorded, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if superseded(*run, fence) {
			ownedBy = run.OwnerEpoch
			return false
		}
		ownedBy = 0
		if run.LaunchID != launch || !slices.Contains(Awaiting, run.Status) || run.Answer != nil {
			return false
		}
		for _, id := range answer.EventIDs {
			if slices.Contains(run.DeclinedAnswers, id) {
				return false
			}
		}
		recorded := answer
		recorded.From = run.Status
		run.Status = StatusAnswered
		run.Answer = &recorded
		return true
	})
	if err == nil && ownedBy > 0 {
		err = fmt.Errorf("sandbox: recording an answer to run %s, which a newer lease (epoch %d) "+
			"than the recording node's (%d) owns: %w", turnID, ownedBy, fence.Epoch, ErrSeatNotHeld)
	}
	return recorded, won, err
}

// DeclineAnswer lets go of a recorded answer, recording the copies it owes
// the seat's inbox in the same write. See the contract on [PendingStore].
func (s *CoordStore) DeclineAnswer(ctx context.Context, turnID, launch string, answer []string,
	handBack []HandedBack, fence Fence,
) (PendingRun, bool, error) {
	if len(answer) == 0 {
		return PendingRun{}, false, fmt.Errorf("sandbox: declining an answer to run %s that names no delivery", turnID)
	}
	owed := false
	declined, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusAnswered || run.LaunchID != launch || run.Answer == nil ||
			outranked(*run, fence) || !slices.Equal(run.Answer.EventIDs, answer) {
			return false
		}
		if owed = len(run.HandBack) > 0; owed {
			return false
		}
		ids := slices.Clone(answer)
		for _, copied := range handBack {
			ids = append(ids, copied.ID)
		}
		run.Status = run.Answer.declinedTo()
		run.Answer = nil
		run.DeclinedAnswers = boundedDeclined(run.DeclinedAnswers, ids)
		run.HandBack = slices.Clone(handBack)
		return true
	})
	if err == nil && owed {
		err = fmt.Errorf("sandbox: declining the answer to run %s: %w", turnID, ErrHandBackOwed)
	}
	return declined, won, err
}

// TakeAnswer records that a claimed run's turn took its recorded answer. See
// the contract on [PendingStore].
//
// SUPERSEDED, NOT OUTRANKED: the fence must be the row's own lease or a newer
// one, a zero fence included — the one write here a zero fence does not
// exempt. See the contract for why. And never once the run's ending is decided,
// which [CoordStore.mutate] refuses before this decides anything.
func (s *CoordStore) TakeAnswer(ctx context.Context, turnID, launch string, fence Fence) (bool, error) {
	taken := false
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusResumed || run.LaunchID != launch || run.Answer == nil ||
			superseded(*run, fence) {
			return false
		}
		if taken = run.Answer.Taken(); taken {
			return false
		}
		answer := *run.Answer
		answer.TakenAt = s.clock()
		run.Answer = &answer
		return true
	})
	return won || (err == nil && taken), err
}

// OweHandBack lets go of the recorded answer a run whose ending is decided still
// holds, owing its copies to the seat in the same write. See the contract on
// [PendingStore].
func (s *CoordStore) OweHandBack(ctx context.Context, turnID string, letGo LetGo) (PendingRun, bool, error) {
	if len(letGo.Answer) == 0 {
		return PendingRun{}, false, fmt.Errorf("sandbox: letting go of an answer to run %s that "+
			"names no delivery", turnID)
	}
	var owing PendingRun
	taken, owed := false, false
	written, won, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		if run.Ending == nil || run.Ending.ID != letGo.Ending || run.Answer == nil ||
			!slices.Equal(run.Answer.EventIDs, letGo.Answer) {
			return false
		}
		if taken = !run.answerOwed(); taken {
			owing = *run
			return false
		}
		if owed = len(run.HandBack) > 0; owed {
			owing = *run
			return false
		}
		run.Answer = nil
		run.HandBack = nil
		if len(letGo.HandBack) > 0 {
			run.HandBack = slices.Clone(letGo.HandBack)
		}
		return true
	})
	switch {
	case err != nil:
		return PendingRun{}, false, err
	case taken:
		return owing, false, fmt.Errorf("sandbox: letting go of run %s's answer: %w", turnID, ErrAnswerTaken)
	case owed:
		return owing, false, fmt.Errorf("sandbox: letting go of run %s's answer: %w", turnID, ErrHandBackOwed)
	}
	return written, won, nil
}

// ReviveAnswer gives a claim no resume holds back to its recorded answer,
// fenced to the reviving node and — where the claim was lost — counted. See the
// contract on [PendingStore].
func (s *CoordStore) ReviveAnswer(ctx context.Context, turnID string, revival Revival) (PendingRun, bool, error) {
	if len(revival.Answer) == 0 {
		return PendingRun{}, false, fmt.Errorf("sandbox: reviving an answer to run %s that names no "+
			"delivery", turnID)
	}
	var taken PendingRun
	wasTaken := false
	written, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusResumed || run.LaunchID != revival.Launch || run.Answer == nil ||
			!slices.Equal(run.Answer.EventIDs, revival.Answer) || outranked(*run, revival.Fence) {
			return false
		}
		if wasTaken = run.Answer.Taken(); wasTaken {
			taken = *run
			return false
		}
		answer := *run.Answer
		if revival.Lost {
			answer.LostClaims++
			if answer.FirstLostAt.IsZero() {
				answer.FirstLostAt = s.clock()
			}
		}
		run.Answer = &answer
		run.Status = StatusAnswered
		if revival.Fence.Fenced() {
			run.Owner, run.OwnerEpoch = revival.Fence.Owner, revival.Fence.Epoch
		}
		return true
	})
	switch {
	case err != nil:
		return PendingRun{}, false, err
	case wasTaken:
		return taken, false, fmt.Errorf("sandbox: reviving run %s's answer: %w", turnID, ErrAnswerTaken)
	}
	return written, won, nil
}

// DecideEnding records a run's ending, or hands back the one already decided.
// See the contract on [PendingStore].
func (s *CoordStore) DecideEnding(ctx context.Context, turnID string, d Decision) (PendingRun, bool, error) {
	var decided PendingRun
	already := false
	written, won, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		license := d.License
		if outranked(*run, license.Fence) || !slices.Contains(license.WhileIn, run.Status) ||
			(license.Launch != EveryLaunch && run.LaunchID != license.Launch) {
			return false
		}
		if run.Ending != nil {
			decided, already = *run, true
			return false
		}
		ending := &RecordedEnding{
			ID: uuid.NewString(), At: s.clock(),
			Reason: d.Reason, Detail: d.Detail,
			Unused: license.Unused, Reclaim: d.Reclaim,
		}
		run.Ending = ending
		// WHAT GOES BACK TO THE SEAT, settled with the ending: copies the
		// row already owes, and the answer it holds if that is owed — with
		// a copy to hand back.
		ending.Returned = len(run.HandBack) > 0 ||
			(run.answerOwed() && len(run.Answer.Events) > 0)
		return true
	})
	switch {
	case err != nil:
		return PendingRun{}, false, err
	case already:
		return decided, true, nil
	}
	return written, won, nil
}

// ClearHandBack removes published copies from a run's hand-back. See the
// contract on [PendingStore].
func (s *CoordStore) ClearHandBack(ctx context.Context, turnID string, ids []string) (bool, error) {
	_, won, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		kept := slices.DeleteFunc(slices.Clone(run.HandBack), func(h HandedBack) bool {
			return slices.Contains(ids, h.ID)
		})
		if len(kept) == len(run.HandBack) {
			return false
		}
		if len(kept) == 0 {
			kept = nil
		}
		run.HandBack = kept
		return true
	})
	return won, err
}

// boundedDeclined is the declined ids once a decline adds its own: EVERY id of
// the newest decline, and as many of the earlier ones, newest first, as fit
// within [maxDeclinedAnswers]. An earlier id that falls off is one whose copies
// have long since been delivered — a decline lands only on a row that owes
// none ([ErrHandBackOwed]) — and the bound is what stops a question that is
// answered and declined over and over from growing its row without limit.
//
// THE NEWEST DECLINE WHOLE, even past the bound. Its copies are the ones still
// owed or just published, and an id of theirs that fell off would let that
// copy, arriving on the seat's inbox, be recorded as the answer to the very
// question that let it go — and circle the run it already failed to reach. A
// plain newest-sixteen cut did exactly that to an answer of more than eight
// deliveries (a batch is up to twenty), dropping its originals and the first
// of its copies on the way in.
func boundedDeclined(earlier, newest []string) []string {
	keep := max(maxDeclinedAnswers-len(newest), 0)
	if len(earlier) > keep {
		earlier = earlier[len(earlier)-keep:]
	}
	return append(slices.Clone(earlier), newest...)
}

// ClaimOwnership moves a run to this node, refusing to steal a newer lease.
func (s *CoordStore) ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error) {
	_, won, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		if run.OwnerEpoch > epoch {
			// A newer lease already owns it; taking the run would put
			// two engines on one box.
			return false
		}
		run.Owner, run.OwnerEpoch = owner, epoch
		return true
	})
	return won, err
}

// SetStatus moves a run to a new lifecycle state, fenced on the epoch.
func (s *CoordStore) SetStatus(ctx context.Context, turnID, status string, fence Fence) error {
	if !slices.Contains(Active, status) {
		return fmt.Errorf("sandbox: unknown status %q", status)
	}
	_, _, err := s.mutateLive(ctx, turnID, func(run *PendingRun) bool {
		if outranked(*run, fence) {
			return false
		}
		run.Status = status
		return true
	})
	return err
}

// ExpirePause flips a parked run to reseed — or, for an answered one, keeps
// the answer and forgets the box — and clears its box record. See the
// contract on [PendingStore].
func (s *CoordStore) ExpirePause(ctx context.Context, turnID string) (bool, error) {
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		switch run.Status {
		case StatusAwaiting:
			run.Status = StatusReseed
		case StatusAnswered:
			// The answer stays recorded and the run stays owed: only
			// the box goes, and the resume re-seeds from the branch as a
			// reseeded run's would. The record's From moves with it, so
			// an answer that is let go of puts the run back to reseed
			// rather than to a park naming a box that no longer exists.
			if run.Answer == nil {
				return false
			}
			run.Answer.From = StatusReseed
		default:
			return false
		}
		// Cleared in the SAME write as the flip: two writes leave a
		// state a reader can see, in which a reseeded run still names
		// the box an arriving answer would be told to continue in.
		run.SandboxID, run.CommandID = "", ""
		run.PausedAt = time.Time{}
		return true
	})
	return won, err
}

// AttachSandbox records which box a run is using, fenced on the epoch.
//
// A REFUSED ATTACH IS AN ERROR, like a refused relaunch ([CoordStore.BeginLaunch]):
// the caller is a launch about to start a job in this box, and a row that did
// not take it — a newer lease owns the run, or its ending is decided — names
// some other box, so the launch abandons the box it holds rather than running a
// job nothing will reclaim.
func (s *CoordStore) AttachSandbox(ctx context.Context, turnID string, box BoxRef, fence Fence) error {
	outrankedBy := int64(0)
	_, attached, err := s.mutateLive(ctx, turnID, func(run *PendingRun) bool {
		if outranked(*run, fence) {
			outrankedBy = run.OwnerEpoch
			return false
		}
		run.SandboxID = box.SandboxID
		run.CommandID = box.CommandID
		run.CodingAgent = box.CodingAgent
		run.SessionID = box.SessionID
		run.PauseTTLSeconds = box.PauseTTLSec
		// A box being attached is a box that is RUNNING, so the snapshot
		// stamp goes with it. A reused box is attached while its row still
		// carried the paused_at from the collect that snapshotted it, and
		// paused_at is half of what the operator board draws a held box
		// from — so a live second run rendered as a paused one, being
		// billed for, for the rest of the turn.
		run.PausedAt = time.Time{}
		return true
	})
	switch {
	case err != nil:
		return fmt.Errorf("sandbox: attach box %s to run %s: %w", box.SandboxID, turnID, err)
	case outrankedBy > 0:
		return fmt.Errorf("sandbox: attach box %s to run %s: a newer lease (epoch %d) than the "+
			"launch's (%d) owns it", box.SandboxID, turnID, outrankedBy, fence.Epoch)
	case !attached:
		return fmt.Errorf("sandbox: attach box %s to run %s: the run has no record", box.SandboxID, turnID)
	}
	return nil
}

// MarkBoxPaused stamps the box as snapshotted, with the instant the pause TTL
// runs from.
func (s *CoordStore) MarkBoxPaused(ctx context.Context, turnID string, at time.Time) error {
	_, _, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if at.IsZero() {
			at = s.clock()
		}
		run.PausedAt = at
		return true
	})
	return err
}

// ReleaseBox forgets the box a run was using.
func (s *CoordStore) ReleaseBox(ctx context.Context, turnID string) error {
	_, _, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		// Both together — a paused_at pointing at no box is a snapshot
		// the reaper looks for every tick and never finds.
		run.SandboxID, run.CommandID, run.PausedAt = "", "", time.Time{}
		return true
	})
	return err
}

// AppendBridgeCall records one tool call a bridged run made.
//
// NO FENCE, and that is deliberate. Every other mutation here is an ownership
// decision — a claim, a status flip, a pause — and a node whose lease has
// moved must not make one. This is a LOG APPEND: the call already ran and its
// effect already happened, and refusing to record it because the seat moved
// mid-run would lose evidence of something that is true either way. The row's
// own version still guards the write, so two concurrent appends serialise
// rather than clobbering each other.
//
// A run whose row is gone is not an error: the run ended while a late call was
// in flight, which is the ordinary shape of a box shutting down. Nor is a job
// the row has moved on from, or an append naming no job at all (see the pin on
// [PendingStore]). The append is simply dropped, and the caller — which must
// not fail the box's call over telemetry — goes on as it would have.
func (s *CoordStore) AppendBridgeCall(ctx context.Context, turnID string, a BridgeAppend) (bool, error) {
	if a.Launch == "" {
		return false, nil
	}
	call := a.Call
	if call.At.IsZero() {
		call.At = s.clock()
	}
	_, won, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		if a.Launch != run.LaunchID {
			return false
		}
		run.BridgeCalls, run.BridgeCallsElided = appendBounded(
			run.BridgeCalls, run.BridgeCallsElided, call)
		// Onto THIS job's record: the append names the job the row holds,
		// or it was dropped above.
		run.Launch.Bridged = run.Launch.Bridged.Newest(a.Spent)
		return true
	})
	return won, err
}

// appendBounded adds one call and drops from the MIDDLE past the cap.
//
// The start and the end are what explain a run — how it set about the work and
// how it finished — so a log truncated to its last N loses the half a reader
// most often needs. The count of what was dropped rides along, because a log
// that silently skips is a log that lies about what the run did.
func appendBounded(calls []BridgeCall, elided int, next BridgeCall) ([]BridgeCall, int) {
	calls = append(calls, next)
	if len(calls) <= MaxBridgeCalls {
		return calls, elided
	}
	head := MaxBridgeCalls / 2
	tail := MaxBridgeCalls - head
	dropped := len(calls) - MaxBridgeCalls
	kept := make([]BridgeCall, 0, MaxBridgeCalls)
	kept = append(kept, calls[:head]...)
	kept = append(kept, calls[len(calls)-tail:]...)
	return kept, elided + dropped
}

// MarkSuspended writes the suspended Execute loop and opens the run to the
// completion poll. See the contract on [PendingStore].
func (s *CoordStore) MarkSuspended(ctx context.Context, turnID string, suspension Suspension) (bool, error) {
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusLaunching {
			return false
		}
		run.ExecuteState = bytes.Clone(suspension.State)
		run.Status = StatusRunning
		run.Launch.Iteration = suspension.Iteration
		return true
	})
	return won, err
}

// ListActive returns every run that has not finished.
func (s *CoordStore) ListActive(ctx context.Context) ([]PendingRun, error) {
	return s.list(ctx, func(r PendingRun) bool { return slices.Contains(Active, r.Status) })
}

// ListActiveForSeat returns one seat's unfinished runs.
//
// The read a seat's new owner makes inside on_acquire, and the whole reason
// this store is the fleet's: per-node it answered empty for every run the
// previous owner had launched.
func (s *CoordStore) ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error) {
	return s.list(ctx, func(r PendingRun) bool {
		return r.AgentHandle == handle && slices.Contains(Active, r.Status)
	})
}

// Finish deletes the record of a run whose ending is decided, once it owes the
// seat nothing. See the contract on [PendingStore].
//
// A read-decide-delete under the record's version, like every flip here, so a
// write that lands in between — a let-go, a clear — is seen before the delete.
func (s *CoordStore) Finish(ctx context.Context, turnID, ending string) (PendingRun, bool, error) {
	for range casRetries {
		run, version, found, err := s.read(ctx, turnID)
		if err != nil {
			return PendingRun{}, false, err
		}
		if !found || run.Ending == nil || run.Ending.ID != ending {
			return PendingRun{}, false, nil
		}
		if len(run.HandBack) > 0 {
			return run, false, fmt.Errorf("sandbox: finish run %s: %w", turnID, ErrHandBackOwed)
		}
		if run.answerOwed() {
			return run, false, fmt.Errorf("sandbox: finish run %s: %w", turnID, ErrAnswerOwed)
		}
		gone, err := s.runs.DeleteSandboxRun(ctx, turnID, version)
		if err != nil {
			return PendingRun{}, false, fmt.Errorf("sandbox: finish run %s: %w", turnID, err)
		}
		if gone {
			return run, true, nil
		}
	}
	return PendingRun{}, false, fmt.Errorf(
		"sandbox: finish run %s: the record kept changing under the delete", turnID)
}

// read decodes one record and the version it was read at.
func (s *CoordStore) read(ctx context.Context, turnID string) (PendingRun, uint64, bool, error) {
	record, found, err := s.runs.SandboxRun(ctx, turnID)
	if err != nil {
		return PendingRun{}, 0, false, fmt.Errorf("sandbox: read run %s: %w", turnID, err)
	}
	if !found {
		return PendingRun{}, 0, false, nil
	}
	run, err := decodeRun(record)
	if err != nil {
		return PendingRun{}, 0, false, err
	}
	return run, record.Version, true, nil
}

// mutate is the read-decide-write every conditional flip of a LIVE run runs
// through: a run whose ending is decided ([PendingRun.Ending]) is refused before
// decide is asked, so no write but the ending's own steps can move it — see
// [RecordedEnding] for why that is what makes an ending announceable before its
// delete.
//
// decide returns whether the change should be written. FALSE IS NOT A FAILURE
// — it is the condition not holding, which is the answer for a claim somebody
// else won, a fence a moved lease outranks, a pause the reaper is too late for,
// or a run that is ending. A run that does not exist is the same non-answer,
// matching the SQL store this replaces, where an UPDATE that matched no row was
// never an error.
func (s *CoordStore) mutate(ctx context.Context, turnID string, decide func(*PendingRun) bool) (PendingRun, bool, error) {
	run, won, err := s.mutateLive(ctx, turnID, decide)
	if errors.Is(err, ErrRunEnding) {
		return PendingRun{}, false, nil
	}
	return run, won, err
}

// mutateLive is [CoordStore.mutate] for a write whose caller must hear that the
// run's ending refused it — a launch, an attach, a park, a status move: each is
// followed by work that assumes the write landed. The refusal is
// [ErrRunEnding].
func (s *CoordStore) mutateLive(ctx context.Context, turnID string, decide func(*PendingRun) bool) (PendingRun, bool, error) {
	return s.write(ctx, turnID, true, decide)
}

// mutateAny is [CoordStore.mutate] for the writes a run whose ending is decided
// still takes: the ending's own steps (its decision, its let-go, the clear of
// what it handed back), and the facts that are true whoever holds the row (a
// lease stamped on it, a bridged call it made).
func (s *CoordStore) mutateAny(ctx context.Context, turnID string, decide func(*PendingRun) bool) (PendingRun, bool, error) {
	return s.write(ctx, turnID, false, decide)
}

// write is the read-decide-write itself; live refuses a run whose ending is
// decided, with [ErrRunEnding].
func (s *CoordStore) write(ctx context.Context, turnID string, live bool,
	decide func(*PendingRun) bool,
) (PendingRun, bool, error) {
	for range casRetries {
		run, version, found, err := s.read(ctx, turnID)
		if err != nil {
			return PendingRun{}, false, err
		}
		if !found {
			return PendingRun{}, false, nil
		}
		if live && run.Ending != nil {
			return PendingRun{}, false, fmt.Errorf("sandbox: run %s: %w", turnID, ErrRunEnding)
		}
		next := run
		if !decide(&next) {
			return PendingRun{}, false, nil
		}
		next.UpdatedAt = s.clock()
		raw, err := encodeRun(next)
		if err != nil {
			return PendingRun{}, false, err
		}
		won, err := s.runs.UpdateSandboxRun(ctx, turnID, raw, version)
		if err != nil {
			return PendingRun{}, false, fmt.Errorf("sandbox: update run %s: %w", turnID, err)
		}
		if won {
			return next, true, nil
		}
		// Lost the version. RE-READ AND RE-DECIDE rather than re-writing
		// what we computed: the other writer may have taken the claim,
		// moved the lease, unparked the run or decided its ending, and
		// every condition above is evaluated against fields it could have
		// changed.
	}
	return PendingRun{}, false, fmt.Errorf(
		"sandbox: update run %s: the record kept changing under the write", turnID)
}

// list decodes every record and returns the ones that match, oldest first.
//
// The filters are the caller's because coordination cannot see the fields they
// read — the seat, the status, the conversation key, the pause instant. The
// set is bounded by the seats that can be mid-run at once, which is what makes
// one read and a local filter the right shape rather than a scan to apologise
// for.
func (s *CoordStore) list(ctx context.Context, match func(PendingRun) bool) ([]PendingRun, error) {
	records, err := s.runs.SandboxRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox: list runs: %w", err)
	}
	var out []PendingRun
	for _, record := range records {
		run, err := decodeRun(record)
		if err != nil {
			return nil, err
		}
		if match(run) {
			out = append(out, run)
		}
	}
	// Oldest first, so a recovery pass works its runs in the order they
	// were launched: a listing that reordered every boot would make two
	// runs of the same failure look like different failures.
	slices.SortStableFunc(out, func(a, b PendingRun) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.TurnID, b.TurnID))
	})
	return out, nil
}

// superseded reports whether a newer lease than the fence owns the run — the
// zero fence included, on a row any lease owns. It is [outranked] for the writes
// only the seat's holder makes — a claim, the take of an answer, the record of
// one — where a writer holding no lease on a row a lease owns is one that lost
// the seat, never a recovery that has not taken one yet.
func superseded(run PendingRun, fence Fence) bool {
	return run.OwnerEpoch > fence.Epoch
}

// outranked reports whether a fence has been overtaken by a newer lease.
//
// A zero fence constrains nothing, deliberately: recovery writes and the boot
// pass legitimately hold no lease yet. What must never happen is a node
// writing under a lease it has LOST.
func outranked(run PendingRun, fence Fence) bool {
	return fence.Fenced() && run.OwnerEpoch > fence.Epoch
}

// encodeRun writes a run back whole, keys this build does not know included.
//
// THE KNOWN FIELDS WIN. [PendingRun.Extra] holds only what the decode could
// not place in the struct, so a key in both can only mean a caller put it
// there by hand, and the value this build decided is the one that lands.
// "Known" is what the struct DECLARES ([pendingRunKeys]), never what the
// marshal happened to emit: an omitempty field left at its zero value emits
// nothing, and testing presence would let a carried key of the same name
// bring back the value this build just cleared.
func encodeRun(run PendingRun) ([]byte, error) {
	raw, err := json.Marshal(run)
	if err != nil {
		return nil, fmt.Errorf("sandbox: encode run %s: %w", run.TurnID, err)
	}
	if len(run.Extra) == 0 {
		return raw, nil
	}
	fields := map[string]json.RawMessage{}
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("sandbox: encode run %s: %w", run.TurnID, err)
	}
	declared := pendingRunKeys()
	for key, value := range run.Extra {
		if !declared[key] {
			fields[key] = value
		}
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("sandbox: encode run %s: %w", run.TurnID, err)
	}
	return raw, nil
}

// decodeRun reads a run and KEEPS what it cannot read — see
// [PendingRun.Extra] for the rolling upgrade that loses it otherwise.
func decodeRun(record coord.Record) (PendingRun, error) {
	var run PendingRun
	if err := json.Unmarshal(record.Value, &run); err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: decode run %s: %w", record.Key, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(record.Value, &fields); err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: decode run %s: %w", record.Key, err)
	}
	known := pendingRunKeys()
	for key, value := range fields {
		if known[key] {
			continue
		}
		if run.Extra == nil {
			run.Extra = map[string]json.RawMessage{}
		}
		run.Extra[key] = value
	}
	// A null state is NO state. The struct writes one for a run that has
	// none, and a raw field reads the null back as four bytes — which every `len(...) == 0` asking "is a
	// conversation parked here?" would answer wrongly.
	if bytes.Equal(bytes.TrimSpace(run.ExecuteState), []byte("null")) {
		run.ExecuteState = nil
	}
	// The KEY is the identity, not the field: a record whose body somehow
	// disagrees with the key it is stored under would hand a caller a run
	// it cannot then write back.
	run.TurnID = record.Key
	return run, nil
}

// pendingRunKeys is every wire key [PendingRun] declares, read off its own
// tags so a field added to the struct is known here the moment it exists — a
// hand-kept list would be one more place a new field could be forgotten, and
// a forgotten one would be carried in Extra AND written from the struct.
var pendingRunKeys = sync.OnceValue(func() map[string]bool {
	keys := map[string]bool{}
	typ := reflect.TypeFor[PendingRun]()
	for field := range typ.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch {
		case name == "-" || !field.IsExported():
			continue
		case name == "":
			name = field.Name
		}
		keys[name] = true
	}
	return keys
})
