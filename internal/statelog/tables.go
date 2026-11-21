package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// tables is the framework's SQL over its own two tables and the two a domain
// names. The domain owns those two's schema (its migration); the framework owns
// their statements, one implementation for all domains, because they hold
// framework facts and contract 2 writes them in the applier's transaction
// beside the rows: the operation ledger (`Divergent`: in every snapshot, out of
// the identity claim, since only `applied_at` differs) and the deferred-record
// table (`Local`).
//
// Table NAMES are interpolated, since a driver cannot bind one, so each is
// checked against [validIdentifier] at construction: unchecked, a domain could
// name a table with a quote in it.
type tables struct {
	stream   string
	prefix   string
	ops      string
	deferred string
	scope    string

	// derivation is the rule set this applier derives at ([Deriver]),
	// stamped on a checkpoint row only when the row is FIRST written: this
	// build derived every row that transaction commits. Zero where the
	// writer is not an applier (a reanchor) reads as "unknown", costing at
	// most one re-derivation at the next boot.
	derivation int
}

// subjectOf is the WIRE subject an anchor is keyed on, spelled once: the
// applier advances the anchor and the publisher reads it, so two spellings
// leave the publisher never finding one, and every arbitrated write exhausts
// its rounds and reports a conflict on an object nobody else touched.
func (t tables) subjectOf(s Subject) string { return wireSubject(t.prefix, s) }

// wireSubject is a subject as the broker carries it on the log with subject
// prefix prefix. The grammar's ([topics.LogSubject]), never spelled here: the
// publisher, the applier's anchor and the eviction probe each compose one, and
// a second spelling is a key two of them disagree about.
func wireSubject(prefix string, s Subject) string {
	return topics.LogSubject(prefix, s.Kind, s.ID)
}

// validIdentifier reports whether a domain may call a table name. Narrower than
// SQL allows: the name is interpolated, so the set is what is unambiguous, not
// what the parser tolerates.
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

// newTables is the framework's statements over domain d's tables on the ONE log
// spec names, whose stream keys the checkpoint and whose prefix every anchor.
func newTables(d Domain, spec StreamSpec) (tables, error) {
	t, err := domainTables(d)
	if err != nil {
		return tables{}, err
	}
	if err := spec.Instantiates(d); err != nil {
		return tables{}, err
	}
	t.stream, t.prefix = spec.Name, spec.SubjectPrefix
	return t, nil
}

