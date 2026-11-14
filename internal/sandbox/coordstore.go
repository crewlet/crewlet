package sandbox

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/words"
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
// # A lost race is the store's to retry
//
// Never a count of attempts made here. A run's record has many writers:
// every tool call its box makes over the bridge appends to it — from as many
// as twenty subagents at once, Claude Code's default — beside its seat's own
// turns, a peer recovering it, a person's answer and a person's stop, so the
// sixteen immediate attempts this file once made could report a healthy,
// busy record as one that "kept changing" and drop the write. The store
// decides a lost race again on the record as it then stands, within the bound
// every fleet key shares (internal/coord/kv's casRetry: twice the writers a
// fleet key is sized for, each attempt after a loss waiting a window of round
// trips), which outlasts the overlap of the calls one run's box can make at
// once, and answers [coord.ErrUnavailable] only once a write has lost every
// attempt of it. What the coordinator still loops on here is never a lost race
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

// RowCrossing is how long a change of a run's value at its bound takes to
// cross this node's connection to the coordination store, its read and every
// send of its write — see [PendingStore.RowCrossing].
func (s *CoordStore) RowCrossing() time.Duration { return s.runs.ValueCrossing() }

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
// turn created the row first, which the next reset takes. A row that vanishes
// once the launch's own create or reset has landed never repeats one: that
// row names this launch, and only its own ending removes it ([ErrRunEnding]).
// So an ordinary
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
//
// A ROW THAT VANISHES ONCE THE LAUNCH'S OWN WRITE LANDED is a different thing,
// and never created again: that row names this launch, its reset refused any
// decided ending, so what removed it is an ending decided for this launch's
// own job — a person's stop — and its finish. The launch is refused
// [ErrRunEnding], as a reset the ending refused is. Created again, the row
// would carry no ending, and the launch would start a box for a turn that
// was stopped.
func (s *CoordStore) BeginLaunch(ctx context.Context, run PendingRun, fence Fence) (_ PendingRun, err error) {
	defer origin.Stamp(&err)
	if run.TurnID == "" {
		return PendingRun{}, fmt.Errorf("sandbox: a pending run needs a turn id")
	}
	now := s.clock()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	// THE JOB'S LAUNCH FLOOR IS THE FLOOR IT OPENS AT, never the caller's to
	// set apart: what the run pushed is read above it, so one written from
	// anything else would hide a push or count another job's.
	run.CodeLaunch = run.CodeFloor
	// THE TURN'S CODE HOLDS, each once and within the row's bound: a create
	// writes them as handed, a reset unions them ([CoordStore.resetLaunch]).
	holds, fits := unionHolds(nil, run.CodeHolds)
	if !fits {
		return PendingRun{}, fmt.Errorf("sandbox: begin launch %s: %d holds: %w", run.TurnID,
			len(holds), ErrCodeHoldsFull)
	}
	run.CodeHolds = holds
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
	// NOR ITS ORDINAL: the first launch of a turn is 1, and the reset below
	// numbers each later one past the row's ([PendingRun.LaunchSeq]).
	run.LaunchSeq = 1
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
	// A CREATED ROW OPENS A ROUND, named by its first job, which no answer
	// has cut yet ([PendingRun.Round]).
	created.Round, created.AnsweredJob = run.LaunchID, ""
	created.Steer = RunSteer{Seat: bytes.Clone(run.Steer.Seat)}
	openNotes(&created.Steer, run, run.LaunchID, now)
	// A TRIGGER'S WORDS, HELD TO THEIR SHARE OF THE ROW ([MaxRunTitleBytes]):
	// what a person reads the run by, twice, and the question of the
	// colleague its turn answers, which nothing else bounds below the
	// broker's payload. Here, in the write that creates the row, because the
	// identity fields are never written again: a relaunch keeps the row's.
	created.TaskDescription, created.Label = keptTitle(run.TaskDescription), keptTitle(run.Label)
	if ask := run.ColleagueAsk; ask != nil {
		held := *ask
		held.Question = keptTitle(ask.Question)
		created.ColleagueAsk = &held
	}
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
			// AND THE JOB'S LOG, a new run's: whatever a key of that
			// name held is no call of this run's.
			last, opened, openErr := s.openLog(ctx, run.TurnID, run.LaunchID,
				logOpening{round: created.Round})
			if openErr != nil {
				return PendingRun{}, openErr
			}
			if !opened {
				return PendingRun{}, endedUnderLaunch(run.TurnID)
			}
			created.OpenedAfter = last
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
// row's, and so do the turn's read of its conversation ([PendingRun.Base]),
// which the create alone writes, and the box reference — its id, its cell, its
// harness and its host ([PendingRun.LaunchNode]), as one — until the launch
// attaches a box of its own ([CoordStore.AttachSandbox]). The reset row hands that box to the
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
	full := false
	var (
		prior   PriorBox
		opening logOpening
	)
	// WHAT THE BOX WAS HANDED is kept beside the row ([CoordStore.Handed]),
	// so it is read before the reset and taken inside it only where it names
	// the job the reset replaces. A set of an earlier job, none, or one that
	// job's attach wrote only after this read carries nothing: the launch's
	// carry then refuses its job's captures rather than scrub less than its
	// box holds ([Coordinator.sealHanded]) — never another job's set carried
	// as the box's.
	handed, err := s.readHanded(ctx, run.TurnID)
	if err != nil {
		return PendingRun{}, false, fmt.Errorf("sandbox: begin launch %s: %w", run.TurnID, err)
	}
	opened, reset, err := s.mutateFenced(ctx, run.TurnID, fence, func(existing *PendingRun) bool {
		// READ BEFORE THE RESET, inside the same compare-and-swap, so the
		// box handed to the launch is exactly the one the write replaced.
		prior = PriorBox{
			SandboxID: existing.SandboxID, Placement: existing.Placement,
			Harness: existing.Harness, Status: existing.Status,
			LaunchNode: existing.LaunchNode, Since: existing.BoxSince,
			Handed: handed.of(existing.LaunchID),
		}
		// THE TURN'S CODE HOLDS ARE UNIONED, never reset with the rest: a
		// relaunch's turn may still depend on what an earlier job, or an
		// earlier half of the turn, named — and once it parks, this row is
		// the only place Gitlet's expiry can read that from.
		holds, fits := unionHolds(existing.CodeHolds, run.CodeHolds)
		if !fits {
			full = true
			return false
		}
		existing.CodeHolds = holds
		// THE ROUND THE JOB BELONGS TO, and the answer its log is still to
		// be cut for — decided here, in the reset's own write, so the
		// opening that follows, or the next relaunch's where that opening
		// never lands, keeps what the round made ([PendingRun.Round]).
		// Whether the replaced job asked is read before the reset clears
		// its question below, and the round its held call was made in
		// before it moves on.
		heldRound := existing.Round
		switch {
		case !run.Continues:
			existing.Round, existing.AnsweredJob = run.LaunchID, ""
			existing.RoundRefusals = nil
		case existing.asked():
			existing.AnsweredJob = existing.LaunchID
			// AND A REFUSAL THE ROUND IS CARRIED ON FROM is kept for the
			// round's review, in the write that clears it below: its
			// reviewer reads the round from its calls, where the run's
			// question asked again reads as the same one sent twice
			// ([PendingRun.RoundRefusals]).
			if refusal, refused := existing.Refused(); refused {
				existing.RoundRefusals = withRoundRefusal(existing.RoundRefusals, existing.LaunchID, refusal)
			}
		}
		// THE CALL A PERSON APPROVED IN THE REPLACED JOB, which the reset
		// clears with its held call below, is carried to the opening in
		// this write, so a log its own append never reached still takes
		// it ([OwedCall]); an owed call an earlier reset carried stays
		// only while the job it carried it to never launched — one that
		// did had its opening land, and the call with it.
		owed := existing.OwedCall
		if existing.Status != StatusLaunching {
			owed = nil
		}
		if h := existing.HeldCall; h != nil && h.Made() && h.Call != nil {
			owed = &OwedCall{Round: heldRound, Call: *h.Call}
		}
		existing.OwedCall = owed
		opening = logOpening{round: existing.Round, answered: existing.AnsweredJob, owed: owed,
			told: run.BriefRead}
		existing.Status = StatusLaunching
		existing.LaunchID = run.LaunchID
		// ONE PAST THE ROW'S, in the write that names the job, so the
		// launch's ordinal and its name are never apart: a row an older build
		// wrote holds none, and this launch is its 1.
		existing.LaunchSeq++
		existing.Launch = run.Launch
		// The turn's cap is the turn's, the same on every launch of it —
		// written anyway, so the row says what this launch was told.
		existing.Deadline = run.Deadline
		// THE JOB'S GRANTS AND FLOORS ARE ITS OWN, replaced rather than kept:
		// the previous job's repositories are not this job's, and a box of
		// the previous job that is still pushing is refused by the launch id
		// it was minted for. Unlike the holds above, which the turn may still
		// depend on whichever job named them. And the colleague the job
		// works for, and whom its pushes are made for, are the ones its own
		// resolution read for.
		existing.Repos = run.Repos
		existing.CodeFloor, existing.CodeLaunch = run.CodeFloor, run.CodeLaunch
		existing.CodeCeiling, existing.CodeWorkingFor = run.CodeCeiling, run.CodeWorkingFor
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
		// AND THE PREVIOUS JOB'S COMMAND AND SESSION ARE NOT THIS JOB'S, nor
		// what its box was handed: the set beside the row names the job it
		// was written for, so from here it reads as none for this one, and
		// the launch's attach writes its own, carrying the previous job's
		// forward where it continues in the same box ([PriorBox.Handed]).
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
		// And the call it held, made or not: a held call is the job's, and
		// what the next job does is its own.
		existing.HeldCall = nil
		// And whom it was put to: the audience is the question's, and a
		// question that is gone waits on nobody.
		existing.AudienceHandles, existing.AudienceFallback = nil, false
		// AND EVERYTHING ITS ANSWER WAS MEASURED AGAINST: when it was
		// asked, what answered it, and which replies it let go of. The
		// next job's question is anchored on its own asking, and a reply
		// one question declined may be exactly what the next one is
		// waiting for.
		existing.AskedAt, existing.Answer, existing.DeclinedAnswers = time.Time{}, nil, nil
		existing.AnswerRefusal = nil
		// AND ITS COST: a parked job's tokens are paid by the resume its
		// answer drives, which has happened by the time a new job opens.
		existing.ParkedInputTokens, existing.ParkedOutputTokens = 0, 0
		existing.ParkedCacheReadTokens, existing.ParkedCacheWriteTokens = 0, 0
		// NOR ARE THE PREVIOUS JOB'S TOOL CALLS THIS JOB'S, unless it is
		// the same round carried on — decided by the write that opens the
		// job's log, once this reset has landed, which folds a replaced
		// round's calls into the turn's earlier ones in the same write
		// ([CoordStore.openLog], [BridgeLog.PriorCalls]).
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
	switch {
	case err != nil:
		return PendingRun{}, false, fmt.Errorf("sandbox: relaunch run %s: %w", run.TurnID, err)
	case full:
		return PendingRun{}, false, fmt.Errorf("sandbox: relaunch run %s: %w", run.TurnID,
			ErrCodeHoldsFull)
	}
	if !reset {
		return opened, false, nil
	}
	opened.Prior = prior
	// THEN THE LOG IS OPENED TO THE JOB, never before: a reset the row
	// refused must leave the replaced job's log as it was. Between the two
	// writes the log still takes the replaced job's late calls as that job's
	// own, which are the round's when the job carries it on — counted before
	// the answer's cut, which the opening takes off the same write — and are
	// dropped with the rest when it does not; after the opening, it lists one
	// of a round carried on marked late, its spend dropped with its job
	// ([BridgeLog.take]).
	last, logged, err := s.openLog(ctx, run.TurnID, opened.LaunchID, opening)
	switch {
	case err != nil:
		return PendingRun{}, false, err
	case !logged:
		return PendingRun{}, false, endedUnderLaunch(run.TurnID)
	}
	opened.OpenedAfter = last
	return opened, true, nil
}

// endedUnderLaunch is [CoordStore.BeginLaunch]'s refusal of a launch whose
// own row was removed before its log was opened to it.
func endedUnderLaunch(turnID string) error {
	return fmt.Errorf("sandbox: begin launch %s: the run ended under its launch: %w",
		turnID, ErrRunEnding)
}

// logOpening is what the write that opens a job's log keeps of the round
// before it, as the row's create or reset decided it — see
// [CoordStore.openLog].
type logOpening struct {
	// round is the round the job belongs to ([PendingRun.Round]).
	round string

	// answered is the job whose answered question the round is cut for
	// ([PendingRun.AnsweredJob]); empty for a round no answer carried on.
	answered string

	// owed is the call a person approved that its round's log is owed
	// ([PendingRun.OwedCall]), nil for none: appended in the opening's own
	// write where the log lacks it, before the opening decides what the job
	// keeps.
	owed *OwedCall

	// told is how many calls the round's log had taken at the read the
	// relaunch's briefs were built from ([PendingRun.BriefRead]), nil for none
	// known: an opening that carries the round on marks every call of it the
	// log took after that read late ([BridgeLog.lateAfter]).
	told *int
}

// opened is l as the opening o of job launch leaves it — see
// [CoordStore.openLog], whose one write it decides.
func (l BridgeLog) opened(launch string, o logOpening) BridgeLog {
	if o.owed != nil {
		l, _ = l.withApproved(o.owed.Round, o.owed.Call)
	}
	next := BridgeLog{Launch: launch, Round: o.round}
	next.PriorCalls, next.PriorCallsElided = l.priorFor(o.round)
	if l.Round == o.round {
		next = l
		next.Launch = launch
		if o.answered != "" && l.CutFor != o.answered {
			next.AnsweredThrough, next.CutFor = l.LastSeq(), o.answered
		}
		if o.told != nil {
			next = next.lateAfter(*o.told)
		}
	}
	return next
}

// lateAfter is l with every call it took after told marked late
// ([BridgeCall.Late]) — but a call a person approved, which the relaunch's lead
// tells — and every one of those it no longer lists, dropped from its middle
// since, counted late ([BridgeLog.LateElided]): what an opening that carries
// the round on makes of the calls the log took after the read its relaunch's
// briefs were built from ([PendingRun.BriefRead]).
//
// THE SAME MARK AS A CALL TAKEN AFTER THE OPENING ([BridgeLog.take]), FOR THE
// SAME REASON. Such a call is one of the job this opening replaces — a worker
// still running, a call its harness gave up on — that landed while the
// relaunch was readied: after its briefs were built, which never told it, and
// before the log moved on, so as that job's own. Unmarked, a later capture of
// the job this opening names, placed by its own read, counts it among the
// calls its session holds, and no continuation of the round is ever told it.
//
// A LIST OF ITS OWN, since l is a copy whose list its caller may still hold.
// A call taken after told with no number — an older build's append, in a fleet
// mid-upgrade — is counted among the dropped ones: an over-count, the safe
// direction. Unmarked, it is held at no save point a capture states, so a
// continuation of a capture is told it anyway; a job's own copy of its session
// in its box, where its harness ended its own run, holds it as it holds every
// call but a late one, and is told only the count.
func (l BridgeLog) lateAfter(told int) BridgeLog {
	l.Calls = slices.Clone(l.Calls)
	listed := 0
	for i, call := range l.Calls {
		if call.Appended <= told {
			continue
		}
		listed++
		if !call.Approved {
			l.Calls[i].Late = true
		}
	}
	l.LateElided += max(l.Appends()-told-listed, 0)
	return l
}

// openLog opens turnID's log to job launch, answering the place of the last
// call it keeps from before the job ([BridgeLog.LastSeq]) and whether it was
// written — false for a run whose record is gone.
//
// ONE COMPARE-AND-SWAP OF THE LOG decides what the job keeps and moves the log
// onto it, so no append of the replaced job can land between the two: an append
// that read the log before this write loses its race and is decided again on a
// log that takes the new job's calls alone.
//
// A NEW ROUND STARTS THE LOG AFRESH, so a second executor round under the same
// turn id — what a reviewer's self_iterate produces — never replays the FIRST
// round's submit_work: a round that in fact submitted nothing would report the
// previous round's outcome instead of being rescued, and its deliveries would
// satisfy this round's delivery check. A job that CONTINUES the round keeps its
// calls ([PendingRun.Continues]) and, carried on by a person's answer, cuts it
// after the last of them; one carried on after a loss keeps the cut it had.
//
// BY THE ROUND, NEVER BY THE JOB BEFORE: what the log keeps is decided by the
// round the row's own write named ([PendingRun.Round]) and the answer it is
// cut for ([PendingRun.AnsweredJob]). An opening that failed, or that its node
// stopped before, leaves the log on an earlier job of the round; the next
// relaunch that carries the round on still keeps its calls, and still takes
// the cut that opening owed. A log of another round is started afresh.
//
// AND WHAT A NEW ROUND TAKES OUT OF THE LOG IS FOLDED INTO THE TURN'S EARLIER
// CALLS IN THE SAME WRITE ([BridgeLog.PriorCalls]): by name and outcome — the
// calls the log kept and the identities of the ones it dropped — which is all
// the run's own ending needs to know whether an earlier round delivered the
// turn's reply. The new round's log keeps neither tally of the old one's: both
// are that round's. One compare-and-swap, so a crash never leaves a round's
// calls gone from the log and missing from the fold. The call a person approved
// that the log is owed ([OwedCall]) is appended first, in the same write, so it
// is kept with its round or folded with it.
func (s *CoordStore) openLog(ctx context.Context, turnID, launch string, o logOpening) (int, bool, error) {
	last := 0
	_, opened, err := s.runs.MutateSandboxRunPart(ctx, turnID, coord.RunLog, func(current coord.Record, found bool) ([]byte, error) {
		next := BridgeLog{Launch: launch, Round: o.round}
		if found {
			var held BridgeLog
			if err := json.Unmarshal(current.Value, &held); err != nil {
				return nil, fmt.Errorf("sandbox: decode the log of run %s: %w", turnID, err)
			}
			next = held.opened(launch, o)
		}
		last = next.LastSeq()
		return json.Marshal(next)
	})
	if err != nil {
		return 0, false, fmt.Errorf("sandbox: open the log of run %s to its job: %w", turnID, err)
	}
	return last, opened, nil
}

// unionHolds is held with keys added — each key once, in the order first
// taken — and whether the union fits [MaxCodeHolds].
func unionHolds(held, keys []string) ([]string, bool) {
	out := make([]string, 0, len(held)+len(keys))
	seen := make(map[string]bool, len(held)+len(keys))
	for _, list := range [][]string{held, keys} {
		for _, key := range list {
			if !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	return out, len(out) <= MaxCodeHolds
}

// AddCodeHolds adds keys to a live run's code holds. See the contract on
// [PendingStore].
func (s *CoordStore) AddCodeHolds(ctx context.Context, turnID string, keys []string) (bool, error) {
	if len(keys) == 0 || slices.Contains(keys, "") {
		return false, fmt.Errorf("sandbox: a hold on run %s names no key", turnID)
	}
	full, already := false, false
	_, wrote, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		holds, fits := unionHolds(run.CodeHolds, keys)
		switch {
		case !fits:
			full = true
			return false
		case len(holds) == len(run.CodeHolds):
			// EVERY KEY ALREADY HELD: the row says it, and nothing is
			// written.
			already = true
			return false
		}
		run.CodeHolds = holds
		return true
	})
	switch {
	case err != nil:
		return false, fmt.Errorf("sandbox: hold %d keys on run %s: %w", len(keys), turnID, err)
	case full:
		return false, fmt.Errorf("sandbox: hold %d keys on run %s: %w", len(keys), turnID, ErrCodeHoldsFull)
	}
	return wrote || already, nil
}

// RaiseCodeFloor raises a live run's code floor while the row holds launch.
// See the contract on [PendingStore].
func (s *CoordStore) RaiseCodeFloor(ctx context.Context, turnID, launchID string,
	raise func(current string) (string, bool),
) (bool, error) {
	if launchID == "" {
		return false, fmt.Errorf("sandbox: a floor raise on run %s names no launch", turnID)
	}
	_, wrote, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launchID {
			return false
		}
		next, ok := raise(run.CodeFloor)
		if !ok || next == "" || next == run.CodeFloor {
			return false
		}
		run.CodeFloor = next
		return true
	})
	if err != nil {
		return false, fmt.Errorf("sandbox: raise the code floor of run %s: %w", turnID, err)
	}
	return wrote, nil
}

