package pages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/store"
)

// THE PAGE CASES: create, rename, patch, trash, restore and purge.

// applyCreate writes the claim, the head, the first revision and the history
// entry — in one transaction, which is the whole reason this domain exists.
func (a *Applier) applyCreate(ctx context.Context, tx *sql.Tx, at applyContext,
	p CreatePayload) (int, error) {

	container, token, err := SplitTitleID(at.subject().ID)
	if err != nil {
		return 0, fmt.Errorf("pages: the create at %s: %w", at.position, err)
	}
	// THE SUBJECT IS THE ADDRESS, so the payload's title has to be the one
	// that was arbitrated. Without this a writer could take one address at
	// the broker and claim another in its payload, and every node would
	// write the second while the first was the one nobody else could
	// have.
	if err := addressMatches(at, container, token, p.Container, p.Title); err != nil {
		return 0, err
	}
	// A CREATE FOR A PURGED PAGE APPLIES NOWHERE. The gate cannot see this
	// — a title subject names its page only in a payload the gate may not
	// be able to read — so the check is here, where the payload is
	// decoded.
	purged, err := pageIsPurged(ctx, tx, p.PageID)
	if err != nil {
		return 0, err
	}
	if purged {
		return 0, nil
	}

	rows := 0
	claimed, err := claimTitle(ctx, tx, at, container, token, p.Title, p.PageID)
	if err != nil {
		return 0, err
	}
	rows += claimed

	head := Page{
		V: DocumentVersion, ID: p.PageID, Container: p.Container,
		ParentID: p.ParentID, Title: p.Title, Body: p.Body,
		Status: p.Status, Labels: sorted(p.Labels), Watchers: sorted(p.Watchers),
		Version: 1, Author: p.Author,
		CreatedAt: at.brokerAt, UpdatedAt: at.brokerAt,
	}
	written, err := a.writeHead(ctx, tx, at, head)
	if err != nil {
		return 0, err
	}
	rows += written

	// THE FIRST REVISION IS WRITTEN HERE, so a page's history starts at
	// the body it was created with rather than at its first edit.
	revised, err := a.writeRevision(ctx, tx, at, p.PageID, 1, p.Title, p.Body,
		"", p.Author)
	if err != nil {
		return 0, err
	}
	rows += revised

	entry, err := a.writeHistory(ctx, tx, at, p.PageID, ChangeCreated, "")
	if err != nil {
		return 0, err
	}
	return rows + entry, nil
}

// applyRename moves a page to a new address, releasing the old claim.
//
// THE RELEASE IS STATED BY THE RECORD, which is what makes it a deletion the
// log committed rather than one this node decided: a rename that computed the
// old title from its own row would delete a different claim on a node whose
// row was at a different version.
func (a *Applier) applyRename(ctx context.Context, tx *sql.Tx, at applyContext,
	p RenamePayload) (int, error) {

	container, token, err := SplitTitleID(at.subject().ID)
	if err != nil {
		return 0, fmt.Errorf("pages: the rename at %s: %w", at.position, err)
	}
	if err := addressMatches(at, container, token, p.Container, p.Title); err != nil {
		return 0, err
	}
	rows := 0
	claimed, err := claimTitle(ctx, tx, at, container, token, p.Title, p.PageID)
	if err != nil {
		return 0, err
	}
	rows += claimed

	// The old claim goes only if it still names THIS page: a claim another
	// page has since taken is not this record's to release. It is looked
	// up by the FORMER title's own token, recomputed here — the record
	// states the title and the token is a function of it, so a stated
	// token would be a second value that could disagree.
	res, err := tx.ExecContext(ctx, `
		DELETE FROM pages_titles
		WHERE container = ? AND title_token = ? AND page_id = ? AND version < ?`,
		p.FormerContainer, TitleToken(p.FormerTitle), p.PageID, at.packed)
	if err != nil {
		return 0, fmt.Errorf("pages: release %s/%s at %s: %w",
			p.FormerContainer, p.FormerTitle, at.position, err)
	}
	n, _ := res.RowsAffected()
	rows += int(n)

	res, err = tx.ExecContext(ctx, `
		UPDATE pages_heads
		SET title = ?, title_norm = ?, container = ?, updated_at = ?,
		    scoped_through = ?
		WHERE id = ? AND scoped_through < ? AND version < ?`,
		p.Title, NormalizeTitle(p.Title), container,
		store.EncodeTime(at.brokerAt), at.packed, p.PageID, at.packed, at.packed)
	if err != nil {
		return 0, fmt.Errorf("pages: rename %s at %s: %w",
			p.PageID, at.position, err)
	}
	n, _ = res.RowsAffected()
	rows += int(n)

	entry, err := a.writeHistory(ctx, tx, at, p.PageID, ChangeRenamed, "")
	if err != nil {
		return 0, err
	}
	return rows + entry, nil
}

