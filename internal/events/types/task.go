package types

import "github.com/crewlet/crewlet/internal/events"

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
}

func (TaskAssigned) EventType() string { return "task_assigned" }

func (e TaskAssigned) Role() string    { return e.RoleName }
func (e TaskAssigned) AgentID() string { return e.Agent }

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

func (TaskStarted) EventType() string { return "task_started" }

func (e TaskStarted) Role() string    { return e.RoleName }
func (e TaskStarted) AgentID() string { return e.Agent }

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

func (TaskCompleted) EventType() string { return "task_completed" }

func (e TaskCompleted) Role() string    { return e.RoleName }
func (e TaskCompleted) AgentID() string { return e.Agent }

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

func (TaskFailed) EventType() string { return "task_failed" }

func (e TaskFailed) Role() string    { return e.RoleName }
func (e TaskFailed) AgentID() string { return e.Agent }

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

func (TaskDelegated) EventType() string { return "task_delegated" }

func (e TaskDelegated) SummaryFor(actor string) string {
	if e.TargetRole != "" {
		return lead(actor, "delegated a subtask to "+e.TargetRole)
	}
	return lead(actor, "delegated a subtask")
}
