package artifactsdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/objstore"
)

// The artifact's own records: the create, the versions, the head's fields, the
// shares, ownership, the trash stamp and the purge.

// headRow is the artifact row as an apply reads it.
type headRow struct {
	kind, status, owner, ownerKind, sourceKey string
	head                                      int
	epoch, bytesHeld                          int64

	// purgeBegun is the broker instant the artifact's purge began, 0 for
	// one no purge has begun on.
	purgeBegun int64
}

// Status is whether an artifact is served.
type Status string

const (
	// StatusLive is an artifact that is served to those it is shared with.
	StatusLive Status = "live"

	// StatusTrashed is an artifact stamped trashed: its rows stay, its
	// source address stays held, and serving stops at once.
	StatusTrashed Status = "trashed"
)

// readHead reads one artifact's row.
func readHead(ctx context.Context, tx *sql.Tx, id string) (headRow, bool, error) {
	var h headRow
	err := tx.QueryRowContext(ctx, `
		SELECT kind, status, owner, owner_kind, source_key, head_number,
		       access_epoch, bytes_held, purge_begun_at
		FROM artifacts WHERE id = ?`, id).Scan(&h.kind, &h.status, &h.owner,
		&h.ownerKind, &h.sourceKey, &h.head, &h.epoch, &h.bytesHeld, &h.purgeBegun)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return headRow{}, false, nil
	case err != nil:
		return headRow{}, false, fmt.Errorf("artifactsdomain: read artifact %s: %w", id, err)
	}
	return h, true, nil
}

// applyArtifact dispatches an artifact-subject record on its op.
func (a *Applier) applyArtifact(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	id := c.record.Subject.ID
	switch p := payload.(type) {
	case CreatePayload:
		if p.Artifact != id {
			return a.refuseOnce(ctx, tx, c, id, OpCreate, RefusedInvalid, 0,
				HistorySummary{}, &p.Authority)
		}
		return a.applyCreate(ctx, tx, c, id, p, false)
	case PublishPayload:
		return a.applyPublish(ctx, tx, c, id, p)
	case PatchPayload:
		return a.applyPatch(ctx, tx, c, id, p)
	case SharePayload:
		return a.applyShare(ctx, tx, c, id, p)
	case UnsharePayload:
		return a.applyUnshare(ctx, tx, c, id, p)
	case TransferPayload:
		return a.applyTransfer(ctx, tx, c, id, p)
	case LinkPayload:
		return a.applyLinkWorkItem(ctx, tx, c, id, p)
	case MovePayload:
		return a.applyMove(ctx, tx, c, id, p)
	case StatusPayload:
		switch c.record.Op {
		case OpTrash:
			return a.applyTrash(ctx, tx, c, id)
		case OpRestore:
			return a.applyRestore(ctx, tx, c, id)
		case OpPurgeBegin:
			return a.applyPurgeBegin(ctx, tx, c, id)
		case OpPurge:
			return a.applyPurge(ctx, tx, c, id)
		}
	}
	return 0, fmt.Errorf("artifactsdomain: the artifact record at %s carries a %T "+
		"for op %s", c.position, payload, c.record.Op)
}

// applySource is a create on a republish-in-place address.
func (a *Applier) applySource(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	p, ok := payload.(CreatePayload)
	if !ok {
		return 0, fmt.Errorf("artifactsdomain: the source record at %s carries a %T",
			c.position, payload)
	}
	// THE HISTORY ROW IS FILED UNDER THE ARTIFACT THE RECORD NAMES, so its
	// writer reads its verdict back where it looks; a record naming none is
	// filed under its subject.
	id := p.Artifact
	if id == "" {
		id = c.record.Subject.ID
	}
	// THE ADDRESS IS RECOMPUTED FROM WHAT THE PAYLOAD CLAIMS, and a record
	// that arbitrated on one address while claiming another is refused: the
	// broker arbitrated the subject, so the subject is the address it won.
	if p.SourceKey == "" || SourceSubject(p.Owner, p.SourceKey) != c.record.Subject {
		return a.refuseOnce(ctx, tx, c, id, OpCreate, RefusedInvalid, 0,
			HistorySummary{}, &p.Authority)
	}
	return a.applyCreate(ctx, tx, c, id, p, true)
}

// refuseOnce refuses a record unless it has already applied here.
func (a *Applier) refuseOnce(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	op OpKind, reason Refusal, number int, summary HistorySummary,
	authority *artifacts.Authority) (int, error) {

	done, err := applied(ctx, tx, c, id)
	if err != nil || done {
		return 0, err
	}
	return refuse(ctx, tx, c, id, op, reason, number, summary, authority)
}

