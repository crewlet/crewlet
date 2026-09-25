package eventfan

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// FleetReadBudget is how long a history read waits for the other nodes.
//
// TWO SECONDS, and it is a judgement rather than a measurement: no fleet with
// thirty days of history has been measured answering these. What bounds it
// from above is the reader — a dashboard read that takes longer than this is
// one a person has already given up on, and the API's own unavailable-retry is
// five seconds, which a read has to finish well inside or the retry and the
// read overlap. What bounds it from below is the work: each node's part is one
// indexed read of its own store, milliseconds on an idle node, so a node that
// has not answered in two seconds is one worth naming rather than waiting for.
// It is what a caller was promised, so the `history_partial` alarm borrows it
// rather than holding a second opinion (ADR-0015): a node that did not answer
// inside it is what makes an answer partial.
const FleetReadBudget = 2 * time.Second

// Fleet answers history questions for the whole fleet.
//
// THE LOCAL READ IS NOT A PARTICIPANT LIKE THE OTHERS. This node's own store is
// read directly, never through the broker, and in parallel with the scatter —
// so a fleet read costs the slower of the two rather than their sum, and a
// broker that hiccupped costs the peers' rows rather than the whole answer.
type Fleet struct {
	// Self is this node's id, its name in every coverage.
	Self string

	// Local is this node's own event store. Required.
	Local *store.EventLog

	// Queue reaches the rest of the fleet. NIL IS A LEGAL DEPLOYMENT — a
	// single node, an embedded engine, a test — and it means this node's
	// store is the fleet's.
	Queue queue.EventQueue

	// Roster answers which nodes are live, this one included. Nil means
	// this node alone. An error still scatters — whoever answers is merged
	// — but the answer cannot be called complete, because nobody can say
	// who was missing.
	Roster func(ctx context.Context) ([]string, error)

	// Budget bounds the wait for the peers. Zero takes [FleetReadBudget].
	Budget time.Duration

	// Report is told what every answer covered and what it took — the one
	// input the `history_partial` alarm has. A hook rather than a metrics
	// dependency, so this package does not import the recorder to say one
	// sentence. Nil counts nothing.
	Report func(Question, Coverage, time.Duration)
}

// Solo is a fleet of this node alone: its own store, nobody to ask.
//
// What a single node, an embedded engine and a test read history through — the
// answer is exactly the store's, with a coverage that says so.
func Solo(self string, local *store.EventLog) *Fleet {
	return &Fleet{Self: self, Local: local}
}

// Listing is a keyset page from the fleet, newest first.
type Listing struct {
	Rows []store.EventRecord

	// More says rows exist past this page. See [MergeListing].
	More bool
}

// Trace is every row of one trace the fleet holds, up to the cap.
type Trace struct {
	Rows []store.EventRecord

	// Total is how many rows the fleet holds; greater than len(Rows) means
	// the view is cut.
	Total int
}

// TurnDetail is one turn from every node that ran part of it.
type TurnDetail struct {
	Rows   []store.EventRecord
	Total  int
	Traces []string
}

// TurnPage is a page of turns from the fleet.
type TurnPage struct {
	Turns []store.Turn

	// Next is the start to resume from, or nil at the end — and always nil
	// for a page ranked by tokens, which is a ranking rather than a walk.
	Next *time.Time
}

// ---- the scatter ------------------------------------------------------- //

// peer is one peer's decoded answer.
type peer[T any] struct {
	node string
	part T
}

// gathered is one scatter's outcome.
type gathered[T any] struct {
	mine     T
	peers    []peer[T]
	coverage Coverage
	// fanned says anybody else was asked, so a caller knows whether a
	// second scatter could add anything.
	fanned bool
}

