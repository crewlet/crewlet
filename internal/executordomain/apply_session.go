package executordomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/store"
)

// THE SESSION CASES: a begin, a capture, an end and a retention delete on a
// conversation's subject, and a seat's removal and the retention of that
// removal on the seat's.
//
// Each reads the rows it decides from IN THIS TRANSACTION, by the same helpers
// the writer's decide reads them through ([readSession], [readTomb],
// [readRemoval], [seatPlan], [expiredIn], [baseStands], [held]), so the decide
// and the apply hold one rule rather than two spellings of it: the decide
// refuses what it can see is wrong, and the apply repeats every deterministic
// guard for what changed between the two.
//
// # A held base is released by nothing
//
// A turn's first read freezes the conversation's head as the BASE its launches
// are to continue while the turn has kept no session of its own, and its
// `begin` writes the turn's row naming it ([Begin]). While that row is live and
// has landed no capture, it HOLDS the base: the seat budget's release, an end's
// absorption and its work-key dedupe, and the retention's delete each skip a
// held row. Each decides it here, in log order, rather than through a subquery
// of the statement that releases, so no statement gives the planner a
// correlated subquery to rewrite into a read of the whole table
// (storetest.Decorrelated): an end's absorption and dedupe and the retention's
// delete ask about each row before they take it ([held]), and the budget's
// release, which walks the seat's ended sessions in order, reads the seat's
// holds once before it walks ([heldBases]). A skipped row stays an ordinary
// ended row: the holder was handed it, so the holder's own end absorbs it once
// the hold has lifted, and so does the next end whose read showed it.
//
// EVERY STATEMENT IS NAMED, here and in the reader, so the plan gate plans the
// statements this package runs rather than copies of them
// (TestTheExecutorStatementsArePlanned).

// THE INDEX A STATEMENT WAS WRITTEN FOR, which it names with `INDEXED BY`.
// Every index on executor_sessions leads with the seat's handle, and the
// planner, which keeps no statistics, sought these statements unhinted on the
// handle alone through whichever such index it took — a turn's row through
// the page's index, a conversation's head through the budget's — reading every
// row the seat holds to answer a question about one turn or one conversation,
// and inside an apply that holds the store's one writer. The hint is the
// statement saying which index it was written for, and a guard: a schema
// change that drops the index fails the statement loudly rather than slowing
// it, as chatlet's mention probes do.
const (
	// sessionKey is the index executor_sessions' primary key (agent_handle,
	// turn_id) builds: one turn's row.
	sessionKey = "sqlite_autoindex_executor_sessions_1"

	// conversationIdx is a conversation's rows in the order they ended: its
	// endings and an end's dedupe.
	conversationIdx = "executor_sessions_conversation_idx"

	// lineIdx is a conversation's rows in its LINE's order — by where each
	// turn read the conversation, then by its end: its head, the newest of
	// them ([Head.Session]).
	lineIdx = "executor_sessions_line_idx"

	// endedIdx is a seat's rows in the order they ended: the budget's
	// release, oldest first.
	endedIdx = "executor_sessions_ended_idx"

	// seatIdx is a seat's rows in the order they began: its page.
	seatIdx = "executor_sessions_seat_idx"

	// heldIdx is the bases live turns hold: a PARTIAL index over exactly the
	// rows that hold one (live, no capture landed, naming a base), whose
	// predicate every statement through it repeats word for word, which is
	// what lets the planner and the hint use it.
	heldIdx = "executor_sessions_held_idx"
)

// Tombstone reasons: why a turn takes no further record.
const (
	// tombEnded is a turn its own `end` closed.
	tombEnded = "ended"

	// tombSeatRemoved is a live turn whose seat the company removed.
	tombSeatRemoved = "seat_removed"
)

// The statements a decide and an apply read the rows they decide from by.
const (
	// readSessionStatement is one turn's row, by its key.
	readSessionStatement = `
		SELECT conversation_key, work_key, session_id, launch_seq, capture_seq, read_through,
		       absorbs, end_seq, object, object_hash, object_size, base_turn, last_at
		FROM executor_sessions INDEXED BY ` + sessionKey + ` WHERE agent_handle = ? AND turn_id = ?`

	// readTombStatement is one turn's tombstone, by its key.
	readTombStatement = `
		SELECT conversation_key, at FROM executor_session_tombs
		WHERE agent_handle = ? AND turn_id = ?`

	// readRemovalStatement is where a seat's rows were last removed.
	readRemovalStatement = `
		SELECT at_seq FROM executor_seat_removals WHERE agent_handle = ?`

	// seatBytesStatement is the object bytes a seat's other turns name.
	seatBytesStatement = `
		SELECT COALESCE(SUM(object_size), 0) FROM executor_sessions
		WHERE agent_handle = ? AND object IS NOT NULL AND turn_id <> ?`

	// seatReleasableStatement is a seat's ended sessions in the order the
	// budget releases them: the oldest end first. AN END'S POSITION IS
	// UNIQUE among the rows that hold one, so it alone is a total order, and
	// the seat's index on it serves the order without a sort.
	seatReleasableStatement = `
		SELECT turn_id, object_size FROM executor_sessions INDEXED BY ` + endedIdx + `
		WHERE agent_handle = ? AND end_seq IS NOT NULL AND object IS NOT NULL
		  AND turn_id <> ?
		ORDER BY end_seq`

	// heldStatement is whether a live turn of the seat other than one holds
	// an ended turn as its base: one point read of the bases' index.
	heldStatement = `
		SELECT 1 FROM executor_sessions INDEXED BY ` + heldIdx + `
		WHERE agent_handle = ? AND base_turn = ? AND end_seq IS NULL AND launch_seq = 0
		  AND base_turn <> '' AND turn_id <> ? LIMIT 1`

	// heldBasesStatement is every base the seat's live turns other than one
	// hold: what the budget's release, which walks the seat's ended sessions
	// in order, reads once before it walks rather than once per row.
	heldBasesStatement = `
		SELECT base_turn FROM executor_sessions INDEXED BY ` + heldIdx + `
		WHERE agent_handle = ? AND end_seq IS NULL AND launch_seq = 0
		  AND base_turn <> '' AND turn_id <> ?`
)

