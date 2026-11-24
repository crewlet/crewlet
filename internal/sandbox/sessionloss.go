package sandbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/redact"
)

// A SESSION NOT KEPT, OR NOT CONTINUED, IS SAID ONCE, IN ONE VOCABULARY: every
// refusal of a capture names its [executorkeep.Loss], stamped on the run's row
// beside its reason and carried by the turn's end ([unkeptOf]), and every
// launch that does not continue the newest session its turn or conversation
// left says so in an [executorkeep.NotContinued]. The two lines are said here
// alone ([sayNotKept], [sayNotContinued]), which the package's source gate
// holds, so no refusal goes unstamped.

// sayNotKept is the one `executor_session_not_kept` line: a capture, or a
// save, not kept, and its loss.
func sayNotKept(ctx context.Context, level slog.Level, loss executorkeep.Loss, turnID string, attrs ...any) {
	log.Log(ctx, level, "executor_session_not_kept", append([]any{"turn_id", turnID, "loss", string(loss)},
		attrs...)...)
}

// sayNotContinued is the one `executor_session_not_continued` line: a launch's
// verdict, in its sentence, with the engine's detail.
func sayNotContinued(ctx context.Context, turnID, agent string, n executorkeep.NotContinued, attrs ...any) {
	log.InfoContext(ctx, "executor_session_not_continued", append([]any{"turn_id", turnID, "agent", agent,
		"loss", string(n.Loss), "instead", string(n.Instead), "said", n.Sentence(),
		"detail", redact.Secrets(n.Detail)}, attrs...)...)
}

// refusedFor is a continuity that refuses for loss, and why.
func refusedFor(loss executorkeep.Loss, why string) SessionContinuity {
	return SessionContinuity{Loss: loss, Why: why}
}

// unkeptLoss is the loss a capture that is not the kept one stands for, as
// its stamp says: its own where it was refused, the log's where its record
// went unanswered, and a capture still begun is one the launch did not wait
// out.
func unkeptLoss(c SessionCapture) executorkeep.Loss {
	switch c.State {
	case CaptureUnresolved:
		return executorkeep.LossLogRefused
	case CaptureBegun:
		return executorkeep.LossCaptureLate
	}
	if c.Loss == "" {
		return executorkeep.LossKeepFailed
	}
	return c.Loss
}

// placingRow is the run's row as a take places its capture by, read again while
// the coordination store does not answer, as a stamp is asked
// ([Coordinator.askStamp]), BEFORE THE TAKE'S BUDGET BEGINS
// ([Coordinator.takeContext]), so a read at its bound costs the pack and the
// scrub nothing, and never past the take's end ([Coordinator.takeBy]). A READ'S
// ROOM, NEVER A CHANGE'S: a record's reason and the reply's crossing alone
// ([RowReadBudget]), what the store gives one read of a run's key, since a read
// sends no write ([placingReadBudget]). Under the read's own context, since
// nothing it reads outlives the read. A store that never answers it takes no
// refusal either, so the take's answer alone carries its loss.
func (c *Coordinator) placingRow(ctx context.Context, by time.Time, turnID string) (PendingRun, bool, error) {
	read, cancel := c.rowReadContext(ctx, c.recordBudget)
	read, cancel = before(read, cancel, c.takeBy(by))
	defer cancel()
	for attempt := 1; ; attempt++ {
		run, found, err := c.pending.Get(read, turnID)
		if err == nil {
			return run, found, nil
		}
		wait := time.NewTimer(backoff.Doubling(attempt, c.sessionRetry, sessionRetryCeiling))
		select {
		case <-read.Done():
			wait.Stop()
			return PendingRun{}, false, err
		case <-wait.C:
		}
	}
}

// SessionMeter is told, once per collected launch that had a session to
// continue, its final verdict ([LaunchRecord.NotContinued], nil where it
// continued it): the engine counts each loss
// (`crewlet.executor.session.not_continued`) and records the session_carry
// objective (internal/objectives); this package links no state log.
type SessionMeter interface {
	SessionCarried(verdict *executorkeep.NotContinued)
}

