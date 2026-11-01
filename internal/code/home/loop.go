package home

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Backstop is the interval between two of a home's backstop passes, and how
// long one repository's turn may take: ONE MINUTE, the compactor's cadence
// (internal/code/compact's Poll), so a home's background work runs at one
// cadence on every git node. A pass is one indexed listing a worker, and a
// minute is what a nudge lost costs its repository's work at the most.
//
// WHAT A PASS COSTS THE FLEET: the change work's listing walks every live
// change of the company, 215 ms at 21 000 of them (BenchmarkChangesDue,
// internal/code's changesdue.go, whose head states the measure), and every
// git node lists every repository each Backstop — G × 215 ms of its data
// nodes' reads a minute for a fleet of G git nodes, about 1 % of one core for
// three.
const Backstop = time.Minute

// Concurrency is how many repositories one home works at once: TWO. A turn
// spends its time waiting — on its git reads, which [OwnReads] bounds apart,
// and on its records, each one turn of its repository's committer — so a
// second turn overlaps one turn's records with the other's read, and a burst
// of nudges does not queue behind one large repository's records. More turns
// buy nothing a read can use: the reads are bounded by the node's share, not
// by the turns. MEASURED (BenchmarkHomeLoop, home_bench_test.go: one run of
// three bursts a cell, -benchtime 3x, on two cores; a model whose facts read
// holds a unit 20 ms and whose record takes 5 ms): sixteen repositories due
// at once drained in 348 ms at two turns against 493 at one, on a share of
// one unit as of two (332 and 494) — the reads' own floor is 320, sixteen
// reads one at a time — and no faster at four (348, 333), whose turns only
// waited on the same reads. Beside them a seat's read waited at most 19 ms,
// one facts read's hold, on a one-unit share, and 0.03 ms on a two-unit one,
// whatever the turns: [OwnReads], not this, is what a seat waits behind.
// query.MaxPatchSpools, the patches a git node spools at once, is this
// many, since no more of the home's facts reads run at once than its turns
// (engine.TestEveryFactsReadOfAHomeHasASpool).
const Concurrency = 2

// OwnReads is how many of the home's own git reads — a revision's facts, a
// checks file (code.SeatWorkEngine) — one node's loop runs at once, of a seat
// git-work share of units units: ALL BUT ONE, AT LEAST ONE. Each holds a unit
// of the ANSWERING node's share for its git (the home's own, which its git
// walk asks first), and a seat's read that finds the share held waits at most
// code.GitShareWait before it is sent to another git node — which holds no
// copy unless a read already filled one, since the walk fills only its home
// (code.UnreadyShare). So the home leaves its seats a unit wherever the share
// has two, and on a node of one (three cores or fewer) runs one read at a time
// — a seat waiting behind one background read at the most, never behind a
// queue of them, since the turns past the bound wait HERE rather than ahead
// of the seats in the share's own FIFO. None would stop the change work.
func OwnReads(units int) int { return max(1, units-1) }

// Placement answers who is a repository's home — the router satisfies it
// (estate.Router.Home and HomeOf): a nudge's one repository ([Placement.Home]),
// and a pass's whole listing against ONE reading of who serves git
// ([Placement.HomeOf]). An error is UNKNOWN, never "not home": a node that
// cannot tell works nothing rather than guess.
type Placement interface {
	Home(repoID string) (string, error)
	HomeOf(repoIDs []string) (map[string]bool, error)
}

// Nudges is the code log as the home watches it: the subject and position of
// every record appended under a filter, headers alone, and every moment the
// watch may have missed one — internal/queue/jetstream's DomainLog.Watch, its
// sequences as positions on the code log, which the engine adapts.
type Nudges interface {
	Watch(ctx context.Context, filter string, h Nudged) (Watching, error)
}

// Nudged is what a watch tells the loop, ONE CALL AT A TIME and none once
// [Watching.Stop] has returned, each on the queue's delivery path: the subject
// and position of each record appended under the filter, and a position at or
// past every record the watch may have missed.
type Nudged struct {
	Subject func(subject string, at statelog.Position)
	Gap     func(through statelog.Position)
}

// Watching is a watch that has begun, stopped by its caller.
type Watching interface{ Stop() }

// Share is the node's seat git-work share as the loop sizes its own reads by
// it — internal/code's *GitShare satisfies it: how many units it holds.
type Share interface{ Units() int }

