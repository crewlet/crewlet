package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The activity feed — every commit this company made, in the order the log
// made them.
//
// # ONE DURABLE TABLE AT ANY AGE
//
// `tracker_history` holds one row per applied commit, quiet ones included: a
// "quiet" commit is one that woke nobody, not one that did not happen, and a
// feed assembled from the notifications would be an account of what was
// ANNOUNCED rather than of what was done. Nothing is swept, so a question
// about last week and a question about three years ago are the same query
// against the same table, with no live/archive boundary to cross.
//
// # THE ORDER IS THE LOG'S, and so is the cursor
//
// Rows are ordered by the COMPOSED POSITION — `(generation << 40) | seq` — not
// by any clock. That is a total order over exactly the records this question
// returns, which a clock is not: two nodes' authored instants can tie, can run
// backwards, and say nothing about which commit the broker accepted first.
//
// So `since:` and the page cursor are both positions, rendered as the
// `stream@generation:seq` triple the log itself uses. The generation is in the
// ordering rather than beside it, which is what lets a cursor SPAN A REANCHOR
// with no gap and no repeat — a sequence from a previous generation is below
// every sequence in the current one however large it is.
//
// # The instants it RENDERS are the authored ones
//
// Every displayed instant here is what the writer typed (D113): a person
// filtering "since Monday" means their Monday, and a card saying "yesterday"
// must not move because a record was redelivered. The EFFECTIVE instant is
// carried too, because that is what every duration and every report window is
// measured on — the two are different facts and the answer names both.

