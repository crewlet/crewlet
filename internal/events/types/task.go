package types

import (
	"strings"

	"github.com/crewlet/crewlet/internal/events"
)

// Task lifecycle. Every one of these carries `role` — the agent's seat, and the
// key every projection groups an agent by. Load-bearing, not decorative: the
// event store tags a row's agent_role from it and the live projection keys on
// it, so a task event published without one is invisible to both. That is what
// happened — an agent's current task never appeared on a dashboard and a seat
// stayed "working" past the end of its turn, because these events carried the
// role only in the envelope's source, which neither consumer reads.

func init() {
	events.Register[TaskCreated]()
	events.Register[TaskAssigned]()
	events.Register[TaskStarted]()
	events.Register[TaskCompleted]()
	events.Register[TaskFailed]()
	events.Register[TaskDelegated]()
}

// TaskCreated marks a unit of work coming into existence, before anyone holds it.
type TaskCreated struct {
	TaskID     string `json:"task_id"`
	Title      string `json:"title"`
	TargetRole string `json:"target_role"`
}

// EventType is the "task_created" wire type.
func (TaskCreated) EventType() string { return "task_created" }

// SummaryFor names the creator, who rides on the envelope's source: this event
// carries no role, so the actor chain resolves to the publisher.
func (e TaskCreated) SummaryFor(actor string) string {
	switch {
	case e.Title != "" && e.TargetRole != "":
		return lead(actor, "created task '"+e.Title+"' for "+e.TargetRole)
	case e.Title != "":
		return lead(actor, "created task '"+e.Title+"'")
	default:
		return lead(actor, "created a task")
	}
}

// TaskAssigned hands a task to a seat. This is also the agent's wake: it is
// what the scheduler and the delegation path publish into an inbox.
type TaskAssigned struct {
	TaskID   string `json:"task_id"`
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	// Description is the work itself — a schedule's `task:` text, or what
	// the delegating seat asked for.
	//
	// TYPED, for the reason [A2ARequest] is: the scheduler used to write
	// this into the envelope's free-form Payload under "task_description"
	// and nothing read it back, so every scheduled fire woke its seat with
	// the literal string "(task_assigned)" and the founder-authored task
	// text never reached a model.
	Description string `json:"description,omitempty"`
	// Schedule names the schedule that fired this, empty for a delegation.
	// It is what tells a seat a recurring duty came round from a one-off
	// hand-off, which changes how it reads "do this again".
	Schedule string `json:"schedule,omitempty"`
	// TimeoutSeconds is the schedule's wall-clock cap for this fire, zero
	// for a delegation.
	//
	// NOT ENFORCED YET, and stated here rather than left implied: the cap is
	// documented (docs/concepts/scheduling.md) and has a GuardKind reserved
	// for it (GuardScheduledTimeout), but nothing raises that breach. It
	// travelled as a write-only Payload key before this field existed, so
	// carrying it typed is what gives the enforcement something to read when
	// it lands — and what stops the value disappearing in the meantime.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// EventType is the "task_assigned" wire type. It is also an inbox WAKE: this
// is what the scheduler and the delegation path publish to start a turn.
func (TaskAssigned) EventType() string { return "task_assigned" }

// Role is the seat the task was handed to.
func (e TaskAssigned) Role() string { return e.RoleName }

// AgentID is the instance holding that seat.
func (e TaskAssigned) AgentID() string { return e.Agent }

// Brief is the assigned work.
//
// The id is carried alongside the description rather than instead of it: the
// seat needs the description to know what to do and the id to write the result
// back to the tracker, and an id on its own is the ask this engine used to
// hand every scheduled turn.
func (e TaskAssigned) Brief() string {
	var b strings.Builder
	if e.Schedule != "" {
		b.WriteString("Scheduled work: " + e.Schedule + "\n\n")
	}
	if e.TaskID != "" {
		b.WriteString("Task " + e.TaskID + "\n\n")
	}
	b.WriteString(e.Description)
	return strings.TrimSpace(b.String())
}

// SummaryFor names the task id when there is one; a task with no id is real
// enough to report, it just cannot be linked to.
func (e TaskAssigned) SummaryFor(actor string) string {
	if e.TaskID != "" {
		return lead(actor, "was assigned task "+e.TaskID)
	}
	return lead(actor, "was assigned a task")
}

// TaskStarted marks a seat picking work up.
type TaskStarted struct {
	TaskID   string `json:"task_id"`
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
}

// EventType is the "task_started" wire type.
func (TaskStarted) EventType() string { return "task_started" }

// Role is the seat that picked the work up.
func (e TaskStarted) Role() string { return e.RoleName }

// AgentID is the instance now working it.
func (e TaskStarted) AgentID() string { return e.Agent }

// SummaryFor names the task id when there is one.
func (e TaskStarted) SummaryFor(actor string) string {
	if e.TaskID != "" {
		return lead(actor, "started working on "+e.TaskID)
	}
	return lead(actor, "started a task")
}

// TaskCompleted marks work finishing.
type TaskCompleted struct {
	TaskID   string `json:"task_id"`
	Agent    string `json:"agent_id"`
	Result   string `json:"result"`
	RoleName string `json:"role"`
}

// EventType is the "task_completed" wire type.
func (TaskCompleted) EventType() string { return "task_completed" }

// Role is the seat that finished the work.
func (e TaskCompleted) Role() string { return e.RoleName }

// AgentID is the instance that finished it.
func (e TaskCompleted) AgentID() string { return e.Agent }

// SummaryFor names the task id and deliberately not the result: a result is a
// whole answer, and a one-line feed entry is not where it belongs.
func (e TaskCompleted) SummaryFor(actor string) string {
	if e.TaskID != "" {
		return lead(actor, "completed task "+e.TaskID)
	}
	return lead(actor, "completed a task")
}

// TaskFailed marks work ending on a failure path. Its type is in
// FailureEventTypes, so history reads it as failed with no payload flag.
type TaskFailed struct {
	TaskID   string `json:"task_id"`
	Agent    string `json:"agent_id"`
	Error    string `json:"error"`
	RoleName string `json:"role"`
}

// EventType is the "task_failed" wire type, and one of the four names in
// FailureEventTypes.
func (TaskFailed) EventType() string { return "task_failed" }

// Role is the seat the failure is attributed to.
func (e TaskFailed) Role() string { return e.RoleName }

// AgentID is the instance that failed it.
func (e TaskFailed) AgentID() string { return e.Agent }

// SummaryFor appends the error, unlike TaskCompleted with its result: a failure
// line that does not say why sends the reader to the logs for the one fact the
// event already holds.
func (e TaskFailed) SummaryFor(actor string) string {
	line := lead(actor, "failed a task")
	if e.TaskID != "" {
		line = lead(actor, "failed task "+e.TaskID)
	}
	if e.Error != "" {
		return line + ": " + e.Error
	}
	return line
}

// TaskDelegated records a subtask being pushed down the hierarchy.
type TaskDelegated struct {
	ParentTaskID string `json:"parent_task_id"`
	ChildTaskID  string `json:"child_task_id"`
	TargetRole   string `json:"target_role"`
}

// EventType is the "task_delegated" wire type.
func (TaskDelegated) EventType() string { return "task_delegated" }

// SummaryFor names the delegator as the actor and the target role as the
// destination. The event carries no role of its own, so the actor resolves from
// the envelope's source — which is the manager doing the delegating.
func (e TaskDelegated) SummaryFor(actor string) string {
	if e.TargetRole != "" {
		return lead(actor, "delegated a subtask to "+e.TargetRole)
	}
	return lead(actor, "delegated a subtask")
}
