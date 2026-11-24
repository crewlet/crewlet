package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE TRIM: without it the log only grows. Each domain's log is a replay window,
// not an archive; once every node the fleet counts has committed past a record
// and nothing else needs it, the record goes. [statelog.Trim] is the pure
// arithmetic over six terms that each move the point DOWN; this file fills that
// struct honestly (a term that "could not be read" blocks the trim), publishes
// its conclusion so every write fence and node can say why the log is not
// shrinking, and only THEN purges the broker ([retention.apply]). On a log with
// arbitration anchors it last appends the records that have every node drop
// anchors below what the log still holds (ADR-0080, [retention.sweepAnchors]).
// A fleet singleton on the `worker:{duty}` lease: two trimmers would overwrite
// each other's floor and reset `blocked_since`. ONLY A DATA NODE ARMS IT
// ([Engine.startRetention]): it reads the estate's eviction rows.

// The trim ticks every [statelog.TrimInterval]. The number lives beside the
// arithmetic rather than here because it is also the resolution of
// `blocked_since`, which the `trim_blocked` alarm reads — see its definition
// for why fifteen minutes.

// retentionDutyName is the fleet singleton the trim claims.
const retentionDutyName = "retention"

// retentionDutyTTL is how long the duty survives without a re-claim.
//
// Three ticks, the ratio every other duty here uses: one missed tick must not
// hand the trim to a peer, because two nodes purging and publishing at once is
// exactly what the singleton avoids.
const retentionDutyTTL = 3 * statelog.TrimInterval

// retention is the trim loop.
type retention struct {
	fleet coord.Fleet
	state *stateLog

	// holders is the live data nodes, the half of every log's counted set
	// that is not the positions register ([liveData]). Nil counts nobody
	// beyond the register.
	holders liveData

	db  *store.DB
	cfg config.TrackerRetention

	// estate is THIS node's replicated copy, resolved per call because an
	// adoption swaps the file — what the anchor sweep counts the anchors
	// below the log from ([retention.sweepAnchors]). Nil counts none and
	// sweeps nothing, the conservative direction: an anchor kept costs its
	// next write one refused append, never a lost update.
	estate func() statelog.ReadEstate

	// claim answers whether this node holds the trim's duty this tick
	// ([Engine.workerDuty]). Nil answers yes — a node with no coordination
	// store, which is nobody's singleton.
	claim schedule.DutyFunc

	// giveBack gives the duty back where this node holds it, on a tick that
	// declines it ([Engine.workerGiveBack]). Nil holds no lease to give.
	giveBack func(context.Context) (bool, error)

	// copies names the first of the domains given whose copy on this node is
	// out of service ([Engine.copyOutOfService]), which the trim declines its
	// duty for ([retention.decidesFrom]); declines remembers whether it is.
	// Nil judges no copy — every one serves.
	copies   func(ctx context.Context, domains ...string) (string, bool)
	declines dutyDecline

	// judge judges an open window's retired participants as the fleet now
	// stands ([Engine.judgeWindow]). Nil judges none, so each is reported as
	// owing what any participant owes: the conservative direction.
	judge func(context.Context, coord.MaintenanceOperation) (coord.MaintenanceOperation, error)

	nodeID string

	// backupOwner is `retention.backup_owner` — read here rather than
	// looked up beside the report, because the one place it matters is the
	// backup term's remedy and a remedy naming a value the document does
	// not carry is one a JSON reader never sees.
	backupOwner string

	// metrics is the process's one recorder, for the alarm inputs that are
	// percentiles rather than states. Nil records nothing, which every
	// condition reads as "nothing to report".
	metrics *metrics.Recorder

	// objectives is the node's service objective meter, whose burning
	// objectives the `objective_burn` alarm reads. Nil (a test's retention)
	// reads as nothing burning.
	objectives *objectives.Meter

	// volumes identifies the volume a directory is on and measures what it
	// has free ([measureVolume], nil); a parameter for the tests.
	volumes func(dir string) (volume string, free int64, err error)

	// alarms turns each evaluation into the two surfaces that are not a
	// screen — the `crewlet.alarm.active{kind}` gauge a collector scrapes,
	// and one WARN on entry and one on exit.
	//
	// WITHOUT IT THE TABLE HAD ONE SURFACE OF THREE: `work_retention`
	// rendered the alarms to whoever asked, and nothing at all reached a
	// collector or a log. An alarm nobody is looking at a dashboard for is
	// an alarm that fires into an empty room, which is indistinguishable
	// from a fleet with nothing wrong.
	alarms *statelog.Tracker

	// wakes publishes every transition alarms makes, for the seats whose
	// role carries the health grant to be woken by (ADR-0095); nil on a
	// node with no broker to publish to.
	wakes *alarmOutbox

	// episodes keeps every transition alarms makes as an episode on the
	// advisor log, for the Advisor's alarm classes (alarmepisodes.go).
	episodes *alarmEpisodes

	// coverage is the fraction of this node's sources carrying a current
	// vector, or false when this company has no embeddings configured.
	// Nil on a node that cannot measure it, which reads as "nothing to
	// report" rather than as no coverage.
	coverage func(context.Context) (float64, bool, error)

	// index fills the semantic index's half of a reading: the recall its
	// last training measured and the floor for the size it measured at.
	// Nil on an engine that holds no vectors, which reads as nothing to
	// report.
	index func(context.Context, *statelog.Reading)

	// objects fills the object store's half of a reading — its health and
	// what its passes found. Nil on an engine built without one, which
	// reads as nothing to report.
	objects func(now time.Time, out *statelog.Reading)

	// code fills Gitlet's half of a reading at now — what this
	// node's committer refused busy for a backlog, and what its git cache
	// and its compactor found ([Engine.codeReading]). Nil reads as nothing
	// to report, which is also what it fills on a node holding neither a
	// committer nor a cache.
	code func(ctx context.Context, now time.Time, out *statelog.Reading)

	// refs reads code_refs_near_limit's figure from this node's code log
	// rows at now, and false where it measures none ([Engine.refsNearLimit]).
	// Measured on the trim's tick alone ([retention.measureRefs]) and read
	// back by every report. Nil reads as nothing to report.
	refs func(ctx context.Context, now time.Time) (gitlet.RefsNear, bool, error)
	// entries fills the per-entry alarms' half of a reading: each model
	// entry's and each MCP server's condition across the fleet. Nil reads
	// as nothing to report.
	entries func(ctx context.Context, now time.Time, out *statelog.Reading)

	// feeds fills the change feeds' half of a reading: every feed this
	// node started that is down ([native.feedsReading]). Nil reads as
	// nothing to report.
	feeds func(now time.Time, out *statelog.Reading)

	// executors fills the executor runtime's half of a reading: the runs of
	// seats whose resume waits for an operator, across the fleet
	// ([Engine.executorReading]). Nil reads as nothing to report.
	executors func(ctx context.Context, now time.Time, out *statelog.Reading)

	// feedSources is the wake feed each log declares, by domain — the
	// source whose dead-letter subject its row reads — and feedLetters the
	// watch that reads them on every report (feeddeadletters.go). A log
	// with no entry declares no feed; a nil watch reads none.
	feedSources map[string]changefeed.Source
	feedLetters *deadLetterWatch

	// seats is the running company's agent-seat count, which the census a
	// log's read rate is held against scales with
	// ([statelog.Census]). READ AT EVERY EVALUATION, because a
	// revision that hires or retires seats moves it and the trim is not
	// rebuilt for one. Nil counts no seat, which the expectation reads as
	// the one seat it never goes below.
	seats func() int

	// background is the barrier records a day the engine's OWN periodic
	// reads put on the domain's log, whatever the seats do
	// ([statelog.Census.Background], [Engine.backgroundBarriers]). Read at
	// every evaluation too, because the data nodes that make them join and
	// leave. Nil puts none.
	background func(domain string) int

	// The third term of the census, the readers a log's own sizing declares
	// ([clientBarriers]), is not a field: it is the log's, never this node's
	// or the moment's, so there is nothing to read at an evaluation.

	// mu guards the coverage cache, the refs figure and the log rates below.
	// The tick and every API request assemble a report, on different
	// goroutines.
	mu sync.Mutex

	// coverFraction and coverKnown are the last tick's coverage measurement,
	// TAKEN ON THE TICK AND READ BY THE REPORT: it scans the whole source corpus,
	// and a report is assembled on every operator request, dashboard poll and
	// alarm beat. It, each log's daily intake and the store's pool waits are the
	// ONLY alarm inputs measured at [statelog.TrimInterval]; none summarises
	// anything faster than a corpus, a day or a tick. Every other input is
	// re-read on each [statelog.AlarmInterval] beat. See
	// [retention.measureCoverage] and [retention.heartbeat].
	coverFraction float64
	coverKnown    bool

	// refsNear and refsNearKnown are the last tick's code_refs_near_limit
	// figure, taken on the tick and read by the report for the coverage's
	// reason: a walk of the repositories and a count over each one's refs
	// near the bound, which no report may repeat. See [retention.measureRefs].
	refsNear      gitlet.RefsNear
	refsNearKnown bool

	// pooled is the last `sql.DBStats` wait counters seen per store file,
	// so the histogram is fed the DELTA rather than the process's
	// cumulative total. Keyed on the file, which is the attribute.
	pooled map[string]poolCounters

	// rates is each strict log's intake over the trailing day, as the last
	// tick measured it — what `log_ceiling_short` holds a ceiling against.
	// Guarded by mu, for the coverage cache's reason. See lograte.go.
	rates map[string]*uint64

	// floors follows how long this node has been unable to read the trim
	// floor, for `floor_unknown`. See floorwatch.go.
	floors *floorWatch

	// windows is what this node last read of each log's capacity window,
	// which `maintenance_open` judges a log it can't read now on. Nil
	// remembers nothing. See capacitywindows.go.
	windows *windowWatch

	// now is the clock every evaluation, every report, every trim tick, the
	// backup gauge and the alarm tracker's ages read ([retention.clock]) —
	// the duty's one clock, but for [retention.watch]'s. Nil reads the wall
	// clock; a case sets it to put a log's first days, or an eviction's fence
	// window, behind it.
	now func() time.Time

	// watch is what the trim has watched of each hold, judged on its own
	// monotonic clock and never on now ([statelog.HoldWatch]). Only read(),
	// on the run goroutine, uses it. Kept across a lost tenure: a sighting
	// from an earlier one still bounds the holder's silence from below.
	watch statelog.HoldWatch

	// beating serialises one alarm evaluation against another: the
	// heartbeat and the trim tick both evaluate, and two interleaved would
	// fold an earlier beat's look at the floors over a later one's and hand
	// the tracker an older reading after a newer one.
	beating sync.Mutex

	// logger is where a block's transitions are said; nil is the package's
	// own logger. See [retention.sayBlock].
	logger *slog.Logger

	stop context.CancelFunc
	done sync.WaitGroup
}

