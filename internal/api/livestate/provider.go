package livestate

import "slices"

// A SEAT'S MODELS AT THEIR USAGE LIMIT, as the dashboard draws it.
//
// One steady condition for as long as the limit lasts, and that is the whole
// point of it: the seat's turns, its failures and its probes all move while a
// limit holds, and a screen that drew the condition from them blinked between
// working, failed and stopped about once a second. The condition is moved only
// by what says the limit began or ended:
//
//   - `seat_provider_parked`: every model of the seat's chain refused, and the
//     engine parked the seat until the first is tried again
//     (internal/engine/providerpark.go). The seat reads `stopped`/`provider`,
//     even through the probe the engine runs when its alarm lifts the park;
//   - `provider_fallback` of kind `rate_limit` onto a next member: the seat's
//     head is limited and the seat runs on its fallback, so it is NOT stopped,
//     and the condition says which entry carries it;
//   - a phase that completes through the limited entry, or, for a parked seat,
//     through any entry: a call answered, so the limit is over as far as this
//     seat knows. As is a phase answered by an entry other than the fallback
//     it was running on, which is the seat's chain having changed;
//   - `seat_provider_released`, a park lifted for a reason other than its
//     probe, and `agent_spawned`, a new instance of the seat.
//
// A probe's release publishes nothing, so nothing here moves between a probe
// and the limit it meets again.

// ProviderLimit is a seat's models at their usage limit.
type ProviderLimit struct {
	// ProviderKey is the providers.llm key of the limited entry: the chain's
	// head for a parked seat, the member it fell from for one on its
	// fallback.
	ProviderKey string `json:"provider_key"`
	// ProviderKeys are, for a parked seat, every key of its chain, each of
	// which refused; the one limited entry otherwise.
	ProviderKeys []string `json:"provider_keys"`
	// Parked is whether the seat waits for the limit (every member refused)
	// rather than running on a fallback.
	Parked bool `json:"parked"`
	// RunningOn is the fallback carrying the seat while its head is
	// limited; nil for a parked seat.
	RunningOn *string `json:"running_on"`
	// ResetsAt is when the vendor said the limit lifts, RFC 3339 in UTC;
	// nil where that is not known.
	ResetsAt *string `json:"resets_at"`
	// RetryAt is when a parked seat's models are tried again; nil for a seat
	// on its fallback, which is never held.
	RetryAt *string `json:"retry_at"`
	// Since is when the condition began, RFC 3339 in UTC: the instant of the
	// event that first reported it, which a renewal keeps.
	Since string `json:"since"`
	// Said is the vendor's own sentence, where the report carried it.
	Said string `json:"said,omitempty"`
	// CLI is the CLI the limited entry drives (`claude-code`), and
	// Subscription whether it signs in on a plan: what the limit is called
	// where a person reads it, without the entry's configuration, which only
	// an admin reads. CLI is empty where the report did not say.
	CLI          string `json:"cli"`
	Subscription bool   `json:"subscription"`
}

func (p *ProviderLimit) clone() *ProviderLimit {
	if p == nil {
		return nil
	}
	c := *p
	c.ProviderKeys = slices.Clone(p.ProviderKeys)
	if p.RunningOn != nil {
		on := *p.RunningOn
		c.RunningOn = &on
	}
	if p.ResetsAt != nil {
		at := *p.ResetsAt
		c.ResetsAt = &at
	}
	if p.RetryAt != nil {
		at := *p.RetryAt
		c.RetryAt = &at
	}
	return &c
}

func sameLimit(a, b *ProviderLimit) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ProviderKey == b.ProviderKey && slices.Equal(a.ProviderKeys, b.ProviderKeys) &&
		a.Parked == b.Parked && samePtr(a.RunningOn, b.RunningOn) &&
		samePtr(a.ResetsAt, b.ResetsAt) && samePtr(a.RetryAt, b.RetryAt) &&
		a.Since == b.Since && a.Said == b.Said && a.CLI == b.CLI &&
		a.Subscription == b.Subscription
}

func samePtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// applyProviderLimit moves a seat's limit for one event, reporting whether it
// did. Ordered by the event's own instant against the last one that moved it,
// as a pause is, because the events that move it travel on different subjects.
func (s *LiveState) applyProviderLimit(agent *agentLive, env Envelope, payload map[string]any) bool {
	next, applies := s.nextLimit(agent.providerLimit, env, payload)
	if !applies {
		return false
	}
	at := newStamp(env.Timestamp)
	if !at.empty() && !agent.providerLimitAt.empty() && at.before(agent.providerLimitAt) {
		return false
	}
	if !at.empty() {
		agent.providerLimitAt = at
	}
	before := agent.providerLimit
	agent.providerLimit = next
	return !sameLimit(before, next)
}

// nextLimit is what env makes of a seat's limit, and whether env speaks to it
// at all.
func (s *LiveState) nextLimit(was *ProviderLimit, env Envelope, payload map[string]any) (*ProviderLimit, bool) {
	since := env.Timestamp
	switch env.Type {
	case "seat_provider_parked":
		keys := strList(payload, "provider_keys")
		head := ""
		if len(keys) > 0 {
			head = keys[0]
		}
		if was != nil && was.ProviderKey == head {
			since = was.Since
		}
		return &ProviderLimit{
			ProviderKey: head, ProviderKeys: keys, Parked: true,
			ResetsAt: optional(str(payload, "resets_at")), RetryAt: optional(str(payload, "retry_at")),
			Since: since, Said: str(payload, "said"),
			CLI: str(payload, "cli"), Subscription: flag(payload, "subscription"),
		}, true

	case "provider_fallback":
		to := str(payload, "to_provider_key")
		if str(payload, "error_kind") != "rate_limit" || to == "" {
			return was, false
		}
		if was != nil && was.Parked {
			// A PARKED SEAT'S PROBE, falling through its chain into the
			// same limit: the park that follows says what holds.
			return was, false
		}
		from := str(payload, "from_provider_key")
		if was != nil && was.ProviderKey == from {
			since = was.Since
		}
		return &ProviderLimit{
			ProviderKey: from, ProviderKeys: []string{from}, RunningOn: optional(to),
			ResetsAt: optional(str(payload, "resets_at")), Since: since,
			CLI: str(payload, "from_cli"), Subscription: flag(payload, "from_subscription"),
		}, true

	case "agent_phase_completed":
		if was == nil || flag(payload, "failed") {
			return was, false
		}
		answered := str(payload, "provider_key")
		switch {
		case was.Parked, answered == was.ProviderKey:
			return nil, true
		case was.RunningOn != nil && answered != *was.RunningOn:
			return nil, true
		}
		return was, false

	case "seat_provider_released", "agent_spawned":
		return nil, was != nil
	}
	return was, false
}

// strList reads a list of strings, dropping anything in it that is not one.
func strList(payload map[string]any, key string) []string {
	raw, _ := payload[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
