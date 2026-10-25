package assistant

import (
	"context"
	"strconv"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/redact"
)

// THE STEP A PARKED TURN WAITS ON SETTLES WITH ITS PROPOSAL. A write or a page
// change ends its step "waiting" when the loop suspends on it; once the
// proposal is decided (approved and applied, declined, found stale or
// failed) or the turn ends without it (stopped, expired), the step is moved to
// what became of it, on the stream and in the transcript alike, so neither a
// live view nor a reload shows a finished turn still waiting. A turn waits on
// one call at a time (the loop suspends on the first write it proposes), so
// "the waiting step" is every step of the turn in that state.

// waitingSteps are the turn's steps still waiting on a proposal, as the
// transcript holds them.
func waitingSteps(t assistantdomain.Transcript, turn uuid.UUID) []wire.StepEvent {
	var out []wire.StepEvent
	for _, st := range t.Steps {
		if st.State == wire.StepWaiting && assistantdomain.StepID(turn, st.Index) == st.StepID {
			st.TurnID = turn
			out = append(out, st)
		}
	}
	return out
}

// stepOutcome is the state, summary and refusal a waiting step settles to for
// a proposal now in state, and false where the proposal still waits (proposed,
// approved, applying, or a write in doubt the person may Retry).
func stepOutcome(state wire.ProposalState, failure *wire.Failure) (wire.StepState, string,
	*wire.Refusal, bool) {

	switch state {
	case wire.ProposalApplied:
		return wire.StepDone, "Applied", nil, true
	case wire.ProposalDeclined:
		return wire.StepStopped, "Declined", nil, true
	case wire.ProposalExpired:
		return wire.StepStopped, "Expired", nil, true
	case wire.ProposalStale:
		return wire.StepFailed, "Not sent: the record changed", nil, true
	case wire.ProposalFailed:
		if failure != nil {
			refusal := failure.Refusal
			return wire.StepRefused, "Not applied", &refusal, true
		}
		return wire.StepFailed, "Not applied", nil, true
	}
	return "", "", nil, false
}

// settleWaiting moves steps to what became of their proposal (state), records
// each under w and hands each to emit, where a stream tells it (nil where no
// stream is open). A record that does not land is logged: the step was shown,
// and a reload reads its last recorded state.
func (s *Service) settleWaiting(ctx context.Context, w Write, steps []wire.StepEvent,
	state wire.ProposalState, failure *wire.Failure, emit func(wire.Frame)) {

	to, summary, refusal, ok := stepOutcome(state, failure)
	if !ok {
		return
	}
	for _, st := range steps {
		st.State, st.Summary, st.Refusal = to, summary, refusal
		if emit != nil {
			ev := st
			emit(wire.Frame{Kind: wire.EventStep, Step: &ev})
		}
		if err := s.records.RecordStep(ctx, w.Step("step", strconv.Itoa(st.Index)), st.TurnID,
			st); err != nil {
			s.log.WarnContext(ctx, "assistant_record_failed", "turn", st.TurnID,
				"record", "step", "error", redact.Secrets(err.Error()),
				"detail", "the step's settled state is missing from the transcript")
		}
	}
}