// The statements an apply writes the rows with.
const (
	// releaseStatement takes an ended turn's object off its row, keeping
	// the row's ending.
	releaseStatement = `
		UPDATE executor_sessions INDEXED BY ` + sessionKey + ` SET object = NULL, object_hash = '',
			object_size = 0, content_key = '', version = ?
		WHERE agent_handle = ? AND turn_id = ?`

	// retireStatement takes a session that ran out of context off every ended row
	// of one conversation that names it, saying which turn it ran out in.
	retireStatement = `
		UPDATE executor_sessions INDEXED BY ` + conversationIdx + ` SET object = NULL, object_hash = '',
			object_size = 0, content_key = '', retired_by = ?, version = ?
		WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
		  AND session_id = ? AND object IS NOT NULL`

	// insertCaptureStatement is a turn's first capture where no begin of
	// it landed first: the row begins here, at first_seq.
	insertCaptureStatement = `
		INSERT INTO executor_sessions (
			agent_handle, turn_id, conversation_key, work_key, harness, harness_version,
			session_id, object, object_hash, object_size, content_key, archive_format,
			launch_seq, capture_seq, home, workspace, context_tokens, interrupted,
			captured_at, read_through, absorbs, last_at, first_seq, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, turn_id) DO NOTHING`

	// updateCaptureStatement is a later capture of a live turn, or its first
	// onto the row its begin wrote: every value is the one [applyCapture]
	// decided, the turn's first read and its work key included, so the
	// statement itself decides nothing. It leaves the row's base as the
	// begin wrote it; the capture's own launch_seq is what lifts the hold.
	updateCaptureStatement = `
		UPDATE executor_sessions INDEXED BY ` + sessionKey + ` SET work_key = ?, harness = ?, harness_version = ?,
			session_id = ?, object = ?, object_hash = ?, object_size = ?,
			content_key = ?, archive_format = ?, launch_seq = ?, capture_seq = ?,
			home = ?, workspace = ?, context_tokens = ?, interrupted = ?, captured_at = ?,
			read_through = ?, absorbs = ?, last_at = ?, version = ?
		WHERE agent_handle = ? AND turn_id = ? AND end_seq IS NULL AND version < ?`

	// insertBeginStatement is the row of a turn whose begin landed before
	// any capture: its first read and the base it holds, naming no object,
	// at launch 0, so the bases' index lists it while it holds one.
	insertBeginStatement = `
		INSERT INTO executor_sessions (agent_handle, turn_id, conversation_key, work_key,
			read_through, absorbs, base_turn, last_at, first_seq, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, turn_id) DO NOTHING`

	// updateBeginStatement is a begin landing on the row a capture of the
	// turn wrote first with no read: the read and the base the row names
	// from then on. It holds nothing, the row having landed a capture. Every
	// value is the one [applyBegin] decided, from the row — that it holds no
	// read yet, since a turn's first read stands, included — so the
	// statement itself decides nothing.
	updateBeginStatement = `
		UPDATE executor_sessions INDEXED BY ` + sessionKey + ` SET work_key = ?, read_through = ?,
			absorbs = ?, base_turn = ?, version = ?
		WHERE agent_handle = ? AND turn_id = ? AND end_seq IS NULL AND version < ?`

	// endRowStatement stamps a turn's end on the row a capture of it, or its
	// begin, wrote.
	endRowStatement = `
		UPDATE executor_sessions INDEXED BY ` + sessionKey + ` SET work_key = ?, ended_at = ?, end_seq = ?,
			outcome = ?, delivered = ?, ending = ?, ask_excerpt = ?, last_at = ?,
			version = ?
		WHERE agent_handle = ? AND turn_id = ? AND end_seq IS NULL`

	// insertEndingStatement is the ended row of a turn that has none: it
	// captured nothing and no begin of it landed, so its ending alone, naming
	// no object.
	insertEndingStatement = `
		INSERT INTO executor_sessions (agent_handle, turn_id, conversation_key,
			work_key, ended_at, end_seq, outcome, delivered, ending, ask_excerpt,
			last_at, first_seq, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, turn_id) DO NOTHING`

	// attemptsStatement is the older ended attempts at one unit of work in
	// a conversation, each with whether its reply went out.
	attemptsStatement = `
		SELECT turn_id, delivered FROM executor_sessions INDEXED BY ` + conversationIdx + `
		WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
		  AND work_key = ? AND turn_id <> ?`

	// dedupeStatement deletes one of those attempts, by its key: one at a
	// time, each the apply found no live turn holding ([held]).
	dedupeStatement = `
		DELETE FROM executor_sessions INDEXED BY ` + sessionKey + `
		WHERE agent_handle = ? AND turn_id = ? AND end_seq IS NOT NULL`

	// absorbStatement deletes one ended turn the ending turn's first read
	// handed out, by its key, and only where it ended at or before that read
	// (a read can show no turn that ended after it).
	absorbStatement = `
		DELETE FROM executor_sessions INDEXED BY ` + sessionKey + `
		WHERE agent_handle = ? AND turn_id = ? AND conversation_key = ?
		  AND end_seq IS NOT NULL AND end_seq <= ?`

	// tombStatement writes a turn's tombstone, keeping the first one a turn
	// was given.
	tombStatement = `
		INSERT INTO executor_session_tombs
			(agent_handle, turn_id, conversation_key, at_seq, at, reason)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, turn_id) DO NOTHING`

	// deleteSessionStatement and deleteTombStatement remove one turn's row
	// and its tombstone past the retention.
	deleteSessionStatement = `
		DELETE FROM executor_sessions INDEXED BY ` + sessionKey + ` WHERE agent_handle = ? AND turn_id = ?`
	deleteTombStatement = `
		DELETE FROM executor_session_tombs WHERE agent_handle = ? AND turn_id = ?`

	// seatLiveStatement is a removed seat's turns that had not ended, in
	// the order of their key.
	seatLiveStatement = `
		SELECT turn_id, conversation_key FROM executor_sessions
		WHERE agent_handle = ? AND end_seq IS NULL ORDER BY turn_id`

	// seatRowsStatement deletes every row a removed seat holds.
	seatRowsStatement = `DELETE FROM executor_sessions WHERE agent_handle = ?`

	// removeSeatStatement records where a seat's rows were removed, moving
	// only forward.
	removeSeatStatement = `
		INSERT INTO executor_seat_removals (agent_handle, at_seq, at)
		VALUES (?, ?, ?)
		ON CONFLICT (agent_handle) DO UPDATE SET
			at_seq = excluded.at_seq, at = excluded.at
		WHERE excluded.at_seq > executor_seat_removals.at_seq`

	// forgetRemovalStatement deletes a seat's record of its removal past
	// the retention's cutoff.
	forgetRemovalStatement = `
		DELETE FROM executor_seat_removals WHERE agent_handle = ? AND at <= ?`
)

