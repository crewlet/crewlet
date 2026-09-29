package iamdomain

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/secrets"
)

// WHAT A REMOVAL ERASES BEYOND THE PERSON'S OWN ROWS.
//
// The person, their credentials, their sessions and their revocation epoch go
// with the rows [Applier.writeRemoval] deletes. Two tables keep a copy of their
// sealed values under rows that are not theirs, and both outlive the removal
// on purpose:
//
//   - an INVITATION addressed to them keeps its row until the sweep collects
//     it, so an operator can still see who invited whom — and it holds their
//     address, sealed;
//   - the AUTHENTICATION TRAIL keeps a row per record that changed them, for
//     the audit a removal must never erase — and each row carries the record
//     it was written by, whose payload holds their name, their address and any
//     seed they enrolled, sealed.
//
// So the removal's own apply CLEARS every sealed value those rows hold of
// theirs, and leaves every other column as it was: the ids, the blinds, the
// logins, the actors, the ops and the instants are what the trail is FOR, and
// none of them opens to anything personal. Afterwards no row on any node holds
// a value of theirs that opens under any keyring.
//
// # A sealed value is recognised by its SHAPE
//
// Every value this estate seals is a keyring envelope ([secrets.IsEnvelope])
// and every envelope it holds is somebody's personal value — nothing else here
// is encrypted — so the trail's documents are cleared by walking them and
// emptying every envelope, rather than by a table of which payload carries
// which field. A table would be a second list of the sealed fields that the
// next field added to a payload is missing from; the shape is the one fact
// every sealed value shares.
//
// # Which addresses are theirs, and why the removal's SCOPE names them
//
// An invitation is filed under its ADDRESS's bucket, not a person's — it has no
// person until it is redeemed — and so is the trail row its record wrote. So
// the rows this erasure clears sit in the buckets of the addresses that were
// the person's: the one the removal releases, and every one an invitation they
// redeemed was sent to, which is not always the same address once one has
// been claimed in place of another. [erasedBlinds] is the one answer to which
// those are, asked by the removal's WRITER before it publishes — whose record
// must declare every bucket its apply writes, or a node that deferred an
// invitation to one of those addresses applied the removal ahead of it and was
// left with a copy of the address nobody erased — and by the apply that
// clears them.
//
// # Deterministic, because every node does it
//
// The walk re-encodes a document it changed with encoding/json, which writes
// object keys in sorted order and numbers as they were read ([json.Number]),
// so every node that applies the removal to the same rows writes the same
// bytes — the identity claim holds across the erasure as it does across every
// other apply. A document with nothing to clear is left byte for byte.

// erasedBlinds is every address blind whose rows a removal of personID
// erases: released — the address claim the removal gives back, or empty for
// somebody who held none — and the address of every invitation they redeemed,
// sorted and without repeats.
//
// THE INVITATIONS AN ADDRESS WAS SENT are about nobody until one is redeemed,
// and their trail rows name nobody at all, so an address is the only thing
// they are found by; and a person may have redeemed an invitation to an
// address they hold no longer. Read from the rows the caller's transaction
// sees, so the writer's snapshot and each node's apply answer it the same way
// for the same log.
func erasedBlinds(ctx context.Context, tx *sql.Tx, personID, released string) (
	[]string, error) {

	var out []string
	if released != "" {
		out = append(out, released)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT email_blind FROM iam_invites
		WHERE person_id = ? AND email_blind <> ''`, personID)
	if err != nil {
		return nil, fmt.Errorf("iamdomain: read the addresses person %s was "+
			"invited at: %w", personID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var blind string
		if err := rows.Scan(&blind); err != nil {
			return nil, fmt.Errorf("iamdomain: read the addresses person %s was "+
				"invited at: %w", personID, err)
		}
		out = append(out, blind)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iamdomain: read the addresses person %s was "+
			"invited at: %w", personID, err)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// eraseSealed clears every sealed value of one removed person's that a row not
// their own still holds, reporting how many rows it rewrote.
//
// blind is the address claim the removal released, or empty for somebody who
// held none; the addresses erased are that one and every one the person
// redeemed an invitation at ([erasedBlinds]).
func eraseSealed(ctx context.Context, tx *sql.Tx, personID, blind string) (int, error) {
	blinds, err := erasedBlinds(ctx, tx, personID, blind)
	if err != nil {
		return 0, err
	}
	among, args := inList(blinds)
	invites, err := eraseIn(ctx, tx, "iam_invites", `
		SELECT id, document FROM iam_invites
		WHERE person_id = ? OR email_blind IN `+among,
		`UPDATE iam_invites SET email_sealed = x'', document = ? WHERE id = ?`,
		append([]any{personID}, args...)...)
	if err != nil {
		return invites, err
	}
	trail, err := eraseIn(ctx, tx, "iam_history", `
		SELECT id, document FROM iam_history
		WHERE person_id = ?
		   OR (person_id = '' AND object_kind = 'email' AND object_id IN `+among+`)`,
		`UPDATE iam_history SET document = ? WHERE id = ?`,
		append([]any{personID}, args...)...)
	return invites + trail, err
}

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
// READ WHOLE, THEN WRITTEN: the rows are collected before the first update so
// no statement writes a table while a cursor over it is open.
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
			// NOTHING SEALED IN IT, which for an invitation means its
			// column holds nothing either: the two are written from the
			// one sealed value, so a document with none left is a row
			// an earlier erasure already reached.
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
