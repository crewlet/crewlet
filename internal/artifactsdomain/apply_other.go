package artifactsdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The records on an asset's subject and on a reader's.

// historyOp is the op a history row of an asset record is filed under, so the
// artifact's history tells an asset's create from the artifact's own.
func historyOp(kind ObjectKind, op OpKind) OpKind {
	if kind == KindAsset {
		return "asset_" + op
	}
	return op
}

// applyAsset adds or removes one asset.
func (a *Applier) applyAsset(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	op := historyOp(KindAsset, c.record.Op)
	artifact, asset, splitErr := SplitAssetID(c.record.Subject.ID)
	if splitErr != nil {
		return a.refuseOnce(ctx, tx, c, c.record.Subject.ID, op, RefusedInvalid, 0,
			HistorySummary{}, nil)
	}
	done, err := applied(ctx, tx, c, artifact)
	if err != nil || done {
		return 0, err
	}
	switch p := payload.(type) {
	case AssetPayload:
		return a.addAsset(ctx, tx, c, artifact, asset, op, p)
	case StatusPayload:
		return a.removeAsset(ctx, tx, c, artifact, asset, op)
	}
	return 0, fmt.Errorf("artifactsdomain: the asset record at %s carries a %T",
		c.position, payload)
}

// addAsset writes one asset. NEVER CARRIED: its object must be named by no
// other row, so a key is named only by the write that uploaded it.
func (a *Applier) addAsset(ctx context.Context, tx *sql.Tx, c applyContext, artifact, asset string,
	op OpKind, p AssetPayload) (int, error) {

	auth := &p.Authority
	summary := HistorySummary{Asset: asset, Name: p.Name, Bytes: p.Object.Size}
	no := func(reason Refusal) (int, error) {
		return refuse(ctx, tx, c, artifact, op, reason, 0, summary, auth)
	}
	head, found, err := readHead(ctx, tx, artifact)
	switch {
	case err != nil:
		return 0, err
	case !found:
		return no(RefusedNotFound)
	case head.status == string(StatusTrashed):
		return no(RefusedTrashed)
	case !stampedLimits(p.Authority) || p.Object.Validate() != nil:
		return no(RefusedInvalid)
	}
	var existing int
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifact_assets WHERE artifact_id = ? AND asset_id = ?`,
		artifact, asset).Scan(&existing); err != nil {
		return 0, fmt.Errorf("artifactsdomain: read asset %s of %s: %w", asset, artifact, err)
	}
	if existing > 0 {
		return no(RefusedExists)
	}
	var named int
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := tx.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM artifact_objects WHERE object = ?) +
		       (SELECT COUNT(*) FROM artifact_assets WHERE object = ?)`,
		p.Object.Key.String(), p.Object.Key.String()).Scan(&named); err != nil {
		return 0, fmt.Errorf("artifactsdomain: read who names object %s: %w", p.Object.Key, err)
	}
	if named > 0 {
		return no(RefusedObjectNamedElsewhere)
	}
	usage, err := readUsage(ctx, tx)
	if err != nil {
		return 0, err
	}
	if usage.bytes+p.Object.Size > p.Authority.Limits.CompanyBytes {
		summary.Limit, summary.Usage, summary.Key =
			p.Authority.Limits.CompanyBytes, usage.bytes, QuotaKeyCompanyBytes
		return no(RefusedQuotaBytes)
	}
	actor := c.record.actor()
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_assets
			(artifact_id, asset_id, name, content_type, object, hash, size,
			 added_by, added_by_kind, added_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		artifact, asset, p.Name, p.ContentType, p.Object.Key.String(),
		string(p.Object.Hash), p.Object.Size, actor.Name, string(actor.Kind), c.at(),
		c.packed); err != nil {
		return 0, fmt.Errorf("artifactsdomain: add asset %s to %s: %w", asset, artifact, err)
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifacts SET bytes_held = bytes_held + ?, updated_by = ?,
			updated_by_kind = ?, updated_at = ?, version = ?
		WHERE id = ?`, p.Object.Size, actor.Name, string(actor.Kind), c.at(), c.packed,
		artifact); err != nil {
		return 0, fmt.Errorf("artifactsdomain: count asset %s against %s: %w", asset, artifact, err)
	}
	rows := 2
	n, err := addUsage(ctx, tx, usageRow{bytes: p.Object.Size, assets: 1})
	if err != nil {
		return 0, err
	}
	rows += n
	if n, err = writeHistory(ctx, tx, c, historyEntry{
		artifact: artifact, op: op, outcome: OutcomeApplied, summary: summary,
	}, auth); err != nil {
		return 0, err
	}
	rows += n
	a.move(c, artifact, MoveAsset, head.epoch, head.head)
	return rows, nil
}

// removeAsset leaves the asset's row as a tombstone naming no object, so its id
// stays held and its history readable, while the collector may reclaim its
// file.
func (a *Applier) removeAsset(ctx context.Context, tx *sql.Tx, c applyContext, artifact, asset string,
	op OpKind) (int, error) {

	summary := HistorySummary{Asset: asset}
	head, found, err := readHead(ctx, tx, artifact)
	if err != nil {
		return 0, err
	}
	if !found {
		return refuse(ctx, tx, c, artifact, op, RefusedNotFound, 0, summary, nil)
	}
	var object sql.NullString
	var size int64
	var name string
	err = tx.QueryRowContext(ctx, `
		SELECT object, size, name FROM artifact_assets WHERE artifact_id = ? AND asset_id = ?`,
		artifact, asset).Scan(&object, &size, &name)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && !object.Valid):
		return refuse(ctx, tx, c, artifact, op, RefusedNotFound, 0, summary, nil)
	case err != nil:
		return 0, fmt.Errorf("artifactsdomain: read asset %s of %s: %w", asset, artifact, err)
	}
	summary.Name, summary.Bytes = name, size
	actor := c.record.actor()
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifact_assets SET object = NULL, removed_by = ?, removed_by_kind = ?,
			removed_at = ?, version = ?
		WHERE artifact_id = ? AND asset_id = ?`, actor.Name, string(actor.Kind), c.at(),
		c.packed, artifact, asset); err != nil {
		return 0, fmt.Errorf("artifactsdomain: remove asset %s of %s: %w", asset, artifact, err)
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifacts SET bytes_held = bytes_held - ?, updated_by = ?,
			updated_by_kind = ?, updated_at = ?, version = ?
		WHERE id = ?`, size, actor.Name, string(actor.Kind), c.at(), c.packed,
		artifact); err != nil {
		return 0, fmt.Errorf("artifactsdomain: uncount asset %s of %s: %w", asset, artifact, err)
	}
	rows := 2
	n, err := addUsage(ctx, tx, usageRow{bytes: -size, assets: -1})
	if err != nil {
		return 0, err
	}
	rows += n
	if n, err = writeHistory(ctx, tx, c, historyEntry{
		artifact: artifact, op: op, outcome: OutcomeApplied, summary: summary,
	}, nil); err != nil {
		return 0, err
	}
	rows += n
	a.move(c, artifact, MoveAsset, head.epoch, head.head)
	return rows, nil
}

// applyReader writes one person's own state about one artifact.
//
// A PIN, AN UNPIN AND AN OPENED MARK WRITE NO HISTORY: they are one person's
// browsing, and the artifact's history is what happened to the artifact. Each
// is a total function of the record and the row, so a redelivery writes the
// same row again. A record about an artifact this log holds no row of (purged
// between the decide and the apply) writes nothing at all: there is nothing to
// pin.
//
// AN ELEVATED READ IS HISTORY (D09): the artifact's owner and editors see that
// the admin path opened it.
func (a *Applier) applyReader(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	p, ok := payload.(ReaderPayload)
	if !ok {
		return 0, fmt.Errorf("artifactsdomain: the reader record at %s carries a %T",
			c.position, payload)
	}
	seat := c.record.Subject.ID
	if c.record.Op == OpUnpin {
		res, err := tx.ExecContext(ctx, //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			`DELETE FROM artifact_pins WHERE seat = ? AND artifact_id = ?`, seat, p.Artifact)
		if err != nil {
			return 0, fmt.Errorf("artifactsdomain: unpin %s for %s: %w", p.Artifact, seat, err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	_, found, err := readHead(ctx, tx, p.Artifact)
	if err != nil || !found {
		return 0, err
	}
	if c.record.Op == OpReadElevated {
		done, doneErr := applied(ctx, tx, c, p.Artifact)
		if doneErr != nil || done {
			return 0, doneErr
		}
		return writeHistory(ctx, tx, c, historyEntry{
			artifact: p.Artifact, op: OpReadElevated, outcome: OutcomeApplied, number: p.Version,
		}, nil)
	}
	var res sql.Result
	switch c.record.Op {
	case OpPin:
		res, err = tx.ExecContext(ctx, `
			INSERT INTO artifact_pins (seat, artifact_id, pinned_at) VALUES (?, ?, ?)
			ON CONFLICT (seat, artifact_id) DO NOTHING`, seat, p.Artifact, c.at())
	case OpOpened:
		// MONOTONE ON BROKER TIME: the later instant wins whatever order two
		// marks are applied in, so a redelivered older mark moves nothing.
		res, err = tx.ExecContext(ctx, `
			INSERT INTO artifact_reads (seat, artifact_id, opened_at) VALUES (?, ?, ?)
			ON CONFLICT (seat, artifact_id) DO UPDATE SET
				opened_at = MAX(artifact_reads.opened_at, excluded.opened_at)`,
			seat, p.Artifact, c.at())
	default:
		return 0, fmt.Errorf("artifactsdomain: %s is not an op a reader record carries",
			c.record.Op)
	}
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: apply %s of %s for %s: %w",
			c.record.Op, p.Artifact, seat, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
