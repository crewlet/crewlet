package assistantdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/secrets"
)

// SealedCount is how many of the assistant's sealed values sit under each
// keyring key: what a keyring rotation waits out before it drops an old key.
//
// WAITED OUT, NEVER MOVED: a value here is written once, by the record that
// carries it, and leaves only with its conversation, which is purged once it
// has been untouched for its retention or is cleared by its owner. So a
// rotation keeps the old key on the ring until this count under it reaches
// zero. A conversation still in use keeps the values it was sealed with
// before the flip, so the count can stay above zero for as long as somebody
// keeps using a conversation started before it; what the count guarantees is
// that the key is never dropped while it still opens a value somebody can
// read. Dropped sooner, those conversations read as [ErrUnreadable].
type SealedCount struct {
	Values secrets.KeyCounts `json:"values,omitempty"`
}

// Outside is how many of the values are sealed under a key other than active.
func (c SealedCount) Outside(active string) int { return c.Values.Outside(active) }

// sealedColumns is every column of this domain that holds a keyring envelope,
// by table: what [Reader.SealedKeys] counts. A test holds it to the schema, so
// a sealed column added there is counted here too.
var sealedColumns = []struct{ table, column string }{
	{"assistant_conversations", "title_sealed"},
	{"assistant_turns", "page_title_sealed"},
	{"assistant_messages", "text_sealed"},
	{"assistant_messages", "page_title_sealed"},
	{"assistant_messages", "attachments_sealed"},
	{"assistant_steps", "body_sealed"},
	{"assistant_cards", "body_sealed"},
	{"assistant_proposals", "query_sealed"},
	{"assistant_proposals", "body_sealed"},
	{"assistant_proposals", "card_sealed"},
	{"assistant_proposals", "receipt_sealed"},
	{"assistant_parked", "messages_sealed"},
	{"assistant_audit_reads", "reason_sealed"},
	{"assistant_files", "name_sealed"},
}

// sealedValuesQuery selects every non-empty sealed value of the domain.
var sealedValuesQuery = func() string {
	parts := make([]string, 0, len(sealedColumns))
	for _, c := range sealedColumns {
		parts = append(parts, fmt.Sprintf("SELECT %[2]s FROM %[1]s WHERE length(%[2]s) > 0",
			c.table, c.column))
	}
	return strings.Join(parts, "\nUNION ALL\n")
}()

// SealedKeys counts every sealed value this node's rows hold, by the keyring
// key each is sealed under, opening none of them: the assistant's line in a
// rotation's "is anything still under the old key".
//
// A FILE'S CONTENT COUNTS TOO, by the key its row names (`content_key`): it is
// sealed into an object rather than a row, and a key dropped while an object
// is still sealed under it leaves that file unreadable exactly as a row's
// value would be. Counted from the row, so no object is fetched.
func (r *Reader) SealedKeys(ctx context.Context) (SealedCount, error) {
	out := SealedCount{Values: secrets.KeyCounts{}}
	err := r.read(ctx, DomainScope(), true, func(tx *sql.Tx) error {
		clear(out.Values)
		if err := secrets.CountKeys(ctx, tx, sealedValuesQuery, out.Values); err != nil {
			return err
		}
		return countContentKeys(ctx, tx, out.Values)
	})
	if err != nil {
		return SealedCount{}, fmt.Errorf("assistantdomain: count the sealed values by key: %w", err)
	}
	return out, nil
}

// countContentKeys adds every file's content to into, by the key its row says
// the content is sealed under.
func countContentKeys(ctx context.Context, tx *sql.Tx, into secrets.KeyCounts) error {
	rows, err := tx.QueryContext(ctx, `SELECT content_key, COUNT(*) FROM assistant_files
		WHERE length(content_key) > 0 GROUP BY content_key`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var (
			key string
			n   int
		)
		if err := rows.Scan(&key, &n); err != nil {
			_ = rows.Close()
			return err
		}
		into[key] += n
	}
	return errors.Join(rows.Err(), rows.Close())
}
