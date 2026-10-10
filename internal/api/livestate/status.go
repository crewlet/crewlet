package livestate

import (
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// WHAT A MEMBER IS SERVED OF A SEAT: its state, and nothing the machine
// processed to reach it.
//
// ADR-0031 draws one line between the two keyed reaches. A member reads what the
// company published — who is working on what — and an admin also reads what the
// machine processed: prompts, responses, tool calls, narration, spend. A seat's
// overlay ([Overlay]) is the second kind through and through: the live call is
// the prompt in flight, its response and every tool result; the rounds are
// timings and token counts; the node is how the fleet is run. So a member is not
// served a trimmed overlay — a trim is a list of what to remove, and the next
// field somebody adds to the overlay is on it by default — but its own types,
// [AgentStatus], [SeatStatus] and [BudgetStatus], which name what they carry,
// field by field, and carry nothing else. TestAMemberIsServedNoMachineField
// holds each one's keys to a reviewed list.

// Step is where in a turn a seat is, in the three words a person reads: getting
// up to speed, doing the work, checking it.
//
// A NAMED STRING WITH Valid, the engine's enum idiom. The same three steps the
// dashboard's stepper draws (`TURN_STEPS`), computed here so a member — who is
// served no phase and no live call to derive them from — reads the same step an
// admin's screen draws for the same seat.
type Step string

const (
	// StepContext — the turn is assembling what it knows before it acts:
	// its context stage, and a seat's first-turn onboarding pass.
	StepContext Step = "context"
	// StepExecute — the turn is doing the work, or is parked on a coding
	// run that is.
	StepExecute Step = "execute"
	// StepReview — the turn is checking its work.
	StepReview Step = "review"
)

// Steps is the closed set, in the order a turn walks them.
var Steps = []Step{StepContext, StepExecute, StepReview}

// Valid reports whether s is a step this build knows.
func (s Step) Valid() bool { return slices.Contains(Steps, s) }

// AgentStatus is one seat's state as a member is served it: the `agents` push's
// row, and the live half of a member's roster row ([SeatStatus]).
//
// EVERY FIELD ALWAYS PRESENT, null where it has no value, for the reason
// [Overlay.Turn] is: a client merges a row onto the one it holds, so an omitted
// key reads as "unchanged" and would leave a seat that finished its turn still
// wearing the item and the step it was on.
type AgentStatus struct {
	// Handle is the seat's identity, which a client merges the row by.
	Handle string `json:"handle"`

	// Activity and StoppedReason are the seat-state vocabulary, exactly as
	// an admin's row states them (activity.go).
	Activity      Activity       `json:"activity"`
	StoppedReason *StoppedReason `json:"stopped_reason"`

	// WorkItem is the item the seat's turn is charged to (ADR-0022), as the
	// projection holds it — its key, id, project and tracker, never a title,
	// which would be a read of the tracker the projection does not make —
	// and null while the seat is on no turn or on one charged to nothing.
	WorkItem *types.WorkItem `json:"work_item"`

	// Step is where in its turn the seat is, and null while it is on none.
	Step *Step `json:"step"`

	// Since is when the seat's state began, RFC 3339 in UTC: the turn's
	// start while it works or waits on its run, the pause's while it is
	// paused, its last turn's end while it is idle — and null where the
	// projection cannot say (a stop it has no instant for, an idle seat
	// that has ended no turn it knows of).
	Since *string `json:"since"`

	// PausedBy is the seat of the person who paused it, and null while it is
	// not paused or was paused by a credential no seat binds: a credential's
	// name says how the engine is run, which an admin reads (ADR-0031).
	PausedBy *string `json:"paused_by"`
}

// SeatStatus is one roster row as a member is served it: the seat's public
// identity — the address every screen links it by, its name and its handle,
// which the org chart already shows to anybody — beside its [AgentStatus].
//
// NOT ITS AGENT ID, which the admin row carries for the budget scopes and the
// streamed phases it matches, neither of which a member is served.
type SeatStatus struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	AgentStatus
}

// BudgetStatus is the live token meter as a member is served it: WHICH scopes
// are refusing or near their ceiling, and when each window resets — and no
// figure. What a window has spent and what it is capped at are spend and
// configuration, which an admin reads; that a seat cannot take work until
// Monday is the one fact a person needs to plan around.
//
// A NIL BudgetStatus is "no report has arrived yet", as a nil [OrgBudget] is
// for an admin, and is distinct from one listing no scope: "nothing is near a
// ceiling".
type BudgetStatus struct {
	// Timezone is the company clock every window is cut on.
	Timezone string `json:"timezone"`

	// Org is the company's own windows that are refusing or near, empty
	// rather than absent while none is.
	Org []WindowStatus `json:"org"`

	// Seats is every seat with a window refusing or near, by handle, in
	// handle order; empty rather than absent while none has one.
	Seats []SeatBudgetStatus `json:"seats"`
}

// SeatBudgetStatus is one seat's refusing or near windows.
type SeatBudgetStatus struct {
	Handle  string         `json:"handle"`
	Windows []WindowStatus `json:"windows"`
}

// WindowStatus is one calendar window's state and when it turns over: the
// engine's judgement of a window ([types.BudgetState]) with its spend and its
// ceiling left out.
type WindowStatus struct {
	Period   string            `json:"period"`
	Window   string            `json:"window"`
	State    types.BudgetState `json:"state"`
	ResetsAt string            `json:"resets_at"`
}