// poolCounters is one store file's cumulative connection-wait counters.
type poolCounters struct {
	count  int64
	waited time.Duration
}

// startRetention arms the trim.
//
// Started with the state log rather than beside the sweep, because it is the
// state log's own gate: it has no meaning on a node running no domains, and a
// loop that ran there would publish a floor derived from streams it does not
// have.
func (e *Engine) startRetention(ctx context.Context, boot *config.Bootstrap, s *stateLog) {
	if s == nil || len(s.running()) == 0 || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	r := &retention{
		fleet:       e.backends.Fleet,
		holders:     e.holdersOf(),
		state:       s,
		db:          e.backends.Store,
		estate:      func() statelog.ReadEstate { return s.estate().Reader() },
		cfg:         boot.Stream.TrackerRetention,
		backupOwner: boot.Retention.BackupOwner,
		metrics:     e.metrics,
		objectives:  e.objectives,
		claim:       e.workerDuty(retentionDutyName, retentionDutyTTL),
		giveBack:    e.workerGiveBack(retentionDutyName),
		copies:      e.copyOutOfService,
		judge:       e.judgeWindow,
		nodeID:      s.nodeID,
		coverage:    e.vectorCoverage,
		index:       e.indexReading,
		objects:     e.objectsReading,
		code:        e.codeReading,
		refs:        e.refsNearLimit,
		entries:     e.entriesReading,
		feeds:       e.feedsReading,
		executors:   e.executorReading,
		feedSources: e.feedSources(s.running()),
		feedLetters: newDeadLetterWatch(e.deadLetterLedger, e.backends.Fleet, e.metrics),
		seats:       func() int { return seatCount(e.Company()) },
		background:  e.backgroundBarriers,
		pooled:      map[string]poolCounters{},
		floors: newFloorWatch(func(ctx context.Context, domains []string) publishedFloors {
			return readFloorsOf(ctx, e.backends.Fleet, domains)
		}),
		windows: newWindowWatch(),
	}
	// ON THE DUTY'S OWN CLOCK, which every report and evaluation reads: an
	// alarm's age is the tracker's clock less the instant it was raised, and
	// a tracker on the wall clock aged an alarm the evaluations beside it
	// had raised on another.
	r.alarms = statelog.NewTracker(e.metrics, r.clock)
	// KEPT IN THE NODE'S OWN FILE, so a restart says only what changed.
	if e.backends.Store != nil {
		r.alarms.Remember(e.backends.Store)
	}
	// THE ALARMS' WAKES, told of every transition before the first
	// evaluation, so what that evaluation finds firing is handed on too
	// rather than said by the log alone — on the tracker just made, since a
	// transition is told by the tracker it happens on. A raise the broker
	// took is told back, which is what keeps it across a restart.
	var handOn []func(statelog.AlarmTransition)
	if e.backends.Queue != nil {
		r.wakes = newAlarmOutbox(e.backends.Queue, e.metrics)
		r.wakes.taken = r.alarms.Told
		handOn = append(handOn, r.wakes.Add)
	}
	// THE ADVISOR'S ALARM HISTORY, kept from the same transitions where they
	// are made: each instance's every fire and clear (alarmepisodes.go).
	r.episodes = newAlarmEpisodes(e.AdvisorWrites)
	if r.wakes == nil {
		r.episodes.taken = r.alarms.Told
	}
	handOn = append(handOn, r.episodes.Add)
	r.alarms.OnTransition(s.nodeID, func(t statelog.AlarmTransition) {
		for _, add := range handOn {
			add(t)
		}
	})
	// DETACHED from the caller's context, for the reason every other
	// long-running loop here is: a loop bound to a signal context stops at
	// SIGTERM, which would make its lifetime differ from the appliers it
	// is trimming behind for no reason a reader could find.
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	r.stop = stop
	e.retention.Store(r)
	// PUBLISHED FOR THE HEALTH ENVELOPE before either loop starts, so the
	// first probe after a boot reads the tracker the first beat will feed.
	e.alarms.Store(r.alarms)
	r.done.Add(2)
	go func() {
		defer r.done.Done()
		r.run(loop)
	}()
	go func() {
		defer r.done.Done()
		r.heartbeat(loop)
	}()
	if r.wakes != nil {
		// ENDED WITH THE TRIM AND THE HEARTBEAT, by the same cancel and
		// the same wait: a transition still queued when the node stops is
		// not recovered, and the next start's first evaluation says what
		// is still firing.
		r.done.Go(func() { r.wakes.run(loop) })
	}
	// ENDED THE SAME WAY: an episode still queued at stop is lost, and the
	// next start's first evaluation raises what still stands.
	r.done.Go(func() { r.episodes.run(loop) })
}

// stopRetention ends the trim and the alarm heartbeat, waiting for an
// in-flight tick of either.
func (e *Engine) stopRetention() {
	r := e.retention.Swap(nil)
	if r == nil {
		return
	}
	r.stop()
	r.done.Wait()
}

