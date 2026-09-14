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

	// WorkKey is the unit of work this row's run was an attempt at — see
	// ADR-0017 and [ListQuery.WorkKey].
	//
	// OFF THE COLUMN, and it is the one promoted value that is NOT a copy
	// of a tag. schema/0029 backfilled the column from `turn_id`, which is
	// where the work key lived before the split, and it could not
	// reasonably rewrite every historical tags blob to match — so for rows
	// written before that migration the column holds the work key and
	// `Tags["work_key"]` is empty. A reader going through the tags would
	// therefore answer "no unit of work" for exactly the history the
	// backfill exists to preserve, while `/events?work_key=` — which
	// filters on the column — returned those same rows. One authority,
	// and it is the column every other work-key reader already uses
	// (turnlist's grouping, the phase-token rollup, the filter above).
	WorkKey string `json:"work_key,omitempty"`

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

	// TurnID and WorkKey are IDENTITY rather than cost, and they are here
	// because this type is the carrier for every promoted column — see
	// [EventLog.Append], which fills them for any event that names one and
	// not only for the phase completions [SpendFor] reads. TurnID names one
	// RUN of a turn; WorkKey names the unit of work it was dispatched for,
	// which a re-run repeats and a run id does not. See ADR-0017.
	TurnID    string `json:"turn_id,omitempty"`
	WorkKey   string `json:"work_key,omitempty"`
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

	// TurnID selects one RUN of a turn — every phase of it, its own
	// completion record, and the fallbacks and breaches that happened
	// inside it. Rows written before migration 0014 carry an empty
	// turn_id and do not answer this filter; see the migration.
	TurnID string

	// WorkKey selects EVERY RUN of one unit of work — the attempts at a
	// trigger that was redelivered, which TurnID by construction cannot
	// ask for once it names one execution. Backed by the partial index
	// schema/0029 ships; rows from before it carry the work key in
	// turn_id, and that migration's backfill copies it across so the
	// history answers this filter too. See ADR-0017.
	WorkKey string

	// RelatedAgent is a broad filter: events whose actor is the agent, or
	// whose tags name it as agent_role / target / recipient / sender, plus
	// every event sharing a trace with one of those — so the inbound
	// webhook that caused the agent's work shows up beside it.
	//
	// It over-fetches and post-filters, so a page shorter than Limit does
	// NOT mean history is exhausted here; only a zero-row page does.
	RelatedAgent string

	// Since and Until bound the window a caller is asking about, as a
	// half-open interval `[Since, Until)`.
	//
	// SEPARATE FROM [ListQuery.Before], which is a CURSOR: the cursor is
	// where this page resumes and moves with every page, while these are
	// what the reader asked for and do not. Folding a window into the
	// cursor would make the second page of a bounded read unbounded, which
	// is the failure a keyset exists to avoid wearing a filter's clothes.
	//
	// The history window still applies underneath: a `Since` older than
	// [EventHistory] does not reach rows the log no longer keeps, and
	// saying so is the caller's job rather than this read's.
	//
	// Zero means unbounded on that side, which is a meaningful zero: an
	// instant nobody named is not midnight in 1970.
	Since time.Time
	Until time.Time

	// Before is an exclusive cursor. Nil starts at the newest row.
	Before *Cursor
}

// EventLog is the audit and observability event store.
//
// EVERY LIST READ HERE ANSWERS AN ALLOCATED SLICE, never nil, because every one
// of them has a JSON surface above it and that is the only place the two
// differ: nil marshals as `null` and empty as `[]`, so a client doing the
// obvious thing with the answer crashes on "nothing matched" and works on
// everything else. The one deliberate exception is [EventLog.AgentPhases],
// which answers nil for a seat it cannot name at all — a question it did not
// understand, rather than one whose answer is empty. A caller that needs to
// know whether there are rows asks `len`, which is right either way.
type EventLog struct{ db *DB }

// Events returns the audit log backed by this database.
func (d *DB) Events() *EventLog { return &EventLog{db: d} }

const eventInsertSQL = `
INSERT INTO crewlet_events (
	event_time, event_id, event_type, source, category,
	trace_id, span_id, parent_span_id,
	agent_id, agent_role, task_id, channel_id, sender,
	summary, actor, tags, payload,
	phase, host_phase, worker, model, turn_id, work_key, iteration,
	input_tokens, output_tokens, total_tokens
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
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
	// THE UNIT OF WORK BESIDE THE RUN, for the same reason and by the same
	// route. A turn that fails without acting is redelivered, so one
	// trigger legitimately runs several times: turn_id tells the attempts
	// apart and this is what still groups them. See ADR-0017.
	if spend.WorkKey == "" {
		spend.WorkKey = tags["work_key"]
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
			spend.TurnID, spend.WorkKey, spend.Iteration,
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
	summary, actor, trace_id, span_id, parent_span_id, tags, work_key`

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

