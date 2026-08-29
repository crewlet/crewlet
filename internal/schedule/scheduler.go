// Package schedule is the scheduler: role- and unit-scoped cron-style
// recurring work.
//
// A seat — or a whole unit — owns recurring work without an external cron
// emitting webhooks into the engine. A QA engineer runs a smoke suite every
// morning, a team holds an async standup at 09:30, a knowledge agent audits
// the wiki weekly, all declared in the org config beside everything else about
// that seat.
//
// # The shape
//
// One loop ticks on a short interval. Each tick reads the LIVE org, works out
// which schedules became due since the previous tick, resolves each due fire's
// runner seats, CLAIMS the fire in the dispatch ledger, and publishes an
// ordinary TaskAssigned into the runner's inbox. Nothing about the agent
// runtime path differs from work that arrived any other way — a scheduled turn
// runs Plan → Execute → Review like any other, and the whole learning loop
// runs on it. Periodic work is more worth learning from, not less.
//
// # The three things that carry the correctness
//
//  1. AT-MOST-ONCE, and it lives in the ledger, not here. Every fire is
//     claimed before it is published, on an identity whose fire label is the
//     LOCAL wall-clock minute. See [Claimer] and [FireLabel].
//
//  2. THE WINDOW IS NEVER ADVANCED BY A TICK THAT DID NOT EVALUATE IT. A node
//     that is shedding, or that does not hold the duty, leaves its window open
//     — so if it later starts evaluating, the catchup pass covers what it
//     skipped rather than a window stretching back to boot.
//
//  3. FAILURE IS PER-FIRE. One unparseable cron, one decommissioned runner,
//     one broker refusal must not stop the other nineteen schedules. Every
//     error path in a tick logs and continues.
//
// # What it is NOT
//
// Not a job runner: the scheduler always dispatches to an agent turn, and a
// cheap monitor is a low-budget seat rather than a script here. Not a delivery
// router either — the runner already owns its outbound surfaces, so where the
// result goes is the agent's business.
package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

var log = logging.Get("schedule.scheduler")

// Defaults, and the argument for each.
const (
	// DefaultTimezone is the zone a schedule that names none is evaluated
	// in, and what a Scheduler built without one uses.
	DefaultTimezone = "UTC"

	// DefaultTick is the poll interval.
	//
	// Cron fires at minute granularity, so any interval under a minute
	// catches every fire; what the value buys is how LATE a fire can be,
	// and at 10 s that is seconds rather than a minute. It is also the
	// claim rate for the fleet-singleton duty, which is one lease call per
	// node per tick — cheap enough that the latency argument wins.
	DefaultTick = 10 * time.Second

	// MaxTick is the ceiling config validation enforces, restated here
	// because a Scheduler can be built programmatically. At or above a
	// minute a tick can step over a whole cron minute, which turns "a fire
	// is late" into "a fire never happened".
	MaxTick = time.Minute

	// DefaultCatchupMin and DefaultCatchupMax clamp the missed-tick window.
	//
	// Two minutes to two hours. The floor keeps a restart from re-firing
	// something that has only just run; the ceiling is what stops a morning
	// restart from replaying the whole night. Between them the window is
	// half the schedule's own period, so a five-minute schedule catches up
	// a fire missed by two minutes and a daily one does not catch up
	// yesterday's.
	DefaultCatchupMin = 2 * time.Minute
	DefaultCatchupMax = 2 * time.Hour

	// DutyName is the singleton duty the fleet claims through
	// coord.WorkerResource.
	DutyName = "scheduler"
)

// Publisher is the queue surface the scheduler needs: one call.
//
// Consumer-defined and deliberately tiny. The scheduler publishes and never
// subscribes, acks, pauses or detaches, and a dispatcher holding the whole
// EventQueue is a dispatcher whose blast radius nobody can see from its type.
type Publisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// DutyFunc reports whether this node holds the scheduler duty for this tick.
//
// It is CLAIMED PER TICK rather than held, so a node that dies between ticks
// releases the duty by lapsing and needs no failure detector. Nil means there
// is no fleet to be a singleton within — the single-node case, which always
// holds it.
//
// The tri-state is the return signature: (true, nil) holds, (false, nil) is a
// peer holding it, and any error is UNKNOWN. A tick treats unknown as "do not
// fire", which is the fail-closed direction: an unreadable lease store says
// nothing about who holds the duty, and firing on that basis puts every node
// back to racing the ledger claim — the exact duplicated work the duty exists
// to remove.
type DutyFunc func(ctx context.Context) (bool, error)

