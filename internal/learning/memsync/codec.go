// Package memsync makes a seat's memory follow the seat.
//
// # The bug this exists for
//
// A seat's memory — its diary, its episodes, the counterparties it has
// learned, the skills it has synthesized, what it has already said in a
// thread — is written to the node's OWN database. That was a deliberate
// choice, and its stated test was "would a peer reading this change any
// answer?", answered no because a seat's memory is read by the node running
// that seat.
//
// The answer is yes, because the node running that seat CHANGES. Placement
// claims up to `ceil(seats / live nodes)` and converges in both directions,
// so a seat moves on a node joining, a node leaving, a drain, or a rolling
// upgrade. Its rows do not move with it: the new owner queries its own store
// and finds none of them. The seat keeps working and has silently forgotten
// everything it learned somewhere else — and on a node that died, forgotten
// it permanently.
//
// # How it is fixed
//
// The store's own doc already calls it "a rebuildable index over what the
// replicated layer already holds". For memory that was aspiration rather than
// fact: nothing replicated it. This package makes it true.
//
// Every memory row is published to a COMPACTED CHANGELOG on the stream —
// one subject per row, `crewlet.memory.<handle>.<table>.<key>`, on a stream
// configured to retain exactly one message per subject. The stream therefore
// holds the current value of every row, replicated with everything else on
// the stream, and a node that acquires a seat replays that seat's subjects
// into its own database before the seat is allowed to take work.
//
// # What is deliberately not replicated
//
// DELETES. The lifecycle drops rows constantly — mid-state turns, tool-free
// turns, rows a skill absorbed, whole clusters replaced by a summary — and
// carrying tombstones for each would double the protocol to keep a table
// converged that already converges itself. A hydrated node may resurrect rows
// its predecessor had swept; the next lifecycle pass drops them again by the
// same rules, and rows a summary already covers are removed by the orphan
// sweep that exists for precisely that shape. The cost of a resurrection is a
// few rows carried until the next pass. The cost of a tombstone protocol is a
// second thing to keep correct forever.
package memsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

var log = logging.Get("learning.memsync")

// Row is one memory row on the wire.
//
// The table travels WITH the row rather than being inferred from the subject,
// because a hydrating node has to know how to write a row before it can trust
// where it came from — and a subject is a routing decision, not a schema.
type Row struct {
	Table string `json:"table"`

	// Values is the row, column name to value. JSON rather than a
	// positional tuple so a build whose table has gained a column can read
	// one written before it: the missing column takes its default rather
	// than shifting every value after it.
	Values map[string]any `json:"values"`
}

// export reads a seat's rows for one table.
//
// after bounds an incremental read on the table's rowid — see
// table.wholeEachCycle for which tables use it and why. It returns the
// highest rowid it saw, so the caller can advance its watermark.
func export(ctx context.Context, db *sql.DB, t table, seat seatRef, after int64) ([]Row, int64, error) {
	where := t.seatCol + " = ?"
	args := []any{seat.value(t)}
	if !t.wholeEachCycle {
		where += " AND rowid > ?"
		args = append(args, after)
	}
	query := "SELECT rowid, " + strings.Join(t.columns, ", ") +
		" FROM " + t.name + " WHERE " + where + " ORDER BY rowid"

	sqlRows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, after, fmt.Errorf("memsync: read %s: %w", t.name, err)
	}
	defer func() { _ = sqlRows.Close() }()

	high := after
	var out []Row
	for sqlRows.Next() {
		var rowid int64
		cells := make([]any, len(t.columns))
		into := make([]any, 0, len(t.columns)+1)
		into = append(into, &rowid)
		for i := range cells {
			into = append(into, &cells[i])
		}
		if err := sqlRows.Scan(into...); err != nil {
			return nil, after, fmt.Errorf("memsync: scan %s: %w", t.name, err)
		}
		values := make(map[string]any, len(t.columns))
		for i, column := range t.columns {
			values[column] = encodeCell(t, column, cells[i])
		}
		out = append(out, Row{Table: t.name, Values: values})
		if rowid > high {
			high = rowid
		}
	}
	if err := sqlRows.Err(); err != nil {
		return nil, after, fmt.Errorf("memsync: read %s: %w", t.name, err)
	}
	return out, high, nil
}

