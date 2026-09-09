package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier, and the four rules that make it a PURE FUNCTION of the log.
//
// N nodes derive one SQL state from one ordered stream, and the claim that
// their tables are byte-identical is checkable — a checksum over the
// replicated file. That claim survives exactly as long as these four hold:
//
//  1. IT READS NO CONFIG AND NO CLOCK. Everything it would otherwise read
//     arrives in [statelog.ApplyOptions]: the batch's instant, the broker's own
//     timestamp, and the epoch values the domain declared it reads. A node that
//     read its own configuration mid-apply would write rows its peer did not.
//  2. IT WRITES ONLY WHAT THE RECORD SAYS. No lookup against a live counter,
//     no "current" anything: the record carries its complete new values,
//     because that is what makes a replay from zero produce the same rows as
//     the original apply did.
//  3. EVERY DERIVED INSTANT IS A MAX OVER A SET, never a fold over an arrival
//     order. Two nodes at one checkpoint have seen the same set and a different
//     order, so a fold gives them different answers — permanently, on a column
//     nothing repairs.
//  4. EVERY UPSERT CARRIES ITS VERSION GUARD, and the guard is a SKIP rather
//     than an error: a redelivered record is ordinary traffic, and a
//     constraint violation inside this transaction would abort it identically
//     on every node and stall the whole fleet's log.

// Applier writes this node's copy of the tracker's state.
type Applier struct {
	// NodeID is this node's own id, which the eviction gate compares a
	// record's writer against.
	NodeID string
}

// NewApplier builds the applier for one node.
func NewApplier(nodeID string) *Applier { return &Applier{NodeID: nodeID} }

// Committed is the post-commit half, and it is deliberately empty.
//
// EVERY CONSEQUENCE OF A TRACKER RECORD IS A ROW. A wake is derived by the
// change feed — something that outlives this process — rather than published
// here as a courtesy, which is the whole reason a wake survives the node that
// wrote the record dying between the commit and the publish.
func (a *Applier) Committed(context.Context) {}

// Gated reports a record that must produce no rows at all.
//
// TWO GATES, READ FROM THIS SAME TRANSACTION, because the answer has to come
// from the state the record would have applied against rather than from a
// cache that may be a heartbeat old:
//
//  1. THE EVICTION GATE. A record written by a node the fleet evicted before
//     the record's own position is dropped everywhere. It depends on nothing
//     but the log's own order, which is what makes it the fence that holds
//     when coordination cannot be reached at all.
//  2. THE DELETION GATE. A record about a task a purge destroyed applies
//     nowhere, for ever — otherwise a redelivery months later would resurrect
//     rows an operator deliberately removed.
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (statelog.Reason, bool, error) {
	if rec.Writer != "" {
		var from, readmitted sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position
			FROM tracker_evictions WHERE node_id = ? AND log_stream = ?`,
			rec.Writer, rec.Position.Stream).Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return "", false, fmt.Errorf("tracker: read the eviction gate for "+
				"node %s: %w", rec.Writer, err)
		default:
			at := rec.Position.Packed()
			// THE WINDOW IS HALF-OPEN AT BOTH ENDS, and both ends
			// matter: a record at or below the eviction's own position
			// was written while the node was still counted, and one at
			// or above a readmission is written by a node the fleet has
			// taken back.
			evicted := from.Valid && at > from.Int64
			back := readmitted.Valid && at >= readmitted.Int64
			if evicted && !back {
				return statelog.ReasonEvicted, true, nil
			}
		}
	}

	// The deletion gate reads the subject's own marker. A purge is the one
	// operation that removes rows, and its marker is what makes the
	// removal permanent rather than a race a redelivery can undo.
	if ObjectKind(rec.Subject.Kind) == KindTask {
		var purged int
		err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tracker_deletions WHERE task_id = ?`,
			rec.Subject.ID).Scan(&purged)
		if err != nil {
			return "", false, fmt.Errorf("tracker: read the deletion gate for "+
				"task %s: %w", rec.Subject.ID, err)
		}
		if purged > 0 {
			// A purge's own record is what WROTE that marker, so it is
			// not gated by it — every other record about the task is.
			if OpKind(rec.Op) != OpPurge {
				return statelog.ReasonDeleted, true, nil
			}
		}
	}
	return "", false, nil
}

