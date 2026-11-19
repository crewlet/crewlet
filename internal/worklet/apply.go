package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier is a PURE FUNCTION of the log, so every node's replicated file
// is byte-identical (a checksum checks it). Four rules keep it so:
//
//  1. NO CONFIG AND NO CLOCK: all it reads arrives in [statelog.ApplyOptions].
//  2. ONLY WHAT THE RECORD SAYS: a record carries its complete new values, so
//     a replay from zero writes the original apply's rows.
//  3. EVERY DERIVED INSTANT IS A MAX OVER A SET, never a fold over arrival
//     order, which differs between nodes at one checkpoint ([effectiveAt]).
//  4. EVERY UPSERT CARRIES A VERSION GUARD THAT SKIPS rather than errs: a
//     redelivery is ordinary, and a constraint violation would abort the
//     transaction on every node and stall the fleet's log.

// Applier writes this node's copy of the tracker's state.
//
// It carries NO NODE IDENTITY ([statelog.Applier] forbids one: it could write
// rows its peers do not); the eviction gate reads [statelog.Record.Writer]
// instead. Its one hold on its node is a LISTENER told after a commit whose
// inbox moved, which writes no row.
type Applier struct {
	// moved is told, after a committed batch, whose inbox moved on this
	// node. Nil means nobody is listening.
	moved func([]inbox.Movement)

	// inboxed is the notices the batch wrote or deleted, filled by Apply and
	// drained by Committed; [inbox.Batch] says why it holds no lock.
	inboxed inbox.Batch
}

// NewApplier builds the applier for one node.
//
// moved is called after a COMMITTED batch with every inbox it moved, never
// inside the transaction: the store re-runs a transiently failed attempt, and
// a movement announced from inside it could name a notice no row holds.
func NewApplier(moved func([]inbox.Movement)) *Applier {
	return &Applier{moved: moved}
}

// Committed is the post-commit half.
//
// Every consequence another node could need is a ROW (a wake is derived by
// the change feed, so it survives this node dying after the commit). Here is
// only the one no other node needs: telling THIS node's open dashboards whose
// inbox moved. Every data node applies every record and only a data node
// serves dashboards, so each tells its own.
func (a *Applier) Committed(context.Context) {
	// DRAINED BEFORE THE CALL: the next batch may wait behind a slow
	// listener, and a set drained after would be delivered twice or lost.
	moved := a.inboxed.Drain(Domain{}.Name())
	if len(moved) > 0 && a.moved != nil {
		a.moved(moved)
	}
}

// Gated reports a record that must produce no rows at all.
//
// TWO GATES, both READ FROM THIS TRANSACTION, never a cache a heartbeat old:
//
//  1. THE EVICTION GATE: a record by a node evicted before the record's
//     position is dropped everywhere. It depends only on the log's order, so
//     it fences even when coordination is unreachable.
//  2. THE DELETION GATE: a record about a purged task (a turn charged to it
//     included) applies nowhere, for ever, so a late redelivery cannot
//     resurrect rows an operator removed.
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
			return "", false, fmt.Errorf("worklet: read the eviction gate for "+
				"node %s: %w", rec.Writer, err)
		default:
			at := rec.Position.Packed()
			// (from, readmitted): at or below the eviction the node was
			// still counted, and at or above a readmission it is back.
			evicted := from.Valid && at > from.Int64
			back := readmitted.Valid && at >= readmitted.Int64
			if evicted && !back {
				return statelog.ReasonEvicted, true, nil
			}
		}
	}

	// The marker of the task the record is ABOUT ([markedTask], which the
	// publisher's reader asks too). A TURN IS GATED TOO (its subject id is its
	// task's): it carries no expectation, so nothing orders it against a
	// purge, and ungated its spend update would find no row and stop the
	// applier on every data node. Gated, it counts as a hit on the marker.
	if taskID, ok := markedTask(rec.Subject); ok {
		var author sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT purge_record_id FROM tracker_deletions WHERE task_id = ?`,
			taskID).Scan(&author)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return "", false, nil
		case err != nil:
			return "", false, fmt.Errorf("worklet: read the deletion gate for "+
				"task %s: %w", taskID, err)
		}
		// THE RECORD THAT WROTE THE MARKER passes, matched by id: by op kind
		// a second purge would get through, and by committed sequence a
		// republished copy could never write its node's marker.
		if author.Valid && author.String == rec.OpID {
			return "", false, nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tracker_deletions
			SET rejects = rejects + 1, last_reject_at = ?
			WHERE task_id = ?`, store.EncodeTime(rec.StoredAt), taskID); err != nil {
			return "", false, fmt.Errorf("worklet: count a gate hit on the "+
				"purged task %s: %w", taskID, err)
		}
		return statelog.ReasonDeleted, true, nil
	}
	return "", false, nil
}

