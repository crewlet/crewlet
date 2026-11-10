package worklet

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A seat's READY work, in the order the work pickup offers it (ADR-0112).
//
// # Ready
//
// Assigned to the seat, `in_progress` or `todo`, not archived, not mid-merge,
// not in the trash, past its start date if it has one, waiting on no open
// blocker, and not held on a question the seat itself asked on it and nobody
// has answered yet. `in_review` is ready on the same terms for the seat that
// holds it when somebody else moved it into review: a seat never reviews what
// it submitted itself.
//
// # The order
//
// Started work first, then the reviews the seat holds, then its priority list
// in its lead's order, then the queue: priority, then due date, the order
// `my_work` lists `assigned` in.
//
// # What a pickup keys on, and whom its turn works for
//
// [ReadyItem.Moved] is the last change to the item that would have woken the
// seat, so neither its own comment nor a change that only tells it (another
// agent's words that ask nothing, a watcher added) hands it the same item
// again. [ReadyItem.Quiet] is when the item went quiet without a typed answer
// from the seat, which the pickup rechecks once a day (ADR-0122).
// [ReadyItem.For] is whom the item's work is for, so the turn is held to
// their apps (ADR-0109).

// ReadyRows is one page of ready work: the pickup reads the next page only
// when every item on this one was already offered.
const ReadyRows = MyWorkRows

// MaxReady is how many items of each status the order is taken over: a seat
// holding more is offered its first MaxReady of each, and told so.
const MaxReady = 200

// ReadyWorkQuery asks for one page of a seat's ready work.
type ReadyWorkQuery struct {
	// Handle is whose. Required.
	Handle string

	// Level is the read level; a pickup reads at the node's own copy.
	Level statelog.ReadLevel

	// Page is which page of [ReadyRows] items, from 0.
	Page int
}

// ReadyItem is one item a seat could start now, and why it sits where it does.
type ReadyItem struct {
	TaskRow

	// Why is what put it at its place ([types.PickupWhy]).
	Why types.PickupWhy `json:"why"`

	// Moved is the log position of the last change to the item that would
	// have woken the seat, zero where none has, and MovedAt that change's
	// instant (the item's creation where Moved is zero).
	Moved   uint64    `json:"moved"`
	MovedAt time.Time `json:"moved_at"`

	// Progressed is the log position of the seat's own last change to a
	// started item other than a comment: its progress, which hands the
	// item over again. Zero on an item not started.
	Progressed uint64 `json:"progressed,omitempty"`

	// Quiet is when the item went quiet for the seat: the latest of MovedAt,
	// the start it waited until and the seat's own last write on it since.
	// Zero where that last write is a typed answer (typedanswer.go), which
	// holds the item until a change would wake the seat again (ADR-0122).
	Quiet time.Time `json:"quiet,omitzero"`

	// For is whom the item's work is for: the writer of its last
	// assignment by somebody else, else its reporter; nil for the seat's
	// own work.
	For *types.WorkingFor `json:"for,omitempty"`
}

// ReadyWork is one page of a seat's ready items in pickup order.
type ReadyWork struct {
	Handle string      `json:"handle"`
	Items  []ReadyItem `json:"items"`

	// Total is how many ready items the order holds, More whether a page
	// follows this one, and Capped whether a status held more than
	// [MaxReady], so Total is a floor.
	Total  int  `json:"total"`
	More   bool `json:"more,omitempty"`
	Capped bool `json:"capped,omitempty"`

	// Fingerprint names the whole ordered set, every item's id, status and
	// version, so a pickup that found every item offered asks again only
	// once something in it moved.
	Fingerprint string `json:"fingerprint"`

	Level      statelog.ReadLevel `json:"read_level"`
	Complete   bool               `json:"complete"`
	Incomplete *Incomplete        `json:"incomplete,omitempty"`
}

