package types

import (
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/toolname"
)

// The seat's WAKE, and it is the only task event left.
//
// It carries the agent's seat as `agent_id` and `role` — its id, which the live
// projection keys an agent by, and its name, which the event store tags a
// row's agent_role from. Load-bearing, not decorative: a task event published
// without them is invisible to both. That is what happened — an agent's
// current task never appeared on a dashboard and a seat stayed "working" past
// the end of its turn, because the event carried the seat only in the
// envelope's source, which neither consumer reads.
//
// # Why there is no task LIFECYCLE here any more
//
// There were five more — created, started, completed, failed, delegated — and
// none of them had a publisher, because they described an engine-owned task
// object that Worklet replaced. Work is [internal/worklet]'s first
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

// TaskAssigned hands a seat work of its own: a schedule's task, or its next
// ready work item. It is the agent's wake for both, and it has TWO producers:
// the scheduler, which fills [TaskAssigned.Description] and
// [TaskAssigned.Schedule], and the work pickup (ADR-0112), which fills
// [TaskAssigned.Pickup] alone. A hand-off between seats is a tracker
// assignment or a colleague's ask, each with a wake of its own.
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
	// ScopeType and ScopeID are the scope that owns [TaskAssigned.Schedule]:
	// with the name, the schedule's identity as the dispatch ledger keys a
	// fire on it. A name alone is not one, since two units can each declare
	// a `standup`. They are what a fire the seat does not run is recorded
	// under ([ScheduledFireSuperseded]), so the schedule's history can say
	// so whichever node keeps the record.
	ScopeType ScheduleScope `json:"scope_type,omitempty"`
	ScopeID   string        `json:"scope_id,omitempty"`
	// TimeoutSeconds is the schedule's wall-clock cap for this fire; zero
	// is no cap, which the one producer never sends (a schedule with none
	// of its own carries the default).
	//
	// ENFORCED BETWEEN ROUNDS by the turn loop (turn.Settings.Deadline),
	// from the moment the turn began and across every park: a turn past the
	// cap starts no further round — in whichever segment it is — and ends
	// with a scheduled_timeout breach, which reaches the turn event's
	// error_kind. Never mid-phase in the engine — a phase that is running
	// may already have fired side effects, and abandoning it there would
	// leave a post half-written. The executor's detached run carries the
	// same instant, and the poll ends a run that outlasts it, collecting
	// what it did for the turn to judge.
	//
	// It travelled as a write-only Payload key before this field existed, so
	// nothing could read it and the documented cap bounded nothing.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// ScheduledAt is the instant this fire came due, in UTC: the tick it
	// stands for rather than the moment it was published. It is what orders
	// two fires of one schedule that waited in an inbox together, and what
	// the run that stands for both names the other by ([TaskAssigned.Missed]).
	ScheduledAt time.Time `json:"scheduled_at,omitzero"`
	// Missed are the earlier fires of this schedule this one run stands for:
	// fires that waited in the seat's inbox while it could not work (a
	// provider limit, a budget, a pause of its executor) and reached it
	// together with this one. The scheduler never sets it. The dispatcher
	// does, when it runs only the latest fire of a waiting schedule, so the
	// seat is told it is catching up once rather than handed the same duty
	// once per missed tick.
	Missed []MissedFire `json:"missed,omitempty"`
	// Pickup is the work item the work pickup hands an idle seat, and why
	// it chose that one; nil on a schedule's fire.
	Pickup *WorkPickup `json:"pickup,omitempty"`
}

// WorkPickup is the item a pickup names, and whom its work is for. Every
// field is the tracker's own value, never anybody's prose.
type WorkPickup struct {
	// Item is the task's id, Key its address and Status where it stands.
	Item   string `json:"item"`
	Key    string `json:"key"`
	Status string `json:"status"`
	// Moved is the log position of the last change somebody other than the
	// seat made to the item, and Progressed the seat's own last progress on
	// it when started: with the seat, the item and its status, the pickup's
	// identity.
	Moved      uint64 `json:"moved"`
	Progressed uint64 `json:"progressed,omitempty"`
	// Why is what put the item first ([PickupWhy]).
	Why PickupWhy `json:"why"`
	// Ready is how many ready items the seat holds, this one included, and
	// ReadyCapped whether it holds more than the pickup counted.
	Ready       int  `json:"ready"`
	ReadyCapped bool `json:"ready_capped,omitempty"`
	// WorkingFor is whom the item's work is for, so the turn is held to
	// their apps (ADR-0109); nil for the seat's own work.
	WorkingFor *WorkingFor `json:"working_for,omitempty"`
}

// PickupWhy is what put an item first in a seat's ready work.
type PickupWhy string

