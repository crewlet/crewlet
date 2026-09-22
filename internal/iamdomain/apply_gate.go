package iamdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
)

// THE TWO GATE KINDS, and the sweep.
//
// Neither an eviction nor a reanchor is about anybody's identity: each is a
// fact about the LOG. They write one row apiece so a replay reproduces them —
// a recovery that had to be remembered is not a recovery — and they are the
// only two kinds here whose scope is the whole estate.

// applyEviction records a node's removal from this log, or its readmission.
//
// A READMISSION IS AN INVERSE COMMIT rather than a delete: an eviction's whole
// history survives a replay, so a node that was evicted, readmitted and
// evicted again reads correctly rather than as one long absence.
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
	var readmitted any
	if eviction.Readmitted > 0 {
		readmitted = int64(eviction.Readmitted)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_evictions
			(node_id, at, by, from_position, readmitted_position, version)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET
			at                  = excluded.at,
			by                  = excluded.by,
			from_position       = excluded.from_position,
			readmitted_position = excluded.readmitted_position,
			version             = excluded.version
		WHERE excluded.version > iam_evictions.version`,
		at.record.Subject.ID, at.unix(), eviction.By, int64(eviction.From),
		readmitted, at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: record the eviction of node %s: %w",
			at.record.Subject.ID, err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}

// applyGeneration writes a reanchor's audit row.
//
// A REANCHOR IS A COMMITTED RECORD and therefore reproducible from the log;
// this row is what a recovery replay writes and what the operator surface
// reads, rather than an event that happened to a fleet and has to be
// remembered.
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

// encodeGrants renders a grant list for a column.
//
// IT KEEPS A GRANT THIS BUILD DOES NOT KNOW, which is the whole reason it is a
// function rather than a `json.Marshal` at the call site: an invitation issued
// by a newer peer may confer a capability this build has never heard of, and
// dropping it here would silently narrow what the person gets when they redeem
// — on whichever node happened to apply the record.
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
