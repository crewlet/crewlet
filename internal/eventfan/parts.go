package eventfan

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

var logger = logging.Get("eventfan")

// ONE NODE'S PART OF EACH QUESTION, read from its own store.
//
// Each part carries exactly what the merge needs to be exact and nothing it
// does not: a page says whether it FILLED (so the merge knows where this
// node's rows stop being known), a trace and a turn say how many rows the node
// HOLDS (so the merged view can say it is cut), and a turn carries its newest
// rows beside its oldest (so a long turn keeps its ending).
//
// EVERY PART IS READ AT ONE INSTANT, the asker's: every read a part makes is
// floored at it, so a count asked beside the rows it counts is floored where
// they are — read at two instants, a row crossing the floor between the two
// made the count come back short of the rows beside it, and a long turn
// whose count fell to the cap was reported whole and lost its ending.

// listPart is one node's page of a keyset listing, newest first.
type listPart struct {
	Rows []store.EventRecord `json:"rows"`

	// Full says the node holds more rows past the last one here — its read
	// found a row past its page (asked, never guessed from a page that
	// filled: see the store's own reads), or its reply was cut to fit the
	// transport. The merge cannot place anything older than this node's last
	// row, so it stops there.
	Full bool `json:"full"`
}

func (p listPart) rows() int { return len(p.Rows) }

func (p listPart) keep(n int) any {
	return listPart{Rows: p.Rows[:n], Full: true}
}

func listPartOf(ctx context.Context, log *store.EventLog, q store.ListQuery) (listPart, error) {
	if q.Limit <= 0 {
		q.Limit = store.DefaultListLimit
	}
	rows, err := log.List(ctx, q)
	if err != nil {
		return listPart{}, err
	}
	// FULL WHEN THE PAGE FILLED — the one listing that still reads it that
	// way, and deliberately. Its page is not one read: a related-agent page
	// folds the traces' siblings into the direct matches and cuts the union
	// at the size, so a row past the DIRECT page says nothing about whether
	// the merged one ends. What the flag buys here is the merge's horizon,
	// for which a filled page is the safe answer, and never a cursor: the
	// event list's answer offers `next` on every non-empty page and ends at
	// an empty one (`exhausted`), so no reader takes this as the end of the
	// walk. The phase and turn listings, whose `more` IS their cursor, ask
	// the store one row past the page instead.
	return listPart{Rows: rows, Full: len(rows) >= q.Limit}, nil
}

// eventPart is one node's copy of one event, or nothing.
type eventPart struct {
	Event *store.EventRecord `json:"event,omitempty"`
}

func eventPartOf(ctx context.Context, log *store.EventLog, id string, at time.Time) (eventPart, error) {
	rec, err := log.ByID(ctx, id, at)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// NOT HELD HERE is an answer, and the ordinary one: an event
		// lives in exactly one node's store.
		return eventPart{}, nil
	case err != nil:
		return eventPart{}, err
	}
	return eventPart{Event: &rec}, nil
}

// tracePart is one node's rows of one trace, oldest first, how many of them it
// keeps, and the ones it holds unsettled.
type tracePart struct {
	Rows []store.EventRecord `json:"rows"`

	// Total is how many rows of the trace this node KEEPS, whatever the read
	// returned — the rows it holds, asked only when the read filled, since a
	// read that did not fill holds every row there is, less the Unsettled
	// ones.
	Total int `json:"total"`

	// Unsettled is the trace's rows this node holds of a custody batch it
	// has written and not settled, named rather than counted (see the
	// package doc).
	Unsettled []store.UnsettledRow `json:"unsettled,omitempty"`
}

// held is how many rows of the trace the node holds, kept or not.
func (p tracePart) held() int { return p.Total + len(p.Unsettled) }

func (p tracePart) rows() int { return len(p.Rows) }

func (p tracePart) keep(n int) any {
	return tracePart{Rows: p.Rows[:n], Total: p.Total, Unsettled: p.Unsettled}
}

// unsent reports whether the node holds rows of the trace it did not send —
// its read stopped at the cap, or its reply was cut to fit the transport —
// and, when it does, the last row it sent, after which every one of them
// lies: nil when it sent none, which is what [MergeTrace] needs to place
// nothing past it.
func (p tracePart) unsent() (last *store.EventRecord, held bool) {
	if p.held() <= len(p.Rows) {
		return nil, false
	}
	return lastOf(p.Rows), true
}

