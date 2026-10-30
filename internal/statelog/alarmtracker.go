package statelog

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// alarmGauge is the instrument every firing alarm sets.
const alarmGauge = metrics.AlarmActive

// Tracker turns successive evaluations into the two surfaces that are not a
// screen: a log line on every transition, and a gauge an operator's
// collector scrapes.
//
// # Why a transition and not a level
//
// An alarm evaluated on the position heartbeat ([AlarmInterval]) is true
// for as long as the condition is, which is minutes or days. Logging the
// level would write the same line six times a minute for a week and make the
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
// [AlarmInterval], the trim tick evaluates again the moment its own
// measurements land, on a different goroutine, and the health envelope reads
// [Tracker.Standing] from every request and push tick.
type Tracker struct {
	rec *metrics.Recorder
	now func() time.Time

	// logger is where the two transition lines go: the package's own
	// outside a case.
	logger *slog.Logger

	mu     sync.Mutex
	firing map[instance]firing

	// order is the kinds the latest evaluation reported, in the order it
	// reported them (the table's own), and observed whether any evaluation
	// has run at all. Both are for [Tracker.Standing].
	order    []Kind
	observed bool

	// latest is the latest evaluation's alarms, for [Tracker.Latest].
	latest []Alarm
}

// instance is what the tracker writes ONE PAIR OF LOG LINES for: a kind, and
// the log it stands on when it is a per-log condition ([PerLogKinds]), or the
// model entry when it is a per-entry one ([PerEntryKinds]).
//
// THE KIND ALONE IS NOT THE LOG LINE'S IDENTITY. Seven conditions are
// evaluated once per log ([Report]), so one alarm can stand on the tracker's
// log and the knowledge base's at once ([Alarm.Domains]). Keyed on the kind,
// the second log joining it raised no line of its own, a log whose condition
// cleared while another's stood said nothing, and the one `alarm_cleared` that
// finally came carried the LAST log's detail against the FIRST one's start.
//
// AND THE KIND ALONE IS STILL THE ALARM'S IDENTITY everywhere else: the alarm
// is one per kind, the gauge one series per kind and [Tracker.Standing] one
// entry per kind, so an instance is a log line's key and nothing more. A
// node-wide kind lists no log, so it is one instance with none.
//
// A PER-ENTRY KIND ([PerEntryKinds]) is the same move over model entries: one
// instance per entry it stands on, so the second model whose token comes due
// raises a line of its own.
type instance struct {
	kind   Kind
	domain string
	entry  string
}

// firing is one instance currently up.
type firing struct {
	since  time.Time
	detail string
}

