package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/named"

	"github.com/crewlet/crewlet/internal/tokens"
)

// ErrNotFound reports that a row a caller named by id does not exist.
var ErrNotFound = errors.New("store: not found")

const (
	// EventHistory is how far back the event log is read: the hard bottom of
	// paging, past which every page is empty. Named so the API can ship it to a
	// dashboard footer. Measured from the instant a read is ASKED AT (see
	// [EventLog]), never from a fresh clock read in each half of one answer.
	//
	// A READ THAT PINS NO INSTANT IS MEASURED FROM THE WALL CLOCK ([now]), so a
	// row's timestamp, the floor and the sweep agree on "now". A fixture writing
	// rows at an ABSOLUTE date and reading them unpinned fails for ever once the
	// window passes that date: anchor a fixture clock to time.Now, or pin the
	// read. Appending BELOW the floor is legal; its tests do so on purpose.
	EventHistory = 30 * 24 * time.Hour

	// EventRetention is how long rows are kept — one day past the read
	// floor, and never less than it.
	//
	// The direction matters more than the margin. Sweeping SHORTER than the
	// read floor deletes rows a reader can still ask for, so a page assembled
	// at the boundary loses rows underneath it. A day of slack is longer than
	// any paging session and three orders of magnitude longer than the
	// maintenance tick, so the sweep never races a reader.
	EventRetention = EventHistory + 24*time.Hour

	// EventPurgeBatch bounds how many rows one purge statement deletes, so
	// no single statement holds the writer for a whole backlog — see Purge
	// for why that matters to the inline event Append. Five hundred rows at
	// the fat end of real payloads (tens of KB of phase prompts each) is
	// 10–25 MB of pages per statement, tens of milliseconds of writer hold,
	// while a week-long overhang still clears in a couple hundred
	// statements within one maintenance tick. Exported for the contract
	// suite, which proves the sweep drains a backlog wider than one batch.
	EventPurgeBatch = 500

	// MaxTraceEvents caps one trace's rows. A trace is unbounded in
	// principle — a long turn with sub-agents accumulates thousands of
	// spans — and the whole thing goes out in a single WebSocket frame, so
	// an uncapped read is a query that times out client-side and reports as
	// a generic failure. The OLDEST rows are kept: the root is what
	// explains a trace, and a truncated tail is legible where a truncated
	// head is not.
	MaxTraceEvents = 500

	// DefaultListLimit is the page size when a caller names none.
	DefaultListLimit = 50

	// THERE IS NO OVER-FETCH ANY MORE. The RelatedAgent filter used to pull
	// five pages of raw history per page it wanted, capped at 500 rows, and
	// sift them in Go — because the thing it matched on lived in a JSON
	// blob no index covers. It is a join against an indexed party table
	// now (schema/0016), so the query returns the matches themselves and
	// there is nothing to sift.
)

// EventRecord is one row of the audit log.
//
// Payload is populated only by ByID. List and Trace deliberately leave it nil:
// they never select the column, because thirty days of serialized events is a
// large amount of JSON to move for a listing that shows a summary line.
type EventRecord struct {
	// THE TAGS ARE THE WIRE CONTRACT, and the names are the DASHBOARD's.
	//
	// /events, /events/{id} and /events/trace/{id} answer with this struct and
	// the client reads these names off it; a renamed tag blanks every one of
	// those screens without failing any.
	//
	// The names match livestate.FeedRow field for field: the same screens show
	// a live row from the projection and a historical one from here.
	ID       string    `json:"id"`
	Type     string    `json:"type"`
	Source   string    `json:"source"`
	Time     time.Time `json:"timestamp"`
	Category string    `json:"category"`
	Summary  string    `json:"summary"`
	// Refs is what Summary names (internal/named), off the row's refs
	// column (node/0069); absent where it names nothing.
	Refs         []named.Ref `json:"refs,omitempty"`
	Actor        string      `json:"actor"`
	TraceID      string      `json:"trace_id"`
	SpanID       string      `json:"span_id"`
	ParentSpanID string      `json:"parent_span_id"`

	// Tags are the filterable dimensions the writer extracted. The
	// promoted columns (agent_id, agent_role, task_id, channel_id, sender,
	// and since schema/0033 work_item) are copies of six of these; the rest
	// exist only here.
	Tags map[string]string `json:"tags,omitempty"`

	// WorkKey is the unit of work this row's run was an attempt at — see
	// ADR-0017 and [ListQuery.WorkKey].
	//
	// OFF THE COLUMN: one authority, and it is the column every other
	// work-key reader already uses (turnlist's grouping, the phase-token
	// rollup, the filter above), so a row read here and a row matched by
	// `/events?work_key=` can never disagree about which unit of work it
	// belongs to.
	WorkKey string `json:"work_key,omitempty"`

	// AgentID is the agent seat the event is about, when it is about one:
	// the derived id (ADR-0013), which a namesake does not share.
	// Named on the row rather than left for a reader to dig out of Tags
	// because a feed narrowed to one seat filters its LIVE rows the same way
	// the store filtered these, and livestate.FeedRow carries it under the
	// same name. A copy of the tag, as the promoted column is.
	AgentID string `json:"agent_id,omitempty"`

	// Node is the node that PUBLISHED the event — the envelope's origin
	// (events.Event.Node), which is where the work it describes ran — and
	// NOT the node whose store the row was read from. The two were one until
	// custody (ADR-0025): a node without the `data` role keeps no event log,
	// and the data node that keeps its events holds rows it never
	// published. So a reader asking where something happened asks the row,
	// never the store that answered. Empty only on a row no node published
	// (a test harness's queue). node/0054.
	Node string `json:"node,omitempty"`

	// Payload is the full serialized event. Nil on a listing — see above.
	Payload json.RawMessage `json:"payload,omitempty"`

	// Tokens and Calls are the row's total_tokens and calls columns, what a
	// spend record cost and how many model calls it made (a launch's are its
	// harness's rounds), so a listing of runs says both without its payload.
	Tokens int `json:"tokens,omitempty"`
	Calls  int `json:"calls,omitempty"`

	// Failed says whether the work this event reports failed. Derived on
	// read from the event type plus the stored `failed` tag, because a
	// listing never selects the payload and the tag is all that survives
	// into history.
	Failed bool `json:"failed"`

	// Spend is what one LLM call cost, present only on a spend record: a
	// phase completion, a collected launch or an auxiliary record.
	//
	// Promoted out of the payload and into columns because the rollup that
	// reads it is an AGGREGATION: it wants a dozen small values from every
	// row in a window, and reaching them through the payload meant hauling
	// each phase's whole prompt and response across the driver to decode
	// them in Go. See schema/0015.
	Spend *Spend `json:"spend,omitempty"`
}

// Spend is one LLM call's identity and its token cost.
//
// A pointer on [EventRecord] rather than flat fields: it is set on one event
// type out of dozens, and flattening it would put thirteen always-empty fields on
// every row the dashboard renders.
type Spend struct {
	// Phase is which phase ran; HostPhase is the phase a nested call ran
	// under, and Worker names the auxiliary worker when Phase is
	// "auxiliary" — which is why the worker rollup keys on the pair.
	Phase     string `json:"phase,omitempty"`
	HostPhase string `json:"host_phase,omitempty"`
	Worker    string `json:"worker,omitempty"`
	Model     string `json:"model,omitempty"`

	// TurnID and WorkKey are IDENTITY rather than cost, and they are here
	// because this type is the carrier for every promoted column — see
	// [EventLog.Append], which fills them for any event that names one and
	// not only for the spend records [SpendFor] reads. TurnID names one
	// RUN of a turn; WorkKey names the unit of work it was dispatched for,
	// which a re-run repeats and a run id does not. See ADR-0017.
	TurnID    string `json:"turn_id,omitempty"`
	WorkKey   string `json:"work_key,omitempty"`
	Iteration int    `json:"iteration,omitempty"`

	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`

	// CacheReadTokens and CacheWriteTokens are the share of InputTokens the
	// provider's prompt cache served and stored — a BREAKDOWN of the input,
	// never an addition to it (see tokens.Bucket). Columns since
	// schema/0032, for the reason the counts above are: the rollup reads
	// columns, so a count left in the payload is a count it reads as zero.
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`

	// ProviderKey is the configured provider entry that served the call,
	// as distinct from Model, the model that entry reported: a fallback
	// chain serves several models under one key. Model falls back to it
	// when a phase names no model; this is the key itself, never the
	// fallback. schema/0032.
	ProviderKey string `json:"provider_key,omitempty"`

	// Calls is how many PROVIDER CALLS the row covers — a phase's rounds,
	// an auxiliary record's coalesced calls — and zero on a row that is no
	// call. Stage is an auxiliary record's stage (`turn`, `reflection`, …),
	// empty on every other row. schema/0040.
	Calls int    `json:"calls,omitempty"`
	Stage string `json:"stage,omitempty"`
}

// Cursor is an exclusive keyset position: the reader holds this row and wants
// what comes before it.
//
// The ID half is not optional. (Time, ID) is the table's primary key and
// therefore unique; Time alone is not, because burst writes routinely share a
// timestamp at microsecond resolution. A cursor over a non-unique key skips or
// repeats whatever collided with it, and the reader who scrolled past the gap
// gets no error either way.
type Cursor struct {
	Time time.Time
	ID   string
}

