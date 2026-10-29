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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE TRIM, and it is the one loop without which the log only grows.
//
// # What it is, in one paragraph
//
// Every domain's ordered log is a replay window rather than an archive: the
// durable tables are the record of truth, and the log's job is to carry
// records to nodes that have not applied them yet. Once every node the fleet
// counts has committed past a record — and nothing else needs it — the record
// can go. [statelog.Trim] is the arithmetic that decides how far, from six
// terms that each move the point DOWN; this file is what reads those terms
// from a live fleet, publishes what it concluded — so a node that is not
// holding the duty can still say why the log is not shrinking, and so every
// write fence knows what may be gone — and only THEN applies the answer to the
// broker, for the reason [retention.apply] gives. Last, on a log that writes
// arbitration anchors, it appends the trimmed records that have every node
// remove the anchors below what the log no longer holds (ADR-0050,
// [retention.sweepAnchors]): an anchor there names a record that is gone, and
// can only send its subject's next write to be refused.
//
// # Why nothing here re-decides
//
// The six terms have an inversion in them that three readers got backwards,
// which is exactly why the arithmetic is a pure function over a struct of
// values. This loop's whole job is to fill that struct honestly — including
// with the THIRD value, "could not be read", which blocks the trim rather than
// being treated as satisfied. A term nobody could read is not a term nobody
// needs.
//
// # A fleet singleton, and what happens without one
//
// The trim is a burst of purges against shared streams, and two nodes deciding
// concurrently would race on the register's published floor: the loser's
// conclusion would overwrite the winner's and `blocked_since` — the field that
// says how long this has been going on — would reset on every tick. So it is a
// duty like the sweep, on the `worker:{duty}` lease, and a fleet whose nodes
// all declare `roles: [seats]` deliberately does not trim.
//
// ONLY A DATA NODE ARMS IT, because the trim reads the applied eviction rows
// of the estate ([retention.tombstones]) and only a node running the logs has
// them: [Engine.startRetention] is reached only where a state log runs, so a
// node without `data` never competes for the lease it could not use.

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
	// role carries the health grant to be woken by (ADR-0065); nil on a
	// node with no broker to publish to.
	wakes *alarmOutbox

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

	// code fills the code host's half of a reading at now — what this
	// node's committer refused busy for a backlog, and what its git cache
	// and its compactor found ([Engine.codeReading]). Nil reads as nothing
	// to report, which is also what it fills on a node holding neither a
	// committer nor a cache.
	code func(ctx context.Context, now time.Time, out *statelog.Reading)

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

	// mu guards the coverage cache and the log rates below. The tick and
	// every API request assemble a report, on different goroutines.
	mu sync.Mutex

	// coverFraction and coverKnown are the last tick's coverage
	// measurement.
	//
	// TAKEN ON THE TICK AND READ BY THE REPORT, because the measurement is
	// a scan of the whole source corpus and a report is assembled on every
	// operator request, every dashboard poll and every alarm beat. It, each
	// log's daily intake and the store's connection-pool waits are the ONLY
	// alarm inputs measured at the trim's [statelog.TrimInterval] — two
	// scans and a delta of cumulative counters, each honest at a
	// quarter-hour because none summarises anything that moves faster than
	// a corpus, a day or a tick. Every other input the reading carries —
	// apply lag, the backup register, the maintenance window, free space,
	// the other windowed metrics, each log's held record and whether each
	// log's floor can be read — is re-read on
	// every [statelog.AlarmInterval] beat. See [retention.measureCoverage]
	// and [retention.heartbeat].
	coverFraction float64
	coverKnown    bool

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

	// now is the clock every evaluation and every report reads. Nil reads
	// the wall clock; a case sets it to put a log's first days behind it.
	now func() time.Time

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
		nodeID:      s.nodeID,
		alarms:      statelog.NewTracker(e.metrics, nil),
		coverage:    e.vectorCoverage,
		index:       e.indexReading,
		objects:     e.objectsReading,
		code:        e.codeReading,
		seats:       func() int { return seatCount(e.Company()) },
		background:  e.backgroundBarriers,
		pooled:      map[string]poolCounters{},
		floors: newFloorWatch(func(ctx context.Context, domains []string) publishedFloors {
			return readFloorsOf(ctx, e.backends.Fleet, domains)
		}),
	}
	// THE ALARMS' WAKES, told of every transition before the first
	// evaluation, so what that evaluation finds firing is handed on too
	// (as firing_at_boot) rather than said by the log alone.
	if e.backends.Queue != nil {
		r.wakes = newAlarmOutbox(e.backends.Queue, e.metrics)
		r.alarms.OnTransition(s.nodeID, r.wakes.Add)
	}
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
	// AND THEN THE ANCHORS BELOW WHAT EACH LOG NO LONGER HOLDS — ADR-0050 —
	// once every log's purge has landed, so the sweeps share their one
	// interval among the logs that have anything to sweep.
	r.sweepAnchors(ctx, sweeps)
}

