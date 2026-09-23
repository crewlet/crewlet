package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// tables is the SQL the FRAMEWORK owns, over its own two tables and over the
// two a domain names.
//
// # Why the framework writes a domain's tables
//
// The operation ledger and the deferred-record table are `Local` — this node's
// own — but they are written in the APPLIER's transaction, beside the rows,
// because contract 2 puts them there and a transaction is one file. Their
// contents are framework facts: an operation id and a position, a record this
// build could not decode. So the domain owns the schema, which is in its own
// migration, and the framework owns the statements, which are here — one
// implementation rather than one per domain.
//
// The table NAMES are interpolated, and every one is checked against a strict
// identifier pattern at construction. A table name is not a value a driver can
// bind, and the alternative to checking is a domain that can name a table with
// a quote in it.
type tables struct {
	stream   string
	prefix   string
	ops      string
	deferred string
	scope    string
}

// subjectOf is the WIRE subject an anchor is keyed on.
//
// ONE SPELLING, and it lives here because three callers need it and two of
// them are on opposite sides of the same key: the applier advances the anchor
// and the publisher reads it, so a bare path in one and a prefixed one in the
// other is a publisher that never finds an anchor at all — every arbitrated
// write falls through to the last-message probe, loops its whole round budget
// and reports a conflict on an object nobody else touched.
func (t tables) subjectOf(s Subject) string { return t.prefix + "." + s.String() }

// identifier is what a domain may call a table. Deliberately narrower than SQL
// allows: these names are interpolated into statements, so the set is what is
// unambiguous rather than what the parser tolerates.
func validIdentifier(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func newTables(d Domain) (tables, error) {
	spec := d.Stream()
	t := tables{
		stream:   spec.Name,
		prefix:   spec.SubjectPrefix,
		ops:      d.OpsTable(),
		deferred: d.DeferredTable(),
		scope:    d.ScopeIndex(),
	}
	for field, name := range map[string]string{
		"DeferredTable": t.deferred,
		"ScopeIndex":    t.scope,
	} {
		if !validIdentifier(name) {
			return tables{}, fmt.Errorf("statelog: domain %q names %s %q — a "+
				"table name is interpolated into a statement rather than bound, "+
				"so it must be lowercase letters, digits and underscores",
				d.Name(), field, name)
		}
	}
	// The ops table is the one that may legitimately be absent.
	if t.ops != "" && !validIdentifier(t.ops) {
		return tables{}, fmt.Errorf("statelog: domain %q names OpsTable %q, which "+
			"is not a plain identifier", d.Name(), t.ops)
	}
	return t, nil
}

// cursorRow is one domain's checkpoint row, whole.
type cursorRow struct {
	at      Position
	created time.Time

	// storedAt is the broker's own instant for the record at the
	// checkpoint's sequence — the one this node consumed there — and zero
	// where that is unknown: a row older than the column, a checkpoint at
	// sequence 0, or one a reanchor placed where the log holds no record.
	// It is what [Runner.VerifyCheckpoint] compares the log's record with.
	storedAt time.Time

	// void is the generations the reanchor that placed this checkpoint
	// ABANDONED — see [ReanchorPlan.From].
	void voidRange
}

// encodeInstant is a broker instant as a checkpoint column holds it: zero for
// an unknown one, which [store.EncodeTime] would write as the year one and read
// back as a real instant nothing could ever equal.
func encodeInstant(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return store.EncodeTime(t)
}

// decodeInstant is [encodeInstant]'s inverse: zero is the zero instant.
func decodeInstant(micros int64) time.Time {
	if micros == 0 {
		return time.Time{}
	}
	return store.DecodeTime(micros)
}

// voidRange is what the reanchor that placed a checkpoint made VOID wherever
// that checkpoint is followed from, in two rules: the generations strictly
// between after and before, the ones it ABANDONED ([ReanchorPlan.From]); and,
// for a RESTORED reanchor, every record positioned after staleAfter — the
// generation record it appended — whose generation is below before, the one it
// opened ([ReanchorPlan.StaleAfter]). Zero, zero and zero on every checkpoint
// no such reanchor placed.
type voidRange struct {
	after, before uint32
	staleAfter    uint64
}

// abandons reports whether generation gen is one a reanchor abandoned.
func (v voidRange) abandons(gen uint32) bool { return gen > v.after && gen < v.before }

// overtakes reports whether a record at seq written in generation gen is one a
// restored reanchor overtook: after its generation record, in a generation
// below the one it opened.
func (v voidRange) overtakes(seq uint64, gen uint32) bool {
	return v.staleAfter > 0 && seq > v.staleAfter && gen < v.before
}

// readCursor reads this domain's checkpoint, reporting false when the applier
// has never committed on this stream.
func (t tables) readCursor(ctx context.Context, tx *sql.Tx) (cursorRow, bool, error) {
	var gen, seq, created, storedAt, voidAfter, voidBefore, staleAfter int64
	err := tx.QueryRowContext(ctx, `
		SELECT generation, seq, stream_created_at, stored_at, void_after, void_before,
			stale_after
		FROM statelog_cursor WHERE stream = ?`,
		t.stream).Scan(&gen, &seq, &created, &storedAt, &voidAfter, &voidBefore, &staleAfter)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return cursorRow{at: Position{Stream: t.stream}}, false, nil
	case err != nil:
		return cursorRow{}, false, fmt.Errorf("statelog: read the cursor: %w", err)
	}
	return cursorRow{
		at:       Position{Stream: t.stream, Generation: uint32(gen), Seq: uint64(seq)},
		created:  store.DecodeTime(created),
		storedAt: decodeInstant(storedAt),
		void: voidRange{after: uint32(voidAfter), before: uint32(voidBefore),
			staleAfter: uint64(staleAfter)},
	}, true, nil
}

