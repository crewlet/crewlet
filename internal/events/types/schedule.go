package types

import "github.com/crewlet/crewlet/internal/events"

func init() {
	events.Register[ScheduledTaskFired]()
}

// ScheduleScope is what a recurring schedule is attached to.
type ScheduleScope string

const (
	ScheduleScopeRole ScheduleScope = "role"
	ScheduleScopeUnit ScheduleScope = "unit"
)

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

func (ScheduledTaskFired) EventType() string { return "scheduled_task_fired" }

func (e ScheduledTaskFired) Summary() string {
	who := e.TargetHandle
	if who == "" {
		who = e.ScopeID
	}
	return "Scheduled '" + e.ScheduleName + "' fired for " + who
}
