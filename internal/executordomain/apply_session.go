package executordomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// THE SESSION CASES: a capture, an end and a retention delete on a
// conversation's subject, and a seat's removal on the seat's.
//
// Each reads the rows it decides from IN THIS TRANSACTION, by the same helpers
// the writer's decide reads them through ([readSession], [readTomb],
// [seatPlan]), so the decide and the apply hold one rule rather than two
// spellings of it: the decide refuses what it can see is wrong, and the apply
// repeats every deterministic guard for what changed between the two.

// Tombstone reasons: why a turn takes no further record.
const (
	// tombEnded is a turn its own `end` closed.
	tombEnded = "ended"

	// tombSeatRemoved is a live turn whose seat the company removed.
	tombSeatRemoved = "seat_removed"
)

// sessionRow is what a decide and an apply read of one turn's row.
type sessionRow struct {
	conversationKey string
	workKey         string
	launchSeq       int64
	captureSeq      int64
	readThrough     int64
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
	err := tx.QueryRowContext(ctx, `
		SELECT conversation_key, work_key, launch_seq, capture_seq, read_through,
		       end_seq, object, object_size, last_at
		FROM executor_sessions WHERE agent_handle = ? AND turn_id = ?`,
		handle, turn).Scan(&row.conversationKey, &row.workKey, &row.launchSeq,
		&row.captureSeq, &row.readThrough, &endSeq, &object, &row.objectSize, &row.lastAt)
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
	err := tx.QueryRowContext(ctx, `
		SELECT conversation_key, at FROM executor_session_tombs
		WHERE agent_handle = ? AND turn_id = ?`, handle, turn).Scan(&tomb.conversationKey, &tomb.at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return tombRow{}, false, nil
	case err != nil:
		return tombRow{}, false, fmt.Errorf("executordomain: read %s's tombstone of "+
			"turn %s: %w", handle, turn, err)
	}
	return tomb, true, nil
}

// newer reports whether a capture's ordinals are past the row's: a later
// launch, or a later capture of the same launch.
func newer(launch, capture, rowLaunch, rowCapture int64) bool {
	return launch > rowLaunch || (launch == rowLaunch && capture > rowCapture)
}

