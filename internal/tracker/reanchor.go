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

// versionedTables are the object tables whose `version` a reanchor resets:
// every tracker table whose `version` is a composed position.
//
// Two tables have a `version` column that is something else, and are absent
// for that reason:
//
//   - `tracker_log_deferred`, where it is the RECORD version a build could not
//     decode — the number an operator picks a build by, which a reset would
//     overwrite.
//   - `tracker_body_revisions`, where it is the BODY version: revision N is
//     the body the task held at version N, and `(task_id, version)` is the
//     table's primary key. A reset writes one floor value into every row it
//     reaches, so a task with two revisions fails the primary key and stops
//     the transition, and a task with one has its revision renumbered to a
//     body version it never had.
var versionedTables = []string{
	"tracker_tasks", "tracker_projects",
	"tracker_counters", "tracker_tagsets", "tracker_catalogues",
	"tracker_views", "tracker_goals", "tracker_persons", "tracker_rank_orders",
}

// ResetVersions puts every object row's version at the new generation's floor.
//
// A stored version is a COMPOSED position — the generation in its high bits,
// the stream sequence in its low ones — so after a reanchor every row's version
// is a position on a stream that no longer exists. This puts each at the new
// generation's floor.
//
// RESUMABLE BY RE-RUNNING, and correct when interrupted: what a write
// arbitrates against is its subject's anchor, and an anchor below the current
// generation takes the "no anchor at this generation" branch whether or not the
// row beside it was reset.
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
// A CLAIM ON THE GENERATION'S OWN SUBJECT ([statelog.Publisher.Claim]): the
// first node to publish there holds the transition, a second node deriving the
// same number meets that record and is refused, and this node's own earlier
// attempt — one that crashed after this step — is found by its op id and
// counts as done. The op id names the node for that reason: two nodes minting
// one id would each read the other's record as their own.
func (w *Writer) PublishGeneration(ctx context.Context, gen uint32,
	in statelog.ReanchorInputs) error {

	switch {
	case w.refusal != nil:
		return w.refusal
	case w.nodeID == "":
		return fmt.Errorf("tracker: generation %d's record names the node that "+
			"re-anchored, and this writer has no node id", gen)
	}
	subject := GenerationSubject(gen)
	scope := ScopeSet{Subject: true}
	at := w.Now()
	opID := statelog.GenerationOpID(gen, in.StreamCreatedAt, w.nodeID)
	return w.publisher.Claim(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: w.mintedAt(at),
		Pattern:  statelog.PatternArbitrated,
		Session:  w.after,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return w.decide(subject, OpGeneration, "", scope, opID, Generation{
				V:                   DocumentVersion,
				Gen:                 gen,
				PrevStreamCreatedAt: in.PrevStreamCreatedAt.UTC(),
				NewStreamCreatedAt:  in.StreamCreatedAt.UTC(),
				PrevLastSeqSeen:     in.Highest,
				ReanchoredBy:        w.Actor,
				Reason:              reanchorReason,
			}, nil, at)
		},
	})
}

// reanchorReason is the one reason a generation transition is ever made.
const reanchorReason = "the stream was recreated and its sequences restarted"

// RecordGeneration writes the audit row, in the SAME transaction as the
// cursor move it belongs to.
//
// "What was the old stream's head when we walked away from it, and why did we"
// is the whole content of a reanchor's record, and it is the one thing a
// recovery replay cannot reconstruct — the old stream is gone.
//
// THE ROW NAMES THE RECORD by the op id [Writer.PublishGeneration] published it
// under, which is why it takes the node that re-anchored: the applier writes
// the same row from that record on every node, and on this one the row written
// here is the one that stands, so a record id spelled any other way names a
// record nothing published.
func RecordGeneration(ctx context.Context, tx *sql.Tx, gen uint32,
	in statelog.ReanchorInputs, by, nodeID string) error {

	_, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_log_generations
			(generation, at, by, prev_stream_created_at, new_stream_created_at,
			 prev_last_seq_seen, reason, record_id)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (generation) DO NOTHING`,
		gen, store.EncodeTime(time.Now().UTC()), by,
		store.EncodeTime(in.PrevStreamCreatedAt.UTC()),
		store.EncodeTime(in.StreamCreatedAt.UTC()),
		in.Highest, reanchorReason,
		statelog.GenerationOpID(gen, in.StreamCreatedAt, nodeID))
	if err != nil {
		return fmt.Errorf("tracker: record generation %d: %w", gen, err)
	}
	return nil
}