// domainTables is domain d's own table names, checked, with no log bound: for
// what is kept per DOMAIN rather than per log, the operation ledger and its
// lost-rows watermark, keyed by the ledger's table.
func domainTables(d Domain) (tables, error) {
	t := tables{
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

	// storedAt is the broker's instant for the record this node consumed
	// at the checkpoint's sequence, which [Runner.VerifyCheckpoint]
	// compares with the log's; zero at sequence 0, or where a reanchor
	// placed it at a sequence holding no record.
	storedAt time.Time

	// void is what the reanchor that placed this checkpoint made void.
	void voidRange
}

// encodeInstant is a broker instant as a checkpoint column holds it: zero for
// an unknown one, which [store.EncodeTime] would write as the year one, read
// back as a real instant nothing could equal.
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

// voidRange is what the reanchor that placed a checkpoint made VOID, in two
// rules: the generations strictly between after and before, which it ABANDONED
// ([ReanchorPlan.From]); and, for a RESTORED reanchor, every record after
// staleAfter (the generation record it appended) in a generation below before,
// the one it opened ([ReanchorPlan.StaleAfter]). All zero where no such
// reanchor placed the checkpoint.
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

// setCursor writes the checkpoint in the SAME transaction as the rows it
// covers (package doc, "The checkpoint commits with the rows").
//
// storedAt is the broker's instant for the record this transaction consumed at
// p, by which the checkpoint NAMES its record ([cursorRow.storedAt]). applied is
// the highest record version this transaction applied, zero for none, and
// raises the row's own ([tables.raiseApplied]).
func (t tables) setCursor(ctx context.Context, tx *sql.Tx, p Position, created, storedAt,
	now time.Time, applied int) error {

	// THE DERIVATION IS WRITTEN ON INSERT ONLY: an existing row's moves
	// only in [tables.setDerivation], beside the rows it re-derived, so a
	// checkpoint advancing over an old build's rows never claims this
	// build derived them.
	_, err := tx.ExecContext(ctx, `
		INSERT INTO statelog_cursor
			(stream, generation, seq, stream_created_at, stored_at, updated_at,
			 applied_version, derivation)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (stream) DO UPDATE SET
			generation        = excluded.generation,
			seq               = excluded.seq,
			stream_created_at = excluded.stream_created_at,
			stored_at         = excluded.stored_at,
			updated_at        = excluded.updated_at,
			applied_version   = `+raisedApplied("excluded.applied_version"),
		t.stream, int64(p.Generation), int64(p.Seq),
		store.EncodeTime(created), encodeInstant(storedAt), store.EncodeTime(now),
		int64(applied), t.derivation)
	if err != nil {
		return fmt.Errorf("statelog: write the cursor at %s: %w", p, err)
	}
	return nil
}

// raiseApplied raises the highest record version this stream's rows were
// applied from to at least v, in a transaction that applied a record without
// moving the checkpoint: a build reprocessing what an earlier one retained.
//
// A snapshot's manifest states it, and a joiner refuses rows holding a record
// version its build cannot read, since arriving past that record it could never
// apply, defer or reprocess it. Not the donor BUILD's version: that names
// versions no record carries yet, so a rolling upgrade's older nodes would
// refuse every upgraded donor.
func (t tables) raiseApplied(ctx context.Context, tx *sql.Tx, v int) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE statelog_cursor SET applied_version = `+raisedApplied("?")+`
		WHERE stream = ?`, int64(v), t.stream)
	if err != nil {
		return fmt.Errorf("statelog: raise the applied record version to %d: %w", v, err)
	}
	return nil
}

// raisedApplied is the one expression raising the row's applied record version
// to at least by: only ever raised, since a lower-version record applied later
// says nothing about the ones the rows already hold.
func raisedApplied(by string) string {
	return `MAX(statelog_cursor.applied_version, ` + by + `)`
}

// readDerivation reads the rule set this stream's rows were derived by,
// reporting false when there is no checkpoint — and so no rows — yet.
func (t tables) readDerivation(ctx context.Context, tx *sql.Tx) (int, bool, error) {
	var v int
	err := tx.QueryRowContext(ctx,
		`SELECT derivation FROM statelog_cursor WHERE stream = ?`, t.stream).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("statelog: read the derivation: %w", err)
	}
	return v, true, nil
}

// setDerivation records that this stream's rows are now derived at v, in the
// transaction that derived them.
func (t tables) setDerivation(ctx context.Context, tx *sql.Tx, v int) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE statelog_cursor SET derivation = ? WHERE stream = ?`, v, t.stream); err != nil {
		return fmt.Errorf("statelog: record derivation %d: %w", v, err)
	}
	return nil
}

