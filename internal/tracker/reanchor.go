package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE THREE WRITER-SIDE HALVES OF A REANCHOR.
//
// A reanchor is the one recovery for a genuinely recreated stream: the durable
// tables are the record of truth and the stream is a replay window, so
// re-anchoring says "these rows are what they are; follow the new stream from
// its head". It is a full GENERATION TRANSITION rather than a re-stamp, which
// is what makes an old position comparable and safely stale rather than
// indistinguishable from a current one.
//
// [statelog.Reanchor] owns the seven steps and their ordering. What lives here
// is what only this domain can do: reset its own rows' versions, publish its
// own generation record, and write its own audit row.

// versionedTables are the object tables whose `version` a reanchor resets.
//
// EVERY TABLE WITH A COMPOSED VERSION, and the list is derived from the
// identity claim rather than written again: an object table this misses keeps
// versions in a dead number space, and the next write against it forms an
// expectation from a generation that no longer exists.
var versionedTables = []string{
	"tracker_tasks", "tracker_comments", "tracker_body_revisions",
	"tracker_task_keys", "tracker_projects", "tracker_sprints",
	"tracker_counters", "tracker_tagsets", "tracker_catalogues",
	"tracker_tags", "tracker_types", "tracker_fields",
	"tracker_field_options", "tracker_views", "tracker_goals",
	"tracker_persons", "tracker_rank_orders",
}

// ResetVersions puts every object row's version at the new generation's floor.
//
// # Why the rows are rewritten at all
//
// A stored version is a COMPOSED position — the generation in its high bits,
// the stream sequence in its low ones — and after a reanchor the old sequence
// names a number space nothing will ever write to again. Left alone, a row
// carrying it forms an expectation the broker cannot arbitrate: the write is
// refused, the writer re-reads the same number, and the object is wedged for
// as long as the retention gate holds.
//
// # And why it is an optimisation rather than the correctness
//
// The lazy rule covers every row this does not reach: a version at a LOWER
// generation forms `expect = 0` on its next write, which is the create-only
// expectation and is exactly right for a stream whose head is its beginning.
// So an interrupted reset is resumable by re-running it, and the eager pass is
// what stops every first write after a reanchor paying a refused round.
//
// BOUNDED TRANSACTIONS, because this touches every row the company has: one
// statement per table per pass would hold a write transaction across a corpus
// that is tens of gigabytes at a mature company, and the applier shares that
// connection pool.
func ResetVersions(ctx context.Context, db *store.DB, gen uint32) error {
	// THE GENERATION'S OWN FLOOR — (gen << 40) | 0 — which is the
	// packed position of "this generation, sequence zero".
	floor := int64(uint64(gen) * statelog.GenerationStride)
	for _, table := range versionedTables {
		for {
			var moved int64
			err := db.Tx(ctx, func(tx *sql.Tx) error {
				res, err := tx.ExecContext(ctx, fmt.Sprintf(`
					UPDATE %s SET version = ?
					WHERE rowid IN (
						SELECT rowid FROM %s WHERE version < ? LIMIT ?
					)`, table, table),
					floor, floor, statelog.ApplyTxRowBudget)
				if err != nil {
					return err
				}
				moved, err = res.RowsAffected()
				return err
			})
			if err != nil {
				return fmt.Errorf("tracker: reset %s's versions to generation "+
					"%d: %w", table, gen, err)
			}
			if moved == 0 {
				break
			}
		}
	}
	return nil
}

// PublishGeneration writes the reanchor's own record onto the NEW stream.
//
// CREATE-ONLY AT AN EXPECTATION OF ZERO, which is first-writer-wins used for
// the one thing it is perfectly suited to: two operators deriving the same
// generation number race at the broker and exactly one wins.
func (w *Writer) PublishGeneration(ctx context.Context, gen uint32,
	in statelog.ReanchorInputs) error {

	subject := GenerationSubject(gen)
	scope := ScopeSet{Subject: true}
	at := w.Now()
	opID := fmt.Sprintf("reanchor:%d:%d", gen, in.StreamCreatedAt.UTC().UnixNano())
	_, err := w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return w.decide(subject, OpGeneration, "", scope, opID, Generation{
				V:                  GateRecordVersion,
				Gen:                gen,
				NewStreamCreatedAt: in.StreamCreatedAt.UTC(),
				PrevLastSeqSeen:    in.Highest,
				ReanchoredBy:       w.Actor,
				Reason:             "the stream was recreated and its sequences restarted",
			}, nil, at)
		},
	})
	return err
}

// RecordGeneration writes the audit row, in the SAME transaction as the
// cursor move it belongs to.
//
// "What was the old stream's head when we walked away from it, and why did we"
// is the whole content of a reanchor's record, and it is the one thing a
// recovery replay cannot reconstruct — the old stream is gone.
func RecordGeneration(ctx context.Context, tx *sql.Tx, gen uint32,
	in statelog.ReanchorInputs, by string) error {

	_, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_log_generations
			(generation, at, by, prev_stream_created_at, new_stream_created_at,
			 prev_last_seq_seen, reason, record_id)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (generation) DO NOTHING`,
		gen, store.EncodeTime(time.Now().UTC()), by,
		store.EncodeTime(time.Time{}), store.EncodeTime(in.StreamCreatedAt.UTC()),
		in.Highest,
		"the stream was recreated and its sequences restarted",
		fmt.Sprintf("reanchor:%d", gen))
	if err != nil {
		return fmt.Errorf("tracker: record generation %d: %w", gen, err)
	}
	return nil
}
