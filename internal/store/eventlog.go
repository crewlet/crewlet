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

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
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

	// EventPurgeBatch bounds how many rows one purge statement deletes, and
	// EventPurgeBytes how many bytes of payload, whichever is reached first —
	// so no single statement holds the writer for long; see Purge for why
	// that matters to the inline event Append.
	//
	// TWO BOUNDS, because a row's cost has two parts. Every row is an entry
	// in each of the table's indexes, which is what the row bound prices:
	// measured, five hundred rows of twenty kilobytes deleted in 30–50 ms.
	// And a row's payload is pages to free, which the row bound cannot see:
	// a row is up to one whole event — a phase record published whole, or
	// a part of one published cut — and parts of about 8.3 MB measured at
	// 5–10 ms each, so five hundred of them in one statement would hold the
	// writer for seconds, past the busy timeout an inline Append waits
	// under. Thirty-two MiB of payload is four such parts, the same tens of
	// milliseconds the row bound buys. A row larger than the byte bound is
	// deleted in a statement of its own, since a statement takes one row at
	// least. Exported for the contract suite, which proves the sweep drains
	// a backlog wider than one batch.
	EventPurgeBatch = 500

	// EventPurgeBytes is the byte half of the purge's bound: the payload one
	// statement deletes, however few rows it takes. See EventPurgeBatch for
	// both halves and what each was measured at.
	EventPurgeBytes = 32 << 20

	// eventPartyPurgeBatch bounds how many rows of the party index one purge
	// statement deletes. Those rows are a party name and a key and nothing
	// else, so the row bound is the whole of their cost: ten thousand of them
	// measured at 45–55 ms, the same tens of milliseconds as an event batch,
	// and a multi-day overhang — several party rows for every event — still
	// goes in bounded statements.
	eventPartyPurgeBatch = 10_000

	// MaxTraceEvents caps one trace's rows. A trace is unbounded in
	// principle — a long turn with sub-agents accumulates thousands of
	// spans — and the whole thing goes out in a single WebSocket frame, so
	// an uncapped read is a query that times out client-side and reports as
	// a generic failure. The OLDEST rows are kept: the root is what
	// explains a trace, and a truncated tail is legible where a truncated
	// head is not.
	//
	// The rows past it are NOT gone: [EventLog.Trace] says where they are.
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
// Payload is populated by the reads that show a record whole — ByID, ByKey,
// Turn, TurnClosing, AgentPhases and Phases. List and Trace deliberately leave
// it nil: they never select the column, because thirty days of serialized
// events is a large amount of JSON to move for a listing that shows a summary
// line.
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
	// under, and Worker names the `workers:` template a "subagent" phase
	// ran — the worker rollup keys on the pair (internal/tokens).
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

// ErrHalfCursor is returned for a [Cursor] that names no row id.
//
// REFUSED, because both ways of serving it are wrong. Read as its time alone it
// is a strict comparison on a key that is not unique, so every row sharing the
// boundary's instant is on no page; read as no cursor at all it answers "the
// next page" with the first one, and a pager following it never ends.
var ErrHalfCursor = errors.New("store: a cursor needs the row's id as well as its time")

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
	// It over-fetches and merges, so a page shorter than Limit does NOT
	// mean history is exhausted here; only a zero-row page does. Both
	// reads behind such a page take the same [ListQuery.window] and the
	// same [ListQuery.cursor], so a walk returns no row twice and still
	// ends — see [EventLog.traceSiblings] for what is deliberately NOT
	// shared between them.
	//
	// It does NOT return every row. A trace sibling the page's cut dropped
	// can be on no page of the walk at all; [cutToPage] says which rows
	// those are and which read returns them.
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

	// Before is an exclusive cursor. Nil starts at the newest row; one with
	// no id is refused with [ErrHalfCursor].
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
	// [RecordFor] sets it from the one decode it already makes, and the
	// event writer builds every row it writes through RecordFor, so this
	// derives a spend only for an agent_phase_completed row assembled by
	// hand, which only tests assemble. The webhook receiver assembles its
	// rows by hand too, but a delivery is not a phase completion, and
	// [SpendFor] derives nothing for any other type.
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

	where, args = q.window(col)
	// STORAGE IS NOT AN EVENT. A type events.Unlisted names is written only
	// as a piece of another row — a phase record's whole, in parts nearly as
	// large as one event — and carries no category, source, actor or tag, so
	// every filter above and below that could exclude it is one a caller may
	// leave empty. Refused BY TYPE here, once, because this predicate is what
	// the listing, its histogram and its tallies share.
	for _, unlisted := range events.UnlistedTypes() {
		where = append(where, col("event_type")+" != ?")
		args = append(args, unlisted)
	}
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

// window is the span of time a read covers: the history floor the log keeps,
// and the caller's own half-open [Since, Until) on top of it.
//
// THE WINDOW, half-open, on the same column the keyset walks — so it narrows
// the index range the read already scans rather than adding a term the planner
// has to filter on.
//
// ITS OWN METHOD BECAUSE TWO READS ANSWER ONE PAGE. The direct read and
// [EventLog.traceSiblings] are merged and cut into a single answer, so a bound
// applied to one of them is not a narrower page — it is a page holding rows
// from outside the span the reader asked for. And because the merge sorts
// newest-first and then cuts, a sibling from ABOVE `Until` does not merely
// appear beside the matches: it displaces them.
func (q ListQuery) window(col func(string) string) (where []string, args []any) {
	where = []string{col("event_time") + " >= ?"}
	args = []any{EncodeTime(now().Add(-EventHistory))}
	if !q.Since.IsZero() {
		where = append(where, col("event_time")+" >= ?")
		args = append(args, EncodeTime(q.Since))
	}
	if !q.Until.IsZero() {
		where = append(where, col("event_time")+" < ?")
		args = append(args, EncodeTime(q.Until))
	}
	return where, args
}