// ActivityRecord is one commit as the feed renders it.
type ActivityRecord struct {
	ID string `json:"id"`

	// The composed position, and its three parts. The triple is what a
	// caller stores: a bare sequence names no stream and no generation,
	// so a cursor built from one cannot survive a reanchor.
	LogSeq        uint64 `json:"log_seq"`
	LogStream     string `json:"log_stream"`
	LogGeneration uint32 `json:"log_generation"`

	// At is the AUTHORED instant — what the writer's own clock said — and
	// EffectiveAt is the fleet-agreed one every duration is measured on.
	// Both, because they are different facts and a surface that carried
	// one of them silently answers a different question than it looks like.
	At          time.Time `json:"at"`
	EffectiveAt time.Time `json:"effective_at"`

	Kind       ChangeKind `json:"kind"`
	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`

	SubjectKind ObjectKind `json:"subject_kind"`
	SubjectID   string     `json:"subject_id"`

	// SubjectKey is the human-readable key of the task a row is about,
	// resolved here because a feed of uuids is a feed nobody reads. Empty
	// for a subject that has no key — a project, a view, a person.
	SubjectKey string `json:"subject_key,omitempty"`
	Project    string `json:"project,omitempty"`

	Excerpt   string           `json:"excerpt,omitempty"`
	Fields    map[string]Delta `json:"fields,omitempty"`
	CommentID string           `json:"comment_id,omitempty"`
	BatchID   string           `json:"batch_id,omitempty"`
	TurnID    string           `json:"turn_id,omitempty"`

	// Notified says whether the commit carried a notification — whether
	// this change was ANNOUNCED. It is how a reader tells "nothing was
	// announced" from "nothing happened", which is the whole reason a
	// quiet commit still writes a row.
	//
	// IT IS DELIBERATELY NOT "SOMEBODY WAS WOKEN", and the applier could
	// not answer that even if the column meant it: who was actually told
	// is [Route]'s answer, and Route needs the company's CURRENT roster —
	// which the applier deliberately does not hold, because two nodes
	// briefly on different epochs would then write different rows for one
	// record. An announced change can reach nobody: every candidate may
	// be the actor, or may have left.
	Notified bool `json:"notified"`

	// Late marks a record the broker accepted well after it was authored.
	Late bool `json:"late,omitempty"`
}

// ActivityAnswer is the feed, with the framework's own verdict on the read.
type ActivityAnswer struct {
	Records []ActivityRecord `json:"records"`

	// NextCursor resumes exactly after the last row, as a log position.
	NextCursor string `json:"next_cursor,omitempty"`

	// Keys names the tasks this page's DELTAS point at, id to item key.
	//
	// ON THE ANSWER AND NEVER ON THE RECORD, which is the whole of the
	// decision. A delta names the other end of a relation by its ID
	// because a key is a fact about ANOTHER task's row, and
	// `tracker_history` is inside this domain's identity claim and is
	// written once and repaired by nothing — so a node that had not
	// applied that task would store a different string there for ever.
	// See `deltas.go`. Resolved HERE the answer is a rendering aid the
	// read states once, from the rows this node holds at the instant it
	// answers, and it binds nothing: two nodes may legitimately answer
	// with different maps, exactly as they may answer with different
	// coverage.
	//
	// It is the same join [readActivity] already makes for
	// [ActivityRecord.SubjectKey], asked of the ids a row POINTS AT
	// rather than of the row's own subject — and it exists because
	// neither side could fix "Waiting on: — → 0f3c…" alone: the engine
	// may not put a key on the record, and a surface holds no map to
	// resolve one with.
	//
	// AN ID THIS NODE HOLDS NO ROW FOR IS SIMPLY ABSENT, never an empty
	// string: a renderer falls back to the id, which is the honest
	// degradation and the same one it takes past [MaxActivityKeys].
	Keys map[string]string `json:"keys,omitempty"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// MaxActivityRows is how many commits one page carries.
const MaxActivityRows = 200

// MaxActivityKeys bounds the key map one answer carries.
//
// FIVE PER ROW ON A FULL PAGE, which is more counterparties than a row can
// legibly show: every delta side is cut at [MaxDeltaValue] and ends in its own
// dropped count, so a row naming more than a handful is already telling the
// reader it has more. The cap is what stops a page of dependency commits
// turning a label into the most expensive part of the read — and past it the
// remaining ids render as ids, which is the same degradation as an id this
// node holds no row for and therefore needs no second shape.
//
// A TASK DETAIL'S OWN HISTORY SHARES IT and is nowhere near: that read is
// capped at [DetailHistoryDefault] rows, so its walk cannot reach a fifth of
// this even if every row moved every counterparty field. It is one number
// because it bounds one walk — a second constant would be a second opinion
// about what a rendering aid is allowed to cost, free to drift from this one
// and with no reading of its own to justify it.
const MaxActivityKeys = MaxActivityRows * 5

// ActivityQuery asks for a slice of the feed.
type ActivityQuery struct {
	// Task narrows to one subject — a task id or any of its keys, former
	// ones included.
	Task string

	// Project narrows to one container. Empty with Workspace false is the
	// whole company, which is a valid question for every filter but `q`.
	Project   string
	Workspace bool

	// Since is a lower bound. EITHER a log position or an authored
	// instant, never both: a position is the total order this feed is in,
	// and an instant is what a person types.
	Since   statelog.Position
	SinceAt time.Time

	// From and To bound the AUTHORED instants, which is what a caller
	// typing a wall-clock range means (D113).
	From time.Time
	To   time.Time

	Kinds []ChangeKind
	Actor string

	// ActorKinds narrows to who was WRITING rather than to which handle:
	// every commit an operator token made, or every one the engine made
	// for itself. Empty is every kind.
	//
	// IT IS NOT `Actor` WITH A PREFIX. An `operator` commit carries the
	// TOKEN's name and an `agent` one carries a seat handle, so the two
	// name spaces are disjoint and a caller asking "what did a person do"
	// cannot express it as a set of handles — that set is the roster,
	// which changes, and a commit by somebody who has left would drop out
	// of an audit built from it.
	ActorKinds []AuthorKind

	Assignee string

	// Q is an escaped LIKE over the excerpt, and is GATED — see
	// [ActivityQuerySpanDays].
	Q string

	// Notified narrows to what was or was not ANNOUNCED — see
	// [ActivityRecord.Notified]. Not to what woke somebody: a change can
	// be announced and still reach nobody.
	Notified *bool

	Batch string

	Limit  int
	Cursor string

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration

	// MaxLagSeq is the same bound counted in RECORDS, which is what
	// the broker actually answers — the duration above is derived
	// from it through this node's own drain rate. Both may be set
	// and the read refuses past whichever is reached first.
	MaxLagSeq uint64
}

// Activity answers a slice of the company's own history.
func (r *Reader) Activity(ctx context.Context, q ActivityQuery, now time.Time) (
	ActivityAnswer, error) {

	if q.Level == "" {
		return ActivityAnswer{}, fmt.Errorf("tracker: this activity read names " +
			"no level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	if err := gateActivityQuery(q, now); err != nil {
		return ActivityAnswer{}, err
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxActivityRows {
		limit = MaxActivityRows
	}

	var answer ActivityAnswer
	served, err := r.log.Read(ctx, statelog.Query{
		Level:       q.Level,
		Scope:       activityScope(q),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		MaxLagSeq:   q.MaxLagSeq,
		Set:         true,
	}, func(tx *sql.Tx) error {
		records, next, err := readActivity(ctx, tx, q, limit)
		if err != nil {
			return err
		}
		answer.Records, answer.NextCursor = records, next
		// IN THE SAME TRANSACTION as the rows it labels, so the map
		// cannot name a key from a later instant than the page it is
		// about — the same reason the count and the coverage probe
		// share this read.
		keys, err := counterpartyKeys(ctx, tx, activityDeltaSides(records),
			r.db.Caps().MaxVariables)
		if err != nil {
			return err
		}
		answer.Keys = keys
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		answer.LogSeq, answer.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return ActivityAnswer{}, err
	}
	answer.Level = served.Level
	answer.Complete = served.Complete
	answer.LogLag = served.Lag
	if served.Incomplete != nil {
		answer.Incomplete = incompleteFrom(served.Incomplete)
	}
	return answer, nil
}

// gateActivityQuery refuses what this feed cannot serve.
//
// THE GATE IS ON WHAT THE QUERY WOULD SCAN, never on which keys were named.
// Written as "requires `task`, `container` or a `since:` bound" it was a gate
// a caller satisfied in one attempt and learned nothing from:
// `container=workspace&q=` names a container, an unbounded `since:` names a
// bound, and both run the same full scan the gate exists to stop.
func gateActivityQuery(q ActivityQuery, now time.Time) error {
	if strings.TrimSpace(q.Q) == "" {
		return nil
	}
	if strings.TrimSpace(q.Task) != "" {
		// ONE SUBJECT IS AN INDEX RANGE on `(subject_id, log_seq DESC)`,
		// however old, so no window is needed.
		return nil
	}
	if ProjectKey(q.Project) == "" {
		return fmt.Errorf("tracker: searching the activity feed needs a "+
			"subject or a container: pass `task`, or `container=project:<KEY>` "+
			"with a `since` no further back than %d days — at company scope a "+
			"text search reads every commit this company has ever made and "+
			"times out rather than answering", ActivityQuerySpanDays)
	}
	span := activitySpan(q, now)
	if span <= 0 {
		return fmt.Errorf("tracker: searching a project's activity needs a "+
			"`since` bound no further back than %d days — without one this "+
			"reads the project's whole history", ActivityQuerySpanDays)
	}
	if span > ActivityQuerySpanDays*24*time.Hour {
		return fmt.Errorf("tracker: `since` reaches back %d days and a text "+
			"search over the activity feed covers at most %d — narrow the "+
			"window, or drop `q` and filter by `kinds` and `actor`, which are "+
			"indexed", int(span.Hours()/24), ActivityQuerySpanDays)
	}
	return nil
}

// activitySpan is how far back a text search would reach, or zero for unbounded.
//
// A LOG POSITION IS NOT A SPAN. A caller resuming from a cursor has named a
// point in the log rather than a wall-clock bound, and nothing here can say
// how old that point is without reading it — so a position alone does not
// satisfy the window, and the refusal above says which key does.
func activitySpan(q ActivityQuery, now time.Time) time.Duration {
	switch {
	case !q.From.IsZero():
		return now.Sub(q.From)
	case !q.SinceAt.IsZero():
		return now.Sub(q.SinceAt)
	}
	return 0
}

// activityScope is the closure this read is about.
//
// A SUBJECT narrows to that object, a container to that project, and anything
// wider is the domain — honestly, because a feed of the whole company is a
// question every deferred record concerns and the honest answer to "is this
// complete" is then no.
func activityScope(q ActivityQuery) statelog.ScopeSet {
	switch {
	case strings.TrimSpace(q.Task) != "":
		// THE CONTAINER rather than the object, because a row about a
		// task can be written by a record whose subject is the project
		// it sits in — a chart apply, a policy change — and a closure
		// naming only the task would certify a feed complete that one of
		// those was holding.
		if key := ProjectKey(q.Project); key != "" {
			return statelog.ScopeSet{Paths: []string{
				ScopeTerm{Kind: TermContainer, ID: key}.Path(),
			}}.Normalised()
		}
		return statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised()
	case ProjectKey(q.Project) != "":
		return statelog.ScopeSet{Paths: []string{
			ScopeTerm{Kind: TermContainer, ID: ProjectKey(q.Project)}.Path(),
		}}.Normalised()
	}
	return statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised()
}

// readActivity reads one page of the feed, newest first.
func readActivity(ctx context.Context, tx *sql.Tx, q ActivityQuery, limit int) (
	[]ActivityRecord, string, error) {

	where, args, err := compileActivity(ctx, tx, q)
	if err != nil {
		return nil, "", err
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + joinAnd(where)
	}
	// NEWEST FIRST. A feed is read from the top, and the cursor pages
	// backwards through the log — which is why the keyset is `<` rather
	// than `>` and why it needs no tie-break: a composed position is
	// unique per record by construction.
	rows, err := tx.QueryContext(ctx, `
		SELECT h.id, h.log_seq, h.log_stream, h.log_generation,
		       h.created_at, h.effective_at, h.kind, h.actor, h.actor_kind,
		       h.operator_id, h.subject_kind, h.subject_id, h.project_key,
		       h.excerpt, h.fields_json, h.comment_id, h.batch_id, h.turn_id,
		       h.notified, h.late,
		       (SELECT k.key FROM tracker_tasks k WHERE k.id = h.subject_id)
		FROM tracker_history h`+clause+`
		ORDER BY h.log_seq DESC
		LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, "", fmt.Errorf("tracker: read the activity feed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ActivityRecord, 0, limit)
	more := false
	for rows.Next() {
		if len(out) == limit {
			// THE EXTRA ROW IS THE CURSOR'S EVIDENCE and never an
			// answer: a page that returned it would overrun the limit
			// the caller asked for.
			more = true
			break
		}
		var record ActivityRecord
		var packed int64
		var authored, effective int64
		var kind, actorKind, subjectKind string
		var fields string
		var batch, key sql.NullString
		var notified, late int
		if err := rows.Scan(&record.ID, &packed, &record.LogStream,
			&record.LogGeneration, &authored, &effective, &kind, &record.Actor,
			&actorKind, &record.OperatorID, &subjectKind, &record.SubjectID,
			&record.Project, &record.Excerpt, &fields, &record.CommentID,
			&batch, &record.TurnID, &notified, &late, &key); err != nil {
			return nil, "", fmt.Errorf("tracker: scan an activity row: %w", err)
		}
		record.LogSeq = uint64(packed) % statelog.GenerationStride
		record.At = store.DecodeTime(authored)
		record.EffectiveAt = store.DecodeTime(effective)
		record.Kind = ChangeKind(kind)
		record.ActorKind = AuthorKind(actorKind)
		record.SubjectKind = ObjectKind(subjectKind)
		record.BatchID = batch.String
		record.SubjectKey = key.String
		record.Notified, record.Late = notified != 0, late != 0
		if fields != "" && fields != "{}" {
			if err := json.Unmarshal([]byte(fields), &record.Fields); err != nil {
				// A ROW WHOSE DELTAS DO NOT DECODE IS STILL A ROW. The
				// commit happened; dropping it because one column is
				// unreadable would make a feed silently skip history.
				record.Fields = nil
			}
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("tracker: read the activity feed: %w", err)
	}
	next := ""
	if more && len(out) > 0 {
		last := out[len(out)-1]
		next = statelog.Position{
			Stream: last.LogStream, Generation: last.LogGeneration,
			Seq: last.LogSeq,
		}.String()
	}
	return out, next, nil
}

// counterpartyKeys resolves the task ids a page of deltas points at.
//
// See [ActivityAnswer.Keys] for why the map is on the answer and not on the
// record. What this function is, mechanically, is one indexed lookup per
// chunk of ids over `tracker_tasks` — the same join [readActivity] makes for
// each row's own subject, asked of the ids the rows POINT AT.
//
// IT TAKES THE DELTA SIDES rather than the rows, because TWO reads ask it and
// they hold their deltas in two shapes: the feed decodes `fields_json` into
// [Delta] pairs, and a task detail's history keeps the raw snapshot, which the
// applier writes as a from/to pair where it could compare two documents and as
// the notification's own value where it could not. Collecting the sides is
// therefore per shape ([activityDeltaSides], [historyDeltaSides]) and
// everything that can drift — which fields name a task, what counts as a
// member, the cap, the dedupe and the lookup — is here, once. Written twice,
// the item's own History tab rendered a re-parent as two uuids while the
// company-wide log resolved the same delta to two keys.
//
// It resolves nothing and fails nothing when there is nothing to resolve,
// which is the overwhelming majority of pages: a feed of status changes and
// comments names no counterparty at all, and the walk below then issues no
// statement.
func counterpartyKeys(ctx context.Context, tx *sql.Tx, sides []string,
	maxVariables int) (map[string]string, error) {

	wanted := make([]string, 0, len(sides))
	seen := make(map[string]bool, len(sides))
	for _, side := range sides {
		for _, member := range strings.Split(side, ", ") {
			switch {
			case member == "" || seen[member]:
				continue
			case strings.HasPrefix(member, "+"):
				// THE DROPPED COUNT, not a member. A bounded
				// list ends in `+12 more`, and no id this
				// package mints begins with a plus.
				continue
			case len(wanted) >= MaxActivityKeys:
				// PAST THE CAP THE REST RENDER AS IDS, which is
				// what an unresolved id already does — so the
				// walk stops rather than growing a map the page
				// cannot use.
				return resolveTaskKeys(ctx, tx, wanted, maxVariables)
			}
			seen[member] = true
			wanted = append(wanted, member)
		}
	}
	return resolveTaskKeys(ctx, tx, wanted, maxVariables)
}

// activityDeltaSides is every counterparty side a feed page carries.
//
// The feed's own shape: `fields_json` decoded into [Delta] pairs, so each
// field contributes exactly two sides.
func activityDeltaSides(records []ActivityRecord) []string {
	out := make([]string, 0, len(records)*2)
	for _, record := range records {
		for _, field := range counterpartyDeltaFields {
			delta, carried := record.Fields[field]
			if !carried {
				continue
			}
			out = append(out, delta.From, delta.To)
		}
	}
	return out
}

// historyDeltaSides is the same over a task's own history rows.
//
// THE RAW SNAPSHOT, which is the other shape the same column takes: the
// applier writes a from/to pair where it could compare two documents and the
// notification's own value where it could not (a comment, a mention, an ask).
// A collector that assumed the pair would resolve nothing on the second, and a
// renderer reading both — which is what the dashboard does — would then print
// an id under one heading and a key under the next.
func historyDeltaSides(entries []HistoryEntry) []string {
	out := make([]string, 0, len(entries)*2)
	for _, entry := range entries {
		for _, field := range counterpartyDeltaFields {
			raw, carried := entry.Fields[field]
			if !carried {
				continue
			}
			out = appendDeltaSides(out, raw)
		}
	}
	return out
}

// appendDeltaSides flattens one raw snapshot value into the strings that may
// hold ids.
//
// RECURSIVE OVER THE THREE SHAPES `encoding/json` decodes a snapshot into, and
// silent on every other: a number, a bool or a nested object in this position
// is not an id, and the lookup that follows would spend a parameter on it.
func appendDeltaSides(out []string, raw any) []string {
	switch v := raw.(type) {
	case string:
		return append(out, v)
	case []any:
		for _, item := range v {
			out = appendDeltaSides(out, item)
		}
		return out
	case map[string]any:
		// A FROM/TO PAIR AND NOTHING ELSE. Any other object here is a
		// shape this field does not take, and walking its values would
		// be guessing at which of them names a task.
		for _, side := range [...]string{"from", "to"} {
			if member, carried := v[side]; carried {
				out = appendDeltaSides(out, member)
			}
		}
		return out
	}
	return out
}

// counterpartyDeltaFields are the delta fields whose members are TASK IDS.
//
// DERIVED FROM [RelationKinds] rather than listed beside it, for the reason
// that slice exists: a fifth kind of edge is resolved with no second edit.
// [RelationPage] is the one exclusion and it is a fact about the type — a
// `page` edge names a knowledge-base page, and a map that answered for one
// would be claiming a page is a task.
//
// FOUR ARE BEYOND THE EDGES, and each one names a task for its own reason:
// `blocking` is the mirror a blocker carries, `priorities` is a person's own
// queue — an ordered list of task ids that renders on the same page —
// `parent` is the task this one hangs under, and `removed_with` is the root a
// cascade took it with. The last two are scalars rather than lists and are
// resolved here all the same: [TaskDeltas] records both by ID for the reason
// it records every relation by one — a key is a fact about another task's row
// and a history row is repaired by nothing — so without them a reparent and a
// cascade removal are the two rows on an item's own History tab that still
// render a uuid.
var counterpartyDeltaFields = func() []string {
	out := make([]string, 0, len(RelationKinds)+4)
	for _, kind := range RelationKinds {
		if kind == RelationPage {
			continue
		}
		out = append(out, string(kind))
	}
	return append(out, "blocking", "priorities", "parent", "removed_with")
}()

// resolveTaskKeys reads the item keys of ids this node holds a row for.
//
// CHUNKED ON THE ESTATE'S OWN PARAMETER LIMIT, which is the shape the applier
// takes for the same reason — a statement with more bound parameters than the
// engine accepts fails outright rather than answering less. A limit the store
// could not probe degrades to one id per statement: slow, never wrong.
func resolveTaskKeys(ctx context.Context, tx *sql.Tx, ids []string,
	maxVariables int) (map[string]string, error) {

	if len(ids) == 0 {
		return nil, nil
	}
	if maxVariables < 1 {
		maxVariables = 1
	}
	out := make(map[string]string, len(ids))
	for chunk := range slices.Chunk(ids, maxVariables) {
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT id, key FROM tracker_tasks WHERE id IN (`+
				placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("tracker: resolve the item keys this "+
				"activity page points at: %w", err)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := scanTaskKeys(rows, out); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		// NIL RATHER THAN AN EMPTY MAP, so the field is omitted: a page
		// that named nothing resolvable and a page that named nothing
		// at all are one fact to a reader, and two spellings of it in
		// one field is what `deltas.go` refuses on the write side.
		return nil, nil
	}
	return out, nil
}

// scanTaskKeys drains one chunk into the map.
func scanTaskKeys(rows *sql.Rows, into map[string]string) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			return fmt.Errorf("tracker: scan an item key: %w", err)
		}
		if key == "" {
			// A ROW WITH NO KEY YET IS AN ABSENT ANSWER, not an
			// empty one — see [ActivityAnswer.Keys].
			continue
		}
		into[id] = key
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("tracker: read the item keys: %w", err)
	}
	return nil
}

// compileActivity turns the query into a predicate.
func compileActivity(ctx context.Context, tx *sql.Tx, q ActivityQuery) (
	[]string, []any, error) {

	var where []string
	var args []any
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}

	if task := strings.TrimSpace(q.Task); task != "" {
		// THE ID, RESOLVED FROM WHATEVER THE CALLER HELD. A feed asked
		// for by a FORMER key must answer the same rows — the history is
		// the one place a rename is most likely to be looked up from.
		id, err := resolveTaskID(ctx, tx, task)
		if err != nil {
			return nil, nil, err
		}
		add("h.subject_id = ?", id)
	}
	if project := ProjectKey(q.Project); project != "" {
		add("h.project_key = ?", project)
	}
	if !q.Since.IsZero() {
		add("h.log_seq > ?", q.Since.Packed())
	}
	if !q.SinceAt.IsZero() {
		add("h.created_at >= ?", store.EncodeTime(q.SinceAt))
	}
	if !q.From.IsZero() {
		add("h.created_at >= ?", store.EncodeTime(q.From))
	}
	if !q.To.IsZero() {
		add("h.created_at <= ?", store.EncodeTime(q.To))
	}
	if len(q.Kinds) > 0 {
		values := make([]any, 0, len(q.Kinds))
		for _, kind := range q.Kinds {
			values = append(values, string(kind))
		}
		add("h.kind IN ("+placeholders(len(values))+")", values...)
	}
	if actor := strings.TrimSpace(q.Actor); actor != "" {
		add("h.actor = ?", actor)
	}
	if len(q.ActorKinds) > 0 {
		// THE INDEX IS `(actor_kind, log_seq DESC)` — migration 0012 — so
		// a single kind is a range on it and the feed's own newest-first
		// order comes out of the index rather than a temp b-tree over
		// every commit the company has ever made. A set of kinds is one
		// range each, which SQLite plans as a lookup per value.
		values := make([]any, 0, len(q.ActorKinds))
		for _, kind := range q.ActorKinds {
			values = append(values, string(kind))
		}
		add("h.actor_kind IN ("+placeholders(len(values))+")", values...)
	}
	if batch := strings.TrimSpace(q.Batch); batch != "" {
		add("h.batch_id = ?", batch)
	}
	if assignee := strings.TrimSpace(q.Assignee); assignee != "" {
		// A JOIN TO THE TASK'S CURRENT ASSIGNEE, which is what a caller
		// asking "what happened to the work X holds" means — not "what
		// did X do", which is `actor`. The two are routinely different
		// and naming them apart is the whole reason both exist.
		add("EXISTS (SELECT 1 FROM tracker_tasks t WHERE t.id = h.subject_id "+
			"AND t.assignee = ?)", assignee)
	}
	if q.Notified != nil {
		add("h.notified = ?", boolInt(*q.Notified))
	}
	if text := strings.TrimSpace(q.Q); text != "" {
		add(`h.excerpt LIKE ? ESCAPE '\'`, "%"+likeEscape(text)+"%")
	}
	if cursor := strings.TrimSpace(q.Cursor); cursor != "" {
		at, err := ParseLogPosition(cursor)
		if err != nil {
			return nil, nil, err
		}
		// STRICTLY BEFORE, because the feed is newest-first: the cursor
		// names the last row of the previous page.
		add("h.log_seq < ?", at.Packed())
	}
	return where, args, nil
}