// setCursor writes the checkpoint, in the SAME transaction as the rows it
// covers.
//
// That is the one contract every consumer of this framework gets for free and
// can break invisibly. The nearest neighbour in this tree does the opposite —
// it commits its batch and then writes its cursor in a second transaction, on
// the stated reasoning that a crash between the two replays the batch, which
// is free. That is true while the source can always redeliver, and FALSE for a
// log that gets trimmed: the replay it counts on is a replay of records the
// trim has already removed.
//
// storedAt is the broker's instant for the record at p — the one this
// transaction consumed there — which is how the checkpoint NAMES its record
// ([cursorRow.storedAt]).
func (t tables) setCursor(ctx context.Context, tx *sql.Tx, p Position, created, storedAt,
	now time.Time) error {

	_, err := tx.ExecContext(ctx, `
		INSERT INTO statelog_cursor
			(stream, generation, seq, stream_created_at, stored_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (stream) DO UPDATE SET
			generation        = excluded.generation,
			seq               = excluded.seq,
			stream_created_at = excluded.stream_created_at,
			stored_at         = excluded.stored_at,
			updated_at        = excluded.updated_at`,
		t.stream, int64(p.Generation), int64(p.Seq),
		store.EncodeTime(created), encodeInstant(storedAt), store.EncodeTime(now))
	if err != nil {
		return fmt.Errorf("statelog: write the cursor at %s: %w", p, err)
	}
	return nil
}

