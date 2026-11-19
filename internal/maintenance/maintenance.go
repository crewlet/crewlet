// Package maintenance is the retention sweep for the tables that answer
// "recently" rather than "ever".
//
// Several tables exist to answer a short-horizon question and are written on
// every event that asks it: have I already handled this delivery, how many
// notifications in the last second, has this fire already been claimed, has
// this trigger already been worked, who is on this agent-to-agent channel,
// where is each node on the config pointer. The same sweep keeps each state-log
// domain's operation ledger ([StatelogJobs]) and the coordination store's
// removal markers ([MarkerJobs]), which no clock ages and every listing re-reads.
//
// They were all designed to be swept — every one of their migrations says
// so, and each ships the index a range delete needs. The failure to avoid is
// a `purge` that exists on every store and on the seam, with NOTHING anywhere
// calling it — then all of them grow for the life of the deployment. The event log is the audit trail; these are
// not, and a table nobody sweeps is one that eventually decides how long a
// question takes to answer.
//
// # A fleet singleton
//
// Not because concurrent deletes would corrupt anything — they are
// idempotent range deletes — but because N nodes each scanning and deleting
// the same rows every tick is N times the write amplification for one
// table's worth of benefit. The duty is CLAIMED per tick rather than held,
// the same shape the sandbox waiter uses: a node that dies mid-sweep
// releases it by lapsing, and a peer picks it up on its next tick with no
// handoff protocol.
//
// # The one job that is not a range delete
//
// A removed seat's MAILBOX is retired here too, once the seat has been absent
// from the active revision for [MailboxRetirementGrace]. It is the odd one out
// in three ways, each written down in mailboxes.go: what it deletes is broker
// state (the seat's durable subscriptions and the mail they hold) rather than
// rows; it cannot be judged from a table, so every node records each seat's
// mailbox in the coordination store before creating it, through
// [Mailboxes.Register], which is why this package also serves the node; and a
// delete that cannot be undone is not idempotent in the way a range delete is,
// so it does not lean on the duty at all. Every write is a compare-and-set,
// and the retirement claims the seat's own lease while it deletes.
// A seat whose run waits on a still-running tool call is no failure: the
// mailbox is kept and retried, and said at info ([SeatRunsWaiting]).
//
// A human seat's chat read state is judged the same way and for the same
// reason ([ChatReadJobs], chatreads.go): it lives in a coordination bucket with
// no age, so the record of a seat that left is forgotten only by a decision
// taken against the human seats of the company the fleet is pointed at —
// stamped when a sweep first finds the seat gone, forgotten
// [ChatReadForgetGrace] later, the mailbox's grace, so a seat restored inside
// it gets back its mail and its rooms together.
//
// # Never a replicated row
//
// Every table swept here is the NODE's own, or a broker's. The replicated
// estate is written only by a state log's applier (ADR-0002), so a row of it
// is never deleted here, however short its question: a sweep on each node's
// own tick would leave two copies of one log holding different rows. The
// state logs' arbitration anchors are the case that tested it — one row per
// subject on every node, which a per-node job here once deleted below a
// floor each node read for itself — and the trim now removes them with a
// trimmed record every applier takes at one position (ADR-0080). What such a
// table retains rides a record, never a job of this package's.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("maintenance")

// Interval is how often the sweep runs.
//
// Fifteen minutes, deliberately SHORTER than every retention below it: a tick
// longer than a retention would let a table sit past its horizon. Every sweep
// enforces this ([Worker.horizon] raises a horizon at or below the tick, and
// says so), because two retentions are the company's and an apply can move
// them under a running worker.
//
// It need not be much shorter: these are range deletes on indexed columns, so
// a faster tick buys a slightly smaller table and costs a write burst plus a
// lease round-trip per node, and a node that misses several ticks changes
// nothing an operator would notice.
const Interval = 15 * time.Minute

// DutyFunc claims the single-owner sweep duty for one tick. Nil means "no
// fleet", which is the single-node case: there is nobody to be a singleton
// among.
type DutyFunc func(ctx context.Context) (bool, error)

// Scope says who a job's rows belong to, and therefore where it must run.
//
// An enum, not a bool, because a bool's zero value is a valid SETTING: a job
// that never mentioned it was silently a fleet job, wrong for every node-owned
// table. An enum with no valid zero makes each job answer: is this state the
// FLEET agrees on, or THIS MACHINE's alone? [New] refuses a job that did not.
// The failure is asymmetric: a node-local job left [Fleet] grows a table for
// ever on every node but one, invisibly; a fleet job left [NodeLocal] only
// multiplies idempotent deletes by N. Hence refusal at construction.
type Scope string