// encodeCell makes one scanned value safe to carry as JSON.
//
// The driver hands back []byte for both TEXT and BLOB, and the two must not
// be confused: a TEXT column re-inserted as bytes changes the stored type,
// and a BLOB re-inserted as text stores an embedding as its own base64
// spelling — which reads back as a vector of nothing and breaks that row's
// recall silently. The registry says which is which, and this is where that
// declaration is spent.
func encodeCell(t table, column string, cell any) any {
	raw, isBytes := cell.([]byte)
	if !isBytes {
		return cell
	}
	if t.isBlob(column) {
		// Marked so the decode side can tell a genuine blob from a
		// string that merely looks like base64.
		return map[string]any{"$blob": base64.StdEncoding.EncodeToString(raw)}
	}
	return string(raw)
}

// decodeCell reverses encodeCell.
func decodeCell(cell any) (any, error) {
	if number, isNumber := cell.(json.Number); isNumber {
		// JSON HAS ONE NUMBER TYPE and Go's default decoding makes it a
		// float64, which carries 53 bits: an int64 past 2^53 comes back
		// changed, silently, and column affinity then stores the wrong
		// value rather than refusing it. Decoding through json.Number
		// and preferring the integer reading removes the cliff instead
		// of documenting where it is.
		if whole, err := number.Int64(); err == nil {
			return whole, nil
		}
		fraction, err := number.Float64()
		if err != nil {
			return nil, fmt.Errorf("memsync: decode the number %q: %w", number, err)
		}
		return fraction, nil
	}
	wrapper, wrapped := cell.(map[string]any)
	if !wrapped {
		return cell, nil
	}
	encoded, ok := wrapper["$blob"].(string)
	if !ok {
		return nil, fmt.Errorf("memsync: a value is an object that is not a blob: %v", wrapper)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("memsync: decode a blob: %w", err)
	}
	return raw, nil
}