// Options configures a [Scheduler]. Everything but the first three has a
// working default.
type Options struct {
	// Publisher is where fires go. Required.
	Publisher Publisher

	// Org reads the LIVE company, called fresh on every tick so hot reload
	// costs nothing. Required.
	//
	// A function rather than a value because the org is swapped, never
	// edited: holding the pointer would pin a Scheduler to the company that
	// existed when it was built.
	Org func() *org.Organization

	// Ledger is the at-most-once claim. Required — there is no memory
	// default here on purpose: silently accepting no ledger is how a fleet
	// ends up firing every schedule once per node.
	Ledger Claimer

	// DefaultTimezone applies to any schedule that names none.
	DefaultTimezone string

	// Tick is the poll interval; zero takes [DefaultTick] and anything at
	// or above [MaxTick] is refused.
	Tick time.Duration

	// Jitter spreads schedules that share a popular cron minute — everyone
	// writes `0 9 * * *`. Each schedule gets a DETERMINISTIC offset in
	// [0, Jitter] derived from its scope and name, so the 9am wave fans out
	// instead of arriving at once.
	//
	// Quantised to whole seconds, because the canonical fire MINUTE is what
	// forms the identity and sub-second spread buys nothing a queue does not
	// already absorb. Zero fires exactly on the minute, which is the
	// default: the concurrency controller already queues a burst fairly, so
	// this is smoothing, not correctness.
	Jitter time.Duration

	// CatchupMin and CatchupMax clamp the missed-tick window.
	CatchupMin time.Duration
	CatchupMax time.Duration

	// Admits gates the tick on config posture — a node that cannot apply
	// the current epoch must not fire the previous company's schedules.
	// Nil always admits.
	//
	// A schedule's fire identity comes from the ORG: its name, its cron, its
	// target seat. A node serving a stale epoch would fire crons that were
	// edited, seats that were deleted, schedules that were removed outright.
	// Unlike a delivery there is no queued copy to fall back on, which is
	// why a shedding tick skips WHOLE rather than firing something and
	// letting a peer sort it out.
	Admits func() bool

	// Duty is the fleet-singleton claim; nil means single-node. See
	// [DutyFunc] and [ClaimDuty].
	Duty DutyFunc

	// Trace mints the trace context each fire runs under; nil uses a
	// built-in W3C-shaped random minter.
	//
	// A seam rather than a hard dependency because this build has no
	// telemetry package yet. Whatever provides one later wires it here, and
	// nothing else in the scheduler changes.
	Trace func() events.TraceContext
}

// Scheduler dispatches role- and unit-scoped recurring work.
type Scheduler struct {
	pub       Publisher
	org       func() *org.Organization
	ledger    Claimer
	defaultTZ string
	tick      time.Duration
	jitter    time.Duration

	catchupMin time.Duration
	catchupMax time.Duration

	admits func() bool
	duty   DutyFunc
	trace  func() events.TraceContext

	// mu serialises ticks and guards lastTick.
	//
	// Two overlapping ticks would evaluate one window twice; the ledger
	// would absorb the duplicate fires, but the second tick would then
	// advance the window past what the first was still working on. Holding
	// the lock for the whole tick is also what makes Tick safe to call from
	// a test while Run is going.
	mu sync.Mutex

	// lastTick is the end of the window the previous evaluated tick closed.
	// Zero means no tick has evaluated anything yet, which is what triggers
	// the missed-tick catchup pass.
	lastTick time.Time
}

// Errors from [New]. Each names a wiring mistake that would otherwise present
// as a company whose schedules quietly never run.
var (
	ErrNoPublisher = errors.New("schedule: no publisher")
	ErrNoOrg       = errors.New("schedule: no org provider")
	ErrNoLedger    = errors.New("schedule: no dispatch ledger")
	ErrTickTooLong = errors.New("schedule: tick interval is a minute or more")
)

