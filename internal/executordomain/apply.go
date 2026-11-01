package executordomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier, and the rules that make it a PURE FUNCTION of the log.
//
// N nodes derive one SQL state from one ordered stream, and the identity claim
// says their tables are byte-identical. That holds exactly as long as these do:
//
//  1. IT READS NO CONFIG, NO CLOCK, NO KEYRING AND NO OTHER DOMAIN. Every
//     value it writes is the record's or the broker's: the seat's byte budget
//     rides the capture, and every instant a row records of its own is
//     [statelog.ApplyOptions.StoredAt].
//  2. A RECORD THE ROWS NO LONGER ADMIT WRITES NOTHING, NEVER AN ERROR. A
//     capture of an ended turn, a stale capture, a capture the seat's budget
//     cannot hold, a second end, a delete of a turn captured since its
//     cutoff: each applies as no rows on every node alike, and its writer,
//     which decided it from a snapshot of these rows, reads the rows back.
//     An error would stall every node's log on a record that is merely late.
//  3. EVERY WRITE IS GUARDED by the record's ordinals, the turn's tombstone
//     or the record's position, so a redelivery writes nothing it has not
//     written.

// Applier writes this node's copy of the executor estate.
//
// IT HOLDS NOTHING: no keyring, no clock and no listener, which is the first
// rule above made structural. A batch moves nothing another subsystem on this
// node acts on: a launch reads the conversation's head when it starts.
type Applier struct{}

// NewApplier builds the executor applier.
func NewApplier() *Applier { return &Applier{} }

// Committed is the post-commit half, and there is nothing to tell.
func (*Applier) Committed(context.Context) {}

// Gated reports a record that must produce no rows at all, from this same
// transaction: the EVICTION GATE (a record written by a node the fleet evicted
// before its position), the one apply gate on this log. A tombstone is a guard
// the session cases below keep, not a gate (see the package doc).
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (
	statelog.Reason, bool, error) {

	if rec.Writer == "" {
		return "", false, nil
	}
	evicted, err := evictedAt(ctx, tx, rec.Writer, rec.Position.Packed())
	if err != nil {
		return "", false, err
	}
	if evicted {
		return statelog.ReasonEvicted, true, nil
	}
	return "", false, nil
}

// evictedAt reports whether a record at packed, written by writer, falls in
// writer's eviction window. THE WINDOW IS HALF-OPEN AT BOTH ENDS: a record at
// or below the eviction's own position was written while the node was counted,
// and one at or above a readmission by a node the fleet took back.
func evictedAt(ctx context.Context, tx *sql.Tx, writer string, packed int64) (bool, error) {
	var from, readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM executor_evictions
		WHERE node_id = ?`, writer).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("executordomain: read the eviction gate for node "+
			"%s: %w", writer, err)
	}
	evicted := from.Valid && packed > from.Int64
	back := readmitted.Valid && packed >= readmitted.Int64
	return evicted && !back, nil
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record   MutationRecord
	position statelog.Position

	// packed is the composed position: every row's version, and an end's
	// own place in its conversation.
	packed int64

	// brokerAt is the broker's own instant for this record, the ONLY
	// instant a row records of its own.
	brokerAt time.Time
}

// at is the broker instant as it is stored.
func (c applyContext) at() int64 { return store.EncodeTime(c.brokerAt) }

// Apply writes one record's rows. The dispatch is on the SUBJECT KIND, and
// every kind has a case.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("executordomain: decode the record at %s: %w", rec.Position, err)
	}
	c := applyContext{
		record: record, position: rec.Position, packed: rec.Position.Packed(),
		brokerAt: opts.StoredAt,
	}
	payload, err := DecodeMutation(record)
	if err != nil {
		return 0, fmt.Errorf("executordomain: the record at %s: %w", rec.Position, err)
	}
	switch record.Subject.Kind {
	case KindBarrier:
		// THE READ INDEX'S OWN APPEND writes no row on any node.
		return 0, nil
	case KindEviction:
		return applyEviction(ctx, tx, c, payload)
	case KindGeneration:
		return applyGeneration(ctx, tx, c, payload)
	case KindConversation:
		return applyConversation(ctx, tx, c, payload)
	case KindSeat:
		return applySeat(ctx, tx, c, payload)
	}
	// A KIND THIS BUILD DOES NOT KNOW REACHES HERE ONLY AT A VERSION IT CAN
	// READ, which is a writer publishing a kind it never declared; failing is
	// what makes that writer's mistake visible.
	return 0, fmt.Errorf("executordomain: %s is not a kind this build applies, "+
		"and the record at %s claims version %d: a kind is declared before it is "+
		"published", record.Subject.Kind, rec.Position, record.V)
}

// applyEviction records a node's removal from this log, or its readmission, as
// an INVERSE COMMIT rather than a delete.
func applyEviction(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	e, ok := payload.(Eviction)
	if !ok || e.NodeID == "" {
		return 0, fmt.Errorf("executordomain: the eviction at %s names no node", c.position)
	}
	var (
		res sql.Result
		err error
	)
	if e.Readmitted {
		res, err = tx.ExecContext(ctx, `
			UPDATE executor_evictions SET readmitted_position = ?, version = ?
			WHERE node_id = ? AND version < ?`, c.packed, c.packed, e.NodeID, c.packed)
	} else {
		// A SECOND EVICTION CLEARS THE READMISSION, so the evicted,
		// readmitted, evicted sequence reads as three facts.
		res, err = tx.ExecContext(ctx, `
			INSERT INTO executor_evictions
				(node_id, at, by, from_position, readmitted_position, version)
			VALUES (?, ?, ?, ?, NULL, ?)
			ON CONFLICT (node_id) DO UPDATE SET
				at = excluded.at, by = excluded.by,
				from_position = excluded.from_position,
				readmitted_position = NULL, version = excluded.version
			WHERE excluded.version > executor_evictions.version`,
			e.NodeID, c.at(), e.EvictedBy, c.packed, c.packed)
	}
	if err != nil {
		return 0, fmt.Errorf("executordomain: apply the eviction of %s at %s: %w",
			e.NodeID, c.position, err)
	}
	return affected(res), nil
}

// applyGeneration writes a reanchor's audit row, create-only.
func applyGeneration(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	g, ok := payload.(Generation)
	if !ok {
		return 0, fmt.Errorf("executordomain: the generation record at %s carries "+
			"a %T", c.position, payload)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO executor_log_generations
			(generation, at, by, new_stream_created_at, prev_last_seq_seen, record_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (generation) DO NOTHING`,
		g.Generation, c.at(), g.By, store.EncodeTime(g.StreamAt),
		int64(g.PrevHighest), c.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("executordomain: record generation %d at %s: %w",
			g.Generation, c.position, err)
	}
	return affected(res), nil
}

// affected is how many rows a statement wrote, the row budget's input.
func affected(res sql.Result) int {
	n, _ := res.RowsAffected()
	return int(n)
}

// present reports an id a payload carries, false for an empty one: a record
// naming no object applies as nothing. Ids are the opaque keys the rows hold
// here; that a turn's is a UUID is the writer's to hold.
func present(id string) bool { return strings.TrimSpace(id) != "" }
