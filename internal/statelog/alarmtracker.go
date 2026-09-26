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
const alarmGauge = metrics.AlarmActive

// Tracker turns successive evaluations into the two surfaces that are not a
// screen: a log line on every transition, and a gauge an operator's
// collector scrapes.
//
// # Why a transition and not a level
//
// An alarm evaluated on a ten-second heartbeat is true for as long as the
// condition is, which is minutes or days. Logging the level would write the
// same line six times a minute for a week and make the log useless for
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
// A Tracker is safe for concurrent use: one loop evaluates, and the health
// envelope reads [Tracker.Standing] from every request and push tick.
type Tracker struct {
	rec *metrics.Recorder
	now func() time.Time

	mu     sync.Mutex
	firing map[Kind]firing

	// order is the kinds the latest evaluation reported, in the order it
	// reported them — the table's own — and observed whether any
	// evaluation has run at all. Both are for [Tracker.Standing].
	order    []Kind
	observed bool
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

	t.observed = true
	t.order = t.order[:0]
	for _, a := range alarms {
		t.order = append(t.order, a.Kind)
	}
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

// Standing reports the kinds the latest evaluation found firing, the
// LONGEST-STANDING first, and false before any evaluation has run.
//
// For the fourth surface, the health envelope every node pushes to every tab,
// which carries a count and ONE name rather than the table: the name a health
// card can afford is the condition that has gone unanswered longest. Not the
// table's order, which is the order the reference reads in and asserts no
// ranking — a table sorted by severity would be a second opinion about each
// alarm beside the threshold that already decides it. How long something has
// been wrong is a fact this tracker holds and nothing else does, and the table's
// order breaks a tie only between alarms raised by one evaluation.
//
// FALSE IS NOT "NONE FIRING": a node whose alarm table has not been evaluated
// yet — mid-boot, or running no state log at all — cannot say, and an empty
// list here would tell a health card it is healthy.
func (t *Tracker) Standing() ([]Kind, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.observed {
		return nil, false
	}
	out := slices.Clone(t.order)
	if out == nil {
		out = []Kind{}
	}
	slices.SortStableFunc(out, func(a, b Kind) int {
		return t.firing[a].since.Compare(t.firing[b].since)
	})
	return out, true
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