const (
	// Fleet is state the whole company agrees on: the sweep runs once
	// across the fleet, under the duty, because N nodes deleting the same
	// rows every tick is N times the write amplification for one table's
	// worth of benefit.
	Fleet Scope = "fleet"

	// NodeLocal is a table each node owns its own copy of: the sweep runs
	// on EVERY node, whether or not this one holds the duty, because a
	// copy nobody sweeps grows for the life of the deployment.
	NodeLocal Scope = "node_local"
)

// Valid reports whether s is one of the two answers.
//
// The zero value is not one of them, deliberately: an unset scope is a job
// whose author did not answer the placement question, and that is the state
// this type exists to make unrepresentable past [New].
func (s Scope) Valid() bool { return s == Fleet || s == NodeLocal }

// Job is one unit of housekeeping.
//
// One shape for both kinds the sweep has: a range delete over a retention
// horizon, and the state change that closes an agent-to-agent channel
// nobody ever answered. Both are "do a bounded amount of tidying and say
// how many rows it touched", and modelling them separately would mean two
// loops, two error paths and two log lines saying the same thing.
type Job struct {
	// Name is what the log calls it — normally the table.
	Name string

	// Horizon is how far back this job keeps rows, asked afresh at every
	// sweep. Nil means the job carries its own (the event log's retention
	// is a property of the log): it is handed the sweep's instant as its
	// cutoff and is exempt from the interval check below.
	Horizon Horizon

	// Run does the work for the tick, reporting rows touched.
	//
	// A job that did part of its work and then failed reports BOTH the rows
	// it touched and the error: the mailbox retirement can retire three
	// seats and fail on a fourth, and the three are real.
	//
	// It takes BOTH times: a range delete needs the cutoff, while closing an
	// abandoned channel needs the cutoff to select it AND now to stamp it
	// closed. The worker derives cutoff from Horizon on every sweep, so a
	// moved or raised horizon takes effect without rebuilding the job.
	Run func(ctx context.Context, now, cutoff time.Time) (int64, error)

	// Scope says whether this job's rows are the fleet's or this node's
	// own, and therefore whether it runs under the duty or on every node.
	//
	// It has no default. [New] refuses a job whose scope is not [Valid]: only
	// the job's author knows the answer, and a wrong guess is invisible (see
	// [Scope]).
	Scope Scope

	// Gate reports whether this job has anything to do at all, so a RARE job
	// costs nothing on the ticks with none: a re-spread walk runs when a
	// project's own row says it needs one, one indexed read where its own
	// selection would be a scan. A nil gate always runs.
	//
	// A gate that ERRORS skips its job and is reported: "could not tell" is
	// not "no work", and treating it so is how a duty stops silently.
	Gate func(ctx context.Context) (bool, error)
}

// Horizon answers how far back a job keeps rows. The worker asks it at EVERY
// sweep, never once.
//
// A FUNCTION RATHER THAN A DURATION, because a horizon can be the COMPANY's
// (the inbox's `inbox.retention_days`) and every apply replaces the company
// under a running engine: read once, it would hold the sweep to the booted
// revision, keeping rows the company asked to forget, or deleting rows it just
// asked to keep. This build's own horizons never move; they take [Fixed].
// An answer of zero or less is REFUSED for that sweep: its cutoff is now, which
// deletes every row. A horizon with a floor applies it inside the function.
type Horizon func() time.Duration

// Fixed is a horizon that never moves: a retention this build decides.
func Fixed(d time.Duration) Horizon { return func() time.Duration { return d } }

// Purge builds a range-delete job over a retention horizon.
//
// scope is a PARAMETER rather than a field the caller may forget, for the
// reason [Scope] gives: the placement question has one right answer per table
// and no safe default, so the constructor asks it rather than assuming it.
func Purge(table string, scope Scope, horizon Horizon, fn func(ctx context.Context, cutoff time.Time) (int64, error)) Job {
	return Job{Name: table, Scope: scope, Horizon: horizon,
		Run: func(ctx context.Context, _, cutoff time.Time) (int64, error) {
			return fn(ctx, cutoff)
		}}
}

// PurgeN adapts a purge that counts in int rather than int64.
//
// Two width conventions exist across the stores, and normalising here beats
// either changing a store's signature to suit its sweeper or making every
// caller remember which is which.
func PurgeN(table string, scope Scope, horizon Horizon, fn func(ctx context.Context, cutoff time.Time) (int, error)) Job {
	return Purge(table, scope, horizon, func(ctx context.Context, cutoff time.Time) (int64, error) {
		n, err := fn(ctx, cutoff)
		return int64(n), err
	})
}