// gather asks every live node one question and reads this node's own answer
// beside it.
func gather[T any](ctx context.Context, f *Fleet, q Question, params any, ids []string,
	local func(context.Context) (T, error),
) (gathered[T], error) {
	var zero gathered[T]
	roster, rosterErr := f.roster(ctx)
	var peers []string
	for _, node := range roster {
		if node != f.Self {
			peers = append(peers, node)
		}
	}
	fan := f.Queue != nil && (rosterErr != nil || len(peers) > 0)

	type scattered struct {
		replies [][]byte
		err     error
	}
	var replies chan scattered
	budget := cmp.Or(f.Budget, FleetReadBudget)
	if fan {
		body, err := json.Marshal(params)
		if err != nil {
			return zero, fmt.Errorf("eventfan: encode the %s parameters: %w", q, err)
		}
		req, err := json.Marshal(request{
			Version: Protocol, Asker: f.Self, Question: q, Params: body, TurnIDs: ids,
		})
		if err != nil {
			return zero, fmt.Errorf("eventfan: encode a %s request: %w", q, err)
		}
		// WANT EVERY PEER WHEN THE ROSTER IS KNOWN, so the read returns
		// the moment the last one answers rather than at the budget. An
		// unknown roster waits the budget out: nobody can say how many
		// answers are coming.
		want := len(peers)
		if rosterErr != nil {
			want = 0
		}
		replies = make(chan scattered, 1)
		go func() {
			deadline, done := context.WithTimeout(ctx, budget)
			defer done()
			// THIS ERR IS THE GOROUTINE'S OWN — the local read below
			// writes the outer one while this is in flight.
			//nolint:govet // shadow: deliberate; see the line above.
			got, err := f.Queue.Ask(deadline, Subject, req, want)
			replies <- scattered{got, err}
		}()
	}

	mine, err := local(ctx)
	if err != nil {
		// THE LOCAL READ IS THE ONE FAILURE THAT IS AN ERROR. Every
		// other node's silence is a gap the coverage names; this one is
		// the store under the caller's own feet. The scatter is left to
		// its deadline: its channel is buffered, so it ends on its own.
		return zero, err
	}
	out := gathered[T]{mine: mine, fanned: fan}
	nodes := map[string]NodeCoverage{f.Self: {ID: f.Self, Answered: true}}
	for _, node := range peers {
		nodes[node] = NodeCoverage{ID: node, Error: fmt.Sprintf(
			"no answer within the %s fleet read budget", budget)}
	}
	if replies != nil {
		got := <-replies
		if got.err != nil {
			for _, node := range peers {
				nodes[node] = NodeCoverage{ID: node, Error: "the fleet could not be " +
					"asked: " + got.err.Error()}
			}
		}
		for _, raw := range got.replies {
			node, part, why := decodeReply[T](raw)
			if node == "" || node == f.Self {
				// AN UNREADABLE REPLY IS A MISSING ONE, and one
				// that names nobody cannot even say whose it was:
				// its sender stays counted as not answering.
				continue
			}
			if why != "" {
				nodes[node] = NodeCoverage{ID: node, Error: why}
				continue
			}
			nodes[node] = NodeCoverage{ID: node, Answered: true}
			out.peers = append(out.peers, peer[T]{node: node, part: part})
		}
	}
	out.coverage.Complete = rosterErr == nil
	for _, n := range nodes {
		out.coverage.Nodes = append(out.coverage.Nodes, n)
		if !n.Answered {
			out.coverage.Complete = false
		}
	}
	slices.SortFunc(out.coverage.Nodes, func(a, b NodeCoverage) int { return cmp.Compare(a.ID, b.ID) })
	// Peers in node order, so a merge that keeps first-seen order is the
	// same merge on every read.
	slices.SortFunc(out.peers, func(a, b peer[T]) int { return cmp.Compare(a.node, b.node) })
	if rosterErr != nil {
		logger.WarnContext(ctx, "history_roster_unreadable", "question", q,
			"error", rosterErr, "detail", "the answer merges whoever replied and "+
				"is reported incomplete, because nobody can say who was missing")
	}
	return out, nil
}