const (
	// PickupInProgress is an item the seat already started, which comes
	// before any it has not.
	PickupInProgress PickupWhy = "in_progress"
	// PickupPriorityList is the first item of the seat's priority list,
	// the order somebody arranged for it.
	PickupPriorityList PickupWhy = "priority_list"
	// PickupQueue is the first ready item by priority, then due date.
	PickupQueue PickupWhy = "queue"
)

// Valid reports whether a reason off the wire is one this build knows.
func (w PickupWhy) Valid() bool {
	switch w {
	case PickupInProgress, PickupPriorityList, PickupQueue:
		return true
	}
	return false
}

// sentence is the reason as the brief says it.
func (w PickupWhy) sentence() string {
	switch w {
	case PickupInProgress:
		return "It is in progress and nothing is working on it now, so it comes before work you have not started."
	case PickupPriorityList:
		return "It is the first ready item on your priority list, the order your lead set for you."
	case PickupQueue:
		return "It is your first ready item by priority, then due date."
	}
	return ""
}

// MissedFire is one earlier fire a scheduled run stands for.
type MissedFire struct {
	// TaskID is the fire's run id, as [TaskAssigned.TaskID] carried it.
	TaskID string `json:"task_id"`
	// ScheduledAt is when that fire came due, in UTC.
	ScheduledAt time.Time `json:"scheduled_at,omitzero"`
}

// TriggerOrigin is the schedule that fired ([Originated]), or nothing for a
// task no schedule names.
func (e TaskAssigned) TriggerOrigin() TriggerOrigin {
	if p := e.Pickup; p != nil {
		return TriggerOrigin{Kind: OriginWorkItem, ID: p.Key, Reason: PickupReason}
	}
	if e.Schedule == "" {
		return TriggerOrigin{}
	}
	return TriggerOrigin{Kind: OriginSchedule, ID: e.Schedule}
}

// PickupReason is the origin reason a pickup's turn is started under, beside
// the tracker's own routing reasons.
const PickupReason = "pickup"

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
	if p := e.Pickup; p != nil {
		return p.brief()
	}
	var b strings.Builder
	if e.Schedule != "" {
		b.WriteString("Scheduled work: " + e.Schedule + "\n\n")
	}
	b.WriteString(e.Description)
	if line := e.missedLine(); line != "" {
		b.WriteString("\n\n" + line)
	}
	return strings.TrimSpace(b.String())
}

// missedLine tells a seat that this run stands for fires it missed, and how
// to treat them: as one piece of work done once, as of now.
//
// SAID IN THE BRIEF, because without it a seat that knows its schedule's
// cadence reads a run that comes due hours late as an anomaly to report, and
// one that counts its own checks numbers this one as if the missed ticks had
// run.
func (e TaskAssigned) missedLine() string {
	if len(e.Missed) == 0 {
		return ""
	}
	due := make([]string, 0, len(e.Missed))
	for _, m := range e.Missed {
		if m.ScheduledAt.IsZero() {
			continue
		}
		due = append(due, m.ScheduledAt.UTC().Format(time.RFC3339))
	}
	fires := "1 earlier fire"
	if n := len(e.Missed); n > 1 {
		fires = strconv.Itoa(n) + " earlier fires"
	}
	line := "This run also stands for " + fires + " of this schedule that " +
		"waited while you could not work"
	if len(due) > 0 {
		line += " (due " + strings.Join(due, ", ") + ")"
	}
	return line + ". Do the work once, as of now, and do not repeat it for " +
		"each missed fire."
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
	if p := e.Pickup; p != nil {
		return lead(actor, "was handed its next ready work item "+p.Key)
	}
	if e.Schedule != "" {
		return lead(actor, "was assigned scheduled work "+e.Schedule)
	}
	return lead(actor, "was assigned a task")
}

// brief is the pickup's ask: which item, why, and the four honest answers,
// named because a promise that changes nothing is never reminded.
func (p WorkPickup) brief() string {
	var b strings.Builder
	b.WriteString("Work pickup: you are idle and hold assigned work that is ready. ")
	b.WriteString("Your next item is " + p.Key + " (" + p.Status + ").")
	if why := p.Why.sentence(); why != "" {
		b.WriteString(" " + why)
	}
	switch {
	case p.ReadyCapped:
		b.WriteString(" You hold at least " + strconv.Itoa(p.Ready) + " ready items.")
	case p.Ready > 1:
		b.WriteString(" You hold " + strconv.Itoa(p.Ready) + " ready items in all.")
	}
	b.WriteString("\n\nRead it with " + toolname.GetWorkItem + ", then do one of these in this turn:\n" +
		"- start or continue the work now;\n" +
		"- if something outside your control blocks it, record the blocker on the item " +
		"(waiting_on) or ask the person who can unblock it;\n" +
		"- if it is not yours to do, hand it back or reassign it;\n" +
		"- if it should wait, say why with " + toolname.CommentOnWorkItem + ".\n" +
		"You will not be handed this item again until it changes.")
	return b.String()
}
