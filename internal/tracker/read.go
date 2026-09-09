package tracker

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The reader, and the two facts it never conflates.
//
// `read_level` is HOW FRESH what was read is. `complete` is whether everything
// that should have been read was there to read. They are different questions
// with different answers, and folding them into one name is the mistake the
// pair exists to prevent: a set read is served at the level asked for FOR THE
// ROWS IT RETURNS, and makes no completeness claim at all whenever a deferred
// record's declared scope could intersect the question.
//
// The reason is precise rather than cautious. A filter tests columns —
// project, status, assignee, a field value, archived, removed — that live
// INSIDE a record this node could not decode, so a deferred create is not a
// stale row but an ABSENCE. Nothing about the rows that ARE returned says so.

// TaskRow is one row of an answer.
//
// A FIXED SHAPE rather than the whole task, because the answer is read by a
// model as often as by a screen and a body would put a task's 64 KiB into
// every row of a fifty-row page. What a caller needs beyond this is a
// single-task read, which is flat at any age.
type TaskRow struct {
	ID          string      `json:"id"`
	Key         string      `json:"key"`
	Title       string      `json:"title"`
	Status      Status      `json:"status"`
	StatusGroup StatusGroup `json:"status_group"`
	Priority    Priority    `json:"priority"`
	Assignee    string      `json:"assignee,omitempty"`
	Project     string      `json:"project"`
	Sprint      *int        `json:"sprint,omitempty"`
	Parent      string      `json:"parent,omitempty"`
	Depth       int         `json:"depth,omitempty"`
	Start       *time.Time  `json:"start,omitempty"`
	Due         *time.Time  `json:"due,omitempty"`

	// Overdue is the row's own copy of the preset's predicate, so a
	// renderer never re-derives it differently — which is how one screen
	// shows a task as overdue and another does not.
	Overdue bool `json:"overdue,omitempty"`

	EstimateMinutes int     `json:"estimate_min,omitempty"`
	Points          float64 `json:"points,omitempty"`

	// Blocked is DATA a filter and a badge read. It gates nothing: closing
	// a task with open blockers is allowed, and the facts a reviewer would
	// want are returned beside the status rather than enforced.
	Blocked  bool      `json:"blocked,omitempty"`
	Archived bool      `json:"archived,omitempty"`
	Rank     Rank      `json:"rank,omitempty"`
	Updated  time.Time `json:"updated"`
	Version  uint64    `json:"version"`
}

// Incomplete says what an answer could not account for.
type Incomplete struct {
	// Records is how many this node holds that it cannot decode and whose
	// scope could intersect the question.
	Records int `json:"records"`

	// From is where the lowest of them sits, which is the position a build
	// that can read them resumes at.
	From statelog.Position `json:"from"`

	// Scope names what is affected. It does NOT say in which direction,
	// and this build cannot compute the direction — that is what "cannot
	// decode" means.
	Scope []string `json:"scope"`

	// Version is the record version this node could not read, which is the
	// one number an operator needs to pick a build.
	Version int `json:"version"`
}

// Answer is one query's result and everything a caller needs to judge it.
type Answer struct {
	Rows []TaskRow `json:"rows"`

	// TotalHint is capped by construction, because an exact count over an
	// unbounded set is the query that turns a poll into a scan.
	TotalHint  int    `json:"total_hint"`
	NextCursor string `json:"next_cursor,omitempty"`

	// Level is the level ACTUALLY served, set from the statement that
	// satisfied the barrier — never the level asked for. A level never
	// silently downgrades, so the two can only differ by a refusal.
	Level statelog.ReadLevel `json:"read_level"`

	// LogSeq is this node's committed position and AppliedThrough the
	// prefix whose consequences it holds. Two numbers because a node
	// applying nothing while its position advances looks identical to one
	// that is caught up.
	LogSeq         uint64 `json:"log_seq"`
	AppliedThrough uint64 `json:"applied_through"`

	// LogLag is ABSENT rather than zero when the broker could not be
	// reached: a read asks how far behind an answer may be, and an
	// unreachable broker answers "not at all".
	LogLag *uint64 `json:"log_lag,omitempty"`

	Complete   bool        `json:"complete"`
	Incomplete *Incomplete `json:"incomplete,omitempty"`
}