// Get returns one run by turn id.
func (s *CoordStore) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	run, _, found, err := s.read(ctx, turnID)
	return run, found, err
}

// StampCapture records one capture of the turn's session on its row. See the
// contract on [PendingStore].
//
// THROUGH THE WRITE EVERY STATE TAKES ([CoordStore.mutateAny]), as a bridged
// call's append is: a capture is a fact about what left the box, and a stop's is
// stamped once its ending is decided.
//
// A STAMP THE ROW ALREADY HOLDS IS ANSWERED TAKEN, writing nothing: decided on
// the row as it stands, in the same read-decide-write, so its asker learns the
// place is its own where the store applied an earlier ask and its answer never
// came back.
func (s *CoordStore) StampCapture(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	return s.stampCapture(ctx, turnID, capture, nil)
}

// SettleBegun records a settle of the turn's newest capture over the begun
// stamp it was decided from. See the contract on [PendingStore].
//
// THE SAME WRITE AS [CoordStore.StampCapture], its condition decided in the
// same read-decide-write on the row as it stands ([stillBegun]) — never on the
// copy the settle was decided from, which a taker's own stamp landing between
// the two would leave behind.
func (s *CoordStore) SettleBegun(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	return s.stampCapture(ctx, turnID, capture, stillBegun)
}