// ReadyWork answers one page of what a seat could start now, at `now`.
func (r *Reader) ReadyWork(ctx context.Context, q ReadyWorkQuery, now time.Time) (ReadyWork, error) {
	if q.Handle == "" {
		return ReadyWork{}, fmt.Errorf("worklet: a ready-work read names no seat; " +
			"the pickup names the seat it is deciding for")
	}
	if q.Level == "" {
		return ReadyWork{}, fmt.Errorf("worklet: a ready-work read names no level; " +
			"the pickup states the level it reads at")
	}
	if q.Page < 0 {
		return ReadyWork{}, fmt.Errorf("worklet: a ready-work page is %d; pages count from 0", q.Page)
	}
	out := ReadyWork{Handle: q.Handle, Items: []ReadyItem{}}
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// THE DOMAIN, for my_work's reason: a seat's work is in every project.
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, func(tx *sql.Tx) error {
		return readReady(ctx, tx, q, now, &out)
	})
	if err != nil {
		return ReadyWork{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	if served.Incomplete != nil {
		out.Incomplete = incompleteFrom(served.Incomplete)
	}
	return out, nil
}

// readyBlock is one status's ready items for a seat, in queue order: named for
// the plan test, which holds it to a seek of the queue index.
func readyBlock(handle string, status Status, now time.Time) blockQuery {
	return blockQuery{
		where: "t.removed_at IS NULL AND t.assignee = ? AND t.status_group = ? " +
			"AND t.status = ? AND t.archived = 0 AND t.merging = 0 " +
			"AND (t.start_at IS NULL OR t.start_at <= ?) " +
			"AND NOT EXISTS (SELECT 1 FROM tracker_task_deps d " +
			"WHERE d.task_id = t.id AND d.blocker_open = 1) " +
			"AND NOT EXISTS (SELECT 1 FROM tracker_comments c " +
			"WHERE c.task_id = t.id AND c.author = ? AND c.ask <> '' " +
			"AND c.resolved = 0 AND c.answered_by IS NULL AND c.removed = 0)",
		args: []any{handle, string(status.Group()), string(status),
			store.EncodeTime(now), handle},
		terms: []sortTerm{
			{Column: "t.prio_rank", Descending: true},
			{Column: "t.due_at"}, {Column: "t.id"},
		},
	}
}

func readReady(ctx context.Context, tx *sql.Tx, q ReadyWorkQuery, now time.Time,
	out *ReadyWork) error {

	var blocks [3][]TaskRow
	for i, status := range []Status{StatusInProgress, StatusInReview, StatusTodo} {
		block := readyBlock(q.Handle, status, now)
		rows, more, err := readTasks(ctx, tx, tasksWhere(block.where, block.args),
			block.terms, MaxReady, now)
		if err != nil {
			return fmt.Errorf("worklet: read %s's %s work: %w", q.Handle, status, err)
		}
		blocks[i] = rows
		out.Capped = out.Capped || more != ""
	}
	reviews, err := theirReviews(ctx, tx, q.Handle, blocks[1])
	if err != nil {
		return err
	}
	person, _, err := readPerson(ctx, tx, q.Handle)
	if err != nil {
		return err
	}
	all := orderReady(blocks[0], reviews, blocks[2], person.Priorities)
	out.Total = len(all)
	out.Fingerprint = fingerprintOf(all)
	from := min(q.Page*ReadyRows, len(all))
	to := min(from+ReadyRows, len(all))
	out.More = to < len(all)
	out.Items = all[from:to]
	if err := loadMoved(ctx, tx, q.Handle, out.Items); err != nil {
		return err
	}
	if err := loadProgressed(ctx, tx, q.Handle, out.Items); err != nil {
		return err
	}
	if err := loadQuiet(ctx, tx, q.Handle, out.Items); err != nil {
		return err
	}
	return loadFor(ctx, tx, q.Handle, out.Items)
}

// fingerprintOf names an ordered ready set by its items' ids, statuses and
// versions.
func fingerprintOf(items []ReadyItem) string {
	h := sha256.New()
	for _, item := range items {
		fmt.Fprintf(h, "%s:%s:%d;", item.ID, item.Status, item.Version)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readyProgressArm is one started item's last change by the seat itself that
// changed a field: a comment or a write that changed nothing is no progress.
const readyProgressArm = `SELECT h.subject_id, h.log_seq, h.created_at FROM tracker_history h
	WHERE h.subject_id = ? AND h.actor = ? AND h.fields_json <> '{}'
	  AND h.kind NOT IN ('comment', 'comment_edited', 'comment_resolved', 'comment_removed')
	ORDER BY h.log_seq DESC LIMIT 1`

// loadProgressed stamps each started item with the seat's own last change to
// it that is not a comment.
func loadProgressed(ctx context.Context, tx *sql.Tx, handle string, items []ReadyItem) error {
	var args []any
	for _, item := range items {
		if item.Status == StatusInProgress {
			args = append(args, item.ID, handle)
		}
	}
	if len(args) == 0 {
		return nil
	}
	got, err := seqAt(ctx, tx, arms(len(args)/2, readyProgressArm), args)
	if err != nil {
		return fmt.Errorf("worklet: read %s's progress on its started work: %w", handle, err)
	}
	for i := range items {
		items[i].Progressed = got[items[i].ID].seq
	}
	return nil
}

// readyLastWordArm is the seat's own last write to one item after the change
// that last woke it (its log position) and the start it waited until (its
// instant), and whether that write is a typed answer. A creation, a change of
// who watches and a write that changed nothing are no answer at all.
const readyLastWordArm = `SELECT h.subject_id, h.log_seq, h.created_at, ` + typedWrite + `
	FROM tracker_history h
	WHERE h.subject_id = ? AND h.actor = ? AND h.log_seq > ? AND h.created_at >= ?
	  AND h.kind NOT IN ('` + string(ChangeCreated) + `', '` + string(ChangeWatchers) + `')
	  AND (h.fields_json <> '{}' OR h.kind IN ` + conversationKinds + `)
	ORDER BY h.log_seq DESC LIMIT 1`

// loadQuiet stamps each item with when it went quiet for the seat: the latest
// of the change that last woke it, the start it waited until and the seat's
// own last word on it since, unless that word is a typed answer.
func loadQuiet(ctx context.Context, tx *sql.Tx, handle string, items []ReadyItem) error {
	if len(items) == 0 {
		return nil
	}
	args := make([]any, 0, 4*len(items))
	for _, item := range items {
		var start int64
		if item.Start != nil {
			start = store.EncodeTime(*item.Start)
		}
		args = append(args, item.ID, handle, int64(item.Moved), start)
	}
	rows, err := tx.QueryContext(ctx, arms(len(items), readyLastWordArm), args...)
	if err != nil {
		return fmt.Errorf("worklet: read %s's last word on its ready work: %w", handle, err)
	}
	type word struct {
		at    time.Time
		typed bool
	}
	last := map[string]word{}
	for rows.Next() {
		var id string
		var seq, at int64
		var typed bool
		if serr := rows.Scan(&id, &seq, &at, &typed); serr != nil {
			_ = rows.Close()
			return fmt.Errorf("worklet: read %s's last word on its ready work: %w", handle, serr)
		}
		last[id] = word{at: store.DecodeTime(at), typed: typed}
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("worklet: read %s's last word on its ready work: %w", handle, err)
	}
	for i := range items {
		w, said := last[items[i].ID]
		if said && w.typed {
			continue
		}
		quiet := items[i].MovedAt
		if start := items[i].Start; start != nil && start.After(quiet) {
			quiet = *start
		}
		if said && w.at.After(quiet) {
			quiet = w.at
		}
		items[i].Quiet = quiet
	}
	return nil
}

// orderReady is the pickup order over the blocks and the priority list:
// started work in queue order, then the reviews the seat holds, then the
// waiting items the list names in its order, then the rest in queue order.
func orderReady(started, reviews, waiting []TaskRow, priorities []string) []ReadyItem {
	out := make([]ReadyItem, 0, len(started)+len(reviews)+len(waiting))
	for _, row := range started {
		out = append(out, ReadyItem{TaskRow: row, Why: types.PickupInProgress})
	}
	for _, row := range reviews {
		out = append(out, ReadyItem{TaskRow: row, Why: types.PickupReview})
	}
	listed := make(map[string]bool, len(priorities))
	byID := make(map[string]TaskRow, len(waiting))
	for _, row := range waiting {
		byID[row.ID] = row
	}
	for _, id := range priorities {
		if row, ok := byID[id]; ok && !listed[id] {
			listed[id] = true
			out = append(out, ReadyItem{TaskRow: row, Why: types.PickupPriorityList})
		}
	}
	for _, row := range waiting {
		if !listed[row.ID] {
			out = append(out, ReadyItem{TaskRow: row, Why: types.PickupQueue})
		}
	}
	return out
}

// arms is one statement of n arms joined by UNION ALL, each a seek of its own
// item: the newest row of each item that one predicate selects.
func arms(n int, arm string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "SELECT * FROM (" + arm + ")"
	}
	return strings.Join(parts, " UNION ALL ")
}

// readyMovedArm is the newest notice to the seat about one item that started
// its turn: a change that would have woken it, by the notifier's own decision.
const readyMovedArm = `SELECT n.task_id, n.log_seq, n.created_at
	FROM tracker_notifications n INDEXED BY tracker_notifications_woke_idx
	WHERE n.task_id = ? AND n.recipient = ? AND n.starts_turn = 1 ORDER BY n.log_seq DESC LIMIT 1`

// readyReviewArm is who moved one item into its status, or filed it where no
// status move came after: who asked for the review of an item in review.
const readyReviewArm = `SELECT h.subject_id, h.actor FROM tracker_history h
	WHERE h.subject_id = ?
	  AND (json_extract(h.fields_json, '$.status.to') IS NOT NULL OR h.kind = 'created')
	ORDER BY h.log_seq DESC LIMIT 1`

// theirReviews is the items in review handle holds that somebody else moved
// into review: a seat never reviews what it submitted itself.
func theirReviews(ctx context.Context, tx *sql.Tx, handle string, inReview []TaskRow) ([]TaskRow, error) {
	if len(inReview) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(inReview))
	for _, row := range inReview {
		args = append(args, row.ID)
	}
	rows, err := tx.QueryContext(ctx, arms(len(inReview), readyReviewArm), args...)
	if err != nil {
		return nil, fmt.Errorf("worklet: read who asked for %s's reviews: %w", handle, err)
	}
	defer rows.Close()
	askedBy := make(map[string]string, len(inReview))
	for rows.Next() {
		var id, actor string
		if err := rows.Scan(&id, &actor); err != nil {
			return nil, fmt.Errorf("worklet: read who asked for %s's reviews: %w", handle, err)
		}
		askedBy[id] = actor
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worklet: read who asked for %s's reviews: %w", handle, err)
	}
	out := make([]TaskRow, 0, len(inReview))
	for _, row := range inReview {
		if by, known := askedBy[row.ID]; known && by != handle {
			out = append(out, row)
		}
	}
	return out, nil
}

// readyFirstArm is one item's first change, the creation the seat made itself.
const readyFirstArm = `SELECT h.subject_id, h.log_seq, h.created_at FROM tracker_history h
	WHERE h.subject_id = ? ORDER BY h.log_seq LIMIT 1`

// readyAssignedArm is one item's last assignment by a person, a colleague or
// a credential other than the seat: anybody but the engine's own duties.
const readyAssignedArm = `SELECT h.subject_id, h.actor, h.actor_kind, h.actor_apps
	FROM tracker_history h
	WHERE h.subject_id = ? AND h.kind = 'assignee' AND h.actor <> ?
	  AND h.actor_kind <> 'system'
	ORDER BY h.log_seq DESC LIMIT 1`

// readyCreatedArm is one item's creation by a person, a colleague or a
// credential other than the seat: whom work filed already assigned is for.
const readyCreatedArm = `SELECT h.subject_id, h.actor, h.actor_kind, h.actor_apps
	FROM tracker_history h
	WHERE h.subject_id = ? AND h.kind = 'created' AND h.actor <> ?
	  AND h.actor_kind <> 'system'
	ORDER BY h.log_seq LIMIT 1`

// loadMoved stamps each item with the last change that would have woken handle,
// and that change's instant; an item no such change reached takes the instant
// it was created.
func loadMoved(ctx context.Context, tx *sql.Tx, handle string, items []ReadyItem) error {
	if len(items) == 0 {
		return nil
	}
	args := make([]any, 0, 2*len(items))
	for _, item := range items {
		args = append(args, item.ID, handle)
	}
	got, err := seqAt(ctx, tx, arms(len(items), readyMovedArm), args)
	if err != nil {
		return fmt.Errorf("worklet: read who last changed %s's ready work: %w", handle, err)
	}
	var lonely []any
	for i := range items {
		if at, ok := got[items[i].ID]; ok {
			items[i].Moved, items[i].MovedAt = at.seq, at.at
			continue
		}
		lonely = append(lonely, items[i].ID)
	}
	if len(lonely) == 0 {
		return nil
	}
	first, err := seqAt(ctx, tx, arms(len(lonely), readyFirstArm), lonely)
	if err != nil {
		return fmt.Errorf("worklet: read when %s's own work was filed: %w", handle, err)
	}
	for i := range items {
		if at, ok := first[items[i].ID]; ok && items[i].Moved == 0 {
			items[i].MovedAt = at.at
		}
	}
	return nil
}

type seqInstant struct {
	seq uint64
	at  time.Time
}

func seqAt(ctx context.Context, tx *sql.Tx, query string, args []any) (map[string]seqInstant, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]seqInstant{}
	for rows.Next() {
		var id string
		var seq, at int64
		if err := rows.Scan(&id, &seq, &at); err != nil {
			return nil, err
		}
		out[id] = seqInstant{seq: uint64(seq), at: store.DecodeTime(at)}
	}
	return out, rows.Err()
}