// decidesFrom is every domain whose rows the trim decides from, in the
// register's order: the logs that claim identity, whose evictions it counts
// from this node's own rows ([logTombstones]), and the logs that write
// arbitration anchors, whose anchors below the log it counts from this node's
// own rows before appending the records that remove them
// ([retention.sweepAnchors]). Every other term it reads is the fleet's
// register or the log itself.
//
// THE TWO SETS ARE NAMED APART although they are the same logs today — every
// strict log the register runs, and neither compacted one — because a log that
// writes anchors and claims no identity is a domain somebody can add, and a
// copy of it out of service would have the trim count anchors on rows nobody
// can trust.
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
// THE MEASUREMENT COMES FIRST, because the reading the table is evaluated
// against reads these back: a tick that observed before it measured would
// evaluate the previous tick's disk against this tick's log. The vector
// coverage and the logs' intake are measured HERE and nowhere else — each is
// a scan a report assembled per dashboard poll must not repeat, and a
// quarter-hour is the resolution every one of them is honest at (see
// [statelog.TrimInterval]). What the table is evaluated on between these ticks is
// [retention.heartbeat].
func (r *retention) evaluate(ctx context.Context) {
	r.capacity(ctx)
	r.measureCoverage(ctx)
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
	r.alarms.Observe(ctx, r.Report(ctx).Alarms)
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
	in := fleetInputs{at: time.Now().UTC(), previous: map[string]coord.TrimFloor{}}

	positions, err := r.fleet.Positions(ctx)
	if err != nil {
		return in, fmt.Errorf("read the positions register: %w", err)
	}
	in.positions, in.readable = positions, true

	if in.holds, err = r.fleet.Holds(ctx); err != nil {
		return in, fmt.Errorf("read the trim holds: %w", err)
	}
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
	// THE LIVE DATA NODES ARE WHAT CATCH A NODE BETWEEN BOOT AND ITS FIRST
	// HEARTBEAT — which is exactly a node adopting a snapshot. It counts at
	// position zero and blocks, which is correct: trimming past a node that
	// is joining is deleting what it is about to replay. The listing is the
	// gate's own ([livePresences]), so an eviction and the trim judge
	// presence from one answer.
	//
	// UNKNOWN IS NOT FATAL TO THE TICK, unlike the register: the logs are
	// still evaluated, each reading its counted set as UNKNOWN, so every
	// floor is published BLOCKED on the applied term with the reason — a
	// presence listing that did not answer, say — rather than left standing
	// unexplained.
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
	in.Holds = holdsFor(shared.holds, running.spec.Name)
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

// sweepAnchors appends, on every log the tick trimmed that writes anchors, the
// trimmed records that remove the arbitration anchors this node's copy holds
// below what the log no longer holds — one per
// [statelog.AnchorsPerTrimmedRecord] of them, at most
// [statelog.TrimmedRecordsPerSweep] a log a tick, and none on a log with none
// below, so an idle fleet's logs gain nothing.
//
// A RECORD AND NEVER A DELETE, because the anchors are replicated-estate state
// and the trim is not their writer: every node's applier removes them, at the
// record's position, in the transaction that applies it.
//
// ON THIS NODE'S OWN APPLIER, which each log's count waits to have applied the
// log's end and each log's records are paced on, one committed before the
// next is appended — so a holder whose copy is behind appends nothing, and a
// write or a read behind the sweep on this node waits one record's apply
// rather than the whole run's ([statelog.PlanSweep], [statelog.SweepPlan]).
//
// ALL OF IT INSIDE ONE [statelog.AlarmInterval], counted first: every log is counted
// under the interval's deadline, and then the logs that have anything to
// sweep share what is left of it ([runSweeps]). See [anchorSweepDeadline] for
// why one interval, and [runSweeps] for why counted first.
//
// A FAILURE DOES NOT FAIL THE TICK. The floor and the purge have already
// landed, and an anchor kept costs its next write one refused append and never
// a lost update — so it is said, and the next tick's count retries it. A sweep
// whose budget ran out is no failure at all: it says what it appended and that
// more remain, as one stopped by the cap does, and the next tick goes on.
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
// THE SWEEPS RUN ON THE TRIM'S OWN GOROUTINE ([retention.run]), one log after
// another, and each paces its records on this node's applier — so a sweep's
// length is whatever that applier takes, and what the bound limits is the work
// every OTHER data node is handed: each applies every record on its one write
// lane, so a node that applies the records k times slower than this one is
// left at most about k − 1 of these bounds behind them when the sweep ends
// ([statelog.SweepPlan.Sweep]) — under [statelog.StallGrace], the lag
// `apply_lag` names a node at, for any node up to six times slower. The alarms
// no longer ride this goroutine ([retention.heartbeat]), so the bound delays no
// evaluation. A longer one queues more in front of a slower node's own writes;
// a shorter one only drains a backlog slower, and the bound binds only on a
// large table, where records apply slowest (the drain arithmetic, at these ten
// seconds, is [statelog.TrimmedRecordsPerSweep]'s).
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
// SHARED AMONG THE LOGS WITH ANYTHING TO SWEEP, AND COUNTED FIRST, because an
// equal share of the interval for every log that writes anchors spent a share
// on each idle one: registering a third such log cut the tracker's and the
// knowledge base's sweeps from five seconds a tick to three and a third on
// every node, a company with no repositories at all included, and a sixth
// would cut them again. Counted first, a log with nothing below its floor
// costs the others its count and nothing more, wherever it sits in the order.
// And SHARED rather than handed out first come: what one sweep leaves unspent
// goes to the sweeps after it, and a backlog on an early log never takes the
// whole interval from a later one.
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

// apply carries out what one tick decided: it publishes the conclusion, and
// then it purges.
//
// THE FLOOR IS PUBLISHED BEFORE THE PURGE IT LICENSES, and a floor that
// could not be published licenses nothing.
//
// The write fence clears an expectation of zero against this floor —
// [statelog]'s floor theorem, clause (ii) — and the proof needs every record
// ever removed to sit below the floor a writer reads. Purged first, the two
// steps leave a window in which the log has lost records the published floor
// still says are held, and a node the purge just left behind is cleared
// against the old number. Nor is that window one tick wide: a purge followed
// by a publish that FAILS leaves it open until some later tick's publish
// succeeds, for as long as the register refuses writes.
//
// The opposite failure — published, then the purge fails or this process dies
// between the two — is the safe one. Readers treat records the log still holds
// as possibly gone, which refuses only a node below what this tick licensed; no
// node the tick counted is, because the applied term put it at or under every
// counted position, and one it did not count catches up by replay, since the
// records are still there. Meanwhile a node between the log's first record and
// the floor refuses reads as `behind` while it replays them
// ([statelog.FloorReplaying]) and is never sent to adopt a snapshot. The gap
// closes only when a later tick licenses removing at least that far, and that
// can be a while: a blocked tick purges nothing, and one whose lowest counted
// node is lower purges less.
//
// It answers the log's first surviving sequence once the tick is done — what
// it purged to, or the first it was handed — which is the floor the anchor
// sweep removes below ([retention.sweepAnchors]).
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

// nextFloor is the [coord.TrimFloor.Floor] a tick publishes: the previous
// row's floor carried forward, raised by what this tick licenses removing and
// by what the stream has already lost.
//
// # Why it never moves down
//
// A tick's own conclusion does. It is zero on every blocked tick, and it falls
// to the lowest counted node's position whenever that node is lower than the
// last tick's minimum — a readmitted node, one restored from an old backup,
// one that came up on its own history because nobody could donate. Records an
// earlier tick licensed removing are gone all the same, so a floor that
// followed the conclusion down would tell exactly that node it holds
// everything that may be missing: a minimum that includes a node can never
// exceed it. That is the vacuous comparand [stateLog.trimFloor] once was,
// arriving by a second route.
//
// # Why a row from a HIGHER generation refuses
//
// This node's positions and this tick's conclusion name a sequence space the
// fleet has left, and publishing over that row would put a floor nobody on the
// live generation can read where theirs was — the reader's own rule in
// [floorFor], applied to the writer. Refusing publishes nothing, and so purges
// nothing: the purge is licensed only by a floor that was published.
//
// A row from a LOWER generation is not carried either: its sequences name a
// dead number space, so this generation's floor starts from what this tick
// licenses and what the stream still holds.
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
			Absent: t.Absent, Detail: t.Detail,
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
// changes, and when it ends — and never on a tick that changed none of those.
//
// # Why transitions and not a line per tick
//
// It was a WARN on every tick of every blocked domain, and a blocked trim is a
// state a healthy fleet lives in for weeks: every fresh deployment waits on its
// first backup and its first donors, and a log nobody writes to is blocked for
// the life of the deployment, since every node sits at position zero on it.
// That was four WARN lines an hour for each such log, for ever, on a fleet with
// nothing wrong — the log-line half of the `trim_blocked` alarm that fired on
// every young fleet. Whether a block has come to COST something is the alarm's
// question, at a threshold the configuration already made; a line's job is to
// say, for whoever reads logs rather than screens, when a block began, what
// holds it, and how long it lasted — which is the alarm tracker's own rate, for
// the tracker's reason: a level repeated every tick buries when it started.
//
// FROM THE PUBLISHED ROW rather than from memory, because the duty moves on a
// lease: a holder that took over mid-block reads the block its predecessor
// published, and says nothing about a block that did not start on its watch —
// which is the reason `blocked_since` is published at all.
//
// BOTH AT WARN, the tracker's pair's level, so a filter that shows a block's
// start shows its end. Splitting them by the term — a term nobody could read at
// WARN, one that permits nothing yet at INFO — reads well until a block changes
// term: one that started on a missing backup and ended on a joining node was a
// WARN start with an INFO end, and a reader filtering on WARN saw a block that
// never ended. What it costs is one WARN, once, for each ordinary block a fresh
// deployment starts with.
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

// holdsFor is every live pin on one domain's log.
//
// BY STREAM NAME, because that is [coord.TrimHold.Streams]' key space — a hold
// is a pin on a log, and both the backup and a joining node state theirs from
// `statelog_cursor`, which is keyed on the stream.
func holdsFor(holds []coord.TrimHold, stream string) []statelog.Hold {
	out := make([]statelog.Hold, 0, len(holds))
	for _, hold := range holds {
		at, pins := hold.Streams[stream]
		if !pins {
			continue
		}
		out = append(out, statelog.Hold{
			Owner: hold.Owner, Generation: at.Generation, Seq: at.Seq, At: hold.At,
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

	newest, ok := r.newestCounted(points)
	if !ok {
		return 0, time.Time{}, 0, false
	}
	reach, covers := newest.Streams[stream]
	if !covers {
		// A BACKUP THAT DOES NOT COVER THIS LOG IS NOT A BACKUP AT
		// SEQUENCE ZERO — it is no backup of this domain at all, which
		// blocks. The distinction is the whole reason the term is
		// three-valued.
		return 0, time.Time{}, 0, false
	}
	return reach.Seq, newest.At, reach.Generation, true
}

// newestCounted is the newest verified backup the operator's policy takes the
// word of — the one point both the trim's backup term and the backup-age alarm
// read, through [coord.CountedBackups], so the alarm can never age a backup the
// trim does not count.
func (r *retention) newestCounted(points []coord.BackupPoint) (coord.BackupPoint, bool) {
	return coord.NewestBackup(coord.CountedBackups(points,
		r.cfg.Floor() == config.BackupFloorOperator))
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
	_ evictionLister = tracker.Domain{}
	_ evictionLister = pages.Domain{}
	_ evictionLister = iamdomain.Domain{}
	_ evictionLister = artifactsdomain.Domain{}
	_ evictionLister = artifactdata.Domain{}
	_ evictionLister = code.Domain{}

	// AND EVERY ONE READS ITS GATE RECORDS OFF THE LOG — an eviction and the
	// readmission that lifts it — which [checkRegister] refuses a registered
	// identity-claiming domain for lacking.
	_ statelog.EvictionProbe = tracker.Domain{}
	_ statelog.EvictionProbe = pages.Domain{}
	_ statelog.EvictionProbe = iamdomain.Domain{}
	_ statelog.EvictionProbe = artifactsdomain.Domain{}
	_ statelog.EvictionProbe = artifactdata.Domain{}
	_ statelog.EvictionProbe = code.Domain{}
)

// tombstones is every eviction this node has applied for one domain's log.
//
// READ FROM THE REPLICATED ROWS rather than from coordination, because that is
// where an eviction actually lives: it is a record on the log like any other,
// so every node's copy is identical and the applier's own gate depends on this
// same table. A second copy in a bucket would give the fleet two answers.
//
// A read failure yields NO tombstones, which is the conservative direction: an
// evicted node stays counted and pins the floor, rather than the trim
// advancing past a node it could not establish was gone.
//
// AND IT SAYS SO: read is false wherever the rows were not read. No tombstone
// is then not an answer about any node — the report prints every node as not
// evicted on that log, and a reader that took that for a readmission released
// an eviction it had just made. A domain that claims no identity carries no
// evictions and was read in full, trivially.
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
// # Why a binary search rather than a stored index
//
// The log is ordered by sequence and its timestamps are the BROKER's own, so
// they are monotone in sequence — which makes "the newest record older than T"
// a binary search over [first, last] with one [jetstream.DomainLog.At] per
// step. At a year-five log that is about twenty-eight reads a quarter of an
// hour, which is cheaper than any index that would have to be maintained on
// every append and correct after every purge.
//
// It answers the FIRST sequence to KEEP, exclusive-style like every other
// term: a log whose newest record is already older than the floor answers
// last+1 (everything may go, as far as this term is concerned), and one whose
// oldest record is younger answers first (nothing may go).
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
// # Why here and not where each number is produced
//
// Every one of them is a fact about a DOMAIN at a moment, and this tick is the
// only place that holds all of them at once: the stream's size and ceiling
// come from one info call, the blocked term from one evaluation, and the
// backup's age from one register read. Setting them anywhere else would mean
// re-fetching, and a gauge set from a second fetch describes a state the
// decision beside it was not taken against.
//
// A NIL RECORDER RECORDS NOTHING, which is a legal deployment: the numbers are
// on the report either way, and a collector is what this adds.
//
// THE HEADROOM IS OF THE CEILING ORDINARY WRITES ARE HELD TO, which on a log
// that keeps a gate reserve is [statelog.Reservation.Bytes] below the broker's —
// the figure the report prints and the alarm fires on, from the one function
// all three read, so a collector's graph reaches zero exactly where writes
// start being refused.
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
