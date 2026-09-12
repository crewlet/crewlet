package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
)

// Reading ONE task, and why it is not a board query with a filter.
//
// A board answers "what is there" over many rows, narrowly: it returns the
// columns a card renders and nothing else, because a hundred rows carrying
// their whole documents is a page nobody can afford. One task is the opposite
// question — "tell me everything about this" — and every part of the answer
// comes from a different table.
//
// So they are two reads with two shapes, and the thing they share is the
// COVERAGE they report. Both answer at a read level, both say how far this
// node has applied, and both name what they could not account for. A detail
// read that skipped that would be the one screen in the product where a
// lagging node looks identical to a caught-up one.

// ErrNoTask reports a task this node has no row for.
//
// ITS OWN SENTINEL, because the caller's answer differs: a tool says "no such
// task" to a model, an API says 404, and neither should say either when what
// actually happened is that this node has not applied the record that creates
// it. The detail read distinguishes those — see [TaskDetail.Complete].
var ErrNoTask = errors.New("tracker: no such task")

// ErrNoProject reports a project this node has no row for.
//
// ITS OWN SENTINEL beside [ErrNoTask], for the same reason and with the same
// caveat: a project reader that answered an empty listing for an unknown key
// would tell a model the project is empty when what actually happened is that
// it typed the key wrong.
var ErrNoProject = errors.New("tracker: no such project")

// DetailWants says which parts of the answer to assemble.
//
// EXPLICIT rather than "everything", because the parts have very different
// costs: the task itself is one indexed read, the comments are a thread that
// can be hundreds of rows, and the history is append-only and grows for the
// life of the task. A caller rendering a list of links does not want either.
type DetailWants struct {
	Comments bool
	History  bool
	Links    bool

	// HistoryLimit caps the feed, newest first. Zero takes
	// [DetailHistoryDefault].
	HistoryLimit int

	// CommentCursor pages the thread. Empty starts at the newest.
	CommentCursor string
}

// DetailHistoryDefault is how many history rows a detail read returns when the
// caller says nothing.
//
// FIFTY, which is the activity panel's own first screen. It is a cap rather
// than a page because the caller that wants more has `work_activity`, which
// pages properly with a cursor — and a detail read that grew without bound
// would put a task's whole life inside one tool result.
const DetailHistoryDefault = 50