// clock is [retention.now], or the wall clock.
func (r *retention) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now().UTC()
}

// RetentionReport answers "is the log being trimmed, and what is stopping it"
// from outside the package — the question `crewlet retention status` exists
// for. The bool is false on a node that armed no trim — a maintenance-mode
// node, which publishes nothing, its trim included — and that is a real
// deployment and NOT a report of zeros: a document full of zeros would claim a
// fleet whose log is perfectly trimmed.
func (e *Engine) RetentionReport(ctx context.Context) (statelog.Report, bool) {
	r := e.retention.Load()
	if r == nil {
		return statelog.Report{}, false
	}
	return r.Report(ctx), true
}

// Alarms is the kinds this node's latest evaluation found standing, one per
// kind however many logs it stands on and the longest-standing first
// ([statelog.Tracker.Standing]), and false where no evaluation has run: a node
// mid-boot, or one running no state log, which has no alarm table to evaluate.
//
// THE EVALUATION THE GAUGE AND THE LOG WERE FED, never a fresh report. The
// health envelope reads this on every probe and every push tick, and a report
// assembled there would be a second evaluation per caller — the cost scaling
// with how many tabs are open, and the count on the screen able to disagree
// with the alarm line the log just wrote.
func (e *Engine) Alarms() ([]statelog.Kind, bool) {
	tracker := e.alarms.Load()
	if tracker == nil {
		return nil, false
	}
	return tracker.Standing()
}

// run ticks until the context ends.
//
// IT TICKS IMMEDIATELY, which matters more here than the usual reason: the
// published floor is what every other surface reads a blocked trim from, and a
// fleet that had just started would otherwise answer "no floor published" for
// fifteen minutes — indistinguishable from a fleet whose duty is not running.
//
// ONLY THE TRIM, and the anchor sweeps that follow it ([retention.sweepAnchors]).
// The alarm table is evaluated on its own faster beat as well
// ([retention.heartbeat]), on a goroutine of its own, so neither a purge nor a
// sweep paced on this node's applier holds an evaluation up; [retention.beating]
// keeps the two evaluations from interleaving.
func (r *retention) run(ctx context.Context) {
	ticker := time.NewTicker(statelog.TrimInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			// CHECKED BEFORE THE TICK, not only after: a loop started
			// under a context that is already done would otherwise
			// take one tick on the way out and log a duty claim
			// failing during a shutdown that is going fine.
			return
		}
		r.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick evaluates every log once, if this node holds the duty.
func (r *retention) tick(ctx context.Context) {
	// FIRST, AND ON EVERY NODE — before the duty claim, deliberately.
	//
	// The trim is a fleet singleton because two nodes purging one log is
	// waste; the ALARMS are the opposite. [statelog.Reading] describes ONE
	// node — its own applier's lag, its own disk, its own refusals — so a
	// table evaluated only where the duty happens to sit would report the
	// duty holder's health as the fleet's, and the wedged node would be the
	// one nobody hears from. It is also what makes this loop useful on a
	// node that never wins the lease at all.
	r.evaluate(ctx)
	// THEN THE DUTY IS DECLINED, before it is claimed, while this node's
	// copy of a log whose evictions the trim counts is out of service — and
	// given back, if this node holds it ([dutyDecline]).
	if _, out := r.declines.check(ctx, retentionDecline, r.copies, r.decidesFrom()); out {
		r.declines.giveBack(ctx, retentionDecline, r.giveBack)
		return
	}
	if r.claim != nil {
		mine, err := r.claim(ctx)
		if err != nil {
			log.WarnContext(ctx, "retention_duty_unclaimed", "err", err)
			return
		}
		if !mine {
			return
		}
	}
	// ONE READ FOR EVERY LOG THIS TICK EVALUATES — see [retention.read].
	shared, err := r.read(ctx)
	if err != nil {
		// EVERY TERM DERIVED FROM THE REGISTER IS UNKNOWN, which blocks
		// — so nothing is purged and the reason is said once rather
		// than once per domain.
		// ITS OWN EVENT NAME, not the per-domain one: a blocked-trim
		// line carries a domain and a term, and one with neither would
		// be counted by anything filtering on that name as a domain
		// blocked by a term called "register".
		log.WarnContext(ctx, "retention_inputs_unreadable", "err", err)
		return
	}
	var sweeps []sweepTarget
	for _, running := range r.state.running() {
		below, err := r.domain(ctx, running, shared)
		if err != nil {
			log.WarnContext(ctx, "retention_trim_failed", "domain", running.domain.Name(), "err", err)
			continue
		}
		sweeps = append(sweeps, sweepTarget{running: running, below: below})
	}
	// AND THEN THE ANCHORS BELOW WHAT EACH LOG NO LONGER HOLDS — ADR-0080 —
	// once every log's purge has landed, so the sweeps share their one
	// interval among the logs that have anything to sweep.
	r.sweepAnchors(ctx, sweeps)
	// AND THE BACKUP POINTS OF THE NODES THE FLEET HAS STOPPED COUNTING.
	r.forgetEvicted(ctx, shared)
}

// forgetEvicted removes the backup point of every node evicted on every log that
// counts nodes, past the fence window, so neither a log's backup term nor
// `GET /backups` counts a copy on a host the operator says is gone.
//
// The position row stays ([coord.PositionRegister], what a READMISSION is judged
// by) but the backup point goes: left standing, `backup_floor: engine` would
// trim a log against a backup nobody can restore from. Forgetting is safe: a
// log no surviving copy covers blocks and raises `backup_age`, and a readmitted
// node records its next backup afresh. Only when EVERY IDENTITY-CLAIMING LOG
// has evicted it, PAST [statelog.EvictionFenceWindow]; a log whose evictions
// could not be read forgets nobody. Operator acknowledgements are never
// forgotten. A failed forget is retried next tick.
func (r *retention) forgetEvicted(ctx context.Context, shared fleetInputs) {
	var logs []logEvictions
	for _, running := range r.state.running() {
		if !running.domain.ClaimsIdentity() {
			continue
		}
		tombs, read := r.tombstones(ctx, running, running.runner.Committed().Generation)
		logs = append(logs, logEvictions{tombs: tombs, read: read})
	}
	for _, point := range evictedPoints(shared.at, logs, shared.backups) {
		if err := r.fleet.ForgetBackupPoint(ctx, point.Owner); err != nil {
			log.WarnContext(ctx, "retention_backup_point_unforgotten",
				"node", point.Owner, "err", err)
			continue
		}
		log.InfoContext(ctx, "retention_backup_point_forgotten",
			"node", point.Owner, "taken_at", point.At, "dir", point.Dir)
	}
}

// logEvictions is one identity-claiming log's evictions as this node applied
// them, and whether they could be read at all.
type logEvictions struct {
	tombs []statelog.Tombstone
	read  bool
}

// evictedPoints is the backup points [retention.forgetEvicted] forgets: every
// node's whose owner a tombstone older than [statelog.EvictionFenceWindow]
// uncounts on EVERY log in logs — one entry per log that counts nodes — and
// none at all when a log's evictions were not read or no log counts nodes. An
// operator acknowledgement is never a node's, whatever a node is called.
func evictedPoints(now time.Time, logs []logEvictions, points []coord.BackupPoint) []coord.BackupPoint {
	if len(logs) == 0 {
		return nil
	}
	uncounted := map[string]int{}
	for _, l := range logs {
		if !l.read {
			return nil
		}
		seen := map[string]bool{}
		for _, tomb := range l.tombs {
			if now.Sub(tomb.At) > statelog.EvictionFenceWindow && !seen[tomb.NodeID] {
				seen[tomb.NodeID] = true
				uncounted[tomb.NodeID]++
			}
		}
	}
	var out []coord.BackupPoint
	for _, point := range points {
		if point.Owner != coord.OperatorBackupOwner && uncounted[point.Owner] == len(logs) {
			out = append(out, point)
		}
	}
	return out
}

// decidesFrom is every domain whose rows the trim decides from, in the
// register's order: the logs that claim identity, whose evictions it counts
// from this node's own rows ([logTombstones]), and the logs that write
// arbitration anchors, whose anchors below the log it counts from this node's
// own rows ([retention.sweepAnchors]). Every other term is the fleet's register
// or the log itself. THE TWO SETS ARE NAMED APART though they are the same logs
// today: a log that writes anchors and claims no identity can be added, and a
// copy of it out of service would have the trim count anchors on untrusted rows.
func (r *retention) decidesFrom() []string {
	if r.state == nil {
		return nil
	}
	var out []string
	for _, running := range r.state.running() {
		if running.domain.ClaimsIdentity() || statelog.WritesAnchors(running.spec) {
			out = append(out, running.domain.Name())
		}
	}
	return out
}

// evaluate records what one trim tick can measure about this node's own
// hardware, its vector coverage and its logs, then evaluates the alarm table
// against it at once.
//
// THE MEASUREMENT COMES FIRST: the reading reads these back, so observing
// before measuring would evaluate the previous tick's disk against this tick's
// log. Vector coverage and the logs' intake are measured HERE only, each a scan
// a per-poll report must not repeat, honest at [statelog.TrimInterval]. Between
// ticks the table is evaluated by [retention.heartbeat].
func (r *retention) evaluate(ctx context.Context) {
	r.capacity(ctx)
	r.measureCoverage(ctx)
	r.measureRefs(ctx)
	r.measureRates(ctx, r.clock())
	r.beat(ctx)
}

// heartbeat evaluates the alarm table every [statelog.AlarmInterval], on every
// node, until the context ends.
//
// ITS OWN LOOP, beside the trim's rather than inside it, and it is what makes
// the table's thresholds true: every alarm there fires at a threshold of a
// minute or more that ANOTHER decision already made, and an evaluation paced
// by the trim's quarter-hour silently raised each of them to fifteen minutes.
// The expensive measurements stay on the trim's tick and are read back here;
// what a beat reads is what the report reads — one listing of each fleet
// fact and one state read per log — plus one observation of the trim floors,
// each log's read by its own key, all at once, as the read path reads them
// (see floorwatch.go).
func (r *retention) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(statelog.AlarmInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		r.beat(ctx)
	}
}