// New builds a Scheduler, applying defaults and refusing an unusable wiring.
func New(opts Options) (*Scheduler, error) {
	switch {
	case opts.Publisher == nil:
		return nil, ErrNoPublisher
	case opts.Org == nil:
		return nil, ErrNoOrg
	case opts.Ledger == nil:
		// Refused rather than defaulted to the memory twin. A scheduler
		// with a process-local claim looks identical to a correct one until
		// there are two nodes, and then every company gets two standups.
		return nil, ErrNoLedger
	case opts.Tick >= MaxTick:
		return nil, fmt.Errorf("%w: %v — cron fires at minute granularity, so a tick that "+
			"long steps over fires rather than delaying them", ErrTickTooLong, opts.Tick)
	}

	s := &Scheduler{
		pub:        opts.Publisher,
		org:        opts.Org,
		ledger:     opts.Ledger,
		defaultTZ:  cmpOr(opts.DefaultTimezone, DefaultTimezone),
		tick:       durOr(opts.Tick, DefaultTick),
		jitter:     max(opts.Jitter, 0),
		catchupMin: durOr(opts.CatchupMin, DefaultCatchupMin),
		catchupMax: durOr(opts.CatchupMax, DefaultCatchupMax),
		admits:     opts.Admits,
		duty:       opts.Duty,
		trace:      cmpOrFunc(opts.Trace, newTrace),
	}
	// A max below the min is a config the operator did not mean; taking the
	// min is the conservative reading, since it can only shorten the window
	// in which a missed fire is replayed.
	s.catchupMax = max(s.catchupMax, s.catchupMin)
	return s, nil
}

// Run ticks until ctx is done.
//
// It has no failure mode of its own and so returns nothing: every failure a
// tick can have belongs to ONE schedule and is logged there, because one
// broken cron must not stop the other nineteen. A caller wanting an errgroup
// signature wraps it.
//
// The first tick lands one interval in, not immediately. That is deliberate:
// the first tick is the one that evaluates missed-tick catchup, and running it
// before the rest of the engine has come up would dispatch into a queue whose
// subscriptions do not exist yet.
func (s *Scheduler) Run(ctx context.Context) {
	log.Info("scheduler_started",
		"tick_seconds", s.tick.Seconds(),
		"default_timezone", s.defaultTZ,
		"jitter_seconds", s.jitter.Seconds(),
		"fleet_singleton", s.duty != nil)
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("scheduler_stopped")
			return
		case <-ticker.C:
			s.Tick(ctx, time.Time{})
		}
	}
}

// RetentionFloor is the shortest retention the dispatch ledger may be swept
// with, and exists so a sweeper can ask rather than assume.
//
// It is the catchup window's upper clamp. Anything a tick could still evaluate
// must still have its row: deleting one inside the window does not merely lose
// history, it lets that fire run a second time.
func (s *Scheduler) RetentionFloor() time.Duration { return s.catchupMax }

// Tick evaluates every schedule once and dispatches what is due, returning how
// many fires were published.
//
// A zero `now` reads the clock. Exposed so a caller can drive a tick without
// waiting on the interval, and so tests do not have to sleep through one.
func (s *Scheduler) Tick(ctx context.Context, at time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.admits != nil && !s.admits() {
		// Deliberately WITHOUT advancing lastTick: the skipped window stays
		// open, so once this node converges the catchup pass evaluates it.
		// Anything a peer already fired is absorbed by the ledger claim, and
		// anything nobody fired still runs.
		log.Info("scheduler_tick_shed")
		return 0
	}

	if !s.holdsDuty(ctx) {
		// The same rule, for the same reason. A node that never wins the
		// duty therefore stays on its FIRST tick, so if it ever does win one
		// it evaluates the catchup window rather than a window stretching
		// back to boot.
		//
		// The tick is a singleton because it is pure duplicated work, not
		// because a peer's tick would be wrong: every node enumerates every
		// schedule, and all but one lose the claim race on every due fire.
		log.Debug("scheduler_tick_not_this_node")
		return 0
	}

	if at.IsZero() {
		at = now()
	} else {
		at = at.UTC()
	}

	first := s.lastTick.IsZero()
	windowStart := s.lastTick
	company := s.org()

	fired := 0
	for _, entry := range Entries(company) {
		fired += s.evaluate(ctx, company, entry, at, windowStart, first)
	}

	s.lastTick = at
	return fired
}

