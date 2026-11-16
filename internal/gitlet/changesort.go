package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// A LIVE STATE BY WHEN ITS CHANGES LAST MOVED walks its lifecycle's index and the conversations'
// in step, as a window does (changewindow.go), floored at the lifecycle side's cut and stepping
// over ended changes' conversations within MaxSpokenScanned a page. With no window, a page that
// reads the lifecycle side to its end reads no conversation.

// ChangeSort is the order a `changes` listing is asked in; a closed set ([ChangeSorts]).
type ChangeSort string

const (
	// ChangeSortOpened is newest opened first: a repository's by number, a reviewer's by id.
	ChangeSortOpened ChangeSort = "opened"
	// ChangeSortUpdated is newest last move first, by [ChangeSummary.UpdatedAt].
	ChangeSortUpdated ChangeSort = "updated"
)

// ChangeSorts is the closed set, in this order.
func ChangeSorts() []ChangeSort { return []ChangeSort{ChangeSortOpened, ChangeSortUpdated} }

// Valid reports a sort this build knows.
func (s ChangeSort) Valid() bool { return slices.Contains(ChangeSorts(), s) }

// MaxSpokenScanned is the most conversations one page of a live state by last move reads: their
// index holds every change ever discussed, ended ones included, which the page steps over; past
// it the page answers what it found with a cursor.
const MaxSpokenScanned = 1_000

// The live changes by state and last move, every repository's or one's, and one repository's
// conversations by when somebody last spoke (replicated "A live change is listed by when it last
// moved").
const (
	changesLiveMovedKey       = "code_changes_live_updated_idx"
	changesRepoLiveMovedKey   = "code_changes_repo_live_updated_idx"
	conversationsRepoMovedKey = "code_conversations_repo_updated_idx"
)

// spokenRow is a conversation the live walk read: its change, when somebody last spoke in it,
// and the change's state and lifecycle instant, which decide whether the walk keeps it.
type spokenRow struct {
	id                    string
	spokenAt, lifecycleAt int64
	state                 ChangeState
}

// liveMovedWalk is one page of a live state's walk by last move: at most limit changes in the
// walk's state, newest [changeRow.moved] first and then id, each once, after cursor where it is
// set, and the cursor of the page after it, "" where both sides are read to their end.
func (w changeWalk) liveMovedWalk(ctx context.Context, tx *sql.Tx, cursor string, limit int) ([]changeRow,
	string, error) {

	repo := len(w.keys) == 2
	byChange, byConversation := liveMovedSQL, spokenOfSQL
	if repo {
		byChange, byConversation = repoLiveMovedSQL, repoSpokenOfSQL
	}
	var after []any
	var resumed *changeRow
	if cursor != "" {
		at, id, ok := cutLogCursor(cursor)
		if !ok {
			return nil, "", fmt.Errorf("%w: %q", ErrBadCursor, cursor)
		}
		after, resumed = []any{at, at, id}, &changeRow{id: id, moved: at}
		byChange, byConversation = liveMovedAfterSQL, spokenOfAfterSQL
		if repo {
			byChange, byConversation = repoLiveMovedAfterSQL, repoSpokenOfAfterSQL
		}
	}
	lifecycle, err := readChangesBy(ctx, tx, byChange, slices.Concat(w.keys, []any{w.since}, after, []any{limit}),
		"their last move")
	if err != nil {
		return nil, "", err
	}
	if err = withConversations(ctx, tx, lifecycle); err != nil {
		return nil, "", err
	}
	// THE LIFECYCLE SIDE READ TO ITS END, with no window, holds every live change the walk has not
	// listed yet, each dated by its conversation: one whose lifecycle is above the cursor moved
	// above it too, so the conversations need no read and the last page steps over none.
	if w.since == 0 && len(lifecycle) < limit {
		var rest []changeRow
		for _, row := range lifecycle {
			if resumed == nil || movedBefore(row, *resumed) {
				rest = append(rest, row)
			}
		}
		newestMoveFirst(rest)
		return rest, "", nil
	}
	// BELOW WHERE A SIDE THAT FILLED ITS PAGE STOPPED, that side holds rows this page did not read,
	// so the page ends at the later of the two; no conversation below the lifecycle side's cut can
	// be on it, so that side is not read below it.
	var bound *changeRow
	floor := w.since
	if len(lifecycle) == limit {
		last := lifecycle[len(lifecycle)-1]
		last.moved = last.updatedAt
		bound, floor = &last, max(floor, last.updatedAt)
	}
	spoken, err := readSpoken(ctx, tx, byConversation,
		slices.Concat(w.keys[:len(w.keys)-1], []any{floor}, after, []any{w.scan}))
	if err != nil {
		return nil, "", err
	}
	if len(spoken) > 0 && len(spoken) == w.scan {
		last := changeRow{id: spoken[len(spoken)-1].id, moved: spoken[len(spoken)-1].spokenAt}
		if bound == nil || movedBefore(*bound, last) {
			bound = &last
		}
	}
	// EACH CHANGE ONCE, at its last move: from the lifecycle side where its lifecycle moved it last
	// (or both at one instant), from the conversation side where it is in the walk's state and
	// somebody spoke of it later; the latter's rows are read once the page is cut.
	state, _ := w.keys[len(w.keys)-1].(string)
	var merged []changeRow
	for _, row := range lifecycle {
		if row.moved == row.updatedAt && (bound == nil || !movedBefore(row, *bound)) {
			merged = append(merged, row)
		}
	}
	for _, s := range spoken {
		row := changeRow{id: s.id, moved: s.spokenAt}
		if string(s.state) == state && s.spokenAt > s.lifecycleAt && (bound == nil || !movedBefore(row, *bound)) {
			merged = append(merged, row)
		}
	}
	newestMoveFirst(merged)
	next := ""
	switch {
	case len(merged) > limit:
		merged = merged[:limit]
		next = movedCursor(merged[len(merged)-1])
	case bound != nil:
		next = movedCursor(*bound)
	}
	if err = readSpokenRows(ctx, tx, merged); err != nil {
		return nil, "", err
	}
	return merged, next, nil
}

