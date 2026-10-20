package artifactsdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/store"
)

// The folder tree's records, and an artifact's move.
//
// EVERY RULE HERE IS DECIDED FROM THE RECORD AND THIS LOG'S ROWS ALONE: a
// depth, a children or a company bound is the one STAMPED in the record, never
// this build's constant, so a later build that raises one cannot part the
// fleet on a record at the boundary. The writer decided each from its
// snapshot of the one tree subject, so for a well-behaved writer none of these
// refusals fires; they are here so that a writer's mistake is a refusal every
// node agrees on rather than rows that differ.
//
// A RECORD ON AN ARTIFACT SUBJECT THAT READS A FOLDER (a create into one, a
// move) is not arbitrated against the tree, so the folder it names may have
// been removed between its decide and its apply: it is refused `folder_gone`
// here, at its position, on every node alike, and its writer told the folder
// is no longer there.

// applyFolder applies one record on the tree subject.
func (a *Applier) applyFolder(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	switch p := payload.(type) {
	case FolderCreatePayload:
		return a.applyFolderCreate(ctx, tx, c, p)
	case FolderRenamePayload:
		return a.applyFolderRename(ctx, tx, c, p)
	case FolderMovePayload:
		return a.applyFolderMove(ctx, tx, c, p)
	case FolderRemovePayload:
		return a.applyFolderRemove(ctx, tx, c, p)
	}
	return 0, fmt.Errorf("artifactsdomain: the folder record at %s carries a %T for op %s",
		c.position, payload, c.record.Op)
}

// folderApplied reports whether the record at c already wrote its history row
// under folder: it has applied here, and a redelivery writes nothing.
func folderApplied(ctx context.Context, tx *sql.Tx, c applyContext, folder string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifact_folder_history WHERE folder_id = ? AND position = ?`,
		folder, c.packed).Scan(&n); err != nil {
		return false, fmt.Errorf("artifactsdomain: read whether the folder record at %s "+
			"applied: %w", c.position, err)
	}
	return n > 0, nil
}

// writeFolderHistory writes the record's folder history row, filed under
// folder, with the actor the record names and the authority it was stamped
// with. The summary names folder ids and counts, never a name.
func writeFolderHistory(ctx context.Context, tx *sql.Tx, c applyContext, folder string, op OpKind,
	outcome Outcome, reason Refusal, summary FolderHistorySummary, authority artifacts.Authority) (int, error) {

	body, err := json.Marshal(summary)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: encode the folder history summary at %s: %w",
			c.position, err)
	}
	elevated := 0
	if authority.Elevated {
		elevated = 1
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_folder_history
			(folder_id, position, op_id, op, outcome, reason, actor, actor_kind, operator_id,
			 elevated, origin, at, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (folder_id, position) DO NOTHING`,
		folder, c.packed, c.record.OpID, string(op), string(outcome), string(reason),
		c.record.Actor, string(c.record.ActorKind), c.record.OperatorID, elevated,
		string(authority.Origin), c.at(), string(body))
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: write the folder history row at %s: %w", c.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// refuseFolder applies a folder record as a refusal: its history row, and
// nothing else.
func refuseFolder(ctx context.Context, tx *sql.Tx, c applyContext, folder string, op OpKind,
	reason Refusal, summary FolderHistorySummary, authority artifacts.Authority) (int, error) {

	return writeFolderHistory(ctx, tx, c, folder, op, OutcomeRefused, reason, summary, authority)
}