// stillBegun reports whether run's newest capture is capture's own take, still
// standing begun: the stamp a settle decided from it may be written over
// ([PendingStore.SettleBegun]).
func stillBegun(run PendingRun, capture SessionCapture) bool {
	newest := run.Capture
	return newest != nil && capture.sameTake(*newest) && newest.State == CaptureBegun
}

// StandBy records capture's take as the read standing by for the row's newest
// capture. See the contract on [PendingStore].
//
// THE SAME READ-DECIDE-WRITE AS EVERY STAMP ([CoordStore.writeFound]), on the
// row as it stands: two reads that find one place taken each ask, and the
// store lets one of them stand.
func (s *CoordStore) StandBy(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	if err := stampable(turnID, capture); err != nil {
		return false, err
	}
	var held bool
	_, won, found, err := s.writeFound(ctx, turnID, false, Fence{}, func(run *PendingRun) bool {
		newest := run.Capture
		held = newest != nil && newest.standsBy(capture)
		if held || newest == nil || !newest.samePlace(capture) || newest.Launch != capture.Launch ||
			capture.sameTake(*newest) || newest.State == CaptureLanded || !newest.StandbyAt.IsZero() {
			return false
		}
		stood := *newest
		stood.StandbyTaker, stood.StandbyTakerEpoch, stood.StandbyAt = capture.Taker, capture.TakerEpoch, capture.At
		run.Capture = &stood
		return true
	})
	if err != nil {
		return false, err
	}
	return won || (found && held), nil
}

// TakeOver stamps capture in the place after the capture standby stood by for.
// See the contract on [PendingStore].
//
// THE SAME WRITE AS [CoordStore.StampCapture], its condition decided in the
// same read-decide-write on the row as it stands ([stoodByUnlanded]).
func (s *CoordStore) TakeOver(ctx context.Context, turnID string, standby, capture SessionCapture) (bool, error) {
	return s.stampCapture(ctx, turnID, capture, func(run PendingRun, _ SessionCapture) bool {
		return stoodByUnlanded(run, standby)
	})
}

// stoodByUnlanded reports whether run's newest capture is one standby stands by
// for and that did not land: the capture a standby may take the next place
// after ([PendingStore.TakeOver]).
func stoodByUnlanded(run PendingRun, standby SessionCapture) bool {
	newest := run.Capture
	return newest != nil && newest.State != CaptureLanded && newest.standsBy(standby)
}

// StandDown withdraws standby from the row's newest capture. See the contract
// on [PendingStore].
func (s *CoordStore) StandDown(ctx context.Context, turnID string, standby SessionCapture) (bool, error) {
	if err := stampable(turnID, standby); err != nil {
		return false, err
	}
	_, won, _, err := s.writeFound(ctx, turnID, false, Fence{}, func(run *PendingRun) bool {
		newest := run.Capture
		if newest == nil || !newest.standsBy(standby) {
			return false
		}
		withdrawn := *newest
		withdrawn.StandbyTaker, withdrawn.StandbyTakerEpoch, withdrawn.StandbyAt = "", 0, time.Time{}
		run.Capture = &withdrawn
		return true
	})
	if err != nil {
		return false, err
	}
	return won, nil
}

// stampable is why capture cannot be stamped on run turnID's row, nil where it
// can: placed from (1, 1), in a state this build knows, and within the bounds
// the row holds a capture to ([SessionCapture.Bounded]) — what every write of
// a capture's stamp holds it to before it reaches the row.
func stampable(turnID string, capture SessionCapture) error {
	if capture.LaunchSeq <= 0 || capture.CaptureSeq <= 0 || !capture.State.Valid() {
		return fmt.Errorf("sandbox: a capture of run %s is placed at (%d, %d) in state %q, and "+
			"every capture is placed from (1, 1) in one of %s", turnID, capture.LaunchSeq,
			capture.CaptureSeq, capture.State, words.Or(CaptureStates))
	}
	// HELD TO ITS BOUNDS before it reaches the row: see [SessionCapture.Bounded].
	if err := capture.Bounded(); err != nil {
		return fmt.Errorf("sandbox: a capture of run %s: %w", turnID, err)
	}
	return nil
}

// stampCapture is the write [CoordStore.StampCapture], [CoordStore.SettleBegun]
// and [CoordStore.TakeOver] make: the row's own rule ([stampOnto]), where over
// — nil for none — holds of the row as the write finds it.
func (s *CoordStore) stampCapture(ctx context.Context, turnID string, capture SessionCapture,
	over func(PendingRun, SessionCapture) bool) (bool, error) {

	if err := stampable(turnID, capture); err != nil {
		return false, err
	}
	// SET ON EVERY ASK OF decide, which runs again on a lost race: the last
	// one is the row the answer is about — where the last attempt found one
	// at all, which a record removed under an earlier attempt's decision is
	// not ([CoordStore.writeFound]).
	var held bool
	_, won, found, err := s.writeFound(ctx, turnID, false, Fence{}, func(run *PendingRun) bool {
		held = holdsStamp(*run, capture)
		if over != nil && !over(*run, capture) {
			return false
		}
		return stampOnto(run, capture)
	})
	if err != nil {
		return false, err
	}
	return won || (found && held), nil
}

// RetireSessions records the sessions a job of the turn ran out of its model's
// context in. See the contract on [PendingStore].
func (s *CoordStore) RetireSessions(ctx context.Context, turnID string, sessions []string) error {
	_, _, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		grown := withExhausted(run.ExhaustedSessions, sessions)
		if slices.Equal(grown, run.ExhaustedSessions) {
			return false
		}
		run.ExhaustedSessions = grown
		return true
	})
	if err != nil {
		return fmt.Errorf("sandbox: recording the sessions run %s ran out of context in: %w", turnID, err)
	}
	return nil
}

// OweEnd records the end the run's turn owes the executor log. See the contract
// on [PendingStore].
//
// THROUGH THE WRITE EVERY STATE TAKES ([CoordStore.mutateAny]): a loss's or a
// stop's end is decided by that ending's own steps, on a row whose ending is
// decided.
func (s *CoordStore) OweEnd(ctx context.Context, turnID string, ending json.RawMessage) (PendingRun, bool, error) {
	if len(bytes.TrimSpace(ending)) == 0 {
		return PendingRun{}, false, fmt.Errorf("sandbox: run %s is owed an end with no ending", turnID)
	}
	// THE ROW AS IT STANDS where an end is owed already: the caller records
	// that one, so it is answered rather than the zero row a write that
	// changed nothing answers.
	var seen PendingRun
	written, won, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		seen = *run
		if len(run.OwedEnd) > 0 || run.EndRecorded {
			return false
		}
		run.OwedEnd = bytes.Clone(ending)
		return true
	})
	if err != nil || won {
		return written, won, err
	}
	return seen, false, nil
}