// Apply writes one record's rows.
//
// Every SUBJECT KIND has a case, the two that write nothing included, so a
// kind added without one faults below rather than being ignored: a Go switch
// is not exhaustive, and that fault (TestAKindNobodyDeclaredFaults) is the
// only check.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("worklet: decode the record at %s: %w", rec.Position, err)
	}
	at := applyContext{
		record:       record,
		position:     rec.Position,
		packed:       rec.Position.Packed(),
		brokerAt:     opts.StoredAt,
		epoch:        opts.Epoch,
		maxVariables: opts.MaxVariables,
	}

	switch ObjectKind(rec.Subject.Kind) {
	case KindBarrier:
		// THE READ INDEX'S APPEND: no row, only a quorum-committed position
		// for a linearizable read to wait through.
		return 0, nil
	case KindTrimmed:
		// THE TRIM'S RECORD: the framework removes the anchors below its
		// floor in this transaction (ADR-0080); this domain writes nothing.
		return 0, nil
	case KindTurn:
		return a.applyTurn(ctx, tx, at)
	case KindFlow:
		return a.applyFlow(ctx, tx, at)
	case KindEviction:
		return a.applyEviction(ctx, tx, at)
	case KindGeneration:
		return a.applyGeneration(ctx, tx, at)
	case KindAlias:
		return a.applyAlias(ctx, tx, at)
	case KindRankOrder:
		return a.applyRankOrder(ctx, tx, at)
	case KindRoster:
		return a.applyRoster(ctx, tx, at)
	case KindCounter:
		return a.applyCounter(ctx, tx, at)
	case KindTask:
		return a.applyTask(ctx, tx, at)
	case KindProject, KindTags, KindCatalogue, KindView, KindPerson, KindFile:
		return a.applyDocument(ctx, tx, at)
	}
	// An unknown kind at a version this build reads is a writer publishing a
	// kind it never declared: failing makes the mistake visible, where
	// retaining it would file it under a kind nothing ever applies.
	return 0, fmt.Errorf("worklet: %s is not a kind this build applies, and the "+
		"record at %s claims version %d — a kind is declared before it is "+
		"published", rec.Subject.Kind, rec.Position, record.V)
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record   MutationRecord
	position statelog.Position

	// packed is the composed position: the version every object row's guard
	// compares against.
	packed int64

	// brokerAt is the broker's instant for this record, read in place of a
	// clock so an effective instant is byte-identical on every node.
	brokerAt time.Time

	epoch map[string]any

	// maxVariables is the estate's parameter limit, bounding every multi-row
	// insert ([insertMany]). Zero means none was probed: a row per
	// statement, slow but never wrong.
	maxVariables int
}

// subject is the record's own subject.
func (c applyContext) subject() Subject { return c.record.Subject }

// authored is the writer's own instant: REPORTED, never ordered on.
func (c applyContext) authored() time.Time { return c.record.CreatedAt }

// skewMs is the difference between the two clocks, published, not corrected.
//
// AGAINST THE RAW BROKER VALUE, never a clamped effective instant, which would
// blame whichever node wrote second for its neighbour's skew.
func (c applyContext) skewMs() int64 {
	if c.authored().IsZero() || c.brokerAt.IsZero() {
		return 0
	}
	return c.brokerAt.Sub(c.authored()).Milliseconds()
}