// ownReads is the home's Estate with its git reads held to [OwnReads] at
// once — every turn's, of every worker — waiting here, under the turn's
// context, for a read of their own before the read takes the share's unit.
type ownReads struct {
	Estate
	reads *semaphore.Weighted
	n     int
}

// GitRead waits for one of the loop's reads, then reads.
func (o ownReads) GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	if err := o.reads.Acquire(ctx, 1); err != nil {
		return code.GitAnswer{}, fmt.Errorf("home: wait for one of the home's %d git reads at once: %w", o.n, err)
	}
	defer o.reads.Release(1)
	return o.Estate.GitRead(ctx, q)
}

// Worker is one kind of work a repository's home owes, from the rows: the
// repositories it has work in (Due, every page from its cursor, read at floor
// where floor is set and stale where it is zero) and one repository's work
// (Work, its rows read at or past floor). Work runs under the turn's context
// — one [Backstop] at the most — and a repository with more than a turn holds
// is listed again by the next pass.
type Worker interface {
	Name() string
	Due(ctx context.Context, floor statelog.Position, cursor string) (repos []string, next string, err error)
	Work(ctx context.Context, repoID string, floor statelog.Position) error
}

// Options builds a [Loop]. Every field but Metrics, Logger and Now is
// required, and New refuses a missing one by name.
type Options struct {
	// Estate is the code log and the git reads as the home's change work
	// reaches them: the router's code seam.
	Estate Estate
	// Placement answers who is home; Nudges is the code log's watch.
	Placement Placement
	Nudges    Nudges
	// Cache is this node's git cache, a copy of which every merge of the
	// home's runs over in a scratch beside it ([MergeScratchID]), and Git the
	// node's git pool the merge runs in, in its maintenance lane.
	Cache Cache
	Git   Pool
	// Chains builds the merge queue's trains on this node, and Tasks closes
	// a landed change's work item as its enqueuer (queue.go).
	Chains Chains
	Tasks  Tasks
	// Share is this node's seat git-work share, the one its git reads are
	// answered under: the home's own reads run [OwnReads] its units at once.
	Share Share
	// NodeID is this node: the home every placement answer is compared
	// with, and the node every record the home asks names.
	NodeID string
	// Metrics nil records nothing; Logger nil is the package's own
	// (component code.home); Now nil is time.Now.
	Metrics *metrics.Recorder
	Logger  *slog.Logger
	Now     func() time.Time
}

// ErrOptions is an Options value New refuses, wrapped naming every field.
var ErrOptions = errors.New("home: refused options")

// PollReport is what one backstop pass did: how many repositories its
// workers' listings named, and how many of those it scheduled — the ones this
// node is home to.
type PollReport struct {
	Listed, Scheduled int
}

// The outcomes of crewlet.code.home.passes: a repository's turn of a worker
// that did its work, failed, or was cut by its turn's bound; and of
// crewlet.code.home.nudges: a record's repository scheduled here, home
// elsewhere, home unknown — who serves git unreadable — a subject that names
// no repository, and a gap, which schedules a pass.
const (
	outcomeWorked = "worked"
	outcomeFailed = "failed"
	outcomeCut    = "cut"

	nudgeScheduled = "scheduled"
	nudgeElsewhere = "elsewhere"
	nudgeUnknown   = "unknown"
	nudgeIgnored   = "ignored"
	nudgeGap       = "gap"
)

// nudgeFilters are the filters the loop watches the code log under, a watch
// each: every record on a repository's subject — a push, a change's open,
// edit, retarget, close, reopen and landing — and every record on a
// repository's CHECKS subject — an attempt queued, claimed or ended, a
// head's dispatch — which moves the check work and, through the readiness
// a change's head's checks are an input of, the change work.
var nudgeFilters = []string{
	topics.CodeLogPrefix + "." + string(code.KindRepo) + ".>",
	topics.CodeLogPrefix + "." + string(code.KindChecks) + ".>",
}