// seatPlan is which ended turns' sessions a capture of size bytes on turn must
// release for the seat to hold budget, and whether it fits at all.
//
// THE OLDEST ENDINGS GO FIRST, by the position each ended at, and never a live
// turn's session or this turn's own: a live turn's is the one its next launch
// continues, and this turn's is being replaced. The release is a total order of
// the rows (the end's position, then the turn id), so every node releases the
// same sessions. Nothing is chosen when even every ended session released would
// not make room: the seat's live turns already hold the budget, and the capture
// is refused whole rather than releasing what does not help.
func seatPlan(ctx context.Context, tx *sql.Tx, handle, turn string, size, budget int64) (
	release []string, fits bool, err error) {

	var held int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(object_size), 0) FROM executor_sessions
		WHERE agent_handle = ? AND object IS NOT NULL AND turn_id <> ?`,
		handle, turn).Scan(&held); err != nil {
		return nil, false, fmt.Errorf("executordomain: read %s's session bytes: %w", handle, err)
	}
	need := held + size
	if need <= budget {
		return nil, true, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT turn_id, object_size FROM executor_sessions
		WHERE agent_handle = ? AND end_seq IS NOT NULL AND object IS NOT NULL
		  AND turn_id <> ?
		ORDER BY end_seq, turn_id`, handle, turn)
	if err != nil {
		return nil, false, fmt.Errorf("executordomain: read %s's ended sessions: %w", handle, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			ended string
			bytes int64
		)
		if err := rows.Scan(&ended, &bytes); err != nil {
			return nil, false, err
		}
		release = append(release, ended)
		if need -= bytes; need <= budget {
			return release, true, rows.Err()
		}
	}
	return nil, false, rows.Err()
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
func applyCapture(ctx context.Context, tx *sql.Tx, c applyContext, p Capture) (int, error) {
	if !present(p.Handle) || !present(p.Turn) || !present(p.Object) || p.ObjectSize <= 0 ||
		p.SeatBudget <= 0 {
		return 0, nil
	}
	if _, tombed, err := readTomb(ctx, tx, p.Handle, p.Turn); err != nil || tombed {
		return 0, err
	}
	row, found, err := readSession(ctx, tx, p.Handle, p.Turn)
	if err != nil {
		return 0, err
	}
	if found && (row.ended || row.conversationKey != p.ConversationKey ||
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
		res, err := tx.ExecContext(ctx, `
			UPDATE executor_sessions SET object = NULL, object_hash = '',
				object_size = 0, content_key = '', version = ?
			WHERE agent_handle = ? AND turn_id = ?`, c.packed, p.Handle, turn)
		if err != nil {
			return written, fmt.Errorf("executordomain: release %s's session of turn %s "+
				"at %s: %w", p.Handle, turn, c.position, err)
		}
		written += affected(res)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO executor_sessions (
			agent_handle, turn_id, conversation_key, work_key, cli, cli_version,
			session_id, object, object_hash, object_size, content_key, archive_format,
			launch_seq, capture_seq, home, workspace, captured_at, read_through,
			last_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, turn_id) DO UPDATE SET
			cli = excluded.cli, cli_version = excluded.cli_version,
			session_id = excluded.session_id, object = excluded.object,
			object_hash = excluded.object_hash, object_size = excluded.object_size,
			content_key = excluded.content_key, archive_format = excluded.archive_format,
			launch_seq = excluded.launch_seq, capture_seq = excluded.capture_seq,
			home = excluded.home, workspace = excluded.workspace,
			captured_at = excluded.captured_at,
			read_through = CASE WHEN executor_sessions.read_through = 0
				THEN excluded.read_through ELSE executor_sessions.read_through END,
			last_at = excluded.last_at, version = excluded.version
		WHERE executor_sessions.version < excluded.version`,
		p.Handle, p.Turn, p.ConversationKey, p.WorkKey, p.CLI, p.CLIVersion,
		p.SessionID, p.Object, p.ObjectHash, p.ObjectSize, p.ContentKey, p.ArchiveFormat,
		p.LaunchSeq, p.CaptureSeq, p.Home, p.Workspace, c.at(), p.ReadThrough,
		c.at(), c.packed)
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
// what the turn's first launch was shown and dedupes on the work key.
func applyEnd(ctx context.Context, tx *sql.Tx, c applyContext, p End) (int, error) {
	if !present(p.Handle) || !present(p.Turn) || !p.Ending.within() ||
		len(p.AskExcerpt) > AskExcerptMaxBytes {
		return 0, nil
	}
	if _, tombed, err := readTomb(ctx, tx, p.Handle, p.Turn); err != nil || tombed {
		return 0, err
	}
	row, found, err := readSession(ctx, tx, p.Handle, p.Turn)
	if err != nil {
		return 0, err
	}
	if found && (row.ended || row.conversationKey != p.ConversationKey) {
		return 0, nil
	}
	var body endBody
	if err := json.Unmarshal(c.record.Mutation, &body); err != nil {
		return 0, fmt.Errorf("executordomain: the end at %s: %w", c.position, err)
	}
	delivered := p.Ending.Delivered
	written := 0
	if found && p.ConversationKey != "" {
		absorbed, merged, err := absorb(ctx, tx, c, p, row.readThrough)
		if err != nil {
			return 0, err
		}
		written += absorbed
		delivered = delivered || merged
	}
	var res sql.Result
	if found {
		res, err = tx.ExecContext(ctx, `
			UPDATE executor_sessions SET ended_at = ?, end_seq = ?, outcome = ?,
				delivered = ?, ending = ?, ask_excerpt = ?, last_at = ?, version = ?
			WHERE agent_handle = ? AND turn_id = ? AND end_seq IS NULL`,
			c.at(), c.packed, string(p.Ending.Outcome), flag(delivered), string(body.Ending),
			p.AskExcerpt, c.at(), c.packed, p.Handle, p.Turn)
	} else {
		// NOTHING WAS CAPTURED: an ended row naming no object, which holds
		// the ending alone, so the conversation's next turn is still told
		// how this one ended. It absorbs nothing: it read nothing it could
		// stand in for.
		res, err = tx.ExecContext(ctx, `
			INSERT INTO executor_sessions (agent_handle, turn_id, conversation_key,
				work_key, ended_at, end_seq, outcome, delivered, ending, ask_excerpt,
				last_at, version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (agent_handle, turn_id) DO NOTHING`,
			p.Handle, p.Turn, p.ConversationKey, p.WorkKey, c.at(), c.packed,
			string(p.Ending.Outcome), flag(delivered), string(body.Ending), p.AskExcerpt,
			c.at(), c.packed)
	}
	if err != nil {
		return written, fmt.Errorf("executordomain: end %s's turn %s at %s: %w",
			p.Handle, p.Turn, c.position, err)
	}
	written += affected(res)
	n, err := tomb(ctx, tx, c, p.Handle, p.Turn, p.ConversationKey, tombEnded)
	return written + n, err
}

// absorb deletes the conversation's ended rows the ending turn stands in for:
// every one that ended at or below the position its first launch read the
// conversation's head at (that launch continued the newest of them or was
// shown them), and every older attempt at the same unit of work, whose
// `delivered` it reports so the surviving row says the work's reply went out.
//
// AN EMPTY WORK KEY NEVER DEDUPES: it is the honest default of a turn with no
// unit of work to collapse on (ADR-0017), and matching on it would let every
// keyless turn erase its keyless siblings. Each deleted row's tombstone was
// written by its own `end`, so nothing recreates it.
func absorb(ctx context.Context, tx *sql.Tx, c applyContext, p End, readThrough int64) (
	int, bool, error) {

	written, delivered := 0, false
	if p.WorkKey != "" {
		var n int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM executor_sessions
			WHERE agent_handle = ? AND conversation_key = ? AND work_key = ?
			  AND end_seq IS NOT NULL AND turn_id <> ? AND delivered = 1`,
			p.Handle, p.ConversationKey, p.WorkKey, p.Turn).Scan(&n); err != nil {
			return 0, false, fmt.Errorf("executordomain: read the attempts at %s's work "+
				"%s: %w", p.Handle, p.WorkKey, err)
		}
		delivered = n > 0
		res, err := tx.ExecContext(ctx, `
			DELETE FROM executor_sessions
			WHERE agent_handle = ? AND conversation_key = ? AND work_key = ?
			  AND end_seq IS NOT NULL AND turn_id <> ?`,
			p.Handle, p.ConversationKey, p.WorkKey, p.Turn)
		if err != nil {
			return 0, false, fmt.Errorf("executordomain: dedupe %s's work %s at %s: %w",
				p.Handle, p.WorkKey, c.position, err)
		}
		written += affected(res)
	}
	if readThrough > 0 {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM executor_sessions
			WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
			  AND end_seq <= ? AND turn_id <> ?`,
			p.Handle, p.ConversationKey, readThrough, p.Turn)
		if err != nil {
			return written, false, fmt.Errorf("executordomain: absorb %s's conversation "+
				"at %s: %w", p.Handle, c.position, err)
		}
		written += affected(res)
	}
	return written, delivered, nil
}

// tomb writes a turn's tombstone, keeping the first one a turn was given.
func tomb(ctx context.Context, tx *sql.Tx, c applyContext, handle, turn,
	conversationKey, reason string) (int, error) {

	res, err := tx.ExecContext(ctx, `
		INSERT INTO executor_session_tombs
			(agent_handle, turn_id, conversation_key, at_seq, at, reason)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, turn_id) DO NOTHING`,
		handle, turn, conversationKey, c.packed, c.at(), reason)
	if err != nil {
		return 0, fmt.Errorf("executordomain: tombstone %s's turn %s at %s: %w",
			handle, turn, c.position, err)
	}
	return affected(res), nil
}

// applyDelete removes one turn's row and tombstone past the retention, decided
// again here against the row's own last instant: a turn captured since the
// duty nominated it stays. A turn whose row an absorption took already is
// removed by its tombstone's instant.
func applyDelete(ctx context.Context, tx *sql.Tx, c applyContext, p Delete) (int, error) {
	if !present(p.Handle) || !present(p.Turn) {
		return 0, nil
	}
	cutoff := store.EncodeTime(p.Cutoff)
	row, found, err := readSession(ctx, tx, p.Handle, p.Turn)
	if err != nil {
		return 0, err
	}
	if found {
		if row.conversationKey != p.ConversationKey || row.lastAt > cutoff {
			return 0, nil
		}
	} else {
		marker, tombed, err := readTomb(ctx, tx, p.Handle, p.Turn)
		if err != nil || !tombed || marker.conversationKey != p.ConversationKey ||
			marker.at > cutoff {
			return 0, err
		}
	}
	return removeTurn(ctx, tx, c, p.Handle, p.Turn)
}

// removeTurn deletes one turn's row and its tombstone.
//
// THE TOMBSTONE GOES WITH THE ROW because by now nothing can arrive for the
// turn: the retention is at least a day, so its last capture is past the
// objstore.RecordWithin bound any capture's object has to be named inside, and
// the duty never nominates a turn whose run is still live.
func removeTurn(ctx context.Context, tx *sql.Tx, c applyContext, handle, turn string) (int, error) {
	written := 0
	for _, table := range []string{"executor_sessions", "executor_session_tombs"} {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM `+table+` WHERE agent_handle = ? AND turn_id = ?`, handle, turn)
		if err != nil {
			return written, fmt.Errorf("executordomain: delete %s's turn %s from %s at %s: %w",
				handle, turn, table, c.position, err)
		}
		written += affected(res)
	}
	return written, nil
}

