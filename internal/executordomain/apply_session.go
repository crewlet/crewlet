package executordomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/store"
)

// THE SESSION CASES: a capture, an end and a retention delete on a
// conversation's subject, and a seat's removal and the retention of that
// removal on the seat's.
//
// Each reads the rows it decides from IN THIS TRANSACTION, by the same helpers
// the writer's decide reads them through ([readSession], [readTomb],
// [readRemoval], [seatPlan], [expiredIn]), so the decide and the apply hold one
// rule rather than two spellings of it: the decide refuses what it can see is
// wrong, and the apply repeats every deterministic guard for what changed
// between the two.
//
// EVERY STATEMENT IS NAMED, here and in the reader, so the plan gate plans the
// statements this package runs rather than copies of them
// (TestTheExecutorStatementsArePlanned).

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
		SELECT conversation_key, work_key, launch_seq, capture_seq, read_through,
		       absorbs, end_seq, object, object_size, last_at
		FROM executor_sessions WHERE agent_handle = ? AND turn_id = ?`

	// readTombStatement is one turn's tombstone, by its key.
	readTombStatement = `
		SELECT conversation_key, at FROM executor_session_tombs
		WHERE agent_handle = ? AND turn_id = ?`

	// readRemovalStatement is where a seat's rows were last removed.
	readRemovalStatement = `
		SELECT at_seq FROM executor_seat_removals WHERE agent_handle = ?`

	// seatHeldStatement is the object bytes a seat's other turns hold.
	seatHeldStatement = `
		SELECT COALESCE(SUM(object_size), 0) FROM executor_sessions
		WHERE agent_handle = ? AND object IS NOT NULL AND turn_id <> ?`

	// seatReleasableStatement is a seat's ended sessions in the order the
	// budget releases them: the oldest end first. AN END'S POSITION IS
	// UNIQUE among the rows that hold one, so it alone is a total order, and
	// the seat's index on it serves the order without a sort.
	seatReleasableStatement = `
		SELECT turn_id, object_size FROM executor_sessions
		WHERE agent_handle = ? AND end_seq IS NOT NULL AND object IS NOT NULL
		  AND turn_id <> ?
		ORDER BY end_seq`
)

// The statements an apply writes the rows with.
const (
	// releaseStatement takes an ended turn's object off its row, keeping
	// the row's ending.
	releaseStatement = `
		UPDATE executor_sessions SET object = NULL, object_hash = '',
			object_size = 0, content_key = '', version = ?
		WHERE agent_handle = ? AND turn_id = ?`

	// insertCaptureStatement is a turn's first capture: the row begins
	// here, at first_seq.
	insertCaptureStatement = `
		INSERT INTO executor_sessions (
			agent_handle, turn_id, conversation_key, work_key, cli, cli_version,
			session_id, object, object_hash, object_size, content_key, archive_format,
			launch_seq, capture_seq, home, workspace, captured_at, read_through,
			absorbs, last_at, first_seq, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, turn_id) DO NOTHING`

	// updateCaptureStatement is a later capture of a live turn: every
	// value is the one [applyCapture] decided, the turn's first read and
	// its work key included, so the statement itself decides nothing.
	updateCaptureStatement = `
		UPDATE executor_sessions SET work_key = ?, cli = ?, cli_version = ?,
			session_id = ?, object = ?, object_hash = ?, object_size = ?,
			content_key = ?, archive_format = ?, launch_seq = ?, capture_seq = ?,
			home = ?, workspace = ?, captured_at = ?, read_through = ?, absorbs = ?,
			last_at = ?, version = ?
		WHERE agent_handle = ? AND turn_id = ? AND end_seq IS NULL AND version < ?`

	// endRowStatement stamps a captured turn's end on its row.
	endRowStatement = `
		UPDATE executor_sessions SET work_key = ?, ended_at = ?, end_seq = ?,
			outcome = ?, delivered = ?, ending = ?, ask_excerpt = ?, last_at = ?,
			version = ?
		WHERE agent_handle = ? AND turn_id = ? AND end_seq IS NULL`

	// insertEndingStatement is the ended row of a turn that captured
	// nothing: its ending alone, naming no object.
	insertEndingStatement = `
		INSERT INTO executor_sessions (agent_handle, turn_id, conversation_key,
			work_key, ended_at, end_seq, outcome, delivered, ending, ask_excerpt,
			last_at, first_seq, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, turn_id) DO NOTHING`

	// deliveredAttemptsStatement counts the older ended attempts at one
	// unit of work in a conversation whose reply went out.
	deliveredAttemptsStatement = `
		SELECT COUNT(*) FROM executor_sessions
		WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
		  AND work_key = ? AND turn_id <> ? AND delivered = 1`

	// dedupeStatement deletes those older attempts.
	dedupeStatement = `
		DELETE FROM executor_sessions
		WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
		  AND work_key = ? AND turn_id <> ?`

	// absorbStatement deletes one ended turn the ending turn's first read
	// handed out, by its key, and only where it ended at or before that read
	// (a read can show no turn that ended after it).
	absorbStatement = `
		DELETE FROM executor_sessions
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
		DELETE FROM executor_sessions WHERE agent_handle = ? AND turn_id = ?`
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
	launchSeq       int64
	captureSeq      int64
	readThrough     int64
	absorbs         string
	ended           bool
	hasObject       bool
	objectSize      int64
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
		&row.conversationKey, &row.workKey, &row.launchSeq, &row.captureSeq,
		&row.readThrough, &row.absorbs, &endSeq, &object, &row.objectSize, &row.lastAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return sessionRow{}, false, nil
	case err != nil:
		return sessionRow{}, false, fmt.Errorf("executordomain: read %s's turn %s: %w",
			handle, turn, err)
	}
	row.ended, row.hasObject = endSeq.Valid, object.Valid
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
// continues, and this turn's is being replaced. The release is a total order of
// the rows (an end's position is unique among them), so every node releases the
// same sessions. Nothing is chosen when even every ended session released would
// not make room: the seat's live turns already hold the budget, and the capture
// is refused whole rather than releasing what does not help.
func seatPlan(ctx context.Context, tx *sql.Tx, handle, turn string, size, budget int64) (
	[]string, bool, error) {

	var held int64
	if err := tx.QueryRowContext(ctx, seatHeldStatement, handle, turn).Scan(&held); err != nil {
		return nil, false, fmt.Errorf("executordomain: read %s's session bytes: %w", handle, err)
	}
	need := held + size
	if need <= budget {
		return nil, true, nil
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
		release = append(release, ended)
		if need -= bytes; need <= budget {
			return release, true, rows.Err()
		}
	}
	return nil, false, rows.Err()
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
func expiredIn(ctx context.Context, tx *sql.Tx, handle, conversationKey, turn string,
	cutoff int64) (bool, error) {

	row, found, err := readSession(ctx, tx, handle, turn)
	if err != nil {
		return false, err
	}
	if found {
		return row.conversationKey == conversationKey && row.lastAt <= cutoff, nil
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
		!nameable(p.Object, p.ObjectHash) || !wellFormed {
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
			work, p.CLI, p.CLIVersion, p.SessionID, p.Object, p.ObjectHash, p.ObjectSize,
			p.ContentKey, p.ArchiveFormat, p.LaunchSeq, p.CaptureSeq, p.Home, p.Workspace,
			c.at(), readThrough, absorbed, c.at(), c.packed,
			p.Handle, p.Turn, c.packed)
	} else {
		res, err = tx.ExecContext(ctx, insertCaptureStatement,
			p.Handle, p.Turn, p.ConversationKey, work, p.CLI, p.CLIVersion,
			p.SessionID, p.Object, p.ObjectHash, p.ObjectSize, p.ContentKey, p.ArchiveFormat,
			p.LaunchSeq, p.CaptureSeq, p.Home, p.Workspace, c.at(), readThrough,
			absorbed, c.at(), c.packed, c.packed)
	}
	if err != nil {
		return written, fmt.Errorf("executordomain: record %s's capture of turn %s at %s: %w",
			p.Handle, p.Turn, c.position, err)
	}
	return written + affected(res), nil
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
func applyEnd(ctx context.Context, tx *sql.Tx, c applyContext, p End) (int, error) {
	if !present(p.Handle) || !present(p.Turn) || !p.Ending.within() ||
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
		// so a row now is one a capture of the seat's next tenure wrote.
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
	if found && p.ConversationKey != "" {
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
		// NOTHING WAS CAPTURED: an ended row naming no object, which holds
		// the ending alone, so the conversation's next turn is still told
		// how this one ended. It absorbs nothing: it read nothing it could
		// stand in for.
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
	n, err := tomb(ctx, tx, c, p.Handle, p.Turn, p.ConversationKey, tombEnded)
	return written + n, err
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
// Each deleted row's tombstone was written by its own `end`, so nothing
// recreates it.
func absorb(ctx context.Context, tx *sql.Tx, c applyContext, p End, work string,
	row sessionRow) (int, bool, error) {

	written, delivered := 0, false
	if work != "" {
		var n int
		if countErr := tx.QueryRowContext(ctx, deliveredAttemptsStatement,
			p.Handle, p.ConversationKey, work, p.Turn).Scan(&n); countErr != nil {
			return 0, false, fmt.Errorf("executordomain: read the attempts at %s's work "+
				"%s: %w", p.Handle, work, countErr)
		}
		delivered = n > 0
		res, dedupeErr := tx.ExecContext(ctx, dedupeStatement,
			p.Handle, p.ConversationKey, work, p.Turn)
		if dedupeErr != nil {
			return 0, false, fmt.Errorf("executordomain: dedupe %s's work %s at %s: %w",
				p.Handle, work, c.position, dedupeErr)
		}
		written += affected(res)
	}
	if row.readThrough <= 0 {
		return written, delivered, nil
	}
	for _, shown := range splitAbsorbs(row.absorbs) {
		res, absorbErr := tx.ExecContext(ctx, absorbStatement,
			p.Handle, shown, p.ConversationKey, row.readThrough)
		if absorbErr != nil {
			return written, false, fmt.Errorf("executordomain: absorb %s's turn %s at %s: %w",
				p.Handle, shown, c.position, absorbErr)
		}
		written += affected(res)
	}
	return written, delivered, nil
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