// Loop is one git node's home work: its registered workers' repositories,
// scheduled by the nudges and the backstop, worked [Concurrency] at a time.
type Loop struct {
	o   Options
	log *slog.Logger
	now func() time.Time
	// backstop is the passes' interval and a turn's bound: [Backstop], a
	// suite's own (export_test.go). ticks, nil in production, is a suite's
	// own pacing of the passes in place of a ticker of backstop
	// (export_test.go), so a case says exactly when a paced pass begins.
	backstop time.Duration
	ticks    <-chan time.Time
	// turnsAtOnce is how many turns run at once: [Concurrency], a
	// benchmark's own (export_test.go).
	turnsAtOnce int
	// changes, checks and merges are the change work, the check work and the
	// merge queue, the three workers every loop is built with.
	changes *changes
	checks  *checks
	merges  *queue

	mu      sync.Mutex
	workers []Worker
	started bool
	// stopped is set once Stop has begun, so a Start after it starts
	// nothing.
	stopped bool
	// pending is every (worker, repository) queued, at the furthest floor
	// it was asked at, and queue their order; busy is every pair a turn is
	// working, and again every busy pair asked for once more meanwhile, at
	// the furthest floor — worked once more when its turn ends.
	pending map[turnKey]statelog.Position
	queue   []turnKey
	busy    map[turnKey]bool
	again   map[turnKey]statelog.Position
	// gap is the furthest position a Gap reached that no pass has read at
	// yet, and watches each filter's watch while it runs — nil for one that
	// has not begun.
	gap     statelog.Position
	watches []Watching

	// ready says a pair is queued, kick that a Gap asks for a pass before
	// its tick; each holds one signal, which is all either needs.
	ready chan struct{}
	kick  chan struct{}

	life context.Context
	end  context.CancelFunc
	wg   sync.WaitGroup
}

// turnKey is one worker's work in one repository.
type turnKey struct {
	worker int
	repo   string
}

// New builds a Loop with the change work, the check work and the merge queue
// registered, refusing every required field missing, by name, in one error.
// It starts nothing: [Loop.Start] does.
func New(o Options) (*Loop, error) {
	var missing []string
	need := func(absent bool, name string) {
		if absent {
			missing = append(missing, name)
		}
	}
	need(o.Estate == nil, "Estate")
	need(o.Placement == nil, "Placement")
	need(o.Nudges == nil, "Nudges")
	need(o.Cache == nil, "Cache")
	need(o.Git == nil, "Git")
	need(o.Chains == nil, "Chains")
	need(o.Tasks == nil, "Tasks")
	need(o.Share == nil || o.Share.Units() < 1, "Share")
	need(!config.ValidNodeID(o.NodeID), "NodeID")
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s missing", ErrOptions, strings.Join(missing, ", "))
	}
	l := &Loop{o: o, log: o.Logger, now: o.Now, backstop: Backstop, turnsAtOnce: Concurrency,
		watches: make([]Watching, len(nudgeFilters)),
		pending: map[turnKey]statelog.Position{}, busy: map[turnKey]bool{}, again: map[turnKey]statelog.Position{},
		ready: make(chan struct{}, 1), kick: make(chan struct{}, 1)}
	if l.log == nil {
		l.log = logging.Get("code.home")
	}
	if l.now == nil {
		l.now = time.Now
	}
	l.life, l.end = context.WithCancel(context.Background())
	// EVERY WORKER READS THROUGH ONE BOUND: the change work's facts reads and
	// the check work's facts and checks-file reads draw on the same units.
	reads := OwnReads(o.Share.Units())
	o.Estate = ownReads{Estate: o.Estate, reads: semaphore.NewWeighted(int64(reads)), n: reads}
	l.changes = &changes{estate: o.Estate, cache: o.Cache, git: o.Git, node: o.NodeID, metrics: o.Metrics,
		log: l.log, now: l.now, rechecks: newRecheckBudget()}
	l.checks = &checks{estate: o.Estate, node: o.NodeID, log: l.log, now: l.now}
	l.merges = &queue{estate: o.Estate, chains: o.Chains, tasks: o.Tasks, checks: l.checks, node: o.NodeID,
		log: l.log, now: l.now}
	l.workers = []Worker{l.changes, l.checks, l.merges}
	return l, nil
}

// AlarmReading fills the reading's merge-rate half from the merge queues this
// node is home to: how many were asked to land more within the window than
// they can, and the one asked furthest past its capacity — what raises
// code_merge_rate_over_capacity. A node home to no queue, or one whose queues
// keep up, leaves the fields zero, which the alarm table reads as nothing
// wrong here. Assigned through the queue worker's own concrete method, so the
// reading's producers gate reaches each field (internal/engine's
// TestEveryAlarmReadingFieldHasAProducer).
func (l *Loop) AlarmReading(out *statelog.Reading) {
	l.merges.AlarmReading(out)
}