// reanchorCursor writes the checkpoint a reanchor places — at p, keyed to
// created, naming the log's record at p by storedAt (zero where the log holds
// none there) — together with what it made void: every generation strictly
// between from and p's own ([ReanchorPlan.From]), and, for a restored reanchor,
// every lower-generation record after staleAfter, the generation record it
// appended ([ReanchorPlan.StaleAfter]), zero when there is no such rule.
//
// ITS OWN STATEMENT rather than a flag on setCursor, because the two write
// different things for a reason: the applier's checkpoint moves every batch
// and must leave the abandoned range alone, since a node part-way through the
// abandoned generation's records keeps voiding them after a restart; and a
// reanchor must REPLACE it, since the range an earlier reanchor abandoned says
// nothing about the log this one follows.
func (t tables) reanchorCursor(ctx context.Context, tx *sql.Tx, p Position,
	created, storedAt time.Time, from uint32, staleAfter uint64, now time.Time) error {

	_, err := tx.ExecContext(ctx, `
		INSERT INTO statelog_cursor
			(stream, generation, seq, stream_created_at, stored_at, updated_at,
			 void_after, void_before, stale_after)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (stream) DO UPDATE SET
			generation        = excluded.generation,
			seq               = excluded.seq,
			stream_created_at = excluded.stream_created_at,
			stored_at         = excluded.stored_at,
			updated_at        = excluded.updated_at,
			void_after        = excluded.void_after,
			void_before       = excluded.void_before,
			stale_after       = excluded.stale_after`,
		t.stream, int64(p.Generation), int64(p.Seq),
		store.EncodeTime(created), encodeInstant(storedAt), store.EncodeTime(now),
		int64(from), int64(p.Generation), int64(staleAfter))
	if err != nil {
		return fmt.Errorf("statelog: write the reanchored cursor at %s: %w", p, err)
	}
	return nil
}

// advanceAnchor records that this node consumed a record at p on subject,
// WHATEVER the record then did.
//
// MAX rather than assignment, because a deferred record is replayed at its
// ORIGINAL position when a build that can read it arrives — below a successor
// this node already applied. An assignment there would move the anchor
// backwards and hand the next writer an expectation the broker refuses for
// ever.
func (t tables) advanceAnchor(ctx context.Context, tx *sql.Tx, subject string, p Position) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO statelog_anchor (stream, subject, anchor) VALUES (?, ?, ?)
		ON CONFLICT (stream, subject) DO UPDATE SET anchor = MAX(anchor, excluded.anchor)`,
		t.stream, subject, p.Packed())
	if err != nil {
		return fmt.Errorf("statelog: advance the anchor on %s to %s: %w", subject, p, err)
	}
	return nil
}

// anchor reads a subject's arbitration anchor by exact key.
func (t tables) anchor(ctx context.Context, tx *sql.Tx, subject string, gen uint32) (Position, error) {
	var packed int64
	err := tx.QueryRowContext(ctx,
		`SELECT anchor FROM statelog_anchor WHERE stream = ? AND subject = ?`,
		t.stream, subject).Scan(&packed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Position{Stream: t.stream, Generation: gen}, nil
	case err != nil:
		return Position{}, fmt.Errorf("statelog: read the anchor on %s: %w", subject, err)
	}
	return Position{
		Stream:     t.stream,
		Generation: uint32(packed / GenerationStride),
		Seq:        uint64(packed % GenerationStride),
	}, nil
}

// writeOp records that this node applied an operation at a position, by the
// record whose broker instant is storedAt.
func (t tables) writeOp(ctx context.Context, tx *sql.Tx, opID, subject string, p Position,
	storedAt, now time.Time) error {

	if t.ops == "" || opID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO `+t.ops+` (op_id, subject, position, applied_at, stored_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (op_id) DO NOTHING`,
		opID, subject, p.Packed(), store.EncodeTime(now), encodeInstant(storedAt))
	if err != nil {
		return fmt.Errorf("statelog: record operation %q at %s: %w", opID, p, err)
	}
	return nil
}

// OpsPurgeBatch bounds how many rows one sweep statement deletes.
//
// THE APPLIER'S CONNECTION IS PINNED AND ITS COMMITS ARE THE SAME FILE'S, so a
// statement that deleted a whole backlog would hold the writer for as long as
// it took — and the backlog case is exactly the one that matters: a node
// returning from a month away has a month of rows to shed on its first tick.
// Two thousand rows of a five-column table with a TEXT primary key is a few
// hundred KB of pages, single-digit milliseconds of writer hold, while a
// month's overhang still clears in a few hundred statements inside one
// maintenance tick. It is the same shape and the same reasoning as
// [store.EventPurgeBatch], at a larger batch because these rows are a
// hundredth the size.
const OpsPurgeBatch = 2000