// lastOf is the last of some rows, or nil when there are none.
func lastOf(rows []store.EventRecord) *store.EventRecord {
	if len(rows) == 0 {
		return nil
	}
	last := rows[len(rows)-1]
	return &last
}

// tracePartOf reads one node's part of a trace from ONE SNAPSHOT of its log:
// the rows, how many it holds, and which of those are unsettled — read apart, a
// custody batch settled between two of them is counted by one and named by the
// other (see internal/store's unsettled.go).
func tracePartOf(ctx context.Context, log *store.EventLog, id string, at time.Time) (tracePart, error) {
	var part tracePart
	err := log.Snapshot(ctx, func(s store.EventSnapshot) error {
		rows, err := s.Trace(ctx, id, at)
		if err != nil {
			return err
		}
		part = tracePart{Rows: rows, Total: len(rows)}
		if len(rows) >= store.MaxTraceEvents {
			// ASKED, NOT INFERRED: a trace of exactly the cap holds every
			// row it has. DEGRADES, because the rows are in hand and a
			// missing caution badge beats a missing screen.
			total, countErr := s.TraceEventCount(ctx, id, at)
			if countErr != nil {
				logger.WarnContext(ctx, "trace_extent_unavailable", "trace", id, "error", countErr)
			} else {
				part.Total = total
			}
		}
		named, err := s.Unsettled(ctx, "trace_id", id, at)
		if err != nil {
			return err
		}
		if len(named) > 0 {
			part.Total = max(part.Total-len(named), 0)
			part.Unsettled = named
		}
		return nil
	})
	if err != nil {
		return tracePart{}, err
	}
	return part, nil
}

// turnPart is one node's share of one turn: its oldest rows, its newest ones
// when the oldest did not reach them, how many it keeps and the ones it holds
// unsettled ([tracePart]'s two fields, for its reason), and the traces it
// touched with when.
type turnPart struct {
	Head      []store.EventRecord  `json:"head"`
	Closing   []store.EventRecord  `json:"closing,omitempty"`
	Total     int                  `json:"total"`
	Traces    []store.TurnTrace    `json:"traces"`
	Unsettled []store.UnsettledRow `json:"unsettled,omitempty"`
}

// held is how many rows of the turn the node holds, kept or not.
func (p turnPart) held() int { return p.Total + len(p.Unsettled) }

// unsent reports whether the node holds rows of the turn it sent in neither
// its opening nor its ending — its read stopped at the cap with a gap before
// the ending, or its reply was cut to fit the transport — and, when it does,
// the last row of the opening it sent, after which every one of them lies:
// nil when it sent no opening, for [tracePart.unsent]'s reason.
func (p turnPart) unsent() (last *store.EventRecord, held bool) {
	if p.held() <= len(union(p.Head, p.Closing)) {
		return nil, false
	}
	return lastOf(p.Head), true
}

// rows counts the HEAD only: the closing rows are the turn's ending — where
// its outcome, its wall clock and its summary are — and are the last thing a
// cut gives up.
func (p turnPart) rows() int { return len(p.Head) }

func (p turnPart) keep(n int) any {
	out := p
	if len(out.Closing) == 0 && len(out.Head) > TurnClosingEvents {
		// A HEAD WITH NO CLOSING HOLDS THE WHOLE TURN, so its last rows
		// ARE the ending. Split them off before cutting, or the cut
		// would take the ending first — the one part of a long turn
		// nothing can stand in for.
		cut := len(out.Head) - TurnClosingEvents
		out.Closing = out.Head[cut:]
		out.Head = out.Head[:cut]
	}
	if n < len(out.Head) {
		out.Head = out.Head[:n]
	}
	return out
}

