package sandbox

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
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
// On a per-node store that pass found nothing, so the parked turn was
// unreachable and the sandbox was neither resumed nor reaped:
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
// re-evaluated against what the store actually holds, so each is a DECISION
// handed to the store's read-decide-write ([coord.SandboxRuns.MutateSandboxRun]),
// which writes it under the record's version and, on a lost race, asks it
// again on the record as it then stands. That is what makes the SECOND writer
// see the first one's decision instead of overwriting it.
//
// # A lost race is the store's to retry, for its patience
//
// Not a count of attempts made here. A run's record has more writers than a
// count can be set for: every tool call its box makes over the bridge appends
// to it — from as many as twenty subagents at once, Claude Code's default —
// beside its seat's own turns, a peer recovering it, a person's answer and a
// person's stop, so the sixteen immediate attempts this file once made could
// report a healthy, busy record as one that "kept changing" and drop the
// write. The store retries a lost race after a jittered wait for as long as
// its patience allows (internal/coord/kv's race.go: a lost race measures load,
// not health), and answers [coord.ErrUnavailable] only once a write has lost
// for all of it. What the coordinator still loops on here is never a lost race
// but an OUTCOME — a decision refused, a row gone between two operations — and
// each such loop carries a bound of its own, with its reason.
type CoordStore struct {
	runs coord.SandboxRuns
	now  func() time.Time
	// undecodable reports the records a listing skips; nil is the
	// process's own ([undecodableRuns]).
	undecodable *undecodableReport
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

// launchRounds bounds [CoordStore.BeginLaunch]'s create-or-reset loop.
//
// A round repeats only when the row VANISHED between the create that found it
// and the reset that read it — the turn's previous run finishing in that
// window — and the next create then wins, unless another launch of the same
// turn created the row first, which the next reset takes. So an ordinary
// launch takes one round, a launch that met a finishing run two, and one that
// also met a racing launch three; a fourth needs the row to vanish again
// inside the next create-to-reset window, which nothing in this engine does
// repeatedly — the reasoning internal/coord/kv's createRounds gives its own
// create loop. Exhausting it is reported as an error, never as a launch.
const launchRounds = 3

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
	// And the job's own record starts here: the instant the launch exists
	// is the instant it began, and a previous job's record — its start, its
	// iteration, whether its record was published — is not this one's. Only
	// the model, the entry it runs on, the sign-in it was handed and why it
	// follows the job before it are the caller's.
	run.Launch = LaunchRecord{StartedAt: now, Model: run.Launch.Model,
		ProviderKey: run.Launch.ProviderKey, SignInID: run.Launch.SignInID,
		FollowUp: run.Launch.FollowUp}
	// THE TURN'S NOTES START HERE, on a record the launch creates: the seat
	// anchor the caller sealed, the brief's notes, read by this job, and then
	// the notes the turn's box still held, waiting — see
	// [PendingStore.BeginLaunch]. Nothing else of the caller's: a record made
	// here holds no note it was not handed.
	created := run
	created.Steer = RunSteer{Seat: bytes.Clone(run.Steer.Seat)}
	openNotes(&created.Steer, run, run.LaunchID, now)
	raw, err := encodeRun(created)
	if err != nil {
		return PendingRun{}, err
	}
	for range launchRounds {
		made, err := s.runs.CreateSandboxRun(ctx, run.TurnID, raw)
		if err != nil {
			return PendingRun{}, fmt.Errorf("sandbox: create run %s: %w", run.TurnID, err)
		}
		if made {
			return created, nil
		}
		opened, reset, err := s.resetLaunch(ctx, run, fence)
		if err != nil {
			return PendingRun{}, err
		}
		if reset {
			return opened, nil
		}
		// The row went between the create and the reset: create it again.
	}
	return PendingRun{}, fmt.Errorf(
		"sandbox: begin launch %s: the record kept appearing and vanishing under the launch",
		run.TurnID)
}

