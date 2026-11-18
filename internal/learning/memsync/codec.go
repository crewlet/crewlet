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
// it. The replay then reads on to an END BARRIER it publishes, so a row
// replaced before the replay reached it is read as its replacement ("The end
// barrier"). Nothing a hydration asks is
// answered with a row but its pulls, and its barriers are the one thing it
// writes, so a broker that refuses this node's writes refuses its hydrations
// ([Syncer.Hydrate]).
//
// BOUNDED BY ITS PROGRESS, NEVER BY A TOTAL (crewlet/crewlet#1065). A
// hydration takes what its bytes take: time refuses it only when a pull hears
// nothing for its stall, or no copy it is placed on delivers anything for a
// minute, and a replay whose copy is behind is placed again on another, past
// the last sequence it received ("What progress is", "A copy behind is placed
// again").
//
// A PULL AT A TIME, bounded in messages and in bytes, each pull's rows written
// in one transaction: at most fetchBatch messages, which sets a table's round
// trips where its rows are small, and at most one message at the transport's
// limit and the room the broker counts beside it ([Syncer.pullBytes]), which
// is what a hydration holds of the changelog's rows at once; across a link, a
// window the floor carries within a request's term less its own round trip, so
// a request sent behind its pull is answered in time ([Syncer.pullBound]; what
// it does not cover is below). Bounded by its count
// alone, one pull held up to 256 rows of up to 8 MiB each
// (crewlet/crewlet#1052). A row larger than this node's pull is refused by
// name, never read as a copy that stalled.
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
// [Syncer.PublishBacklog] the backlog, after it, in the engine's memory sync
// cycle's turn the held seats' backlogs share, after their own passes, so
// while those leave that turn the time to move a mark what a seat writes waits
// for no seat's backlog; a release's flush makes the same two calls as
// [Syncer.FlushOwn] and [Syncer.FlushBacklog], the backlog while its bound has
// time. The backlog is the republish node migration 0041 keeps: it puts this
// node's rows back on a changelog restored or recreated while the node ran,
// and carries what an earlier holding's flush did not reach.
//
// ONE PUBLISHER OF A SEAT AT A TIME, ON THIS NODE. The changelog keeps the
// last message on each row's subject, so a node that publishes a seat it does
// not hold puts its own, possibly older, copies over the holder's. So a seat
// is published only under its HOLDING here: live from the moment its
// hydration completes, when the memory sync cycle carries it; releasing from
// the moment its release begins, when only the release's flush does; and
// none before the hydration completes, after one that failed, and once
// the release has forgotten it. Every call checks the holding before each row
// it sends, so a cycle partway through a seat as its release begins stops at
// its next row (crewlet/crewlet#511).
//
// # The end barrier
//
// A replay reads until a barrier at or past the hydration's (B,
// [Syncer.barrier]) arrives: at or past, because a racing hydration's
// barriers replace this one's on the seat's one subject; one below B is an
// earlier hydration's on a copy that has not applied this one, and is read
// past. It then publishes the seat's barrier again and reads on to a barrier
// at or past that one's sequence (E). A row whose version the changelog
// acknowledged before B either arrives, or was removed by a later version
// first; that version was applied on the copy before B arrived, so before E
// was proposed, and raft orders the log, so it lies below E and arrives
// before it, and the same holds for any later version. A row is missed only
// where it was republished twice inside one table's replay, around B and
// again after E was proposed: two holders of the seat at once,
// crewlet/crewlet#511's window, whose newest version stays on the changelog.
//
// The copy's running count of what follows a delivery decides nothing: it
// runs low where a replacement's signal has not reached the copy, and high
// where a delivery skipped a row its removal had not yet taken off
// (nats-server consumer.go, checkNumPending), so an end read from it misses
// both versions of a row in the first case and waits for a message never
// sent in the second. A writer that never stops cannot hold the replay open:
// E is one publish, made once, and nothing after the first barrier at or
// past it is read.
//
// # A copy behind is placed again
//
// A replay is an R1 consumer the server places on a RANDOM member
// (nats-server jetstream_cluster.go, createGroupForConsumer), delivering out
// of that member's copy. No consumer can be placed on the leader instead (its
// configuration names no placement), and a read of the leader answers a row
// base64-encoded in JSON, a third larger, past the 8 MiB a leaf carries, on
// which a leaf closes its whole link, lease renewals with it (nats-server
// leafnode.go, processLeafMsgArgs).
//
// The barrier lies past the WHOLE changelog as the leader held it, so a copy
// behind the leader (a member catching up after a restart) holds the replay
// until it has applied all of it. A replay whose copy holds nothing for
// [copyBehindWait] while its barrier is owed is placed again: its consumer
// deleted and made again from one past the last sequence it received, which
// the broker places afresh, preferring members its metadata leader counts
// caught up, so a draw holds the barrier at least one time in three on three
// members. So is a consumer the broker said is gone or whose pull it refused,
// one that never got its pull (up to [jsapi.FirstPullRemakes] times in a
// row), and one whose delivery the link lost. A placement that delivered
// nothing is followed by the next no sooner than [copyBehindWait] after it
// ([tableReplay.again]).
//
// NOTHING IS SKIPPED: every message at or below where a placement begins was
// received, or removed by a later version past it, and sequences are the
// stream's, so this holds across copies. NOTHING IS READ TWICE: a placement
// delivers nothing at or below where it begins, and the upsert is idempotent
// anyway. Whatever ends a pull, its rows are written in its transaction
// before the replay is placed again or refused, so a transaction the store
// fails fails the hydration: a replay placed again past rows that never
// committed would carry the seat without them.
//
// # What progress is
//
// A DELIVERED MESSAGE, a row or a barrier: the client hands a message over
// only whole (nats.go's processMsg), and a leaf's compressed link decodes a
// flush as one block, so nothing finer proves bytes arrived. A heartbeat is
// no progress (a copy behind sends them for ever); it tells "the copy holds
// nothing for me now" from "nothing heard", and serves two clocks:
//
//   - SILENCE runs from the pull's send, from the last word on its inbox (a
//     delivery, a heartbeat, a status), and from the end of the replay's own
//     end-barrier publish, which holds it from its inbox. Past the pull's
//     stall ([Syncer.stall]) the seat is refused rather than placed again: a
//     dead link fails a new placement too, and nothing tells a dead server
//     from a pull still crossing at the floor. It refuses only once the inbox
//     is empty ([jsapi.Pulls.Ready]), so a word that arrived while the
//     replay's own request held it is heard first. A probe's answer
//     ([jsapi.Pulls.Quiet]) decides what a quiet replay does next, never how
//     long it may stay quiet: a consumer that keeps saying a delivery is on
//     its way while none arrives is held to the stall like any other.
//   - BEHIND runs from the first word that a copy holds nothing while the
//     barrier is owed, or from the first placement again for any cause,
//     whichever comes first since the last delivery, across placements and
//     the time spent making them; any delivery resets it. Behind for
//     [copyBehindWait] on one copy places the replay again
//     ([tableReplay.place]); nothing delivered for [behindWait] across every
//     placement, whatever each copy said (behind, gone, refusing its pulls,
//     never pulled, losing a delivery), refuses the seat
//     ([tableReplay.behind]).
//
// # What silence is
//
// ACROSS A LINK, THE LINK'S. A replay's words cross a link behind whatever
// else it carries (an event or fleet record of megabytes, which the link's
// window does not hold), and neither broker nor client sees any
// of a message until its last byte is in, so a replay behind an 8 MiB message
// at the floor hears nothing for four minutes from a copy that answered at
// once. Only the link's own socket meter ([Config.Link]) tells that from a
// copy or link that stopped answering. So at the end of a pull's stall with
// nothing heard ([tableReplay.judge]):
//
//   - a link that did not move over the stall, or none metered (a member's
//     connection among them), is SILENCE, and refuses the seat;
//   - a link that moved holds the replay, judged again every [quietWait] with
//     its consumer probed before each, so a link that falls still ends the
//     wait within a stall and a second of its last byte;
//   - once the link has read [Syncer.quietBytes] since the replay last heard
//     anything, the replay is placed again ([placedQuiet]), which the behind
//     clock neither arms nor reads, since the link may hold a word past a
//     minute;
//   - once no copy has said anything for [Syncer.quietCap], the seat is
//     refused whatever the link does ([tableReplay.creeping]).
//
// Every request a hydration hedges is held the same way while the link moves
// ([Syncer.waits]). Only a copy that answers nothing at all, on a link other
// traffic keeps moving, is refused later than the stall: it waits for the
// caps. A consumer that is gone or never got its pull still answers the probe
// behind that traffic, and is placed again sooner.
//
// # A window across a link
//
// A replay's pulls take from the ONE window of the node's link
// ([Config.Window], internal/jsapi's Window), which the object store's
// uploads, downloads and walks on the same link take from too, so what the
// node's bulk transfers hold out across the link, both ways together, never
// crosses the floor in more than a request's term leaves after its own round
// trip, and a lease renewal sent behind them is answered inside its term.
// (A node hydrates one seat at a time: the seat host's placement pass claims
// them one after another.) What the window does not hold is an event
// delivered to one of the node's seats, which crosses whole, and a request
// whose reply queues behind it waits for it. A replay's next row queued
// there refuses no seat: across a metered link the replay waits while the
// link moves ("What silence is").
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
// The table travels in the row as well as in its subject, so a replay refuses
// a row whose table is not the one whose subjects it read ([tableReplay.receive]).
type Row struct {
	Table string `json:"table"`

	// Values is the row, column name to value: a map rather than a positional
	// tuple, so a row written before a column was added leaves that column its
	// default instead of shifting every value after it.
	Values map[string]any `json:"values"`
}