// sessionRow is what a decide and an apply read of one turn's row.
type sessionRow struct {
	conversationKey string
	workKey         string
	sessionID       string
	launchSeq       int64
	captureSeq      int64
	readThrough     int64
	absorbs         string
	ended           bool
	endSeq          int64
	hasObject       bool
	object          string
	objectHash      string
	objectSize      int64
	baseTurn        string
	lastAt          int64
}

// readSession is one turn's row, and false where there is none.
func readSession(ctx context.Context, tx *sql.Tx, handle, turn string) (sessionRow, bool, error) {
	var (
		row    sessionRow
		endSeq sql.NullInt64
		object sql.NullString
	)
	err := tx.QueryRowContext(ctx, readSessionStatement, handle, turn).Scan(
		&row.conversationKey, &row.workKey, &row.sessionID, &row.launchSeq, &row.captureSeq,
		&row.readThrough, &row.absorbs, &endSeq, &object, &row.objectHash, &row.objectSize,
		&row.baseTurn, &row.lastAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return sessionRow{}, false, nil
	case err != nil:
		return sessionRow{}, false, fmt.Errorf("executordomain: read %s's turn %s: %w",
			handle, turn, err)
	}
	row.ended, row.endSeq = endSeq.Valid, endSeq.Int64
	row.hasObject, row.object = object.Valid, object.String
	return row, true, nil
}

// tombRow is one turn's tombstone.
type tombRow struct {
	conversationKey string
	at              int64
}

// readTomb is one turn's tombstone, and false where there is none.
func readTomb(ctx context.Context, tx *sql.Tx, handle, turn string) (tombRow, bool, error) {
	var tomb tombRow
	err := tx.QueryRowContext(ctx, readTombStatement, handle, turn).Scan(
		&tomb.conversationKey, &tomb.at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return tombRow{}, false, nil
	case err != nil:
		return tombRow{}, false, fmt.Errorf("executordomain: read %s's tombstone of "+
			"turn %s: %w", handle, turn, err)
	}
	return tomb, true, nil
}

// readRemoval is the packed position a seat's rows were last removed at, zero
// where they never were (or that removal is past the retention).
func readRemoval(ctx context.Context, tx *sql.Tx, handle string) (int64, error) {
	var at int64
	err := tx.QueryRowContext(ctx, readRemovalStatement, handle).Scan(&at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("executordomain: read seat %s's removal: %w", handle, err)
	}
	return at, nil
}

// newer reports whether a capture's ordinals are past the row's: a later
// launch, or a later capture of the same launch.
func newer(launch, capture, rowLaunch, rowCapture int64) bool {
	return launch > rowLaunch || (launch == rowLaunch && capture > rowCapture)
}

// sameWork reports whether a record's work key agrees with the row's: A TURN
// HAS ONE UNIT OF WORK, the first non-empty key any record about it carried,
// and a record naming another is about some other dispatch. An empty key on
// either side claims nothing.
func sameWork(rowKey, recordKey string) bool {
	return rowKey == "" || recordKey == "" || rowKey == recordKey
}

// nameable reports whether a capture names an object every reader of the row
// can parse: a key in the store's canonical spelling and a SHA-256 digest. The
// collector, its audit, the backup and every head and page read of the seat
// parse both, so a row naming anything else would fail all of them, on every
// node, for as long as the row stays.
func nameable(object, hash string) bool {
	_, keyErr := objstore.ParseKey(object)
	_, hashErr := objstore.ParseHash(hash)
	return keyErr == nil && hashErr == nil
}

// canonicalTurn reports whether s is a turn id in its one spelling: a UUID in
// canonical lowercase form. A run is keyed by its id as the engine minted it,
// so a turn spelled two ways (braced, uppercase, a URN, undashed) would be two
// rows, two conversation subjects and two associated data for one run, and the
// retention's check of the run's liveness would miss the other spelling.
func canonicalTurn(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}

// absorbsOf is the turns a capture says its first read handed out, checked,
// and whether they could be: at most [MaxAbsorbs] canonical turn ids, none
// twice, never the capturing turn's own, and only beside a read
// ([Capture.ReadThrough]) they were handed out by.
func absorbsOf(turns []string, own string, readThrough int64) ([]string, bool) {
	if len(turns) == 0 {
		return nil, true
	}
	if readThrough <= 0 || len(turns) > MaxAbsorbs {
		return nil, false
	}
	seen := make(map[string]bool, len(turns))
	for _, t := range turns {
		if !canonicalTurn(t) || t == own || seen[t] {
			return nil, false
		}
		seen[t] = true
	}
	return turns, true
}

// splitAbsorbs reads a row's absorbs column back: turn ids a comma apart,
// which a canonical turn id never holds.
func splitAbsorbs(column string) []string {
	if column == "" {
		return nil
	}
	return strings.Split(column, ",")
}

