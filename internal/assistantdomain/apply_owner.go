package assistantdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// applyOwner applies a person's erasure: every conversation they own is
// removed, each leaving its deletion marker, and the person is marked erased so
// no conversation is created for them afterwards ([Applier.Gated]).
//
// ONE RECORD FOR EVERYTHING THEY OWN, decided from the rows at its position
// rather than from a list the writer enumerated: a conversation created on
// another node between the writer's read and this apply is the owner's too,
// and an erasure that left it behind would leave a removed person's words on
// every node. The conversations are visited in id order, so every node writes
// the same rows in the same order.
func applyOwner(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	p, ok := payload.(OwnerErased)
	if !ok {
		return 0, fmt.Errorf("assistantdomain: the %s record at %s carries a %T",
			c.record.Op, c.position, payload)
	}
	owner := c.record.Subject.ID
	if !present(owner) || p.Owner != owner {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO assistant_erasures (owner_id, at, by, by_kind, record_id, version)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner_id) DO NOTHING`,
		owner, c.at(), c.record.Actor, string(c.record.ActorKind), c.record.OpID, c.packed)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: mark %s erased at %s: %w", owner, c.position, err)
	}
	written := affected(res)
	conversations, err := ownedConversations(ctx, tx, owner)
	if err != nil {
		return written, err
	}
	for _, id := range conversations {
		removed, err := removeConversation(ctx, tx, c, id, owner, PurgeErased)
		written += removed
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// ownedConversations is every conversation owner holds, in id order.
func ownedConversations(ctx context.Context, tx *sql.Tx, owner string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM assistant_conversations WHERE owner_id = ? ORDER BY id`, owner)
	if err != nil {
		return nil, fmt.Errorf("assistantdomain: list %s's conversations: %w", owner, err)
	}
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("assistantdomain: list %s's conversations: %w", owner, err)
		}
		out = append(out, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("assistantdomain: list %s's conversations: %w", owner, err)
	}
	return out, nil
}
