package gitlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The applies of an import's and a pull mirror's records ([ImportChunk],
// [ImportDone], [MirrorSync]), each pure in the record, its position and its
// instant. The decide held the repository importing at the chunk's turn, or
// active and following a source at the sync's epoch, and every command's old
// value; every guard is a skip (apply.go's head, rule 3): a record finding the
// repository otherwise applies as nothing, logged (code_remote_not_applied),
// and only a redelivered record the ledger would have collapsed does.

// remoteState is what an import's or a mirror's apply reads of its
// repository's row: its status, how many of its import's chunks have landed,
// and whether it follows a source.
type remoteState struct {
	status    RepoStatus
	importSeq int64
	mirrored  bool
	found     bool
}

// remoteStateIn reads repoID's remoteState.
func remoteStateIn(ctx context.Context, tx *sql.Tx, at applyContext, repoID string) (remoteState, error) {
	var s remoteState
	var status string
	switch err := tx.QueryRowContext(ctx, remoteStateSQL, repoID).Scan(&status, &s.importSeq, &s.mirrored); {
	case errors.Is(err, sql.ErrNoRows):
		return s, nil
	case err != nil:
		return s, fmt.Errorf("gitlet: read repository %s at %s: %w", repoID, at.position, err)
	}
	s.status, s.found = RepoStatus(status), true
	return s, nil
}

// applyImportChunk lands one chunk of an import: the repository's import_seq
// moved to its Seq first, on the row the push's counters share, then its push
// through [Applier.landPushes]: its packs kind import, its push row `via
// import` by the engine's own account, a drop for every command that leaves an
// object, and the horizons with what the record's rows leave them.
func (a *Applier) applyImportChunk(ctx context.Context, tx *sql.Tx, at applyContext, c ImportChunk) (int, error) {
	s, err := remoteStateIn(ctx, tx, at, c.RepoID)
	if err != nil {
		return 0, err
	}
	switch {
	case !s.found || s.status != RepoStatusImporting:
		return remoteNotApplied(ctx, at, c.RepoID, "the repository is not being imported")
	case s.importSeq != c.Seq-1:
		return remoteNotApplied(ctx, at, c.RepoID, fmt.Sprintf("its import has landed %d chunks, and this "+
			"is chunk %d", s.importSeq, c.Seq))
	}
	if _, err = tx.ExecContext(ctx, importSeqSQL, c.Seq, c.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: move %s's import to chunk %d at %s: %w", c.RepoID, c.Seq, at.position, err)
	}
	entry := PushEntry{PushID: c.ChunkID, Principal: Principal{Kind: PrincipalSystem}, Via: ViaImport,
		Node: c.Node, Commands: c.Commands, ReceivedAt: at.brokerAt}
	pushed, err := a.landPushes(ctx, tx, at, PushBatch{RepoID: c.RepoID, Pushes: []PushEntry{entry}},
		recordPacks{packs: c.Packs, kind: PackImport}, 1)
	if err != nil {
		return 0, err
	}
	return 1 + pushed, nil
}

// applyImportDone ends an import in one write of the repository's row: active,
// imported_at the record's instant, import_error its Error and the default
// branch it names, if any. A pull mirror's import that landed whole also moves
// mirror_synced_at to that instant, since it is level with its source there,
// so `mirrors_due` lists it after MirrorInterval rather than at once; one that
// ended short keeps zero, so its first sync comes at the next pass.
func (a *Applier) applyImportDone(ctx context.Context, tx *sql.Tx, at applyContext, d ImportDone) (int, error) {
	s, err := remoteStateIn(ctx, tx, at, d.RepoID)
	if err != nil {
		return 0, err
	}
	switch {
	case !s.found || s.status != RepoStatusImporting:
		return remoteNotApplied(ctx, at, d.RepoID, "the repository is not being imported")
	case s.importSeq != d.Seq:
		return remoteNotApplied(ctx, at, d.RepoID, fmt.Sprintf("its import has landed %d chunks, and its "+
			"end names %d", s.importSeq, d.Seq))
	}
	before, err := tallyStandingIn(ctx, tx, d.RepoID)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, importDoneSQL, at.at(), d.Error, d.DefaultBranch, d.DefaultBranch, d.Error,
		at.at(), at.packed, d.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: end %s's import at %s: %w", d.RepoID, at.position, err)
	}
	moved, err := retally(ctx, tx, d.RepoID, before)
	if err != nil {
		return 0, err
	}
	return 1 + moved, nil
}

// applyMirrorSync lands one sync of a pull mirror: the repository's
// mirror_synced_at moved to the record's instant first, then its push, as a
// chunk's, with packs kind mirror and a push row `via mirror`. A sync that
// moves nothing ([MirrorSync.Current]) writes no push, so the repository's
// push listings do not show an empty push every sync.
func (a *Applier) applyMirrorSync(ctx context.Context, tx *sql.Tx, at applyContext, m MirrorSync) (int, error) {
	s, err := remoteStateIn(ctx, tx, at, m.RepoID)
	if err != nil {
		return 0, err
	}
	if !s.found || s.status != RepoStatusActive || !s.mirrored {
		return remoteNotApplied(ctx, at, m.RepoID, "the repository is not an active pull mirror")
	}
	if _, err = tx.ExecContext(ctx, mirrorSyncedSQL, at.at(), at.packed, m.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: record %s's sync at %s: %w", m.RepoID, at.position, err)
	}
	if m.Current() {
		return 1, nil
	}
	entry := PushEntry{PushID: m.SyncID, Principal: Principal{Kind: PrincipalSystem}, Via: ViaMirror,
		Node: m.Node, Commands: m.Commands, ReceivedAt: m.FetchedAt}
	pushed, err := a.landPushes(ctx, tx, at, PushBatch{RepoID: m.RepoID, Pushes: []PushEntry{entry}},
		recordPacks{packs: m.Packs, kind: PackMirror}, 1)
	if err != nil {
		return 0, err
	}
	return 1 + pushed, nil
}

// remoteNotApplied logs an import or mirror record that applies as nothing
// because its repository is not as its decide held it, and answers no rows.
func remoteNotApplied(ctx context.Context, at applyContext, repoID, why string) (int, error) {
	applyLog.WarnContext(ctx, "code_remote_not_applied", "repo", repoID, "op", string(at.record.Op),
		"op_id", at.record.OpID, "position", at.position.String(), "why", why)
	return 0, nil
}

// The statements of an import's and a mirror's apply, each a seek of the
// repository's key, registered in plan_test.go.
const (
	remoteStateSQL = `SELECT status, import_seq, mirror_from != '' FROM code_repos WHERE id = ?`
	importSeqSQL   = `UPDATE code_repos SET import_seq = ? WHERE id = ?`
	// importDoneSQL keeps the default branch where the end names none, and
	// moves a pull mirror's last sync to an import's end that landed whole.
	importDoneSQL = `UPDATE code_repos SET status = 'active', imported_at = ?, import_error = ?,
		default_branch = CASE WHEN ? = '' THEN default_branch ELSE ? END,
		mirror_synced_at = CASE WHEN mirror_from != '' AND ? = '' THEN ? ELSE mirror_synced_at END,
		version = ? WHERE id = ?`
	mirrorSyncedSQL = `UPDATE code_repos SET mirror_synced_at = ?, version = ? WHERE id = ?`
)