// seatPlan is which ended turns' sessions a capture of size bytes on turn must
// release for the seat to hold budget, and whether it fits at all.
//
// THE OLDEST ENDINGS GO FIRST, by the position each ended at, and never a live
// turn's session or this turn's own: a live turn's is the one its next launch
// continues, and this turn's is being replaced. NOR A BASE ANOTHER LIVE TURN
// HOLDS ([heldBases], read once for the whole walk): that turn's relaunch is
// to continue it. A base THIS turn holds is released like any ended session,
// because this capture is the turn's first landed one and lifts the hold in
// the same apply: from it on, the turn's launches continue its own session,
// which holds everything the base did, so releasing the base loses nothing —
// what keeps a seat at its budget's floor able to take a continued turn's
// first capture at all.
//
// The release is a total order of the rows (an end's position is unique among
// them), so every node releases the same sessions. Nothing is chosen when even
// every releasable session released would not make room: the seat's live turns
// and the bases they hold already fill the budget, and the capture is refused
// whole rather than releasing what does not help.
func seatPlan(ctx context.Context, tx *sql.Tx, handle, turn string, size, budget int64) (
	[]string, bool, error) {

	var occupied int64
	if err := tx.QueryRowContext(ctx, seatBytesStatement, handle, turn).Scan(&occupied); err != nil {
		return nil, false, fmt.Errorf("executordomain: read %s's session bytes: %w", handle, err)
	}
	need := occupied + size
	if need <= budget {
		return nil, true, nil
	}
	// THE HOLDS ARE READ WHOLE BEFORE THE WALK, which reads the seat's ended
	// sessions in their release order and stops once the capture fits: the
	// bases the seat's live turns hold are a handful, and no statement runs
	// while the walk's rows are open.
	holds, err := heldBases(ctx, tx, handle, turn)
	if err != nil {
		return nil, false, err
	}
	rows, queryErr := tx.QueryContext(ctx, seatReleasableStatement, handle, turn)
	if queryErr != nil {
		return nil, false, fmt.Errorf("executordomain: read %s's ended sessions: %w",
			handle, queryErr)
	}
	defer func() { _ = rows.Close() }()
	var release []string
	for rows.Next() {
		var (
			ended string
			bytes int64
		)
		if scanErr := rows.Scan(&ended, &bytes); scanErr != nil {
			return nil, false, scanErr
		}
		if holds[ended] {
			continue
		}
		release = append(release, ended)
		if need -= bytes; need <= budget {
			return release, true, rows.Err()
		}
	}
	return nil, false, rows.Err()
}

// held reports whether a live turn of handle's seat other than except holds
// turn as its base: its row names turn, has not ended and has landed no
// capture of its own. What every release of one row asks before it takes it.
func held(ctx context.Context, tx *sql.Tx, handle, turn, except string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, heldStatement, handle, turn, except).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("executordomain: read whether %s's turn %s is held: %w",
			handle, turn, err)
	}
	return true, nil
}