// beat is one alarm evaluation: the trim floor observed, and the table
// evaluated against everything the report reads.
func (r *retention) beat(ctx context.Context) {
	r.beating.Lock()
	defer r.beating.Unlock()
	r.floors.observe(ctx, r.clock(), r.floorSubjects())
	if r.alarms == nil {
		return
	}
	rep := r.Report(ctx)
	r.alarms.Observe(ctx, rep.Alarms, rep.Unjudged...)
}

// floorSubjects is every log this node runs, at the generation its applier is
// on — the generation a floor has to be published at for this node to use it.
func (r *retention) floorSubjects() []floorSubject {
	if r.state == nil {
		return nil
	}
	running := r.state.running()
	out := make([]floorSubject, 0, len(running))
	for _, l := range running {
		if l.runner == nil {
			continue
		}
		out = append(out, floorSubject{domain: l.domain.Name(),
			generation: l.runner.Committed().Generation})
	}
	return out
}

// fleetInputs is what one tick reads once and every log shares.
type fleetInputs struct {
	at        time.Time
	positions []coord.NodePositions
	readable  bool
	holds     []coord.TrimHold
	silent    map[string]time.Duration
	backups   []coord.BackupPoint
	previous  map[string]coord.TrimFloor

	// floorsUnread is every domain whose published floor could not be read,
	// and why — a domain whose trim this tick neither publishes nor purges,
	// since the floor it carries forward is the row it could not read.
	floorsUnread map[string]error

	// holders is the live data nodes, read once for every log the tick
	// evaluates — and holdersUnknown why they could not be listed, which
	// leaves every log's counted set UNKNOWN: its trim blocks, and says so
	// on the floor it publishes.
	holders        []statelog.Presence
	holdersUnknown string
}

// read fetches the fleet-wide half of the inputs.
//
// ONCE PER TICK RATHER THAN ONCE PER DOMAIN, and it is a correctness property
// rather than a saving: two domains evaluated against two listings of the same
// register can disagree about who is counted, so one domain's floor would be
// published against a fleet the other's was not.
func (r *retention) read(ctx context.Context) (fleetInputs, error) {
	in := fleetInputs{at: r.clock(), previous: map[string]coord.TrimFloor{}}

	// THE HOLDS ARE READ FIRST, BEFORE WHAT EACH IS HANDED OVER TO: the order
	// keeps a handover from falling between two reads. A pin is released only
	// after its owner wrote what replaces it (a join's positions row counts the
	// tail, [stateLog.releaseCountedTail]; a backup announced its point,
	// [backup.Service], and an unannounced one leaves the older, which keeps
	// more), so a hold this read misses was released, and its replacement landed,
	// before the later reads below. Read the other way, a joiner that wrote its
	// row and released its pin between the reads is in neither, and the tick
	// could purge the tail it just installed itself in front of.
	var err error
	if in.holds, in.silent, err = r.watch.Read(ctx, r.fleet); err != nil {
		return in, fmt.Errorf("read the trim holds: %w", err)
	}

	positions, err := r.fleet.Positions(ctx)
	if err != nil {
		return in, fmt.Errorf("read the positions register: %w", err)
	}
	in.positions, in.readable = positions, true

	if in.backups, err = r.fleet.BackupPoints(ctx); err != nil {
		return in, fmt.Errorf("read the backup points: %w", err)
	}
	// EACH DOMAIN'S FLOOR ON ITS OWN: a domain whose row did not read is
	// that domain's trim's to refuse ([retention.domain]), while every other
	// log trims. The register's listings above are every domain's at once,
	// and so is their failure.
	floors := readFloorsOf(ctx, r.fleet, registeredNames())
	in.floorsUnread = map[string]error{}
	for _, name := range registeredNames() {
		row, published, unread := floors.row(name)
		switch {
		case unread != nil:
			in.floorsUnread[name] = unread
		case published:
			in.previous[name] = row
		}
	}
	// THE LIVE DATA NODES CATCH A NODE WHOSE ROW HAS NOT LANDED (a presence lease
	// whose heartbeat has not published): it counts at position zero and blocks,
	// correctly, since trimming past it would delete what it has to apply. A node
	// ADOPTING A SNAPSHOT AT BOOT is caught by neither this nor the register: its
	// own hold, renewed until its first row counts it ([stateLog.holdUntilCounted]),
	// keeps its tail. The listing is the gate's own ([livePresences]), so an
	// eviction and the trim judge presence from one answer.
	//
	// UNKNOWN IS NOT FATAL TO THE TICK, unlike the register: each log reads its
	// counted set as UNKNOWN and publishes its floor BLOCKED on the applied
	// term with the reason, rather than unexplained.
	if r.holders != nil {
		if in.holders, err = r.holders.LiveData(ctx); err != nil {
			in.holdersUnknown = fmt.Sprintf("the live data nodes could not be listed (%v)", err)
		}
	}
	return in, nil
}

// counted is who the trim counts on one log: the positions register's rows
// naming it, every live data node, less the log's own tombstones past their
// window ([statelog.CountedSet]).
func (shared fleetInputs) counted(running *runningLog, tombs []statelog.Tombstone) []statelog.NodePosition {
	return statelog.CountedSet(shared.at, reportedPositions(shared.positions, running.domain.Name()),
		shared.holders, tombs)
}

