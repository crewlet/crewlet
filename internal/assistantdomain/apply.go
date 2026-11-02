package assistantdomain

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
//     value it writes is the record's or the broker's: a sealed value is the
//     writer's envelope written through as bytes, never opened, and every
//     instant a row records of its own is [statelog.ApplyOptions.StoredAt].
//  2. A RECORD THE ROWS NO LONGER ADMIT WRITES NOTHING, NEVER AN ERROR. A turn
//     closed twice, a move from a state the proposal has left, a record about a
//     conversation no create wrote: each applies as no rows on every node
//     alike, and its writer, which decided it from a snapshot of these rows,
//     reads the rows back. An error would stall every node's log on a record
//     that is merely late.
//  3. EVERY WRITE IS GUARDED BY THE RECORD'S POSITION or keyed by an id the
//     record names, so a redelivery writes nothing it has not written.

// Applier writes this node's copy of the assistant estate.
//
// IT HOLDS NOTHING: no keyring, no clock and no listener, which is the first
// rule above made structural. A batch moves nothing another subsystem on this
// node acts on: the person's open drawer learns of a turn's progress on its
// own channel (internal/assistant/live), never from an apply.
type Applier struct{}

// NewApplier builds the assistant applier.
func NewApplier() *Applier { return &Applier{} }

// Committed is the post-commit half, and there is nothing to tell.
func (*Applier) Committed(context.Context) {}

// Gated reports a record that must produce no rows at all, from this same
// transaction: the EVICTION GATE (a record written by a node the fleet evicted
// before its position), the DELETION GATE (a record about a conversation a
// purge or an erasure destroyed, except the purge's own record), and the
// ERASURE GATE (a conversation created for a person whose erasure took
// everything they owned).
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
	if conversation, ok := gatedConversation(rec.Subject.Kind, rec.Subject.ID); ok {
		author, marked, err := deletionMarker(ctx, tx, conversation)
		if err != nil {
			return "", false, err
		}
		// THE ONE EXCEPTION IS THE RECORD THAT WROTE THE MARKER, by its own
		// id: "any purge" would let a second purge through, and by
		// position would fail a republished copy.
		if marked && author != rec.OpID {
			if _, err := tx.ExecContext(ctx, `
				UPDATE assistant_deletions SET rejects = rejects + 1, last_reject_at = ?
				WHERE conversation_id = ?`, store.EncodeTime(rec.StoredAt),
				conversation); err != nil {
				return "", false, fmt.Errorf("assistantdomain: count a gate hit on "+
					"the purged conversation %s: %w", conversation, err)
			}
			return statelog.ReasonDeleted, true, nil
		}
	}
	erased, err := erasedOwnerOf(ctx, tx, rec.Subject.Kind, rec.Op, rec.Payload)
	if err != nil {
		return "", false, err
	}
	if erased {
		return statelog.ReasonDeleted, true, nil
	}
	return "", false, nil
}

// erasedOwnerOf reports a conversation's create for a person whose erasure has
// applied. THE PAYLOAD NAMES THE OWNER, so the gate reads it: the subject names
// the conversation alone. A payload this build cannot read is not judged here:
// such a record is retained rather than applied, and judged when it is.
func erasedOwnerOf(ctx context.Context, tx *sql.Tx, kind, op string, payload []byte) (bool, error) {
	if ObjectKind(kind) != KindConversation || OpKind(op) != OpConversationCreated ||
		len(payload) == 0 {
		return false, nil
	}
	record, err := Decode(payload)
	if err != nil {
		return false, nil //nolint:nilerr // see the doc: retained, not judged
	}
	created, err := decodePayload[ConversationCreated](record)
	if err != nil || created.Owner == "" {
		return false, nil //nolint:nilerr // a payload the apply refuses as well
	}
	return erasedOwner(ctx, tx, created.Owner)
}