// heldBases is every base the live turns of handle's seat other than except
// hold, read in one pass of the bases' index.
func heldBases(ctx context.Context, tx *sql.Tx, handle, except string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, heldBasesStatement, handle, except)
	if err != nil {
		return nil, fmt.Errorf("executordomain: read the bases %s's live turns hold: %w", handle, err)
	}
	out := map[string]bool{}
	for rows.Next() {
		var base string
		if scanErr := rows.Scan(&base); scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		out[base] = true
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// baseStands is why a begin's base cannot be held, or "" where it can: the base
// turn's row is still there, ended at or before the read that saw it, in the
// begin's conversation, and naming the very session that read saw. What the
// apply decides the row's base by, and the writer reads back to say why a
// begin holds nothing.
//
// ANYTHING ELSE IS A BASE THAT MOVED between the read and the record: an end
// ordered before the begin absorbed or deduped it, a capture released it past
// the seat's budget, the retention deleted it, or the seat was removed. The
// begin then holds nothing rather than a row naming another session, or none.
func baseStands(ctx context.Context, tx *sql.Tx, handle, conversationKey string,
	readThrough int64, baseTurn, object, hash string) (string, error) {

	row, found, err := readSession(ctx, tx, handle, baseTurn)
	switch {
	case err != nil:
		return "", err
	case !found:
		return "the base turn's row is gone: another turn's end absorbed or deduped it, " +
			"the retention deleted it or its seat was removed", nil
	case !row.ended || row.endSeq > readThrough:
		return "the base turn's row is not the ended turn the read saw", nil
	case row.conversationKey != conversationKey:
		return "the base turn is another conversation's", nil
	case !row.hasObject || row.object != object || row.objectHash != hash:
		return "the base turn's row no longer names the session the read saw: the seat's " +
			"budget or its turn's end released it", nil
	}
	return "", nil
}

// expiredIn reports whether a turn has a row or a tombstone in conversation
// whose last instant is at or before cutoff (a broker instant as the estate
// stores one): what a retention delete removes, decided by the writer and again
// by the applier.
//
// A TURN THAT NEVER ENDED IS TAKEN TOO. The retention duty never nominates a
// turn whose run the fleet still holds, so a row with no end and no run is one
// whose end was lost, and the retention is what bounds it; a rule that kept it
// would keep it for ever.
//
// A ROW A LIVE TURN HOLDS AS ITS BASE IS NOT TAKEN, however old ([held]): that
// turn's relaunch continues it. The hold lasts until the holder lands a capture
// or ends, and a holder whose run was lost is ended within [LostAfter], so the
// row is taken on a pass after that.
func expiredIn(ctx context.Context, tx *sql.Tx, handle, conversationKey, turn string,
	cutoff int64) (bool, error) {

	row, found, err := readSession(ctx, tx, handle, turn)
	if err != nil {
		return false, err
	}
	if found {
		if row.conversationKey != conversationKey || row.lastAt > cutoff {
			return false, nil
		}
		holds, holdErr := held(ctx, tx, handle, turn, turn)
		return !holds, holdErr
	}
	marker, tombed, tombErr := readTomb(ctx, tx, handle, turn)
	if tombErr != nil || !tombed {
		return false, tombErr
	}
	return marker.conversationKey == conversationKey && marker.at <= cutoff, nil
}

// applyConversation dispatches a record on a conversation's subject.
//
// THE SUBJECT IS HELD TO THE PAYLOAD: a record whose payload names a turn of
// another conversation than the subject it was arbitrated on writes nothing,
// because the arbitration it passed was about a different object.
func applyConversation(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	switch p := payload.(type) {
	case Begin:
		if !c.on(ConversationSubject(p.Handle, p.ConversationKey, p.Turn)) {
			return 0, nil
		}
		return applyBegin(ctx, tx, c, p)
	case Capture:
		if !c.on(ConversationSubject(p.Handle, p.ConversationKey, p.Turn)) {
			return 0, nil
		}
		return applyCapture(ctx, tx, c, p)
	case End:
		if !c.on(ConversationSubject(p.Handle, p.ConversationKey, p.Turn)) {
			return 0, nil
		}
		return applyEnd(ctx, tx, c, p)
	case Delete:
		if !c.on(ConversationSubject(p.Handle, p.ConversationKey, p.Turn)) {
			return 0, nil
		}
		return applyDelete(ctx, tx, c, p)
	}
	return 0, fmt.Errorf("executordomain: the %s record at %s carries a %T",
		c.record.Op, c.position, payload)
}

// flag is a boolean as the estate stores one.
func flag(b bool) int {
	if b {
		return 1
	}
	return 0
}

// on reports whether the record was published on subject.
func (c applyContext) on(subject Subject) bool { return c.record.Subject == subject }

// applyCapture names a new object for a live turn, releasing the seat's
// oldest ended sessions first where its budget needs the room.
//
// A CAPTURE THAT WOULD NAME WHAT NO READER CAN PARSE WRITES NOTHING: an object
// key or digest the store does not spell, or a list of absorbed turns that is
// not one ([absorbsOf]). The writer refuses both; this is the applier holding a
// record that skipped it to the rule, rather than a row that fails the
// collector, the audit, the backup and every read of the seat on every node.
func applyCapture(ctx context.Context, tx *sql.Tx, c applyContext, p Capture) (int, error) {
	absorbs, wellFormed := absorbsOf(p.Absorbs, p.Turn, p.ReadThrough)
	if !present(p.Handle) || !present(p.Turn) || p.ObjectSize <= 0 || p.SeatBudget <= 0 ||
		p.ContextTokens < 0 || !nameable(p.Object, p.ObjectHash) || !wellFormed {
		return 0, nil
	}
	// THE SEAT'S REMOVAL FIRST: a capture decided before a removal it did
	// not see is a turn of the seat the company removed, whether or not the
	// removal found a row of it to tombstone.
	removal, err := readRemoval(ctx, tx, p.Handle)
	if err != nil {
		return 0, err
	}
	if removal > p.SeenRemoval {
		return 0, nil
	}
	if _, tombed, tombErr := readTomb(ctx, tx, p.Handle, p.Turn); tombErr != nil || tombed {
		return 0, tombErr
	}
	row, found, err := readSession(ctx, tx, p.Handle, p.Turn)
	if err != nil {
		return 0, err
	}
	if found && (row.ended || row.conversationKey != p.ConversationKey ||
		!sameWork(row.workKey, p.WorkKey) ||
		!newer(p.LaunchSeq, p.CaptureSeq, row.launchSeq, row.captureSeq)) {
		return 0, nil
	}
	release, fits, err := seatPlan(ctx, tx, p.Handle, p.Turn, p.ObjectSize, p.SeatBudget)
	if err != nil || !fits {
		return 0, err
	}
	written := 0
	for _, turn := range release {
		// THE OBJECT GOES AND THE ENDING STAYS: the ending is the account
		// of the turn a transcript cannot carry, and what the budget bounds
		// is bytes in the object store, which the row's naming holds.
		res, releaseErr := tx.ExecContext(ctx, releaseStatement, c.packed, p.Handle, turn)
		if releaseErr != nil {
			return written, fmt.Errorf("executordomain: release %s's session of turn %s "+
				"at %s: %w", p.Handle, turn, c.position, releaseErr)
		}
		written += affected(res)
	}
	// THE TURN'S FIRST READ AND ITS WORK STAY WHAT THEY WERE, decided here
	// from the row rather than inside the statement: the read is the turn's
	// first launch's (a relaunch reads the turn's own row, not the head), and
	// the work key is the first non-empty one any record about the turn
	// carried.
	readThrough, absorbed, work := p.ReadThrough, strings.Join(absorbs, ","), p.WorkKey
	var res sql.Result
	if found {
		if row.readThrough > 0 {
			readThrough, absorbed = row.readThrough, row.absorbs
		}
		if row.workKey != "" {
			work = row.workKey
		}
		res, err = tx.ExecContext(ctx, updateCaptureStatement,
			work, p.Harness, p.HarnessVersion, p.SessionID, p.Object, p.ObjectHash, p.ObjectSize,
			p.ContentKey, p.ArchiveFormat, p.LaunchSeq, p.CaptureSeq, p.Home, p.Workspace,
			p.ContextTokens, flag(p.Interrupted), c.at(), readThrough, absorbed, c.at(), c.packed,
			p.Handle, p.Turn, c.packed)
	} else {
		res, err = tx.ExecContext(ctx, insertCaptureStatement,
			p.Handle, p.Turn, p.ConversationKey, work, p.Harness, p.HarnessVersion,
			p.SessionID, p.Object, p.ObjectHash, p.ObjectSize, p.ContentKey, p.ArchiveFormat,
			p.LaunchSeq, p.CaptureSeq, p.Home, p.Workspace, p.ContextTokens, flag(p.Interrupted),
			c.at(), readThrough, absorbed, c.at(), c.packed, c.packed)
	}
	if err != nil {
		return written, fmt.Errorf("executordomain: record %s's capture of turn %s at %s: %w",
			p.Handle, p.Turn, c.position, err)
	}
	return written + affected(res), nil
}

// applyBegin records a turn's first read of its conversation and the base that
// read froze, which the turn's row then holds while it is live and has landed
// no capture.
//
// EVERY GUARD IS THE DECIDE'S ([Writer.Begin]), repeated in log order:
//
//   - THE SHAPE: a read (a position, and the turns it handed out as
//     [absorbsOf] holds them), on a conversation, and a base that is one of
//     those turns, not the beginning turn itself, naming a session every
//     reader can parse — or no base at all;
//   - THE SEAT AND THE TURN: no removal of the seat newer than the one the
//     decide saw, and no tombstone of the turn;
//   - THE TURN'S ROW: none, or a live one of the same conversation and unit of
//     work that holds no read yet. A turn's FIRST READ STANDS, whichever of
//     its records carried it first, so a begin landing after it — retried,
//     or late — writes nothing;
//   - THE BASE: held only where its row still names the session the read saw
//     ([baseStands]); a base that moved since is written as none, so the
//     begin still records the read and holds nothing.
//
// A begin and every end, lost end and retention delete of the conversation are
// arbitrated on one subject, so whichever the log ordered first decides: an end
// first leaves no base to hold, and a begin first holds the base against the
// end ([held]).
func applyBegin(ctx context.Context, tx *sql.Tx, c applyContext, p Begin) (int, error) {
	absorbs, wellFormed := absorbsOf(p.Absorbs, p.Turn, p.ReadThrough)
	if !present(p.Handle) || !present(p.Turn) || p.ConversationKey == "" ||
		p.ReadThrough <= 0 || !wellFormed || !p.baseShaped(absorbs) {
		return 0, nil
	}
	removal, err := readRemoval(ctx, tx, p.Handle)
	if err != nil {
		return 0, err
	}
	if removal > p.SeenRemoval {
		return 0, nil
	}
	if _, tombed, tombErr := readTomb(ctx, tx, p.Handle, p.Turn); tombErr != nil || tombed {
		return 0, tombErr
	}
	row, found, err := readSession(ctx, tx, p.Handle, p.Turn)
	if err != nil {
		return 0, err
	}
	if found && (row.ended || row.conversationKey != p.ConversationKey ||
		!sameWork(row.workKey, p.WorkKey) || row.readThrough > 0) {
		return 0, nil
	}
	base := p.BaseTurn
	if base != "" {
		why, standErr := baseStands(ctx, tx, p.Handle, p.ConversationKey, p.ReadThrough,
			p.BaseTurn, p.BaseObject, p.BaseHash)
		if standErr != nil {
			return 0, standErr
		}
		if why != "" {
			base = ""
		}
	}
	absorbed := strings.Join(absorbs, ",")
	var res sql.Result
	if found {
		work := p.WorkKey
		if row.workKey != "" {
			work = row.workKey
		}
		res, err = tx.ExecContext(ctx, updateBeginStatement,
			work, p.ReadThrough, absorbed, base, c.packed, p.Handle, p.Turn, c.packed)
	} else {
		res, err = tx.ExecContext(ctx, insertBeginStatement,
			p.Handle, p.Turn, p.ConversationKey, p.WorkKey, p.ReadThrough, absorbed, base,
			c.at(), c.packed, c.packed)
	}
	if err != nil {
		return 0, fmt.Errorf("executordomain: record %s's begin of turn %s at %s: %w",
			p.Handle, p.Turn, c.position, err)
	}
	return affected(res), nil
}

// baseShaped reports a begin's base as a read hands one out: none at all, or one
// of absorbs — the turns the read handed out, as [absorbsOf] checked them —
// naming a session every reader can parse ([nameable]). Every turn absorbsOf
// passes is a canonical turn id other than the beginning turn's own, so a base
// among them is one too, and neither needs a test of its own.
func (p Begin) baseShaped(absorbs []string) bool {
	if p.BaseTurn == "" {
		return p.BaseObject == "" && p.BaseHash == ""
	}
	return slices.Contains(absorbs, p.BaseTurn) && nameable(p.BaseObject, p.BaseHash)
}

// endBody is an end's payload with its ending as the bytes the record carries,
// which the row keeps exactly: every node writes the bytes the record holds,
// whatever its own build would encode the same ending as.
type endBody struct {
	Ending json.RawMessage `json:"ending"`
}

// applyEnd stamps how a turn ended and tombstones it; with a row it absorbs
// what the turn's first launch was handed and dedupes on the work key.
//
// THE END IS AUTHORITATIVE FOR THE TURN'S WORK KEY: a row a capture wrote with
// no key takes the end's, so the next attempt at the same unit of work finds
// this one to dedupe whether or not its captures carried the key; an end
// naming another key than the row's writes nothing, as the decide refuses it.
//
// AN END'S RULE IS ITS STAMP'S. Where the turn's newest capture was not kept and
// the row names an older one, an end stamped [wentOnVersion] or above keeps that
// session and absorbs and dedupes as any turn that landed a capture does, its
// ending saying the turn went on past the session; one stamped below releases
// the session and absorbs nothing, as the builds that write such ends apply
// them. Decided on the stamp rather than on whether the ending carries went_on,
// so a record means one thing on every node, whichever build applies it.
func applyEnd(ctx context.Context, tx *sql.Tx, c applyContext, p End) (int, error) {
	if !present(p.Handle) || !present(p.Turn) || !p.Ending.Within() ||
		len(p.AskExcerpt) > AskExcerptMaxBytes {
		return 0, nil
	}
	if _, tombed, tombErr := readTomb(ctx, tx, p.Handle, p.Turn); tombErr != nil || tombed {
		return 0, tombErr
	}
	row, found, err := readSession(ctx, tx, p.Handle, p.Turn)
	if err != nil {
		return 0, err
	}
	if found && (row.ended || row.conversationKey != p.ConversationKey ||
		!sameWork(row.workKey, p.WorkKey)) {
		return 0, nil
	}
	if !found {
		// A TURN THE SEAT'S REMOVAL CANNOT HAVE SEEN: it had no row to
		// tombstone, so its end, decided before a removal it did not see,
		// would write an ending for a seat that is gone. A turn with a row
		// needs no such check: the removal deleted every row the seat held,
		// so a row now is one a capture or a begin of the seat's next tenure
		// wrote, each held to the removal it saw.
		removal, removalErr := readRemoval(ctx, tx, p.Handle)
		if removalErr != nil {
			return 0, removalErr
		}
		if removal > p.SeenRemoval {
			return 0, nil
		}
	}
	var body endBody
	if err = json.Unmarshal(c.record.Mutation, &body); err != nil {
		return 0, fmt.Errorf("executordomain: the end at %s: %w", c.position, err)
	}
	work := p.WorkKey
	if found && row.workKey != "" {
		work = row.workKey
	}
	delivered := p.Ending.Delivered
	written := 0
	// BY THE RULE OF THE END'S OWN STAMP ([wentOnVersion]). An end stamped below
	// it keeps the rule it was written under: the turn's last attempt was not
	// kept, so the older capture the row names goes below — decided on the row's
	// own place, in log order, so a capture of that attempt that landed after all
	// is the row's place, and is kept. An end stamped at it or above never takes
	// the row's session off for an attempt that was not kept: the session holds
	// what the turn did up to it, and the end says the turn went on past it
	// ([executorkeep.Ending.WentOn]), which every reader is told.
	stale := false
	if u := p.Ending.Unkept; found && row.hasObject && u != nil && c.record.V < wentOnVersion {
		stale = unkeptSupersedes(*u, row.launchSeq, row.captureSeq)
	}
	// ONLY A TURN WHOSE ROW KEEPS A SESSION ABSORBS AND DEDUPES: no other holds what
	// its read handed out, so those turns are told again rather than lost with it.
	keepsSession := found && row.hasObject && !stale && !slices.Contains(p.Ending.Exhausted, row.sessionID)
	if keepsSession && p.ConversationKey != "" {
		absorbed, merged, absorbErr := absorb(ctx, tx, c, p, work, row)
		if absorbErr != nil {
			return 0, absorbErr
		}
		written += absorbed
		delivered = delivered || merged
	}
	var res sql.Result
	if found {
		res, err = tx.ExecContext(ctx, endRowStatement,
			work, c.at(), c.packed, string(p.Ending.Outcome), flag(delivered),
			string(body.Ending), p.AskExcerpt, c.at(), c.packed, p.Handle, p.Turn)
	} else {
		// NOTHING WAS CAPTURED AND NOTHING BEGUN: an ended row naming no
		// object, which holds the ending alone, so the conversation's next
		// turn is still told how this one ended. It absorbs nothing: it
		// read nothing it could stand in for. A turn whose begin wrote its
		// row ends on that row instead, above, and absorbs nothing either.
		res, err = tx.ExecContext(ctx, insertEndingStatement,
			p.Handle, p.Turn, p.ConversationKey, work, c.at(), c.packed,
			string(p.Ending.Outcome), flag(delivered), string(body.Ending), p.AskExcerpt,
			c.at(), c.packed, c.packed)
	}
	if err != nil {
		return written, fmt.Errorf("executordomain: end %s's turn %s at %s: %w",
			p.Handle, p.Turn, c.position, err)
	}
	written += affected(res)
	retired, err := retireExhausted(ctx, tx, c, p)
	if err != nil {
		return written, err
	}
	written += retired
	// AFTER THE RETIREMENT, so a row that ran out says that rather than this.
	if stale {
		res, err = tx.ExecContext(ctx, releaseStatement, c.packed, p.Handle, p.Turn)
		if err != nil {
			return written, fmt.Errorf("executordomain: release %s's stale session of turn %s at %s: %w",
				p.Handle, p.Turn, c.position, err)
		}
		written += affected(res)
	}
	n, err := tomb(ctx, tx, c, p.Handle, p.Turn, p.ConversationKey, tombEnded)
	return written + n, err
}

// retireExhausted takes each session the turn ran out of context in off every ended
// row of its conversation, its own included. Held rows too: a hold keeps a session
// for a relaunch to continue, which would only meet the same end.
func retireExhausted(ctx context.Context, tx *sql.Tx, c applyContext, p End) (int, error) {
	if p.ConversationKey == "" {
		return 0, nil
	}
	written := 0
	for _, session := range p.Ending.Exhausted {
		res, err := tx.ExecContext(ctx, retireStatement, p.Turn, c.packed, p.Handle, p.ConversationKey, session)
		if err != nil {
			return written, fmt.Errorf("executordomain: retire %s's session %s at %s: %w",
				p.Handle, session, c.position, err)
		}
		written += affected(res)
	}
	return written, nil
}

// absorb deletes the conversation's ended rows the ending turn stands in for,
// and reports whether an older attempt at its work delivered the work's reply.
//
// EXACTLY THE TURNS ITS FIRST READ HANDED OUT ([Capture.Absorbs]), each only
// where it ended at or before that read: the head the launch continued, the
// endings it was told and the siblings it was offered are held by this turn's
// own session and account now. A turn the read left out — past [MaxEndings],
// or ended after it — was never shown to this one, so it stays for the next
// turn's read to show; absorbed blind, its ending and its session would be
// gone with nobody ever told of them.
//
// AND EVERY OLDER ATTEMPT AT THE SAME UNIT OF WORK, shown or not, whose
// `delivered` it reports so the surviving row says the work's reply went out:
// an attempt the work was dispatched again over is superseded by the attempt
// that ended after it. AN EMPTY WORK KEY NEVER DEDUPES: it is the honest
// default of a turn with no unit of work to collapse on (ADR-0017), and
// matching on it would let every keyless turn erase its keyless siblings.
//
// NEITHER TAKES A ROW A LIVE TURN HOLDS AS ITS BASE ([held]), asked of each
// row before it is deleted: that turn's relaunch continues it. What an attempt
// delivered is merged whether it is deleted or held. A held row stays an
// ordinary ended row, which its holder's own end absorbs in its turn.
//
// Each deleted row's tombstone was written by its own `end`, so nothing
// recreates it.
func absorb(ctx context.Context, tx *sql.Tx, c applyContext, p End, work string,
	row sessionRow) (int, bool, error) {

	written, delivered := 0, false
	if work != "" {
		attempts, listErr := olderAttempts(ctx, tx, p.Handle, p.ConversationKey, work, p.Turn)
		if listErr != nil {
			return 0, false, listErr
		}
		for _, older := range attempts {
			delivered = delivered || older.delivered
			n, dropErr := dropUnheld(ctx, tx, c, dedupeStatement, p.Handle, older.turn, p.Turn,
				p.Handle, older.turn)
			if dropErr != nil {
				return written, false, dropErr
			}
			written += n
		}
	}
	if row.readThrough <= 0 {
		return written, delivered, nil
	}
	for _, shown := range splitAbsorbs(row.absorbs) {
		n, dropErr := dropUnheld(ctx, tx, c, absorbStatement, p.Handle, shown, p.Turn,
			p.Handle, shown, p.ConversationKey, row.readThrough)
		if dropErr != nil {
			return written, false, dropErr
		}
		written += n
	}
	return written, delivered, nil
}

// attempt is one older ended attempt at a unit of work.
type attempt struct {
	turn      string
	delivered bool
}

// olderAttempts lists the older ended attempts at work in a conversation, read
// whole and closed before any of them is deleted.
func olderAttempts(ctx context.Context, tx *sql.Tx, handle, conversationKey, work,
	turn string) ([]attempt, error) {

	rows, err := tx.QueryContext(ctx, attemptsStatement, handle, conversationKey, work, turn)
	if err != nil {
		return nil, fmt.Errorf("executordomain: read the attempts at %s's work %s: %w",
			handle, work, err)
	}
	var out []attempt
	for rows.Next() {
		var (
			a         attempt
			delivered int
		)
		if scanErr := rows.Scan(&a.turn, &delivered); scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		a.delivered = delivered == 1
		out = append(out, a)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// dropUnheld runs statement, a delete of one ended turn's row, unless a live
// turn of the seat other than except holds that turn as its base.
func dropUnheld(ctx context.Context, tx *sql.Tx, c applyContext, statement, handle, turn,
	except string, args ...any) (int, error) {

	holds, err := held(ctx, tx, handle, turn, except)
	if err != nil || holds {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, statement, args...)
	if err != nil {
		return 0, fmt.Errorf("executordomain: delete %s's turn %s at %s: %w",
			handle, turn, c.position, err)
	}
	return affected(res), nil
}

// tomb writes a turn's tombstone, keeping the first one a turn was given.
func tomb(ctx context.Context, tx *sql.Tx, c applyContext, handle, turn,
	conversationKey, reason string) (int, error) {

	res, err := tx.ExecContext(ctx, tombStatement,
		handle, turn, conversationKey, c.packed, c.at(), reason)
	if err != nil {
		return 0, fmt.Errorf("executordomain: tombstone %s's turn %s at %s: %w",
			handle, turn, c.position, err)
	}
	return affected(res), nil
}

// applyDelete removes one turn's row and tombstone past the retention, decided
// again here against the row's own last instant ([expiredIn]): a turn captured
// since the duty nominated it stays. A turn whose row an absorption took
// already is removed by its tombstone's instant.
func applyDelete(ctx context.Context, tx *sql.Tx, c applyContext, p Delete) (int, error) {
	if !present(p.Handle) || !present(p.Turn) {
		return 0, nil
	}
	due, err := expiredIn(ctx, tx, p.Handle, p.ConversationKey, p.Turn, store.EncodeTime(p.Cutoff))
	if err != nil || !due {
		return 0, err
	}
	return removeTurn(ctx, tx, c, p.Handle, p.Turn)
}

// removeTurn deletes one turn's row and its tombstone.
//
// THE TOMBSTONE GOES WITH THE ROW, which is a departure from the rule that a
// delete leaves one, and deliberately: a tombstone refuses a record DECIDED
// before the row went, and by the retention none can still land. The term is at
// least a day past the row's own last instant, so every capture is past the
// objstore.RecordWithin bound its object has to be named inside, and the duty
// never nominates a turn whose run is still live, which is what writes a turn's
// records. What can still arrive is an `end` a run's own retry publishes late;
// it writes an ending-only row, which the retention takes in its turn. A
// tombstone kept instead would itself be a row the retention had to take a
// second time, a term later, for nothing.
func removeTurn(ctx context.Context, tx *sql.Tx, c applyContext, handle, turn string) (int, error) {
	written := 0
	for _, statement := range []string{deleteSessionStatement, deleteTombStatement} {
		res, err := tx.ExecContext(ctx, statement, handle, turn)
		if err != nil {
			return written, fmt.Errorf("executordomain: delete %s's turn %s at %s: %w",
				handle, turn, c.position, err)
		}
		written += affected(res)
	}
	return written, nil
}

// applySeat dispatches a record on a seat's subject: the seat's removal, or the
// retention of that removal.
func applySeat(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	switch p := payload.(type) {
	case SeatDelete:
		if !present(p.Handle) || !c.on(SeatSubject(p.Handle)) {
			return 0, nil
		}
		return removeSeat(ctx, tx, c, p.Handle)
	case Delete:
		if !present(p.Handle) || p.Turn != "" || p.ConversationKey != "" ||
			!c.on(SeatSubject(p.Handle)) {
			return 0, nil
		}
		res, err := tx.ExecContext(ctx, forgetRemovalStatement, p.Handle, store.EncodeTime(p.Cutoff))
		if err != nil {
			return 0, fmt.Errorf("executordomain: forget seat %s's removal at %s: %w",
				p.Handle, c.position, err)
		}
		return affected(res), nil
	}
	return 0, fmt.Errorf("executordomain: the %s record at %s carries a %T",
		c.record.Op, c.position, payload)
}

// removeSeat removes every row a removed seat holds, tombstoning the turns that
// had not ended, and records the position it applied at.
//
// THE TOMBSTONES AND THE RECORD OF THE REMOVAL GUARD TWO DIFFERENT TURNS. A
// turn with a live row is tombstoned, so a later capture of it is refused as
// any ended turn's is; an ended turn's tombstone is its own `end`'s. A turn
// that had no row yet — its first capture, or the end of a turn that captured
// nothing, decided before the removal and landing after it — has nothing to
// tombstone, and is refused by the removal's position instead, which every
// capture and end carries the one its decide saw ([Capture.SeenRemoval]).
func removeSeat(ctx context.Context, tx *sql.Tx, c applyContext, handle string) (int, error) {
	type live struct{ turn, conversationKey string }
	var open []live
	rows, err := tx.QueryContext(ctx, seatLiveStatement, handle)
	if err != nil {
		return 0, fmt.Errorf("executordomain: read %s's live turns at %s: %w",
			handle, c.position, err)
	}
	for rows.Next() {
		var l live
		if scanErr := rows.Scan(&l.turn, &l.conversationKey); scanErr != nil {
			_ = rows.Close()
			return 0, scanErr
		}
		open = append(open, l)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	written := 0
	for _, l := range open {
		n, tombErr := tomb(ctx, tx, c, handle, l.turn, l.conversationKey, tombSeatRemoved)
		if tombErr != nil {
			return written, tombErr
		}
		written += n
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{seatRowsStatement, []any{handle}},
		{removeSeatStatement, []any{handle, c.packed, c.at()}},
	} {
		res, execErr := tx.ExecContext(ctx, statement.sql, statement.args...)
		if execErr != nil {
			return written, fmt.Errorf("executordomain: remove seat %s at %s: %w",
				handle, c.position, execErr)
		}
		written += affected(res)
	}
	return written, nil
}