// evaluate runs one schedule's share of a tick, returning how many fires it
// published. Every failure inside it is logged and swallowed: a tick evaluates
// every schedule, and one that gave up on the first bad cron would let a typo
// stop a company's whole ritual calendar.
func (s *Scheduler) evaluate(ctx context.Context, company *org.Organization, e Entry, at, windowStart time.Time, first bool) int {
	sch := e.Schedule
	if !sch.IsEnabled() {
		return 0
	}
	zone := cmpOr(sch.Timezone, s.defaultTZ)
	loc, err := time.LoadLocation(zone)
	if err != nil {
		log.Error("schedule_parse_failed", "schedule", sch.Name, "scope_type", e.Scope,
			"scope_id", e.ScopeID, "timezone", zone, "error", err)
		return 0
	}
	cron, err := Parse(sch.Cron)
	if err != nil {
		log.Error("schedule_parse_failed", "schedule", sch.Name, "scope_type", e.Scope,
			"scope_id", e.ScopeID, "cron", sch.Cron, "error", err)
		return 0
	}

	toFire, toSkip := s.due(cron, loc, e, at, windowStart, first)
	for _, ft := range toSkip {
		s.recordSkip(ctx, e, ft, loc)
	}
	if len(toFire) == 0 {
		return 0
	}

	runners := e.Runners(company)
	if len(runners) == 0 {
		log.Warn("schedule_no_runners", "schedule", sch.Name, "scope_type", e.Scope,
			"scope_id", e.ScopeID, "target", sch.Target)
		return 0
	}

	fired := 0
	for _, ft := range toFire {
		for _, handle := range runners {
			// One member's failure never aborts the fan-out: each runner
			// has its own identity in the ledger, so a slow or broken seat
			// cannot suppress its colleagues' standups.
			if s.fire(ctx, company, e, handle, ft, loc) {
				fired++
			}
		}
	}
	return fired
}

// due returns the canonical UTC fire times this tick should dispatch, and the
// ones it should record as a deliberate catchup skip.
//
// The jitter shifts each schedule's EFFECTIVE firing instant by a
// deterministic offset so a popular cron minute does not stampede. The
// canonical, unshifted fire time is what forms the identity, so dedupe is
// unaffected and two nodes with the same config compute the same offset.
func (s *Scheduler) due(cron Expr, loc *time.Location, e Entry, at, windowStart time.Time, first bool) (toFire, toSkip []time.Time) {
	jitter := s.jitterFor(e.ScopeID, e.Schedule.Name)
	effNow := at.Add(-jitter)

	if !first {
		if windowStart.IsZero() {
			return nil, nil
		}
		return cron.FireTimes(windowStart.Add(-jitter), effNow, loc), nil
	}

	// First evaluated tick after (re)start: consider exactly ONE missed
	// fire, the most recent. Older misses are never backfilled — a company
	// that was down for a day does not want yesterday's standups.
	prev, ok := cron.Prev(effNow, loc)
	if !ok || !e.Schedule.CatchesUp() {
		return nil, nil
	}
	if age := effNow.Sub(prev); age <= s.catchupWindow(cron, loc, effNow) {
		return []time.Time{prev}, nil
	}
	return nil, []time.Time{prev}
}

// catchupWindow is half the schedule's own period, clamped.
//
// Half, because a fire more than half a period old is closer to the NEXT one
// than to the one it would replay — running it then would put two fires within
// half a period of each other, which is exactly what a period is meant to
// prevent. It is a bounded heuristic and says so: for an irregular cadence
// (`0 9 * * 1,5`) the interval between the next two fires approximates the
// period rather than equalling it.
func (s *Scheduler) catchupWindow(cron Expr, loc *time.Location, ref time.Time) time.Duration {
	t1, ok := cron.Next(ref, loc)
	if !ok {
		return s.catchupMin
	}
	t2, ok := cron.Next(t1, loc)
	if !ok {
		return s.catchupMin
	}
	return min(max(t2.Sub(t1)/2, s.catchupMin), s.catchupMax)
}

