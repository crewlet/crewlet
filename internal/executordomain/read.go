package executordomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reader answers questions about the seats' executor sessions from this node's
// own applied rows.
//
// # Through the read authority, at the level the caller asks
//
// Every read goes through [statelog.Reader], so the refusal ladder holds (an
// evicted node, one below the trim floor and one whose applier stopped serve
// nothing) and a record this node could not decode is reported rather than
// answered around. The reads a launch takes name their level: a conversation's
// head is read at `linearizable`, because the previous turn's `end` may have
// been published by another node and a copy that has not applied it serves an
// older head — a launch then continues a session missing the last turn and is
// told an older turn's ending. A read the ladder refuses is the caller's to
// fail open on: a launch starts fresh, saying it could not read its history.
type Reader struct {
	db  store.ReplicatedReader
	log *statelog.Reader
}

// ReaderOptions configure a reader.
type ReaderOptions struct {
	DB store.ReplicatedReader

	// Log is this domain's read authority. REQUIRED: without it every read
	// level is a label rather than a guarantee.
	Log *statelog.Reader
}

// NewReader builds the executor read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	switch {
	case opts.DB.IsZero():
		return nil, errors.New("executordomain: a reader needs the replicated estate")
	case opts.Log == nil:
		return nil, errors.New("executordomain: a reader needs its domain's read " +
			"authority: without it every read level is a label rather than a guarantee")
	}
	return &Reader{db: opts.DB, log: opts.Log}, nil
}

// stale is the level the engine's own duties read at: a nomination that is
// decided again in the write's own snapshot, and a census that is a count.
var stale = statelog.Freshness{Level: statelog.ReadStale}

// Session is one turn's row.
type Session struct {
	Handle          string
	Turn            string
	ConversationKey string
	WorkKey         string

	Harness        string
	HarnessVersion string
	SessionID      string

	// Object is the sealed session, its Key zero on a row that names none:
	// an ending-only row, or one whose session the seat's budget released.
	Object        objstore.Object
	ContentKey    string
	ArchiveFormat int

	LaunchSeq  int64
	CaptureSeq int64

	Home      string
	Workspace string

	// CapturedAt is the broker's instant for the newest capture, zero on a
	// row nothing was captured into.
	CapturedAt time.Time

	// ReadThrough is where the turn's first launch read its conversation's
	// head, zero where it read none, and Absorbs the turns that read handed
	// out in full, which the turn's end absorbs.
	ReadThrough statelog.Position
	Absorbs     []string

	// EndedAt and EndedAtPosition are the end's broker instant and its
	// position on the log, both zero while the turn is live; Ending is how
	// it ended, nil until then.
	EndedAt         time.Time
	EndedAtPosition statelog.Position
	Ending          *Ending

	// AskExcerpt is what the turn was asked: somebody else's words.
	AskExcerpt string `untrusted:"fence"`
}

// Ended reports a turn whose end applied.
func (s Session) Ended() bool { return s.Ending != nil }

// HasObject reports a row that names a session's object.
func (s Session) HasObject() bool { return !s.Object.Key.IsZero() }

// rowScanner is one row, from a query or a query row.
type rowScanner interface{ Scan(dest ...any) error }

// sessionColumns is every column a [Session] is read from, in [scanSession]'s
// order.
const sessionColumns = `agent_handle, turn_id, conversation_key, work_key, harness,
	harness_version, session_id, object, object_hash, object_size, content_key,
	archive_format, launch_seq, capture_seq, home, workspace, captured_at,
	read_through, absorbs, ended_at, end_seq, outcome, delivered, ending, ask_excerpt`

