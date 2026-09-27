package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A task's own account of the agent work done on it: one entry per TURN, newest
// first, each numbered "Turn n" by the task.
//
// # A turn, not a segment
//
// A turn that parks on a coding run completes more than once under one run id,
// and each completion charges a row of its own (see the engine's turnspend.go)
// — so the rows are SEGMENTS and a list of them would show one turn as three
// cards, the second and third headed by an outcome of "suspended" that is not
// how the turn ended. The list groups them by run id: the tokens, rounds, wall
// time and tools are summed, the phases are the union in first-run order, and
// the outcome, summary and review are the NEWEST segment's that states one.
//
// # "Turn n" is the task's count, not the list's
//
// The ordinal is the 1-based position of the turn's COUNTED row among every
// counted row on the task — a row whose spend carries `turns`, which exactly
// one segment of a charged turn does (the dispatch, or the segment that paid
// for a dispatch charged to nothing). So the newest turn's number IS the
// task's `spend_turns`, and "Turn 3" on a card and "3 turns" in the cost
// panel beside it are one fact counted once. A turn whose every segment on
// this task carries no count — more of a turn charged to another item — has
// no ordinal, which the answer states as zero rather than inventing one.
//
// # Paged by the turn's first segment
//
// A cursor is the log position of the oldest turn a page returned, and the
// next page is the turns whose FIRST segment is below it. The first segment
// and not the newest, because a turn's later segments arrive as its coding
// runs are collected, and a cursor on the newest would move a turn from one
// page to another while a reader walked them.

// DefaultTaskTurns is the page a task's turn list answers when none is named;
// MaxTaskTurns the most one page holds — the ceiling every other paged tracker
// read holds, which keeps a page of fifty turns with a summary and a review
// each near 60 KiB.
const (
	DefaultTaskTurns = 20
	MaxTaskTurns     = 50
)

// TaskTurn is one turn on a task, its segments folded.
type TaskTurn struct {
	// TurnID is the run the segments share (ADR-0017) — what a trace link
	// and every event of the turn carry.
	TurnID string `json:"turn_id"`
	// Ordinal is the task's own count: "Turn n". Zero for a turn with no
	// counted segment on this task.
	Ordinal int `json:"ordinal"`
	// Seat is the handle whose turn it was.
	Seat string `json:"seat"`
	// Trigger is what woke it.
	Trigger string `json:"trigger,omitempty"`
	// Segments is how many completions were charged here: one for a turn
	// that never parked.
	Segments int `json:"segments"`

	// Tokens is input plus output, over every segment — the figure the
	// task's own `spend_tokens` sums.
	Tokens    int `json:"tokens"`
	CacheRead int `json:"cache_read"`
	Rounds    int `json:"rounds"`
	WallMs    int `json:"wall_ms"`
	Workers   int `json:"workers,omitempty"`
	SentBack  int `json:"sent_back,omitempty"`

	// Outcome is how the turn ended, off its newest segment: the turn's
	// decision, `failed`, or `suspended` while a coding run is still out.
	Outcome string `json:"outcome"`
	// Phases are the phases that ran, in the order each first ran.
	Phases []string `json:"phases"`

	// Summary, Review and Tools are what it did — see [TurnRecord].
	// Absent on a turn recorded by a build before the record carried them.
	Summary string     `json:"summary,omitempty"`
	Review  string     `json:"review,omitempty"`
	Tools   []TurnTool `json:"tools,omitempty"`
	// FailedIn is the phase that failed, off the newest segment with the
	// outcome — see [TurnRecord.FailedIn]. It follows [TaskTurn.Outcome],
	// so a turn whose resumed segment succeeded names no failure.
	FailedIn string `json:"failed_in,omitempty"`

	// At is the fleet-agreed instant of the newest segment: when the turn,
	// as far as this task knows, ended.
	At time.Time `json:"at"`
}

