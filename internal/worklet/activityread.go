package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The activity feed: every commit this company made, in the log's order.
//
// `tracker_history` holds one row per applied commit, quiet ones included (a
// quiet commit woke nobody; it still happened), and nothing is swept, so last
// week and three years ago are one query with no archive boundary.
//
// Rows, `since:` and the cursor are ordered by the composed position
// `(generation << 40) | seq`, never a clock (authored instants tie and run
// backwards), rendered as `stream@generation:seq`: an earlier generation sorts
// below the current one, so a cursor spans a reanchor with no gap or repeat.
// Instants rendered and filtered on are the authored ones (D113); the
// effective one rides beside them for durations and report windows.

// ActivityRecord is one commit as the feed renders it.
type ActivityRecord struct {
	ID string `json:"id"`

	// The composed position and its parts. A caller stores the triple: a
	// bare sequence names no stream or generation, so cannot survive a reanchor.
	LogSeq        uint64 `json:"log_seq"`
	LogStream     string `json:"log_stream"`
	LogGeneration uint32 `json:"log_generation"`

	// At is the authored instant and EffectiveAt the fleet-agreed one every
	// duration is measured on: two facts, so the record carries both.
	At          time.Time `json:"at"`
	EffectiveAt time.Time `json:"effective_at"`

	Kind       ChangeKind `json:"kind"`
	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`

	SubjectKind ObjectKind `json:"subject_kind"`
	SubjectID   string     `json:"subject_id"`

	// SubjectKey is the key of the task a row is about, empty for a subject
	// with none (a project, a view, a person).
	SubjectKey string `json:"subject_key,omitempty"`

	// SubjectKeyCollision is the subject's [TaskRow.KeyCollision]: another
	// task claimed the key first, so a link built from it opens THAT task
	// and this subject is reached by [ActivityRecord.SubjectID] ([ItemAddress]).
	SubjectKeyCollision bool `json:"subject_key_collision,omitempty"`

	Project string `json:"project,omitempty"`

	Excerpt   string           `json:"excerpt,omitempty" untrusted:"fence"`
	Fields    map[string]Delta `json:"fields,omitempty"`
	CommentID string           `json:"comment_id,omitempty"`
	TurnID    string           `json:"turn_id,omitempty"`

	// Ask is the question this commit asked or answered, as it stands now
	// ([AskView]); nil for no comment or an ordinary remark.
	Ask *AskView `json:"ask,omitempty"`

	// Notified says whether the commit carried a notification, telling
	// "nothing was announced" from "nothing happened". It is not "somebody
	// was woken": that is [Route]'s answer from the current roster, which the
	// applier must not hold (nodes on different epochs would write different
	// rows for one record); an announced change can reach nobody.
	Notified bool `json:"notified"`

	// Late is the commit's [Notify.Late]: an announcement a repair duty
	// issued after the write it is about, so a late card says why. Its
	// authored instant is the duty's.
	Late bool `json:"late,omitempty"`
}

// ActivityAnswer is the feed, with the framework's own verdict on the read.
type ActivityAnswer struct {
	Records []ActivityRecord `json:"records"`

	// NextCursor resumes exactly after the last row, as a log position.
	NextCursor string `json:"next_cursor,omitempty"`

	// Keys maps the task ids this page's deltas point at to their item keys.
	//
	// On the answer, never the record: a key is a fact about another task's
	// row, and `tracker_history` is written once and repaired by nothing, so a
	// node that had not applied that task would store another string for ever
	// (`deltas.go`). Here it is a rendering aid from this node's rows at answer
	// time, binding nothing; two nodes may answer different maps. An id with no
	// row here is absent, never "": a renderer falls back to the id, as it
	// does past [MaxActivityKeys].
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

// MaxActivityKeys bounds the key map one answer carries: five per row of a
// full page, more counterparties than a row legibly shows (each delta side is
// cut at [MaxDeltaValue] with its own dropped count). It keeps a page of
// dependency commits from making labels the costliest part of the read; past
// it ids render as ids, as an id with no row here does. A task detail's
// history (at most [DetailHistoryDefault] rows) shares it: one walk, one bound.
const MaxActivityKeys = MaxActivityRows * 5

// ActivityQuery asks for a slice of the feed.
type ActivityQuery struct {
	// Task narrows to one subject: a task id or any of its keys, former
	// ones included.
	Task string

	// Project narrows to one container; empty with Workspace false is the
	// whole company, valid for every filter but `q`.
	Project   string
	Workspace bool

	// Since (a log position) or SinceAt (an authored instant, what a
	// person types) is a lower bound; never both.
	Since   statelog.Position
	SinceAt time.Time

	// From and To bound the authored instants, which a caller typing a
	// wall-clock range means (D113).
	From time.Time
	To   time.Time

	// Kinds keeps changes of these kinds, plus any change that moved a field
	// a kind names ([ChangeKind.MovedFields]): a hand-over that also sets
	// status is a `status` row carrying the assignee. A creation matches only
	// `created`, though it records every filed value as a move from nothing:
	// a feed of status changes is not a feed of filings.
	Kinds []ChangeKind
	Actor string

	// ActorKinds narrows to who was writing (every operator-token commit,
	// every engine commit); empty is every kind. Not `Actor` with a prefix:
	// operator and agent commits name disjoint spaces (a token's name, a seat
	// handle), and a set of handles is the roster, which would drop the
	// commits of somebody who has left.
	ActorKinds []AuthorKind

	Assignee string

	// Q is an escaped LIKE over the excerpt, gated by
	// [ActivityQuerySpanDays].
	Q string

	// Notified narrows to what was or was not announced
	// ([ActivityRecord.Notified]), not to what woke somebody.
	Notified *bool

	Limit  int
	Cursor string

	Level   statelog.ReadLevel
	Session statelog.Position
	Floors  []statelog.Position
	MaxLag  time.Duration

	// MaxLagSeq is the same bound in records, which the broker answers;
	// MaxLag is derived from it by this node's drain rate. With both set
	// the read refuses past whichever is reached first.
	MaxLagSeq uint64
}

// Activity answers a slice of the company's own history.
func (r *Reader) Activity(ctx context.Context, q ActivityQuery, now time.Time) (
	ActivityAnswer, error) {

	if q.Level == "" {
		return ActivityAnswer{}, fmt.Errorf("worklet: this activity read names " +
			"no level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	if err := activityFilters(q); err != nil {
		return ActivityAnswer{}, err
	}
	if err := gateActivityQuery(q, now); err != nil {
		return ActivityAnswer{}, err
	}
	if err := feedPositions(q.Cursor, q.Since); err != nil {
		return ActivityAnswer{}, err
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxActivityRows {
		limit = MaxActivityRows
	}

	var answer ActivityAnswer
	served, err := r.log.Read(ctx, statelog.Query{
		Level:     q.Level,
		Scope:     activityScope(q),
		Session:   q.Session,
		Floors:    q.Floors,
		MaxLag:    q.MaxLag,
		MaxLagSeq: q.MaxLagSeq,
		Set:       true,
	}, func(tx *sql.Tx) error {
		records, next, err := readActivity(ctx, tx, q, limit)
		if err != nil {
			return err
		}
		answer.Records, answer.NextCursor = records, next
		// In the rows' own transaction, so the map names no key from a
		// later instant than its page.
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

// activityFilters refuses a change kind or an author kind this build does not
// have, naming the set: an unknown kind matches no row, and the empty feed
// reads as a still company. Refused here, where every surface's request
// passes, as [ErrBadQuery], since every node refuses it alike.
func activityFilters(q ActivityQuery) error {
	for _, kind := range q.Kinds {
		if !kind.Valid() {
			return fmt.Errorf("%w: %q is not a change kind — the kinds are %s",
				ErrBadQuery, kind, joinKinds(ChangeKinds))
		}
	}
	for _, kind := range q.ActorKinds {
		if !kind.Valid() {
			return fmt.Errorf("%w: %q is not an author kind — the kinds are %s",
				ErrBadQuery, kind, joinKinds(AuthorKinds))
		}
	}
	return nil
}

// joinKinds spells a closed set of kinds for a refusal naming it.
func joinKinds[K ~string](kinds []K) string {
	out := make([]string, len(kinds))
	for i, kind := range kinds {
		out[i] = string(kind)
	}
	return strings.Join(out, ", ")
}

// gateActivityQuery refuses a text search this feed cannot serve. It gates on
// what the query would scan, never on which keys were named:
// `container=workspace&q=` or an unbounded `since:` names a key and still
// runs the full scan.
func gateActivityQuery(q ActivityQuery, now time.Time) error {
	if strings.TrimSpace(q.Q) == "" {
		return nil
	}
	if strings.TrimSpace(q.Task) != "" {
		// One subject is an index range on `(subject_id, log_seq DESC)`,
		// however old, so no window is needed.
		return nil
	}
	if ProjectKey(q.Project) == "" {
		return fmt.Errorf("%w: searching the activity feed needs a "+
			"subject or a container: pass `task`, or `container=project:<KEY>` "+
			"with a `since` no further back than %d days — at company scope a "+
			"text search reads every commit this company has ever made and "+
			"times out rather than answering", ErrBadQuery, ActivityQuerySpanDays)
	}
	span := activitySpan(q, now)
	if span <= 0 {
		return fmt.Errorf("%w: searching a project's activity needs a "+
			"`since` bound no further back than %d days — without one this "+
			"reads the project's whole history", ErrBadQuery, ActivityQuerySpanDays)
	}
	if span > ActivityQuerySpanDays*24*time.Hour {
		return fmt.Errorf("%w: `since` reaches back %d days and a text "+
			"search over the activity feed covers at most %d — narrow the "+
			"window, or drop `q` and filter by `kinds` and `actor`, which are "+
			"indexed", ErrBadQuery, int(span.Hours()/24), ActivityQuerySpanDays)
	}
	return nil
}

// ErrBadQuery is a feed read whose request this package refused before reading
// a row: a text search wider than the feed can scan, a cursor that is not a
// log position, a cursor or a `since` bound on another log. The caller changes
// what it asks; every node refuses the same keys alike. Its own sentinel, so a
// surface tells the caller's mistake from a store fault, and a model is not
// told to retry the same cursor.
var ErrBadQuery = errors.New("worklet: this read's request cannot be run")

// feedPositions holds the cursor and the `since` bound to well-formed
// positions on this log ([statelog.Position.On]): `log_seq` is packed with no
// stream, so another log's position would pick a page silently. Before the
// read, because one parsed inside the transaction fails like a store fault.
func feedPositions(cursor string, since statelog.Position) error {
	if raw := strings.TrimSpace(cursor); raw != "" {
		at, err := statelog.ParsePosition(raw)
		if err != nil {
			return fmt.Errorf("%w: cursor: %w", ErrBadQuery, err)
		}
		if err := at.On(trackerStream); err != nil {
			return fmt.Errorf("%w: cursor: %w", ErrBadQuery, err)
		}
	}
	if err := since.On(trackerStream); err != nil {
		return fmt.Errorf("%w: since: %w", ErrBadQuery, err)
	}
	return nil
}

// activitySpan is how far back a text search would reach, or zero for unbounded.
// A log position is not a span (its age is unknown without reading it), so a
// cursor alone does not satisfy the window.
func activitySpan(q ActivityQuery, now time.Time) time.Duration {
	switch {
	case !q.From.IsZero():
		return now.Sub(q.From)
	case !q.SinceAt.IsZero():
		return now.Sub(q.SinceAt)
	}
	return 0
}

// activityScope is the closure this read is about: a task or a container
// narrows to the project, and anything wider is the domain, which every
// deferred record concerns, so a whole-company feed reports itself incomplete.
func activityScope(q ActivityQuery) statelog.ScopeSet {
	switch {
	case strings.TrimSpace(q.Task) != "":
		// The container, not the object: a record whose subject is the
		// project (a chart apply, a policy change) can write a row about the
		// task, and a task-only closure would certify complete a feed one of
		// those holds.
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

	predicate, err := compileActivity(ctx, tx, q)
	if err != nil {
		return nil, "", err
	}
	statement, args := activityStatement(predicate, limit+1)
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, "", fmt.Errorf("worklet: read the activity feed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ActivityRecord, 0, limit)
	more := false
	for rows.Next() {
		if len(out) == limit {
			// The extra row is the cursor's evidence, never returned: it
			// would overrun the caller's limit.
			more = true
			break
		}
		var record ActivityRecord
		var packed int64
		var authored, effective int64
		var kind, actorKind, subjectKind string
		var fields string
		var key sql.NullString
		var notified, late, collision int
		if err := rows.Scan(&record.ID, &packed, &record.LogStream,
			&record.LogGeneration, &authored, &effective, &kind, &record.Actor,
			&actorKind, &record.OperatorID, &subjectKind, &record.SubjectID,
			&record.Project, &record.Excerpt, &fields, &record.CommentID,
			&record.TurnID, &notified, &late, &key, &collision); err != nil {
			return nil, "", fmt.Errorf("worklet: scan an activity row: %w", err)
		}
		record.LogSeq = uint64(packed) % statelog.GenerationStride
		record.At = store.DecodeTime(authored)
		record.EffectiveAt = store.DecodeTime(effective)
		record.Kind = ChangeKind(kind)
		record.ActorKind = AuthorKind(actorKind)
		record.SubjectKind = ObjectKind(subjectKind)
		record.SubjectKey = key.String
		record.SubjectKeyCollision = collision == 1
		record.Notified, record.Late = notified != 0, late != 0
		if fields != "" && fields != "{}" {
			if err := json.Unmarshal([]byte(fields), &record.Fields); err != nil {
				// A row whose deltas do not decode is still a row: the
				// commit happened, and dropping it would skip history.
				record.Fields = nil
			}
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("worklet: read the activity feed: %w", err)
	}
	// The ask each commit is about, in one read over the page, through
	// the inbox's function.
	comments := make([]string, 0, len(out))
	for _, record := range out {
		if record.CommentID != "" {
			comments = append(comments, record.CommentID)
		}
	}
	asks, askErr := readAskViews(ctx, tx, comments)
	if askErr != nil {
		return nil, "", askErr
	}
	for i := range out {
		out[i].Ask = asks[out[i].CommentID]
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

// activityStatement is the feed's one statement over [compileActivity]'s
// predicate, reading `rows` rows, and its arguments; named for the plan test.
// Newest first, so the keyset is `<`, with no tie-break: a composed position
// is unique per record.
//
// Each author kind is its own arm, `actor_kind = ? … ORDER BY log_seq DESC
// LIMIT ?`, a range of `tracker_history_actor_kind_seq_idx` that stops after
// a page, and the arms merge under the page's order and limit. `actor_kind IN
// (…)`, even with one value, seeks each value and then sorts all it found:
// every commit any person or token ever made, on each audit load.
func activityStatement(p activityPredicate, rows int) (string, []any) {
	switch len(p.actorKinds) {
	case 0:
		return `
		SELECT ` + activityColumns + `, ` + activityKeys + `
		FROM tracker_history h` + whereClause(p.where) + `
		ORDER BY h.log_seq DESC
		LIMIT ?`, append(append([]any{}, p.args...), rows)
	case 1:
		return `
		SELECT ` + activityColumns + `, ` + activityKeys + `
		FROM tracker_history h` + whereClause(append(slices.Clone(p.where),
			"h.actor_kind = ?")) + `
		ORDER BY h.log_seq DESC
		LIMIT ?`, append(append(append([]any{}, p.args...), p.actorKinds[0]), rows)
	}
	arms := make([]string, len(p.actorKinds))
	var args []any
	for i, kind := range p.actorKinds {
		arms[i] = `SELECT * FROM (
			SELECT ` + activityColumns + `
			FROM tracker_history h` + whereClause(append(slices.Clone(p.where),
			"h.actor_kind = ?")) + `
			ORDER BY h.log_seq DESC
			LIMIT ?)`
		args = append(append(append(args, p.args...), kind), rows)
	}
	return `
		SELECT ` + activityColumns + `, ` + activityKeys + `
		FROM (` + strings.Join(arms, `
		UNION ALL `) + `) h
		ORDER BY h.log_seq DESC
		LIMIT ?`, append(args, rows)
}

// activityColumns is every column a feed row is read with, under the alias
// `h` — the table's, or an audit arm's.
const activityColumns = `h.id, h.log_seq, h.log_stream, h.log_generation,
		       h.created_at, h.effective_at, h.kind, h.actor, h.actor_kind,
		       h.operator_id, h.subject_kind, h.subject_id, h.project_key,
		       h.excerpt, h.fields_json, h.comment_id, h.turn_id,
		       h.notified, h.late`

// activityKeys is the subject's key and its collision flag, read over the
// merged arms rather than inside each, so an audit resolves one page of keys.
const activityKeys = `(SELECT k.key FROM tracker_tasks k WHERE k.id = h.subject_id),
		       COALESCE((SELECT k.key_collision FROM tracker_tasks k
		                 WHERE k.id = h.subject_id), 0)`

// whereClause is ` WHERE ` and the clauses ANDed, or nothing.
func whereClause(where []string) string {
	if len(where) == 0 {
		return ""
	}
	return " WHERE " + joinAnd(where)
}

// activityPredicate is the feed's compiled filter: the clauses every row must
// meet with their arguments, and the author kinds the feed is narrowed to,
// which [activityStatement] reads one arm at a time rather than as a clause.
type activityPredicate struct {
	where      []string
	args       []any
	actorKinds []any
}

// counterpartyKeys resolves the task ids a page of deltas points at
// ([ActivityAnswer.Keys]), one indexed lookup per chunk over `tracker_tasks`.
// It takes delta sides, not rows, because two reads hold deltas in two shapes
// ([activityDeltaSides], [historyDeltaSides]); everything that can drift
// (which fields name a task, membership, the cap, the dedupe, the lookup) is
// here once. A page naming no counterparty, the usual case, issues no statement.
func counterpartyKeys(ctx context.Context, tx *sql.Tx, sides []string,
	maxVariables int) (map[string]string, error) {

	wanted := make([]string, 0, len(sides))
	seen := make(map[string]bool, len(sides))
	for _, side := range sides {
		for _, member := range strings.Split(side, ", ") {
			switch {
			case member == "" || seen[member]:
				continue
			case len(wanted) >= MaxActivityKeys:
				// Past the cap the rest render as ids, as an
				// unresolved id does, so the walk stops.
				return resolveTaskKeys(ctx, tx, wanted, maxVariables)
			}
			seen[member] = true
			wanted = append(wanted, member)
		}
	}
	return resolveTaskKeys(ctx, tx, wanted, maxVariables)
}

// activityDeltaSides is every counterparty side a feed page carries: each
// [Delta]'s two sides and, for a set, every member it added and removed.
func activityDeltaSides(records []ActivityRecord) []string {
	out := make([]string, 0, len(records)*2)
	for _, record := range records {
		for _, field := range counterpartyDeltaFields {
			delta, carried := record.Fields[field]
			if !carried {
				continue
			}
			out = append(out, delta.From, delta.To)
			out = append(out, delta.Added...)
			out = append(out, delta.Removed...)
		}
	}
	return out
}

// historyDeltaSides is the same over a task's own history rows, whose raw
// snapshot is a from/to pair where the applier compared two documents and the
// notification's own value where it could not (a comment, a mention, an ask);
// a collector assuming the pair would leave the dashboard an id beside a key.
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
// hold ids, recursing over a string, an array and a [Delta] object; a number
// or a bool is no id and would only spend a lookup parameter.
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
		// A [Delta] and nothing else: walking another object's values
		// would guess at which of them names a task.
		for _, side := range [...]string{"from", "to", "added", "removed"} {
			if member, carried := v[side]; carried {
				out = appendDeltaSides(out, member)
			}
		}
		return out
	}
	return out
}