// domain evaluates and applies one log's trim, and answers the floor the
// log's anchor sweep removes below ([retention.sweepAnchors]): the log's first
// surviving sequence once the purge has landed — never the published floor,
// which can lead the log after a purge that failed, and an anchor of a record
// the log still holds is one its next write uses.
func (r *retention) domain(ctx context.Context, running *runningLog, shared fleetInputs) (statelog.Position, error) {
	name := running.domain.Name()
	if err := shared.floorsUnread[name]; err != nil {
		// THE FLOOR IS CARRIED FORWARD FROM THE ROW, and never moves down
		// within a generation ([nextFloor]) — so a tick that cannot read
		// the row cannot know what it would carry, and a floor published
		// without it could fall below a purge already licensed. This log
		// neither publishes nor purges; every other one goes on.
		return statelog.Position{}, fmt.Errorf("its published floor could not be "+
			"read, so this tick neither publishes over it nor purges: %w", err)
	}
	stats, err := running.log.Stats(ctx)
	if err != nil {
		// THE STREAM ITSELF IS UNREADABLE, so there is no ceiling to
		// report headroom against and no first sequence to purge from.
		// Publishing a floor here would name a fleet state nobody
		// observed, so the tick says so and leaves the previous floor
		// standing — which is older, and honestly labelled by its own
		// `at`.
		return statelog.Position{}, fmt.Errorf("read the stream's state: %w", err)
	}
	generation := running.runner.Committed().Generation

	in := statelog.TrimInputs{
		Generation:      generation,
		Now:             shared.at,
		CountedReadable: shared.readable && shared.holdersUnknown == "",
		CountedUnknown:  shared.holdersUnknown,
		HoldsReadable:   true,
		BackupMaxAge:    r.cfg.BackupMaxAge(),
		HoldStale:       statelog.TrimHoldStale,
	}
	// AN UNREAD LOG'S TOMBSTONES ARE NONE HERE, which is the conservative
	// direction for the trim: a node it could not establish was gone stays
	// counted. The report is the one reader that has to say so as well.
	tombs, _ := r.tombstones(ctx, running, generation)
	in.Counted = shared.counted(running, tombs)
	in.Holds = holdsFor(shared.holds, shared.silent, running.spec.Name)
	in.BackupFloor, in.BackupAt, in.BackupFloorGen, in.HasBackupFloor =
		r.backupTerm(shared.backups, running.spec.Name)
	in.FeedAckFloor, in.HasFeed, in.FeedReadable = r.feedTerm(ctx, running)
	in.AgeFloor, err = r.ageFloor(ctx, running.log, stats, r.cfg.MinAge(), shared.at)
	if err != nil {
		return statelog.Position{}, fmt.Errorf("find the age floor: %w", err)
	}

	decision := statelog.Trim(in.Terms())
	r.gauges(name, statelog.ReservationOf(running.domain), stats, decision, shared)
	first, err := r.apply(ctx, running.log, name, generation, decision, stats.FirstSeq, shared)
	if err != nil {
		return statelog.Position{}, err
	}
	return statelog.Position{Stream: running.spec.Name, Generation: generation, Seq: first}, nil
}

// sweepTarget is one log a tick sweeps the anchors of, and the floor
// [retention.domain] answered for it.
type sweepTarget struct {
	running *runningLog
	below   statelog.Position
}

// sweepAnchors appends, on every trimmed log that writes anchors, the records
// that remove the anchors this node's copy holds below what the log no longer
// holds: one per [statelog.AnchorsPerTrimmedRecord], at most
// [statelog.TrimmedRecordsPerSweep] a log a tick, none where none lie below.
// A RECORD AND NEVER A DELETE: anchors are replicated-estate state, which every
// node's applier removes in the transaction that applies the record. Paced ON
// THIS NODE'S OWN APPLIER, one record committed before the next, so a behind
// holder appends nothing ([statelog.PlanSweep], [statelog.SweepPlan]). ALL
// INSIDE ONE [statelog.AlarmInterval], counted first ([runSweeps],
// [anchorSweepDeadline]). A FAILURE DOES NOT FAIL THE TICK: a kept anchor costs
// its next write one refused append, never a lost update; the next count retries.
func (r *retention) sweepAnchors(ctx context.Context, targets []sweepTarget) {
	deadline := time.Now().Add(anchorSweepDeadline)
	counting, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var counted []countedSweep
	for _, target := range targets {
		deps, sweeps := r.sweepDeps(target.running)
		if !sweeps {
			continue
		}
		name := target.running.domain.Name()
		plan, err := statelog.PlanSweep(counting, deps, target.below)
		if err != nil {
			log.WarnContext(ctx, "retention_anchor_sweep_failed", "domain", name,
				"generation", target.below.Generation, "below", target.below.Seq,
				"records", 0, "err", err)
			continue
		}
		counted = append(counted, countedSweep{domain: name, below: target.below, plan: plan})
	}
	runSweeps(ctx, deadline, time.Now, counted)
}

// anchorSweepDeadline is how long a tick's anchor sweeps may take together,
// counts and appends alike: ONE [statelog.AlarmInterval], ten seconds.
//
// The sweeps run on the trim's own goroutine ([retention.run]), each paced on
// this node's applier, so the bound limits the work every OTHER data node is
// handed on its one write lane: a node k times slower is left at most about
// k − 1 bounds behind when the sweep ends ([statelog.SweepPlan.Sweep]), under
// [statelog.StallGrace], the lag `apply_lag` names, for any node up to six times
// slower. A longer bound queues more before a slower node's own writes; a
// shorter one only drains a backlog slower, and binds only on a large table
// (the drain arithmetic is [statelog.TrimmedRecordsPerSweep]'s).
const anchorSweepDeadline = statelog.AlarmInterval

// countedSweep is one log's anchor sweep, counted, as [runSweeps] schedules
// it.
type countedSweep struct {
	domain string
	below  statelog.Position
	plan   anchorSweep
}

// anchorSweep is what [runSweeps] asks of a counted sweep: how many records
// it appends, and to append them within a budget — [statelog.SweepPlan].
type anchorSweep interface {
	Records() int
	Sweep(ctx context.Context, budget time.Duration) (statelog.Swept, error)
}

// runSweeps appends every counted sweep that has records to append, in order,
// each within an equal share of what is left before deadline among the sweeps
// still to run.
//
// SHARED AMONG THE LOGS WITH ANYTHING TO SWEEP, AND COUNTED FIRST: an equal
// share for every anchor-writing log spends a share on each idle one, so every
// further such log would cut the others' sweeps, a company with no repositories
// included. Counted first, an idle log costs the others its count and nothing
// more. SHARED rather than first come: what one sweep leaves unspent goes to
// the sweeps after it, and a backlog on an early log never takes the whole
// interval from a later one.
func runSweeps(ctx context.Context, deadline time.Time, now func() time.Time, counted []countedSweep) {
	var busy []countedSweep
	for _, c := range counted {
		if c.plan.Records() > 0 {
			busy = append(busy, c)
		}
	}
	for i, c := range busy {
		budget := deadline.Sub(now()) / time.Duration(len(busy)-i)
		if budget <= 0 {
			// THE COUNTS SPENT THE INTERVAL: every copy was slow to reach
			// its log's end. Nothing is appended past the bound, and the
			// next tick counts again.
			log.WarnContext(ctx, "retention_anchor_sweep_failed", "domain", c.domain,
				"generation", c.below.Generation, "below", c.below.Seq, "records", 0,
				"err", fmt.Errorf("the %v a tick's anchor sweeps share was spent "+
					"counting the logs' anchors, so none is left to append %d "+
					"record(s) in", anchorSweepDeadline, c.plan.Records()))
			continue
		}
		swept, err := c.plan.Sweep(ctx, budget)
		if err != nil {
			log.WarnContext(ctx, "retention_anchor_sweep_failed", "domain", c.domain,
				"generation", c.below.Generation, "below", c.below.Seq, "records", swept.Records,
				"err", err)
			continue
		}
		if swept.Records > 0 {
			log.InfoContext(ctx, "retention_anchors_swept", "domain", c.domain,
				"generation", c.below.Generation, "below", c.below.Seq, "records", swept.Records,
				"more_remain", swept.Remain)
		}
	}
}