// reanchorCursor writes the checkpoint a reanchor places at p, keyed to
// created, naming the log's record at p by storedAt (zero where there is none),
// with the [voidRange] it made: generations strictly between from and p's own,
// and for a restored reanchor the lower-generation records after staleAfter
// (zero for no such rule).
//
// NOT A FLAG ON setCursor: the applier's checkpoint must leave the void range
// alone, since a node part-way through an abandoned generation keeps voiding it
// after a restart, while a reanchor must REPLACE it, since an earlier
// reanchor's range says nothing about the log this one follows. Nor does it
// write the derivation: it re-derives no rows, so a created row takes zero,
// "unknown" ([tables.derivation]).
func (t tables) reanchorCursor(ctx context.Context, tx *sql.Tx, p Position,
	created, storedAt time.Time, from uint32, staleAfter uint64, now time.Time) error {

	// A CREATED ROW STARTS AT APPLIED VERSION ZERO (no row means nothing
	// was ever applied on the stream); a replaced row keeps its own, since
	// a reanchor keeps every row.
	_, err := tx.ExecContext(ctx, `
		INSERT INTO statelog_cursor
			(stream, generation, seq, stream_created_at, stored_at, updated_at,
			 void_after, void_before, stale_after, applied_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
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
// MAX, not assignment: a deferred record is reprocessed at its ORIGINAL
// position, below a successor already applied, and moving the anchor backwards
// hands the next writer an expectation the broker refuses for ever.
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

// anchor reads a subject's arbitration anchor by exact key ([anchorSQL]).
func (t tables) anchor(ctx context.Context, tx *sql.Tx, subject string, gen uint32) (Position, error) {
	var packed int64
	err := tx.QueryRowContext(ctx, anchorSQL, t.stream, subject).Scan(&packed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Position{Stream: t.stream, Generation: gen}, nil
	case err != nil:
		return Position{}, fmt.Errorf("statelog: read the anchor on %s: %w", subject, err)
	}
	return Unpack(t.stream, packed), nil
}

// The anchor table's three statements, each held to its index by
// TestEveryAnchorStatementSeeksItsKey: with no statistics (no deployment
// gathers any) the pinned driver's planner picks between two indexes that both
// lead with the stream on the stream alone.
const (
	// anchorSQL reads ONE subject's anchor by the primary key (stream,
	// subject), named because otherwise the planner takes
	// statelog_anchor_swept_idx on the stream and walks every anchor in the
	// trim's window on every write's snapshot, slower with each subject
	// written (BenchmarkReadingASubjectsAnchor).
	anchorSQL = `SELECT anchor FROM statelog_anchor INDEXED BY sqlite_autoindex_statelog_anchor_1
		WHERE stream = ? AND subject = ?`

	// sweepAnchorsSQL and anchorsBelowSQL are ranges over
	// statelog_anchor_swept_idx (stream, anchor), shipped for these two.
	sweepAnchorsSQL = `DELETE FROM statelog_anchor WHERE rowid IN (
		SELECT rowid FROM statelog_anchor
		WHERE stream = ? AND anchor < ?
		ORDER BY anchor LIMIT ?)`
	anchorsBelowSQL = `SELECT COUNT(*) FROM (
		SELECT 1 FROM statelog_anchor
		WHERE stream = ? AND anchor < ? LIMIT ?)`
)

// sweepAnchors removes up to [AnchorsPerTrimmedRecord] of this stream's
// anchors below below, LOWEST FIRST, and reports how many went — a trimmed
// record's whole effect (ADR-0080). The comparison is PACKED, so every anchor
// of an earlier generation goes too; the publisher already ignores one
// ([Snap.anchorIn]).
//
// Which rows go depends on the table's contents alone, so nodes holding the
// same anchors remove the same rows: one position is one record on one
// subject, so no two anchors are equal and the order has no ties.
func (t tables) sweepAnchors(ctx context.Context, tx *sql.Tx, below Position) (int, error) {
	res, err := tx.ExecContext(ctx, sweepAnchorsSQL, t.stream, below.Packed(), AnchorsPerTrimmedRecord)
	if err != nil {
		return 0, fmt.Errorf("statelog: remove the anchors on %s below %s: %w", t.stream, below, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("statelog: count the anchors removed on %s: %w", t.stream, err)
	}
	return int(n), nil
}

// anchorsBelow counts this stream's anchors below below, up to limit: the
// caller needs how many records to append, which limit already caps.
func (t tables) anchorsBelow(ctx context.Context, db ReadEstate, below Position, limit int) (int, error) {
	var n int
	err := db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, anchorsBelowSQL, t.stream, below.Packed(), limit).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("statelog: count the anchors on %s below %s: %w", t.stream, below, err)
	}
	return n, nil
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

// OpsPurgeBatch bounds how many rows one sweep statement deletes. The
// applier's connection is pinned to the same file, so one statement over a
// backlog (a node back from a month away) would hold the writer throughout.
// 2000 rows of a five-column, TEXT-keyed table is a few hundred KB of pages,
// single-digit milliseconds of hold, and a month still clears in one
// maintenance tick: [store.EventPurgeBatch]'s reasoning, at a larger batch
// because these rows are a hundredth the size.
const OpsPurgeBatch = 2000

// purgeOps deletes operation rows applied before cutoff, reporting how many
// went: a range over the age, which `<domain>_ops_swept_idx` serves, in
// [OpsPurgeBatch] batches, each its own transaction so the applier is never
// queued behind the whole backlog.
//
// A BATCH THAT DELETED ANYTHING RECORDS WHAT IT FORGOT in its own transaction
// ([tables.markLost]): the publisher tells a never-written row from a swept one
// by that watermark ([tables.lostBefore]), and one written apart from the
// delete could trail the rows gone, re-deciding an operation that landed.
func (t tables) purgeOps(ctx context.Context, db Estate, cutoff time.Time) (int64, error) {
	return t.purgeOpsWhere(ctx, db, func(tx *sql.Tx) error {
		return t.markLost(ctx, tx, cutoff)
	}, `applied_at < ?`, store.EncodeTime(cutoff))
}

// purgeOpsOfKind deletes the operation rows on one subject KIND applied before
// cutoff, reporting how many went.
//
// The ledger stores the wire subject (prefix, kind, id), so a kind is the
// subjects between `<kind>.` and the next character up, plus the bare kind. It
// seeks a `(subject, applied_at)` index the DOMAIN ships only when it declares
// a kind horizon.
//
// ITS LOSS IS THE KIND'S ([tables.markLostKind]): recorded as the table's, an
// hourly sweep of one kind would make every other kind's operation older than
// an hour answer `unknown` unpublished (replicated migration 0041).
func (t tables) purgeOpsOfKind(ctx context.Context, db Estate, kind string,
	cutoff time.Time) (int64, error) {

	whole := t.prefix + "." + kind
	return t.purgeOpsWhere(ctx, db, func(tx *sql.Tx) error {
		return t.markLostKind(ctx, tx, kind, cutoff)
	}, `(subject = ? OR (subject >= ? AND subject < ?)) AND applied_at < ?`,
		whole, whole+".", whole+"/", store.EncodeTime(cutoff))
}

// purgeOpsWhere is both sweeps' loop over one predicate, recording what each
// batch that deleted anything forgot through mark, in that batch's own
// transaction.
func (t tables) purgeOpsWhere(ctx context.Context, db Estate, mark func(*sql.Tx) error,
	predicate string, args ...any) (int64, error) {

	if t.ops == "" {
		return 0, nil
	}
	var total int64
	for {
		var deleted int64
		err := db.Tx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `
				DELETE FROM `+t.ops+` WHERE rowid IN (
					SELECT rowid FROM `+t.ops+` WHERE `+predicate+` LIMIT ?)`,
				append(args, OpsPurgeBatch)...)
			if err != nil {
				return err
			}
			deleted, err = res.RowsAffected()
			if err != nil || deleted == 0 {
				return err
			}
			return mark(tx)
		})
		if err != nil {
			return total, fmt.Errorf("statelog: sweep %s: %w", t.ops, err)
		}
		total += deleted
		if deleted < OpsPurgeBatch {
			// A SHORT BATCH IS THE END: stopping only at zero would
			// cost an empty statement after every sweep.
			return total, nil
		}
	}
}

// markLost records that the ops table may have lost rows applied before
// before, in the caller's transaction, which must be the one that loses them.
// MONOTONE, so a later loss with an earlier instant (a clock stepped back, a
// shorter horizon) never un-forgets rows an earlier one lost.
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

// markLostKind records that the ops table may have lost rows of one subject
// KIND applied before before, as [tables.markLost] does for the table: the
// watermark of a kind a domain sweeps sooner than the rest ([OpsHorizon.Kinds],
// replicated migration 0041).
func (t tables) markLostKind(ctx context.Context, tx *sql.Tx, kind string,
	before time.Time) error {

	if t.ops == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO statelog_ops_lost_kind (ops_table, subject_kind, lost_before)
		VALUES (?, ?, ?)
		ON CONFLICT (ops_table, subject_kind) DO UPDATE SET
			lost_before = MAX(lost_before, excluded.lost_before)`,
		t.ops, kind, store.EncodeTime(before))
	if err != nil {
		return fmt.Errorf("statelog: record that %s lost %s rows before %s: %w",
			t.ops, kind, before.UTC().Format(time.RFC3339Nano), err)
	}
	return nil
}

