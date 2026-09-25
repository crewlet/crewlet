package types

import (
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// A person's control over a running seat: pausing it, resuming it, and a turn
// a pause stopped. A pause itself is a coordination record (coord.SeatPause);
// these are the fleet's account of each change, published once by the writer
// whose compare-and-set won, so two people pausing one seat at once put one row
// in the log rather than two.

func init() {
	events.Register[SeatPaused]()
	events.Register[SeatResumed]()
	events.Register[AgentTurnStopped]()
}

// SeatPaused records that a person paused a seat: it takes no new work until
// somebody resumes it, and its mail waits on its inbox meanwhile.
type SeatPaused struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`

	// PausedBy is who paused it — the author, which for a person acting
	// through a credential is the token's own name — and PausedBySeat the
	// person that credential is bound to, empty for one nobody bound.
	PausedBy     string `json:"paused_by"`
	PausedBySeat string `json:"paused_by_seat,omitempty"`

	// Reason is why, in the pauser's own words. Optional.
	Reason string `json:"reason,omitempty"`

	// StopRunning is whether the pause also asked the turn the seat was on
	// to stop at its next round rather than finish. Its effect, when a turn
	// was running, is that turn's own [AgentTurnStopped].
	StopRunning bool `json:"stop_running,omitempty"`

	// PausedAt is when the pause was taken — the record's own instant,
	// which a re-announced pause (one amended to stop the running turn)
	// keeps.
	PausedAt time.Time `json:"paused_at"`
}

// EventType is the "seat_paused" wire type.
func (SeatPaused) EventType() string { return "seat_paused" }

// Role is the paused seat.
func (e SeatPaused) Role() string { return e.RoleName }

// AgentID is the paused seat's instance.
func (e SeatPaused) AgentID() string { return e.Agent }

// SummaryFor names who paused the seat, which is the one question a feed row
// about a pause is asked.
func (e SeatPaused) SummaryFor(actor string) string {
	who := e.PausedBySeat
	if who == "" {
		who = e.PausedBy
	}
	phrase := "was paused by " + who
	if e.StopRunning {
		phrase += ", stopping its turn"
	}
	return lead(actor, phrase)
}

// SeatResumed records that a person lifted a seat's pause, so it takes work
// again — the mail that waited first, in order.
type SeatResumed struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`

	// ResumedBy and ResumedBySeat are who lifted it, in the same two halves
	// as [SeatPaused.PausedBy].
	ResumedBy     string `json:"resumed_by"`
	ResumedBySeat string `json:"resumed_by_seat,omitempty"`

	// PausedBy and PausedAt are the pause this lifted, so the row that ends
	// a pause says how long it lasted and whose it was without joining back
	// to a row that may have aged out of the log.
	PausedBy string    `json:"paused_by"`
	PausedAt time.Time `json:"paused_at"`
}

// EventType is the "seat_resumed" wire type.
func (SeatResumed) EventType() string { return "seat_resumed" }

// Role is the resumed seat.
func (e SeatResumed) Role() string { return e.RoleName }

// AgentID is the resumed seat's instance.
func (e SeatResumed) AgentID() string { return e.Agent }

// SummaryFor names who resumed the seat.
func (e SeatResumed) SummaryFor(actor string) string {
	who := e.ResumedBySeat
	if who == "" {
		who = e.ResumedBy
	}
	return lead(actor, "was resumed by "+who)
}

// AgentTurnStopped records that a turn was ended by a person rather than by
// itself: a pause that asked to stop the running turn reached that turn's next
// round, and the turn ended there.
//
// THE TRIGGER IS SPENT, not handed back: the dispatcher records it in the
// completion ledger, because a person who stopped a turn did not ask for it to
// be run again the moment they resume the seat. What the turn did before the
// stop is on its own phases; the turn's completion carries `stopped` rather
// than `failed`, and this row says who stopped it.
type AgentTurnStopped struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work the turn was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey string `json:"work_key,omitempty"`

	// StoppedBy and StoppedBySeat are who paused the seat with the stop,
	// in the same two halves as [SeatPaused.PausedBy], and Reason is what
	// they gave.
	StoppedBy     string `json:"stopped_by"`
	StoppedBySeat string `json:"stopped_by_seat,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// EventType is the "agent_turn_stopped" wire type.
func (AgentTurnStopped) EventType() string { return "agent_turn_stopped" }

// Role is the seat whose turn was stopped.
func (e AgentTurnStopped) Role() string { return e.RoleName }

// AgentID is the instance whose turn was stopped.
func (e AgentTurnStopped) AgentID() string { return e.Agent }

// SummaryFor names who stopped the turn.
func (e AgentTurnStopped) SummaryFor(actor string) string {
	who := e.StoppedBySeat
	if who == "" {
		who = e.StoppedBy
	}
	return lead(actor, "had its turn stopped by "+who)
}
