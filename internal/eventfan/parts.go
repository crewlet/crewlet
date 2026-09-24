package eventfan

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/store"
)

var logger = logging.Get("eventfan")

// ONE NODE'S PART OF EACH QUESTION, read from its own store.
//
// Each part carries exactly what the merge needs to be exact and nothing it
// does not: a page says whether it FILLED (so the merge knows where this
// node's rows stop being known), a trace and a turn say how many rows the node
// HOLDS (so the merged view can say it is cut), and a turn carries its newest
// rows beside its oldest (so a long turn keeps its ending).

// listPart is one node's page of a keyset listing, newest first.
type listPart struct {
	Rows []store.EventRecord `json:"rows"`

	// Full says the node holds more rows past the last one here — its page
	// filled, or its reply was cut to fit the transport. The merge cannot
	// place anything older than this node's last row, so it stops there.
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
	return listPart{Rows: rows, Full: len(rows) >= q.Limit}, nil
}

// eventPart is one node's copy of one event, or nothing.
type eventPart struct {
	Event *store.EventRecord `json:"event,omitempty"`
}

func eventPartOf(ctx context.Context, log *store.EventLog, id string) (eventPart, error) {
	rec, err := log.ByID(ctx, id)
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

func tracePartOf(ctx context.Context, log *store.EventLog, id string) (tracePart, error) {
	rows, err := log.Trace(ctx, id)
	if err != nil {
		return tracePart{}, err
	}
	part := tracePart{Rows: rows, Total: len(rows)}
	if len(rows) >= store.MaxTraceEvents {
		// ASKED, NOT INFERRED: a trace of exactly the cap holds every
		// row it has. DEGRADES, because the rows are in hand and a
		// missing caution badge beats a missing screen.
		total, err := log.TraceEventCount(ctx, id)
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

func turnPartOf(ctx context.Context, log *store.EventLog, id string) (turnPart, error) {
	head, err := log.Turn(ctx, id)
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
		total, countErr := log.TurnEventCount(ctx, id)
		switch {
		case countErr != nil:
			logger.WarnContext(ctx, "turn_extent_unavailable", "turn", id, "error", countErr)
		case total > len(head):
			part.Total = total
			closing, closingErr := log.TurnClosing(ctx, id, TurnClosingEvents)
			if closingErr != nil {
				logger.WarnContext(ctx, "turn_ending_unavailable", "turn", id, "error", closingErr)
			} else {
				part.Closing = closing
			}
		}
	}
	// EVERY TRACE THIS TURN TOUCHED, asked rather than derived from rows a
	// cap may have dropped. Degrades like the reads above.
	traces, tracesErr := log.TurnTraces(ctx, id)
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
	parts, err := log.TurnPartials(ctx, q)
	if err != nil {
		return turnsPart{}, err
	}
	return turnsPart{
		Turns: parts,
		Full:  len(q.IDs) == 0 && len(parts) >= turnPage(q.Limit),
	}, nil
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
