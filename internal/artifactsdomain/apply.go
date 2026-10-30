package artifactsdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier, and the rules that make it a PURE FUNCTION of the log.
//
// N nodes derive one SQL state from one ordered stream, and the identity claim
// says their tables are byte-identical. That holds exactly as long as these do:
//
//  1. IT READS NO CONFIG, NO CLOCK AND NO OTHER DOMAIN. Whatever needs Tier A,
//     IAM or the chart (the quota limits, the caller's role and kind) was
//     decided at the API and STAMPED into the record; every instant written is
//     the broker's ([statelog.ApplyOptions.StoredAt]).
//  2. IT APPLIES NO RULE A LATER BUILD MAY CHANGE. The media table, the path
//     grammar and every bound in internal/artifacts/limits.go are the
//     writer's to enforce: an older node applying a newer node's record under
//     its own older rules would refuse what its peers accept, and the fleet's
//     copies would part. The applier enforces only what its own rows decide
//     (a head that moved, a carried object no longer named, a quota against
//     the stamped limits, a label already taken), and those deterministically.
//  3. A REFUSAL IS A ROW, NEVER AN ERROR. A record the broker accepted that the
//     rows refuse is applied as a no-op that writes its history row with the
//     outcome `refused` and the reason, on every data node alike, and its writer
//     reads its own verdict back by operation id. An error would stall every
//     node's log on a record that is merely too late.
//  4. EVERY RECORD THAT WRITES A HISTORY ROW IS APPLIED ONCE. The row is keyed
//     by the record's position, and an apply that finds its own row already
//     there has applied, so a redelivery writes nothing.

// Outcome is what an apply did with a record, as its history row says.
type Outcome string

const (
	// OutcomeApplied is a record whose rows were written.
	OutcomeApplied Outcome = "applied"

	// OutcomeRefused is a record the rows refused: it wrote only its
	// history row.
	OutcomeRefused Outcome = "refused"
)

// Refusal is why an apply refused a record. A CLOSED SET, because the writer
// translates each into the refusal its caller acts on.
type Refusal string

// The refusals an applier can write.
const (
	// RefusedInvalid is a record its writer should never have formed: an id
	// that does not parse, a source address the payload does not match, a
	// limit stamped as zero, a version list that names the wrong versions.
	RefusedInvalid Refusal = "invalid"

	// RefusedNotFound is a record about an artifact or an asset this log
	// holds no row for.
	RefusedNotFound Refusal = "not_found"

	// RefusedExists is a create of an object that already exists.
	RefusedExists Refusal = "exists"

	// RefusedPurged is a create naming an artifact the deletion marker
	// holds.
	RefusedPurged Refusal = "purged"

	// RefusedTrashed is a write to a trashed artifact, or a second trash.
	RefusedTrashed Refusal = "trashed"

	// RefusedLive is a restore of an artifact that is not trashed.
	RefusedLive Refusal = "live"

	// RefusedPurging is a restore of an artifact whose purge has begun, or a
	// second beginning of one: only the purge finishes it.
	RefusedPurging Refusal = "purging"

	// RefusedHeadMoved is a version that is not head + 1.
	RefusedHeadMoved Refusal = "head_moved"

	// RefusedCarriedObjectGone is a carried entry whose object this
	// artifact no longer names: the paths must be uploaded again.
	RefusedCarriedObjectGone Refusal = "carried_object_gone"

	// RefusedObjectNamedElsewhere is an uploaded entry or an asset naming an
	// object another artifact or asset already names: a carry across
	// artifacts, which is never allowed.
	RefusedObjectNamedElsewhere Refusal = "object_named_elsewhere"

	// RefusedQuotaArtifacts and RefusedQuotaBytes are the stamped quotas.
	RefusedQuotaArtifacts Refusal = "quota_artifacts"
	RefusedQuotaBytes     Refusal = "quota_bytes"

	// RefusedLabelTaken is a label another version of the artifact holds.
	RefusedLabelTaken Refusal = "label_taken"

	// RefusedVersionNotFound is a label for a version this artifact does not
	// hold.
	RefusedVersionNotFound Refusal = "version_not_found"

	// RefusedSourceTaken is a create on a source address another artifact
	// holds.
	RefusedSourceTaken Refusal = "source_taken"

	// RefusedFolderGone is a create into, or a move into or under, a folder
	// that is no longer live (removed since the writer decided), or a
	// folder record naming a folder this log holds no live row for.
	RefusedFolderGone Refusal = "folder_gone"

	// RefusedFolderCycle is a folder move under itself or a folder beneath
	// it.
	RefusedFolderCycle Refusal = "folder_cycle"

	// RefusedFolderDepth is a folder create or move that would place a
	// folder deeper than the depth stamped on the record.
	RefusedFolderDepth Refusal = "folder_depth"

	// RefusedFolderNameTaken is a folder create, rename or move that would
	// give two live siblings one name key.
	RefusedFolderNameTaken Refusal = "folder_name_taken"

	// RefusedFolderLimit is a folder create past the live folders a company
	// holds, or past the children one folder holds.
	RefusedFolderLimit Refusal = "folder_limit"
)