// decodeReply reads one peer's reply: whose it is, its part, and — when it is
// not an answer — why.
func decodeReply[T any](raw []byte) (node string, part T, why string) {
	var r reply
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", part, ""
	}
	switch {
	case r.Version != Protocol:
		return r.Node, part, fmt.Sprintf("answered in history protocol v%d, and "+
			"this node speaks v%d; it is running a different build", r.Version, Protocol)
	case r.Error != "":
		return r.Node, part, r.Error
	}
	if err := json.Unmarshal(r.Answer, &part); err != nil {
		return r.Node, part, "its answer could not be read: " + err.Error()
	}
	return r.Node, part, ""
}

// roster is who should answer, this node always included.
func (f *Fleet) roster(ctx context.Context) ([]string, error) {
	if f.Roster == nil {
		return []string{f.Self}, nil
	}
	nodes, err := f.Roster(ctx)
	if err != nil {
		return []string{f.Self}, err
	}
	if !slices.Contains(nodes, f.Self) {
		// THIS NODE IS ALWAYS PART OF THE ANSWER, whatever presence
		// says: a roster that has not yet seen this node's own lease
		// must not turn its own store into a gap.
		nodes = append(slices.Clone(nodes), f.Self)
	}
	slices.Sort(nodes)
	return slices.Compact(nodes), nil
}

func (f *Fleet) report(q Question, c Coverage, started time.Time) {
	if f.Report != nil {
		f.Report(q, c, time.Since(started))
	}
}

// parts is the asker's part followed by every peer's, in node order.
func (g gathered[T]) parts() []T {
	out := make([]T, 0, 1+len(g.peers))
	out = append(out, g.mine)
	for _, p := range g.peers {
		out = append(out, p.part)
	}
	return out
}

// ---- the questions ----------------------------------------------------- //

// List answers a page of the log.
//
// A RELATED-AGENT page is two scatters, for the reason the store's own read is
// two queries: an agent's work is CAUSED by something that names the agent
// nowhere — an inbound webhook, a schedule tick — and across a fleet the cause
// routinely sits in a different node's store from the work, since a delivery
// lands on whichever node the load balancer picked. So once the page is
// merged, every node is asked for the rows of the traces it holds.
func (f *Fleet) List(ctx context.Context, q store.ListQuery) (Listing, Coverage, error) {
	started := time.Now()
	if q.Limit <= 0 {
		q.Limit = store.DefaultListLimit
	}
	g, err := gather(ctx, f, QuestionEvents, listParamsOf(q), nil,
		func(ctx context.Context) (listPart, error) { return listPartOf(ctx, f.Local, q) })
	if err != nil {
		return Listing{}, Coverage{}, err
	}
	rows, more := MergeListing(g.parts(), q.Limit)
	coverage := g.coverage
	if q.RelatedAgent != "" && g.fanned {
		traces := store.TraceIDsOf(rows)
		if len(traces) > 0 {
			p := traceRowsParams{TraceIDs: traces, Limit: q.Limit}
			sib, err := gather(ctx, f, QuestionTraceRows, p, nil,
				func(ctx context.Context) (listPart, error) {
					sibs, err := f.Local.TraceRows(ctx, traces, q.Limit)
					return listPart{Rows: sibs}, err
				})
			if err != nil {
				return Listing{}, Coverage{}, err
			}
			var siblings [][]store.EventRecord
			for _, part := range sib.parts() {
				siblings = append(siblings, part.Rows)
			}
			rows = MergeRelated(rows, union(siblings...), q.Limit, more)
			coverage = coverage.And(sib.coverage)
		}
	}
	f.report(QuestionEvents, coverage, started)
	return Listing{Rows: rows, More: more}, coverage, nil
}