// applyFolderCreate writes a chain of new folders, and a new top-level
// folder's shares, in one transaction.
func (a *Applier) applyFolderCreate(ctx context.Context, tx *sql.Tx, c applyContext,
	p FolderCreatePayload) (int, error) {

	if len(p.Folders) == 0 {
		// A CREATE OF NOTHING files its refusal under no folder: the log
		// carries it, and no writer forms it.
		return refuseFolder(ctx, tx, c, "", OpCreate, RefusedInvalid, FolderHistorySummary{}, p.Authority)
	}
	first := p.Folders[0]
	done, err := folderApplied(ctx, tx, c, first.ID)
	if err != nil || done {
		return 0, err
	}
	ids := make([]string, len(p.Folders))
	for i, f := range p.Folders {
		ids[i] = f.ID
	}
	summary := FolderHistorySummary{Parent: first.Parent, Created: ids}
	no := func(reason Refusal) (int, error) {
		return refuseFolder(ctx, tx, c, first.ID, OpCreate, reason, summary, p.Authority)
	}
	if !wellFormedCreate(p) {
		return no(RefusedInvalid)
	}
	tree, err := readLiveTree(ctx, tx)
	if err != nil {
		return 0, err
	}
	for _, f := range p.Folders {
		exists, existsErr := folderRowExists(ctx, tx, f.ID)
		if existsErr != nil {
			return 0, existsErr
		}
		if exists {
			return no(RefusedExists)
		}
	}
	root := first.ID
	if first.Parent != "" {
		parent := tree.live(first.Parent)
		if parent == nil {
			return no(RefusedFolderGone)
		}
		root = parent.root
	}
	switch {
	case tree.depth(first.Parent)+len(p.Folders) > p.MaxDepth:
		return no(RefusedFolderDepth)
	case tree.liveCount()+len(p.Folders) > p.MaxFolders,
		len(tree.kids[first.Parent])+1 > p.MaxChildren:
		return no(RefusedFolderLimit)
	case tree.named(first.Parent, namespaceOf(p.Owner, p.Shares), first.NameKey, "") != nil:
		return no(RefusedFolderNameTaken)
	}
	actor := c.record.actor()
	rows := 0
	for _, f := range p.Folders {
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO artifact_folders
				(id, parent, root, name, name_key, owner, owner_kind, max_depth, status,
				 created_by, created_by_kind, created_at, updated_by, updated_by_kind,
				 updated_at, version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, f.Parent, root, f.Name, f.NameKey, p.Owner, string(p.OwnerKind), p.MaxDepth,
			string(FolderLive), actor.Name, string(actor.Kind), c.at(), actor.Name,
			string(actor.Kind), c.at(), c.packed); err != nil {
			return 0, fmt.Errorf("artifactsdomain: create folder %s at %s: %w", f.ID, c.position, err)
		}
		rows++
	}
	n, err := writeFolderShares(ctx, tx, c, first.ID, p.Shares)
	if err != nil {
		return 0, err
	}
	rows += n
	if n, err = writeFolderHistory(ctx, tx, c, first.ID, OpCreate, OutcomeApplied, "", summary,
		p.Authority); err != nil {
		return 0, err
	}
	return rows + n, nil
}

// wellFormedCreate reports a create its writer could have formed: a chain of
// parsed ids, each after the first inside the one before it, named, owned,
// with every bound stamped, and shares only on a new top-level folder.
func wellFormedCreate(p FolderCreatePayload) bool {
	if p.Owner == "" || !p.OwnerKind.Valid() || p.MaxDepth <= 0 || p.MaxFolders <= 0 ||
		p.MaxChildren <= 0 || (len(p.Shares) > 0 && p.Folders[0].Parent != "") {
		return false
	}
	for i, f := range p.Folders {
		if _, err := uuid.Parse(f.ID); err != nil || f.Name == "" || f.NameKey == "" {
			return false
		}
		if i > 0 && f.Parent != p.Folders[i-1].ID {
			return false
		}
	}
	for _, s := range p.Shares {
		if s.Audience.Validate() != nil || s.Role.Rank() == 0 {
			return false
		}
	}
	return true
}

// folderRowExists reports whether any row, live or a tombstone, holds id: a
// removed folder's id is never brought back.
func folderRowExists(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_folders WHERE id = ?`, id).
		Scan(&n); err != nil {
		return false, fmt.Errorf("artifactsdomain: read folder %s: %w", id, err)
	}
	return n > 0, nil
}

// writeFolderShares writes a top-level folder's shares, each at the record's
// broker instant (a share's own instant is the writer's clock, and never
// written).
func writeFolderShares(ctx context.Context, tx *sql.Tx, c applyContext, folder string,
	shares []artifacts.Share) (int, error) {

	for _, s := range shares {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO artifact_folder_shares
				(folder_id, audience_kind, audience_id, role, granted_by, granted_by_kind, granted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (folder_id, audience_kind, audience_id) DO UPDATE SET
				role = excluded.role, granted_by = excluded.granted_by,
				granted_by_kind = excluded.granted_by_kind, granted_at = excluded.granted_at`,
			folder, string(s.Audience.Kind), s.Audience.ID, string(s.Role), s.GrantedBy.Name,
			actorKindOf(s.GrantedBy), c.at()); err != nil {
			return 0, fmt.Errorf("artifactsdomain: share folder %s at %s: %w", folder, c.position, err)
		}
	}
	return len(shares), nil
}