// The folder refusals above are decided by the WRITER from its snapshot of the
// one tree subject, so for a well-behaved writer the apply never writes them:
// it checks them again because a record is applied on every node, and a
// writer's mistake must be a refusal every node agrees on rather than rows
// that differ. An adoption's refusal of an absent version is
// [RefusedVersionNotFound]. A brand's contrast is never an apply's question:
// it is the writer's, refused before the append ([ErrBrandContrast]).

// The Tier A keys a quota refusal names, so the operator reading it knows what
// to raise.
const (
	QuotaKeyArtifacts    = "artifacts.limits.artifacts"
	QuotaKeyCompanyBytes = "artifacts.limits.company_bytes"
)

// HistorySummary is what a history row says beyond its columns.
type HistorySummary struct {
	Files        int      `json:"files,omitempty"`
	Bytes        int64    `json:"bytes,omitempty"`
	Pruned       []int    `json:"pruned,omitempty"`
	Forced       bool     `json:"forced,omitempty"`
	RestoredFrom int      `json:"restored_from,omitempty"`
	Label        string   `json:"label,omitempty"`
	Fields       []string `json:"fields,omitempty"`
	Audience     string   `json:"audience,omitempty"`
	Role         string   `json:"role,omitempty"`
	From         string   `json:"from,omitempty"`
	To           string   `json:"to,omitempty"`
	Asset        string   `json:"asset,omitempty"`
	Name         string   `json:"name,omitempty"`

	// The refusal's own facts: the paths to upload again, the head a moved
	// version found, and the quota it crossed.
	Paths []string `json:"paths,omitempty"`
	Head  int      `json:"head,omitempty"`
	Limit int64    `json:"limit,omitempty"`
	Usage int64    `json:"usage,omitempty"`
	Key   string   `json:"key,omitempty"`

	// FromFolder and ToFolder are a move's folders by id, empty for the top
	// level: never a name, so the row discloses nothing about a folder its
	// reader cannot see.
	FromFolder string `json:"from_folder,omitempty"`
	ToFolder   string `json:"to_folder,omitempty"`
}

// FolderHistorySummary is what a folder history row says beyond its columns:
// folder ids and counts, never a name or an artifact.
type FolderHistorySummary struct {
	// Parent is the parent a create, a move or a removal names, and From
	// the parent a move took the folder from; empty for the top level.
	Parent string `json:"parent,omitempty"`
	From   string `json:"from,omitempty"`

	// Created is the chain a create made, parents first.
	Created []string `json:"created,omitempty"`

	// Folders and Artifacts are what a removal moved up to its parent.
	Folders   int `json:"folders,omitempty"`
	Artifacts int `json:"artifacts,omitempty"`
}