// Apply writes one record's rows.
//
// The dispatch is on the SUBJECT KIND, and every kind has a case — including
// the two that write nothing, which are cases rather than a default so that a
// kind added later without one is a compile-time hole rather than a silently
// ignored record.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("tracker: decode the record at %s: %w", rec.Position, err)
	}
	at := applyContext{
		record:   record,
		position: rec.Position,
		packed:   rec.Position.Packed(),
		brokerAt: opts.StoredAt,
		epoch:    opts.Epoch,
	}

	switch ObjectKind(rec.Subject.Kind) {
	case KindBarrier:
		// THE READ INDEX'S OWN APPEND. It writes no row on any node, and
		// that is its entire content: it exists so a linearizable read
		// has a quorum-committed position to wait through.
		return 0, nil
	case KindTurn:
		return a.applyTurn(ctx, tx, at)
	case KindEviction:
		return a.applyEviction(ctx, tx, at)
	case KindGeneration:
		return a.applyGeneration(ctx, tx, at)
	case KindAlias:
		return a.applyAlias(ctx, tx, at)
	case KindRankOrder:
		return a.applyRankOrder(ctx, tx, at)
	case KindCounter:
		return a.applyCounter(ctx, tx, at)
	case KindTask:
		return a.applyTask(ctx, tx, at)
	case KindProject, KindSprint, KindTags, KindCatalogue, KindView,
		KindGoal, KindPerson:
		return a.applyDocument(ctx, tx, at)
	}
	// A KIND THIS BUILD DOES NOT KNOW REACHES HERE ONLY BY WAY OF A RECORD
	// AT A VERSION IT CAN READ, which is a writer publishing a kind it
	// never declared. Retaining it would file it under a kind nothing will
	// ever apply; failing is what makes the writer's mistake visible.
	return 0, fmt.Errorf("tracker: %s is not a kind this build applies, and the "+
		"record at %s claims version %d — a kind is declared before it is "+
		"published", rec.Subject.Kind, rec.Position, record.V)
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record   MutationRecord
	position statelog.Position

	// packed is the composed position, which is the version every object
	// row's guard compares against.
	packed int64

	// brokerAt is the broker's own instant for this record. THE APPLIER
	// NEVER READS A CLOCK: this is what makes an effective instant
	// byte-identical on every node.
	brokerAt time.Time

	epoch map[string]any
}

// subject is the record's own subject.
func (c applyContext) subject() Subject { return c.record.Subject }

// authored is the writer's own instant, which is REPORTED and never ordered
// on.
func (c applyContext) authored() time.Time { return c.record.CreatedAt }

// skewMs is the difference between the two clocks, published as a number
// rather than corrected.
//
// AGAINST THE RAW BROKER VALUE, never against a clamped effective instant:
// differencing a clamp against a clock reports a large skew on whichever node
// happened to write second and names it for its neighbour's error.
func (c applyContext) skewMs() int64 {
	if c.authored().IsZero() || c.brokerAt.IsZero() {
		return 0
	}
	return c.brokerAt.Sub(c.authored()).Milliseconds()
}

