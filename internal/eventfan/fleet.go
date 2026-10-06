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

	// Clock is the ASKER'S clock: every question is asked at one instant
	// read from it, which this node's own read and every peer's floor the
	// history at — see [store.EventLog]. Nil is the wall clock. Read to the
	// microsecond, whatever it carries finer ([instant]).
	//
	// A dependency rather than a call to time.Now buried in each question,
	// because the instant is what the answer is about: a question asked of
	// three nodes is one question only if all three answer it as of the
	// same moment, and the moment has to come from one place to be one.
	Clock func() time.Time
}

// now is the instant a question is asked at, read ONCE per question.
func (f *Fleet) now() time.Time {
	if f.Clock != nil {
		return instant(f.Clock())
	}
	return instant(time.Now())
}

// askedAt is the instant a question is asked at: the one its caller pinned —
// another answer's own, so the two halves of one screen share it — or [now].
func (f *Fleet) askedAt(pinned time.Time) time.Time {
	if pinned.IsZero() {
		return f.now()
	}
	return instant(pinned)
}

// instant is t at the STORE'S RESOLUTION, the microsecond ([store.EncodeTime]).
//
// Finer than that, one instant is two edges. Every statement floors at the
// ENCODED instant, which drops the nanoseconds, while every comparison the
// asker makes itself — the horizon it holds what comes back to ([heldTo]), the
// window a caller names beside an answer — reads them. So a row at the floor's
// own microsecond was inside the history to the store and under it to the
// asker: counted by one question of an answer and missing from the listing
// beside it. Read at the microsecond, the floor is one value on both sides.
func instant(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

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

	// More says rows exist past this page. See [MergeListing]. EXACT for the
	// phase listings, whose reads ask one row past the page; for the event
	// list it is "a node's page filled", which its answer never reads as a
	// cursor — see listPartOf.
	More bool

	// At is the instant the page was asked at, which every node floored it
	// at — for [TurnDetail.At]'s reason: a caller composing one answer from
	// this page and a second read asks the second at the same instant.
	At time.Time
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

	// Nodes is every node whose OWN store holds part of this turn, sorted —
	// which is where the turn RAN, since an event is written only to the
	// store of the node that published it. More than one for a turn resumed
	// on another node after a restart, or one whose seat moved while a
	// coding run was out. A stored row carries no node of its own, so this
	// is the only place the answer can say it.
	Nodes []string

	// At is the instant the turn was asked at, which every node floored
	// its part at. Returned so a caller composing ONE answer from this
	// read and another — the turn page's other attempts — asks the second
	// at the same instant ([store.TurnQuery.At]): read at a later one, a
	// row between the two horizons is in this turn and missing from the
	// second read, which then lists the turn shown as starting later, or
	// not at all.
	At time.Time
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
	// THE LOWEST VERSION THAT ANSWERS THIS, so a peer an upgrade has not
	// reached yet still answers every question it can answer correctly.
	version := versionOf(q, params)
	if fan {
		body, err := json.Marshal(params)
		if err != nil {
			return zero, fmt.Errorf("eventfan: encode the %s parameters: %w", q, err)
		}
		req, err := json.Marshal(request{
			Version: version, Asker: f.Self, Question: q, Params: body, TurnIDs: ids, Names: true,
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
			node, part, why := decodeReply[T](raw, version)
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

// decodeReply reads one peer's reply to a question asked in version `asked`:
// whose it is, its part, and — when it is not an answer — why.
//
// A REFUSAL IS READ BEFORE THE VERSION, because a peer that refused says why in
// its own words — an older build naming the version it was asked in is the
// clearest account there is of why it is missing. An ANSWER in any version but
// the one asked is not read at all: its fields are not the ones the merge
// expects, and a part missing a filter or a summed field is wrong rather than
// short.
func decodeReply[T any](raw []byte, asked int) (node string, part T, why string) {
	var r reply
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", part, ""
	}
	switch {
	case r.Error != "":
		return r.Node, part, r.Error
	case r.Version != asked:
		return r.Node, part, fmt.Sprintf("answered in history protocol v%d, and "+
			"was asked in v%d; it is running a different build", r.Version, asked)
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

// settle is the second question an answer that COUNTS needs when some node
// named a row rather than counting it: every node's part, from the first
// scatter, beside which of the named rows each node keeps, and the coverage of
// both scatters. `named` reads a part's named rows.
//
// TWO SCATTERS WHEN A ROW IS IN FLIGHT, and one otherwise. Each node counts the
// rows it KEEPS and names the ones it holds of a custody batch it has not
// settled — a row a second data node may hold too, kept or not. When any node
// named one, every node is asked which of them it keeps ([QuestionKept]), and
// the merge counts each once ([once]). Nothing is named on a fleet with no
// stateless node, nor between batches on one that has them, which is nearly
// always, so the second question is the exception; and a node alone needs
// none, since a row only it holds is counted once by naming it.
//
// A NODE THAT ANSWERED ONLY THE SECOND counted nothing the merge holds — the
// coverage names it for the first — so what it keeps is not read: taken as
// evidence that a row was counted, a row it keeps and nobody counted would be
// in no count at all.
func settle[T any](ctx context.Context, f *Fleet, first gathered[T],
	named func(T) []store.UnsettledRow,
) ([]Counted[T], Coverage, error) {
	parts := make([]Counted[T], 0, 1+len(first.peers))
	parts = append(parts, Counted[T]{Node: f.Self, Part: first.mine})
	for _, p := range first.peers {
		parts = append(parts, Counted[T]{Node: p.node, Part: p.part})
	}
	rows := identities(parts, named)
	if len(rows) == 0 || !first.fanned {
		return parts, first.coverage, nil
	}
	second, err := gather(ctx, f, QuestionKept, keptParams{Rows: rows}, nil,
		func(ctx context.Context) (keptPart, error) {
			kept, err := f.Local.KeptRows(ctx, rows)
			return keptPart{Rows: kept}, err
		})
	if err != nil {
		return nil, Coverage{}, err
	}
	parts[0].Kept = second.mine.Rows
	for _, p := range second.peers {
		if i := slices.IndexFunc(parts, func(c Counted[T]) bool { return c.Node == p.node }); i >= 0 {
			parts[i].Kept = p.part.Rows
		}
	}
	return parts, first.coverage.And(second.coverage), nil
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
	// ONE INSTANT for the page and the siblings, on every node.
	q.At = f.askedAt(q.At)
	g, err := gather(ctx, f, QuestionEvents, listParamsOf(q), nil,
		func(ctx context.Context) (listPart, error) { return listPartOf(ctx, f.Local, q) })
	if err != nil {
		return Listing{}, Coverage{}, err
	}
	g = heldTo(g, q.At)
	rows, more := MergeListing(g.parts(), q.Limit)
	coverage := g.coverage
	if q.RelatedAgent != "" && g.fanned {
		traces := store.TraceIDsOf(rows)
		if len(traces) > 0 {
			p := traceRowsParams{TraceIDs: traces, Limit: q.Limit, At: q.At}
			sib, err := gather(ctx, f, QuestionTraceRows, p, nil,
				func(ctx context.Context) (listPart, error) {
					sibs, err := f.Local.TraceRows(ctx, traces, q.Limit, q.At)
					return listPart{Rows: sibs}, err
				})
			if err != nil {
				return Listing{}, Coverage{}, err
			}
			var siblings [][]store.EventRecord
			for _, part := range heldTo(sib, q.At).parts() {
				siblings = append(siblings, part.Rows)
			}
			rows = MergeRelated(rows, union(siblings...), q.Limit, more)
			coverage = coverage.And(sib.coverage)
		}
	}
	f.report(QuestionEvents, coverage, started)
	return Listing{Rows: rows, More: more, At: q.At}, coverage, nil
}

// Histogram answers the log's time axis over the fleet.
//
// THE WINDOW IS PINNED to this node's clock before anybody is asked, so every
// node cuts the same bars and floors the rows it counts into them at the same
// instant — see [store.ListQuery.At].
//
// THE PARTIAL BAR IS DROPPED HERE, after the sum and never before it. Every
// node cuts a window the history clips down to the bucket the floor falls in
// ([store.HistogramQuery.Window]) — the shape every build cuts, so a node on
// any build is summed with the rest — and the axis a caller is shown begins at
// the first whole bucket inside the history ([store.EventHistogram.InsideHistory]),
// cut at the instant every node was asked at.
func (f *Fleet) Histogram(ctx context.Context, q store.HistogramQuery) (store.EventHistogram, Coverage, error) {
	started := time.Now()
	q.At = f.askedAt(q.At)
	g, err := gather(ctx, f, QuestionSeries, seriesParamsOf(q), nil,
		func(ctx context.Context) (store.EventHistogram, error) { return f.Local.Histogram(ctx, q) })
	if err != nil {
		return store.EventHistogram{}, Coverage{}, err
	}
	parts, coverage, err := settle(ctx, f, g, histogramNamed)
	if err != nil {
		return store.EventHistogram{}, Coverage{}, err
	}
	merged, refused := MergeSeries(q, parts)
	for _, node := range refused {
		coverage = coverage.And(Coverage{Complete: false, Nodes: []NodeCoverage{{
			ID: node, Error: "it answered a different window, so its bars " +
				"cannot be summed with this node's; it is running a different build",
		}}})
	}
	f.report(QuestionSeries, coverage, started)
	return merged.InsideHistory(q.At), coverage, nil
}

// ByID answers one event, from whichever node holds it.
//
// NOT FOUND ANYWHERE is [store.ErrNotFound], and says which nodes could not be
// asked: a dead link is the ordinary case, and one whose node was merely
// silent is a different fact. A copy under the history horizon is not found
// either, whichever node answered with it: a build before the floor read every
// copy it still held, so the horizon is held HERE, on the asker that owns the
// instant — see [heldTo].
func (f *Fleet) ByID(ctx context.Context, id string) (store.EventRecord, Coverage, error) {
	started := time.Now()
	at := f.now()
	g, err := gather(ctx, f, QuestionEvent, idParams{ID: id, At: at}, nil,
		func(ctx context.Context) (eventPart, error) { return eventPartOf(ctx, f.Local, id, at) })
	if err != nil {
		return store.EventRecord{}, Coverage{}, err
	}
	f.report(QuestionEvent, g.coverage, started)
	rec, found := FirstFound(heldTo(g, at).parts())
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
	at := f.now()
	g, err := gather(ctx, f, QuestionTrace, idParams{ID: id, At: at}, nil,
		func(ctx context.Context) (tracePart, error) { return tracePartOf(ctx, f.Local, id, at) })
	if err != nil {
		return Trace{}, Coverage{}, err
	}
	parts, coverage, err := settle(ctx, f, heldTo(g, at),
		func(p tracePart) []store.UnsettledRow { return p.Unsettled })
	if err != nil {
		return Trace{}, Coverage{}, err
	}
	rows, total := MergeTrace(parts)
	f.report(QuestionTrace, coverage, started)
	return Trace{Rows: rows, Total: total}, coverage, nil
}

// Turn answers every event of one turn, from every node that ran part of it.
func (f *Fleet) Turn(ctx context.Context, id string) (TurnDetail, Coverage, error) {
	started := time.Now()
	at := f.now()
	g, err := gather(ctx, f, QuestionTurn, idParams{ID: id, At: at}, nil,
		func(ctx context.Context) (turnPart, error) { return turnPartOf(ctx, f.Local, id, at) })
	if err != nil {
		return TurnDetail{}, Coverage{}, err
	}
	g = heldTo(g, at)
	parts, coverage, err := settle(ctx, f, g,
		func(p turnPart) []store.UnsettledRow { return p.Unsettled })
	if err != nil {
		return TurnDetail{}, Coverage{}, err
	}
	rows, total, traces := MergeTurn(parts)
	f.report(QuestionTurn, coverage, started)
	return TurnDetail{Rows: rows, Total: total, Traces: traces, Nodes: turnNodes(f.Self, g), At: at},
		coverage, nil
}

// turnNodes is every node that answered with part of the turn.
func turnNodes(self string, g gathered[turnPart]) []string {
	var nodes []string
	if g.mine.rows() > 0 || len(g.mine.Closing) > 0 {
		nodes = append(nodes, self)
	}
	for _, p := range g.peers {
		if p.part.rows() > 0 || len(p.part.Closing) > 0 {
			nodes = append(nodes, p.node)
		}
	}
	slices.Sort(nodes)
	return slices.Compact(nodes)
}

// Phases answers the company's phase records, newest first, payload included —
// or one seat's, named by the id every node derives for its handle.
func (f *Fleet) Phases(ctx context.Context, agentID string, limit int, before *store.Cursor) (Listing, Coverage, error) {
	started := time.Now()
	limit = phaseLimit(limit)
	at := f.now()
	p := phasesParams{AgentID: agentID, Limit: limit, Before: cursorOf(before), At: at}
	g, err := gather(ctx, f, QuestionPhases, p, nil,
		func(ctx context.Context) (listPart, error) {
			rows, more, err := f.Local.Phases(ctx, agentID, limit, before, at)
			return listPart{Rows: rows, Full: more}, err
		})
	if err != nil {
		return Listing{}, Coverage{}, err
	}
	rows, more := MergeListing(heldTo(g, at).parts(), limit)
	f.report(QuestionPhases, g.coverage, started)
	return Listing{Rows: rows, More: more, At: at}, g.coverage, nil
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
	at := f.now()
	p := phasesParams{AgentID: agentID, Role: role, Before: cursorOf(before), At: at}
	g, err := gather(ctx, f, QuestionSeatPhases, p, nil,
		func(ctx context.Context) (listPart, error) {
			rows, more, err := f.Local.AgentPhases(ctx, agentID, role, before, at)
			return listPart{Rows: rows, Full: more}, err
		})
	if err != nil {
		return Listing{}, Coverage{}, err
	}
	rows, more := MergeListing(heldTo(g, at).parts(), store.AgentPhaseLimit)
	f.report(QuestionSeatPhases, g.coverage, started)
	return Listing{Rows: rows, More: more, At: at}, g.coverage, nil
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
//
// A TURN IS PAGED BY WHERE IT IS LISTED, which is not always where it began.
// The cursor is a position on each node's own page, so a turn can be resumed
// from only at a start some node's page reaches — the earliest start among the
// nodes that LIST it, which every node says of its own share on the second
// scatter ([store.EventLog.ListedTurns]). The fold of every share says where
// the turn began, and that is the start shown; the two differ for a turn whose
// earliest half its own node does not list — the clean half of a turn that
// failed later, under a page of failures, or a half under the asker's horizon
// (below). Paged by the start shown, such a turn sat below a position no page
// reached: a full node's page or the page's size cut it, and the cursor moved
// on past the half that did list it, so no page of the walk held it.
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
	// THE WINDOW IS PINNED to this node's clock before anybody is asked, for
	// [Fleet.PhaseTokens]' reason — and because the whole turn has to be
	// held against it: a node's half of a resumed turn can start inside the
	// window while the turn began outside it on another node. The INSTANT
	// travels with it, so every node floors the window and its shares at the
	// asker's history horizon rather than at its own — see
	// [store.TurnQuery.At].
	q.At = f.askedAt(q.At)
	q.Since, q.Until = q.Window(q.At)
	q.SinceDays = 0
	history := q.At.Add(-store.EventHistory)
	// A WINDOW REACHING THE HISTORY IS ASKED FOR AS THE HISTORY, in whole
	// days, rather than as the asker's horizon. A node on this build reads
	// the two alike — it floors both at the instant it was handed — but a
	// build that ignores the instant reads days back from its OWN clock and
	// an instant as the instant, so handed the asker's horizon while its clock
	// ran behind, it judged every turn with a row in the strip between the two
	// horizons as one that began before the window and listed none of them: a
	// turn only it held was on no page, the turn page's own attempts included.
	// Asked for its history, it lists them from its own horizon, and the
	// asker holds them to its own below. It also asks a window with no upper
	// edge in v1, which every build answers.
	reachesHistory := q.Since.Equal(history)
	wire := turnsParamsOf(q)
	if reachesHistory {
		wire.Since, wire.SinceDays = time.Time{}, store.MaxTurnDays
	}
	first, err := gather(ctx, f, QuestionTurns, wire, nil,
		func(ctx context.Context) (turnsPart, error) { return turnsPartOf(ctx, f.Local, q) })
	if err != nil {
		return TurnPage{}, Coverage{}, err
	}
	var turns []pagedTurn
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
		// THIS NODE IS THE FLEET: its page is whole as it stands, and every
		// turn on it is listed where it began.
		for _, p := range first.mine.Turns {
			turns = append(turns, pagedTurn{TurnPartial: p, listedAt: p.StartedAt})
		}
	} else {
		lists := make([][]store.TurnPartial, 0, 1+len(first.peers))
		listed := listings{}
		for _, part := range first.parts() {
			lists = append(lists, part.Turns)
			// A TURN ON A NODE'S PAGE IS LISTED THERE, whatever its share
			// says below — the evidence a share read a moment later cannot
			// take back.
			for _, p := range part.Turns {
				listed.at(p.TurnID, p.StartedAt)
			}
		}
		var partials []store.TurnPartial
		ids := idsOf(lists...)
		if len(ids) > 0 {
			share := q
			share.IDs = ids
			second, err := gather(ctx, f, QuestionTurns, wire, ids,
				func(ctx context.Context) (turnsPart, error) {
					return turnsPartOf(ctx, f.Local, share)
				})
			if err != nil {
				return TurnPage{}, Coverage{}, err
			}
			for _, part := range second.parts() {
				lister := listerOf(part, q, reachesHistory)
				for _, p := range part.Turns {
					if lister(p) {
						listed.at(p.TurnID, p.StartedAt)
					}
				}
			}
			// EACH SHARE SUMS THE ROWS ITS NODE KEEPS, and a row a custody
			// batch in flight puts on two nodes is added once ([settle]).
			shares, sharesCoverage, err := settle(ctx, f, second,
				func(p turnsPart) []store.UnsettledRow { return p.Unsettled })
			if err != nil {
				return TurnPage{}, Coverage{}, err
			}
			partials = MergeTurnShares(shares)
			coverage = coverage.And(sharesCoverage)
		}
		for _, p := range partials {
			// NO TURN WITH NOTHING INSIDE THE ASKER'S HISTORY. Only a node
			// on a build that ignores the instant answers one, from the
			// strip between its horizon and the asker's when its clock runs
			// behind — and what lies under the horizon is not history this
			// asker serves.
			if p.EndedAt.Before(history) {
				continue
			}
			// NO TURN STARTS BELOW THE ASKER'S HORIZON. A share is floored
			// at the history rather than at the window, and that same node
			// floors it at its own clock — so it folds rows from the strip
			// into a turn, and the merged start lands below the horizon.
			// Held against the window as it stands, that start read as a
			// turn that began before the window, and the turn left the page
			// whole — the turn page's own attempts among them, which are
			// asked from the horizon. So the start shown is held to the
			// horizon and the counts keep the strip, and the turn is paged
			// where it is listed; a window starting above the horizon still
			// drops it, since it did begin before that window.
			if p.StartedAt.Before(history) {
				p.StartedAt = history
			}
			turns = append(turns, pagedTurn{TurnPartial: p, listedAt: listed[p.TurnID]})
		}
		// THE TURN-LEVEL FILTERS AGAIN, over the WHOLE turn: a node lists a
		// turn as clean when its own half is, and the other half may have
		// failed. The cursor is on where the turn is listed, which is what
		// the cursor that led here was taken from.
		turns = slices.DeleteFunc(turns, func(t pagedTurn) bool {
			if q.Failed != nil && t.Failed != *q.Failed {
				return true
			}
			if t.StartedAt.Before(q.Since) {
				return true
			}
			return !byTokens && !q.Before.IsZero() && !t.listedAt.Before(q.Before)
		})
		if byTokens {
			slices.SortStableFunc(turns, func(a, b pagedTurn) int {
				return cmp.Or(cmp.Compare(b.TotalTokens, a.TotalTokens),
					b.StartedAt.Compare(a.StartedAt), cmp.Compare(b.TurnID, a.TurnID))
			})
		} else {
			slices.SortStableFunc(turns, func(a, b pagedTurn) int {
				return cmp.Or(b.listedAt.Compare(a.listedAt), cmp.Compare(b.TurnID, a.TurnID))
			})
			if horizon != nil {
				turns = slices.DeleteFunc(turns, func(t pagedTurn) bool {
					return t.listedAt.Before(*horizon)
				})
			}
		}
	}
	if len(turns) > q.Limit {
		turns, more = turns[:q.Limit], true
	}
	page := TurnPage{Turns: make([]store.Turn, 0, len(turns))}
	for _, t := range turns {
		page.Turns = append(page.Turns, t.Turn())
	}
	// A CURSOR ONLY WHERE THERE IS MORE. Every non-empty page used to carry
	// one, so the last page of a seat's forty-seven turns offered "older"
	// and a reader who asked got an empty page — and a client counting what
	// it had loaded could only ever write it as a floor ("47+"), because the
	// answer never said the walk had ended, which is what a nil `Next` is
	// documented to say. It is where the page's last turn is LISTED, never
	// the start shown for it, which may be a position no node's page reaches.
	if !byTokens && more {
		switch {
		case len(turns) > 0:
			at := turns[len(turns)-1].listedAt
			page.Next = &at
		case horizon != nil:
			// NOTHING BETWEEN THE CURSOR AND THE HORIZON, and more past
			// it: resume from the horizon rather than report an end.
			page.Next = horizon
		}
	}
	f.report(QuestionTurns, coverage, started)
	return page, coverage, nil
}

// pagedTurn is a folded turn and where a page lists it.
type pagedTurn struct {
	store.TurnPartial

	// listedAt is the earliest start at which a node's page lists the turn:
	// what it is ordered and cut by, and what a cursor after it is.
	listedAt time.Time
}

// listings is the earliest start at which any node lists each turn.
type listings map[string]time.Time

func (l listings) at(id string, start time.Time) {
	if seen, ok := l[id]; !ok || start.Before(seen) {
		l[id] = start
	}
}

// listerOf reports, for one node's answer to the second scatter, whether its
// share of a turn is one its page lists.
//
// THE NODE'S OWN JUDGEMENT where it gave one ([turnsPart.Judged]). A build
// before the field gives none, and its share is judged here by what it
// carries: a start inside the window — or, asked for the whole history, any
// start, since that build floored both its page and its share at its own
// horizon — and its own failure and models, which are its page's rows when the
// start is inside the window. What a share does not carry is whether its rows
// name the work item a page is narrowed to, so such a half is read as listing
// the turn whatever it names.
func listerOf(part turnsPart, q store.TurnQuery, reachesHistory bool) func(store.TurnPartial) bool {
	if part.Judged {
		return func(p store.TurnPartial) bool { return slices.Contains(part.Listed, p.TurnID) }
	}
	return func(p store.TurnPartial) bool {
		switch {
		case !reachesHistory && p.StartedAt.Before(q.Since):
			return false
		case !q.Until.IsZero() && !p.StartedAt.Before(q.Until):
			return false
		case q.Failed != nil && p.Failed != *q.Failed:
			return false
		case q.Model != "" && !slices.Contains(p.Models, q.Model):
			return false
		}
		return true
	}
}

// PhaseTokens answers the per-phase spend records of a window from every node,
// newest first, cut to the query's limit — what the live projection's spend
// rollup is seeded from.
//
// THE WINDOW IS PINNED to this node's clock before anybody is asked, for
// [Fleet.Histogram]'s reason: a peer counting "a day back" from its own clock
// would answer a window its neighbours did not. And so is the instant it was
// cut against, which every node floors it at — see [store.PhaseTokenQuery.At].
func (f *Fleet) PhaseTokens(ctx context.Context, q store.PhaseTokenQuery) ([]tokens.Record, Coverage, error) {
	started := time.Now()
	q.At = f.askedAt(q.At)
	q.Since, q.Until = q.Window(q.At)
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

// NotificationOutcomes answers how many notifications each third-party app had
// dropped and merged over `[q.Since, q.At)`, summed across every node.
//
// A COUNT OVER A NAMED WINDOW, never a page of the events it counts: the
// integrations answer states one window for the deliveries and what became of
// them, and the newest page of outcome events spans whatever it spans.
//
// THE INSTANT IS THE ASKER'S, read once when the caller pinned none — and a
// caller that names a window beside another read passes that read's instant
// ([Listing.At]), so both halves of one answer share an edge. It is the
// window's top and the floor's anchor on every node at once. Nothing that comes
// back is held to the asker's horizon here, because there is nothing to hold:
// a count carries no instants, and no build that answers this question ignores
// `at` — the question arrived in the version that sent it (see [Protocol]), and
// an older build refuses it and is named in the coverage instead.
//
// Each node counts the rows it KEEPS and names the ones it holds of a custody
// batch it has not settled ([store.NotificationOutcomes.Unsettled]), and the
// asker counts each named row once ([settle], [MergeOutcomes]).
func (f *Fleet) NotificationOutcomes(ctx context.Context, q store.OutcomeQuery) (store.NotificationOutcomes, Coverage, error) {
	started := time.Now()
	q.At = f.askedAt(q.At)
	first, err := gather(ctx, f, QuestionNotificationOutcomes, outcomeParamsOf(q), nil,
		func(ctx context.Context) (store.NotificationOutcomes, error) {
			return f.Local.NotificationOutcomes(ctx, q)
		})
	if err != nil {
		return store.NotificationOutcomes{}, Coverage{}, err
	}
	parts, coverage, err := settle(ctx, f, first, outcomesNamed)
	if err != nil {
		return store.NotificationOutcomes{}, Coverage{}, err
	}
	merged := MergeOutcomes(parts)
	f.report(QuestionNotificationOutcomes, coverage, started)
	return merged, coverage, nil
}

// ErrRankedCursor refuses a cursor on a page ranked by tokens: a ranking has no
// position to resume from, and paging one by start time would mix two orders
// on one screen.
var ErrRankedCursor = errors.New("eventfan: a page ranked by tokens takes no cursor")
