package atlasdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier is a PURE FUNCTION of the log: it reads no config, clock,
// keyring or other domain; every instant it writes is the broker's or one the
// record carries; it judges no value, so two builds write one row; a record the
// rows no longer admit writes nothing rather than failing; and every write is
// guarded so a redelivery writes nothing it has not written.

// Applier writes this node's copy of the atlas estate. It holds nothing.
type Applier struct{}

// NewApplier builds the atlas applier.
func NewApplier() *Applier { return &Applier{} }

// Committed is the post-commit half, and there is nothing to tell: a reader
// asks again.
func (*Applier) Committed(context.Context) {}

// Gated reports a record that must produce no rows: one its writer published
// inside that writer's eviction window, and one a purge marker holds (a record
// about a purged entry or carrying a purged value, its purge's own excepted),
// which it counts on the marker as a return the purge blocked.
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (
	statelog.Reason, bool, error) {

	if rec.Writer != "" {
		evicted, err := evictedAt(ctx, tx, rec.Writer, rec.Position.Packed())
		if err != nil {
			return "", false, err
		}
		if evicted {
			return statelog.ReasonEvicted, true, nil
		}
	}
	m, held, err := heldBy(ctx, tx, markersOf(rec.Subject, rec.Payload), rec.OpID)
	if err != nil || !held {
		return "", false, err
	}
	// A RETURN IS COUNTED ONCE, AND ONLY ABOVE THE PURGE: a redelivered record
	// the purge already erased is dropped without being counted again.
	packed := rec.Position.Packed()
	if _, err := tx.ExecContext(ctx, `
		UPDATE atlas_purges SET rejects = rejects + 1, last_reject_at = ?, last_reject_position = ?
		WHERE kind = ? AND id = ? AND position < ? AND last_reject_position < ?`,
		store.EncodeTime(rec.StoredAt), packed, string(m.kind), m.id, packed, packed); err != nil {
		return "", false, fmt.Errorf("atlasdomain: count a return the purge of %s %s blocked: %w",
			m.kind, m.id, err)
	}
	return statelog.ReasonDeleted, true, nil
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record   MutationRecord
	position statelog.Position
	packed   int64

	// brokerAt is the broker's own instant for this record, the only instant
	// a row records of its own.
	brokerAt time.Time
}

// at is the broker instant as it is stored.
func (c applyContext) at() int64 { return store.EncodeTime(c.brokerAt) }

// Apply writes one record's rows, dispatched on the subject kind.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("atlasdomain: decode the record at %s: %w", rec.Position, err)
	}
	c := applyContext{record: record, position: rec.Position, packed: rec.Position.Packed(),
		brokerAt: opts.StoredAt}
	payload, err := DecodeMutation(record)
	if err != nil {
		return 0, fmt.Errorf("atlasdomain: the record at %s: %w", rec.Position, err)
	}
	switch record.Subject.Kind {
	case KindBarrier:
		return 0, nil
	case KindTrimmed:
		// The framework removes this log's anchors below the floor in this
		// same transaction (ADR-0080); this domain writes nothing for it.
		return 0, nil
	case KindEviction:
		return applyEviction(ctx, tx, c, payload)
	case KindGeneration:
		return applyGeneration(ctx, tx, c, payload)
	case KindEntry:
		if record.Op == OpPurge {
			return applyPurge(ctx, tx, c, markerEntry)
		}
		return applyClaim(ctx, tx, c, payload)
	case KindValue:
		return applyPurge(ctx, tx, c, markerValue)
	case KindExpiry:
		return applyExpire(ctx, tx, c, payload)
	}
	// A kind this build does not know at a version it reads is a writer
	// publishing a kind it never declared; failing makes that visible.
	return 0, fmt.Errorf("atlasdomain: %s is not a kind this build applies, and the record "+
		"at %s claims version %d", record.Subject.Kind, rec.Position, record.V)
}