// ListQuery selects a page of the event log. The zero value asks for the most
// recent DefaultListLimit events.
type ListQuery struct {
	Limit    int
	Type     string
	Source   string
	Category string
	TraceID  string
	Actor    string

	// ChannelID selects the events of one agent-to-agent conversation, by
	// the channel id every A2A event carries — the promoted `channel_id`
	// column (schema/0001), which is set on those events alone. It is what
	// a conversation's page asks the log, "what happened on this channel",
	// and it reads the partial index schema/0034 ships.
	ChannelID string

	// AgentID selects the events about ONE agent seat, by the id every node
	// derives for it ([org.DeriveAgentID], ADR-0013): the promoted, indexed
	// `agent_id` column. Not by NAME, which two seats may share. NOT
	// [ListQuery.RelatedAgent], which also matches events that merely name the
	// seat as a party and pulls in their traces.
	AgentID string

	// AnsweredBy selects the answers ONE person gave, by the `answered_by`
	// every record of an answer carries (executor_answered names who
	// answered an executor's question): the promoted column schema/0053
	// ships, and its partial index. It is what a person's answered
	// decisions ask ([types.ExecutorAnswered]): the answers they gave,
	// rather than every answer in the window read and kept in memory.
	AnsweredBy string

	// Suspended selects by whether a completion record PARKED its turn — the
	// `suspended` flag a turn's segment carries when it launched a detached
	// executor ([types.AgentTurnCompleted.Suspended]) — and nil means every
	// row. THREE-VALUED, like the turn list's `failed`: absent is the ordinary
	// case, so false with `Type: agent_turn_completed` is the turns that
	// ENDED and true the ones that parked.
	//
	// A turns axis counts over it: a turn that parks and resumes writes two
	// completion records, so the type alone draws it twice beside a list that
	// shows it once. Read from the payload, as [suspendedExpr] is (never a
	// column); the type filter narrows first.
	Suspended *bool

	// Failed selects by whether the event reports a failure — the rule
	// [EventRecord.Failed] is stamped by on every read ([types.Failed]: the
	// stored `failed` tag, or a type in the failure set), spelled once as
	// [failedRow] — and nil means every row. THREE-VALUED for the reason
	// Suspended is: absent is the ordinary case.
	//
	// A FILTER, not a mark the reader applies to the page it holds. The
	// event log's "Failures only" used to narrow the rows a tab had already
	// paged in, so the axis above it counted every event in the window
	// while the list beneath showed the failures among the newest hundred —
	// and every older page it fetched was the unfiltered log, most of which
	// the same mark then hid.
	Failed *bool

	// TurnID selects one RUN of a turn — every phase of it, its own
	// completion record, and the fallbacks and breaches that happened
	// inside it. A row whose event carries no turn_id field — a cron fire,
	// a trigger the dispatcher declined — writes an empty column and does
	// not answer this filter.
	TurnID string

	// WorkKey selects EVERY RUN of one unit of work — the attempts at a
	// trigger that was redelivered, which TurnID by construction cannot
	// ask for once it names one execution. Backed by the partial index
	// schema/0029 ships. See ADR-0017.
	WorkKey string

	// WorkItem selects every event on one work item, by its identity
	// across trackers — `<backend>:<id>`, [types.WorkItem.Ref] — and never by
	// its key, which a move rewrites. Every turn-level record carries the
	// item it was charged to, so this is "everything that happened on this
	// item": each turn's start, its phases, its completion, an executor it
	// launched. Backed by the partial index schema/0033 ships; the column
	// is filled from the same payload field as the row's `work_item` tag.
	WorkItem string

	// RelatedAgent is a broad filter, by an AGENT ID: the events that
	// involve that agent — its own, and every one naming it as a party
	// (see [DB.SetEventSeats]) — plus every event sharing a trace with one
	// of those, so the inbound webhook that caused the agent's work shows up
	// beside it.
	//
	// The trace siblings ride on the page they were found from, so a page
	// shorter than Limit does NOT mean history is exhausted here; only a
	// zero-row page does.
	RelatedAgent string

	// FeedOnly selects the rows the ACTIVITY FEED carries: every stored type
	// but the ones [events.Unfed] keeps out of it — a related agent's trace
	// siblings included. It is what every read that is merged with or drawn
	// beside the live ring asks: the live projection's startup seed, which
	// rebuilds the feed from the store and must fill it with what the stream
	// would have put there, and the activity views' older pages and axes,
	// which a reader scrolls on from the ring. Filtered in the read rather
	// than afterwards, because a page of the newest rows filtered afterwards
	// would hand back fewer than it asked for whenever a burst of accounting
	// rows was newest. [EventLog.Histogram] counts through the same
	// predicate, so a feed-only bar counts what a feed-only page lists.
	FeedOnly bool

	// Since and Until bound the window a caller is asking about, as a
	// half-open interval `[Since, Until)`.
	//
	// SEPARATE FROM [ListQuery.Before], a CURSOR that moves with every page:
	// folding a window into it would make the second page of a bounded read
	// unbounded. The history window applies underneath; saying a `Since`
	// older than [EventHistory] reaches nothing is the caller's job. Zero
	// means unbounded on that side, not midnight in 1970.
	Since time.Time
	Until time.Time

	// Before is an exclusive cursor. Nil starts at the newest row.
	Before *Cursor

	// At is the instant the read is asked at: the history floor sits
	// [EventHistory] before it, and on an axis ([HistogramQuery]) it is also
	// what an unbounded top edge means and what the window is cut against.
	// Zero is now. A listing's unbounded top edge stays unbounded.
	//
	// A FIELD, not each log's own clock: a fleet asks several logs ONE question
	// (internal/eventfan) and sums bars index by index, so skewed clocks would
	// snap to windows one bar apart. It also pins the floor, so a peer reading
	// a past At drops no row between its floor and the asker's: they are still
	// on disk ([EventRetention] keeps a day past the floor).
	At time.Time
}

// at is the instant the read is asked at — read ONCE per answer, because an
// unpinned query's is the clock.
func (q ListQuery) at() time.Time { return askedAt(q.At) }

// EventLog is the audit and observability event store.
//
// EVERY LIST READ ANSWERS AN ALLOCATED SLICE, never nil (`null` crashes a
// client); [EventLog.AgentPhases] is nil for an unnamable seat. EVERY READ IS
// ASKED AT AN INSTANT (`At`, or the last argument `at`; zero is now), so a
// fleet floors at the ASKER'S instant. A GROUPED READ SELECTS IN A DERIVED
// TABLE so the planner seeks the filter's index, not the primary key's floor
// range ([ListQuery.facetSQL], [turnTracesSQL], [TurnQuery.partialsSQL],
// [OutcomeQuery.countSQL]; the bars, [HistogramQuery.barsSQL], seek as they
// stand): TestEveryGroupedReadSeeksItsFiltersIndex. A PAGE OF TURNS IS READ
// FROM ITS NEWEST EDGE, a span at a time ([EventLog.pageOfTurns]).
type EventLog struct{ db *DB }

// Events returns the audit log backed by this database.
func (d *DB) Events() *EventLog { return &EventLog{db: d} }

// EventSnapshot is the event log read at ONE SNAPSHOT: every read on it sees
// the log exactly as the others do, whatever is written or settled between
// them.
//
// For a reader that assembles one answer from several reads — a trace's rows,
// how many it holds and which of them are unsettled; a turn's opening, its
// count, its ending, its traces and its unsettled rows. Read one statement at a
// time, a custody batch settled between two of them is counted by one and named
// by the other, and the answer counts it twice or not at all (see unsettled.go).
// Read-only by construction: it carries reads and nothing else, so nothing
// written inside one is mistaken for part of the snapshot it was read at.
type EventSnapshot struct{ q querier }

// Snapshot runs fn against one snapshot of the log. fn may run more than once —
// a read that met a transient conflict is retried on a fresh snapshot — so it
// builds its answer afresh each time.
func (l *EventLog) Snapshot(ctx context.Context, fn func(EventSnapshot) error) error {
	return l.db.Read(ctx, func(tx *sql.Tx) error { return fn(EventSnapshot{q: tx}) })
}

// live is the log read one statement at a time, each at its own snapshot —
// what every read answered by a single statement needs and no more.
func (l *EventLog) live() EventSnapshot { return EventSnapshot{q: l.db.sql} }

// Unsettled is the rows of one trace or one turn — `column` is `trace_id` or
// `turn_id` — inside the history at `at` that this node holds of a custody
// batch it has written and not settled: the rows a count of the trace's or the
// turn's extent leaves to the asker (see unsettled.go). Read at this snapshot,
// beside the count it is taken out of. Never nil.
func (s EventSnapshot) Unsettled(ctx context.Context, column, id string, at time.Time) ([]UnsettledRow, error) {
	switch column {
	case "turn_id", "trace_id":
	default:
		return nil, fmt.Errorf("store: unsettled rows: %q is not a countable column", column)
	}
	rows, err := readUnsettled(ctx, s.q, []string{column + " = ?", "event_time >= ?"},
		[]any{id, EncodeTime(askedAt(at).Add(-EventHistory))})
	if err != nil {
		return nil, fmt.Errorf("store: unsettled rows of %s %s: %w", column, id, err)
	}
	return rows, nil
}

const eventInsertSQL = `
INSERT INTO crewlet_events (
	event_time, event_id, event_type, source, category,
	trace_id, span_id, parent_span_id,
	agent_id, agent_role, task_id, channel_id, sender,
	summary, refs, actor, tags, payload,
	phase, host_phase, worker, model, turn_id, work_key, iteration,
	input_tokens, output_tokens, total_tokens,
	cache_read_tokens, cache_write_tokens, provider_key, work_item,
	calls, spend_stage, answered_by, node,
	read_repo_id, read_principal_kind, read_principal, read_via, read_seat, read_bytes, read_reads
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (event_time, event_id) DO NOTHING`

// ErrIncompleteRecord reports a record missing part of its identity.
var ErrIncompleteRecord = errors.New("store: incomplete event record")

// Append writes one record, idempotently.
//
// ON CONFLICT DO NOTHING rather than an error, because the duplicate is
// expected: a publish retry, a replay, a redelivery after a node died all
// present the same (time, id) pair. Raising instead would make every one of
// them log a write failure that describes nothing wrong.
func (l *EventLog) Append(ctx context.Context, rec EventRecord) error {
	row, err := prepareEvent(rec, l.db.eventSeatsNow())
	if err != nil {
		return err
	}
	if err := l.db.Tx(ctx, func(tx *sql.Tx) error {
		return row.insert(ctx, tx)
	}); err != nil {
		return fmt.Errorf("store: append event %s: %w", rec.ID, err)
	}
	return nil
}

