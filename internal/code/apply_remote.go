package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// THE APPLY OF AN IMPORT'S AND A PULL MIRROR'S RECORDS ([ImportChunk],
// [ImportDone], [MirrorSync]), in the one transaction the framework holds a
// record's apply to, PURE IN THE RECORD, ITS POSITION AND ITS INSTANT. The
// decide held the repository importing at the chunk's turn, or active and
// following a source at the sync's epoch, and every command's old value; the
// broker's arbitration on the repository's subject guarantees the record
// applies against the rows that decide read. So every guard here is a SKIP,
// never a refusal, which would stop the log on every node: a record that
// finds the repository otherwise applies as NOTHING, whole, logged
// (code_remote_not_applied) — no correct log carries one but a record the
// ledger would have collapsed, met again.

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
		return s, fmt.Errorf("code: read repository %s at %s: %w", repoID, at.position, err)
	}
	s.status, s.found = RepoStatus(status), true
	return s, nil
}

// applyImportChunk lands one chunk of an import: the repository's import_seq
// moved to its Seq first — the repository's row, which the push's counters
// share — and then ITS PUSH, through the one apply every push takes
// ([Applier.landPushes]): its refs, its packs kind import, its push row `via
// import` by the engine's own account, a drop for every command that leaves
// an object, and the horizons with what the record's rows leave them.
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
	if _, err := tx.ExecContext(ctx, importSeqSQL, c.Seq, c.RepoID); err != nil {
		return 0, fmt.Errorf("code: move %s's import to chunk %d at %s: %w", c.RepoID, c.Seq, at.position, err)
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

// applyImportDone ends an import: the repository active, imported_at the
// record's instant, import_error its Error and, where it names one, its
// default branch moved — one write of the repository's row.
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
	if _, err := tx.ExecContext(ctx, importDoneSQL, at.at(), d.Error, d.DefaultBranch, d.DefaultBranch, at.packed,
		d.RepoID); err != nil {
		return 0, fmt.Errorf("code: end %s's import at %s: %w", d.RepoID, at.position, err)
	}
	return 1, nil
}

// applyMirrorSync lands one sync of a pull mirror: the repository's
// mirror_synced_at moved to the record's instant first, then ITS PUSH, as a
// chunk's — its packs kind mirror, its push row `via mirror`.
func (a *Applier) applyMirrorSync(ctx context.Context, tx *sql.Tx, at applyContext, m MirrorSync) (int, error) {
	s, err := remoteStateIn(ctx, tx, at, m.RepoID)
	if err != nil {
		return 0, err
	}
	if !s.found || s.status != RepoStatusActive || !s.mirrored {
		return remoteNotApplied(ctx, at, m.RepoID, "the repository is not an active pull mirror")
	}
	if _, err := tx.ExecContext(ctx, mirrorSyncedSQL, at.at(), m.RepoID); err != nil {
		return 0, fmt.Errorf("code: record %s's sync at %s: %w", m.RepoID, at.position, err)
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
// because its repository is not as its decide held it, and answers that it
// wrote no row.
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
	// importDoneSQL keeps the default branch an end names none of.
	importDoneSQL = `UPDATE code_repos SET status = 'active', imported_at = ?, import_error = ?,
		default_branch = CASE WHEN ? = '' THEN default_branch ELSE ? END, version = ? WHERE id = ?`
	mirrorSyncedSQL = `UPDATE code_repos SET mirror_synced_at = ? WHERE id = ?`
)