// jitterFor is a schedule's deterministic offset within the jitter window.
//
// Derived from the scope and name rather than randomised, so every node
// computes the same offset for the same schedule — a random one would put two
// nodes' effective windows out of step and let a fire fall between them.
func (s *Scheduler) jitterFor(scopeID, name string) time.Duration {
	seconds := int64(s.jitter / time.Second)
	if seconds <= 0 {
		return 0
	}
	digest := sha256.Sum256([]byte(scopeID + ":" + name))
	// Masked to 63 bits so the modulus operates on a non-negative value; a
	// signed conversion of the raw eight bytes is negative half the time.
	n := int64(binary.BigEndian.Uint64(digest[:8]) &^ (1 << 63))
	return time.Duration(n%(seconds+1)) * time.Second
}

// fire claims one dispatch and publishes it, reporting whether it went out.
func (s *Scheduler) fire(ctx context.Context, company *org.Organization, e Entry, handle string, fireUTC time.Time, loc *time.Location) bool {
	// The runner is resolved from the ORG, never from a local agent pool. A
	// schedule fires into a seat's inbox and the node that OWNS that seat
	// consumes it, which is usually not the node whose tick won the duty.
	// Asking a pool made a fire conditional on the runner happening to run
	// here — every node reached the same conclusion, and the schedule simply
	// never ran.
	seat := company.AgentSeatByHandle(handle)
	if seat == nil {
		// A handle naming no agent seat in the current org: a decommissioned
		// role, or a config edit that landed between runner resolution and
		// this call. Do NOT claim — a later tick against a corrected org
		// should still be able to fire.
		log.Warn("schedule_runner_not_found", "handle", handle,
			"schedule", e.Schedule.Name, "scope_id", e.ScopeID)
		return false
	}
	agentID, ok := company.AgentIDFor(seat)
	if !ok {
		log.Warn("schedule_runner_not_found", "handle", handle,
			"schedule", e.Schedule.Name, "scope_id", e.ScopeID,
			"reason", "the seat has no derivable agent id")
		return false
	}

	label := FireLabel(fireUTC, loc)
	// Each dispatched run gets its OWN trace, detached from the tick, so the
	// ledger row and exactly this turn's calls are linked. The TaskAssigned
	// carries it and the agent's turn restores it.
	trace := s.trace()

	claimed, err := s.ledger.Claim(ctx, Run{
		FireKey: FireKey{
			Scope:        e.Scope,
			ScopeID:      e.ScopeID,
			ScheduleName: e.Schedule.Name,
			FireLabel:    label,
			TargetHandle: handle,
		},
		ScheduledAt: fireUTC,
		Outcome:     OutcomeFired,
		TraceID:     trace.TraceID,
	})
	if err != nil {
		// Unknown, and the fail-closed direction is to skip: firing on an
		// unreadable ledger is how one fire becomes N.
		log.Error("schedule_claim_failed", "schedule", e.Schedule.Name,
			"handle", handle, "fire_label", label, "error", err)
		return false
	}
	if !claimed {
		return false
	}

	runID := runID(e, label, handle)
	task := events.New(types.TaskAssigned{
		TaskID:   runID,
		Agent:    agentID.String(),
		RoleName: seat.Name,
	}, trace)
	task.Source = "scheduler"
	task.Payload = map[string]any{
		"task_description": e.Schedule.Task,
		"scheduled":        true,
		"schedule_name":    e.Schedule.Name,
		"scope_type":       string(e.Scope),
		"scope_id":         e.ScopeID,
		"timeout_seconds":  int(e.Schedule.Timeout() / time.Second),
		"run_id":           runID,
	}

	if err := s.pub.Publish(ctx, topics.AgentInbox(handle), task); err != nil {
		// The claim is already spent, and that is the at-most-once side of
		// the trade rather than an oversight: a publish failure is
		// AMBIGUOUS — the broker may have persisted the event and lost the
		// acknowledgement — so releasing the claim to retry would risk
		// waking the seat twice. The fire is dropped, loudly.
		log.Error("schedule_publish_failed", "schedule", e.Schedule.Name, "handle", handle,
			"fire_label", label, "error", err,
			"hint", "the fire is CLAIMED and will not be retried; the ledger row records a "+
				"dispatch that never reached the seat")
		return false
	}

	log.Info("schedule_fired", "schedule", e.Schedule.Name, "scope_type", e.Scope,
		"scope_id", e.ScopeID, "handle", handle,
		"scheduled_at", fireUTC.Format(time.RFC3339), "trace_id", trace.TraceID)

	// Observability only, and best-effort: the seat has already been woken,
	// so a failure here costs a dashboard row rather than a run.
	fired := events.New(types.ScheduledTaskFired{
		ScopeType:    e.Scope,
		ScopeID:      e.ScopeID,
		ScheduleName: e.Schedule.Name,
		TargetHandle: handle,
		ScheduledAt:  fireUTC.Format(time.RFC3339),
	}, trace)
	fired.Source = "scheduler"
	if err := s.pub.Publish(ctx, topics.Event(fired.Type), fired); err != nil {
		log.Error("scheduled_task_fired_publish_failed", "schedule", e.Schedule.Name, "error", err)
	}
	return true
}

