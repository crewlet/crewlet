package pages

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// TurnWrite is one change a run committed to a page — what a turn's story says
// it left behind in the knowledge base.
//
// The pages' history names the run that made each change (`turn_id`), for the
// reason the tracker's does (see `tracker.TurnWrite`): it is the one durable,
// replicated account of what a run CHANGED, where its own events say only what
// it was given, called and spent.
type TurnWrite struct {
	// PageID and Title name the page as it reads now: a title is the page's
	// address and a rename moves it, so a link names the page as it is.
	PageID string `json:"id"`
	Title  string `json:"title"`
	// Kind is what the change was — created, saved, commented — in the
	// pages' own change vocabulary.
	Kind ChangeKind `json:"kind"`
	// At is when the change was committed.
	At time.Time `json:"at"`
}

// MaxTurnWriteRuns is how many runs one [Reader.TurnWrites] names, and
// MaxTurnWrites how many changes one run is answered with — the tracker's
// bounds (`tracker.MaxTurnWriteRuns`, `tracker.MaxTurnWrites`) for the same
// reasons, since a story reads both for one page of runs.
const (
	MaxTurnWriteRuns = 200
	MaxTurnWrites    = 32
)

// TurnWrites answers, for each run named, the changes it committed to pages in
// the order they landed — a run that changed no page is absent. A page in the
// trash, or purged, is left out: a story must not link "edited the runbook" to
// a page a reader cannot open.
//
// QUIET CHANGES INCLUDED. A quiet save is one that announced nothing — a typo
// fixed without waking the page's watchers — and it is still a change the run
// made; what a story lists is what the session did, not what it announced.
func (r *Reader) TurnWrites(ctx context.Context, runs []string,
	fresh statelog.Freshness) (map[string][]TurnWrite, error) {

	if fresh.Level == "" {
		return nil, fmt.Errorf("pages: this turn read names no level — a " +
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
		return nil, invalid("runs", "name at most %d runs, not %d", MaxTurnWriteRuns, len(ids))
	}
	out := map[string][]TurnWrite{}
	if len(ids) == 0 {
		return out, nil
	}
	_, err := r.log.Read(ctx, fresh.Query(ReadScope(""), true), func(tx *sql.Tx) error {
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
	args = append(args, string(StatusTrashed))
	for _, run := range runs {
		args = append(args, run)
	}
	// THROUGH `pages_history_turn_idx` (replicated 0041), oldest change first
	// per run, so the bound keeps what the run did first.
	rows, err := tx.QueryContext(ctx, `
		SELECT h.turn_id, h.page_id, p.title, h.kind, h.created_at
		  FROM pages_history h
		  JOIN pages_heads p ON p.id = h.page_id AND p.status <> ?
		 WHERE h.turn_id <> '' AND h.turn_id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(runs)), ",")+`)
		 ORDER BY h.turn_id, h.version`, args...)
	if err != nil {
		return fmt.Errorf("pages: read what %d runs wrote: %w", len(runs), err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			run, kind string
			at        int64
			write     TurnWrite
		)
		if err := rows.Scan(&run, &write.PageID, &write.Title, &kind, &at); err != nil {
			return fmt.Errorf("pages: scan what a run wrote: %w", err)
		}
		if len(out[run]) >= MaxTurnWrites {
			continue
		}
		write.Kind = ChangeKind(kind)
		write.At = store.DecodeTime(at)
		out[run] = append(out[run], write)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pages: read what %d runs wrote: %w", len(runs), err)
	}
	return nil
}
