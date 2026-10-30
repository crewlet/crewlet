package artifactsdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/store"
)

// THE APPLIER'S HALF OF THE ADOPTED BRAND (brand.go has the writer's, the
// reader's and the reason): an adoption replaces the snapshot and prunes what
// nothing pins any more, a clear withdraws it, and a framed version's pin is
// checked against the rows at its own position.

// pinnedBrand is the stylesheet a framed version is stored as pinning: the one
// its record names while an adoption still serves it, else (an adoption pruned
// it between the decide and this apply) the stylesheet adopted now, or none.
// A record naming a stylesheet names [BrandTerm] in its scope, so every node
// reads these rows at the same point of the log.
func pinnedBrand(ctx context.Context, tx *sql.Tx, named string) (string, error) {
	if named == "" {
		return "", nil
	}
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifact_brand_files WHERE css_sha = ? AND slot = ?`,
		named, string(brandformat.SlotCSS)).Scan(&n); err != nil {
		return "", fmt.Errorf("artifactsdomain: read whether stylesheet %s is served: %w", named, err)
	}
	if n > 0 {
		return named, nil
	}
	return currentCSSSha(ctx, tx)
}

// ---- the applier's brand records ------------------------------------------ //

// applyBrand applies one record on the brand subject.
func (a *Applier) applyBrand(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	var n int
	if read := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifact_brand_history WHERE position = ?`, c.packed).Scan(&n); read != nil {
		return 0, fmt.Errorf("artifactsdomain: read whether the brand record at %s applied: %w",
			c.position, read)
	}
	if n > 0 {
		return 0, nil
	}
	var rows int
	switch p := payload.(type) {
	case BrandAdoptPayload:
		rows, err = a.applyAdopt(ctx, tx, c, p)
	case BrandClearPayload:
		rows, err = a.applyClear(ctx, tx, c, p)
	default:
		return 0, fmt.Errorf("artifactsdomain: the brand record at %s carries a %T", c.position, payload)
	}
	if err != nil {
		return 0, err
	}
	pruned, err := pruneBrandHistory(ctx, tx, c)
	if err != nil {
		return 0, err
	}
	return rows + pruned, nil
}

// brandHistory is one brand history row, before it is written.
type brandHistory struct {
	op         OpKind
	outcome    Outcome
	reason     Refusal
	kit        string
	kitVersion int
	summary    BrandHistorySummary
}