// counterpartyDeltaFields are the delta fields whose members are task ids:
// every [RelationKinds] edge that [RelationKind.NamesTask] (derived, so a new
// edge kind needs no second edit; `page` and `atlas_entry` name no task), and
// `blocking` (a blocker's mirror), `priorities` (a person's ordered queue),
// `parent` and `removed_with` (a cascade's root), the last two scalars that
// [TaskDeltas] records by id like every relation.
var counterpartyDeltaFields = func() []string {
	out := make([]string, 0, len(RelationKinds)+4)
	for _, kind := range RelationKinds {
		if !kind.NamesTask() {
			continue
		}
		out = append(out, string(kind))
	}
	return append(out, "blocking", "priorities", "parent", "removed_with")
}()

// taskKeysStatement is [resolveTaskKeys]' read of n tasks' keys, named for
// the plan test.
func taskKeysStatement(n int) string {
	return `SELECT id, key FROM tracker_tasks WHERE id IN (` + placeholders(n) + `)`
}

// resolveTaskKeys reads the item keys of ids this node holds a row for,
// chunked on the estate's parameter limit (more bound parameters fail the
// statement outright); an unprobed limit degrades to one id per statement.
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
		rows, err := tx.QueryContext(ctx, taskKeysStatement(len(chunk)), args...)
		if err != nil {
			return nil, fmt.Errorf("worklet: resolve the item keys this "+
				"activity page points at: %w", err)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := scanTaskKeys(rows, out); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		// Nil, not an empty map, so the field is omitted: nothing
		// resolvable and nothing named are one fact, as `deltas.go`
		// holds on the write side.
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
			return fmt.Errorf("worklet: scan an item key: %w", err)
		}
		if key == "" {
			// A row with no key yet is absent ([ActivityAnswer.Keys]).
			continue
		}
		into[id] = key
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("worklet: read the item keys: %w", err)
	}
	return nil
}

