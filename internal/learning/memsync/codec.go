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
// This is ADR-0014: a compacted changelog is a durability class of its own,
// beside the three ADR-0003 names, and that record says why none of the three
// fits a seat's memory.
//
// Every memory row is published to a COMPACTED CHANGELOG on the stream —
// one subject per row, `crewlet.memory.<seat-id>.<table>.<key-digest>`, on a
// stream configured to retain exactly one message per subject. The stream
// therefore holds the current value of every row, replicated with everything
// else on the stream, and a node that acquires a seat replays that seat's
// subjects into its own database before the seat is allowed to take work.
//
// A TABLE AT A TIME, in the registry's order. A compacted stream keeps no
// order across subjects, and a skill's versions reference the skill: every
// publish sends a seat's skills again and each version once, so the changelog
// holds a skill's only copy after the versions published before it, and one
// replay in the changelog's order wrote those versions first and refused the
// seat on any store without the skill (crewlet/crewlet#1051). Replayed table
// by table, parents are written before children whatever order the changelog
// keeps.
//
// EACH TABLE READ TO A BARRIER. A replay is a consumer the server places on
// any member of the stream, which delivers out of that member's copy, and a
// copy can be behind the leader. So before the first table a hydration
// publishes a barrier on the seat's own subject (topics.MemoryBarrierToken),
// which the changelog acknowledges at a sequence past every row it had
// acknowledged before, and each table's consumer reads the table's subjects
// and the barrier's: it delivers in the stream's order, so its copy hands over
// the table's rows below the barrier and then the barrier, once it has applied
// it, and the replay ends at a barrier at or past the hydration's, a racing
// hydration's included, once its copy holds nothing more. Nothing a hydration
// asks is answered with a row but its pulls, and the barrier is the one thing
// it writes, so a broker that refuses this node's writes refuses its
// hydrations ([Syncer.Hydrate]).
//
// A PULL AT A TIME, bounded in messages and in bytes, each pull's rows written
// in one transaction: at most fetchBatch messages, which sets a table's round
// trips, and at most one message at the transport's limit and the room the
// broker counts beside it ([Syncer.pullBytes]), which is what a hydration
// holds of the changelog's rows at once. Bounded by its count alone, one pull
// held up to 256 rows of up to 8 MiB each (crewlet/crewlet#1052). A row larger
// than this node's pull is refused by name, never read as a copy that stalled.
//
// WHAT A HOLDING WRITES GOES FIRST. A seat's marks live in memory, so every
// acquisition and every restart leaves this node a backlog: everything its
// store holds for the seat, the rows the hydration inserted and those it found
// already present, which keep their stamps. Everything the seat writes next is
// stamped above all of it, so in stamp order it waited behind that backlog,
// and a handoff or a crash before the backlog had drained lost it
// (crewlet/crewlet#1039). So a hydration records the seat's BOUNDARY as it
// completes, where this node's change sequence stood ([Syncer.Hydrate]), and
// each table with a watermark has two marks per seat: one over the rows
// stamped above the boundary, what the holding writes, its OWN, and one over
// those at or below it, its BACKLOG. A publish is two calls: [Syncer.PublishOwn]
// carries the seat's own rows and then the tables sent whole, and
// [Syncer.PublishBacklog] the backlog, which every caller makes after it, while
// its bound has time. The backlog is the republish node migration 0041 keeps:
// it puts this node's rows back on a changelog restored or recreated while the
// node ran, and carries what an earlier holding's flush did not reach. A seat
// with no boundary, one whose hydration here has not completed or that a
// release has forgotten, has every row as its own, which is how a publish
// carried a seat before boundaries.
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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/words"
)

var log = logging.Get("learning.memsync")

// Row is one memory row on the wire.
//
// The table travels WITH the row as well as in its subject, so a replay can
// hold a row to the subjects it read it on: a replay reads one table's
// subjects and writes each row as that table's, and a row whose own table is
// another is refused rather than written in that table's turn ([received]).
type Row struct {
	Table string `json:"table"`

	// Values is the row, column name to value. JSON rather than a
	// positional tuple so a build whose table has gained a column can read
	// one written before it: the missing column takes its default rather
	// than shifting every value after it.
	Values map[string]any `json:"values"`
}

