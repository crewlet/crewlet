package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

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
// carried on ([Coordinator.loseWithHost]). And the claim's release writes what
// the host said onto the job's record ([LaunchRecord.HostStoreSilentSince]), the
// clock's durable twin, which the release of a collection that reads the box
// clears.
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
// SO NO DELIVERY OF THE COMPLETION IS ACKNOWLEDGED WHILE THE CLOCK RUNS, OR
// WHILE THE RUN'S ROW SAYS NO WAITER ANNOUNCES ITS JOB ([unannouncedOnRow]),
// unless the run settled, which drops the clock ([hostUnsettled]) and moves the
// row on: every other answer hands it back, marked [ErrUnannounced], and renews
// it, WHATEVER ITS KIND ([Coordinator.renewing]). The host's own answer that
// its store did not answer; a claim this node's own store does not answer, or
// one this node refuses while a release it owes is out ([errReleaseOwed]); a
// host that does not answer the read at all ([ErrBoxHostSilent]), which is
// acknowledged only where no clock runs and the run's row says nothing of one;
// a record of the job's loss this node's store did not take
// ([Coordinator.loseWithHost]), or a carry of its round that fails after it; a
// refusal of the node's gate, or a completion this node holds off itself
// ([Coordinator.keepRefused]), which is kept for its condition's signal
// besides; and an answer that settled nothing, a claim another attempt holds or
// a resume the run's row holds for the seat's budget ([HeldError]), whose
// host's waiter would announce the job once that hold lifts. This node's memory
// of the job, a completion it keeps for a condition included, outlives neither
// its restart nor its letting go of the seat, so while the clock runs the
// delivery is the one copy of the completion that does. KEYED ON THE CLOCK,
// NEVER ON THE HAND-BACK'S KIND: renewed for the host's answer alone, a claim
// or a release the store did not answer meanwhile was NAK'd until its budget
// was spent, and a host that did not answer for a minute, a refusal whose
// condition nothing signals clearing, and a resume held on its row each had
// their completion acknowledged, and each left the job with nothing to drive
// it.
//
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
// AND ON THE ROW, NEVER ON ONE NODE'S MEMORY ALONE ([Coordinator.unannouncedJob]).
// The clock goes with a restart, and the seat's next holder starts with none,
// while the host's waiter still announces nothing. So a node that keeps no
// clock reads the run's row before it lets a delivery go, and keys the same
// hand-back on what the row says durably: the job recorded lost with that host
// for its store's silence ([LossLaunchNodeStoreSilent]), until its round is
// carried on, which a carry's failure after a restart has no collection to
// start the clock again for, since such a loss's carry reads no box; or the
// job's record saying that host last found its store silent, which a refusal
// on a restarted node or the seat's next holder meets before any collection of
// its own has started a clock. Kept by memory alone, a carry that failed after
// a restart was NAK'd until its budget was spent, and such a refusal was
// acknowledged, each leaving the job with nothing to announce it until that
// host's store answered.
//
// A WAITER'S ANNOUNCEMENT FROM A READ SINCE ENDS BOTH. That waiter's
// announcement says when it asked its store for the run's row it was made from
// ([rowAskedField]): it announces the job only having read the row through the
// store the clock times, so one whose read was asked after the clock started,
// or after the instant the job's record holds, says that store answered since
// ([announcedAfter]). It ends this node's clock as it is delivered
// ([Coordinator.hearAnnouncement]) and the record at the release a claim of it
// makes ([Coordinator.hostStoreSeen]), and a refusal of it is acknowledged as
// any is where a waiter stands behind it (ADR-0063). One whose read was asked
// before ends nothing, however late it was made or delivered: the waiter
// polls the box between its read and its announcement, which can take a
// minute ([pollBound]) after a slot's wait, so a silence that began in between
// and was found meanwhile was, by the announcement's stamp, read as over while
// it lasted. Nor does a renewal, which reads no row and so says no read
// ([Coordinator.renew]). Ended by nothing, the clock and its record stood
// through every announcement that waiter made once its store answered again:
// while a refusal that settles nothing lasted, each was handed back and
// renewed beside the chain already in flight, one more for every
// announcement, about twelve an hour. The chain in flight is renewed on its own
// budget until a release clears the record, one chain, as before. A fleet
// mixed with a build from before renewals existed is unchanged by the read's
// instant, which rides the envelope's bag every backend carries verbatim and
// every republish keeps: that build's host never answers that its store did
// not answer its read ([BoxReadReply.Unsettled] is this build's), so no node
// keeps a clock or a record on a job whose box it hosts, and its waiter's
// announcements, which say no read, end nothing; and holding the seat, that
// build neither renews nor reads the instant, and acknowledges a refusal and
// NAKs a failure as it always did.
//
// AND A REFUSAL ACKNOWLEDGED BEFORE ANY NODE HEARD OF THE SILENCE IS OWED ON
// THE ROW. A refusal of a job no collection has read since its host's store
// went silent finds no clock and nothing on the row to say so, since no node
// has heard that host say it, so it is acknowledged as any is where a waiter
// stands behind it, and kept for its condition's signal, whose re-fire's
// collection starts the clock and renews the completion at once. That keep is
// this node's memory, and a silence of the store's runs alone has no bound:
// the host's lease and presence are another stream, so no watchdog ends that
// host and no peer finds it gone. So the refusal is written on the job's
// record before its delivery is acknowledged ([LaunchRecord.RefusalOwed],
// [Coordinator.oweRefused]), as every acknowledged refusal is, and the seat's
// recovery, this node's after a restart or the seat's next holder's, re-arms
// it and asks its own gate at once ([Coordinator.rearmOwed]); a record the
// store does not take is no acknowledgement, and the delivery is handed back
// as this node's failure. Kept in memory alone, a restart or a letting go of
// the seat before that signal left the job with nothing to announce it for as
// long as that host's store was silent. And WITH NO RESTART AND NO SEAT MOVE,
// the keep re-fires itself at its refusal's instant, no later than
// [answerWaitBackstop] unless this node holds it off itself until that instant
// (whose backstop before it asks only whether its turn's deadline has passed,
// and ends the run if so, [Coordinator.recheckHeld]), and at the backstop where
// it names none ([Coordinator.keepLocked]), as a
// recovery's re-arm and a re-fire that settled nothing do
// ([Coordinator.keepAgain]): a condition that lifts on the clock with no edge
// (a lease merely stale, a provider's limit at its instant) left the kept
// completion waiting on this node, for the signal that never came and the
// announcement that host's waiter never made, for as long as that store's
// silence lasted. So is a completion whose resume its run's row holds for the
// seat's budget, whose delivery the hold spends where no clock runs: kept
// unheld, re-fired at the hold's instant and never sooner, since no claim wins
// before it, and on its condition's signal once its own lift of the hold has
// landed ([Coordinator.keepHeldOnRow]), kept through a delivery of it or a
// re-fire that meets the hold ([Coordinator.forgetAdmitted],
// [Coordinator.keepHeldClaim]), and kept again by the seat's recovery from the
// row ([Coordinator.keepHeldRows]); nothing else announced the job once its
// hold had lifted while that host's store was silent. Its re-fire's
// collection starts the clock where the silence still stands.
//
// WHAT IT COSTS: a hand-back that settles nothing for as long as it lasts is
// NAK'd again at the queue's backoff, at most every thirty seconds, each
// delivery one claim attempt or one admission and, where this node keeps no
// clock and the delivery read no box, one read of the run's row; and renewed
// about every seven and a half minutes, since the delivery with
// [renewReserve] left is the twentieth of the broker's twenty-five, after
// nineteen returns spaced 1, 2, 4, 8 and 16 seconds and fourteen times thirty
// (451 s, about 361 s at the floor of the backoff's jitter;
// internal/queue/jetstream's LeastTimeToDeadLetter). The
// longest such is a carry whose resume its row holds for the seat's budget
// while the clock runs, for as long as that hold lasts. A duplicate the claim
// makes once, and nothing dead-letters.
//
// A RE-FIRE HAS NO DELIVERY IN HAND. A completion this node kept for a
// condition and re-fires once it clears ([Coordinator.refire]) was kept from a
// delivery that was acknowledged where no clock ran then, and handed back
// where one did. So a re-fire is renewed at once only where its own attempt
// started the clock, its claim having won and its read of the box found the
// host's store silent, and that clock still runs once the attempt is done:
// nothing else carries it then. One whose own attempt then settled the run,
// its hand-back refused and the run ended, announces nothing, the run's end
// having dropped the clock with it. A re-fire whose claim lost to a
// delivery's, or that found the clock that delivery's collection started,
// announces nothing: that delivery, handed back, carries it. Where the clock
// ran before it, the delivery refused while the clock ran is in flight still,
// handed back and renewed on its own budget, and a re-fire that announced it
// afresh beside it would leave two announcements in flight, and more with
// every refusal and re-fire after; and so does one the run's row handed back,
// which it carries the same way. Nor does any delivery bring back a re-fire
// whose renewal the broker does not take, so this node makes that renewal
// again on its own timer until one lands or this node settles the run
// ([refirePublish]), past the seat's release and a lease it lost, and its own
// line says so, never that a delivery was handed back to make the renewal
// again: WHAT A REFUSED RENEWAL LEAVES IS ITS CALLER'S TO SAY
// ([Coordinator.renewing], [Coordinator.refire]).
//
// NOTHING IS RENEWED WHILE NEITHER THE CLOCK NOR THE ROW SAYS SO: a step this
// node's own store did not answer is NAK'd and nothing more, since the waiter
// that drives the box, whose store does answer or answers once the silence is
// over, announces the job again on its schedule
// ([Coordinator.collectFailedAt]); and a delivery whose count the transport
// did not state is NAK'd as before this existed, since a missing number is no
// evidence of a spent budget.

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

