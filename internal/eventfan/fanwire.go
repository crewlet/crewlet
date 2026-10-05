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
// reason: a rolling upgrade puts two builds on one broker. But an older peer
// IGNORES a field it does not know, and for this wire that is not harmless in
// two cases: a new FILTER, which the peer drops and answers a WIDER question
// than was asked — rows merged in as though they matched — and a new ANSWER
// field a merge sums, which the peer never sends and the merge reads as zero.
//
// So every request is stamped with the LOWEST version that answers it, on the
// tracker's rule for its records (internal/statelog's RecordFields): a
// question carrying nothing new goes out as v1 and every build answers it,
// and one that needs a newer field goes out at that field's version, which an
// older peer refuses by version rather than answering around. A node answers
// any version from 1 to its own [Protocol] and replies in the version it was
// asked in, and the asker names a peer that refused — or replied in another
// version — in the coverage rather than guessing at fields it cannot read.
// [versionOf] is the table: adding a filter or a summed answer field is adding
// a row there and moving [Protocol] to its version.

// Protocol is the highest scatter version this build speaks — never, on its
// own, the version it asks in; see [versionOf].
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
//   - v5: the `notification_outcomes` question — what became of the
//     notifications each third-party app delivered, counted over a window
//     the asker names — which the integrations answer used to take from the
//     newest page of notification events, whose span was its own. A NEW
//     QUESTION rather than a new field: a build before it does not know it,
//     so asked in v5 it refuses by version and is named in the coverage,
//     exactly as it names any question it cannot answer. Its parameters
//     carry the asker's instant from the start, so every build that answers
//     it floors the window there and nothing it counts needs holding (see
//     [Fleet.NotificationOutcomes]).
//
// ONE ADDITION MOVES NO VERSION: the asker's instant, `at`, on every
// question's parameters — the instant the question is asked at, which every
// node on this build floors the history at (see [store.EventLog]). A build
// that does not read it answers as of its own clock instead, and the builds
// before it did NOT all floor alike:
//
//   - every question but `event` was floored at that build's own clock, so
//     its answer differs from the asker's by the rows between two clocks'
//     thirty-day horizons — a strip as wide as the skew between them;
//   - `event`, one event by id, was not floored at all, so such a build
//     answers with any copy it still holds: the day retention keeps past the
//     horizon, and any age on a node whose sweep has lapsed.
//
// So THE ASKER HOLDS WHAT COMES BACK TO ITS OWN HORIZON wherever an answer
// carries rows with their instants ([heldTo]): one event, a listing and its
// trace siblings, a trace, a turn and both phase histories are cut at `at` −
// [store.EventHistory] before anything is merged, so no peer, whatever build it
// runs, can put a row past the horizon into an answer this build serves, and a
// dead link asked of a node on this build answers not found whatever its peers
// run. A link served by a node still on an earlier build is that build's
// answer, which holds no copy by id to any horizon, until that node is
// upgraded. What the asker CANNOT re-check is what arrives as a count,
// aggregated at the older build's own horizon: the axis's `by_category`, the
// turns a share folds (a page of turns' second scatter is floored at the
// history, not at the window), and a trace's or a turn's total beyond what it
// corrects by the rows it dropped. Those can carry the strip and nothing wider:
// a turn whose share reaches into it is held to start at the horizon rather
// than dropped as one that began before the window ([Fleet.Turns]). The cut
// leaves a capped trace or turn holding rows it did not send, after its last
// one, and the merge places nothing past that row ([MergeTrace], [MergeTurn]) —
// a view cut short and saying so, never one with a hole. The rest is bounded by
// edges every build honours: the axis's bars and totals lie inside the window
// every build cuts from `at` alike, whose first bar — the one the floor cuts,
// which is where an older build behind the asker's clock counts its strip — the
// asker drops after summing ([store.EventHistogram.InsideHistory]); the spend
// window and a page of turns name both edges as the asker's instants, and a
// turn's merged start is held to its window here.
//
// It is still not a filter that build would answer around, nor a summed
// field it would leave at zero — and a version would make that build REFUSE
// the whole question for the length of an upgrade, costing every row it
// holds to save that strip. The axis has carried its `at` since v1, for the
// window it cuts.
const Protocol = 5

// versionOf is the lowest scatter version that answers one question with
// these parameters.
//
// A HISTOGRAM IS ALWAYS AT LEAST v2, because its answer carries the failed
// split and a v1 peer would contribute bars with none — a sum that
// under-counts failures by exactly that node's share, with nothing to say so —
// and higher when its filters are. A listing is asked in its filters' version,
// so one narrowing by nothing new is still answered by the whole fleet during
// an upgrade. The company's phases narrowed to a seat are v3: an older peer
// reads only the role name the question used to carry, and would answer every
// seat's. A QUESTION is asked in the version that added it, whatever it
// carries: no earlier build can answer it at all.
func versionOf(q Question, params any) int {
	switch q {
	case QuestionNotificationOutcomes:
		return 5
	case QuestionSeries:
		if p, ok := params.(seriesParams); ok {
			return max(2, p.List.version())
		}
		return 2
	case QuestionEvents:
		if p, ok := params.(listParams); ok {
			return p.version()
		}
	case QuestionPhases:
		if p, ok := params.(phasesParams); ok && p.AgentID != "" {
			return 3
		}
	case QuestionTurns:
		// A PEER THAT READS ONLY `since_days` would answer the last week
		// for a one-hour bar three days ago — a page of the wrong turns,
		// every one of which the asker then has to throw away.
		if p, ok := params.(turnsParams); ok && (!p.Since.IsZero() || !p.Until.IsZero()) {
			return 3
		}
	}
	return 1
}