// ClearEnd removes the end a row owes once the log has taken it, and marks it
// recorded in the same write. See the contract on [PendingStore].
func (s *CoordStore) ClearEnd(ctx context.Context, turnID string, owed json.RawMessage) (bool, error) {
	_, cleared, err := s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		if len(run.OwedEnd) == 0 || !bytes.Equal(run.OwedEnd, owed) {
			return false
		}
		run.OwedEnd, run.EndRecorded = nil, true
		return true
	})
	return cleared, err
}

// foldPriorCalls is the turn's earlier successful calls, by name, with the
// successful ones of l's round added — the calls the log kept and the
// identities of the ones it dropped — and whether any call of the turn's is
// not accounted for by name ([BridgeLog.PriorCalls]).
//
// A SET OF NAMES, sorted, never a log: whether a round delivered is whether a
// call to a delivering tool succeeded, which a name and an outcome answer, and
// a set is bounded by the seat's catalogue rather than by how many calls a
// turn makes — and held to [MaxPriorCalls] all the same, past which it says
// its record is incomplete rather than grow.
//
// AND A CALL STILL IN FLIGHT MAKES IT INCOMPLETE ([BridgeLog.InFlight]): its
// outcome is not known, so no reader of the fold may say from it that the
// round did not reply.
func foldPriorCalls(l BridgeLog) ([]string, bool) {
	names := slices.Clone(l.PriorCalls)
	incomplete := l.PriorCallsElided || !l.ElisionKnown() || len(l.InFlight) > 0
	add := func(name string) {
		switch {
		case name == "" || slices.Contains(names, name):
		case len(names) >= MaxPriorCalls:
			incomplete = true
		default:
			names = append(names, name)
		}
	}
	for _, call := range l.Calls {
		if !call.Failed {
			add(call.Name)
		}
	}
	for _, e := range l.Unlisted() {
		if e.Calls > e.Failed {
			add(e.Name)
		}
	}
	slices.Sort(names)
	return names, incomplete
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
		// job the row holds, or it was refused above. And what the claim's
		// turn said it does with the round is the claim's, which this hands
		// back: the next claim's turn says it again ([LaunchRecord.ResumeCarry]).
		run.Launch.ResumeCarry = ""
		if release.Published {
			run.Launch.Published = true
		}
		if at := release.CollectFailedAt; !at.IsZero() {
			run.Launch.CollectFailures++
			if run.Launch.CollectFailingSince.IsZero() {
				run.Launch.CollectFailingSince = at.UTC()
			}
		}
		if at := release.ResumeFailedAt; !at.IsZero() {
			run.Launch.ResumeFailures++
			if run.Launch.ResumeFailingSince.IsZero() {
				run.Launch.ResumeFailingSince = at.UTC()
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
		// THE FIRST OF THEM, held to [MaxAudienceHandles], in the order the
		// resolution names them — a `team` audience its unit's lead and then
		// its people in the chart's order, the fallback its lead chain
		// nearest first — so past the bound a unit keeps its lead and its
		// first members, and a chain its nearest leads.
		run.AudienceHandles = slices.Clone(q.Answerers.Handles[:min(len(q.Answerers.Handles),
			MaxAudienceHandles)])
		run.AudienceFallback = q.Answerers.Fallback
		run.ParkedInputTokens, run.ParkedOutputTokens = q.InputTokens, q.OutputTokens
		run.ParkedCacheReadTokens, run.ParkedCacheWriteTokens = q.CacheReadTokens, q.CacheWriteTokens
		// A NEW QUESTION, measured from its own asking, with nothing yet
		// recorded against it and nothing yet declined: an answer and the
		// replies let go of belong to the question they were matched to.
		run.AskedAt = q.AskedAt
		run.Answer, run.DeclinedAnswers, run.AnswerRefusal = nil, nil, nil
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
	return s.recordOutcome(ctx, turnID, launch, answer, nil, fence)
}

// RecordRefusal records a reply too large to be the answer as the outcome of a
// parked run's question, under the recording node's lease. See the contract on
// [PendingStore].
func (s *CoordStore) RecordRefusal(ctx context.Context, turnID, launch string, answer RecordedAnswer,
	refusal AnswerRefusal, fence Fence,
) (PendingRun, bool, error) {
	if len(answer.Events) > 0 {
		return PendingRun{}, false, fmt.Errorf("sandbox: the refusal of a reply to run %s carries %d of "+
			"its deliveries, which are the bytes the row could not carry", turnID, len(answer.Events))
	}
	if err := refusal.check(answer.EventIDs); err != nil {
		return PendingRun{}, false, fmt.Errorf("sandbox: the refusal of a reply to run %s: %w", turnID, err)
	}
	return s.recordOutcome(ctx, turnID, launch, answer, &refusal, fence)
}

// check reports what is wrong with a refusal as one recorded against the
// answer naming answer's deliveries: it names exactly those, and its figures
// are a reply past a share.
func (r AnswerRefusal) check(answer []string) error {
	switch {
	case len(r.EventIDs) == 0 || !slices.Equal(r.EventIDs, answer):
		return fmt.Errorf("it names deliveries %s, and its answer names %s",
			words.And(r.EventIDs), words.And(answer))
	case r.MaxBytes <= 0 || r.Bytes <= r.MaxBytes:
		return fmt.Errorf("it states a reply of %d bytes against a share of %d, which is no refusal",
			r.Bytes, r.MaxBytes)
	}
	if n := addressBytes(r.Address); n > MaxRefusalAddressBytes {
		return fmt.Errorf("its address weighs %s on the row, and a refusal's may weigh %s",
			kept.SizeUp(n), kept.Size(MaxRefusalAddressBytes))
	}
	return nil
}

// recordOutcome is [CoordStore.RecordAnswer] and [CoordStore.RecordRefusal]:
// one compare-and-set recording answer as the question's outcome, the refusal
// it is beside it where refusal is set, and no refusal where it is not.
func (s *CoordStore) recordOutcome(ctx context.Context, turnID, launch string, answer RecordedAnswer,
	refusal *AnswerRefusal, fence Fence,
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
	// HELD TO ITS SHARE OF THE ROW, as written and as its decline would owe
	// it, before anything is written: see [MaxAnswerBytes]. MEASURED ONCE,
	// here, since what it weighs is the answer's alone, and REFUSED INSIDE
	// THE WRITE, only once the row as it stands is found still waiting on the
	// question ([PendingRun.takesAnswer]): a reply to a run answered another
	// way, moved on or ending since the caller matched it is that run's
	// ordinary loss whatever it weighs. Refused before the row was read, it
	// was announced as a reply too large for a question nobody was waiting
	// on any more.
	fits := answerFits(turnID, answer)
	if fits != nil && !errors.Is(fits, ErrAnswerTooLarge) {
		return PendingRun{}, false, fits
	}
	ownedBy := int64(0)
	var refused error
	recorded, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		// EVERY ATTEMPT DECIDES AFRESH: the store re-reads and re-decides a
		// lost race, and a refusal the last read did not make is no answer.
		refused = nil
		if superseded(*run, fence) {
			ownedBy = run.OwnerEpoch
			return false
		}
		ownedBy = 0
		if !run.takesAnswer(launch, answer.EventIDs) {
			return false
		}
		if fits != nil {
			refused = fits
			return false
		}
		recorded := answer
		recorded.From = run.Status
		run.Status = StatusAnswered
		run.Answer = &recorded
		// THE REFUSAL IN THE SAME WRITE, and none beside an answer: a row
		// never holds an answer half recorded as a refusal ([PendingRun.Refused]).
		run.AnswerRefusal = nil
		if refusal != nil {
			kept := *refusal
			kept.EventIDs, kept.Address = slices.Clone(refusal.EventIDs), maps.Clone(refusal.Address)
			run.AnswerRefusal = &kept
		}
		return true
	})
	switch {
	case err == nil && ownedBy > 0:
		err = fmt.Errorf("sandbox: recording an answer to run %s, which a newer lease (epoch %d) "+
			"than the recording node's (%d) owns: %w", turnID, ownedBy, fence.Epoch, ErrSeatNotHeld)
	case err == nil && refused != nil:
		return PendingRun{}, false, refused
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
		run.Answer, run.AnswerRefusal = nil, nil
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

// ErrHeldCallUnclaimed is a held call's mark or result asked of a row that is
// not the claim of its answer: the run moved on, was let go of, or another
// lease fenced it. Nothing was written, and the call is not to be made.
var ErrHeldCallUnclaimed = errors.New("sandbox: the run is not the claim that holds the held call's answer")

// heldClaim reports whether run is the claim of launch's answer that may make
// its held call: [PendingStore.MarkHeldCall]'s terms.
func heldClaim(run PendingRun, launch string, fence Fence) bool {
	return run.Status == StatusResumed && run.LaunchID == launch && run.Answer != nil &&
		run.HeldCall != nil && run.HeldCall.LaunchID == launch && !superseded(run, fence)
}

// MarkHeldCall records that a claim's resume is making its run's held call. See
// the contract on [PendingStore].
func (s *CoordStore) MarkHeldCall(ctx context.Context, turnID, launch string, fence Fence) (HeldCall, bool, error) {
	var held HeldCall
	claimed := false
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if claimed = heldClaim(*run, launch, fence); !claimed {
			return false
		}
		held = *run.HeldCall
		if held.Calling() {
			return false
		}
		held.CallingAt = s.clock()
		run.HeldCall = &held
		return true
	})
	switch {
	case err != nil:
		return HeldCall{}, false, err
	case !claimed:
		return HeldCall{}, false, fmt.Errorf("%w: run %s, launch %s", ErrHeldCallUnclaimed, turnID, launch)
	}
	return held, won, nil
}

