package statelog

import (
	"cmp"
	"context"
	"database/sql"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// alarmGauge is the instrument every firing alarm sets.
const alarmGauge = metrics.AlarmActive

// Tracker turns successive evaluations into a log line per transition, a
// gauge an operator's collector scrapes, and an [AlarmTransition] handed on
// ([Tracker.OnTransition]; package doc, "A node's alarm transition is the
// record its wake derives from"). Lines and wakes follow transitions, not the
// level: a level re-said every [AlarmInterval] cannot say when it STARTED. The
// gauge is a level, one series per kind, 1 while the alarm holds on any log
// and 0 when it clears, never deleted: a vanished series reads as a node that
// stopped reporting. An instance raised again within [RelapseWindow] of
// clearing is a new raise aged from its run's first.
//
// Safe for concurrent use: the heartbeat and the trim tick evaluate on
// different goroutines, and the health envelope reads [Tracker.Standing].
type Tracker struct {
	rec *metrics.Recorder
	now func() time.Time

	// logger takes the transition lines: the package's own outside a test.
	logger *slog.Logger

	mu     sync.Mutex
	firing map[instance]firing
	// lapsed is every instance cleared within the last [RelapseWindow], with
	// its run's first raise, which a raise inside the window resumes.
	lapsed map[instance]lapse

	// node and handOn are who is told of every transition, and as which
	// node ([Tracker.OnTransition]); a nil handOn tells nobody.
	node   string
	handOn func(AlarmTransition)

	// order is the kinds the latest evaluation reported, in the table's
	// order, and observed whether any evaluation has run; both for
	// [Tracker.Standing].
	order    []Kind
	observed bool

	// latest is the latest evaluation's alarms, for [Tracker.Latest].
	latest []Alarm

	// kept keeps the instances up, and the lapses, across a restart; nil
	// keeps them in memory alone.
	kept *store.DB
}

// instance is what the tracker writes ONE PAIR OF LOG LINES for: a kind, plus
// the log it stands on for a per-log kind ([PerLogKinds]) or the configured
// entry (a model entry or MCP server) for a per-entry one ([PerEntryKinds]).
// Keyed on the kind alone, a second log or entry joining a kind already up, or
// one clearing while another stood, said nothing, and the clear carried the
// last log's detail against the first one's start. Everywhere else the kind is
// the alarm's identity (the gauge, [Tracker.Standing]): an instance keys log
// lines and nothing more.
type instance struct {
	kind   Kind
	domain string
	entry  string
}

// firing is one instance up. told is whether whoever is handed transitions
// took its raise: only a told instance is kept across a restart. run is its
// run's first raise (since, or a raise cleared less than [RelapseWindow]
// before it), which every age reads.
type firing struct {
	since  time.Time
	run    time.Time
	detail string
	told   bool
}

// lapse is an instance that cleared at an instant, and the first raise of the
// run it ended.
type lapse struct {
	run, cleared time.Time
}

// instancesOf is an alarm's instances, each with the detail its lines carry:
// one per configured entry or log the alarm lists, with that one's own
// measurement, else one with no log carrying the alarm's. The lists decide,
// with no second check of the kind: only a per-entry kind lists entries
// ([Evaluate] alone fills [Alarm.Entries]) and only a per-log kind lists logs
// (the report's test holds both directions). An [AlarmDomain] naming several
// logs is an instance per log.
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

// NewTracker builds a tracker. A nil recorder records nothing and still logs:
// the alarm is a fact about the company, not about the collector.
func NewTracker(rec *metrics.Recorder, now func() time.Time) *Tracker {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Tracker{rec: rec, now: now, logger: log, firing: map[instance]firing{},
		lapsed: map[instance]lapse{}}
}

// Remember makes the tracker keep the instances it has up in db, this node's
// own file, and read them back at its first evaluation, so after a restart
// only a change is a transition: one that cleared while the node was down
// clears with its original instant. Set it before then.
func (t *Tracker) Remember(db *store.DB) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.kept = db
}

// OnTransition makes the tracker hand fn every transition from now on,
// stamped as node's, one per instance in the order the evaluation made them.
//
// fn IS CALLED UNDER THE TRACKER'S LOCK, so racing evaluations (the heartbeat,
// the trim tick) hand on in order; it must not block or call the tracker. Set
// it before the first evaluation, or what that one finds up goes to the log
// alone.
func (t *Tracker) OnTransition(node string, fn func(AlarmTransition)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.node, t.handOn = node, fn
}

// transition is one instance's change, as [Tracker.OnTransition] hands it on.
func (t *Tracker) transition(key instance, change TransitionChange, detail string,
	since, at time.Time) AlarmTransition {

	return AlarmTransition{
		ID:   TransitionID(t.node, key.kind, key.domain, key.entry, change, since),
		Node: t.node, Kind: key.kind, Domain: key.domain, Entry: key.entry, Change: change, Detail: detail,
		Since: since, At: at,
	}
}