// purgeOps deletes operation rows applied before cutoff, reporting how many
// went.
//
// A RANGE DELETE OVER THE AGE, which is the shape the ops table's own index
// was shipped for: without `<domain>_ops_swept_idx` a node returning from a
// month away scans the whole table on every tick.
//
// BATCHED, AND EACH BATCH ITS OWN TRANSACTION. One transaction over the whole
// backlog would hold the writer the applier is queued behind for the length of
// it, which on a first tick after a long absence is the whole month.
//
// A BATCH THAT DELETED ANYTHING RECORDS WHAT IT FORGOT, in its own
// transaction ([tables.markLost]): the watermark is how the publisher tells a
// row that was never written from one that was swept ([tables.lostBefore]),
// and one written after the delete it describes — or in another transaction —
// could trail the rows actually gone, which is the one direction that
// re-decides an operation that already landed. A batch that deleted nothing
// records nothing, because nothing was forgotten.
func (t tables) purgeOps(ctx context.Context, db Estate, cutoff time.Time) (int64, error) {
	if t.ops == "" {
		return 0, nil
	}
	var total int64
	for {
		var deleted int64
		err := db.Tx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `
				DELETE FROM `+t.ops+` WHERE rowid IN (
					SELECT rowid FROM `+t.ops+` WHERE applied_at < ? LIMIT ?)`,
				store.EncodeTime(cutoff), OpsPurgeBatch)
			if err != nil {
				return err
			}
			deleted, err = res.RowsAffected()
			if err != nil || deleted == 0 {
				return err
			}
			return t.markLost(ctx, tx, cutoff)
		})
		if err != nil {
			return total, fmt.Errorf("statelog: sweep %s: %w", t.ops, err)
		}
		total += deleted
		if deleted < OpsPurgeBatch {
			// A SHORT BATCH IS THE END, and the loop stops on it
			// rather than on a zero: a final batch that exactly
			// filled the limit costs one more empty statement, and a
			// loop that only stopped at zero would run one anyway.
			return total, nil
		}
	}
}