// eventRow is a record made ready for its insert — its identity checked, its
// tags encoded, its spend derived and its parties resolved — so the insert
// itself can run inside whichever transaction the caller holds: its own
// ([EventLog.Append]), or one that writes a whole custody batch
// ([EventLog.WriteCustody]).
type eventRow struct {
	rec     EventRecord
	tags    map[string]string
	tagJSON string
	// refs is the record's Refs as the column holds them: JSON, or "" for none.
	refs    string
	payload string
	spend   Spend
	read    repoRead
	parties []string

	// origins are the objects the record says started its turn
	// ([TurnOrigins]): written beside it into crewlet_turn_origins, the
	// index "which turn did this message start" seeks. See node/0059.
	origins []string
}

// prepareEvent readies one record for its insert, or refuses it. seats is what
// a handle the record names resolves to an agent id through ([partiesOf]): the
// writing node's, which is the custody keeper's for a batch it writes for a
// stateless node — the chart both run is the company's, so the two resolve one
// handle to one id.
func prepareEvent(rec EventRecord, seats SeatResolver) (eventRow, error) {
	// The identity is checked here because SQL cannot check it. NOT NULL
	// catches a missing column; it does not catch a zero one, and both
	// halves of this key have a zero value that stores fine and then reads
	// as nothing: a zero Time lands in year 1, permanently below the read
	// floor, and an empty ID collides with every other record that forgot
	// the same field. Either way the row exists and no query returns it.
	if rec.ID == "" {
		return eventRow{}, fmt.Errorf("%w: no event id", ErrIncompleteRecord)
	}
	if rec.Time.IsZero() {
		return eventRow{}, fmt.Errorf("%w: event %s has no timestamp", ErrIncompleteRecord, rec.ID)
	}
	tags := rec.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	tagJSON, err := json.Marshal(tags)
	if err != nil {
		return eventRow{}, fmt.Errorf("store: encode event tags (%s): %w", rec.ID, err)
	}
	payload := rec.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	// DERIVED HERE WHEN THE CALLER DID NOT SET IT: a hand-built record with
	// only a payload would otherwise write a phase completion with zero spend
	// columns, and a rollup would report a company that spent nothing. The
	// normal path is observe.Record, which sets Spend from bytes it already
	// has, so this fallback is the exception.
	//
	// The zero value writes what the column defaults would, so every non-phase
	// row is unaffected.
	var spend Spend
	switch {
	case rec.Spend != nil:
		spend = *rec.Spend
	default:
		if derived := SpendFor(rec.Type, payload); derived != nil {
			spend = *derived
		}
	}
	// EVERY event that names a turn gets the column, not just the phase
	// completions [SpendFor] is scoped to: that reads one event type for SPEND,
	// but reading one turn means every row it touched (a tool skill loaded, a
	// refused call, `executor_launched` carry a turn id and no token count).
	// Only this identifier is copied, so a non-phase row cannot acquire a
	// phase's numbers.
	//
	// THIS CANNOT give a turn id to an event that carries no `turn_id` FIELD
	// (the promoted TAG is read off it): a cron fire, a declined trigger, or an
	// event published where no single turn is right to name writes an empty
	// column and cannot be read back by turn. The Turn screen's bands are held
	// against that in internal/events/types/turnbands_client_test.go.
	if spend.TurnID == "" {
		spend.TurnID = tags["turn_id"]
	}
	// THE UNIT OF WORK BESIDE THE RUN, for the same reason and by the same
	// route. A turn that fails without acting is redelivered, so one
	// trigger legitimately runs several times: turn_id tells the attempts
	// apart and this is what still groups them. See ADR-0017.
	if spend.WorkKey == "" {
		spend.WorkKey = tags["work_key"]
	}
	return eventRow{rec: rec, tags: tags, tagJSON: string(tagJSON), refs: refsColumn(rec.Refs),
		payload: string(payload), spend: spend, read: repoReadFor(rec.Type, payload),
		parties: partiesOf(tags, seats),
		origins: TurnOrigins(rec.Type, payload)}, nil
}

// repoRead is what a repository read's columns hold (node/0061, and node/0062
// its count): who read which repository, how, how many times and what they
// were sent — the fields the usage day groups and sums a day of reads by
// ([DB.usageRepoReads]), filled when the row is written so the derivation,
// paid on every tick its day moves, reads no JSON. The zero value writes the
// columns' defaults, which is every other row's.
type repoRead struct {
	repo, kind, principal, via, seat string
	bytes, reads                     int
}

// repoReadFor is a row's repository read: a `code_fetched` record's fields,
// and the zero value for every other type. Read from the payload's JSON keys,
// as [ExtractTags] reads its, so a newer peer's record carrying a value this
// build does not know is counted as written rather than refused.
func repoReadFor(eventType string, payload []byte) repoRead {
	if eventType != repoReadType {
		return repoRead{}
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		return repoRead{}
	}
	return repoRead{
		repo: jsonString(body["repo_id"]), kind: jsonString(body["principal_kind"]),
		principal: jsonString(body["principal"]), via: jsonString(body["via"]),
		seat: jsonString(body["seat"]), bytes: jsonInt(body["bytes"]), reads: jsonInt(body["reads"]),
	}
}

