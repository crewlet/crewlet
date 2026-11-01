package livestate

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
//     (internal/engine/providerpark.go). The seat reads `stopped`/`provider`.
//     The engine's probe is a call outside any turn, and one that meets the
//     limit publishes this again, which renews the condition and moves
//     nothing a reader draws;
//   - `provider_fallback` of kind `rate_limit` onto a next member: the seat's
//     head is limited and the seat runs on its fallback, so it is NOT stopped;
//   - a phase that completes through the limited entry, or, for a parked seat,
//     through any entry: a call answered, so the limit is over as far as this
//     seat knows — and a launch of the executor's run collected the same way,
//     which is its harness's calls answered through the entry the launch picked.
//     As is a phase answered by an entry other than the fallback it was
//     running on, which is the seat's chain having changed;
//   - `seat_provider_released`, a park lifted (its probe answered, its chain
//     changed, or the node gave the seat up), and `agent_spawned`, a new
//     instance of the seat.
//
// # What it says, and what it does not
//
// Livestate reaches EVERY reader of the dashboard, so the condition carries
// what a person is told and nothing else: what the limit is OF (the harness's
// product and whether it signs in on a plan), when the vendor said it lifts,
// since when it holds, and whether the seat waits for it or runs on a fallback
// meanwhile. Which `providers.llm` entries are limited and which one carries
// the seat is the company's configuration, which only an admin reads (on
// Settings, Models), so the entry keys the condition is moved by are held
// here and never broadcast. Nor is the vendor's own sentence, which can name
// the account's zone and plan.

// ProviderLimit is a seat's models at their usage limit.
type ProviderLimit struct {
	// Parked is whether the seat waits for the limit (every member refused)
	// rather than running on a fallback, which is not named.
	Parked bool `json:"parked"`
	// ResetsAt is when the vendor said the limit lifts, RFC 3339 in UTC;
	// nil where that is not known.
	ResetsAt *string `json:"resets_at"`
	// Since is when the condition began, RFC 3339 in UTC: the instant of the
	// event that first reported it, which a renewal keeps.
	Since string `json:"since"`
	// CLI is the harness the limited entry drives (`claude-code`), and
	// Subscription whether it signs in on a plan: what the limit is called
	// where a person reads it. CLI is empty where the report did not say.
	CLI          string `json:"cli"`
	Subscription bool   `json:"subscription"`

	// entry is the providers.llm key of the limited entry (the chain's head
	// for a parked seat, the member it fell from for one on its fallback),
	// and runningOn the fallback carrying the seat, empty for a parked one.
	// What moves the condition, and never broadcast (see the file's doc).
	entry, runningOn string
}

func (p *ProviderLimit) clone() *ProviderLimit {
	if p == nil {
		return nil
	}
	c := *p
	if p.ResetsAt != nil {
		at := *p.ResetsAt
		c.ResetsAt = &at
	}
	return &c
}

// sameLimit reports whether two limits read alike, entries included: a reader
// draws neither, but the next event is judged against them.
func sameLimit(a, b *ProviderLimit) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Parked == b.Parked && samePtr(a.ResetsAt, b.ResetsAt) && a.Since == b.Since &&
		a.CLI == b.CLI && a.Subscription == b.Subscription &&
		a.entry == b.entry && a.runningOn == b.runningOn
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
		head := ""
		if keys := strList(payload, "provider_keys"); len(keys) > 0 {
			head = keys[0]
		}
		if was != nil && was.entry == head {
			since = was.Since
		}
		return &ProviderLimit{
			Parked: true, ResetsAt: optional(str(payload, "resets_at")), Since: since,
			CLI: str(payload, "cli"), Subscription: flag(payload, "subscription"), entry: head,
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
		if was != nil && was.entry == from {
			since = was.Since
		}
		return &ProviderLimit{
			ResetsAt: optional(str(payload, "resets_at")), Since: since,
			CLI: str(payload, "from_cli"), Subscription: flag(payload, "from_subscription"),
			entry: from, runningOn: to,
		}, true

	case "agent_phase_completed", launchType:
		if was == nil || flag(payload, "failed") {
			return was, false
		}
		answered := str(payload, "provider_key")
		switch {
		case was.Parked, answered == was.entry:
			return nil, true
		case was.runningOn != "" && answered != was.runningOn:
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