// Histogram answers the log's time axis over the fleet.
//
// THE WINDOW IS PINNED to this node's clock before anybody is asked, so every
// node cuts the same bars — see [store.HistogramQuery.At].
func (f *Fleet) Histogram(ctx context.Context, q store.HistogramQuery) (store.EventHistogram, Coverage, error) {
	started := time.Now()
	if q.At.IsZero() {
		q.At = time.Now().UTC()
	}
	p := seriesParams{List: listParamsOf(q.ListQuery), Bucket: q.Bucket, At: q.At}
	g, err := gather(ctx, f, QuestionSeries, p, nil,
		func(ctx context.Context) (store.EventHistogram, error) { return f.Local.Histogram(ctx, q) })
	if err != nil {
		return store.EventHistogram{}, Coverage{}, err
	}
	others := make([]store.EventHistogram, 0, len(g.peers))
	for _, p := range g.peers {
		others = append(others, p.part)
	}
	merged, refused := MergeSeries(g.mine, others)
	coverage := g.coverage
	for _, i := range refused {
		coverage = coverage.And(Coverage{Complete: false, Nodes: []NodeCoverage{{
			ID: g.peers[i].node, Error: "it answered a different window, so its bars " +
				"cannot be summed with this node's; it is running a different build",
		}}})
	}
	f.report(QuestionSeries, coverage, started)
	return merged, coverage, nil
}

// ByID answers one event, from whichever node holds it.
//
// NOT FOUND ANYWHERE is [store.ErrNotFound], and says which nodes could not be
// asked: a dead link is the ordinary case, and one whose node was merely
// silent is a different fact.
func (f *Fleet) ByID(ctx context.Context, id string) (store.EventRecord, Coverage, error) {
	started := time.Now()
	g, err := gather(ctx, f, QuestionEvent, idParams{ID: id}, nil,
		func(ctx context.Context) (eventPart, error) { return eventPartOf(ctx, f.Local, id) })
	if err != nil {
		return store.EventRecord{}, Coverage{}, err
	}
	f.report(QuestionEvent, g.coverage, started)
	rec, found := FirstFound(g.parts())
	if !found {
		if missing := g.coverage.Missing(); len(missing) > 0 {
			return store.EventRecord{}, g.coverage, fmt.Errorf("%w: event %s is held by "+
				"no node that answered, and %v did not answer", store.ErrNotFound, id, missing)
		}
		return store.EventRecord{}, g.coverage, fmt.Errorf("%w: event %s", store.ErrNotFound, id)
	}
	return rec, g.coverage, nil
}

// Trace answers every row sharing one trace, oldest first.
func (f *Fleet) Trace(ctx context.Context, id string) (Trace, Coverage, error) {
	started := time.Now()
	g, err := gather(ctx, f, QuestionTrace, idParams{ID: id}, nil,
		func(ctx context.Context) (tracePart, error) { return tracePartOf(ctx, f.Local, id) })
	if err != nil {
		return Trace{}, Coverage{}, err
	}
	rows, total := MergeTrace(g.parts())
	f.report(QuestionTrace, g.coverage, started)
	return Trace{Rows: rows, Total: total}, g.coverage, nil
}

// Turn answers every event of one turn, from every node that ran part of it.
func (f *Fleet) Turn(ctx context.Context, id string) (TurnDetail, Coverage, error) {
	started := time.Now()
	g, err := gather(ctx, f, QuestionTurn, idParams{ID: id}, nil,
		func(ctx context.Context) (turnPart, error) { return turnPartOf(ctx, f.Local, id) })
	if err != nil {
		return TurnDetail{}, Coverage{}, err
	}
	rows, total, traces := MergeTurn(g.parts())
	f.report(QuestionTurn, g.coverage, started)
	return TurnDetail{Rows: rows, Total: total, Traces: traces}, g.coverage, nil
}

// Phases answers the company's phase records, newest first, payload included.
func (f *Fleet) Phases(ctx context.Context, role string, limit int, before *store.Cursor) (Listing, Coverage, error) {
	started := time.Now()
	limit = phaseLimit(limit)
	p := phasesParams{Role: role, Limit: limit, Before: cursorOf(before)}
	g, err := gather(ctx, f, QuestionPhases, p, nil,
		func(ctx context.Context) (listPart, error) {
			rows, err := f.Local.Phases(ctx, role, limit, before)
			return listPart{Rows: rows, Full: len(rows) >= limit}, err
		})
	if err != nil {
		return Listing{}, Coverage{}, err
	}
	rows, more := MergeListing(g.parts(), limit)
	f.report(QuestionPhases, g.coverage, started)
	return Listing{Rows: rows, More: more}, g.coverage, nil
}