// TotalHintCeiling is where the count stops.
//
// Ten thousand, and the answer says "10000+" past it rather than counting on:
// an exact total over an unbounded set is the one query in this grammar that
// turns a sixty-second poll into a scan, and nobody reading a board needs the
// difference between eleven thousand and twelve.
const TotalHintCeiling = 10_000

// PageDefault and PageMax bound one page.
const (
	PageDefault = 50
	PageMax     = 500
)

// Reader answers queries from this node's own tables.
type Reader struct{ db *store.DB }

// NewReader builds the read surface over a node's replicated estate.
func NewReader(db *store.DB) *Reader { return &Reader{db: db} }

// Tasks answers one query.
//
// ONE READ TRANSACTION for the rows, the count and the coverage probe — which
// is what stops the total describing a different snapshot from the rows it
// counts, and stops a completeness claim being made against state the rows
// were not read from.
func (r *Reader) Tasks(ctx context.Context, q Query, now time.Time) (Answer, error) {
	where, args, err := compile(q, now)
	if err != nil {
		return Answer{}, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = PageDefault
	}
	if limit > PageMax {
		limit = PageMax
	}
	order := orderBy(q)

	answer := Answer{Level: q.Level, Complete: true}
	err = r.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, cursor, err := readTasks(ctx, tx, where, args, order, limit)
		if err != nil {
			return err
		}
		answer.Rows, answer.NextCursor = rows, cursor

		hint, err := countHint(ctx, tx, where, args)
		if err != nil {
			return err
		}
		answer.TotalHint = hint

		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		answer.LogSeq, answer.AppliedThrough = position, applied

		incomplete, err := coverage(ctx, tx, q)
		if err != nil {
			return err
		}
		if incomplete != nil {
			answer.Complete, answer.Incomplete = false, incomplete
		}
		return nil
	})
	if err != nil {
		return Answer{}, err
	}
	return answer, nil
}

