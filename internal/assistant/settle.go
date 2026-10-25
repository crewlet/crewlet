package assistant

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/live"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/redact"
)

// A TURN'S END IS SETTLED, never only logged. A turn whose close record did
// not land reads as running to every node: its conversation takes no other
// question (one turn at a time) and every stream of it waits on a runner that
// is gone. So the runner retries its close under the SAME operation (the
// ledger collapses a retry of one that did land) until it lands, the turn is
// found already ended (another node settled it through the arbitrated end),
// or its conversation is gone; and past [settleWindow], or once the node is
// draining, it leaves the turn to the interrupt path, which every stream of it
// takes ([Service.closeInterrupted]).

// settleWindow is how long a runner keeps retrying its close: the frame log's
// retention, for which the turn's end is still answered from its frames, so a
// person sees nothing amiss while the close is retried.
const settleWindow = live.EndedRetention

// settleBase and settleCeiling pace the retries: a quarter second doubling to
// fifteen, a dozen attempts in the window, enough for a state log that lost
// its leader to elect another.
const (
	settleBase    = 250 * time.Millisecond
	settleCeiling = 15 * time.Second
)

// closeOnce makes one attempt at the turn's close record, reporting whether
// the turn is settled: closed now, or found already ended or gone.
func (t *turn) closeOnce(ctx context.Context, end assistantdomain.TurnEnd) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	err := t.svc.records.CloseTurn(ctx, t.write().Step("close"), t.id, end)
	var moved *assistantdomain.TurnMoved
	switch {
	case err == nil:
		return true
	case errors.As(err, &moved):
		t.svc.log.InfoContext(ctx, "assistant_turn_already_settled", "turn", t.id,
			"state", string(moved.State))
		return true
	case errors.Is(err, assistantdomain.ErrNotFound), errors.Is(err, assistantdomain.ErrPurged),
		errors.Is(err, assistantdomain.ErrErased):
		return true
	}
	t.svc.log.WarnContext(ctx, "assistant_close_unrecorded", "turn", t.id,
		"error", redact.Secrets(err.Error()), "detail", "the close is retried")
	return false
}

// settle retries the turn's close until it is settled, the window passes or
// the node drains.
func (t *turn) settle(ctx context.Context, end assistantdomain.TurnEnd) {
	deadline := time.Now().Add(settleWindow)
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		wait := backoff.Jitter(backoff.Doubling(attempt, settleBase, settleCeiling), 0.2)
		select {
		case <-t.svc.base.Done():
			t.svc.log.ErrorContext(ctx, "assistant_turn_unsettled", "turn", t.id,
				"detail", "the node is draining; the turn is closed interrupted by the "+
					"next stream of it on any node")
			return
		case <-time.After(wait):
		}
		if t.closeOnce(ctx, end) {
			return
		}
	}
	t.svc.log.ErrorContext(ctx, "assistant_turn_unsettled", "turn", t.id,
		"detail", "the close did not land in its window; the turn is closed interrupted "+
			"by the next stream of it on any node")
}
