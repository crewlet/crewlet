package sandbox

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/events/types"
)

// ErrResumeUnavailable reports that this node cannot resume the run a
// completion refers to: a failure of routing, not of the run. The caller hands
// the completion back to reach the seat's node rather than settling the run
// done, which would tear the box down and discard the turn.
// [ErrResumeElsewhere] is the one another node or build can get past. See the
// package doc, "Resume routing".
var ErrResumeUnavailable = errors.New("sandbox: this node cannot resume the run")

// ErrResumeElsewhere is an [ErrResumeUnavailable] another node or build can get
// past: a seat this node does not hold, a parked state of a version or a reply
// kind this build does not read, a seat or company not this node's. An error
// wrapping it matches both. It is the node's failure on the seat's control
// topic, neither the run's nor the message's, and counts toward the resume
// give-up only once no live node may resume the run
// ([CoordinatorOptions.ResumableElsewhere]). See the package doc, "Resume
// routing".
var ErrResumeElsewhere = fmt.Errorf("%w; another node or build may", ErrResumeUnavailable)

// ErrResumeAbandoned reports a resumed turn that broke and must not be resumed
// again from the same completion.
//
// The counterpart of the dispatcher's abandon decision: a redelivery re-runs
// every call the resumed segment made, and a coding run most likely pushed
// already. A PANICKED turn or a resume REFUSED for a reason every retry meets
// (turn.ErrPermanent) is abandoned too. A resumer wraps its error in this only
// when the turn must not run again: a turn.Abandon, a panic outside the turn
// loop, a person's stop, a failed relaunch, an [Unresumable]. The coordinator
// never reverts the claim: it settles the delivery and run, reclaiming the box.
// Every other failure reverts and comes back.
var ErrResumeAbandoned = errors.New("sandbox: the resumed turn broke and must not be resumed again")

// Unresumable is a run no build will ever resume, as the engine judges it. It
// matches [ErrResumeAbandoned], so a resume that returns it ends the run before
// its turn begins, announced under Reason, with a person's reply handed back.
type Unresumable struct{ Reason, Detail string }

func (u *Unresumable) Error() string {
	return fmt.Sprintf("sandbox: no build will resume this run (%s): %s", u.Reason, u.Detail)
}

// Is matches [ErrResumeAbandoned]: no build will resume the run, so it ends.
func (u *Unresumable) Is(target error) bool { return target == ErrResumeAbandoned }

// note is what the run's ending announces.
func (u *Unresumable) note() *failureNote { return &failureNote{reason: u.Reason, detail: u.Detail} }

// abandonedNote is what a resume abandoned before its turn began announces.
func abandonedNote(err error) *failureNote {
	if u, ok := errors.AsType[*Unresumable](err); ok {
		return u.note()
	}
	return &failureNote{reason: types.ExecutorFailureResumeBroken, detail: resumeBrokenDetail}
}

// ErrResumeHeld reports a resumed turn its seat's conditions refused — the
// token budget its review or its relaunch was refused under, today — returned
// wrapped in a [HeldError] that says what the refusal waits for.
//
// NOT A FAILURE, AND NOT A RETRY. The coordinator hands the claim back holding
// the resume until the refusal lifts ([PendingRun.HeldFor]) and spends the
// completion that drove it: the job stays collected in its paused box, the seat
// stays held, and the poll announces the job again once the hold lifts. An
// ordinary retry would re-collect the job and re-run its review into the same
// refusal.
var ErrResumeHeld = errors.New("sandbox: the resume waits for its seat's conditions")

// HeldError is a resume refused by its seat's conditions — see [ErrResumeHeld]
// — and the [Refusal] it waits on.
type HeldError struct {
	Refusal Refusal
	Err     error
}

func (e *HeldError) Error() string {
	return "sandbox: the resume is held (" + e.Refusal.Reason + "): " + e.Err.Error()
}

// Unwrap reports the refusal as both [ErrResumeHeld] and its own cause.
func (e *HeldError) Unwrap() []error { return []error{ErrResumeHeld, e.Err} }

// ErrHandBackOwed is a store's refusal to delete a run, or to record more
// copies on it, while it still owes the seat's inbox the copies it carries
// ([PendingRun.HandBack]). The answer is to publish and clear them first
// ([Coordinator.deliverHandBack]) and then ask again.
var ErrHandBackOwed = errors.New("sandbox: the run still owes the seat a reply it has not handed back")

// ErrAnswerOwed is a store's refusal to delete a run whose row still holds a
// person's reply no turn took ([PendingRun.Answer]). The reply's delivery was
// spent when it was recorded, so the row is the only thing still carrying it.
// The answer is to let it go back to the seat first ([PendingStore.OweHandBack],
// [Coordinator.letGoAnswer]) and then ask again.
var ErrAnswerOwed = errors.New("sandbox: the run still holds a person's reply that no turn took")

// ErrEndOwed is a store's refusal to delete a run whose row still owes the
// executor log its turn's end ([PendingRun.OwedEnd]): the row is the end's only
// record until the log takes it. The answer is to record it
// ([Coordinator.publishEnd]), clear it ([PendingStore.ClearEnd]) and ask again.
var ErrEndOwed = errors.New("sandbox: the run still owes the executor log its turn's end")

// ErrAnswerTaken is a store's refusal to let go of a recorded answer a resumed
// turn already took ([PendingStore.OweHandBack]): the reply has been used, and
// a copy handed back now would answer the person twice.
var ErrAnswerTaken = errors.New("sandbox: a resumed turn already took the reply")

// ErrSeatNotHeld is a write on behalf of a seat this node does not hold: the
// lease the coordinator asks ([CoordinatorOptions.Lease]) says the seat is not
// held here, or the store finds a newer lease than the writer's own on the run
// ([PendingStore.RecordAnswer]). Either way the write is the seat's holder's to
// make, and the delivery that asked for it goes back to reach that holder.
//
// NOT THE ZERO FENCE: zero means UNFENCED (a node with no seat host), which
// constrains nothing and would let a node that lost the seat record, claim and
// take an answer past its successor's fence.
var ErrSeatNotHeld = errors.New("sandbox: this node does not hold the seat")

// ErrRunEnding is a store's refusal of a write to a run whose ending is decided
// ([PendingRun.Ending]): from then on the row takes only the ending's own steps
// (no claim, take, release, relaunch, new answer or park), so the ending can be
// announced before its record is deleted ([RecordedEnding]). It keeps the take
// and the let-go exclusive: a take that lands first is seen by the decision, a
// later one is refused.
var ErrRunEnding = errors.New("sandbox: the run's ending is decided, so it takes no other write")