// TaskDetail is one task and everything asked for beside it.
type TaskDetail struct {
	Task Task `json:"task"`

	Comments []Comment      `json:"comments,omitempty"`
	History  []HistoryEntry `json:"history,omitempty"`

	// CommentsCursor pages the thread, and is empty when this page is the
	// whole of it.
	//
	// PRESENT RATHER THAN A COUNT, because the caller's question is "is
	// there more" and the answer that lets them act is the cursor itself —
	// a total would need a second scan over a table that grows for the
	// life of the task, to tell them something the cursor already says.
	CommentsCursor string `json:"comments_cursor,omitempty"`

	// Links are BOTH DIRECTIONS, so a reader sees "blocks" and "blocked
	// by" without a second query and without knowing which end authored
	// which.
	Links []DetailLink `json:"links,omitempty"`

	// Blocked is the SAME predicate [TaskRow.Blocked] carries — an open
	// dependency edge — computed here rather than derived from Links,
	// which say what the relations ARE and not whether any blocker is
	// still open.
	//
	// It is on the answer because a renderer that showed the badge on a
	// board row and could not show it on the item itself is one that
	// contradicts its own list a click later, and because deriving it in
	// the browser would be a second definition of blocked.
	Blocked bool `json:"blocked,omitempty"`

	// The coverage half, identical in meaning to a board's — see [Answer].
	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// DetailLink is one relation as a reader sees it.
type DetailLink struct {
	Kind  RelationKind `json:"kind"`
	Other string       `json:"other"`

	// Key, Title and Status are the other end's, resolved here so a
	// renderer does not fetch a row per link.
	Key    string `json:"key,omitempty"`
	Title  string `json:"title,omitempty"`
	Status Status `json:"status,omitempty"`

	Note string `json:"note,omitempty"`

	// Derived marks the half nobody authored, so an editor knows which end
	// to change and a renderer can say so.
	Derived bool `json:"derived,omitempty"`

	// OneSided marks an edge whose mirror was never written, and
	// OneSidedFinal one whose mirror was refused permanently.
	OneSided      bool `json:"one_sided,omitempty"`
	OneSidedFinal bool `json:"one_sided_final,omitempty"`
}

// HistoryEntry is one change as a reader sees it.
type HistoryEntry struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`
	CommentID  string     `json:"comment_id,omitempty"`
	Excerpt    string     `json:"excerpt,omitempty"`
	TurnID     string     `json:"turn_id,omitempty"`

	// Fields is what changed, as the notification snapshot recorded it.
	Fields map[string]any `json:"fields,omitempty"`

	// Quiet marks a commit that ANNOUNCED NOTHING — one that carried no
	// notification at all. It is a fact about the change rather than
	// about its importance — a bulk edit is quiet by construction — and
	// it is what an activity feed renders differently.
	//
	// A LOUD COMMIT MAY STILL HAVE REACHED NOBODY: who was told is
	// resolved against the live roster at wake time, and every candidate
	// may be the actor or may have left. This says what the change
	// claimed, not what landed.
	Quiet bool `json:"quiet,omitempty"`

	// At is the EFFECTIVE instant: the fleet-agreed one rather than the
	// writer's own clock, so two nodes render one feed in one order.
	At time.Time `json:"at"`

	// LogSeq is where the change sits, which is the cursor a feed pages
	// on. It is a position on a stream, so it is never compared with one
	// from another.
	LogSeq uint64 `json:"log_seq"`
}

// Task reads one task by id or by key, with whatever else was asked for.
//
// ONE READ TRANSACTION for every part and the coverage probe, which is what
// stops a comment appearing under a task the same answer says does not have
// it yet, and stops a completeness claim being made against state the rows
// were not read from.
func (r *Reader) Task(ctx context.Context, idOrKey string, want DetailWants,
	level statelog.ReadLevel) (TaskDetail, error) {

	idOrKey = strings.TrimSpace(idOrKey)
	if idOrKey == "" {
		return TaskDetail{}, fmt.Errorf("tracker: name a task by id or by key")
	}
	var out TaskDetail
	// A POINT READ, and that is what makes a deferred scope a REFUSAL here
	// where the listing continues: this answer is about one object, and a
	// record this node cannot decode covering it means the rows it is
	// about to read may already be wrong.
	//
	// THE SCOPE IS THE TASK ITSELF and cannot be formed until the id is
	// resolved, which happens inside the transaction — so the framework
	// read is given the object's own term once, from the reference, and
	// the coverage probe inside the transaction is what catches an alias.
	served, err := r.log.Read(ctx, statelog.Query{
		Level: level,
		Scope: statelog.ScopeSet{Paths: []string{ScopeTerm{
			Kind: TermObject, ID: idOrKey,
		}.Path()}}.Normalised(),
	}, func(tx *sql.Tx) error {
		id, err := resolveTaskID(ctx, tx, idOrKey)
		if err != nil {
			return err
		}
		task, err := readTaskDocument(ctx, tx, id)
		if err != nil {
			return err
		}
		out.Task = task
		if want.Comments {
			out.Comments, out.CommentsCursor, err = readComments(ctx, tx, id,
				want.CommentCursor)
			if err != nil {
				return err
			}
		}
		if want.History {
			limit := want.HistoryLimit
			if limit <= 0 {
				limit = DetailHistoryDefault
			}
			if out.History, err = readHistory(ctx, tx, id, limit); err != nil {
				return err
			}
		}
		if want.Links {
			if out.Links, err = readLinks(ctx, tx, id); err != nil {
				return err
			}
		}
		// THE SAME EXISTS THE BOARD ROW USES, in this same transaction,
		// so the badge on the item and the badge on its row cannot
		// disagree about one task at one instant.
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM tracker_task_deps d
			 WHERE d.task_id = ? AND d.blocker_open = 1)`,
			id).Scan(&out.Blocked); err != nil {
			return fmt.Errorf("tracker: read %s's open blockers: %w", id, err)
		}

		// THE COVERAGE, IN THE SAME TRANSACTION as the rows. A detail
		// read that reported it from a second one would be answering
		// "is this complete" about a state the rows were not read from
		// — and the whole value of the number is that it describes THIS
		// answer.
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		out.LogSeq, out.AppliedThrough = position, applied

		// SCOPED TO THE TASK, not to the company. A deferred record
		// somewhere else in the tracker makes a BOARD incomplete and
		// says nothing about this task — reporting it here would put a
		// permanent warning on every task in a company holding one
		// undecodable record about one other task.
		incomplete, err := coverageOf(ctx, tx, statelog.ScopeSet{
			Paths: []string{ScopeTerm{
				Kind: TermObject, ID: id, Container: out.Task.Project,
			}.Path()},
		}.Normalised())
		if err != nil {
			return err
		}
		if incomplete != nil {
			out.Incomplete = incomplete
		} else {
			out.Complete = true
		}
		return nil
	})
	if err != nil {
		return TaskDetail{}, err
	}
	// THE LEVEL SERVED, never the level asked for. Assigning the argument
	// here — which is the only thing this function used to do with it —
	// is what made the level a label: a read that refused and one that
	// went through a quorum-committed barrier reported the same word.
	out.Level = served.Level
	if !served.Complete {
		out.Complete = false
	}
	return out, nil
}