// BrandHistorySummary is what a brand history row says beyond its columns.
type BrandHistorySummary struct {
	// CSSSha is the adopted stylesheet's hash, and Files how many files the
	// adoption serves.
	CSSSha string `json:"css_sha,omitempty"`
	Files  int    `json:"files,omitempty"`

	// Label is the label the adoption set on the kit's version.
	Label string `json:"label,omitempty"`
}

// historyPruneBatch is how many history rows past [artifacts.HistoryRetention]
// one record removes: a bounded batch, so a node returning from months away
// does not delete a year in the transaction holding the store's only writer,
// while a company writing one record a day still retires rows faster than it
// writes them.
const historyPruneBatch = 64

// Applier writes this node's copy of the artifacts estate.
type Applier struct {
	// NodeID is this node's own id.
	NodeID string

	// committed is told, after a committed batch, what it moved. Nil is
	// legal and means nobody is listening.
	committed func([]Moved)

	// moved is filled inside Apply and drained by Committed. NOT guarded by
	// a mutex: an applier is ONE writer, and Apply and Committed run on one
	// goroutine with the commit in between.
	moved []Moved
}

var _ statelog.RetentionHook = (*Applier)(nil)

// NewApplier builds the artifacts applier for one node. committed is called
// after a committed batch that moved anything, AFTER the commit and never
// inside the transaction: the store re-runs the body of an attempt that failed
// transiently, and a listener told about rows that then rolled back would
// decide from rows no node holds.
func NewApplier(nodeID string, committed func([]Moved)) *Applier {
	return &Applier{NodeID: nodeID, committed: committed}
}

// Committed is the post-commit half: what the batch moved, told once. It must
// not block, and neither may the listener.
func (a *Applier) Committed(context.Context) {
	if len(a.moved) == 0 {
		return
	}
	// RESET BEFORE THE CALL, so a move raised while the listener runs is the
	// next batch's rather than one this reset erases.
	moved := a.moved
	a.moved = nil
	if a.committed != nil {
		a.committed(moved)
	}
}

// Retained tells the listeners that a committed batch retained records this
// build cannot read: whatever they are about may have moved with no row here to
// say so, and the hook is handed a count rather than the records, so it names
// everything ([MoveReset]).
func (a *Applier) Retained(context.Context, int) {
	a.moved = append(a.moved, Reset())
}

// move notes one movement for the post-commit half, at the record c is
// applying: its position and the broker's instant for it.
func (a *Applier) move(c applyContext, id string, what MoveKind, epoch int64, head int) {
	parsed, err := artifacts.ParseID(id)
	if err != nil {
		// An id that does not parse names no artifact anybody can open,
		// and every record carrying one was refused before it moved.
		return
	}
	a.moved = append(a.moved, Moved{
		Artifact: parsed, What: what, AccessEpoch: epoch, Head: head, Position: c.position,
		At: c.brokerAt,
	})
}