// applyPage is a patch, a trash, a restore or a purge on an existing page.
func (a *Applier) applyPage(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	switch p := payload.(type) {
	case PagePatch:
		return a.applyPatch(ctx, tx, at, p)
	case StatusPayload:
		return a.applyStatusChange(ctx, tx, at, p)
	}
	return 0, fmt.Errorf("pages: the page record at %s carries a %T",
		at.position, payload)
}

// applyPatch writes one change to a page head.
func (a *Applier) applyPatch(ctx context.Context, tx *sql.Tx, at applyContext,
	p PagePatch) (int, error) {

	head, found, err := readHead(ctx, tx, at.subject().ID)
	if err != nil {
		return 0, err
	}
	if !found {
		// A PATCH FOR A PAGE THIS NODE DOES NOT HAVE, under a STRICT
		// replay, is a malformed record rather than a race: the create
		// is below this position on the same ordered log, so a node
		// that applied that position and has no row applied it wrong.
		return 0, fmt.Errorf("pages: the patch at %s names page %s, which this "+
			"node has no row for — under a strict replay its create is below "+
			"this position, so a missing row is a record this build applied "+
			"incorrectly rather than one that has not arrived",
			at.position, at.subject().ID)
	}

	rows := 0
	changed := ChangeSaved
	if p.Body != nil {
		// REVISION N IS THE BODY AT VERSION N — so the head moves first
		// and the revision is written at the version it produced.
		//
		// The other reading, "the revision holds what was replaced",
		// collides with itself on the first save: a create already
		// wrote version 1, and a save from version 1 would write
		// version 1 again. It also leaves the CURRENT body reachable
		// only through the head, so "open version 7" works for every
		// version except the newest.
		head.Body = *p.Body
		head.Version++
		written, err := a.writeRevision(ctx, tx, at, head.ID, head.Version,
			head.Title, head.Body, deref(p.Message), at.record.Actor)
		if err != nil {
			return 0, err
		}
		rows += written
	}
	if p.ParentID != nil {
		head.ParentID = *p.ParentID
		changed = ChangeMoved
	}
	if p.Status != nil {
		head.Status = *p.Status
		changed = ChangeStatus
	}
	if p.Labels != nil {
		head.Labels = sorted(p.Labels)
		changed = ChangeLabels
	}
	if p.Watchers != nil || p.Muted != nil {
		head.Watchers = sorted(p.Watchers)
		head.Muted = sorted(p.Muted)
		changed = ChangeWatchers
	}
	if p.Comment != nil {
		written, kind, err := a.applyComment(ctx, tx, at, head.ID, *p.Comment)
		if err != nil {
			return 0, err
		}
		rows += written
		changed = kind
	}
	head.UpdatedAt = at.brokerAt

	written, err := a.writeHead(ctx, tx, at, head)
	if err != nil {
		return 0, err
	}
	rows += written

	// THE PRUNE RIDES THE COMMIT as the record's own list, so every node
	// deletes exactly the same rows at exactly the same position.
	for _, version := range p.RetiredRevisions {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM pages_revisions WHERE page_id = ? AND edit_version = ?`,
			head.ID, version)
		if err != nil {
			return 0, fmt.Errorf("pages: retire revision %d of %s at %s: %w",
				version, head.ID, at.position, err)
		}
		n, _ := res.RowsAffected()
		rows += int(n)
	}

	entry, err := a.writeHistory(ctx, tx, at, head.ID, changed, commentID(p.Comment))
	if err != nil {
		return 0, err
	}
	return rows + entry, nil
}

// applyStatusChange is a trash, a restore or a purge.
func (a *Applier) applyStatusChange(ctx context.Context, tx *sql.Tx,
	at applyContext, p StatusPayload) (int, error) {

	id := at.subject().ID
	switch at.record.Op {
	case OpPurge:
		return a.applyPurge(ctx, tx, at, p)
	case OpTombstone, OpRestore:
		status, trashed := StatusTrashed, sql.NullInt64{
			Int64: store.EncodeTime(at.brokerAt), Valid: true,
		}
		kind := ChangeRemoved
		if at.record.Op == OpRestore {
			status, trashed, kind = StatusPublished, sql.NullInt64{}, ChangeStatus
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE pages_heads
			SET status = ?, trashed_at = ?, updated_at = ?, version = ?
			WHERE id = ? AND version < ?`,
			string(status), trashed, store.EncodeTime(at.brokerAt), at.packed,
			id, at.packed)
		if err != nil {
			return 0, fmt.Errorf("pages: set %s's status at %s: %w",
				id, at.position, err)
		}
		n, _ := res.RowsAffected()
		entry, err := a.writeHistory(ctx, tx, at, id, kind, "")
		if err != nil {
			return 0, err
		}
		return int(n) + entry, nil
	}
	return 0, fmt.Errorf("pages: %s is not a status operation, and the record "+
		"at %s carries one", at.record.Op, at.position)
}