// Register adds a worker before the loop starts: its repositories are listed
// at every pass and nudged as every worker's are. A worker named twice, or one
// registered once the loop runs, is refused.
func (l *Loop) Register(w Worker) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.started:
		return fmt.Errorf("home: worker %q registered once the loop runs: register before Start", w.Name())
	case l.named(w.Name()):
		return fmt.Errorf("home: a worker named %q is registered already", w.Name())
	}
	l.workers = append(l.workers, w)
	return nil
}

// named reports a registered worker of that name. Called under mu.
func (l *Loop) named(name string) bool {
	for _, w := range l.workers {
		if w.Name() == name {
			return true
		}
	}
	return false
}

// Start begins the watch, runs a pass at once and then one every [Backstop],
// and works what they schedule, [Concurrency] repositories at a time, on
// goroutines of its own, until [Loop.Stop]. The watch BEGINS BEFORE THE PASS,
// so a record appended in between is told to the watch or is in the rows the
// pass reads; a watch that could not begin is asked again before every pass,
// the backstop doing its work meanwhile. Its caller starts it once the
// runtime its seams read through is published and the node writes: its first
// pass asks who serves git and lists the rows at once, and every turn asks
// records of the repositories' writers. A second Start does nothing, and so
// does one once Stop has begun.
func (l *Loop) Start() {
	l.mu.Lock()
	if l.started || l.stopped {
		l.mu.Unlock()
		return
	}
	l.started = true
	workers := len(l.workers)
	// COUNTED UNDER mu, beside started: a Stop that reads started reads the
	// goroutines it waits for already counted.
	l.wg.Add(1 + l.turnsAtOnce)
	l.mu.Unlock()
	go l.passes()
	for range l.turnsAtOnce {
		go l.turns()
	}
	l.log.Info("code_home_started", "node", l.o.NodeID, "workers", workers, "backstop", l.backstop,
		"concurrency", l.turnsAtOnce, "own_reads", OwnReads(l.o.Share.Units()))
}

// Stop ends the loop: the watch, the passes and every turn in flight — a
// facts read cut, a merge's git ended, a record asked of a committer
// cancelled, which lands or does not on its own — and waits for them to end,
// until ctx does. Called by whoever stops the code host, BEFORE the cache a
// merge reads. Idempotent; a stop of a loop never started ends nothing, and a
// Start after it starts nothing.
func (l *Loop) Stop(ctx context.Context) {
	l.mu.Lock()
	l.stopped = true
	started, watches := l.started, l.watches
	l.watches = make([]Watching, len(nudgeFilters))
	l.mu.Unlock()
	l.end()
	for _, w := range watches {
		if w != nil {
			w.Stop()
		}
	}
	if !started {
		return
	}
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		l.log.Warn("code_home_stop_unfinished", "error", ctx.Err(),
			"detail", "a repository's turn was still running when the stop's wait ended")
	}
}

// passes runs a pass at once and then at every tick, or at once again where a
// Gap asks for one, each after asking for the watch where none runs. The
// first pass and every tick's are PACED, and renew each repository's share of
// a pass's rechecks before they list ([recheckBudget]); a pass a Gap asks
// early renews nothing.
func (l *Loop) passes() {
	defer l.wg.Done()
	ticks := l.ticks
	if ticks == nil {
		ticker := time.NewTicker(l.backstop)
		defer ticker.Stop()
		ticks = ticker.C
	}
	paced := true
	for {
		l.ensureWatch()
		if paced {
			l.changes.rechecks.newPass()
		}
		floor := l.takeGap()
		if _, err := l.Poll(l.life, floor); err != nil && l.life.Err() == nil {
			l.log.Warn("code_home_pass_failed", "error", err)
		}
		select {
		case <-l.life.Done():
			return
		case <-ticks:
			paced = true
		case <-l.kick:
			paced = false
		}
	}
}