// newestMoveFirst sorts rows newest [changeRow.moved] first and then by id.
func newestMoveFirst(rows []changeRow) {
	slices.SortFunc(rows, func(a, b changeRow) int {
		return cmp.Or(cmp.Compare(b.moved, a.moved), cmp.Compare(b.id, a.id))
	})
}

// readSpokenRows fills in place each row of page the conversation side kept, an id and an
// instant alone (no repository), with its change's row and what its conversation says of it.
func readSpokenRows(ctx context.Context, tx *sql.Tx, page []changeRow) error {
	var ids []string
	for _, row := range page {
		if row.repoID == "" {
			ids = append(ids, row.id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	byID, err := changesByIDIn(ctx, tx, ids)
	if err != nil {
		return err
	}
	var filled []changeRow
	for _, id := range ids {
		row, found := byID[id]
		if !found {
			return fmt.Errorf("gitlet: a live listing's conversations name change %s, which the rows do not hold",
				id)
		}
		filled = append(filled, row)
	}
	if err = withConversations(ctx, tx, filled); err != nil {
		return err
	}
	for i, j := 0, 0; i < len(page); i++ {
		if page[i].repoID == "" {
			page[i], j = filled[j], j+1
		}
	}
	return nil
}

// readSpoken is the conversations stmt answers, bound by args, each with its change's state and
// lifecycle instant.
func readSpoken(ctx context.Context, tx *sql.Tx, stmt string, args []any) (out []spokenRow, err error) {
	rows, err := tx.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("gitlet: list changes by what was last said of them: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s spokenRow
		if err = rows.Scan(&s.id, &s.spokenAt, &s.state, &s.lifecycleAt); err != nil {
			return nil, fmt.Errorf("gitlet: list changes by what was last said of them: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// A LIVE STATE'S WALK, registered in plan_test.go: its changes by their lifecycle's last move, every
// repository's or one's, and the conversations by when somebody last spoke, each a first page and a
// page after a cursor, newest first, the <= beside the OR keeping the range seek (planner.go).
const (
	liveMovedSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesLiveMovedKey + `
		WHERE state = ? AND ` + liveChanges + ` AND updated_at >= ? ORDER BY updated_at DESC, id DESC LIMIT ?`
	liveMovedAfterSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesLiveMovedKey + `
		WHERE state = ? AND ` + liveChanges + ` AND updated_at >= ? AND updated_at <= ?
			AND (updated_at < ? OR id < ?) ORDER BY updated_at DESC, id DESC LIMIT ?`
	repoLiveMovedSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesRepoLiveMovedKey + `
		WHERE repo_id = ? AND state = ? AND ` + liveChanges + ` AND updated_at >= ?
		ORDER BY updated_at DESC, id DESC LIMIT ?`
	repoLiveMovedAfterSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` +
		changesRepoLiveMovedKey + `
		WHERE repo_id = ? AND state = ? AND ` + liveChanges + ` AND updated_at >= ? AND updated_at <= ?
			AND (updated_at < ? OR id < ?) ORDER BY updated_at DESC, id DESC LIMIT ?`
	spokenOfSQL = `SELECT v.change_id, v.updated_at, c.state, c.updated_at
		FROM code_conversations AS v INDEXED BY ` + conversationsMovedKey + ` CROSS JOIN code_changes AS c
		WHERE v.updated_at > 0 AND v.updated_at >= ? AND c.id = v.change_id
		ORDER BY v.updated_at DESC, v.change_id DESC LIMIT ?`
	spokenOfAfterSQL = `SELECT v.change_id, v.updated_at, c.state, c.updated_at
		FROM code_conversations AS v INDEXED BY ` + conversationsMovedKey + ` CROSS JOIN code_changes AS c
		WHERE v.updated_at > 0 AND v.updated_at >= ? AND v.updated_at <= ?
			AND (v.updated_at < ? OR v.change_id < ?) AND c.id = v.change_id
		ORDER BY v.updated_at DESC, v.change_id DESC LIMIT ?`
	repoSpokenOfSQL = `SELECT v.change_id, v.updated_at, c.state, c.updated_at
		FROM code_conversations AS v INDEXED BY ` + conversationsRepoMovedKey + ` CROSS JOIN code_changes AS c
		WHERE v.repo_id = ? AND v.updated_at > 0 AND v.updated_at >= ? AND c.id = v.change_id
		ORDER BY v.updated_at DESC, v.change_id DESC LIMIT ?`
	repoSpokenOfAfterSQL = `SELECT v.change_id, v.updated_at, c.state, c.updated_at
		FROM code_conversations AS v INDEXED BY ` + conversationsRepoMovedKey + ` CROSS JOIN code_changes AS c
		WHERE v.repo_id = ? AND v.updated_at > 0 AND v.updated_at >= ? AND v.updated_at <= ?
			AND (v.updated_at < ? OR v.change_id < ?) AND c.id = v.change_id
		ORDER BY v.updated_at DESC, v.change_id DESC LIMIT ?`
)