// Statuses renders the member rows of the named seats, as the `agents` push
// carries them: one per seat the projection holds an entry for, skipping the
// rest for [LiveState.OverlayRows]' reason.
func (s *LiveState) Statuses(handles []string) []AgentStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AgentStatus, 0, len(handles))
	for _, handle := range handles {
		if s.agents[handle] == nil {
			continue
		}
		out = append(out, s.statusOf(handle))
	}
	return out
}

// MergeStatuses renders a member's roster from the static rows: each row's
// public identity — `id`, `role` and `handle`, and nothing else the row holds
// — with the seat's [AgentStatus]. EVERY ROW CARRIES A STATE, for
// [LiveState.MergeAgents]' reason. Order follows the input.
func (s *LiveState) MergeStatuses(static []map[string]any) []SeatStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SeatStatus, 0, len(static))
	for _, row := range static {
		id, _ := row["id"].(string)
		role, _ := row["role"].(string)
		handle, _ := row["handle"].(string)
		st := s.statusOf(handle)
		st.Handle = handle
		out = append(out, SeatStatus{ID: id, Role: role, AgentStatus: st})
	}
	return out
}

// statusOf is one seat's member row. A seat with no live entry has a state all
// the same, and nulls for the rest.
//
// Called under s.mu.
func (s *LiveState) statusOf(handle string) AgentStatus {
	agent := s.agents[handle]
	st := s.stateOf(handle, agent)
	out := AgentStatus{
		Handle:        handle,
		Activity:      st.activity,
		StoppedReason: st.stoppedReason(),
	}
	if agent == nil {
		return out
	}
	if agent.turn != nil {
		out.WorkItem = cloneItem(agent.turn.WorkItem)
		step := agent.step()
		out.Step = &step
	}
	out.Since = agent.since(st)
	if agent.paused != nil && agent.paused.BySeat {
		by := agent.paused.By
		out.PausedBy = &by
	}
	return out
}

// step is where in its turn the seat is, BY THE RULE THE DASHBOARD'S STEPPER
// APPLIES TO AN ADMIN'S ROW (`turnSteps`, lib/turnsteps.ts), so a member and an
// admin looking at one seat at one moment read one step: a PARKED turn is in
// Execute, its coding run being the executor's work; otherwise the turn's
// current phase — `review` is Review, `execute` is Execute, and anything else
// is Context: the context stage before any phase has opened, and a seat's
// first-turn onboarding pass. The stepper reads the call's phase before the
// row's, and the two are one value: a phase's opening and every round of its
// call set both. A phase nested under another (a worker, a judge) opens
// nothing and streams no call, so it never moves the step off its host.
func (a *agentLive) step() Step {
	if a.turn.Stage == StageParked {
		return StepExecute
	}
	switch types.Phase(a.currentPhase) {
	case types.PhaseReview:
		return StepReview
	case types.PhaseExecute:
		return StepExecute
	}
	return StepContext
}

// since is when the seat's current state began, or nil where the projection
// holds no instant for it. See [AgentStatus.Since].
func (a *agentLive) since(st seatState) *string {
	var at string
	switch {
	case st.activity == ActivityStopped && st.reason == StoppedPaused && a.paused != nil:
		at = a.paused.At
	case (st.activity == ActivityWorking || st.activity == ActivityNeeds) && a.turn != nil:
		at = a.turn.StartedAt
	case st.activity == ActivityIdle && a.lastTurn != nil:
		at = a.lastTurn.EndedAt
	}
	if at == "" {
		return nil
	}
	return &at
}

// BudgetStatus is the live meter as a member is served it, or nil before any
// report has arrived. See [BudgetStatus].
//
// A WINDOW WHOSE END HAS PASSED IS NOT LISTED, whatever its last report said:
// the park it described lifted at its end, the bound [refusing] applies to a
// seat's stop, and a window listed as refusing after it turned over would tell
// a person to wait for a reset that has already come.
func (s *LiveState) BudgetStatus() *BudgetStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.budget == nil {
		return nil
	}
	now := s.clock()
	out := &BudgetStatus{
		Timezone: s.budget.Timezone,
		Org:      pressing(s.budget.Org.Windows, now),
		Seats:    []SeatBudgetStatus{},
	}
	for _, handle := range slices.Sorted(maps.Keys(s.agents)) {
		agent := s.agents[handle]
		if agent.budget == nil {
			continue
		}
		if windows := pressing(agent.budget.Windows, now); len(windows) > 0 {
			out.Seats = append(out.Seats, SeatBudgetStatus{Handle: handle, Windows: windows})
		}
	}
	return out
}

// pressing is the windows refusing or near their ceiling and not yet turned
// over, figures left out; empty rather than nil.
func pressing(windows []WindowMeter, now time.Time) []WindowStatus {
	out := []WindowStatus{}
	for _, w := range windows {
		if w.State != types.BudgetRefusing && w.State != types.BudgetNear {
			continue
		}
		if ends, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil && !now.Before(ends) {
			continue
		}
		out = append(out, WindowStatus{Period: w.Period, Window: w.Window, State: w.State, ResetsAt: w.ResetsAt})
	}
	return out
}
