package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// WHEN A CHANGE LAST MOVED, and the listing narrowed to the changes that moved
// within a window (replicated 0147).
//
// A change moves on two subjects (doc.go): its lifecycle and the pushes that
// move its head on its repository's, which stamp code_changes.updated_at with
// every record that moves the row's version; and what somebody says about it
// on its own, which stamps code_conversations.updated_at with every record
// somebody wrote ([spoken]) — never the home's facts, which a target branch's
// every move would otherwise date. A table is written by the records of one
// subject only, so the instant is kept once a subject, and when a change last
// moved is the later of the two ([ChangeSummary.UpdatedAt]).
//
// # A window walks the two indexes in step
//
// A listing that names a window and nothing an index of who or where leads —
// no repository, work item, reviewer or need, and no live state — walks the
// two instants' indexes together, newest first ([windowWalk]): a page of each, every change taken at
// the later of its two instants and left to the walk of the other index where
// that one is the later, the two merged by instant and id. A change is so
// listed once, at its last move. A page is cut where the walk of an index that
// filled its page stopped, since below that the index holds rows the page did
// not read; its cursor is that place, and the next page reads both indexes
// after it. So a page is still the rows the page READ, two pages' worth at
// most, and may be short, as every listing's is.
//
// A listing that names a window beside what leads another walk tests the
// window on each row its page read, as it tests every filter its index does not
// seek (changeread.go), and lists in that walk's order. A LIVE STATE (draft,
// open) is one of those, the commonest — a screen of changes asks for the open
// ones until told otherwise — because its index holds the live set alone, where
// these two hold every change that moved within the window, every merged one
// among them: in a company that merges a thousand changes a day and keeps five
// hundred open, a month's window walked here reads some thirty thousand rows to
// keep the five hundred, which the state's own walk reads ten pages of.

// ChangeWindow is how far back a `changes` listing narrowed to a window
// reaches: the changes that moved within it, by [ChangeSummary.UpdatedAt].
//
// AN ESTATE REQUEST'S FIELD, NOT A RECORD'S, so it takes no versionedFields
// row. AS A STATED CONDITION (M3-O84 §2 item 5): no build that carries the
// code domain has run without it — none of the domain is on main — so no
// build ignores it and answers the wider listing it narrows. Were the domain
// to reach main first, a listing by window would move to a kind of its own,
// which an older build refuses whole rather than ignore a field.
type ChangeWindow string

// The windows a listing of changes is narrowed to: what a person opening the
// screen after a day, a week or a month away asks first.
const (
	WindowDay   ChangeWindow = "24h"
	WindowWeek  ChangeWindow = "7d"
	WindowMonth ChangeWindow = "30d"
)

// ChangeWindows is the closed set, shortest first.
func ChangeWindows() []ChangeWindow { return []ChangeWindow{WindowDay, WindowWeek, WindowMonth} }

// Valid reports a window this build knows.
func (w ChangeWindow) Valid() bool { return slices.Contains(ChangeWindows(), w) }

// Duration is how far back the window reaches, 0 for none this build knows.
func (w ChangeWindow) Duration() time.Duration {
	switch w {
	case WindowDay:
		return 24 * time.Hour
	case WindowWeek:
		return 7 * 24 * time.Hour
	case WindowMonth:
		return 30 * 24 * time.Hour
	}
	return 0
}

// changesMovedKey is code_changes_updated_idx, (updated_at, id), and
// conversationsMovedKey code_conversations_updated_idx, (updated_at,
// change_id) WHERE updated_at > 0 (replicated 0147): what a window walks.
const (
	changesMovedKey       = "code_changes_updated_idx"
	conversationsMovedKey = "code_conversations_updated_idx"
)

// withConversations sets on every row of rows what its conversation says of
// it ([changeRow.moved], [changeRow.openFindings]), each read by its key in one
// statement driven from the rows' ids — a change nobody has discussed holds
// none, and moved when its lifecycle last did.
func withConversations(ctx context.Context, tx *sql.Tx, rows []changeRow) error {
	for i := range rows {
		rows[i].moved = rows[i].updatedAt
	}
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.id
	}
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	read, err := tx.QueryContext(ctx, conversationsOfSQL, list)
	if err != nil {
		return fmt.Errorf("gitlet: read the conversations of %d changes: %w", len(rows), err)
	}
	defer func() { _ = read.Close() }()
	type said struct{ openFindings, spokenAt int64 }
	of := map[string]said{}
	for read.Next() {
		var id string
		var s said
		if err := read.Scan(&id, &s.openFindings, &s.spokenAt); err != nil {
			return fmt.Errorf("gitlet: read the conversations of %d changes: %w", len(rows), err)
		}
		of[id] = s
	}
	if err := read.Err(); err != nil {
		return fmt.Errorf("gitlet: read the conversations of %d changes: %w", len(rows), err)
	}
	for i := range rows {
		s := of[rows[i].id]
		rows[i].openFindings, rows[i].moved = s.openFindings, max(rows[i].updatedAt, s.spokenAt)
	}
	return nil
}