// applyPurge destroys a page and writes the marker that makes it permanent.
//
// THE MARKER OUTLIVES EVERY OTHER ROW, which is what lets a node that was away
// tell a page that never existed from one that was deliberately destroyed.
func (a *Applier) applyPurge(ctx context.Context, tx *sql.Tx, at applyContext,
	p StatusPayload) (int, error) {

	id := at.subject().ID
	var container string
	err := tx.QueryRowContext(ctx,
		`SELECT container FROM pages_heads WHERE id = ?`, id).Scan(&container)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("pages: read %s before purging it at %s: %w",
			id, at.position, err)
	}

	rows := 0
	// EVERY CHILD ROW IS NAMED, because a cascade is a delete nobody
	// committed: the statements below are part of this record's own effect
	// and are therefore identical on every node.
	for _, stmt := range []struct{ table, column string }{
		{"pages_labels", "page_id"},
		{"pages_watchers", "page_id"},
		{"pages_revisions", "page_id"},
		{"pages_comments", "page_id"},
		{"pages_history", "page_id"},
		{"pages_titles", "page_id"},
		{"pages_skills", "page_id"},
	} {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM `+stmt.table+` WHERE `+stmt.column+` = ?`, id)
		if err != nil {
			return 0, fmt.Errorf("pages: purge %s from %s at %s: %w",
				id, stmt.table, at.position, err)
		}
		n, _ := res.RowsAffected()
		rows += int(n)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM pages_heads WHERE id = ?`, id)
	if err != nil {
		return 0, fmt.Errorf("pages: purge %s at %s: %w", id, at.position, err)
	}
	n, _ := res.RowsAffected()
	rows += int(n)

	res, err = tx.ExecContext(ctx, `
		INSERT INTO pages_deletions
			(page_id, container, at, by, reason, purge_record_id, version)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (page_id) DO NOTHING`,
		id, container, store.EncodeTime(at.brokerAt), at.record.Actor,
		p.Reason, at.record.OpID, at.packed)
	if err != nil {
		return 0, fmt.Errorf("pages: mark %s deleted at %s: %w",
			id, at.position, err)
	}
	n, _ = res.RowsAffected()
	return rows + int(n), nil
}