// instancesOf is an alarm's instances, each with the detail its lines carry:
// one per log or model entry the alarm stands on, carrying that log's or that
// entry's own measurement, and otherwise one with no log, carrying the
// alarm's.
//
// THE LOGS AN ALARM LISTS ARE ITS INSTANCES, with no second check of the kind:
// only a per-log kind lists any, which the report's own test holds in both
// directions, so a check here would be a second opinion about one fact. An
// entry that names several logs — one measurement taken identically on each
// ([AlarmDomain.Domains]) — is an instance per log it names, so carrying a
// measurement once changes nothing about which log lines are written. The
// model entries an alarm lists are its instances by the same rule: only a
// per-entry kind lists any, since [Evaluate] fills [Alarm.Entries] for the
// table's per-entry rows and nothing else fills it.
func instancesOf(a Alarm) ([]instance, []string) {
	if len(a.Entries) > 0 {
		keys := make([]instance, 0, len(a.Entries))
		details := make([]string, 0, len(a.Entries))
		for _, e := range a.Entries {
			keys = append(keys, instance{kind: a.Kind, entry: e.Key})
			details = append(details, e.Detail)
		}
		return keys, details
	}
	if len(a.Domains) == 0 {
		return []instance{{kind: a.Kind}}, []string{a.Detail}
	}
	var keys []instance
	var details []string
	for _, d := range a.Domains {
		for _, domain := range d.Domains {
			keys = append(keys, instance{kind: a.Kind, domain: domain})
			details = append(details, d.Detail)
		}
	}
	return keys, details
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
// EACH INSTANCE IS RAISED AND CLEARED ON ITS OWN in the log: a node-wide kind,
// or one log a per-log kind stands on (see [instance]). So a log line starts
// and ends each, as the published reference promises, however many logs share
// a kind, and a log joining or leaving a kind that stays up is said.
//
// EVERY KIND IS WRITTEN TO THE GAUGE on every observation, firing or not.
// Writing only the firing ones would leave a cleared alarm's last value at 1
// for as long as the collector remembers it, which is an alarm that never
// goes away for anybody reading the metric rather than the log. The gauge
// stays ONE SERIES PER KIND, at 1 while the kind's alarm is up on any log: a
// series per log would be a label a collector keeps for every log a
// deployment ever ran, for a question the log line and the screen already
// answer by name.
func (t *Tracker) Observe(ctx context.Context, alarms []Alarm) []Alarm {
	now := t.now()
	type raised struct {
		key    instance
		detail string
		remedy string
	}
	var up []raised
	live := make(map[instance]bool, len(alarms))
	kinds := make(map[Kind]bool, len(alarms))
	for _, a := range alarms {
		kinds[a.Kind] = true
		keys, details := instancesOf(a)
		for i, key := range keys {
			if live[key] {
				// One log listed twice under one kind is one
				// instance; the first listing's detail stands.
				continue
			}
			live[key] = true
			up = append(up, raised{key: key, detail: details[i], remedy: a.Remedy})
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.observed = true
	t.latest = slices.Clone(alarms)
	t.order = t.order[:0]
	for _, a := range alarms {
		if !slices.Contains(t.order, a.Kind) {
			t.order = append(t.order, a.Kind)
		}
	}
	for _, r := range up {
		if was, already := t.firing[r.key]; already {
			// Still up. The detail may have moved (a lag grows), and
			// that is not a transition.
			t.firing[r.key] = firing{since: was.since, detail: r.detail}
			continue
		}
		t.firing[r.key] = firing{since: now, detail: r.detail}
		t.logger.WarnContext(ctx, "alarm_raised", r.key.attrs(
			"detail", r.detail, "remedy", r.remedy)...)
	}
	for _, key := range slices.SortedFunc(maps.Keys(t.firing), compareInstances) {
		if live[key] {
			continue
		}
		was := t.firing[key]
		delete(t.firing, key)
		t.logger.WarnContext(ctx, "alarm_cleared", key.attrs(
			"for", spoken(now.Sub(was.since)), "detail", was.detail)...)
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

// attrs is a transition line's attributes: the alarm, the log it stands on
// when it is a per-log one, and the rest.
func (i instance) attrs(rest ...any) []any {
	out := []any{"alarm", string(i.kind)}
	if i.domain != "" {
		out = append(out, "domain", i.domain)
	}
	if i.entry != "" {
		out = append(out, "entry", i.entry)
	}
	return append(out, rest...)
}

// compareInstances orders instances by kind and then log, so the clears one
// observation logs come out in the same order every time.
func compareInstances(a, b instance) int {
	if c := cmp.Compare(a.kind, b.kind); c != 0 {
		return c
	}
	if c := cmp.Compare(a.domain, b.domain); c != 0 {
		return c
	}
	return cmp.Compare(a.entry, b.entry)
}

// Latest is the alarms the latest evaluation found, in the table's order, and
// nil before any evaluation has run.
//
// THE SAME EVALUATION every other surface reads: the Models page shows this
// node's latest instances of the model kinds from here rather than judging the
// thresholds a second time, so the page, the health count, the gauge and the
// log lines cannot disagree about whether a token is expiring.
func (t *Tracker) Latest() []Alarm {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.latest)
}

// Standing reports the kinds the latest evaluation found firing, the
// LONGEST-STANDING first, and false before any evaluation has run.
//
// For the fourth surface, the health envelope every node pushes to every tab,
// which carries a count and ONE name rather than the table: the name a health
// card can afford is the condition that has gone unanswered longest. Not the
// table's order, which is the order the reference reads in and asserts no
// ranking: a table sorted by severity would be a second opinion about each
// alarm beside the threshold that already decides it. How long something has
// been wrong is a fact this tracker holds and nothing else does, and the
// table's order breaks a tie only between alarms raised by one evaluation.
//
// ONE ENTRY PER KIND, however many logs it stands on: the count is how many
// problems an operator has to look at, and one cause standing on several logs
// is one of them, as the gauge beside it says. A per-log kind has stood since
// its longest-standing log's instance was raised.
//
// FALSE IS NOT "NONE FIRING": a node whose alarm table has not been evaluated
// yet (mid-boot, or running no state log at all) cannot say, and an empty
// list here would tell a health card it is healthy.
func (t *Tracker) Standing() ([]Kind, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.observed {
		return nil, false
	}
	since := make(map[Kind]time.Time, len(t.order))
	for key, f := range t.firing {
		if at, seen := since[key.kind]; !seen || f.since.Before(at) {
			since[key.kind] = f.since
		}
	}
	out := append(make([]Kind, 0, len(t.order)), t.order...)
	slices.SortStableFunc(out, func(a, b Kind) int {
		return since[a].Compare(since[b])
	})
	return out, true
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
