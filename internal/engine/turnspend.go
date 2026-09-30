package engine

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A TURN'S SPEND, ON THE TASK IT WAS SPENT ON.
//
// The tracker keeps, per task, what the work has cost — turns, rounds, tokens
// and wall-clock — as eight counters its applier adds each turn's record to
// ([tracker.Writer.RecordTurn]). Nothing called it: every task in every
// company reported that it had cost nothing, and every total, sort and
// filter over the `spend_*` columns answered over zeros.
//
// # Which task a turn is spent on
//
// The one its TRIGGER names, and only that one: a turn the tracker woke about
// a task — assigned, mentioned, asked, unblocked, commented on — is a turn
// spent on that task. That is derived from the wake the change feed built,
// never from what the turn's tools touched afterwards: a turn that files three
// follow-ups while answering a chat message was not spent on any of them, and
// splitting its cost by the calls a model chose to make would make the counter
// a record of the model's choices rather than of the work. A turn no task woke
// is spent on none, and records nothing here — its cost is still the seat's,
// on every other spend surface.
//
// # Once per turn, whichever half it is
//
// A turn that detached a coding run is two executions: the half that parked
// and the half that resumed, possibly on another node. Each records its own
// spend — its phases' tokens and its own wall-clock — and only the first
// counts as a TURN, while the second's rounds are the ones it ran past the
// round it re-entered. The two sum to one turn. The resumed half learns the
// task from the run's row, having no trigger of its own.
//
// # And a coding run's tokens with it
//
// What a detached coding agent spent inside its box is no phase's spend — the
// turn was suspended while it ran — so neither half carries it. It is recorded
// when the run is COLLECTED, as a record of its own on the same task: a run
// that parks on a question is resumed by a person's answer, which collects
// nothing, so a run's tokens reach the task there or never ([runSpender]).
// It counts no turn and no round, and no wall-clock: a task's wall-clock is
// the time the engine's turns on it ran, which is what every turn's duration
// means on every other surface.
//
// # And never twice for one execution
//
// The operation id is DERIVED from the run and the round it began at, so a
// retry of the write — the outcome unknown, the data node that took it gone —
// reproduces it, and the applier adds a turn's spend only for an operation it
// has not applied. A collected run's is derived from the launch it collected,
// so every retry of the completion that collects the same job reproduces it.

// turnSpendBudget bounds the write of one turn's spend.
//
// TWICE [statelog.DefaultResolveBudget]: one append resolves within the
// resolve budget, and the second is the room a node without `data` needs to
// ask the next data node when the first did not answer. It is spent AFTER the
// turn, on a context the turn's cancellation does not reach — a drain that
// lets a turn finish must not then drop what it cost — so it is bounded here
// rather than by the caller.
const turnSpendBudget = 2 * statelog.DefaultResolveBudget

// workItemOf is the tracker task a partition's wakes are about: the one task
// every tracker wake in it names, or empty when none does or they disagree.
//
// A TASK WAKE ONLY. A wake about a person's own priority list names the task
// that reached its top, but the turn it starts is about the list — and a turn
// woken by anything else names no task at all.
func workItemOf(evs []*events.Event) string {
	item := ""
	for _, n := range notificationsIn(evs) {
		if n.NotificationSource != tracker.Source ||
			n.Metadata[tracker.MetaObject] != string(tracker.KindTask) {
			continue
		}
		id := n.Metadata[tracker.MetaTaskID]
		switch {
		case id == "":
			continue
		case item != "" && item != id:
			// ONE PARTITION IS ONE TASK, because the tracker's wakes
			// partition on the task's key; two ids here is a partition
			// this derivation does not understand, and a guess would
			// charge one task for another's work.
			return ""
		}
		item = id
	}
	return item
}

// recordTaskSpend adds what this execution of a turn cost to the task it was
// spent on — see the file's doc. Nothing when no task woke it, or when the
// company does not run the native tracker.
func (e *Engine) recordTaskSpend(ctx context.Context, t turnTelemetry,
	spend runner.Spend, res turn.Result) {

	if t.workItem == "" {
		return
	}
	halves, ok := e.trackerHalves()
	if !ok {
		return
	}
	record := turnRecordOf(t, spend, res, time.Now().UTC())
	opID := statelog.DeriveOpID(t.startedAt, "turn_spend", t.runID,
		strconv.Itoa(t.resumedRound))
	// TELEMETRY NEVER FAILS THE WORK, and a turn's own spend has no retry:
	// the turn is over when it is written. So a spend that could not be
	// recorded is logged and whatever spent it stands.
	if err := writeSpend(ctx, halves, opID, record); err != nil {
		log.WarnContext(ctx, "turn_spend_unrecorded", "handle", record.Seat,
			"turn_id", record.TurnID, "task", record.Task, "operation", opID,
			"error", err.Error(),
			"detail", "the work stands; what it cost may be missing from the "+
				"task's spend, and every other spend surface still counts it")
	}
}

// errSpendUnknown is a spend write whose outcome never came back: it may
// have landed. The operation id makes a repeat of it count it at most once.
var errSpendUnknown = errors.New("engine: whether the task's spend counts this " +
	"is unknown")

