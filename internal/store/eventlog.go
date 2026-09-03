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

	"github.com/crewlet/crewlet/internal/events/types"

	"github.com/crewlet/crewlet/internal/tokens"
)

// ErrNotFound reports that a row a caller named by id does not exist.
var ErrNotFound = errors.New("store: not found")

const (
	// EventHistory is how far back the event log is read. This is the hard
	// bottom of paging: once a cursor crosses it every page is empty
	// forever, and a UI that cannot name the floor draws that as "the org
	// went quiet". It is a named constant so the API can ship the number to
	// a dashboard footer rather than each query inlining a literal.
	EventHistory = 30 * 24 * time.Hour

	// EventRetention is how long rows are kept — one day past the read
	// floor, and never less than it.
	//
	// The direction matters more than the margin. Sweeping SHORTER than the
	// read floor deletes rows a reader can still ask for, so a page
	// assembled at the boundary loses rows underneath it and the floor
	// stops describing the table. A day of slack is longer than any paging
	// session and three orders of magnitude longer than the maintenance
	// tick, so the sweep never races a reader.
	//
	// That a sweep exists at all is the change from the Postgres schema,
	// where this table — the highest-volume one in the deployment — had no
	// retention policy while every read of it stopped at 30 days. Rows past
	// the floor were unreachable and permanent.
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

	// defaultListLimit is the page size when a caller names none.
	defaultListLimit = 50

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
	// This struct is what /events, /events/{id} and /events/trace/{id}
	// answer with, and the client reads id, type, source, timestamp,
	// category, summary, trace_id and payload off it. Untagged, Go
	// marshalled it as ID, Type, Source, Time, TraceID, Payload — so
	// every one of those screens rendered a blank event with an empty
	// payload, and none of them failed: the fields were simply not the
	// ones being read.
	//
	// The names match livestate.FeedRow field for field on purpose. The
	// same screens show a live row from the projection and a historical
	// one from here, and two spellings of one event would make the two
	// halves of one list render differently.
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Source       string    `json:"source"`
	Time         time.Time `json:"timestamp"`
	Category     string    `json:"category"`
	Summary      string    `json:"summary"`
	Actor        string    `json:"actor"`
	TraceID      string    `json:"trace_id"`
	SpanID       string    `json:"span_id"`
	ParentSpanID string    `json:"parent_span_id"`

	// Tags are the filterable dimensions the writer extracted. The
	// promoted columns (agent_id, agent_role, task_id, channel_id, sender)
	// are copies of five of these; the rest exist only here.
	Tags map[string]string `json:"tags,omitempty"`

	// Payload is the full serialized event. Nil on a listing — see above.
	Payload json.RawMessage `json:"payload,omitempty"`

	// Failed says whether the work this event reports failed. Derived on
	// read from the event type plus the stored `failed` tag, because a
	// listing never selects the payload and the tag is all that survives
	// into history. Events written before the writer stamped that tag read
	// back as not-failed — a real discontinuity at that point in the
	// timeline, not a bug to paper over.
	Failed bool `json:"failed"`

	// Spend is what one LLM call cost, present only on a phase completion.
	//
	// Promoted out of the payload and into columns because the rollup that
	// reads it is an AGGREGATION: it wants nine small values from every
	// row in a window, and reaching them through the payload meant hauling
	// each phase's whole prompt and response across the driver to decode
	// them in Go. See schema/0015.
	Spend *Spend `json:"spend,omitempty"`
}

