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
// This one is not a lifecycle event at all. It is what the scheduler puts
// into an INBOX, which is why it survives them.

func init() {
	events.Register[TaskAssigned]()
}

// TaskAssigned hands a schedule's task to a seat. This is also the agent's
// wake: it is what the scheduler publishes into an inbox, and the scheduler is
// its ONE producer — a hand-off between seats is a tracker assignment or a
// colleague's ask, each with a wake of its own. Its docs used to name a
// "delegation path" as a second producer; nothing in this tree publishes one.
type TaskAssigned struct {
	// TaskID is the fire's run id — scope, schedule, instant and runner —
	// kept for telemetry and the feed's detail. It names one FIRE and no
	// tracker item, so nothing a seat reads leads with it ([Brief],
	// [SummaryFor]).
	TaskID   string `json:"task_id"`
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	// Description is the work itself — the schedule's `task:` text.
	//
	// TYPED, for the reason [A2ARequest] is: the scheduler used to write
	// this into the envelope's free-form Payload under "task_description"
	// and nothing read it back, so every scheduled fire woke its seat with
	// the literal string "(task_assigned)" and the founder-authored task
	// text never reached a model.
	Description string `json:"description,omitempty"`
	// Schedule names the schedule that fired this. It is what tells a seat
	// a recurring duty came round, which changes how it reads "do this
	// again", and it is what the fire's label names ([SummaryFor]), so two
	// fires of one schedule read alike.
	Schedule string `json:"schedule,omitempty"`
	// TimeoutSeconds is the schedule's wall-clock cap for this fire; zero
	// is no cap, which the one producer never sends (a schedule with none
	// of its own carries the default).
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
// is what the scheduler publishes to start a turn.
func (TaskAssigned) EventType() string { return "task_assigned" }

// Role is the seat the task was handed to.
func (e TaskAssigned) Role() string { return e.RoleName }

// AgentID is the instance holding that seat.
func (e TaskAssigned) AgentID() string { return e.Agent }

// Brief is the assigned work: the schedule it is a fire of, and the
// schedule's own task text.
//
// WITHOUT THE TASK ID, which it used to lead with as "Task <id>" on the
// stated ground that the seat needs it "to write the result back to the
// tracker". It does not and cannot: the one producer is the scheduler, whose
// id is a readable name for the fire — scope, schedule, instant and runner —
// kept for telemetry and gating nothing, so a seat handed it as a task was
// handed something that reads as a tracker key and names no item. And as
// the turn's ask it made two fires of one schedule unalike to every
// similarity search, by the one part that differs on every fire.
func (e TaskAssigned) Brief() string {
	var b strings.Builder
	if e.Schedule != "" {
		b.WriteString("Scheduled work: " + e.Schedule + "\n\n")
	}
	b.WriteString(e.Description)
	return strings.TrimSpace(b.String())
}

// SummaryFor names the SCHEDULE a fire came from, never the fire's id.
//
// The summary is not only a feed line: it is the trigger's label
// ([Trigger.Summary]), so it becomes the turn's task_summary and the recalled
// episode's "woken by" line, and it is the first paragraph of the text the
// episode's vector is made of. It led with the task id, which is [Brief]'s
// mistake over again: the one producer's id is a readable name for one fire,
// so every recall of a scheduled turn handed the seat something that reads as
// a tracker key and names no item, and every fire of one schedule was unalike
// to a similarity search by the one part that differs on every fire. The id
// stays on the payload, where telemetry and the feed's detail read it.
func (e TaskAssigned) SummaryFor(actor string) string {
	if e.Schedule != "" {
		return lead(actor, "was assigned scheduled work "+e.Schedule)
	}
	return lead(actor, "was assigned a task")
}