// Gated reports a record that must produce no rows at all, from this same
// transaction: the EVICTION GATE (a record written by a node the fleet evicted
// before its position) and the DELETION GATE (a record about an artifact a
// purge destroyed, except the purge's own record).
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (
	statelog.Reason, bool, error) {

	if rec.Writer != "" {
		evicted, err := evictedAt(ctx, tx, rec.Writer, rec.Position.Packed())
		if err != nil {
			return "", false, err
		}
		if evicted {
			return statelog.ReasonEvicted, true, nil
		}
	}
	artifact, ok := gatedArtifact(rec.Subject.Kind, rec.Subject.ID)
	if !ok {
		return "", false, nil
	}
	author, marked, err := deletionMarker(ctx, tx, artifact)
	if err != nil || !marked {
		return "", false, err
	}
	// THE ONE EXCEPTION IS THE RECORD THAT WROTE THE MARKER, by its own id:
	// "any purge" would let a second purge through, and by position would
	// fail a republished copy.
	if author == rec.OpID {
		return "", false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifacts_deletions SET rejects = rejects + 1, last_reject_at = ?
		WHERE artifact_id = ?`, store.EncodeTime(rec.StoredAt), artifact); err != nil {
		return "", false, fmt.Errorf("artifactsdomain: count a gate hit on the "+
			"purged artifact %s: %w", artifact, err)
	}
	return statelog.ReasonDeleted, true, nil
}

// evictedAt reports whether a record at packed, written by writer, falls in
// writer's eviction window. THE WINDOW IS HALF-OPEN AT BOTH ENDS: a record at
// or below the eviction's own position was written while the node was counted,
// and one at or above a readmission by a node the fleet took back.
func evictedAt(ctx context.Context, tx *sql.Tx, writer string, packed int64) (bool, error) {
	var from, readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM artifacts_evictions
		WHERE node_id = ?`, writer).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("artifactsdomain: read the eviction gate for node "+
			"%s: %w", writer, err)
	}
	evicted := from.Valid && packed > from.Int64
	back := readmitted.Valid && packed >= readmitted.Int64
	return evicted && !back, nil
}

// deletionMarker is the artifact's deletion marker: the purge record's id, and
// whether there is one.
func deletionMarker(ctx context.Context, tx *sql.Tx, artifact string) (string, bool, error) {
	var author string
	err := tx.QueryRowContext(ctx,
		`SELECT purge_record_id FROM artifacts_deletions WHERE artifact_id = ?`,
		artifact).Scan(&author)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("artifactsdomain: read the deletion gate for "+
			"artifact %s: %w", artifact, err)
	}
	return author, true, nil
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record   MutationRecord
	position statelog.Position

	// packed is the composed position: every row's version guard and every
	// history row's key.
	packed int64

	// brokerAt is the broker's own instant for this record, the ONLY
	// instant this domain writes.
	brokerAt time.Time

	// maxVariables sizes a multi-row insert. It decides how many statements
	// a collection is written in, never which rows land.
	maxVariables int
}

// at is the broker instant as it is stored.
func (c applyContext) at() int64 { return store.EncodeTime(c.brokerAt) }

// Apply writes one record's rows. The dispatch is on the SUBJECT KIND, and
// every kind has a case, the one that writes nothing included.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: decode the record at %s: %w", rec.Position, err)
	}
	c := applyContext{
		record: record, position: rec.Position, packed: rec.Position.Packed(),
		brokerAt: opts.StoredAt, maxVariables: opts.MaxVariables,
	}
	var rows int
	switch record.Subject.Kind {
	case KindBarrier:
		// THE READ INDEX'S OWN APPEND writes no row on any node.
		return 0, nil
	case KindEviction:
		return a.applyEviction(ctx, tx, c)
	case KindGeneration:
		return a.applyGeneration(ctx, tx, c)
	case KindArtifact:
		rows, err = a.applyArtifact(ctx, tx, c)
	case KindSource:
		rows, err = a.applySource(ctx, tx, c)
	case KindAsset:
		rows, err = a.applyAsset(ctx, tx, c)
	case KindReader:
		rows, err = a.applyReader(ctx, tx, c)
	case KindFolder:
		rows, err = a.applyFolder(ctx, tx, c)
	case KindBrand:
		rows, err = a.applyBrand(ctx, tx, c)
	default:
		// A KIND THIS BUILD DOES NOT KNOW REACHES HERE ONLY AT A VERSION IT
		// CAN READ, which is a writer publishing a kind it never declared;
		// failing is what makes that writer's mistake visible.
		return 0, fmt.Errorf("artifactsdomain: %s is not a kind this build "+
			"applies, and the record at %s claims version %d: a kind is declared "+
			"before it is published", record.Subject.Kind, rec.Position, record.V)
	}
	if err != nil {
		return 0, err
	}
	pruned, err := pruneHistory(ctx, tx, c)
	if err != nil {
		return 0, err
	}
	folders, err := pruneFolderHistory(ctx, tx, c)
	if err != nil {
		return 0, err
	}
	return rows + pruned + folders, nil
}

