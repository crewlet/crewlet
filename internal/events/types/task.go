package types

import (
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/toolname"
)

// The seat's own WAKES, a schedule's fire and a work pickup, and the only
// task events left.
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
// These are not lifecycle events at all. They are what the scheduler and the
// work pickup put into an INBOX, which is why they survive them.

func init() {
	events.Register[TaskAssigned]()
	events.Register[WorkPickup]()
}

// TaskAssigned hands a seat a schedule's task: the agent's wake for a fire,
// published by the scheduler. A hand-off between seats is a tracker assignment
// or a colleague's ask, and an idle seat's next ready item a [WorkPickup],
// each with a wake of its own.
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
}

// WorkPickup hands an idle agent seat its next ready work item (ADR-0112). A
// type of its own, so a build that does not know it never reads it as a
// schedule's fire. Every field is the tracker's own value, never prose.
type WorkPickup struct {
	// Agent and RoleName are the seat, as [TaskAssigned] carries it.
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	// Item is the task's id, Key its address and Status where it stands.
	Item   string `json:"item"`
	Key    string `json:"key"`
	Status string `json:"status"`
	// Moved is the log position of the last change to the item that would
	// have woken the seat, and Progressed the seat's own last progress on it
	// when started: with the seat, the item and its status, the pickup's
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
	// Quiet is set on a recheck (ADR-0122): when the item went quiet with no
	// typed answer from the seat, a day or more before this pickup.
	Quiet *time.Time `json:"quiet,omitempty"`
	// SubmittedBy is who moved an item in review into review: the asker of
	// a review, or the seat itself on one nobody reviews.
	SubmittedBy string `json:"submitted_by,omitempty"`
	// WaitsOn is the persons an item set aside until a date names without an
	// open ask from the seat ([PickupWaitsOnPerson]), by handle.
	WaitsOn []string `json:"waits_on,omitempty"`
	// DeadWaits is the changes nothing moves that the item waits on
	// ([PickupDeadWait]), in reference order.
	DeadWaits []DeadChange `json:"dead_waits,omitempty"`
}

