package work

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/projection"
	"github.com/crewlet/crewlet/internal/store"
)

// Applier projects the work family into a node's own tables.
//
// It implements [projection.Applier], and both of that contract's rules bind
// it: an apply is IDEMPOTENT (a redelivery or a boot re-fetch must reach the
// same rows) and it is a PURE FUNCTION of (row, change) — no clock it stores,
// no coordination read, no network. A rebuild replays the same changes and
// must land on the same projection, or a rebuild is a different projection.
type Applier struct{}

// NewApplier builds the work family's applier.
func NewApplier() *Applier { return &Applier{} }

// Committed implements [projection.Applier]: nothing.
//
// The tracker's consequences are all ROWS. What the pages applier uses the
// hook for — re-reading the tool-skill registry when a skill page moves — has
// no counterpart here: every reader of an item reads the projection, and the
// wakes are the change feed's business rather than the projection's.
func (a *Applier) Committed(context.Context) {}

// Family is the family this applier serves.
func (a *Applier) Family() projection.Family { return coord.FamilyWork }

// Apply writes one change's rows.
//
// A KEY CLASS THIS BUILD HAS NO RULE FOR IS IGNORED rather than refused: a
// newer node writes classes this one does not know, and a rolling upgrade
// must not wedge the older half's projector on a record it was never meant to
// understand.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, change coord.Change) error {
	class, ok := ClassOf(change.Key)
	if !ok {
		log.WarnContext(ctx, "work_projection_foreign_key", "key", change.Key,
			"detail", "a key this grammar did not write; skipped rather than guessed at")
		return nil
	}
	id, _ := ItemIDOf(change.Key)
	switch class {
	case ClassItem:
		return a.applyItem(ctx, tx, change, id)
	case ClassComment:
		return a.applyComment(ctx, tx, change)
	case ClassChange:
		return a.applyChange(ctx, tx, change)
	case ClassCounter:
		return a.applyCounter(ctx, tx, change)
	}
	return nil
}

// Order ranks a key so a batch applies parents before children.
//
// THE ITEM HEAD COMES FIRST, and everything that references it after. A
// comment or a change whose item is not projected yet is skipped by the
// guards below, and the projection key set then records it as applied — so
// nothing ever re-fetches it and a thread is permanently short, with nothing
// anywhere reporting it. Measured before this existed: a twenty-comment item
// projected twelve of them on a fresh node.
//
// The counter is ranked first of all because it references nothing and a
// board reads it; the relative order of comments and changes does not matter,
// and they share a rank rather than being given an arbitrary one.
func (a *Applier) Order(key string) int {
	class, ok := ClassOf(key)
	if !ok {
		return orderUnknown
	}
	switch class {
	case ClassCounter:
		return orderCounter
	case ClassItem:
		return orderItem
	case ClassComment, ClassChange:
		return orderChild
	}
	return orderUnknown
}

// The ranks. Spaced so a class can be inserted between two without renumbering
// the rest — a renumbering that missed one would reorder a batch silently.
const (
	orderCounter = 10
	orderItem    = 20
	orderChild   = 30

	// orderUnknown is LAST, so a class this build has no rule for cannot
	// come before a parent it might reference. It is skipped by Apply
	// anyway; ranking it early would only make that skip depend on the
	// order of a map.
	orderUnknown = 90
)

// Reset drops every row this applier owns.
//
// THE ORDER IS CHILDREN FIRST even though the foreign keys cascade, because a
// cascade is a per-row trigger and a whole-table rebuild is millions of them:
// the explicit deletes are one scan each.
func (a *Applier) Reset(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []string{
		"work_history", "work_comments", "work_links", "work_watchers",
		"work_labels", "work_items", "work_counters",
	} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("work: reset %s: %w", table, err)
		}
	}
	return nil
}

