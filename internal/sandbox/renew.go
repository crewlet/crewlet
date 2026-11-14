package sandbox

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
)

// # A completion is renewed while the clock on its box's host runs
//
// A collection whose box is on another node's host asks that host to read it,
// and a host whose coordination store does not answer its read of the run's
// row reads nothing, while this node's own store answers the claim
// ([hostUnsettled]). This node then keeps a clock on that host's store
// ([Coordinator.hostClocked]): the claim is handed back uncounted and the
// completion NAK'd, and asked again until the host's store answers or the box
// timeout passes, when the job is recorded lost with that host and its round
// carried on ([Coordinator.loseWithHost]).
//
// WHILE THAT CLOCK RUNS, THE BROKER'S REDELIVERY IS ALL THAT ASKS AGAIN. Every
// other hand-back of a running, unheld job has the waiter that drives its box
// behind it: its next announcement is a fresh message with a fresh budget
// (reannounce.go). But a box on a node's host is driven by that node's own
// waiter alone, and that waiter reads the run's row through the very store
// that is silent, so it announces nothing for as long as the silence lasts.
// The broker carries a failing message for about ten minutes before its dead
// letter, never under eight (internal/queue/jetstream's LeastTimeToDeadLetter),
// and the box timeout is fifteen by default: NAK'd and nothing more, the
// completion was dead-lettered before the bound was reached, the job was never
// recorded lost, and the seat was held behind a run nothing drove again until
// the host's store answered.
//
// SO EVERY HAND-BACK OF THE COMPLETION IS RENEWED WHILE THE CLOCK RUNS,
// WHATEVER ITS KIND ([Coordinator.renewing]): the host's own answer that its
// store did not answer; a claim this node's own store does not answer, or
// refuses while a release this node owes is out ([errReleaseOwed]); a host that
// does not answer the read at all ([ErrBoxHostSilent]), which is acknowledged
// only where no clock runs; a record of the job's loss this node's store did
// not take ([Coordinator.loseWithHost]); and any other step that fails. KEYED
// ON THE CLOCK, NEVER ON THE HAND-BACK'S KIND: renewed for the host's answer
// alone, a claim or a release the store did not answer meanwhile was NAK'd
// until its budget was spent, and a host that did not answer for a minute had
// its completion acknowledged, and each left the job with nothing to drive it.
// Before the delivery in hand is the last its budget allows ([renewReserve],
// [queue.DeliveriesLeftFor]), the completion is announced afresh: a new event
// of the same completion ([events.New]: a fresh id, which the stream's
// duplicate window would otherwise drop, and with it a fresh budget), to the
// seat's control topic, PUBLISHED BEFORE THE DELIVERY IN HAND IS ACKNOWLEDGED,
// which it then is, so the fresh one carries the completion on and each renewal
// leaves one announcement in flight, never a growing number. A renewal the
// broker does not take leaves the delivery in hand NAK'd, with deliveries still
// left to make it again on. A duplicate is harmless either way: the claim is
// made once.
//
// A RE-FIRE HAS NO DELIVERY IN HAND. A completion this node kept for a
// condition and re-fires once it clears ([Coordinator.refire]) was
// acknowledged when it was kept, so a re-fire that hands its completion back
// while the clock runs is renewed at once: nothing else would carry it. Nor
// does anything bring back a re-fire whose renewal the broker does not take,
// so its own line says the completion waits for that host's store, and never
// that a delivery was handed back to make the renewal again: WHAT A REFUSED
// RENEWAL LEAVES IS ITS CALLER'S TO SAY ([Coordinator.renewing],
// [Coordinator.refire]).
//
// NOTHING IS RENEWED WHILE NO CLOCK RUNS: a step this node's own store did not
// answer is NAK'd and nothing more, since the waiter that drives the box, whose
// store does answer or answers once the silence is over, announces the job
// again on its schedule ([Coordinator.collectFailedAt]); and a delivery whose
// count the transport did not state is NAK'd as before this existed, since a
// missing number is no evidence of a spent budget.