// applyCreate writes an artifact, its first version, its objects, its shares,
// its source claim where it has one, the counters and its history row, in one
// transaction.
func (a *Applier) applyCreate(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p CreatePayload, sourced bool) (int, error) {

	done, err := applied(ctx, tx, c, id)
	if err != nil || done {
		return 0, err
	}
	auth := &p.Authority
	no := func(reason Refusal, summary HistorySummary) (int, error) {
		return refuse(ctx, tx, c, id, OpCreate, reason, 1, summary, auth)
	}
	parsed, err := artifacts.ParseID(id)
	if err != nil {
		return no(RefusedInvalid, HistorySummary{})
	}
	if _, marked, err := deletionMarker(ctx, tx, id); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		return 0, err
	} else if marked {
		return no(RefusedPurged, HistorySummary{})
	}
	if _, found, err := readHead(ctx, tx, id); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		return 0, err
	} else if found {
		return no(RefusedExists, HistorySummary{})
	}
	var owner, token string
	if sourced {
		owner, token = p.Owner, SourceToken(p.SourceKey)
		holder, held, err := sourceHolder(ctx, tx, owner, token) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return 0, err
		}
		if held && holder != id {
			return no(RefusedSourceTaken, HistorySummary{})
		}
	}
	v := p.Version
	if !stampedLimits(p.Authority) || v.Number != 1 || v.Base != 0 || len(v.Prune) > 0 ||
		p.Owner == "" || !p.Kind.Valid() {
		return no(RefusedInvalid, HistorySummary{})
	}
	// NOTHING IS CARRIED INTO A CREATE: the artifact holds no object yet, so
	// a carried entry names an object the carry rule cannot find here.
	var carried []string
	for _, e := range v.Manifest.Files {
		if e.Carried {
			carried = append(carried, e.Path)
		}
	}
	if len(carried) > 0 {
		return no(RefusedCarriedObjectGone, HistorySummary{Paths: carried})
	}
	if p.Folder != "" {
		// THE FOLDER WAS LIVE WHEN THE WRITER DECIDED, on another subject:
		// a removal that landed since files nothing into it.
		live, liveErr := folderLive(ctx, tx, p.Folder)
		if liveErr != nil {
			return 0, liveErr
		}
		if !live {
			return no(RefusedFolderGone, HistorySummary{ToFolder: p.Folder})
		}
	}
	plan, refusal, err := planObjects(ctx, tx, id, v.Manifest.Files, nil)
	if err != nil {
		return 0, err
	}
	if refusal != nil {
		return no(refusal.reason, HistorySummary{Paths: refusal.paths})
	}
	usage, err := readUsage(ctx, tx)
	if err != nil {
		return 0, err
	}
	if usage.artifacts+1 > p.Authority.Limits.Artifacts {
		return no(RefusedQuotaArtifacts, HistorySummary{
			Limit: p.Authority.Limits.Artifacts, Usage: usage.artifacts, Key: QuotaKeyArtifacts,
		})
	}
	if plan.added > 0 && usage.bytes+plan.added > p.Authority.Limits.CompanyBytes {
		return no(RefusedQuotaBytes, HistorySummary{
			Limit: p.Authority.Limits.CompanyBytes, Usage: usage.bytes, Key: QuotaKeyCompanyBytes,
		})
	}

	capabilities, err := encodeCapabilities(v.Capabilities)
	if err != nil {
		return 0, err
	}
	actor := c.record.actor()
	rows := 0
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifacts
			(id, label, kind, title, description, icon, owner, owner_kind,
			 source_key, head_number, contract, capabilities, status,
			 access_epoch, bytes_held, search_text, created_by, created_by_kind,
			 created_at, updated_by, updated_by_kind, updated_at, version, folder_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, parsed.Label().String(), string(p.Kind), p.Title, p.Description, p.Icon,
		p.Owner, string(p.OwnerKind), p.SourceKey, v.Contract, capabilities,
		string(StatusLive), plan.added, v.SearchText, actor.Name, string(actor.Kind),
		c.at(), actor.Name, string(actor.Kind), c.at(), c.packed, p.Folder); err != nil {
		return 0, fmt.Errorf("artifactsdomain: create artifact %s at %s: %w", id, c.position, err)
	}
	rows++
	if sourced {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO artifact_source_claims (owner, token, source_key, artifact_id, version)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (owner, token) DO NOTHING`,
			owner, token, p.SourceKey, id, c.packed); err != nil {
			return 0, fmt.Errorf("artifactsdomain: claim the source of %s: %w", id, err)
		}
		rows++
	}
	n, err := writeVersion(ctx, tx, c, id, v, capabilities)
	if err != nil {
		return 0, err
	}
	rows += n
	if n, err = applyObjects(ctx, tx, id, 1, plan); err != nil {
		return 0, err
	}
	rows += n
	for _, s := range p.Shares {
		if n, err = upsertShare(ctx, tx, c, id, s); err != nil {
			return 0, err
		}
		rows += n
	}
	if n, err = addUsage(ctx, tx, usageRow{artifacts: 1, bytes: plan.added, versions: 1}); err != nil {
		return 0, err
	}
	rows += n
	if n, err = writeHistory(ctx, tx, c, historyEntry{
		artifact: id, op: OpCreate, outcome: OutcomeApplied, number: 1,
		summary: HistorySummary{
			Files: len(v.Manifest.Files), Bytes: manifest.Bytes(v.Manifest), Label: v.Label,
		},
	}, auth); err != nil {
		return 0, err
	}
	rows += n
	a.move(c, id, MoveVersion, 0, 1)
	return rows, nil
}

// applyPublish adds a version: a publish, or a rollback whose every entry is
// carried.
func (a *Applier) applyPublish(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p PublishPayload) (int, error) {

	done, err := applied(ctx, tx, c, id)
	if err != nil || done {
		return 0, err
	}
	op, v, auth := c.record.Op, p.Version, &p.Authority
	no := func(reason Refusal, summary HistorySummary) (int, error) {
		return refuse(ctx, tx, c, id, op, reason, v.Number, summary, auth)
	}
	head, found, err := readHead(ctx, tx, id)
	switch {
	case err != nil:
		return 0, err
	case !found:
		return no(RefusedNotFound, HistorySummary{})
	case head.status == string(StatusTrashed):
		return no(RefusedTrashed, HistorySummary{})
	case v.Number != head.head+1:
		return no(RefusedHeadMoved, HistorySummary{Head: head.head})
	case !stampedLimits(p.Authority) || v.Base != head.head:
		return no(RefusedInvalid, HistorySummary{})
	}
	if v.Label != "" {
		taken, err := labelHolder(ctx, tx, id, v.Label) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return 0, err
		}
		if taken != 0 {
			return no(RefusedLabelTaken, HistorySummary{Label: v.Label})
		}
	}
	retired, ok, err := retiredEntries(ctx, tx, id, v.Prune, v.Number, nil)
	if err != nil {
		return 0, err
	}
	if !ok {
		return no(RefusedInvalid, HistorySummary{Pruned: sortedInts(v.Prune)})
	}
	plan, refusal, err := planObjects(ctx, tx, id, v.Manifest.Files, retired)
	if err != nil {
		return 0, err
	}
	if refusal != nil {
		return no(refusal.reason, HistorySummary{Paths: refusal.paths})
	}
	delta := plan.added - plan.freed
	usage, err := readUsage(ctx, tx)
	if err != nil {
		return 0, err
	}
	if delta > 0 && usage.bytes+delta > p.Authority.Limits.CompanyBytes {
		return no(RefusedQuotaBytes, HistorySummary{
			Limit: p.Authority.Limits.CompanyBytes, Usage: usage.bytes, Key: QuotaKeyCompanyBytes,
		})
	}

	capabilities, err := encodeCapabilities(v.Capabilities)
	if err != nil {
		return 0, err
	}
	rows, err := writeVersion(ctx, tx, c, id, v, capabilities)
	if err != nil {
		return 0, err
	}
	n, err := applyObjects(ctx, tx, id, v.Number, plan)
	if err != nil {
		return 0, err
	}
	rows += n
	pruned := sortedInts(v.Prune)
	if n, err = deleteVersions(ctx, tx, id, pruned); err != nil {
		return 0, err
	}
	rows += n
	actor := c.record.actor()
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifacts SET head_number = ?, contract = ?, capabilities = ?,
			search_text = ?, bytes_held = bytes_held + ?, updated_by = ?,
			updated_by_kind = ?, updated_at = ?, version = ?
		WHERE id = ?`, v.Number, v.Contract, capabilities, v.SearchText, delta,
		actor.Name, string(actor.Kind), c.at(), c.packed, id); err != nil {
		return 0, fmt.Errorf("artifactsdomain: move %s's head to %d: %w", id, v.Number, err)
	}
	rows++
	if n, err = addUsage(ctx, tx, usageRow{
		bytes: delta, versions: 1 - int64(len(pruned)),
	}); err != nil {
		return 0, err
	}
	rows += n
	if n, err = writeHistory(ctx, tx, c, historyEntry{
		artifact: id, op: op, outcome: OutcomeApplied, number: v.Number,
		summary: HistorySummary{
			Files: len(v.Manifest.Files), Bytes: manifest.Bytes(v.Manifest),
			Pruned: pruned, Forced: v.Forced, RestoredFrom: v.RestoredFrom, Label: v.Label,
		},
	}, auth); err != nil {
		return 0, err
	}
	rows += n
	a.move(c, id, MoveVersion, head.epoch, v.Number)
	return rows, nil
}