// dropFolderShares removes a folder's shares: it is no longer a top-level
// folder, or no longer live.
func dropFolderShares(ctx context.Context, tx *sql.Tx, folder string) (int, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM artifact_folder_shares WHERE folder_id = ?`, folder)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: drop folder %s's shares: %w", folder, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// touchFolder stamps a folder's row written by the record at c.
func touchFolder(ctx context.Context, tx *sql.Tx, c applyContext, folder, set string, args ...any) error {
	actor := c.record.actor()
	if set != "" {
		set += ", "
	}
	_, err := tx.ExecContext(ctx, `UPDATE artifact_folders SET `+set+
		`updated_by = ?, updated_by_kind = ?, updated_at = ?, version = ? WHERE id = ?`,
		append(args, actor.Name, string(actor.Kind), c.at(), c.packed, folder)...)
	if err != nil {
		return fmt.Errorf("artifactsdomain: write folder %s at %s: %w", folder, c.position, err)
	}
	return nil
}

// applyFolderRename renames one folder.
func (a *Applier) applyFolderRename(ctx context.Context, tx *sql.Tx, c applyContext,
	p FolderRenamePayload) (int, error) {

	done, err := folderApplied(ctx, tx, c, p.Folder)
	if err != nil || done {
		return 0, err
	}
	no := func(reason Refusal) (int, error) {
		return refuseFolder(ctx, tx, c, p.Folder, OpRename, reason, FolderHistorySummary{}, p.Authority)
	}
	if p.Name == "" || p.NameKey == "" {
		return no(RefusedInvalid)
	}
	tree, err := readLiveTree(ctx, tx)
	if err != nil {
		return 0, err
	}
	f := tree.live(p.Folder)
	switch {
	case f == nil:
		return no(RefusedFolderGone)
	case tree.named(f.parent, namespaceOf(f.owner, tree.shares[f.id]), p.NameKey, f.id) != nil:
		return no(RefusedFolderNameTaken)
	}
	if err = touchFolder(ctx, tx, c, f.id, `name = ?, name_key = ?`, p.Name, p.NameKey); err != nil {
		return 0, err
	}
	n, err := writeFolderHistory(ctx, tx, c, f.id, OpRename, OutcomeApplied, "",
		FolderHistorySummary{}, p.Authority)
	return 1 + n, err
}

// applyFolderMove moves one folder, and everything beneath it, under another
// parent: the folder's place, its subtree's top-level folder, and the shares a
// top-level folder holds (taken from one moved under a folder, given to one
// moved to the top level).
func (a *Applier) applyFolderMove(ctx context.Context, tx *sql.Tx, c applyContext,
	p FolderMovePayload) (int, error) {

	done, err := folderApplied(ctx, tx, c, p.Folder)
	if err != nil || done {
		return 0, err
	}
	tree, err := readLiveTree(ctx, tx)
	if err != nil {
		return 0, err
	}
	f := tree.live(p.Folder)
	summary := FolderHistorySummary{Parent: p.Parent}
	if f != nil {
		summary.From = f.parent
	}
	no := func(reason Refusal) (int, error) {
		return refuseFolder(ctx, tx, c, p.Folder, OpMove, reason, summary, p.Authority)
	}
	if p.MaxDepth <= 0 || p.MaxChildren <= 0 || (p.Parent != "" && len(p.Shares) > 0) {
		return no(RefusedInvalid)
	}
	if f == nil {
		return no(RefusedFolderGone)
	}
	root := p.Folder
	if p.Parent != "" {
		parent := tree.live(p.Parent)
		switch {
		case parent == nil:
			return no(RefusedFolderGone)
		case tree.within(p.Parent, f.id):
			return no(RefusedFolderCycle)
		}
		root = parent.root
	}
	switch {
	case f.parent == p.Parent || p.Root != root:
		return no(RefusedInvalid)
	case tree.depth(p.Parent)+tree.height(f.id) > p.MaxDepth:
		return no(RefusedFolderDepth)
	case len(tree.kids[p.Parent])+1 > p.MaxChildren:
		return no(RefusedFolderLimit)
	case tree.named(p.Parent, namespaceOf(f.owner, p.Shares), f.nameKey, f.id) != nil:
		return no(RefusedFolderNameTaken)
	}
	var rows, n int
	if f.parent == "" {
		// IT WAS A TOP-LEVEL FOLDER: its new top-level folder decides it now.
		if n, err = dropFolderShares(ctx, tx, f.id); err != nil {
			return 0, err
		}
		rows += n
	}
	if n, err = writeFolderShares(ctx, tx, c, f.id, p.Shares); err != nil {
		return 0, err
	}
	rows += n
	if err = touchFolder(ctx, tx, c, f.id, `parent = ?, root = ?`, p.Parent, root); err != nil {
		return 0, err
	}
	rows++
	if root != f.root {
		if n, err = reroot(ctx, tx, c, tree.subtree(f.id)[1:], root); err != nil {
			return 0, err
		}
		rows += n
	}
	if n, err = writeFolderHistory(ctx, tx, c, f.id, OpMove, OutcomeApplied, "", summary,
		p.Authority); err != nil {
		return 0, err
	}
	return rows + n, nil
}

// reroot files every folder of ids under the top-level folder root, the record
// at c writing each row. Only a row's place in a tree changes, so its
// `updated_at` is left alone.
func reroot(ctx context.Context, tx *sql.Tx, c applyContext, ids []string, root string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE artifact_folders SET root = ?, version = ?
		WHERE id IN (SELECT value FROM json_each(?))`, root, c.packed, jsonIDs(ids))
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: re-root %d folders under %s at %s: %w", len(ids),
			root, c.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyFolderRemove removes one folder: its child folders (tombstones
// included) and every artifact filed in it, live and trashed, move up to its
// parent, and its row becomes a tombstone. A child moved up to the top level
// becomes a top-level folder holding the removed folder's shares, owned by
// its own owner: everybody the shares reached keeps seeing it, and the old
// top-level folder's owner does too unless the child is somebody else's in a
// tree with no company share, which only an administrator's elevated move
// produces (ADR-0047 says why no share is granted on their behalf).
//
// ONE HISTORY ROW, WITH THE COUNTS, and no row per artifact: an unbounded
// folder would turn one person's gesture into as many history writes inside
// the one transaction every other record waits behind. The artifacts' own
// histories say nothing about it, because nothing about any of them changed
// but where it is filed.
func (a *Applier) applyFolderRemove(ctx context.Context, tx *sql.Tx, c applyContext,
	p FolderRemovePayload) (int, error) {

	done, err := folderApplied(ctx, tx, c, p.Folder)
	if err != nil || done {
		return 0, err
	}
	summary := FolderHistorySummary{Parent: p.Parent}
	no := func(reason Refusal) (int, error) {
		return refuseFolder(ctx, tx, c, p.Folder, OpRemove, reason, summary, p.Authority)
	}
	tree, err := readLiveTree(ctx, tx)
	if err != nil {
		return 0, err
	}
	f := tree.live(p.Folder)
	switch {
	case f == nil:
		return no(RefusedFolderGone)
	case f.parent != p.Parent:
		return no(RefusedInvalid)
	case len(movedUpCollisions(tree, f)) > 0:
		return no(RefusedFolderNameTaken)
	}
	var rows, n int
	kids := tree.kids[f.id]
	if f.parent == "" {
		// EACH CHILD BECOMES A TOP-LEVEL FOLDER keeping the removed one's
		// audience, and roots its own subtree.
		shares := tree.shares[f.id]
		for _, kid := range kids {
			if n, err = writeFolderShares(ctx, tx, c, kid.id, shares); err != nil {
				return 0, err
			}
			rows += n
			if n, err = reroot(ctx, tx, c, tree.subtree(kid.id), kid.id); err != nil {
				return 0, err
			}
			rows += n
		}
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE artifact_folders SET parent = ?, version = ? WHERE parent = ?`,
		f.parent, c.packed, f.id)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: move folder %s's children up at %s: %w", f.id,
			c.position, err)
	}
	moved, _ := res.RowsAffected()
	rows += int(moved)
	res, err = tx.ExecContext(ctx, `
		UPDATE artifacts SET folder_id = ?, version = ? WHERE folder_id = ?`,
		f.parent, c.packed, f.id)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: move folder %s's artifacts up at %s: %w", f.id,
			c.position, err)
	}
	refiled, _ := res.RowsAffected()
	rows += int(refiled)
	if n, err = dropFolderShares(ctx, tx, f.id); err != nil {
		return 0, err
	}
	rows += n
	actor := c.record.actor()
	if err = touchFolder(ctx, tx, c, f.id,
		`status = ?, removed_by = ?, removed_by_kind = ?, removed_at = ?`,
		string(FolderRemoved), actor.Name, string(actor.Kind), c.at()); err != nil {
		return 0, err
	}
	rows++
	summary.Folders, summary.Artifacts = len(kids), int(refiled)
	if n, err = writeFolderHistory(ctx, tx, c, f.id, OpRemove, OutcomeApplied, "", summary,
		p.Authority); err != nil {
		return 0, err
	}
	return rows + n, nil
}

// applyMove files one artifact in another folder: its `folder_id`, and a
// history row naming the folders by id. Its `updated_at` is left alone (a move
// is not an edit) and nothing about its access changes.
func (a *Applier) applyMove(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p MovePayload) (int, error) {

	done, err := applied(ctx, tx, c, id)
	if err != nil || done {
		return 0, err
	}
	auth := &p.Authority
	head, found, err := readHead(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	from, err := artifactFolder(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	summary := HistorySummary{FromFolder: from, ToFolder: p.To}
	no := func(reason Refusal) (int, error) {
		return refuse(ctx, tx, c, id, OpMove, reason, 0, summary, auth)
	}
	switch {
	case !found:
		return no(RefusedNotFound)
	case head.status == string(StatusTrashed):
		return no(RefusedTrashed)
	}
	if p.To != "" {
		live, liveErr := folderLive(ctx, tx, p.To)
		if liveErr != nil {
			return 0, liveErr
		}
		if !live {
			return no(RefusedFolderGone)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE artifacts SET folder_id = ?, version = ? WHERE id = ?`,
		p.To, c.packed, id); err != nil {
		return 0, fmt.Errorf("artifactsdomain: file %s in folder %q at %s: %w", id, p.To, c.position, err)
	}
	n, err := writeHistory(ctx, tx, c, historyEntry{
		artifact: id, op: OpMove, outcome: OutcomeApplied, summary: summary,
	}, auth)
	if err != nil {
		return 0, err
	}
	a.move(c, id, MoveFolder, head.epoch, head.head)
	return 1 + n, nil
}

// artifactFolder is the folder an artifact is filed in now, ” at the top
// level or for an artifact with no row.
func artifactFolder(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var folder string
	err := tx.QueryRowContext(ctx, `SELECT folder_id FROM artifacts WHERE id = ?`, id).Scan(&folder)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("artifactsdomain: read where %s is filed: %w", id, err)
	}
	return folder, nil
}

// pruneFolderHistory removes folder history rows past
// [artifacts.HistoryRetention] of broker time, the artifact history's term and
// batch, in one total order, so every node removes the same rows at the same
// position.
func pruneFolderHistory(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	if c.brokerAt.IsZero() {
		return 0, nil
	}
	cutoff := store.EncodeTime(c.brokerAt.Add(-artifacts.HistoryRetention))
	rows, err := tx.QueryContext(ctx, `
		SELECT folder_id, position FROM artifact_folder_history WHERE at < ?
		ORDER BY at, folder_id, position LIMIT ?`, cutoff, historyPruneBatch)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: read the folder history past its term: %w", err)
	}
	type key struct {
		folder   string
		position int64
	}
	var expired []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.folder, &k.position); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("artifactsdomain: read the folder history past its term: %w", err)
		}
		expired = append(expired, k)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("artifactsdomain: read the folder history past its term: %w", err)
	}
	for _, k := range expired {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM artifact_folder_history WHERE folder_id = ? AND position = ?`,
			k.folder, k.position); err != nil {
			return 0, fmt.Errorf("artifactsdomain: prune the folder history: %w", err)
		}
	}
	return len(expired), nil
}