// exported is one row a page read, with its watermark value: the mark a
// publish moves the seat's to once the changelog acknowledges the row. The mark
// stays off the wire: the column is this node's own (node migration 0041) and
// means nothing on a peer.
type exported struct {
	row  Row
	mark int64
}

// exportPage reads one page of a seat's rows of a watermark table: the rows
// strictly above after, in the watermark's order, until their carried values
// hold exportPageBytes, and always at least one. It says whether the budget
// stopped the page; one it did not stop is the table's last.
//
// A publish holds one page whatever lies behind it, and closing the statement
// leaves the rest unread: the driver steps only as rows are asked for, and the
// plan is a range seek with no sort (TestTheIncrementalExportIsARangeSeekNotASort).
// [exportBacklogPage] and [exportWhole] are the other two readers, rather than
// one taking bounds half the registry ignores.
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

// readPage reads statement's rows, asked with args, until their carried values
// hold exportPageBytes, and always at least one, saying whether the budget
// stopped the page.
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
		// Spent after the row is taken, so a page always holds one: stopping
		// short of a row past the budget would read the same empty page forever.
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

// pageStatement reads the seat's rows strictly above a mark, in the
// watermark's order, the watermark first. A function of its own, so the plan
// gate plans the text that runs.
func pageStatement(t table) string {
	return "SELECT " + t.watermark + ", " + strings.Join(t.columns, ", ") +
		" FROM " + t.name + " WHERE " + t.seatCol + " = ? AND " + t.watermark + " > ?" +
		" ORDER BY " + t.watermark
}

