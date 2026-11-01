package executordomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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

	CLI        string
	CLIVersion string
	SessionID  string

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
	// head, zero where it read none.
	ReadThrough statelog.Position

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
const sessionColumns = `agent_handle, turn_id, conversation_key, work_key, cli,
	cli_version, session_id, object, object_hash, object_size, content_key,
	archive_format, launch_seq, capture_seq, home, workspace, captured_at,
	read_through, ended_at, end_seq, outcome, delivered, ending, ask_excerpt`

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
		delivered             int
	)
	if err := row.Scan(&s.Handle, &s.Turn, &s.ConversationKey, &s.WorkKey, &s.CLI,
		&s.CLIVersion, &s.SessionID, &object, &hash, &size, &s.ContentKey,
		&s.ArchiveFormat, &s.LaunchSeq, &s.CaptureSeq, &s.Home, &s.Workspace, &captured,
		&readThrough, &endedAt, &endSeq, &outcome, &delivered, &ending,
		&s.AskExcerpt); err != nil {
		return Session{}, err
	}
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
type Head struct {
	// Session is the conversation's HEAD: its newest ended turn that left a
	// session. Nil where none did, and the launch starts fresh.
	Session *Session

	// Endings are the ended turns newer than the head, session or not: how
	// they ended is what the next turn is told beside the session it
	// continues. At most [MaxEndings], the newest of them in the order they
	// ended, and EndingsOmitted counts the older ones left out.
	Endings        []Session
	EndingsOmitted int

	// Siblings are the conversation's other ended turns with sessions that
	// no later turn absorbed, because they ran at once with the head: a
	// launch names them and may restore them. At most [MaxSiblings], the
	// newest first, and SiblingsOmitted counts the rest.
	Siblings        []Session
	SiblingsOmitted int

	// Position is the checkpoint the answer was read at: what the turn's
	// captures carry as their ReadThrough, so its end absorbs exactly what
	// this answer showed it.
	Position statelog.Position
}

// Head reads a conversation's head, the endings after it and its siblings, in
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
			head, err := oneSession(ctx, tx, `SELECT `+sessionColumns+`
				FROM executor_sessions
				WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
				  AND object IS NOT NULL
				ORDER BY end_seq DESC LIMIT 1`, handle, conversationKey)
			if err != nil {
				return err
			}
			var after int64
			if head != nil {
				out.Session = head
				after = head.EndedAtPosition.Packed()
			}
			if out.Endings, out.EndingsOmitted, err = boundedSessions(ctx, tx, `
				FROM executor_sessions
				WHERE agent_handle = ? AND conversation_key = ? AND end_seq > ?`,
				MaxEndings, handle, conversationKey, after); err != nil {
				return err
			}
			// IN THE ORDER THEY ENDED, which is how the next turn is told
			// them: the query takes the newest, and they read oldest first.
			slices.Reverse(out.Endings)
			if head == nil {
				return nil
			}
			out.Siblings, out.SiblingsOmitted, err = boundedSessions(ctx, tx, `
				FROM executor_sessions
				WHERE agent_handle = ? AND conversation_key = ? AND end_seq < ?
				  AND object IS NOT NULL`,
				MaxSiblings, handle, conversationKey, after)
			return err
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

