package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/alarmwake"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ADVISOR'S ALARM HISTORY (ADR-0164): every transition this node's alarm
// tracker makes is kept as an episode on the advisor log, a fire opening one
// and a clear closing it, written where the transition is made, so each
// instance (the alarm, and the log or entry it stands on) and every flap is
// kept, which the fleet's heartbeat, holding only what stands now, cannot say.
// The Advisor's alarm classes read the episodes.
//
// History, not delivery: a transition the log does not take after
// [alarmEpisodeAttempts] is dropped and logged, and one still queued at stop
// is lost. Each write is under an operation id derived from the transition,
// so a retry is the same operation, and the log opens at most one episode of
// a series at a time, so a fire told twice opens one.

// alarmEpisodeCap is how many transitions a node holds unwritten before it
// drops the oldest: the outbox's own bound, for the same table.
const alarmEpisodeCap = alarmOutboxCap

// alarmEpisodeAttempts and alarmEpisodeRetry bound how hard one transition is
// written: a data node electing or catching up is back within a few seconds.
const (
	alarmEpisodeAttempts = 3
	alarmEpisodeRetry    = 2 * time.Second
)

// alarmEpisodes holds a node's unwritten transitions, oldest first.
type alarmEpisodes struct {
	// writes is the advisor log's writes as the engine reaches them now:
	// false on a node that reaches none yet.
	writes func() (estate.AdvisorWriter, bool)
	// wait is how long a refused write waits before its next attempt.
	wait time.Duration
	// taken, where set, is told of every fire once its episode is done with,
	// written or not: on a node with no broker this is the one hand-off,
	// and a raise it hands on is kept across a restart as it was when
	// nothing was told (statelog.Tracker.Told). Nil beside the outbox,
	// whose broker tells it.
	taken func(ctx context.Context, id string)

	ready   chan struct{}
	mu      sync.Mutex
	pending []statelog.AlarmTransition
}

func newAlarmEpisodes(writes func() (estate.AdvisorWriter, bool)) *alarmEpisodes {
	return &alarmEpisodes{writes: writes, wait: alarmEpisodeRetry, ready: make(chan struct{}, 1)}
}

// Add queues one transition, and NEVER BLOCKS: the tracker calls it under its
// own lock. At the bound the oldest is dropped.
func (a *alarmEpisodes) Add(t statelog.AlarmTransition) {
	a.mu.Lock()
	var dropped []statelog.AlarmTransition
	if over := len(a.pending) + 1 - alarmEpisodeCap; over > 0 {
		dropped = slices.Clone(a.pending[:over])
		a.pending = slices.Delete(a.pending, 0, over)
	}
	a.pending = append(a.pending, t)
	a.mu.Unlock()
	for _, d := range dropped {
		log.Warn("alarm_episode_dropped", "transition", d.ID, "alarm", string(d.Kind),
			"detail", "the advisor log has not taken a transition for as long as the bound holds")
	}
	select {
	case a.ready <- struct{}{}:
	default:
	}
}

// run writes the queue until ctx ends.
func (a *alarmEpisodes) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.ready:
		}
		for {
			a.mu.Lock()
			if len(a.pending) == 0 {
				a.mu.Unlock()
				break
			}
			t := a.pending[0]
			a.pending = a.pending[1:]
			a.mu.Unlock()
			a.keep(ctx, t)
		}
	}
}

// keep writes one transition, retrying a refusal, and logs one it could not.
func (a *alarmEpisodes) keep(ctx context.Context, t statelog.AlarmTransition) {
	if a.taken != nil && t.Change == statelog.TransitionFired {
		defer a.taken(ctx, t.ID)
	}
	var err error
	for attempt := range alarmEpisodeAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(a.wait):
			}
		}
		if err = a.write(ctx, t); err == nil {
			return
		}
	}
	log.Warn("alarm_episode_not_kept", "transition", t.ID, "alarm", string(t.Kind),
		"change", string(t.Change), "error", err.Error())
}

// errNoAdvisorLog is a node that reaches no advisor log to keep episodes on.
var errNoAdvisorLog = errors.New("engine: this node reaches no advisor log to keep the alarm's episode on")

// write is one transition as its episode's write: a fire raises the series, a
// clear clears it, each under an operation derived from the transition.
func (a *alarmEpisodes) write(ctx context.Context, t statelog.AlarmTransition) error {
	w, ok := a.writes()
	if !ok {
		return errNoAdvisorLog
	}
	instance := alarmwake.InstanceID(t)
	call := advisordomain.Call{By: duty.Engine, OpID: statelog.DeriveOpID(t.At, "advisor_alarm", t.ID)}
	var err error
	switch t.Change {
	case statelog.TransitionFired:
		_, err = w.RaiseAlarm(ctx, call, advisordomain.AlarmRaise{Node: t.Node, Kind: string(t.Kind),
			Instance: instance, RaisedAt: t.Since})
	case statelog.TransitionCleared:
		_, err = w.ClearAlarm(ctx, call, t.Node, string(t.Kind), instance,
			advisordomain.AlarmClear{ClearedAt: t.At})
	}
	return err
}
