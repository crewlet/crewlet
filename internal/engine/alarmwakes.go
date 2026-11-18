package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/alarmwake"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE ALARMS' OUTBOX: every transition this node's alarm tracker makes,
// published as an inbound delivery of source `alarms` for the notify spine to
// route to the seats whose role holds the health grant (internal/alarmwake),
// retried until the broker takes it.
//
// ADR-0095: the publish IS the write, so nothing commits beside it. The outbox
// owes publishing each transition in order however long the broker is gone,
// and a long outage costs the OLDEST transitions, counted, never the node's
// memory. One still queued at stop is lost: the current state is the
// heartbeat's (`fleet_health`), which a wake only prompts a seat to read, and
// a raise is kept across a restart only once taken, so the next start raises
// it again.

// alarmOutboxCap is how many transitions a node holds unpublished before it
// drops the oldest (the newest are nearest the state a woken seat reads).
//
// 2048: a fire and a clear of every instance the table can raise on a data
// node (each node-wide kind once, each [statelog.PerLogKinds] kind once per
// [registeredDomains] log) fit six times over, which
// TestTheAlarmOutboxHoldsSixFlapsOfTheWholeTable computes; at a few hundred
// bytes a transition, under a megabyte. crewlet.alarm.transitions.dropped's
// description and the event-system page restate the figure, held to this
// constant by TestEveryTextNamingTheAlarmOutboxBoundNamesTheConstant.
const alarmOutboxCap = 2048

// alarmRetryBase and alarmRetryCeiling pace a publish the broker refused:
// two seconds first, the change feed's nak delay, so a reconnect or leader
// election completes inside one delay; doubling to the alarm interval and no
// further, since transitions are made at that cadence and a slower retry would
// tell the seats later than the node's own heartbeat already says.
const (
	alarmRetryBase    = 2 * time.Second
	alarmRetryCeiling = statelog.AlarmInterval
)

// alarmRetryJitter spreads the retries of nodes that lost one broker together,
// by backoff's own fraction for a fleet waking in lockstep.
const alarmRetryJitter = 0.2

// alarmOutbox holds a node's unpublished alarm transitions, oldest first, and
// publishes them one at a time.
type alarmOutbox struct {
	publisher queue.Publisher
	metrics   *metrics.Recorder

	// retry is how long a refused publish waits before its attempt-th
	// retry. Nil is [alarmRetryBase] doubling to [alarmRetryCeiling],
	// jittered; a case sets one that does not wait.
	retry func(attempt int) time.Duration

	// taken is told the id of every transition the broker took, nil for none.
	taken func(ctx context.Context, id string)

	// ready holds one token while a transition may be waiting: Add never
	// blocks on it, and the loop drains the queue whole on each.
	ready chan struct{}

	mu      sync.Mutex
	pending []statelog.AlarmTransition
	// sending marks the head as in flight: the overflow drops the next
	// oldest instead, so a transition the broker may already hold is never
	// counted as lost.
	sending bool
}

// newAlarmOutbox builds an outbox publishing through p.
func newAlarmOutbox(p queue.Publisher, rec *metrics.Recorder) *alarmOutbox {
	return &alarmOutbox{publisher: p, metrics: rec, ready: make(chan struct{}, 1)}
}

// Add queues one transition, and NEVER BLOCKS: the tracker calls it under its
// own lock (statelog.Tracker.OnTransition). At the bound the oldest
// transition not in flight is dropped, counted and logged.
func (o *alarmOutbox) Add(t statelog.AlarmTransition) {
	o.mu.Lock()
	var dropped []statelog.AlarmTransition
	for len(o.pending) >= alarmOutboxCap {
		i := 0
		if o.sending {
			i = 1
		}
		dropped = append(dropped, o.pending[i])
		o.pending = slices.Delete(o.pending, i, i+1)
	}
	o.pending = append(o.pending, t)
	o.mu.Unlock()
	for _, d := range dropped {
		o.drop(d, "the outbox is full: the broker has not taken a transition for as long as "+
			"the bound holds, so the oldest goes first")
	}
	select {
	case o.ready <- struct{}{}:
	default:
	}
}

// drop counts and logs one transition no seat will be woken by.
func (o *alarmOutbox) drop(t statelog.AlarmTransition, why string) {
	if o.metrics != nil {
		o.metrics.Add(metrics.AlarmTransitionsDropped, 1, nil)
	}
	log.Warn("alarm_transition_dropped", "transition", t.ID, "alarm", string(t.Kind),
		"domain", t.Domain, "change", string(t.Change), "detail", why)
}

// run publishes the queue until ctx ends.
func (o *alarmOutbox) run(ctx context.Context) {
	failing := 0
	for {
		t, ok := o.head()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-o.ready:
				continue
			}
		}
		err := o.publish(ctx, t)
		switch {
		case err == nil:
			o.done(true)
			if o.taken != nil {
				o.taken(ctx, t.ID)
			}
			if failing > 0 {
				log.InfoContext(ctx, "alarm_transitions_publishing", "after", failing,
					"detail", "the broker takes the alarm transitions again")
			}
			failing = 0
			if o.metrics != nil {
				o.metrics.Add(metrics.AlarmTransitions, 1, metrics.Attrs{"change": string(t.Change)})
			}
			continue
		case errors.Is(err, alarmwake.ErrNotATransition):
			// A TRANSITION NO RETRY CAN SEND: one the tracker made without
			// what a wake is derived from. Dropped, not retried for ever
			// at the head of every transition behind it.
			o.done(true)
			o.drop(t, err.Error())
			continue
		}
		o.done(false)
		if ctx.Err() != nil {
			return
		}
		failing++
		if failing == 1 {
			log.WarnContext(ctx, "alarm_transition_unpublished", "transition", t.ID,
				"alarm", string(t.Kind), "error", err.Error(),
				"detail", "retried until the broker takes it; no seat is woken by it meanwhile")
		}
		wait := time.NewTimer(o.wait(failing))
		select {
		case <-ctx.Done():
			wait.Stop()
			return
		case <-wait.C:
		}
	}
}

// head is the oldest transition, marked in flight, and false when none waits.
func (o *alarmOutbox) head() (statelog.AlarmTransition, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.pending) == 0 {
		return statelog.AlarmTransition{}, false
	}
	o.sending = true
	return o.pending[0], true
}

// done ends the head's flight, removing it where it is settled.
func (o *alarmOutbox) done(settled bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sending = false
	if settled && len(o.pending) > 0 {
		o.pending = slices.Delete(o.pending, 0, 1)
	}
}

// wait is how long the attempt-th retry waits.
func (o *alarmOutbox) wait(attempt int) time.Duration {
	if o.retry != nil {
		return o.retry(attempt)
	}
	return backoff.Jitter(backoff.Doubling(attempt, alarmRetryBase, alarmRetryCeiling), alarmRetryJitter)
}

// publish sends one transition as the inbound delivery alarmwake reads.
func (o *alarmOutbox) publish(ctx context.Context, t statelog.AlarmTransition) error {
	w, err := alarmwake.Encode(t)
	if err != nil {
		return err
	}
	ev := events.New(w, events.NewTrace())
	ev.Source = alarmwake.Source
	return o.publisher.Publish(ctx, topics.NotificationsInbound, ev)
}

// pendingCount is how many transitions wait, the one in flight included.
func (o *alarmOutbox) pendingCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pending)
}