// applyEviction records a node's removal from this log, or its readmission, as
// an INVERSE COMMIT rather than a delete.
func (a *Applier) applyEviction(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	e, ok := payload.(Eviction)
	if !ok || e.NodeID == "" {
		return 0, fmt.Errorf("artifactsdomain: the eviction at %s names no node", c.position)
	}
	var res sql.Result
	if e.Readmitted {
		res, err = tx.ExecContext(ctx, `
			UPDATE artifacts_evictions SET readmitted_position = ?, version = ?
			WHERE node_id = ? AND version < ?`, c.packed, c.packed, e.NodeID, c.packed)
	} else {
		// A SECOND EVICTION CLEARS THE READMISSION, so the evicted,
		// readmitted, evicted sequence reads as three facts.
		res, err = tx.ExecContext(ctx, `
			INSERT INTO artifacts_evictions
				(node_id, at, by, from_position, readmitted_position, version)
			VALUES (?, ?, ?, ?, NULL, ?)
			ON CONFLICT (node_id) DO UPDATE SET
				at = excluded.at, by = excluded.by,
				from_position = excluded.from_position,
				readmitted_position = NULL, version = excluded.version
			WHERE excluded.version > artifacts_evictions.version`,
			e.NodeID, c.at(), e.EvictedBy, c.packed, c.packed)
	}
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: apply the eviction of %s at %s: %w",
			e.NodeID, c.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyGeneration writes a reanchor's audit row, create-only.
func (a *Applier) applyGeneration(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	g, ok := payload.(Generation)
	if !ok {
		return 0, fmt.Errorf("artifactsdomain: the generation record at %s carries "+
			"a %T", c.position, payload)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO artifacts_log_generations
			(generation, at, by, new_stream_created_at, prev_last_seq_seen, record_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (generation) DO NOTHING`,
		g.Generation, c.at(), g.By, store.EncodeTime(g.StreamAt),
		int64(g.PrevHighest), c.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: record generation %d at %s: %w",
			g.Generation, c.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---- the history row every artifact record writes ----------------------- //

// historyEntry is one history row, before it is written.
type historyEntry struct {
	artifact string
	op       OpKind
	outcome  Outcome
	reason   Refusal
	number   int
	summary  HistorySummary
}

// applied reports whether the record at c already wrote its history row on
// artifact: it has applied here, and a redelivery writes nothing.
func applied(ctx context.Context, tx *sql.Tx, c applyContext, artifact string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifact_history WHERE artifact_id = ? AND position = ?`,
		artifact, c.packed).Scan(&n); err != nil {
		return false, fmt.Errorf("artifactsdomain: read whether the record at %s "+
			"applied: %w", c.position, err)
	}
	return n > 0, nil
}

// writeHistory writes the record's history row, with the actor the record
// names and the authority it was stamped with where it carries one.
func writeHistory(ctx context.Context, tx *sql.Tx, c applyContext, h historyEntry,
	authority *artifacts.Authority) (int, error) {

	summary, err := json.Marshal(h.summary)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: encode the history summary at %s: %w",
			c.position, err)
	}
	var elevated int
	var origin, workItem string
	if authority != nil {
		if authority.Elevated {
			elevated = 1
		}
		origin, workItem = string(authority.Origin), authority.WorkItem
	}
	if h.op == OpReadElevated {
		elevated = 1
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_history
			(artifact_id, position, op_id, op, outcome, reason, number, actor,
			 actor_kind, operator_id, elevated, origin, work_item, at, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (artifact_id, position) DO NOTHING`,
		h.artifact, c.packed, c.record.OpID, string(h.op), string(h.outcome),
		string(h.reason), h.number, c.record.Actor, string(c.record.ActorKind),
		c.record.OperatorID, elevated, origin, workItem, c.at(), string(summary))
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: write the history row at %s: %w",
			c.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// refuse applies the record as a refusal: its history row, and nothing else.
func refuse(ctx context.Context, tx *sql.Tx, c applyContext, artifact string, op OpKind,
	reason Refusal, number int, summary HistorySummary, authority *artifacts.Authority) (int, error) {

	return writeHistory(ctx, tx, c, historyEntry{
		artifact: artifact, op: op, outcome: OutcomeRefused, reason: reason,
		number: number, summary: summary,
	}, authority)
}

// pruneHistory removes history rows past [artifacts.HistoryRetention] of broker
// time, a bounded batch per record in one total order, so every data node removes the
// same rows at the same position.
func pruneHistory(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	if c.brokerAt.IsZero() {
		return 0, nil
	}
	cutoff := store.EncodeTime(c.brokerAt.Add(-artifacts.HistoryRetention))
	rows, err := tx.QueryContext(ctx, `
		SELECT artifact_id, position FROM artifact_history WHERE at < ?
		ORDER BY at, artifact_id, position LIMIT ?`, cutoff, historyPruneBatch)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: read the history past its term: %w", err)
	}
	type key struct {
		artifact string
		position int64
	}
	var expired []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.artifact, &k.position); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("artifactsdomain: read the history past its term: %w", err)
		}
		expired = append(expired, k)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("artifactsdomain: read the history past its term: %w", err)
	}
	for _, k := range expired {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM artifact_history WHERE artifact_id = ? AND position = ?`,
			k.artifact, k.position); err != nil {
			return 0, fmt.Errorf("artifactsdomain: prune the history: %w", err)
		}
	}
	return len(expired), nil
}