// compile turns a parsed query into a predicate and its arguments.
//
// EVERY VALUE IS BOUND, never interpolated: the only thing this function
// composes into SQL is its own column and operator names, from closed sets
// declared in the grammar. A filter's value reaching a statement as text is
// how a saved view becomes a way to run a query nobody wrote.
func compile(q Query, now time.Time) (string, []any, error) {
	var where []string
	var args []any
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}

	switch {
	case q.Scope.Project != "":
		add("t.project_key = ?", q.Scope.Project)
	case q.Scope.Workspace:
		// The Everything level, and it is explicit: no predicate.
	}

	// EVERY QUERY EXCLUDES A REMOVED TASK except the two that are about
	// them. ONE predicate for a removed task's whole life, at any age —
	// which is what makes a restore at any age answer exactly what it
	// answered before.
	add("t.removed_at IS NULL")

	if len(q.Status) > 0 {
		add("t.status IN ("+placeholders(len(q.Status))+")", anyOf(q.Status)...)
	}
	if len(q.StatusNot) > 0 {
		add("t.status NOT IN ("+placeholders(len(q.StatusNot))+")", anyOf(q.StatusNot)...)
	}
	if len(q.StatusGroups) > 0 {
		add("t.status_group IN ("+placeholders(len(q.StatusGroups))+")",
			anyOf(q.StatusGroups)...)
	}
	if len(q.Priorities) > 0 {
		add("t.priority IN ("+placeholders(len(q.Priorities))+")",
			anyOf(q.Priorities)...)
	}
	if len(q.Types) > 0 {
		add("t.type IN ("+placeholders(len(q.Types))+")", anyOf(q.Types)...)
	}
	if len(q.Unit) > 0 {
		add("t.filed_unit IN ("+placeholders(len(q.Unit))+")", anyOf(q.Unit)...)
	}
	if len(q.RoutingUnit) > 0 {
		add("t.routing_unit IN ("+placeholders(len(q.RoutingUnit))+")",
			anyOf(q.RoutingUnit)...)
	}
	if handles := q.Assignee; len(handles) > 0 {
		// `none` is a VALUE rather than a missing filter: "unassigned"
		// is a real question and the commonest half of the queue preset.
		clause, values := handleClause("t.assignee", handles)
		add(clause, values...)
	}
	if len(q.Reporter) > 0 {
		clause, values := handleClause("t.reporter", q.Reporter)
		add(clause, values...)
	}
	for _, join := range []struct {
		handles []string
		table   string
	}{
		{q.Collaborator, "tracker_collaborators"},
		{q.Watcher, "tracker_watchers"},
	} {
		if len(join.handles) == 0 {
			continue
		}
		add("EXISTS (SELECT 1 FROM "+join.table+" x WHERE x.task_id = t.id "+
			"AND x.handle IN ("+placeholders(len(join.handles))+"))",
			anyOf(join.handles)...)
	}
	if len(q.ChecklistAssignee) > 0 {
		add("EXISTS (SELECT 1 FROM tracker_checklist_items x "+
			"WHERE x.task_id = t.id AND x.assignee IN ("+
			placeholders(len(q.ChecklistAssignee))+"))", anyOf(q.ChecklistAssignee)...)
	}
	if len(q.Tags.Tags) > 0 {
		clause, values := tagClause(q.Tags)
		add(clause, values...)
	}
	if q.Parent != "" {
		add("t.parent_id = ?", q.Parent)
	}
	if q.Root != "" {
		add("EXISTS (SELECT 1 FROM tracker_task_closure c "+
			"WHERE c.descendant_id = t.id AND c.ancestor_id = ?)", q.Root)
	}
	if q.Batch != "" {
		add("t.batch_id = ?", q.Batch)
	}
	if q.References != "" {
		add("EXISTS (SELECT 1 FROM tracker_references r "+
			"JOIN tracker_task_keys k ON k.task_id = r.to_task "+
			"WHERE r.from_task = t.id AND k.key = ?)", q.References)
	}
	if q.LinkedPage != "" {
		add("EXISTS (SELECT 1 FROM tracker_relations x WHERE x.task_id = t.id "+
			"AND x.kind = ? AND x.other_id = ?)", string(RelationPage), q.LinkedPage)
	}
	if q.Goal != "" {
		add("EXISTS (SELECT 1 FROM tracker_goal_target_refs g "+
			"WHERE g.goal_id = ? AND g.kind = 'task' AND g.ref = t.id)", q.Goal)
	}
	if q.Blocked != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_task_deps d " +
			"WHERE d.task_id = t.id AND d.blocker_open = 1)"
		if !*q.Blocked {
			clause = "NOT " + clause
		}
		add(clause)
	}
	if q.HasChildren != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_tasks c WHERE c.parent_id = t.id)"
		if !*q.HasChildren {
			clause = "NOT " + clause
		}
		add(clause)
	}
	if q.HasParent != nil {
		if *q.HasParent {
			add("t.parent_id IS NOT NULL")
		} else {
			add("t.parent_id IS NULL")
		}
	}
	if q.HasOpenAsks != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_comments m WHERE m.task_id = t.id " +
			"AND m.ask <> '' AND m.resolved = 0 AND m.answered_by IS NULL AND m.removed = 0)"
		if !*q.HasOpenAsks {
			clause = "NOT " + clause
		}
		add(clause)
	}
	for column, filter := range q.Dates {
		clause, values, err := dateClause(column, filter)
		if err != nil {
			return "", nil, err
		}
		add(clause, values...)
		if filter.Overdue {
			// THE ALIAS CARRIES ITS OPEN CONDITION, which is what makes
			// it the same predicate as the preset that means the same
			// thing rather than a second one that drifts.
			add("t.status_group IN ('not_started','active')")
		}
	}
	for column, filter := range map[string]*NumFilter{
		"t.estimate_min": q.Estimate, "t.points": q.Points,
		"t.spend_tokens": q.Spend,
	} {
		if filter == nil {
			continue
		}
		clause, values := numClause(column, *filter)
		add(clause, values...)
	}
	for _, flag := range q.Flags {
		column, ok := flagColumn(flag)
		if !ok {
			return "", nil, fmt.Errorf("tracker: %q is not an attention flag", flag)
		}
		add(column + " = 1")
	}

	switch q.Archived {
	case ArchivedExclude:
		// A JOIN RATHER THAN A COLUMN: a project's archive is the
		// project's own fact, and copying it onto every task was the one
		// unbounded cross-object write this design removed.
		add("t.archived = 0 AND NOT EXISTS (SELECT 1 FROM tracker_projects p " +
			"WHERE p.key = t.project_key AND p.archived = 1)")
	case ArchivedOnly:
		add("(t.archived = 1 OR EXISTS (SELECT 1 FROM tracker_projects p " +
			"WHERE p.key = t.project_key AND p.archived = 1))")
	}

	switch {
	case q.ShowClosed.All:
	case q.ShowClosed.Recent > 0:
		// THE WINDOW APPLIES TO THE WHOLE FINISHED SET, so a task
		// cancelled inside it is in the answer exactly as one done
		// inside it is — which is what the finish stamp being by GROUP
		// buys, and why the board's Cancelled column is not empty.
		add("(t.status_group IN ('not_started','active') OR "+
			"(t.finished_at IS NOT NULL AND t.finished_at >= ?))",
			store.EncodeTime(now.Add(-q.ShowClosed.Recent)))
	default:
		add("t.status_group IN ('not_started','active')")
	}

	if len(q.Any) > 0 {
		var branches []string
		for _, branch := range q.Any {
			clause, values, err := compile(branch, now)
			if err != nil {
				return "", nil, err
			}
			branches = append(branches, "("+clause+")")
			args = append(args, values...)
		}
		// ONE LEVEL, ANDed with the top-level keys — which is what makes
		// a disjunction a filter rather than a second query.
		where = append(where, "("+strings.Join(branches, " OR ")+")")
	}

	if q.Cursor != "" {
		clause, values, err := cursorClause(q)
		if err != nil {
			return "", nil, err
		}
		add(clause, values...)
	}
	return strings.Join(where, " AND "), args, nil
}

