package statelog

import (
	"cmp"
	"context"
	"log/slog"
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
// An alarm evaluated on the fifteen-second heartbeat ([AlarmInterval]) is
// true for as long as the condition is, which is minutes or days. Logging the
// level would write the same line four times a minute for a week and make the
// log useless for finding when it STARTED — which is the one thing an
// operator needs and the one thing a level cannot say. So entry and exit are each logged once,
// carrying how long the alarm was up.
//
// The gauge is the opposite and is a level by construction: a collector
// samples it, so it has to be true at the moment it is read. It carries one
// series per kind, set to 1 while the alarm holds — on any log, for a per-log
// one — and 0 when it clears: never DELETED, because a series that disappears
// reads as "no data" on every dashboard, and "no data" is indistinguishable
// from a node that stopped reporting.
//
// A Tracker is safe for concurrent use: the heartbeat evaluates every
// [AlarmInterval], and the trim tick evaluates again the moment its own
// measurements land, on a different goroutine.
type Tracker struct {
	rec *metrics.Recorder
	now func() time.Time

	// logger is where the two transition lines go: the package's own
	// outside a case.
	logger *slog.Logger

	mu     sync.Mutex
	firing map[instance]firing
}

// instance is ONE alarm: its kind, and the log it is about when it is a
// per-log condition.
//
// THE KIND ALONE IS NOT THE IDENTITY. Five conditions are evaluated once per
// log ([Report]), so one evaluation can carry `trim_blocked` for the tracker's
// log and again for the knowledge base's. Keyed on the kind, the second was
// folded into the first: it raised no line of its own, a log whose alarm
// cleared while another's stood said nothing, and the one `alarm_cleared`
// that finally came carried the LAST log's detail against the FIRST one's
// start — the dashboard had already found this out about its own list and
// keyed its rows on the pair, and the log line was left behind.
type instance struct {
	kind   Kind
	domain string
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
	return &Tracker{rec: rec, now: now, logger: log, firing: map[instance]firing{}}
}

// Observe records one evaluation and returns the alarms currently up, in the
// table's own order.
//
// EACH ALARM IS RAISED AND CLEARED ON ITS OWN — its kind and, for a per-log
// condition, its log (see [instance]) — so a log line starts and ends each
// one, as the published reference promises, however many logs share a kind.
//
// EVERY KIND IS WRITTEN TO THE GAUGE on every observation, firing or not.
// Writing only the firing ones would leave a cleared alarm's last value at 1
// for as long as the collector remembers it, which is an alarm that never
// goes away for anybody reading the metric rather than the log. The gauge
// stays ONE SERIES PER KIND, at 1 while any log's instance is up: a series
// per log would be a label a collector keeps for every log a deployment ever
// ran, for a question the log line and the screen already answer by name.
func (t *Tracker) Observe(ctx context.Context, alarms []Alarm) []Alarm {
	now := t.now()
	up := make(map[instance]Alarm, len(alarms))
	kinds := make(map[Kind]bool, len(alarms))
	for _, a := range alarms {
		up[instance{kind: a.Kind, domain: a.Domain}] = a
		kinds[a.Kind] = true
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, a := range alarms {
		key := instance{kind: a.Kind, domain: a.Domain}
		if was, already := t.firing[key]; already {
			// Still up. The detail may have moved — a lag grows — and
			// that is not a transition.
			t.firing[key] = firing{since: was.since, detail: a.Detail}
			continue
		}
		t.firing[key] = firing{since: now, detail: a.Detail}
		t.logger.WarnContext(ctx, "alarm_raised", key.attrs(
			"detail", a.Detail, "remedy", a.Remedy)...)
	}
	for _, key := range slices.SortedFunc(maps.Keys(t.firing), compareInstances) {
		if _, still := up[key]; still {
			continue
		}
		was := t.firing[key]
		delete(t.firing, key)
		t.logger.WarnContext(ctx, "alarm_cleared", key.attrs(
			"for", round(now.Sub(was.since)).String(), "detail", was.detail)...)
	}

	if t.rec != nil {
		for _, kind := range Kinds() {
			value := 0.0
			if kinds[kind] {
				value = 1
			}
			t.rec.Set(alarmGauge, value, metrics.Attrs{"kind": string(kind)})
		}
	}
	return alarms
}

// attrs is a transition line's attributes: the alarm, the log it is about when
// it is a per-log one, and the rest.
func (i instance) attrs(rest ...any) []any {
	out := []any{"alarm", string(i.kind)}
	if i.domain != "" {
		out = append(out, "domain", i.domain)
	}
	return append(out, rest...)
}

// compareInstances orders instances by kind and then log, so the clears one
// observation logs come out in the same order every time.
func compareInstances(a, b instance) int {
	if c := cmp.Compare(a.kind, b.kind); c != 0 {
		return c
	}
	return cmp.Compare(a.domain, b.domain)
}

// Firing reports how long each kind currently up has been up: the longest of
// its instances, since a per-log kind can stand on several logs at once.
//
// An alarm's AGE is what separates "this started four minutes ago" from "this
// started on Tuesday", and the alarm itself carries neither.
func (t *Tracker) Firing() map[Kind]time.Duration {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[Kind]time.Duration, len(t.firing))
	for key, f := range t.firing {
		out[key.kind] = max(out[key.kind], now.Sub(f.since))
	}
	return out
}
