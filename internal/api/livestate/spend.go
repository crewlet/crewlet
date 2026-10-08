package livestate

import (
	"slices"
	"sort"
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

// spendTypes are the two events whose payload is a spend record: a phase's own,
// and an auxiliary record's coalesced calls (types.AuxiliarySpend). Neither
// more nor fewer than the event store's writer folds, for the reason every
// field below matches its columns.
var spendTypes = map[string]bool{
	"agent_phase_completed": true,
	auxiliarySpendType:      true,
}

// auxiliarySpendType is the auxiliary record's wire type.
const auxiliarySpendType = "auxiliary_spend"

// foldSpend records one spend record, reporting whether the rollup moved.
//
// Deduped by event id so a redelivered envelope cannot inflate the rollup, and
// aged on the projection's CLOCK: every arrival first drops what the window
// has aged past, and a record already outside it is not counted at all.
//
// THE CLOCK, NEVER THE ARRIVING RECORD'S OWN STAMP. The window used to be cut
// at the arriving record's stamp minus a day, which made it a window only while
// records kept arriving and only while every node's clock agreed: a quiet
// company kept showing spend older than a day under a heading that said it was
// the last one, and one record stamped ahead by a node with a fast clock moved
// the cutoff forward, dropped every correctly stamped record a day behind it
// and forgot their ids — so a redelivery of one of them counted it again.
func (s *LiveState) foldSpend(env Envelope, payload map[string]any) bool {
	now := s.clock()
	moved := s.expireSpend(now)
	if env.ID != "" {
		if _, counted := s.spendIDs[env.ID]; counted {
			return moved
		}
	}
	// The stamp is PARSED ONCE, here, and carried with the record. The
	// window is kept in the order its records age out, and re-parsing a
	// held stamp — up to three layouts each — would happen inside the
	// projection's write lock, which is the mutex every /agents request and
	// every websocket snapshot waits on.
	at := newStamp(env.Timestamp)
	if at.valid && at.t.Before(now.Add(-LiveSpendWindow)) {
		// ALREADY AGED: the window it would have counted in has passed.
		// Not indexed either, so a redelivery is refused the same way
		// rather than held by an id no record in the window answers for.
		return moved
	}
	if env.ID != "" {
		s.spendIDs[env.ID] = struct{}{}
	}
	rec := tokens.Record{
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
		// Every value the store's columns carry (schema/0015, 0032,
		// 0040): the live window and a queried one fold through one
		// aggregation, and a value one producer carries and the other
		// drops is a rollup that changes when the window crosses the
		// live edge.
		CacheReadTokens:  num(payload, "cache_read_tokens"),
		CacheWriteTokens: num(payload, "cache_write_tokens"),
		ProviderKey:      str(payload, "provider_key"),
		CostUSD:          fraction(payload, "cost_usd"),
		Calls:            tokens.PhaseCalls(length(payload, "rounds"), num(payload, "rounds_used")),
	}
	if env.Type == auxiliarySpendType {
		// THE AUXILIARY MODEL'S SPEND, filed as the store's writer files
		// it: the auxiliary phase, its purpose as the worker, its stage,
		// the calls it coalesced — and a PERSON's as the person, under
		// their seat's role, since a person is no agent role.
		rec.Phase, rec.HostPhase, rec.Iteration = tokens.PhaseAuxiliary, "", 0
		rec.Worker = str(payload, "purpose")
		rec.Stage = str(payload, "stage")
		rec.Calls = max(num(payload, "calls"), 1)
		if rec.AgentID == "" {
			rec.Person = str(payload, "actor_seat")
		}
		if rec.AgentRole == "" {
			rec.AgentRole = str(payload, "actor_role")
		}
	}
	s.holdSpend(spendEntry{at: at, Record: rec})
	s.capSpend()
	return true
}

// length is the number of elements of a list field, zero for anything else.
func length(payload map[string]any, key string) int {
	list, _ := payload[key].([]any)
	return len(list)
}

// THE WINDOW IS HELD IN THE ORDER ITS RECORDS AGE OUT, which is what makes an
// arrival constant work.
//
// The dated records ([LiveState.spend]) are kept oldest stamp first, records
// sharing an instant in the order they arrived, so what the window has aged
// past is always a PREFIX, and the count cap's oldest records are the same
// prefix. An arrival in stamp order — nearly every one — is an append; one
// that lost a cross-topic race, or was stamped by a node whose clock runs
// behind, is inserted at its place, which costs a move of the records stamped
// after it and nothing else.
//
// The window used to be held in ARRIVAL order, and that order cannot be aged
// from the front: a broadcast subscription reads across topics with no order
// between them and a fleet's clocks disagree, so one recent record at the head
// would have stopped a front-popping loop for good. So every arrival scanned
// the whole window for an aged record, handing each 300-odd-byte entry to the
// predicate by value — under the projection's lock, on every spend record the
// company published: a scan of 24 000 records per arrival at the cap, and
// quadratic to fill. The seed sorted the window into stamp order all the
// while, so the two paths into it did not even agree on its order.
//
// A record whose stamp does not parse can be neither placed in that order nor
// aged on time, so it is held apart ([LiveState.undatedSpend]), in arrival
// order, and KEPT — for the reason the sandbox sweep keeps an undateable
// entry: dropping it on that basis would be arbitrary. The count cap is what
// bounds those, and it takes them first, as [stamp.chronological] orders them.

// holdSpend places one record in the window.
func (s *LiveState) holdSpend(e spendEntry) {
	if !e.at.valid {
		s.undatedSpend = append(s.undatedSpend, e)
		return
	}
	n := len(s.spend)
	if n == 0 || !e.at.t.Before(s.spend[n-1].at.t) {
		s.spend = append(s.spend, e)
		return
	}
	// AFTER every record sharing its instant, so equal stamps keep the order
	// they arrived in and the cap's oldest of them is the earlier arrival.
	at := sort.Search(n, func(i int) bool { return s.spend[i].at.t.After(e.at.t) })
	s.spend = slices.Insert(s.spend, at, e)
}

// expireSpend drops the records the window has aged past as of now, reporting
// whether any left. Its work is the records it drops: they are the front of the
// dated records, and it stops at the first that is still inside the window.
func (s *LiveState) expireSpend(now time.Time) bool {
	aged := s.agedSpend(now)
	s.spend = s.dropSpend(s.spend, aged)
	return aged > 0
}

// agedSpend is how many of the dated records the window has aged past as of
// now: the length of the front they make up.
func (s *LiveState) agedSpend(now time.Time) int {
	cutoff := now.Add(-LiveSpendWindow)
	aged := 0
	for aged < len(s.spend) && s.spend[aged].at.t.Before(cutoff) {
		aged++
	}
	return aged
}

// capSpend holds the window to [SpendRecordLimit], dropping the OLDEST: the
// undateable records first, then the earliest stamped.
//
// The count cap binds before the window for an org emitting more than the cap
// in a day. Truncating the oldest is what makes a rollup past the cap cover
// slightly less than a window rather than report a wrong total.
func (s *LiveState) capSpend() {
	over := len(s.undatedSpend) + len(s.spend) - SpendRecordLimit
	if over <= 0 {
		return
	}
	undated := min(over, len(s.undatedSpend))
	s.undatedSpend = s.dropSpend(s.undatedSpend, undated)
	s.spend = s.dropSpend(s.spend, over-undated)
}

// dropSpend removes a list's first n records, and their ids from the index.
//
// RESLICED FROM THE FRONT, never copied: once the cap binds this runs on EVERY
// arrival, under the projection's lock, and a fresh slice of the whole window
// per arrival was a 24 000-entry copy (about 10 MB) for each spend record the
// company published. The dropped entries are cleared so their strings are not
// held, and the backing array is replaced by append's own growth — which
// copies only the live records, once per quarter of the cap's worth of
// arrivals — so the work per arrival is constant and the memory at most a
// growth step past the window.
func (s *LiveState) dropSpend(list []spendEntry, n int) []spendEntry {
	if n == 0 {
		return list
	}
	s.forgetSpend(list[:n])
	clear(list[:n])
	return list[n:]
}

// forgetSpend drops the index entries of records leaving the window.
//
// The index is only ever as large as the records it tracks BECAUSE of this:
// an id left behind by a dropped record would make the map the one structure
// here that grows for the life of the process.
func (s *LiveState) forgetSpend(leaving []spendEntry) {
	for i := range leaving {
		delete(s.spendIDs, leaving[i].EventID)
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

// SpendWindow is the live window as one read: the records inside it, and the
// two instants that bound it, read off the clock the window was aged against.
//
// ONE INSTANT for the eviction and the label. A rollup prints its window
// beside its numbers, and a label taken from a second read of a second clock
// is a heading over records that nothing ever cut to it.
type SpendWindow struct {
	// Records are the records inside the window: the undateable ones first,
	// then the rest oldest first.
	Records []tokens.Record

	// Until is the projection's clock at the read, and Since is
	// [LiveSpendWindow] before it.
	Since, Until time.Time
}

// Spend reads the live window as of the projection's clock, leaving out what
// the window has aged past — so a company that has published nothing for a day
// reads an empty window, rather than the last day it was busy under the
// heading of this one.
//
// A READ, NEVER AN EXPIRY. What leaves the window is reported once, by
// whatever drops it ([LiveState.ExpireSpend], an arrival), and the stream
// re-pushes the rollup on that report. A read that dropped the records first
// would leave the report nothing to say: a tab connecting a moment after a
// record aged out would take the report with it, and every tab already open
// would keep the figure.
func (s *LiveState) Spend() SpendWindow {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	inside := s.spend[s.agedSpend(now):]
	out := make([]tokens.Record, 0, len(s.undatedSpend)+len(inside))
	for i := range s.undatedSpend {
		out = append(out, s.undatedSpend[i].Record)
	}
	for i := range inside {
		out = append(out, inside[i].Record)
	}
	return SpendWindow{Records: out, Since: now.Add(-LiveSpendWindow), Until: now}
}

// SpendRecords returns the records inside the live window: [LiveState.Spend]'s,
// for a reader that has no use for the window's bounds.
//
// KEPT ON PURPOSE with no production reader. The rollups label what they fold,
// so they read Spend; the suites that hold what the window CONTAINS — this
// package's, observe's seed and the store's cache columns — ask exactly this
// question, about two dozen times over, and have no use for the bounds.
func (s *LiveState) SpendRecords() []tokens.Record { return s.Spend().Records }

// ExpireSpend drops what the live window has aged past as of the projection's
// clock, reporting whether the rollup moved.
//
// For a caller that pushes the rollup only when it moves: a record ageing out
// publishes nothing, so without this a screen holding the last push kept a
// figure the window no longer holds until the next record arrived — on a quiet
// company, indefinitely. Its work is the records it drops.
func (s *LiveState) ExpireSpend() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expireSpend(s.clock())
}