// Spend is one LLM call's identity and its token cost.
//
// A pointer on [EventRecord] rather than flat fields: it is set on one event
// type out of dozens, and flattening it would put nine always-empty fields on
// every row the dashboard renders.
type Spend struct {
	// Phase is which phase ran; HostPhase is the phase a nested call ran
	// under, and Worker names the auxiliary worker when Phase is
	// "auxiliary" — which is why the worker rollup keys on the pair.
	Phase     string `json:"phase,omitempty"`
	HostPhase string `json:"host_phase,omitempty"`
	Worker    string `json:"worker,omitempty"`
	Model     string `json:"model,omitempty"`

	TurnID    string `json:"turn_id,omitempty"`
	Iteration int    `json:"iteration,omitempty"`

	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
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
// recent defaultListLimit events.
type ListQuery struct {
	Limit    int
	Type     string
	Source   string
	Category string
	TraceID  string
	Actor    string

	// TurnID selects one unit of agent work — every phase of it, its own
	// completion record, and the fallbacks and breaches that happened
	// inside it. Rows written before migration 0014 carry an empty
	// turn_id and do not answer this filter; see the migration.
	TurnID string

	// RelatedAgent is a broad filter: events whose actor is the agent, or
	// whose tags name it as agent_role / target / recipient / sender, plus
	// every event sharing a trace with one of those — so the inbound
	// webhook that caused the agent's work shows up beside it.
	//
	// It over-fetches and post-filters, so a page shorter than Limit does
	// NOT mean history is exhausted here; only a zero-row page does.
	RelatedAgent string

	// Before is an exclusive cursor. Nil starts at the newest row.
	Before *Cursor
}

// EventLog is the audit and observability event store.
type EventLog struct{ db *DB }

// Events returns the audit log backed by this database.
func (d *DB) Events() *EventLog { return &EventLog{db: d} }

const eventInsertSQL = `
INSERT INTO crewlet_events (
	event_time, event_id, event_type, source, category,
	trace_id, span_id, parent_span_id,
	agent_id, agent_role, task_id, channel_id, sender,
	summary, actor, tags, payload,
	phase, host_phase, worker, model, turn_id, iteration,
	input_tokens, output_tokens, total_tokens
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?, ?)
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
	// The identity is checked here because SQL cannot check it. NOT NULL
	// catches a missing column; it does not catch a zero one, and both
	// halves of this key have a zero value that stores fine and then reads
	// as nothing: a zero Time lands in year 1, permanently below the read
	// floor, and an empty ID collides with every other record that forgot
	// the same field. Either way the row exists and no query returns it.
	if rec.ID == "" {
		return fmt.Errorf("%w: no event id", ErrIncompleteRecord)
	}
	if rec.Time.IsZero() {
		return fmt.Errorf("%w: event %s has no timestamp", ErrIncompleteRecord, rec.ID)
	}
	tags := rec.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	tagJSON, err := json.Marshal(tags)
	if err != nil {
		return fmt.Errorf("store: encode event tags (%s): %w", rec.ID, err)
	}
	payload := rec.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	// DERIVED HERE WHEN THE CALLER DID NOT SET IT, rather than trusted to
	// have been set. A caller that builds a record by hand and sets only
	// the payload would otherwise write a phase completion whose spend
	// columns are all zero, and a rollup reading them would report a
	// company that spent nothing. Silence is the one failure this column
	// promotion exists to remove, so it must not be reintroduced by the
	// write path.
	//
	// The comment here used to say [RecordFor] fills it on the one
	// production path "so this normally costs nothing". That was false in
	// both halves: RecordFor has no production caller — the wiring is
	// observe.NewWriter — so this branch was the ONLY one ever taken, and
	// it re-decoded the payload on every phase completion. observe.Record
	// now sets Spend from the bytes it already has, which makes the
	// fallback the exception it was always described as.
	//
	// The zero value writes the same empty strings and zeroes the column
	// defaults would, so every non-phase row is unaffected.
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
	// completions [SpendFor] is scoped to. That function's subject is
	// SPEND, so it reads one event type and returns nil for the rest — which
	// is right for the rollup and wrong for identity: reading one turn means
	// every row it touched, and a delivery, a tool call or an A2A ask carries
	// a turn id without carrying a token count. Scoped to the column that is
	// an identifier so a non-phase row cannot acquire a phase's numbers.
	if spend.TurnID == "" {
		spend.TurnID = tags["turn_id"]
	}
	// IN ONE TRANSACTION with its party rows, because the party table is an
	// INDEX of this one and an index that can be missing entries is not an
	// index: an event stored without its parties is invisible to the filter
	// that reads them, permanently and with nothing to say so. The cost is
	// a begin and a commit on a path that runs a handful of times a second.
	if err := l.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, eventInsertSQL,
			EncodeTime(rec.Time), rec.ID, rec.Type, rec.Source, rec.Category,
			rec.TraceID, rec.SpanID, rec.ParentSpanID,
			tags["agent_id"], tags["agent_role"], tags["task_id"],
			tags["channel_id"], tags["sender"],
			rec.Summary, rec.Actor, string(tagJSON), string(payload),
			spend.Phase, spend.HostPhase, spend.Worker, spend.Model,
			spend.TurnID, spend.Iteration,
			spend.InputTokens, spend.OutputTokens, spend.TotalTokens,
		); err != nil {
			return err
		}
		for _, party := range partiesOf(rec.Actor, tags) {
			if _, err := tx.ExecContext(ctx, partyInsertSQL,
				party, EncodeTime(rec.Time), rec.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("store: append event %s: %w", rec.ID, err)
	}
	return nil
}

// partyInsertSQL records one (party, event) pair. Idempotent for the same
// reason the event insert is: a publish retry replays the whole append.
const partyInsertSQL = `
INSERT INTO crewlet_event_parties (party, event_time, event_id) VALUES (?, ?, ?)
ON CONFLICT (party, event_time, event_id) DO NOTHING`

// partiesOf names every agent an event involves, deduplicated.
//
// THE ACTOR AND THE FOUR TAGS, and the set is exactly what the RelatedAgent
// filter used to test in Go — moved to write time so the read can be an index
// seek. Deduplicated here rather than left to the conflict clause, because an
// event whose actor is also its sender is the common case, not the rare one.
func partiesOf(actor string, tags map[string]string) []string {
	seen := make(map[string]struct{}, len(agentTagKeys)+1)
	var out []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	add(actor)
	for _, key := range agentTagKeys {
		add(tags[key])
	}
	return out
}

// listColumns is every column a listing reads. `payload` is absent
// deliberately — see EventRecord.Payload.
const listColumns = `event_time, event_id, event_type, source, category,
	summary, actor, trace_id, span_id, parent_span_id, tags`

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

// List returns a page of events, newest first, ordered by (time, id)
// descending.
func (l *EventLog) List(ctx context.Context, q ListQuery) ([]EventRecord, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}

	// The RelatedAgent filter is a JOIN rather than another WHERE clause,
	// because "involves this agent" is one fact spread over five places on
	// the event, and the party table is where it was normalised to. The
	// join is two index seeks — the party's covering index, then the log's
	// primary key — so the work scales with the number of MATCHES rather
	// than with the size of the log. See schema/0016.
	joined := q.RelatedAgent != ""
	// EVERY column of the log is qualified when joined, not just the two
	// that collide. Qualifying only `event_time` and `event_id` — the pair
	// the party table also carries — would work today and break the day
	// that table grows a column sharing a name with one of these.
	col := func(name string) string {
		if joined {
			return "crewlet_events." + name
		}
		return name
	}

	where := []string{col("event_time") + " >= ?"}
	args := []any{EncodeTime(now().Add(-EventHistory))}
	addEq := func(name, val string) {
		if val != "" {
			where = append(where, col(name)+" = ?")
			args = append(args, val)
		}
	}
	addEq("event_type", q.Type)
	addEq("source", q.Source)
	addEq("category", q.Category)
	addEq("trace_id", q.TraceID)
	addEq("actor", q.Actor)
	addEq("turn_id", q.TurnID)

	from := "crewlet_events"
	if joined {
		from = `crewlet_events JOIN crewlet_event_parties
			ON crewlet_event_parties.event_time = crewlet_events.event_time
			AND crewlet_event_parties.event_id = crewlet_events.event_id`
		where = append(where, "crewlet_event_parties.party = ?")
		args = append(args, q.RelatedAgent)
	}

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

	out, err := l.scanRows(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if joined {
		siblings, err := l.traceSiblings(ctx, out, limit)
		if err != nil {
			return nil, err
		}
		out = mergeRelated(out, siblings, limit)
	}
	return out, nil
}

// traceSiblings fetches the other events in the traces a page of direct
// matches belongs to.
//
// A SECOND QUERY rather than a wider first one, and this is the half that
// makes the filter mean something: an agent's work is CAUSED by something — a
// webhook, a notification, a schedule tick — and that trigger names the agent
// nowhere. Pulling in everything sharing a trace with a direct match is what
// puts the cause next to the effect.
//
// It reads through the trace index, so it costs one seek per trace on the
// page. The old shape found siblings only among the rows it happened to have
// over-fetched, which meant a cause older than that window was simply missing.
func (l *EventLog) traceSiblings(ctx context.Context, direct []EventRecord, limit int) ([]EventRecord, error) {
	traces := make([]any, 0, len(direct))
	seen := make(map[string]struct{}, len(direct))
	for _, rec := range direct {
		if rec.TraceID == "" {
			continue
		}
		if _, dup := seen[rec.TraceID]; dup {
			continue
		}
		seen[rec.TraceID] = struct{}{}
		traces = append(traces, rec.TraceID)
	}
	if len(traces) == 0 {
		return nil, nil
	}
	query := "SELECT " + listColumns + " FROM crewlet_events WHERE trace_id IN (?" +
		strings.Repeat(",?", len(traces)-1) +
		") ORDER BY event_time DESC, event_id DESC LIMIT ?"
	return l.scanRows(ctx, query, append(traces, limit)...)
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
// MaxTraceEvents rows should say the view is truncated.
func (l *EventLog) Trace(ctx context.Context, traceID string) ([]EventRecord, error) {
	return l.scanRows(ctx,
		"SELECT "+listColumns+" FROM crewlet_events "+
			"WHERE trace_id = ? AND event_time >= ? "+
			"ORDER BY event_time ASC, event_id ASC LIMIT ?",
		traceID, EncodeTime(now().Add(-EventHistory)), MaxTraceEvents)
}

// Turn returns every event of one turn, OLDEST first.
//
// Ordered like a trace and for the same reason: a turn is read forwards — plan,
// then execute, then review — rather than as a feed. It is a DIFFERENT set from
// the trace, which is why it is a separate read: one trace can span several
// turns (a webhook that wakes two seats), and a turn resumed on another node
// after a restart can span several traces.
//
// A caller that gets exactly MaxTurnEvents rows should say the view is
// truncated. The cap is the trace's, for the same reason: a turn that has
// self-iterated many times is the one worth reading, and a bound low enough to
// cut it short would hide exactly that.
func (l *EventLog) Turn(ctx context.Context, turnID string) ([]EventRecord, error) {
	return l.scanPayloads(ctx,
		"SELECT "+listColumns+", payload FROM crewlet_events "+
			"WHERE turn_id = ? AND event_time >= ? "+
			"ORDER BY event_time ASC, event_id ASC LIMIT ?",
		turnID, EncodeTime(now().Add(-EventHistory)), MaxTurnEvents)
}

// MaxTurnEvents bounds one turn's read.
const MaxTurnEvents = MaxTraceEvents

// ByID returns one event WITH its payload, or ErrNotFound.
//
// The identity is (event_time, event_id) and a caller holding only an id — a
// link, a line pasted from a log — has no time to seek with, so this reads the
// id index and takes the newest match.
func (l *EventLog) ByID(ctx context.Context, id string) (EventRecord, error) {
	row := l.db.sql.QueryRowContext(ctx,
		"SELECT "+listColumns+", payload FROM crewlet_events "+
			"WHERE event_id = ? ORDER BY event_time DESC LIMIT 1", id)

	var rec EventRecord
	var micros int64
	var tagJSON, payload string
	if err := row.Scan(&micros, &rec.ID, &rec.Type, &rec.Source, &rec.Category,
		&rec.Summary, &rec.Actor, &rec.TraceID, &rec.SpanID, &rec.ParentSpanID,
		&tagJSON, &payload,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return EventRecord{}, fmt.Errorf("%w: event %s", ErrNotFound, id)
		}
		return EventRecord{}, fmt.Errorf("store: read event %s: %w", id, err)
	}
	finishRecord(&rec, micros, tagJSON)
	rec.Payload = json.RawMessage(payload)
	return rec, nil
}

// Purge deletes events past EventRetention and reports how many went.
//
// It takes no window, deliberately. The retention is derived from the read
// floor and has no honest reason to vary per deployment: a shorter one deletes
// rows the log still serves, and a longer one keeps rows nothing can reach.
// Offering the choice would only make both mistakes possible.
//
// It deletes in batches, each its own autocommit statement, because this is
// the highest-volume table in the deployment and its rows carry whole phase
// payloads: one DELETE over a multi-day overhang — a fleet that was down, a
// singleton duty that lapsed — holds the single writer for the whole
// statement, and the event Append runs INLINE in the publishing goroutine,
// which drops the event with a warning once busy_timeout runs out. Batching
// releases the writer between statements, so live appends interleave with the
// catch-up instead of losing to it. On the steady-state tick the overhang is
// one batch and the loop runs once.
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
	return l.scan(ctx, false, query, args...)
}

// scanPayloads is scanRows for a query whose SELECT ends in `payload`.
//
// Kept as one scanner behind a flag rather than two: the column list and the
// Scan call have to agree, and two copies of that agreement is exactly how a
// column added to `listColumns` comes to be read into the wrong field by one
// of them.
func (l *EventLog) scanPayloads(ctx context.Context, query string, args ...any) ([]EventRecord, error) {
	return l.scan(ctx, true, query, args...)
}

func (l *EventLog) scan(ctx context.Context, withPayload bool, query string, args ...any) ([]EventRecord, error) {
	rows, err := l.db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []EventRecord
	for rows.Next() {
		var rec EventRecord
		var micros int64
		var tagJSON, payload string
		dest := []any{&micros, &rec.ID, &rec.Type, &rec.Source,
			&rec.Category, &rec.Summary, &rec.Actor, &rec.TraceID,
			&rec.SpanID, &rec.ParentSpanID, &tagJSON}
		if withPayload {
			dest = append(dest, &payload)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		finishRecord(&rec, micros, tagJSON)
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
	rec.Failed = types.Failed(rec.Type, false, tags["failed"] == "true")
}

// agentTagKeys are the tag keys that mean "this event involves that agent".
var agentTagKeys = []string{"agent_role", "target", "recipient", "sender"}

func truncate(recs []EventRecord, limit int) []EventRecord {
	if limit > 0 && len(recs) > limit {
		return recs[:limit]
	}
	return recs
}

// PhaseTokenQuery selects the phase records a spend breakdown aggregates.
type PhaseTokenQuery struct {
	// SinceDays is the window, in whole days back from now. Zero or less
	// takes DefaultPhaseTokenDays.
	SinceDays int

	// AgentRole restricts the rollup to one seat. Empty is the whole org.
	AgentRole string
}

const (
	// DefaultPhaseTokenDays is the window a caller that named none gets.
	//
	// A week: long enough to cover "what did last sprint cost", short
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
	// There used to be one — twenty thousand rows, newest first — because
	// each row carried the phase's whole prompt and response and folding a
	// month of them was a second of CPU and hundreds of megabytes. It also
	// meant the answer to "what did this company spend" was SILENTLY SHORT
	// for any org past that many phase completions in the window, which
	// this file's own arithmetic put at a third of a busy month.
	//
	// The numbers are columns now (schema/0015), so a row is nine narrow
	// values instead of a document, and the whole window folds. A cap here
	// would only reintroduce an undercount that looks like an underspend.
)

const phaseTokenSQL = `
SELECT event_time, event_id, agent_id, agent_role,
       phase, host_phase, worker, model, turn_id, iteration,
       input_tokens, output_tokens, total_tokens
FROM crewlet_events
WHERE event_type = 'agent_phase_completed' AND event_time >= ?`

// AgentPhaseLimit bounds a seat's phase history.
//
// Sized to the screen rather than to the table: the seat page renders these as
// an expandable list and each row carries the phase's prompts and its whole
// response verbatim, so a hundred of them is already megabytes on the wire for
// a list nobody scrolls to the end of. The dashboard keeps its own cap at the
// same number, so the page and the answer agree about where history stops.
const AgentPhaseLimit = 50

// The event_time floor is EventHistory, the same one every other read of this
// table applies — and it was the one read without it. The floor is the hard
// bottom of paging, so a read that does not apply it answers below it: the
// rows in EventRetention's day of slack, and on a node whose maintenance
// singleton is not sweeping, rows of any age. A seat's own page then showed
// turns the company-wide Phases read excludes, which is the one comparison an
// operator makes to decide whether a seat has gone quiet.
const agentPhaseSQL = `
SELECT ` + listColumns + `, payload
FROM crewlet_events
WHERE event_type = 'agent_phase_completed' AND event_time >= ?
  AND (agent_id = ? OR agent_role = ?)`

// agentPhaseCursorSQL is the same read, one page older.
//
// Keyset on (event_time, event_id) rather than OFFSET, for the reason every
// other paged read here gives: the pair is the primary key, so it is unique and
// already in index order — no sort node, and no drift as new phases land at the
// head while a reader pages backwards.
const agentPhaseCursorSQL = ` AND (event_time, event_id) < (?, ?)`

const agentPhaseOrderSQL = ` ORDER BY event_time DESC, event_id DESC LIMIT ?`

// AgentPhases returns one seat's durable per-phase records, newest first,
// PAYLOAD INCLUDED.
//
// The seat page's LLM-invocation list. It is a separate method rather than a
// flag on ListQuery precisely because of the payload: the feed's own listing
// deliberately never selects it — a page of events with every payload attached
// is the query that makes an activity screen slow — and a boolean on the
// shared query type would put that mistake one keystroke away.
//
// Matched on EITHER identifier, because a caller holds whichever the seat page
// gave it: the roster carries the handle-derived agent id and the projection
// keys on the role name. Both are promoted columns, so neither is a scan.
//
// agent_phase_completed only. That is the durable record — the prompts, the
// response, the tools, the tokens — while agent_turn_progress is stream-only
// by design, so history here is exactly the calls that finished.
func (l *EventLog) AgentPhases(ctx context.Context, agentID, agentRole string, before *Cursor) ([]EventRecord, error) {
	if agentID == "" && agentRole == "" {
		return nil, nil
	}
	query := agentPhaseSQL
	args := []any{EncodeTime(now().Add(-EventHistory)), agentID, agentRole}
	if before != nil && before.ID != "" {
		query += agentPhaseCursorSQL
		args = append(args, EncodeTime(before.Time), before.ID)
	}
	query += agentPhaseOrderSQL
	args = append(args, AgentPhaseLimit)
	return l.scanPayloads(ctx, query, args...)
}

// phasesSQL is AgentPhases without the seat filter.
const phasesSQL = `
SELECT ` + listColumns + `, payload
FROM crewlet_events
WHERE event_type = 'agent_phase_completed' AND event_time >= ?`

// Phases returns the COMPANY's durable per-phase records, newest first,
// PAYLOAD INCLUDED.
//
// The same read as AgentPhases with the seat filter lifted, because the
// question "what have the models been doing" is a company-level one and the
// answer that served it before was a per-seat list capped at fifty rows with no
// pager. There was no other way to ask.
//
// It is a read of its own rather than a flag on ListQuery, for the reason
// AgentPhases gives: the feed's listing deliberately never selects the payload,
// and a page of ordinary events with every payload attached is the query that
// makes an activity screen slow. A boolean on the shared type would put that
// one keystroke away.
//
// `role` narrows to one seat when a caller wants it; empty means the company.
func (l *EventLog) Phases(ctx context.Context, role string, limit int, before *Cursor) ([]EventRecord, error) {
	query := phasesSQL
	args := []any{EncodeTime(now().Add(-EventHistory))}
	if role != "" {
		query += ` AND agent_role = ?`
		args = append(args, role)
	}
	if before != nil && before.ID != "" {
		query += agentPhaseCursorSQL
		args = append(args, EncodeTime(before.Time), before.ID)
	}
	query += agentPhaseOrderSQL
	if limit <= 0 || limit > MaxPhasePage {
		limit = MaxPhasePage
	}
	args = append(args, limit)
	return l.scanPayloads(ctx, query, args...)
}

// MaxPhasePage bounds one page of company-wide phase records.
//
// Lower than the event feed's 400: each of these carries a phase's prompts and
// its whole response verbatim, so the limit is set by the size of a row rather
// than by how many a screen can show.
const MaxPhasePage = 60

// PhaseTokens returns the per-phase spend records inside a window.
//
// The rows the dashboard's spend breakdown is folded from — see
// internal/tokens, which does the folding for BOTH this and the live window,
// so a rollup over seven days and a rollup over the live one cannot disagree
// about what a phase costs.
//
// The token counts come out of the PAYLOAD rather than from columns of their
// own. That is deliberate: they are five numbers on one event type, and
// promoting them would mean a migration and five more columns that are NULL on
// every other row in the table. The filterable dimensions — the ones a query
// selects ON — are the promoted ones.
func (l *EventLog) PhaseTokens(ctx context.Context, q PhaseTokenQuery) ([]tokens.Record, error) {
	days := q.SinceDays
	switch {
	case days <= 0:
		days = DefaultPhaseTokenDays
	case days > MaxPhaseTokenDays:
		days = MaxPhaseTokenDays
	}
	since := now().Add(-time.Duration(days) * 24 * time.Hour)

	sql := phaseTokenSQL
	args := []any{EncodeTime(since)}
	if q.AgentRole != "" {
		sql += " AND agent_role = ?"
		args = append(args, q.AgentRole)
	}
	// Newest first, which is the order the breakdown renders in. No LIMIT:
	// see the note where the cap used to be.
	sql += " ORDER BY event_time DESC, event_id DESC"

	rows, err := l.db.sql.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: phase tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []tokens.Record
	for rows.Next() {
		var (
			at  int64
			rec tokens.Record
		)
		if err := rows.Scan(&at, &rec.EventID, &rec.AgentID, &rec.AgentRole,
			&rec.Phase, &rec.HostPhase, &rec.Worker, &rec.Model,
			&rec.TurnID, &rec.Iteration,
			&rec.InputTokens, &rec.OutputTokens, &rec.TotalTokens,
		); err != nil {
			return nil, fmt.Errorf("store: phase tokens: scan: %w", err)
		}
		// RFC3339Nano, the same encoding the live window carries, so the
		// two orderings cannot disagree about which record is newer.
		//
		// The rollup PARSES it back rather than comparing bytes (see
		// tokens.compareStamp): RFC3339Nano trims trailing zeros, so a
		// whole-second stamp ends in 'Z' where a fractional one ends in a
		// digit, and 'Z' sorts after '.' — which put 03:04:05Z ahead of
		// 03:04:05.9Z. The encoding is still what matters here; it is the
		// one both sides agree to parse.
		rec.Timestamp = DecodeTime(at).Format(time.RFC3339Nano)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: phase tokens: %w", err)
	}
	return out, nil
}