// loadFor stamps each item with whom its work is for: the writer of its last
// assignment by somebody else, else the writer that filed it, each a seat or a
// credential with the apps it wrote under; nil for the seat's own work.
func loadFor(ctx context.Context, tx *sql.Tx, handle string, items []ReadyItem) error {
	if len(items) == 0 {
		return nil
	}
	args := make([]any, 0, 2*len(items))
	for _, item := range items {
		args = append(args, item.ID, handle)
	}
	assigned, err := writersOf(ctx, tx, arms(len(items), readyAssignedArm), args)
	if err != nil {
		return fmt.Errorf("worklet: read who assigned %s's ready work: %w", handle, err)
	}
	filed, err := writersOf(ctx, tx, arms(len(items), readyCreatedArm), args)
	if err != nil {
		return fmt.Errorf("worklet: read who filed %s's ready work: %w", handle, err)
	}
	for i := range items {
		if w, ok := assigned[items[i].ID]; ok {
			items[i].For = w
		} else {
			items[i].For = filed[items[i].ID]
		}
	}
	return nil
}

// writersOf runs one statement of writer arms and reads each item's writer.
func writersOf(ctx context.Context, tx *sql.Tx, query string, args []any) (
	map[string]*types.WorkingFor, error) {

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]*types.WorkingFor{}
	for rows.Next() {
		var id, actor, kind, apps string
		if serr := rows.Scan(&id, &actor, &kind, &apps); serr != nil {
			_ = rows.Close()
			return nil, serr
		}
		out[id] = workingForWriter(AuthorKind(kind), actor, apps)
	}
	return out, closeRows(rows)
}

// closeRows closes rows and answers the error its walk ended on.
func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}

// workingForWriter is whom work a writer assigned is for: the writer's seat,
// or the credential with the apps it wrote under.
func workingForWriter(kind AuthorKind, actor, apps string) *types.WorkingFor {
	if kind != AuthorOperator {
		return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: actor}
	}
	w := &types.WorkingFor{Kind: types.WorkingForCredential, Login: actor}
	if apps != "" {
		// A LIST THE APPLIER WROTE; one this build cannot read grants
		// nothing, the closed end.
		var grants []iam.Grant
		if json.Unmarshal([]byte(apps), &grants) == nil {
			w.Apps = grants
		}
	}
	return w
}
