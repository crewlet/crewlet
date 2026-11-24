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

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/store"
)

// THE SESSION CASES: a begin, a capture, an end and a retention delete on a
// conversation's subject, and a seat's removal and its retention on the seat's.
// Each reads its rows IN THIS TRANSACTION through the decide's own helpers
// ([readSession], [readTomb], [readRemoval], [seatPlan], [expiredIn],
// [baseStands], [held]), so both hold one rule: the decide refuses what it can
// see, and the apply repeats every deterministic guard for what changed since.
//
// Every release skips a held row (package doc, "A turn's first read holds its
// base"), asked per row ([held], [heldBases]) rather than in a subquery the
// planner rewrites into a read of the whole table (storetest.Decorrelated).
// Every statement is named, so the plan gate plans these very statements
// (TestTheExecutorStatementsArePlanned).

// THE INDEX A STATEMENT WAS WRITTEN FOR, which it names with `INDEXED BY`.
// Every index on executor_sessions leads with the seat's handle, and the
// planner, which keeps no statistics, otherwise seeks on the handle alone
// through whichever such index it takes, reading every row of the seat inside
// an apply that holds the store's one writer. The hint also makes a schema
// change that drops the index fail the statement loudly rather than slow it.
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

	// sessionIdx is the rows naming one session id, whatever their seat:
	// whether a minted id is fresh ([Reader.SessionHeld]).
	sessionIdx = "executor_sessions_session_idx"

	// heldIdx is the bases live turns hold: a PARTIAL index over exactly the
	// rows that hold one (live, no capture landed, naming a base), whose
	// predicate every statement through it repeats word for word, which is
	// what lets the planner and the hint use it.
	heldIdx = "executor_sessions_held_idx"

	// servesKey is the index executor_session_serves' primary key
	// (agent_handle, turn_id, conversation_key) builds: the conversations one
	// turn serves beyond its own, and one of them.
	servesKey = "sqlite_autoindex_executor_session_serves_1"

	// servesLineIdx is the turns a conversation reads as served, in the
	// order their ends applied: its served head and endings.
	servesLineIdx = "executor_session_serves_line_idx"

	// summaryKey is the index executor_session_summaries' primary key
	// (agent_handle, turn_id) builds: one turn's summary.
	summaryKey = "sqlite_autoindex_executor_session_summaries_1"
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
	// hold, read once before the budget's release walks rather than per row.
	heldBasesStatement = `
		SELECT base_turn FROM executor_sessions INDEXED BY ` + heldIdx + `
		WHERE agent_handle = ? AND end_seq IS NULL AND launch_seq = 0
		  AND base_turn <> '' AND turn_id <> ?`

	// servesAnyStatement is whether a turn serves a conversation beyond its
	// own; servesStatement whether it serves one.
	servesAnyStatement = `
		SELECT 1 FROM executor_session_serves INDEXED BY ` + servesKey + `
		WHERE agent_handle = ? AND turn_id = ? LIMIT 1`
	servesStatement = `
		SELECT 1 FROM executor_session_serves INDEXED BY ` + servesKey + `
		WHERE agent_handle = ? AND turn_id = ? AND conversation_key = ?`

	// servedSessionStatement is the turns a conversation reads as served whose
	// rows still name one session, which an end retiring that session drops.
	servedSessionStatement = `
		SELECT v.turn_id
		  FROM executor_session_serves AS v INDEXED BY ` + servesLineIdx + `
		 CROSS JOIN executor_sessions AS s INDEXED BY ` + sessionKey + `
		 WHERE v.agent_handle = ? AND v.conversation_key = ?
		   AND s.agent_handle = v.agent_handle AND s.turn_id = v.turn_id
		   AND s.session_id = ? AND s.object IS NOT NULL`
)