// markLost records that the ops table may have lost rows applied before
// before, in the caller's transaction — which must be the one that loses them.
//
// MONOTONE, so a later loss with an earlier instant — a sweep after a clock
// stepped back, a shorter horizon — never un-forgets rows an earlier one lost.
func (t tables) markLost(ctx context.Context, tx *sql.Tx, before time.Time) error {
	if t.ops == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO statelog_ops_lost (ops_table, lost_before)
		VALUES (?, ?)
		ON CONFLICT (ops_table) DO UPDATE SET
			lost_before = MAX(lost_before, excluded.lost_before)`,
		t.ops, store.EncodeTime(before))
	if err != nil {
		return fmt.Errorf("statelog: record that %s lost rows before %s: %w",
			t.ops, before.UTC().Format(time.RFC3339Nano), err)
	}
	return nil
}

// lostBefore answers the instant before which the ops table may have lost
// rows, reporting false when it has lost none — see [Rows.LostBefore] and
// migration 0017.
func (t tables) lostBefore(ctx context.Context, tx *sql.Tx) (time.Time, bool, error) {
	if t.ops == "" {
		return time.Time{}, false, nil
	}
	var before int64
	err := tx.QueryRowContext(ctx,
		`SELECT lost_before FROM statelog_ops_lost WHERE ops_table = ?`,
		t.ops).Scan(&before)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("statelog: read how far back %s "+
			"may have lost rows: %w", t.ops, err)
	}
	return store.DecodeTime(before), true, nil
}

// OpEntry is one row of a domain's operation ledger: an operation this node's
// applier applied, where its record landed, and the WIRE subject it landed on
// — the domain's prefix and the object's kind and id, as the anchor is keyed.
//
// THE SUBJECT IS WHAT MAKES A ROW AN ANSWER. An operation id is the caller's,
// and one carried to a second object finds the first object's row under it —
// so a row answers for a write only on the subject that write is to, and
// [Publisher.heldHere] refuses the rest.
type OpEntry struct {
	Position Position
	Subject  string
}

// op answers where an operation was applied on this node, and on what.
func (t tables) op(ctx context.Context, tx *sql.Tx, opID string) (OpEntry, bool, error) {
	if t.ops == "" {
		return OpEntry{}, false, nil
	}
	var packed int64
	var subject string
	err := tx.QueryRowContext(ctx,
		`SELECT position, subject FROM `+t.ops+` WHERE op_id = ?`, opID).
		Scan(&packed, &subject)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return OpEntry{}, false, nil
	case err != nil:
		return OpEntry{}, false, fmt.Errorf("statelog: read operation %q: %w", opID, err)
	}
	return OpEntry{
		Position: Position{
			Stream:     t.stream,
			Generation: uint32(packed / GenerationStride),
			Seq:        uint64(packed % GenerationStride),
		},
		Subject: subject,
	}, true, nil
}

// appliedRecord answers whether this node applied the record with operation
// opID, stored by the broker at storedAt, at position p — the one question a
// restored reanchor asks of the ledger ([UnheldTail]).
//
// ALL THREE MUST AGREE. The operation alone is a caller's intent, which a
// retry after the restore carries into a second record; the position alone is
// a sequence, which the restored log reissues to whatever was written there
// next; the instant is the record's own, and a row that names none (written
// before the ledger kept it) vouches for nothing.
func (t tables) appliedRecord(ctx context.Context, tx *sql.Tx, opID string, p Position,
	storedAt time.Time) (bool, error) {

	if t.ops == "" || opID == "" {
		return false, nil
	}
	var packed, recorded int64
	err := tx.QueryRowContext(ctx,
		`SELECT position, stored_at FROM `+t.ops+` WHERE op_id = ?`, opID).
		Scan(&packed, &recorded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("statelog: read operation %q: %w", opID, err)
	}
	named := decodeInstant(recorded)
	return packed == p.Packed() && !named.IsZero() && sameRecord(named, storedAt), nil
}

// consumed is what this node's own ledgers say about the record it consumed at
// one position ([tables.consumedAt]): its broker instant and which evidence
// named it, or — where the operation ledger names at that position another
// operation than the log's record carries, and keeps no instant — that
// operation.
type consumed struct {
	storedAt time.Time
	evidence string
	otherOp  string
}

// consumedAt answers, from this node's own ledgers in tx, what it consumed at
// position p — given that the log's record there carries operation opID (empty
// where its envelope did not decode) and was stored at held. See
// [Runner.nameCheckpoint].
//
//   - `ledger_instant`: the operation ledger's row at p keeps its record's
//     instant. That instant is the answer whether or not it is the log's —
//     another one is exactly the divergence the caller compares for.
//   - `ledger_operation`: the row at p predates the instant column and names
//     opID, so the operation and the position agreeing is the evidence, and
//     the log's instant the answer.
//   - `retained`: this node kept a record it could not decode at p, with its
//     instant.
//   - otherOp: the row at p predates the instant column and names another
//     operation than opID — the log's record at p is not the one this node
//     applied there.
//
// No row at p is not evidence either way: the record there may have written
// none (a read barrier, a repeat of an operation applied earlier, a record a
// gate kept out of the rows), or the sweep may have taken it.
//
// BY THE LOG'S OPERATION FIRST, which is the table's primary key and the
// ordinary answer; BY POSITION only where that misses, which no index serves —
// a scan of a ledger the sweep bounds to its retention window, asked once per
// copy of the rows of a checkpoint that names nothing, and never on the apply
// path.
func (t tables) consumedAt(ctx context.Context, tx *sql.Tx, p Position, opID string,
	held time.Time) (consumed, error) {

	if t.ops != "" {
		named := func(recorded int64, op string) (consumed, bool) {
			switch {
			case recorded != 0:
				return consumed{storedAt: decodeInstant(recorded), evidence: "ledger_instant"}, true
			case opID == "":
				// NOTHING TO COMPARE the ledger's operation with.
				return consumed{}, false
			case op == opID:
				return consumed{storedAt: held, evidence: "ledger_operation"}, true
			}
			return consumed{otherOp: op}, true
		}
		if opID != "" {
			var packed, recorded int64
			err := tx.QueryRowContext(ctx,
				`SELECT position, stored_at FROM `+t.ops+` WHERE op_id = ?`, opID).
				Scan(&packed, &recorded)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return consumed{}, fmt.Errorf("statelog: read operation %q: %w", opID, err)
			case packed == p.Packed():
				if c, ok := named(recorded, opID); ok {
					return c, nil
				}
			}
		}
		var op string
		var recorded int64
		err := tx.QueryRowContext(ctx,
			`SELECT op_id, stored_at FROM `+t.ops+` WHERE position = ? LIMIT 1`, p.Packed()).
			Scan(&op, &recorded)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return consumed{}, fmt.Errorf("statelog: read the operation applied at %s: %w", p, err)
		default:
			if c, ok := named(recorded, op); ok {
				return c, nil
			}
		}
	}
	if t.deferred != "" {
		var recorded int64
		err := tx.QueryRowContext(ctx,
			`SELECT stored_at FROM `+t.deferred+` WHERE position = ?`, p.Packed()).
			Scan(&recorded)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return consumed{}, fmt.Errorf("statelog: read the retained record at %s: %w", p, err)
		case recorded != 0:
			return consumed{storedAt: store.DecodeTime(recorded), evidence: "retained"}, nil
		}
	}
	return consumed{}, nil
}

// nameCursor writes storedAt as the record the checkpoint at p names — only
// while the row stands at p and names none, so a write for a checkpoint that
// has since moved, or been named by a commit, changes nothing.
func (t tables) nameCursor(ctx context.Context, tx *sql.Tx, p Position, storedAt time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE statelog_cursor SET stored_at = ?
		WHERE stream = ? AND generation = ? AND seq = ? AND stored_at = 0`,
		encodeInstant(storedAt), t.stream, int64(p.Generation), int64(p.Seq))
	if err != nil {
		return fmt.Errorf("statelog: name the record at the cursor %s: %w", p, err)
	}
	return nil
}