// TaskTurns is one page of a task's turns, with the coverage it was read at.
type TaskTurns struct {
	// Task and Key name the task the page is about, whichever spelling the
	// caller used.
	Task string `json:"item"`
	Key  string `json:"key"`

	Turns []TaskTurn `json:"turns"`
	// Next is the cursor the next page resumes from; empty at the end.
	Next string `json:"next_cursor,omitempty"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// TurnsOf reads one page of a task's turns, newest first. The task is named by
// id or key; the cursor is a previous page's [TaskTurns.Next], or empty for the
// newest page; limit is held to [MaxTaskTurns].
func (r *Reader) TurnsOf(ctx context.Context, idOrKey, cursor string, limit int,
	fresh statelog.Freshness) (TaskTurns, error) {

	idOrKey = strings.TrimSpace(idOrKey)
	if idOrKey == "" {
		return TaskTurns{}, invalid("tracker: name a task by id or by key")
	}
	below := int64(math.MaxInt64)
	if cursor = strings.TrimSpace(cursor); cursor != "" {
		at, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || at <= 0 {
			return TaskTurns{}, invalid("tracker: %q is not a turn cursor — pass "+
				"a page's own next_cursor back unchanged", cursor)
		}
		below = at
	}
	switch {
	case limit <= 0:
		limit = DefaultTaskTurns
	case limit > MaxTaskTurns:
		limit = MaxTaskTurns
	}

	var out TaskTurns
	// A POINT READ ON THE TASK, for [Reader.Task]'s reason: the page is about
	// one object, and a record this node cannot decode covering it means the
	// rows are not the whole story.
	served, err := r.log.Read(ctx, fresh.Query(statelog.ScopeSet{Paths: []string{ScopeTerm{
		Kind: TermObject, ID: idOrKey,
	}.Path()}}.Normalised(), false), func(tx *sql.Tx) error {
		id, err := resolveTaskID(ctx, tx, idOrKey)
		if err != nil {
			return err
		}
		var project string
		if err = tx.QueryRowContext(ctx,
			`SELECT key, project_key FROM tracker_tasks WHERE id = ?`, id).
			Scan(&out.Key, &project); err != nil {
			return fmt.Errorf("tracker: read task %s for its turns: %w", id, err)
		}
		out.Task = id
		turns, next, err := readTaskTurns(ctx, tx, id, below, limit)
		if err != nil {
			return err
		}
		out.Turns, out.Next = turns, next

		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		out.LogSeq, out.AppliedThrough = position, applied
		incomplete, err := coverageOf(ctx, tx, statelog.ScopeSet{
			Paths: []string{ScopeTerm{Kind: TermObject, ID: id, Container: project}.Path()},
		}.Normalised())
		if err != nil {
			return err
		}
		out.Incomplete, out.Complete = incomplete, incomplete == nil
		return nil
	})
	if err != nil {
		return TaskTurns{}, err
	}
	out.Level = served.Level
	if !served.Complete {
		out.Complete = false
	}
	return out, nil
}

// turnKey is the SQL a turn row is grouped by: its run id, or — for a row with
// none, which no current writer produces — the row's own id, so two such rows
// are never folded into one turn nobody ran.
const turnKey = `CASE WHEN turn_id = '' THEN id ELSE turn_id END`

// readTaskTurns is the page inside the caller's transaction.
func readTaskTurns(ctx context.Context, tx *sql.Tx, task string, below int64,
	limit int) ([]TaskTurn, string, error) {

	// THE PAGE'S TURNS, by the position of each one's first segment. One
	// more than the page, so the cursor is only handed out when a page
	// past this one exists.
	rows, err := tx.QueryContext(ctx, `
		SELECT `+turnKey+`, MIN(log_seq)
		  FROM tracker_turns
		 WHERE task_id = ?
		 GROUP BY 1
		HAVING MIN(log_seq) < ?
		 ORDER BY 2 DESC
		 LIMIT ?`, task, below, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("tracker: list the turns of task %s: %w", task, err)
	}
	var keys []string
	anchors := map[string]int64{}
	for rows.Next() {
		var key string
		var anchor int64
		if scanErr := rows.Scan(&key, &anchor); scanErr != nil {
			_ = rows.Close()
			return nil, "", fmt.Errorf("tracker: scan a turn of task %s: %w", task, scanErr)
		}
		keys = append(keys, key)
		anchors[key] = anchor
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, "", fmt.Errorf("tracker: list the turns of task %s: %w", task, err)
	}
	if err = rows.Close(); err != nil {
		return nil, "", fmt.Errorf("tracker: list the turns of task %s: %w", task, err)
	}
	next := ""
	if len(keys) > limit {
		keys = keys[:limit]
		next = strconv.FormatInt(anchors[keys[limit-1]], 10)
	}
	if len(keys) == 0 {
		return []TaskTurn{}, "", nil
	}

	ordinals, err := turnOrdinals(ctx, tx, task)
	if err != nil {
		return nil, "", err
	}

	// EVERY SEGMENT OF THOSE TURNS, oldest first, folded in that order so
	// "the newest segment that states one" is simply the last write.
	args := []any{task}
	for _, key := range keys {
		args = append(args, key)
	}
	segs, err := tx.QueryContext(ctx, `
		SELECT `+turnKey+`, seat, turn_id, trigger, input_tokens, output_tokens,
		       cache_read, phases_json, rounds, wall_ms, outcome, effective_at,
		       document
		  FROM tracker_turns
		 WHERE task_id = ? AND `+turnKey+` IN (`+placeholders(len(keys))+`)
		 ORDER BY log_seq`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("tracker: read the segments of task %s's turns: %w", task, err)
	}
	defer func() { _ = segs.Close() }()
	folded := map[string]*TaskTurn{}
	for segs.Next() {
		var (
			key, seat, turnID, trigger, phasesJSON, outcome string
			in, outTok, cacheRead, rounds, wallMs           int
			effective                                       int64
			document                                        []byte
		)
		if err := segs.Scan(&key, &seat, &turnID, &trigger, &in, &outTok, &cacheRead,
			&phasesJSON, &rounds, &wallMs, &outcome, &effective, &document); err != nil {
			return nil, "", fmt.Errorf("tracker: scan a segment of task %s: %w", task, err)
		}
		var record TurnRecord
		if err := json.Unmarshal(document, &record); err != nil {
			return nil, "", fmt.Errorf("tracker: decode a turn segment of task %s: %w", task, err)
		}
		var phases []string
		if err := json.Unmarshal([]byte(phasesJSON), &phases); err != nil {
			return nil, "", fmt.Errorf("tracker: decode the phases of a turn of task %s: %w",
				task, err)
		}
		turn := folded[key]
		if turn == nil {
			turn = &TaskTurn{
				TurnID: turnID, Ordinal: ordinals[key], Seat: seat, Trigger: trigger,
				Phases: []string{},
			}
			folded[key] = turn
		}
		turn.Segments++
		turn.Tokens += in + outTok
		turn.CacheRead += cacheRead
		turn.Rounds += rounds
		turn.WallMs += wallMs
		turn.Workers += record.Spend.Workers
		turn.SentBack += record.Spend.SentBack
		for _, name := range phases {
			if !slices.Contains(turn.Phases, name) {
				turn.Phases = append(turn.Phases, name)
			}
		}
		if outcome != "" {
			turn.Outcome = outcome
			turn.FailedIn = record.FailedIn
		}
		if record.Summary != "" {
			turn.Summary = record.Summary
		}
		if record.Review != "" {
			turn.Review = record.Review
		}
		turn.Tools = mergeTurnTools(turn.Tools, record.Tools)
		turn.At = store.DecodeTime(effective)
	}
	if err := segs.Err(); err != nil {
		return nil, "", fmt.Errorf("tracker: read the segments of task %s's turns: %w", task, err)
	}
	out := make([]TaskTurn, 0, len(keys))
	for _, key := range keys {
		if turn := folded[key]; turn != nil {
			out = append(out, *turn)
		}
	}
	return out, next, nil
}

// turnOrdinals numbers every counted row on a task in log order and answers,
// per turn, the number of its first counted row. One pass over the task's own
// rows through the task's index; the count is read off the record, which is
// where [TurnSpend.Turns] lives.
func turnOrdinals(ctx context.Context, tx *sql.Tx, task string) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT `+turnKey+`
		  FROM tracker_turns
		 WHERE task_id = ? AND COALESCE(json_extract(document, '$.spend.turns'), 0) > 0
		 ORDER BY log_seq`, task)
	if err != nil {
		return nil, fmt.Errorf("tracker: number the turns of task %s: %w", task, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	n := 0
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("tracker: number the turns of task %s: %w", task, err)
		}
		n++
		if _, seen := out[key]; !seen {
			out[key] = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: number the turns of task %s: %w", task, err)
	}
	return out, nil
}

// mergeTurnTools adds one segment's tool counts into a turn's, keeping first-
// call order and the [MaxTurnTools] bound.
func mergeTurnTools(into, add []TurnTool) []TurnTool {
	for _, tool := range add {
		i := slices.IndexFunc(into, func(t TurnTool) bool { return t.Name == tool.Name })
		switch {
		case i >= 0:
			into[i].Calls += tool.Calls
		case len(into) < MaxTurnTools:
			into = append(into, tool)
		}
	}
	return into
}
