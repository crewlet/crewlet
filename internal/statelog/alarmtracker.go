package statelog

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

var log = logging.Get("statelog")

// alarmGauge is the instrument every firing alarm sets.
const alarmGauge = "crewlet.alarm.active"

// Tracker turns successive evaluations into the two surfaces that are not a
// screen: a log line on every transition, and a gauge an operator's
// collector scrapes.
//
// # Why a transition and not a level
//
// An alarm evaluated on a fifteen-second heartbeat is true for as long as the
// condition is, which is minutes or days. Logging the level would write the
// same line four times a minute for a week and make the log useless for
// finding when it STARTED — which is the one thing an operator needs and the
// one thing a level cannot say. So entry and exit are each logged once,
// carrying how long the alarm was up.
//
// The gauge is the opposite and is a level by construction: a collector
// samples it, so it has to be true at the moment it is read. It carries one
// series per kind, set to 1 while the alarm holds and 0 when it clears —
// never DELETED, because a series that disappears reads as "no data" on every
// dashboard, and "no data" is indistinguishable from a node that stopped
// reporting.
//
// A Tracker is safe for concurrent use: the trim tick and the heartbeat both
// evaluate, on different goroutines and different cadences.
type Tracker struct {
	rec *metrics.Recorder
	now func() time.Time

	mu     sync.Mutex
	firing map[Kind]firing
}

// firing is one alarm currently up.
type firing struct {
	since  time.Time
	detail string
}

// NewTracker builds a tracker. A nil recorder records nothing and still logs,
// which is the honest behaviour for a node whose metrics are not configured:
// the alarm is a fact about the company, not about the collector.
func NewTracker(rec *metrics.Recorder, now func() time.Time) *Tracker {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Tracker{rec: rec, now: now, firing: map[Kind]firing{}}
}

// Observe records one evaluation and returns the alarms currently up, in the
// table's own order.
//
// EVERY KIND IS WRITTEN TO THE GAUGE on every observation, firing or not.
// Writing only the firing ones would leave a cleared alarm's last value at 1
// for as long as the collector remembers it, which is an alarm that never
// goes away for anybody reading the metric rather than the log.
func (t *Tracker) Observe(ctx context.Context, alarms []Alarm) []Alarm {
	now := t.now()
	up := make(map[Kind]Alarm, len(alarms))
	for _, a := range alarms {
		up[a.Kind] = a
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, a := range alarms {
		if _, already := t.firing[a.Kind]; already {
			// Still up. The detail may have moved — a lag grows — and
			// that is not a transition.
			t.firing[a.Kind] = firing{since: t.firing[a.Kind].since, detail: a.Detail}
			continue
		}
		t.firing[a.Kind] = firing{since: now, detail: a.Detail}
		log.WarnContext(ctx, "alarm_raised",
			"alarm", string(a.Kind), "detail", a.Detail, "remedy", a.Remedy)
	}
	for _, kind := range slices.Sorted(maps.Keys(t.firing)) {
		if _, still := up[kind]; still {
			continue
		}
		was := t.firing[kind]
		delete(t.firing, kind)
		log.WarnContext(ctx, "alarm_cleared",
			"alarm", string(kind), "for", round(now.Sub(was.since)).String(),
			"detail", was.detail)
	}

	if t.rec != nil {
		for _, kind := range Kinds() {
			value := 0.0
			if _, firing := up[kind]; firing {
				value = 1
			}
			t.rec.Set(alarmGauge, value, metrics.Attrs{"kind": string(kind)})
		}
	}
	return alarms
}

// Firing reports how long each alarm currently up has been up.
//
// For the third surface — the operator's screen — which renders an alarm's
// AGE beside it: "this started four minutes ago" and "this started on Tuesday"
// are different problems, and the alarm itself carries neither.
func (t *Tracker) Firing() map[Kind]time.Duration {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[Kind]time.Duration, len(t.firing))
	for kind, f := range t.firing {
		out[kind] = now.Sub(f.since)
	}
	return out
}