// applyPatch changes the head's displayed fields or one version's label.
func (a *Applier) applyPatch(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p PatchPayload) (int, error) {

	done, err := applied(ctx, tx, c, id)
	if err != nil || done {
		return 0, err
	}
	number := 0
	if p.Label != nil {
		number = p.Label.Number
	}
	no := func(reason Refusal, summary HistorySummary) (int, error) {
		return refuse(ctx, tx, c, id, OpPatch, reason, number, summary, nil)
	}
	head, found, err := readHead(ctx, tx, id)
	switch {
	case err != nil:
		return 0, err
	case !found:
		return no(RefusedNotFound, HistorySummary{})
	case head.status == string(StatusTrashed):
		return no(RefusedTrashed, HistorySummary{})
	}
	var fields []string
	var unlabelled map[int]bool
	if p.Label != nil {
		held, err := versionExists(ctx, tx, id, p.Label.Number) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return 0, err
		}
		if !held {
			return no(RefusedVersionNotFound, HistorySummary{})
		}
		if p.Label.Label != "" {
			taken, err := labelHolder(ctx, tx, id, p.Label.Label)
			if err != nil {
				return 0, err
			}
			if taken != 0 && taken != p.Label.Number {
				return no(RefusedLabelTaken, HistorySummary{Label: p.Label.Label})
			}
		} else {
			// A LABEL'S REMOVAL makes its version an unlabelled one this
			// same record may retire.
			unlabelled = map[int]bool{p.Label.Number: true}
		}
		fields = append(fields, "label")
	}
	if slices.Contains(p.Prune, head.head) {
		return no(RefusedInvalid, HistorySummary{Pruned: sortedInts(p.Prune)})
	}
	retired, ok, err := retiredEntries(ctx, tx, id, p.Prune, head.head+1, unlabelled)
	if err != nil {
		return 0, err
	}
	if !ok {
		return no(RefusedInvalid, HistorySummary{Pruned: sortedInts(p.Prune)})
	}
	plan, refusal, err := planObjects(ctx, tx, id, nil, retired)
	if err != nil {
		return 0, err
	}
	if refusal != nil {
		return no(refusal.reason, HistorySummary{Paths: refusal.paths})
	}

	rows := 0
	set := []string{}
	args := []any{}
	for _, f := range []struct {
		name  string
		value *string
	}{{"title", p.Title}, {"description", p.Description}, {"icon", p.Icon}} {
		if f.value != nil {
			set = append(set, f.name+" = ?")
			args = append(args, *f.value)
			fields = append(fields, f.name)
		}
	}
	if p.Label != nil {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if _, err := tx.ExecContext(ctx, `
			UPDATE artifact_versions SET label = ? WHERE artifact_id = ? AND number = ?`,
			p.Label.Label, id, p.Label.Number); err != nil {
			return 0, fmt.Errorf("artifactsdomain: label %s version %d: %w", id, p.Label.Number, err)
		}
		rows++
	}
	pruned := sortedInts(p.Prune)
	n, err := applyObjects(ctx, tx, id, 0, plan)
	if err != nil {
		return 0, err
	}
	rows += n
	if n, err = deleteVersions(ctx, tx, id, pruned); err != nil {
		return 0, err
	}
	rows += n
	actor := c.record.actor()
	set = append(set, "bytes_held = bytes_held - ?", "updated_by = ?", "updated_by_kind = ?",
		"updated_at = ?", "version = ?")
	args = append(args, plan.freed, actor.Name, string(actor.Kind), c.at(), c.packed, id)
	if _, err := tx.ExecContext(ctx, `UPDATE artifacts SET `+strings.Join(set, ", ")+ //nolint:govet // shadow: scoped to this block; see .golangci.yml
		` WHERE id = ?`, args...); err != nil {
		return 0, fmt.Errorf("artifactsdomain: patch artifact %s: %w", id, err)
	}
	rows++
	if n, err = addUsage(ctx, tx, usageRow{
		bytes: -plan.freed, versions: -int64(len(pruned)),
	}); err != nil {
		return 0, err
	}
	rows += n
	summary := HistorySummary{Fields: fields, Pruned: pruned}
	if p.Label != nil {
		summary.Label = p.Label.Label
	}
	if n, err = writeHistory(ctx, tx, c, historyEntry{
		artifact: id, op: OpPatch, outcome: OutcomeApplied, number: number, summary: summary,
	}, nil); err != nil {
		return 0, err
	}
	rows += n
	a.move(c, id, MoveMeta, head.epoch, head.head)
	return rows, nil
}