// renewReserve is how many further deliveries a completion's delivery has when
// a hand-back while the clock on its box's host runs is announced afresh
// ([Coordinator.renewing]): the renewal is made on the delivery that has this
// many left, or on any after it that comes back.
//
// FIVE, AS [AnswerDeliveryReserve] IS, and for its reasons: what can spend a
// delivery between the renewal and the dead letter is the renewal's own
// publish failing (one each try) and a seat handoff while the delivery is in
// flight (a deferral, one or two). Five span about two and a half minutes at
// the broker's 30 s ceiling, which outlasts a stream's leader election many
// times over and a seat lease's turnover. Renewed sooner, every delivery would
// be a fresh message the broker hands over at once, a box read made again at
// the pace of the host's answer rather than the backoff's. Not derived from
// the broker's budget, for the reason [AnswerDeliveryReserve] gives.
const renewReserve = 5

// renewing is what a delivery of ev's completion leaves with the broker, err
// being what the coordinator made of it ([Coordinator.OnCompleted]): err as it
// is, unless this node keeps a clock on the store of the host of the job's box
// ([Coordinator.hostClocked]) and trigger, the delivery in hand, has at most
// [renewReserve] deliveries left, when the completion is announced afresh
// ([Coordinator.renew]) and nil is answered, so the delivery is acknowledged
// and the fresh one carries the completion on. A renewal the broker does not
// take answers err beside why, so the delivery is NAK'd and comes back to make
// it again, which its line says (sandbox_completion_renew_failed).
func (c *Coordinator) renewing(ctx context.Context, ev types.ExecutorFinished, trigger *events.Event,
	err error,
) error {
	if err == nil || trigger == nil || !c.hostClocked(ev.TurnID, ev.LaunchID) {
		return err
	}
	left, stated := queue.DeliveriesLeftFor(ctx, trigger.ID)
	if !stated || left > renewReserve {
		return err
	}
	refused := c.renew(ctx, ev, trigger)
	if refused == nil {
		return nil
	}
	log.WarnContext(ctx, "sandbox_completion_renew_failed", "turn_id", ev.TurnID,
		"launch_id", ev.LaunchID, "agent", ev.AgentHandle, "deliveries_left", left,
		"error", refused.Error(),
		"detail", "the completion of a job whose box's host cannot read the coordination store "+
			"could not be announced afresh; the delivery in hand is handed back, and the renewal "+
			"is made again when it comes back")
	return fmt.Errorf("%w; %w", err, refused)
}

// renew announces ev's completion afresh on its seat's control topic, for a
// hand-back while the clock on its box's host runs, and answers why it could
// not where the broker did not take it. What that leaves is the caller's to
// say: a delivery in hand comes back to make it again ([Coordinator.renewing]),
// and a re-fire, which has none, waits for the host's store
// ([Coordinator.refire]).
//
// TRIGGER'S TRACE AND SOURCE, so the resume it leads to nests under the turn
// that launched the job, as the waiter's own announcement does; trigger may be
// nil, which carries neither.
func (c *Coordinator) renew(ctx context.Context, ev types.ExecutorFinished, trigger *events.Event) error {
	control := controlSubject(ev.Agent)
	if control == "" {
		return fmt.Errorf("sandbox: the completion of %s names no seat whose control topic could carry it "+
			"afresh", ev.TurnID)
	}
	var trace events.TraceContext
	if trigger != nil {
		trace = events.TraceContext{TraceID: trigger.TraceID, ParentSpanID: trigger.ParentSpanID}
	}
	renewed := events.New(ev, trace)
	if trigger != nil {
		renewed.Source = trigger.Source
	}
	if err := c.queue.Publish(ctx, control, renewed); err != nil {
		return fmt.Errorf("sandbox: announcing the completion of %s afresh: %w", ev.TurnID, err)
	}
	log.InfoContext(ctx, "sandbox_completion_renewed", "turn_id", ev.TurnID,
		"launch_id", ev.LaunchID, "agent", ev.AgentHandle, "announcement", renewed.ID.String(),
		"detail", "the job's box is on a node whose coordination store does not answer it, so that "+
			"node's waiter cannot announce the job again; this node announced it afresh, in place of a "+
			"delivery about to run out or of a re-fire that had none, and the collection is asked again "+
			"until the store answers or the box timeout records the job lost with that node")
	return nil
}