// ErrUnannounced marks a delivery of a job's completion handed back because
// nothing announces the completion again but that delivery: this node keeps a
// clock on the store of the host of the job's box ([Coordinator.hostClocked]),
// or the run's row says what such a clock would ([unannouncedOnRow]), whose
// waiter reads the run's row through that store and announces nothing, and
// the run has not settled, which would have dropped the clock and moved the
// row on. A delivery
// so marked is never acknowledged, whatever refusal it carries
// (internal/engine's controlOutcome): it is handed back, and renewed before
// its last (renew.go).
var ErrUnannounced = errors.New("sandbox: no waiter announces the job's completion again, so its delivery " +
	"is handed back")

// unannounced is err, the coordinator's answer to a delivery of ev's
// completion while the clock on the host of its box runs, marked
// [ErrUnannounced]; and for a nil answer, which settled nothing (a claim
// another attempt holds, or a resume the run's row holds), the mark alone with
// what it is.
func unannounced(ev types.ExecutorFinished, err error) error {
	if err == nil {
		return fmt.Errorf("%w: the delivery of %s settled nothing, and its run has not settled",
			ErrUnannounced, ev.TurnID)
	}
	return fmt.Errorf("%w: %w", ErrUnannounced, err)
}

// renewing is what a delivery of ev's completion leaves with the broker, err
// being what the coordinator made of it ([Coordinator.OnCompleted]): err as it
// is, unless this node keeps a clock on the store of the host of the job's box
// ([Coordinator.hostClocked]) once that answer is in, or, keeping none, the
// run's row says nothing else announces the job ([Coordinator.unannouncedJob]).
// Then the run has not settled, which drops the clock and moves the row on,
// so the delivery is handed back marked [ErrUnannounced], err or no err; and
// where trigger, the delivery in hand, has at most [renewReserve] deliveries
// left, the completion is announced afresh
// ([Coordinator.renew]) and nil is answered instead, so the delivery is
// acknowledged and the fresh one carries the completion on. A renewal the
// broker does not take answers the hand-back beside why, so the delivery is
// NAK'd and comes back to make it again, which its line says
// (sandbox_completion_renew_failed).
func (c *Coordinator) renewing(ctx context.Context, ev types.ExecutorFinished, trigger *events.Event,
	err error,
) error {
	if trigger == nil || !c.unannouncedJob(ctx, ev, trigger) {
		return err
	}
	err = unannounced(ev, err)
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

// unannouncedJob reports whether nothing but a delivery of ev's completion
// announces it again: this node keeps a clock on the store of the host of the
// job's box ([Coordinator.hostClocked]), or, where it keeps none, the run's
// row says so ([unannouncedOnRow]), read here; trigger is the delivery in
// hand, nil for a re-fire, which has none.
//
// THE ROW, NEVER THIS NODE'S MEMORY ALONE: the clock does not outlive a
// restart, and the seat's next holder starts with none, while the host's
// waiter reads the row through its silent store and announces nothing either
// way. A row this node's own store does not answer says nothing, and the
// delivery is left as it came: the waiter, whose store answers once this
// node's does, announces the job again then.
//
// AND ONLY WHERE THE DELIVERY'S OWN COLLECTION READ NO BOX ([boxWasRead]): the
// box's host reads the run's row through its store before it reads the box,
// so a read of it says that host's waiter announces the job again from there,
// and the release of a claim that read the box clears the record anyway
// ([Coordinator.hostStoreSeen]). Read after every delivery, a successful one
// cost a read of the coordination store for an answer that could not change
// what it left. What is left reads it: a refusal of the node's gate, which
// would otherwise be acknowledged while no waiter can announce the job, a
// hand-back made before the box answered, an answer that settled nothing,
// and the carry of a job lost with its host, which reads no box.
func (c *Coordinator) unannouncedJob(ctx context.Context, ev types.ExecutorFinished, trigger *events.Event) bool {
	if c.hostClocked(ev.TurnID, ev.LaunchID) {
		return true
	}
	if boxWasRead(ctx) {
		return false
	}
	run, found, err := c.pending.Get(ctx, ev.TurnID)
	return err == nil && found && unannouncedOnRow(run, ev.LaunchID, trigger)
}

// unannouncedOnRow reports whether run's row says that nothing but a delivery
// of its job launch's completion announces it again, while its run has not
// settled (still running or claimed, with no ending decided, on that job): the
// job is recorded lost with the host of its box because that host's store did
// not answer it ([LossLaunchNodeStoreSilent]), and its round is not carried
// on yet, which the job's next launch or the run's ending is; or its record
// says that host last found its store silent
// ([LaunchRecord.HostStoreSilentSince]) and trigger, the delivery in hand, is
// no waiter's announcement of the job made since ([announcedAfter]), which
// says that store answered.
func unannouncedOnRow(run PendingRun, launch string, trigger *events.Event) bool {
	if run.LaunchID != launch || run.Ending != nil ||
		(run.Status != StatusRunning && run.Status != StatusResumed) {
		return false
	}
	if run.Lost != nil && run.Lost.Cause == LossLaunchNodeStoreSilent {
		return true
	}
	since := run.Launch.HostStoreSilentSince
	return !since.IsZero() && !announcedAfter(trigger, since)
}

// rowAskedField is the envelope bag key under which a waiter's announcement
// of a job's completion says when that waiter asked its coordination store
// for the run's row it announced the job from ([askedRow]): an instant in RFC
// 3339, in UTC, which every backend carries verbatim and every republish
// keeps, as [queue.PriorityField] is.
//
// THE READ'S INSTANT, NEVER THE ANNOUNCEMENT'S. What the node holding the
// job's seat reads off it is that the store of the host of the job's box
// answered that waiter after an instant this node can compare with its own
// clock on that store ([announcedAfter]). The announcement's own stamp
// ([events.Event.Timestamp]) is taken once the waiter has polled the box,
// which can take a minute ([pollBound]) after the wait for a slot: compared
// instead, an announcement whose read that store answered before a silence
// that began during the poll, made after a collection had found the silence,
// was read as the silence's end while it lasted, its clock dropped and its
// record cleared by a claim that learned nothing more.
const rowAskedField = "completion_row_asked"

// askedRow marks ev, a waiter's announcement of a job's completion, with
// asked, when that waiter asked its store for the run's row it announced the
// job from ([rowAskedField]).
func askedRow(ev *events.Event, asked time.Time) {
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	ev.Payload[rowAskedField] = asked.UTC().Format(time.RFC3339Nano)
}

// rowAsked is when the waiter that announced ev asked its store for the run's
// row it announced the job from ([rowAskedField]): zero where ev says none, a
// renewal, which reads no row, or an announcement of a build that says none.
func rowAsked(ev *events.Event) time.Time {
	if ev == nil {
		return time.Time{}
	}
	stamped, _ := ev.Payload[rowAskedField].(string)
	asked, err := time.Parse(time.RFC3339Nano, stamped)
	if err != nil {
		return time.Time{}
	}
	return asked
}

// announcedAfter reports whether trigger, a delivery of a job's completion, is
// a waiter's announcement of it made from a read of the run's row asked after
// since, the start of a clock on the store of the host of its box or its
// record of it: the read it says it was made from ([rowAskedField]) asked
// after that instant, which a renewal, reading no row, never says. That waiter read the row
// through the store the clock times, so the store answered since.
//
// STRICTLY AFTER, BY THE READ'S OWN INSTANT, never the announcement's stamp
// ([events.Event.Timestamp]): an announcement whose read was asked before the
// clock started says nothing of the silence the clock found, however late it
// was made or delivered, its first delivery among them, and read as that
// store's answer it dropped the clock while the store was still silent, and
// the delivery that was the job's one copy was acknowledged. One that says no
// read, a build's that stamps none, says nothing either. The instant is the
// waiter's node's clock and since the collecting node's, as a hold's instant
// is written by one node and read by another's ([PendingRun.Held]): an
// announcement read wrongly takes a skew between the two larger than the time
// from the waiter's read to the collecting node's receipt of the host's answer
// that its store did not answer, which that host's own read, its bound and the
// answer's carry are part of.
func announcedAfter(trigger *events.Event, since time.Time) bool {
	return trigger != nil && rowAsked(trigger).After(since)
}

// deliveredKey is the context key under which a delivery's attempt carries
// what it learns of the store of the host of its box ([delivering]).
type deliveredKey struct{}

// attempt is what a delivery of a job's completion carries through the
// attempt it is made for: the delivery itself, and whether that attempt's
// collection read the job's box.
type attempt struct {
	trigger *events.Event
	boxRead atomic.Bool
}

// delivering is ctx carrying trigger, the delivery of a job's completion an
// attempt is made for, so the release that hands its claim back records what
// that delivery says of the store of the host of its box
// ([Coordinator.hostStoreSeen]), and the attempt's collection says whether it
// read the box ([readTheBox]).
func delivering(ctx context.Context, trigger *events.Event) context.Context {
	return context.WithValue(ctx, deliveredKey{}, &attempt{trigger: trigger})
}

// deliveredIn is the delivery ctx's attempt is made for ([delivering]), or nil.
func deliveredIn(ctx context.Context) *events.Event {
	if a, ok := ctx.Value(deliveredKey{}).(*attempt); ok {
		return a.trigger
	}
	return nil
}

// readTheBox records that the collection of the attempt ctx carries
// ([delivering]) read the job's box. An attempt carried by nothing, a
// re-fire's, records nothing.
func readTheBox(ctx context.Context) {
	if a, ok := ctx.Value(deliveredKey{}).(*attempt); ok {
		a.boxRead.Store(true)
	}
}

// boxWasRead reports whether the collection of the attempt ctx carries read
// the job's box ([readTheBox]).
func boxWasRead(ctx context.Context) bool {
	a, ok := ctx.Value(deliveredKey{}).(*attempt)
	return ok && a.boxRead.Load()
}

// hearAnnouncement ends this node's clock on the store of the host of the box
// of ev's job ([hostUnsettled]) where trigger, the delivery in hand, is a
// waiter's announcement of the job made from a read of the run's row asked
// after that clock started ([announcedAfter]): that store answered since, and
// the waiter that read the row through it announces the job again from there.
// Answered here, before the delivery is claimed, so the claim's collection
// starts the clock again where that store is silent once more, and a refusal
// of the delivery is acknowledged as any is where a waiter stands behind it.
func (c *Coordinator) hearAnnouncement(ctx context.Context, ev types.ExecutorFinished, trigger *events.Event) {
	c.mu.Lock()
	kept, ok := c.unsettled[ev.TurnID]
	heard := ok && kept.launch == ev.LaunchID && announcedAfter(trigger, kept.since)
	if heard {
		delete(c.unsettled, ev.TurnID)
	}
	c.mu.Unlock()
	if heard {
		log.InfoContext(ctx, "sandbox_host_store_answered",
			"turn_id", ev.TurnID, "launch_id", ev.LaunchID, "agent", ev.AgentHandle,
			"silent_for_s", rowAsked(trigger).Sub(kept.since).Seconds(),
			"detail", "the waiter that drives the job's box announced it from a read of the run its "+
				"node's coordination store answered after that store was found silent; this node stops "+
				"renewing the job's completion")
	}
}

// renew announces ev's completion afresh on its seat's control topic, for a
// hand-back while the clock on its box's host runs, and answers why it could
// not where the broker did not take it. What that leaves is the caller's to
// say: a delivery in hand comes back to make it again ([Coordinator.renewing]),
// and a re-fire, which has none, is made again on this node's timer
// ([Coordinator.refire], [refirePublish]).
//
// TRIGGER'S TRACE AND SOURCE, so the resume it leads to nests under the turn
// that launched the job, as the waiter's own announcement does; trigger may be
// nil, which carries neither. NEVER THE READ TRIGGER SAYS ([rowAskedField]): a
// renewal is made because no waiter can read the run's row, and reads none
// itself, so it says no read and no node takes it for that waiter's
// announcement made since a silence began ([announcedAfter]).
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

// refirePublish is a re-fire's announcement afresh that the broker did not take
// ([Coordinator.refire]), which this node makes again on its own timer.
//
// A RE-FIRE HAS NO DELIVERY TO COME BACK ON. It renews only where its own
// collection started the clock on its box's host, so the delivery it was kept
// from was acknowledged, no clock running then, and neither that host's waiter
// nor anything else carries the completion: a renewal the broker refused, left
// so, held the job until that host's store answered. So it is published again
// after the hand-back's spacing ([answerRetryDelay]: a second, doubling to
// thirty, the queue's own failure backoff), each refusal arming the next
// attempt, and KEPT UNTIL ONE LANDS OR THIS NODE SETTLES THE RUN
// ([Coordinator.runSettled]): from there whatever settled the run has
// announced what became of it. NOT DROPPED WHEN THE SEAT IS LET GO, NOR AT A
// LEASE THIS NODE LOST ([Coordinator.ReleaseSeat]): the announcement goes to
// the seat's control topic, which takes a publish under no lease and which
// whoever holds the seat next reads, and nothing else would tell that holder
// of the job, since the delivery the re-fire was kept from was acknowledged
// and the host's waiter announces nothing; dropped there, the completion
// waited for that host's store. Outside the clock, should the host's store
// answer again meanwhile, that host's waiter's schedule stands behind it as
// well; inside, nothing but this does. [Coordinator.Stop] cancels its armed
// attempt, and it is IN THIS NODE'S MEMORY, as the kept completion it came
// from is: a restart forgets both.
type refirePublish struct {
	ev       types.ExecutorFinished
	trigger  *events.Event
	cond     Condition
	attempts int
	// stop cancels the armed attempt, reporting whether it had not started,
	// [CoordinatorOptions.After]'s contract; nil once that attempt began.
	// Under mu.
	stop func() bool
}

// armRepublish keeps kept's announcement afresh, which the broker refused for
// the attempts-th time, and arms its next attempt after the hand-back's
// spacing, answering how long that is and whether it was armed: not for a
// coordinator that is stopping, nor, where prev is set, for an announcement
// that is no longer prev, its run settled here or a later re-fire's
// announcement landed meanwhile.
func (c *Coordinator) armRepublish(kept refusedCompletion, attempts int, prev *refirePublish,
) (time.Duration, bool) {
	turnID := kept.ev.TurnID
	delay := answerRetryDelay(attempts)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || (prev != nil && c.republish[turnID] != prev) {
		return delay, false
	}
	c.dropRepublishLocked(turnID)
	p := &refirePublish{ev: kept.ev, trigger: kept.trigger, cond: kept.cond, attempts: attempts}
	c.republish[turnID] = p
	c.inflight.Add(1)
	p.stop = c.after(delay, func() {
		defer c.inflight.Done()
		c.republishNow(turnID, p)
	})
	return delay, true
}

// republishNow makes the attempt p was armed for, where p is still owed: the
// announcement made, and dropped once the broker takes it, or, refused again,
// armed again after a longer spacing, which its line says. WHETHER OR NOT THIS
// NODE STILL HOLDS THE SEAT: the publish needs no lease, and reaches whoever
// holds the seat ([refirePublish]).
func (c *Coordinator) republishNow(turnID string, p *refirePublish) {
	ctx := c.life
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	owed := c.republish[turnID] == p
	if owed {
		p.stop = nil
	}
	c.mu.Unlock()
	if !owed {
		return
	}
	refused := c.renew(ctx, p.ev, p.trigger)
	if refused == nil {
		c.dropRepublish(turnID, p)
		return
	}
	delay, armed := c.armRepublish(refusedCompletion{cond: p.cond, ev: p.ev, trigger: p.trigger},
		p.attempts+1, p)
	if !armed {
		return
	}
	log.WarnContext(ctx, "sandbox_completion_refire_unsettled",
		"turn_id", p.ev.TurnID, "agent", p.ev.AgentHandle, "condition", string(p.cond),
		"attempts", p.attempts+1, "retry_in_s", delay.Seconds(), "error", refused.Error(),
		"detail", "the completion re-fired when its condition cleared still could not be announced "+
			"afresh, and its box's host cannot read the coordination store, so no waiter announces it; "+
			"this node announces it again on its timer until one lands or it settles the run, to "+
			"whoever holds the seat")
}

// dropRepublish drops p, turnID's owed announcement, where it still is.
func (c *Coordinator) dropRepublish(turnID string, p *refirePublish) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.republish[turnID] == p {
		c.dropRepublishLocked(turnID)
	}
}

// dropRepublishLocked drops turnID's owed announcement, if any, its armed
// attempt cancelled. Under mu.
func (c *Coordinator) dropRepublishLocked(turnID string) {
	p := c.republish[turnID]
	if p == nil {
		return
	}
	if p.stop != nil && p.stop() {
		c.inflight.Done()
	}
	p.stop = nil
	delete(c.republish, turnID)
}

// runSettled drops what this node keeps of turnID's run only until the run
// settles: the clock on the store of its box's host ([hostUnsettled], which
// says where a run settles) and a re-fire's announcement it still owes
// ([refirePublish]).
func (c *Coordinator) runSettled(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runSettledLocked(turnID)
}

// runSettledLocked is [Coordinator.runSettled] for a caller holding mu.
func (c *Coordinator) runSettledLocked(turnID string) {
	delete(c.unsettled, turnID)
	c.dropRepublishLocked(turnID)
}
