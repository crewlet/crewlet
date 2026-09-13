package types

import (
	"strings"

	"github.com/crewlet/crewlet/internal/events"
)

// The seat's WAKE, and it is the only task event left.
//
// It carries `role` — the agent's seat, and the key every projection groups an
// agent by. Load-bearing, not decorative: the event store tags a row's
// agent_role from it and the live projection keys on it, so a task event
// published without one is invisible to both. That is what happened — an
// agent's current task never appeared on a dashboard and a seat stayed
// "working" past the end of its turn, because the event carried the role only
// in the envelope's source, which neither consumer reads.
//
// # Why there is no task LIFECYCLE here any more
//
// There were five more — created, started, completed, failed, delegated — and
// none of them had a publisher, because they described an engine-owned task
// object that the native tracker replaced. Work is [internal/tracker]'s first
// state-log domain now: every change is one arbitrated record, applied into N
// identical SQL copies with a history row beside it, and reachable through the
// tracker's own change feed. A second, lossier copy of the same facts on the
// event stream would be a second answer to "what happened to this item", and
// the two would disagree the first time one of them was not published.
//
// This one is not a lifecycle event at all. It is what the scheduler and the
// delegation path put into an INBOX, which is why it survives them.

func init() {
	events.Register[TaskAssigned]()
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
	// ENFORCED BETWEEN ROUNDS by the turn loop (turn.Settings.MaxWallClock):
	// a turn past the cap starts no further round and ends with a
	// scheduled_timeout breach, which reaches the turn event's error_kind.
	// Never mid-phase — a phase that is running may already have fired side
	// effects, and abandoning it there would leave a post half-written.
	//
	// It travelled as a write-only Payload key before this field existed, so
	// nothing could read it and the documented cap bounded nothing.
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