// applyShare grants one audience a role. A grant that LOWERS a role the
// audience held is a narrowing, and moves the access epoch.
func (a *Applier) applyShare(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p SharePayload) (int, error) {

	done, err := applied(ctx, tx, c, id)
	if err != nil || done {
		return 0, err
	}
	summary := HistorySummary{Audience: p.Share.Audience.String(), Role: string(p.Share.Role)}
	head, found, err := readHead(ctx, tx, id)
	switch {
	case err != nil:
		return 0, err
	case !found:
		return refuse(ctx, tx, c, id, OpShare, RefusedNotFound, 0, summary, nil)
	case head.status == string(StatusTrashed):
		return refuse(ctx, tx, c, id, OpShare, RefusedTrashed, 0, summary, nil)
	case p.Share.Audience.Kind == "" || p.Share.Role == "" || p.Share.Role == artifacts.RoleOwner:
		return refuse(ctx, tx, c, id, OpShare, RefusedInvalid, 0, summary, nil)
	}
	before, held, err := shareRole(ctx, tx, id, p.Share.Audience)
	if err != nil {
		return 0, err
	}
	epoch := head.epoch
	// A LOWERED ROLE IS A NARROWING, by the ladder's own ordering
	// ([artifacts.Role.Rank]); a role this build does not know ranks below
	// every real one, so replacing one is never read as a narrowing.
	if held && p.Share.Role.Rank() < before.Rank() {
		epoch++
	}
	rows, err := upsertShare(ctx, tx, c, id, p.Share)
	if err != nil {
		return 0, err
	}
	n, err := touchHead(ctx, tx, c, id, epoch)
	if err != nil {
		return 0, err
	}
	rows += n
	if n, err = writeHistory(ctx, tx, c, historyEntry{
		artifact: id, op: OpShare, outcome: OutcomeApplied, summary: summary,
	}, nil); err != nil {
		return 0, err
	}
	rows += n
	a.move(c, id, MoveShare, epoch, head.head)
	return rows, nil
}

// applyUnshare withdraws one audience's share, a narrowing whenever there was
// one to withdraw.
func (a *Applier) applyUnshare(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p UnsharePayload) (int, error) {

	done, err := applied(ctx, tx, c, id)
	if err != nil || done {
		return 0, err
	}
	summary := HistorySummary{Audience: p.Audience.String()}
	head, found, err := readHead(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if !found {
		return refuse(ctx, tx, c, id, OpUnshare, RefusedNotFound, 0, summary, nil)
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM artifact_shares
		WHERE artifact_id = ? AND audience_kind = ? AND audience_id = ?`,
		id, string(p.Audience.Kind), p.Audience.ID)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: withdraw %s's share on %s: %w",
			p.Audience, id, err)
	}
	removed, _ := res.RowsAffected()
	epoch := head.epoch
	if removed > 0 {
		epoch++
	}
	rows := int(removed)
	n, err := touchHead(ctx, tx, c, id, epoch)
	if err != nil {
		return 0, err
	}
	rows += n
	if n, err = writeHistory(ctx, tx, c, historyEntry{
		artifact: id, op: OpUnshare, outcome: OutcomeApplied, summary: summary,
	}, nil); err != nil {
		return 0, err
	}
	rows += n
	a.move(c, id, MoveShare, epoch, head.head)
	return rows, nil
}

// applyTransfer moves ownership. The previous owner's source address is
// released, the new owner's own seat share (which ownership now subsumes) is
// dropped, and the access epoch moves: the previous owner may have lost access.
func (a *Applier) applyTransfer(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p TransferPayload) (int, error) {

	done, err := applied(ctx, tx, c, id)
	if err != nil || done {
		return 0, err
	}
	head, found, err := readHead(ctx, tx, id)
	summary := HistorySummary{From: head.owner, To: p.To}
	switch {
	case err != nil:
		return 0, err
	case !found:
		return refuse(ctx, tx, c, id, OpTransfer, RefusedNotFound, 0, summary, nil)
	case head.status == string(StatusTrashed):
		return refuse(ctx, tx, c, id, OpTransfer, RefusedTrashed, 0, summary, nil)
	case p.To == "" || p.ToKind == "" || p.To == head.owner:
		return refuse(ctx, tx, c, id, OpTransfer, RefusedInvalid, 0, summary, nil)
	}
	rows := 0
	res, err := tx.ExecContext(ctx,
		`DELETE FROM artifact_source_claims WHERE artifact_id = ?`, id)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: release %s's source address: %w", id, err)
	}
	released, _ := res.RowsAffected()
	rows += int(released)
	res, err = tx.ExecContext(ctx, `
		DELETE FROM artifact_shares
		WHERE artifact_id = ? AND audience_kind = ? AND audience_id = ?`,
		id, string(artifacts.AudienceSeat), p.To)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: drop the new owner's share on %s: %w", id, err)
	}
	dropped, _ := res.RowsAffected()
	rows += int(dropped)
	actor := c.record.actor()
	epoch := head.epoch + 1
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifacts SET owner = ?, owner_kind = ?, source_key = '',
			access_epoch = ?, updated_by = ?, updated_by_kind = ?, updated_at = ?,
			version = ?
		WHERE id = ?`, p.To, string(p.ToKind), epoch, actor.Name, string(actor.Kind),
		c.at(), c.packed, id); err != nil {
		return 0, fmt.Errorf("artifactsdomain: transfer %s to %s: %w", id, p.To, err)
	}
	rows++
	n, err := writeHistory(ctx, tx, c, historyEntry{
		artifact: id, op: OpTransfer, outcome: OutcomeApplied, summary: summary,
	}, nil)
	if err != nil {
		return 0, err
	}
	rows += n
	a.move(c, id, MoveTransfer, epoch, head.head)
	return rows, nil
}

// applyLinkWorkItem links an artifact to a work item: its history row, filed
// under the item, and nothing else. No artifact row moves, so a link touches
// neither the head's version guard nor its access epoch, and tells nobody a
// movement: the artifact a room holds open is the same artifact.
//
// A link the rows refuse (an artifact gone or trashed since the decide, or a
// record naming no item) writes its refused row WITH the item, so the history
// says what was tried, and a refused row lists the artifact under nothing
// ([ListQuery.WorkItem] reads applied rows alone).
func (a *Applier) applyLinkWorkItem(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p LinkPayload) (int, error) {

	done, err := applied(ctx, tx, c, id)
	if err != nil || done {
		return 0, err
	}
	no := func(reason Refusal) (int, error) {
		return refuse(ctx, tx, c, id, OpLinkWorkItem, reason, 0, HistorySummary{}, &p.Authority)
	}
	head, found, err := readHead(ctx, tx, id)
	switch {
	case err != nil:
		return 0, err
	case !found:
		return no(RefusedNotFound)
	case head.status == string(StatusTrashed):
		return no(RefusedTrashed)
	case strings.TrimSpace(p.Authority.WorkItem) == "":
		return no(RefusedInvalid)
	}
	return writeHistory(ctx, tx, c, historyEntry{
		artifact: id, op: OpLinkWorkItem, outcome: OutcomeApplied,
	}, &p.Authority)
}

// applyTrash stamps an artifact trashed: its rows stay, its source address stays
// held so nobody takes the address of something that may come back, serving
// stops at once and the access epoch moves.
func (a *Applier) applyTrash(ctx context.Context, tx *sql.Tx, c applyContext, id string) (int, error) {
	done, err := applied(ctx, tx, c, id)
	if err != nil || done {
		return 0, err
	}
	head, found, err := readHead(ctx, tx, id)
	switch {
	case err != nil:
		return 0, err
	case !found:
		return refuse(ctx, tx, c, id, OpTrash, RefusedNotFound, 0, HistorySummary{}, nil)
	case head.status == string(StatusTrashed):
		return refuse(ctx, tx, c, id, OpTrash, RefusedTrashed, 0, HistorySummary{}, nil)
	}
	actor := c.record.actor()
	epoch := head.epoch + 1
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifacts SET status = ?, trashed_by = ?, trashed_by_kind = ?,
			trashed_at = ?, access_epoch = ?, updated_by = ?, updated_by_kind = ?,
			updated_at = ?, version = ?
		WHERE id = ?`, string(StatusTrashed), actor.Name, string(actor.Kind), c.at(),
		epoch, actor.Name, string(actor.Kind), c.at(), c.packed, id); err != nil {
		return 0, fmt.Errorf("artifactsdomain: trash %s: %w", id, err)
	}
	n, err := writeHistory(ctx, tx, c, historyEntry{
		artifact: id, op: OpTrash, outcome: OutcomeApplied,
	}, nil)
	if err != nil {
		return 0, err
	}
	a.move(c, id, MoveTrash, epoch, head.head)
	return 1 + n, nil
}

// applyRestore clears the trash stamp. A widening, so the epoch stays.
func (a *Applier) applyRestore(ctx context.Context, tx *sql.Tx, c applyContext, id string) (int, error) {
	done, err := applied(ctx, tx, c, id)
	if err != nil || done {
		return 0, err
	}
	head, found, err := readHead(ctx, tx, id)
	switch {
	case err != nil:
		return 0, err
	case !found:
		return refuse(ctx, tx, c, id, OpRestore, RefusedNotFound, 0, HistorySummary{}, nil)
	case head.status != string(StatusTrashed):
		return refuse(ctx, tx, c, id, OpRestore, RefusedLive, 0, HistorySummary{}, nil)
	case head.purgeBegun > 0:
		return refuse(ctx, tx, c, id, OpRestore, RefusedPurging, 0, HistorySummary{}, nil)
	}
	actor := c.record.actor()
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifacts SET status = ?, trashed_by = '', trashed_by_kind = '',
			trashed_at = 0, updated_by = ?, updated_by_kind = ?, updated_at = ?,
			version = ?
		WHERE id = ?`, string(StatusLive), actor.Name, string(actor.Kind), c.at(),
		c.packed, id); err != nil {
		return 0, fmt.Errorf("artifactsdomain: restore %s: %w", id, err)
	}
	n, err := writeHistory(ctx, tx, c, historyEntry{
		artifact: id, op: OpRestore, outcome: OutcomeApplied,
	}, nil)
	if err != nil {
		return 0, err
	}
	a.move(c, id, MoveRestore, head.epoch, head.head)
	return 1 + n, nil
}