// predicate is the FROM, the WHERE terms and their arguments a query's filters
// compile to — everything the caller ASKED FOR, and nothing about where a page
// resumes.
//
// SHARED, because [EventLog.List] and [EventLog.Histogram] answer two halves of
// one screen: a bar counting rows the list below it would not show is worse
// than no bar at all, and two copies of six filters is how that happens. The
// cursor is deliberately NOT here — it is where a page resumes and moves with
// every page, while these are what the reader asked for and do not, so folding
// it in would make a histogram change as somebody scrolled.
//
// `col` qualifies a column name for whichever FROM was built, and callers use
// it for every column rather than for the two that collide: qualifying only
// `event_time` and `event_id` — the pair the party table also carries — would
// work today and break the day that table grows a column sharing a name with
// one of these.
func (q ListQuery) predicate() (from string, where []string, args []any, col func(string) string) {
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
	args = []any{EncodeTime(now().Add(-EventHistory))}
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
	addEq("work_key", q.WorkKey)
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
		limit = defaultListLimit
	}

	from, where, args, col := q.predicate()
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
// Ordered like a trace and for the same reason: a turn is read forwards, the
// executor and then the reviewer round by round, rather than as a feed. It is
// a DIFFERENT set from the trace, which is why it is a separate read: one
// trace can span several turns (a webhook that wakes two seats), and a turn
// resumed on another node after a restart can span several traces.
//
// A caller that gets exactly MaxTurnEvents rows should say the view is
// truncated, and should read [EventLog.TurnClosing] beside it: because this
// read is ordered forwards, what a cut loses is the turn's own ENDING, which
// is where the records a reader came for live. The cap is the trace's, for the
// same reason: a turn that has self-iterated many times is the one worth
// reading, and a bound low enough to cut it short would hide exactly that.
func (l *EventLog) Turn(ctx context.Context, turnID string) ([]EventRecord, error) {
	return l.scanPayloads(ctx,
		"SELECT "+listColumns+", payload FROM crewlet_events "+
			"WHERE turn_id = ? AND event_time >= ? "+
			"ORDER BY event_time ASC, event_id ASC LIMIT ?",
		turnID, EncodeTime(now().Add(-EventHistory)), MaxTurnEvents)
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
func (l *EventLog) TurnEventCount(ctx context.Context, turnID string) (int, error) {
	return l.countEvents(ctx, "turn_id", turnID)
}

// TurnTraces is every trace one turn touched, in the order it first touched
// them.
//
// ASKED RATHER THAN DERIVED FROM THE ROWS, and the reason is the cap above. A
// turn RESUMED on another node after a restart spans more than one trace —
// which is exactly the turn somebody opens a turn page to understand — and a
// caller deriving the set from the rows it was given loses any trace whose
// events fell in the middle a capped read dropped. One button labelled
// "trace" then leads to half the story with nothing saying a second half
// exists.
//
// A DISTINCT walk of the same (turn_id, event_time, event_id) index the read
// walked, bounded by the same history window, so it is a seek over one turn's
// range rather than a scan. Ordered by first appearance, because that is the
// order a reader follows them in: the trace the turn started under comes
// first.
func (l *EventLog) TurnTraces(ctx context.Context, turnID string) ([]string, error) {
	rows, err := l.db.sql.QueryContext(ctx,
		"SELECT trace_id, MIN(event_time) AS first_at FROM crewlet_events "+
			"WHERE turn_id = ? AND event_time >= ? AND trace_id != '' "+
			"GROUP BY trace_id ORDER BY first_at ASC, trace_id ASC",
		turnID, EncodeTime(now().Add(-EventHistory)))
	if err != nil {
		return nil, fmt.Errorf("store: read the traces of turn %s: %w", turnID, err)
	}
	defer func() { _ = rows.Close() }()

	out := []string{}
	for rows.Next() {
		var id string
		var first int64
		if err := rows.Scan(&id, &first); err != nil {
			return nil, fmt.Errorf("store: scan the traces of turn %s: %w", turnID, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the traces of turn %s: %w", turnID, err)
	}
	return out, nil
}

// TraceEventCount is the same question about a trace. See [EventLog.TurnEventCount].
func (l *EventLog) TraceEventCount(ctx context.Context, traceID string) (int, error) {
	return l.countEvents(ctx, "trace_id", traceID)
}

// countEvents counts one id's rows inside the history window.
//
// The COLUMN is chosen from this file's own two callers and never from a
// parameter a request can reach: it is interpolated into the statement, which
// is the one place in this package where that would be an injection rather
// than a convenience.
func (l *EventLog) countEvents(ctx context.Context, column, id string) (int, error) {
	switch column {
	case "turn_id", "trace_id":
	default:
		return 0, fmt.Errorf("store: count events: %q is not a countable column", column)
	}
	var n int
	err := l.db.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM crewlet_events WHERE "+column+" = ? AND event_time >= ?",
		id, EncodeTime(now().Add(-EventHistory))).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count events: %w", err)
	}
	return n, nil
}

// TurnClosing returns the NEWEST rows of one turn, oldest first among
// themselves, with their payloads.
//
// For the one thing a capped [EventLog.Turn] read loses. That read is ordered
// oldest first, because a turn is read forwards — so what a long turn loses is
// its ENDING, and the ending is where `agent_turn_completed` and
// `turn_completed` are: the two records a reader takes the outcome, the wall
// clock and the plan summary from. A view that shows a turn's opening and
// cannot say how it ended has lost the one thing somebody opened it for, and
// it looks exactly like a turn that never finished.
//
// Deliberately NOT folded into Turn. Turn answers "this turn, forwards, up to
// a bound", which is a log's own shape and what its other callers want; a
// single method that silently returned a head and a tail would make every
// caller's row count stop meaning what it says, and there would be no honest
// place left to state the gap. This answers "and how did it end", which is a
// question about the VIEW, so the view is what asks it and the view is what
// merges the two.
//
// Oldest first among themselves, so a caller appends rather than reverses.
// The rows may OVERLAP the head read on a turn that only just reached the cap
// — they are the same rows from the other end — so a caller merges on the
// event id rather than concatenating.
func (l *EventLog) TurnClosing(ctx context.Context, turnID string, limit int) ([]EventRecord, error) {
	if limit <= 0 {
		// ALLOCATED, like every other list read here — asking for no rows is
		// still a read that succeeded, and the contract on [EventLog] does
		// not have a second exception. Reachable only from a caller passing
		// a computed limit; today's one caller passes a constant.
		return []EventRecord{}, nil
	}
	out, err := l.scanPayloads(ctx,
		"SELECT "+listColumns+", payload FROM crewlet_events "+
			"WHERE turn_id = ? AND event_time >= ? "+
			"ORDER BY event_time DESC, event_id DESC LIMIT ?",
		turnID, EncodeTime(now().Add(-EventHistory)), limit)
	if err != nil {
		return nil, err
	}
	slices.Reverse(out)
	return out, nil
}

// ByID returns one event WITH its payload, or ErrNotFound.
//
// The identity is (event_time, event_id) and a caller holding only an id — a
// link, a line pasted from a log — has no time to seek with, so this reads the
// id index and takes the newest match.
//
// THROUGH THE SHARED SCANNER although it wants one row, which is what
// QueryRow would give it more directly. A second hand-written Scan is a second
// copy of the agreement between `listColumns` and the destination list, and
// that is precisely the drift [EventLog.scanPayloads] exists to prevent: the
// `work_key` promotion added a column to the list, every listing picked it up,
// and this reader kept a twelve-argument Scan that failed at RUNTIME — on the
// one read a person reaches by pasting an id. LIMIT 1 makes the slice at most
// one row, so the cost is one allocation on a path that serves a link.
func (l *EventLog) ByID(ctx context.Context, id string) (EventRecord, error) {
	recs, err := l.scanPayloads(ctx,
		"SELECT "+listColumns+", payload FROM crewlet_events "+
			"WHERE event_id = ? ORDER BY event_time DESC LIMIT 1", id)
	if err != nil {
		return EventRecord{}, fmt.Errorf("store: read event %s: %w", id, err)
	}
	if len(recs) == 0 {
		return EventRecord{}, fmt.Errorf("%w: event %s", ErrNotFound, id)
	}
	return recs[0], nil
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
		var tagJSON, payload string
		dest := []any{&micros, &rec.ID, &rec.Type, &rec.Source,
			&rec.Category, &rec.Summary, &rec.Actor, &rec.TraceID,
			&rec.SpanID, &rec.ParentSpanID, &tagJSON, &rec.WorkKey}
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

	// AgentRole restricts the rollup to one seat. Empty is the whole org.
	AgentRole string

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
}

// Window reports the instants this query actually covers, after the floor.
//
// Exported because the CALLER labels the answer: a rollup headed with the
// window that was asked for, over rows from the window that was served, is a
// lie about the numbers beside it — and the clamp lives here, where the floor
// is defined, rather than being re-derived at every surface.
//
// TOTAL IN BOTH EDGES: an unbounded top edge means "up to now", and what this
// reports is now rather than the zero time. It answered the zero once and
// every caller wrote the same three lines back — an axis must run to now
// rather than to the newest record, so a company quiet for six hours has six
// empty buckets rather than a chart that stops where the spending did, and a
// rollup must say which instant it counted through. Two copies of one rule is
// how the chart and the figures above it come to disagree about where a
// window ends.
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

// The price is the one value here still read out of the PAYLOAD, and
// deliberately: it is set by a single backend on a minority of phases, so
// promoting it would be a migration and a column that is NULL on almost every
// row of the table. The extraction is free of a scan cost the filter does not
// already pay — the event_type and event_time predicates are what choose the
// rows, and json_extract runs only on the ones they keep.
const phaseTokenSQL = `
SELECT event_time, event_id, agent_id, agent_role,
       phase, host_phase, worker, model, turn_id, work_key, iteration,
       input_tokens, output_tokens, total_tokens,
       COALESCE(json_extract(payload, '$.cost_usd'), 0)
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
WHERE event_type = 'agent_phase_completed' AND event_time >= ?`

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
	args := []any{EncodeTime(now().Add(-EventHistory))}
	// ONLY THE IDENTIFIERS THE CALLER ACTUALLY HAS.
	//
	// It was `(agent_id = ? OR agent_role = ?)` with both bound
	// unconditionally, so an EMPTY one matched every row that carries
	// none: a handle the roster could not resolve to a role asked for that
	// seat's phases and was answered every non-agent event in the window.
	// The guard above catches only the case where BOTH are empty.
	clause, ids := seatClause(agentID, agentRole)
	query += clause
	args = append(args, ids...)
	if before != nil && before.ID != "" {
		query += agentPhaseCursorSQL
		args = append(args, EncodeTime(before.Time), before.ID)
	}
	query += agentPhaseOrderSQL
	args = append(args, AgentPhaseLimit)
	return l.scanPayloads(ctx, query, args...)
}