// ensureWatch begins each filter's watch where none runs, and leaves none
// where it cannot begin — the backstop passes the work meanwhile, and the
// next pass asks again.
//
// DECIDED BY stopped, UNDER mu, NEVER BY THE LIFE'S END: Stop sets stopped and
// takes the watches under mu, and ends the life only after it lets mu go, so a
// watch that began in between would find the life still running, and stored
// once Stop had read the watches, nothing would ever stop it — a consumer of
// the code log, and its supervision, calling a stopped loop until the queue's
// connection closed.
func (l *Loop) ensureWatch() {
	for i, filter := range nudgeFilters {
		l.mu.Lock()
		running, stopped := l.watches[i] != nil, l.stopped
		l.mu.Unlock()
		if stopped {
			return
		}
		if running {
			continue
		}
		w, err := l.o.Nudges.Watch(l.life, filter, Nudged{Subject: l.nudged, Gap: l.gapped})
		if err != nil {
			if l.life.Err() == nil {
				l.log.Warn("code_home_watch_failed", "filter", filter, "error", err, "detail", "the home's "+
					"work waits for its backstop, a minute at the most, until the watch begins")
			}
			continue
		}
		l.mu.Lock()
		if l.stopped {
			// STOPPED WHILE IT BEGAN: Stop took the watches before this one
			// was stored, so it is stopped here.
			l.mu.Unlock()
			w.Stop()
			return
		}
		l.watches[i] = w
		l.mu.Unlock()
	}
}

// takeGap is the furthest position a Gap reached since the last pass, zero
// for none, and forgets it: the pass about to run reads at it.
func (l *Loop) takeGap() statelog.Position {
	l.mu.Lock()
	defer l.mu.Unlock()
	floor := l.gap
	l.gap = statelog.Position{}
	return floor
}

// Poll is one backstop pass: every worker's listing, every page, read at
// floor — a Gap's position, so the rows hold what the watch may have missed —
// or stale where it is zero; who serves git, ONE READING for every repository
// the pass lists; and every listed repository this node is home to scheduled
// for that worker, at floor. A placement that cannot be read schedules
// nothing that pass; a worker whose listing fails is skipped, the others
// listed. Logged code_home_pass, at DEBUG unless it scheduled something.
func (l *Loop) Poll(ctx context.Context, floor statelog.Position) (PollReport, error) {
	began := l.now()
	l.mu.Lock()
	workers := l.workers
	l.mu.Unlock()
	var (
		report PollReport
		failed []error
		every  []string
	)
	listed := make([][]string, len(workers))
	seen := map[string]bool{}
	for i, w := range workers {
		ids, err := listing(ctx, w, floor)
		if err != nil {
			failed = append(failed, fmt.Errorf("home: list the repositories %s work is due in: %w", w.Name(), err))
			continue
		}
		listed[i] = ids
		report.Listed += len(ids)
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				every = append(every, id)
			}
		}
	}
	if len(every) > 0 {
		homes, err := l.o.Placement.HomeOf(every)
		if err != nil {
			failed = append(failed, fmt.Errorf("home: who serves git could not be read, so this pass schedules "+
				"nothing: %w", err))
			return report, errors.Join(failed...)
		}
		for i, ids := range listed {
			for _, id := range ids {
				if homes[id] {
					l.schedule(i, id, floor)
					report.Scheduled++
				}
			}
		}
	}
	log := l.log.Debug
	if report.Scheduled > 0 {
		log = l.log.Info
	}
	log("code_home_pass", "listed", report.Listed, "scheduled", report.Scheduled, "floor", floor.String(),
		"took", l.now().Sub(began))
	return report, errors.Join(failed...)
}