// exported is one row a page read, with its value of the table's watermark
// column: the mark a publish moves the seat's to once the changelog has
// acknowledged the row.
//
// THE MARK IS NOT PART OF THE ROW, so it never reaches the wire: the column
// is this node's own (node migration 0041), and its values mean nothing on a
// peer.
type exported struct {
	row  Row
	mark int64
}

// exportPage reads one page of a seat's rows of a watermark table: the rows
// strictly above after, in the watermark's order, until their carried values
// hold exportPageBytes, and always at least one. It says whether the budget
// stopped the page; a page the budget did not stop is the table's last.
//
// A PAGE, NOT THE BACKLOG. [Syncer.PublishOwn] and [Syncer.PublishBacklog]
// send a page a row at a time, moving the mark with each row the changelog
// acknowledges, and read the next page above the last, so what a publish
// holds is one page whatever lies behind it. The read ends where the page
// does: the driver steps a statement only as its rows are asked for, and the
// plan hands them over in the index's order as it finds them, with no sort to
// read the rest first (TestTheIncrementalExportIsARangeSeekNotASort), so
// closing the statement leaves the rest unread.
//
// One of THREE READERS, rather than one taking a mark it ignores for half the
// registry and an end half the rest ignore: [exportBacklogPage] reads a page
// of a seat's backlog, which ends at its boundary, and [exportWhole] reads the
// tables that have no watermark.
func exportPage(ctx context.Context, db *sql.DB, t table, seat seatRef, after int64) ([]exported, bool, error) {
	return readPage(ctx, db, t, pageStatement(t), seat.value(t), after)
}

// exportBacklogPage reads one page of a seat's backlog of a watermark table:
// the rows strictly above after and at or below through, the seat's boundary,
// in the watermark's order, a page as [exportPage] reads one.
func exportBacklogPage(ctx context.Context, db *sql.DB, t table, seat seatRef,
	after, through int64) ([]exported, bool, error) {

	return readPage(ctx, db, t, backlogStatement(t), seat.value(t), after, through)
}

// readPage is the one read a page is, whichever of a seat's ranges it is of:
// statement's rows, asked with args, in the watermark's order, until their
// carried values hold exportPageBytes, and always at least one. It says
// whether the budget stopped the page.
func readPage(ctx context.Context, db *sql.DB, t table, statement string,
	args ...any) ([]exported, bool, error) {

	sqlRows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, false, fmt.Errorf("memsync: read %s: %w", t.name, err)
	}
	defer func() { _ = sqlRows.Close() }()

	var page []exported
	held := 0
	for sqlRows.Next() {
		var mark int64
		values, size, err := scanRow(sqlRows, t, &mark)
		if err != nil {
			return nil, false, err
		}
		page = append(page, exported{row: Row{Table: t.name, Values: values}, mark: mark})
		// SPENT AFTER THE ROW IS TAKEN, so a page always holds one: a row
		// past the budget ends the page it is read into, where a page that
		// stopped short of it would read the same nothing for ever, and
		// leave every row after it behind.
		if held += size; held >= exportPageBytes {
			return page, true, nil
		}
	}
	if err := sqlRows.Err(); err != nil {
		return nil, false, fmt.Errorf("memsync: read %s: %w", t.name, err)
	}
	return page, false, nil
}

// exportWhole reads every one of a seat's rows of a wholeEachCycle table,
// which has no watermark to read above.
func exportWhole(ctx context.Context, db *sql.DB, t table, seat seatRef) ([]Row, error) {
	sqlRows, err := db.QueryContext(ctx, wholeStatement(t), seat.value(t))
	if err != nil {
		return nil, fmt.Errorf("memsync: read %s: %w", t.name, err)
	}
	defer func() { _ = sqlRows.Close() }()

	var out []Row
	for sqlRows.Next() {
		values, _, err := scanRow(sqlRows, t)
		if err != nil {
			return nil, err
		}
		out = append(out, Row{Table: t.name, Values: values})
	}
	if err := sqlRows.Err(); err != nil {
		return nil, fmt.Errorf("memsync: read %s: %w", t.name, err)
	}
	return out, nil
}