// windowWalk is one page of a window's walk (see the file's doc): at most
// limit changes that moved at or after since, newest first by [changeRow.moved]
// and then id, each once, after cursor where it is set — and the cursor of the
// page after it, "" where both indexes are read to the window's start.
func windowWalk(ctx context.Context, tx *sql.Tx, since int64, cursor string, limit int) ([]changeRow, string,
	error) {

	byChange, byConversation := changesMovedSQL, conversationsMovedSQL
	args := []any{since}
	if cursor != "" {
		at, id, ok := cutLogCursor(cursor)
		if !ok {
			return nil, "", fmt.Errorf("%w: %q", ErrBadCursor, cursor)
		}
		byChange, byConversation = changesMovedAfterSQL, conversationsMovedAfterSQL
		args = append(args, at, at, id)
	}
	lifecycle, err := readChangesBy(ctx, tx, byChange, append(slices.Clone(args), limit), "their last move")
	if err != nil {
		return nil, "", err
	}
	spoken, err := readChangesBy(ctx, tx, byConversation, append(slices.Clone(args), limit),
		"what was last said of them")
	if err != nil {
		return nil, "", err
	}
	// THE PLACE NEITHER INDEX'S PAGE STOPPED BEFORE: below where the walk of
	// an index that filled its page stopped, that index holds rows this page
	// did not read, so the page ends there — at the later of the two where
	// both filled.
	var bound *changeRow
	cutAt := func(read []changeRow, at func(changeRow) int64) {
		if len(read) < limit {
			return
		}
		last := read[len(read)-1]
		last.moved = at(last)
		if bound == nil || movedBefore(*bound, last) {
			bound = &last
		}
	}
	cutAt(lifecycle, func(r changeRow) int64 { return r.updatedAt })
	cutAt(spoken, func(r changeRow) int64 { return r.moved })
	all := slices.Concat(lifecycle, spoken)
	if err := withConversations(ctx, tx, all); err != nil {
		return nil, "", err
	}
	// EACH CHANGE ONCE, at its last move: a row of the lifecycle's index
	// where its lifecycle moved it last (or both at one instant), a row of
	// the conversation's where somebody spoke of it later.
	var merged []changeRow
	for i, row := range all {
		fromLifecycle := i < len(lifecycle)
		if (fromLifecycle && row.moved == row.updatedAt) || (!fromLifecycle && row.moved > row.updatedAt) {
			if bound == nil || !movedBefore(row, *bound) {
				merged = append(merged, row)
			}
		}
	}
	slices.SortFunc(merged, func(a, b changeRow) int {
		return cmp.Or(cmp.Compare(b.moved, a.moved), cmp.Compare(b.id, a.id))
	})
	switch {
	case len(merged) > limit:
		merged = merged[:limit]
		return merged, movedCursor(merged[len(merged)-1]), nil
	case bound != nil:
		return merged, movedCursor(*bound), nil
	}
	return merged, "", nil
}

// movedBefore reports whether a comes after b in a window's walk, newest
// first: it moved earlier, or at the same instant with a lesser id.
func movedBefore(a, b changeRow) bool {
	return a.moved < b.moved || (a.moved == b.moved && a.id < b.id)
}

// movedCursor is where a page of a window's walk that ended at row resumes:
// its last move and its id ([logCursor]'s spelling).
func movedCursor(row changeRow) string { return logCursor(row.moved, row.id) }

// readChangesBy is the changes stmt answers, bound by args, a row of
// [changeColumns] each — and, where a statement reads a conversation's instant
// after them, that instant as the row's [changeRow.moved].
func readChangesBy(ctx context.Context, tx *sql.Tx, stmt string, args []any, what string) ([]changeRow, error) {
	rows, err := tx.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("gitlet: list changes by %s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	var out []changeRow
	spoken := stmt == conversationsMovedSQL || stmt == conversationsMovedAfterSQL
	for rows.Next() {
		var row changeRow
		var scanErr error
		if spoken {
			var at int64
			row, scanErr = scanChange(func(dest ...any) error { return rows.Scan(append(dest, &at)...) })
			row.moved = at
		} else {
			row, scanErr = scanChange(rows.Scan)
		}
		if scanErr != nil {
			return nil, fmt.Errorf("gitlet: list changes by %s: %w", what, scanErr)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// The window's reads, each registered in plan_test.go: the changes whose
// lifecycle moved within the window, and those somebody spoke of within it —
// the conversation's index walked and each change sought by its key — each a
// first page and a page after a cursor, newest first, with the <= beside the
// OR that keeps the range seek (planner.go), and the partial index's predicate
// repeated verbatim; and the conversations of a page's changes, driven from
// their list.
const (
	changesMovedSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesMovedKey + `
		WHERE updated_at >= ? ORDER BY updated_at DESC, id DESC LIMIT ?`
	changesMovedAfterSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesMovedKey + `
		WHERE updated_at >= ? AND updated_at <= ? AND (updated_at < ? OR id < ?)
		ORDER BY updated_at DESC, id DESC LIMIT ?`
	conversationsMovedSQL = `SELECT ` + qualifiedChangeColumns + `, v.updated_at
		FROM code_conversations AS v INDEXED BY ` + conversationsMovedKey + ` CROSS JOIN code_changes AS c
		WHERE v.updated_at > 0 AND v.updated_at >= ? AND c.id = v.change_id
		ORDER BY v.updated_at DESC, v.change_id DESC LIMIT ?`
	conversationsMovedAfterSQL = `SELECT ` + qualifiedChangeColumns + `, v.updated_at
		FROM code_conversations AS v INDEXED BY ` + conversationsMovedKey + ` CROSS JOIN code_changes AS c
		WHERE v.updated_at > 0 AND v.updated_at >= ? AND v.updated_at <= ?
			AND (v.updated_at < ? OR v.change_id < ?) AND c.id = v.change_id
		ORDER BY v.updated_at DESC, v.change_id DESC LIMIT ?`
	conversationsOfSQL = `SELECT v.change_id, v.open_findings, v.updated_at FROM json_each(?) AS j
		CROSS JOIN code_conversations AS v WHERE v.change_id = CAST(j.value AS TEXT)`
)