// TurnClosingEvents is how many of a long turn's last rows are recovered
// beside its opening.
//
// Thirty-two rather than two, because the records a reader came for are not
// reliably the last two. A turn's ending is its final review phase and
// everything after it, which on a turn with learning enabled is, counted in the
// order it is stamped:
//
//   - the review phase's own record: 1;
//   - its card's rewrite, an IN-turn `auxiliary_spend` stamped at that call —
//     after the review it condenses, before the completion: 1. Every other
//     in-turn record is stamped at its own last call, which is before the
//     review phase ended, so it sorts above this tail whenever it was flushed;
//   - the stop's own record, when a guard, the budget or the provider chain
//     ended the turn: 1;
//   - `agent_turn_completed` and `turn_completed`: 2;
//   - the audit record of a colleague's answer, when the turn was an ask: 1;
//   - the REFLECTION PASS, which publishes after them: an episode, a persist
//     decision, a counterparty profile per distinct sender, a synthesized,
//     refined or promoted skill and its sentinel — 7 with one sender;
//   - what that pass's model calls cost, as `auxiliary_spend` of the
//     reflection stage — one per purpose per model: the persist decider, the
//     profiler, the synthesizer and the refiner, 4 on one model;
//   - and the conversation entry's rewrites, beside the pass: one record per
//     kind of payload it condensed, an argument and a failed call's error, 2.
//
// Nineteen, then, before a coalesced trigger's second sender or a fallback
// chain that answered the pass on a second model adds a row each. Twenty —
// this constant's old value, set when the ending was counted at fifteen —
// cleared that by one, and a busier ending pushed the review phase out of
// the recovered rows: the one a reader who came for "how did it end" wants
// beside the words. Thirty-two keeps thirteen rows of headroom for exactly
// those, while staying small enough that the second read is a seek rather
// than a scan. The recovered rows replace nothing: a cut view holds
// [store.MaxTurnEvents] opening rows plus at most this many closing ones,
// which is the same payload budget the cap exists for with a bounded addition
// — fleet-wide as well as per node, because the merge cuts to the same two
// numbers.
const TurnClosingEvents = 32

// turnPartOf reads one node's share of a turn from ONE SNAPSHOT of its log, for
// [tracePartOf]'s reason.
func turnPartOf(ctx context.Context, log *store.EventLog, id string, at time.Time) (turnPart, error) {
	var part turnPart
	err := log.Snapshot(ctx, func(s store.EventSnapshot) error {
		var err error
		part, err = readTurnPart(ctx, s, id, at)
		return err
	})
	if err != nil {
		return turnPart{}, err
	}
	return part, nil
}

// readTurnPart is [turnPartOf] inside its snapshot.
func readTurnPart(ctx context.Context, log store.EventSnapshot, id string, at time.Time) (turnPart, error) {
	head, err := log.Turn(ctx, id, at)
	if err != nil {
		return turnPart{}, err
	}
	part := turnPart{Head: head, Total: len(head), Traces: []store.TurnTrace{}}
	// THE READ STOPPED AT THE CAP, not at the end of the turn: the read is
	// oldest first, so what a long turn loses is its ENDING. So a cut view
	// gets its ending back and reports the gap in the MIDDLE.
	//
	// ASKED, NOT INFERRED — a turn of exactly the cap holds every row it
	// has — and only on a read that filled. Both follow-ups DEGRADE: the
	// rows are in hand, and failing them over a count turns the largest
	// turns into `query_failed`.
	if len(head) >= store.MaxTurnEvents {
		total, countErr := log.TurnEventCount(ctx, id, at)
		switch {
		case countErr != nil:
			logger.WarnContext(ctx, "turn_extent_unavailable", "turn", id, "error", countErr)
		case total > len(head):
			part.Total = total
			closing, closingErr := log.TurnClosing(ctx, id, TurnClosingEvents, at)
			if closingErr != nil {
				logger.WarnContext(ctx, "turn_ending_unavailable", "turn", id, "error", closingErr)
			} else {
				part.Closing = closing
			}
		}
	}
	// EVERY TRACE THIS TURN TOUCHED, asked rather than derived from rows a
	// cap may have dropped. Degrades like the reads above.
	traces, tracesErr := log.TurnTraces(ctx, id, at)
	if tracesErr != nil {
		logger.WarnContext(ctx, "turn_traces_unavailable", "turn", id, "error", tracesErr)
	} else {
		part.Traces = traces
	}
	named, err := log.Unsettled(ctx, "turn_id", id, at)
	if err != nil {
		return turnPart{}, err
	}
	if len(named) > 0 {
		part.Total = max(part.Total-len(named), 0)
		part.Unsettled = named
	}
	return part, nil
}