// cursor is where a page resumes: nothing at all when the caller is asking for
// the newest rows.
//
// Keyset, not OFFSET: (event_time, event_id) is the primary key, so it is
// unique and already in index order — no sort node, and no drift as new rows
// land at the head while a reader pages backwards.
//
// SHARED BY BOTH READS BEHIND ONE PAGE, and that is what makes the walk
// terminate rather than being a tidiness argument. Without it on the sibling
// read, every page after the first merged in rows NEWER than the caller's
// cursor, the sort put them at the head, the cut kept them, and the page came
// back holding rows the walk had already passed — measured on a real store as
// the same ten rows on pages one through five with the cursor frozen, the
// other fifty rows of the trace returned by no page at all, and the
// dashboard's "load more" looping for ever because [ListQuery.RelatedAgent]
// says only a zero-row page ends the walk.
//
// Separate from [ListQuery.window] because the two are different facts with
// different lifetimes — the window is what the reader asked for and does not
// move, the cursor moves with every page — which is the same reason
// [ListQuery.predicate] does not carry it.
//
// Always the PAIR: [EventLog.List] refuses a cursor without its id before it
// gets here, with [ErrHalfCursor].
func (q ListQuery) cursor(col func(string) string) (where []string, args []any) {
	if q.Before == nil {
		return nil, nil
	}
	return []string{"(" + col("event_time") + ", " + col("event_id") + ") < (?, ?)"},
		[]any{EncodeTime(q.Before.Time), q.Before.ID}
}

