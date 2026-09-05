package pages

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/projection"
	"github.com/crewlet/crewlet/internal/store"
)

// SkillDetector reports whether a page's body is a tool skill.
//
// A SEAM rather than a direct call into the skills package, because it is
// this build's parser answering about this build's rules: a page written by a
// newer node must not carry a claim an older node's parser disagrees with, so
// the flag is DERIVED at apply time and recomputed on every rebuild. Nil
// answers no, which is what a build with no skill parser wired has.
type SkillDetector interface {
	IsSkill(body string) bool
}

// Applier projects the pages family into a node's own tables.
type Applier struct {
	skills SkillDetector
}

// NewApplier builds the knowledge base's applier.
func NewApplier(skills SkillDetector) *Applier { return &Applier{skills: skills} }

// Family is the family this applier serves.
func (a *Applier) Family() projection.Family { return coord.FamilyPages }

// Order ranks a key so a batch applies parents before children.
//
// The CONTAINER first, then the page, then everything that references it. A
// child applied before its page is skipped by the guards below, and the
// projection key set then records it as applied — so nothing re-fetches it and
// a page's history is permanently short. See [projection.Applier.Order] for
// the measurement that produced this.
func (a *Applier) Order(key string) int {
	class, ok := ClassOf(key)
	if !ok {
		return orderUnknown
	}
	switch class {
	case ClassContainer:
		return orderContainer
	case ClassPage:
		return orderPage
	case ClassRevision, ClassComment, ClassChange:
		return orderChild
	case ClassTitle:
		// A CLAIM IS NOT PROJECTED at all — it is a coordination-only
		// record, and the projection's own unique index on
		// (container, title) is what a reader needs. Ranked last so it
		// cannot come before anything.
		return orderUnknown
	}
	return orderUnknown
}

// The ranks, spaced so a class can be inserted without renumbering.
const (
	orderContainer = 10
	orderPage      = 20
	orderChild     = 30
	orderUnknown   = 90
)

// Apply writes one change's rows.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, change coord.Change) error {
	class, ok := ClassOf(change.Key)
	if !ok {
		log.WarnContext(ctx, "pages_projection_foreign_key", "key", change.Key,
			"detail", "a key this grammar did not write; skipped rather than guessed at")
		return nil
	}
	switch class {
	case ClassContainer:
		return a.applyContainer(ctx, tx, change)
	case ClassPage:
		return a.applyPage(ctx, tx, change)
	case ClassRevision:
		return a.applyRevision(ctx, tx, change)
	case ClassComment:
		return a.applyComment(ctx, tx, change)
	case ClassChange:
		return a.applyChange(ctx, tx, change)
	case ClassTitle:
		// Coordination-only: the claim is what makes a title an address,
		// and a reader asks the projection's own index instead.
		return nil
	}
	return nil
}

// Reset drops every row this applier owns.
func (a *Applier) Reset(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []string{
		"page_history", "page_comments", "page_revisions", "page_watchers",
		"page_labels", "pages", "page_containers",
	} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("pages: reset %s: %w", table, err)
		}
	}
	return nil
}