// handleClause builds a handle filter, with `none` as a real value.
func handleClause(column string, handles []string) (string, []any) {
	var named []any
	none := false
	for _, handle := range handles {
		if handle == "none" {
			none = true
			continue
		}
		named = append(named, handle)
	}
	switch {
	case none && len(named) > 0:
		return "(" + column + " = '' OR " + column + " IN (" +
			placeholders(len(named)) + "))", named
	case none:
		return column + " = ''", nil
	}
	return column + " IN (" + placeholders(len(named)) + ")", named
}

// tagClause builds the three tag modes.
func tagClause(filter TagFilter) (string, []any) {
	args := make([]any, 0, len(filter.Tags)+1)
	for _, tag := range filter.Tags {
		args = append(args, tag)
	}
	exists := "EXISTS (SELECT 1 FROM tracker_task_tags x WHERE x.task_id = t.id " +
		"AND x.slug IN (" + placeholders(len(filter.Tags)) + "))"
	switch filter.Mode {
	case TagNone:
		return "NOT " + exists, args
	case TagAll:
		// EVERY tag, which is a count rather than a membership: a task
		// carrying two of three is not a match, and an IN test cannot
		// say so.
		return "(SELECT COUNT(DISTINCT x.slug) FROM tracker_task_tags x " +
				"WHERE x.task_id = t.id AND x.slug IN (" +
				placeholders(len(filter.Tags)) + ")) = ?",
			append(args, len(filter.Tags))
	}
	return exists, args
}