// waitsOnDelegates reports whether run's held call heldID waits on its
// delegates on launch: [PendingStore.GiveDelegateVerdict]'s terms.
func waitsOnDelegates(run PendingRun, launch, heldID string) bool {
	return slices.Contains(Awaiting, run.Status) && run.Ending == nil && run.LaunchID == launch &&
		run.HeldCall != nil && run.HeldCall.ID == heldID && run.HeldCall.Delegated()
}

// GiveDelegateVerdict records one delegate's answer. See the contract on
// [PendingStore].
func (s *CoordStore) GiveDelegateVerdict(ctx context.Context, turnID, launch, heldID string,
	v HeldVerdict) (HeldCall, bool, error) {

	if err := v.validate(); err != nil {
		return HeldCall{}, false, fmt.Errorf("sandbox: a verdict on held call %s of run %s: %w", heldID, turnID, err)
	}
	var held HeldCall
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if !waitsOnDelegates(*run, launch, heldID) {
			return false
		}
		d := *run.HeldCall.Delegation
		if !slices.Contains(d.Delegates, v.By) ||
			slices.ContainsFunc(d.Given, func(g HeldVerdict) bool { return g.By == v.By }) {
			return false
		}
		given := v
		given.Findings = slices.Clone(v.Findings)
		d.Given = append(slices.Clone(d.Given), given)
		held = *run.HeldCall
		held.Delegation = &d
		run.HeldCall = &held
		return true
	})
	if err != nil || !won {
		return HeldCall{}, false, err
	}
	return held, true, nil
}

// EscalateHeld puts a call waiting on its delegates to people. See the
// contract on [PendingStore].
func (s *CoordStore) EscalateHeld(ctx context.Context, turnID, launch, heldID string,
	e HeldEscalation) (bool, error) {

	if err := e.validate(); err != nil {
		return false, fmt.Errorf("sandbox: escalating held call %s of run %s: %w", heldID, turnID, err)
	}
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if !waitsOnDelegates(*run, launch, heldID) {
			return false
		}
		d := *run.HeldCall.Delegation
		escalated := HeldEscalation{Overruled: slices.Clone(e.Overruled), Engine: slices.Clone(e.Engine), At: e.At}
		d.Escalated = &escalated
		held := *run.HeldCall
		held.Delegation = &d
		run.HeldCall = &held
		return true
	})
	return won, err
}

// RecordHeldResult records what a marked held call came to: its result and the
// call as its round's log keeps it, on the row, then the call in that log. See
// the contract on [PendingStore].
func (s *CoordStore) RecordHeldResult(ctx context.Context, turnID, launch string, made HeldMade,
	fence Fence) (bool, error) {

	claimed, already := false, false
	var (
		held  HeldCall
		round string
	)
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if claimed = heldClaim(*run, launch, fence) && run.HeldCall.Calling(); !claimed {
			return false
		}
		held, round = *run.HeldCall, run.Round
		if already = held.Made(); already {
			return false
		}
		held.Result, held.MadeAt = made.Result, s.clock()
		call := approvedCall(made.Call, held)
		held.Call = &call
		run.HeldCall = &held
		return true
	})
	switch {
	case err != nil:
		return false, err
	case !claimed:
		return false, fmt.Errorf("%w: run %s, launch %s", ErrHeldCallUnclaimed, turnID, launch)
	case !won && !already:
		return false, nil
	case held.Call == nil:
		// A RESULT A BUILD WITHOUT THE CALL'S ENTRY RECORDED: nothing to
		// append, and the result stands.
		return true, nil
	}
	// THE CALL, AFTER ITS RESULT, in its round's log and once by the held
	// call's id: the attempt that recorded the result, or the retry that
	// found it recorded by one that stopped before this append. Where this
	// write never lands, the next job's opening appends it ([OwedCall]).
	if _, _, err := s.runs.MutateSandboxRunPart(ctx, turnID, coord.RunLog, func(current coord.Record, found bool) ([]byte, error) {
		if !found {
			return nil, nil
		}
		var log BridgeLog
		if err := json.Unmarshal(current.Value, &log); err != nil {
			return nil, fmt.Errorf("sandbox: decode the log of run %s: %w", turnID, err)
		}
		next, appended := log.withApproved(round, *held.Call)
		if !appended {
			return nil, nil
		}
		return json.Marshal(next)
	}); err != nil {
		return false, fmt.Errorf("sandbox: append run %s's approved call to its log: %w", turnID, err)
	}
	return true, nil
}