// lostBefore answers the instant before which the ops table may have lost
// rows of an operation on a subject of this kind, false when it lost none
// ([Rows.LostBefore]; replicated migrations 0017 and 0041): the later of the
// table's watermark and the kind's own. An empty kind asks the table's alone.
func (t tables) lostBefore(ctx context.Context, tx *sql.Tx, kind string) (time.Time, bool, error) {
	if t.ops == "" {
		return time.Time{}, false, nil
	}
	var table, ofKind sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT
			(SELECT lost_before FROM statelog_ops_lost WHERE ops_table = ?),
			(SELECT lost_before FROM statelog_ops_lost_kind
				WHERE ops_table = ? AND subject_kind = ? AND ? <> '')`,
		t.ops, t.ops, kind, kind).Scan(&table, &ofKind)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("statelog: read how far back %s "+
			"may have lost rows: %w", t.ops, err)
	}
	switch {
	case !table.Valid && !ofKind.Valid:
		return time.Time{}, false, nil
	case !ofKind.Valid || (table.Valid && table.Int64 >= ofKind.Int64):
		return store.DecodeTime(table.Int64), true, nil
	}
	return store.DecodeTime(ofKind.Int64), true, nil
}

// OpEntry is one row of a domain's operation ledger: where an operation this
// node applied landed, and on which WIRE subject (as the anchor is keyed).
//
// THE SUBJECT IS WHAT MAKES A ROW AN ANSWER: an operation id is the caller's,
// and one reused on a second object finds the first's row, so a row answers
// only for a write to its own subject ([Publisher.heldHere]).
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
// opID, stored by the broker at storedAt, at position p ([UnheldTail]).
//
// ALL THREE MUST AGREE: a retry after the restore carries the operation into a
// second record, and the restored log reissues the position to whatever is
// written there next. The instant is the broker's, so the ledger's (what the
// applier consumed) and the log's (what it holds) are the record's own.
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
	return packed == p.Packed() && sameRecord(decodeInstant(recorded), storedAt), nil
}

// retainedRecord answers whether this node consumed and RETAINED the record at
// position p stored by the broker at storedAt: a record held here unread is
// these rows' history as much as one they applied.
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
// ONE TRANSACTION for the record and its scope: a deferral without its scope
// makes rows stale invisibly, so the probe passes, a writer retries at zero,
// and the deferred mutation is overwritten with nothing reporting it.
func (t tables) retain(ctx context.Context, tx *sql.Tx, rec Record, compacted bool,
	maxVariables int) error {
	// A COMPACTED DOMAIN KEYS ON THE SUBJECT, a strict one on the
	// position: its stream keeps one message per subject, so positional
	// retention would reprocess superseded records into state the broker
	// no longer holds.
	key := rec.Position.Packed()
	subject := rec.Subject.String()
	if compacted {
		// THE CHILD GOES FIRST: with no foreign key (a cascade is a
		// delete nobody committed) the scope rows are found through the
		// parent's position, so deleting the parent first strands them.
		// A probe then reports a deferral this node no longer holds, for
		// ever: the read barrier waits and the writer's step 0 refuses to
		// publish, with nothing able to clear either.
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
		key, subject, rec.Subject.Kind, rec.Subject.ID, rec.V, rec.framed,
		store.EncodeTime(rec.StoredAt)); err != nil {
		return fmt.Errorf("statelog: retain the record at %s: %w", rec.Position, err)
	}
	// THE ONE DATA-DRIVEN COLLECTION HERE: a scope is every object path
	// the apply may write, and paths NEST (a task's keys state the task
	// and every key beside it; the tracker's carry up to 64 terms).
	//
	// The error names no path: a chunk carries up to a thousand, so one
	// would be a guess; the engine's own error, carried by %w, names the
	// offending row.
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

// deferredAfter reads up to limit retained records above a position, oldest
// first, so a long-stalled upgrade's backlog is paged rather than loaded whole.
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

// release drops a retained record and its scope index in this transaction, the
// child first for [tables.retain]'s reason.
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
// TWO CLAUSES, since containment runs both ways: a stored path in the query's
// CLOSURE covers what the query is about (a record deferred on the container);
// one BENEATH a query root is covered by it (a record deferred on an object
// inside what this operation rewrites). Without the second the probe is blind
// to the neighbour a deferred record left stale: silent data loss.
func (t tables) deferredIn(ctx context.Context, tx *sql.Tx, s ScopeSet) (Deferral, bool, error) {
	return t.deferredBelow(ctx, tx, s, nil)
}

// deferredBelow is [tables.deferredIn] over the retained records BELOW a
// position only (nil: the whole table). The reprocess asks it whether a record
// it can now read is still covered by one it cannot; only an EARLIER one can,
// as the live loop retained it when the table held nothing above it, and a
// whole-table probe would let a later record hold an earlier back for ever.
func (t tables) deferredBelow(ctx context.Context, tx *sql.Tx, s ScopeSet, below *Position) (Deferral, bool, error) {
	closure := s.Closure()
	roots := s.Roots()
	if len(closure) == 0 {
		return Deferral{}, false, nil
	}

	// THE ROOTS ARE A ROW SET, NEVER A CHAIN OF `OR`s: one root per object
	// written, and a chained `OR` parses LEFT-DEEP, so Turso refuses a
	// batch of fifty-odd objects (`Expression tree is too large (maximum
	// depth 100)`). A `SELECT ? UNION ALL …` CTE is a compound SELECT whose
	// depth does not grow (TestAScopeWithHundredsOfRootsIsProbedRatherThanRefused).
	// Exact matches stay a flat `IN` list, roots included: a root is both a
	// path the query is about and a prefix of those beneath it.
	//
	// THE BINDINGS FOLLOW THE TEXT: the CTE comes first, so its prefixes
	// bind before the exact paths; reversed, no descendant ever matches
	// and LIKE runs case-blind with `_` as a wildcard
	// (TestTheCoverageProbeFindsBothDirectionsAndNothingElse).
	exact := make([]string, 0, len(closure)+len(roots))
	exact = append(exact, closure...)
	exact = append(exact, roots...)
	args := make([]any, 0, len(roots)+len(exact)+1)
	q := `SELECT d.position, d.version FROM ` + t.scope + ` s
	      JOIN ` + t.deferred + ` d ON d.position = s.position
	      WHERE (s.path IN (` + placeholders(len(exact)) + `)`
	if len(roots) > 0 {
		q = `WITH beneath(prefix) AS (` + unionOfPlaceholders(len(roots)) + `) ` + q +
			` OR EXISTS (SELECT 1 FROM beneath
			             WHERE s.path LIKE beneath.prefix ESCAPE '\')`
		for _, r := range roots {
			args = append(args, store.LikePrefix(r+ScopeSeparator))
		}
	}
	for _, p := range exact {
		args = append(args, p)
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

// holdsDeferred reports whether this node holds any record it could not
// decode.
func (t tables) holdsDeferred(ctx context.Context, tx *sql.Tx) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM `+t.deferred+` LIMIT 1`).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("statelog: look for a deferred record: %w", err)
	}
	return true, nil
}

// oldestDeferred is the earliest record this node could not decode, from which
// its applied-through position and an operator's "which build do I need" are
// derived. WITH ITS SCOPE, as [Deferral] promises: an empty one would answer
// "not about this object" for every object there is.
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
	scope, err := t.scopeOf(ctx, tx, packed)
	if err != nil {
		return Deferral{}, false, err
	}
	return Deferral{
		Position: Position{
			Stream:     t.stream,
			Generation: uint32(packed / GenerationStride),
			Seq:        uint64(packed % GenerationStride),
		},
		Version: int(version),
		Scope:   scope,
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

// unionOfPlaceholders is n bound values as a one-column row set: a COMPOUND
// SELECT, whose depth does not grow with n as a chained `OR` or
// `IN (SELECT …)` does ([tables.deferredBelow]).
func unionOfPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(n * len("SELECT ? UNION ALL "))
	for i := range n {
		if i > 0 {
			b.WriteString(" UNION ALL ")
		}
		b.WriteString("SELECT ?")
	}
	return b.String()
}