// retainedRecord answers whether this node consumed and RETAINED the record at
// position p stored by the broker at storedAt: a record this build could not
// read is held here byte for byte, and is these rows' history as much as one
// they applied.
func (t tables) retainedRecord(ctx context.Context, tx *sql.Tx, p Position,
	storedAt time.Time) (bool, error) {

	if t.deferred == "" {
		return false, nil
	}
	var recorded int64
	err := tx.QueryRowContext(ctx,
		`SELECT stored_at FROM `+t.deferred+` WHERE position = ?`, p.Packed()).
		Scan(&recorded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("statelog: read the retained record at %s: %w", p, err)
	}
	return sameRecord(store.DecodeTime(recorded), storedAt), nil
}

// retain stores a record this build cannot decode, byte for byte, with every
// term of its scope.
//
// ONE TRANSACTION for the record and its scope, and the rule is not tidiness:
// a deferral recorded without its scope is a record that makes rows stale with
// nothing able to see that it does — so the probe passes, a writer takes the
// retry-at-zero branch, and the deferred mutation is overwritten with nothing
// anywhere reporting it.
func (t tables) retain(ctx context.Context, tx *sql.Tx, rec Record, compacted bool,
	maxVariables int) error {
	// A COMPACTED DOMAIN KEYS ON THE SUBJECT and a strict one on the
	// position. The stream itself keeps one message per subject there, so
	// a positional retention would keep records the stream has already
	// superseded — and reprocessing them would write state the broker no
	// longer holds.
	key := rec.Position.Packed()
	subject := rec.Subject.String()
	if compacted {
		// THE CHILD GOES FIRST, and the order is the whole statement
		// pair. There is no foreign key here — a cascade is a delete
		// nobody committed — so the scope rows are found through the
		// parent's own position, and a parent deleted first leaves a
		// subquery that matches nothing and scope rows that outlive
		// every record. A probe then reports a deferral on a record
		// this node no longer holds, for ever: the read barrier waits
		// on it and the writer's step 0 refuses to publish, with
		// nothing able to clear either.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+t.scope+` WHERE position IN
				(SELECT position FROM `+t.deferred+` WHERE subject = ?)`, subject); err != nil {
			return fmt.Errorf("statelog: supersede the deferred scope on %s: %w", subject, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+t.deferred+` WHERE subject = ?`, subject); err != nil {
			return fmt.Errorf("statelog: supersede the deferred record on %s: %w", subject, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO `+t.deferred+`
			(position, subject, subject_kind, subject_id, version, payload, stored_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (position) DO NOTHING`,
		key, subject, rec.Subject.Kind, rec.Subject.ID, rec.V, rec.Payload,
		store.EncodeTime(rec.StoredAt)); err != nil {
		return fmt.Errorf("statelog: retain the record at %s: %w", rec.Position, err)
	}
	// THE ONE DATA-DRIVEN COLLECTION IN THIS FILE. A record's scope is the
	// complete set of object paths its apply may write, stated by the
	// writer, and the paths NEST — a bulk edit over a project states the
	// project and every task under it, so this is a collection whose size
	// is the edit's, not a fixed one.
	//
	// THE ERROR NO LONGER NAMES THE PATH, and that is the deliberate cost:
	// a chunk carries up to a thousand of them, so naming one would be a
	// guess that reads as a fact. The engine names the offending row in its
	// own error, which %w carries. Nothing matched on the old text.
	paths := rec.Scope.Normalised().Paths
	if _, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO `+t.scope+` (position, path) VALUES`, `(?, ?)`,
		`ON CONFLICT (position, path) DO NOTHING`,
		len(paths), func(i int) []any { return []any{key, paths[i]} }); err != nil {
		return fmt.Errorf("statelog: index the %d-path deferred scope at %s: %w",
			len(paths), rec.Position, err)
	}
	return nil
}

// retained is one retained record as the reprocess reads it back: the bytes as
// published, at the position they were published at.
type retained struct {
	position int64
	version  int
	payload  []byte
	storedAt time.Time
}

// deferredAfter reads the retained records above a position, oldest first,
// bounded so a long-stalled upgrade's backlog is walked in pages rather than
// loaded whole.
func (t tables) deferredAfter(ctx context.Context, tx *sql.Tx, after int64, limit int) ([]retained, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT position, version, payload, stored_at FROM `+t.deferred+`
		 WHERE position > ? ORDER BY position LIMIT ?`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("statelog: read the retained records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []retained
	for rows.Next() {
		var r retained
		var version, stored int64
		if err := rows.Scan(&r.position, &version, &r.payload, &stored); err != nil {
			return nil, fmt.Errorf("statelog: scan a retained record: %w", err)
		}
		r.version = int(version)
		r.storedAt = store.DecodeTime(stored)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("statelog: iterate the retained records: %w", err)
	}
	return out, nil
}

// release drops a retained record and its scope index, in this transaction —
// the child first, for the reason [tables.retain] gives about the supersede:
// there is no foreign key, and a scope row that outlives its record is a
// deferral this node reports and can never clear.
func (t tables) release(ctx context.Context, tx *sql.Tx, p Position) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+t.scope+` WHERE position = ?`, p.Packed()); err != nil {
		return fmt.Errorf("statelog: release the scope of %s: %w", p, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+t.deferred+` WHERE position = ?`, p.Packed()); err != nil {
		return fmt.Errorf("statelog: release the record at %s: %w", p, err)
	}
	return nil
}