// applyItem upserts a head and everything derived from it.
func (a *Applier) applyItem(ctx context.Context, tx *sql.Tx, change coord.Change, id string) error {
	if change.Op == coord.OpPurge {
		// The children cascade, and so does the index row's source — but
		// the index row itself is the indexer's to remove, on its own
		// pass, for the reason its package doc gives.
		_, err := tx.ExecContext(ctx, `DELETE FROM work_items WHERE id = ?`, id)
		return err
	}
	item, err := DecodeItem(change.Value)
	if err != nil {
		// A HEAD A NEWER BUILD WROTE IS SKIPPED WITH A WARNING, not fatal.
		// Failing here would stop the whole projector on a rolling
		// upgrade, taking every other item down with the one it could not
		// read.
		log.WarnContext(ctx, "work_item_unreadable", "key", change.Key,
			"revision", change.Revision, "error", err.Error(),
			"detail", "this item is left out of the projection until a build "+
				"that understands it applies the change")
		return nil
	}
	if err := a.upsertItem(ctx, tx, item, change.Revision); err != nil {
		return err
	}
	if err := replaceSet(ctx, tx, "work_labels", "item_id", "label", item.ID, item.Labels); err != nil {
		return err
	}
	if err := a.replaceWatchers(ctx, tx, item); err != nil {
		return err
	}
	return a.replaceLinks(ctx, tx, item)
}

func (a *Applier) upsertItem(ctx context.Context, tx *sql.Tx, item Item, revision uint64) error {
	// IDEMPOTENT BY REVISION: a change at or below the row's revision is a
	// redelivery or a boot re-fetch of something already applied, and
	// applying it again would be harmless but would also overwrite a newer
	// head the live watch had already delivered.
	_, err := tx.ExecContext(ctx, `
		INSERT INTO work_items (
			id, item_key, project, type, parent_id, title, body, status,
			close_reason, duplicate_of, priority, reporter, assignee,
			due_at, created_at, updated_at, closed_at, revision, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			item_key     = excluded.item_key,
			project      = excluded.project,
			type         = excluded.type,
			parent_id    = excluded.parent_id,
			title        = excluded.title,
			body         = excluded.body,
			status       = excluded.status,
			close_reason = excluded.close_reason,
			duplicate_of = excluded.duplicate_of,
			priority     = excluded.priority,
			reporter     = excluded.reporter,
			assignee     = excluded.assignee,
			due_at       = excluded.due_at,
			created_at   = excluded.created_at,
			updated_at   = excluded.updated_at,
			closed_at    = excluded.closed_at,
			revision     = excluded.revision,
			document     = excluded.document
		WHERE excluded.revision > work_items.revision`,
		item.ID, item.Key, item.Project, string(item.Type), item.ParentID,
		item.Title, item.Body, string(item.Status), string(item.CloseReason),
		item.DuplicateOf, string(item.Priority), item.Reporter, item.Assignee,
		nullableTime(item.Due), store.EncodeTime(item.CreatedAt),
		store.EncodeTime(item.UpdatedAt), nullableTime(item.ClosedAt),
		int64(revision), string(mustEncodeItem(item)))
	if err != nil {
		return fmt.Errorf("work: project item %s: %w", item.Key, err)
	}
	return nil
}

// mustEncodeItem re-renders a head for the document column.
//
// RE-ENCODED RATHER THAN STORING change.Value, so the column always holds
// what this build's decoder produced from it — including the carried unknown
// fields. Storing the raw bytes would work equally well today and would drift
// the moment a decoder normalised anything.
func mustEncodeItem(item Item) []byte {
	data, err := EncodeItem(item)
	if err != nil {
		// It decoded a moment ago, so this cannot fail on any input that
		// reached here — and a projector that swallowed it would write an
		// empty document column nobody would notice until a read.
		panic("work: an item that decoded will not encode: " + err.Error())
	}
	return data
}