// Options configures a [Worker].
type Options struct {
	// Jobs is what to sweep. An empty list is valid and yields a worker
	// that does nothing — the shape a deployment with no store has, and
	// correct for it: an in-memory twin prunes itself inline, because a
	// process-local map dies with the process.
	Jobs []Job

	// Interval is the tick cadence. Zero takes [Interval].
	Interval time.Duration

	// ClaimDuty gates the tick in a fleet. Nil means single-node.
	ClaimDuty DutyFunc

	// Now is the clock, injectable so a test need not wait a quarter hour.
	Now func() time.Time
}

// Worker sweeps expired rows from the short-horizon tables, fleet-wide once.
type Worker struct {
	jobs      []Job
	interval  time.Duration
	claimDuty DutyFunc
	now       func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	// raised is, per job, the horizon last reported as raised to the
	// tick, so a company whose horizon sits below the tick is told once
	// per value it asks for rather than on every sweep. Guarded by
	// raisedMu: a sweep and a test's direct Tick may overlap.
	raisedMu sync.Mutex
	raised   map[string]time.Duration
}

// New builds a worker, refusing a job that did not say whose rows it sweeps.
//
// The interval-below-every-horizon invariant is enforced per SWEEP, not here,
// because a horizon is asked for at every sweep ([Horizon]): one below the tick
// is RAISED to the tick and logged, not refused. Two horizons are the
// company's, and failing a sweep over a short retention would be a hard stop
// for a soft problem, while silently accepting it would sweep on a schedule
// that cannot honour its horizon. A MALFORMED JOB IS REFUSED RATHER THAN
// DROPPED: skipping a job with no Run or Name is the silence [Scope] describes.
// The error names every offending job, not the first.
func New(opts Options) (*Worker, error) {
	w := &Worker{
		interval:  opts.Interval,
		claimDuty: opts.ClaimDuty,
		now:       opts.Now,
		raised:    map[string]time.Duration{},
	}
	if w.interval <= 0 {
		w.interval = Interval
	}
	if w.now == nil {
		w.now = time.Now
	}
	var bad []string
	for i, j := range opts.Jobs {
		switch {
		case j.Name == "":
			bad = append(bad, fmt.Sprintf("jobs[%d]: no Name — the log has "+
				"nothing to call it and an operator has nothing to grep", i))
			continue
		case j.Run == nil:
			bad = append(bad, fmt.Sprintf("%s: no Run — set one, or do not "+
				"register the job", j.Name))
			continue
		case !j.Scope.Valid():
			bad = append(bad, fmt.Sprintf("%s: Scope is %q — set "+
				"maintenance.Fleet for state the company agrees on, or "+
				"maintenance.NodeLocal for a table each node owns its own "+
				"copy of. A node-local job left unset is swept on one node "+
				"and grows for ever on every peer", j.Name, string(j.Scope)))
			continue
		}
		w.jobs = append(w.jobs, j)
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("maintenance: %d job(s) cannot be registered: %s",
			len(bad), strings.Join(bad, "; "))
	}
	return w, nil
}

// Jobs names what this worker sweeps, for logs and tests.
func (w *Worker) Jobs() []string {
	out := make([]string, 0, len(w.jobs))
	for _, j := range w.jobs {
		out = append(out, j.Name)
	}
	return out
}

// horizon is how far back a job keeps rows on THIS sweep: its own answer,
// asked now, raised to the tick when it asks for no more than the tick.
//
// AT OR BELOW, because [Interval]'s invariant is that the tick is SHORTER than
// every horizon. A horizon exactly on the tick does not move when raised, so
// the warning is the whole behaviour there: a retention on the sweep's own
// cadence is one the SWEEP decides, while the caller sizes a table from it.
//
// A horizon answering zero or less is refused for the sweep: its cutoff is the
// sweep's own instant, which deletes every row, and no configured retention
// means that.
func (w *Worker) horizon(ctx context.Context, j Job) (time.Duration, error) {
	if j.Horizon == nil {
		return 0, nil
	}
	asked := j.Horizon()
	if asked <= 0 {
		return 0, fmt.Errorf("%s: its horizon answered %v, which would delete every "+
			"row it holds; the job was skipped this sweep", j.Name, asked)
	}
	if asked > w.interval {
		return asked, nil
	}
	w.raisedMu.Lock()
	told := w.raised[j.Name] == asked
	w.raised[j.Name] = asked
	w.raisedMu.Unlock()
	if !told {
		log.WarnContext(ctx, "maintenance_horizon_raised_to_the_tick",
			"job", j.Name, "asked", asked.String(), "using", w.interval.String())
	}
	return w.interval, nil
}