// resolveTaskID turns an id or a key — current or former — into an id.
//
// THE FORMER KEY RESOLVES TOO, and that is not a nicety: a task moved between
// projects keeps its old key as an alias precisely so every link, chat message
// and comment that named it still opens the right record. A lookup that only
// knew the current key would break every reference the move was meant to keep.
func resolveTaskID(ctx context.Context, tx *sql.Tx, idOrKey string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM tracker_tasks WHERE id = ?
		UNION ALL
		SELECT id FROM tracker_tasks WHERE key = ?
		UNION ALL
		SELECT task_id FROM tracker_task_keys WHERE key = ?
		LIMIT 1`,
		idOrKey, strings.ToUpper(idOrKey), strings.ToUpper(idOrKey)).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: %s", ErrNoTask, idOrKey)
	case err != nil:
		return "", fmt.Errorf("tracker: resolve %q: %w", idOrKey, err)
	}
	return id, nil
}

// readTaskDocument decodes the task's own record.
//
// FROM THE DOCUMENT rather than from the columns, and that is the same rule
// the applier writes under: the columns beside it are extracted for filtering
// and sorting and are a CACHE of what the document says. A rolling upgrade
// puts a newer node's fields on the wire, and a reader that rebuilt a task
// from its columns would hand a caller a task with the new field stripped.
func readTaskDocument(ctx context.Context, tx *sql.Tx, id string) (Task, error) {
	var body []byte
	err := tx.QueryRowContext(ctx,
		`SELECT document FROM tracker_tasks WHERE id = ?`, id).Scan(&body)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Task{}, fmt.Errorf("%w: %s", ErrNoTask, id)
	case err != nil:
		return Task{}, fmt.Errorf("tracker: read task %s: %w", id, err)
	}
	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		return Task{}, fmt.Errorf("tracker: decode task %s: %w", id, err)
	}
	return task, nil
}

// DetailComments is how many comments one detail read returns.
//
// TWENTY, which is the design's own figure and is what a thread's recent shape
// takes: past that a reader is scrolling rather than catching up, and the
// cursor is how they do it. A task's thread is the ONE collection on a detail
// read with no bound of its own — a body is capped, a history feed is capped,
// a relation set is capped — so it is the shape that decides whether this
// answer fits [ToolAnswerBytes] at all.
const DetailComments = 20

// CommentBodyShown is how much of each body a detail read carries.
//
// 2 KiB, against [MaxCommentBody]'s 32 KiB — which is the whole point: twenty
// comments at their full length is 640 KiB, ten times the ceiling on ONE tool
// answer, for a thread nobody asked to read in full. The excerpt is what a
// reader skims; `get_work_item(comment:)` is how they open one.
const CommentBodyShown = 2 << 10

// readComments is a page of the thread, NEWEST FIRST.
//
// NEWEST FIRST, unlike the whole thread a person scrolls, because a PAGE has
// to start somewhere and the end is what a reader catching up needs: the
// oldest twenty of a two-hundred-comment thread is the conversation's
// beginning, which is the part they have already read. The page is reversed
// before it is returned, so what a caller holds is still in the order it was
// written — a reply resolves against what came before it.
//
// A REMOVED COMMENT KEEPS ITS ROW with a blank body, which is what lets a
// reply still resolve against something — so it is returned rather than
// filtered, and its `removed` flag is what a renderer reads.
func readComments(ctx context.Context, tx *sql.Tx, taskID, cursor string) (
	[]Comment, string, error) {

	// ONE MORE THAN THE PAGE, which is how the cursor knows whether there
	// IS a next page without a second count over a table that grows for
	// the life of the task.
	rows, err := tx.QueryContext(ctx, `
		SELECT document, created_at, id FROM tracker_comments
		WHERE task_id = ? AND (? = '' OR (created_at, id) < (?, ?))
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, taskID, cursor, cursorAt(cursor), cursorID(cursor),
		DetailComments+1)
	if err != nil {
		return nil, "", fmt.Errorf("tracker: read the thread on %s: %w", taskID, err)
	}
	defer func() { _ = rows.Close() }()
	var (
		out  []Comment
		next string
	)
	for rows.Next() {
		var body []byte
		var at int64
		var id string
		if err := rows.Scan(&body, &at, &id); err != nil {
			return nil, "", err
		}
		if len(out) == DetailComments {
			// THE EXTRA ROW IS THE CURSOR, not a result: it is the
			// first comment of the NEXT page, and naming it is
			// cheaper than counting what is left.
			next = formatCommentCursor(at, id)
			break
		}
		var comment Comment
		if err := json.Unmarshal(body, &comment); err != nil {
			return nil, "", fmt.Errorf("tracker: decode a comment on %s: %w",
				taskID, err)
		}
		// THE BODY IS ELIDED HERE and not at the caller, because the
		// caller that forgot would send the whole thread — and the
		// elision is what makes twenty of them fit an answer at all.
		comment.Body = elideCommentBody(comment.Body)
		out = append(out, comment)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	slices.Reverse(out)
	return out, next, nil
}