// backlogStatement reads the seat's rows strictly above a mark and at or below
// its boundary, in the watermark's order, the watermark first. A function of its
// own, so the plan gate plans the text that runs; the second bound is on the
// same (seat, change_seq) index, so it only narrows the seek.
func backlogStatement(t table) string {
	return "SELECT " + t.watermark + ", " + strings.Join(t.columns, ", ") +
		" FROM " + t.name + " WHERE " + t.seatCol + " = ? AND " + t.watermark + " > ? AND " +
		t.watermark + " <= ? ORDER BY " + t.watermark
}

// wholeStatement reads every one of the seat's rows of a wholeEachCycle table,
// in the order they were written.
func wholeStatement(t table) string {
	return "SELECT " + strings.Join(t.columns, ", ") + " FROM " + t.name +
		" WHERE " + t.seatCol + " = ? ORDER BY rowid"
}

// scanRow reads one row's carried columns, after lead's destinations, into
// wire values, and says how many bytes they hold ([carriedBytes]).
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

// carriedBytes is what one encoded cell holds: a text its length, a blob its
// base64. A number or a NULL is negligible beside a row's text and vector.
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
// The driver returns []byte for both TEXT and BLOB, so the registry decides:
// a TEXT re-inserted as bytes changes its stored type, and a BLOB re-inserted
// as text stores an embedding as its base64 spelling, silently breaking recall.
func encodeCell(t table, column string, cell any) any {
	raw, isBytes := cell.([]byte)
	if !isBytes {
		return cell
	}
	if t.isBlob(column) {
		// Marked so decode tells a blob from a string that looks like base64.
		return map[string]any{"$blob": base64.StdEncoding.EncodeToString(raw)}
	}
	return string(raw)
}