// scanSession reads one row.
func scanSession(row rowScanner) (Session, error) {
	var (
		s                     Session
		object                sql.NullString
		hash                  string
		size                  int64
		captured, readThrough int64
		endedAt, endSeq       sql.NullInt64
		outcome, ending       string
		absorbs               string
		delivered             int
	)
	if err := row.Scan(&s.Handle, &s.Turn, &s.ConversationKey, &s.WorkKey, &s.Harness,
		&s.HarnessVersion, &s.SessionID, &object, &hash, &size, &s.ContentKey,
		&s.ArchiveFormat, &s.LaunchSeq, &s.CaptureSeq, &s.Home, &s.Workspace, &captured,
		&readThrough, &absorbs, &endedAt, &endSeq, &outcome, &delivered, &ending,
		&s.AskExcerpt); err != nil {
		return Session{}, err
	}
	s.Absorbs = splitAbsorbs(absorbs)
	stream := Domain{}.Stream().Name
	if object.Valid {
		key, err := objstore.ParseKey(object.String)
		if err != nil {
			return Session{}, fmt.Errorf("executordomain: %s's turn %s names %w",
				s.Handle, s.Turn, err)
		}
		digest, err := objstore.ParseHash(hash)
		if err != nil {
			return Session{}, fmt.Errorf("executordomain: %s's turn %s: %w",
				s.Handle, s.Turn, err)
		}
		s.Object = objstore.Object{Key: key, Hash: digest, Size: size}
	}
	if captured > 0 {
		s.CapturedAt = store.DecodeTime(captured)
	}
	if readThrough > 0 {
		s.ReadThrough = statelog.Unpack(stream, readThrough)
	}
	if endSeq.Valid {
		s.EndedAt = store.DecodeTime(endedAt.Int64)
		s.EndedAtPosition = statelog.Unpack(stream, endSeq.Int64)
		var e Ending
		if err := json.Unmarshal([]byte(ending), &e); err != nil {
			return Session{}, fmt.Errorf("executordomain: %s's turn %s's ending: %w",
				s.Handle, s.Turn, err)
		}
		// THE COLUMNS ARE THE ACCOUNT: the outcome as the row was ended
		// with, and whether a reply went out, which a dedupe merges into
		// the row rather than into the ending's bytes.
		e.Outcome, e.Delivered = Outcome(outcome), delivered == 1
		s.Ending = &e
	}
	return s, nil
}

// Head is what a conversation's next turn launches from.
//
// # What is left of a conversation is what no ended turn was shown
//
// A turn's end absorbs exactly the turns its first launch's read handed out
// ([Head.Absorbs], carried on its captures), so every ended row a conversation
// still holds is one no ended turn was shown: a turn that ended beside the
// head, after it, or before it but past the bound of the read that showed the
// head's turn the rest. So the next turn is shown ALL of them — never only the
// ones newer than the head, which would leave a turn that ended beside a turn
// parked on a person (and so older than the head when the parked turn ended)
// shown to nobody, and then absorbed by the next end as though it had been.
type Head struct {
	// Session is the conversation's HEAD: its newest ended turn that left a
	// session, which the next turn continues. Nil where none did, and the
	// launch starts fresh.
	Session *Session

	// Endings are the conversation's other ended turns, session or not: how
	// they ended is what the next turn is told beside the session it
	// continues. At most [MaxEndings], the newest of them, in the order they
	// ended; EndingsOmitted counts the older ones left out, which stay for
	// the turn after this one to be shown.
	Endings        []Session
	EndingsOmitted int

	// Siblings are the Endings that still name a session — turns that ran
	// beside the head and that no later turn continued — which a launch
	// names and may restore. At most [MaxSiblings], the newest of them, in
	// the order they ended; SiblingsOmitted counts every other ended turn of
	// the conversation that names a session, shown as an ending or not.
	Siblings        []Session
	SiblingsOmitted int

	// Position is the checkpoint the answer was read at: what the turn's
	// captures carry as their ReadThrough, beside [Head.Absorbs].
	Position statelog.Position
}

// Absorbs is the turns this answer handed out in full, which the launching
// turn's captures carry ([NewCapture.Absorbs]) and its end absorbs: the head it
// continues, every ending that names no session (told), and every sibling
// (told, and offered). An ending that still names a session but did not make
// [MaxSiblings] was told and never offered, so it is left out: absorbed, its
// session would be gone without any launch having been offered it.
func (h Head) Absorbs() []string {
	var out []string
	if h.Session != nil {
		out = append(out, h.Session.Turn)
	}
	offered := make(map[string]bool, len(h.Siblings))
	for _, s := range h.Siblings {
		offered[s.Turn] = true
	}
	for _, e := range h.Endings {
		if !e.HasObject() || offered[e.Turn] {
			out = append(out, e.Turn)
		}
	}
	return out
}