// List returns a page of events, newest first, ordered by (time, id)
// descending.
func (l *EventLog) List(ctx context.Context, q ListQuery) ([]EventRecord, error) {
	if q.Before != nil && q.Before.ID == "" {
		return nil, ErrHalfCursor
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}

	from, where, args, col := q.predicate()
	joined := q.RelatedAgent != ""

	cursorWhere, cursorArgs := q.cursor(col)
	where = append(where, cursorWhere...)
	args = append(args, cursorArgs...)
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
		siblings, err := l.traceSiblings(ctx, q, out, limit)
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
//
// # It answers the SAME QUESTION as the direct read about time
//
// THE CURSOR AND THE WINDOW ARE THE CALLER'S OWN, through
// [ListQuery.cursor] and [ListQuery.window], because the two reads are merged
// and cut into one page: a bound that held on one of them and not the other
// is not a wider answer, it is a broken one. The cursor is the load-bearing
// half — see its doc for the paging failure that carrying it fixes — and the
// window is what stops a reader who scrubbed to a time range being shown
// rows from outside it, which the cut would let displace the matches inside
// it.
//
// WHAT IS DELIBERATELY NOT SHARED is everything else [ListQuery.predicate]
// compiles: the party join, and the type / source / category / actor / turn /
// work-key filters. Those describe WHICH EVENTS the reader wants, and the
// cause is by construction none of them — it involves another party, and it
// carries another type and another category, which is exactly why it had to
// be fetched separately. Applying them here would delete the only row this
// query exists to add. The cursor and the window are not that kind of filter:
// one is where the page resumes and the other is the span the page covers,
// and both are on the column the merge ORDERS and CUTS on.
//
// # The budget
//
// THE LIMIT IS THE PAGE'S OWN, and that is the exactly sufficient budget
// rather than a number reused for want of a better one. The page [mergeRelated]
// builds is the newest `limit` rows of (direct matches ∪ their traces' rows),
// and this query returns the newest `limit` rows of those traces — which is a
// superset of every sibling that could be in that answer, since the direct
// matches themselves live in the same traces and satisfy the same window and
// the same cursor. Reading MORE would only add rows older than the cut and
// change nothing; reading FEWER could leave a sibling out of a page it
// belonged on. A per-trace cap would be the wrong shape for the same reason:
// the page is chosen across all of them at once, not fairly between them.
//
// The siblings it does not reach are older than what it did return, and what
// becomes of them is [cutToPage]'s subject: a sibling below the page's cut
// may be returned by no page at all, and is reached by the trace id of a row
// the caller was given — [cutToPage] says through which reads, and is the one
// place that says it. A sibling NEWER than the cursor is in the same position
// for the same reason: it was a candidate on the page the cursor came from, so
// it is on that page or reached the same way.
func (l *EventLog) traceSiblings(ctx context.Context, q ListQuery, direct []EventRecord,
	limit int,
) ([]EventRecord, error) {
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
	// This read never joins the party table, so no column needs
	// qualifying — the shared terms are built against the log's own names.
	plain := func(name string) string { return name }
	where := []string{"trace_id IN (?" + strings.Repeat(",?", len(traces)-1) + ")"}
	args := append([]any(nil), traces...)
	windowWhere, windowArgs := q.window(plain)
	where = append(where, windowWhere...)
	args = append(args, windowArgs...)
	cursorWhere, cursorArgs := q.cursor(plain)
	where = append(where, cursorWhere...)
	args = append(args, cursorArgs...)
	args = append(args, limit)

	// Every fragment joined into `where` is a compile-time constant; each
	// one's value travels as a bound parameter in args.
	query := "SELECT " + listColumns + " FROM crewlet_events WHERE " +
		strings.Join(where, " AND ") +
		" ORDER BY event_time DESC, event_id DESC LIMIT ?"
	return l.scanRows(ctx, query, args...)
}

// mergeRelated folds the siblings into the direct matches, newest first, and
// cuts the result to one page.
//
// The cut is [cutToPage], and its doc is where the reason lives: the page has
// to end at the row the caller's next cursor resumes from, and what that
// leaves behind is reachable by the trace id of a row the caller was given —
// [cutToPage] says through which reads.
func mergeRelated(direct, siblings []EventRecord, limit int) []EventRecord {
	// DEDUPLICATED ON THE PRIMARY KEY, (event_time, event_id). A direct match
	// is in its own trace, so the sibling read returns it again and one copy
	// has to go — but the id alone does not name a row, and a key narrower
	// than the table's drops a DISTINCT row for sharing an id with one
	// already on the page. Microseconds rather than the time.Time, because
	// that is exactly what the column holds and a time.Time is not a safe
	// map key.
	type key struct {
		at int64
		id string
	}
	seen := make(map[key]struct{}, len(direct)+len(siblings))
	out := make([]EventRecord, 0, len(direct)+len(siblings))
	for _, group := range [][]EventRecord{direct, siblings} {
		for _, rec := range group {
			k := key{EncodeTime(rec.Time), rec.ID}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
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
	return cutToPage(out, limit)
}

// Trace returns a trace's OLDEST [MaxTraceEvents] rows inside the history
// window, oldest first, because a trace is read as a causal sequence rather
// than a feed.
//
// WHETHER THE READ WAS CUT is [EventLog.TraceEventCount]'s to say, not the row
// count's: a trace of exactly MaxTraceEvents rows holds every row it has, and
// a caller that read a full page as a cut one would mark a complete trace
// short.
//
// WHERE THE REST IS. A cut leaves out the trace's NEWEST rows, and
// [EventLog.List] with [ListQuery.TraceID] reads that same trace newest first,
// paged by [ListQuery.Before], under the same history window and with no cap on
// the walk. The two orders are mirror images of one (time, id) order, so that
// walk, stopped at the last row this read returned — matched on its time AND
// its id, since the id alone is not unique — has by then returned every row of
// the trace newer than it, which is what this read stopped short of.
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
// WHETHER THE READ WAS CUT is [EventLog.TurnEventCount]'s to say, not the row
// count's, for the reason that method gives. A caller whose read WAS cut reads
// [EventLog.TurnClosing] beside it: because this read is ordered forwards, what
// a cut loses is the turn's own ENDING, which is where the records a reader
// came for live. The cap is the trace's, for the same reason: a turn that has
// self-iterated many times is the one worth reading, and a bound low enough to
// cut it short would hide exactly that.
//
// WHERE THE MIDDLE IS. What neither this read nor TurnClosing returns is on
// [EventLog.List] with [ListQuery.TurnID], which reads the same turn newest
// first, paged by [ListQuery.Before], under the same history window and with no
// cap on the walk. A listing carries no payload, so a row reached that way is
// read whole by [EventLog.ByKey], with the time and id the listing gave it —
// not by [EventLog.ByID], which answers the newest row carrying the id and so
// can hand back another row's payload.
func (l *EventLog) Turn(ctx context.Context, turnID string) ([]EventRecord, error) {
	return l.scanPayloads(ctx,
		"SELECT "+listColumns+", payload FROM crewlet_events "+
			"WHERE turn_id = ? AND event_time >= ? "+
			"ORDER BY event_time ASC, event_id ASC LIMIT ?",
		turnID, EncodeTime(now().Add(-EventHistory)), MaxTurnEvents)
}

// MaxTurnEvents bounds one turn's read. [EventLog.Turn] says what a cut leaves
// out and where it is.
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
// — they are the same rows from the other end — so a caller merges rather than
// concatenating, and merges on (event_time, event_id), the table's primary
// key. Not on the id alone: nothing constrains it to one row, and a merge on
// it drops a distinct row for sharing an id with one already held.
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
// link, a line pasted from a log — has no time to seek with, so this reads
// every row carrying the id and answers the newest. That is a guess whenever
// the id is shared: the primary key is the pair, and nothing in the schema
// constrains the id alone. A caller that got the row from a listing holds both
// halves and reads it with [EventLog.ByKey] instead.
func (l *EventLog) ByID(ctx context.Context, id string) (EventRecord, error) {
	return l.one(ctx, "event "+id, byIDWhere, id)
}

// ByKey returns the one event at (at, id) WITH its payload, or ErrNotFound.
//
// THE ROW A LISTING NAMED, which [EventLog.ByID] cannot promise: it holds only
// the id and answers the newest row carrying it, so a caller that took the id
// off a listing row is handed ANOTHER row's payload whenever an id is shared.
// Every row a listing returns carries its [EventRecord.Time] and
// [EventRecord.ID], and this reads by both.
//
// No history window, like ByID: both read a row the caller already names.
func (l *EventLog) ByKey(ctx context.Context, at time.Time, id string) (EventRecord, error) {
	return l.one(ctx, "event "+id+" at "+at.UTC().Format(time.RFC3339Nano),
		byKeyWhere, EncodeTime(at), id)
}

// pointReadSQL is the select [EventLog.one] reads a row through, before its
// WHERE.
const pointReadSQL = "SELECT " + listColumns + ", payload FROM crewlet_events "

// byIDWhere and byKeyWhere are the point reads' WHEREs.
//
// NEITHER ORDERS, and that is what keeps a point read a point read. Measured
// with EXPLAIN QUERY PLAN, `WHERE event_id = ?` is a SEARCH on
// crewlet_events_id_idx (event_id=?), and so is the key read, whose instant
// is then checked on the id's rows. The same read ordered by event_time DESC
// and limited to one — the obvious way to ask for the newest — planned as a
// SCAN of the primary key's own index, walked newest first until a row
// matched: linear in the table, 0.34 ms at three thousand rows and 1.8 ms at
// twenty thousand, against about 0.1 ms for the search at either.
// [EventLog.one] picks the newest of an id's rows instead.
// TestAPointReadIsAnIndexSearch holds both plans.
const (
	byIDWhere  = "WHERE event_id = ?"
	byKeyWhere = "WHERE event_time = ? AND event_id = ?"
)

// one is the point read ByID and ByKey share: the NEWEST of the rows a WHERE
// names, with its payload.
//
// THROUGH THE SHARED SCANNER although it wants one row, which is what
// QueryRow would give it more directly. A second hand-written Scan would be a
// second copy of the agreement between `listColumns` and the destination
// list, which is the drift [EventLog.scanPayloads] exists to prevent: a column
// added to the list reaches every read through it, and a read holding its own
// Scan fails at run time. A WHERE here names one row unless an id is shared,
// so the cost is one allocation on a path that serves a link.
func (l *EventLog) one(ctx context.Context, what, where string, args ...any) (EventRecord, error) {
	recs, err := l.scanPayloads(ctx, pointReadSQL+where, args...)
	if err != nil {
		return EventRecord{}, fmt.Errorf("store: read %s: %w", what, err)
	}
	if len(recs) == 0 {
		return EventRecord{}, fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	newest := recs[0]
	for _, rec := range recs[1:] {
		if rec.Time.After(newest.Time) {
			newest = rec
		}
	}
	return newest, nil
}

// PhaseWhole is one phase record whole, as [EventLog.PhaseRecordWhole] reads it.
type PhaseWhole struct {
	// Payload is the record's event exactly as it would be stored had it
	// been published whole — the same bytes a row's payload column holds.
	Payload json.RawMessage

	// Parts is how many parts it was reassembled from: zero when the
	// record's own row is the whole.
	Parts int
}

// MissingWholeError reports a phase record published cut whose whole this
// store cannot put back together: the parts it holds, contiguous from the
// first byte, stop short of the whole's length.
//
// TYPED, because the reasons send a reader to different places and the
// numbers are the answer: how much of the whole is here, and of how much.
// Kept and Recorded say which reason it is, as far as this store can tell.
type MissingWholeError struct {
	// ID is the phase record's own event id.
	ID string

	// FoundBytes is how much of the whole the parts here cover, contiguous
	// from its first byte, in Parts parts.
	FoundBytes, Parts int

	// WholeBytes is the whole's length, as the record's row states it or,
	// with no row, as its first part does. Zero when neither is here.
	WholeBytes int

	// Kept says the record's row states that every part was published, so
	// the parts that are missing are not in this store: a part whose write
	// failed on this node was logged there as event_write_failed, and the
	// retention sweep deletes in batches and can stop between two. False
	// with a row: the parts were never all published, and the record's
	// notes say why.
	Kept bool

	// Recorded says the record's own row is in this store. Without it
	// nothing here says whether every part was published: a part whose
	// publish failed was logged on this node as phase_record_whole_not_kept,
	// and one that was published can be missing for either reason Kept
	// gives.
	Recorded bool
}

func (e *MissingWholeError) Error() string {
	switch {
	case e.Kept:
		return fmt.Sprintf("store: phase record %s: this store holds %d of the %d bytes of its "+
			"whole, in %d parts, and the record says every part was published — the rest "+
			"is not in this store: a part write that failed here (event_write_failed), or "+
			"the retention sweep", e.ID, e.FoundBytes, e.WholeBytes, e.Parts)
	case e.Recorded:
		return fmt.Sprintf("store: phase record %s was published cut and its whole (%d bytes) "+
			"was not kept — the record's notes say why; %d bytes of it are here, in %d parts",
			e.ID, e.WholeBytes, e.FoundBytes, e.Parts)
	default:
		return fmt.Sprintf("store: phase record %s has no row in this store, and the parts "+
			"of its whole here stop at byte %d of %d: a part's publish failed "+
			"(phase_record_whole_not_kept), or the rest is not in this store (a part write "+
			"that failed here, event_write_failed, or the retention sweep)",
			e.ID, e.FoundBytes, e.WholeBytes)
	}
}

// phaseRecordRowSQL is the record's own rows and what each says about its
// whole: every row carrying the id, the newest of which [newestRow] keeps,
// for the reason [EventLog.ByID] answers the newest. The two integers
// are read in SQL so the payload is decoded only by the reader it is handed
// to.
//
// NO ORDER BY, for the reason [byIDWhere] gives. Measured with EXPLAIN QUERY
// PLAN, this is a SEARCH on crewlet_events_id_idx (event_id=?), the type
// checked on the id's rows. Ordered by event_time DESC and limited to one, it
// planned as a MULTI-INDEX AND over that index and
// crewlet_events_type_time_idx with a sorter behind it, linear in the rows of
// the type: 0.43 ms at three thousand phase records and 2.3 ms at twenty
// thousand, against about 0.1 ms for the search at either.
// TestAPointReadIsAnIndexSearch holds the plan.
const phaseRecordRowSQL = `
SELECT event_time, payload,
       COALESCE(json_extract(payload, '$.whole_bytes'), 0),
       COALESCE(json_extract(payload, '$.whole_parts'), 0)
  FROM crewlet_events
 WHERE event_id = ? AND event_type = ?`

// phasePart is a part's wire type, taken from the payload type itself for the
// reason [phaseCompleted] is.
var phasePart = types.AgentPhaseRecordPart{}.EventType()

// phaseRecordPartSQL is one part's rows, the newest of which [newestRow]
// keeps. Measured with EXPLAIN QUERY PLAN, a SEARCH on the id index too;
// ordered and limited to one, it planned the MULTI-INDEX AND and the sorter
// [phaseRecordRowSQL] did, linear in the rows of the part type. A part's id
// is derived from the record's ([types.PhaseRecordPartID]), which is what
// lets this read need no index of its own.
const phaseRecordPartSQL = `
SELECT event_time, payload
  FROM crewlet_events
 WHERE event_id = ? AND event_type = ?`

// newestRow runs a point read and keeps the newest of its rows, reporting
// false when there is none. scan reads one row into a value of its own and
// returns the row's event_time beside it.
//
// ONE ROW KEPT AT A TIME: each row is scanned into a fresh value, and a row
// older than the one kept is dropped as soon as it is read, so at most two
// rows' payloads are held at once — a part's is up to nearly as large as one
// event may be.
func newestRow[T any](ctx context.Context, db *sql.DB, query string, args []any,
	scan func(*sql.Rows) (int64, T, error),
) (T, bool, error) {
	var (
		kept  T
		at    int64
		found bool
	)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return kept, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		when, v, err := scan(rows)
		if err != nil {
			return kept, false, err
		}
		if !found || when > at {
			kept, at, found = v, when, true
		}
	}
	return kept, found, rows.Err()
}

// phaseRow is a phase record's own row, as [phaseRecordRowSQL] reads it.
type phaseRow struct {
	payload                []byte
	wholeBytes, wholeParts int
}

// PhaseRecordWhole returns one phase record WHOLE.
//
// A record published whole is its own row, and that row's payload is the
// answer. A record the transport refused as too large was published CUT, with
// `whole_bytes` saying so, and its whole was published before it as parts
// ([types.AgentPhaseRecordPart]) under ids derived from the record's own — the
// answer is those parts reassembled, verified contiguous from the first byte
// to the whole's length. The parts answer on their own when this store holds
// no row for the record — every cut form of it refused, or its publish or its
// write failing where its parts' did not — because the first of them states
// the whole's length and every later one is held to it.
//
// ONE PART AT A TIME, through the id index: every part but the last is up to
// nearly as large as one event may be, so a batched read would hold several of
// them beside the whole being assembled.
//
// No history window, like [EventLog.ByID]: this reads rows the caller names,
// and the parts share the record's lifetime — the retention sweep takes them
// on the same horizon as every row.
//
// [ErrNotFound] when this store holds neither the record's row nor any part of
// it; [*MissingWholeError] when it holds a cut record, or parts, that do not
// reach the whole.
func (l *EventLog) PhaseRecordWhole(ctx context.Context, id string) (PhaseWhole, error) {
	row, recorded, err := newestRow(ctx, l.db.sql, phaseRecordRowSQL, []any{id, phaseCompleted},
		func(rows *sql.Rows) (int64, phaseRow, error) {
			var (
				at int64
				r  phaseRow
			)
			err := rows.Scan(&at, &r.payload, &r.wholeBytes, &r.wholeParts)
			return at, r, err
		})
	switch {
	case err != nil:
		return PhaseWhole{}, fmt.Errorf("store: read phase record %s: %w", id, err)
	case recorded && row.wholeBytes == 0:
		return PhaseWhole{Payload: row.payload}, nil
	}
	record, err := uuid.Parse(id)
	switch {
	case err != nil && !recorded:
		// A part's id is derived from its record's UUID, so an id that is
		// not one names no part, and with no row it names nothing here.
		return PhaseWhole{}, fmt.Errorf("%w: phase record %s", ErrNotFound, id)
	case err != nil:
		// A ROW THAT STATES A WHOLE IN PARTS UNDER AN ID NO PART CAN BE
		// DERIVED FROM. No build of this engine writes one — every event id
		// it mints is a UUID — but the row is data this read did not write,
		// so it is answered as what it is: a record whose parts cannot be
		// named, which says nothing about whether they were published.
		return PhaseWhole{}, fmt.Errorf("store: phase record %s states a whole of %d bytes in "+
			"parts, and its parts' ids are derived from a record id that is a UUID, which "+
			"%q is not: %w", id, row.wholeBytes, id, err)
	}
	whole, parts, stated, err := l.phaseRecordParts(ctx, record, row.wholeBytes)
	if err != nil {
		return PhaseWhole{}, err
	}
	wholeBytes := row.wholeBytes
	if wholeBytes == 0 {
		wholeBytes = stated
	}
	switch {
	case parts == 0 && !recorded:
		return PhaseWhole{}, fmt.Errorf("%w: phase record %s", ErrNotFound, id)
	case parts == 0 || len(whole) < wholeBytes:
		return PhaseWhole{}, &MissingWholeError{ID: id, FoundBytes: len(whole), Parts: parts,
			WholeBytes: wholeBytes, Kept: row.wholeParts > 0, Recorded: recorded}
	}
	return PhaseWhole{Payload: whole, Parts: parts}, nil
}

// phaseRecordParts reads a record's parts in order and assembles them, stopping
// at the first part this store does not hold or at the whole's length.
//
// want is the whole's length as the record's row states it, or zero when there
// is no row, in which case the first part's statement is taken and every later
// part is held to it. It returns the bytes assembled, how many parts they came
// from and the length the parts state.
//
// A part that does not continue the whole — another record's, out of order,
// overlapping, past the end, or stating another length — is REFUSED rather than
// skipped: assembled bytes that are not the whole are worse than none, because
// nothing downstream could tell.
func (l *EventLog) phaseRecordParts(ctx context.Context, record uuid.UUID, want int) ([]byte, int, int, error) {
	var whole []byte
	for index := 0; ; index++ {
		if want > 0 && len(whole) == want {
			return whole, index, want, nil
		}
		raw, found, err := newestRow(ctx, l.db.sql, phaseRecordPartSQL,
			[]any{types.PhaseRecordPartID(record, index).String(), phasePart},
			func(rows *sql.Rows) (int64, []byte, error) {
				var (
					at   int64
					data []byte
				)
				err := rows.Scan(&at, &data)
				return at, data, err
			})
		if err != nil {
			return nil, 0, 0, fmt.Errorf("store: read part %d of phase record %s: %w", index, record, err)
		}
		if !found {
			return whole, index, want, nil
		}
		var part types.AgentPhaseRecordPart
		if err := json.Unmarshal(raw, &part); err != nil {
			return nil, 0, 0, fmt.Errorf("store: decode part %d of phase record %s: %w", index, record, err)
		}
		if want == 0 {
			want = part.WholeBytes
		}
		switch {
		case part.RecordID != record.String(), part.Index != index:
			return nil, 0, 0, fmt.Errorf("store: part %d of phase record %s names record %q "+
				"and index %d: the parts do not describe this record", index, record,
				part.RecordID, part.Index)
		case part.WholeBytes != want:
			return nil, 0, 0, fmt.Errorf("store: part %d of phase record %s states a whole of %d "+
				"bytes where the whole is %d", index, record, part.WholeBytes, want)
		case part.Offset != len(whole), len(part.Data) == 0, part.Offset+len(part.Data) > want:
			return nil, 0, 0, fmt.Errorf("store: part %d of phase record %s covers bytes %d to %d "+
				"where the whole of %d bytes continues at %d: the parts are not contiguous",
				index, record, part.Offset, part.Offset+len(part.Data), want, len(whole))
		}
		whole = append(whole, part.Data...)
	}
}

// Purge deletes events past EventRetention and reports how many went.
//
// It takes no window, deliberately. The retention is derived from the read
// floor and has no honest reason to vary per deployment: a shorter one deletes
// rows the log still serves, and a longer one keeps rows nothing can reach.
// Offering the choice would only make both mistakes possible.
//
// It deletes in batches, each its own autocommit statement, because this is
// the highest-volume table in the deployment and its rows carry whole event
// payloads: one DELETE over a multi-day overhang — a node that was down for
// days comes back to one — holds the single writer for the whole statement,
// and the event Append runs INLINE in the publishing goroutine, which drops
// the event with a warning once busy_timeout runs out. Batching releases the
// writer between statements, so live appends interleave with the catch-up
// instead of losing to it. A batch is bounded by rows and by payload bytes
// (see EventPurgeBatch), and the party index it keeps in step is deleted in
// batches of its own.
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
	for {
		res, err := l.db.sql.ExecContext(ctx, partyPurgeSQL, cutoff, eventPartyPurgeBatch)
		if err != nil {
			return 0, fmt.Errorf("store: purge event parties: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: purge event parties count: %w", err)
		}
		if n < eventPartyPurgeBatch {
			break
		}
	}
	var total int64
	for {
		batch, more, err := l.nextPurgeBatch(ctx, cutoff, EventPurgeBatch, EventPurgeBytes)
		if err != nil {
			return total, fmt.Errorf("store: purge events: %w", err)
		}
		if len(batch.rowids) == 0 {
			return total, nil
		}
		n, err := l.deleteBatch(ctx, cutoff, batch)
		total += n
		if err != nil {
			return total, fmt.Errorf("store: purge events: %w", err)
		}
		if !more {
			return total, nil
		}
	}
}

// partyPurgeSQL deletes one batch of the party index's rows past the cutoff.
const partyPurgeSQL = `DELETE FROM crewlet_event_parties WHERE rowid IN (
	SELECT rowid FROM crewlet_event_parties WHERE event_time < ? LIMIT ?)`

// purgeCandidatesSQL is the oldest rows past the cutoff, in key order, with
// the size of each one's payload.
//
// READ OUTSIDE THE WRITE, as a statement of its own: a payload's size is read
// off its bytes — measured at about as long as deleting the row takes — and a
// read holds no writer. The sizes are computed as the rows are stepped
// through (measured: stepping four of twelve 8.3 MB rows cost what four do),
// so a batch that stops at its byte bound has read no further than the row
// that would have crossed it.
const purgeCandidatesSQL = `SELECT rowid, octet_length(payload) FROM crewlet_events
	WHERE event_time < ? ORDER BY event_time, event_id LIMIT ?`

// purgeBatch is one purge statement's rows.
type purgeBatch struct {
	rowids []any
	// bytes is what their payloads weigh together.
	bytes int64
}

// nextPurgeBatch reads the next batch past cutoff: the oldest rows, in key
// order, as many as fit maxRows rows and maxBytes of payload — and one at
// least, so a row larger than maxBytes goes in a statement of its own rather
// than never. more is false when the rows past the cutoff ran out before
// either bound, so the sweep is over once this batch goes.
func (l *EventLog) nextPurgeBatch(ctx context.Context, cutoff int64, maxRows int, maxBytes int64) (purgeBatch, bool, error) {
	var batch purgeBatch
	rows, err := l.db.sql.QueryContext(ctx, purgeCandidatesSQL, cutoff, maxRows)
	if err != nil {
		return batch, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var rowid, size int64
		if err := rows.Scan(&rowid, &size); err != nil {
			return batch, false, err
		}
		if len(batch.rowids) > 0 && batch.bytes+size > maxBytes {
			// This row starts the next batch.
			return batch, true, nil
		}
		batch.rowids = append(batch.rowids, rowid)
		batch.bytes += size
	}
	if err := rows.Err(); err != nil {
		return batch, false, err
	}
	return batch, len(batch.rowids) == maxRows, nil
}

// deleteBatch deletes one batch's rows.
//
// THE CUTOFF IS CHECKED AGAIN, on the rows the batch names. A rowid is the
// table's own and not the event's identity, nothing in the table's schema
// reserves one once its row is gone (it has no AUTOINCREMENT), and the read
// that chose these rows ran as a statement of its own: the check is what
// makes a rowid that names a newer row by the time this runs a row it leaves
// alone.
func (l *EventLog) deleteBatch(ctx context.Context, cutoff int64, batch purgeBatch) (int64, error) {
	res, err := l.db.sql.ExecContext(ctx, deleteBatchSQL(len(batch.rowids)),
		append([]any{cutoff}, batch.rowids...)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// deleteBatchSQL deletes the rows of a batch of n, by rowid, past the cutoff.
func deleteBatchSQL(n int) string {
	return "DELETE FROM crewlet_events WHERE event_time < ? AND rowid IN (" +
		strings.TrimSuffix(strings.Repeat("?, ", n), ", ") + ")"
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

// cutToPage keeps the newest limit records of a merged set, oldest dropped.
//
// THE CUT IS WHAT KEEPS THE CURSOR HONEST, which is why it is not optional and
// why the obvious alternatives are worse. A caller pages this log by keyset:
// the page's LAST row is the cursor, and the next page asks for rows strictly
// older than it. So the page must END where the walk resumes. Return the
// merged set whole — direct matches plus every trace sibling — and its last
// row is the oldest SIBLING, which can be older than the oldest direct match
// read; the next page then starts before it and silently SKIPS every direct
// match in between, which is a hole rather than a shortening. Cutting only the
// siblings has the same defect for the same reason. Cutting the merged set to
// the newest limit rows is the one shape where every row this page did not
// return is strictly older than its cursor, and therefore still ahead of the
// walk.
//
// THAT LAST SENTENCE HAS A PRECONDITION, and it is not this function's to
// keep: both reads behind the merge must carry the SAME cursor. They do —
// [ListQuery.cursor] is applied to the direct read and to
// [EventLog.traceSiblings] alike — and without it the arithmetic here is not
// merely weaker but inverted: a sibling NEWER than the cursor sorts to the
// head of the merged set, survives the cut, and the page comes back holding
// rows the walk has already returned, with its own last row no older than the
// cursor it was asked from. The walk then never advances. So the proof is
// stated where it can be read, and the input it rests on is stated here.
//
// WHAT IT ACTUALLY DROPS, and where that is reachable. Only
// [EventLog.List]'s related-agent path merges, so this runs nowhere else. A
// dropped DIRECT match is not lost at all: it is older than the cursor, so the
// next page's direct read returns it — and if that page's cut drops it again,
// every row that page kept is newer than it, so each such page brings the walk
// closer and a later one returns it. A dropped SIBLING may never be returned
// by any page —
// it is older than the cursor while the direct match that pulled its trace in
// can be newer, so that trace is not re-queried further down the walk. That
// row is recovered by TRACE: the direct match IS on a page the caller gets
// (this one, or a later one if it was cut too), every returned record carries
// its TraceID, and [EventLog.Trace] returns that trace oldest first, up to
// MaxTraceEvents rows. A sibling exists only because it shares a trace with a
// row the caller was given, so there is no dropped sibling whose trace the
// caller cannot NAME.
//
// THE TRACE READ IS CAPPED, and that is not where the recovery stops. It is
// ordered forwards, so what it returns is that trace's OLDEST MaxTraceEvents
// rows within EventHistory, and on a longer trace a dropped sibling can be past
// them. It is then on [EventLog.List] with [ListQuery.TraceID], which reads the
// same trace newest first with no cap on the walk — see [EventLog.Trace]. Which
// case a caller is in is [EventLog.TraceEventCount]'s answer, never the row
// count's.
//
// A limit of zero or less returns the set unchanged, matching
// [EventLog.List], which substitutes defaultListLimit before it gets here —
// so this is the arithmetic's own identity case and never a page size.
func cutToPage(recs []EventRecord, limit int) []EventRecord {
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

	// THERE IS NO ROW BOUND HERE, and that is structural rather than a
	// default. [EventLog.PhaseTokens] reads the whole window because a total
	// folded from part of one is an undercount that reads as an underspend;
	// the bounded read is [EventLog.PhaseTokenTail], a different method with
	// a different second answer, so a rollup cannot be cut by a field it
	// forgot to leave at zero.
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

// AgentPhaseLimit bounds one page of a seat's phase history.
//
// Sized by the ROW rather than by the table: each row carries the phase's
// prompts and its whole response verbatim, which is the reason
// [MaxPhasePage] gives for the company-wide page too.
//
// A PAGE, NOT THE RECORD. [EventLog.AgentPhases] reports whether the seat's
// history holds more than this, and the rest is reached by handing the page's
// last row back as its `before` cursor.
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
//
// THE SECOND RETURN SAYS THE SEAT'S RECORD HOLDS MORE than this page, read as
// one row past [AgentPhaseLimit] and dropped (see [probed]). Without it a seat
// with exactly a page of history and one with a month of it answer
// identically, and a caller offering an older page on a FULL one offers an
// empty page on the first. The rest is the next page: the last row's (time, id)
// handed back as `before`.
func (l *EventLog) AgentPhases(ctx context.Context, agentID, agentRole string, before *Cursor) ([]EventRecord, bool, error) {
	if agentID == "" && agentRole == "" {
		return nil, false, nil
	}
	if before != nil && before.ID == "" {
		return nil, false, ErrHalfCursor
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
	if before != nil {
		query += agentPhaseCursorSQL
		args = append(args, EncodeTime(before.Time), before.ID)
	}
	query += agentPhaseOrderSQL
	args = append(args, AgentPhaseLimit+1)
	out, err := l.scanPayloads(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	out, more := probed(out, AgentPhaseLimit)
	return out, more, nil
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
//
// The second return is [EventLog.AgentPhases]' own and for its reason: the
// record holds more than this page, read as one row past `limit` and dropped.
// The rest is the next page, from the last row's (time, id) as `before`.
func (l *EventLog) Phases(ctx context.Context, role string, limit int, before *Cursor) ([]EventRecord, bool, error) {
	if before != nil && before.ID == "" {
		return nil, false, ErrHalfCursor
	}
	query := phasesSQL
	args := []any{EncodeTime(now().Add(-EventHistory))}
	if role != "" {
		query += ` AND agent_role = ?`
		args = append(args, role)
	}
	if before != nil {
		query += agentPhaseCursorSQL
		args = append(args, EncodeTime(before.Time), before.ID)
	}
	query += agentPhaseOrderSQL
	if limit <= 0 || limit > MaxPhasePage {
		limit = MaxPhasePage
	}
	args = append(args, limit+1)
	out, err := l.scanPayloads(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	out, more := probed(out, limit)
	return out, more, nil
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
// The token counts are COLUMNS (schema/0015), so the window folds without
// reading a payload; the price is the one value still read out of it, for the
// reason given at [phaseTokenSQL].
//
// THE WHOLE WINDOW, always. A caller that keeps only a bounded tail reads
// [EventLog.PhaseTokenTail] instead.
func (l *EventLog) PhaseTokens(ctx context.Context, q PhaseTokenQuery) ([]tokens.Record, error) {
	return l.phaseTokens(ctx, q, 0)
}

// PhaseTokenTail returns the NEWEST `limit` spend records inside the window,
// newest first, and whether the window held more than that.
//
// For a caller that RETAINS only a bounded tail anyway, which is the live
// projection's startup seed: reading the whole of a day busier than its record
// cap would load every record into memory inside the seed's time budget only to
// drop the oldest on arrival.
//
// THE SECOND RETURN IS THE POINT, and it is read, not inferred. A caller that
// kept a tail has to head what it folds with the span the tail covers rather
// than with the window it asked for — a total over eighteen hours under a
// twenty-four-hour heading is a wrong total — and a page of exactly `limit`
// records cannot say which it is: a window that held exactly that many and one
// that held ten times as many answer with the same rows. One record past the
// limit is read as the evidence and dropped (see [probed]). What the tail
// leaves out is the older part of the same window, which [EventLog.PhaseTokens]
// returns whole.
//
// A limit below one is REFUSED rather than read as "no bound": the whole
// window is the other method, and a tail read that silently became it is the
// memory spike this read exists to avoid.
func (l *EventLog) PhaseTokenTail(ctx context.Context, q PhaseTokenQuery, limit int) ([]tokens.Record, bool, error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("store: a phase-token tail needs a limit of at least 1, got %d "+
			"— the whole window is PhaseTokens", limit)
	}
	out, err := l.phaseTokens(ctx, q, limit+1)
	if err != nil {
		return nil, false, err
	}
	out, more := probed(out, limit)
	return out, more, nil
}

// phaseTokens is the one statement both spend reads run: `depth` rows deep, or
// the whole window at zero.
func (l *EventLog) phaseTokens(ctx context.Context, q PhaseTokenQuery, depth int) ([]tokens.Record, error) {
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
	// order a tail keeps the head of.
	sql += " ORDER BY event_time DESC, event_id DESC"
	if depth > 0 {
		sql += " LIMIT ?"
		args = append(args, depth)
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