// ConversationTeller says in a turn's conversation the verdict its runs' sessions
// leave it ([PendingRun.ConversationNotice]). ASKED AGAIN wherever the end's
// publish is ([Coordinator.publishEnd]), so it says it once however often asked;
// best effort, as nothing of the turn waits on it.
type ConversationTeller interface {
	NotContinued(ctx context.Context, run PendingRun, verdict executorkeep.NotContinued)
}

// ConversationNotice is the one verdict a turn's conversation is told of its
// runs' sessions, nil where they lost none: the turn's first
// ([PendingRun.turnVerdicts]), what the conversation's session lost going into
// the turn, or what its first launch to lose one lost. SAID ONCE PER TURN, never
// per launch, in its sentence alone ([executorkeep.NotContinued.Sentence]),
// never its detail: beside the engine's answer to a colleague who asked
// (internal/engine's a2a.go), and in the conversation where the engine narrates
// it ([CoordinatorOptions.Conversations]).
func (r PendingRun) ConversationNotice() *executorkeep.NotContinued {
	said := r.turnVerdicts()
	if len(said) == 0 {
		return nil
	}
	first := said[0]
	return &first
}

// meterCarry tells the meter a collected launch's verdict, where it had a
// session to continue: its brief continued one, or it says what it lost. A
// launch with neither (a conversation's first turn, a company or harness that
// keeps none) is no event. Called under the record's once-per-launch guard
// ([LaunchRecord.Published]).
func (c *Coordinator) meterCarry(facts LaunchRecord, verdict *executorkeep.NotContinued) {
	if c.sessionMeter == nil || (verdict == nil && !facts.Brief.Continues()) {
		return
	}
	c.sessionMeter.SessionCarried(verdict)
}

// finalNotContinued is a collected launch's final verdict on its session: the
// launch's own ([LaunchRecord.NotContinued]), or, where its box's restore
// failed and its script started a session of its own ([Result.FellBack]),
// [executorkeep.LossRestoreFailed] with the script's reason, of the base's turn
// where the launch continued its conversation's.
func finalNotContinued(run PendingRun, facts LaunchRecord, result Result) *executorkeep.NotContinued {
	if result.FellBack == "" {
		return facts.NotContinued
	}
	verdict := executorkeep.Lost(executorkeep.LossRestoreFailed, executorkeep.InsteadFresh,
		BoundFellBack(result.FellBack))
	if facts.Brief == BriefContinuedConversation && run.Base != nil && run.Base.Session != nil {
		verdict.LostTurn = run.Base.Session.Turn
	}
	fitted := verdict.Fit()
	return &fitted
}

// launchVerdict is the current job's final verdict as its run's record says it:
// [executorkeep.LossRestoreFailed] where its box started a session of its own
// in place of the one its brief continued ([LaunchRecord.SessionStart]), else
// the launch's own ([LaunchRecord.NotContinued]).
func (r PendingRun) launchVerdict() *executorkeep.NotContinued {
	if !r.Launch.Brief.Continues() || r.Launch.SessionStart != SessionStartFresh {
		return r.Launch.NotContinued
	}
	verdict := executorkeep.Lost(executorkeep.LossRestoreFailed, executorkeep.InsteadFresh,
		"the box's restore of the session it was handed failed, and its script started a session of its own")
	if r.Launch.Brief == BriefContinuedConversation && r.Base != nil && r.Base.Session != nil {
		verdict.LostTurn = r.Base.Session.Turn
	}
	return verdict
}

// turnVerdicts is the turn's verdicts with its current job's beside them
// ([executorkeep.HoldNotContinued]): what its end carries.
func (r PendingRun) turnVerdicts() []executorkeep.NotContinued {
	if v := r.launchVerdict(); v != nil {
		return executorkeep.HoldNotContinued(r.NotContinued, *v)
	}
	return r.NotContinued
}