// erasedOwner reports whether owner's erasure has applied.
func erasedOwner(ctx context.Context, tx *sql.Tx, owner string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM assistant_erasures WHERE owner_id = ?`, owner).Scan(&n); err != nil {
		return false, fmt.Errorf("assistantdomain: read the erasure gate for %s: %w", owner, err)
	}
	return n > 0, nil
}

// evictedAt reports whether a record at packed, written by writer, falls in
// writer's eviction window. THE WINDOW IS HALF-OPEN AT BOTH ENDS: a record at
// or below the eviction's own position was written while the node was counted,
// and one at or above a readmission by a node the fleet took back.
func evictedAt(ctx context.Context, tx *sql.Tx, writer string, packed int64) (bool, error) {
	var from, readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM assistant_evictions
		WHERE node_id = ?`, writer).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("assistantdomain: read the eviction gate for node "+
			"%s: %w", writer, err)
	}
	evicted := from.Valid && packed > from.Int64
	back := readmitted.Valid && packed >= readmitted.Int64
	return evicted && !back, nil
}

// deletionMarker is the conversation's deletion marker: the purge record's id,
// and whether there is one.
func deletionMarker(ctx context.Context, tx *sql.Tx, conversation string) (string, bool, error) {
	var author string
	err := tx.QueryRowContext(ctx,
		`SELECT purge_record_id FROM assistant_deletions WHERE conversation_id = ?`,
		conversation).Scan(&author)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("assistantdomain: read the deletion gate for "+
			"conversation %s: %w", conversation, err)
	}
	return author, true, nil
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record   MutationRecord
	position statelog.Position

	// packed is the composed position: every row's version guard.
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
		return 0, fmt.Errorf("assistantdomain: decode the record at %s: %w", rec.Position, err)
	}
	c := applyContext{
		record: record, position: rec.Position, packed: rec.Position.Packed(),
		brokerAt: opts.StoredAt,
	}
	payload, err := DecodeMutation(record)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: the record at %s: %w", rec.Position, err)
	}
	switch record.Subject.Kind {
	case KindBarrier:
		// THE READ INDEX'S OWN APPEND writes no row on any node.
		return 0, nil
	case KindTrimmed:
		// THE TRIM'S OWN RECORD. The framework removes this log's anchors
		// below the floor its subject names, in this same transaction
		// (ADR-0073); this domain writes nothing for it.
		return 0, nil
	case KindEviction:
		return applyEviction(ctx, tx, c, payload)
	case KindGeneration:
		return applyGeneration(ctx, tx, c, payload)
	case KindConversation:
		return applyConversation(ctx, tx, c, payload)
	case KindTurn:
		return applyTurn(ctx, tx, c, payload)
	case KindProposal:
		return applyProposal(ctx, tx, c, payload)
	case KindOwner:
		return applyOwner(ctx, tx, c, payload)
	}
	// A KIND THIS BUILD DOES NOT KNOW REACHES HERE ONLY AT A VERSION IT CAN
	// READ, which is a writer publishing a kind it never declared; failing is
	// what makes that writer's mistake visible.
	return 0, fmt.Errorf("assistantdomain: %s is not a kind this build applies, "+
		"and the record at %s claims version %d: a kind is declared before it is "+
		"published", record.Subject.Kind, rec.Position, record.V)
}

// applyEviction records a node's removal from this log, or its readmission, as
// an INVERSE COMMIT rather than a delete.
func applyEviction(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	e, ok := payload.(Eviction)
	if !ok || e.NodeID == "" {
		return 0, fmt.Errorf("assistantdomain: the eviction at %s names no node", c.position)
	}
	var (
		res sql.Result
		err error
	)
	if e.Readmitted {
		res, err = tx.ExecContext(ctx, `
			UPDATE assistant_evictions SET readmitted_position = ?, version = ?
			WHERE node_id = ? AND version < ?`, c.packed, c.packed, e.NodeID, c.packed)
	} else {
		// A SECOND EVICTION CLEARS THE READMISSION, so the evicted,
		// readmitted, evicted sequence reads as three facts.
		res, err = tx.ExecContext(ctx, `
			INSERT INTO assistant_evictions
				(node_id, at, by, from_position, readmitted_position, version)
			VALUES (?, ?, ?, ?, NULL, ?)
			ON CONFLICT (node_id) DO UPDATE SET
				at = excluded.at, by = excluded.by,
				from_position = excluded.from_position,
				readmitted_position = NULL, version = excluded.version
			WHERE excluded.version > assistant_evictions.version`,
			e.NodeID, c.at(), e.EvictedBy, c.packed, c.packed)
	}
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: apply the eviction of %s at %s: %w",
			e.NodeID, c.position, err)
	}
	return affected(res), nil
}