// listing is every repository w's listing names, every page.
func listing(ctx context.Context, w Worker, floor statelog.Position) ([]string, error) {
	var out []string
	cursor := ""
	for {
		ids, next, err := w.Due(ctx, floor, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, ids...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// nudged is a watch's word that a record landed on subject at at: the
// repository it names — a repository's subject, or its checks' — scheduled
// for every worker at that position, where this node is its home. Called on the queue's delivery path, so it schedules work
// and does none.
func (l *Loop) nudged(subject string, at statelog.Position) {
	kind, repoID, ok := topics.CodeLogPath(subject)
	if !ok || (kind != string(code.KindRepo) && kind != string(code.KindChecks)) {
		l.nudge(nudgeIgnored)
		return
	}
	home, err := l.o.Placement.Home(repoID)
	switch {
	case err != nil:
		l.nudge(nudgeUnknown)
		l.log.Debug("code_home_nudge_unplaced", "repo", repoID, "at", at.String(), "error", err,
			"detail", "who serves git could not be read; the backstop lists the repository at its next pass")
		return
	case home != l.o.NodeID:
		l.nudge(nudgeElsewhere)
		return
	}
	l.mu.Lock()
	workers := len(l.workers)
	l.mu.Unlock()
	for i := range workers {
		l.schedule(i, repoID, at)
	}
	l.nudge(nudgeScheduled)
}

// gapped is the watch's word that it may have missed records up to through:
// a pass is asked for at once, reading at through.
func (l *Loop) gapped(through statelog.Position) {
	l.mu.Lock()
	l.gap = further(l.gap, through)
	l.mu.Unlock()
	l.nudge(nudgeGap)
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// nudge counts one of the watch's words.
func (l *Loop) nudge(outcome string) {
	if l.o.Metrics != nil {
		l.o.Metrics.Add(metrics.CodeHomeNudges, 1, metrics.Attrs{"outcome": outcome})
	}
}

// schedule queues worker's work in repoID at floor: once, at the furthest
// floor it was asked at — and, where a turn is working it now, once more
// after that turn.
func (l *Loop) schedule(worker int, repoID string, floor statelog.Position) {
	k := turnKey{worker: worker, repo: repoID}
	l.mu.Lock()
	switch held, queued := l.pending[k]; {
	case l.busy[k]:
		l.again[k] = further(l.again[k], floor)
	case queued:
		l.pending[k] = further(held, floor)
	default:
		l.pending[k] = floor
		l.queue = append(l.queue, k)
	}
	l.mu.Unlock()
	l.signal()
}

// signal says a pair is queued.
func (l *Loop) signal() {
	select {
	case l.ready <- struct{}{}:
	default:
	}
}

// turns works the queue, one pair at a time, until the loop stops.
func (l *Loop) turns() {
	defer l.wg.Done()
	for {
		k, floor, ok := l.next()
		if !ok {
			select {
			case <-l.life.Done():
				return
			case <-l.ready:
			}
			continue
		}
		l.turn(k, floor)
	}
}

// next takes the queue's first pair and marks it busy — signalling again where
// more are queued, so another turn takes the next rather than waiting for this
// one's — or answers false for an empty queue or a stopped loop.
func (l *Loop) next() (turnKey, statelog.Position, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) == 0 || l.life.Err() != nil {
		return turnKey{}, statelog.Position{}, false
	}
	k := l.queue[0]
	l.queue = l.queue[1:]
	floor := l.pending[k]
	delete(l.pending, k)
	l.busy[k] = true
	if len(l.queue) > 0 {
		l.signal()
	}
	return k, floor, true
}

// turn works one pair under a context one [Backstop] long, counts it, and
// queues it once more where it was asked for again meanwhile.
func (l *Loop) turn(k turnKey, floor statelog.Position) {
	l.mu.Lock()
	w := l.workers[k.worker]
	l.mu.Unlock()
	ctx, cancel := context.WithTimeout(l.life, l.backstop)
	began := l.now()
	err := w.Work(ctx, k.repo, floor)
	cut := ctx.Err() != nil && l.life.Err() == nil
	cancel()
	outcome := outcomeWorked
	switch {
	case cut:
		outcome = outcomeCut
		l.log.Info("code_home_turn_cut", "worker", w.Name(), "repo", k.repo, "took", l.now().Sub(began),
			"detail", "the repository's work outlasted its turn; the next pass lists what is left of it")
	case err != nil && l.life.Err() == nil:
		outcome = outcomeFailed
		l.log.Warn("code_home_turn_failed", "worker", w.Name(), "repo", k.repo, "error", err,
			"detail", "the next pass lists what is left of the repository's work")
	}
	if l.o.Metrics != nil && l.life.Err() == nil {
		l.o.Metrics.Add(metrics.CodeHomePasses, 1, metrics.Attrs{"worker": w.Name(), "outcome": outcome})
	}
	l.mu.Lock()
	delete(l.busy, k)
	if again, asked := l.again[k]; asked {
		delete(l.again, k)
		l.pending[k] = again
		l.queue = append(l.queue, k)
		l.signal()
	}
	l.mu.Unlock()
}

// further is the later of two positions on the code log — the zero position
// earliest — which is the floor a pair asked twice is read at.
func further(a, b statelog.Position) statelog.Position {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case b.Packed() > a.Packed():
		return b
	}
	return a
}
