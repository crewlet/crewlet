package eventfan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// THE SCATTER'S WIRE, and why it is its own encoding rather than an event.
//
// A history read is not an event: nothing subscribes to it, nothing replays
// it, no node needs to know it happened, and it must leave no trace — see
// [queue.EventQueue.Ask]. So it carries its own small JSON document on the
// ephemeral verbs.
//
// ONE VERSION ON THE WIRE. Every request carries the protocol version its asker
// speaks ([Protocol]) and a node answers only a request in its own: a peer on
// any other version refuses by version, in its own words, and the asker names
// it in the coverage rather than reading an answer it cannot vouch for. A field
// a peer does not know is never answered around — a FILTER it dropped would
// answer a wider question than was asked, rows merged in as though they
// matched, and an ANSWER field a merge sums would read as zero from a peer that
// never sends it — because no peer that does not know it is ever asked. Adding
// a question, a filter or a field a merge reads is moving [Protocol].

// Protocol is the scatter version this build speaks, asks in and answers.
//
//   - v1: the base format.
//   - v2: `channel_id` and `agent_id` on a listing's filters, and the
//     `failed` split on every histogram bar and total.
//   - v3: `suspended` on a listing's filters, `agent_id` on the company's
//     phases — the question that narrowed by a role name, which two unit
//     seats share, and now narrows by the seat's own id — and `since` and
//     `until` on a page of turns, the window as the asker's two instants on
//     the turn's start rather than whole days back from each peer's clock.
//   - v4: `failed` on a listing's filters — the event log's "Failures only",
//     which used to narrow the rows a tab had paged in rather than the rows
//     it was sent.
//   - v5: the asker's instant, `at`, on every question's parameters — the
//     instant every node floors the history at (see [store.EventLog]), so no
//     node answers as of its own clock; the rows a count NAMES rather than
//     counts, beside every count — a custody batch the node has written and
//     not settled (see the package doc) — and the second question that
//     resolves them, `kept`: which of the named rows each node keeps, so a
//     row two data nodes hold is counted once; the `notification_outcomes`
//     question, what became of the notifications each third-party app
//     delivered over a window the asker names, which the integrations answer
//     used to take from the newest page of notification events, whose span
//     was its own; `before_id` on a page of turns, the cursor's second term
//     beside its start ([store.TurnCursor]); and `listed` on the second
//     scatter of `turns`, which of the named turns the node's page lists, so
//     the asker pages each turn where a page lists it ([Fleet.Turns]).
const Protocol = 5

// Subject is where a history question is scattered: ONE subject for the whole
// fleet, every node serving it, because the answerers are every node rather
// than whichever the asker thought to name.
const Subject = topics.ObserveRead

// Question names what a request asks.
//
// A NAMED STRING with Valid, so a question a newer build asks is a value this
// one refuses by name rather than a panic.
type Question string

// The questions, one per history read the API serves plus the two second
// scatters a first one needs.
const (
	QuestionEvents     Question = "events"
	QuestionEvent      Question = "event"
	QuestionSeries     Question = "event_series"
	QuestionTrace      Question = "trace"
	QuestionTurn       Question = "turn"
	QuestionTurns      Question = "turns"
	QuestionPhases     Question = "phases"
	QuestionSeatPhases Question = "seat_phases"
	QuestionTraceRows  Question = "trace_rows"
	// QuestionPhaseTokens is the per-phase spend records of a window, the
	// live projection's spend rollup's seed: without every node's, a
	// restarted node's rollup described only the phases it had published
	// itself.
	QuestionPhaseTokens Question = "phase_tokens"
	// QuestionNotificationOutcomes is how many notifications each
	// third-party app had dropped and merged over a window, the integrations
	// answer's outcome counts: an outcome event is written to the store of
	// the node that decided it, or of the data node keeping its custody
	// batch, so one node's count of the rows it keeps is its share of the
	// fleet's — beside the rows it holds unsettled, named. v5 — see
	// [Protocol].
	QuestionNotificationOutcomes Question = "notification_outcomes"
	// QuestionKept is every count's second question: which of the rows some
	// node named rather than counted — a custody batch it has written and not
	// settled — each node KEEPS, so the asker counts each such row once
	// whichever data nodes hold it ([once]). It asks about a row by its
	// identity alone, so one question answers for every count. v5.
	QuestionKept Question = "kept"
)