// writeSpend writes one spend record as the seat it belongs to, bounded — see
// the file's doc — and answers whether its fate is SETTLED: nil when the
// task's spend counts it, or never can because the task is gone for good —
// purged, or created by no record on its log ([tracker.ErrNoTask], which no
// retry changes) — or when no repeat here can
// learn whether it does (an unknown the answering node's ledger cannot vouch
// for); and an error when whether it counts is not known YET — the write
// refused for now, unanswered, or answered with an outcome a repeat resolves —
// which a repeat under the same operation id settles without counting it
// twice.
func writeSpend(ctx context.Context, halves trackerSeams, opID string,
	record tracker.TurnRecord) error {

	writer := halves.as(builtin.Actor{
		Handle: record.Seat, Kind: tracker.AuthorAgent, TurnID: record.TurnID,
	})
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), turnSpendBudget)
	defer cancel()
	result, err := writer.RecordTurn(bounded, opID, record)
	switch {
	case err == nil && result.Outcome == statelog.OutcomeUnknown && result.Unvouched:
		// UNKNOWN FOR GOOD HERE, unlike a lost acknowledgement: the ledger
		// of the node that answered cannot vouch for an operation minted
		// before it may have lost rows, and answers every repeat of it the
		// same way until the record reaches it — which, if the first
		// attempt never landed, it never does. Repeating it is a caller
		// held for ever on telemetry, so the fate is settled as unknown
		// and said by name.
		minted, _ := statelog.OpMintedAt(opID)
		log.WarnContext(ctx, "turn_spend_unvouched", "handle", record.Seat,
			"turn_id", record.TurnID, "task", record.Task, "operation", opID,
			"minted_at", minted,
			"detail", "the data node that answered cannot say whether the task's "+
				"spend counts this: its operation ledger may have lost rows from "+
				"before the operation was minted, and holds none for it; every "+
				"other spend surface still counts it")
		return nil
	case errors.Is(err, tracker.ErrNoTask):
		log.InfoContext(ctx, "turn_spend_task_gone", "handle", record.Seat,
			"turn_id", record.TurnID, "task", record.Task, "operation", opID,
			"error", err.Error(),
			"detail", "the task is gone for good — purged, or created by no record "+
				"on its log, as the error says — so nothing it cost can be recorded "+
				"against it; every other spend surface still counts it")
		return nil
	case err != nil:
		return err
	case result.Outcome == statelog.OutcomeUnknown:
		return errSpendUnknown
	}
	return nil
}

// runSpender records a collected coding run's tokens on the task its turn is
// spent on — [sandbox.Spender], and see the file's doc.
type runSpender struct{ engine *Engine }

// RunSpent implements [sandbox.Spender]: nil when the run's spend is on its
// task or never can be, and an error — which holds the collect for its retry —
// when whether it is on it is not known yet ([writeSpend]).
func (s runSpender) RunSpent(ctx context.Context, run sandbox.PendingRun,
	result sandbox.Result) error {

	if run.WorkItem == "" || result.InputTokens+result.OutputTokens == 0 {
		return nil
	}
	halves, ok := s.engine.trackerHalves()
	if !ok {
		return nil
	}
	// THE LAUNCH, at the instant its row was written: every retry of the
	// completion collects the same finished job from the same row, and a
	// second job in the same turn is a new launch with spend of its own.
	opID := statelog.DeriveOpID(run.CreatedAt, "run_spend", run.TurnID, run.LaunchID)
	return writeSpend(ctx, halves, opID, runRecordOf(run, result))
}

// runRecordOf is one collected run's spend as the tracker's turn record
// carries it: tokens, and no turn, round or wall-clock of its own.
func runRecordOf(run sandbox.PendingRun, result sandbox.Result) tracker.TurnRecord {
	outcome := "failed"
	switch {
	case result.NeedsInput:
		outcome = "needs_input"
	case result.Success:
		outcome = "succeeded"
	}
	return tracker.TurnRecord{
		Task: run.WorkItem, Seat: run.AgentHandle, TurnID: run.TurnID,
		Trigger: types.SandboxRunCompleted{}.EventType(), Outcome: outcome,
		Spend: tracker.TurnSpend{
			Input: result.InputTokens, Output: result.OutputTokens,
		},
	}
}

// turnRecordOf is one execution's spend as the tracker's turn record carries
// it.
func turnRecordOf(t turnTelemetry, spend runner.Spend, res turn.Result,
	ended time.Time) tracker.TurnRecord {

	turns := 1
	if t.resumedRound > 0 {
		// THE SAME TURN, resumed: the half that parked counted it.
		turns = 0
	}
	outcome := spend.Outcome
	if outcome == "" {
		outcome = string(res.Decision)
	}
	var phases []string
	if spend.ExecuteModel != "" {
		phases = append(phases, phase.Execute.String())
	}
	if spend.ReviewModel != "" {
		phases = append(phases, phase.Review.String())
	}
	return tracker.TurnRecord{
		Task: t.workItem, Seat: t.handle, TurnID: t.runID,
		Trigger: t.trigger.Type, Outcome: outcome, Phases: phases,
		Spend: tracker.TurnSpend{
			Turns: turns,
			// THE ROUNDS THIS EXECUTION RAN: a resumed turn re-enters
			// the round it parked in and counts on from there, and the
			// rounds up to it are the parked half's.
			Rounds:     max(res.Rounds-t.resumedRound, 0),
			Input:      spend.InputTokens,
			Output:     spend.OutputTokens,
			CacheRead:  spend.CacheRead,
			CacheWrite: spend.CacheWrite,
			WallMs:     int(max(ended.Sub(t.startedAt), 0) / time.Millisecond),
		},
	}
}