// sweepDeps is what running's anchor sweep is handed, or false for a log the
// trim sweeps nothing on: one that writes no anchor, which has no trimmed
// record, or a node with no copy of the estate to count from.
func (r *retention) sweepDeps(running *runningLog) (statelog.SweepDeps, bool) {
	encode := trimmedEncoder(running.domain)
	if encode == nil || r.estate == nil {
		return statelog.SweepDeps{}, false
	}
	deps := statelog.SweepDeps{
		Domain: running.domain, Spec: running.spec, Log: running.log,
		Ends: func(ctx context.Context) (statelog.LogEnds, error) {
			first, last, err := running.log.Bounds(ctx)
			return statelog.LogEnds{First: first, Last: last}, err
		},
		Runner: running.runner,
		// SIGNED WITH THE LOG'S OWN SIGNER, the one its reanchor and its
		// read index's barriers are: every applier opens a record's frame
		// before it reads one, so a trimmed record nobody signed would be
		// refused on every node and its anchors kept for good.
		Signer: running.signer,
		Encode: encode, DB: r.estate(), Writer: r.nodeID, Evicted: running.evicted,
	}
	// NEVER A NIL *Reserve IN THE INTERFACE: a domain that keeps none is
	// handed none, which the sweep tells apart from one that forgot it.
	if running.reserve != nil {
		deps.Admission = running.reserve
	}
	return deps, true
}

// apply publishes what one tick decided, then purges. THE FLOOR IS PUBLISHED
// BEFORE THE PURGE IT LICENSES; one that could not be published licenses
// nothing. The write fence clears an expectation of zero against this floor
// ([statelog]'s floor theorem, clause (ii)), which needs every record ever
// removed to sit below the floor a writer reads. Purged first, a node the purge
// left behind is cleared against the old number, and a publish that then FAILS
// keeps that window open. The opposite failure (published, purge fails or the
// process dies) is safe: a node the tick counted is never below what it
// licensed, and one it did not count catches up by replay, refusing reads as
// `behind` meanwhile ([statelog.FloorReplaying]). It answers the log's first
// surviving sequence, the floor [retention.sweepAnchors] removes below.
func (r *retention) apply(ctx context.Context, stream *jetstream.DomainLog, name string,
	generation uint32, decision statelog.TrimDecision, first uint64, shared fleetInputs) (uint64, error) {

	if err := r.publish(ctx, name, generation, decision, first, shared); err != nil {
		return 0, err
	}
	if decision.Blocked() || decision.To <= first {
		return first, nil
	}
	if err := stream.Purge(ctx, decision.To); err != nil {
		return 0, fmt.Errorf("purge below %d: %w", decision.To, err)
	}
	log.InfoContext(ctx, "retention_trimmed", "domain", name,
		"trim_to", decision.To, "was", first, "generation", generation)
	return decision.To, nil
}

// nextFloor is the [coord.TrimFloor.Floor] a tick publishes: the previous row's
// floor carried forward, raised by what this tick licenses and what the stream
// has already lost. It never moves down, though a tick's conclusion does (zero
// when blocked, a readmitted or restored node's position): records already
// removed are gone, so a floor following it down would tell that node it holds
// everything that may be missing, the vacuous comparand [stateLog.trimFloor]
// once was. A row from a HIGHER generation refuses: publishing over a sequence
// space the fleet has left would put a floor nobody on the live generation can
// read where theirs was ([floorFor]); nothing published means nothing purged. A
// LOWER generation's row is not carried: its sequences name a dead number space.
func nextFloor(previous coord.TrimFloor, had bool, generation uint32, to, first uint64) (uint64, error) {
	floor := max(to, first)
	switch {
	case !had || previous.Generation < generation:
	case previous.Generation > generation:
		return 0, fmt.Errorf("the published floor for %s is at generation %d and "+
			"this node is on %d — its positions name a sequence space the fleet "+
			"has left, so it neither publishes over that floor nor purges",
			previous.Domain, previous.Generation, generation)
	default:
		floor = max(floor, previous.Floor)
	}
	return floor, nil
}

// publish writes what this tick concluded.
//
// THE ONE PIECE OF STATE THAT SURVIVES A LEASE HANDOVER is `blocked_since`,
// and it is carried through the register rather than in memory for exactly
// that reason: the duty moves, and a value held by the holder would reset on
// every flap — so `trim_blocked`, which fires only once a block has outlived
// the `min_age` window, would never be reached on a fleet whose lease moved
// even once a week, which is the bug the field exists to close. The floor is
// the other, and it is carried for the reason [nextFloor] gives.
func (r *retention) publish(ctx context.Context, name string, generation uint32,
	decision statelog.TrimDecision, first uint64, shared fleetInputs) error {

	previous, had := shared.previous[name]
	floor, err := nextFloor(previous, had, generation, decision.To, first)
	if err != nil {
		return err
	}
	row := coord.TrimFloor{
		Domain:     name,
		Generation: generation,
		TrimTo:     decision.To,
		Floor:      floor,
		BlockedBy:  string(decision.BlockedBy),
		At:         shared.at,
		By:         r.nodeID,
	}
	for _, t := range decision.Terms {
		row.Terms = append(row.Terms, coord.TrimTerm{
			Name: string(t.Name), Seq: t.Seq, Known: t.Known,
			Absent: t.Absent, Detail: coord.FitText(t.Detail, coord.MaxTrimDetailBytes),
		})
	}
	switch {
	case !decision.Blocked():
		// NOT BLOCKED IS NOT "blocked since now": the field is only
		// meaningful while something is stopping the trim, and leaving
		// a stale instant on an advancing domain would age into an
		// alarm on a healthy fleet.
		row.BlockedSince = time.Time{}
	case had && previous.Blocked() && !previous.BlockedSince.IsZero():
		// STILL BLOCKED, so the clock keeps running — and it keeps
		// running ACROSS A CHANGE OF TERM, deliberately. A fleet whose
		// blocking term rotates between `applied` and `backup_floor`
		// while the trim goes nowhere has been blocked throughout, and
		// restarting the clock on each rotation would hide the longest
		// outages behind the noisiest ones.
		//
		// There is no third case comparing the POINT, because there is
		// no point to compare: [statelog.Trim] reports Blocked exactly
		// when it permits removing up to zero, so a blocked tick's
		// TrimTo is always 0 and a "blocked further along than last
		// time" state does not exist.
		row.BlockedSince = previous.BlockedSince
	default:
		row.BlockedSince = shared.at
	}
	if err := r.fleet.PutFloor(ctx, row); err != nil {
		return fmt.Errorf("publish the trim floor: %w", err)
	}
	r.sayBlock(ctx, name, previous, had, decision, row)
	return nil
}

// sayBlock logs a domain's block when it starts, when the term holding it
// changes, and when it ends, never on a tick that changed none of those.
//
// Transitions, not a line per tick: a blocked trim is a state a healthy fleet
// lives in for weeks (a fresh deployment awaits its first backup and donors; a
// log nobody writes to stays at position zero), so a WARN per tick is noise.
// Whether a block COSTS something is the `trim_blocked` alarm's question. FROM
// THE PUBLISHED ROW, not memory: a duty holder that took over mid-block says
// nothing about a block that did not start on its watch. BOTH AT WARN, so a
// filter that shows a block's start shows its end; splitting by term leaves a
// WARN start with an INFO end when a block changes term.
func (r *retention) sayBlock(ctx context.Context, name string,
	previous coord.TrimFloor, had bool, decision statelog.TrimDecision,
	row coord.TrimFloor) {

	wasBlocked := had && previous.Blocked()
	switch {
	case decision.Blocked() &&
		(!wasBlocked || previous.BlockedBy != string(decision.BlockedBy)):
		attrs := []any{"domain", name, "term", decision.BlockedBy,
			"detail", decision.Detail, "blocked_since", row.BlockedSince}
		if wasBlocked {
			// THE CLOCK DID NOT RESTART — see [retention.publish] —
			// so the line says which term held the block before.
			attrs = append(attrs, "was", previous.BlockedBy)
		}
		r.logs().WarnContext(ctx, "retention_trim_blocked", attrs...)
	case !decision.Blocked() && wasBlocked:
		attrs := []any{"domain", name, "was", previous.BlockedBy,
			"trim_to", decision.To}
		if !previous.BlockedSince.IsZero() {
			attrs = append(attrs, "for",
				row.At.Sub(previous.BlockedSince).Round(time.Second).String())
		}
		r.logs().WarnContext(ctx, "retention_trim_unblocked", attrs...)
	}
}