// Questions is the closed set.
var Questions = []Question{
	QuestionEvents, QuestionEvent, QuestionSeries, QuestionTrace, QuestionTurn,
	QuestionTurns, QuestionPhases, QuestionSeatPhases, QuestionTraceRows,
	QuestionPhaseTokens, QuestionNotificationOutcomes, QuestionKept,
}

// Valid reports whether q is a question this build answers.
func (q Question) Valid() bool {
	for _, known := range Questions {
		if q == known {
			return true
		}
	}
	return false
}

// request is one scattered question.
type request struct {
	// Version is what the ASKER speaks.
	Version int `json:"version"`

	// Asker is the node that scattered it, so that node's own answerer —
	// which receives every request on the subject like every other — stays
	// silent rather than answering a question its asker already read
	// locally.
	Asker string `json:"asker"`

	Question Question        `json:"question"`
	Params   json.RawMessage `json:"params"`

	// TurnIDs is the second scatter of `turns`: every node's share of
	// exactly these turns.
	TurnIDs []string `json:"turn_ids,omitempty"`
}

// reply is one node's answer.
type reply struct {
	Version int    `json:"version"`
	Node    string `json:"node"`

	// Answer is the question's own part, present when Error is empty.
	Answer json.RawMessage `json:"answer,omitempty"`

	// Error is why this node could not answer, in its own words — a read
	// that failed, a question it does not know, an answer larger than the
	// transport carries. A reply rather than silence, so the coverage says
	// WHY a node is missing rather than only that it is.
	Error string `json:"error,omitempty"`
}

// ---- the parameters, one wire type per question ------------------------- //
//
// TAGGED WIRE TYPES rather than the store's query structs marshalled as they
// are: those carry no tags, so a field renamed in the store would silently
// rename it on the wire, and a peer reading the old name would drop the filter
// — a wider answer than was asked for, merged in as though it matched.
//
// EVERY QUESTION CARRIES THE ASKER'S INSTANT, `at`, and a node refuses one that
// carries none ([instantOf]): it is what every node floors the history at, and
// a node answering as of its own clock would answer a different question.

type cursorWire struct {
	Time time.Time `json:"time"`
	ID   string    `json:"id"`
}

func cursorOf(c *store.Cursor) *cursorWire {
	if c == nil {
		return nil
	}
	return &cursorWire{Time: c.Time, ID: c.ID}
}

func (c *cursorWire) cursor() *store.Cursor {
	if c == nil {
		return nil
	}
	return &store.Cursor{Time: c.Time, ID: c.ID}
}