// Start runs the sweep loop until Stop or the context ends.
//
// The FIRST tick waits a full interval. A node that has just booted has
// nothing to sweep that a node before it did not, and sweeping at boot means
// a rolling deploy runs the sweep once per replica in quick succession —
// each claiming the duty from the last — which is exactly the write
// amplification the singleton exists to avoid.
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return
	}
	if len(w.jobs) == 0 {
		// Nothing to sweep is not an error and needs no goroutine. Said
		// once at Info so an operator wondering why a table is growing
		// can tell "the sweep is not wired" from "the sweep found
		// nothing", which are otherwise the same silence.
		log.InfoContext(ctx, "maintenance_worker_idle", "reason", "no jobs configured")
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	w.cancel, w.done = cancel, make(chan struct{})
	done := w.done
	go func() {
		defer close(done)
		w.loop(ctx)
	}()
	log.InfoContext(ctx, "maintenance_worker_started",
		"interval", w.interval.String(), "jobs", w.Jobs(),
		"fleet_singleton", w.claimDuty != nil)
}

// Running reports whether the sweep loop is live.
//
// The question that had no answer when the sweep did not exist: a worker
// that was CONSTRUCTED and never started looks identical, from every other
// vantage point, to one that is quietly doing its job.
func (w *Worker) Running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cancel != nil
}

// Stop ends the loop and waits for the in-flight tick to finish.
func (w *Worker) Stop() {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.cancel, w.done = nil, nil
	w.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	log.Info("maintenance_worker_stopped")
}

func (w *Worker) loop(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.WarnContext(ctx, "maintenance_tick_failed", "error", err.Error())
			}
		}
	}
}

// Tick sweeps every job once, reporting rows removed per job.
//
// EVERY JOB RUNS even when one fails: they are independent tables, and letting
// the first failure skip the rest means one unreachable store stops all the
// housekeeping. The errors are joined, and a job that failed part-way still has
// the rows it did touch counted.
//
// Returns a nil map and no error when this node does not hold the duty:
// "somebody else swept" and "nothing needed sweeping" are different facts, and
// an empty map would merge them.
func (w *Worker) Tick(ctx context.Context) (map[string]int64, error) {
	// THE DUTY IS CLAIMED ONCE AND CONSULTED PER JOB. A [NodeLocal] job
	// runs whether or not this node holds it, because the table it sweeps
	// is this node's own — and a [Fleet] job runs only if it does.
	// Skipping the whole tick on a lost claim would let every node's own
	// tables grow for as long as one peer holds the duty.
	holds, err := w.mayTick(ctx)
	if err != nil {
		return nil, err
	}
	now := w.now()
	swept := make(map[string]int64, len(w.jobs))
	var errs []error
	for _, j := range w.jobs {
		if !holds && j.Scope == Fleet {
			continue
		}
		if j.Gate != nil {
			work, err := j.Gate(ctx)
			if err != nil {
				// UNKNOWN IS NOT "NO WORK". A gate that cannot be
				// read is a job that cannot be scheduled, and
				// reading its failure as "nothing to do" is how a
				// duty stops running with nothing to show for it.
				errs = append(errs, fmt.Errorf("%s: read whether there is "+
					"work: %w", j.Name, err))
				continue
			}
			if !work {
				continue
			}
		}
		horizon, err := w.horizon(ctx, j)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		n, err := j.Run(ctx, now, now.Add(-horizon))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", j.Name, err))
		}
		if n > 0 {
			swept[j.Name] = n
		}
	}
	if !holds && len(swept) == 0 && len(errs) == 0 {
		// SOMEBODY ELSE SWEPT THE FLEET'S TABLES and this node's own
		// needed nothing, which is not the same fact as "nothing needed
		// sweeping" — a nil map keeps them apart.
		return nil, nil
	}
	if len(swept) > 0 {
		log.InfoContext(ctx, "maintenance_swept", "rows", total(swept), "tables", swept)
	}
	return swept, errors.Join(errs...)
}

// mayTick reports whether this node holds the sweep duty.
//
// FAILS CLOSED, and unlike most of this package that is the cheap direction:
// skipping a tick costs one interval, which the next tick recovers in full
// because a range delete over a horizon is not incremental. Sweeping without
// the duty is the N-nodes-deleting-the-same-rows case the duty exists for.
func (w *Worker) mayTick(ctx context.Context) (bool, error) {
	if w.claimDuty == nil {
		return true, nil
	}
	holds, err := w.claimDuty(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return false, err
		}
		log.WarnContext(ctx, "maintenance_duty_claim_failed", "error", err.Error())
		return false, nil
	}
	return holds, nil
}

func total(swept map[string]int64) int64 {
	var n int64
	for _, v := range swept {
		n += v
	}
	return n
}