// dateColumns maps a grammar key to the column it filters.
//
// `status_entered` reads the EFFECTIVE instant because it is the start of a
// duration; the rest read the AUTHORED one, because each is a wall-clock bound
// the caller typed and a clamp would answer a different question from the one
// on the screen.
var dateColumns = map[string]string{
	"due": "t.due_at", "start": "t.start_at", "created": "t.created_at",
	"updated": "t.updated_at", "done": "t.done_at", "closed": "t.closed_at",
	"finished": "t.finished_at", "status_entered": "t.status_entered_at",
}

func dateClause(key string, filter DateFilter) (string, []any, error) {
	column, ok := dateColumns[key]
	if !ok {
		return "", nil, fmt.Errorf("tracker: %q names no date column", key)
	}
	from := store.EncodeTime(filter.From.At)
	switch filter.Op {
	case DateLT:
		return column + " < ?", []any{from}, nil
	case DateLTE:
		return column + " <= ?", []any{from}, nil
	case DateGT:
		return column + " > ?", []any{from}, nil
	case DateGTE:
		return column + " >= ?", []any{from}, nil
	case DateRange:
		// HALF-OPEN, which is what makes "this week" contain every
		// instant of Sunday.
		return column + " >= ? AND " + column + " < ?",
			[]any{from, store.EncodeTime(filter.To.At)}, nil
	}
	return "", nil, fmt.Errorf("tracker: %q is not a date comparison", filter.Op)
}

func numClause(column string, filter NumFilter) (string, []any) {
	switch filter.Op {
	case NumLT:
		return column + " < ?", []any{filter.From}
	case NumLTE:
		return column + " <= ?", []any{filter.From}
	case NumGT:
		return column + " > ?", []any{filter.From}
	case NumGTE:
		return column + " >= ?", []any{filter.From}
	case NumRange:
		return column + " >= ? AND " + column + " <= ?", []any{filter.From, filter.To}
	case NumNull:
		return column + " = 0", nil
	case NumNotNull:
		return column + " <> 0", nil
	}
	return "1", nil
}

// flagColumn maps an attention flag to its column, from a CLOSED SET — the one
// place a grammar value reaches a statement as a name rather than a bound
// value, and therefore the one that has to be enumerated.
func flagColumn(flag string) (string, bool) {
	switch flag {
	case "cycle":
		return "t.cycle", true
	case "too_deep":
		return "t.too_deep", true
	case "inconsistent_project":
		return "t.inconsistent_project", true
	case "key_collision":
		return "t.key_collision", true
	}
	return "", false
}

// orderBy compiles the sort, always ending in the id.
//
// THE TRAILING ID IS WHAT MAKES TWO NODES RENDER ONE BOARD IN ONE ORDER even
// if a duplicate rank ever occurs — and it is free, because the three
// rank-bearing indexes carry it as a trailing column and still serve the range
// scan with no sort step.
func orderBy(q Query) string {
	columns := map[string]string{
		"rank": "t.rank", "updated": "t.updated_at", "due": "t.due_at",
		"priority": "t.prio_rank", "created": "t.created_at",
		"title": "t.title", "estimate": "t.estimate_min", "points": "t.points",
		"spend": "t.spend_tokens", "status_entered": "t.status_entered_at",
	}
	var terms []string
	for _, sort := range q.Sort {
		column, known := columns[sort.Key]
		if !known {
			continue
		}
		if sort.Descending {
			column += " DESC"
		}
		terms = append(terms, column)
	}
	if len(terms) == 0 {
		// The default is the manual order inside a project and the most
		// recently touched everywhere else, because a rank is only an
		// order within the container that owns it.
		if q.Scope.Project != "" {
			terms = append(terms, "t.rank")
		} else {
			terms = append(terms, "t.updated_at DESC")
		}
	}
	return strings.Join(append(terms, "t.id"), ", ")
}

