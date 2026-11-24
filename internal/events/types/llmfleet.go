package types

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/words"
)

// LLM fleets (internal/providers/llm/fleet): a seat job on `fleet:<key>` runs
// on whichever member the balancer picks. A fleet's hand-off is a move, not a
// provider_fallback, which stays for pinned lists.

func init() {
	events.Register[LLMFleetMoved]()
	events.Register[LLMFleetFull]()
	events.Register[LLMFleetRoom]()
}

// FleetDecision is the decision record of a turn on an LLM fleet: which fleet
// picked its member, by which strategy, at what share and why. Empty on a
// pinned turn.
type FleetDecision struct {
	Fleet    string `json:"fleet,omitempty"`
	Strategy string `json:"strategy,omitempty"`
	// ShareAtPick is the member's usage share in whole points when it was
	// picked, absent where unknown.
	ShareAtPick *int `json:"share_at_pick,omitempty"`
	// FleetReason is lowest_share, within_threshold, next_in_order,
	// overflow or limit_handover (fleet.Reason).
	FleetReason string `json:"fleet_reason,omitempty"`
}

// LLMFleetMoved records a seat job's work going to a member other than its
// home: between turns, mid-turn after a limit, or to nobody.
type LLMFleetMoved struct {
	Agent    string `json:"agent_id,omitempty"`
	RoleName string `json:"role,omitempty"`
	Fleet    string `json:"fleet"`
	// Subject is the seat's handle, or the helper; Job the phase.
	Subject string `json:"subject"`
	Job     string `json:"job"`
	// From is the home; To the member that took the work, null for none.
	From string  `json:"from"`
	To   *string `json:"to"`
	// Why is the reason in a person's words; Work what happened to the work
	// in flight.
	Why    string `json:"why"`
	Work   string `json:"work"`
	TurnID string `json:"turn_id,omitempty"`
}

// EventType is the "llm_fleet_moved" wire type.
func (LLMFleetMoved) EventType() string { return "llm_fleet_moved" }

// Role is the seat whose work moved.
func (e LLMFleetMoved) Role() string { return e.RoleName }

// AgentID is the instance whose work moved.
func (e LLMFleetMoved) AgentID() string { return e.Agent }

// SummaryFor names the move and its reason.
func (e LLMFleetMoved) SummaryFor(actor string) string {
	if e.To == nil {
		return lead(actor, fmt.Sprintf("%s: %s left %s, no member has room (%s)", e.Fleet, e.Job, e.From, e.Why))
	}
	return lead(actor, fmt.Sprintf("%s: %s moved %s → %s (%s)", e.Fleet, e.Job, e.From, *e.To, e.Why))
}

// LLMFleetFull records that no member of a fleet can take work: its seats
// wait, parked until the first room.
type LLMFleetFull struct {
	Fleet string `json:"fleet"`
	// Waiting are the subjects that wait.
	Waiting []string `json:"waiting"`
	// FirstRoomAt and FirstRoomMember are the earliest instant a member has
	// room again and that member, absent where no member names one.
	FirstRoomAt     *time.Time `json:"first_room_at"`
	FirstRoomMember string     `json:"first_room_member,omitempty"`
}

// EventType is the "llm_fleet_full" wire type.
func (LLMFleetFull) EventType() string { return "llm_fleet_full" }

// SummaryFor names the fleet and who waits.
func (e LLMFleetFull) SummaryFor(string) string {
	return fmt.Sprintf("Every member of fleet %s is at its limit: %s wait",
		e.Fleet, words.Count(len(e.Waiting), "seat", "seats"))
}

// LLMFleetRoom records that a member has room again after [LLMFleetFull],
// and the subjects released to it.
type LLMFleetRoom struct {
	Fleet    string   `json:"fleet"`
	Member   string   `json:"member"`
	Released []string `json:"released"`
}

// EventType is the "llm_fleet_room" wire type.
func (LLMFleetRoom) EventType() string { return "llm_fleet_room" }

// SummaryFor names the member with room.
func (e LLMFleetRoom) SummaryFor(string) string {
	return fmt.Sprintf("Room again in fleet %s on %s: %s released", e.Fleet, e.Member,
		words.Count(len(e.Released), "seat", "seats"))
}
