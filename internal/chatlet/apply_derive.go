package chatlet

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE APPLIER MAINTAINS FOUR DERIVED COLUMNS: see [Applier.Rederive].
var _ statelog.Deriver = (*Applier)(nil)

// DerivationVersion is the rule set this build derives its columns at.
//
// 1 is replicated migration 0071's four columns, each a projection the apply
// writes from the record and [Applier.Rederive] recomputes from the rows:
// a message's `event` (the kind of the room event it records, empty on words
// somebody said) and `collective` (it addressed the whole room), a room's
// `auto_join`, and a reaction's `message_author` (whose message it is on).
// Bumped by any change to what a derived column holds, so a node holding rows
// derived under another rule set converges on this one.
const DerivationVersion = 1

// DerivationVersion implements [statelog.Deriver].
func (a *Applier) DerivationVersion() int { return DerivationVersion }

// Rederive implements [statelog.Deriver]: every derived column recomputed
// from what its row already holds.
//
// THE SAME DECODE THE APPLY WRITES THEM FROM ([DecodeMessage],
// [DecodeChannel]), never a second reading of a document in SQL, for the
// reason the statelog.Deriver doc gives: two implementations of one rule are
// two answers the day either is edited. Only rows that differ are written, so
// a node whose rows this build maintained repairs none, and a row re-derived
// here equals the row a node replaying the log writes.
//
// EACH TABLE IS READ WHOLE BEFORE ANY OF ITS ROWS IS WRITTEN, because an
// UPDATE of a table a cursor is still walking is a write the cursor may or
// may not see again. What is held between the two is the repairs alone.
func (a *Applier) Rederive(ctx context.Context, tx *sql.Tx,
	_ statelog.ApplyOptions) (int, error) {

	messages, err := rederiveMessages(ctx, tx)
	if err != nil {
		return 0, err
	}
	rooms, err := rederiveRooms(ctx, tx)
	if err != nil {
		return 0, err
	}
	reactions, err := rederiveReactionAuthors(ctx, tx)
	if err != nil {
		return 0, err
	}
	return messages + rooms + reactions, nil
}

// rederiveMessages brings every message's `event` and `collective` to what
// its document says.
func rederiveMessages(ctx context.Context, tx *sql.Tx) (int, error) {
	type stale struct {
		id, event  string
		collective int
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, event, collective, document FROM chat_messages ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("chatlet: read the messages to re-derive: %w", err)
	}
	var repair []stale
	for rows.Next() {
		var (
			id, event  string
			collective int
			document   []byte
		)
		if err = rows.Scan(&id, &event, &collective, &document); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("chatlet: read a message to re-derive: %w", err)
		}
		message, decodeErr := DecodeMessage(document)
		if decodeErr != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("chatlet: open message %s to re-derive it: %w",
				id, decodeErr)
		}
		want := stale{id: id, event: eventColumn(message),
			collective: boolValue(message.Collective)}
		if want.event != event || want.collective != collective {
			repair = append(repair, want)
		}
	}
	if err = closeRows(rows, "the messages to re-derive"); err != nil {
		return 0, err
	}
	for _, r := range repair {
		if _, err = tx.ExecContext(ctx,
			`UPDATE chat_messages SET event = ?, collective = ? WHERE id = ?`,
			r.event, r.collective, r.id); err != nil {
			return 0, fmt.Errorf("chatlet: re-derive message %s: %w", r.id, err)
		}
	}
	return len(repair), nil
}

// rederiveRooms brings every room's `auto_join` to what its document says.
func rederiveRooms(ctx context.Context, tx *sql.Tx) (int, error) {
	type stale struct {
		id string
		on int
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, auto_join, document FROM chat_channels ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("chatlet: read the rooms to re-derive: %w", err)
	}
	var repair []stale
	for rows.Next() {
		var (
			id       string
			on       int
			document []byte
		)
		if err = rows.Scan(&id, &on, &document); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("chatlet: read a room to re-derive: %w", err)
		}
		room, decodeErr := DecodeChannel(document)
		if decodeErr != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("chatlet: open room %s to re-derive it: %w",
				id, decodeErr)
		}
		if want := boolValue(room.AutoJoin); want != on {
			repair = append(repair, stale{id: id, on: want})
		}
	}
	if err = closeRows(rows, "the rooms to re-derive"); err != nil {
		return 0, err
	}
	for _, r := range repair {
		if _, err = tx.ExecContext(ctx,
			`UPDATE chat_channels SET auto_join = ? WHERE id = ?`, r.on, r.id); err != nil {
			return 0, fmt.Errorf("chatlet: re-derive room %s: %w", r.id, err)
		}
	}
	return len(repair), nil
}

// rederiveReactionAuthors brings every reaction's `message_author` to the
// author of the message it is on, which is what the apply copies onto it.
func rederiveReactionAuthors(ctx context.Context, tx *sql.Tx) (int, error) {
	type stale struct{ messageID, emoji, handle, author string }
	rows, err := tx.QueryContext(ctx, `
		SELECT r.message_id, r.emoji, r.handle, m.author_handle
		  FROM chat_reactions r
		  JOIN chat_messages m ON m.id = r.message_id
		 WHERE r.message_author <> m.author_handle
		 ORDER BY r.message_id, r.emoji, r.handle`)
	if err != nil {
		return 0, fmt.Errorf("chatlet: read the reactions to re-derive: %w", err)
	}
	var repair []stale
	for rows.Next() {
		var r stale
		if err = rows.Scan(&r.messageID, &r.emoji, &r.handle, &r.author); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("chatlet: read a reaction to re-derive: %w", err)
		}
		repair = append(repair, r)
	}
	if err = closeRows(rows, "the reactions to re-derive"); err != nil {
		return 0, err
	}
	for _, r := range repair {
		if _, err = tx.ExecContext(ctx, `
			UPDATE chat_reactions SET message_author = ?
			 WHERE message_id = ? AND emoji = ? AND handle = ?`,
			r.author, r.messageID, r.emoji, r.handle); err != nil {
			return 0, fmt.Errorf("chatlet: re-derive the reaction on message %s: %w",
				r.messageID, err)
		}
	}
	return len(repair), nil
}

// eventColumn is a message's `event` column: its room event's kind, or empty
// on words somebody said.
func eventColumn(m Message) string {
	if m.Event == nil {
		return ""
	}
	return string(m.Event.Kind)
}

// closeRows closes a walk and reports what either the close or the walk
// itself failed on.
func closeRows(rows *sql.Rows, what string) error {
	if err := rows.Close(); err != nil {
		return fmt.Errorf("chatlet: read %s: %w", what, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("chatlet: read %s: %w", what, err)
	}
	return nil
}