// ParseLogPosition reads back the `stream@generation:seq` triple a cursor is.
//
// THE TRIPLE RATHER THAN A BARE SEQUENCE, because a sequence names no stream
// and no generation: handed one from a previous generation, a reader cannot
// tell a position that is behind from one that is impossibly far ahead — and
// the whole reason the public cursor carries all three is that it then never
// needs a migration.
func ParseLogPosition(raw string) (statelog.Position, error) {
	refuse := func() (statelog.Position, error) {
		return statelog.Position{}, fmt.Errorf("tracker: %q is not a log "+
			"position — one reads `<stream>@<generation>:<sequence>`, and the "+
			"answer that produced it carries the value to send back", raw)
	}
	stream, rest, found := strings.Cut(raw, "@")
	if !found || stream == "" {
		return refuse()
	}
	generation, sequence, found := strings.Cut(rest, ":")
	if !found {
		return refuse()
	}
	gen, err := strconv.ParseUint(generation, 10, 32)
	if err != nil {
		return refuse()
	}
	seq, err := strconv.ParseUint(sequence, 10, 64)
	if err != nil {
		return refuse()
	}
	at := statelog.Position{Stream: stream, Generation: uint32(gen), Seq: seq}
	if err := at.Valid(); err != nil {
		return statelog.Position{}, fmt.Errorf("tracker: %q is not a usable "+
			"log position: %w", raw, err)
	}
	return at, nil
}
