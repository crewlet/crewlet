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
		if s.countedPhases.has(env.ID) {
			return false
		}
		s.countedPhases.put(env.ID, struct{}{})
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
		s.spend = append(make([]spendEntry, 0, spendRecordLimit),
			s.spend[len(s.spend)-spendRecordLimit:]...)
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
	s.spend = slices.DeleteFunc(s.spend, aged)
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

// SpendRecords returns the records inside the live window.
func (s *LiveState) SpendRecords() []tokens.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]tokens.Record, len(s.spend))
	for i, e := range s.spend {
		out[i] = e.Record
	}
	return out
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

// HydrateSpend seeds the window from records the store already holds, and
// reports how many landed.
//
// WITHOUT IT A RESTART IS A DAY OF ZEROES. The projection is fed by one
// EPHEMERAL subscription, so it starts empty and fills only as new phases
// complete — while the event store beside it holds the whole window. Every
// screen that reads this rollup then said the company had spent nothing, in a
// window it labelled a full day, next to a chart drawn from the store that
// showed the real spend. Two facts on one screen that cannot both be true.
//
// DEDUPED BY EVENT ID against what is already here, which is what makes the
// ORDER of hydration and subscription a non-question: the API subscribes
// before it hydrates, so a live event lands ahead of the older records this
// appends behind it, and either one arriving twice is counted once. It is also
// why [LiveState.pruneSpend] is order-independent rather than popping the head.
//
// A record with no id is DROPPED rather than counted: the dedupe has nothing
// to hold it by, so a second hydration — a reconnect, a second call — would
// add it again, and a rollup that grows on every reload is worse than one that
// is slightly short.
func (s *LiveState) HydrateSpend(records []tokens.Record) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	landed := 0
	for _, rec := range records {
		if rec.EventID == "" || s.countedPhases.has(rec.EventID) {
			continue
		}
		s.countedPhases.put(rec.EventID, struct{}{})
		s.spend = append(s.spend, spendEntry{at: newStamp(rec.Timestamp), Record: rec})
		landed++
	}
	if landed == 0 {
		return 0
	}
	// PRUNED AGAINST THE NEWEST RECORD HELD, not against the newest in
	// this batch: a hydration that seeded only records older than the
	// window would otherwise anchor the window on its own oldest row and
	// keep every one of them, widening what the rollup claims to cover.
	//
	// COMPARED AS INSTANTS, never as the strings they arrived as. Two
	// encodings of one instant reach this projection — the store truncates
	// to microseconds while a live row keeps nanoseconds, and Go's
	// RFC3339Nano trims trailing zeros — so `…:07Z` sorts before `…:07.42Z`
	// on 'Z' against '.', which puts the LATER instant first and would
	// anchor the prune early.
	var newest stamp
	for _, e := range s.spend {
		if e.at.valid && (!newest.valid || newest.before(e.at)) {
			newest = e.at
		}
	}
	if newest.valid {
		s.pruneSpend(newest.raw)
	}
	return landed
}
