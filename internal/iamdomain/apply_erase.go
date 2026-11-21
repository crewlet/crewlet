package iamdomain

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/secrets"
)

// WHAT A REMOVAL ERASES BEYOND THE PERSON'S OWN ROWS, which
// [Applier.writeRemoval] deletes. Two kinds of row outlive the removal on
// purpose and hold their sealed values: an INVITATION addressed to them (until
// the sweep, so who invited whom stays visible) holds their address; the
// AUTHENTICATION TRAIL's rows carry the records that changed them, holding
// their name, address and seeds. The apply clears every sealed value there and
// keeps every other column: ids, blinds, logins, actors, ops and instants are
// what the trail is for, and none opens to anything personal.
//
// A sealed value is found by its SHAPE ([secrets.IsEnvelope]): every envelope
// here is somebody's personal value, and a table of which payload field is
// sealed would be a second list the next sealed field is missing from.

// The rows to clear sit in the bucket of the address the removal releases (an
// invitation and its trail rows are filed under the address), the only one a
// person ever holds: a redemption binds its invitation's address, and nothing
// moves it later. So the removal's WRITER declares that bucket in its scope, or
// a node that deferred an invitation to it would apply the removal first and
// keep a copy nobody erased. Trail rows are found by the address, so they are
// erased even once the sweep collected the invitation.
//
// DETERMINISTIC: a changed document is re-encoded by encoding/json, keys sorted
// and numbers as read ([json.Number]), so every node writes the same bytes; a
// document with nothing to clear is left byte for byte.

// eraseSealed clears every sealed value of one removed person's that a row not
// their own still holds, reporting how many rows it rewrote.
//
// blind is the address the removal released, or empty for somebody who held
// none — a machine, which no invitation was ever sent to.
func eraseSealed(ctx context.Context, tx *sql.Tx, personID, blind string) (int, error) {
	var blinds []string
	if blind != "" {
		blinds = []string{blind}
	}
	among, args := inList(blinds)
	invites, err := eraseIn(ctx, tx, "iam_invites",
		erasedInvitationsStatement(among), eraseInvitationStatement,
		append([]any{personID}, args...)...)
	if err != nil {
		return invites, err
	}
	trail, err := eraseIn(ctx, tx, "iam_history",
		erasedTrailStatement(among), eraseTrailStatement,
		append([]any{personID}, args...)...)
	return invites + trail, err
}

// erasedInvitationsStatement selects the invitations a removal erases: the
// one the removed person redeemed, and every invitation to the address the
// removal releases, among being that address's placeholder list ([inList]).
func erasedInvitationsStatement(among string) string {
	return `
		SELECT id, document FROM iam_invites
		WHERE person_id = ? OR email_blind IN ` + among
}

// erasedTrailStatement selects the trail rows a removal erases: every row
// about the removed person, and every row filed under the address it releases
// (an invitation's issue and cancellation, [Applier.applyDirectory]), each arm
// a seek of its own index.
//
// THE ADDRESS ARM DOES NOT ASK THAT THE ROW NAME NOBODY: given that term, the
// planner seeks every unnamed row through the person index rather than the
// object index. It loses nothing: only those two records are filed under
// an address, neither names a person, and an address has one holder at a time.
func erasedTrailStatement(among string) string {
	return `
		SELECT id, document FROM iam_history
		WHERE person_id = ?
		   OR (object_kind = '` + historyObjectAddress + `'
		       AND object_id IN ` + among + `)`
}

// eraseTrailStatement rewrites one trail row's document, by its key.
const eraseTrailStatement = `UPDATE iam_history SET document = ? WHERE id = ?`

// eraseInvitationStatement clears one invitation's sealed address, in its
// column and in its document, by the invitation's key.
const eraseInvitationStatement = `UPDATE iam_invites SET email_sealed = x'', document = ? WHERE id = ?`

// inList is a parenthesised list of placeholders for values and the arguments
// that fill it — `(”)` for none, which matches no address, since a blind is
// never empty and an empty IN list is not a statement every engine parses.
func inList(values []string) (string, []any) {
	if len(values) == 0 {
		return "('')", nil
	}
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return "(" + strings.Repeat("?, ", len(values)-1) + "?)", args
}

// eraseIn rewrites every row a query selects whose document holds a sealed
// value, clearing it — and, for a table that keeps the same value in a column
// beside the document, that column too, which the update statement names.
//
// READ WHOLE, THEN WRITTEN, so no statement writes a table while a cursor over
// it is open.
func eraseIn(ctx context.Context, tx *sql.Tx, table, query, update string,
	args ...any) (int, error) {

	type found struct {
		id       string
		document []byte
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: read %s for an erasure: %w", table, err)
	}
	var todo []found
	for rows.Next() {
		var row found
		if err := rows.Scan(&row.id, &row.document); err != nil {
			rows.Close()
			return 0, fmt.Errorf("iamdomain: scan %s for an erasure: %w", table, err)
		}
		todo = append(todo, row)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iamdomain: read %s for an erasure: %w", table, err)
	}
	written := 0
	for _, row := range todo {
		cleared, changed, err := eraseEnvelopes(row.document)
		if err != nil {
			return written, fmt.Errorf("iamdomain: erase %s row %s: %w", table,
				row.id, err)
		}
		if !changed {
			// NOTHING SEALED: an invitation's column and document are written
			// from one sealed value, so this is a row an earlier erasure reached.
			continue
		}
		if _, err := tx.ExecContext(ctx, update, cleared, row.id); err != nil {
			return written, fmt.Errorf("iamdomain: erase %s row %s: %w", table,
				row.id, err)
		}
		written++
	}
	return written, nil
}

// eraseEnvelopes is a JSON document with every sealed value in it emptied, and
// whether there was one.
func eraseEnvelopes(document []byte) ([]byte, bool, error) {
	if len(document) == 0 {
		return document, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(document))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, false, fmt.Errorf("decode the document: %w", err)
	}
	tree, changed := eraseValue(tree)
	if !changed {
		return document, false, nil
	}
	out, err := json.Marshal(tree)
	if err != nil {
		return nil, false, fmt.Errorf("re-encode the document: %w", err)
	}
	return out, true, nil
}

// eraseValue empties every envelope inside one decoded JSON value.
func eraseValue(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		if secrets.IsEnvelope(t) {
			return "", true
		}
	case map[string]any:
		changed := false
		for key, inner := range t {
			if cleared, did := eraseValue(inner); did {
				t[key], changed = cleared, true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, inner := range t {
			if cleared, did := eraseValue(inner); did {
				t[i], changed = cleared, true
			}
		}
		return t, changed
	}
	return v, false
}