// resetLaunch is [CoordStore.BeginLaunch]'s half for a row that was already
// there — a second launch in this turn, or a redelivered kick-off —
// reporting the row as it reset it and whether it did; a row that is gone is
// neither.
//
// Only the LAUNCH-SCOPED state is reset: the identity fields stay the existing
// row's, and so does the box reference — its id, its cell, its harness and its
// host ([PendingRun.LaunchNode]), as one — until the launch attaches a box of
// its own ([CoordStore.AttachSandbox]). The reset row hands that box to the
// launch as [PendingRun.Prior], which decides whether the new job continues in
// it or reclaims it through its own cell first; the row naming it meanwhile is
// what lets recovery reclaim it if the launching node dies before deciding, and
// it is reclaimed where it is: a box on another node's host, by that host.
//
// WHAT WAS THE PREVIOUS JOB'S AND NOT ITS BOX'S GOES: its command and its
// session. The row names the new job from here, and a reader that found the
// previous job's command on it — a person's stop, before this launch attaches
// one of its own — read that finished job back off the box and charged it a
// second time, as this job's.
//
// A RESET THE ROW REFUSES IS NEVER A LAUNCH THAT WENT AHEAD: a newer lease
// owns the run — an error wrapping [ErrSeatNotHeld] — or its ending is decided
// — one wrapping [ErrRunEnding] ([CoordStore.mutateFenced] says which wins).
// Answered as a launch, either would start a job in a box the row did not
// record — killed under it by the ending, or billed and named by nothing.
func (s *CoordStore) resetLaunch(ctx context.Context, run PendingRun, fence Fence) (PendingRun, bool, error) {
	var prior PriorBox
	opened, reset, err := s.mutateFenced(ctx, run.TurnID, fence, func(existing *PendingRun) bool {
		// READ BEFORE THE RESET, inside the same compare-and-swap, so the
		// box handed to the launch is exactly the one the write replaced.
		prior = PriorBox{
			SandboxID: existing.SandboxID, Placement: existing.Placement,
			Harness: existing.Harness, Status: existing.Status,
			LaunchNode: existing.LaunchNode,
		}
		// And whether the job it replaces asked, which the reset clears
		// below and the round's cut is decided by.
		continuesAnswer := run.Continues && existing.asked()
		existing.Status = StatusLaunching
		existing.LaunchID = run.LaunchID
		existing.Launch = run.Launch
		// The turn's cap is the turn's, the same on every launch of it —
		// written anyway, so the row says what this launch was told.
		existing.Deadline = run.Deadline
		// THE BOX'S HOST STAYS WITH THE BOX: the row still names the
		// previous job's box, which is on the host of the node that
		// launched that job, and the attach that names this job's box names
		// this node with it. A row with no box names this job's node. And
		// no verdict about the previous job's node is this job's.
		if existing.SandboxID == "" {
			existing.LaunchNode = run.LaunchNode
		}
		// A LOSS IS COUNTED BY THE ROUND IT CARRIES ON, and the verdict
		// itself was the previous job's: a relaunch that continues the round
		// adds that job to the round's losses where it was lost, and a new
		// round has lost nothing ([PendingRun.RoundLosses]).
		switch {
		case !run.Continues:
			existing.RoundLosses = 0
		case existing.Lost != nil:
			existing.RoundLosses++
		}
		existing.Lost = nil
		// AND THE PREVIOUS JOB'S COMMAND AND SESSION ARE NOT THIS JOB'S.
		existing.CommandID, existing.SessionID = "", ""
		// A hold was on the previous job's resume, which is over.
		existing.HeldFor, existing.HeldUntil = "", time.Time{}
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
		existing.QuestionContext, existing.Decision = "", nil
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
		existing.ParkedCacheReadTokens, existing.ParkedCacheWriteTokens = 0, 0
		// NOR ARE THE PREVIOUS JOB'S TOOL CALLS THIS JOB'S. The bridged
		// log is the whole record a run's resume rebuilds its phase
		// from, and a second executor round under the same turn id — what
		// a reviewer's self_iterate produces — would otherwise replay the
		// FIRST round's submit_work: a round that in fact submitted
		// nothing would report the previous round's outcome instead of
		// being rescued, and its deliveries would satisfy this round's
		// delivery check.
		//
		// EXCEPT FOR A JOB THAT CONTINUES ONE THAT ASKED: that is the same
		// round carried on, and the calls made before the question are its
		// calls ([PendingRun.Continues]).
		//
		// AND WHERE THE ANSWER CUT THE ROUND: a job that continues one which
		// asked starts after every call the row holds now
		// ([PendingRun.AnsweredThrough]). A job that continues a lost one
		// keeps the cut it had, and a new round has none.
		switch {
		case !run.Continues:
			existing.BridgeCalls, existing.BridgeCallsElided = nil, 0
			existing.AnsweredThrough = 0
		case continuesAnswer:
			existing.AnsweredThrough = existing.LastBridgeSeq()
		}
		// AND THE PREVIOUS JOB'S CHARGE IS NOT THIS JOB'S. Carried over,
		// it would tell this job's completion that its spend is already
		// counted, and the company would never be billed for it.
		existing.Charged, existing.CompanyCharged = false, false
		// THE TURN'S NOTES ARE THE TURN'S, not the job's: every note, read
		// or waiting, and their numbering stay, and so does the seat anchor
		// — written here only where the record has none. What is the job's
		// is whether its harness asked for them ([RunSteer.Pulled]), which a new
		// job has not; and the brief's notes are read BY THIS JOB, in the
		// write that names it, so its harness can never take them again — and
		// the box's, if the launch hands any, filed after them.
		existing.Steer.Notes = slices.Clone(existing.Steer.Notes)
		if len(existing.Steer.Seat) == 0 {
			existing.Steer.Seat = bytes.Clone(run.Steer.Seat)
		}
		existing.Steer.Pulled = ""
		openNotes(&existing.Steer, run, run.LaunchID, s.clock())
		return true
	})
	if err != nil {
		return PendingRun{}, false, fmt.Errorf("sandbox: relaunch run %s: %w", run.TurnID, err)
	}
	if reset {
		opened.Prior = prior
	}
	return opened, reset, nil
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
// claims only a row no lease owns — see the contract on [PendingStore]. And a
// row whose resume is held, at this store's instant, is no claim's.
func (s *CoordStore) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	var before string
	now := s.clock()
	run, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != tail.Launch || !slices.Contains(tail.From, run.Status) ||
			!slices.Contains(Claimable, run.Status) || superseded(*run, fence) || run.Held(now) {
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
		// THE RUN OF FAILURES ENDS AT A COLLECTION THAT READ THE BOX, and
		// the job is its turn's from then on ([LaunchRecord.Collected]).
		if release.Collected {
			run.Launch.CollectFailures = 0
			run.Launch.CollectFailingSince = time.Time{}
			run.Launch.Collected = true
			if run.Launch.CollectedAt.IsZero() && !release.CollectedAt.IsZero() {
				run.Launch.CollectedAt = release.CollectedAt.UTC()
			}
		}
		// HELD, or not: every release decides it afresh, so a hold an
		// earlier release put on the run does not outlast the attempt it
		// held back.
		run.HeldFor, run.HeldUntil = "", time.Time{}
		if hold := release.Hold; hold != nil {
			run.HeldFor, run.HeldUntil = hold.Condition, hold.Until.UTC()
		}
		return true
	})
	return released, err
}

