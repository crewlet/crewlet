package assistantdomain

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/store"
)

// applyConversation applies a record on a conversation's own subject.
func applyConversation(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	id := c.record.Subject.ID
	if ok := present(id); !ok {
		return 0, nil
	}
	switch p := payload.(type) {
	case ConversationCreated:
		return applyCreated(ctx, tx, c, id, p)
	case ConversationTitled:
		res, err := tx.ExecContext(ctx, `
			UPDATE assistant_conversations SET title_sealed = ?,
				updated_at = MAX(updated_at, ?), version = ?
			WHERE id = ? AND version < ?`, p.TitleSealed, c.at(), c.packed, id, c.packed)
		if err != nil {
			return 0, fmt.Errorf("assistantdomain: title conversation %s at %s: %w",
				id, c.position, err)
		}
		return affected(res), nil
	case ConversationPurged:
		return applyPurged(ctx, tx, c, id, p)
	case AuditReadRecorded:
		return applyAuditRead(ctx, tx, c, id, p)
	case FileRecorded:
		return applyFile(ctx, tx, c, id, p)
	case TurnOpened:
		return applyTurnOpened(ctx, tx, c, id, p)
	}
	return 0, fmt.Errorf("assistantdomain: the %s record at %s carries a %T",
		c.record.Op, c.position, payload)
}

// applyCreated writes a conversation's row, create-only: a redelivery, or a
// create of an id another create already wrote, writes nothing.
func applyCreated(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p ConversationCreated) (int, error) {

	if ok := present(p.Owner); !ok {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO assistant_conversations
			(id, owner_id, title_sealed, created_at, updated_at, last_activity_at,
			 turns, open_turn_id, version)
		VALUES (?, ?, ?, ?, ?, ?, 0, '', ?)
		ON CONFLICT (id) DO NOTHING`,
		id, p.Owner, p.TitleSealed, c.at(), c.at(), c.at(), c.packed)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: create conversation %s at %s: %w",
			id, c.position, err)
	}
	return affected(res), nil
}

// applyPurged removes a conversation for good.
//
// A RETENTION PURGE IS DECIDED AGAIN HERE: the conversation's activity moves on
// its turns' subjects, which its own arbitration does not see, so a message
// recorded after the reaper decided and before this record would otherwise be
// purged with a conversation that was in use. Such a purge writes nothing, and
// the reaper's next pass finds the conversation no longer due.
func applyPurged(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p ConversationPurged) (int, error) {

	if !p.Reason.Valid() {
		return 0, nil
	}
	row, found, err := readConversation(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if p.Reason == PurgeRetention && found &&
		row.lastActive >= store.EncodeTime(p.InactiveBefore) {
		return 0, nil
	}
	return removeConversation(ctx, tx, c, id, row.owner, p.Reason)
}

// applyAuditRead records one compliance read beside the conversation. It does
// NOT move the conversation's activity: an administrator reading it is not its
// owner using it, and must not keep it past its retention.
func applyAuditRead(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p AuditReadRecorded) (int, error) {

	if _, found, err := readConversation(ctx, tx, id); err != nil || !found {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO assistant_audit_reads
			(conversation_id, at, reader, reader_kind, operator_id, reason_sealed, record_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (conversation_id, at, reader) DO NOTHING`,
		id, c.at(), c.record.Actor, string(c.record.ActorKind), c.record.OperatorID,
		p.ReasonSealed, c.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: record a compliance read of %s at %s: %w",
			id, c.position, err)
	}
	return affected(res), nil
}

// applyTurnOpened opens a turn: its row, and the conversation's count and open
// turn. The writer decided it from a conversation with no open turn and this
// turn's number, and the broker's arbitration on the conversation holds both;
// a record that finds either moved writes nothing.
func applyTurnOpened(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p TurnOpened) (int, error) {

	if ok := present(p.Turn); !ok {
		return 0, nil
	}
	row, found, err := readConversation(ctx, tx, id)
	if err != nil || !found || row.owner != p.Owner {
		return 0, err
	}
	if row.openTurn != "" || p.Seq != row.turns+1 {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO assistant_turns
			(id, conversation_id, owner_id, seq, state, node, started_at,
			 prompt_version, model, provider_key, trace_id, page_title_sealed,
			 page_screen, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		p.Turn, id, p.Owner, p.Seq, string(wire.TurnRunning), p.Node, c.at(),
		p.PromptVersion, p.Model, p.ProviderKey, p.TraceID, p.PageTitleSealed,
		p.PageScreen, c.packed)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: open turn %s at %s: %w", p.Turn, c.position, err)
	}
	written := affected(res)
	if written == 0 {
		return 0, nil
	}
	res, err = tx.ExecContext(ctx, `
		UPDATE assistant_conversations SET turns = ?, open_turn_id = ?,
			last_activity_at = MAX(last_activity_at, ?), updated_at = MAX(updated_at, ?),
			version = ?
		WHERE id = ? AND version < ?`,
		p.Seq, p.Turn, c.at(), c.at(), c.packed, id, c.packed)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: open turn %s on conversation %s at %s: %w",
			p.Turn, id, c.position, err)
	}
	return written + affected(res), nil
}