// The statements a launch's reads run.
const (
	// headStatement is a conversation's newest ended turn that names a
	// session.
	headStatement = `SELECT ` + sessionColumns + `
		FROM executor_sessions
		WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
		  AND object IS NOT NULL
		ORDER BY end_seq DESC LIMIT 1`

	// endingsStatement is a conversation's other ended turns, the newest
	// first, and endingsCountStatement how many there are. AN END'S
	// POSITION IS UNIQUE among the rows that hold one, so it alone orders
	// them, and the conversation's index serves the order.
	endingsStatement = `SELECT ` + sessionColumns + `
		FROM executor_sessions
		WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
		  AND turn_id <> ?
		ORDER BY end_seq DESC LIMIT ?`
	endingsCountStatement = `
		SELECT COUNT(*) FROM executor_sessions
		WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
		  AND turn_id <> ?`

	// siblingsCountStatement is how many of a conversation's other ended
	// turns still name a session.
	siblingsCountStatement = `
		SELECT COUNT(*) FROM executor_sessions
		WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
		  AND object IS NOT NULL AND turn_id <> ?`

	// turnConversationStatement is the conversation one turn's row is in,
	// which a relaunch's read resolves its scope by.
	turnConversationStatement = `
		SELECT conversation_key FROM executor_sessions
		WHERE agent_handle = ? AND turn_id = ?`

	// sessionStatement is one turn's row, whole.
	sessionStatement = `SELECT ` + sessionColumns + `
		FROM executor_sessions WHERE agent_handle = ? AND turn_id = ?`
)

// Head reads a conversation's head, its other ended turns and its siblings, in
// one transaction, at the level fresh asks for.
//
// A conversation of no key is no conversation: a turn with none continues
// nothing, and is refused [ErrInvalid] rather than answered as an empty one.
func (r *Reader) Head(ctx context.Context, handle, conversationKey string,
	fresh statelog.Freshness) (Head, error) {

	if conversationKey == "" {
		return Head{}, invalid("a turn with no conversation continues no session")
	}
	if err := checkSeat(handle, conversationKey); err != nil {
		return Head{}, err
	}
	var out Head
	answer, err := r.log.Read(ctx, fresh.Query(ConversationScope(handle, conversationKey, ""), false),
		func(tx *sql.Tx) error {
			out = Head{}
			head, headErr := oneSession(ctx, tx, headStatement, handle, conversationKey)
			if headErr != nil {
				return headErr
			}
			// NO TURN HAS AN EMPTY ID, so with no head nothing is left out.
			var headTurn string
			if head != nil {
				out.Session, headTurn = head, head.Turn
			}
			endings, total, endingsErr := boundedSessions(ctx, tx, endingsStatement,
				endingsCountStatement, MaxEndings, handle, conversationKey, headTurn)
			if endingsErr != nil {
				return endingsErr
			}
			// IN THE ORDER THEY ENDED, which is how the next turn is told
			// them: the query takes the newest, and they read oldest first.
			slices.Reverse(endings)
			out.Endings, out.EndingsOmitted = endings, total-len(endings)
			var withSessions int
			if countErr := tx.QueryRowContext(ctx, siblingsCountStatement,
				handle, conversationKey, headTurn).Scan(&withSessions); countErr != nil {
				return countErr
			}
			for i := len(endings) - 1; i >= 0 && len(out.Siblings) < MaxSiblings; i-- {
				if endings[i].HasObject() {
					out.Siblings = append(out.Siblings, endings[i])
				}
			}
			slices.Reverse(out.Siblings)
			out.SiblingsOmitted = withSessions - len(out.Siblings)
			return nil
		})
	if err != nil {
		return Head{}, err
	}
	out.Position = answer.Position
	return out, nil
}

// oneSession is the one row query answers, nil where it answers none.
func oneSession(ctx context.Context, tx *sql.Tx, query string, args ...any) (*Session, error) {
	s, err := scanSession(tx.QueryRowContext(ctx, query, args...))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &s, nil
}