// LiftHold clears a run's hold for cond — see [PendingStore.LiftHold].
func (s *CoordStore) LiftHold(ctx context.Context, turnID string, cond Condition) (bool, error) {
	_, lifted, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.HeldFor == "" || run.HeldFor != cond {
			return false
		}
		run.HeldFor, run.HeldUntil = "", time.Time{}
		return true
	})
	return lifted, err
}

// MarkAwaiting parks a run until a person answers.
func (s *CoordStore) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	if q.AskedAt.IsZero() {
		return fmt.Errorf("sandbox: parking run %s on a question with no instant it was asked at", turnID)
	}
	_, _, err := s.mutateLive(ctx, turnID, func(run *PendingRun) bool {
		run.Status = StatusAwaiting
		run.AudienceHandles = append([]string(nil), q.Answerers.Handles...)
		run.AudienceFallback = q.Answerers.Fallback
		run.SessionID = q.SessionID
		run.ParkedInputTokens, run.ParkedOutputTokens = q.InputTokens, q.OutputTokens
		run.ParkedCacheReadTokens, run.ParkedCacheWriteTokens = q.CacheReadTokens, q.CacheWriteTokens
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
	if len(answer.Sealed) == 0 {
		// The words live only inside the seal ([RecordedAnswer.Sealed]), so
		// an answer without one is no answer any reader could resume with.
		return PendingRun{}, false, fmt.Errorf("sandbox: an answer to run %s carries no seal", turnID)
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
		// A STOP OF A COLLECTED JOB: its turn's — see [LaunchRecord.Collected].
		if d.Stop != nil && run.Ending == nil && run.Launch.Collected {
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
		// WHO STOPPED IT, in the same write: see [PendingRun.Stop].
		if d.Stop != nil {
			stop := *d.Stop
			run.Stop = &stop
		}
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

// CollectStop records what a stop's ending read of its run before reclaiming
// the box. See the contract on [PendingStore].
func (s *CoordStore) CollectStop(ctx context.Context, turnID string, c StopCollection) (PendingRun, bool, error) {
	return s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		if run.Ending == nil || run.Ending.ID != c.Ending || run.Stop == nil || run.Stop.Collected {
			return false
		}
		stop := *run.Stop
		stop.Collected = true
		stop.InputTokens, stop.OutputTokens = c.InputTokens, c.OutputTokens
		stop.CacheReadTokens, stop.CacheWriteTokens = c.CacheReadTokens, c.CacheWriteTokens
		stop.Condensed = c.Condensed
		run.Stop = &stop
		// RECORDED, NEVER CLEARED, as a release records them: a charge or
		// a publish an earlier attempt made stays made.
		run.Charged = run.Charged || c.Charged
		run.CompanyCharged = run.CompanyCharged || c.CompanyCharged
		run.Launch.Published = run.Launch.Published || c.Published
		return true
	})
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

// MarkLost records the verdict that a running job was lost with the host it
// ran on. See the contract on [PendingStore].
//
// NO FENCE: the verdict is the waiter's, which holds no seat, and it is a fact
// about a job and its host rather than an ownership decision — what it licenses
// is the seat holder's own carrying on of the round, made under that holder's
// lease. What guards it is the job, the status and the node the caller judged.
func (s *CoordStore) MarkLost(ctx context.Context, turnID, launch string, loss JobLoss) (bool, error) {
	if launch == "" {
		return false, nil
	}
	stands := false
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launch || run.Status != StatusRunning ||
			(loss.Node != "" && run.LaunchNode != loss.Node) {
			return false
		}
		if run.Lost != nil {
			stands = true
			return false
		}
		at := loss.At
		if at.IsZero() {
			at = s.clock()
		}
		recorded := loss
		recorded.At = at.UTC()
		run.Lost = &recorded
		return true
	})
	if err != nil {
		return false, fmt.Errorf("sandbox: record run %s's job as lost: %w", turnID, err)
	}
	return won || stands, nil
}

// AttachSandbox records which box a run is using, fenced on the epoch.
//
// A REFUSED ATTACH IS AN ERROR, like a refused relaunch ([CoordStore.BeginLaunch]):
// the caller is a launch about to start a job in this box, and a row that did
// not take it — a newer lease owns the run ([ErrSeatNotHeld]), or its ending is
// decided ([ErrRunEnding]) — names some other box, so the launch abandons the
// box it holds rather than running a job nothing will reclaim. The attach
// comes after the box is made and set up, which can take minutes, and the
// seat can move meanwhile; but its next holder ends a launching run without
// fencing it ([Coordinator.RecoverSeat]), so that move reaches the attach as
// the ending, or as no record at all, rather than as a newer lease.
func (s *CoordStore) AttachSandbox(ctx context.Context, turnID string, box BoxRef, fence Fence) error {
	_, attached, err := s.mutateFenced(ctx, turnID, fence, func(run *PendingRun) bool {
		run.SandboxID = box.SandboxID
		run.CommandID = box.CommandID
		// A COMMAND IS A JOB THAT STARTED, recorded on the job's own record
		// where nothing that forgets the box reaches it — see
		// [LaunchRecord.Started]. Only ever set: the attach before the job
		// names no command, and unsays nothing.
		if box.CommandID != "" {
			run.Launch.Started = true
		}
		// The box's cell moves with the box — see [BoxRef.Placement] — and
		// so does its host ([BoxRef.Node]).
		if box.Placement != "" {
			run.Placement = box.Placement
		}
		if box.Node != "" {
			run.LaunchNode = box.Node
		}
		run.Harness = box.Harness
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

// RecordAsk records the question a running job asked. See the contract on
// [PendingStore].
//
// THROUGH THE LIVE WRITE, so a run whose ending was decided is not given a
// question it can never be parked on.
func (s *CoordStore) RecordAsk(ctx context.Context, turnID, launch string, ask Ask) (AskOutcome, error) {
	if launch == "" {
		return AskNotRunning, nil
	}
	if err := ask.Validate(); err != nil {
		return "", fmt.Errorf("sandbox: recording the question run %s asked: %w", turnID, err)
	}
	ask = ask.Redacted()
	// WHY NOT, as the decision that last ran saw it. Never "recorded": only
	// the store's own answer says a write landed, and a decision that agreed
	// to write can lose its race to a writer that removed the row, after which
	// no decision runs at all.
	refusal := AskNotRunning
	_, won, err := s.mutateLive(ctx, turnID, func(run *PendingRun) bool {
		switch {
		case run.LaunchID != launch ||
			(run.Status != StatusLaunching && run.Status != StatusRunning):
			refusal = AskNotRunning
			return false
		case run.asked():
			refusal = AskAlreadyAsked
			return false
		}
		run.Question, run.QuestionContext = ask.Question, ask.Context
		run.Decision, run.Audience, run.Branch = ask.Decision, ask.To, ask.Branch
		refusal = AskNotRunning
		return true
	})
	switch {
	case errors.Is(err, ErrRunEnding):
		return AskNotRunning, nil
	case err != nil:
		return "", err
	case won:
		return AskRecorded, nil
	}
	return refusal, nil
}

// appendBounded adds one call and drops from the MIDDLE past the cap.
//
// The start and the end are what explain a run (how it set about the work and
// how it finished), so a log truncated to its last N loses the half a reader
// most often needs. The count of what was dropped rides along, because a log
// that silently skips is a log that lies about what the run did.
//
// AND THE MIDDLE GIVES UP ITS ORDINARY CALLS FIRST. A call that ends the run
// ([EndsRun]: a submission, an ask) is what the round's record and its
// reviewer read the run's outcome by, and dropped from the middle, a long
// run's record lost the question it parked on or the submission it was
// corrected by. So the call dropped is the oldest ordinary one after the
// head, never the one just made; and only a log with no ordinary call left
// there gives up the oldest of the others, so the log stays at its cap
// whatever the run calls.
func appendBounded(calls []BridgeCall, elided int, next BridgeCall) ([]BridgeCall, int) {
	calls = append(calls, next)
	if len(calls) <= MaxBridgeCalls {
		return calls, elided
	}
	head := MaxBridgeCalls / 2
	drop := len(calls) - MaxBridgeCalls
	// The candidates: everything after the head but the call just made.
	candidates := calls[head : len(calls)-1]
	ordinary := 0
	for _, call := range candidates {
		if !EndsRun(call.Name) {
			ordinary++
		}
	}
	dropOrdinary := min(drop, ordinary)
	dropAny := drop - dropOrdinary
	kept := make([]BridgeCall, 0, MaxBridgeCalls)
	kept = append(kept, calls[:head]...)
	for _, call := range candidates {
		switch {
		case !EndsRun(call.Name) && dropOrdinary > 0:
			dropOrdinary--
		case EndsRun(call.Name) && dropAny > 0:
			dropAny--
		default:
			kept = append(kept, call)
		}
	}
	kept = append(kept, next)
	return kept, elided + drop
}

// MarkSuspended writes the parked turn's state and opens the run to the
// completion poll. See the contract on [PendingStore].
//
// THROUGH THE LIVE WRITE, so a run whose ending was decided under its launch is
// told [ErrRunEnding] rather than a plain false: see the contract.
func (s *CoordStore) MarkSuspended(ctx context.Context, turnID string, suspension Suspension) (bool, error) {
	_, won, err := s.mutateLive(ctx, turnID, func(run *PendingRun) bool {
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

// OwnerEpochs returns, for each seat, the highest lease epoch any of its run
// records carries, ended runs included: every token the seat's fencing counter
// has to stay above, so its next holder is never older than a row it must
// write ([coord/kv.Epochs.Floor] is what the engine raises it with at boot).
// A row naming no seat, or written by no lease, carries no epoch to keep above.
//
// A record that does not decode is skipped, as every listing skips it ([CoordStore.list]).
func (s *CoordStore) OwnerEpochs(ctx context.Context) (map[uuid.UUID]int64, error) {
	runs, err := s.list(ctx, func(PendingRun) bool { return true })
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]int64{}
	for _, run := range runs {
		seat := seatOf(run.AgentID)
		if seat == uuid.Nil || run.OwnerEpoch <= 0 {
			continue
		}
		out[seat] = max(out[seat], run.OwnerEpoch)
	}
	return out, nil
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

// ListActiveForSeatID returns one seat's unfinished runs by the seat's id: the
// read a mailbox retirement makes. See the contract on [PendingStore].
//
// Through [seatOf], the parse a run's control and inbox subjects are built
// with, so the runs a retirement ends are exactly the runs whose completion and
// answer travel on the subscriptions it is about to delete.
func (s *CoordStore) ListActiveForSeatID(ctx context.Context, seat uuid.UUID) ([]PendingRun, error) {
	if seat == uuid.Nil {
		return nil, errors.New("sandbox: a seat's runs are listed by its id, and none was given")
	}
	return s.list(ctx, func(r PendingRun) bool {
		return seatOf(r.AgentID) == seat && slices.Contains(Active, r.Status)
	})
}

// Finish deletes the record of a run whose ending is decided, once it owes the
// seat nothing. See the contract on [PendingStore].
//
// A read-decide-delete under the record's version, like every flip here, so a
// write that lands in between — a let-go, a clear — is seen before the delete:
// the delete is decided again on the row as it then stands.
func (s *CoordStore) Finish(ctx context.Context, turnID, ending string) (PendingRun, bool, error) {
	var (
		run     PendingRun
		refused error // the decision's own error, returned as it is
	)
	_, gone, err := s.runs.MutateSandboxRun(ctx, turnID, func(record coord.Record) (coord.SandboxRunChange, error) {
		decoded, err := decodeRun(record)
		if err != nil {
			refused = err
			return coord.SandboxRunChange{}, err
		}
		run = decoded
		switch {
		case run.Ending == nil || run.Ending.ID != ending:
			return coord.SandboxRunChange{}, nil
		case len(run.HandBack) > 0:
			refused = fmt.Errorf("sandbox: finish run %s: %w", turnID, ErrHandBackOwed)
			return coord.SandboxRunChange{}, refused
		case run.answerOwed():
			refused = fmt.Errorf("sandbox: finish run %s: %w", turnID, ErrAnswerOwed)
			return coord.SandboxRunChange{}, refused
		}
		return coord.SandboxRunChange{Delete: true}, nil
	})
	switch {
	case err != nil && refused != nil && errors.Is(err, refused):
		// THE ROW AS IT STANDS rides an owed refusal, for the ending to
		// hand back what it still owes and finish again.
		if errors.Is(err, ErrHandBackOwed) || errors.Is(err, ErrAnswerOwed) {
			return run, false, err
		}
		return PendingRun{}, false, err
	case err != nil:
		return PendingRun{}, false, fmt.Errorf("sandbox: finish run %s: %w", turnID, err)
	case !gone:
		return PendingRun{}, false, nil
	}
	return run, true, nil
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
// run's ending refused it — a park, a status move: each is followed by work
// that assumes the write landed. The refusal is [ErrRunEnding].
func (s *CoordStore) mutateLive(ctx context.Context, turnID string, decide func(*PendingRun) bool) (PendingRun, bool, error) {
	return s.write(ctx, turnID, true, Fence{}, decide)
}

// mutateFenced is [CoordStore.mutateLive] for a launch's own writes (its
// relaunch and its attach), which carry the launching lease: a row a newer
// lease owns refuses them with [ErrSeatNotHeld], whether or not its ending is
// decided too.
//
// THE LEASE IS ASKED FIRST. The seat's next holder recovers a run it finds
// claimed, which its resumed turn may be relaunching, by fencing it to its
// own lease before it ends it ([Coordinator.RecoverSeat]), so that relaunch,
// on the node that lost the seat, almost always met an ending as well as the
// newer lease, and was answered [ErrRunEnding]: a run this lease's own turn
// is closing, rather than a seat this node no longer holds, which is what
// happened. A run it finds launching it ends without fencing, so an attach
// there meets that ending, or no record at all, and never this refusal. A
// zero fence asks nothing, as [outranked] says.
func (s *CoordStore) mutateFenced(ctx context.Context, turnID string, fence Fence,
	decide func(*PendingRun) bool) (PendingRun, bool, error) {

	return s.write(ctx, turnID, true, fence, decide)
}

// mutateAny is [CoordStore.mutate] for the writes a run whose ending is decided
// still takes: the ending's own steps (its decision, its let-go, the clear of
// what it handed back), and the facts that are true whoever holds the row (a
// lease stamped on it, a bridged call it made).
func (s *CoordStore) mutateAny(ctx context.Context, turnID string, decide func(*PendingRun) bool) (PendingRun, bool, error) {
	return s.write(ctx, turnID, false, Fence{}, decide)
}

// write is the read-decide-write itself. live refuses a run whose ending is
// decided, with [ErrRunEnding], and a fence a newer lease outranks refuses the
// write before that, with [ErrSeatNotHeld].
//
// THE STORE RUNS IT ([coord.SandboxRuns.MutateSandboxRun]), and on a lost race
// it RE-READS AND RE-DECIDES rather than re-writing what was computed: the
// other writer may have taken the claim, moved the lease, unparked the run or
// decided its ending, and every condition decide evaluates is evaluated
// against fields it could have changed. So decide may run more than once, on
// a fresh copy of the row each time, and what this answers is the last run's.
func (s *CoordStore) write(ctx context.Context, turnID string, live bool, fence Fence,
	decide func(*PendingRun) bool,
) (PendingRun, bool, error) {
	next, won, _, err := s.writeFound(ctx, turnID, live, fence, decide)
	return next, won, err
}

// writeFound is [CoordStore.write], answering as well whether the LAST attempt
// found the record: decide is not asked about a record that is gone, so a
// caller keeping what an earlier attempt decided — before it lost a race to a
// write that removed the record — reads this to know that decision is not the
// answer.
func (s *CoordStore) writeFound(ctx context.Context, turnID string, live bool, fence Fence,
	decide func(*PendingRun) bool,
) (PendingRun, bool, bool, error) {
	var (
		next    PendingRun
		refused error // the decision's own error, returned as it is
	)
	last, won, err := s.runs.MutateSandboxRun(ctx, turnID, func(record coord.Record) (coord.SandboxRunChange, error) {
		run, err := decodeRun(record)
		if err != nil {
			refused = err
			return coord.SandboxRunChange{}, err
		}
		if outranked(run, fence) {
			refused = fmt.Errorf("sandbox: run %s is held by a newer lease (epoch %d) than "+
				"this write's (%d): %w", turnID, run.OwnerEpoch, fence.Epoch, ErrSeatNotHeld)
			return coord.SandboxRunChange{}, refused
		}
		if live && run.Ending != nil {
			refused = fmt.Errorf("sandbox: run %s: %w", turnID, ErrRunEnding)
			return coord.SandboxRunChange{}, refused
		}
		next = run
		if !decide(&next) {
			return coord.SandboxRunChange{}, nil
		}
		next.UpdatedAt = s.clock()
		raw, err := encodeRun(next)
		if err != nil {
			refused = err
			return coord.SandboxRunChange{}, err
		}
		return coord.SandboxRunChange{Put: raw}, nil
	})
	found := last.Key != ""
	switch {
	case err != nil && refused != nil && errors.Is(err, refused):
		return PendingRun{}, false, true, err
	case err != nil:
		return PendingRun{}, false, found, fmt.Errorf("sandbox: update run %s: %w", turnID, err)
	case !won:
		return PendingRun{}, false, found, nil
	}
	return next, true, true, nil
}

// list decodes every record and returns the ones that match, oldest first.
//
// The filters are the caller's because coordination cannot see the fields they
// read — the seat, the status, the conversation key, the pause instant. The
// set is bounded by the seats that can be mid-run at once, which is what makes
// one read and a local filter the right shape rather than a scan to apologise
// for.
//
// # A record that does not decode is skipped, never the listing failed
//
// Its seat, status and owner cannot be known, so no filter can match it, and
// no write can touch it, since every write decodes the row first. Failing the
// whole listing over it failed every reader of every seat's runs on one
// record: the boot's epoch floor (so no node started), each seat's recovery
// (so every seat this node claimed was handed back), the waiter's poll, the
// answer lookup, a retirement and the dashboard. Skipped, it is left exactly as
// it is for the documented repair, and said once per process
// ([CoordStore.decodedOrSkipped], [undecodableReport]).
// A listing that fails as a whole is still an error.
func (s *CoordStore) list(ctx context.Context, match func(PendingRun) bool) ([]PendingRun, error) {
	records, err := s.runs.SandboxRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox: list runs: %w", err)
	}
	var out []PendingRun
	for _, record := range records {
		run, ok := s.decodedOrSkipped(ctx, record)
		if !ok {
			continue
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

// encodeRun writes a run back whole, keys this build does not know included —
// at the top of the record from [PendingRun.Extra], and inside each section that
// carries its own (wirekeys.go) — under the one rule [marshalCarrying] states:
// the fields this build declares win.
func encodeRun(run PendingRun) ([]byte, error) {
	raw, err := marshalCarrying(run, pendingRunKeys(), run.Extra)
	if err != nil {
		return nil, fmt.Errorf("sandbox: encode run %s: %w", run.TurnID, err)
	}
	return raw, nil
}

// decodedOrSkipped decodes a record a listing returned, or reports it as one no
// reader can act on and answers false: the one place a listing decides that
// ([CoordStore.list]).
func (s *CoordStore) decodedOrSkipped(ctx context.Context, record coord.Record) (PendingRun, bool) {
	run, err := decodeRun(record)
	if err != nil {
		report := s.undecodable
		if report == nil {
			report = undecodableRuns
		}
		report.skipped(ctx, record.Key, err)
		return PendingRun{}, false
	}
	return run, true
}

// undecodableRuns is the process's report of the run records its listings
// skip, shared by every store it opens, since each opens its own.
var undecodableRuns = &undecodableReport{log: log}

// undecodableReport says each skipped record once.
//
// AT ERROR THE FIRST TIME A PROCESS SEES IT, AND AT DEBUG AFTER. The runs are
// listed on every poll of the waiter, every recount of a seat, every reply
// lookup and every load of the dashboard's home, so an error per listing was a
// line every few seconds for as long as the record stayed, and on the embedded
// broker it stays until something can remove it. One error per process says
// what an operator needs (the record, why, and the repair) at the boot that
// first meets it and at every restart; the repetition at debug is there for
// whoever is tracing a listing. The set holds only keys actually seen, which
// are bounded by the bucket's own records.
type undecodableReport struct {
	log  *slog.Logger
	mu   sync.Mutex
	seen map[string]struct{}
}

func (u *undecodableReport) skipped(ctx context.Context, key string, err error) {
	u.mu.Lock()
	_, again := u.seen[key]
	if !again {
		if u.seen == nil {
			u.seen = map[string]struct{}{}
		}
		u.seen[key] = struct{}{}
	}
	u.mu.Unlock()
	level := slog.LevelError
	if again {
		level = slog.LevelDebug
	}
	u.log.Log(ctx, level, "sandbox_run_undecodable", "key", key, "error", err,
		"detail", "this run record cannot be read, so no listing returns it: no recovery, "+
			"poll, answer, retirement or epoch floor acts on it, and no write can change it",
		"remedy", "remove the record from the sandbox runs bucket once you know its run "+
			"is over (see the deployment guide)")
}

// decodeRun reads a run and KEEPS what it cannot read (see
// [PendingRun.Extra] for the rolling upgrade that loses it otherwise).
func decodeRun(record coord.Record) (PendingRun, error) {
	var run PendingRun
	extra, err := unmarshalCarrying(record.Value, &run, pendingRunKeys())
	if err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: decode run %s: %w", record.Key, err)
	}
	run.Extra = extra
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

// pendingRunKeys is every wire key [PendingRun] declares ([wireKeys]).
func pendingRunKeys() map[string]bool { return wireKeys(reflect.TypeFor[PendingRun]()) }