// applyPurgeBegin commits an artifact to its purge: it stamps the instant the
// purge began and, for an artifact not yet in the trash, trashes it in the same
// row (serving stops and the access epoch moves, as a trash's does), so from
// this record on the artifact is never served, never written and never
// restored, and only the purge's own record removes it. The purge runs its
// dependents' irreversible steps only once this has applied.
func (a *Applier) applyPurgeBegin(ctx context.Context, tx *sql.Tx, c applyContext, id string) (int, error) {
	done, err := applied(ctx, tx, c, id)
	if err != nil || done {
		return 0, err
	}
	head, found, err := readHead(ctx, tx, id)
	switch {
	case err != nil:
		return 0, err
	case !found:
		return refuse(ctx, tx, c, id, OpPurgeBegin, RefusedNotFound, 0, HistorySummary{}, nil)
	case head.purgeBegun > 0:
		return refuse(ctx, tx, c, id, OpPurgeBegin, RefusedPurging, 0, HistorySummary{}, nil)
	}
	actor := c.record.actor()
	epoch := head.epoch
	if head.status == string(StatusTrashed) {
		_, err = tx.ExecContext(ctx, `
			UPDATE artifacts SET purge_begun_at = ?, updated_by = ?, updated_by_kind = ?,
				updated_at = ?, version = ?
			WHERE id = ?`, c.at(), actor.Name, string(actor.Kind), c.at(), c.packed, id)
	} else {
		epoch++
		_, err = tx.ExecContext(ctx, `
			UPDATE artifacts SET status = ?, trashed_by = ?, trashed_by_kind = ?,
				trashed_at = ?, access_epoch = ?, purge_begun_at = ?, updated_by = ?,
				updated_by_kind = ?, updated_at = ?, version = ?
			WHERE id = ?`, string(StatusTrashed), actor.Name, string(actor.Kind), c.at(),
			epoch, c.at(), actor.Name, string(actor.Kind), c.at(), c.packed, id)
	}
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: begin the purge of %s: %w", id, err)
	}
	n, err := writeHistory(ctx, tx, c, historyEntry{
		artifact: id, op: OpPurgeBegin, outcome: OutcomeApplied,
	}, nil)
	if err != nil {
		return 0, err
	}
	a.move(c, id, MoveTrash, epoch, head.head)
	return 1 + n, nil
}