type listParams struct {
	Type         string      `json:"type,omitempty"`
	Source       string      `json:"source,omitempty"`
	Category     string      `json:"category,omitempty"`
	TraceID      string      `json:"trace_id,omitempty"`
	Actor        string      `json:"actor,omitempty"`
	TurnID       string      `json:"turn_id,omitempty"`
	WorkKey      string      `json:"work_key,omitempty"`
	WorkItem     string      `json:"work_item,omitempty"`
	RelatedAgent string      `json:"related_agent,omitempty"`
	Since        time.Time   `json:"since,omitzero"`
	Until        time.Time   `json:"until,omitzero"`
	Before       *cursorWire `json:"before,omitempty"`
	Limit        int         `json:"limit"`

	ChannelID string `json:"channel_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	Suspended *bool  `json:"suspended,omitempty"`
	Failed    *bool  `json:"failed,omitempty"`

	// At is the asker's instant.
	At time.Time `json:"at"`
}

func listParamsOf(q store.ListQuery) listParams {
	return listParams{
		Type: q.Type, Source: q.Source, Category: q.Category,
		TraceID: q.TraceID, Actor: q.Actor, TurnID: q.TurnID,
		WorkKey: q.WorkKey, WorkItem: q.WorkItem, RelatedAgent: q.RelatedAgent,
		Since: q.Since, Until: q.Until, Before: cursorOf(q.Before), Limit: q.Limit,
		ChannelID: q.ChannelID, AgentID: q.AgentID, Suspended: q.Suspended,
		Failed: q.Failed, At: q.At,
	}
}

func (p listParams) query() store.ListQuery {
	return store.ListQuery{
		Type: p.Type, Source: p.Source, Category: p.Category,
		TraceID: p.TraceID, Actor: p.Actor, TurnID: p.TurnID,
		WorkKey: p.WorkKey, WorkItem: p.WorkItem, RelatedAgent: p.RelatedAgent,
		Since: p.Since, Until: p.Until, Before: p.Before.cursor(), Limit: p.Limit,
		ChannelID: p.ChannelID, AgentID: p.AgentID, Suspended: p.Suspended,
		Failed: p.Failed, At: p.At,
	}
}

// seriesParams is the axis's question: a listing's filters, a bar width, and
// the instant the asker cut the window against.
type seriesParams struct {
	List   listParams        `json:"list"`
	Bucket store.EventBucket `json:"bucket"`
	// At is the ASKER's clock, so every node cuts the same window and floors
	// the rows it counts at the same instant.
	//
	// THE AXIS'S OWN FIELD, and the one place its instant travels: the
	// listing's own `at` is left off the filters this carries rather than
	// sent twice, and set from this one when the question is read.
	At time.Time `json:"at"`
}

func seriesParamsOf(q store.HistogramQuery) seriesParams {
	list := listParamsOf(q.ListQuery)
	list.At = time.Time{}
	return seriesParams{List: list, Bucket: q.Bucket, At: q.At}
}

func (p seriesParams) query() store.HistogramQuery {
	list := p.List.query()
	list.At = p.At
	return store.HistogramQuery{ListQuery: list, Bucket: p.Bucket}
}

type idParams struct {
	ID string `json:"id"`

	// At is the asker's instant — one for every read of a part.
	At time.Time `json:"at"`
}

type traceRowsParams struct {
	TraceIDs []string `json:"trace_ids"`
	Limit    int      `json:"limit"`

	// At is the page's own instant, so the siblings are floored where the
	// matches were.
	At time.Time `json:"at"`
}

type turnsParams struct {
	SinceDays int            `json:"since_days,omitempty"`
	AgentRole string         `json:"role,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	Model     string         `json:"model,omitempty"`
	WorkKey   string         `json:"work_key,omitempty"`
	WorkItem  string         `json:"work_item,omitempty"`
	Failed    *bool          `json:"failed,omitempty"`
	Before    time.Time      `json:"before,omitzero"`
	Sort      store.TurnSort `json:"sort,omitempty"`
	Limit     int            `json:"limit"`

	Since time.Time `json:"since,omitzero"`
	Until time.Time `json:"until,omitzero"`

	// BeforeID is the cursor's second term, the turn id at `before`
	// ([store.TurnCursor]).
	BeforeID string `json:"before_id,omitempty"`

	// At is the instant the asker cut the window against, which every node
	// floors it at.
	At time.Time `json:"at"`
}

func turnsParamsOf(q store.TurnQuery) turnsParams {
	p := turnsParams{
		Since: q.Since, Until: q.Until, At: q.At,
		SinceDays: q.SinceDays, AgentRole: q.AgentRole, AgentID: q.AgentID,
		Model: q.Model, WorkKey: q.WorkKey, WorkItem: q.WorkItem,
		Failed: q.Failed, Sort: q.Sort, Limit: q.Limit,
	}
	if q.Before != nil {
		p.Before, p.BeforeID = q.Before.Start, q.Before.TurnID
	}
	return p
}