// The statements an apply writes the rows with.
const (
	// releaseStatement takes an ended turn's object off its row, keeping
	// the row's ending.
	releaseStatement = `
		UPDATE executor_sessions INDEXED BY ` + sessionKey + ` SET object = NULL, object_hash = '',
			object_size = 0, content_key = '', version = ?
		WHERE agent_handle = ? AND turn_id = ?`

	// retireStatement takes a spent session off every ended row of one
	// conversation that names it, saying which turn spent it and how.
	retireStatement = `
		UPDATE executor_sessions INDEXED BY ` + conversationIdx + ` SET object = NULL, object_hash = '',
			object_size = 0, content_key = '', retired_by = ?, retired_loss = ?, version = ?
		WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
		  AND session_id = ? AND object IS NOT NULL`

	// insertCaptureStatement is a turn's first capture where no begin of
	// it landed first: the row begins here, at first_seq.
	insertCaptureStatement = `
		INSERT INTO executor_sessions (
			agent_handle, turn_id, conversation_key, work_key, harness, harness_version,
			entry, session_id, object, object_hash, object_size, content_key, archive_format,
			launch_seq, capture_seq, home, workspace, context_tokens, interrupted,
			captured_at, read_through, absorbs, last_at, first_seq, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, turn_id) DO NOTHING`

	// updateCaptureStatement is a later capture of a live turn, or its first
	// onto the row its begin wrote: every value, first read and work key
	// included, is the one [applyCapture] decided. It leaves the base as the
	// begin wrote it; the capture's own launch_seq is what lifts the hold.
	updateCaptureStatement = `
		UPDATE executor_sessions INDEXED BY ` + sessionKey + ` SET work_key = ?, harness = ?, harness_version = ?,
			entry = ?, session_id = ?, object = ?, object_hash = ?, object_size = ?,
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
	// value is the one [applyBegin] decided from the row.
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

	// insertServedStatement is one conversation an ending turn served beyond
	// its own, placed at the end's position.
	insertServedStatement = `
		INSERT INTO executor_session_serves (agent_handle, turn_id, conversation_key, end_seq)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (agent_handle, turn_id, conversation_key) DO NOTHING`

	// absorbServedStatement deletes one turn a conversation reads as served
	// that the ending turn's first read handed out, by its key, and only where
	// it was placed at or before that read, as [absorbStatement] does.
	absorbServedStatement = `
		DELETE FROM executor_session_serves INDEXED BY ` + servesKey + `
		WHERE agent_handle = ? AND turn_id = ? AND conversation_key = ? AND end_seq <= ?`

	// dropServedStatement deletes one turn a conversation reads as served.
	dropServedStatement = `
		DELETE FROM executor_session_serves INDEXED BY ` + servesKey + `
		WHERE agent_handle = ? AND turn_id = ? AND conversation_key = ?`

	// turnServedStatement deletes every conversation one turn serves beyond
	// its own, with its row; seatServedStatement every one a removed seat's
	// turns serve.
	turnServedStatement = `
		DELETE FROM executor_session_serves INDEXED BY ` + servesKey + `
		WHERE agent_handle = ? AND turn_id = ?`
	seatServedStatement = `DELETE FROM executor_session_serves WHERE agent_handle = ?`

	// insertSummaryStatement names the summary an end named on its turn's
	// row ([executorkeep.Ending.Summary]); releaseSummaryStatement takes one
	// turn's off with its row's session or its row, and seatSummariesStatement
	// every one a removed seat's rows keep.
	insertSummaryStatement = `
		INSERT INTO executor_session_summaries (agent_handle, turn_id, object, object_hash, object_size,
			content_key, harness, session_id, kept_at, bytes, kept_bytes, cut, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, turn_id) DO NOTHING`
	releaseSummaryStatement = `
		DELETE FROM executor_session_summaries INDEXED BY ` + summaryKey + `
		WHERE agent_handle = ? AND turn_id = ?`
	seatSummariesStatement = `DELETE FROM executor_session_summaries WHERE agent_handle = ?`

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
// can parse: a key in the store's canonical spelling and a SHA-256 digest. A row
// naming anything else would fail the collector, its audit, the backup and every
// head and page read of the seat, on every node, for as long as it stays.
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

// checkServed refuses an end's other conversations where they could not be
// ones it served ([End.Served]): past [MaxServed], a key no row could hold, one
// twice, or the turn's own. The writer refuses by it and the apply repeats it.
func checkServed(served []string, own string) error {
	if len(served) > MaxServed {
		return invalid("the end names %d other conversations served and the maximum is %d",
			len(served), MaxServed)
	}
	seen := make(map[string]bool, len(served))
	for _, key := range served {
		switch {
		case key == "":
			return invalid("the end names a conversation served with no key")
		case key == own:
			return invalid("the end names its own conversation among the others it served")
		case seen[key]:
			return invalid("the end names conversation %q as served twice", key)
		}
		if err := checkText("a conversation served", key, ConversationKeyMaxBytes); err != nil {
			return err
		}
		seen[key] = true
	}
	return nil
}

// servedValid reports whether served passes [checkServed]: a record the writer
// would have refused applies as nothing, never as an error, which would stop
// the applier on a record every node reads alike.
func servedValid(served []string, own string) bool { return checkServed(served, own) == nil }

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
// THE OLDEST ENDINGS GO FIRST, never a live turn's session (its next launch
// continues it), this turn's own (being replaced) or a base another live turn
// holds ([heldBases]). A base THIS turn holds is released: this capture is the
// turn's first landed one and lifts the hold, its own session now holding all
// the base did, which is what lets a seat at its budget's floor take a
// continued turn's first capture at all. An end's position is unique, so every
// node releases the same sessions. When even every releasable session would not
// make room, nothing is chosen and the capture is refused whole.
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
	// The holds (a handful) are read whole first, so no statement runs while
	// the walk's rows are open.
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

// served reports whether turn of handle's seat serves a conversation beyond its
// own: conversationKey, or any where it is "". What an absorption or a dedupe
// in the turn's own conversation asks before it takes the turn's row, and what
// a begin asks of a base read as served.
func served(ctx context.Context, tx *sql.Tx, handle, turn, conversationKey string) (bool, error) {
	statement, args := servesAnyStatement, []any{handle, turn}
	if conversationKey != "" {
		statement, args = servesStatement, append(args, conversationKey)
	}
	var one int
	err := tx.QueryRowContext(ctx, statement, args...).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("executordomain: read the conversations %s's turn %s serves: %w",
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
// begin's conversation or serving it ([served]), and naming the very session
// that read saw. The apply
// decides the row's base by it, and the writer says by it why a begin holds
// nothing.
//
// Anything else is a base that moved between the read and the record (absorbed,
// deduped, released past the budget, deleted by the retention, or its seat
// removed), so the begin holds nothing rather than a row naming another session.
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
		serves, servesErr := served(ctx, tx, handle, baseTurn, conversationKey)
		if servesErr != nil || !serves {
			return "the base turn is another conversation's, and no longer serves this one", servesErr
		}
	}
	if !row.hasObject || row.object != object || row.objectHash != hash {
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
// A TURN THAT NEVER ENDED IS TAKEN TOO: the duty never nominates a turn whose
// run the fleet still holds, so such a row lost its end, and only the retention
// bounds it. A ROW A LIVE TURN HOLDS AS ITS BASE IS NOT, however old ([held]); a
// lost holder is ended within [LostAfter], so a later pass takes the row.
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
// A CAPTURE NAMING WHAT NO READER CAN PARSE WRITES NOTHING ([nameable],
// [absorbsOf]): the writer refuses it too, and the applier holds a record that
// skipped the writer to the rule rather than write a row every reader fails on.
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
		// THE OBJECT GOES AND THE ENDING STAYS: the budget bounds bytes in
		// the object store, and the ending is the account no transcript carries.
		res, releaseErr := tx.ExecContext(ctx, releaseStatement, c.packed, p.Handle, turn)
		if releaseErr != nil {
			return written, fmt.Errorf("executordomain: release %s's session of turn %s "+
				"at %s: %w", p.Handle, turn, c.position, releaseErr)
		}
		written += affected(res)
	}
	// THE TURN'S FIRST READ AND ITS WORK KEY STAND, decided here from the
	// row: a relaunch reads the turn's own row, not the head, and the work key
	// is the first non-empty one any record about the turn carried.
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
			work, p.Harness, p.HarnessVersion, p.Entry, p.SessionID, p.Object, p.ObjectHash, p.ObjectSize,
			p.ContentKey, p.ArchiveFormat, p.LaunchSeq, p.CaptureSeq, p.Home, p.Workspace,
			p.ContextTokens, flag(p.Interrupted), c.at(), readThrough, absorbed, c.at(), c.packed,
			p.Handle, p.Turn, c.packed)
	} else {
		res, err = tx.ExecContext(ctx, insertCaptureStatement,
			p.Handle, p.Turn, p.ConversationKey, work, p.Harness, p.HarnessVersion, p.Entry,
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
// read froze, which the row holds while it is live and has landed no capture.
// It repeats the decide's guards ([Writer.Begin]) in log order:
//   - the shape: a read, its turns as [absorbsOf] holds them, and a base among
//     them naming a parseable session ([Begin.baseShaped]), or none;
//   - no seat removal newer than the decide saw, and no tombstone of the turn;
//   - no row, or a live one of the same conversation and work with no read
//     yet: a turn's FIRST READ STANDS, so a retried or late begin writes nothing;
//   - a base held only where [baseStands], else written as none.
//
// Begins, ends and retention deletes share one subject, so the log's first
// decides: an end first leaves no base, a begin first holds it ([held]).
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
// of absorbs naming a session every reader can parse ([nameable]). [absorbsOf]
// passes only canonical turn ids other than the beginning turn's, so a base
// among them needs no test of either.
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
// THE END IS AUTHORITATIVE FOR THE WORK KEY: a keyless row takes the end's, so
// the next attempt at the work finds this one to dedupe; an end naming another
// key than the row's writes nothing, as the decide refuses it.
//
// AN END'S RULE IS ITS STAMP'S, never whether its ending carries went_on, so a
// record means one thing on every node. Where the row names an older capture
// than the turn's unkept newest, an end stamped [wentOnVersion] or above keeps
// that session and absorbs and dedupes; one below releases it, absorbing nothing.
func applyEnd(ctx context.Context, tx *sql.Tx, c applyContext, p End) (int, error) {
	// SERVED BY THE STAMP, as [wentOnVersion] is: below [servedVersion] the end
	// is applied as the build before applies it, naming nothing served.
	if c.record.V < servedVersion {
		p.Served = nil
	}
	if !present(p.Handle) || !present(p.Turn) || !p.Ending.Within() ||
		len(p.AskExcerpt) > AskExcerptMaxBytes || !servedValid(p.Served, p.ConversationKey) {
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
		// A TURN WITH NO ROW HAD NOTHING FOR A REMOVAL TO TOMBSTONE, so its
		// end is held to the removal it saw. A row needs no such check: the
		// removal deleted every row, so one now is the seat's next tenure's,
		// written by a record already held to the removal it saw.
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
	// BY THE END'S OWN STAMP ([applyEnd]). Below [wentOnVersion], the older
	// capture the row names is released below, judged on the row's place in log
	// order, so a capture of the unkept attempt that landed after all is kept.
	// At or above, the session stays: it holds the turn up to it, and the
	// ending says the turn went on past it ([executorkeep.Ending.WentOn]).
	stale := false
	if u := p.Ending.Unkept; found && row.hasObject && u != nil && c.record.V < wentOnVersion {
		stale = unkeptSupersedes(*u, row.launchSeq, row.captureSeq)
	}
	// ONLY A TURN WHOSE ROW KEEPS A SESSION ABSORBS AND DEDUPES: no other holds what
	// its read handed out, so those turns are told again rather than lost with it.
	keepsSession := found && row.hasObject && !stale &&
		!slices.ContainsFunc(p.Ending.SpentSessions, func(s executorkeep.SpentSession) bool {
			return s.Session == row.sessionID
		})
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
		// NOTHING WAS CAPTURED OR BEGUN: an ended row naming no object keeps
		// the ending alone for the conversation's next turn. It absorbs
		// nothing, having read nothing to stand in for.
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
	named, err := nameSummary(ctx, tx, c, p)
	if err != nil {
		return written, err
	}
	written += named
	retired, err := retireSpent(ctx, tx, c, p)
	if err != nil {
		return written, err
	}
	written += retired
	for _, key := range p.Served {
		res, err = tx.ExecContext(ctx, insertServedStatement, p.Handle, p.Turn, key, c.packed)
		if err != nil {
			return written, fmt.Errorf("executordomain: record that %s's turn %s served %q at %s: %w",
				p.Handle, p.Turn, key, c.position, err)
		}
		written += affected(res)
	}
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

// nameSummary names on the ending turn's row the newest summary its end names
// ([executorkeep.Ending.Summary]), where it names one every reader can parse.
// A SPENT, STALE OR BUDGET-RELEASED SESSION KEEPS IT: a summary is what a run
// that cannot continue the session starts from, and the seat's budget counts
// none, so only what deletes the row releases it ([releaseSummary]).
func nameSummary(ctx context.Context, tx *sql.Tx, c applyContext, p End) (int, error) {
	summary := p.Ending.Summary
	if summary == nil || !summary.Within() {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, insertSummaryStatement, p.Handle, p.Turn, summary.Object.Key.String(),
		string(summary.Object.Hash), summary.Object.Size, summary.ContentKey, summary.Harness, summary.Session,
		store.EncodeTime(summary.At), summary.Bytes, summary.Kept, flag(summary.Cut), c.packed)
	if err != nil {
		return 0, fmt.Errorf("executordomain: name %s's summary of turn %s at %s: %w",
			p.Handle, p.Turn, c.position, err)
	}
	return affected(res), nil
}

// releaseSummary takes one turn's summary off with its row.
func releaseSummary(ctx context.Context, tx *sql.Tx, c applyContext, handle, turn string) (int, error) {
	res, err := tx.ExecContext(ctx, releaseSummaryStatement, handle, turn)
	if err != nil {
		return 0, fmt.Errorf("executordomain: release %s's summary of turn %s at %s: %w",
			handle, turn, c.position, err)
	}
	return affected(res), nil
}

// retireSpent takes each session a launch of the turn spent off every ended row
// of its conversation, its own included, keeping how it was spent on each
// ([Session.RetiredLoss]). Held rows too: a hold keeps a session for a relaunch
// to continue, which would only meet the same end. A row the conversation reads
// as served is another conversation's too, so the conversation stops reading it
// instead, and the row keeps its session for the conversations that still read it.
func retireSpent(ctx context.Context, tx *sql.Tx, c applyContext, p End) (int, error) {
	if p.ConversationKey == "" {
		return 0, nil
	}
	written := 0
	for _, spent := range p.Ending.SpentSessions {
		session := spent.Session
		res, err := tx.ExecContext(ctx, retireStatement, p.Turn, string(spent.Loss), c.packed, p.Handle,
			p.ConversationKey, session)
		if err != nil {
			return written, fmt.Errorf("executordomain: retire %s's session %s at %s: %w",
				p.Handle, session, c.position, err)
		}
		written += affected(res)
		turns, listErr := servedNaming(ctx, tx, p.Handle, p.ConversationKey, session)
		if listErr != nil {
			return written, listErr
		}
		for _, turn := range turns {
			if res, err = tx.ExecContext(ctx, dropServedStatement, p.Handle, turn, p.ConversationKey); err != nil {
				return written, fmt.Errorf("executordomain: drop %s's served turn %s at %s: %w",
					p.Handle, turn, c.position, err)
			}
			written += affected(res)
		}
	}
	return written, nil
}

// servedNaming is the turns conversationKey reads as served whose rows name
// session, read whole and closed before any is dropped.
func servedNaming(ctx context.Context, tx *sql.Tx, handle, conversationKey, session string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, servedSessionStatement, handle, conversationKey, session)
	if err != nil {
		return nil, fmt.Errorf("executordomain: read the turns %s's conversation reads as served: %w",
			handle, err)
	}
	var out []string
	for rows.Next() {
		var turn string
		if scanErr := rows.Scan(&turn); scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		out = append(out, turn)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// absorb deletes the conversation's ended rows the ending turn stands in for,
// and reports whether an older attempt at its work delivered the work's reply:
//   - exactly the turns its first read handed out ([Capture.Absorbs]) that
//     ended at or before that read; one the read left out (past [MaxEndings],
//     or ended later) stays for the next read to show, never gone untold;
//   - every older attempt at the same work, shown or not, its `delivered`
//     merged even where held. AN EMPTY WORK KEY NEVER DEDUPES (ADR-0017), or
//     every keyless turn would erase its keyless siblings.
//
// Neither takes a row a live turn holds ([held]) or another conversation reads
// as served ([served]); a later end shown it does. A deleted row's tombstone is
// its own `end`'s. A served turn is absorbed off this conversation alone.
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
		// NO HOLD IS ASKED: dropping what the conversation reads releases no
		// session, which stays on the turn's own row for whoever holds it.
		res, execErr := tx.ExecContext(ctx, absorbServedStatement, p.Handle, shown, p.ConversationKey,
			row.readThrough)
		if execErr != nil {
			return written, false, fmt.Errorf("executordomain: absorb %s's served turn %s at %s: %w",
				p.Handle, shown, c.position, execErr)
		}
		written += affected(res)
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
// turn of the seat other than except holds that turn as its base or another
// conversation reads it as served.
func dropUnheld(ctx context.Context, tx *sql.Tx, c applyContext, statement, handle, turn,
	except string, args ...any) (int, error) {

	holds, err := held(ctx, tx, handle, turn, except)
	if err != nil || holds {
		return 0, err
	}
	if holds, err = served(ctx, tx, handle, turn, ""); err != nil || holds {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, statement, args...)
	if err != nil {
		return 0, fmt.Errorf("executordomain: delete %s's turn %s at %s: %w",
			handle, turn, c.position, err)
	}
	deleted := affected(res)
	if deleted == 0 {
		return 0, nil
	}
	n, err := releaseSummary(ctx, tx, c, handle, turn)
	return deleted + n, err
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

// removeTurn deletes one turn's row, its tombstone and its summary.
//
// THE TOMBSTONE GOES TOO, unlike any other delete's: it refuses a record
// decided before the row went, and past the retention none can land. The term
// is at least a day past the row's last instant, beyond objstore.RecordWithin
// for any capture, and the duty never nominates a live run's turn. A run's
// late-retried `end` writes an ending-only row the retention takes in its
// turn; a kept tombstone would only be one more row to take a term later.
func removeTurn(ctx context.Context, tx *sql.Tx, c applyContext, handle, turn string) (int, error) {
	written := 0
	for _, statement := range []string{deleteSessionStatement, deleteTombStatement, turnServedStatement,
		releaseSummaryStatement} {
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
// A TURN WITH A LIVE ROW IS TOMBSTONED, so a later capture is refused as any
// ended turn's is. A turn with no row yet (a first capture or a captureless
// end, decided before the removal and landing after) has nothing to tombstone
// and is refused by the removal's position, which every capture and end
// carries as its decide saw it ([Capture.SeenRemoval]).
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
		{seatServedStatement, []any{handle}},
		{seatSummariesStatement, []any{handle}},
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
