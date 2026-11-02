package types

import "github.com/crewlet/crewlet/internal/events"

func init() {
	events.Register[ScheduledTaskFired]()
	events.Register[ScheduledFireSuperseded]()
}

// ScheduleScope is what a recurring schedule is attached to.
type ScheduleScope string

// The two things a schedule can hang off. A role schedule has exactly one runner
// — the seat itself. A unit schedule resolves its runners from the unit and its
// target, so one tick can dispatch several times.
const (
	ScheduleScopeRole ScheduleScope = "role"
	ScheduleScopeUnit ScheduleScope = "unit"
)

// ScheduleScopes is the closed set, which is what makes a refusal able to say
// what would have worked.
func ScheduleScopes() []ScheduleScope {
	return []ScheduleScope{ScheduleScopeRole, ScheduleScopeUnit}
}

// Valid reports whether s is a scope this build knows.
//
// EVERY ENUM IN THIS TREE HAS ONE, and this one did not: a value off the wire
// has to be a value rather than a panic, and a surface taking a scope as a
// parameter has to be able to refuse an unknown one naming the two. It is on
// the type rather than at each caller so two callers cannot disagree about
// what a scope is.
func (s ScheduleScope) Valid() bool {
	return s == ScheduleScopeRole || s == ScheduleScopeUnit
}

// ScheduledTaskFired records the scheduler dispatching a recurring run.
//
// Observability only: the agent's actual wake is a TaskAssigned published to
// the runner's inbox. This event is what the dashboard and the event store see,
// so a tick that fired is visible even when the work it caused is not.
type ScheduledTaskFired struct {
	ScopeType ScheduleScope `json:"scope_type"`

	// ScopeID is the identity the at-most-once ledger keys this fire on —
	// a seat's agent id, or a unit's key. ScopeName is the same scope as a
	// person reads it: the seat's handle, or the unit's key.
	//
	// BOTH, because they answer different questions. A reader following
	// this event back to the schedule's own history needs the id, and a
	// reader of the summary below needs a name.
	ScopeID   string `json:"scope_id"`
	ScopeName string `json:"scope_name,omitempty"`

	ScheduleName string `json:"schedule_name"`
	TargetHandle string `json:"target_handle"`
	// ScheduledAt is the tick's own time as an ISO 8601 string, not the
	// dispatch time the envelope stamps: a catchup run for a missed tick
	// reports the tick it is making up, which is the whole point of naming it.
	ScheduledAt string `json:"scheduled_at"`
}

// EventType is the "scheduled_task_fired" wire type.
func (ScheduledTaskFired) EventType() string { return "scheduled_task_fired" }

// Summary names the seat the run was dispatched to, falling back to the
// scope's NAME and then to its id: a unit schedule fires once per runner, so
// the handle is what tells two dispatches of the same tick apart.
//
// THE NAME BEFORE THE ID, because this line is read by a person and a role
// scope's id is a uuid. The id is the last resort rather than the first for
// the same reason it is still there at all: an event written by a build that
// carried no name must still say which schedule fired.
func (e ScheduledTaskFired) Summary() string {
	who := e.TargetHandle
	if who == "" {
		who = e.ScopeName
	}
	if who == "" {
		who = e.ScopeID
	}
	return "Scheduled '" + e.ScheduleName + "' fired for " + who
}

// ScheduledFireSuperseded records a fire that reached its seat and was not
// run, because a later fire of the same schedule reached it at the same time
// and ran in its place.
//
// Fires wait in a seat's inbox while the seat cannot work (a provider limit,
// a budget, an executor holding it), and a schedule keeps firing meanwhile.
// Run one by one when the seat came back, three health checks queued behind
// one limit ran within four minutes and reported the same thing three times.
// The dispatcher now runs only the latest and records each earlier one with
// this event, under THAT fire's own trace, so the schedule's history shows the
// fire as skipped rather than as a dispatch with no turn behind it.
type ScheduledFireSuperseded struct {
	ScheduleName string `json:"schedule_name"`
	TargetHandle string `json:"target_handle"`

	// TaskID and ScheduledAt name the fire that was not run.
	TaskID      string `json:"task_id"`
	ScheduledAt string `json:"scheduled_at,omitempty"`

	// SupersededBy is the run id of the fire that ran in its place.
	SupersededBy string `json:"superseded_by"`
}

// ScheduleOutcomeSuperseded is the outcome a schedule's history reports for a
// fire with a [ScheduledFireSuperseded] record: dispatched, reached its seat,
// and not run because a later fire ran in its place.
//
// NOT A DISPATCH LEDGER OUTCOME. The ledger records what the scheduler did,
// and the scheduler did fire it; the seat's node is what decided not to run
// it, and it records that on the event log, where the history's derivation
// reads it beside the turn a fire's trace ran.
const ScheduleOutcomeSuperseded = "skipped_superseded"

// EventType is the "scheduled_fire_superseded" wire type.
func (ScheduledFireSuperseded) EventType() string { return "scheduled_fire_superseded" }

// Summary names the schedule and the seat, which is what tells two skipped
// fires of a unit schedule apart.
func (e ScheduledFireSuperseded) Summary() string {
	return "Scheduled '" + e.ScheduleName + "' skipped a missed fire for " +
		e.TargetHandle + ": a later fire ran in its place"
}