// approvedCall is the call a person approved as its round's log keeps it
// ([HeldCall.Call]): answered as made says, named the held call's tool and
// id, stamped with the instant its result was recorded, marked approved, and
// with no arguments, no digest of them and no output, which ride the row
// sealed ([BridgeCall.Withheld]) — every other field held to a call's share
// ([keptCall]), as the log holds it and as the row carries it, on the held
// call and on to the reset that owes it to the log ([OwedCall]). With no place
// in either order either, and no mark: each is the log's to give it as it
// takes it ([BridgeLog.withApproved]), and a call the store appends for its
// own round is never late ([BridgeCall.Late]) — nor one nobody waited for
// ([BridgeCall.Abandoned]), which the bridge alone marks: the resume made this
// one on the engine host, and no harness gave it up. NOR DOES IT CARRY AN ID
// OF A HARNESS'S ([BridgeCall.CallID]), which the bridge gives the calls a box
// makes: no harness named this one, and what the box's session holds is the
// HELD call's, under the held call's own id.
func approvedCall(made BridgeCall, held HeldCall) BridgeCall {
	made.Name, made.Approved, made.At, made.HeldID = held.Tool, true, held.MadeAt, held.ID
	made.Args, made.Output, made.Held, made.Seq, made.Appended, made.Digest = "", "", false, 0, 0, ""
	made.Late, made.Abandoned, made.CallID = false, false, ""
	return keptCall(made, MaxBridgeArgsBytes)
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
		run.Answer, run.AnswerRefusal = nil, nil
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
			Reason: d.Reason, Detail: d.Detail, ErrorKind: d.ErrorKind,
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
		// a copy to hand back, or a refusal, whose reply goes back as the
		// delivery it came in, kept on the seat's inbox until the refusal
		// leaves the run ([AnswerRefused]).
		_, refused := run.Refused()
		owedRefusal := run.answerOwed() && refused
		ending.Returned = len(run.HandBack) > 0 ||
			(run.answerOwed() && len(run.Answer.Events) > 0) || owedRefusal
		run.EndingReturnsRefusal = owedRefusal
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
		run.PausedAt, run.BoxSince = time.Time{}, time.Time{}
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
func (s *CoordStore) AttachSandbox(ctx context.Context, turnID string, box BoxRef, fence Fence) (err error) {
	defer origin.Stamp(&err)
	var job handedSet
	_, attached, err := s.mutateFenced(ctx, turnID, fence, func(run *PendingRun) bool {
		job = handedSet{}
		// A BOX THE ROW DID NOT NAME IS DATED HERE, and one it already
		// names keeps its date — see [PendingRun.BoxSince]: a job continued
		// in its turn's kept box attaches that box again, twice, and a
		// re-dated box would never age past [KeptBoxMaxAge].
		if run.SandboxID != box.SandboxID {
			run.BoxSince = s.clock()
		}
		run.SandboxID = box.SandboxID
		run.CommandID = box.CommandID
		// A COMMAND IS A JOB THAT STARTED, recorded on the job's own record
		// where nothing that forgets the box reaches it — see
		// [LaunchRecord.Started]. Only ever set: the attach before the job
		// names no command, and unsays nothing.
		if box.CommandID != "" {
			run.Launch.Started = true
			// AND WHAT THE JOB WAS HANDED, named for the job: what a
			// capture of its box scrubs out, written beside the row once
			// this write has landed ([CoordStore.Handed]).
			if box.Handed != "" {
				job = handedSet{Launch: run.LaunchID, LaunchSeq: run.LaunchSeq, Sealed: box.Handed}
			}
			// And which brief ([LaunchRecord.Brief]).
			run.Launch.Brief = box.Brief
			// AND THE SESSION ITS SCRIPT STARTS OF ITS OWN in a continued
			// one's place, for which of the two its box said it ran
			// ([PendingRun.SessionRan]).
			run.Launch.FreshSessionID = box.FreshSessionID
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
	if job.Launch != "" {
		s.writeHanded(ctx, turnID, job)
	}
	return nil
}

// handedSet is the part kept beside a run's row holding what its job's box
// was handed, sealed ([CoordStore.Handed]), named for the job it was written
// for: by its launch, and every token the completion poll renewed in the box
// since ([CoordStore.AddHanded]).
//
// BESIDE THE ROW, NEVER ON IT ([coord.RunHanded]): up to [MaxHandedBytes]
// before sealing — a launch whose environment holds a large file comes near
// it — which every read of the row would carry, every git request of a coding
// box and every listing among them, though only three read it: a capture of
// the job's session, to scrub it out; the next launch's opening, to carry it
// into a box it continues in; and the poll's record of a token it renews in
// the box, which reads the part and grows it by one compare-and-swap, or marks
// it ([handedSet.Incomplete]).
type handedSet struct {
	// Launch is the job the set was written for ([PendingRun.LaunchID]): a
	// set read for another job is none.
	Launch string `json:"launch"`
	// LaunchSeq is that job's ordinal in its turn ([PendingRun.LaunchSeq]),
	// so a write that arrives after a later job's never replaces the later
	// job's set.
	LaunchSeq int64 `json:"launch_seq"`
	// Sealed is the set as [SessionKeep.SealHanded] sealed it.
	Sealed string `json:"sealed"`
	// Incomplete is why the job's set no longer holds every value its box
	// holds, where a refresh could not add a token it wrote there
	// ([PendingStore.AddHanded]); Sealed is then empty. Words of the
	// engine's, never a value, since it sits unsealed. AN EMPTY SEAL IS WHAT
	// REFUSES, so an older build, which reads none of this, takes the job as
	// one that keeps nothing and refuses its captures as this build does —
	// and a later job continuing in the box carries nothing of it, and
	// refuses its own ([Coordinator.carryHanded]).
	Incomplete string `json:"incomplete,omitempty"`

	// Extra is every key of the part this build does not know, kept
	// verbatim through a renewal's write of it (wirekeys.go), and dropped
	// with the job's set by the next job's attach, which replaces it whole.
	// Filled by the decode, never by a caller.
	Extra map[string]json.RawMessage `json:"-"`
}

// of is the sealed set where it was written for launch, and "" otherwise.
func (h handedSet) of(launch string) string {
	if launch == "" || h.Launch != launch {
		return ""
	}
	return h.Sealed
}

// incompleteFor is why launch's set holds less than its box does, or "" where
// the set is not launch's or not marked so.
func (h handedSet) incompleteFor(launch string) string {
	if launch == "" || h.Launch != launch {
		return ""
	}
	return h.Incomplete
}

// readHanded reads the handed set beside turnID's run — the zero set where
// there is none.
func (s *CoordStore) readHanded(ctx context.Context, turnID string) (handedSet, error) {
	record, found, err := s.runs.SandboxRunPart(ctx, turnID, coord.RunHanded)
	switch {
	case err != nil:
		return handedSet{}, fmt.Errorf("read what run %s's box was handed: %w", turnID, err)
	case !found:
		return handedSet{}, nil
	}
	var held handedSet
	if err := json.Unmarshal(record.Value, &held); err != nil {
		return handedSet{}, fmt.Errorf("decode what run %s's box was handed: %w", turnID, err)
	}
	return held, nil
}

// writeHanded writes job's set beside turnID's run, once the attach that
// names the job has landed — never before it: a set written for a job whose
// attach then did not land would be read by the next launch's opening as
// what the box the row still names was handed, and carried forward in place
// of what that box does hold.
//
// NEVER OVER A LATER JOB'S: a set a later launch wrote is left as it is. NOR
// OVER ITS OWN JOB'S MARK ([handedSet.Incomplete]): only a poll of the running
// job writes one, after a token it wrote into the box found no set to join,
// so a write of the job's own set that reaches the store after it — applied
// once its asker had given up — would hold none of that token.
//
// A FAILURE IS LOGGED, NOT RETURNED: the attach has landed and its job runs,
// and an attach answered as refused would abandon a box the row names. A job
// whose set is not beside its row has every capture refused
// ([ErrHandedUnknown]), which is what a launch that could not seal its set
// gets: the session is not kept, rather than kept scrubbed of less than its
// box was handed.
func (s *CoordStore) writeHanded(ctx context.Context, turnID string, job handedSet) {
	raw, err := json.Marshal(job)
	if err == nil {
		_, _, err = s.runs.MutateSandboxRunPart(ctx, turnID, coord.RunHanded,
			func(current coord.Record, found bool) ([]byte, error) {
				if found {
					var held handedSet
					if json.Unmarshal(current.Value, &held) == nil && (held.LaunchSeq > job.LaunchSeq ||
						held.incompleteFor(job.Launch) != "") {
						return nil, nil
					}
				}
				return raw, nil
			})
	}
	if err != nil {
		log.WarnContext(ctx, "sandbox_handed_not_kept", "turn_id", turnID, "launch", job.Launch,
			"error", err.Error(),
			"detail", "the job runs, and every capture of its session is refused, since what its box "+
				"was handed cannot be read back to scrub it out")
	}
}

// Handed reads what run's job's box was handed, sealed. See the contract on
// [PendingStore].
func (s *CoordStore) Handed(ctx context.Context, run PendingRun) (string, error) {
	held, err := s.readHanded(ctx, run.TurnID)
	if err != nil {
		return "", fmt.Errorf("sandbox: %w", err)
	}
	if why := held.incompleteFor(run.LaunchID); why != "" {
		return "", fmt.Errorf("%w: %s", ErrHandedIncomplete, why)
	}
	return held.of(run.LaunchID), nil
}

// AddHanded hands run's job's sealed set to add before a refresh writes a
// token into the job's box. See the contract on [PendingStore].
//
// THE ROW FIRST, THEN THE PART. The part's own compare-and-swap decides the
// set (the job's, by its launch, as [handedSet.of] reads it), and the read of
// the row before it refuses a poll whose listing is older than the job's end:
// a relaunch's opening reads the set before it resets the row
// ([CoordStore.resetLaunch]) and a stop's or a loss's capture reads it after
// the ending's decision, so a token recorded after either would be in a set
// nobody reads again. Between the two reads is one round trip, which no claim,
// collection and relaunch fits in.
func (s *CoordStore) AddHanded(ctx context.Context, run PendingRun,
	add func(sealed string) (next, incomplete string)) (bool, error) {

	current, found, err := s.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		return false, fmt.Errorf("sandbox: read run %s to record what its box is handed: %w", run.TurnID, err)
	case !found || current.LaunchID != run.LaunchID || current.LaunchSeq != run.LaunchSeq ||
		current.Status != StatusRunning || current.Launch.Collected || current.Lost != nil ||
		current.endDecided():
		return false, nil
	}
	// SET ON EVERY ASK OF decide, which runs again on a lost race.
	later, wrote := false, false
	_, changed, err := s.runs.MutateSandboxRunPart(ctx, run.TurnID, coord.RunHanded,
		func(record coord.Record, found bool) ([]byte, error) {
			later, wrote = false, false
			var held handedSet
			if found && json.Unmarshal(record.Value, &held) != nil {
				// A SET THAT DOES NOT DECODE is none the job's captures
				// could read either: marked below, like a set never written.
				held = handedSet{}
			}
			switch {
			case held.Launch == run.LaunchID && held.Sealed == "":
				// MARKED ALREADY, or kept empty: every capture of the job
				// is refused, so the token adds nothing to keep.
				return nil, nil
			case held.Launch == run.LaunchID:
				// THE JOB'S OWN PART, written back with the keys a later
				// build put in it (wirekeys.go): a renewal changes the seal
				// and the mark, and nothing else of it.
				next, why := add(held.Sealed)
				if next == held.Sealed {
					return nil, nil
				}
				held.Sealed, held.Incomplete = next, ""
				if next == "" {
					held.Incomplete = why
				}
			case held.LaunchSeq > run.LaunchSeq:
				// A LATER JOB'S SET: this job is over, whatever the row read
				// a moment ago said.
				later = true
				return nil, nil
			default:
				// NONE FOR THE JOB — its launch sealed none, or its write
				// did not land — MARKED as the job's, refusing every
				// capture of it as its captures are refused already, so a
				// write of the launch's own set that lands late, its answer
				// lost, cannot land without this token in it.
				held = handedSet{Launch: run.LaunchID, LaunchSeq: run.LaunchSeq, Incomplete: handedUnkept}
			}
			wrote = true
			return json.Marshal(held)
		})
	switch {
	case err != nil:
		return false, fmt.Errorf("sandbox: record what run %s's box is handed: %w", run.TurnID, err)
	case later, wrote && !changed:
		// A LATER JOB'S SET, or a run that ended under the write, which the
		// store undid.
		return false, nil
	}
	return true, nil
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
		// the reaper looks for every tick and never finds. And the box's
		// date with it, which is the box's and no later one's.
		run.SandboxID, run.CommandID, run.PausedAt = "", "", time.Time{}
		run.BoxSince = time.Time{}
		return true
	})
	return err
}

// AppendBridgeCall makes one step of a tool call a bridged run made: the
// call's entry written ahead of its run, its one update as it finishes, or the
// withdrawal of an entry whose call was then not run ([AppendStep]).
//
// NO FENCE, and that is deliberate. Every other mutation here is an ownership
// decision — a claim, a status flip, a pause — and a node whose lease has
// moved must not make one. This is a LOG APPEND: a record's call already ran
// and its effect already happened, and refusing to record it because the seat
// moved mid-run would lose evidence of something that is true either way; a
// begin records a call its opening admitted under the seat's fence. The log's
// own version still guards the write, so two concurrent appends serialise
// rather than clobbering each other.
//
// ON THE LOG ALONE: the condition — the log is open for the call's job, or
// for its round — is the log's own field, so each step is one compare-and-swap
// of one key, and the run's record is neither read nor written for it. The
// job's spend is the one fact the record keeps of an append, written after a
// record is in the log, only for the log's own job and only where the total
// moved: a call answered by a builtin costs the engine nothing, and its record
// leaves the record alone, and so does a late call of the round, whose spend
// is dropped with its job, and every begin and withdrawal ([BridgeLog.take]).
// THE ONE WRITER of a call's steps and of its spend, so the job check and the
// spend stay in one place whichever step a call is at.
//
// A run whose log is gone is not an error: the run ended while a late call was
// in flight, which is the ordinary shape of a box shutting down. Nor is a call
// of a round the log has moved on from, or an append naming no job at all (see
// the pin on [PendingStore]). The append is simply dropped, and the caller —
// which must not fail the box's call over a record, and runs no call whose
// begin was dropped — goes on as it would have. A late call of the round is
// answered recorded, beside [ErrJobReplaced]: its spend can reach no record.
func (s *CoordStore) AppendBridgeCall(ctx context.Context, turnID string, a BridgeAppend) (bool, error) {
	if !a.Step.Valid() {
		return false, fmt.Errorf("sandbox: %q is no step of a bridged call", a.Step)
	}
	if a.Launch == "" || a.Call.Name == "" {
		return false, nil
	}
	stamped := func(call BridgeCall) BridgeCall {
		if call.At.IsZero() {
			call.At = s.clock()
		}
		return call
	}
	// SET ON EVERY ASK OF decide, which runs again on a lost race: what the
	// log as the last attempt read it takes the append for, and whether that
	// attempt had anything to write.
	taken, wrote := takeNone, false
	_, won, err := s.runs.MutateSandboxRunPart(ctx, turnID, coord.RunLog, func(current coord.Record, found bool) ([]byte, error) {
		taken, wrote = takeNone, false
		if !found {
			return nil, nil
		}
		var logged BridgeLog
		if err := json.Unmarshal(current.Value, &logged); err != nil {
			return nil, fmt.Errorf("sandbox: decode the log of run %s: %w", turnID, err)
		}
		var refused error
		if taken, wrote, refused = logged.take(a, stamped); refused != nil || !wrote {
			return nil, refused
		}
		return json.Marshal(logged)
	})
	switch {
	case err != nil:
		// A BEGIN THE LOG HAS NO ROOM FOR among them, as it is
		// ([ErrCallsInFlight]): nothing was written.
		return false, err
	case taken == takeNone, wrote && !won:
		// NO LOG OPEN FOR THE JOB OR ITS ROUND, or one the run's end removed
		// under the write that landed in it, which the store undid: a run
		// that is gone, or a round that is over.
		return false, nil
	case a.Step != AppendRecord:
		// A BEGIN OR A WITHDRAWAL: no spend rides it.
		return true, nil
	case taken != takeOwn:
		// A LATE CALL OF THE ROUND, in the log and paid by nobody: the row
		// holds a later job, whose resume would pay it as its own. Said, so a
		// caller asking again for an earlier attempt's spend stops asking.
		return true, ErrJobReplaced
	}
	// Onto THIS job's record, and only while the row holds it: the log's
	// opening for the next job comes after the reset that names it on the row
	// ([CoordStore.BeginLaunch]), so between the two an append the log still
	// takes as its job's own finds the row moved on, and its spend is dropped
	// with the job. Written whether or not this append moved the log: one
	// asked again finds its calls there and may be the first whose spend
	// reaches the record.
	_, _, err = s.mutateAny(ctx, turnID, func(run *PendingRun) bool {
		if a.Launch != run.LaunchID {
			return false
		}
		newest := run.Launch.Bridged.Newest(a.Spent)
		if newest == run.Launch.Bridged {
			return false
		}
		run.Launch.Bridged = newest
		return true
	})
	if err != nil {
		return true, fmt.Errorf("sandbox: record what run %s's bridged calls cost: %w", turnID, err)
	}
	return true, nil
}

// logTake is what a run's log takes one bridged append for
// ([BridgeLog.take]).
type logTake int

const (
	// takeNone is an append the log takes nothing of: one naming no job, or
	// a job of a round the log has moved on from, or a job the log is not on
	// and no round — a call that outlived a round a relaunch replaced with a
	// new one, or the run's end.
	takeNone logTake = iota
	// takeOwn is an append of the log's own job ([BridgeLog.Launch]): its
	// calls listed, and its spend written onto the job's record.
	takeOwn
	// takeLate is an append of an earlier job of the log's round
	// ([BridgeLog.Round]): its calls listed, marked late ([BridgeCall.Late]),
	// and its spend written nowhere.
	takeLate
)

// take decides what l does with one bridged append, a, and does it: THE ONE
// DECISION an append is taken on, made once per ask of the append's
// compare-and-swap ([CoordStore.AppendBridgeCall]) on the log as that ask read
// it — whether the log takes the append at all, whether the call it lists is
// marked late, and whether the append's spend is the job's to write
// ([logTake]) — answered with whether it wrote anything, which an append
// asked again does not, and the refusal of a begin the log has no room for
// ([ErrCallsInFlight]). stamp dates a call its caller did not.
//
// BY THE LOG'S OWN FIELDS: the job it is open for, and the round it holds,
// which an opening for a later job of the same round keeps and one of another
// round replaces ([BridgeLog.opened]). So a call still running when a relaunch
// carried its round on is listed in the round it was made in, and one of a
// round a send-back replaced is not: the new round numbers its own calls. An
// append that names no round ([BridgeAppend.Round]) is taken by its own job's
// log alone. A BEGIN IS TAKEN AS A RECORD IS: a call a replaced job's box makes
// while its opening still serves it — the relaunch that replaced the job opens
// its own session a moment after it moved the log on — is a call of the round,
// whose update the log marks late.
//
// BY THE STEP ([AppendStep]):
//
//   - A BEGIN writes the call's entry in flight ([keptEntry]): once, an entry
//     the log holds already, or a call it lists, writing nothing — a begin
//     asked again after its answer was lost — and none past
//     [MaxCallsInFlight].
//   - A RECORD is the call's ONE UPDATE: its entry, found by its identity
//     ([BridgeLog.inFlightAt]), taken out, and the finished call listed in
//     the same write — where the log does not list it already: a record
//     ASKED AGAIN, one that landed and whose answer was lost, lists nothing,
//     the call and what it carried being in the log once. A call with no
//     entry, one answered without running, is listed as it is. Each call is
//     held to the log's budget ([BridgeLog.appendCall]), and is MARKED LATE
//     exactly where the append is late: the mark is the log's to give, as a
//     call's number is, and replaces whatever a caller handed in.
//   - A WITHDRAWAL takes the entry of a call that was then not run out, and
//     lists nothing.
func (l *BridgeLog) take(a BridgeAppend, stamp func(BridgeCall) BridgeCall) (logTake, bool, error) {
	if a.Launch == "" || a.Call.Name == "" {
		return takeNone, false, nil
	}
	var taken logTake
	switch {
	case a.Launch == l.Launch:
		taken = takeOwn
	case a.Round != "" && a.Round == l.Round:
		taken = takeLate
	default:
		return takeNone, false, nil
	}
	at := l.inFlightAt(a.Call)
	switch a.Step {
	case AppendBegin:
		if at >= 0 || l.lists(a.Call) {
			return taken, false, nil
		}
		if len(l.InFlight) >= MaxCallsInFlight {
			return taken, false, ErrCallsInFlight
		}
		l.InFlight = append(slices.Clip(l.InFlight), stamp(keptEntry(a.Call)))
		return taken, true, nil
	case AppendWithdraw:
		if at < 0 {
			return taken, false, nil
		}
		l.InFlight = slices.Delete(slices.Clone(l.InFlight), at, at+1)
		return taken, true, nil
	}
	listed := l.lists(a.Call)
	if at < 0 && listed {
		return taken, false, nil
	}
	if at >= 0 {
		l.InFlight = slices.Delete(slices.Clone(l.InFlight), at, at+1)
	}
	if !listed {
		call := a.Call
		call.Late = taken == takeLate
		// A FINISHED CALL IS NOBODY'S TO WAIT FOR: it carries neither.
		call.Runner, call.Until = "", time.Time{}
		l.appendCall(stamp(call), a.Absorbed)
	}
	return taken, true, nil
}

// WatchLog follows the bridged tool log beside run turnID's record. See the
// contract on [PendingStore].
//
// THE WATCH'S FIRST STATE IS A READ FROM THE STREAM'S LEADER
// ([coord.SandboxRuns.SandboxRunPart]), made once the part's watch is open, in
// place of the watch's own certified first state, which may lag the newest
// write by a replica's lag: everything the store took before the read is in
// it, and everything after it reaches the watch, which then delivers only a
// state newer than one already delivered, or the log gone.
//
// THE PART'S WATCH IS THIS CALL'S OWN, under a context it cancels: on a read
// that fails, and once its goroutine ends — the caller's context done, or the
// part's watch closed — so nothing it started outlives it.
func (s *CoordStore) WatchLog(ctx context.Context, turnID string) (<-chan LogView, error) {
	ctx, stop := context.WithCancel(ctx)
	updates, err := s.runs.WatchSandboxRunPart(ctx, turnID, coord.RunLog)
	if err != nil {
		stop()
		return nil, fmt.Errorf("sandbox: watch the log of run %s: %w", turnID, err)
	}
	read, found, err := s.runs.SandboxRunPart(ctx, turnID, coord.RunLog)
	if err != nil {
		stop()
		return nil, fmt.Errorf("sandbox: read the log of run %s: %w", turnID, err)
	}
	out := make(chan LogView, 1)
	go func() {
		defer stop()
		defer close(out)
		var last uint64
		deliver := func(u coord.SandboxRunUpdate) bool {
			if u.Found && u.Record.Version <= last {
				return true
			}
			view := LogView{Found: u.Found, Version: u.Record.Version}
			if u.Found {
				if err := json.Unmarshal(u.Record.Value, &view.Log); err != nil {
					// A STATE THIS BUILD CANNOT READ is passed over, never
					// delivered as an empty log.
					log.WarnContext(ctx, "sandbox_log_view_unread", "turn_id", turnID, "error", err.Error())
					return true
				}
				last = u.Record.Version
			}
			// THE NEWEST STATE WINS, as the notes' watch has it ([CoordStore.WatchNotes]).
			select {
			case <-out:
			default:
			}
			select {
			case out <- view:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !deliver(coord.SandboxRunUpdate{Found: found, Record: read}) {
			return
		}
		first := true
		for u := range updates {
			if first {
				// THE WATCH'S OWN FIRST STATE, which the read above stands in for.
				first = false
				continue
			}
			if !deliver(u) {
				return
			}
		}
	}()
	return out, nil
}

// BridgeLog reads the bridged tool log of run's job. See the contract on
// [PendingStore].
func (s *CoordStore) BridgeLog(ctx context.Context, run PendingRun) (BridgeLog, error) {
	none := BridgeLog{Launch: run.LaunchID, Round: run.Round}
	record, found, err := s.runs.SandboxRunPart(ctx, run.TurnID, coord.RunLog)
	switch {
	case err != nil:
		return BridgeLog{}, fmt.Errorf("sandbox: read the log of run %s: %w", run.TurnID, err)
	case !found:
		return none, nil
	}
	var held BridgeLog
	if err := json.Unmarshal(record.Value, &held); err != nil {
		return BridgeLog{}, fmt.Errorf("sandbox: decode the log of run %s: %w", run.TurnID, err)
	}
	if held.Launch != run.LaunchID {
		return none, nil
	}
	return held, nil
}

// TurnLog reads the calls of run's turn as its job stands to hold them. See
// the contract on [PendingStore].
func (s *CoordStore) TurnLog(ctx context.Context, run PendingRun) (BridgeLog, error) {
	record, found, err := s.runs.SandboxRunPart(ctx, run.TurnID, coord.RunLog)
	switch {
	case err != nil:
		return BridgeLog{}, fmt.Errorf("sandbox: read the log of run %s: %w", run.TurnID, err)
	case !found:
		return BridgeLog{Launch: run.LaunchID, Round: run.Round}, nil
	}
	var held BridgeLog
	if err := json.Unmarshal(record.Value, &held); err != nil {
		return BridgeLog{}, fmt.Errorf("sandbox: decode the log of run %s: %w", run.TurnID, err)
	}
	// THE CALL A PERSON APPROVED IN THIS JOB, recorded on the row and not
	// yet in the log: a call of the round all the same.
	held = held.withHeld(run)
	// AND THE LOG AS THE JOB'S OPENING LEAVES IT, where that opening has not
	// landed: the round's calls and the turn's earlier ones, never none.
	if held.Launch != run.LaunchID {
		held = held.opened(run.LaunchID, logOpening{round: run.Round, answered: run.AnsweredJob,
			owed: run.OwedCall})
	}
	return held, nil
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
	if ask.Held != nil {
		// HELD TO ITS SHARE OF THE ROW before anything is written: see
		// [MaxHeldArgsBytes].
		if err := HeldArgsFit(ask.Held.Arguments); err != nil {
			return "", fmt.Errorf("sandbox: recording the call run %s held: %w", turnID, err)
		}
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
		run.Decision, run.Audience = ask.Decision, ask.To
		if ask.Held != nil {
			// A HELD CALL NAMES NO BRANCH, since the run did not choose to
			// stop: the branch an earlier question named stays for the
			// job that carries the work on.
			held := *ask.Held
			held.LaunchID = launch
			run.HeldCall = &held
		} else {
			run.Branch = ask.Branch
		}
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

// appendBounded adds one call and drops from the MIDDLE past the cap,
// answering the calls it dropped so their identities are kept
// ([BridgeLog.appendCall]).
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
// corrected by. A call NO SAVE POINT HOLDS ([BridgeCall.unheld]: a late one,
// or one its harness stopped waiting for) is told to every continuation of its
// round by its mark, which a dropped call keeps none of: dropped, it is only
// counted ([BridgeLog.LateElided]) and told as a count, past every save point,
// beside the calls that tally names. So the call dropped is the oldest
// ordinary one after the head, never the one just made; and only a log with no
// ordinary call left there gives up the oldest of the others, so the log stays
// at its cap whatever the run calls.
func appendBounded(calls []BridgeCall, elided int, next BridgeCall) ([]BridgeCall, int, []BridgeCall) {
	calls = append(calls, next)
	if len(calls) <= MaxBridgeCalls {
		return calls, elided, nil
	}
	head := MaxBridgeCalls / 2
	drop := len(calls) - MaxBridgeCalls
	// The candidates: everything after the head but the call just made.
	candidates := calls[head : len(calls)-1]
	// outlasts is a candidate the middle gives up only once no ordinary one
	// is left: a call that ended its job, or one no save point holds.
	outlasts := func(call BridgeCall) bool { return call.Ends() || call.unheld() }
	ordinary := 0
	for _, call := range candidates {
		if !outlasts(call) {
			ordinary++
		}
	}
	dropOrdinary := min(drop, ordinary)
	dropAny := drop - dropOrdinary
	kept := make([]BridgeCall, 0, MaxBridgeCalls)
	kept = append(kept, calls[:head]...)
	var dropped []BridgeCall
	for _, call := range candidates {
		switch {
		case !outlasts(call) && dropOrdinary > 0:
			dropOrdinary--
			dropped = append(dropped, call)
		case outlasts(call) && dropAny > 0:
			dropAny--
			dropped = append(dropped, call)
		default:
			kept = append(kept, call)
		}
	}
	kept = append(kept, next)
	return kept, elided + drop, dropped
}

// MarkSuspended writes the parked turn's state and opens the run to the
// completion poll. See the contract on [PendingStore].
//
// THROUGH THE LIVE WRITE, so a run whose ending was decided under its launch is
// told [ErrRunEnding] rather than a plain false: see the contract.
func (s *CoordStore) MarkSuspended(ctx context.Context, turnID string, suspension Suspension) (bool, error) {
	if n := len(suspension.State); n > ExecuteStateBytes {
		return false, fmt.Errorf("sandbox: the parked state of run %s is %d bytes, and its share of "+
			"the row is %d: %w", turnID, n, ExecuteStateBytes, ErrStateTooLarge)
	}
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

// ListRuns returns every run the store holds, whatever its status. See the
// contract on [PendingStore].
func (s *CoordStore) ListRuns(ctx context.Context) ([]PendingRun, error) {
	return s.list(ctx, func(PendingRun) bool { return true })
}

// MarkSessionStart records which session a job's box said it started. See the
// contract on [PendingStore].
func (s *CoordStore) MarkSessionStart(ctx context.Context, turnID, launch string, start SessionStart) (bool, error) {
	if !start.Valid() {
		return false, fmt.Errorf("sandbox: %q is no session a job's box starts", start)
	}
	if launch == "" {
		return false, nil
	}
	_, marked, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launch || run.Launch.SessionStart != "" {
			return false
		}
		run.Launch.SessionStart = start
		return true
	})
	return marked, err
}

// MarkEndedAtDeadline records that a job's collection saw it still working past
// its turn's cap and is ending it. See the contract on [PendingStore].
func (s *CoordStore) MarkEndedAtDeadline(ctx context.Context, turnID, launch string) (bool, error) {
	if launch == "" {
		return false, nil
	}
	_, marked, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launch || run.Launch.EndedAtDeadline {
			return false
		}
		run.Launch.EndedAtDeadline = true
		return true
	})
	return marked, err
}

// MarkHarnessEnd records how a job's harness ended, as a read that collected
// the job found it. See the contract on [PendingStore].
func (s *CoordStore) MarkHarnessEnd(ctx context.Context, turnID, launch string, end HarnessEnd) (bool, error) {
	if !end.Valid() {
		return false, fmt.Errorf("sandbox: %q is no way a job's harness ends; it is one of %s",
			end, words.Or(HarnessEnds))
	}
	if launch == "" {
		return false, nil
	}
	_, marked, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		next := run.Launch.HarnessEnd.after(end)
		if run.LaunchID != launch || next == run.Launch.HarnessEnd {
			return false
		}
		run.Launch.HarnessEnd = next
		return true
	})
	return marked, err
}