// applyClaim writes one claim on its entry, creates or moves the entry, and
// keeps one current claim per voice on each slot: the newer of the two by when
// its source saw it, then by its place on the log, and the other is history.
func applyClaim(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	cl, ok := payload.(Claim)
	switch {
	case !ok:
		return 0, fmt.Errorf("atlasdomain: the claim record at %s carries a %T", c.position, payload)
	case cl.ID == "" || cl.Slot == "" || cl.Entry.Kind == "" || cl.Entry.Key == "" || cl.Source.Kind == "",
		cl.Target != nil && (cl.Target.ID == "" || cl.Target.Kind == "" || cl.Target.Key == ""):
		return 0, fmt.Errorf("atlasdomain: the claim record at %s does not name its claim, slot, "+
			"entry, target and source", c.position)
	}
	entry := c.record.Subject.ID
	observed := c.brokerAt
	if !cl.ObservedAt.IsZero() && cl.ObservedAt.Before(observed) {
		observed = cl.ObservedAt.UTC()
	}
	var targetID, targetKind, targetKey, targetDigest string
	if cl.Target != nil {
		targetID, targetKind = cl.Target.ID, string(cl.Target.Kind)
		targetKey, targetDigest = cl.Target.Key, Digest(cl.Target.Key)
	}
	var value Value
	var valueDigest string
	if cl.Value != nil {
		value, valueDigest = *cl.Value, Digest(cl.Value.Text)
	}
	actor := c.record.actor()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO atlas_claims (claim_id, entry_id, slot, facet, fact, relation, target_id,
			target_kind, target_key, target_key_digest, external, target_name, value_type,
			value_text, value_private, value_digest, denied, source_kind, source_name, by,
			by_kind, observed_at, at, position, claim)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (claim_id) DO NOTHING`,
		cl.ID, entry, cl.Slot, string(cl.Facet), string(cl.Fact), string(cl.Relation), targetID,
		targetKind, targetKey, targetDigest, string(cl.External), cl.Name, string(value.Type),
		value.Text, boolInt(value.Private), valueDigest, boolInt(cl.Denied), string(cl.Source.Kind),
		cl.Source.Name, actor.Name, string(actor.Kind), store.EncodeTime(observed), c.at(), c.packed,
		string(c.record.Mutation))
	if err != nil {
		return 0, fmt.Errorf("atlasdomain: record claim %s at %s: %w", cl.ID, c.position, err)
	}
	written := affected(res)
	if written == 0 {
		return 0, nil
	}
	moved, err := tx.ExecContext(ctx, `
		INSERT INTO atlas_entries (entry_id, kind, key, key_digest, version, exists_position,
			retired, retired_at, last_at, at, position)
		VALUES (?, ?, ?, ?, ?, 0, 0, 0, ?, ?, ?)
		ON CONFLICT (entry_id) DO UPDATE SET version = excluded.version,
			last_at = MAX(atlas_entries.last_at, excluded.last_at)
		WHERE excluded.version > atlas_entries.version`,
		entry, string(cl.Entry.Kind), cl.Entry.Key, Digest(cl.Entry.Key), c.packed, c.at(),
		c.at(), c.packed)
	if err != nil {
		return 0, fmt.Errorf("atlasdomain: record entry %s at %s: %w", entry, c.position, err)
	}
	written += affected(moved)
	replaced, err := supersede(ctx, tx, c, cl, entry, store.EncodeTime(observed))
	if err != nil {
		return 0, err
	}
	written += replaced
	if cl.Facet == atlas.FacetExists && cl.Source.Kind == atlas.SourcePerson {
		// A PERSON'S NEWEST WORD ON THE ENTRY ITSELF decides whether it is
		// retired, in log order, which is what the expiry reads.
		retiredAt := int64(0)
		if cl.Denied {
			retiredAt = c.at()
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE atlas_entries SET exists_position = ?, retired = ?, retired_at = ?
			WHERE entry_id = ? AND exists_position < ?`, c.packed, boolInt(cl.Denied), retiredAt,
			entry, c.packed)
		if err != nil {
			return 0, fmt.Errorf("atlasdomain: record whether %s is retired at %s: %w",
				entry, c.position, err)
		}
		written += affected(res)
	}
	return written, nil
}