// SeatPhases answers one seat's phase records, newest first, payload included.
//
// A SEAT MOVES — placement hands it to another node when its owner leaves or
// the fleet rebalances — so its history is on every node that ever held it,
// and the seat page read from one node showed only the turns since the last
// move.
func (f *Fleet) SeatPhases(ctx context.Context, agentID, role string, before *store.Cursor) (Listing, Coverage, error) {
	started := time.Now()
	if agentID == "" && role == "" {
		// A SEAT NOBODY CAN NAME is a question with no answer, not one
		// with an empty one — the store says so the same way.
		return Listing{}, solo(f.Self), nil
	}
	p := phasesParams{AgentID: agentID, Role: role, Before: cursorOf(before)}
	g, err := gather(ctx, f, QuestionSeatPhases, p, nil,
		func(ctx context.Context) (listPart, error) {
			rows, err := f.Local.AgentPhases(ctx, agentID, role, before)
			return listPart{Rows: rows, Full: len(rows) >= store.AgentPhaseLimit}, err
		})
	if err != nil {
		return Listing{}, Coverage{}, err
	}
	rows, more := MergeListing(g.parts(), store.AgentPhaseLimit)
	f.report(QuestionSeatPhases, g.coverage, started)
	return Listing{Rows: rows, More: more}, g.coverage, nil
}