// ---- the company's counters -------------------------------------------- //

// usageScope is the one row the counters live in.
const usageScope = "company"

// usageRow is the counters the stamped quotas are enforced against.
type usageRow struct {
	artifacts, bytes, versions, assets int64
}

// readUsage reads the counters, zero before the first record.
func readUsage(ctx context.Context, tx *sql.Tx) (usageRow, error) {
	var u usageRow
	err := tx.QueryRowContext(ctx, `
		SELECT artifacts, bytes, versions, assets FROM artifact_usage WHERE scope = ?`,
		usageScope).Scan(&u.artifacts, &u.bytes, &u.versions, &u.assets)
	if errors.Is(err, sql.ErrNoRows) {
		return usageRow{}, nil
	}
	if err != nil {
		return usageRow{}, fmt.Errorf("artifactsdomain: read the usage counters: %w", err)
	}
	return u, nil
}

// addUsage moves the counters by the deltas given.
func addUsage(ctx context.Context, tx *sql.Tx, d usageRow) (int, error) {
	if d == (usageRow{}) {
		return 0, nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_usage (scope, artifacts, bytes, versions, assets)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (scope) DO UPDATE SET
			artifacts = artifact_usage.artifacts + excluded.artifacts,
			bytes = artifact_usage.bytes + excluded.bytes,
			versions = artifact_usage.versions + excluded.versions,
			assets = artifact_usage.assets + excluded.assets`,
		usageScope, d.artifacts, d.bytes, d.versions, d.assets)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: move the usage counters: %w", err)
	}
	return 1, nil
}

// stampedLimits refuses a stamp that carries no limits: a record the API forms
// always stamps the limits in force, so a zero is a writer's mistake, and
// reading it as "unlimited" would let that mistake past every quota.
func stampedLimits(a artifacts.Authority) bool {
	return a.Limits.Artifacts > 0 && a.Limits.CompanyBytes > 0
}

// actorKind is the kind column a share's granter is kept under.
func actorKindOf(a iam.Actor) string { return string(a.Kind) }

// sortedInts is a copy of xs sorted and unique.
func sortedInts(xs []int) []int {
	out := slices.Clone(xs)
	slices.Sort(out)
	return slices.Compact(out)
}