// applySeat removes every row a removed seat holds, tombstoning the turns
// that had not ended so a capture decided before the removal and landing after
// it recreates nothing. An ended turn's tombstone is its own `end`'s.
func applySeat(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	p, ok := payload.(SeatDelete)
	if !ok {
		return 0, fmt.Errorf("executordomain: the %s record at %s carries a %T",
			c.record.Op, c.position, payload)
	}
	if !present(p.Handle) || c.record.Subject != SeatSubject(p.Handle) {
		return 0, nil
	}
	type live struct{ turn, conversationKey string }
	var open []live
	rows, err := tx.QueryContext(ctx, `
		SELECT turn_id, conversation_key FROM executor_sessions
		WHERE agent_handle = ? AND end_seq IS NULL ORDER BY turn_id`, p.Handle)
	if err != nil {
		return 0, fmt.Errorf("executordomain: read %s's live turns at %s: %w",
			p.Handle, c.position, err)
	}
	for rows.Next() {
		var l live
		if err := rows.Scan(&l.turn, &l.conversationKey); err != nil {
			_ = rows.Close()
			return 0, err
		}
		open = append(open, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	written := 0
	for _, l := range open {
		n, err := tomb(ctx, tx, c, p.Handle, l.turn, l.conversationKey, tombSeatRemoved)
		if err != nil {
			return written, err
		}
		written += n
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM executor_sessions WHERE agent_handle = ?`,
		p.Handle)
	if err != nil {
		return written, fmt.Errorf("executordomain: remove seat %s at %s: %w",
			p.Handle, c.position, err)
	}
	return written + affected(res), nil
}
