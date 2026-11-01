package secrets

import (
	"context"
	"database/sql"
)

// KeyCounts is how many sealed values sit under each keyring key, by key id:
// what a rotation reads before it drops a key, since a value sealed under a
// key the ring no longer holds never opens again.
type KeyCounts map[string]int

// Outside is how many of the values are sealed under a key other than active:
// what a rotation still has to move, or wait out, before the old key goes.
func (c KeyCounts) Outside(active string) int {
	n := 0
	for key, count := range c {
		if key != active {
			n += count
		}
	}
	return n
}

// Querier is the slice of a database transaction [CountKeys] reads through.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// CountKeys adds every envelope one query answers to into, by the key id it is
// sealed under, opening none of them.
//
// THE KEY ID IS READ OFF THE ENVELOPE ([EnvelopeKeyID]), the one parse of that
// format, so what a rotation has to move is countable on a node that could not
// open a single value. A value that is not an envelope counts under no key.
func CountKeys(ctx context.Context, q Querier, query string, into KeyCounts, args ...any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sealed []byte
		if err := rows.Scan(&sealed); err != nil {
			return err
		}
		if key, ok := EnvelopeKeyID(string(sealed)); ok {
			into[key]++
		}
	}
	return rows.Err()
}