// query is the store's question, its cursor the start and the turn id together
// ([store.TurnCursor]).
func (p turnsParams) query(ids []string) store.TurnQuery {
	q := store.TurnQuery{
		Since: p.Since, Until: p.Until, At: p.At,
		SinceDays: p.SinceDays, AgentRole: p.AgentRole, AgentID: p.AgentID,
		Model: p.Model, WorkKey: p.WorkKey, WorkItem: p.WorkItem,
		Failed: p.Failed, Sort: p.Sort, Limit: p.Limit,
		IDs: ids,
	}
	if !p.Before.IsZero() {
		q.Before = &store.TurnCursor{Start: p.Before, TurnID: p.BeforeID}
	}
	return q
}

// phasesParams is both phase questions' parameters. The company's phases read
// only AgentID (v3) and a seat's read both, since a seat's history is matched by
// either identifier its rows were written under.
type phasesParams struct {
	AgentID string      `json:"agent_id,omitempty"`
	Role    string      `json:"role,omitempty"`
	Limit   int         `json:"limit,omitempty"`
	Before  *cursorWire `json:"before,omitempty"`

	// At is the asker's instant.
	At time.Time `json:"at"`
}

// phaseTokenParams names the window as the ASKER's two instants, so every node
// cuts the same one rather than each counting back from its own clock — and
// the instant it was cut against, so every node floors it there too.
type phaseTokenParams struct {
	Since     time.Time `json:"since"`
	Until     time.Time `json:"until"`
	AgentRole string    `json:"role,omitempty"`
	Limit     int       `json:"limit,omitempty"`

	// At is the instant the window was cut against.
	At time.Time `json:"at"`
}

func phaseTokenParamsOf(q store.PhaseTokenQuery) phaseTokenParams {
	return phaseTokenParams{Since: q.Since, Until: q.Until, AgentRole: q.AgentRole,
		Limit: q.Limit, At: q.At}
}

func (p phaseTokenParams) query() store.PhaseTokenQuery {
	return store.PhaseTokenQuery{Since: p.Since, Until: p.Until, AgentRole: p.AgentRole,
		Limit: p.Limit, At: p.At}
}

// outcomeParams is the outcome count's window: its bottom edge and the
// asker's instant, which is its top edge and the instant the history floor
// sits under — so every node counts `[since, at)` floored at one horizon.
type outcomeParams struct {
	Since time.Time `json:"since,omitzero"`
	At    time.Time `json:"at"`
}

func outcomeParamsOf(q store.OutcomeQuery) outcomeParams {
	return outcomeParams{Since: q.Since, At: q.At}
}

func (p outcomeParams) query() store.OutcomeQuery {
	return store.OutcomeQuery{Since: p.Since, At: p.At}
}

// keptParams names the rows [QuestionKept] asks about, by the event log's
// identity alone: their instant and their id.
type keptParams struct {
	Rows []store.UnsettledRow `json:"rows"`
}

// keptPart is one node's answer to [QuestionKept]: the named rows it keeps.
type keptPart struct {
	Rows []store.UnsettledRow `json:"rows"`
}

// ---- serving --------------------------------------------------------- //

// Serve makes this node an answerer for the fleet's history questions, from
// its own store.
//
// EVERY NODE SERVES, whatever its roles and whatever its mode: a node in
// maintenance still holds its history, and a node that stopped answering while
// it was being looked at would be a gap in every screen of the company for
// exactly the window somebody was investigating.
func Serve(ctx context.Context, q queue.EventQueue, self string, local *store.EventLog) (queue.Unsubscribe, error) {
	if q == nil || local == nil || self == "" {
		return nil, errors.New("eventfan: serve needs a queue, a store and this node's id")
	}
	return q.Serve(ctx, Subject, func(ctx context.Context, raw []byte) ([]byte, error) {
		var req request
		if err := json.Unmarshal(raw, &req); err != nil {
			// UNATTRIBUTABLE, so silent: an answer to a request this
			// node could not read would be an answer to a question
			// nobody can say it was asked.
			return nil, fmt.Errorf("eventfan: decode a request: %w", err)
		}
		if req.Asker == self {
			// THE ASKER READ ITS OWN STORE DIRECTLY. A second copy
			// of the same rows over the broker would be merged in
			// beside the first.
			return nil, errAsker
		}
		if req.Version != Protocol {
			return encodeError(self, fmt.Sprintf("this node speaks history protocol "+
				"v%d and was asked in v%d; it is running a different build", Protocol, req.Version))
		}
		part, err := answer(ctx, local, req.Question, req.Params, req.TurnIDs)
		if err != nil {
			return encodeError(self, err.Error())
		}
		return fit(self, part, queue.MaxPayloadBytes, req.Version)
	})
}