// seatClause narrows to a seat by whichever identifier the caller holds.
//
// A caller passes the handle-derived agent id, the role name, or both — the
// roster carries one and the projection keys on the other — and binding an
// EMPTY one is how a filter turns into a match on every row that has none.
// Returns an empty clause when the caller holds neither, which its own callers
// treat as "no seat named" rather than "every seat".
func seatClause(agentID, agentRole string) (string, []any) {
	var terms []string
	var args []any
	if agentID != "" {
		terms = append(terms, "agent_id = ?")
		args = append(args, agentID)
	}
	if agentRole != "" {
		terms = append(terms, "agent_role = ?")
		args = append(args, agentRole)
	}
	if len(terms) == 0 {
		return "", nil
	}
	return " AND (" + strings.Join(terms, " OR ") + ")", args
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
	since, until := q.Window(now())

	// BOTH EDGES, ALWAYS, and the top one EXCLUSIVE — matching the
	// half-open window the bucketing folds over, so a record on the
	// boundary belongs to exactly one of two adjacent windows. Applied even
	// for a caller that named no top edge, because [PhaseTokenQuery.Window]
	// reports that as now and the rows have to be the rows the label
	// claims: a phase stamped in the future by a skewed clock inside a
	// window headed "counted through now" is a number with no window.
	sql := phaseTokenSQL + " AND event_time < ?"
	args := []any{EncodeTime(since), EncodeTime(until)}
	if q.AgentRole != "" {
		sql += " AND agent_role = ?"
		args = append(args, q.AgentRole)
	}
	// Newest first, which is the order the breakdown renders in, and the
	// order a Limit keeps the head of. No LIMIT unless the caller asked for
	// a tail: see the note where the rollup's cap used to be.
	sql += " ORDER BY event_time DESC, event_id DESC"
	if q.Limit > 0 {
		sql += " LIMIT ?"
		args = append(args, q.Limit)
	}

	rows, err := l.db.sql.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: phase tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []tokens.Record{}
	for rows.Next() {
		var (
			at  int64
			rec tokens.Record
		)
		if err := rows.Scan(&at, &rec.EventID, &rec.AgentID, &rec.AgentRole,
			&rec.Phase, &rec.HostPhase, &rec.Worker, &rec.Model,
			&rec.TurnID, &rec.WorkKey, &rec.Iteration,
			&rec.InputTokens, &rec.OutputTokens, &rec.TotalTokens,
			&rec.CostUSD,
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
