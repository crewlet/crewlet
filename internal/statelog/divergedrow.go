package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A DIVERGED LOG IS REMEMBERED, in the node estate, for as long as the rows it
// was found against are the rows this node holds.
//
// # Why the log cannot be trusted to show it again
//
// [ErrLogDiverged] is found by comparing the record the checkpoint names with
// the log's record at the same sequence, and a restart re-derives it the same
// way — which needs the log to still HOLD a record there. Nothing guarantees
// it: an evicted node stops being counted by the trim, which may then remove
// its checkpoint's record; an operator can purge a stream by hand; and a broker
// restored a second time, from a copy older still, may not reach the sequence
// at all. A restarted node then had nothing to compare, found the log
// replayable from its checkpoint, applied the other history on top of its rows,
// and published that the log no longer diverged — lifting every peer's
// truncation fence ([ErrLogTruncated]) on the one node that most needed it.
//
// # What ends it
//
// The row is keyed to the stream and holds the checkpoint and both instants.
// It is recalled only while the checkpoint row names the SAME position and the
// SAME record ([Runner.recallDiverged]): a reanchor moves the checkpoint into a
// new generation and an adoption installs another checkpoint, and each of those
// is a file the verdict was never about — so the row is removed then, and
// never on any reading of the log.
//
// # Why the node estate
//
// It is this node's finding about its own rows, derived from no log, so it is
// not replicated state; and an adoption replaces the replicated file, which
// would take a row there away at exactly the moment it has to be judged
// against the file that replaced it.

// NodeEstate is this node's own database, as the applier needs it: where it
// keeps what it has concluded about its own rows that the log may not show it
// again ([ErrLogDiverged]).
//
// DECLARED HERE because the applier is the caller. The node estate is never
// replaced under a running process, so a handle to it may be held.
type NodeEstate interface {
	Read(ctx context.Context, fn func(*sql.Tx) error) error
	Tx(ctx context.Context, fn func(*sql.Tx) error) error
}

// writeDiverged records that stream's log diverged from these rows at d.
//
// ONE ROW PER STREAM, replaced: a later divergence is necessarily at another
// checkpoint, which only a reanchor or an adoption reaches, and either ended
// the one before it.
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