// errAsker is how the asker's own answerer declines its own request.
var errAsker = errors.New("eventfan: this node asked; it read its own store")

// encodeError is a refusal, stamped with the version this node speaks: it
// answers nothing, so there is no asked version to echo, and the number is what
// tells the asker which build refused.
func encodeError(self, why string) ([]byte, error) {
	return json.Marshal(reply{Version: Protocol, Node: self, Error: why})
}

// answer is ONE node's part of one question, read from its own store.
//
// THE SAME PART READERS FOR THE ASKER AND FOR EVERY PEER, so the asker's own
// share and a peer's are the same shape by construction rather than by two
// readers agreeing.
func answer(ctx context.Context, log *store.EventLog, q Question, params json.RawMessage, ids []string) (any, error) {
	decode := func(into any, at func() time.Time) error {
		if err := json.Unmarshal(params, into); err != nil {
			return fmt.Errorf("the %s parameters do not decode: %w", q, err)
		}
		if at != nil {
			return instantOf(q, at())
		}
		return nil
	}
	switch q {
	case QuestionEvents:
		var p listParams
		if err := decode(&p, func() time.Time { return p.At }); err != nil {
			return nil, err
		}
		return listPartOf(ctx, log, p.query())
	case QuestionSeries:
		var p seriesParams
		if err := decode(&p, func() time.Time { return p.At }); err != nil {
			return nil, err
		}
		return log.Histogram(ctx, p.query())
	case QuestionEvent:
		var p idParams
		if err := decode(&p, func() time.Time { return p.At }); err != nil {
			return nil, err
		}
		return eventPartOf(ctx, log, p.ID, p.At)
	case QuestionTrace:
		var p idParams
		if err := decode(&p, func() time.Time { return p.At }); err != nil {
			return nil, err
		}
		return tracePartOf(ctx, log, p.ID, p.At)
	case QuestionTurn:
		var p idParams
		if err := decode(&p, func() time.Time { return p.At }); err != nil {
			return nil, err
		}
		return turnPartOf(ctx, log, p.ID, p.At)
	case QuestionTurns:
		var p turnsParams
		if err := decode(&p, func() time.Time { return p.At }); err != nil {
			return nil, err
		}
		return turnsPartOf(ctx, log, p.query(ids))
	case QuestionPhases:
		var p phasesParams
		if err := decode(&p, func() time.Time { return p.At }); err != nil {
			return nil, err
		}
		rows, more, err := log.Phases(ctx, p.AgentID, p.Limit, p.Before.cursor(), p.At)
		if err != nil {
			return nil, err
		}
		return listPart{Rows: rows, Full: more}, nil
	case QuestionSeatPhases:
		var p phasesParams
		if err := decode(&p, func() time.Time { return p.At }); err != nil {
			return nil, err
		}
		rows, more, err := log.AgentPhases(ctx, p.AgentID, p.Role, p.Before.cursor(), p.At)
		if err != nil {
			return nil, err
		}
		return listPart{Rows: rows, Full: more}, nil
	case QuestionPhaseTokens:
		var p phaseTokenParams
		if err := decode(&p, func() time.Time { return p.At }); err != nil {
			return nil, err
		}
		return spendPartOf(ctx, log, p.query())
	case QuestionNotificationOutcomes:
		var p outcomeParams
		if err := decode(&p, func() time.Time { return p.At }); err != nil {
			return nil, err
		}
		return log.NotificationOutcomes(ctx, p.query())
	case QuestionKept:
		// ABOUT ROWS BY THEIR IDENTITY, and at no instant: the rows were
		// named by a first question that was floored at one.
		var p keptParams
		if err := decode(&p, nil); err != nil {
			return nil, err
		}
		rows, err := log.KeptRows(ctx, p.Rows)
		if err != nil {
			return nil, err
		}
		return keptPart{Rows: rows}, nil
	case QuestionTraceRows:
		var p traceRowsParams
		if err := decode(&p, func() time.Time { return p.At }); err != nil {
			return nil, err
		}
		rows, err := log.TraceRows(ctx, p.TraceIDs, p.Limit, p.At)
		if err != nil {
			return nil, err
		}
		return listPart{Rows: rows, Full: len(rows) >= p.Limit}, nil
	}
	return nil, fmt.Errorf("this node does not know the history question %q; "+
		"it is running a different build", q)
}