// insert writes the row and its party rows inside tx.
//
// IN ONE TRANSACTION with its party rows, because the party table is an INDEX
// of this one and an index that can be missing entries is not an index: an
// event stored without its parties is invisible to the filter that reads them,
// permanently and with nothing to say so. The cost is a begin and a commit on
// a path that runs a handful of times a second.
func (r eventRow) insert(ctx context.Context, tx *sql.Tx) error {
	rec, tags, spend, read := r.rec, r.tags, r.spend, r.read
	if _, err := tx.ExecContext(ctx, eventInsertSQL,
		EncodeTime(rec.Time), rec.ID, rec.Type, rec.Source, rec.Category,
		rec.TraceID, rec.SpanID, rec.ParentSpanID,
		tags["agent_id"], tags["agent_role"], tags["task_id"],
		tags["channel_id"], tags["sender"],
		rec.Summary, r.refs, rec.Actor, r.tagJSON, r.payload,
		spend.Phase, spend.HostPhase, spend.Worker, spend.Model,
		spend.TurnID, spend.WorkKey, spend.Iteration,
		spend.InputTokens, spend.OutputTokens, spend.TotalTokens,
		spend.CacheReadTokens, spend.CacheWriteTokens, spend.ProviderKey,
		// THE ITEM THE ROW IS ON, off the tag [ExtractTags] composes
		// from the nested `work_item` object — a copy of a tag, like
		// agent_id's beside it, so the column and the tag can never
		// name two different items. See schema/0033.
		tags["work_item"],
		spend.Calls, spend.Stage,
		// WHO ANSWERED, off the `answered_by` tag, for the reason
		// work_item is a copy of its own. See schema/0053.
		tags["answered_by"],
		// WHO PUBLISHED IT, off the record rather than off this node:
		// a custody keeper writes rows another node published. See
		// node/0054.
		rec.Node,
		read.repo, read.kind, read.principal, read.via, read.seat, read.bytes, read.reads,
	); err != nil {
		return err
	}
	for _, party := range r.parties {
		if _, err := tx.ExecContext(ctx, partyInsertSQL,
			party, EncodeTime(rec.Time), rec.ID); err != nil {
			return err
		}
	}
	// WHAT STARTED THE TURN, in the same transaction for the party index's
	// reason: an index that can miss entries is not one. A record naming no
	// turn starts nothing a reader could open.
	if spend.TurnID != "" {
		for _, origin := range r.origins {
			if _, err := tx.ExecContext(ctx, originInsertSQL,
				origin, spend.TurnID, EncodeTime(rec.Time), rec.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

const originInsertSQL = `
INSERT INTO crewlet_turn_origins (origin, turn_id, event_time, event_id) VALUES (?, ?, ?, ?)
ON CONFLICT (origin, turn_id, event_time, event_id) DO NOTHING`

// partyInsertSQL records one (party, event) pair. Idempotent for the same
// reason the event insert is: a publish retry replays the whole append.
const partyInsertSQL = `
INSERT INTO crewlet_event_parties (party, event_time, event_id) VALUES (?, ?, ?)
ON CONFLICT (party, event_time, event_id) DO NOTHING`

// SeatResolver answers the AGENT id of the seat a handle names, and false for a
// handle no agent seat answers to, which is a human seat, a stranger or
// nobody.
type SeatResolver func(handle string) (agentID string, ok bool)

// SetEventSeats installs what the event log resolves a handle an event names
// through, to file that event under the agent it involves. Nil files every
// event under its own agent alone.
//
// The store cannot compute the answer: an agent id is a UUIDv5 over the
// company's name and the seat's handle (ADR-0013), and whether a handle names an
// agent seat is the org's to say. The engine installs one reading its live
// epoch, so the row is keyed on what the chart said when the event was written.
// A handle that is not open takes it as nothing.
func (d *DB) SetEventSeats(seats SeatResolver) {
	if d == nil {
		return
	}
	if seats == nil {
		d.eventSeats.Store(nil)
		return
	}
	d.eventSeats.Store(&seats)
}

// eventSeatsNow is the installed resolver, or nil.
func (d *DB) eventSeatsNow() SeatResolver {
	if d == nil {
		return nil
	}
	if held := d.eventSeats.Load(); held != nil {
		return *held
	}
	return nil
}

// partiesOf names every agent an event involves, by AGENT ID, deduplicated.
//
// IDs, never names: two seats may share a name. The parties are the event's own
// `agent_id` and each seat a participant tag names, resolved through seats.
// [seatTagKeys] name seats on every event; `sender` only on an A2A channel's own
// records (carrying `channel_id`), where it is the seat that spoke. On an
// external notification it is a vendor-side name that, resolved as a handle,
// would file a stranger's message under whichever seat shares it.
func partiesOf(tags map[string]string, seats SeatResolver) []string {
	seen := make(map[string]struct{}, len(seatTagKeys)+2)
	var out []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	add(tags["agent_id"])
	if seats == nil {
		return out
	}
	resolve := func(handle string) {
		if handle == "" {
			return
		}
		if id, ok := seats(handle); ok {
			add(id)
		}
	}
	for _, key := range seatTagKeys {
		resolve(tags[key])
	}
	if tags["channel_id"] != "" {
		resolve(tags["sender"])
	}
	return out
}

// listColumns is every column a listing reads. `payload` is absent
// deliberately — see EventRecord.Payload.
const listColumns = `event_time, event_id, event_type, source, category,
	summary, refs, actor, trace_id, span_id, parent_span_id, tags, work_key, node, total_tokens, calls`

// qualifiedListColumns names the same columns on the log's own table.
//
// Needed only when the query joins: every one of these names exists on
// crewlet_events alone, except event_time and event_id, which the party table
// carries too — so an unqualified select over the join is ambiguous and the
// engine refuses it.
func qualifiedListColumns(joined bool) string {
	if !joined {
		return listColumns
	}
	parts := strings.Split(strings.ReplaceAll(listColumns, "\n\t", ""), ", ")
	for i, col := range parts {
		parts[i] = "crewlet_events." + strings.TrimSpace(col)
	}
	return strings.Join(parts, ", ")
}

// predicate is the FROM, WHERE terms and arguments a query's filters compile to:
// what the caller ASKED FOR, nothing about where a page resumes.
//
// SHARED by [EventLog.List] and [EventLog.Histogram], two halves of one screen:
// a bar counting rows the list would not show is worse than no bar. The cursor
// is NOT here, or a histogram would change as somebody scrolled. `col`
// qualifies every column, not just the two the party table shares.
// `at` is the instant asked at, the HISTORY FLOOR [EventHistory] before it: a
// PARAMETER, not a clock read here, so every compilation of one answer floors
// at the same instant.
func (q ListQuery) predicate(at time.Time) (from string, where []string, args []any, col func(string) string) {
	// The RelatedAgent filter is a JOIN rather than another WHERE clause,
	// because "involves this agent" is one fact spread over five places on
	// the event, and the party table is where it was normalised to. The
	// join is two index seeks — the party's covering index, then the log's
	// primary key — so the work scales with the number of MATCHES rather
	// than with the size of the log. See schema/0016.
	joined := q.RelatedAgent != ""
	col = func(name string) string {
		if joined {
			return "crewlet_events." + name
		}
		return name
	}

	where = []string{col("event_time") + " >= ?"}
	args = []any{EncodeTime(at.Add(-EventHistory))}
	addEq := func(name, val string) {
		if val != "" {
			where = append(where, col(name)+" = ?")
			args = append(args, val)
		}
	}
	// A COLUMN WHOSE INDEX IS PARTIAL takes its index's own predicate as a
	// second term, which is redundant to the answer and is the only thing
	// that lets the planner use the index at all: it proves a query implies
	// `x <> ''` only when the query SAYS so, never from `x = ?` with a bound
	// value it cannot see. Without the term every one of these filters
	// walked the primary key newest-first until it had a page — for an item,
	// a unit of work or a channel with fewer rows than a page, the whole
	// thirty-day log (measured with EXPLAIN QUERY PLAN; see
	// TestEveryPartiallyIndexedFilterSeeksItsIndex).
	addIndexed := func(name, val string) {
		if val != "" {
			where = append(where, col(name)+" = ?", col(name)+" <> ''")
			args = append(args, val)
		}
	}
	addEq("event_type", q.Type)
	addEq("source", q.Source)
	addEq("category", q.Category)
	addEq("trace_id", q.TraceID)
	addEq("actor", q.Actor)
	addEq("agent_id", q.AgentID)
	addEq("turn_id", q.TurnID)
	addIndexed("work_key", q.WorkKey)
	addIndexed("work_item", q.WorkItem)
	addIndexed("channel_id", q.ChannelID)
	addIndexed("answered_by", q.AnsweredBy)
	if q.Suspended != nil {
		// [suspendedExpr]'s own rule, qualified for whichever FROM this is.
		where = append(where, "COALESCE(json_extract("+col("payload")+", '$.suspended'), 0) = ?")
		flag := 0
		if *q.Suspended {
			flag = 1
		}
		args = append(args, flag)
	}
	if q.Failed != nil {
		expr, failedArgs := failedRow(col)
		if !*q.Failed {
			expr = "NOT " + expr
		}
		where = append(where, expr)
		args = append(args, failedArgs...)
	}
	if unfed := events.Unfed(); q.FeedOnly && len(unfed) > 0 {
		where = append(where, col("event_type")+" NOT IN ("+
			strings.TrimSuffix(strings.Repeat("?,", len(unfed)), ",")+")")
		for _, name := range unfed {
			args = append(args, name)
		}
	}
	// THE WINDOW, half-open, on the same column the keyset walks — so it
	// narrows the index range the read already scans rather than adding a
	// term the planner has to filter on.
	if !q.Since.IsZero() {
		where = append(where, col("event_time")+" >= ?")
		args = append(args, EncodeTime(q.Since))
	}
	if !q.Until.IsZero() {
		where = append(where, col("event_time")+" < ?")
		args = append(args, EncodeTime(q.Until))
	}

	from = "crewlet_events"
	if joined {
		from = `crewlet_events JOIN crewlet_event_parties
			ON crewlet_event_parties.event_time = crewlet_events.event_time
			AND crewlet_event_parties.event_id = crewlet_events.event_id`
		where = append(where, "crewlet_event_parties.party = ?")
		args = append(args, q.RelatedAgent)
	}
	return from, where, args, col
}

// List returns a page of events, newest first, ordered by (time, id)
// descending.
func (l *EventLog) List(ctx context.Context, q ListQuery) ([]EventRecord, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	// ONE INSTANT FOR THE PAGE AND ITS SIBLINGS, so a related-agent page is
	// one set floored once: the siblings read the clock a query later, and a
	// trace straddling the floor kept its direct matches on the page while
	// the rows beside them that had just crossed it went missing.
	at := q.at()
	query, args := q.listSQL(limit, at)
	out, err := l.scanRows(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if q.RelatedAgent != "" {
		siblings, err := l.traceSiblings(ctx, out, limit, at)
		if err != nil {
			return nil, err
		}
		if q.FeedOnly {
			// A TRACE'S SIBLINGS CARRY NONE OF THE PAGE'S FILTERS — they
			// are the cause beside the effect — but a feed-only page is
			// the feed's rows, and a turn's trace holds every one of its
			// accounting records.
			siblings = slices.DeleteFunc(siblings, func(r EventRecord) bool {
				return events.KeptOutOfFeed(r.Type)
			})
		}
		out = mergeRelated(out, siblings, limit)
	}
	return out, nil
}

// listSQL is the statement [EventLog.List] runs for one page, and its
// arguments — a function of its own so the plan a filter gets can be read
// back for exactly the statement that runs (see
// TestEveryPartiallyIndexedFilterSeeksItsIndex). `at` is the instant the
// page is asked at; see [ListQuery.predicate].
func (q ListQuery) listSQL(limit int, at time.Time) (string, []any) {
	from, where, args, col := q.predicate(at)
	joined := q.RelatedAgent != ""

	if q.Before != nil {
		// Keyset, not OFFSET: (event_time, event_id) is the primary key,
		// so it is unique and already in index order — no sort node, and
		// no drift as new rows land at the head while a reader pages
		// backwards.
		if q.Before.ID != "" {
			where = append(where,
				"("+col("event_time")+", "+col("event_id")+") < (?, ?)")
			args = append(args, EncodeTime(q.Before.Time), q.Before.ID)
		} else {
			where = append(where, col("event_time")+" < ?")
			args = append(args, EncodeTime(q.Before.Time))
		}
	}
	args = append(args, limit)

	// Every fragment joined into `where` is a compile-time constant; each
	// one's value travels as a bound parameter in args.
	query := "SELECT " + qualifiedListColumns(joined) +
		" FROM " + from + " WHERE " + strings.Join(where, " AND ") +
		" ORDER BY " + col("event_time") + " DESC, " + col("event_id") + " DESC LIMIT ?"
	return query, args
}

// traceSiblings fetches the other events in the traces a page of direct
// matches belongs to.
//
// A SECOND QUERY rather than a wider first one: an agent's work is CAUSED by
// something (a webhook, a notification, a schedule tick) that names the agent
// nowhere. Pulling in everything sharing a trace with a direct match puts the
// cause next to the effect, at one trace-index seek per trace on the page.
//
// Floored at `at`, the instant the page itself was read at.
func (l *EventLog) traceSiblings(ctx context.Context, direct []EventRecord, limit int, at time.Time) ([]EventRecord, error) {
	return l.TraceRows(ctx, TraceIDsOf(direct), limit, at)
}

// TraceRows is the newest rows of any of these traces, newest first, up to
// limit — the sibling half of [ListQuery.RelatedAgent], asked on its own.
//
// Exported for the one caller that has to ask it SEPARATELY: a fleet, whose
// related-agent page is merged from several logs before anybody knows which
// traces it holds, and whose siblings sit in logs that held no direct match at
// all — the inbound webhook row lives on the node the delivery reached, and
// the agent work it caused on the node that holds the seat. Floored at `at`
// (see [EventLog]) — the page's own instant, so the siblings are the same
// window as the matches they sit beside.
func (l *EventLog) TraceRows(ctx context.Context, traceIDs []string, limit int, at time.Time) ([]EventRecord, error) {
	if len(traceIDs) == 0 {
		return []EventRecord{}, nil
	}
	if limit <= 0 {
		limit = DefaultListLimit
	}
	args := make([]any, 0, len(traceIDs)+2)
	for _, id := range traceIDs {
		args = append(args, id)
	}
	args = append(args, EncodeTime(askedAt(at).Add(-EventHistory)), limit)
	query := "SELECT " + listColumns + " FROM crewlet_events WHERE trace_id IN (?" +
		strings.Repeat(",?", len(traceIDs)-1) +
		") AND event_time >= ? ORDER BY event_time DESC, event_id DESC LIMIT ?"
	return l.scanRows(ctx, query, args...)
}

// TraceIDsOf is the distinct non-empty traces of some rows, in first-seen
// order.
func TraceIDsOf(rows []EventRecord) []string {
	out := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, rec := range rows {
		if rec.TraceID == "" {
			continue
		}
		if _, dup := seen[rec.TraceID]; dup {
			continue
		}
		seen[rec.TraceID] = struct{}{}
		out = append(out, rec.TraceID)
	}
	return out
}

// mergeRelated folds the siblings into the direct matches, newest first.
func mergeRelated(direct, siblings []EventRecord, limit int) []EventRecord {
	seen := make(map[string]struct{}, len(direct)+len(siblings))
	out := make([]EventRecord, 0, len(direct)+len(siblings))
	for _, group := range [][]EventRecord{direct, siblings} {
		for _, rec := range group {
			if _, dup := seen[rec.ID]; dup {
				continue
			}
			seen[rec.ID] = struct{}{}
			out = append(out, rec)
		}
	}
	// The sibling pass appends in its own scan order, so the merged list is
	// no longer the newest-first order each query guaranteed. Same
	// (time, id) tiebreak as the queries, for the same reason.
	slices.SortStableFunc(out, func(a, b EventRecord) int {
		// BOTH KEYS DESCEND: newest instant first, and within one instant
		// the higher id first, so a page boundary falls in the same place
		// on every read.
		return cmp.Or(b.Time.Compare(a.Time), cmp.Compare(b.ID, a.ID))
	})
	return truncate(out, limit)
}

// Trace returns every event in a trace, OLDEST first, because a trace is read
// as a causal sequence rather than a feed. A caller that gets exactly
// MaxTraceEvents rows should say the view is truncated — and asks
// [EventLog.TraceEventCount] at the SAME `at`, or the count is floored
// elsewhere than the rows it counts.
func (l *EventLog) Trace(ctx context.Context, traceID string, at time.Time) ([]EventRecord, error) {
	return l.live().Trace(ctx, traceID, at)
}

// Trace is [EventLog.Trace] read at this snapshot.
func (s EventSnapshot) Trace(ctx context.Context, traceID string, at time.Time) ([]EventRecord, error) {
	return s.scanRows(ctx,
		"SELECT "+listColumns+" FROM crewlet_events "+
			"WHERE trace_id = ? AND event_time >= ? "+
			"ORDER BY event_time ASC, event_id ASC LIMIT ?",
		traceID, EncodeTime(askedAt(at).Add(-EventHistory)), MaxTraceEvents)
}

// Turn returns every event of one turn, OLDEST first.
//
// Ordered like a trace, forwards. A DIFFERENT set from the trace: one trace can
// span several turns, and a turn resumed on another node several traces.
//
// A caller that gets exactly MaxTurnEvents rows should say the view is
// truncated and read [EventLog.TurnClosing] beside it: a forwards cut loses the
// turn's ENDING. The cap is the trace's.
//
// Every read of one turn's view takes the SAME `at`, or a row crossing the
// floor between two reads makes the count come back short of its rows.
func (l *EventLog) Turn(ctx context.Context, turnID string, at time.Time) ([]EventRecord, error) {
	return l.live().Turn(ctx, turnID, at)
}

// Turn is [EventLog.Turn] read at this snapshot.
func (s EventSnapshot) Turn(ctx context.Context, turnID string, at time.Time) ([]EventRecord, error) {
	return s.scanPayloads(ctx,
		"SELECT "+listColumns+", payload FROM crewlet_events "+
			"WHERE turn_id = ? AND event_time >= ? "+
			"ORDER BY event_time ASC, event_id ASC LIMIT ?",
		turnID, EncodeTime(askedAt(at).Add(-EventHistory)), MaxTurnEvents)
}

// MaxTurnEvents bounds one turn's read.
const MaxTurnEvents = MaxTraceEvents

// TurnEventCount is how many rows one turn has in the window, whatever a
// capped read of it returned.
//
// Because "did the read reach the end" is NOT `len(rows) == cap`, and that
// inference is wrong on the one boundary it is asked about most: a turn of
// exactly MaxTurnEvents rows holds every row it has and would be reported
// cut. A view that recovers the turn's ending beside its opening widens the
// wrong answer rather than narrowing it — a turn a little past the cap ends
// up whole on the page under a banner saying part of it is missing — so the
// question is asked rather than guessed at, and only on the reads that
// filled. It is a range scan of the same (turn_id, event_time, event_id)
// index the read walked.
func (l *EventLog) TurnEventCount(ctx context.Context, turnID string, at time.Time) (int, error) {
	return l.live().TurnEventCount(ctx, turnID, at)
}

// TurnEventCount is [EventLog.TurnEventCount] read at this snapshot.
func (s EventSnapshot) TurnEventCount(ctx context.Context, turnID string, at time.Time) (int, error) {
	return s.countEvents(ctx, "turn_id", turnID, at)
}

// TurnTraces is every trace one turn touched, in the order it first touched
// them.
//
// ASKED RATHER THAN DERIVED FROM THE ROWS, because of the cap above: a turn
// RESUMED on another node spans more than one trace, and a caller deriving the
// set from a capped read loses any trace whose events fell in the dropped
// middle, with nothing saying a second half exists.
//
// A DISTINCT walk of the same (turn_id, event_time, event_id) index, bounded by
// the same history window, a seek over one turn's range through a derived table
// (see [EventLog] and [turnTracesSQL]). Ordered by first appearance: the trace
// the turn started under comes first.
func (l *EventLog) TurnTraces(ctx context.Context, turnID string, at time.Time) ([]TurnTrace, error) {
	return l.live().TurnTraces(ctx, turnID, at)
}

// TurnTraces is [EventLog.TurnTraces] read at this snapshot.
func (s EventSnapshot) TurnTraces(ctx context.Context, turnID string, at time.Time) ([]TurnTrace, error) {
	query, args := turnTracesSQL(turnID, at)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read the traces of turn %s: %w", turnID, err)
	}
	defer func() { _ = rows.Close() }()

	out := []TurnTrace{}
	for rows.Next() {
		var id string
		var first int64
		if err := rows.Scan(&id, &first); err != nil {
			return nil, fmt.Errorf("store: scan the traces of turn %s: %w", turnID, err)
		}
		out = append(out, TurnTrace{TraceID: id, FirstAt: DecodeTime(first)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the traces of turn %s: %w", turnID, err)
	}
	return out, nil
}

// turnTracesSQL is the statement [EventLog.TurnTraces] runs and its arguments,
// a function of its own so its plan can be read back for exactly the statement
// that runs (TestEveryGroupedReadSeeksItsFiltersIndex).
//
// THE TURN'S ROWS ARE SELECTED IN A DERIVED TABLE and grouped outside it. Over
// the table itself this GROUP BY was planned as a range of the primary key from
// the history floor up — every row of the thirty-day log, on every turn page,
// on every node — under a doc comment promising a seek.
func turnTracesSQL(turnID string, at time.Time) (string, []any) {
	return "SELECT trace_id, MIN(event_time) AS first_at FROM (" +
			"SELECT trace_id, event_time FROM crewlet_events " +
			"WHERE turn_id = ? AND event_time >= ?) " +
			"WHERE trace_id != '' GROUP BY trace_id ORDER BY first_at ASC, trace_id ASC",
		[]any{turnID, EncodeTime(askedAt(at).Add(-EventHistory))}
}

// TurnNodes is every node that published any of one turn's rows this log
// holds, sorted: where the turn RAN, as far as this log knows.
//
// ASKED OF THE ROWS, NEVER OF THE STORE THAT ANSWERED: a data node keeps a
// stateless node's events in custody (ADR-0025), so holding the turn's rows does
// not say the turn ran here. And not derived from a capped read, for
// [EventLog.TurnTraces]' reason. A DISTINCT walk of the turn's index range
// through a derived table ([turnTracesSQL]); a row with no origin is left out
// rather than named as a node called "".
func (l *EventLog) TurnNodes(ctx context.Context, turnID string, at time.Time) ([]string, error) {
	return l.live().TurnNodes(ctx, turnID, at)
}

// TurnNodes is [EventLog.TurnNodes] read at this snapshot.
func (s EventSnapshot) TurnNodes(ctx context.Context, turnID string, at time.Time) ([]string, error) {
	query, args := turnNodesSQL(turnID, at)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read the nodes of turn %s: %w", turnID, err)
	}
	defer func() { _ = rows.Close() }()

	out := []string{}
	for rows.Next() {
		var node string
		if err := rows.Scan(&node); err != nil {
			return nil, fmt.Errorf("store: scan the nodes of turn %s: %w", turnID, err)
		}
		out = append(out, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the nodes of turn %s: %w", turnID, err)
	}
	return out, nil
}

// turnNodesSQL is the statement [EventLog.TurnNodes] runs and its arguments,
// a function of its own so its plan can be read back for exactly the statement
// that runs (TestEveryGroupedReadSeeksItsFiltersIndex).
func turnNodesSQL(turnID string, at time.Time) (string, []any) {
	return "SELECT DISTINCT node FROM (" +
			"SELECT node FROM crewlet_events " +
			"WHERE turn_id = ? AND event_time >= ?) " +
			"WHERE node != '' ORDER BY node ASC",
		[]any{turnID, EncodeTime(askedAt(at).Add(-EventHistory))}
}

// TurnTrace is one trace a turn touched and when it first did.
//
// THE INSTANT TRAVELS WITH THE ID because the order is a property of every log
// the turn wrote to: a turn resumed on another node has a trace on each, and
// only the instants say which came first once the two lists meet.
type TurnTrace struct {
	TraceID string    `json:"trace_id"`
	FirstAt time.Time `json:"first_at"`
}

// TraceEventCount is the same question about a trace. See [EventLog.TurnEventCount].
func (l *EventLog) TraceEventCount(ctx context.Context, traceID string, at time.Time) (int, error) {
	return l.live().TraceEventCount(ctx, traceID, at)
}

// TraceEventCount is [EventLog.TraceEventCount] read at this snapshot.
func (s EventSnapshot) TraceEventCount(ctx context.Context, traceID string, at time.Time) (int, error) {
	return s.countEvents(ctx, "trace_id", traceID, at)
}

// countEvents counts one id's rows inside the history window.
//
// The COLUMN is chosen from this file's own two callers and never from a
// parameter a request can reach: it is interpolated into the statement, which
// is the one place in this package where that would be an injection rather
// than a convenience.
func (s EventSnapshot) countEvents(ctx context.Context, column, id string, at time.Time) (int, error) {
	switch column {
	case "turn_id", "trace_id":
	default:
		return 0, fmt.Errorf("store: count events: %q is not a countable column", column)
	}
	var n int
	err := s.q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM crewlet_events WHERE "+column+" = ? AND event_time >= ?",
		id, EncodeTime(askedAt(at).Add(-EventHistory))).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count events: %w", err)
	}
	return n, nil
}

// TurnClosing returns the NEWEST rows of one turn, oldest first among
// themselves, with their payloads.
//
// For what a capped [EventLog.Turn] read loses: a long turn loses its ENDING,
// where `agent_turn_completed` and `turn_completed` carry the outcome, the wall
// clock and the plan summary; without them a view looks unfinished.
//
// NOT folded into Turn: a method silently returning a head and a tail would
// make every caller's row count stop meaning what it says. The view asks both
// and merges. The rows may OVERLAP the head on a turn that only just reached
// the cap, so a caller merges on the event id rather than concatenating.
func (l *EventLog) TurnClosing(ctx context.Context, turnID string, limit int, at time.Time) ([]EventRecord, error) {
	return l.live().TurnClosing(ctx, turnID, limit, at)
}

// TurnClosing is [EventLog.TurnClosing] read at this snapshot.
func (s EventSnapshot) TurnClosing(ctx context.Context, turnID string, limit int, at time.Time) ([]EventRecord, error) {
	if limit <= 0 {
		// ALLOCATED, like every other list read here — asking for no rows is
		// still a read that succeeded, and the contract on [EventLog] does
		// not have a second exception. Reachable only from a caller passing
		// a computed limit; today's one caller passes a constant.
		return []EventRecord{}, nil
	}
	out, err := s.scanPayloads(ctx,
		"SELECT "+listColumns+", payload FROM crewlet_events "+
			"WHERE turn_id = ? AND event_time >= ? "+
			"ORDER BY event_time DESC, event_id DESC LIMIT ?",
		turnID, EncodeTime(askedAt(at).Add(-EventHistory)), limit)
	if err != nil {
		return nil, err
	}
	slices.Reverse(out)
	return out, nil
}

// ByID returns one event WITH its payload, or ErrNotFound.
//
// A caller holding only an id has no time to seek with, so this seeks the id
// index and takes the newest match: see [byIDSQL]. THROUGH THE SHARED SCANNER
// although it wants one row, so `listColumns` and the destination list agree in
// one place ([EventLog.scanPayloads]).
//
// FLOORED LIKE EVERY OTHER READ, at `at` (see [EventLog]). Rows below
// [EventHistory] are the sweep's slack ([EventRetention] keeps a day, a lapsed
// singleton any age). Unfloored, a link past the horizon resolves on a node
// that has not swept and not on one that has, onto an event whose floored trace
// and turn read empty.
func (l *EventLog) ByID(ctx context.Context, id string, at time.Time) (EventRecord, error) {
	query, args := byIDSQL(id, at)
	recs, err := l.scanPayloads(ctx, query, args...)
	if err != nil {
		return EventRecord{}, fmt.Errorf("store: read event %s: %w", id, err)
	}
	if len(recs) == 0 {
		return EventRecord{}, fmt.Errorf("%w: event %s", ErrNotFound, id)
	}
	return recs[0], nil
}

// byIDSQL is the statement [EventLog.ByID] runs, a function so its plan is read
// back (TestALookupByIDSeeksTheIDIndex).
//
// BOTH `event_time` TERMS CARRY A UNARY PLUS, hiding the column from the
// planner so the only term it can seek on is the id (`crewlet_events_id_idx`,
// schema/0001). Unadorned, the planner uses the primary key `event_time` leads:
// walking the whole log newest first for an old id or dead link, or reading the
// floor as a RANGE intersected with the id index (`MULTI-INDEX AND`), linear in
// the window (4 ms a lookup against 0.2 ms on 60,000 rows). The floor still
// applies, to the rows the seek returns.
func byIDSQL(id string, at time.Time) (string, []any) {
	return "SELECT " + listColumns + ", payload FROM crewlet_events " +
			"WHERE event_id = ? AND +event_time >= ? ORDER BY +event_time DESC LIMIT 1",
		[]any{id, EncodeTime(askedAt(at).Add(-EventHistory))}
}

// Purge deletes events past EventRetention and reports how many went.
//
// It takes no window, deliberately: the retention is derived from the read
// floor, a shorter one deletes rows the log still serves and a longer one keeps
// rows nothing can reach.
//
// It deletes in batches, each its own autocommit statement: one DELETE over a
// multi-day overhang holds the single writer throughout, and the event Append
// runs INLINE in the publishing goroutine, which drops the event with a warning
// once busy_timeout runs out. Batching lets live appends interleave. In steady
// state the overhang is one batch.
func (l *EventLog) Purge(ctx context.Context) (int64, error) {
	cutoff := EncodeTime(now().Add(-EventRetention))
	// THE PARTY INDEX GOES FIRST, and on its own horizon rather than per
	// batch: it indexes this table, so a row of it that outlives its event
	// is a primary-key seek that finds nothing — and left unswept it would
	// grow for the life of the deployment while the thing it points at is
	// swept every tick. Deleted ahead of the events so the window where a
	// party row has no event is the short one, rather than the reverse,
	// where an event would briefly be invisible to the filter that reads
	// them. See schema/0016.
	if _, err := l.db.sql.ExecContext(ctx,
		"DELETE FROM crewlet_event_parties WHERE event_time < ?", cutoff,
	); err != nil {
		return 0, fmt.Errorf("store: purge event parties: %w", err)
	}
	// AND THE ORIGIN INDEX, on the same horizon for the same reason: it
	// indexes this table's turn-start records. See node/0059.
	if _, err := l.db.sql.ExecContext(ctx,
		"DELETE FROM crewlet_turn_origins WHERE event_time < ?", cutoff,
	); err != nil {
		return 0, fmt.Errorf("store: purge turn origins: %w", err)
	}
	// AND THE CUSTODY BATCHES THIS NODE SETTLED, on the same horizon,
	// counted from the settle: a record exists to turn away a redelivery
	// of its batch (see [EventLog.WriteCustody]), which a group makes when
	// an acknowledgement was lost — within the delivery's own retries,
	// minutes after the settle, and never a retention window later.
	if _, err := l.db.sql.ExecContext(ctx,
		"DELETE FROM custody_settled WHERE settled_at < ?", cutoff,
	); err != nil {
		return 0, fmt.Errorf("store: purge settled custody batches: %w", err)
	}
	// AND THE CUT OF EVERY USAGE DAY that ended before the same horizon: the
	// usage publisher never derives a day older than the read floor again,
	// so its recorded window ([DB.PinUsageDay]) answers nothing any more.
	if _, err := l.db.sql.ExecContext(ctx,
		"DELETE FROM usage_day_windows WHERE end_at < ?", cutoff,
	); err != nil {
		return 0, fmt.Errorf("store: purge the cuts of old usage days: %w", err)
	}
	var total int64
	for {
		res, err := l.db.sql.ExecContext(ctx,
			`DELETE FROM crewlet_events WHERE rowid IN (
				SELECT rowid FROM crewlet_events WHERE event_time < ? LIMIT ?)`,
			cutoff, EventPurgeBatch)
		if err != nil {
			return total, fmt.Errorf("store: purge events: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("store: purge events count: %w", err)
		}
		total += n
		if n < EventPurgeBatch {
			return total, nil
		}
	}
}

func (l *EventLog) scanRows(ctx context.Context, query string, args ...any) ([]EventRecord, error) {
	return l.live().scanRows(ctx, query, args...)
}

func (s EventSnapshot) scanRows(ctx context.Context, query string, args ...any) ([]EventRecord, error) {
	return s.scan(ctx, false, query, args...)
}

// scanPayloads is scanRows for a query whose SELECT ends in `payload`.
//
// Kept as one scanner behind a flag rather than two: the column list and the
// Scan call have to agree, and two copies of that agreement is exactly how a
// column added to `listColumns` comes to be read into the wrong field by one
// of them.
func (l *EventLog) scanPayloads(ctx context.Context, query string, args ...any) ([]EventRecord, error) {
	return l.live().scanPayloads(ctx, query, args...)
}

func (s EventSnapshot) scanPayloads(ctx context.Context, query string, args ...any) ([]EventRecord, error) {
	return s.scan(ctx, true, query, args...)
}

func (s EventSnapshot) scan(ctx context.Context, withPayload bool, query string, args ...any) ([]EventRecord, error) {
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// ALLOCATED EMPTY, never nil. Every one of these reads has a JSON surface
	// above it, and that is the only place the two differ: a nil slice
	// marshals as `null` and an empty one as `[]`, so a client doing the
	// obvious thing with the answer — reading `.length`, iterating it — gets a
	// crash for "nothing matched" and a working screen for everything else.
	// The Trace screen answered "not found" for every empty trace on exactly
	// that, and patching it at the one answer that noticed left the same nil
	// reaching six other reads. There is nothing a caller can do with the
	// distinction here that `len` does not already do.
	out := []EventRecord{}
	for rows.Next() {
		var rec EventRecord
		var micros int64
		var tagJSON, refs, payload string
		dest := []any{&micros, &rec.ID, &rec.Type, &rec.Source,
			&rec.Category, &rec.Summary, &refs, &rec.Actor, &rec.TraceID,
			&rec.SpanID, &rec.ParentSpanID, &tagJSON, &rec.WorkKey, &rec.Node, &rec.Tokens, &rec.Calls}
		if withPayload {
			dest = append(dest, &payload)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		finishRecord(&rec, micros, tagJSON)
		rec.Refs = refsOf(refs)
		if withPayload {
			rec.Payload = json.RawMessage(payload)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate events: %w", err)
	}
	return out, nil
}

// finishRecord fills in the fields derived from the raw columns.
func finishRecord(rec *EventRecord, micros int64, tagJSON string) {
	rec.Time = DecodeTime(micros)
	tags := map[string]string{}
	// A tags blob that will not decode costs the failure flag and the
	// related-agent match for that one row; it must not cost the row. This
	// is deliberate: the alternative — failing the page — hides every
	// event around the malformed one too.
	if err := json.Unmarshal([]byte(tagJSON), &tags); err != nil {
		tags = map[string]string{}
	}
	rec.Tags = tags
	rec.AgentID = tags["agent_id"]
	rec.Failed = types.Failed(rec.Type, false, tags["failed"] == "true")
}

// refsColumn is what the refs column holds for refs: their JSON, or "" for
// none, so a row that names nothing costs the column nothing.
func refsColumn(refs []named.Ref) string {
	if len(refs) == 0 {
		return ""
	}
	out, err := json.Marshal(refs)
	if err != nil {
		return ""
	}
	return string(out)
}

// refsOf reads the refs column back. A blob that will not decode costs the
// row its references, never the row: it still says what happened in words.
func refsOf(column string) []named.Ref {
	if column == "" {
		return nil
	}
	var refs []named.Ref
	if err := json.Unmarshal([]byte(column), &refs); err != nil {
		return nil
	}
	return refs
}

// seatTagKeys are the tag keys that name a SEAT of this company by a handle
// wherever they appear — an A2A channel's two ends and the one that closed it,
// and the seat a vendor delivery was addressed to — so each resolves to a party
// of the event. See [partiesOf] for `sender`, which names one only on an A2A
// channel's records, and for why `agent_role`, a name, is not here at all.
var seatTagKeys = []string{"requester", "target", "recipient", "closed_by"}

func truncate(recs []EventRecord, limit int) []EventRecord {
	if limit > 0 && len(recs) > limit {
		return recs[:limit]
	}
	return recs
}

// PhaseTokenQuery selects the phase records a spend breakdown aggregates.
type PhaseTokenQuery struct {
	// SinceDays is the window, in whole days back from the instant the
	// read is measured from (see [EventLog.PhaseTokens]). Zero or less
	// takes DefaultPhaseTokenDays.
	SinceDays int

	// Since and Until name the window as INSTANTS, which SinceDays cannot:
	// a time-range control produces two edges, and a cost explorer's
	// compare-to-previous asks for the window immediately before the one on
	// screen — neither of which is "N days back from now".
	//
	// Since at its zero value falls back to SinceDays. Until at its zero
	// value is unbounded, and it is EXCLUSIVE, so two adjacent windows
	// share their boundary instant without double-counting it.
	//
	// Since is floored at MaxPhaseTokenDays back whichever way it was
	// named: a request further back cannot return more rows, and honouring
	// it would make a scan of the whole table look like a supported query.
	Since time.Time
	Until time.Time

	// AgentID restricts the rollup to one seat, by the agent id every phase
	// row promotes. Empty is the whole org. Never the role name beside it,
	// which two seats may share.
	AgentID string

	// Limit keeps only the newest Limit records of the window. Zero or less
	// is the WHOLE window, and that is what a rollup must ask for: see the
	// note below on why the rollup has no row cap, since a total folded from
	// a truncated window is an undercount that reads as an underspend.
	//
	// It exists for a caller that RETAINS only a bounded tail anyway, which
	// is the live projection's startup seed: without it a day busier than
	// the projection's record cap was read in full, into memory, inside the
	// seed's time budget, only to be cut down to that cap on arrival.
	Limit int

	// At is the instant the window is cut against — what SinceDays counts
	// back from, what an unbounded top edge means, and where the
	// [MaxPhaseTokenDays] floor sits. Zero is now.
	//
	// A FIELD for [ListQuery.At]'s reason: a fleet pins the window on
	// the asker's clock and every node must floor it at the asker's instant
	// rather than its own. The asker's Since is already floored at ITS
	// clock; floored again at a peer's later one, every record between the
	// two floors was dropped from a window the answer said it covered.
	At time.Time

	// Before resumes a read below a record already held: only records
	// strictly older than it in the read's own (event_time, event_id)
	// order. Nil reads from the top of the window.
	//
	// A KEYSET, for the reason [ListQuery.Before] is one: records share a
	// microsecond in a burst, and a cursor on the instant alone skips or
	// repeats whatever collided with it — and a repeated spend record is a
	// double count.
	Before *Cursor
}

// at is the instant the window is cut against, read ONCE per answer.
func (q PhaseTokenQuery) at() time.Time { return askedAt(q.At) }

// Window reports the instants this query actually covers, after the floor,
// against `now` — the query's own [PhaseTokenQuery.At] when the caller is
// answering a pinned question.
//
// Exported because the CALLER labels the answer: a heading with the window
// asked for over rows from the window served is a lie, so the clamp lives
// where the floor is defined. The caller sets [PhaseTokenQuery.At] to the same
// now it passes here, making heading and rows one evaluation.
//
// TOTAL IN BOTH EDGES: an unbounded top edge reports now, not the zero time:
// an axis runs to now rather than to the newest record (a quiet company has
// empty buckets), and a rollup must say which instant it counted through.
func (q PhaseTokenQuery) Window(now time.Time) (since, until time.Time) {
	floor := now.Add(-time.Duration(MaxPhaseTokenDays) * 24 * time.Hour)
	since = q.Since
	if since.IsZero() {
		days := q.SinceDays
		if days <= 0 {
			days = DefaultPhaseTokenDays
		}
		since = now.Add(-time.Duration(days) * 24 * time.Hour)
	}
	if since.Before(floor) {
		since = floor
	}
	until = q.Until
	if until.IsZero() {
		until = now
	}
	// An inverted window is a caller error that must not read as a quiet
	// company: it collapses to an empty one at the later edge, so the
	// answer covers nothing and SAYS it covers nothing.
	if until.Before(since) {
		since = until
	}
	return since.UTC(), until.UTC()
}

const (
	// DefaultPhaseTokenDays is the window a caller that named none gets.
	//
	// A week: long enough to cover "what did last week cost", short
	// enough that the scan stays inside the index's recent pages. The
	// dashboard's own default matches it, so an unparameterised REST call
	// and an unparameterised socket query answer the same question.
	DefaultPhaseTokenDays = 7

	// MaxPhaseTokenDays bounds what a caller may ask for.
	//
	// Thirty days, matching the retention this table is swept on: asking
	// for more cannot return more, and accepting the request would make a
	// scan of the whole table look like a supported query.
	MaxPhaseTokenDays = 30

	// THERE IS NO ROW CAP ON THE ROLLUP, and its absence is the point.
	//
	// The numbers are columns (schema/0015, 0032), so a row is a few narrow
	// values and the whole window folds. A cap would silently undercount
	// spend for any org past it, which reads as an underspend.
)

// The price is the one value still read from the PAYLOAD, deliberately: one
// backend sets it on a minority of records, so a column would be NULL almost
// everywhere. json_extract runs only on rows the predicates keep.
//
// EVERY SPEND TYPE ([spendTypes]); a PERSON's auxiliary spend names no agent or
// role and carries their seat's role as payload `actor_role`, which the
// per-seat breakdown names it by. The person is the envelope's ACTOR.
//
// ONE TYPE PER STATEMENT, `event_type = ?` beside the window's edges, a range
// of the type's index: `IN (…)` reads every row of the type and applies the
// window afterwards (see [OutcomeQuery.countSQL]). [EventLog.PhaseTokens] runs
// it once per type, in one snapshot.
const phaseTokenSQL = `
SELECT event_time, event_id, agent_id,
       CASE WHEN agent_role = '' AND event_type = 'auxiliary_spend'
            THEN COALESCE(json_extract(payload, '$.actor_role'), '')
            ELSE agent_role END,
       CASE WHEN agent_id = '' AND event_type = 'auxiliary_spend' THEN actor ELSE '' END,
       phase, host_phase, worker, model, turn_id, work_key, iteration,
       input_tokens, output_tokens, total_tokens,
       cache_read_tokens, cache_write_tokens, provider_key,
       COALESCE(json_extract(payload, '$.cost_usd'), 0),
       calls, spend_stage
FROM crewlet_events
WHERE event_type = ? AND event_time >= ?`

// AgentPhaseLimit bounds a seat's phase history.
//
// Sized to the screen, not the table: each row carries the phase's prompts and
// whole response verbatim. The dashboard keeps its own cap at the same number.
//
// A PHASE'S RECORD AND A LAUNCH'S ([phaseRecordTypes]); an auxiliary record is
// deliberately neither: it is a SUM of calls with no prompt or response, and a
// compaction's burst would push a seat's real model calls off its first page.
const AgentPhaseLimit = 50

// phaseRecordTypes are what a phase list answers with: each phase's own
// record, and each launch of an executor's run as its collection read it — the
// executor round's work, which carries what the run spent and the report it
// wrote, and which a reader draws inside that round. A launch is no phase, so
// it is named here rather than found by a phase word.
var phaseRecordTypes = []string{phaseCompleted, launchSpendType}

// phaseListSQL is one record type's rows of a phase list, the seat's or the
// company's — see [EventLog.phaseList].
//
// The event_time floor is EventHistory, as in every read of this table: the
// hard bottom of paging. Without it a read answers below it (EventRetention's
// day of slack, and rows of any age on a node not sweeping), so a seat's page
// would show turns the company-wide Phases read excludes.
const phaseListSQL = `
SELECT ` + listColumns + `, payload
FROM crewlet_events
WHERE event_type = ? AND event_time >= ?`

// phaseListCursorSQL is the same read, one page older.
//
// Keyset on (event_time, event_id) rather than OFFSET, for the reason every
// other paged read here gives: the pair is the primary key, so it is unique and
// already in index order — no sort node, and no drift as new phases land at the
// head while a reader pages backwards.
const phaseListCursorSQL = ` AND (event_time, event_id) < (?, ?)`

const phaseListOrderSQL = ` ORDER BY event_time DESC, event_id DESC LIMIT ?`

// AgentPhases returns one seat's durable per-phase records, newest first,
// PAYLOAD INCLUDED.
//
// The seat page's LLM-invocation list. A separate method rather than a flag on
// ListQuery because of the payload: the feed's listing never selects it (a page
// of events with every payload is what makes a live screen slow).
//
// Matched on the seat's AGENT ID alone: a role name is prose two seats may
// share. The durable records ([phaseRecordTypes]) only; agent_turn_progress is
// stream-only, so history is exactly the calls that finished.
//
// more reports that the seat holds phases past this page — see [pastPage].
func (l *EventLog) AgentPhases(ctx context.Context, agentID string, before *Cursor, at time.Time) (rows []EventRecord, more bool, err error) {
	// An EMPTY id names no seat, and bound as one it would match every row
	// that carries none — every non-agent event in the window.
	if agentID == "" {
		return nil, false, nil
	}
	return l.phaseList(ctx, agentID, AgentPhaseLimit, before, at)
}

// Phases returns the COMPANY's durable per-phase records, newest first,
// PAYLOAD INCLUDED.
//
// The same read as AgentPhases with the seat filter lifted, and a read of its
// own rather than a flag on ListQuery for the reason AgentPhases gives.
//
// `agentID` narrows to ONE SEAT, by the id every node derives for it
// ([org.DeriveAgentID]), the promoted `agent_id` column [ListQuery.AgentID]
// reads; empty means the company. Not a role name: seats may share one, and a
// rename changes it while the history keeps the old one. more reports that
// records exist past this page — see [pastPage].
func (l *EventLog) Phases(ctx context.Context, agentID string, limit int, before *Cursor, at time.Time) (rows []EventRecord, more bool, err error) {
	if limit <= 0 || limit > MaxPhasePage {
		limit = MaxPhasePage
	}
	return l.phaseList(ctx, agentID, limit, before, at)
}

// phaseList is a page of `limit` phase records ([phaseRecordTypes]), newest
// first, payload included: one seat's where agentID names it and the
// company's where it is empty, below `before`, over the history at `at`.
//
// ONE STATEMENT PER RECORD TYPE, in ONE SNAPSHOT, merged here. Each is the
// single-type read the type's index answers in order, stopped one row past the
// page, so a page costs a page and a row of each type however long the history
// is. As one `event_type IN (…)` the planner would sort every row of both
// types, payloads and all. One snapshot makes the page and its past-the-page
// row one reading of the log.
func (l *EventLog) phaseList(ctx context.Context, agentID string, limit int, before *Cursor, at time.Time) ([]EventRecord, bool, error) {
	floor := EncodeTime(askedAt(at).Add(-EventHistory))
	var rows []EventRecord
	err := l.Snapshot(ctx, func(s EventSnapshot) error {
		// AFRESH ON EVERY ATTEMPT: a snapshot that met a conflict runs
		// this again, and a page kept from the attempt it abandoned would
		// be counted twice.
		rows = rows[:0]
		for _, kind := range phaseRecordTypes {
			query := phaseListSQL
			args := []any{kind, floor}
			if agentID != "" {
				query += ` AND agent_id = ?`
				args = append(args, agentID)
			}
			if before != nil && before.ID != "" {
				query += phaseListCursorSQL
				args = append(args, EncodeTime(before.Time), before.ID)
			}
			query += phaseListOrderSQL
			args = append(args, limit+1)
			page, err := s.scanPayloads(ctx, query, args...)
			if err != nil {
				return err
			}
			rows = append(rows, page...)
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	// THE ORDER EACH STATEMENT READ IN, across all of them: the instant as
	// the column holds it, then the id, both descending — the primary key's
	// own order, which the cursor a later page resumes from is cut in.
	slices.SortFunc(rows, func(a, b EventRecord) int {
		return cmp.Or(cmp.Compare(EncodeTime(b.Time), EncodeTime(a.Time)), cmp.Compare(b.ID, a.ID))
	})
	rows, more := pastPage(rows, limit)
	return rows, more, nil
}

// pastPage cuts a read that asked for ONE ROW MORE than its page back to the
// page, and reports whether that row existed: whether anything lies past the
// page.
//
// ASKED, NEVER INFERRED FROM A FULL PAGE: "the page filled, so there may be
// more" is wrong exactly when the history is a multiple of the page size. The
// extra row comes from the same statement, so it is the same snapshot as the
// page; a second query could see a row the page did not. A payload-carrying
// row costs one row's payload, the price of an exact answer.
func pastPage[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// MaxPhasePage bounds one page of company-wide phase records.
//
// Lower than the event feed's 400: each of these carries a phase's prompts and
// its whole response verbatim, so the limit is set by the size of a row rather
// than by how many a screen can show.
const MaxPhasePage = 60

// PhaseTokens returns the spend records inside a window: every phase's, every
// collected launch's (under the executor's phase) and every auxiliary record's.
// internal/tokens folds these for BOTH this and the live window.
//
// EVERY VALUE BUT THE PRICE IS A COLUMN (schema/0015, 0032), so a month folds
// without reading a payload; a count left in the payload reads as zero. The
// price stays there for the reason given at phaseTokenSQL.
//
// The window is measured from q.At, the CALLER's instant: a caller labelling
// its answer with [PhaseTokenQuery.Window] sets At to the label's instant, so
// label and rows are ONE evaluation, not two clock readings. A zero At is now,
// read ONCE.
func (l *EventLog) PhaseTokens(ctx context.Context, q PhaseTokenQuery) ([]tokens.Record, error) {
	since, until := q.Window(q.at())

	// BOTH EDGES, ALWAYS, and the top one EXCLUSIVE — matching the
	// half-open window the bucketing folds over, so a record on the
	// boundary belongs to exactly one of two adjacent windows. Applied even
	// for a caller that named no top edge, because [PhaseTokenQuery.Window]
	// reports that as now and the rows have to be the rows the label
	// claims: a phase stamped in the future by a skewed clock inside a
	// window headed "counted through now" is a number with no window.
	tail := " AND event_time < ?"
	args := []any{EncodeTime(since), EncodeTime(until)}
	if q.AgentID != "" {
		tail += " AND agent_id = ?"
		args = append(args, q.AgentID)
	}
	if q.Before != nil {
		tail += " AND (event_time, event_id) < (?, ?)"
		args = append(args, EncodeTime(q.Before.Time), q.Before.ID)
	}
	// Newest first, which is the order the breakdown renders in, and the
	// order a Limit keeps the head of. No LIMIT unless the caller asked for
	// a tail: see the note where the rollup's cap used to be. A tail of each
	// type is enough for a tail of all of them: the newest Limit records of
	// the window are among the newest Limit of whichever type each is.
	tail += " ORDER BY event_time DESC, event_id DESC"
	if q.Limit > 0 {
		tail += " LIMIT ?"
		args = append(args, q.Limit)
	}

	// ONE STATEMENT PER SPEND TYPE, in one snapshot — see [phaseTokenSQL] —
	// so the rollup is one reading of the log, as one statement's would be.
	kinds := spendTypes()
	var held []stampedSpend
	err := l.Snapshot(ctx, func(s EventSnapshot) error {
		// AFRESH ON EVERY ATTEMPT: a snapshot that met a conflict runs this
		// again, and records kept from the attempt it abandoned would be
		// counted twice.
		held = held[:0]
		for _, kind := range kinds {
			got, err := scanSpend(ctx, s.q, phaseTokenSQL+tail, append([]any{kind}, args...))
			if err != nil {
				return err
			}
			held = append(held, got...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(held, func(a, b stampedSpend) int {
		return cmp.Or(cmp.Compare(b.at, a.at), cmp.Compare(b.rec.EventID, a.rec.EventID))
	})
	if q.Limit > 0 && len(held) > q.Limit {
		held = held[:q.Limit]
	}

	out := make([]tokens.Record, 0, len(held))
	for _, h := range held {
		// RFC3339Nano, the same encoding the live window carries, so the
		// two orderings cannot disagree about which record is newer.
		//
		// The rollup PARSES it back rather than comparing bytes (see
		// tokens.compareStamp): RFC3339Nano trims trailing zeros, so a
		// whole-second stamp ends in 'Z' where a fractional one ends in a
		// digit, and 'Z' sorts after '.' — which put 03:04:05Z ahead of
		// 03:04:05.9Z. The encoding is still what matters here; it is the
		// one both sides agree to parse.
		h.rec.Timestamp = DecodeTime(h.at).Format(time.RFC3339Nano)
		out = append(out, h.rec)
	}
	return out, nil
}

// stampedSpend is one spend record as [phaseTokenSQL] read it, beside the
// instant it was read at in the column's own encoding — what the merge of the
// spend types orders by before the instant is rendered for the rollup.
type stampedSpend struct {
	at  int64
	rec tokens.Record
}

// scanSpend runs one spend type's [phaseTokenSQL] on q and reads its rows.
func scanSpend(ctx context.Context, q querier, query string, args []any) ([]stampedSpend, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: phase tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []stampedSpend
	for rows.Next() {
		var h stampedSpend
		rec := &h.rec
		if err := rows.Scan(&h.at, &rec.EventID, &rec.AgentID, &rec.AgentRole, &rec.Person,
			&rec.Phase, &rec.HostPhase, &rec.Worker, &rec.Model,
			&rec.TurnID, &rec.WorkKey, &rec.Iteration,
			&rec.InputTokens, &rec.OutputTokens, &rec.TotalTokens,
			&rec.CacheReadTokens, &rec.CacheWriteTokens, &rec.ProviderKey,
			&rec.CostUSD, &rec.Calls, &rec.Stage,
		); err != nil {
			return nil, fmt.Errorf("store: phase tokens: scan: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: phase tokens: %w", err)
	}
	return out, nil
}