// Observe records one evaluation and returns the alarms up, in the table's
// order. Each [instance] is raised and cleared on its own in the log.
//
// EVERY KIND IS WRITTEN TO THE GAUGE on every observation, firing or not:
// writing only the firing ones leaves a cleared alarm at 1 for whoever reads
// the metric. The gauge stays one series per kind; a series per log is a label
// kept for every log a deployment ever ran, for what the log line names.
//
// AN INSTANCE THE EVALUATION COULD NOT JUDGE ([Unjudged]) IS KEPT AS IT
// STANDS, its start unmoved, so a source a boot or a stop has not read yet
// does not clear a condition that still holds.
func (t *Tracker) Observe(ctx context.Context, alarms []Alarm, unjudged ...AlarmRef) []Alarm {
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
				// One log listed twice is one instance; the first
				// listing's detail stands.
				continue
			}
			live[key] = true
			up = append(up, raised{key: key, detail: details[i], remedy: a.Remedy})
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// THE FIRST EVALUATION READS BACK WHAT STOOD BEFORE A RESTART, so only
	// a change is a transition.
	if !t.observed {
		t.recall(ctx)
	}
	var made []AlarmTransition
	moved := false
	t.observed = true
	t.latest = slices.Clone(alarms)
	t.order = t.order[:0]
	for _, a := range alarms {
		if !slices.Contains(t.order, a.Kind) {
			t.order = append(t.order, a.Kind)
		}
	}
	for key, l := range t.lapsed {
		if now.Sub(l.cleared) >= RelapseWindow {
			delete(t.lapsed, key)
		}
	}
	for _, r := range up {
		if was, already := t.firing[r.key]; already {
			// Still up: a moved detail (a lag grows) is not a transition.
			was.detail = r.detail
			t.firing[r.key] = was
			continue
		}
		run, attrs := now, []any{"detail", r.detail, "remedy", r.remedy}
		if l, relapsed := t.lapsed[r.key]; relapsed {
			// RAISED AGAIN INSIDE THE WINDOW: one run, aged from its first
			// raise, so a flapping condition still reaches a person.
			run = l.run
			delete(t.lapsed, r.key)
			attrs = append(attrs, "first_raised", run)
		}
		t.firing[r.key] = firing{since: now, run: run, detail: r.detail, told: t.handOn == nil}
		moved = true
		t.logger.WarnContext(ctx, "alarm_raised", r.key.attrs(attrs...)...)
		if t.handOn != nil {
			made = append(made, t.transition(r.key, TransitionFired, r.detail, now, now))
		}
	}
	for _, key := range slices.SortedFunc(maps.Keys(t.firing), compareInstances) {
		if live[key] {
			continue
		}
		if judgedNot(key, unjudged) {
			kinds[key.kind] = true
			if !slices.Contains(t.order, key.kind) {
				t.order = append(t.order, key.kind)
			}
			continue
		}
		was := t.firing[key]
		delete(t.firing, key)
		t.lapsed[key] = lapse{run: was.run, cleared: now}
		moved = true
		t.logger.WarnContext(ctx, "alarm_cleared", key.attrs(
			"for", spoken(now.Sub(was.since)), "detail", was.detail)...)
		if t.handOn != nil {
			made = append(made, t.transition(key, TransitionCleared, was.detail, was.since, now))
		}
	}
	for _, tr := range made {
		t.handOn(tr)
	}
	if moved {
		t.keep(ctx)
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

// judgedNot reports whether key is among the instances an evaluation could not
// judge.
func judgedNot(key instance, unjudged []AlarmRef) bool {
	return slices.ContainsFunc(unjudged, func(r AlarmRef) bool {
		return r.Kind == key.kind && (r.Entry == "" || r.Entry == key.entry)
	})
}

// recall reads back the instances that stood when this node last kept them,
// and those cleared within the relapse window. An unreadable file recalls
// nothing, said once, so every standing instance is raised again.
func (t *Tracker) recall(ctx context.Context) {
	if t.kept == nil {
		return
	}
	err := t.kept.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT kind, domain, entry, since, run, cleared, detail FROM alarm_standing`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key instance
			var kind string
			var since int64
			var run, cleared sql.NullInt64
			var was firing
			if err := rows.Scan(&kind, &key.domain, &key.entry, &since, &run, &cleared, &was.detail); err != nil {
				return err
			}
			key.kind, was.since, was.run, was.told = Kind(kind), store.DecodeTime(since), store.DecodeTime(since), true
			if run.Valid {
				was.run = store.DecodeTime(run.Int64)
			}
			if cleared.Valid {
				t.lapsed[key] = lapse{run: was.run, cleared: store.DecodeTime(cleared.Int64)}
				continue
			}
			t.firing[key] = was
		}
		return rows.Err()
	})
	if err != nil {
		t.logger.WarnContext(ctx, "alarm_standing_unread", "error", err.Error(),
			"detail", "the alarms that stood before this node started could not be read, "+
				"so every alarm up now is raised again")
	}
}

// Told records that the raise with transition id was taken by whoever is told
// of transitions, so its instance is kept across a restart from now on.
func (t *Tracker) Told(ctx context.Context, id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, was := range t.firing {
		if !was.told && TransitionID(t.node, key.kind, key.domain, key.entry, TransitionFired, was.since) == id {
			was.told = true
			t.firing[key] = was
			t.keep(ctx)
			return
		}
	}
}

// keep replaces what was kept with the told instances up now and those
// cleared within the relapse window. An untold raise is not kept, so a restart
// before it is taken raises it again.
func (t *Tracker) keep(ctx context.Context) {
	if t.kept == nil {
		return
	}
	err := t.kept.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM alarm_standing`); err != nil {
			return err
		}
		insert := func(key instance, since, run time.Time, cleared sql.NullInt64, detail string) error {
			var first sql.NullInt64
			if !run.Equal(since) {
				first = sql.NullInt64{Int64: store.EncodeTime(run), Valid: true}
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO alarm_standing
				(kind, domain, entry, since, run, cleared, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				string(key.kind), key.domain, key.entry, store.EncodeTime(since), first, cleared, detail)
			return err
		}
		for key, was := range t.firing {
			if !was.told {
				continue
			}
			if err := insert(key, was.since, was.run, sql.NullInt64{}, was.detail); err != nil {
				return err
			}
		}
		for key, l := range t.lapsed {
			if err := insert(key, l.run, l.run, sql.NullInt64{Int64: store.EncodeTime(l.cleared), Valid: true}, ""); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.logger.WarnContext(ctx, "alarm_standing_unkept", "error", err.Error(),
			"detail", "the alarms up now could not be kept, so a restart before the next "+
				"change raises the ones still up again")
	}
}

// attrs is a transition line's attributes: the alarm, its log or entry if
// any, and the rest.
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

// compareInstances orders instances by kind, log and entry, so one
// observation's clears are logged in a stable order.
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
// nil before any evaluation has run. The Models page reads its model kinds
// here rather than judging the thresholds again, so it cannot disagree with
// the health count, the gauge and the log lines.
func (t *Tracker) Latest() []Alarm {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.latest)
}

