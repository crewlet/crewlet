package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// TurnWrite is one change a run committed to a task — what a turn's story says
// it left behind on the tracker.
//
// # Why the tracker answers it rather than the turn's own events
//
// A turn's events say what it was given, what it called and what it spent; none
// of them says what it CHANGED. The engine keeps a set of the items a turn
// wrote, in memory, for exactly one question — whether it wrote one item alone
// (ADR-0022) — and publishes nothing of it. What every change records is the
// run that made it: the history row a commit writes carries `turn_id`, so the
// tracker's own account of the company is also the account of what each run
// did to it, written once, on every node, and read here by the run.
type TurnWrite struct {
	// TaskID, Key and Title name the task the change landed on, as it reads
	// now: a key moves with the task, and a link names it as it is.
	TaskID string `json:"id"`
	Key    string `json:"key"`
	Title  string `json:"title"`
	// Kind is what the change was — a comment, a status move, a checklist
	// tick — in the change feed's own vocabulary, so a reader phrases it the
	// way the task's own history does.
	Kind ChangeKind `json:"kind"`
	// Status is the status a change moved the task to, and absent on one
	// that moved none: "moved it to In progress" is the one delta a story
	// names, because it is the one a person acts on.
	Status string `json:"status,omitempty"`
	// At is the fleet-agreed instant the change landed.
	At time.Time `json:"at"`
}

// MaxTurnWriteRuns is how many runs one [Reader.TurnWrites] names — a page of
// turn stories with room, at the bound [MaxTurnPlaces] holds the same question
// to.
const MaxTurnWriteRuns = MaxTurnPlaces

// MaxTurnWrites is how many changes one run is answered with. A turn is a
// person's colleague doing a piece of work, and the changes that work leaves are
// a handful; past this it is a bulk operation — a relabel, a sweep — whose list
// nobody reads change by change, and the bound keeps one such run from growing
// a page of stories without limit. The FIRST changes are kept, in the order they
// landed, because the first is what the run set out to do.
const MaxTurnWrites = 32

// TurnWrites answers, for each run named, the changes it committed to tasks in
// the order they landed — a run that changed no task is absent. A task REMOVED
// since is left out, for the reason [Reader.TurnPlaces] leaves out a run whose
// task was: a story must not link "commented on ENG-412" to a task that is gone.
//
// ONLY TASKS. A project, a view, a person record and the catalogue are changes
// to how the company is organised rather than to its work, and none of them is
// something a person follows a link to from a story about the work.
func (r *Reader) TurnWrites(ctx context.Context, runs []string,
	fresh statelog.Freshness) (map[string][]TurnWrite, error) {

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
	if len(ids) > MaxTurnWriteRuns {
		return nil, invalid("name at most %d runs, not %d", MaxTurnWriteRuns, len(ids))
	}
	out := map[string][]TurnWrite{}
	if len(ids) == 0 {
		return out, nil
	}
	_, err := r.log.Read(ctx, fresh.Query(
		statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(), true), func(tx *sql.Tx) error {
		return readTurnWrites(ctx, tx, ids, out)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readTurnWrites is [Reader.TurnWrites] inside the caller's transaction.
func readTurnWrites(ctx context.Context, tx *sql.Tx, runs []string,
	out map[string][]TurnWrite) error {

	args := make([]any, 0, len(runs)+1)
	args = append(args, string(KindTask))
	for _, run := range runs {
		args = append(args, run)
	}
	// THROUGH `tracker_history_turn_idx` (replicated 0041), oldest change
	// first per run, so the bound keeps what the run did first.
	rows, err := tx.QueryContext(ctx, `
		SELECT h.turn_id, h.subject_id, k.key, k.title, h.kind, h.fields_json,
		       h.effective_at
		  FROM tracker_history h
		  JOIN tracker_tasks k ON k.id = h.subject_id AND k.removed_at IS NULL
		 WHERE h.subject_kind = ? AND h.turn_id <> '' AND h.turn_id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(runs)), ",")+`)
		 ORDER BY h.turn_id, h.log_seq`, args...)
	if err != nil {
		return fmt.Errorf("tracker: read what %d runs wrote: %w", len(runs), err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			run, kind, fields string
			effective         int64
			write             TurnWrite
		)
		if err := rows.Scan(&run, &write.TaskID, &write.Key, &write.Title, &kind,
			&fields, &effective); err != nil {
			return fmt.Errorf("tracker: scan what a run wrote: %w", err)
		}
		if len(out[run]) >= MaxTurnWrites {
			continue
		}
		write.Kind = ChangeKind(kind)
		write.At = store.DecodeTime(effective)
		// THE STATUS IT MOVED TO, off the change's own deltas. A row whose
		// deltas do not decode still says what kind of change it was, so it
		// is kept without one rather than costing the run its account.
		var deltas map[string]Delta
		if fields != "" && json.Unmarshal([]byte(fields), &deltas) == nil {
			write.Status = deltas["status"].To
		}
		out[run] = append(out[run], write)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("tracker: read what %d runs wrote: %w", len(runs), err)
	}
	return nil
}
