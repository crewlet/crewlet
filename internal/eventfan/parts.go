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

// readAt is the instant one node's part is read at: the asker's, or — for a
// question from a build that sent none — this node's clock, read ONCE for the
// whole part.
func readAt(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now().UTC()
	}
	return at
}

// THE ASKER HOLDS EVERY ROW THAT COMES BACK TO ITS OWN HORIZON.
//
// A node on this build floors every read at the asker's instant, but a node on
// an earlier build ignores the instant and answers as of its own clock — and
// its lookup of one event by id was not floored at all (see [Protocol]). So
// whatever a part carries with its instant is cut at the asker's horizon,
// `at` − [store.EventHistory], before any merge sees it, and a row past the
// horizon reaches no answer from any build: a dead link stays dead on a fleet
// half way through an upgrade. The asker owns `at`, so it is the one place the
// cut can be made whichever build replied.
//
// What a part holds only as a COUNT cannot be cut — see each part's `within`.

// floorable is a part that can be held to a horizon.
type floorable[T any] interface{ within(floor time.Time) T }

// heldTo holds this node's part and every peer's to the horizon under `at`.
// This node's own read is already floored there, so its part comes back as it
// went in.
func heldTo[T floorable[T]](g gathered[T], at time.Time) gathered[T] {
	floor := at.Add(-store.EventHistory)
	g.mine = g.mine.within(floor)
	peers := make([]peer[T], 0, len(g.peers))
	for _, p := range g.peers {
		peers = append(peers, peer[T]{node: p.node, part: p.part.within(floor)})
	}
	g.peers = peers
	return g
}

// below reports whether a row lies under the horizon.
func below(floor time.Time) func(store.EventRecord) bool {
	return func(r store.EventRecord) bool { return r.Time.Before(floor) }
}

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