// supersede keeps one current claim for the voice on the slot: of the claim
// just written and the voice's current one, the older is marked replaced at
// this record. Claims are kept as history until the retention takes them.
func supersede(ctx context.Context, tx *sql.Tx, c applyContext, cl Claim, entry string,
	observed int64) (int, error) {

	var (
		current               string
		currentAt, currentPos int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT claim_id, observed_at, position FROM atlas_claims INDEXED BY atlas_claims_voice_idx
		WHERE entry_id = ? AND slot = ? AND source_kind = ? AND source_name = ?
			AND replaced = 0 AND claim_id != ?`,
		entry, cl.Slot, string(cl.Source.Kind), cl.Source.Name, cl.ID).Scan(&current, &currentAt, &currentPos)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("atlasdomain: read the current claim on %s's slot at %s: %w",
			entry, c.position, err)
	}
	older := current
	if observed < currentAt || (observed == currentAt && c.packed < currentPos) {
		older = cl.ID
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE atlas_claims SET replaced = 1, superseded_at = ?, superseded_position = ?
		WHERE claim_id = ? AND replaced = 0`, c.at(), c.packed, older)
	if err != nil {
		return 0, fmt.Errorf("atlasdomain: mark claim %s replaced at %s: %w", older, c.position, err)
	}
	return affected(res), nil
}

// applyPurge erases a purged entry or value everywhere, its history included,
// and writes its marker whether or not anything was there, so a record about it
// that arrives later is gated on every data node alike. An entry whose last
// claim went with it goes too.
func applyPurge(ctx context.Context, tx *sql.Tx, c applyContext, kind markerKind) (int, error) {
	id := c.record.Subject.ID
	actor := c.record.actor()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO atlas_purges (kind, id, at, by, by_kind, purge_record_id, rejects,
			last_reject_at, position)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, ?)
		ON CONFLICT (kind, id) DO NOTHING`,
		string(kind), id, c.at(), actor.Name, string(actor.Kind), c.record.OpID, c.packed)
	if err != nil {
		return 0, fmt.Errorf("atlasdomain: mark %s %s purged at %s: %w", kind, id, c.position, err)
	}
	written := affected(res)
	erase := purgeStatements[kind]
	touched, err := entriesOf(ctx, tx, erase.claims, id)
	if err != nil {
		return 0, fmt.Errorf("atlasdomain: find what the purge of %s %s reaches at %s: %w",
			kind, id, c.position, err)
	}
	for _, statement := range []string{erase.claims, erase.entries} {
		res, err := tx.ExecContext(ctx, `DELETE `+statement, repeat(id, statement)...)
		if err != nil {
			return 0, fmt.Errorf("atlasdomain: purge %s %s at %s: %w", kind, id, c.position, err)
		}
		written += affected(res)
	}
	for _, entry := range touched {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM atlas_entries WHERE entry_id = ?
				AND NOT EXISTS (SELECT 1 FROM atlas_claims WHERE atlas_claims.entry_id = ?)`,
			entry, entry)
		if err != nil {
			return 0, fmt.Errorf("atlasdomain: drop entry %s the purge emptied at %s: %w",
				entry, c.position, err)
		}
		written += affected(res)
	}
	return written, nil
}

// purgeStatement is what one purge deletes: the claims it reaches, and the
// entries it erases, each a FROM clause whose every parameter is the purged id.
type purgeStatement struct{ claims, entries string }

// purgeStatements are the two purges. An entry's: every claim on it and every
// claim naming it, and the entry. A value's: every claim on an entry it is the
// key of, every claim naming such an entry, every claim carrying it, and those
// entries.
var purgeStatements = map[markerKind]purgeStatement{
	markerEntry: {
		claims:  `FROM atlas_claims WHERE entry_id = ? OR target_id = ?`,
		entries: `FROM atlas_entries WHERE entry_id = ?`,
	},
	markerValue: {
		claims: `FROM atlas_claims WHERE value_digest = ? OR target_key_digest = ?
			OR entry_id IN (SELECT entry_id FROM atlas_entries WHERE key_digest = ?)`,
		entries: `FROM atlas_entries WHERE key_digest = ?`,
	},
}

