package statelog

import (
	"context"
	"database/sql"
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

// readCursor reads this domain's checkpoint, reporting false when the applier
// has never committed on this stream.
func (t tables) readCursor(ctx context.Context, tx *sql.Tx) (Position, time.Time, bool, error) {
	var gen int64
	var seq int64
	var created int64
	err := tx.QueryRowContext(ctx,
		`SELECT generation, seq, stream_created_at FROM statelog_cursor WHERE stream = ?`,
		t.stream).Scan(&gen, &seq, &created)
	switch {
	case err == sql.ErrNoRows:
		return Position{Stream: t.stream}, time.Time{}, false, nil
	case err != nil:
		return Position{}, time.Time{}, false, fmt.Errorf("statelog: read the cursor: %w", err)
	}
	return Position{Stream: t.stream, Generation: uint32(gen), Seq: uint64(seq)},
		store.DecodeTime(created), true, nil
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
func (t tables) setCursor(ctx context.Context, tx *sql.Tx, p Position, created time.Time, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO statelog_cursor (stream, generation, seq, stream_created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (stream) DO UPDATE SET
			generation        = excluded.generation,
			seq               = excluded.seq,
			stream_created_at = excluded.stream_created_at,
			updated_at        = excluded.updated_at`,
		t.stream, int64(p.Generation), int64(p.Seq),
		store.EncodeTime(created), store.EncodeTime(now))
	if err != nil {
		return fmt.Errorf("statelog: write the cursor at %s: %w", p, err)
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
	case err == sql.ErrNoRows:
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

// writeOp records that this node applied an operation at a position.
func (t tables) writeOp(ctx context.Context, tx *sql.Tx, opID, subject string, p Position, now time.Time) error {
	if t.ops == "" || opID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO `+t.ops+` (op_id, subject, position, applied_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (op_id) DO NOTHING`,
		opID, subject, p.Packed(), store.EncodeTime(now))
	if err != nil {
		return fmt.Errorf("statelog: record operation %q at %s: %w", opID, p, err)
	}
	return nil
}

// op answers where an operation was applied on this node.
func (t tables) op(ctx context.Context, tx *sql.Tx, opID string) (Position, bool, error) {
	if t.ops == "" {
		return Position{}, false, nil
	}
	var packed int64
	err := tx.QueryRowContext(ctx,
		`SELECT position FROM `+t.ops+` WHERE op_id = ?`, opID).Scan(&packed)
	switch {
	case err == sql.ErrNoRows:
		return Position{}, false, nil
	case err != nil:
		return Position{}, false, fmt.Errorf("statelog: read operation %q: %w", opID, err)
	}
	return Position{
		Stream:     t.stream,
		Generation: uint32(packed / GenerationStride),
		Seq:        uint64(packed % GenerationStride),
	}, true, nil
}

// retain stores a record this build cannot decode, byte for byte, with every
// term of its scope.
//
// ONE TRANSACTION for the record and its scope, and the rule is not tidiness:
// a deferral recorded without its scope is a record that makes rows stale with
// nothing able to see that it does — so the probe passes, a writer takes the
// retry-at-zero branch, and the deferred mutation is overwritten with nothing
// anywhere reporting it.
func (t tables) retain(ctx context.Context, tx *sql.Tx, rec Record, compacted bool) error {
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
	for _, path := range rec.Scope.Normalised().Paths {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+t.scope+` (position, path) VALUES (?, ?)
			 ON CONFLICT (position, path) DO NOTHING`, key, path); err != nil {
			return fmt.Errorf("statelog: index the deferred scope %q at %s: %w",
				path, rec.Position, err)
		}
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
	closure := s.Closure()
	roots := s.Roots()
	if len(closure) == 0 {
		return Deferral{}, false, nil
	}

	args := make([]any, 0, len(closure)+len(roots)*2)
	for _, p := range closure {
		args = append(args, p)
	}
	q := `SELECT d.position, d.version FROM ` + t.scope + ` s
	      JOIN ` + t.deferred + ` d ON d.position = s.position
	      WHERE s.path IN (` + placeholders(len(closure)) + `)`
	for _, r := range roots {
		q += ` OR s.path = ? OR s.path LIKE ? ESCAPE '\'`
		args = append(args, r, store.LikePrefix(r+ScopeSeparator))
	}
	q += ` ORDER BY d.position LIMIT 1`

	var packed int64
	var version int64
	err := tx.QueryRowContext(ctx, q, args...).Scan(&packed, &version)
	switch {
	case err == sql.ErrNoRows:
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
	case err == sql.ErrNoRows:
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
