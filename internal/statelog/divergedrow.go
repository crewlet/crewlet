package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A diverged log ([ErrLogDiverged]) is remembered in the node estate for as
// long as this node holds the rows it was found against: finding it again needs
// a record still at the checkpoint's sequence, which a trim that stopped
// counting an evicted node, a hand purge or an older restore can remove, and a
// restarted node would then apply the other history and lift every peer's
// truncation fence ([ErrLogTruncated]). The row, keyed by stream, is recalled
// only while the checkpoint names the same position and record
// ([Runner.recallDiverged]), and removed when a reanchor or an adoption moves
// it, never on a reading of the log; its consumed instant is never zero, since
// a verdict needs a checkpoint naming its record. Not the replicated estate:
// this is the node's own finding, derived from no log, and an adoption
// replaces the replicated file just when the row must be judged against it.

// NodeEstate is this node's own database, where the applier keeps what it has
// concluded about its rows that the log may not show again ([ErrLogDiverged]).
// It is never replaced under a running process, so a handle may be held.
//
// Estate is in the shape so the replicated estate cannot be handed here: a
// [store.ReplicatedHandle] reads and writes alike and sits beside this in
// [RunnerDeps], but does not say which estate it is, so the mistake fails the
// build rather than losing a divergence verdict.
type NodeEstate interface {
	Read(ctx context.Context, fn func(*sql.Tx) error) error
	Tx(ctx context.Context, fn func(*sql.Tx) error) error
	Estate() store.Estate
}

// writeDiverged records that stream's log diverged from these rows at d,
// replacing the stream's one row: a later divergence is at another checkpoint,
// which only a reanchor or an adoption reaches, and either ended the one before.
func writeDiverged(ctx context.Context, db NodeEstate, stream string, d divergedLog,
	now time.Time) error {

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO statelog_diverged
				(stream, generation, seq, consumed_at, held_at, found_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (stream) DO UPDATE SET
				generation  = excluded.generation,
				seq         = excluded.seq,
				consumed_at = excluded.consumed_at,
				held_at     = excluded.held_at,
				found_at    = excluded.found_at`,
			stream, int64(d.at.Generation), int64(d.at.Seq), encodeInstant(d.consumed),
			encodeInstant(d.held), store.EncodeTime(now.UTC()))
		return err
	})
	if err != nil {
		return fmt.Errorf("statelog: record that %s diverged from this node's rows "+
			"at %s: %w", stream, d.at, err)
	}
	return nil
}

// readDiverged is the divergence this node recorded on stream, and false when it
// recorded none.
func readDiverged(ctx context.Context, db NodeEstate, stream string) (divergedLog, bool, error) {
	var generation, seq, consumed, held int64
	err := db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT generation, seq, consumed_at, held_at
			FROM statelog_diverged WHERE stream = ?`, stream).
			Scan(&generation, &seq, &consumed, &held)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return divergedLog{}, false, nil
	case err != nil:
		return divergedLog{}, false, fmt.Errorf("statelog: read whether %s diverged "+
			"from this node's rows: %w", stream, err)
	}
	return divergedLog{
		at:       Position{Stream: stream, Generation: uint32(generation), Seq: uint64(seq)},
		consumed: decodeInstant(consumed),
		held:     decodeInstant(held),
	}, true, nil
}

// clearDiverged removes stream's recorded divergence — only the one at d, so a
// row a later verdict wrote in between is never taken with it.
func clearDiverged(ctx context.Context, db NodeEstate, stream string, d divergedLog) error {
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			DELETE FROM statelog_diverged
			WHERE stream = ? AND generation = ? AND seq = ? AND consumed_at = ?`,
			stream, int64(d.at.Generation), int64(d.at.Seq), encodeInstant(d.consumed))
		return err
	})
	if err != nil {
		return fmt.Errorf("statelog: forget %s's divergence at %s, which is about "+
			"rows this node no longer holds: %w", stream, d.at, err)
	}
	return nil
}