// applyTurn records a turn and adds its spend to the task.
//
// THE ONE ADDITIVE OP. The insert is what gates the addition — the counters
// move only when the row was new — so a redelivery cannot double-count and the
// running total is a function of the applied records rather than a separately
// transmitted number that can disagree with them.
//
// A SPEND UPDATE AFFECTING ZERO ROWS IS A MALFORMED RECORD AND STOPS THE LOOP:
// it names a task this node does not have, which under a strict replay cannot
// happen — the create is below this position — so it is a writer's bug rather
// than a race, and continuing would leave a total nobody can reconcile.
func (a *Applier) applyTurn(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	var turn struct {
		Task    string    `json:"task"`
		Seat    string    `json:"seat"`
		TurnID  string    `json:"turn_id"`
		Trigger string    `json:"trigger"`
		Outcome string    `json:"outcome"`
		Phases  []string  `json:"phases"`
		Spend   TurnSpend `json:"spend"`
	}
	if err := decodePayload(c.record.Mutation, &turn); err != nil {
		return 0, fmt.Errorf("tracker: decode the turn at %s: %w", c.position, err)
	}
	effective, err := effectiveAt(ctx, tx, turn.Task, c)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_turns
			(id, task_id, seat, turn_id, trigger, input_tokens, output_tokens,
			 cache_read, cache_write, phases_json, rounds, wall_ms, outcome,
			 log_seq, log_stream, log_generation, created_at, broker_at,
			 effective_at, skew_ms, document)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (id) DO NOTHING`,
		c.record.OpID, turn.Task, turn.Seat, turn.TurnID, turn.Trigger,
		turn.Spend.Input, turn.Spend.Output, turn.Spend.CacheRead,
		turn.Spend.CacheWrite, jsonOf(turn.Phases), turn.Spend.Rounds,
		turn.Spend.WallMs, turn.Outcome, c.packed, c.position.Stream,
		c.position.Generation, store.EncodeTime(c.authored()),
		store.EncodeTime(c.brokerAt), store.EncodeTime(effective), c.skewMs(),
		[]byte(c.record.Mutation))
	if err != nil {
		return 0, fmt.Errorf("tracker: record the turn at %s: %w", c.position, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("tracker: read the turn insert's effect: %w", err)
	}
	if inserted == 0 {
		// A REDELIVERY, and the whole point of gating on the insert.
		return 0, nil
	}
	spend, err := tx.ExecContext(ctx, `
		UPDATE tracker_tasks SET
			spend_turns = spend_turns + ?, spend_rounds = spend_rounds + ?,
			spend_input = spend_input + ?, spend_output = spend_output + ?,
			spend_cache_read = spend_cache_read + ?,
			spend_cache_write = spend_cache_write + ?,
			spend_wall_ms = spend_wall_ms + ?, spend_tokens = spend_tokens + ?
		WHERE id = ?`,
		turn.Spend.Turns, turn.Spend.Rounds, turn.Spend.Input, turn.Spend.Output,
		turn.Spend.CacheRead, turn.Spend.CacheWrite, turn.Spend.WallMs,
		turn.Spend.Tokens(), turn.Task)
	if err != nil {
		return 0, fmt.Errorf("tracker: add the turn's spend at %s: %w", c.position, err)
	}
	moved, err := spend.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("tracker: read the spend update's effect: %w", err)
	}
	if moved == 0 {
		return 0, fmt.Errorf("tracker: the turn at %s names task %s, which this "+
			"node does not have — under a strict replay its create is below this "+
			"position, so this is a writer's bug rather than a race, and going "+
			"on would leave a running total nobody can reconcile",
			c.position, turn.Task)
	}
	return int(inserted + moved), nil
}

// effectiveAt is D141's closed form: the fleet-agreed instant for a subject at
// a position.
//
// A MAX OVER A SET, never a fold over arrival order. Two nodes at one
// checkpoint have seen the same set of rows and a different order of them, so a
// fold gives them different answers — permanently, on a column nothing
// repairs, and the two readers of this value compare it against each other.
//
// The set is every row ABOUT THIS SUBJECT at a composed position at or below
// this one, across both the history and the turn tables — turn rows are in the
// chain because a turn is something that happened to the task.
func effectiveAt(ctx context.Context, tx *sql.Tx, subjectID string, c applyContext) (time.Time, error) {
	var highest sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT MAX(broker_at) FROM (
			SELECT broker_at FROM tracker_history
			WHERE subject_id = ? AND log_seq <= ?
			UNION ALL
			SELECT broker_at FROM tracker_turns
			WHERE task_id = ? AND log_seq <= ?
			UNION ALL
			SELECT ?
		)`, subjectID, c.packed, subjectID, c.packed, store.EncodeTime(c.brokerAt)).
		Scan(&highest)
	if err != nil {
		return time.Time{}, fmt.Errorf("tracker: compute the effective instant "+
			"for %s at %s: %w", subjectID, c.position, err)
	}
	if !highest.Valid {
		return c.brokerAt, nil
	}
	return store.DecodeTime(highest.Int64), nil
}

// decodePayload reads a typed payload, treating an absent one as an empty
// object rather than as an error — a record whose operation needs no payload
// is ordinary, and a nil check at every call site is a check somebody forgets.
func decodePayload(raw []byte, into any) error {
	if len(raw) == 0 {
		return nil
	}
	return jsonUnmarshal(raw, into)
}