// applyPurge removes an artifact permanently and writes its deletion marker.
//
// EVERY ROW OF THE ARTIFACT GOES IN THIS STATEMENT LIST, since the estate keeps
// no foreign key and a cascade is a delete nobody committed: its versions, its
// objects (which become unnamed, and the collector reclaims their files after
// its grace), its assets, shares, pins, reads, source claim and history. The
// MARKER IS WRITTEN WHETHER OR NOT THE ROW IS HERE, so a record about the
// artifact that arrives later is gated on every data node alike. A purge writes no
// history row of its own: the history goes with the artifact, and the marker
// names the purge's operation, which is what its writer reads back.
func (a *Applier) applyPurge(ctx context.Context, tx *sql.Tx, c applyContext, id string) (int, error) {
	head, found, err := readHead(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	actor := c.record.actor()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO artifacts_deletions
			(artifact_id, at, by, by_kind, purge_record_id, rejects, last_reject_at, version)
		VALUES (?, ?, ?, ?, ?, 0, 0, ?)
		ON CONFLICT (artifact_id) DO NOTHING`,
		id, c.at(), actor.Name, string(actor.Kind), c.record.OpID, c.packed)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: mark %s purged: %w", id, err)
	}
	marked, _ := res.RowsAffected()
	rows := int(marked)
	if !found {
		return rows, nil
	}
	var versions, assets int64
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := tx.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM artifact_versions WHERE artifact_id = ?),
		       (SELECT COUNT(*) FROM artifact_assets WHERE artifact_id = ? AND object IS NOT NULL)`,
		id, id).Scan(&versions, &assets); err != nil {
		return 0, fmt.Errorf("artifactsdomain: count what purging %s removes: %w", id, err)
	}
	pinned, err := pinsAnyBrand(ctx, tx, id, nil)
	if err != nil {
		return 0, err
	}
	for _, stmt := range []string{
		`DELETE FROM artifact_versions WHERE artifact_id = ?`,
		`DELETE FROM artifact_objects WHERE artifact_id = ?`,
		`DELETE FROM artifact_assets WHERE artifact_id = ?`,
		`DELETE FROM artifact_shares WHERE artifact_id = ?`,
		`DELETE FROM artifact_pins WHERE artifact_id = ?`,
		`DELETE FROM artifact_reads WHERE artifact_id = ?`,
		`DELETE FROM artifact_source_claims WHERE artifact_id = ?`,
		`DELETE FROM artifact_history WHERE artifact_id = ?`,
		`DELETE FROM artifacts WHERE id = ?`,
	} {
		res, err := tx.ExecContext(ctx, stmt, id) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return 0, fmt.Errorf("artifactsdomain: purge %s: %w", id, err)
		}
		n, _ := res.RowsAffected()
		rows += int(n)
	}
	n, err := addUsage(ctx, tx, usageRow{
		artifacts: -1, bytes: -head.bytesHeld, versions: -versions, assets: -assets,
	})
	if err != nil {
		return 0, err
	}
	rows += n
	if n, err = pruneRetiredBrands(ctx, tx, pinned); err != nil {
		return 0, err
	}
	rows += n
	a.move(c, id, MovePurge, head.epoch+1, head.head)
	return rows, nil
}

// ---- versions ---------------------------------------------------------- //

// writeVersion inserts one version row, its manifest in canonical form.
func writeVersion(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	v VersionPayload, capabilities string) (int, error) {

	encoded, err := manifest.Encode(v.Manifest)
	if err != nil {
		return 0, err
	}
	forced := 0
	if v.Forced {
		forced = 1
	}
	versionID := v.VersionID
	if _, bad := uuid.Parse(versionID); bad != nil {
		versionID = ""
	}
	brand, err := pinnedBrand(ctx, tx, v.Brand)
	if err != nil {
		return 0, err
	}
	actor := c.record.actor()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_versions
			(artifact_id, number, version_id, base_number, restored_from, forced,
			 label, entry, contract, capabilities, files, bytes, manifest, author,
			 author_kind, created_at, position, brand)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, v.Number, versionID, v.Base, v.RestoredFrom, forced, v.Label,
		v.Manifest.Entry, v.Contract, capabilities, len(v.Manifest.Files),
		manifest.Bytes(v.Manifest), encoded, actor.Name, string(actor.Kind), c.at(),
		c.packed, brand); err != nil {
		return 0, fmt.Errorf("artifactsdomain: write %s version %d: %w", id, v.Number, err)
	}
	return 1, nil
}

// encodeCapabilities is a version's declared capabilities as the sorted JSON
// array a row stores.
func encodeCapabilities(caps []artifacts.Capability) (string, error) {
	names := make([]string, 0, len(caps))
	for _, c := range caps {
		names = append(names, string(c))
	}
	slices.Sort(names)
	raw, err := json.Marshal(slices.Compact(names))
	if err != nil {
		return "", fmt.Errorf("artifactsdomain: encode capabilities: %w", err)
	}
	return string(raw), nil
}

// labelHolder is the version of artifact id that holds label, zero for none.
func labelHolder(ctx context.Context, tx *sql.Tx, id, label string) (int, error) {
	var number int
	err := tx.QueryRowContext(ctx, `
		SELECT number FROM artifact_versions WHERE artifact_id = ? AND label = ?
		ORDER BY number LIMIT 1`, id, label).Scan(&number)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("artifactsdomain: read who holds label %q on %s: %w", label, id, err)
	}
	return number, nil
}