// recordSkip notes, once, that a missed fire was deliberately not backfilled.
//
// The row carries an EMPTY runner handle, which is what keeps it from
// colliding with a fire row for the same minute — every real fire names a
// runner. Without that, recording the skip would consume the claim a later
// corrected tick needs.
func (s *Scheduler) recordSkip(ctx context.Context, e Entry, fireUTC time.Time, loc *time.Location) {
	if _, err := s.ledger.Claim(ctx, Run{
		FireKey: FireKey{
			Scope:        e.Scope,
			ScopeID:      e.ScopeID,
			ScheduleName: e.Schedule.Name,
			FireLabel:    FireLabel(fireUTC, loc),
		},
		ScheduledAt: fireUTC,
		Outcome:     OutcomeSkippedCatchup,
	}); err != nil {
		log.Error("schedule_skip_record_failed", "schedule", e.Schedule.Name, "error", err)
	}
	log.Info("schedule_catchup_skipped", "schedule", e.Schedule.Name, "scope_type", e.Scope,
		"scope_id", e.ScopeID, "scheduled_at", fireUTC.Format(time.RFC3339))
}

// holdsDuty asks whether this node runs the tick, failing closed on unknown.
func (s *Scheduler) holdsDuty(ctx context.Context) bool {
	if s.duty == nil {
		return true
	}
	held, err := s.duty(ctx)
	if err != nil {
		log.Error("scheduler_duty_claim_failed", "error", err)
		return false
	}
	return held
}

// runID is a readable id for a dispatched task — telemetry only.
//
// The durable at-most-once guarantee is the ledger's composite identity, so a
// collision in this joined string (a ':' inside a unit name) is harmless: it
// never gates a dispatch. That is exactly why the ledger does NOT key on a
// string like this one.
func runID(e Entry, label, runner string) string {
	return fmt.Sprintf("%s:%s:%s:%s:%s", e.Scope, e.ScopeID, e.Schedule.Name, label, runner)
}

// newTrace mints a W3C-shaped trace context for one fire.
//
// Shaped rather than merely random: a 32-hex trace id and a 16-hex span id are
// what a real tracer will accept when one is wired, so the ids the ledger has
// already stored stay meaningful across that change. crypto/rand cannot fail
// on any supported platform — it panics internally instead — so there is no
// error to handle and no degraded id to invent.
func newTrace() events.TraceContext { return events.NewTrace() }

// cmpOr returns v unless it is the zero string.
func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// durOr returns d unless it is zero or negative.
func durOr(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// cmpOrFunc returns f unless it is nil.
func cmpOrFunc(f, def func() events.TraceContext) func() events.TraceContext {
	if f == nil {
		return def
	}
	return f
}
