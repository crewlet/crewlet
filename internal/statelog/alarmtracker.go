package statelog

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
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
// # Every transition is handed on, once
//
// The same transitions the log lines say are handed, one [AlarmTransition]
// each, to whoever the node asked to be told ([Tracker.OnTransition]): the
// engine, which publishes each as the inbound delivery the seats holding the
// health grant are woken by. A transition rather than a level for the log
// line's reason — a wake per evaluation would wake a seat six times a minute
// about one alarm — and the record the wake derives from, so nothing commits
// beside it (ADR-0065). The first evaluation's finds are
// [TransitionFiringAtBoot], never fired: the tracker cannot tell an alarm
// that started then from one that stood through the node's restart, and that
// second case is also how a clear the node did not live to hand on is
// answered — what still stands is said again.
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

	// node and handOn are who is told of every transition, and as which
	// node ([Tracker.OnTransition]); a nil handOn tells nobody.
	node   string
	handOn func(AlarmTransition)

	// order is the kinds the latest evaluation reported, in the order it
	// reported them (the table's own), and observed whether any evaluation
	// has run at all. Both are for [Tracker.Standing].
	order    []Kind
	observed bool
}

// instance is what the tracker writes ONE PAIR OF LOG LINES for: a kind, and
// the log it stands on when it is a per-log condition ([PerLogKinds]).
//
// THE KIND ALONE IS NOT THE LOG LINE'S IDENTITY. Six conditions are evaluated
// once per log ([Report]), so one alarm can stand on the tracker's log and the
// knowledge base's at once ([Alarm.Domains]). Keyed on the kind, the second log
// joining it raised no line of its own, a log whose condition cleared while
// another's stood said nothing, and the one `alarm_cleared` that finally came
// carried the LAST log's detail against the FIRST one's start.
//
// AND THE KIND ALONE IS STILL THE ALARM'S IDENTITY everywhere else: the alarm
// is one per kind, the gauge one series per kind and [Tracker.Standing] one
// entry per kind, so an instance is a log line's key and nothing more. A
// node-wide kind is one instance with no log, even where it lists logs of its
// own: `backup_age` names the logs it holds past their replay window, and those
// are one problem with one remedy rather than a line each.
type instance struct {
	kind   Kind
	domain string
}

// firing is one instance currently up.
type firing struct {
	since  time.Time
	detail string
}

// instancesOf is an alarm's instances, each with the detail its lines carry:
// one per log a per-log kind stands on, carrying that log's own measurement,
// and otherwise one with no log, carrying the alarm's.
func instancesOf(a Alarm) ([]instance, []string) {
	if !perLogKind(a.Kind) || len(a.Domains) == 0 {
		return []instance{{kind: a.Kind}}, []string{a.Detail}
	}
	keys := make([]instance, 0, len(a.Domains))
	details := make([]string, 0, len(a.Domains))
	for _, d := range a.Domains {
		keys = append(keys, instance{kind: a.Kind, domain: d.Domain})
		details = append(details, d.Detail)
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

// OnTransition makes the tracker hand fn every transition it makes from now
// on, each stamped as node's: an instance fired, cleared, or found up by the
// tracker's first evaluation ([TransitionFiringAtBoot]) — one per instance, as
// the log lines are, in the order the evaluation made them.
//
// fn IS CALLED UNDER THE TRACKER'S LOCK, so two evaluations that race (the
// heartbeat and the trim tick) hand their transitions on in the order the
// tracker made them; it must not block and must not call the tracker. Set it
// before the first evaluation, or what that evaluation finds up is told to the
// log alone.
func (t *Tracker) OnTransition(node string, fn func(AlarmTransition)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.node, t.handOn = node, fn
}

// transition is one instance's change, as [Tracker.OnTransition] hands it on.
func (t *Tracker) transition(key instance, change TransitionChange, detail string,
	since, at time.Time) AlarmTransition {

	return AlarmTransition{
		ID:   TransitionID(t.node, key.kind, key.domain, change, since),
		Node: t.node, Kind: key.kind, Domain: key.domain, Change: change, Detail: detail,
		Since: since, At: at,
	}
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

	// THE FIRST EVALUATION'S FINDS were up before the tracker looked, for
	// all it can say: handed on as found at boot, never as fired.
	raise := TransitionFired
	if !t.observed {
		raise = TransitionFiringAtBoot
	}
	var made []AlarmTransition
	t.observed = true
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
		if t.handOn != nil {
			made = append(made, t.transition(r.key, raise, r.detail, now, now))
		}
	}
	for _, key := range slices.SortedFunc(maps.Keys(t.firing), compareInstances) {
		if live[key] {
			continue
		}
		was := t.firing[key]
		delete(t.firing, key)
		t.logger.WarnContext(ctx, "alarm_cleared", key.attrs(
			"for", spoken(now.Sub(was.since)), "detail", was.detail)...)
		if t.handOn != nil {
			made = append(made, t.transition(key, TransitionCleared, was.detail, was.since, now))
		}
	}
	for _, tr := range made {
		t.handOn(tr)
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
// problems an operator has to look at, and one cause standing on four logs is
// one of them, as the gauge beside it says. A per-log kind has stood since its
// longest-standing log's instance was raised.
//
// FALSE IS NOT "NONE FIRING": a node whose alarm table has not been evaluated
// yet (mid-boot, or running no state log at all) cannot say, and an empty
// list here would tell a health card it is healthy.
func (t *Tracker) Standing() ([]Kind, bool) {
	kinds, _, observed := t.standing()
	return kinds, observed
}

// Marks is [Tracker.Standing] with each kind's start: the kinds the latest
// evaluation found firing, one mark per kind, the longest-standing first, each
// dated by the instant it was raised — its oldest instance's, for a kind
// standing on several logs — and false before any evaluation has run.
//
// For the fifth surface, the node's presence heartbeat
// ([coord.NodeStatus.Alarms]), which every peer reads, so that "what is wrong
// on that node" is answered by the evaluation the node's own gauge, log lines
// and health count were fed rather than by a second one. ONE COMPUTATION
// BEHIND BOTH, so the heartbeat cannot name a different set, or a different
// longest-standing alarm, from the health card of the same node.
func (t *Tracker) Marks() ([]coord.AlarmMark, bool) {
	kinds, since, observed := t.standing()
	if !observed {
		return nil, false
	}
	out := make([]coord.AlarmMark, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, coord.AlarmMark{Kind: string(kind), Since: since[kind]})
	}
	return out, true
}

// standing is the latest evaluation's firing kinds, the longest-standing
// first, with the instant each has stood since, and whether any evaluation has
// run.
//
// A kind has stood since its OLDEST instance was raised: one cause standing on
// four logs is one alarm, and a log joining it does not make it newer.
func (t *Tracker) standing() ([]Kind, map[Kind]time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.observed {
		return nil, nil, false
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
	return out, since, true
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
