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
	events.Register(LLMUnavailable{})
	events.Register(ProviderFallback{})
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

func (LLMUnavailable) EventType() string { return "llm_unavailable" }

func (e LLMUnavailable) Role() string    { return e.RoleName }
func (e LLMUnavailable) AgentID() string { return e.Agent }

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
type ProviderFallback struct {
	AgentHandle     string `json:"agent_handle"`
	Phase           Phase  `json:"phase"`
	FromProviderKey string `json:"from_provider_key"`
	ToProviderKey   string `json:"to_provider_key"`
	ErrorKind       string `json:"error_kind"`
}

func (ProviderFallback) EventType() string { return "provider_fallback" }

// SummaryFor is handed the publisher as its actor: this event carries a handle
// rather than a role or an agent id, and neither the chain nor its Python
// original reads a handle — contributing AgentHandle as the agent id would
// change behaviour rather than port it.
func (e ProviderFallback) SummaryFor(actor string) string {
	return lead(actor, fmt.Sprintf("fallback %s → %s (%s)",
		e.FromProviderKey, e.ToProviderKey, e.ErrorKind))
}
