package types

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// The LLM provider chain: falling through it, and running out of it.
//
// error_kind on these (and on the phase/turn events) is the classifier's output
// — rate_limit, auth, server, timeout, fatal — but it is deliberately an open
// string: an unclassified
// failure reports the exception's own type name rather than being forced into a
// bucket that would misdescribe it.

func init() {
	events.Register[LLMUnavailable]()
	events.Register[ProviderFallback]()
	events.Register[SeatProviderParked]()
	events.Register[SeatProviderReleased]()
}

// LLMUnavailable fires when the fallback chain is exhausted.
//
// Distinct from ProviderFallback, which reports a single attempt failing while
// the chain is still in progress. This one means no provider in the role's
// chain succeeded: the agent is effectively AFK and the turn terminates as
// failed, which is why its type is a failure BY TYPE — see [Failed].
type LLMUnavailable struct {
	Agent         string   `json:"agent_id"`
	RoleName      string   `json:"role"`
	ProviderChain []string `json:"provider_chain,omitempty"`
	AttemptCount  int      `json:"attempt_count"`
	LastErrorKind string   `json:"last_error_kind"`
	LastError     string   `json:"last_error"`
	TurnID        string   `json:"turn_id"`
	// WorkKey is the unit of work this run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey string `json:"work_key,omitempty"`
}

// EventType is the "llm_unavailable" wire type, and one of the four
// [FailureEventNames].
func (LLMUnavailable) EventType() string { return "llm_unavailable" }

// Role is the seat whose provider chain ran out — the seat that goes AFK.
func (e LLMUnavailable) Role() string { return e.RoleName }

// AgentID is the instance whose turn terminates as failed.
func (e LLMUnavailable) AgentID() string { return e.Agent }

// SummaryFor counts the providers that were tried. The chain itself is on the
// payload; the count is what says whether one flaky provider or a whole
// misconfigured chain took the seat down.
func (e LLMUnavailable) SummaryFor(actor string) string {
	tried := fmt.Sprintf("(%d providers tried)", len(e.ProviderChain))
	if actor == "" {
		// The actor sits mid-sentence rather than opening the line, so an
		// unknown one drops its whole clause instead of leaving "for  (…)".
		// The envelope always resolves one; this is for a direct caller.
		return "LLM unavailable " + tried
	}
	return "LLM unavailable for " + actor + " " + tried
}

// ProviderFallback fires each time the chain falls through from one provider to
// the next. Dashboards count these to spot an unstable provider before it
// exhausts a chain and takes a turn down with it.
//
// IT IS ADDRESSED LIKE A PHASE EVENT — agent id, role, turn id, iteration —
// because the question it answers is "what happened during THIS turn", and for
// as long as it carried only a handle it could not be asked that: `turn_id` is
// the promoted column the turn lookup selects on, so a payload without one is
// invisible to the screen built to show a turn end to end. The Turn screen's
// own subtitle promised these rows and could never have shown one.
type ProviderFallback struct {
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	TurnID   string `json:"turn_id"`
	// WorkKey is the unit of work this run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey   string `json:"work_key,omitempty"`
	Iteration int    `json:"iteration"`
	Phase     Phase  `json:"phase"`
	// FromProviderKey and ToProviderKey are the providers.llm keys an
	// operator configured, not model ids: the key is what they recognise
	// and what they would edit. ToProviderKey is EMPTY on the last member,
	// where the fallback is to nothing and the next event is
	// LLMUnavailable.
	FromProviderKey string `json:"from_provider_key"`
	ToProviderKey   string `json:"to_provider_key"`
	ErrorKind       string `json:"error_kind"`
	// ResetsAt is when the vendor of FromProviderKey said its limit lifts,
	// for a `rate_limit` hand-off whose report named an instant: what a
	// screen showing a seat carried onto its fallback says the head is
	// back at. Absent otherwise.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// FromCLI and FromSubscription are, for a `rate_limit` hand-off, the
	// CLI FromProviderKey drives and whether it signs in on a plan: what the
	// limit is called where a person reads it ("Claude Code subscription
	// limit reached"), without the entry's configuration. Absent otherwise.
	FromCLI          string `json:"from_cli,omitempty"`
	FromSubscription bool   `json:"from_subscription,omitempty"`
}