// writeBrandHistory writes the record's brand history row.
func writeBrandHistory(ctx context.Context, tx *sql.Tx, c applyContext, h brandHistory) (int, error) {
	summary, err := json.Marshal(h.summary)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: encode the brand history summary at %s: %w", c.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_brand_history
			(position, op_id, op, outcome, reason, kit_id, kit_version, actor, actor_kind,
			 operator_id, at, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (position) DO NOTHING`,
		c.packed, c.record.OpID, string(h.op), string(h.outcome), string(h.reason), h.kit,
		h.kitVersion, c.record.Actor, string(c.record.ActorKind), c.record.OperatorID, c.at(),
		string(summary))
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: write the brand history row at %s: %w", c.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyAdopt replaces the adopted brand with the record's snapshot.
//
// IT REFUSES ONLY WHAT CAN MOVE BETWEEN THE DECIDE AND THE APPLY: the kit's
// version retired or purged (`version_not_found`), the kit trashed. Every
// other rule (the format, the contrast, the files) is the writer's, decided
// from the record's own bytes, which no later record changes.
func (a *Applier) applyAdopt(ctx context.Context, tx *sql.Tx, c applyContext, p BrandAdoptPayload) (int, error) {
	refuse := func(reason Refusal) (int, error) {
		return writeBrandHistory(ctx, tx, c, brandHistory{op: OpAdopt, outcome: OutcomeRefused,
			reason: reason, kit: p.Kit, kitVersion: p.KitVersion})
	}
	head, found, err := readHead(ctx, tx, p.Kit)
	switch {
	case err != nil:
		return 0, err
	case !found:
		return refuse(RefusedVersionNotFound)
	case head.status == string(StatusTrashed):
		return refuse(RefusedTrashed)
	}
	// THE VERSION'S AUTHOR is who wrote the brand, the voice included: copied
	// into the adoption beside its adopter, so the voice is never fenced as
	// words the administrator wrote.
	var label, author, authorKind string
	err = tx.QueryRowContext(ctx, `
		SELECT label, author, author_kind FROM artifact_versions WHERE artifact_id = ? AND number = ?`,
		p.Kit, p.KitVersion).Scan(&label, &author, &authorKind)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return refuse(RefusedVersionNotFound)
	case err != nil:
		return 0, fmt.Errorf("artifactsdomain: read kit %s version %d: %w", p.Kit, p.KitVersion, err)
	}
	if len(p.Files) == 0 || p.Files[0].Slot != string(brandformat.SlotCSS) ||
		string(p.Files[0].Object.Hash) != p.CSSSha {
		return refuse(RefusedInvalid)
	}
	actor := c.record.actor()
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO artifact_brand
			(key, kit_id, kit_version, brand_json, css_sha, adopted_by, adopted_by_kind,
			 operator_id, adopted_at, position, version, kit_author, kit_author_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			kit_id = excluded.kit_id, kit_version = excluded.kit_version,
			brand_json = excluded.brand_json, css_sha = excluded.css_sha,
			adopted_by = excluded.adopted_by, adopted_by_kind = excluded.adopted_by_kind,
			operator_id = excluded.operator_id, adopted_at = excluded.adopted_at,
			position = excluded.position, version = excluded.version,
			kit_author = excluded.kit_author, kit_author_kind = excluded.kit_author_kind
		WHERE excluded.version > artifact_brand.version`,
		brandKey, p.Kit, p.KitVersion, p.BrandJSON, p.CSSSha, actor.Name, string(actor.Kind),
		actor.OperatorID, c.at(), c.packed, c.packed, author, authorKind); err != nil {
		return 0, fmt.Errorf("artifactsdomain: adopt kit %s version %d at %s: %w", p.Kit,
			p.KitVersion, c.position, err)
	}
	rows := 1
	for _, f := range p.Files {
		// THE SAME STYLESHEET ADOPTED AGAIN names the copies its own record
		// uploaded: the row is the latest adoption's, and the earlier
		// copies, now named by nothing, are the collector's.
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO artifact_brand_files
				(css_sha, slot, content_type, object, hash, size, position)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (css_sha, slot) DO UPDATE SET
				content_type = excluded.content_type, object = excluded.object,
				hash = excluded.hash, size = excluded.size, position = excluded.position`,
			p.CSSSha, f.Slot, f.ContentType, f.Object.Key.String(), string(f.Object.Hash),
			f.Object.Size, c.packed); err != nil {
			return 0, fmt.Errorf("artifactsdomain: record the brand's %s at %s: %w", f.Slot,
				c.position, err)
		}
		rows++
	}
	pruned, err := pruneBrandFiles(ctx, tx, p.CSSSha)
	if err != nil {
		return 0, err
	}
	rows += pruned
	set := ""
	if p.Label != "" && label == "" {
		n, ok, labelErr := a.labelAdopted(ctx, tx, c, p, head)
		if labelErr != nil {
			return 0, labelErr
		}
		if ok {
			set = p.Label
		}
		rows += n
	}
	n, err := writeBrandHistory(ctx, tx, c, brandHistory{op: OpAdopt, outcome: OutcomeApplied,
		kit: p.Kit, kitVersion: p.KitVersion,
		summary: BrandHistorySummary{CSSSha: p.CSSSha, Files: len(p.Files), Label: set}})
	return rows + n, err
}

// labelAdopted sets the adoption's label on the adopted version, where it is
// still free and the kit may keep another label, with a history row on the kit
// saying who labelled it. A label taken since the decide is left off: the
// snapshot does not depend on it.
func (a *Applier) labelAdopted(ctx context.Context, tx *sql.Tx, c applyContext, p BrandAdoptPayload,
	head headRow) (int, bool, error) {

	holder, err := labelHolder(ctx, tx, p.Kit, p.Label)
	if err != nil || holder != 0 {
		return 0, false, err
	}
	count, err := labelled(ctx, tx, p.Kit)
	if err != nil || count >= artifacts.MaxLabels {
		return 0, false, err
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE artifact_versions SET label = ? WHERE artifact_id = ? AND number = ?`,
		p.Label, p.Kit, p.KitVersion); err != nil {
		return 0, false, fmt.Errorf("artifactsdomain: label kit %s version %d: %w", p.Kit, p.KitVersion, err)
	}
	authority := p.Authority
	n, err := writeHistory(ctx, tx, c, historyEntry{
		artifact: p.Kit, op: OpPatch, outcome: OutcomeApplied, number: p.KitVersion,
		summary: HistorySummary{Label: p.Label, Fields: []string{"label"}},
	}, &authority)
	if err != nil {
		return 0, false, err
	}
	a.move(c, p.Kit, MoveMeta, head.epoch, head.head)
	return n + 1, true, nil
}

// applyClear withdraws the adopted brand.
func (a *Applier) applyClear(ctx context.Context, tx *sql.Tx, c applyContext, _ BrandClearPayload) (int, error) {
	var kit string
	var kitVersion int
	err := tx.QueryRowContext(ctx, `SELECT kit_id, kit_version FROM artifact_brand WHERE key = ?`,
		brandKey).Scan(&kit, &kitVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("artifactsdomain: read the adopted brand at %s: %w", c.position, err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM artifact_brand WHERE key = ?`, brandKey)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: clear the brand at %s: %w", c.position, err)
	}
	rows, _ := res.RowsAffected()
	pruned, err := pruneBrandFiles(ctx, tx, "")
	if err != nil {
		return 0, err
	}
	n, err := writeBrandHistory(ctx, tx, c, brandHistory{op: OpClear, outcome: OutcomeApplied,
		kit: kit, kitVersion: kitVersion})
	return int(rows) + pruned + n, err
}

// pruneBrandFiles removes the file rows of every adoption but the current one
// (current, empty after a clear) that no retained version still names, so
// their objects become the collector's. A version naming a stylesheet keeps
// every file of that adoption served for as long as it is retained.
func pruneBrandFiles(ctx context.Context, tx *sql.Tx, current string) (int, error) {
	res, err := tx.ExecContext(ctx, `
		DELETE FROM artifact_brand_files
		WHERE css_sha != ?
		  AND NOT EXISTS (SELECT 1 FROM artifact_versions v WHERE v.brand = artifact_brand_files.css_sha)`,
		current)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: prune the files of superseded adoptions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// pruneBrandHistory removes brand history rows past [artifacts.HistoryRetention]
// of broker time, in the bounded batch every history prune takes.
func pruneBrandHistory(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	if c.brokerAt.IsZero() {
		return 0, nil
	}
	cutoff := store.EncodeTime(c.brokerAt.Add(-artifacts.HistoryRetention))
	res, err := tx.ExecContext(ctx, `
		DELETE FROM artifact_brand_history WHERE position IN (
			SELECT position FROM artifact_brand_history WHERE at < ?
			ORDER BY at, position LIMIT ?)`, cutoff, historyPruneBatch)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: prune the brand history: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