// compileActivity turns the query into a predicate.
func compileActivity(ctx context.Context, tx *sql.Tx, q ActivityQuery) (
	activityPredicate, error) {

	var p activityPredicate
	add := func(clause string, values ...any) {
		p.where = append(p.where, clause)
		p.args = append(p.args, values...)
	}

	project := ProjectKey(q.Project)
	if task := strings.TrimSpace(q.Task); task != "" {
		// The id, resolved from whatever the caller held: a former key
		// answers the same rows.
		id, filed, err := resolveTaskIn(ctx, tx, task)
		if err != nil {
			return activityPredicate{}, err
		}
		// A task's activity is its whole history, so a container named
		// beside it is a scope check, not a row filter: each row carries
		// the project it was written in, so a filter would hide rows from
		// before a move, and it keeps out nothing a reader is owed (a
		// tracker holder reads the whole tracker, ADR-0033). A container
		// the task is not filed in is refused, naming the one it is.
		if project != "" && filed != project {
			return activityPredicate{}, fmt.Errorf("%w: task %s is filed in "+
				"project %s, not %s — a task's activity is its whole history, "+
				"wherever it was written: name the project it is in, or drop "+
				"the container", ErrBadQuery, task, filed, project)
		}
		add("h.subject_id = ?", id)
	} else if project != "" {
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
	if len(q.Kinds) == 0 && strings.TrimSpace(q.Task) == "" {
		// A person's own bookkeeping (pins, inbox marks, snoozes:
		// [ChangePersonUpdated]) is not the company's work, and would tell
		// every reader when a colleague read their inbox; naming the kind
		// still reads it.
		add("h.kind <> ?", string(ChangePersonUpdated))
	}
	if len(q.Kinds) > 0 {
		values := make([]any, 0, len(q.Kinds))
		var moved []string
		for _, kind := range q.Kinds {
			values = append(values, string(kind))
			for _, field := range kind.MovedFields() {
				moved = append(moved, "json_extract(h.fields_json, '$."+field+"') IS NOT NULL")
			}
		}
		clause := "h.kind IN (" + placeholders(len(values)) + ")"
		if len(moved) > 0 {
			clause = "(" + clause + " OR (h.kind <> 'created' AND (" + strings.Join(moved, " OR ") + ")))"
		}
		add(clause, values...)
	}
	if actor := strings.TrimSpace(q.Actor); actor != "" {
		add("h.actor = ?", actor)
	}
	// Author kinds are the statement's arms, not a clause
	// ([activityStatement]); each once, or its commits appear twice.
	for _, kind := range q.ActorKinds {
		if !slices.Contains(p.actorKinds, any(string(kind))) {
			p.actorKinds = append(p.actorKinds, string(kind))
		}
	}
	if assignee := strings.TrimSpace(q.Assignee); assignee != "" {
		// The task's current assignee: "what happened to the work X
		// holds", not "what did X do", which is `actor`.
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
		at, err := statelog.ParsePosition(cursor)
		if err != nil {
			return activityPredicate{}, err
		}
		// Strictly before: newest first, the cursor names the previous
		// page's last row.
		add("h.log_seq < ?", at.Packed())
	}
	return p, nil
}