// elideCommentBody is what a thread carries of one comment.
//
// MARKED, and that is the half a plain cut leaves out: a body cut at exactly
// the cap and handed over unmarked reads as a comment that ENDED there, which
// is a different message from the one somebody wrote. [textcut.Ellipsis] is
// the tree's one rune-safe cut, so a multi-byte character on the boundary does
// not reach a model as a replacement character.
func elideCommentBody(body string) string {
	return textcut.Ellipsis(body, CommentBodyShown)
}

// The comment cursor is the pair the page is ordered by, because an instant
// alone is not unique: two comments share a created_at routinely, and a cursor
// that compared one would skip whichever the page boundary fell between.
func formatCommentCursor(at int64, id string) string {
	return strconv.FormatInt(at, 10) + ":" + id
}

func cursorAt(cursor string) int64 {
	at, _, _ := strings.Cut(cursor, ":")
	n, _ := strconv.ParseInt(at, 10, 64)
	return n
}

func cursorID(cursor string) string {
	_, id, _ := strings.Cut(cursor, ":")
	return id
}

// readHistory is the activity feed, NEWEST FIRST and capped.
func readHistory(ctx context.Context, tx *sql.Tx, taskID string, limit int) ([]HistoryEntry, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, kind, actor, actor_kind, operator_id, comment_id, excerpt,
		       fields_json, turn_id, notified, effective_at, log_seq
		FROM tracker_history
		WHERE subject_id = ?
		ORDER BY log_seq DESC
		LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the history of %s: %w", taskID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []HistoryEntry
	for rows.Next() {
		var e HistoryEntry
		var actorKind, fields string
		var notified int
		var effective, seq int64
		if err := rows.Scan(&e.ID, &e.Kind, &e.Actor, &actorKind, &e.OperatorID,
			&e.CommentID, &e.Excerpt, &fields, &e.TurnID, &notified,
			&effective, &seq); err != nil {
			return nil, err
		}
		e.ActorKind = AuthorKind(actorKind)
		e.Quiet = notified == 0
		e.At = store.DecodeTime(effective)
		e.LogSeq = uint64(seq)
		if fields != "" && fields != "{}" {
			// A FIELD SET THAT DOES NOT DECODE IS DROPPED, not raised:
			// it is the render half of one change, and refusing the
			// whole feed because one card cannot show its diff would
			// take away the rest of the answer as well.
			_ = json.Unmarshal([]byte(fields), &e.Fields)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// readLinks is both directions of every relation.
//
// TWO QUERIES RATHER THAN A UNION, because the two halves mean different
// things: the rows keyed on this task are the ones it owns, and the rows
// pointing AT it are the mirror — which is what `derived` marks. Merging them
// in SQL would make a renderer re-derive which end authored the edge from a
// column that no longer says.
func readLinks(ctx context.Context, tx *sql.Tx, taskID string) ([]DetailLink, error) {
	const columns = `
		SELECT r.kind, %s, r.derived, r.one_sided, r.one_sided_final, r.note,
		       COALESCE(t.key, ''), COALESCE(t.title, ''), COALESCE(t.status, '')
		FROM tracker_relations r
		LEFT JOIN tracker_tasks t ON t.id = %s
		WHERE %s = ?
		ORDER BY r.kind, %s`
	var out []DetailLink
	for _, q := range []struct {
		statement string
		derived   bool
	}{
		{fmt.Sprintf(columns, "r.other_id", "r.other_id", "r.task_id", "r.other_id"), false},
		{fmt.Sprintf(columns, "r.task_id", "r.task_id", "r.other_id", "r.task_id"), true},
	} {
		rows, err := tx.QueryContext(ctx, q.statement, taskID)
		if err != nil {
			return nil, fmt.Errorf("tracker: read the links on %s: %w", taskID, err)
		}
		for rows.Next() {
			var link DetailLink
			var kind, status string
			var derived, oneSided, oneSidedFinal int
			if err := rows.Scan(&kind, &link.Other, &derived, &oneSided,
				&oneSidedFinal, &link.Note, &link.Key, &link.Title, &status); err != nil {
				_ = rows.Close()
				return nil, err
			}
			link.Kind = RelationKind(kind)
			link.Status = Status(status)
			// THE MIRROR IS DERIVED WHATEVER ITS COLUMN SAYS. The
			// column records how the edge was written; this side of
			// the read is by definition the end that did not author
			// it, and a renderer asking "may I edit this" needs the
			// second fact rather than the first.
			link.Derived = derived == 1 || q.derived
			link.OneSided = oneSided == 1
			link.OneSidedFinal = oneSidedFinal == 1
			out = append(out, link)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}