// applyGeneration writes a reanchor's audit row, create-only.
func applyGeneration(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	g, ok := payload.(Generation)
	if !ok {
		return 0, fmt.Errorf("assistantdomain: the generation record at %s carries "+
			"a %T", c.position, payload)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO assistant_log_generations
			(generation, at, by, new_stream_created_at, prev_last_seq_seen, record_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (generation) DO NOTHING`,
		g.Generation, c.at(), g.By, store.EncodeTime(g.StreamAt),
		int64(g.PrevHighest), c.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: record generation %d at %s: %w",
			g.Generation, c.position, err)
	}
	return affected(res), nil
}

// affected is how many rows a statement wrote, the row budget's input.
func affected(res sql.Result) int {
	n, _ := res.RowsAffected()
	return int(n)
}

// conversationRow is what an apply reads of a conversation it writes about.
type conversationRow struct {
	owner      string
	turns      int
	openTurn   string
	lastActive int64
}

// readConversation is one conversation's row, and false where there is none.
func readConversation(ctx context.Context, tx *sql.Tx, id string) (conversationRow, bool, error) {
	var row conversationRow
	err := tx.QueryRowContext(ctx, `
		SELECT owner_id, turns, open_turn_id, last_activity_at
		FROM assistant_conversations WHERE id = ?`, id).Scan(
		&row.owner, &row.turns, &row.openTurn, &row.lastActive)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return conversationRow{}, false, nil
	case err != nil:
		return conversationRow{}, false, fmt.Errorf("assistantdomain: read "+
			"conversation %s: %w", id, err)
	}
	return row, true, nil
}

// touch moves a conversation's activity to this record's broker instant: what
// its retention is measured from. Monotone, and guarded by position, so a
// redelivery moves nothing.
func touch(ctx context.Context, tx *sql.Tx, c applyContext, conversation string) (int, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE assistant_conversations SET
			last_activity_at = MAX(last_activity_at, ?),
			updated_at = MAX(updated_at, ?), version = ?
		WHERE id = ? AND version < ?`, c.at(), c.at(), c.packed, conversation, c.packed)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: move conversation %s's activity at %s: %w",
			conversation, c.position, err)
	}
	return affected(res), nil
}

// removeConversation deletes one conversation's rows and leaves its deletion
// marker, in one statement list, so a transaction that fails partway leaves
// the conversation whole. THE MARKER FIRST: an apply that stops between the
// two leaves a conversation the gate already holds rather than one deleted and
// recreatable.
func removeConversation(ctx context.Context, tx *sql.Tx, c applyContext, conversation,
	owner string, reason PurgeReason) (int, error) {

	res, err := tx.ExecContext(ctx, `
		INSERT INTO assistant_deletions
			(conversation_id, owner_id, at, by, by_kind, reason, purge_record_id, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (conversation_id) DO NOTHING`,
		conversation, owner, c.at(), c.record.Actor, string(c.record.ActorKind),
		string(reason), c.record.OpID, c.packed)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: mark conversation %s purged at %s: %w",
			conversation, c.position, err)
	}
	written := affected(res)
	for _, table := range conversationTables {
		res, err = tx.ExecContext(ctx,
			`DELETE FROM `+table+` WHERE conversation_id = ?`, conversation)
		if err != nil {
			return written, fmt.Errorf("assistantdomain: purge conversation %s's "+
				"%s at %s: %w", conversation, table, c.position, err)
		}
		written += affected(res)
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM assistant_conversations WHERE id = ?`,
		conversation)
	if err != nil {
		return written, fmt.Errorf("assistantdomain: purge conversation %s at %s: %w",
			conversation, c.position, err)
	}
	return written + affected(res), nil
}

// present reports an id a payload or a subject carries, false for an empty
// one: a record naming no object applies as nothing. Ids are the opaque keys
// the rows hold here; that each is a uuid is the writer's to hold.
func present(id string) bool { return strings.TrimSpace(id) != "" }