// Turns answers a page of turns, one row per turn however many nodes it ran
// on.
//
// TWO SCATTERS, because the node a turn is SELECTED on is not always the only
// node that holds it. The first asks every node for its page — each node's
// share of the turns it would list — and the second asks every node for its
// share of exactly the turns any of them listed, so a turn resumed on another
// node after a restart is folded from both halves rather than listed as two
// half-turns or as whichever half was nearer.
//
// A page by START is exact down to the newest point any full page stopped at,
// for [MergeListing]'s reason, and the cursor resumes from there. A page by
// TOKENS ranks each node's top candidates by their MERGED totals — which is
// exact for every turn that ran on one node, and can miss a turn split across
// nodes whose every half fell below every node's cut. That is stated rather
// than hidden: the coverage is honest about nodes, and a ranking has no cursor
// to be honest about.
func (f *Fleet) Turns(ctx context.Context, q store.TurnQuery) (TurnPage, Coverage, error) {
	started := time.Now()
	if !q.Sort.Valid() {
		return TurnPage{}, Coverage{}, fmt.Errorf("%w: sort %q is not one of %v",
			store.ErrTurnSort, q.Sort, store.TurnSorts)
	}
	byTokens := q.Sort == store.TurnSortTokens
	if byTokens && !q.Before.IsZero() {
		return TurnPage{}, Coverage{}, ErrRankedCursor
	}
	q.Limit = turnPage(q.Limit)
	q.IDs = nil
	first, err := gather(ctx, f, QuestionTurns, turnsParamsOf(q), nil,
		func(ctx context.Context) (turnsPart, error) { return turnsPartOf(ctx, f.Local, q) })
	if err != nil {
		return TurnPage{}, Coverage{}, err
	}
	var partials []store.TurnPartial
	coverage := first.coverage
	more := false
	var horizon *time.Time
	for _, part := range first.parts() {
		if !part.Full {
			continue
		}
		more = true
		if n := len(part.Turns); n > 0 && !byTokens {
			last := part.Turns[n-1].StartedAt
			if horizon == nil || last.After(*horizon) {
				horizon = &last
			}
		}
	}
	if !first.fanned {
		// THIS NODE IS THE FLEET: its page is whole as it stands.
		partials = first.mine.Turns
	} else {
		lists := make([][]store.TurnPartial, 0, 1+len(first.peers))
		for _, part := range first.parts() {
			lists = append(lists, part.Turns)
		}
		ids := idsOf(lists...)
		if len(ids) > 0 {
			share := q
			share.IDs = ids
			second, err := gather(ctx, f, QuestionTurns, turnsParamsOf(q), ids,
				func(ctx context.Context) (turnsPart, error) {
					return turnsPartOf(ctx, f.Local, share)
				})
			if err != nil {
				return TurnPage{}, Coverage{}, err
			}
			shares := make([][]store.TurnPartial, 0, 1+len(second.peers))
			for _, part := range second.parts() {
				shares = append(shares, part.Turns)
			}
			partials = MergeTurnPartials(shares...)
			coverage = coverage.And(second.coverage)
		}
		// THE TURN-LEVEL FILTERS AGAIN, over the WHOLE turn: a node lists
		// a turn as clean when its own half is, and the other half may
		// have failed.
		partials = slices.DeleteFunc(partials, func(p store.TurnPartial) bool {
			if q.Failed != nil && p.Failed != *q.Failed {
				return true
			}
			return !byTokens && !q.Before.IsZero() && !p.StartedAt.Before(q.Before)
		})
		if byTokens {
			slices.SortStableFunc(partials, func(a, b store.TurnPartial) int {
				return cmp.Or(cmp.Compare(b.TotalTokens, a.TotalTokens),
					b.StartedAt.Compare(a.StartedAt), cmp.Compare(b.TurnID, a.TurnID))
			})
		} else {
			slices.SortStableFunc(partials, func(a, b store.TurnPartial) int {
				return cmp.Or(b.StartedAt.Compare(a.StartedAt), cmp.Compare(b.TurnID, a.TurnID))
			})
			if horizon != nil {
				partials = slices.DeleteFunc(partials, func(p store.TurnPartial) bool {
					return p.StartedAt.Before(*horizon)
				})
			}
		}
	}
	if len(partials) > q.Limit {
		partials, more = partials[:q.Limit], true
	}
	page := TurnPage{Turns: make([]store.Turn, 0, len(partials))}
	for _, p := range partials {
		page.Turns = append(page.Turns, p.Turn())
	}
	if !byTokens {
		switch {
		case len(page.Turns) > 0:
			at := page.Turns[len(page.Turns)-1].StartedAt
			page.Next = &at
		case more && horizon != nil:
			// NOTHING BETWEEN THE CURSOR AND THE HORIZON, and more past
			// it: resume from the horizon rather than report an end.
			page.Next = horizon
		}
	}
	f.report(QuestionTurns, coverage, started)
	return page, coverage, nil
}

// PhaseTokens answers the per-phase spend records of a window from every node,
// newest first, cut to the query's limit — what the live projection's spend
// rollup is seeded from.
//
// THE WINDOW IS PINNED to this node's clock before anybody is asked, for
// [Fleet.Histogram]'s reason: a peer counting "a day back" from its own clock
// would answer a window its neighbours did not.
func (f *Fleet) PhaseTokens(ctx context.Context, q store.PhaseTokenQuery) ([]tokens.Record, Coverage, error) {
	started := time.Now()
	q.Since, q.Until = q.Window(time.Now().UTC())
	q.SinceDays = 0
	g, err := gather(ctx, f, QuestionPhaseTokens, phaseTokenParamsOf(q), nil,
		func(ctx context.Context) (spendPart, error) { return spendPartOf(ctx, f.Local, q) })
	if err != nil {
		return nil, Coverage{}, err
	}
	records := MergeSpend(g.parts(), q.Limit)
	f.report(QuestionPhaseTokens, g.coverage, started)
	return records, g.coverage, nil
}

// ErrRankedCursor refuses a cursor on a page ranked by tokens: a ranking has no
// position to resume from, and paging one by start time would mix two orders
// on one screen.
var ErrRankedCursor = errors.New("eventfan: a page ranked by tokens takes no cursor")
