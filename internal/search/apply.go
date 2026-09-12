package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Applier is the vector domain's deterministic state machine: the ONLY writer
// of `kb_vectors` and `kb_vectors_bin`, on every node.
//
// # The two rows are written together, in one statement pair, or not at all
//
// That is what answers the objection this design's shape invites — that
// carrying `model` and `dim` on the narrow table duplicates a fact the wide
// one already holds, and a duplicated fact is a fact that can disagree. It
// cannot disagree here, because there is no window in which one row is written
// and the other is not: both are inside the transaction the framework commits
// with the checkpoint, and the guard that decides whether the wide row moves is
// the same guard the narrow one follows.
//
// # Why the guard is the POSITION and not a revision
//
// A record's own position on this stream is monotone and is a pure function of
// the record — the framework composes it from the writer's generation and the
// broker's sequence — so an applier needs no clock, no config read and no
// second table to decide whether what it is holding is newer than what it has.
// A redelivery is then a no-op by arithmetic rather than by a ledger, which is
// exactly what lets this domain keep none.
type Applier struct{}

// NewApplier builds the vector applier.
//
// IT TAKES NOTHING, unlike the tracker's, and the emptiness is the claim: an
// applier that needed a node id would be an applier whose rows depend on which
// node ran it, which is the property the framework's purity rule exists to
// protect. This domain does not claim identity, so nothing would have caught a
// node id leaking into a row here — which is precisely why it must not.
func NewApplier() Applier { return Applier{} }

// Apply writes this record's rows and reports how many it wrote.
func (a Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record, _ statelog.ApplyOptions) (int, error) {
	vec, err := Decode(rec.Payload)
	if err != nil {
		// A FUTURE VERSION IS THE FRAMEWORK'S TO RETAIN, not this
		// applier's to interpret: it is handed back unchanged so the
		// replication loop files the record at its position, byte for
		// byte, with its declared scope.
		var future *ErrFutureVersion
		if errors.As(err, &future) {
			return 0, err
		}
		return 0, fmt.Errorf("search: apply at %s: %w", rec.Position, err)
	}
	switch vec.Op {
	case OpEmbed:
		return a.embed(ctx, tx, vec, rec.Position)
	case OpForget:
		return a.forget(ctx, tx, vec.Subject, rec.Position)
	}
	// AN UNKNOWN OP AT A KNOWN VERSION IS A WRITER FAULT, not a newer
	// build: the version gate above already let this record through as one
	// this build reads in full, so there is no later build to defer to.
	return 0, fmt.Errorf("search: the record on %s at %s carries op %q, which "+
		"is not one this build writes at version %d",
		vec.Subject, rec.Position, vec.Op, RecordVersion)
}

// embed writes the vector and its sign code.
func (a Applier) embed(ctx context.Context, tx *sql.Tx, vec VectorRecord, at statelog.Position) (int, error) {
	version := int64(at.Packed())
	container := vec.Container
	res, err := tx.ExecContext(ctx, `
		INSERT INTO kb_vectors
			(source, source_id, container, search_shard, model, dim,
			 source_rev, text_sha, embedding, embedded_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		-- THE SHARD IS ABSENT FROM THE UPDATE on purpose: it is a pure
		-- function of the two conflict columns, so an existing row's
		-- bucket cannot change. See [Indexer.upsertOne] for what
		-- re-stamping it would cost the one time it did anything.
		ON CONFLICT (source, source_id) DO UPDATE SET
			container   = excluded.container,
			model       = excluded.model,
			dim         = excluded.dim,
			source_rev  = excluded.source_rev,
			text_sha    = excluded.text_sha,
			embedding   = excluded.embedding,
			embedded_at = excluded.embedded_at,
			version     = excluded.version
		WHERE excluded.version > kb_vectors.version`,
		string(vec.Subject.Source), vec.Subject.ID, container,
		ShardOf(string(vec.Subject.Source), vec.Subject.ID), vec.Model, vec.Dim,
		int64(vec.SourceRev), vec.TextSHA, vec.Embedding,
		store.EncodeTime(vec.CreatedAt), version)
	if err != nil {
		return 0, fmt.Errorf("search: write the vector for %s at %s: %w",
			vec.Subject, at, err)
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if moved == 0 {
		// A REDELIVERY, or a record below what this node already holds.
		// The narrow row is NOT touched, which is what keeps the pair in
		// lockstep: it follows the wide row's guard rather than carrying
		// a second copy of it.
		return 0, nil
	}

	// THE SIGN CODE IS COMPUTED BY THE DATABASE, from the same bytes the
	// record carried, inside this same transaction. No provider call, no
	// second stream, no second cursor and no coverage number of its own —
	// which is the entire reason the first stage costs nothing to maintain.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO kb_vectors_bin
			(source, source_id, container, search_shard, model, dim, bits)
		VALUES (?, ?, ?, ?, ?, ?, vector1bit(?))
		ON CONFLICT (source, source_id) DO UPDATE SET
			container    = excluded.container,
			model        = excluded.model,
			dim          = excluded.dim,
			bits         = excluded.bits`,
		string(vec.Subject.Source), vec.Subject.ID, container,
		// THE SAME CALL, in the same transaction as the row above. Two
		// shards for one document is a document the candidate scan finds
		// in one bucket and the rerank looks for in another.
		ShardOf(string(vec.Subject.Source), vec.Subject.ID), vec.Model,
		vec.Dim, vec.Embedding); err != nil {
		return 0, fmt.Errorf("search: write the sign code for %s at %s: %w",
			vec.Subject, at, err)
	}
	return 2, nil
}

// forget removes both rows for a source that is gone.
//
// GUARDED BY THE POSITION exactly as the write is: a forget below what this
// node already holds is a redelivery replayed after a newer embed, and applying
// it would delete a vector the fleet has since recomputed.
func (a Applier) forget(ctx context.Context, tx *sql.Tx, subject Subject, at statelog.Position) (int, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM kb_vectors WHERE source = ? AND source_id = ? AND version < ?`,
		string(subject.Source), subject.ID, int64(at.Packed()))
	if err != nil {
		return 0, fmt.Errorf("search: forget the vector for %s at %s: %w",
			subject, at, err)
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if moved == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kb_vectors_bin WHERE source = ? AND source_id = ?`,
		string(subject.Source), subject.ID); err != nil {
		return 0, fmt.Errorf("search: forget the sign code for %s at %s: %w",
			subject, at, err)
	}
	return 2, nil
}

// Gated reports whether this record must produce no rows.
//
// NOTHING GATES A VECTOR, and the empty answer is a declaration rather than a
// stub. The tracker's two gates are a permanent deletion marker and an
// eviction fence, and neither has a meaning here: a deleted source is a forget
// record that removes the row outright, and an evicted node's stale embed is
// superseded by the next one the duty publishes — a derived row, recomputable
// from the source, with no history for a fence to protect.
func (Applier) Gated(context.Context, *sql.Tx, statelog.Record) (statelog.Reason, bool, error) {
	return "", false, nil
}

// Committed runs after the transaction commits, for consequences that are not
// rows. There are none: nothing waits on a vector, which is the same fact the
// absent operation ledger states.
func (Applier) Committed(context.Context) {}