// deferredIn answers whether this node holds a record it cannot decode whose
// declared scope intersects s.
//
// TWO CLAUSES, because containment runs both ways and neither finds the
// other's case. A stored path in the query's CLOSURE is one that covers
// something the query is about — a record deferred on the container. A stored
// path BENEATH one of the query's roots is one the query covers — a record
// deferred on an object inside what this operation is rewriting. A probe with
// only the first is blind to exactly the neighbour whose row a deferred record
// left stale, and that is silent data loss rather than a wrong answer.
func (t tables) deferredIn(ctx context.Context, tx *sql.Tx, s ScopeSet) (Deferral, bool, error) {
	return t.deferredBelow(ctx, tx, s, nil)
}

// deferredBelow is [tables.deferredIn] over the retained records BELOW a
// position only.
//
// It is what the reprocess asks: whether a record this build can now read is
// still covered by one it cannot, and only a record EARLIER in the log can
// cover it — the live loop retained it against what the table held at the
// time, and the table held nothing above it then. A probe over the whole
// table would let a later retained record hold an earlier one back for ever.
// nil is the whole table.
func (t tables) deferredBelow(ctx context.Context, tx *sql.Tx, s ScopeSet, below *Position) (Deferral, bool, error) {
	closure := s.Closure()
	roots := s.Roots()
	if len(closure) == 0 {
		return Deferral{}, false, nil
	}

	args := make([]any, 0, len(closure)+len(roots)*2+1)
	for _, p := range closure {
		args = append(args, p)
	}
	q := `SELECT d.position, d.version FROM ` + t.scope + ` s
	      JOIN ` + t.deferred + ` d ON d.position = s.position
	      WHERE (s.path IN (` + placeholders(len(closure)) + `)`
	for _, r := range roots {
		q += ` OR s.path = ? OR s.path LIKE ? ESCAPE '\'`
		args = append(args, r, store.LikePrefix(r+ScopeSeparator))
	}
	q += `)`
	if below != nil {
		q += ` AND d.position < ?`
		args = append(args, below.Packed())
	}
	q += ` ORDER BY d.position LIMIT 1`

	var packed int64
	var version int64
	err := tx.QueryRowContext(ctx, q, args...).Scan(&packed, &version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Deferral{}, false, nil
	case err != nil:
		return Deferral{}, false, fmt.Errorf("statelog: probe the deferred scope: %w", err)
	}
	at := Position{
		Stream:     t.stream,
		Generation: uint32(packed / GenerationStride),
		Seq:        uint64(packed % GenerationStride),
	}
	scope, err := t.scopeOf(ctx, tx, packed)
	if err != nil {
		return Deferral{}, false, err
	}
	return Deferral{Position: at, Version: int(version), Scope: scope}, true, nil
}