// instantOf refuses a question that carries no instant. Every node floors the
// history at the asker's instant, so one answering as of its own clock would
// answer a different question from its peers; and no asker on this protocol
// sends a question without one.
func instantOf(q Question, at time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("the %s question carries no instant to floor the history at", q)
	}
	return nil
}

// phaseLimit is the page [store.EventLog.Phases] actually reads.
func phaseLimit(limit int) int {
	if limit <= 0 || limit > store.MaxPhasePage {
		return store.MaxPhasePage
	}
	return limit
}

// ---- fitting a reply into the transport --------------------------------- //

// cuttable is a part that can give up rows to fit the transport, keeping the
// ones that matter most and saying that it did.
type cuttable interface {
	// rows is how many rows may be cut.
	rows() int
	// keep returns the part with only the first n cuttable rows, marked so
	// the merge knows the node held more.
	keep(n int) any
}

// ErrTooLarge is a node's answer when not even its most important row fits the
// transport. The asker names it in the coverage.
var ErrTooLarge = errors.New("eventfan: the answer exceeds the transport's message limit")

// fit encodes a reply no larger than limit.
//
// A REPLY THAT DOES NOT FIT IS NOT SENT AT ALL — the broker refuses it and the
// asker sees silence — so an answer over the limit is CUT rather than lost:
// rows are given up from the least important end, and the part says it held
// more, which is exactly what the merge already handles for a page that
// filled. The size is the ENCODED size, measured rather than estimated,
// because a JSON document re-encodes at anywhere from one to six times its
// length depending on what it holds.
func fit(self string, part any, limit, version int) ([]byte, error) {
	encode := func(p any) ([]byte, error) {
		body, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("eventfan: encode an answer: %w", err)
		}
		return json.Marshal(reply{Version: version, Node: self, Answer: body})
	}
	whole, err := encode(part)
	if err != nil {
		return nil, err
	}
	if len(whole) <= limit {
		return whole, nil
	}
	c, ok := part.(cuttable)
	if !ok || c.rows() == 0 {
		return encodeError(self, ErrTooLarge.Error())
	}
	// THE LARGEST PREFIX THAT FITS, by bisection: each probe is a full
	// encode, and a page is at most a few hundred rows.
	lo, hi := 0, c.rows()-1 // hi: the most rows known NOT to be required to fail
	var best []byte
	for lo <= hi {
		mid := (lo + hi) / 2
		body, err := encode(c.keep(mid))
		if err != nil {
			return nil, err
		}
		if len(body) <= limit {
			best, lo = body, mid+1
		} else {
			hi = mid - 1
		}
	}
	if best == nil || !keptAny(c, best) {
		return encodeError(self, ErrTooLarge.Error())
	}
	return best, nil
}

// keptAny reports whether a cut reply still carries a row a merge can place.
//
// A LISTING CUT TO NOTHING says only "I hold rows you cannot see", with no
// position to say where they are — so the merge could not bound what it
// missed, and the node is counted as not answering instead.
func keptAny(c cuttable, body []byte) bool {
	if _, isList := c.(listPart); !isList {
		return true
	}
	var r reply
	if json.Unmarshal(body, &r) != nil {
		return false
	}
	var p listPart
	if json.Unmarshal(r.Answer, &p) != nil {
		return false
	}
	return len(p.Rows) > 0
}