// decodeCell reverses encodeCell.
func decodeCell(cell any) (any, error) {
	if number, isNumber := cell.(json.Number); isNumber {
		// The integer reading first: a float64 holds 53 bits, so an int64
		// past 2^53 would come back silently changed and be stored as such.
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

// subject is the row's durable address on the compacted changelog.
//
// The seat is its agent id (ADR-0013), a fixed-width token a subject can hold.
// The key is hashed: a natural key can be anything a chat platform calls a
// counterparty, dots, spaces and wildcards included, which are subject syntax.
// Nothing reads the key back out; the row carries its own values.
func (t table) subject(seat string, values map[string]any) string {
	parts := make([]string, 0, len(t.key))
	for _, column := range t.key {
		parts = append(parts, fmt.Sprint(values[column]))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%s%s.%s.%x", topics.MemoryPrefix, seat, t.name, sum[:16])
}

// tableFilter matches every subject [table.subject] builds for one seat's rows
// of the table named: what a table's replay reads beside the seat's barrier,
// and what a retired table's count and purge take.
func tableFilter(seat, name string) string {
	return topics.MemoryPrefix + seat + "." + name + ".>"
}

// barrierSubject is the seat's one subject that is no table's, which a
// hydration publishes to before its replay and each table's replay reads to
// ([Syncer.replay]).
//
// Four tokens where a row's subject has five, and a last token no table can be
// named (topics.MemoryBarrierToken), so no [tableFilter] matches it: nats-server
// refuses a consumer whose filter subjects overlap. No row is published here.
func barrierSubject(seat string) string {
	return topics.MemoryPrefix + seat + "." + topics.MemoryBarrierToken
}

// upsert writes one carried row into this node's store, on conflict over the
// natural key, so hydrating a seat twice carries its memory once.
//
// The conflict follows table.wholeEachCycle. An append-only row is immutable,
// so DO NOTHING is exact, but for the vector a holder fills afterwards
// ([table.fillVector]). A wholeEachCycle row's update IS its content, so DO
// NOTHING would keep a returning node's stale copy, which it then republishes
// over the peer's. Last writer wins is safe while one node publishes the seat
// (package doc, "How it is fixed"); it still loses to the row
// a cycle was sending as the release began and to two nodes holding the seat at
// once (crewlet/crewlet#511).
func upsert(ctx context.Context, tx *sql.Tx, t table, row Row) error {
	columns := make([]string, 0, len(t.columns))
	values := make([]any, 0, len(t.columns))
	for _, column := range t.columns {
		cell, present := row.Values[column]
		if !present {
			// Written before the column existed: omitted, so its default
			// applies and the row is read rather than rejected.
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
// Looked up rather than left to the foreign key, whose refusal could only be
// told apart by the driver's text and would leave the transaction past a failed
// statement: one orphan would cost the fetch's every row, and the seat.
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

// seatRef is one seat in both spellings the schema uses: its handle, for the
// handle-keyed tables, and the agent id derived from it, for the changelog's
// subjects and the id-keyed tables. Both are stable across nodes because a
// handle is immutable (ADR-0013).
type seatRef struct {
	Handle string

	// AgentID is the UUIDv5 over (org name, handle), derived rather than looked
	// up so every node computes the same address with no database.
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

// decode reads a row off the wire. Its table is checked by the replay that read
// it ([tableReplay.receive]); a newer build's table is never replayed at all.
func decode(body []byte) (Row, error) {
	var row Row
	// UseNumber, so decodeCell sees the digits rather than a lossy float64.
	reader := json.NewDecoder(bytes.NewReader(body))
	reader.UseNumber()
	if err := reader.Decode(&row); err != nil {
		return Row{}, fmt.Errorf("memsync: decode a carried row: %w", err)
	}
	return row, nil
}

// onConflict is what an import does to a row that will not simply insert
// (upsert says why the two kinds of table differ). The SET list is the columns
// carried, not the registry's, so an older build's row leaves columns it never
// had alone rather than nulling them.
//
// The trailing bare clause skips a collision on another unique index, which
// SQLite's targeted upsert would raise: episodes (agent_handle, work_key) and
// synthesized_skills (agent_handle, name) collide in ordinary fleet history,
// such as a re-run of a dead node's work, and a raise inside hydration refuses
// the seat everywhere (TestARowCollidingOnAnUntargetedIndexIsSkippedNotRaised).
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
		// Every carried column is key: DO UPDATE SET with an empty list is a
		// syntax error, not a no-op.
		return skip
	}
	return " ON CONFLICT (" + strings.Join(t.key, ", ") + ") DO UPDATE SET " +
		strings.Join(assignments, ", ") + skip
}

// fillVector is the one update an append-only row takes on import: the vector
// the holder filled, over a stored row whose vector is of no model or of
// another (see [table.fillsVector]). Empty for a table that fills none.
//
// The rest of the row stays this node's, retrieval bookkeeping included. The
// carried vector is taken only when both columns are set, so a copy from
// before the fill never erases this node's vector, and only into a different
// space (another model, or another width a restart can leave behind): the same
// space's vector of immutable text is the same vector.
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