// Standing reports the kinds the latest evaluation found firing, the
// LONGEST-STANDING first, and false before any evaluation has run.
//
// It feeds the health envelope, which carries a count and ONE name: the
// condition unanswered longest. Not the table's order, which ranks nothing; it
// only breaks a tie among alarms raised by one evaluation. ONE ENTRY PER KIND
// however many logs it stands on, as the gauge counts.
//
// FALSE IS NOT "NONE FIRING": before the table is evaluated (mid-boot, or no
// state log), an empty list would tell a health card it is healthy.
func (t *Tracker) Standing() ([]Kind, bool) {
	kinds, _, observed := t.standing()
	return kinds, observed
}

// Marks is [Tracker.Standing] with each kind's start (its oldest instance's
// run's first raise), for the node's presence heartbeat
// ([coord.NodeStatus.Alarms]) every peer reads. ONE COMPUTATION BEHIND BOTH,
// so the heartbeat cannot name a different set, or a different
// longest-standing alarm, from the same node's health card.
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
// first, with each one's start, and whether any evaluation has run. A kind
// has stood since its OLDEST instance's run began: neither a log joining it
// nor a raise inside the relapse window makes it newer.
func (t *Tracker) standing() ([]Kind, map[Kind]time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.observed {
		return nil, nil, false
	}
	since := make(map[Kind]time.Time, len(t.order))
	for key, f := range t.firing {
		if at, seen := since[key.kind]; !seen || f.run.Before(at) {
			since[key.kind] = f.run
		}
	}
	out := append(make([]Kind, 0, len(t.order)), t.order...)
	slices.SortStableFunc(out, func(a, b Kind) int {
		return since[a].Compare(since[b])
	})
	return out, since, true
}

// Firing reports how long each kind up has been up: its longest instance's
// run. The age tells "started four minutes ago" from "started on Tuesday",
// which the alarm itself does not carry.
func (t *Tracker) Firing() map[Kind]time.Duration {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[Kind]time.Duration, len(t.firing))
	for key, f := range t.firing {
		out[key.kind] = max(out[key.kind], now.Sub(f.run))
	}
	return out
}