// EventType is the "provider_fallback" wire type.
func (ProviderFallback) EventType() string { return "provider_fallback" }

// Role is the seat whose chain fell through.
func (e ProviderFallback) Role() string { return e.RoleName }

// AgentID is the instance running the phase that fell through.
func (e ProviderFallback) AgentID() string { return e.Agent }

// SummaryFor names both keys and the classified failure, and says so plainly
// when there was nowhere left to fall to: an empty ToProviderKey is the last
// member of the chain, and rendering it as "→ " would read as a truncated line
// rather than as the end of the chain.
func (e ProviderFallback) SummaryFor(actor string) string {
	if e.ToProviderKey == "" {
		return lead(actor, fmt.Sprintf("%s failed (%s) — no provider left in the chain",
			e.FromProviderKey, e.ErrorKind))
	}
	return lead(actor, fmt.Sprintf("fallback %s → %s (%s)",
		e.FromProviderKey, e.ToProviderKey, e.ErrorKind))
}

// SeatProviderParked records that a seat was PARKED because every member of
// its model chain refused for a rate limit: its mail waits on its inbox, in
// order, until the first member is tried again, rather than being redelivered
// into the same refusal every second (internal/engine/providerpark.go).
//
// Published at each park, and again each time the engine's probe (a call
// outside any turn) meets the same limit, so it is a statement of the seat's
// condition rather than of a change: a reader holds the latest, and a renewal
// with the same instants changes nothing it draws. The condition ends when a
// call through the seat's chain answers.
type SeatProviderParked struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`

	// ProviderKeys are the providers.llm keys of the chain, in its order,
	// every one of which refused.
	ProviderKeys []string `json:"provider_keys"`
	// RetryAt is when the park probes the chain: the earliest instant a
	// member is tried again.
	RetryAt time.Time `json:"retry_at"`
	// ResetsAt is when the vendors said the limit lifts, where every member
	// named an instant; absent where any did not, which a person is told as
	// a resume time that is not known.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// Said is the head member's own sentence.
	Said string `json:"said,omitempty"`
	// CLI and Subscription are the head member's CLI (`claude-code`) and
	// whether it signs in on a plan: what the limit is called.
	CLI          string `json:"cli,omitempty"`
	Subscription bool   `json:"subscription,omitempty"`
}

// EventType is the "seat_provider_parked" wire type.
func (SeatProviderParked) EventType() string { return "seat_provider_parked" }

// Role is the parked seat.
func (e SeatProviderParked) Role() string { return e.RoleName }

// AgentID is the parked seat's instance.
func (e SeatProviderParked) AgentID() string { return e.Agent }

// SummaryFor says what the seat waits for, and until when where that is known.
func (e SeatProviderParked) SummaryFor(actor string) string {
	if e.ResetsAt == nil {
		return lead(actor, "is waiting for its model's usage limit to lift")
	}
	return lead(actor, "is waiting for its model's usage limit to lift at "+
		e.ResetsAt.UTC().Format(time.RFC3339))
}

// SeatProviderReleased records that a seat's provider park was lifted: its
// probe was answered (or failed for a reason that is not a limit), the seat's
// chain changed under it, or the node gave the seat up. A probe that meets the
// limit again lifts nothing, and publishes a [SeatProviderParked] instead.
type SeatProviderReleased struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	// Reason is why, in the engine's words.
	Reason string `json:"reason"`
}

// EventType is the "seat_provider_released" wire type.
func (SeatProviderReleased) EventType() string { return "seat_provider_released" }

// Role is the released seat.
func (e SeatProviderReleased) Role() string { return e.RoleName }

// AgentID is the released seat's instance.
func (e SeatProviderReleased) AgentID() string { return e.Agent }

// SummaryFor names why the wait ended.
func (e SeatProviderReleased) SummaryFor(actor string) string {
	return lead(actor, "stopped waiting for its model's usage limit: "+e.Reason)
}