// turnSpendStatement is [Applier.applyTurn]'s write of a turn's spend onto
// its task's running totals, named for the plan test.
const turnSpendStatement = `
		UPDATE tracker_tasks SET
			spend_turns = spend_turns + ?, spend_rounds = spend_rounds + ?,
			spend_input = spend_input + ?, spend_output = spend_output + ?,
			spend_cache_read = spend_cache_read + ?,
			spend_cache_write = spend_cache_write + ?,
			spend_wall_ms = spend_wall_ms + ?, spend_tokens = spend_tokens + ?,
			spend_workers = spend_workers + ?,
			spend_sent_back = spend_sent_back + ?
		WHERE id = ?`

// applyTurn records a turn and adds its spend to the task.
//
// THE ONE ADDITIVE OP: the counters move only when the insert was new, so a
// redelivery cannot double-count.
//
// A SPEND UPDATE AFFECTING ZERO ROWS STOPS THE LOOP: the writer's decide read
// the task, so under a strict replay its create is below this position: a
// missing one is a writer's bug, and skipping it would leave a total nobody can
// reconcile. The one race, a purge after the decide, is read past by
// [Applier.Gated] first.
func (a *Applier) applyTurn(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	var turn TurnRecord
	if err := decodePayload(c.record.Mutation, &turn); err != nil {
		return 0, fmt.Errorf("worklet: decode the turn at %s: %w", c.position, err)
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
		return 0, fmt.Errorf("worklet: record the turn at %s: %w", c.position, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("worklet: read the turn insert's effect: %w", err)
	}
	if inserted == 0 {
		// A redelivery.
		return 0, nil
	}
	spend, err := tx.ExecContext(ctx, turnSpendStatement,
		turn.Spend.Turns, turn.Spend.Rounds, turn.Spend.Input, turn.Spend.Output,
		turn.Spend.CacheRead, turn.Spend.CacheWrite, turn.Spend.WallMs,
		turn.Spend.Tokens(), turn.Spend.Workers, turn.Spend.SentBack, turn.Task)
	if err != nil {
		return 0, fmt.Errorf("worklet: add the turn's spend at %s: %w", c.position, err)
	}
	moved, err := spend.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("worklet: read the spend update's effect: %w", err)
	}
	if moved == 0 {
		return 0, fmt.Errorf("worklet: the turn at %s names task %s, which this "+
			"node does not have — under a strict replay its create is below this "+
			"position, so this is a writer's bug rather than a race, and going "+
			"on would leave a running total nobody can reconcile",
			c.position, turn.Task)
	}
	return int(inserted + moved), nil
}

// effectiveAtStatement is [effectiveAt]'s one statement, named for the plan
// test.
const effectiveAtStatement = `
		SELECT MAX(broker_at) FROM (
			SELECT broker_at FROM tracker_history
			WHERE subject_id = ? AND log_seq <= ?
			UNION ALL
			SELECT broker_at FROM tracker_turns
			WHERE task_id = ? AND log_seq <= ?
			UNION ALL
			SELECT ?
		)`

// effectiveAt is D141's closed form: the fleet-agreed instant for a subject at
// a position.
//
// A MAX OVER A SET, never a fold over arrival order: two nodes at one
// checkpoint hold the same set in different orders, and a fold would split
// them for good on a column nothing repairs.
//
// The set is every history and turn row about this subject at or below this
// composed position (a turn is something that happened to the task).
func effectiveAt(ctx context.Context, tx *sql.Tx, subjectID string, c applyContext) (time.Time, error) {
	var highest sql.NullInt64
	err := tx.QueryRowContext(ctx, effectiveAtStatement,
		subjectID, c.packed, subjectID, c.packed, store.EncodeTime(c.brokerAt)).
		Scan(&highest)
	if err != nil {
		return time.Time{}, fmt.Errorf("worklet: compute the effective instant "+
			"for %s at %s: %w", subjectID, c.position, err)
	}
	if !highest.Valid {
		return c.brokerAt, nil
	}
	return store.DecodeTime(highest.Int64), nil
}

// decodePayload reads a typed payload, an absent one as an empty object: an
// operation needing no payload is ordinary, and a nil check at every call site
// is one somebody forgets.
func decodePayload(raw []byte, into any) error {
	if len(raw) == 0 {
		return nil
	}
	return jsonUnmarshal(raw, into)
}