// cursorClause turns an opaque page cursor back into a predicate.
//
// KEYSET RATHER THAN OFFSET, because an offset re-reads and re-sorts every row
// it skips: page fifty of a board costs fifty times page one, and a row
// inserted between two polls shifts every page after it.
func cursorClause(q Query) (string, []any, error) {
	raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
	if err != nil {
		return "", nil, fmt.Errorf("tracker: the cursor is not one this surface "+
			"minted: %w", err)
	}
	var page struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || page.ID == "" {
		return "", nil, fmt.Errorf("tracker: the cursor names no row to resume " +
			"after")
	}
	return "t.id > ?", []any{page.ID}, nil
}

// mintCursor encodes the page's own resume point.
func mintCursor(last TaskRow) string {
	body, err := json.Marshal(struct {
		ID string `json:"id"`
	}{ID: last.ID})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

func readTasks(ctx context.Context, tx *sql.Tx, where string, args []any,
	order string, limit int) ([]TaskRow, string, error) {

	// ONE MORE THAN THE PAGE, which is how the answer knows whether there
	// is another page without a second count.
	query := `SELECT t.id, t.key, t.title, t.status, t.status_group, t.priority,
	                 t.assignee, t.project_key, t.sprint_number, t.parent_id,
	                 t.depth, t.start_at, t.due_at, t.estimate_min, t.points,
	                 t.archived, t.rank, t.updated_at, t.version,
	                 EXISTS (SELECT 1 FROM tracker_task_deps d
	                         WHERE d.task_id = t.id AND d.blocker_open = 1)
	          FROM tracker_tasks t
	          WHERE ` + where + `
	          ORDER BY ` + order + `
	          LIMIT ?`
	rows, err := tx.QueryContext(ctx, query, append(append([]any{}, args...), limit+1)...)
	if err != nil {
		return nil, "", fmt.Errorf("tracker: read tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TaskRow
	for rows.Next() {
		var row TaskRow
		var sprint, parent, start, due sql.NullInt64
		var parentID sql.NullString
		var archived, blocked int
		var updated int64
		var version int64
		if err := rows.Scan(&row.ID, &row.Key, &row.Title, &row.Status,
			&row.StatusGroup, &row.Priority, &row.Assignee, &row.Project,
			&sprint, &parentID, &row.Depth, &start, &due, &row.EstimateMinutes,
			&row.Points, &archived, &row.Rank, &updated, &version, &blocked); err != nil {
			return nil, "", fmt.Errorf("tracker: read a task row: %w", err)
		}
		_ = parent
		if sprint.Valid {
			n := int(sprint.Int64)
			row.Sprint = &n
		}
		if parentID.Valid {
			row.Parent = parentID.String
		}
		if start.Valid {
			at := store.DecodeTime(start.Int64)
			row.Start = &at
		}
		if due.Valid {
			at := store.DecodeTime(due.Int64)
			row.Due = &at
		}
		row.Archived = archived == 1
		row.Blocked = blocked == 1
		row.Updated = store.DecodeTime(updated)
		row.Version = uint64(version)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("tracker: walk the task rows: %w", err)
	}
	cursor := ""
	if len(out) > limit {
		out = out[:limit]
		cursor = mintCursor(out[len(out)-1])
	}
	// AND THE OVERDUE FLAG IS DERIVED ONCE, here, so every renderer agrees.
	for i := range out {
		out[i].Overdue = out[i].Due != nil &&
			out[i].StatusGroup.Open() && out[i].Due.Before(time.Now())
	}
	return out, cursor, nil
}

// countHint counts to the ceiling and stops.
func countHint(ctx context.Context, tx *sql.Tx, where string, args []any) (int, error) {
	query := `SELECT COUNT(*) FROM (SELECT 1 FROM tracker_tasks t WHERE ` +
		where + ` LIMIT ?)`
	var n int
	if err := tx.QueryRowContext(ctx, query,
		append(append([]any{}, args...), TotalHintCeiling+1)...).Scan(&n); err != nil {
		return 0, fmt.Errorf("tracker: count the answer: %w", err)
	}
	return n, nil
}

// readCheckpoint reads this node's own position and applied prefix.
func readCheckpoint(ctx context.Context, tx *sql.Tx) (position, applied uint64, err error) {
	var generation, seq int64
	err = tx.QueryRowContext(ctx,
		`SELECT generation, seq FROM statelog_cursor WHERE stream = ?`,
		trackerStream).Scan(&generation, &seq)
	switch {
	case err == sql.ErrNoRows:
		return 0, 0, nil
	case err != nil:
		return 0, 0, fmt.Errorf("tracker: read the checkpoint: %w", err)
	}
	packed := uint64(generation)*uint64(statelog.GenerationStride) + uint64(seq)

	// APPLIED THROUGH is the lowest deferred position minus one, or the
	// checkpoint when this node holds none. Two numbers, because a node
	// applying nothing while its position advances looks identical to one
	// that is caught up.
	var lowest sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MIN(position) FROM tracker_log_deferred`).Scan(&lowest); err != nil {
		return 0, 0, fmt.Errorf("tracker: read the deferred floor: %w", err)
	}
	if lowest.Valid && uint64(lowest.Int64) > 0 {
		return packed, uint64(lowest.Int64) - 1, nil
	}
	return packed, packed, nil
}

// coverage answers whether this node holds a record it cannot decode whose
// scope could intersect the question.
//
// IT NAMES WHAT IS AFFECTED AND NOT IN WHICH DIRECTION, because this build
// cannot compute the direction — that is what "cannot decode" means. Rows may
// be missing, rows that should have left may still be present, and values on
// the rows returned may be behind.
func coverage(ctx context.Context, tx *sql.Tx, q Query) (*Incomplete, error) {
	scope := ReadScope(q)
	closure := scope.Closure()
	roots := scope.Roots()
	if len(closure) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(closure)+len(roots)*2)
	for _, path := range closure {
		args = append(args, path)
	}
	query := `SELECT COUNT(*), MIN(d.position), MIN(d.version)
	          FROM tracker_log_deferred_scope s
	          JOIN tracker_log_deferred d ON d.position = s.position
	          WHERE s.path IN (` + placeholders(len(closure)) + `)`
	for _, root := range roots {
		query += ` OR s.path = ? OR s.path LIKE ? ESCAPE '\'`
		args = append(args, root, store.LikePrefix(root+statelog.ScopeSeparator))
	}
	var count int
	var lowest, version sql.NullInt64
	if err := tx.QueryRowContext(ctx, query, args...).
		Scan(&count, &lowest, &version); err != nil {
		return nil, fmt.Errorf("tracker: probe the answer's coverage: %w", err)
	}
	if count == 0 {
		return nil, nil
	}
	return &Incomplete{
		Records: count,
		From: statelog.Position{
			Stream:     trackerStream,
			Generation: uint32(lowest.Int64 / statelog.GenerationStride),
			Seq:        uint64(lowest.Int64 % statelog.GenerationStride),
		},
		Scope:   scope.Paths,
		Version: int(version.Int64),
	}, nil
}

// anyOf widens a typed slice for a bound IN list.
func anyOf[T ~string](values []T) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	return out
}

// trackerStream is the domain's stream name, read from the declaration rather
// than written again — one spelling, so the framework and every reader compare
// against the same one.
var trackerStream = Domain{}.Stream().Name

// placeholders is a bound-parameter list of n slots.
//
// THE ONLY THING THIS PACKAGE COMPOSES INTO SQL besides its own column names:
// a value never reaches a statement as text, so a filter cannot become a way
// to run a query nobody wrote.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
