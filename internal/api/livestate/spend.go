package livestate

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/tokens"
)

// The projection HOLDS records and never folds them.
//
// The aggregation lives in internal/tokens, which both this and the event
// store hand records to, so the live rollup and the queried one have one
// implementation. It had three once — the REST endpoint's, a
// re-implementation in the browser, and whatever a reconnect left behind —
// and a refresh routinely disagreed with the page it replaced.

// foldSpend records one completed phase's spend, reporting whether it counted.
//
// Deduped by event id so a redelivered envelope cannot inflate the rollup, and
// window-pruned so a long-lived process does not keep aggregating spend that
// has aged out.
func (s *LiveState) foldSpend(env Envelope, payload map[string]any) bool {
	if env.ID != "" {
		if _, counted := s.spendIDs[env.ID]; counted {
			return false
		}
		s.spendIDs[env.ID] = struct{}{}
	}
	// The stamp is PARSED ONCE, here, and carried with the record. The
	// prune below tests every retained record's age on every spend event,
	// and re-parsing them — up to three layouts each, twice per pass —
	// happened inside the projection's write lock, which is the mutex
	// every /agents request and every websocket snapshot waits on.
	s.spend = append(s.spend, spendEntry{at: newStamp(env.Timestamp), Record: tokens.Record{
		EventID:      env.ID,
		Timestamp:    env.Timestamp,
		AgentID:      str(payload, "agent_id"),
		AgentRole:    str(payload, "role", "agent_role"),
		Phase:        str(payload, "phase"),
		HostPhase:    str(payload, "host_phase"),
		Worker:       str(payload, "worker"),
		Model:        str(payload, "model", "provider_key"),
		TurnID:       str(payload, "turn_id"),
		WorkKey:      str(payload, "work_key"),
		Iteration:    num(payload, "iteration"),
		InputTokens:  num(payload, "input_tokens"),
		OutputTokens: num(payload, "output_tokens"),
		TotalTokens:  num(payload, "total_tokens"),
		CostUSD:      fraction(payload, "cost_usd"),
	}})
	s.pruneSpend(env.Timestamp)
	return true
}

// pruneSpend drops records that have aged out of the live window.
//
// ORDER-INDEPENDENT by construction. Popping from the front is only correct
// while the slice is timestamp-ordered, and the live path does not keep it so:
// a broadcast subscription reads across topics with no order between them, and
// a fleet's nodes stamp their events with clocks that disagree, so an older
// record can land behind a newer one. One recent record at the head is enough
// to make a head-popping loop exit immediately and never prune again, and the
// window would silently stop being a window.
//
// The sweep runs only when there is something to drop, so the common case costs
// one pass of comparisons and no allocation.
func (s *LiveState) pruneSpend(nowISO string) {
	if len(s.spend) > SpendRecordLimit {
		// The count cap binds before the window for an org emitting more
		// than the cap in a day. Truncating the OLDEST is what makes a
		// rollup past the cap cover slightly less than a window rather
		// than report a wrong total — but only once the heading says so,
		// which is what this flag is for: see [LiveState.SpendRecords].
		// It LATCHES, because a rollup's window is a fact about what was
		// dropped rather than about what is held now.
		cut := len(s.spend) - SpendRecordLimit
		s.spendCapped = true
		s.forgetSpend(s.spend[:cut])
		s.spend = append(make([]spendEntry, 0, SpendRecordLimit),
			s.spend[cut:]...)
	}
	now := newStamp(nowISO)
	if !now.valid {
		return
	}
	// No `raw`: it is only read when a comparison has an INVALID side, and
	// aged() below tests validity first, so the formatted string was
	// computed on every prune and never looked at.
	cutoff := stamp{t: now.t.Add(-LiveSpendWindow), valid: true}

	// A record whose own timestamp is unusable is KEPT, for the reason the
	// sandbox sweep keeps an undateable entry: it cannot be aged out on
	// time, and dropping it on that basis would be arbitrary. The count
	// cap above is what bounds those.
	aged := func(e spendEntry) bool { return e.at.valid && e.at.before(cutoff) }
	if !slices.ContainsFunc(s.spend, aged) {
		return
	}
	for _, e := range s.spend {
		if aged(e) {
			delete(s.spendIDs, e.EventID)
		}
	}
	s.spend = slices.DeleteFunc(s.spend, aged)
}

// forgetSpend drops the index entries of records leaving the window.
//
// The index is only ever as large as the records it tracks BECAUSE of this:
// an id left behind by a dropped record would make the map the one structure
// here that grows for the life of the process.
func (s *LiveState) forgetSpend(leaving []spendEntry) {
	for _, e := range leaving {
		delete(s.spendIDs, e.EventID)
	}
}

// spendEntry is one record with its timestamp already parsed.
//
// The parse is the point. tokens.Record is the WIRE shape — it carries the
// stamp as the string the dashboard renders — and this is the projection's
// own copy, so the parsed instant lives beside it rather than in it.
type spendEntry struct {
	tokens.Record
	at stamp
}

// SpendRecords returns the records inside the live window, and the EARLIEST
// instant they actually cover.
//
// THE SECOND RETURN IS WHAT KEEPS THE HEADING HONEST. The window is the real
// bound, but [SpendRecordLimit] binds first for a company emitting more than
// the cap in a day — and the pruning drops the OLDEST, so what is left covers
// less than the window the rollup is labelled with. A total over eighteen
// hours under a heading that says twenty-four is a wrong total; it is only
// invisibly wrong, which on a MONEY figure is the worse kind.
//
// ZERO WHERE NOTHING CAN HAVE BEEN DROPPED, which is the common case: the
// caller then keeps the window's own `since`, and nothing about an ordinary
// company's heading changes. A seed counts as a drop when the store says it
// left older records of the window behind, or when it was handed more than
// the cap — see [History.SpendTruncated]; one that merely filled the cap
// does not.
//
// AN INSTANT, NOT THE RECORD'S OWN STRING. Records arrive in whichever of the
// layouts [newStamp] accepts, and RFC 3339 with fractional seconds does not
// sort as text — "12:00:00.5Z" is lexically before "12:00:00Z" — so a minimum
// taken over the strings can name a later record than the earliest, and a
// caller re-parsing it as RFC 3339 would drop a zoneless layout on the floor
// and keep the full window's heading.
func (s *LiveState) SpendRecords() ([]tokens.Record, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]tokens.Record, len(s.spend))
	for i, e := range s.spend {
		out[i] = e.Record
	}
	if !s.spendCapped {
		return out, time.Time{}
	}
	// THE EARLIEST RETAINED, computed rather than remembered: hydration can
	// append behind a live record, so the slice is not reliably ordered —
	// the same reason `pruneSpend` sweeps rather than popping from the
	// front.
	var earliest time.Time
	for _, e := range s.spend {
		if !e.at.valid {
			continue
		}
		if earliest.IsZero() || e.at.t.Before(earliest) {
			earliest = e.at.t
		}
	}
	return out, earliest
}

// LiveSpendWindowDays is the live window expressed as the `since_days` a
// caller would ask for to get exactly it.
//
// NOT A LABEL any more — a rollup names its window with two instants — but
// still the comparison that decides whether a request can be answered from the
// projection in memory rather than by a scan of the event store.
//
// At LEAST one, because the window is measured in hours and a sub-day one
// would round to zero — and a fast path keyed on 0 would never be taken.
func LiveSpendWindowDays() int {
	days := int(LiveSpendWindow / (24 * time.Hour))
	if days < 1 {
		return 1
	}
	return days
}