// versionExists reports whether artifact id holds version number.
func versionExists(ctx context.Context, tx *sql.Tx, id string, number int) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifact_versions WHERE artifact_id = ? AND number = ?`,
		id, number).Scan(&n); err != nil {
		return false, fmt.Errorf("artifactsdomain: read %s version %d: %w", id, number, err)
	}
	return n > 0, nil
}

// retiredEntries is every manifest entry of the versions a record retires,
// read before they go, or false when the list names a version this artifact
// does not hold, a labelled one, or one at or past below (the version the
// record makes, or the head's successor).
//
// unlabelled names versions the record itself unlabels, which count as
// unlabelled here.
func retiredEntries(ctx context.Context, tx *sql.Tx, id string, prune []int, below int,
	unlabelled map[int]bool) ([]artifacts.Entry, bool, error) {

	var out []artifacts.Entry
	for _, n := range sortedInts(prune) {
		if n <= 0 || n >= below {
			return nil, false, nil
		}
		var label string
		var raw []byte
		err := tx.QueryRowContext(ctx, `
			SELECT label, manifest FROM artifact_versions
			WHERE artifact_id = ? AND number = ?`, id, n).Scan(&label, &raw)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, false, nil
		case err != nil:
			return nil, false, fmt.Errorf("artifactsdomain: read %s version %d to "+
				"retire it: %w", id, n, err)
		}
		if label != "" && !unlabelled[n] {
			return nil, false, nil
		}
		m, err := manifest.Decode(raw)
		if err != nil {
			return nil, false, fmt.Errorf("artifactsdomain: %s version %d: %w", id, n, err)
		}
		out = append(out, m.Files...)
	}
	return out, true, nil
}

// deleteVersions removes the versions a record retires, and with them the files
// of a superseded adoption the last of them pinned ([pruneRetiredBrands]).
func deleteVersions(ctx context.Context, tx *sql.Tx, id string, numbers []int) (int, error) {
	if len(numbers) == 0 {
		return 0, nil
	}
	pinned, err := pinsAnyBrand(ctx, tx, id, numbers)
	if err != nil {
		return 0, err
	}
	rows := 0
	for _, n := range numbers {
		res, err := tx.ExecContext(ctx, //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			`DELETE FROM artifact_versions WHERE artifact_id = ? AND number = ?`, id, n)
		if err != nil {
			return 0, fmt.Errorf("artifactsdomain: retire %s version %d: %w", id, n, err)
		}
		k, _ := res.RowsAffected()
		rows += int(k)
	}
	pruned, err := pruneRetiredBrands(ctx, tx, pinned)
	return rows + pruned, err
}

// pinsAnyBrand reports whether any version of artifact id numbered in numbers
// (every version where numbers is nil) pins an adoption's stylesheet, read
// before the versions are deleted.
func pinsAnyBrand(ctx context.Context, tx *sql.Tx, id string, numbers []int) (bool, error) {
	query := `SELECT EXISTS (SELECT 1 FROM artifact_versions WHERE artifact_id = ? AND brand != ''`
	args := []any{id}
	if numbers != nil {
		query += ` AND number IN (` + strings.TrimSuffix(strings.Repeat("?, ", len(numbers)), ", ") + `)`
		for _, n := range numbers {
			args = append(args, n)
		}
	}
	var pinned bool
	if err := tx.QueryRowContext(ctx, query+`)`, args...).Scan(&pinned); err != nil {
		return false, fmt.Errorf("artifactsdomain: read whether %s's retired versions pin a brand: %w", id, err)
	}
	return pinned, nil
}

// pruneRetiredBrands prunes, after versions that pinned a stylesheet are gone
// (pinned), the files of every superseded adoption no retained version names
// any more ([pruneBrandFiles] against the adoption that stands), so a page
// pinned to an old brand keeps it served while it is retained and not an
// apply longer. A record retiring a pinned version names the brand in its
// scope ([namesBrand]); one whose versions pin nothing touches no brand row.
func pruneRetiredBrands(ctx context.Context, tx *sql.Tx, pinned bool) (int, error) {
	if !pinned {
		return 0, nil
	}
	current, err := currentCSSSha(ctx, tx)
	if err != nil {
		return 0, err
	}
	return pruneBrandFiles(ctx, tx, current)
}

// ---- the reference table ------------------------------------------------ //

// objectChange is one object's reference count before and after a record.
type objectChange struct {
	obj           objstore.Object
	before, after int
}

// objectPlan is how one record moves an artifact's object references.
type objectPlan struct {
	changes []objectChange

	// added and freed are the bytes of objects the record makes named and
	// leaves unnamed: what the artifact's held bytes and the company's
	// usage move by.
	added, freed int64
}

// refusalFacts is a plan the rows refuse.
type refusalFacts struct {
	reason Refusal
	paths  []string
}

// planObjects decides, in this transaction, how adding adds and removing
// removes moves artifact id's object references, refusing a record the carry
// rule forbids.
//
// # The carry rule (ADR-0027, as extended for carrying)
//
// A CARRIED ENTRY IS WRITTEN ONLY WHEN THIS TRANSACTION FINDS ITS OBJECT NAMED
// BY THIS ARTIFACT, with a positive count and the digest and size the entry
// claims; otherwise the record is refused and its writer told which paths to
// upload again. An UPLOADED entry's object must be named by no other artifact
// and no asset: a key is named only by the write that uploaded it, so one named
// elsewhere is a carry across artifacts in disguise. Together these keep the
// collector's invariant without a lock: once no row names a key, no later record
// can name it.
//
// THE CARRY CHECK READS THE ROWS BEFORE THIS RECORD'S OWN REMOVALS, because the
// decide found the object named when it chose to carry it, and an object a
// publish both carries and retires a version of is still named afterwards by
// the new version.
func planObjects(ctx context.Context, tx *sql.Tx, id string, adds, removes []artifacts.Entry) (
	objectPlan, *refusalFacts, error) {

	type held struct {
		refs int
		hash objstore.Hash
		size int64
	}
	current := map[objstore.Key]held{}
	read := func(k objstore.Key) (held, error) {
		if h, ok := current[k]; ok {
			return h, nil
		}
		var h held
		var hash string
		err := tx.QueryRowContext(ctx, `
			SELECT refs, hash, size FROM artifact_objects
			WHERE artifact_id = ? AND object = ?`, id, k.String()).Scan(&h.refs, &hash, &h.size)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return held{}, fmt.Errorf("artifactsdomain: read %s's reference to object "+
				"%s: %w", id, k, err)
		default:
			h.hash = objstore.Hash(hash)
		}
		current[k] = h
		return h, nil
	}

	var gone, elsewhere, invalid []string
	for _, e := range adds {
		h, err := read(e.Object.Key)
		if err != nil {
			return objectPlan{}, nil, err
		}
		if e.Carried {
			if h.refs <= 0 || h.hash != e.Object.Hash || h.size != e.Object.Size {
				gone = append(gone, e.Path)
			}
			continue
		}
		if h.refs > 0 && (h.hash != e.Object.Hash || h.size != e.Object.Size) {
			invalid = append(invalid, e.Path)
			continue
		}
		other, err := namedElsewhere(ctx, tx, id, e.Object.Key)
		if err != nil {
			return objectPlan{}, nil, err
		}
		if other {
			elsewhere = append(elsewhere, e.Path)
		}
	}
	switch {
	case len(gone) > 0:
		return objectPlan{}, &refusalFacts{reason: RefusedCarriedObjectGone, paths: gone}, nil
	case len(elsewhere) > 0:
		return objectPlan{}, &refusalFacts{reason: RefusedObjectNamedElsewhere, paths: elsewhere}, nil
	case len(invalid) > 0:
		return objectPlan{}, &refusalFacts{reason: RefusedInvalid, paths: invalid}, nil
	}

	delta := map[objstore.Key]int{}
	objects := map[objstore.Key]objstore.Object{}
	for _, e := range adds {
		delta[e.Object.Key]++
		objects[e.Object.Key] = e.Object
	}
	for _, e := range removes {
		delta[e.Object.Key]--
		if _, ok := objects[e.Object.Key]; !ok {
			objects[e.Object.Key] = e.Object
		}
	}
	keys := make([]objstore.Key, 0, len(delta))
	for k := range delta {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b objstore.Key) int { return strings.Compare(a.String(), b.String()) })
	var plan objectPlan
	for _, k := range keys {
		h, err := read(k)
		if err != nil {
			return objectPlan{}, nil, err
		}
		after := h.refs + delta[k]
		if after < 0 {
			// A REFERENCE COUNT BELOW ZERO is rows that disagree with the
			// versions they count, which no record can produce: stopping
			// here is the honest answer, and every data node stops on it alike.
			return objectPlan{}, nil, fmt.Errorf("artifactsdomain: %s's object %s "+
				"would be named %d times: the reference table disagrees with the "+
				"versions it counts", id, k, after)
		}
		obj := objects[k]
		if h.refs > 0 {
			obj.Hash, obj.Size = h.hash, h.size
		}
		plan.changes = append(plan.changes, objectChange{obj: obj, before: h.refs, after: after})
		switch {
		case h.refs == 0 && after > 0:
			plan.added += obj.Size
		case h.refs > 0 && after == 0:
			plan.freed += obj.Size
		}
	}
	return plan, nil, nil
}

// namedElsewhere reports whether key is named by another artifact's references
// or by any asset.
func namedElsewhere(ctx context.Context, tx *sql.Tx, id string, key objstore.Key) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM artifact_objects WHERE object = ? AND artifact_id != ?) +
		       (SELECT COUNT(*) FROM artifact_assets WHERE object = ?)`,
		key.String(), id, key.String()).Scan(&n); err != nil {
		return false, fmt.Errorf("artifactsdomain: read who names object %s: %w", key, err)
	}
	return n > 0, nil
}