// logs is where the trim speaks: the package's own logger, or a case's.
func (r *retention) logs() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return log
}

// reportedPositions is every node's committed position on one log, keyed as
// the register keys it: every register row that names the log.
func reportedPositions(rows []coord.NodePositions, domain string) []statelog.NodePosition {
	out := make([]statelog.NodePosition, 0, len(rows))
	for _, row := range rows {
		at, runs := row.Domains[domain]
		if !runs {
			// A NODE THAT DOES NOT RUN THIS DOMAIN IS NOT A NODE AT
			// POSITION ZERO. Counting it would pin every domain at
			// the floor of the fleet's least-configured node.
			continue
		}
		out = append(out, statelog.NodePosition{
			NodeID: row.NodeID, Generation: at.Generation, Seq: at.Seq,
			SnapshotSeq: at.SnapshotSeq, RecordVersion: at.RecordVersion,
			// FROM THE INSTANT RATHER THAN THE SEQUENCE. A snapshot
			// taken while a domain's log was still empty covers it
			// at position zero, which is a real artefact a joiner
			// can adopt — and reading "has none" off the sequence
			// would leave a fleet's newest domain permanently one
			// donor short of trimming any of the others.
			HasSnapshot: !at.SnapshotAt.IsZero(),
			// AND THE GENERATION THAT SEQUENCE BELONGS TO, which is
			// not this row's own: a position is republished every
			// ten seconds and a snapshot taken once a day, so after
			// a reanchor the row sits on the new generation while
			// its artefact still names the old one.
			SnapshotGeneration: at.SnapshotGeneration,
			At:                 row.At,
		})
	}
	return out
}

// holdsFor is every live pin on one domain's log, each with how long the trim
// has watched it stand unrenewed (silent, by owner).
//
// BY STREAM NAME, because that is [coord.TrimHold.Streams]' key space — a hold
// is a pin on a log, and both the backup and a joining node state theirs from
// `statelog_cursor`, which is keyed on the stream.
func holdsFor(holds []coord.TrimHold, silent map[string]time.Duration, stream string) []statelog.Hold {
	out := make([]statelog.Hold, 0, len(holds))
	for _, hold := range holds {
		at, pins := hold.Streams[stream]
		if !pins {
			continue
		}
		out = append(out, statelog.Hold{
			Owner: hold.Owner, Generation: at.Generation, Seq: at.Seq,
			Silent: silent[hold.Owner],
		})
	}
	return out
}

// backupTerm is the newest backup's reach in one domain's log, under whichever
// policy the operator declared. BY STREAM NAME, for [holdsFor]'s reason.
//
// The two policies differ in WHICH rows count and in nothing else: `engine`
// follows the copies this fleet's own nodes wrote and verified, `operator`
// follows the acknowledgement a person gave once the copy left the host — a
// fact the engine cannot see for itself.
func (r *retention) backupTerm(points []coord.BackupPoint, stream string) (
	seq uint64, at time.Time, generation uint32, have bool) {

	newest, ok := r.newestCounted(points, stream)
	if !ok {
		// NO COUNTED BACKUP COVERS THIS LOG, which is not a backup at
		// sequence zero — it is no backup of this domain at all, which
		// blocks. The distinction is the whole reason the term is
		// three-valued.
		return 0, time.Time{}, 0, false
	}
	reach := newest.Streams[stream]
	return reach.Seq, newest.At, reach.Generation, true
}

// newestCounted is the newest verified backup covering one log that the
// operator's policy takes the word of — the one point both the trim's backup
// term and the backup-age alarm read for that log, through
// [coord.CountedBackups] and [coord.NewestBackupOf], so the alarm can never
// age a backup the trim does not count.
//
// PER LOG, and the order of the two questions is the fix. Taking the newest
// counted point and THEN asking whether it covered the log blocked every log it
// did not: under `backup_floor: operator` an acknowledgement names one log, so
// the newest one covered the log acknowledged last and every other log's trim
// stopped, while the alarm aged that newest row and stayed green.
func (r *retention) newestCounted(points []coord.BackupPoint, stream string) (coord.BackupPoint, bool) {
	return coord.NewestBackupOf(coord.CountedBackups(points,
		r.cfg.Floor() == config.BackupFloorOperator), stream)
}

// feedTerm is how far this domain's wake feed has acknowledged.
//
// THE GROUP IS THE DOMAIN'S OWN DECLARATION ([statelog.Domain.FeedGroup]),
// never a name this file knows. It read the tracker's group for every domain
// that claims identity, and on the knowledge base's log that consumer does not
// exist — so the lookup below answered "never created", the term permitted
// nothing, and that log was blocked on `feed_ack_floor` for the life of the
// deployment while its own feed acknowledged every record.
func (r *retention) feedTerm(ctx context.Context, running *runningLog) (
	seq uint64, has, readable bool) {

	group := running.domain.FeedGroup()
	if group == "" {
		// THIS DOMAIN HAS NO WAKE FEED, which is absent rather than
		// unreadable: reporting zero would block its trim for ever on a
		// term it does not have.
		return 0, false, false
	}
	floor, exists, err := running.log.GroupAckFloor(ctx, group)
	switch {
	case err != nil:
		// LOGGED HERE because the term can carry only that it is
		// unknown: the blocked-trim line names the term and not the
		// broker's answer, and without this the cause of an unreadable
		// consumer is nowhere at all.
		log.WarnContext(ctx, "retention_feed_unreadable",
			"domain", running.domain.Name(), "group", group, "err", err)
		return 0, true, false
	case !exists:
		// THE DECLARED FEED HAS NOT BEEN OPENED YET, which is a fleet
		// that has never started it rather than one whose consumer
		// could not be read. It permits nothing, because a record no
		// feed has seen is one nobody has been told about — and that is
		// exactly what the term says.
		//
		// It is also precisely what a WRONG group looks like, for ever,
		// which is why the name is the domain's declaration and
		// [Engine.feedFor] refuses a feed that opens any other.
		return 0, true, true
	}
	return floor, true, true
}

// evictionLister is what the trim asks an identity-claiming domain: every
// eviction its OWN applied rows hold, on its OWN log.
//
// DECLARED HERE, by the caller that counts nodes per log, and answered by each
// domain for itself. The trim used to call the tracker's reader for every
// identity-claiming domain with that domain's stream name — so on the pages
// log it asked the tracker's table for rows filed under the pages stream,
// found none, and counted an evicted node there for ever. [checkRegister]
// refuses a registered identity-claiming domain that does not answer this
// ([checkIdentityEntry]), and the statelogtest suite certifies that every one
// answers it correctly.
type evictionLister interface {
	Evictions(ctx context.Context, db store.ReplicatedReader) ([]statelog.EvictionRow, error)
}