// entriesOf is every entry the claims a FROM clause reaches are on, in order.
func entriesOf(ctx context.Context, tx *sql.Tx, from, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT entry_id `+from+` ORDER BY entry_id`,
		repeat(id, from)...)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, entry)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// repeat is id once for every parameter a statement takes.
func repeat(id, statement string) []any {
	out := []any{}
	for _, r := range statement {
		if r == '?' {
			out = append(out, id)
		}
	}
	return out
}

// expiryStatements are what an expiry deletes, each through the instant the
// record names: every claim its voice replaced before then, and every retired
// entry nothing was recorded about since, with all its claims.
var expiryStatements = []string{
	`DELETE FROM atlas_claims WHERE replaced = 1 AND superseded_at < ?`,
	`DELETE FROM atlas_claims WHERE entry_id IN (SELECT entry_id FROM atlas_entries
		INDEXED BY atlas_entries_retired_idx WHERE retired = 1 AND last_at < ?)`,
	`DELETE FROM atlas_entries WHERE retired = 1 AND last_at < ?`,
}

// applyExpire removes the history past the retention through the hour the
// record names.
func applyExpire(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	e, ok := payload.(Expire)
	if !ok || e.Through.IsZero() {
		return 0, fmt.Errorf("atlasdomain: the expiry at %s names no instant", c.position)
	}
	through := store.EncodeTime(e.Through)
	written := 0
	for _, statement := range expiryStatements {
		res, err := tx.ExecContext(ctx, statement, through)
		if err != nil {
			return 0, fmt.Errorf("atlasdomain: expire through %s at %s: %w", e.Through, c.position, err)
		}
		written += affected(res)
	}
	return written, nil
}

// applyEviction records a node's removal from this log, or its readmission, as
// an INVERSE COMMIT rather than a delete.
func applyEviction(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	e, ok := payload.(Eviction)
	if !ok || e.NodeID == "" {
		return 0, fmt.Errorf("atlasdomain: the eviction at %s names no node", c.position)
	}
	var (
		res sql.Result
		err error
	)
	if e.Readmitted {
		res, err = tx.ExecContext(ctx, `
			UPDATE atlas_evictions SET readmitted_position = ?, version = ?
			WHERE node_id = ? AND version < ?`, c.packed, c.packed, e.NodeID, c.packed)
	} else {
		// A SECOND EVICTION CLEARS THE READMISSION, so the evicted,
		// readmitted, evicted sequence reads as three facts.
		res, err = tx.ExecContext(ctx, `
			INSERT INTO atlas_evictions
				(node_id, at, by, from_position, readmitted_position, version)
			VALUES (?, ?, ?, ?, NULL, ?)
			ON CONFLICT (node_id) DO UPDATE SET
				at = excluded.at, by = excluded.by,
				from_position = excluded.from_position,
				readmitted_position = NULL, version = excluded.version
			WHERE excluded.version > atlas_evictions.version`,
			e.NodeID, c.at(), e.EvictedBy, c.packed, c.packed)
	}
	if err != nil {
		return 0, fmt.Errorf("atlasdomain: apply the eviction of %s at %s: %w",
			e.NodeID, c.position, err)
	}
	return affected(res), nil
}

// applyGeneration writes a reanchor's audit row, create-only.
func applyGeneration(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	g, ok := payload.(Generation)
	if !ok {
		return 0, fmt.Errorf("atlasdomain: the generation record at %s carries a %T", c.position, payload)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO atlas_log_generations
			(generation, at, by, new_stream_created_at, prev_last_seq_seen, record_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (generation) DO NOTHING`,
		g.Generation, c.at(), g.By, store.EncodeTime(g.StreamAt),
		int64(g.PrevHighest), c.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("atlasdomain: record generation %d at %s: %w", g.Generation, c.position, err)
	}
	return affected(res), nil
}

// affected is how many rows a statement wrote, the row budget's input.
func affected(res sql.Result) int {
	n, _ := res.RowsAffected()
	return int(n)
}

// boolInt is a flag as a row stores it.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
