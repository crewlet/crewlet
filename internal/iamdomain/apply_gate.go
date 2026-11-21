package iamdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
)

// THE THREE ESTATE-WIDE KINDS, and the sweep. An eviction and a reanchor are
// facts about the LOG, and an invalidation is about everybody. Each writes a
// row so a replay reproduces it, and each scopes the whole estate, affordable
// because none is ever deferred.

// applyInvalidation moves the fleet-wide generation every session bearer
// carries, ending every session in the company as each data node applies it.
//
// ONE MONOTONE ROW: a generation at or below the stored one writes nothing, so
// a redelivered bump does not end sessions minted since. NO PER-PERSON ROW,
// the counter's reason to exist: bumping every person's epoch is a write per
// person under a transaction's row budget, and a company past one transaction
// would end only SOME of its sessions.
func (a *Applier) applyInvalidation(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	if at.record.Op != OpInvalidate {
		return 0, fmt.Errorf("iamdomain: the record at %s is op %q on the "+
			"invalidation subject, which this build has no case for",
			at.position, at.record.Op)
	}
	invalidation, err := DecodeInvalidation(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the invalidation record at %s: %w",
			at.position, err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_session_generation
			(singleton, generation, reason, bumped_at, by, version)
		VALUES (0, ?, ?, ?, ?, ?)
		ON CONFLICT(singleton) DO UPDATE SET
			generation = excluded.generation,
			reason     = excluded.reason,
			bumped_at  = excluded.bumped_at,
			by         = excluded.by,
			version    = excluded.version
		WHERE excluded.generation > iam_session_generation.generation`,
		int64(invalidation.Generation), at.record.Reason, at.unix(),
		invalidation.By, at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: move the session generation to %d: %w",
			invalidation.Generation, err)
	}
	written, _ := result.RowsAffected()
	if written > 0 {
		// EVERY SESSION AND MACHINE TOKEN is over, and no list could name
		// them ([Moved.Everyone]).
		a.moved.Everyone = true
	}
	return int(written), nil
}

// applyEviction records a node's removal from this log, or its readmission.
//
// BOTH BOUNDARIES ARE THIS RECORD'S OWN POSITION ([Eviction] says why neither
// is a payload field). A readmission is an INVERSE COMMIT, not a delete, so an
// evict-readmit-evict history survives a replay rather than reading as one
// long absence; readmitting a node never evicted changes no row.
func (a *Applier) applyEviction(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	if at.record.Op != OpEviction {
		return 0, fmt.Errorf("iamdomain: the record at %s is op %q on an "+
			"eviction subject, which this build has no case for", at.position,
			at.record.Op)
	}
	eviction, err := DecodeEviction(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the eviction record at %s: %w",
			at.position, err)
	}
	node := at.record.Subject.ID
	if node == "" {
		return 0, fmt.Errorf("iamdomain: the eviction at %s names no node, so "+
			"there is nothing for the gate to drop", at.position)
	}
	if eviction.Readmit {
		result, execErr := tx.ExecContext(ctx, `
			UPDATE iam_evictions
			SET readmitted_position = ?, version = ?
			WHERE node_id = ? AND version < ?`,
			at.packed, at.packed, node, at.packed)
		if execErr != nil {
			return 0, fmt.Errorf("iamdomain: readmit node %s at %s: %w",
				node, at.position, execErr)
		}
		written, _ := result.RowsAffected()
		return int(written), nil
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_evictions
			(node_id, at, by, from_position, readmitted_position, version)
		VALUES (?, ?, ?, ?, NULL, ?)
		ON CONFLICT(node_id) DO UPDATE SET
			at                  = excluded.at,
			by                  = excluded.by,
			from_position       = excluded.from_position,
			readmitted_position = NULL,
			version             = excluded.version
		WHERE excluded.version > iam_evictions.version`,
		node, at.unix(), eviction.By, at.packed, at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: record the eviction of node %s: %w",
			node, err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}

// applyGeneration writes a reanchor's audit row: what a recovery replay
// writes and the operator surface reads, since a reanchor is a committed
// record rather than an event a fleet must remember.
func (a *Applier) applyGeneration(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	if at.record.Op != OpGeneration {
		return 0, fmt.Errorf("iamdomain: the record at %s is op %q on a "+
			"generation subject, which this build has no case for",
			at.position, at.record.Op)
	}
	generation, err := DecodeGeneration(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the generation record at %s: %w",
			at.position, err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_log_generations
			(generation, at, by, new_stream_created_at, prev_last_seq_seen, record_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(generation) DO NOTHING`,
		int64(generation.Generation), at.unix(), generation.By,
		millis(generation.NewStreamCreatedAt),
		int64(generation.PrevLastSeqSeen), at.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: record generation %d: %w",
			generation.Generation, err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}

// encodeGrants renders a grant list for a column, KEEPING A GRANT THIS BUILD
// DOES NOT KNOW (never filter at a call site): a newer peer's invitation may
// confer one, and dropping it would narrow what the person gets on whichever
// node applied the record.
func encodeGrants(grants []iam.Grant) (string, error) {
	if len(grants) == 0 {
		return "[]", nil
	}
	data, err := json.Marshal(grants)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