// pageStatement is what a page of a watermark table is read with: the seat's
// rows strictly above a mark, in the watermark's order, the watermark first.
// A function of its own, so the plan gate plans the text that runs.
func pageStatement(t table) string {
	return "SELECT " + t.watermark + ", " + strings.Join(t.columns, ", ") +
		" FROM " + t.name + " WHERE " + t.seatCol + " = ? AND " + t.watermark + " > ?" +
		" ORDER BY " + t.watermark
}

// backlogStatement is what a page of a seat's backlog of a watermark table is
// read with: the seat's rows strictly above a mark and at or below its
// boundary, in the watermark's order, the watermark first. A function of its
// own, so the plan gate plans the text that runs: the second bound is on the
// last column of the same (seat, change_seq) index, so it narrows the seek and
// changes nothing else about it.
func backlogStatement(t table) string {
	return "SELECT " + t.watermark + ", " + strings.Join(t.columns, ", ") +
		" FROM " + t.name + " WHERE " + t.seatCol + " = ? AND " + t.watermark + " > ? AND " +
		t.watermark + " <= ? ORDER BY " + t.watermark
}

// wholeStatement is what a wholeEachCycle table is read with: every one of
// the seat's rows, in the order they were written.
func wholeStatement(t table) string {
	return "SELECT " + strings.Join(t.columns, ", ") + " FROM " + t.name +
		" WHERE " + t.seatCol + " = ? ORDER BY rowid"
}

// scanRow reads one row's carried columns, after the destinations lead takes
// first, into the values the wire carries, and says how many bytes those
// values hold ([carriedBytes]).
func scanRow(rows *sql.Rows, t table, lead ...any) (map[string]any, int, error) {
	cells := make([]any, len(t.columns))
	into := make([]any, 0, len(lead)+len(t.columns))
	into = append(into, lead...)
	for i := range cells {
		into = append(into, &cells[i])
	}
	if err := rows.Scan(into...); err != nil {
		return nil, 0, fmt.Errorf("memsync: scan %s: %w", t.name, err)
	}
	values := make(map[string]any, len(t.columns))
	size := 0
	for i, column := range t.columns {
		cell := encodeCell(t, column, cells[i])
		values[column] = cell
		size += carriedBytes(cell)
	}
	return values, size, nil
}

// carriedBytes is what one encoded cell holds: a text its length, a blob the
// base64 it travels as. A number or a NULL is a few bytes at most beside a
// row's text and its vector, and is not counted.
func carriedBytes(cell any) int {
	switch cell := cell.(type) {
	case string:
		return len(cell)
	case map[string]any:
		blob, _ := cell["$blob"].(string)
		return len(blob)
	}
	return 0
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
		return nil, fmt.Errorf("memsync: a value is an object with the keys %s rather than a blob",
			words.And(slices.Sorted(maps.Keys(wrapper))))
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("memsync: decode a blob: %w", err)
	}
	return raw, nil
}