// The identity-claiming domains this build registers, held to the interface
// — and to the log probe a stranded node asks ([statelog.EvictionProbe]) — at
// compile time, so a reshaped method is a build failure rather than a boot
// refusal.
var (
	_ evictionLister = worklet.Domain{}
	_ evictionLister = doclet.Domain{}
	_ evictionLister = iamdomain.Domain{}
	_ evictionLister = artifactsdomain.Domain{}
	_ evictionLister = artifactdata.Domain{}
	_ evictionLister = assistantdomain.Domain{}
	_ evictionLister = chatlet.Domain{}
	_ evictionLister = executordomain.Domain{}
	_ evictionLister = gitlet.Domain{}

	// AND EVERY ONE READS ITS GATE RECORDS OFF THE LOG — an eviction and the
	// readmission that lifts it — which [checkRegister] refuses a registered
	// identity-claiming domain for lacking.
	_ statelog.EvictionProbe = worklet.Domain{}
	_ statelog.EvictionProbe = doclet.Domain{}
	_ statelog.EvictionProbe = iamdomain.Domain{}
	_ statelog.EvictionProbe = artifactsdomain.Domain{}
	_ statelog.EvictionProbe = artifactdata.Domain{}
	_ statelog.EvictionProbe = assistantdomain.Domain{}
	_ statelog.EvictionProbe = chatlet.Domain{}
	_ statelog.EvictionProbe = executordomain.Domain{}
	_ statelog.EvictionProbe = gitlet.Domain{}
)

// tombstones is every eviction this node has applied for one domain's log.
//
// READ FROM THE REPLICATED ROWS, not coordination: an eviction is a record on
// the log, identical on every data node, and the applier's own gate reads this
// table; a second copy would give the fleet two answers. A read failure yields
// NO tombstones, the conservative direction (an evicted node stays counted and
// pins the floor), AND SAYS SO: read is false wherever the rows were not read,
// so no tombstone is no answer about any node (a reader that took it for a
// readmission released an eviction it had just made). A domain claiming no
// identity carries no evictions and was read in full.
func (r *retention) tombstones(ctx context.Context, running *runningLog,
	generation uint32) (tombs []statelog.Tombstone, read bool) {

	return logTombstones(ctx, r.db, running.domain, generation)
}

// logTombstones is every eviction this node has applied on domain's log, each
// stamped with generation — see [retention.tombstones], which the snapshot
// loop's count reads through too ([Engine.counted]), so the two subtract one
// set of tombstones.
func logTombstones(ctx context.Context, db *store.DB, domain statelog.Domain,
	generation uint32) (tombs []statelog.Tombstone, read bool) {

	if !domain.ClaimsIdentity() {
		// ONLY AN IDENTITY-CLAIMING DOMAIN CARRIES EVICTIONS. A domain
		// that does not claim identity has no say in who the fleet
		// counts on its log.
		return nil, true
	}
	if db == nil {
		return nil, false
	}
	lister, ok := domain.(evictionLister)
	if !ok {
		// UNREACHABLE ON A NODE THAT BOOTED — [checkRegister] refuses
		// such a domain before anything is provisioned — and logged
		// rather than assumed, on the conservative side: nobody is
		// uncounted.
		log.ErrorContext(ctx, "retention_evictions_unlisted",
			"domain", domain.Name())
		return nil, false
	}
	rows, err := lister.Evictions(ctx, db.Replicated().Reader())
	switch {
	case errors.Is(err, store.ErrNoEstate) || errors.Is(err, context.Canceled):
		// A STOP THIS PROCESS ASKED FOR IS NOT AN UNREADABLE TABLE. The
		// replicated estate closes during shutdown and during an
		// adoption's rename, and a tick already in flight reaches it —
		// which is the honest answer rather than a fault, and logging
		// it at WARN would put a line in every clean shutdown. It is
		// still not a read.
		return nil, false
	case err != nil:
		log.WarnContext(ctx, "retention_evictions_unreadable",
			"domain", domain.Name(), "err", err)
		return nil, false
	}
	out := make([]statelog.Tombstone, 0, len(rows))
	for _, row := range rows {
		if row.Back {
			// READMITTED, so there is no tombstone: the node is
			// counted again and its position pins the floor as any
			// other node's does.
			continue
		}
		out = append(out, statelog.Tombstone{
			NodeID: row.NodeID, At: row.At, By: row.By, Generation: generation,
		})
	}
	return out, true
}

// ageFloor is the first sequence the age term will keep.
//
// A binary search, not a stored index: timestamps are the BROKER's own, so
// monotone in sequence, and "the newest record older than T" costs one
// [jetstream.DomainLog.At] per step (about twenty-eight reads a quarter-hour at
// year five), cheaper than an index maintained on every append and correct after
// every purge. It answers the FIRST sequence to KEEP, exclusive-style like every
// term: last+1 when the newest record is already older than the floor
// (everything may go), first when the oldest is younger (nothing may go).
func (r *retention) ageFloor(ctx context.Context, log *jetstream.DomainLog,
	stats jetstream.LogStats, minAge time.Duration, now time.Time) (uint64, error) {

	return ageFloorOf(stats.FirstSeq, stats.LastSeq, stats.Messages,
		now.Add(-minAge), func(seq uint64) (time.Time, bool, error) {
			_, _, storedAt, ok, err := log.At(ctx, seq)
			return storedAt, ok, err
		})
}

// ageFloorOf is the arithmetic, over anything that can be probed by sequence.
//
// SEPARATED FROM THE BROKER deliberately, on this subsystem's own rule: a
// policy that can only be exercised through a live broker is one nobody
// re-checks, and every interesting case here — an empty log, a log entirely
// older than the cutoff, one entirely younger, and one with a purged hole in
// the middle — is reachable in a table test with no broker at all.
func ageFloorOf(first, last, messages uint64, cutoff time.Time,
	at func(seq uint64) (time.Time, bool, error)) (uint64, error) {

	if messages == 0 || first > last {
		// An empty log has nothing to keep and nothing to remove; the
		// term must not block, so it permits everything up to the head.
		return last + 1, nil
	}
	var probeErr error
	span := int(last - first + 1)
	idx := sort.Search(span, func(i int) bool {
		if probeErr != nil {
			return true
		}
		storedAt, ok, err := at(first + uint64(i))
		if err != nil {
			probeErr = err
			return true
		}
		if !ok {
			// A GAP IS NOT AN ERROR AND IS NOT YOUNG. A purged
			// sequence answers "not found", and treating it as
			// BELOW the cutoff is correct: anything already gone is
			// older than anything still held, so the search keeps
			// moving up rather than stopping on a hole.
			return false
		}
		return !storedAt.Before(cutoff)
	})
	if probeErr != nil {
		return 0, probeErr
	}
	return first + uint64(idx), nil
}

// gauges publishes the capacity and gate figures a collector reads.
//
// Set here because this tick alone holds all of them at once (one info call,
// one evaluation, one register read); a gauge from a second fetch would
// describe a state the decision beside it was not taken against. A NIL
// RECORDER RECORDS NOTHING, a legal deployment: the report carries the numbers.
// THE HEADROOM IS OF THE CEILING ORDINARY WRITES ARE HELD TO, on a log with a
// gate reserve [statelog.Reservation.Bytes] below the broker's, from the one
// function the report and the alarm read too, so a graph reaches zero exactly
// where writes start being refused.
func (r *retention) gauges(domain string, reservation statelog.Reservation, stats jetstream.LogStats,
	decision statelog.TrimDecision, shared fleetInputs) {

	if r.metrics == nil {
		return
	}
	at := metrics.Attrs{"domain": domain}
	r.metrics.Set(metrics.StatelogLogBytes, float64(stats.Bytes), at)
	if stats.MaxBytes > 0 {
		// HEADROOM ONLY WHERE THERE IS A CEILING. A fraction of an
		// unbounded log is not zero headroom, and zero is the value the
		// one alarm an operator cannot ignore fires on.
		r.metrics.Set(metrics.StatelogLogMaxBytes, float64(stats.MaxBytes), at)
		if free := reservation.Headroom(stats.Bytes, stats.MaxBytes); free != nil {
			r.metrics.Set(metrics.StatelogLogHeadroomFraction, *free, at)
		}
	}
	blocked := 0.0
	if decision.Blocked() {
		if previous, had := shared.previous[domain]; had && !previous.BlockedSince.IsZero() {
			blocked = shared.at.Sub(previous.BlockedSince).Seconds()
		}
	}
	r.metrics.Set(metrics.StatelogTrimBlockedSeconds, blocked, at)
}
