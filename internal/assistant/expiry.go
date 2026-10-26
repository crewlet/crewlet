package assistant

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/redact"
)

// A PARKED TURN ENDS WITH ITS PROPOSALS. Once nothing it waits on can move it
// ([proposal.Lapsed]) it is closed expired, so its conversation never stays
// locked on a card nobody can decide. Decided wherever the turn is next met
// (a Start in its conversation, a reload, the decision that found its
// proposal expired) and, as the backstop for a turn nobody meets again, by
// the reaper every [ExpiryReapInterval].

// ExpiryReapInterval is how often a node looks for parked turns whose
// proposals have all expired: a minute, a small fraction of the shortest
// proposal lifetime a company may set (sixty seconds), so a lapsed turn is
// closed within a minute of its last proposal expiring.
const ExpiryReapInterval = time.Minute

// expiryReapBatch is how many lapsed turns one pass nominates; the next pass
// takes the rest.
const expiryReapBatch = 100

// engineActor is who records an end nobody asked for: an interrupted turn,
// an expired one.
var engineActor = iam.Actor{Name: "engine", Kind: iam.ActorSystem}

// derivedWrite is the caller of an end nobody asked for, under the operation
// every node derives alike from the turn and name ([Derivation]).
func derivedWrite(owner uuid.UUID, row assistantdomain.Turn, name string) Write {
	return Write{Owner: owner, By: engineActor, Derived: &Derivation{At: row.StartedAt,
		Name: name, Identity: []string{row.ID.String()}}}
}

// expiryWrite is the caller of a parked turn's expiry and what it settles.
func expiryWrite(owner uuid.UUID, row assistantdomain.Turn) Write {
	return derivedWrite(owner, row, "assistant_expire")
}

// closeDerived closes turn in end under w (a [derivedWrite]), and answers the
// state the turn is in now: end where this close or an earlier one of the
// same operation landed, or the state another end left it in
// ([assistantdomain.TurnMoved]). EXACTLY ONCE across the fleet: the state log
// arbitrates the turn's end, so of two nodes noticing at once one records it.
func (s *Service) closeDerived(ctx context.Context, w Write, turn uuid.UUID,
	end assistantdomain.TurnEnd) (wire.TurnState, error) {

	err := s.records.CloseTurn(ctx, w, turn, end)
	var moved *assistantdomain.TurnMoved
	switch {
	case errors.As(err, &moved):
		return moved.State, nil
	case err != nil:
		return "", s.writeFailure(err)
	}
	return end.State, nil
}

// lapse closes row expired when it is parked and none of the proposals it
// waits on can still move it (t is its conversation's transcript). It
// answers whether the turn is over now, by this close or any earlier end.
func (s *Service) lapse(ctx context.Context, owner uuid.UUID, row assistantdomain.Turn,
	t assistantdomain.Transcript) (bool, error) {

	if row.State != wire.TurnWaitingForApproval {
		return row.State.Final(), nil
	}
	var own []assistantdomain.Proposal
	for _, p := range t.Proposals {
		if p.Turn == row.ID {
			own = append(own, p)
		}
	}
	if !proposal.Lapsed(own, s.now()) {
		return false, nil
	}
	w := expiryWrite(owner, row)
	// READ BEFORE THE CLOSE, which drops the parked record: a deep-work run
	// the turn parked on is never continued once it expires.
	keep, keepErr := s.parkedLoop(ctx, owner, row.ID)
	state, err := s.closeDerived(ctx, w, row.ID, assistantdomain.TurnEnd{
		State: wire.TurnExpired, Steps: row.Steps})
	if err != nil {
		return false, err
	}
	if state == wire.TurnExpired {
		if keepErr == nil {
			s.dropParkedDeep(ctx, keep)
		}
		s.settleWaiting(ctx, w, waitingSteps(t, row.ID), wire.ProposalExpired, nil, nil)
		// The frame log this node may hold ends waiting for approval; a
		// stream of the turn now is answered from the transcript, which
		// says it expired.
		s.hub.Forget(row.ID)
		s.revoker.RevokeTurn(row.ID)
	}
	return state.Final(), nil
}

// lapseOpen ends a conversation's open turn as expired where it has lapsed,
// answering whether t still holds the transcript as it stands: false when a
// close landed and t is to be read again.
func (s *Service) lapseOpen(ctx context.Context, owner uuid.UUID,
	t assistantdomain.Transcript) (bool, error) {

	open := t.Conversation.OpenTurn
	if open == uuid.Nil {
		return true, nil
	}
	for _, row := range t.Turns {
		if row.ID == open && row.State == wire.TurnWaitingForApproval {
			closed, err := s.lapse(ctx, owner, row, t)
			return !closed, err
		}
	}
	return true, nil
}

// Lapse ends owner's parked turn as expired where none of its proposals can
// still move it (approval.Turns): the hook the decision that found one of
// them expired calls.
func (s *Service) Lapse(ctx context.Context, owner, conversation, turn uuid.UUID) error {
	t, err := s.transcripts.Transcript(ctx, owner, conversation)
	if err != nil {
		return s.readFailure(err)
	}
	for _, row := range t.Turns {
		if row.ID == turn {
			_, err := s.lapse(ctx, owner, row, t)
			return err
		}
	}
	return nil
}

// ReapLapsed is one pass of the expiry backstop: every parked turn whose
// proposals have all expired, as nominated by the store, is decided again and
// closed expired where it has lapsed. It answers how many it closed. A turn
// that cannot be decided now is logged and left to the next pass.
func (s *Service) ReapLapsed(ctx context.Context) (int, error) {
	due, err := s.transcripts.ParkedPast(ctx, s.now(), expiryReapBatch)
	if err != nil {
		return 0, s.readFailure(err)
	}
	closed := 0
	for _, ref := range due {
		t, err := s.transcripts.Transcript(ctx, ref.Owner, ref.Conversation)
		if err == nil {
			for _, row := range t.Turns {
				if row.ID != ref.Turn || row.State != wire.TurnWaitingForApproval {
					continue
				}
				var over bool
				if over, err = s.lapse(ctx, ref.Owner, row, t); over {
					closed++
				}
			}
		}
		if err != nil {
			s.log.WarnContext(ctx, "assistant_expiry_unreaped", "turn", ref.Turn,
				"error", redact.Secrets(err.Error()), "detail", "the next pass decides it again")
		}
	}
	return closed, nil
}

// reapLoop runs the expiry backstop until the node drains.
//
// ITS LIFETIME IS THE SERVICE'S (s.base, ended by Close), never a caller's:
// the backstop outlives the request that built the service, so no caller's
// context reaches it.
//
//nolint:contextcheck // the service's own lifetime (s.base), never a caller's; see above
func (s *Service) reapLoop() {
	defer s.reaperWG.Done()
	tick := time.NewTicker(ExpiryReapInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.base.Done():
			return
		case <-tick.C:
		}
		ctx, cancel := context.WithTimeout(s.base, ExpiryReapInterval)
		if _, err := s.ReapLapsed(ctx); err != nil {
			s.log.WarnContext(ctx, "assistant_expiry_pass_failed",
				"error", redact.Secrets(err.Error()))
		}
		cancel()
	}
}