// boundedSessions is at most limit rows of query (which takes args, then the
// limit), and how many rows count (which takes args alone) finds in all.
func boundedSessions(ctx context.Context, tx *sql.Tx, query, count string, limit int,
	args ...any) ([]Session, int, error) {

	rows, err := tx.QueryContext(ctx, query, append(slices.Clone(args), limit)...)
	if err != nil {
		return nil, 0, err
	}
	var out []Session
	for rows.Next() {
		s, scanErr := scanSession(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, 0, scanErr
		}
		out = append(out, s)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, 0, err
	}
	if len(out) < limit {
		return out, len(out), nil
	}
	var total int
	if err = tx.QueryRowContext(ctx, count, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// checkSeat refuses a seat and a conversation key no row could hold.
func checkSeat(handle, conversationKey string) error {
	if !iam.ValidSeatHandle(handle) {
		return invalid("%q is not a seat's handle", handle)
	}
	return checkText("the conversation key", conversationKey, ConversationKeyMaxBytes)
}

// Session is one turn's row, at the level fresh asks for: what a relaunch of
// the turn reads to continue its own session. A turn with no row is
// [ErrNotFound].
//
// ITS SCOPE IS READ OFF THE ROW, inside the answer's own transaction, because
// only the row says which conversation the turn is in; a turn this node holds
// no row of reads over its whole seat, since a capture this node could not
// decode is exactly a row it does not hold.
func (r *Reader) Session(ctx context.Context, handle, turn string,
	fresh statelog.Freshness) (Session, error) {

	if err := checkSeat(handle, ""); err != nil {
		return Session{}, err
	}
	if !canonicalTurn(turn) {
		return Session{}, invalid("the turn id %q is not a run id in its canonical spelling", turn)
	}
	resolve := func(ctx context.Context, tx *sql.Tx) (statelog.ScopeSet, error) {
		var key string
		err := tx.QueryRowContext(ctx, turnConversationStatement, handle, turn).Scan(&key)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return SeatScope(handle), nil
		case err != nil:
			return statelog.ScopeSet{}, err
		}
		return ConversationScope(handle, key, turn), nil
	}
	var out *Session
	_, err := r.log.Read(ctx, fresh.Resolved(resolve, false), func(tx *sql.Tx) (readErr error) {
		out, readErr = oneSession(ctx, tx, sessionStatement, handle, turn)
		return readErr
	})
	switch {
	case err != nil:
		return Session{}, err
	case out == nil:
		return Session{}, fmt.Errorf("%w: %s's turn %s", ErrNotFound, handle, turn)
	}
	return *out, nil
}

// Page is one page of a seat's sessions.
type Page struct {
	Sessions []Session

	// Next is the cursor of the page after this one, empty on the last.
	Next string

	// Position is the checkpoint the page was read at, and Complete false
	// where a record this node could not decode may be missing from it.
	Position statelog.Position
	Complete bool
}

// pageStatement is a seat's rows after a cursor, the latest begun first.
const pageStatement = `SELECT ` + sessionColumns + `, first_seq
	FROM executor_sessions
	WHERE agent_handle = ? AND first_seq < ?
	ORDER BY first_seq DESC LIMIT ?`

// Sessions is one page of a seat's rows, the latest begun first, after cursor
// (empty for the first page), at the level fresh asks for: the seat's sessions
// as a read surface and a seat's own search list them.
//
// PAGED BY WHERE EACH ROW BEGAN, the position of the record that first wrote
// it, which nothing moves: every other position on a row moves with its next
// capture, its end or a release of its object, and a page cursor over one of
// those would skip a row that moved past it between two pages and repeat one
// that moved back.
func (r *Reader) Sessions(ctx context.Context, handle, cursor string, limit int,
	fresh statelog.Freshness) (Page, error) {

	if err := checkSeat(handle, ""); err != nil {
		return Page{}, err
	}
	switch {
	case limit <= 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		limit = MaxPageSize
	}
	after, err := parseCursor(cursor)
	if err != nil {
		return Page{}, err
	}
	var out Page
	answer, err := r.log.Read(ctx, fresh.Query(SeatScope(handle), true), func(tx *sql.Tx) error {
		out = Page{}
		rows, queryErr := tx.QueryContext(ctx, pageStatement, handle, after, limit+1)
		if queryErr != nil {
			return queryErr
		}
		var begun []int64
		for rows.Next() {
			var first int64
			s, scanErr := scanSession(trailing{rows, &first})
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out.Sessions, begun = append(out.Sessions, s), append(begun, first)
		}
		if closeErr := errors.Join(rows.Err(), rows.Close()); closeErr != nil {
			return closeErr
		}
		if len(out.Sessions) > limit {
			out.Sessions = out.Sessions[:limit]
			out.Next = strconv.FormatInt(begun[limit-1], 10)
		}
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// trailing scans a session row with one more column after it.
type trailing struct {
	rows *sql.Rows
	last *int64
}

func (t trailing) Scan(dest ...any) error { return t.rows.Scan(append(dest, t.last)...) }

// parseCursor reads a page cursor: the position the previous page's last row
// began at, or past every position for an empty one.
func parseCursor(cursor string) (int64, error) {
	if cursor == "" {
		// PAST EVERY POSITION: a packed position is below 2^63.
		return 1<<63 - 1, nil
	}
	after, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || after <= 0 {
		return 0, invalid("the cursor %q is not one a page of sessions handed out", cursor)
	}
	return after, nil
}

// Expiry is one thing a retention delete may remove: a turn's row, or the
// tombstone an absorbed or removed row left, last touched at At; or, with no
// Turn and no ConversationKey, a removed seat's record of its removal, made at
// At ([SeatDelete]).
type Expiry struct {
	Handle          string
	ConversationKey string
	Turn            string
	At              time.Time
}

// Seat reports an expiry of a seat's removal rather than of a turn.
func (e Expiry) Seat() bool { return e.Turn == "" }

// The retention's nominations, each the oldest first through its own index.
const (
	expiredRowsStatement = `
		SELECT agent_handle, conversation_key, turn_id, last_at
		FROM executor_sessions WHERE last_at <= ?
		ORDER BY last_at, agent_handle, turn_id LIMIT ?`
	expiredTombsStatement = `
		SELECT t.agent_handle, t.conversation_key, t.turn_id, t.at
		FROM executor_session_tombs t
		WHERE t.at <= ? AND NOT EXISTS (
			SELECT 1 FROM executor_sessions s
			WHERE s.agent_handle = t.agent_handle AND s.turn_id = t.turn_id)
		ORDER BY t.at, t.agent_handle, t.turn_id LIMIT ?`
	expiredRemovalsStatement = `
		SELECT agent_handle, '', '', at
		FROM executor_seat_removals WHERE at <= ?
		ORDER BY at, agent_handle LIMIT ?`
)

// Expired nominates at most limit things last touched at or before cutoff, the
// longest untouched first: turns whose row or (row-less) tombstone is that old,
// and removed seats whose removal is. It is the retention duty's listing, and
// reads this node's copy at `stale`: it only nominates, and every delete is
// decided again in its own snapshot and at the apply.
func (r *Reader) Expired(ctx context.Context, cutoff time.Time, limit int) ([]Expiry, error) {
	if limit <= 0 {
		return nil, nil
	}
	at := store.EncodeTime(cutoff)
	var out []Expiry
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		for _, query := range []string{
			expiredRowsStatement, expiredTombsStatement, expiredRemovalsStatement,
		} {
			rows, queryErr := tx.QueryContext(ctx, query, at, limit)
			if queryErr != nil {
				return queryErr
			}
			for rows.Next() {
				var (
					e    Expiry
					last int64
				)
				if scanErr := rows.Scan(&e.Handle, &e.ConversationKey, &e.Turn, &last); scanErr != nil {
					_ = rows.Close()
					return scanErr
				}
				e.At = store.DecodeTime(last)
				out = append(out, e)
			}
			if closeErr := errors.Join(rows.Err(), rows.Close()); closeErr != nil {
				return closeErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("executordomain: list what is past %s: %w",
			cutoff.Format(time.RFC3339), err)
	}
	slices.SortStableFunc(out, func(a, b Expiry) int { return a.At.Compare(b.At) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// unendedStatement is the turns that never ended, the longest untouched
// first, through the index over exactly those rows.
const unendedStatement = `
	SELECT agent_handle, conversation_key, turn_id, last_at
	FROM executor_sessions WHERE end_seq IS NULL AND last_at <= ?
	ORDER BY last_at, agent_handle, turn_id LIMIT ?`

// Unended nominates at most limit turns that never ended and whose last
// capture was at or before before, the longest untouched first: the retention
// duty's listing of turns whose end may have been lost. It reads this node's
// copy at `stale` and only nominates: the duty leaves out every turn whose run
// the fleet still holds, and an end it publishes is decided again in its own
// snapshot, where one that landed since refuses it.
func (r *Reader) Unended(ctx context.Context, before time.Time, limit int) ([]Expiry, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []Expiry
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		rows, queryErr := tx.QueryContext(ctx, unendedStatement, store.EncodeTime(before), limit)
		if queryErr != nil {
			return queryErr
		}
		for rows.Next() {
			var (
				e    Expiry
				last int64
			)
			if scanErr := rows.Scan(&e.Handle, &e.ConversationKey, &e.Turn, &last); scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			e.At = store.DecodeTime(last)
			out = append(out, e)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("executordomain: list the turns that never ended before %s: %w",
			before.Format(time.RFC3339), err)
	}
	return out, nil
}

// handlesStatement is every seat holding a row, in order.
const handlesStatement = `
	SELECT DISTINCT agent_handle FROM executor_sessions ORDER BY agent_handle`

// Handles is every seat that holds a row here, in order, at the level fresh
// asks for: what a seat's retirement matches the departed seat against when
// its record has lost the seat's handle.
//
// AN ANSWER THAT MAY BE MISSING A SEAT IS AN ERROR ([ErrIncomplete]), never a
// shorter list: this node holding a record it could not decode may be holding
// exactly the departed seat's rows, and a retirement that matched against what
// is left would remove nothing and say it was done.
func (r *Reader) Handles(ctx context.Context, fresh statelog.Freshness) ([]string, error) {
	var out []string
	answer, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		rows, queryErr := tx.QueryContext(ctx, handlesStatement)
		if queryErr != nil {
			return queryErr
		}
		for rows.Next() {
			var handle string
			if scanErr := rows.Scan(&handle); scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out = append(out, handle)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	switch {
	case err != nil:
		return nil, fmt.Errorf("executordomain: list the seats holding sessions: %w", err)
	case !answer.Complete:
		return nil, fmt.Errorf("%w: list the seats holding sessions", ErrIncomplete)
	}
	return out, nil
}

// SealedCount is how many sessions sit sealed under each keyring key: what a
// keyring rotation waits out before it drops an old key.
//
// WAITED OUT, NEVER MOVED: a session's object is sealed once, by the capture
// that uploaded it, and leaves only with its row — absorbed by a later turn's
// end, released past the seat's budget, or deleted past the retention. So a
// rotation keeps the old key on the ring until this count under it reaches
// zero; dropped sooner, those sessions read as [ErrUnreadable] and their next
// turns start fresh.
type SealedCount struct {
	Values secrets.KeyCounts `json:"values,omitempty"`
}

// Outside is how many of the sessions are sealed under a key other than
// active.
func (c SealedCount) Outside(active string) int { return c.Values.Outside(active) }

// sealedKeysStatement counts the named objects by the key that sealed them.
const sealedKeysStatement = `
	SELECT content_key, COUNT(*) FROM executor_sessions
	WHERE object IS NOT NULL AND length(content_key) > 0 GROUP BY content_key`

// SealedKeys counts every session object this node's rows name, by the keyring
// key its row says it is sealed under, fetching none of them.
func (r *Reader) SealedKeys(ctx context.Context) (SealedCount, error) {
	out := SealedCount{Values: secrets.KeyCounts{}}
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		clear(out.Values)
		rows, queryErr := tx.QueryContext(ctx, sealedKeysStatement)
		if queryErr != nil {
			return queryErr
		}
		for rows.Next() {
			var (
				key string
				n   int
			)
			if scanErr := rows.Scan(&key, &n); scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out.Values[key] += n
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return SealedCount{}, fmt.Errorf("executordomain: count the sessions by key: %w", err)
	}
	return out, nil
}

// ---- the collector's estate -------------------------------------------- //

// ObjectEstate is this domain as the object store's passes read it: a barrier
// on the executor log and a read of its rows (internal/objstore/collect's
// Estate), through which the collector keeps every object a session's row
// names.
type ObjectEstate struct {
	Reader *Reader
}

// Name is the domain, as [SessionObjectReferences] spells it.
func (ObjectEstate) Name() string { return Domain{}.Name() }

// Barrier waits until this node has applied everything the log had committed
// when it was called, and answers where that is: a linearizable read of the
// whole domain, which appends the read index's barrier.
func (s ObjectEstate) Barrier(ctx context.Context) (statelog.Position, error) {
	if s.Reader == nil {
		return statelog.Position{}, errNoReader
	}
	served, err := s.Reader.log.Read(ctx, statelog.Query{
		Level: statelog.ReadLinearizable, Scope: DomainScope(), Set: true,
	}, func(*sql.Tx) error { return nil })
	if err != nil {
		return statelog.Position{}, err
	}
	return served.Position, nil
}

// Read runs fn over this node's rows, no earlier than at, and reports whether
// they cover every record of the domain: false while this node holds a record
// it could not apply, which the collector must read as incomplete.
func (s ObjectEstate) Read(ctx context.Context, at statelog.Position, fn func(*sql.Tx) error) (bool, error) {
	if s.Reader == nil {
		return false, errNoReader
	}
	served, err := s.Reader.log.Read(ctx, statelog.Query{
		Level: statelog.ReadStale, Scope: DomainScope(), Set: true, MinPosition: at,
	}, fn)
	if err != nil {
		return false, err
	}
	return served.Complete, nil
}

// errNoReader is an object estate built without the domain's reader.
var errNoReader = errors.New("executordomain: the object estate has no reader")