// MarkResumeBegun records what a claim's resumed turn does with its job's
// round. See the contract on [PendingStore].
//
// THROUGH THE WRITE A LIVE RUN TAKES ([CoordStore.mutate]): a run whose ending
// is decided is being ended, and no turn of it begins.
func (s *CoordStore) MarkResumeBegun(ctx context.Context, turnID, launch string, fence Fence,
	carry ResumeCarry) (bool, error) {

	if !carry.Valid() {
		return false, fmt.Errorf("sandbox: %q is nothing a resumed turn does with its round; it is one of %s",
			carry, words.Or(ResumeCarries))
	}
	_, marked, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusResumed || run.LaunchID != launch || outranked(*run, fence) ||
			run.Launch.ResumeCarry == carry {
			return false
		}
		run.Launch.ResumeCarry = carry
		return true
	})
	return marked, err
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
		case len(run.OwedEnd) > 0:
			refused = fmt.Errorf("sandbox: finish run %s: %w", turnID, ErrEndOwed)
			return coord.SandboxRunChange{}, refused
		}
		return coord.SandboxRunChange{Delete: true}, nil
	})
	switch {
	case err != nil && refused != nil && errors.Is(err, refused):
		// THE ROW AS IT STANDS rides an owed refusal, for the ending to
		// hand back, or record, what it still owes and finish again.
		if errors.Is(err, ErrHandBackOwed) || errors.Is(err, ErrAnswerOwed) || errors.Is(err, ErrEndOwed) {
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
func (s *CoordStore) read(ctx context.Context, turnID string) (_ PendingRun, _ uint64, _ bool, err error) {
	defer origin.Stamp(&err)
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
func decodeRun(record coord.Record) (_ PendingRun, err error) {
	defer origin.Stamp(&err)
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