func (a *Applier) applyContainer(ctx context.Context, tx *sql.Tx, change coord.Change) error {
	segs, ok := SegmentsOf(change.Key)
	if !ok || len(segs) != 2 {
		return nil
	}
	key := segs[1]
	if change.Op == coord.OpPurge {
		_, err := tx.ExecContext(ctx, `DELETE FROM page_containers WHERE key = ?`, key)
		return err
	}
	container, err := DecodeContainer(change.Value)
	if err != nil {
		log.WarnContext(ctx, "pages_container_unreadable", "key", change.Key,
			"error", err.Error())
		return nil
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO page_containers (key, name, purpose, created_at, revision, document)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			name = excluded.name, purpose = excluded.purpose,
			revision = excluded.revision, document = excluded.document
		WHERE excluded.revision > page_containers.revision`,
		key, container.Name, container.Purpose,
		store.EncodeTime(container.CreatedAt), int64(change.Revision),
		string(mustEncode(EncodeContainer(container))))
	if err != nil {
		return fmt.Errorf("pages: project the container %s: %w", key, err)
	}
	return nil
}

func (a *Applier) applyPage(ctx context.Context, tx *sql.Tx, change coord.Change) error {
	id, ok := PageIDOf(change.Key)
	if !ok {
		return nil
	}
	if change.Op == coord.OpPurge {
		_, err := tx.ExecContext(ctx, `DELETE FROM pages WHERE id = ?`, id)
		return err
	}
	page, err := DecodePage(change.Value)
	if err != nil {
		log.WarnContext(ctx, "pages_page_unreadable", "key", change.Key,
			"revision", change.Revision, "error", err.Error(),
			"detail", "this page is left out of the projection until a build "+
				"that understands it applies the change")
		return nil
	}

	// DERIVED HERE, on every apply and every rebuild, so a parser fix or a
	// renamed onboarding convention reaches every existing page rather than
	// only the ones edited since.
	skill := 0
	if a.skills != nil && a.skills.IsSkill(page.Body) {
		skill = 1
	}
	onboarding := 0
	if NormalizeTitle(page.Title) == NormalizeTitle(OnboardingTitle) {
		onboarding = 1
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO pages (id, container, parent_id, title, body, status, author,
		                   version, skill, onboarding, created_at, updated_at,
		                   trashed_at, revision, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			container  = excluded.container,
			parent_id  = excluded.parent_id,
			title      = excluded.title,
			body       = excluded.body,
			status     = excluded.status,
			author     = excluded.author,
			version    = excluded.version,
			skill      = excluded.skill,
			onboarding = excluded.onboarding,
			updated_at = excluded.updated_at,
			trashed_at = excluded.trashed_at,
			revision   = excluded.revision,
			document   = excluded.document
		WHERE excluded.revision > pages.revision`,
		page.ID, page.Container, page.ParentID, page.Title, page.Body,
		string(page.Status), page.Author, page.Version, skill, onboarding,
		store.EncodeTime(page.CreatedAt), store.EncodeTime(page.UpdatedAt),
		nullableTime(page.TrashedAt), int64(change.Revision),
		string(mustEncode(EncodePage(page))))
	if err != nil {
		return fmt.Errorf("pages: project %q: %w", page.Title, err)
	}
	if err := replaceSet(ctx, tx, "page_labels", "page_id", "label", page.ID, page.Labels); err != nil {
		return err
	}
	return a.replaceWatchers(ctx, tx, page)
}

func (a *Applier) replaceWatchers(ctx context.Context, tx *sql.Tx, page Page) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM page_watchers WHERE page_id = ?`, page.ID); err != nil {
		return fmt.Errorf("pages: clear watchers on %q: %w", page.Title, err)
	}
	for _, handle := range page.Watchers {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO page_watchers (page_id, handle, muted) VALUES (?, ?, 0)
			ON CONFLICT (page_id, handle) DO UPDATE SET muted = 0`,
			page.ID, handle); err != nil {
			return fmt.Errorf("pages: project a watcher on %q: %w", page.Title, err)
		}
	}
	// THE MUTED ARE ROWS TOO: "this person unwatched" is a fact a re-add
	// has to consult.
	for _, handle := range page.Muted {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO page_watchers (page_id, handle, muted) VALUES (?, ?, 1)
			ON CONFLICT (page_id, handle) DO UPDATE SET muted = 1`,
			page.ID, handle); err != nil {
			return fmt.Errorf("pages: project a mute on %q: %w", page.Title, err)
		}
	}
	return nil
}

// applyRevision projects a revision's METADATA. The body stays in the bucket.
func (a *Applier) applyRevision(ctx context.Context, tx *sql.Tx, change coord.Change) error {
	segs, ok := SegmentsOf(change.Key)
	if !ok || len(segs) != 3 {
		return nil
	}
	pageID := segs[1]
	version, err := strconv.Atoi(segs[2])
	if err != nil {
		return nil
	}
	if change.Op == coord.OpPurge {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM page_revisions WHERE page_id = ? AND version = ?`, pageID, version)
		return err
	}
	rev, err := DecodeRevision(change.Value)
	if err != nil {
		log.WarnContext(ctx, "pages_revision_unreadable", "key", change.Key,
			"error", err.Error())
		return nil
	}
	if !pageExists(ctx, tx, pageID) {
		log.DebugContext(ctx, "pages_revision_without_page", "key", change.Key)
		return nil
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO page_revisions (id, page_id, version, author, message, created_at, revision)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			author = excluded.author, message = excluded.message,
			revision = excluded.revision
		WHERE excluded.revision > page_revisions.revision`,
		rev.ID, pageID, version, rev.Author, rev.Message,
		store.EncodeTime(rev.CreatedAt), int64(change.Revision))
	if err != nil {
		return fmt.Errorf("pages: project revision %d of %s: %w", version, pageID, err)
	}
	return nil
}

func (a *Applier) applyComment(ctx context.Context, tx *sql.Tx, change coord.Change) error {
	segs, ok := SegmentsOf(change.Key)
	if !ok || len(segs) != 3 {
		return nil
	}
	pageID, commentID := segs[1], segs[2]
	if change.Op == coord.OpPurge {
		_, err := tx.ExecContext(ctx, `DELETE FROM page_comments WHERE id = ?`, commentID)
		return err
	}
	comment, err := DecodeComment(change.Value)
	if err != nil {
		log.WarnContext(ctx, "pages_comment_unreadable", "key", change.Key,
			"error", err.Error())
		return nil
	}
	if !pageExists(ctx, tx, pageID) {
		log.DebugContext(ctx, "pages_comment_without_page", "key", change.Key)
		return nil
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO page_comments (id, page_id, author, author_kind, body,
		                           reply_to, created_at, updated_at, revision, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			author = excluded.author, author_kind = excluded.author_kind,
			body = excluded.body, reply_to = excluded.reply_to,
			updated_at = excluded.updated_at, revision = excluded.revision,
			document = excluded.document
		WHERE excluded.revision > page_comments.revision`,
		comment.ID, pageID, comment.Author, string(comment.AuthorKind), comment.Body,
		comment.ReplyTo, store.EncodeTime(comment.CreatedAt),
		store.EncodeTime(comment.UpdatedAt), int64(change.Revision),
		string(mustEncode(EncodeComment(comment))))
	if err != nil {
		return fmt.Errorf("pages: project comment %s: %w", comment.ID, err)
	}
	return nil
}

