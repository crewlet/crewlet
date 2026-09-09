package types

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/events"
)

// The LLM provider chain: falling through it, and running out of it.
//
// error_kind on these (and on the phase/turn events) is the classifier's output
// — rate_limit, auth, server, timeout, pool_exhausted for an exhausted
// credential pool — but it is deliberately an open string: an unclassified
// failure reports the exception's own type name rather than being forced into a
// bucket that would misdescribe it.

func init() {
	events.Register[LLMUnavailable]()
	events.Register[ProviderFallback]()
}

// LLMUnavailable fires when the fallback chain is exhausted.
//
// Distinct from ProviderFallback, which reports a single attempt failing while
// the chain is still in progress. This one means no provider in the role's
// chain succeeded: the agent is effectively AFK and the turn terminates as
// failed, which is why its type is in FailureEventTypes.
type LLMUnavailable struct {
	Agent         string   `json:"agent_id"`
	RoleName      string   `json:"role"`
	ProviderChain []string `json:"provider_chain,omitempty"`
	AttemptCount  int      `json:"attempt_count"`
	LastErrorKind string   `json:"last_error_kind"`
	LastError     string   `json:"last_error"`
	TurnID        string   `json:"turn_id"`
}

// EventType is the "llm_unavailable" wire type, and one of the four names in
// FailureEventTypes.
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
	Agent     string `json:"agent_id"`
	RoleName  string `json:"role"`
	TurnID    string `json:"turn_id"`
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
