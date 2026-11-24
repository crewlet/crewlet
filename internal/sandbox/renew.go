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

// A completion is renewed while this node keeps a clock on the store of its
// box's host ([Coordinator.hostClocked]), or the run's row says no waiter
// announces its job ([unannouncedOnRow]): no delivery is acknowledged unless
// the run settled; each is handed back marked [ErrUnannounced], whatever its
// kind, and announced afresh before the delivery in hand is its last
// ([renewReserve]). A waiter's announcement from a read asked since ends both
// ([announcedAfter]); a re-fire, which has no delivery, renews only where its
// own attempt started the clock ([refirePublish]). See the package doc, "A
// completion is renewed while the clock on its box's host runs".

// renewReserve is how many further deliveries a completion's delivery has when
// a hand-back while the clock on its box's host runs is announced afresh
// ([Coordinator.renewing]): the renewal is made on the delivery with this many
// left, or on any after it that comes back. Five, as [AnswerDeliveryReserve]
// is, and for its reasons: a failed publish and a seat handoff can each spend a
// delivery, and five span about two and a half minutes at the broker's 30 s
// ceiling. Renewed sooner, every delivery would be a fresh message handed over
// at once. Not derived from the broker's budget, for the reason
// [AnswerDeliveryReserve] gives.
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
// is, unless this node keeps a clock on the box host's store
// ([Coordinator.hostClocked]) or the row says nothing else announces the job
// ([Coordinator.unannouncedJob]). Then the delivery is handed back marked
// [ErrUnannounced]; where trigger has at most [renewReserve] deliveries left,
// the completion is announced afresh ([Coordinator.renew]) and nil answered, so
// the fresh one carries it. A renewal the broker does not take answers the
// hand-back beside why (sandbox_completion_renew_failed).
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
// announces it again: this node keeps a clock on the box host's store
// ([Coordinator.hostClocked]), or, keeping none, the run's row says so
// ([unannouncedOnRow]), read here; trigger is the delivery in hand, nil for a
// re-fire. The row, since the clock does not outlive a restart or a seat move;
// a row this node's store does not answer says nothing. Read only where the
// delivery's own collection read no box ([boxWasRead]), since a host that read
// the box announces the job again itself. See the package doc, "A completion is
// renewed while the clock on its box's host runs".
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

// rowAskedField is the envelope bag key under which a waiter's announcement of
// a job's completion says when that waiter asked its coordination store for the
// run's row it announced the job from ([askedRow]): an instant in RFC 3339, in
// UTC, which every backend carries verbatim and every republish keeps, as
// [queue.PriorityField] is. The read's instant, never the announcement's stamp,
// which comes after a poll of up to [pollBound] and could read a silence that
// began during the poll as already over ([announcedAfter]).
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
// a waiter's announcement made from a read of the run's row asked strictly
// after since, the start of a clock on the box host's store or its record
// ([rowAskedField]): that waiter read the row through the store the clock
// times, so the store answered since. Never the announcement's stamp
// ([events.Event.Timestamp]); a renewal or an older build says no read, and
// nothing. It tolerates clock skew up to the time from the waiter's read to the
// host's answer reaching the collecting node. See the package doc, "A
// completion is renewed while the clock on its box's host runs".
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
// not where the broker did not take it: a delivery in hand then comes back to
// make it again ([Coordinator.renewing]), a re-fire is made again on this
// node's timer ([Coordinator.refire], [refirePublish]). It carries trigger's
// trace and source (trigger may be nil), so the resume nests under the turn,
// and never the read trigger says ([rowAskedField]): a renewal reads no row, so
// no node takes it for a waiter's announcement ([announcedAfter]).
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
			"until the store answers or the host grace records the job lost with that node")
	return nil
}

// refirePublish is a re-fire's announcement afresh that the broker did not take
// ([Coordinator.refire]), which this node makes again on its own timer, since a
// re-fire has no delivery to come back on: every [answerRetryDelay] spacing,
// kept until one lands or this node settles the run ([Coordinator.runSettled]).
// Not dropped when the seat is let go or its lease lost
// ([Coordinator.ReleaseSeat]): the control topic takes a publish under no
// lease, and nothing else tells the next holder. [Coordinator.Stop] cancels its
// armed attempt; it is in this node's memory, so a restart forgets it. See the
// package doc, "A completion is renewed while the clock on its box's host
// runs".
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
// says where a run settles), a re-fire's announcement it still owes
// ([refirePublish]) and a resume of it waiting for the operator
// ([Coordinator.ResumesBlocked]).
func (c *Coordinator) runSettled(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runSettledLocked(turnID)
}

// runSettledLocked is [Coordinator.runSettled] for a caller holding mu.
func (c *Coordinator) runSettledLocked(turnID string) {
	delete(c.unsettled, turnID)
	c.dropRepublishLocked(turnID)
	delete(c.blocked, turnID)
}
