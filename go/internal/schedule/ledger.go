package schedule

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// The dispatch ledger — what makes a fire at-most-once.
//
// `scheduled_runs` is a DISPATCH ledger, not a turn-outcome store: a row
// records that the scheduler fired (or deliberately skipped) one run, once.
// Whether the resulting turn finished, failed or timed out lives in the normal
// turn telemetry under the same trace, and keeping the two apart is what stops
// the scheduler from having to know anything about turn internals.

// Outcome is what the ledger recorded about a fire.
type Outcome string

const (
	// OutcomeFired is a run that was dispatched to a runner.
	OutcomeFired Outcome = "fired"

	// OutcomeSkippedCatchup is a missed run that fell outside the catchup
	// window and was deliberately not backfilled. Recorded rather than
	// dropped so an operator asking "why did the 09:00 standup not run
	// after the restart" gets an answer instead of a silence.
	OutcomeSkippedCatchup Outcome = "skipped_catchup"
)

// FireKey is the identity of one dispatch, and the whole of the at-most-once
// guarantee.
//
// It is a STRUCT rather than a joined string, and that is load-bearing in both
// backends: SQL keys the row on the column tuple, and Go's comparable structs
// let the memory twin use this value directly as a map key. A delimiter-joined
// identity collides the moment a delimiter appears in a name — a unit called
// "Q3:Launch" and a schedule called "Launch" would produce the same joined key
// as a unit "Q3" and a schedule ":Launch", and one of the two fires would then
// be silently suppressed by the other.
type FireKey struct {
	// Scope is whether a role or a unit owns the schedule.
	Scope types.ScheduleScope
	// ScopeID is the owning role's HANDLE or the owning unit's NAME.
	ScopeID string
	// ScheduleName is the schedule's name within its scope. Renaming a
	// schedule therefore lets a fire at the same minute run once more,
	// which is the documented behaviour.
	ScheduleName string
	// FireLabel is the schedule's LOCAL wall-clock stamp (YYYYmmddTHHMM in
	// its own timezone), never the UTC instant.
	//
	// That is what makes the dedupe DST-correct: on a fall-back day one
	// local cron minute maps to two UTC instants, and both share a label,
	// so the run fires once rather than twice. See [FireLabel].
	FireLabel string
	// TargetHandle is the runner this fire was addressed to, and empty for
	// a skipped catchup, which resolved no runner. An `each` fan-out mints
	// one identity per member, so a slow or failing member cannot suppress
	// its siblings.
	TargetHandle string
}

// Run is one ledger row: an identity plus what was recorded about it.
type Run struct {
	FireKey

	// ScheduledAt is the UTC instant the fire was due — for a catchup run,
	// the tick being made up rather than the moment it ran.
	ScheduledAt time.Time

	// FiredAt is when the row was written. A zero value asks the ledger to
	// stamp it, which is what every production caller wants; supplying one
	// is for tests that need a deterministic ordering to assert against.
	FiredAt time.Time

	// Outcome is [OutcomeFired] or [OutcomeSkippedCatchup]. The zero value
	// is empty rather than "fired": a caller that forgot to say what it
	// recorded should read back as unset, not as a dispatch.
	Outcome Outcome

	// TraceID links the row to the turn the fire caused. Each fire gets its
	// own trace, so the dashboard can go from a ledger row to exactly that
	// turn's calls.
	TraceID string
}

// Claimer is the ledger surface the [Scheduler] itself needs: one call, made
// before every publish.
//
// Declared separately from [Ledger] because that is all the scheduler uses,
// and a dispatcher that could also read history and delete rows is a
// dispatcher whose blast radius nobody can see from its type.
type Claimer interface {
	// Claim atomically records a fire, reporting whether THIS call wrote
	// the row.
	//
	// True means the caller owns the fire and must go on to dispatch it.
	// False means the identity already existed — another node's tick, or a
	// pre-restart run, already handled it — and the caller must not.
	//
	// An error is UNKNOWN, and callers fail CLOSED on it: not knowing
	// whether a fire was already claimed has exactly one safe answer, and
	// it is "do not fire". That polarity is the opposite of the completion
	// ledger's, deliberately — that one asks "has this work been done", so
	// its safe answer is to re-run; this one asks "may I start", so its
	// safe answer is to wait for the next tick.
	Claim(ctx context.Context, run Run) (bool, error)
}

// Ledger is the full contract a scheduled-run store must satisfy, and the one
// certified by internal/schedule/scheduletest.
//
// It is wider than [Claimer] because two other callers exist and neither is
// the scheduler: the dashboard reads Recent to draw the dispatch history, and
// the retention sweep calls Purge. A backend the contract suite has not
// certified does not exist as far as the engine is concerned.
type Ledger interface {
	Claimer

	// Recent returns the newest rows first, at most limit of them.
	//
	// Ordering is (FiredAt descending, then the identity tuple ascending).
	// The tiebreak is part of the contract rather than an implementation
	// detail: two rows can share a millisecond, and without one the SQL
	// backend and the memory twin would answer a dashboard differently for
	// the same data — which is the class of divergence a twin exists to
	// prevent, not to introduce.
	//
	// A limit of zero or below returns nothing. That is the honest reading
	// of "give me no rows"; treating it as unbounded would make a caller
	// whose page size arrived as 0 pull the whole table.
	Recent(ctx context.Context, limit int) ([]Run, error)

	// Purge drops rows whose FiredAt is strictly before `before`, returning
	// how many went.
	//
	// The cutoff is an INSTANT rather than an age because the caller owns
	// the policy, and here the policy has a floor that is not obvious: the
	// ledger is a claim table first and a dashboard feed second, so nothing
	// may be deleted while a tick could still evaluate the fire it records.
	// That floor is the catchup window's upper clamp — see
	// [Scheduler.RetentionFloor]. Sweeping inside it does not merely lose
	// history, it lets that fire run a second time.
	//
	// Dropping a row MUST drop its claim key with it. A purge that kept the
	// key would go on refusing a fire the store has no evidence of, which
	// is worse than never sweeping at all because the refusal is silent.
	Purge(ctx context.Context, before time.Time) (int, error)
}

// FireLabel renders the identity stamp for a fire: the local wall-clock
// minute, in the schedule's own timezone.
//
// Local, not UTC, and that is the whole of the DST story on the ledger side.
// A fall-back day repeats a local hour, so a 02:30 schedule has two UTC
// instants that day; both render to one label, so the second one loses the
// claim and the schedule fires once. The evaluator deliberately reports both
// instants (see [Expr.FireTimes]) — collapsing them is this function's job,
// and splitting the responsibility that way is what keeps each half testable.
func FireLabel(fireUTC time.Time, loc *time.Location) string {
	return fireUTC.In(loc).Format("20060102T1504")
}

// Stamped fills in a Run's FiredAt when the caller left it zero, and is what
// every [Ledger] backend calls before writing.
//
// The rule lives here rather than in each backend so the twin and the SQL
// ledger cannot disagree about it — a backend that stamped a local-zone time,
// or one that left a zero through, would put its rows at the wrong end of
// every listing and below every retention cutoff. It also keeps the wall-clock
// read in one place; see [now].
func Stamped(run Run) Run {
	if run.FiredAt.IsZero() {
		run.FiredAt = now()
	}
	return run
}

// now is the ONE place this package reads the wall clock.
//
// Everything else takes its time as a parameter — the tick takes `now`, a Run
// carries its own FiredAt — so a test that injects an instant is not quietly
// racing a second clock somewhere below it. TestOnlyOneClockReadsTheWallTime
// holds that on the syntax tree.
func now() time.Time { return time.Now().UTC() }