// version is the lowest scatter version that honours every filter set.
func (p listParams) version() int {
	switch {
	case p.Failed != nil:
		return 4
	case p.Suspended != nil:
		return 3
	case p.ChannelID != "" || p.AgentID != "":
		return 2
	}
	return 1
}

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
	// the node that decided it, so one node's count is its share of the
	// fleet's. v5 — see [Protocol].
	QuestionNotificationOutcomes Question = "notification_outcomes"
)

// Questions is the closed set.
var Questions = []Question{
	QuestionEvents, QuestionEvent, QuestionSeries, QuestionTrace, QuestionTurn,
	QuestionTurns, QuestionPhases, QuestionSeatPhases, QuestionTraceRows,
	QuestionPhaseTokens, QuestionNotificationOutcomes,
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

	// v2 — see [listParams.version].
	ChannelID string `json:"channel_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`

	// v3.
	Suspended *bool `json:"suspended,omitempty"`

	// v4.
	Failed *bool `json:"failed,omitempty"`

	// At is the asker's instant. Unversioned — see [Protocol].
	At time.Time `json:"at,omitzero"`
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
	// THE AXIS'S OWN FIELD, and the one place its instant travels: every
	// build since v1 reads it here to cut the window, so the listing's own
	// `at` is left off the filters this carries rather than sent twice.
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

	// At is the asker's instant — one for every read of a part. Unversioned;
	// see [Protocol].
	At time.Time `json:"at,omitzero"`
}

type traceRowsParams struct {
	TraceIDs []string `json:"trace_ids"`
	Limit    int      `json:"limit"`

	// At is the page's own instant, so the siblings are floored where the
	// matches were. Unversioned; see [Protocol].
	At time.Time `json:"at,omitzero"`
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

	// v3 — see [versionOf].
	Since time.Time `json:"since,omitzero"`
	Until time.Time `json:"until,omitzero"`

	// At is the instant the asker cut the window against, which every node
	// floors it at. Unversioned — see [Protocol].
	At time.Time `json:"at,omitzero"`
}

func turnsParamsOf(q store.TurnQuery) turnsParams {
	return turnsParams{
		Since: q.Since, Until: q.Until, At: q.At,
		SinceDays: q.SinceDays, AgentRole: q.AgentRole, AgentID: q.AgentID,
		Model: q.Model, WorkKey: q.WorkKey, WorkItem: q.WorkItem,
		Failed: q.Failed, Before: q.Before, Sort: q.Sort, Limit: q.Limit,
	}
}

func (p turnsParams) query(ids []string) store.TurnQuery {
	return store.TurnQuery{
		Since: p.Since, Until: p.Until, At: p.At,
		SinceDays: p.SinceDays, AgentRole: p.AgentRole, AgentID: p.AgentID,
		Model: p.Model, WorkKey: p.WorkKey, WorkItem: p.WorkItem,
		Failed: p.Failed, Before: p.Before, Sort: p.Sort, Limit: p.Limit,
		IDs: ids,
	}
}

// phasesParams is both phase questions' parameters. The company's phases read
// only AgentID (v3) and a seat's read both, since a seat's history is matched by
// either identifier its rows were written under.
type phasesParams struct {
	AgentID string      `json:"agent_id,omitempty"`
	Role    string      `json:"role,omitempty"`
	Limit   int         `json:"limit,omitempty"`
	Before  *cursorWire `json:"before,omitempty"`

	// At is the asker's instant. Unversioned; see [Protocol].
	At time.Time `json:"at,omitzero"`
}

// phaseTokenParams names the window as the ASKER's two instants, so every node
// cuts the same one rather than each counting back from its own clock — and
// the instant it was cut against, so every node floors it there too.
type phaseTokenParams struct {
	Since     time.Time `json:"since"`
	Until     time.Time `json:"until"`
	AgentRole string    `json:"role,omitempty"`
	Limit     int       `json:"limit,omitempty"`

	// At is unversioned — see [Protocol].
	At time.Time `json:"at,omitzero"`
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
//
// `at` IS NOT OMITTED WHEN ZERO, unlike every other question's: this question
// carried it from the version that added it, so it is part of the question
// rather than an unversioned addition, and the asker always sets it.
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
		if req.Version < 1 || req.Version > Protocol {
			return encodeError(self, fmt.Sprintf("this node speaks history protocol "+
				"up to v%d and was asked in v%d; it is running a different build", Protocol, req.Version))
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

// encodeError is a refusal, stamped with the HIGHEST version this node speaks:
// it answers nothing, so there is no asked version to echo, and the number is
// what tells the asker which build refused.
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
		return log.Histogram(ctx, p.query())
	case QuestionEvent:
		var p idParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return eventPartOf(ctx, log, p.ID, p.At)
	case QuestionTrace:
		var p idParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return tracePartOf(ctx, log, p.ID, p.At)
	case QuestionTurn:
		var p idParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return turnPartOf(ctx, log, p.ID, p.At)
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
		rows, more, err := log.Phases(ctx, p.AgentID, p.Limit, p.Before.cursor(), p.At)
		if err != nil {
			return nil, err
		}
		return listPart{Rows: rows, Full: more}, nil
	case QuestionSeatPhases:
		var p phasesParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		rows, more, err := log.AgentPhases(ctx, p.AgentID, p.Role, p.Before.cursor(), p.At)
		if err != nil {
			return nil, err
		}
		return listPart{Rows: rows, Full: more}, nil
	case QuestionPhaseTokens:
		var p phaseTokenParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return spendPartOf(ctx, log, p.query())
	case QuestionNotificationOutcomes:
		var p outcomeParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return log.NotificationOutcomes(ctx, p.query())
	case QuestionTraceRows:
		var p traceRowsParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		rows, err := log.TraceRows(ctx, p.TraceIDs, p.Limit, p.At)
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
