package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// TurnPlace is where one run sits in the tracker: the task it was charged to
// and its "Turn n" there — what a page's reader line says as "turn 3 on
// ENG-412", and a turn's story as "session 3".
type TurnPlace struct {
	TaskID string `json:"id"`
	Key    string `json:"key"`
	Title  string `json:"title"`

	// Ordinal is the task's own count of turns (`turnOrdinals`): zero for a
	// run with no counted row on the task.
	Ordinal int `json:"ordinal"`
}

// MaxTurnPlaces is how many runs one [Reader.TurnPlaces] names — the most
// readers one page's answer lists, with room.
const MaxTurnPlaces = 200

// TurnPlaces names the task each run was charged to and its ordinal there,
// for the runs given; a run the tracker holds no row for is absent from the
// answer, and so is a run whose only tasks were REMOVED — a reader line would
// otherwise link "turn 2 on ENG-412" to a task that is gone, the same reason
// the backlinks drop a removed task (`search.linkedTasks`).
//
// # One task per run, the charged one
//
// A turn is charged to ONE work item (ADR-0022), and its counted row — the one
// whose spend carries `turns` — is on that item. A run with rows on more than
// one task (more of a turn charged elsewhere) is placed on the task holding its
// counted row; a run with no counted row anywhere is placed on the task of its
// newest row, with ordinal zero rather than an invented one.
//
// # The ordinal is the task's count
//
// Numbered by `turnOrdinals`, the one numbering of a task's turns, so "turn 3"
// beside a reader, "session 3" on a story and the task's `spend.turns` are one
// count.
func (r *Reader) TurnPlaces(ctx context.Context, runs []string,
	fresh statelog.Freshness) (map[string]TurnPlace, error) {

	if fresh.Level == "" {
		return nil, fmt.Errorf("tracker: this turn read names no level — a " +
			"surface resolves an absent read_level to its own default before " +
			"it reads")
	}
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		if run = strings.TrimSpace(run); run != "" && !slices.Contains(ids, run) {
			ids = append(ids, run)
		}
	}
	if len(ids) > MaxTurnPlaces {
		return nil, invalid("name at most %d runs, not %d", MaxTurnPlaces, len(ids))
	}
	out := map[string]TurnPlace{}
	if len(ids) == 0 {
		return out, nil
	}
	_, err := r.log.Read(ctx, fresh.Query(
		statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(), true), func(tx *sql.Tx) error {
		return readTurnPlaces(ctx, tx, ids, out)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func readTurnPlaces(ctx context.Context, tx *sql.Tx, runs []string,
	out map[string]TurnPlace) error {

	args := make([]any, len(runs))
	for i, run := range runs {
		args[i] = run
	}
	// THROUGH `tracker_turns_turn_idx` (replicated 0030), newest row first per
	// run so the fallback below is the first row it sees.
	rows, err := tx.QueryContext(ctx, `
		SELECT t.turn_id, t.task_id, k.key, k.title,
		       COALESCE(json_extract(t.document, '$.spend.turns'), 0) > 0
		  FROM tracker_turns t
		  JOIN tracker_tasks k ON k.id = t.task_id AND k.removed_at IS NULL
		 WHERE t.turn_id <> '' AND t.turn_id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(runs)), ",")+`)
		 ORDER BY t.turn_id, t.log_seq DESC`, args...)
	if err != nil {
		return fmt.Errorf("tracker: read the tasks of %d runs: %w", len(runs), err)
	}
	counted := map[string]bool{}
	for rows.Next() {
		var run string
		var place TurnPlace
		var isCounted bool
		if err = rows.Scan(&run, &place.TaskID, &place.Key, &place.Title, &isCounted); err != nil {
			_ = rows.Close()
			return fmt.Errorf("tracker: scan a run's task: %w", err)
		}
		_, placed := out[run]
		switch {
		case isCounted && !counted[run]:
			out[run], counted[run] = place, true
		case !placed:
			out[run] = place
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("tracker: read the tasks of %d runs: %w", len(runs), err)
	}
	_ = rows.Close()

	// THE ORDINALS, one numbering per task however many of its runs were
	// asked about.
	ordinals := map[string]map[string]int{}
	for run, place := range out {
		if !counted[run] {
			continue
		}
		numbered, ok := ordinals[place.TaskID]
		if !ok {
			if numbered, err = turnOrdinals(ctx, tx, place.TaskID); err != nil {
				return err
			}
			ordinals[place.TaskID] = numbered
		}
		place.Ordinal = numbered[run]
		out[run] = place
	}
	return nil
}

// turnOrdinals numbers every counted row on a task in log order and answers,
// per turn, the number of its first counted row. One pass over the task's own
// rows through the task's index; the count is read off the record, which is
// where [TurnSpend.Turns] lives.
//
// A TURN'S COUNT, NOT A ROW'S. A turn that parks on a coding run completes more
// than once under one run id, and each completion charges a row of its own (the
// engine's turnspend.go) — so the rows are SEGMENTS, and numbered by row one
// turn would be three. A row whose spend carries `turns` is the one segment of
// a charged turn that counts it (the dispatch, or the segment that paid for a
// dispatch charged to nothing), so the newest turn's number IS the task's
// `spend.turns`: "turn 3" beside a reader and "3 turns" in the task's cost are
// one fact counted once. A turn whose every segment here carries no count —
// more of a turn charged to another item — has no number, which is answered as
// zero rather than invented.
func turnOrdinals(ctx context.Context, tx *sql.Tx, task string) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT turn_id
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