// boundedSessions is at most limit rows of where (a FROM clause and its
// conditions), newest end first, and how many more it holds.
func boundedSessions(ctx context.Context, tx *sql.Tx, where string, limit int,
	args ...any) ([]Session, int, error) {

	rows, err := tx.QueryContext(ctx, `SELECT `+sessionColumns+` `+where+`
		ORDER BY end_seq DESC, turn_id DESC LIMIT ?`, append(slices.Clone(args), limit)...)
	if err != nil {
		return nil, 0, err
	}
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		out = append(out, s)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, 0, err
	}
	if len(out) < limit {
		return out, 0, nil
	}
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total - len(out), nil
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
	resolve := func(ctx context.Context, tx *sql.Tx) (statelog.ScopeSet, error) {
		var key string
		err := tx.QueryRowContext(ctx, `
			SELECT conversation_key FROM executor_sessions
			WHERE agent_handle = ? AND turn_id = ?`, handle, turn).Scan(&key)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return SeatScope(handle), nil
		case err != nil:
			return statelog.ScopeSet{}, err
		}
		return ConversationScope(handle, key, turn), nil
	}
	var out *Session
	_, err := r.log.Read(ctx, fresh.Resolved(resolve, false), func(tx *sql.Tx) error {
		var err error
		out, err = oneSession(ctx, tx, `SELECT `+sessionColumns+`
			FROM executor_sessions WHERE agent_handle = ? AND turn_id = ?`, handle, turn)
		return err
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

// Sessions is one page of a seat's rows, the most recently written first,
// after cursor (empty for the first page), at the level fresh asks for: the
// seat's sessions as a read surface and a seat's own search list them.
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
	afterVersion, afterTurn, err := parseCursor(cursor)
	if err != nil {
		return Page{}, err
	}
	var out Page
	answer, err := r.log.Read(ctx, fresh.Query(SeatScope(handle), true), func(tx *sql.Tx) error {
		out = Page{}
		rows, err := tx.QueryContext(ctx, `SELECT `+sessionColumns+`, version
			FROM executor_sessions
			WHERE agent_handle = ? AND (version < ? OR (version = ? AND turn_id < ?))
			ORDER BY version DESC, turn_id DESC LIMIT ?`,
			handle, afterVersion, afterVersion, afterTurn, limit+1)
		if err != nil {
			return err
		}
		var versions []int64
		for rows.Next() {
			var (
				s       Session
				version int64
			)
			s, err = scanSession(versioned{rows, &version})
			if err != nil {
				_ = rows.Close()
				return err
			}
			out.Sessions, versions = append(out.Sessions, s), append(versions, version)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(out.Sessions) > limit {
			out.Sessions = out.Sessions[:limit]
			last := out.Sessions[limit-1]
			out.Next = strconv.FormatInt(versions[limit-1], 10) + ":" + last.Turn
		}
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// versioned scans a session row with its version column after it.
type versioned struct {
	rows    *sql.Rows
	version *int64
}

func (v versioned) Scan(dest ...any) error { return v.rows.Scan(append(dest, v.version)...) }

// parseCursor reads a page cursor: the version and turn the previous page
// ended on, or the start for an empty one.
func parseCursor(cursor string) (int64, string, error) {
	if cursor == "" {
		// PAST EVERY POSITION: a packed position is below 2^63.
		return 1<<63 - 1, "", nil
	}
	raw, turn, ok := strings.Cut(cursor, ":")
	version, err := strconv.ParseInt(raw, 10, 64)
	if !ok || err != nil || turn == "" {
		return 0, "", invalid("the cursor %q is not one a page of sessions handed out", cursor)
	}
	return version, turn, nil
}

// Expiry is one turn a retention delete may remove: a row, or the tombstone an
// absorbed or removed row left, last touched at At.
type Expiry struct {
	Handle          string
	ConversationKey string
	Turn            string
	At              time.Time
}

// Expired nominates at most limit turns whose row or (row-less) tombstone was
// last touched at or before cutoff, the longest untouched first: the retention
// duty's listing. It reads this node's copy at `stale`, and only nominates:
// every delete is decided again in its own snapshot and at the apply.
func (r *Reader) Expired(ctx context.Context, cutoff time.Time, limit int) ([]Expiry, error) {
	if limit <= 0 {
		return nil, nil
	}
	at := store.EncodeTime(cutoff)
	var out []Expiry
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		for _, query := range []string{`
			SELECT agent_handle, conversation_key, turn_id, last_at
			FROM executor_sessions WHERE last_at <= ?
			ORDER BY last_at, agent_handle, turn_id LIMIT ?`, `
			SELECT t.agent_handle, t.conversation_key, t.turn_id, t.at
			FROM executor_session_tombs t
			WHERE t.at <= ? AND NOT EXISTS (
				SELECT 1 FROM executor_sessions s
				WHERE s.agent_handle = t.agent_handle AND s.turn_id = t.turn_id)
			ORDER BY t.at, t.agent_handle, t.turn_id LIMIT ?`,
		} {
			rows, err := tx.QueryContext(ctx, query, at, limit)
			if err != nil {
				return err
			}
			for rows.Next() {
				var (
					e    Expiry
					last int64
				)
				if err := rows.Scan(&e.Handle, &e.ConversationKey, &e.Turn, &last); err != nil {
					_ = rows.Close()
					return err
				}
				e.At = store.DecodeTime(last)
				out = append(out, e)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("executordomain: list the sessions past %s: %w",
			cutoff.Format(time.RFC3339), err)
	}
	slices.SortStableFunc(out, func(a, b Expiry) int { return a.At.Compare(b.At) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Handles is every seat that holds a row here, in order: what a seat's
// retirement matches the departed seat against when its record has lost the
// seat's handle.
func (r *Reader) Handles(ctx context.Context) ([]string, error) {
	var out []string
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `
			SELECT DISTINCT agent_handle FROM executor_sessions ORDER BY agent_handle`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var handle string
			if err := rows.Scan(&handle); err != nil {
				_ = rows.Close()
				return err
			}
			out = append(out, handle)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("executordomain: list the seats holding sessions: %w", err)
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

// SealedKeys counts every session object this node's rows name, by the keyring
// key its row says it is sealed under, fetching none of them.
func (r *Reader) SealedKeys(ctx context.Context) (SealedCount, error) {
	out := SealedCount{Values: secrets.KeyCounts{}}
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		clear(out.Values)
		rows, err := tx.QueryContext(ctx, `
			SELECT content_key, COUNT(*) FROM executor_sessions
			WHERE object IS NOT NULL AND length(content_key) > 0 GROUP BY content_key`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				key string
				n   int
			)
			if err := rows.Scan(&key, &n); err != nil {
				_ = rows.Close()
				return err
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