// applyObjects writes a plan: a row for an object newly named, a count moved
// for one still named, and a delete for one no longer named.
func applyObjects(ctx context.Context, tx *sql.Tx, id string, number int, plan objectPlan) (int, error) {
	rows := 0
	for _, ch := range plan.changes {
		var err error
		key := ch.obj.Key.String()
		switch {
		case ch.before == ch.after:
			continue
		case ch.before == 0:
			_, err = tx.ExecContext(ctx, `
				INSERT INTO artifact_objects (artifact_id, object, hash, size, refs, first_number)
				VALUES (?, ?, ?, ?, ?, ?)`,
				id, key, string(ch.obj.Hash), ch.obj.Size, ch.after, number)
		case ch.after == 0:
			_, err = tx.ExecContext(ctx,
				`DELETE FROM artifact_objects WHERE artifact_id = ? AND object = ?`, id, key)
		default:
			_, err = tx.ExecContext(ctx,
				`UPDATE artifact_objects SET refs = ? WHERE artifact_id = ? AND object = ?`,
				ch.after, id, key)
		}
		if err != nil {
			return 0, fmt.Errorf("artifactsdomain: move %s's reference to object %s: %w",
				id, key, err)
		}
		rows++
	}
	return rows, nil
}

// ---- shares ------------------------------------------------------------ //

// shareRole is the role an audience holds on artifact id, and whether it holds
// one.
func shareRole(ctx context.Context, tx *sql.Tx, id string, a artifacts.Audience) (artifacts.Role, bool, error) {
	var role string
	err := tx.QueryRowContext(ctx, `
		SELECT role FROM artifact_shares
		WHERE artifact_id = ? AND audience_kind = ? AND audience_id = ?`,
		id, string(a.Kind), a.ID).Scan(&role)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("artifactsdomain: read %s's share on %s: %w", a, id, err)
	}
	return artifacts.Role(role), true, nil
}

// upsertShare writes one share. The grant's instant is the broker's; the
// granter is the share's own.
func upsertShare(ctx context.Context, tx *sql.Tx, c applyContext, id string, s artifacts.Share) (int, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_shares
			(artifact_id, audience_kind, audience_id, role, granted_by, granted_by_kind, granted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (artifact_id, audience_kind, audience_id) DO UPDATE SET
			role = excluded.role, granted_by = excluded.granted_by,
			granted_by_kind = excluded.granted_by_kind, granted_at = excluded.granted_at`,
		id, string(s.Audience.Kind), s.Audience.ID, string(s.Role), s.GrantedBy.Name,
		actorKindOf(s.GrantedBy), c.at()); err != nil {
		return 0, fmt.Errorf("artifactsdomain: share %s with %s: %w", id, s.Audience, err)
	}
	return 1, nil
}

// touchHead records a write to the artifact that moved no displayed field: its
// writer, its instant, its version guard and its access epoch.
func touchHead(ctx context.Context, tx *sql.Tx, c applyContext, id string, epoch int64) (int, error) {
	actor := c.record.actor()
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifacts SET access_epoch = ?, updated_by = ?, updated_by_kind = ?,
			updated_at = ?, version = ?
		WHERE id = ?`, epoch, actor.Name, string(actor.Kind), c.at(), c.packed, id); err != nil {
		return 0, fmt.Errorf("artifactsdomain: record a write to %s: %w", id, err)
	}
	return 1, nil
}

// ---- source claims ------------------------------------------------------ //

// sourceHolder is the artifact that holds owner's address token, and whether
// one does.
func sourceHolder(ctx context.Context, tx *sql.Tx, owner, token string) (string, bool, error) {
	var artifact string
	err := tx.QueryRowContext(ctx, `
		SELECT artifact_id FROM artifact_source_claims WHERE owner = ? AND token = ?`,
		owner, token).Scan(&artifact)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("artifactsdomain: read the source address "+
			"%s.%s: %w", owner, token, err)
	}
	return artifact, true, nil
}