// scopeOf reads a deferred record's declared scope, so a refusal can name what
// is actually stale rather than the whole domain.
func (t tables) scopeOf(ctx context.Context, tx *sql.Tx, packed int64) (ScopeSet, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT path FROM `+t.scope+` WHERE position = ? ORDER BY path`, packed)
	if err != nil {
		return ScopeSet{}, fmt.Errorf("statelog: read a deferred record's scope: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return ScopeSet{}, fmt.Errorf("statelog: scan a deferred scope: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return ScopeSet{}, fmt.Errorf("statelog: iterate a deferred scope: %w", err)
	}
	return ScopeSet{Paths: out}, nil
}

// oldestDeferred is the earliest record this node could not decode, which is
// what the node's own applied-through position is derived from and what an
// operator's "which build do I need" question is answered with.
func (t tables) oldestDeferred(ctx context.Context, tx *sql.Tx) (Deferral, bool, error) {
	var packed int64
	var version int64
	err := tx.QueryRowContext(ctx,
		`SELECT position, version FROM `+t.deferred+` ORDER BY position LIMIT 1`).
		Scan(&packed, &version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Deferral{}, false, nil
	case err != nil:
		return Deferral{}, false, fmt.Errorf("statelog: read the oldest deferred record: %w", err)
	}
	return Deferral{
		Position: Position{
			Stream:     t.stream,
			Generation: uint32(packed / GenerationStride),
			Seq:        uint64(packed % GenerationStride),
		},
		Version: int(version),
	}, true, nil
}

// placeholders renders n bind markers.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// arbitrates reports whether a subject kind carries a per-subject expectation,
// and therefore whether an anchor is written for it.
func arbitrates(kinds []string, kind string) bool {
	return slices.Contains(kinds, kind)
}
