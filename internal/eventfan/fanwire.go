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
// EVOLUTION IS ADDITIVE, on the event envelope's terms and for the same
// reason: a rolling upgrade puts two builds on one broker. A new FIELD needs no
// bump — an older peer ignores it and answers the question it understood. A
// RESHAPE takes a new [Protocol], and a peer on the other one answers with its
// own version and nothing else, which the asker names in the coverage rather
// than guessing at fields it cannot read.

// Protocol is the scatter's payload version.
const Protocol = 1

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
)

// Questions is the closed set.
var Questions = []Question{
	QuestionEvents, QuestionEvent, QuestionSeries, QuestionTrace, QuestionTurn,
	QuestionTurns, QuestionPhases, QuestionSeatPhases, QuestionTraceRows,
	QuestionPhaseTokens,
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
// become a filter an older peer never applies — a wider answer than was asked
// for, merged in as though it matched.

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
}

func listParamsOf(q store.ListQuery) listParams {
	return listParams{
		Type: q.Type, Source: q.Source, Category: q.Category,
		TraceID: q.TraceID, Actor: q.Actor, TurnID: q.TurnID,
		WorkKey: q.WorkKey, WorkItem: q.WorkItem, RelatedAgent: q.RelatedAgent,
		Since: q.Since, Until: q.Until, Before: cursorOf(q.Before), Limit: q.Limit,
	}
}

func (p listParams) query() store.ListQuery {
	return store.ListQuery{
		Type: p.Type, Source: p.Source, Category: p.Category,
		TraceID: p.TraceID, Actor: p.Actor, TurnID: p.TurnID,
		WorkKey: p.WorkKey, WorkItem: p.WorkItem, RelatedAgent: p.RelatedAgent,
		Since: p.Since, Until: p.Until, Before: p.Before.cursor(), Limit: p.Limit,
	}
}

type seriesParams struct {
	List   listParams        `json:"list"`
	Bucket store.EventBucket `json:"bucket"`
	// At is the ASKER's clock, so every node cuts the same window.
	At time.Time `json:"at"`
}

type idParams struct {
	ID string `json:"id"`
}

type traceRowsParams struct {
	TraceIDs []string `json:"trace_ids"`
	Limit    int      `json:"limit"`
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
}

func turnsParamsOf(q store.TurnQuery) turnsParams {
	return turnsParams{
		SinceDays: q.SinceDays, AgentRole: q.AgentRole, AgentID: q.AgentID,
		Model: q.Model, WorkKey: q.WorkKey, WorkItem: q.WorkItem,
		Failed: q.Failed, Before: q.Before, Sort: q.Sort, Limit: q.Limit,
	}
}

func (p turnsParams) query(ids []string) store.TurnQuery {
	return store.TurnQuery{
		SinceDays: p.SinceDays, AgentRole: p.AgentRole, AgentID: p.AgentID,
		Model: p.Model, WorkKey: p.WorkKey, WorkItem: p.WorkItem,
		Failed: p.Failed, Before: p.Before, Sort: p.Sort, Limit: p.Limit,
		IDs: ids,
	}
}

type phasesParams struct {
	AgentID string      `json:"agent_id,omitempty"`
	Role    string      `json:"role,omitempty"`
	Limit   int         `json:"limit,omitempty"`
	Before  *cursorWire `json:"before,omitempty"`
}

// phaseTokenParams names the window as the ASKER's two instants, so every node
// cuts the same one rather than each counting back from its own clock.
type phaseTokenParams struct {
	Since     time.Time `json:"since"`
	Until     time.Time `json:"until"`
	AgentRole string    `json:"role,omitempty"`
	Limit     int       `json:"limit,omitempty"`
}

func phaseTokenParamsOf(q store.PhaseTokenQuery) phaseTokenParams {
	return phaseTokenParams{Since: q.Since, Until: q.Until, AgentRole: q.AgentRole, Limit: q.Limit}
}

func (p phaseTokenParams) query() store.PhaseTokenQuery {
	return store.PhaseTokenQuery{Since: p.Since, Until: p.Until, AgentRole: p.AgentRole, Limit: p.Limit}
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
		return fit(self, part, queue.MaxPayloadBytes)
	})
}

// errAsker is how the asker's own answerer declines its own request.
var errAsker = errors.New("eventfan: this node asked; it read its own store")

func encodeError(self, why string) ([]byte, error) {
	return json.Marshal(reply{Version: Protocol, Node: self, Error: why})
}

// answer is ONE node's part of one question, read from its own store.
//
// THE SAME FUNCTION FOR THE ASKER AND FOR EVERY PEER, so the asker's own share
// and a peer's are the same shape by construction rather than by two readers
// agreeing.
func answer(ctx context.Context, log *store.EventLog, q Question, params json.RawMessage, ids []string) (any, error) {
	decode := func(into any) error {
		if err := json.Unmarshal(params, into); err != nil {
			return fmt.Errorf("the %s parameters do not decode: %w", q, err)
		}
		return nil
	}
	switch q {
	case QuestionEvents:
		var p listParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return listPartOf(ctx, log, p.query())
	case QuestionSeries:
		var p seriesParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		h, err := log.Histogram(ctx, store.HistogramQuery{
			ListQuery: p.List.query(), Bucket: p.Bucket, At: p.At,
		})
		return h, err
	case QuestionEvent:
		var p idParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return eventPartOf(ctx, log, p.ID)
	case QuestionTrace:
		var p idParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return tracePartOf(ctx, log, p.ID)
	case QuestionTurn:
		var p idParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return turnPartOf(ctx, log, p.ID)
	case QuestionTurns:
		var p turnsParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return turnsPartOf(ctx, log, p.query(ids))
	case QuestionPhases:
		var p phasesParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		rows, err := log.Phases(ctx, p.Role, p.Limit, p.Before.cursor())
		if err != nil {
			return nil, err
		}
		return listPart{Rows: rows, Full: len(rows) >= phaseLimit(p.Limit)}, nil
	case QuestionSeatPhases:
		var p phasesParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		rows, err := log.AgentPhases(ctx, p.AgentID, p.Role, p.Before.cursor())
		if err != nil {
			return nil, err
		}
		return listPart{Rows: rows, Full: len(rows) >= store.AgentPhaseLimit}, nil
	case QuestionPhaseTokens:
		var p phaseTokenParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return spendPartOf(ctx, log, p.query())
	case QuestionTraceRows:
		var p traceRowsParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		rows, err := log.TraceRows(ctx, p.TraceIDs, p.Limit)
		if err != nil {
			return nil, err
		}
		return listPart{Rows: rows, Full: len(rows) >= p.Limit}, nil
	}
	return nil, fmt.Errorf("this node does not know the history question %q; "+
		"it is running an older build", q)
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
func fit(self string, part any, limit int) ([]byte, error) {
	encode := func(p any) ([]byte, error) {
		body, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("eventfan: encode an answer: %w", err)
		}
		return json.Marshal(reply{Version: Protocol, Node: self, Answer: body})
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