// applyChange appends to a page's history. Append-only, matching the bucket.
func (a *Applier) applyChange(ctx context.Context, tx *sql.Tx, change coord.Change) error {
	segs, ok := SegmentsOf(change.Key)
	if !ok || len(segs) != 3 {
		return nil
	}
	pageID, changeID := segs[1], segs[2]
	if change.Op == coord.OpPurge {
		_, err := tx.ExecContext(ctx, `DELETE FROM page_history WHERE id = ?`, changeID)
		return err
	}
	record, err := DecodeChange(change.Value)
	if err != nil {
		log.WarnContext(ctx, "pages_change_unreadable", "key", change.Key,
			"error", err.Error())
		return nil
	}
	if !pageExists(ctx, tx, pageID) {
		log.DebugContext(ctx, "pages_change_without_page", "key", change.Key)
		return nil
	}
	quiet := 0
	if record.Quiet {
		quiet = 1
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO page_history (id, page_id, kind, actor, operator_id,
		                          comment_id, excerpt, turn_id, quiet,
		                          created_at, revision, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		changeID, pageID, string(record.Kind), record.Actor, record.OperatorID,
		record.CommentID, record.Excerpt, record.TurnID, quiet,
		store.EncodeTime(record.CreatedAt), int64(change.Revision),
		string(change.Value))
	if err != nil {
		return fmt.Errorf("pages: project change %s: %w", changeID, err)
	}
	return nil
}

// pageExists reports whether a head has been projected.
func pageExists(ctx context.Context, tx *sql.Tx, pageID string) bool {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM pages WHERE id = ?`, pageID).Scan(&one)
	return err == nil
}

// replaceSet rewrites a simple (owner, value) child table.
func replaceSet(ctx context.Context, tx *sql.Tx, table, owner, column, id string, values []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+owner+` = ?`, id); err != nil {
		return fmt.Errorf("pages: clear %s for %s: %w", table, id, err)
	}
	for _, value := range values {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+table+` (`+owner+`, `+column+`) VALUES (?, ?)
			 ON CONFLICT DO NOTHING`, id, value); err != nil {
			return fmt.Errorf("pages: write %s for %s: %w", table, id, err)
		}
	}
	return nil
}

// mustEncode re-renders a record for its document column.
//
// Re-encoded rather than storing the raw change bytes, so the column always
// holds what this build's decoder produced — including the carried unknown
// fields.
func mustEncode(data []byte, err error) []byte {
	if err != nil {
		panic("pages: a record that decoded will not encode: " + err.Error())
	}
	return data
}

// nullableTime encodes an optional instant, NULL when absent.
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return store.EncodeTime(*t)
}
