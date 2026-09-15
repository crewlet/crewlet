package types

import "github.com/crewlet/crewlet/internal/events"

func init() {
	events.Register[ScheduledTaskFired]()
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
	ScopeType    ScheduleScope `json:"scope_type"`
	ScopeID      string        `json:"scope_id"`
	ScheduleName string        `json:"schedule_name"`
	TargetHandle string        `json:"target_handle"`
	// ScheduledAt is the tick's own time as an ISO 8601 string, not the
	// dispatch time the envelope stamps: a catchup run for a missed tick
	// reports the tick it is making up, which is the whole point of naming it.
	ScheduledAt string `json:"scheduled_at"`
}

// EventType is the "scheduled_task_fired" wire type.
func (ScheduledTaskFired) EventType() string { return "scheduled_task_fired" }

// Summary names the seat the run was dispatched to, falling back to the scope
// id: a unit schedule fires once per runner, so the handle is what tells two
// dispatches of the same tick apart.
func (e ScheduledTaskFired) Summary() string {
	who := e.TargetHandle
	if who == "" {
		who = e.ScopeID
	}
	return "Scheduled '" + e.ScheduleName + "' fired for " + who
}