func (a *Applier) replaceWatchers(ctx context.Context, tx *sql.Tx, item Item) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM work_watchers WHERE item_id = ?`, item.ID); err != nil {
		return fmt.Errorf("work: clear watchers on %s: %w", item.Key, err)
	}
	for _, handle := range item.Watchers {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO work_watchers (item_id, handle, muted) VALUES (?, ?, 0)
			ON CONFLICT (item_id, handle) DO UPDATE SET muted = 0`,
			item.ID, handle); err != nil {
			return fmt.Errorf("work: project a watcher on %s: %w", item.Key, err)
		}
	}
	// THE MUTED ARE ROWS TOO, not an absence: "this person unwatched" is a
	// fact a re-add has to consult, and a projection that dropped it would
	// let the next assignment silently re-subscribe somebody who opted out.
	for _, handle := range item.Muted {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO work_watchers (item_id, handle, muted) VALUES (?, ?, 1)
			ON CONFLICT (item_id, handle) DO UPDATE SET muted = 1`,
			item.ID, handle); err != nil {
			return fmt.Errorf("work: project a mute on %s: %w", item.Key, err)
		}
	}
	return nil
}

// replaceLinks writes both directions of every authored link.
//
// THE INVERSE IS DERIVED HERE and marked, so a board can render "blocked by"
// without a second authored record that could disagree with the first. Only
// the derived halves of THIS item's links are replaced — the derived rows
// pointing AT this item belong to whoever authored them.
func (a *Applier) replaceLinks(ctx context.Context, tx *sql.Tx, item Item) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM work_links WHERE item_id = ? AND derived = 0`, item.ID); err != nil {
		return fmt.Errorf("work: clear links on %s: %w", item.Key, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM work_links WHERE other_id = ? AND derived = 1`, item.ID); err != nil {
		return fmt.Errorf("work: clear derived links from %s: %w", item.Key, err)
	}
	for _, link := range item.Links {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO work_links (item_id, other_id, kind, derived) VALUES (?, ?, ?, 0)
			ON CONFLICT (item_id, other_id, kind) DO UPDATE SET derived = 0`,
			item.ID, link.To, string(link.Kind)); err != nil {
			return fmt.Errorf("work: project a link on %s: %w", item.Key, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO work_links (item_id, other_id, kind, derived) VALUES (?, ?, ?, 1)
			ON CONFLICT (item_id, other_id, kind) DO NOTHING`,
			link.To, item.ID, string(link.Kind.Inverse())); err != nil {
			return fmt.Errorf("work: project the inverse link on %s: %w", item.Key, err)
		}
	}
	return nil
}

func (a *Applier) applyComment(ctx context.Context, tx *sql.Tx, change coord.Change) error {
	segs, ok := SegmentsOf(change.Key)
	if !ok || len(segs) != 3 {
		return nil
	}
	itemID, commentID := segs[1], segs[2]
	if change.Op == coord.OpPurge {
		_, err := tx.ExecContext(ctx, `DELETE FROM work_comments WHERE id = ?`, commentID)
		return err
	}
	comment, err := DecodeComment(change.Value)
	if err != nil {
		log.WarnContext(ctx, "work_comment_unreadable", "key", change.Key,
			"error", err.Error())
		return nil
	}
	// A COMMENT WHOSE ITEM IS NOT HERE IS SKIPPED, not an error — and
	// [Applier.Order] is what makes that safe: a batch applies the item
	// first, so the only way to reach here is an item that genuinely does
	// not exist (a removal that raced this comment's own write). Without
	// the ordering this skip was PERMANENT, because the key set records
	// the comment as applied and nothing re-fetches it.
	if !itemExists(ctx, tx, itemID) {
		log.DebugContext(ctx, "work_comment_without_item", "key", change.Key,
			"item", itemID)
		return nil
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO work_comments (id, item_id, author, author_kind, body,
		                           reply_to, created_at, updated_at, revision, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			author      = excluded.author,
			author_kind = excluded.author_kind,
			body        = excluded.body,
			reply_to    = excluded.reply_to,
			updated_at  = excluded.updated_at,
			revision    = excluded.revision,
			document    = excluded.document
		WHERE excluded.revision > work_comments.revision`,
		comment.ID, itemID, comment.Author, string(comment.AuthorKind), comment.Body,
		comment.ReplyTo, store.EncodeTime(comment.CreatedAt),
		store.EncodeTime(comment.UpdatedAt), int64(change.Revision),
		string(mustEncodeComment(comment)))
	if err != nil {
		return fmt.Errorf("work: project comment %s: %w", comment.ID, err)
	}
	return nil
}

func mustEncodeComment(c Comment) []byte {
	data, err := EncodeComment(c)
	if err != nil {
		panic("work: a comment that decoded will not encode: " + err.Error())
	}
	return data
}

// applyChange appends to an item's history.
//
// APPEND-ONLY, matching the bucket: a change key is created once and never
// rewritten, so this row is inserted once and never updated. The conflict
// clause is DO NOTHING rather than an upsert for that reason — a second
// delivery of the same change is a redelivery, not an edit.
func (a *Applier) applyChange(ctx context.Context, tx *sql.Tx, change coord.Change) error {
	segs, ok := SegmentsOf(change.Key)
	if !ok || len(segs) != 3 {
		return nil
	}
	itemID, changeID := segs[1], segs[2]
	if change.Op == coord.OpPurge {
		_, err := tx.ExecContext(ctx, `DELETE FROM work_history WHERE id = ?`, changeID)
		return err
	}
	record, err := DecodeChange(change.Value)
	if err != nil {
		log.WarnContext(ctx, "work_change_unreadable", "key", change.Key,
			"error", err.Error())
		return nil
	}
	if !itemExists(ctx, tx, itemID) {
		// As for a comment: ordered after the item, so reaching here means
		// the item is genuinely gone.
		log.DebugContext(ctx, "work_change_without_item", "key", change.Key,
			"item", itemID)
		return nil
	}
	quiet := 0
	if record.Quiet {
		quiet = 1
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO work_history (id, item_id, kind, actor, operator_id,
		                          comment_id, excerpt, turn_id, quiet,
		                          created_at, revision, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		changeID, itemID, string(record.Kind), record.Actor, record.OperatorID,
		record.CommentID, record.Excerpt, record.TurnID, quiet,
		store.EncodeTime(record.CreatedAt), int64(change.Revision),
		string(change.Value))
	if err != nil {
		return fmt.Errorf("work: project change %s: %w", changeID, err)
	}
	return nil
}

func (a *Applier) applyCounter(ctx context.Context, tx *sql.Tx, change coord.Change) error {
	segs, ok := SegmentsOf(change.Key)
	if !ok || len(segs) != 2 {
		return nil
	}
	project := segs[1]
	if change.Op == coord.OpPurge {
		_, err := tx.ExecContext(ctx, `DELETE FROM work_counters WHERE project = ?`, project)
		return err
	}
	counter, err := DecodeCounter(change.Value)
	if err != nil {
		log.WarnContext(ctx, "work_counter_unreadable", "key", change.Key,
			"error", err.Error())
		return nil
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO work_counters (project, last, revision) VALUES (?, ?, ?)
		ON CONFLICT (project) DO UPDATE SET
			last = excluded.last, revision = excluded.revision
		WHERE excluded.revision > work_counters.revision`,
		project, counter.Last, int64(change.Revision))
	if err != nil {
		return fmt.Errorf("work: project the %s counter: %w", project, err)
	}
	return nil
}

// itemExists reports whether a head has been projected.
func itemExists(ctx context.Context, tx *sql.Tx, itemID string) bool {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM work_items WHERE id = ?`, itemID).Scan(&one)
	return err == nil
}

// replaceSet rewrites a simple (owner, value) child table.
func replaceSet(ctx context.Context, tx *sql.Tx, table, owner, column, id string, values []string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+table+` WHERE `+owner+` = ?`, id); err != nil {
		return fmt.Errorf("work: clear %s for %s: %w", table, id, err)
	}
	for _, value := range slices.Compact(slices.Clone(values)) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+table+` (`+owner+`, `+column+`) VALUES (?, ?)
			 ON CONFLICT DO NOTHING`, id, value); err != nil {
			return fmt.Errorf("work: write %s for %s: %w", table, id, err)
		}
	}
	return nil
}

// nullableTime encodes an optional instant, NULL when absent.
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return store.EncodeTime(*t)
}