// subject is the row's address on the changelog.
//
// The seat and the table are readable; the key is HASHED. A natural key can
// be anything a counterparty is called on a chat platform — with dots,
// spaces, wildcards — and every one of those is a token separator or a
// pattern character in a subject. Hashing makes the address total, and the
// row carries its own values anyway, so nothing needs to read it back out.
func (t table) subject(handle string, values map[string]any) string {
	parts := make([]string, 0, len(t.key))
	for _, column := range t.key {
		parts = append(parts, fmt.Sprint(values[column]))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%s%s.%s.%x", topics.MemoryPrefix, handle, t.name, sum[:16])
}

// upsert writes one carried row into this node's store.
//
// ON CONFLICT over the NATURAL key, so hydrating a seat twice — a re-acquire,
// a redelivery, a node that restarts mid-replay — carries its memory once.
//
// WHAT THE CONFLICT DOES IS THE TABLE'S OWN ANSWER, and it is the same split
// table.wholeEachCycle already makes. An append-only row is immutable once
// written, so a row already here is that row and DO NOTHING is exact. A
// wholeEachCycle row is one whose UPDATE IS THE CONTENT — a counterparty
// profile rewritten as the seat learns, a skill archived, an onboarding
// marker flipped — and DO NOTHING there discards precisely what the table is
// republished every cycle to carry. A node that held the seat, lost it while
// a peer kept learning, and took it back would keep its own stale profile and
// then publish it back over the peer's, regressing the seat's memory for the
// whole fleet.
//
// Last writer wins is safe rather than merely convenient: a seat is held by
// ONE node at a time, so the changelog's latest value for a subject is by
// construction the value its current owner wrote. There is no second writer
// to lose to.
func upsert(ctx context.Context, tx *sql.Tx, t table, row Row) error {
	columns := make([]string, 0, len(t.columns))
	values := make([]any, 0, len(t.columns))
	for _, column := range t.columns {
		cell, present := row.Values[column]
		if !present {
			// A row written by a build that did not have this column
			// yet. Omitted so the column's own default applies, which
			// is the additive-evolution rule the event envelope holds
			// to: an older peer's row is readable, not rejected.
			continue
		}
		decoded, err := decodeCell(cell)
		if err != nil {
			return err
		}
		columns = append(columns, column)
		values = append(values, decoded)
	}
	if len(columns) == 0 {
		return fmt.Errorf("memsync: a carried %s row has no columns this build knows", t.name)
	}
	statement := "INSERT INTO " + t.name + " (" + strings.Join(columns, ", ") +
		") VALUES (" + strings.Repeat("?, ", len(columns)-1) + "?)" +
		t.onConflict(columns)
	if _, err := tx.ExecContext(ctx, statement, values...); err != nil {
		return fmt.Errorf("memsync: write a carried %s row: %w", t.name, err)
	}
	return nil
}

// seatRef is one seat, in both spellings the schema uses for it.
type seatRef struct {
	Handle string

	// AgentID is the derived UUIDv5 over (org name, handle). Derived
	// rather than looked up, so every node computes the same value with no
	// database and no running instance — which is exactly what makes a row
	// written on one node addressable from another.
	AgentID string
}

// value is how this seat is named in one table.
func (s seatRef) value(t table) string {
	if t.byAgentID {
		return s.AgentID
	}
	return s.Handle
}

// encode renders a row for the wire.
func encode(row Row) ([]byte, error) {
	body, err := json.Marshal(row)
	if err != nil {
		return nil, fmt.Errorf("memsync: encode a %s row: %w", row.Table, err)
	}
	return body, nil
}

// decode reads a row off the wire, refusing one whose table this build does
// not carry — a newer peer replicating a table added after this binary was
// built. Skipped rather than failed: a rolling upgrade puts both builds on
// one stream, and refusing to hydrate at all would be worse than hydrating
// what this build understands.
func decode(body []byte) (Row, table, bool, error) {
	var row Row
	// UseNumber, so decodeCell sees the digits rather than a float64 that
	// has already lost them — see there for why that matters.
	reader := json.NewDecoder(bytes.NewReader(body))
	reader.UseNumber()
	if err := reader.Decode(&row); err != nil {
		return Row{}, table{}, false, fmt.Errorf("memsync: decode a carried row: %w", err)
	}
	for _, t := range tables {
		if t.name == row.Table {
			return row, t, true, nil
		}
	}
	return row, table{}, false, nil
}

// onConflict is what an import does to a row that will not simply insert.
//
// See upsert for why the two tables of answer differ on the NATURAL key. The
// SET list is built from the columns actually CARRIED rather than from the
// registry, so a row written by an older build updates what it knew and
// leaves the rest alone instead of nulling columns it never had.
//
// The TRAILING BARE CLAUSE is the other half, and it is not decoration.
// SQLite's upsert only handles the constraint its target names — a carried
// row that collides on any OTHER unique index RAISES. Three of these tables
// have one: episodes is unique on (agent_handle, work_key), synthesized_skills
// on (agent_handle, name), conversation_sessions on its work key. Every one of
// those collisions is ordinary fleet history — a node that died mid-turn and a
// peer that re-ran the work record two rows for one work key under two ids,
// and both reach the changelog. Without this clause a node replaying that seat
// raises, and because hydration runs inside seat acquisition the raise REFUSES
// THE SEAT: one duplicated episode would make a seat unplaceable anywhere in
// the fleet. Skipping is also the right answer on its own terms — the index
// says one row per work key, and the row already here is one.
func (t table) onConflict(carried []string) string {
	skip := " ON CONFLICT DO NOTHING"
	if !t.wholeEachCycle {
		return skip
	}
	assignments := make([]string, 0, len(carried))
	for _, column := range carried {
		if t.isKey(column) {
			continue
		}
		assignments = append(assignments, column+" = excluded."+column)
	}
	if len(assignments) == 0 {
		// Every carried column is part of the key, so there is nothing
		// an update could change. DO UPDATE SET with an empty list is a
		// syntax error rather than a no-op.
		return skip
	}
	return " ON CONFLICT (" + strings.Join(t.key, ", ") + ") DO UPDATE SET " +
		strings.Join(assignments, ", ") + skip
}