// applyComment writes one comment's create, edit or removal.
func (a *Applier) applyComment(ctx context.Context, tx *sql.Tx, at applyContext,
	pageID string, c CommentPatch) (int, ChangeKind, error) {

	if c.Removed {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM pages_comments WHERE id = ? AND version < ?`,
			c.ID, at.packed)
		if err != nil {
			return 0, "", fmt.Errorf("pages: remove comment %s at %s: %w",
				c.ID, at.position, err)
		}
		n, _ := res.RowsAffected()
		return int(n), ChangeCommentEdited, nil
	}
	document, err := EncodeComment(Comment{
		V: DocumentVersion, ID: c.ID, PageID: pageID,
		Author: c.Author, AuthorKind: c.AuthorKind, Body: deref(c.Body),
		Mentions: sorted(c.Mentions), ReplyTo: c.ReplyTo,
		CreatedAt: at.brokerAt, UpdatedAt: at.brokerAt,
	})
	if err != nil {
		return 0, "", err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO pages_comments
			(id, page_id, author, author_kind, body, reply_to, created_at,
			 updated_at, version, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			body = excluded.body, updated_at = excluded.updated_at,
			version = excluded.version, document = excluded.document
		WHERE excluded.version > pages_comments.version`,
		c.ID, pageID, c.Author, string(c.AuthorKind), deref(c.Body), c.ReplyTo,
		store.EncodeTime(at.brokerAt), store.EncodeTime(at.brokerAt),
		at.packed, document)
	if err != nil {
		return 0, "", fmt.Errorf("pages: apply comment %s at %s: %w",
			c.ID, at.position, err)
	}
	n, _ := res.RowsAffected()
	// A CREATE AND AN EDIT ARE TOLD APART BY WHETHER THE INSERT WAS NEW,
	// which the affected count answers — rather than by a fourth field on
	// the patch that a writer could set wrongly.
	kind := ChangeCommentEdited
	if n == 1 {
		kind = ChangeComment
	}
	return int(n), kind, nil
}