// within drops the rows under the horizon. A page whose LAST row lies under it
// is no longer full: every row it did not send is older still, so nothing it
// holds inside the window is missing — and left full, the merge would stop at
// a row it has just dropped.
func (p listPart) within(floor time.Time) listPart {
	kept := slices.DeleteFunc(slices.Clone(p.Rows), below(floor))
	if len(kept) == len(p.Rows) {
		return p
	}
	full := p.Full && !p.Rows[len(p.Rows)-1].Time.Before(floor)
	return listPart{Rows: kept, Full: full}
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

// within drops a copy under the horizon — the one a build before the floor
// answers with, since its lookup by id read every copy it still held.
func (p eventPart) within(floor time.Time) eventPart {
	if p.Event != nil && p.Event.Time.Before(floor) {
		return eventPart{}
	}
	return p
}

func eventPartOf(ctx context.Context, log *store.EventLog, id string, at time.Time) (eventPart, error) {
	rec, err := log.ByID(ctx, id, readAt(at))
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

// tracePart is one node's rows of one trace, oldest first, and how many it
// holds.
type tracePart struct {
	Rows []store.EventRecord `json:"rows"`

	// Total is how many rows this node holds of the trace, whatever the
	// read returned — asked only when the read filled, since a read that
	// did not fill holds every row there is.
	Total int `json:"total"`
}

func (p tracePart) rows() int { return len(p.Rows) }

func (p tracePart) keep(n int) any {
	return tracePart{Rows: p.Rows[:n], Total: p.Total}
}

// unsent reports whether the node holds rows of the trace it did not send —
// its read stopped at the cap, or its reply was cut to fit the transport —
// and, when it does, the last row it sent, after which every one of them
// lies: nil when it sent none, which is what [MergeTrace] needs to place
// nothing past it.
//
// A part the horizon cut to NOTHING ([tracePart.within]) reports that it sent
// none, although it did, and that is exact rather than lost: the last row it
// sent lay under the horizon, beneath every row the merge still holds, so as a
// bound it would place nothing either.
func (p tracePart) unsent() (last *store.EventRecord, held bool) {
	if p.Total <= len(p.Rows) {
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

// within drops the rows under the horizon and takes them off the count. The
// rows are the node's OLDEST, so the ones under the horizon are among them and
// the corrected count is exact — unless the node's capped read held nothing
// but rows under it, when rows it did not send may lie there too and the count
// can only be an upper bound. That takes a trace with as many rows as the cap
// inside the strip between two clocks' horizons.
func (p tracePart) within(floor time.Time) tracePart {
	kept := slices.DeleteFunc(slices.Clone(p.Rows), below(floor))
	dropped := len(p.Rows) - len(kept)
	if dropped == 0 {
		return p
	}
	return tracePart{Rows: kept, Total: max(p.Total-dropped, len(kept))}
}

func tracePartOf(ctx context.Context, log *store.EventLog, id string, at time.Time) (tracePart, error) {
	at = readAt(at)
	rows, err := log.Trace(ctx, id, at)
	if err != nil {
		return tracePart{}, err
	}
	part := tracePart{Rows: rows, Total: len(rows)}
	if len(rows) >= store.MaxTraceEvents {
		// ASKED, NOT INFERRED: a trace of exactly the cap holds every
		// row it has. DEGRADES, because the rows are in hand and a
		// missing caution badge beats a missing screen.
		total, err := log.TraceEventCount(ctx, id, at)
		if err != nil {
			logger.WarnContext(ctx, "trace_extent_unavailable", "trace", id, "error", err)
		} else {
			part.Total = total
		}
	}
	return part, nil
}

// turnPart is one node's share of one turn: its oldest rows, its newest ones
// when the oldest did not reach them, how many it holds, and the traces it
// touched with when.
type turnPart struct {
	Head    []store.EventRecord `json:"head"`
	Closing []store.EventRecord `json:"closing,omitempty"`
	Total   int                 `json:"total"`
	Traces  []store.TurnTrace   `json:"traces"`
}

// unsent reports whether the node holds rows of the turn it sent in neither
// its opening nor its ending — its read stopped at the cap with a gap before
// the ending, or its reply was cut to fit the transport — and, when it does,
// the last row of the opening it sent, after which every one of them lies:
// nil when it sent no opening, for [tracePart.unsent]'s reason and with its
// exactness when the horizon cut the opening away.
func (p turnPart) unsent() (last *store.EventRecord, held bool) {
	if p.Total <= len(union(p.Head, p.Closing)) {
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

// within drops the rows under the horizon, from the opening and the ending
// alike, and corrects the count by what it dropped — exactly, when an ending
// row went (every row older than it is under the horizon too, so what is left
// is the ending), and when the opening's last row is above the horizon (so
// everything it did not send is too); otherwise the count is an upper bound,
// for the reason [tracePart.within] gives. The traces the node names are kept
// while any of its rows is: a trace's first instant says nothing about its
// last, so one that began under the horizon may still have rows above it.
//
// THE ROWS IT KEPT ARE COUNTED ONCE EACH. A turn of 501 to 519 rows sends an
// opening and an ending that OVERLAP ([store.EventLog.TurnClosing] says so),
// so the two lengths added count the shared rows twice — a turn held whole
// reported as cut, with a gap in its middle that nothing is missing from.
func (p turnPart) within(floor time.Time) turnPart {
	head := slices.DeleteFunc(slices.Clone(p.Head), below(floor))
	closing := slices.DeleteFunc(slices.Clone(p.Closing), below(floor))
	droppedHead, droppedClosing := len(p.Head)-len(head), len(p.Closing)-len(closing)
	if droppedHead == 0 && droppedClosing == 0 {
		return p
	}
	out := turnPart{Head: head, Closing: closing, Traces: p.Traces}
	switch {
	case droppedClosing > 0:
		out.Total = len(closing)
	default:
		out.Total = max(p.Total-droppedHead, len(union(head, closing)))
	}
	if len(head) == 0 && len(closing) == 0 {
		out.Traces = []store.TurnTrace{}
	}
	return out
}

// TurnClosingEvents is how many of a long turn's last rows are recovered
// beside its opening.
//
// Twenty rather than two, because the two records a reader came for are not
// reliably the last two. A turn ends with its final review phase, then
// `agent_turn_completed` and `turn_completed` — and then the REFLECTION PASS,
// which publishes after them: an episode, a persist decision, a counterparty
// profile, a synthesized, refined or promoted skill, and its own sentinel,
// each of them a model call that also files its own auxiliary
// `agent_phase_completed`. Two would be swallowed by that tail on any turn
// with learning enabled, and the review phase — the one a reader who came for
// "how did it end" wants beside the words — would go with them.
//
// Twenty clears that with headroom while staying small enough that the second
// read is a seek rather than a scan. The recovered rows replace nothing: a cut
// view holds [store.MaxTurnEvents] opening rows plus at most this many closing
// ones, which is the same payload budget the cap exists for with a bounded
// addition — fleet-wide as well as per node, because the merge cuts to the
// same two numbers.
const TurnClosingEvents = 20

func turnPartOf(ctx context.Context, log *store.EventLog, id string, at time.Time) (turnPart, error) {
	at = readAt(at)
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
	return part, nil
}

// turnsPart is one node's page of turn partials — or, on the second scatter,
// its share of exactly the turns named.
type turnsPart struct {
	Turns []store.TurnPartial `json:"turns"`
	Full  bool                `json:"full"`
}

func (p turnsPart) rows() int { return len(p.Turns) }

func (p turnsPart) keep(n int) any {
	return turnsPart{Turns: p.Turns[:n], Full: true}
}

func turnsPartOf(ctx context.Context, log *store.EventLog, q store.TurnQuery) (turnsPart, error) {
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