// DeadChange is a change (a pull request) a work item waits on that nothing
// moves at Head: no reviewer asked, no review of Head, no merge asked
// (ADR-0128). Author is a seat's handle where a seat's app opened it.
type DeadChange struct {
	Ref    string `json:"ref"`
	Head   string `json:"head"`
	Author string `json:"author,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
}

// line is the change as the brief names it.
func (d DeadChange) line() string {
	out := d.Ref
	if len(d.Head) >= 7 {
		out += " at " + d.Head[:7]
	}
	var about []string
	if d.Author != "" {
		about = append(about, "opened by "+d.Author)
	}
	if d.Draft {
		about = append(about, "a draft")
	}
	if len(about) > 0 {
		out += " (" + strings.Join(about, ", ") + ")"
	}
	return out
}

// PickupWhy is what put an item first in a seat's ready work.
type PickupWhy string

const (
	// PickupInProgress is an item the seat already started, which comes
	// before any it has not.
	PickupInProgress PickupWhy = "in_progress"
	// PickupReview is an item in review that somebody else moved there and
	// the seat holds: its review, which comes before work it has not started.
	PickupReview PickupWhy = "review"
	// PickupUnreviewed is an item in review that the seat moved there itself
	// and nobody reviews: no reviewer holds it and no question of the seat's
	// waits on one, so it acts on the review it was given or names one.
	PickupUnreviewed PickupWhy = "unreviewed"
	// PickupWaitsOnPerson is an item set aside until a date by an agent
	// whose reason names a person the seat has not asked on it (ADR-0122):
	// it comes back before its date, so the wait becomes the ask it is.
	PickupWaitsOnPerson PickupWhy = "waits_on_person"
	// PickupDeadWait is an item whose every open blocker is a pull request
	// nothing moves (ADR-0128): the wait cannot end by itself, so it comes
	// back to its holder, once per head of each.
	PickupDeadWait PickupWhy = "dead_wait"
	// PickupPriorityList is the first item of the seat's priority list,
	// the order somebody arranged for it.
	PickupPriorityList PickupWhy = "priority_list"
	// PickupQueue is the first ready item by priority, then due date.
	PickupQueue PickupWhy = "queue"
)

// Valid reports whether a reason off the wire is one this build knows.
func (w PickupWhy) Valid() bool {
	switch w {
	case PickupInProgress, PickupReview, PickupUnreviewed, PickupWaitsOnPerson, PickupDeadWait,
		PickupPriorityList, PickupQueue:
		return true
	}
	return false
}

// sentence is the reason as the brief says it.
func (w PickupWhy) sentence() string {
	switch w {
	case PickupInProgress:
		return "It is in progress and nothing is working on it now, so it comes before work you have not started."
	case PickupReview:
		return "It is in review and yours to review: somebody else asked for it, so it comes before work you have not started."
	case PickupUnreviewed:
		return "It is in review, but you moved it there yourself and nobody is reviewing it: no " +
			"reviewer holds it and no question of yours waits on one. Act on the review it was " +
			"given (finish it, or take it back to in progress), or name its reviewer with " +
			toolname.UpdateWorkItem + " and `reviewer`."
	case PickupWaitsOnPerson:
		return "It is set aside until a date, and its reason names a person you have not asked on " +
			"it: the date passes whether or not they answer, and nothing reaches their Inbox. Ask " +
			"them with " + toolname.CommentOnWorkItem + " and `ask` (with a `decision`), or, if " +
			"the date is real and waits on nobody, restate `wait_reason` without naming them."
	case PickupDeadWait:
		return "It waits on a pull request that nothing is moving: no reviewer is requested, nobody has " +
			"reviewed its head and no merge is asked, so the wait cannot end by itself. Put it in review " +
			"naming its `reviewer` with " + toolname.UpdateWorkItem + " (and request their review on the pull " +
			"request), ask the reviewer on it with " + toolname.CommentOnWorkItem + " and `ask`, or, if its " +
			"review is done, ask to merge it at its head with " + toolname.AskToMerge + ". You are handed it " +
			"again only when the pull request has a new head that nothing moves either."
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
	if e.Schedule == "" {
		return TriggerOrigin{}
	}
	return TriggerOrigin{Kind: OriginSchedule, ID: e.Schedule}
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
	if e.Schedule != "" {
		return lead(actor, "was assigned scheduled work "+e.Schedule)
	}
	return lead(actor, "was assigned a task")
}

// PickupReason is the origin reason a pickup's turn is started under, beside
// the tracker's own routing reasons, and DeadWaitReason a dead wait's, so the
// turn ledger counts the hand-back apart (ADR-0128).
const (
	PickupReason   = "pickup"
	DeadWaitReason = "dead_wait"
)

// WorkPickupType is the "work_pickup" wire type, an inbox WAKE.
const WorkPickupType = "work_pickup"

// EventType is [WorkPickupType].
func (WorkPickup) EventType() string { return WorkPickupType }

// Role is the seat handed the item.
func (p WorkPickup) Role() string { return p.RoleName }

// AgentID is the instance holding that seat.
func (p WorkPickup) AgentID() string { return p.Agent }

// TriggerOrigin files the turn on the item ([Originated]).
func (p WorkPickup) TriggerOrigin() TriggerOrigin {
	if p.Why == PickupDeadWait {
		return TriggerOrigin{Kind: OriginWorkItem, ID: p.Key, Reason: DeadWaitReason}
	}
	return TriggerOrigin{Kind: OriginWorkItem, ID: p.Key, Reason: PickupReason}
}

// SummaryFor names the item handed over.
func (p WorkPickup) SummaryFor(actor string) string {
	if p.Why == PickupDeadWait {
		return lead(actor, "was handed back "+p.Key+", whose wait on a pull request nothing moves")
	}
	return lead(actor, "was handed its next ready work item "+p.Key)
}

// Brief is the pickup's ask: which item, why, and the five typed answers, each
// a record on the item the engine can act on, because a promise in words
// changes nothing (ADR-0122).
func (p WorkPickup) Brief() string {
	var b strings.Builder
	b.WriteString("Work pickup: you are idle and hold assigned work that is ready. ")
	b.WriteString("Your next item is " + p.Key + " (" + p.Status + ").")
	if why := p.Why.sentence(); why != "" {
		b.WriteString(" " + why)
	}
	if p.Why == PickupReview && p.SubmittedBy != "" {
		b.WriteString(" " + p.SubmittedBy + " moved it into review.")
	}
	if p.Why == PickupWaitsOnPerson && len(p.WaitsOn) > 0 {
		b.WriteString(" Its reason names " + strings.Join(p.WaitsOn, ", ") + ".")
	}
	if p.Why == PickupDeadWait && len(p.DeadWaits) > 0 {
		lines := make([]string, 0, len(p.DeadWaits))
		for _, d := range p.DeadWaits {
			lines = append(lines, d.line())
		}
		b.WriteString(" It waits on " + strings.Join(lines, "; ") + ".")
	}
	switch {
	case p.ReadyCapped:
		b.WriteString(" You hold at least " + strconv.Itoa(p.Ready) + " ready items.")
	case p.Ready > 1:
		b.WriteString(" You hold " + strconv.Itoa(p.Ready) + " ready items in all.")
	}
	if p.Quiet != nil {
		b.WriteString(" It comes back to you because it is still ready for you and nothing has " +
			"moved it since " + p.Quiet.UTC().Format(time.RFC3339) + ": a comment that it still " +
			"waits, or an edit that leaves it ready, records nothing the engine can act on.")
	}
	b.WriteString("\n\nRead it with " + toolname.GetWorkItem + ", then answer it in this turn with " +
		"one of these five, each a change on " + p.Key + " itself:\n" + PickupAnswers + "\n" +
		"A comment alone is not an answer: this turn is not delivered until one of the five is on " +
		"the item, or the item has left you (removed, finished or reassigned by somebody else). " +
		"With an answer that takes it out of your ready work (a question, what blocks it, a wait, " +
		"another assignee), you are not handed this item again until it changes or the date it " +
		"waits until comes; with one that leaves it ready, a change to its content alone, or with " +
		"words only, it comes back to you a day later.")
	return b.String()
}

// PickupAnswers is the five typed answers to a work pickup, one per line: the
// brief lists them, and the refusal of an answer in words names them again.
const PickupAnswers = "- work it: change the item's own fields, its status (start it, finish it, " +
	"move it to review naming its `reviewer`) or its content, with " + toolname.UpdateWorkItem + ";\n" +
	"- if a person or a colleague must answer or decide first, ask them with " +
	toolname.CommentOnWorkItem + " and `ask` (a person with a `decision`), which puts it in " +
	"their Inbox;\n" +
	"- if another item or a pull request blocks it, record it with " + toolname.UpdateWorkItem +
	" (`waiting_on`), and withdraw in the same step any question of yours that it now covers (" +
	toolname.WithdrawAsk + " with `replaced_by`);\n" +
	"- if it waits until a real date, set `wait_until` and `wait_reason` with " +
	toolname.UpdateWorkItem + " (a wait on a person is an `ask`, never a date);\n" +
	"- if it is not yours to do, reassign it with " + toolname.UpdateWorkItem + " (`assignee`)."

// PickupAnswer is how a work pickup's turn left the item it was handed
// (ADR-0122), read off the tracker's rows: what the delivery checks accept a
// delivery or a block by, and what the turn's ending record says.
type PickupAnswer string

const (
	// PickupUnanswered is an item still the seat's with nothing typed from
	// it since the turn began: the one answer the checks refuse.
	PickupUnanswered PickupAnswer = "unanswered"
	// PickupTyped is a change to one of the item's fields, or a question put
	// on it with `ask`, by the seat since the turn began.
	PickupTyped PickupAnswer = "typed"
	// PickupRemoved is an item put in the trash, or purged, while the turn
	// ran: nothing typed is owed on it, and nothing could be written.
	PickupRemoved PickupAnswer = "removed"
	// PickupFinished is an item somebody else moved to a finished status
	// while the turn ran: nothing typed is owed on it.
	PickupFinished PickupAnswer = "finished"
	// PickupReassigned is an item somebody else handed to another seat, or
	// to nobody, while the turn ran: nothing typed is owed on it.
	PickupReassigned PickupAnswer = "reassigned"
	// PickupUnread is a read that failed: the turn falls back to the
	// delivery every turn owes rather than loop on a store it cannot reach.
	PickupUnread PickupAnswer = "unread"
)

// Valid reports whether an answer off the wire is one this build knows.
func (a PickupAnswer) Valid() bool {
	switch a {
	case PickupUnanswered, PickupTyped, PickupRemoved, PickupFinished, PickupReassigned, PickupUnread:
		return true
	}
	return false
}

// Moot reports an item that left the seat while its pickup's turn ran.
func (a PickupAnswer) Moot() bool {
	return a == PickupRemoved || a == PickupFinished || a == PickupReassigned
}

// Accepted reports an answer the delivery checks take: anything but an item
// still the seat's with nothing typed on it.
func (a PickupAnswer) Accepted() bool { return a.Valid() && a != PickupUnanswered }