// writeHead upserts the page row, its two child sets and its divergent skill
// row.
func (a *Applier) writeHead(ctx context.Context, tx *sql.Tx, at applyContext,
	head Page) (int, error) {

	document, err := EncodePage(head)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO pages_heads
			(id, container, parent_id, title, title_norm, body, status, author,
			 edit_version, created_at, updated_at, trashed_at, version,
			 scoped_through, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT (id) DO UPDATE SET
			container = excluded.container, parent_id = excluded.parent_id,
			title = excluded.title, title_norm = excluded.title_norm,
			body = excluded.body, status = excluded.status,
			author = excluded.author, edit_version = excluded.edit_version,
			updated_at = excluded.updated_at, trashed_at = excluded.trashed_at,
			version = excluded.version, document = excluded.document
		WHERE excluded.version > pages_heads.version`,
		head.ID, head.Container, head.ParentID, head.Title,
		NormalizeTitle(head.Title),
		head.Body, string(head.Status), head.Author, head.Version,
		store.EncodeTime(head.CreatedAt), store.EncodeTime(head.UpdatedAt),
		nullableTime(head.TrashedAt), at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("pages: apply the head of %s at %s: %w",
			head.ID, at.position, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// THE VERSION GUARD SKIPPED IT, which is a redelivery. Writing
		// the child sets anyway would replace a newer record's rows
		// with an older one's.
		return 0, nil
	}
	rows := int(n)

	labels, err := replaceChildSet(ctx, tx, "pages_labels", "page_id", "label",
		head.ID, head.Labels)
	if err != nil {
		return 0, err
	}
	rows += labels

	watchers, err := a.writeWatchers(ctx, tx, head)
	if err != nil {
		return 0, err
	}
	rows += watchers

	skills, err := a.writeSkillRow(ctx, tx, at, head)
	if err != nil {
		return 0, err
	}
	return rows + skills, nil
}

// writeWatchers replaces the watcher set, carrying the mute flag.
func (a *Applier) writeWatchers(ctx context.Context, tx *sql.Tx, head Page) (int, error) {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM pages_watchers WHERE page_id = ?`, head.ID); err != nil {
		return 0, fmt.Errorf("pages: clear %s's watchers: %w", head.ID, err)
	}
	muted := map[string]bool{}
	for _, handle := range head.Muted {
		muted[handle] = true
	}
	rows := 0
	// SORTED, because a collection written from a map without sorting it
	// is the one purity rule an applier breaks without noticing: the rows
	// land in a different order on every node and the file's checksum
	// diverges.
	for _, handle := range sorted(head.Watchers) {
		flag := 0
		if muted[handle] {
			flag = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO pages_watchers (page_id, handle, muted) VALUES (?, ?, ?)
			 ON CONFLICT (page_id, handle) DO UPDATE SET muted = excluded.muted`,
			head.ID, handle, flag); err != nil {
			return 0, fmt.Errorf("pages: watch %s as %s: %w", head.ID, handle, err)
		}
		rows++
	}
	return rows, nil
}

// writeSkillRow recomputes THIS BUILD'S answer about a page's body.
//
// DIVERGENT BY CONSTRUCTION: two nodes on different builds legitimately
// disagree, which is why the row is in its own table and outside the identity
// claim. It is recomputed on every apply rather than carried on the record, so
// a parser fix reaches every page on the next rebuild.
func (a *Applier) writeSkillRow(ctx context.Context, tx *sql.Tx,
	at applyContext, head Page) (int, error) {

	skill := 0
	if a.skills != nil && a.skills.IsSkill(head.Body) {
		skill = 1
	}
	onboarding := 0
	if NormalizeTitle(head.Title) == NormalizeTitle(OnboardingTitle) {
		onboarding = 1
	}
	var was int
	err := tx.QueryRowContext(ctx,
		`SELECT skill FROM pages_skills WHERE page_id = ?`, head.ID).Scan(&was)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("pages: read %s's skill flag: %w", head.ID, err)
	}
	if was == 1 || skill == 1 {
		// A PAGE THAT WAS A SKILL OR HAS BECOME ONE, which is both
		// halves of what the registry has to notice: natively there is
		// no page webhook and the change feed drops these changes, so
		// the apply is the only thing that sees both an arrival and a
		// departure.
		a.skillMoved = true
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO pages_skills (page_id, container, skill, onboarding, at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (page_id) DO UPDATE SET
			container = excluded.container, skill = excluded.skill,
			onboarding = excluded.onboarding, at = excluded.at`,
		head.ID, head.Container, skill, onboarding,
		store.EncodeTime(at.brokerAt))
	if err != nil {
		return 0, fmt.Errorf("pages: record %s's skill flag: %w", head.ID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// writeRevision writes one immutable past body.
func (a *Applier) writeRevision(ctx context.Context, tx *sql.Tx, at applyContext,
	pageID string, version int, title, body, message, author string) (int, error) {

	res, err := tx.ExecContext(ctx, `
		INSERT INTO pages_revisions
			(page_id, edit_version, title, body, message, author, created_at,
			 version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (page_id, edit_version) DO NOTHING`,
		pageID, version, title, body, message, author,
		store.EncodeTime(at.brokerAt), at.packed)
	if err != nil {
		return 0, fmt.Errorf("pages: write revision %d of %s at %s: %w",
			version, pageID, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// writeHistory writes one entry of what a card and a digest render from.
func (a *Applier) writeHistory(ctx context.Context, tx *sql.Tx, at applyContext,
	pageID string, kind ChangeKind, commentID string) (int, error) {

	if at.record.OpID == "" {
		// NO OP ID, NO HISTORY ROW. The only record without one is the
		// barrier, which never reaches here — so this is a writer that
		// omitted it, and inventing an id would make the row
		// unrepeatable across a replay.
		return 0, fmt.Errorf("pages: the record at %s writes a history entry "+
			"and carries no operation id, which is what the row is keyed on",
			at.position)
	}
	quiet := 0
	excerpt := ""
	if at.record.Notify == nil {
		quiet = 1
	} else {
		excerpt = at.record.Notify.Excerpt
	}
	change := Change{
		V: DocumentVersion, ID: at.record.OpID, PageID: pageID, Kind: kind,
		Actor: at.record.Actor, ActorKind: at.record.ActorKind,
		OperatorID: at.record.OperatorID, CommentID: commentID,
		Excerpt: excerpt, TurnID: at.record.TurnID, Chain: at.record.Chain,
		Quiet: quiet == 1, CreatedAt: at.brokerAt,
	}
	document, err := EncodeChange(change)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO pages_history
			(id, page_id, kind, actor, actor_kind, operator_id, comment_id,
			 excerpt, turn_id, quiet, created_at, broker_at, version, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		change.ID, pageID, string(kind), change.Actor, string(change.ActorKind),
		change.OperatorID, commentID, excerpt, change.TurnID, quiet,
		store.EncodeTime(at.brokerAt), store.EncodeTime(at.brokerAt),
		at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("pages: write the history entry for %s at %s: %w",
			pageID, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// readHead reads one page's current state out of this transaction.
func readHead(ctx context.Context, tx *sql.Tx, id string) (Page, bool, error) {
	var document []byte
	err := tx.QueryRowContext(ctx,
		`SELECT document FROM pages_heads WHERE id = ?`, id).Scan(&document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Page{}, false, nil
	case err != nil:
		return Page{}, false, fmt.Errorf("pages: read the head of %s: %w", id, err)
	}
	head, err := DecodePage(document)
	if err != nil {
		return Page{}, false, fmt.Errorf("pages: decode the head of %s: %w", id, err)
	}
	return head, true, nil
}

// pageIsPurged reports whether a page carries a permanent deletion marker.
func pageIsPurged(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM pages_deletions WHERE page_id = ?`, id).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("pages: read the deletion marker for %s: %w",
			id, err)
	}
	return true, nil
}

// replaceChildSet rewrites one exploded child table for one owner.
//
// Its own rather than [replaceSet]'s, and only until that one goes with the
// projection: this returns the ROW COUNT, which the framework needs as the
// apply transaction's budget input and which an approximation would turn into
// a transaction holding this store's only writer for as long as the
// approximation is wrong.
func replaceChildSet(ctx context.Context, tx *sql.Tx, table, owner, column,
	id string, values []string) (int, error) {

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+table+` WHERE `+owner+` = ?`, id); err != nil {
		return 0, fmt.Errorf("pages: clear %s for %s: %w", table, id, err)
	}
	rows := 0
	for _, value := range sorted(values) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+table+` (`+owner+`, `+column+`) VALUES (?, ?)
			 ON CONFLICT DO NOTHING`, id, value); err != nil {
			return 0, fmt.Errorf("pages: write %s for %s: %w", table, id, err)
		}
		rows++
	}
	return rows, nil
}

// sorted is a copy in a deterministic order, which is rule 4's other half: a
// collection written from a map without sorting it lands in a different order
// on every node and the file's checksum diverges.
func sorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// deref is a pointer's value, or the empty string.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// commentID is the comment a patch touched, or the empty string.
func commentID(c *CommentPatch) string {
	if c == nil {
		return ""
	}
	return c.ID
}

// claimTitle writes one container's hold on one address.
//
// ONE STATEMENT FOR BOTH THE CREATE AND THE RENAME, because a claim is one
// thing and two copies of an upsert are two places for the version guard to
// differ.
func claimTitle(ctx context.Context, tx *sql.Tx, at applyContext,
	container, token, title, pageID string) (int, error) {

	res, err := tx.ExecContext(ctx, `
		INSERT INTO pages_titles
			(container, title_token, title_norm, page_id, created_at, version)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (container, title_token) DO UPDATE SET
			title_norm = excluded.title_norm, page_id = excluded.page_id,
			version = excluded.version
		WHERE excluded.version > pages_titles.version`,
		container, token, NormalizeTitle(title), pageID,
		store.EncodeTime(at.brokerAt), at.packed)
	if err != nil {
		return 0, fmt.Errorf("pages: claim %s/%s at %s: %w",
			container, title, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// addressMatches refuses a record whose payload names an address other than
// the one its subject arbitrated.
//
// THE SUBJECT IS THE ADDRESS, and this is what makes that true rather than
// conventional: without it a writer could take one title at the broker — where
// exactly one writer can — and write a different one into every node's row,
// leaving the arbitrated name held by nothing and the written name held by two.
func addressMatches(at applyContext, container, token, claimedContainer,
	claimedTitle string) error {

	if strings.ToUpper(claimedContainer) == container &&
		TitleToken(claimedTitle) == token {
		return nil
	}
	return fmt.Errorf("pages: the record at %s arbitrated the address %s/%s and "+
		"its payload claims %s/%q — the subject IS the address, so a record "+
		"that took one and wrote another would leave the arbitrated name held "+
		"by nothing", at.position, container, token,
		strings.ToUpper(claimedContainer), claimedTitle)
}