// subject is the row's address on the changelog.
//
// THE SEAT IS ITS AGENT ID, the UUIDv5 over (org name, handle) (ADR-0013).
// The changelog is COMPACTED — one message retained per subject — so the
// subject IS a row's durable address, and the id is a fixed-width token that
// carries no character a subject cannot.
//
// The table is readable; the key is HASHED. A natural key can be anything a
// counterparty is called on a chat platform — with dots, spaces, wildcards —
// and every one of those is a token separator or a pattern character in a
// subject. Hashing makes the address total, and the row carries its own
// values anyway, so nothing needs to read it back out.
func (t table) subject(seat string, values map[string]any) string {
	parts := make([]string, 0, len(t.key))
	for _, column := range t.key {
		parts = append(parts, fmt.Sprint(values[column]))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%s%s.%s.%x", topics.MemoryPrefix, seat, t.name, sum[:16])
}

// tableFilter matches every subject [table.subject] builds for one seat's rows
// of the table named: the filter a table's replay reads beside the seat's
// barrier, and the one a retired table's count and purge take.
func tableFilter(seat, name string) string {
	return topics.MemoryPrefix + seat + "." + name + ".>"
}

// barrierSubject is the seat's barrier on the changelog: the one subject of the
// seat's that is no table's, which a hydration publishes to before it replays
// the seat and each table's replay reads to ([Syncer.replay]).
//
// FOUR TOKENS where a row's subject has five, and a last token no table can be
// named (topics.MemoryBarrierToken), so no [tableFilter] matches it and a
// replay's two filters never overlap, as nats-server requires of a consumer's
// filter subjects. No row is ever published here: a publish carries the
// registry's tables ([Syncer.PublishOwn], [Syncer.PublishBacklog]), and the
// barrier is none of them.
func barrierSubject(seat string) string {
	return topics.MemoryPrefix + seat + "." + topics.MemoryBarrierToken
}

// upsert writes one carried row into this node's store.
//
// ON CONFLICT over the NATURAL key, so hydrating a seat twice — a re-acquire,
// a redelivery, a node that restarts mid-replay — carries its memory once.
//
// WHAT THE CONFLICT DOES IS THE TABLE'S OWN ANSWER, and it is the same split
// table.wholeEachCycle already makes. An append-only row is immutable once
// written, so a row already here is that row and DO NOTHING is exact — but for
// the one column pair a holder fills afterwards, a diary note's or an
// episode's vector, which the carry takes over a stored vector of no model or
// another (table.fillsVector, [table.fillVector]). A wholeEachCycle row is one
// whose UPDATE IS THE CONTENT — a counterparty profile rewritten as the seat
// learns, a skill archived, an onboarding marker flipped — and DO NOTHING
// there discards precisely what the table is republished every cycle to
// carry. A node that held the seat, lost it while a peer kept learning, and
// took it back would keep its own stale profile and then publish it back over
// the peer's, regressing the seat's memory for the whole fleet.
//
// Last writer wins is safe rather than merely convenient while ONE node at a
// time publishes the seat, which is what holding it means: the changelog's
// latest value for a subject is then the value its current owner wrote. A
// seat's move is where that does not hold: a cycle the releasing node began
// before the release, and a successor still taking the seat, can each publish
// an older copy after the newer one, and the older one wins
// (crewlet/crewlet#511).
func upsert(ctx context.Context, tx *sql.Tx, t table, row Row) error {
	columns := make([]string, 0, len(t.columns))
	values := make([]any, 0, len(t.columns))
	for _, column := range t.columns {
		cell, present := row.Values[column]
		if !present {
			// A row written by a build that did not have this column
			// yet — this one's, read by a successor that adds it.
			// Omitted so the column's own default applies, which is the
			// additive-evolution rule the event envelope holds to: the
			// row is readable, not rejected.
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

// parentHeld reports whether this store holds the row a carried row of t
// belongs to ([table.parent]), and true where t has no parent.
//
// LOOKED UP BEFORE THE WRITE, never left to the foreign key. The key's refusal
// is an error like any other a write can meet, and telling it apart would mean
// matching the driver's text; and the transaction would then have to go on
// past a statement the store refused, which is the driver's business rather
// than this package's. A lookup asks neither of it, and keeps one orphan from
// costing the fetch's every row, and the seat.
func (t table) parentHeld(ctx context.Context, tx *sql.Tx, row Row) (bool, error) {
	if t.parent == nil {
		return true, nil
	}
	key, err := decodeCell(row.Values[t.parent.column])
	if err != nil {
		return false, err
	}
	var one int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM "+t.parent.table+" WHERE "+t.parent.key+" = ?",
		key).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("memsync: look for the %s a carried %s row belongs to: %w",
			t.parent.table, t.name, err)
	}
	return true, nil
}

// seatRef is one seat, in both spellings the schema uses for it: its handle,
// which the handle-keyed tables (episodes, counterparty profiles, the two
// skill tables) hold, and the agent id derived from
// it, which the changelog's subjects and the id-keyed tables are built from.
// Both are stable across nodes because a handle is immutable (ADR-0013).
type seatRef struct {
	Handle string

	// AgentID is the derived UUIDv5 over (org name, handle).
	// Derived rather than looked up, so every node computes the same value
	// with no database and no running instance — which is exactly what
	// makes a row written on one node addressable from another.
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

// decode reads a row off the wire.
//
// The table it names is held to the table whose subjects carried it by the
// replay that read it ([received]), never looked up here: a hydration replays
// the tables this build carries, one at a time, so a row of a table it does
// not carry, a newer build's, is never read at all.
func decode(body []byte) (Row, error) {
	var row Row
	// UseNumber, so decodeCell sees the digits rather than a float64 that
	// has already lost them — see there for why that matters.
	reader := json.NewDecoder(bytes.NewReader(body))
	reader.UseNumber()
	if err := reader.Decode(&row); err != nil {
		return Row{}, fmt.Errorf("memsync: decode a carried row: %w", err)
	}
	return row, nil
}

// onConflict is what an import does to a row that will not simply insert.
//
// See upsert for why the two tables of answer differ on the NATURAL key. The
// SET list is built from the columns actually CARRIED rather than from the
// registry, so a row written by a build whose table had fewer columns — this
// one's, read by a successor that adds one — updates what it knew and leaves
// the rest alone instead of nulling columns it never had.
//
// The TRAILING BARE CLAUSE is the other half, and it is not decoration.
// SQLite's upsert only handles the constraint its target names — a carried
// row that collides on any OTHER unique index RAISES. Two of these tables have
// one: episodes is unique on (agent_handle, work_key), synthesized_skills on
// (agent_handle, name). Every one of those collisions is ordinary fleet
// history — a node that died mid-turn and a peer that re-ran the work record
// two rows for one work key under two ids, and both reach the changelog.
// Without this clause a node replaying that seat
// raises, and because hydration runs inside seat acquisition the raise REFUSES
// THE SEAT: one duplicated episode would make a seat unplaceable anywhere in
// the fleet. Skipping is also the right answer on its own terms — the index
// says one row per work key, and the row already here is one.
func (t table) onConflict(carried []string) string {
	skip := " ON CONFLICT DO NOTHING"
	if !t.wholeEachCycle {
		return t.fillVector(carried) + skip
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

// fillVector is the one update an append-only row takes on import: the vector
// the holder filled, over a stored row whose vector is of no model or of
// another (see [table.fillsVector]). Empty for a table that fills none.
//
// EVERYTHING ELSE ABOUT THE ROW STAYS WHAT IS HERE — its retrieval
// bookkeeping is this node's — and the carried vector is taken only when it is
// a whole one: the row must carry both columns, set, so a carried copy written
// before the fill never erases the vector the fill gave this node. ONLY A
// DIFFERENT SPACE is a change — another model, or the same model at another
// width, which a restart can leave behind: the same model's vector of the same
// immutable text at the same width is the same vector, and rewriting it would
// be churn.
func (t table) fillVector(carried []string) string {
	if !t.fillsVector || !slices.Contains(carried, "embedding") ||
		!slices.Contains(carried, "embedding_model") {
		return ""
	}
	stored := t.name + "."
	return " ON CONFLICT (" + strings.Join(t.key, ", ") + ") DO UPDATE SET" +
		" embedding = excluded.embedding, embedding_model = excluded.embedding_model" +
		" WHERE excluded.embedding IS NOT NULL AND excluded.embedding_model IS NOT NULL" +
		" AND (" + stored + "embedding IS NULL OR " + stored + "embedding_model IS NULL" +
		" OR " + stored + "embedding_model <> excluded.embedding_model" +
		" OR length(" + stored + "embedding) <> length(excluded.embedding))"
}
