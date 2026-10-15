package types

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// A person's control over a running seat: pausing it, resuming it, a turn a
// pause stopped, and a note a person sent a running turn. A pause itself is a
// coordination record (coord.SeatPause); these are the fleet's account of each
// change, published once by the writer whose compare-and-set won, so two people
// pausing one seat at once put one row in the log rather than two.
//
// # Who did it, in three halves
//
// Every `…_by` here is the author iam.ActorFor names — the seat for a person
// the identity directory binds to one, the login for anybody else — beside its
// `…_by_kind` (iam.ActorKind's vocabulary: agent, human, operator, system) and
// the `operator_id` the gesture came through (`pat:<id>`, `session:<lineage>`,
// or the login where the principal is its own credential). The same three
// facts every tracker and page record carries, under the same names, so
// a feed row about a pause and the history row of the work it held up cannot
// name two different people.

func init() {
	events.Register[SeatPaused]()
	events.Register[SeatResumed]()
	events.Register[AgentTurnStopped]()
	events.Register[AgentTurnSteered]()
}

// SeatPaused records that a person paused a seat: it takes no new work until
// somebody resumes it, and its mail waits on its inbox meanwhile.
type SeatPaused struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`

	// PausedBy is who paused it, PausedByKind what sort of author that is,
	// and OperatorID the credential they did it through — see the file's
	// doc for the three halves.
	PausedBy     string `json:"paused_by"`
	PausedByKind string `json:"paused_by_kind"`
	OperatorID   string `json:"operator_id,omitempty"`

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
	phrase := "was paused by " + e.PausedBy
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

	// ResumedBy, ResumedByKind and OperatorID are who lifted it and the
	// credential they did it through, in the three halves of
	// [SeatPaused.PausedBy].
	ResumedBy     string `json:"resumed_by"`
	ResumedByKind string `json:"resumed_by_kind"`
	OperatorID    string `json:"operator_id,omitempty"`

	// PausedBy, PausedByKind and PausedAt are the pause this lifted, so the
	// row that ends a pause says how long it lasted and whose it was
	// without joining back to a row that may have aged out of the log.
	PausedBy     string    `json:"paused_by"`
	PausedByKind string    `json:"paused_by_kind"`
	PausedAt     time.Time `json:"paused_at"`
}

// EventType is the "seat_resumed" wire type.
func (SeatResumed) EventType() string { return "seat_resumed" }

// Role is the resumed seat.
func (e SeatResumed) Role() string { return e.RoleName }

// AgentID is the resumed seat's instance.
func (e SeatResumed) AgentID() string { return e.Agent }

// SummaryFor names who resumed the seat.
func (e SeatResumed) SummaryFor(actor string) string {
	return lead(actor, "was resumed by "+e.ResumedBy)
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

	// StoppedBy, StoppedByKind and OperatorID are who paused the seat with
	// the stop and the credential they did it through, in the three halves
	// of [SeatPaused.PausedBy], and Reason is what they gave.
	StoppedBy     string `json:"stopped_by"`
	StoppedByKind string `json:"stopped_by_kind"`
	OperatorID    string `json:"operator_id,omitempty"`
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
	return lead(actor, "had its turn stopped by "+e.StoppedBy)
}

// SteerOutcome is what became of a person's note to a running turn.
type SteerOutcome string

const (
	// SteerDelivered — the turn read the note: it entered the conversation
	// at a round boundary, and that round's provider call was the first to
	// see it.
	SteerDelivered SteerOutcome = "delivered"

	// SteerExpired — the turn ended or parked before its next round, so
	// nothing read the note. It is not carried to a later turn: a note is
	// an instruction about the work in flight, and a later turn is other
	// work.
	SteerExpired SteerOutcome = "expired"
)

// SteerOutcomes is every outcome this build records.
var SteerOutcomes = []SteerOutcome{SteerDelivered, SteerExpired}

// Valid reports whether an outcome off the wire is one this build knows.
func (o SteerOutcome) Valid() bool { return slices.Contains(SteerOutcomes, o) }

// AgentTurnSteered records what became of one person's note to a running turn
// (internal/agent/steer): read at a round boundary, or expired unread because
// the turn ended first.
//
// PUBLISHED BY THE NODE THAT RAN THE TURN, once it knows — never by the node
// that took the request, which answered `pending` and cannot see the turn. The
// note travels to the turn on an ephemeral scatter that leaves no record, so
// this row is the only durable account that the note existed at all; that is
// why it carries the note's text.
type AgentTurnSteered struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work the turn was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey string `json:"work_key,omitempty"`

	// NoteID is the note's identity, the person's request id: one request
	// is one note, however often it was retried.
	NoteID  string       `json:"note_id"`
	Outcome SteerOutcome `json:"outcome"`

	// Phase, Iteration and Round are where a DELIVERED note landed: the
	// phase and iteration that read it, and the round, on the phase's
	// one-based scale, whose provider call first saw it. Absent on an
	// expired note, which nothing read.
	Phase     Phase `json:"phase,omitempty"`
	Iteration int   `json:"iteration,omitempty"`
	Round     int   `json:"round,omitempty"`

	// Note is what the person wrote.
	Note string `json:"note"`

	// SteeredBy, SteeredByKind and OperatorID are who sent it and the
	// credential they sent it through, in the three halves of
	// [SeatPaused.PausedBy], and SentAt when the turn took it.
	SteeredBy     string    `json:"steered_by"`
	SteeredByKind string    `json:"steered_by_kind"`
	OperatorID    string    `json:"operator_id,omitempty"`
	SentAt        time.Time `json:"sent_at"`
}

// EventType is the "agent_turn_steered" wire type.
func (AgentTurnSteered) EventType() string { return "agent_turn_steered" }

// Role is the seat whose turn was steered.
func (e AgentTurnSteered) Role() string { return e.RoleName }

// AgentID is the instance whose turn was steered.
func (e AgentTurnSteered) AgentID() string { return e.Agent }

// SummaryFor names who sent the note and whether the turn read it.
func (e AgentTurnSteered) SummaryFor(actor string) string {
	if e.Outcome == SteerExpired {
		return lead(actor, "finished before reading a note from "+e.SteeredBy)
	}
	return lead(actor, "read a note from "+e.SteeredBy)
}