// turnsPart is one node's page of turn partials — or, on the second scatter,
// its share of exactly the turns named, and which of them its page lists.
type turnsPart struct {
	Turns []store.TurnPartial `json:"turns"`
	Full  bool                `json:"full"`

	// Listed is, on the second scatter, which of the named turns this node's
	// PAGE selects — its share starts in the window and passes the page's
	// turn-level filters ([store.EventLog.ListedTurns]) — so the asker can
	// page each turn by a start some node's listing reaches ([Fleet.Turns]).
	Listed []string `json:"listed,omitempty"`

	// Unsettled is, on the second scatter, the named turns' rows this node
	// holds of a custody batch it has written and not settled — which the
	// shares' sums leave out, and name instead (see the package doc).
	Unsettled []store.UnsettledRow `json:"unsettled,omitempty"`
}

func (p turnsPart) rows() int { return len(p.Turns) }

// keep cuts the turns and keeps the judgement: which named turns the page
// lists is the same answer whichever shares fit the transport. The rows named
// for a turn that was cut go with it: the merge has no share of that turn from
// this node to add them to.
func (p turnsPart) keep(n int) any {
	kept := map[string]bool{}
	for _, t := range p.Turns[:n] {
		kept[t.TurnID] = true
	}
	names := slices.DeleteFunc(slices.Clone(p.Unsettled), func(r store.UnsettledRow) bool {
		return !kept[r.TurnID]
	})
	if len(names) == 0 {
		names = nil
	}
	return turnsPart{Turns: p.Turns[:n], Full: true, Listed: p.Listed, Unsettled: names}
}

func turnsPartOf(ctx context.Context, log *store.EventLog, q store.TurnQuery) (turnsPart, error) {
	if len(q.IDs) > 0 {
		// THE SECOND SCATTER: this node's share of the named turns, which
		// of them its page lists, and the rows of them it holds unsettled,
		// from one snapshot ([store.EventLog.TurnShares]).
		shares, err := log.TurnShares(ctx, q)
		if err != nil {
			return turnsPart{}, err
		}
		part := turnsPart{Turns: shares.Partials, Listed: shares.Listed}
		if len(shares.Unsettled) > 0 {
			part.Unsettled = shares.Unsettled
		}
		return part, nil
	}
	// FULL ONLY WHERE THE LOG SAID SO. A page that merely filled was read as
	// one with more behind it, so a seat whose history is a multiple of the
	// page got a cursor onto an empty page — the store asks one row past the
	// page and says (see [store.EventLog.TurnPartials]).
	parts, more, err := log.TurnPartials(ctx, q)
	if err != nil {
		return turnsPart{}, err
	}
	return turnsPart{Turns: parts, Full: more}, nil
}

// turnPage is the page [store.EventLog.TurnPartials] actually cuts.
func turnPage(limit int) int {
	switch {
	case limit <= 0:
		return store.DefaultTurnPage
	case limit > store.MaxTurnPage:
		return store.MaxTurnPage
	}
	return limit
}

// idsOf is the turn ids of some partials, in order, once each.
func idsOf(parts ...[]store.TurnPartial) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, group := range parts {
		for _, p := range group {
			if _, dup := seen[p.TurnID]; dup {
				continue
			}
			seen[p.TurnID] = struct{}{}
			out = append(out, p.TurnID)
		}
	}
	return out
}

// spendPart is one node's per-phase spend records of a window, newest first.
type spendPart struct {
	Records []tokens.Record `json:"records"`

	// Full says the node holds more records past the last one here: its
	// read stopped at the limit, or its reply was cut to fit the
	// transport. See [MergeSpend].
	Full bool `json:"full"`
}

func (p spendPart) rows() int { return len(p.Records) }

func (p spendPart) keep(n int) any {
	return spendPart{Records: p.Records[:n], Full: true}
}

func spendPartOf(ctx context.Context, log *store.EventLog, q store.PhaseTokenQuery) (spendPart, error) {
	records, err := log.PhaseTokens(ctx, q)
	if err != nil {
		return spendPart{}, err
	}
	return spendPart{Records: records, Full: q.Limit > 0 && len(records) >= q.Limit}, nil
}
