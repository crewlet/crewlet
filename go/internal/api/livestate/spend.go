package livestate

import "time"

// SpendRecord is one completed phase's token spend, in the shape the shared
// aggregator consumes.
//
// RECORDS are held rather than a folded rollup, so the aggregation has exactly
// one implementation instead of the three it had — the REST endpoint's, a
// re-implementation in the browser, and whatever a reconnect left behind.
type SpendRecord struct {
	EventID   string `json:"event_id"`
	Timestamp string `json:"timestamp"`

	AgentID   string `json:"agent_id"`
	AgentRole string `json:"agent_role"`

	Phase     string `json:"phase"`
	HostPhase string `json:"host_phase"`
	Worker    string `json:"worker"`
	Model     string `json:"model"`

	TurnID    string `json:"turn_id"`
	Iteration int    `json:"iteration"`

	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// foldSpend records one completed phase's spend, reporting whether it counted.
//
// Deduped by event id so a redelivered envelope cannot inflate the rollup, and
// window-pruned so a long-lived process does not keep aggregating spend that
// has aged out.
func (s *LiveState) foldSpend(env Envelope, payload map[string]any) bool {
	if env.ID != "" {
		if s.countedPhases.has(env.ID) {
			return false
		}
		s.countedPhases.put(env.ID, struct{}{})
	}
	s.spend = append(s.spend, SpendRecord{
		EventID:      env.ID,
		Timestamp:    env.Timestamp,
		AgentID:      str(payload, "agent_id"),
		AgentRole:    str(payload, "role", "agent_role"),
		Phase:        str(payload, "phase"),
		HostPhase:    str(payload, "host_phase"),
		Worker:       str(payload, "worker"),
		Model:        str(payload, "model", "provider_key"),
		TurnID:       str(payload, "turn_id"),
		Iteration:    num(payload, "iteration"),
		InputTokens:  num(payload, "input_tokens"),
		OutputTokens: num(payload, "output_tokens"),
		TotalTokens:  num(payload, "total_tokens"),
	})
	s.pruneSpend(env.Timestamp)
	return true
}

// pruneSpend drops records that have aged out of the live window.
//
// ORDER-INDEPENDENT by construction. Popping from the front is only correct
// while the slice is timestamp-ordered, and it is not reliably: the API
// subscribes to the stream before it hydrates, so a live event can land ahead
// of the older records hydration then appends behind it. One recent record at
// the head is enough to make a head-popping loop exit immediately and never
// prune again — the window would silently stop being a window.
//
// The sweep runs only when there is something to drop, so the common case costs
// one pass of comparisons and no allocation.
func (s *LiveState) pruneSpend(nowISO string) {
	if len(s.spend) > spendRecordLimit {
		// The count cap binds before the window for an org emitting more
		// than the cap in a day. Truncating the OLDEST is what makes a
		// rollup past the cap cover slightly less than a window rather
		// than report a wrong total.
		s.spend = append(make([]SpendRecord, 0, spendRecordLimit),
			s.spend[len(s.spend)-spendRecordLimit:]...)
	}
	now := newStamp(nowISO)
	if !now.valid {
		return
	}
	cutoff := stamp{t: now.t.Add(-LiveSpendWindow), valid: true,
		raw: now.t.Add(-LiveSpendWindow).Format(time.RFC3339Nano)}

	// A record whose own timestamp is unusable is KEPT, for the reason the
	// sandbox sweep keeps an undateable entry: it cannot be aged out on
	// time, and dropping it on that basis would be arbitrary. The count
	// cap above is what bounds those.
	aged := func(record SpendRecord) bool {
		ts := newStamp(record.Timestamp)
		return ts.valid && ts.before(cutoff)
	}
	stale := false
	for i := range s.spend {
		if aged(s.spend[i]) {
			stale = true
			break
		}
	}
	if !stale {
		return
	}
	kept := make([]SpendRecord, 0, len(s.spend))
	for _, record := range s.spend {
		if !aged(record) {
			kept = append(kept, record)
		}
	}
	s.spend = kept
}

// SpendRecords returns the records inside the live window.
//
// The aggregation itself lives with the REST endpoint that already implements
// it, so the live rollup and the queried one cannot disagree — the projection's
// job is to hold the records, not to fold them.
func (s *LiveState) SpendRecords() []SpendRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SpendRecord(nil), s.spend...)
}
