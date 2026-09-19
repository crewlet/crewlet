// Package maintenance is the retention sweep for the tables that answer
// "recently" rather than "ever".
//
// Several tables exist to answer a short-horizon question and are written on
// every event that asks it: have I already handled this delivery, how many
// notifications in the last second, has this fire already been claimed, has
// this trigger already been worked, who is on this agent-to-agent channel,
// where is each node on the config pointer.
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
// Fifteen minutes, deliberately SHORTER than every retention below it. That
// ordering is the module's invariant: a tick longer than a retention would
// let a table sit past its horizon for the difference, and the retention
// would stop describing the table. [New] enforces it rather than trusting
// it, because the one configurable retention can be set by an operator.
//
// It does not need to be much shorter. These are range deletes on indexed
// columns answering a question nothing reads back, so a faster tick buys
// only a slightly smaller table between runs and costs a write burst plus a
// lease round-trip per node. Tables are sized by their retention, not by
// this interval, so a node that misses several ticks changes nothing an
// operator would notice — which is why the value sits well below the floor
// rather than just under it.
const Interval = 15 * time.Minute

// DutyFunc claims the single-owner sweep duty for one tick. Nil means "no
// fleet", which is the single-node case: there is nobody to be a singleton
// among.
type DutyFunc func(ctx context.Context) (bool, error)

// Scope says who a job's rows belong to, and therefore where it must run.
//
// # Why this is an enum and not a bool
//
// It was `PerNode bool`, and a bool has a zero value that is a valid SETTING
// rather than an absence — so a job that never mentioned it was silently a
// fleet job, which is the wrong answer for every table a node owns its own
// copy of. Six of the seven local sweeps in [StoreJobs], [LearningJobs],
// [CounterpartyJobs], [ScheduleJobs] and [LedgerJobs] took that default by
// omission: `crewlet_events`, `chat_thread_follows`, `agent_diary`,
// `counterparty_profiles`, `scheduled_runs` and `conversation_sessions` were
// swept only on the node holding the duty and grew for ever on every peer.
// The audit log was among them, so the event retention an operator configured
// applied on one node of the fleet.
//
// The two classes are not a spectrum, and the question has exactly one right
// answer per job: is this state the FLEET agrees on, or state THIS MACHINE
// owns alone? An enum with no valid zero makes a new job answer it, which a
// bool cannot — see [New], which refuses a job that did not.
//
// The asymmetry is worth naming, because it decides which way this fails. A
// node-local job left as [Fleet] grows a table for ever on every node but
// one, and looks identical to a sweep that works to the operator who checks
// the node it ran on. A fleet job left as [NodeLocal] costs N times the write
// amplification for one table's worth of benefit and is otherwise correct,
// since these are idempotent range deletes. Neither is acceptable, but only
// the first is invisible — which is why the refusal is at construction rather
// than a warning somebody reads later.
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

	// Horizon is how far back this job keeps rows. Zero means the job
	// carries its own (the event log's retention is a property of the
	// log), and such a job is exempt from the interval check below.
	Horizon time.Duration

	// Run does the work for the tick, reporting rows touched.
	//
	// A job that did part of its work and then failed reports BOTH: the rows
	// it touched and the error. A job that walks records one at a time (the
	// mailbox retirement) can retire three seats and fail on a fourth, and
	// the three are as real as a clean tick's.
	//
	// It takes BOTH times because the two kinds of job need different
	// ones: a range delete needs the cutoff, while closing an abandoned
	// channel needs the cutoff to select it AND now to stamp it closed.
	// The worker derives cutoff from Horizon, so a horizon [New] raises
	// takes effect without a job rebuilding anything.
	Run func(ctx context.Context, now, cutoff time.Time) (int64, error)

	// Scope says whether this job's rows are the fleet's or this node's
	// own, and therefore whether it runs under the duty or on every node.
	//
	// It has no default. [New] refuses a job whose scope is not [Valid],
	// because the answer is a property of the table that only the author of
	// the job knows and the cost of guessing it wrong is invisible — see
	// [Scope] for what the guess cost when the field was a bool.
	Scope Scope

	// Gate reports whether this job has anything to do at all.
	//
	// It exists so a job whose work is RARE can cost nothing on the ticks
	// where there is none: a re-spread walk runs when a project's own row
	// says it needs one, and asking that is one indexed read where doing
	// the walk's own selection would be a scan. A nil gate always runs.
	//
	// A gate that ERRORS skips its job and is reported: "I could not tell
	// whether there is work" is not "there is no work", and treating it as
	// the second is how a duty stops running and nothing says so.
	Gate func(ctx context.Context) (bool, error)
}

// Purge builds a range-delete job over a retention horizon.
//
// scope is a PARAMETER rather than a field the caller may forget, for the
// reason [Scope] gives: the placement question has one right answer per table
// and no safe default, so the constructor asks it rather than assuming it.
func Purge(table string, scope Scope, horizon time.Duration, fn func(ctx context.Context, cutoff time.Time) (int64, error)) Job {
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
func PurgeN(table string, scope Scope, horizon time.Duration, fn func(ctx context.Context, cutoff time.Time) (int, error)) Job {
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
}

// New builds a worker, enforcing the interval-below-every-horizon invariant
// and refusing a job that did not say whose rows it sweeps.
//
// A horizon at or below the tick is RAISED to the tick and logged, rather
// than refused. The one horizon an operator sets is the conversation
// ledger's, and failing a company's boot over a retention that is merely
// shorter than the sweep would be a hard stop for a soft problem — while
// silently accepting it would mean a table swept on a schedule that cannot
// honour its own horizon.
//
// A MALFORMED JOB IS REFUSED RATHER THAN DROPPED, and that is the opposite
// of what this did. It used to skip a job with no Run or no Name and carry
// on, which is the same silence as the one [Scope] describes: the worker
// starts, logs the jobs it kept, and the table the dropped job was for grows
// with nothing anywhere saying a sweep was configured for it. There is no
// caller for whom "I asked for this job and it is not running" is the
// outcome they wanted, so it is an error the wiring has to answer for.
//
// The error names every offending job in one message rather than the first,
// because a caller fixing one and re-running to find the next is the loop
// this is meant to save them.
func New(opts Options) (*Worker, error) {
	w := &Worker{
		interval:  opts.Interval,
		claimDuty: opts.ClaimDuty,
		now:       opts.Now,
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
		if j.Horizon > 0 && j.Horizon < w.interval {
			log.Warn("maintenance_horizon_raised_to_the_tick",
				"job", j.Name, "asked", j.Horizon.String(), "using", w.interval.String())
			j.Horizon = w.interval
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
// EVERY JOB RUNS even when one fails. They are independent tables, and
// letting the first failure skip the rest means one unreachable store stops
// the housekeeping for all of them — the failure mode this package exists to
// fix, arrived at from the other direction. The errors are joined and
// returned together, and a job that failed part-way still has the rows it did
// touch counted: dropping them made a tick that retired a mailbox and then hit
// one unreadable record report that it had retired nothing.
//
// Returns a nil map and no error when this node does not hold the duty:
// "somebody else swept" and "nothing needed sweeping" are different facts,
// and an empty map would merge them.
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
		n, err := j.Run(ctx, now, now.Add(-j.Horizon))
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
