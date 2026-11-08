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

	"github.com/crewlet/crewlet/internal/executorkeep"
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
	// an ending-only row, or one whose session the seat's budget released or
	// a run's running out of context retired ([Session.RetiredBy]).
	Object        objstore.Object
	ContentKey    string
	ArchiveFormat int

	LaunchSeq  int64
	CaptureSeq int64

	Home      string
	Workspace string

	// ContextTokens and Interrupted are the newest capture's: the whole
	// input of its session's last request, and whether its harness was cut
	// off ([Capture.ContextTokens], [Capture.Interrupted]). Zero on a row
	// nothing was captured into, and on one captured by a build before them.
	ContextTokens int64
	Interrupted   bool

	// CapturedAt is the broker's instant for the newest capture, zero on a
	// row nothing was captured into.
	CapturedAt time.Time

	// ReadThrough is where the turn's first launch read its conversation's
	// head, zero where it read none, and Absorbs the turns that read handed
	// out in full, which the turn's end absorbs.
	ReadThrough statelog.Position
	Absorbs     []string

	// BaseTurn is the ended turn of the conversation whose session the
	// turn's first read froze to continue, as the turn's `begin` recorded it
	// ([Begin]); empty where the read named none, the base had moved before
	// the begin landed, or no begin landed. While the row is live and names
	// no capture, it HOLDS that turn's row against every release but a
	// retirement, which takes a session a run ran out of context in off every
	// ended row that names it, held or not ([RetiredBy]).
	BaseTurn string

	// RetiredBy is the turn in which this row's session ran out of its model's
	// context, which took it off the row ([applyEnd]); empty where none did.
	RetiredBy string

	// EndedAt and EndedAtPosition are the end's broker instant and its
	// position on the log, both zero while the turn is live; Ending is how
	// it ended, nil until then.
	EndedAt         time.Time
	EndedAtPosition statelog.Position
	Ending          *executorkeep.Ending

	// AskExcerpt is what the turn was asked: somebody else's words.
	AskExcerpt string `untrusted:"fence"`
}

// Ended reports a turn whose end applied.
func (s Session) Ended() bool { return s.Ending != nil }

// HasObject reports a row that names a session's object.
func (s Session) HasObject() bool { return !s.Object.Key.IsZero() }

// Unkept is the turn's newest capture its end named as not kept
// ([executorkeep.Ending.Unkept]), WHERE THAT STILL STANDS ON THE ROW: nil where
// the end named none, or the turn has not ended, and nil where the capture it
// named is no newer than the place the row keeps.
//
// DECIDED ON THE ROW'S OWN PLACE, as the end's apply decided whether to take an
// older session off the row ([applyEnd]): an end is decided off the run's
// record, which can name a capture as not kept whose record the log took all
// the same — sent before the end was decided and taken before the end, a record
// a taker handed on before its lease's window closed among them — and the row's
// place is then that capture's, its session kept. One NEWER than the place
// stands whether or not the row still names a session: an end stamped below
// [wentOnVersion] took the older one off, and one stamped at or above kept it,
// saying the turn went on past it ([Session.WentOnNote] names the capture not
// kept beside it). Every account of an end reads the unkept capture through
// this one rule, so none tells a session the row keeps as one that was not:
// [Session.NotKept], [Session.WentOnNote], and [Session.Redacted], the one form
// every reader is shown a row in — the read surface's, a seat's own reads and
// searches of its sessions, and the account of the turn its conversation's
// next turn is told.
func (s Session) Unkept() *executorkeep.Unkept {
	if s.Ending == nil || s.Ending.Unkept == nil ||
		!unkeptSupersedes(*s.Ending.Unkept, s.LaunchSeq, s.CaptureSeq) {
		return nil
	}
	unkept := *s.Ending.Unkept
	return &unkept
}

// WentOn reports a row naming a session its turn WENT ON PAST, or may have: an
// ended row naming an object whose end said the turn went on past it
// ([executorkeep.Ending.WentOn]) — kept from a capture taken while its job still
// worked, or from an earlier launch's — or whose turn was LOST with its
// executor's box or host, which nobody saw end and whose session may not hold
// the last of what it did. Such a row is still continued by the conversation's
// next turn, which is told so ([Session.Kept]), and still read as the turn's
// session; only never as all the turn did.
func (s Session) WentOn() bool {
	return s.HasObject() && s.Ending != nil &&
		(s.Ending.WentOn || s.Ending.Outcome == executorkeep.OutcomeLost)
}

// WentOnNote is what a reader is told of a row its turn went on past
// ([Session.WentOn]), "" of any other: that it is kept up to a session the turn
// went on past — or, of a lost turn whose end did not say so, may have — and,
// where its end names a newer capture that was not kept, which and why. Of an
// end that said so, that the session was taken while the turn still worked
// and what the turn did after is not in it.
//
// THE NOTE IS THE ROW'S OWN FIELDS IN A READER'S WORDS, and the unkept capture
// it names is the one [Session.Unkept] stands: an end stamped [wentOnVersion]
// or above keeps the row's session beside an unkept capture newer than its
// place, which the note says, and one no newer than the place is the row's
// own. SOMEBODY ELSE'S WORDS IN IT, the unkept capture's reason quoting what
// refused it, so a reader asks it of the shown form ([Session.Redacted]),
// whose reason is redacted, as every reader of a row is shown it.
func (s Session) WentOnNote() string {
	if !s.WentOn() {
		return ""
	}
	note := "kept up to a session taken while the turn still worked; the turn went on past it, and what " +
		"it did after is not in it"
	if !s.Ending.WentOn {
		note = "kept up to a session its turn may have gone on past: the turn was lost with its executor's " +
			"box or host, so the session may not hold the last of what it did"
	}
	if unkept := s.Unkept(); unkept != nil {
		reason := unkept.Reason
		if reason == "" {
			reason = "no reason was recorded"
		}
		note += fmt.Sprintf("; its newer capture (launch %d, capture %d) was not kept: %s", unkept.LaunchSeq,
			unkept.CaptureSeq, reason)
	}
	return note
}

// Kept is the row as continuing its turn's kept session reads it
// ([executorkeep.TurnRow]): the ONE conversion of a row into the sandbox's
// view of it, which the engine's restore and the read surface both make, so a
// reader of whether a relaunch would continue the session decides on the
// fields the relaunch does — and the conversation's next turn, continuing a
// session its turn went on past, is told so ([Session.WentOn]).
func (s Session) Kept() executorkeep.TurnRow {
	return executorkeep.TurnRow{
		Handle: s.Handle, Turn: s.Turn,
		Harness: s.Harness, HarnessVersion: s.HarnessVersion, SessionID: s.SessionID,
		ArchiveFormat: s.ArchiveFormat, LaunchSeq: s.LaunchSeq, CaptureSeq: s.CaptureSeq,
		Object: s.Object, Ended: s.Ended(),
		ConversationKey: s.ConversationKey, NotKept: s.NotKept(),
		ContentKey: s.ContentKey, Home: s.Home, Workspace: s.Workspace,
		ContextTokens: s.ContextTokens, Interrupted: s.Interrupted, CapturedAt: s.CapturedAt,
		WentOn: s.WentOn(), Lost: s.Ending != nil && s.Ending.Outcome == executorkeep.OutcomeLost,
		EndedAt: s.EndedAt, BaseTurn: s.BaseTurn,
	}
}

// Kept is the read as a turn's first launch freezes it
// ([executorkeep.ConversationHead]): the ONE conversion of a head into the
// sandbox's view of it, as [Session.Kept] is of a row. A read made at no
// position carries none.
func (h Head) Kept() executorkeep.ConversationHead {
	kept := executorkeep.ConversationHead{Absorbs: h.Absorbs()}
	if h.Position.Seq != 0 {
		kept.ReadThrough = h.Position.Packed()
	}
	for _, s := range h.Siblings {
		kept.Siblings = append(kept.Siblings, s.Kept())
	}
	if h.Session != nil {
		session := h.Session.Kept()
		kept.Session = &session
	}
	return kept
}

// RetiredWhy is why the read names no head where a session ran out of context, ""
// where it names one or nothing did: what a launch that starts fresh logs, so an
// operator can tell a retired line from one that never was.
func (h Head) RetiredWhy() string {
	if h.Session != nil {
		return ""
	}
	for _, ended := range slices.Backward(h.Endings) {
		if ended.RetiredBy != "" {
			return executorkeep.ExhaustedWhy(ended.SessionID, ended.RetiredBy)
		}
	}
	return ""
}

// RelaunchRead is the level a relaunch reads its turn's row at, and so the
// level every reader of whether a relaunch would continue the turn's session
// decides on (internal/sandbox's ReadContinuityOf): the capture it continues
// may have been recorded by another node's keep, which this node's copy may
// not have applied yet.
func RelaunchRead() statelog.Freshness { return statelog.Freshness{Level: statelog.ReadLinearizable} }

// NotKept is why the row names no session's object, in a reader's words, ""
// where it names one. A row names none for one of four reasons, each the
// domain's own and told apart off the row alone: a launch ran its session out of
// its model's context, which retired it ([Session.RetiredBy]); nothing of the
// turn's session was captured (yet, while the turn is live, its row its begin's);
// the turn's newest capture was not kept, which its end said ([executorkeep.Ending.Unkept]) — and
// no older one was, or an end stamped below [wentOnVersion] released the older one the row
// named ([applyEnd]) — told by the unkept capture being newer than the place the row keeps,
// which a release leaves as it was ([Session.Unkept]); or the seat's session budget released it to make room
// ([seatPlan]), the one other release that leaves a row, and one that takes
// only an ended turn's.
//
// SOMEBODY ELSE'S WORDS IN IT: the unkept capture's reason quotes what refused
// it — an error a box or a store wrote — so a reader redacts what this
// answers, as it redacts every text a row holds.
func (s Session) NotKept() string {
	unkept := s.Unkept()
	switch {
	case s.HasObject():
		return ""
	case s.RetiredBy != "":
		return executorkeep.ExhaustedWhy(s.SessionID, s.RetiredBy) + "; how the turn ended is kept"
	case unkept != nil:
		reason := unkept.Reason
		if reason == "" {
			reason = "no reason was recorded"
		}
		return fmt.Sprintf("the turn's newest capture of its session (launch %d, capture %d) was not kept: "+
			"%s; how the turn ended is kept", unkept.LaunchSeq, unkept.CaptureSeq, reason)
	case s.CapturedAt.IsZero() && !s.Ended():
		return "nothing of this turn's session has been captured yet"
	case s.CapturedAt.IsZero():
		return "nothing of this turn's session was captured: its run ended before its harness packed one, " +
			"its harness carries none, or the company kept no session"
	}
	return "the seat's session budget released this session's object to make room for newer ones; how the " +
		"turn ended is kept"
}

// rowScanner is one row, from a query or a query row.
type rowScanner interface{ Scan(dest ...any) error }

// sessionColumns is every column a [Session] is read from, in [scanSession]'s
// order.
const sessionColumns = `agent_handle, turn_id, conversation_key, work_key, harness,
	harness_version, session_id, object, object_hash, object_size, content_key,
	archive_format, launch_seq, capture_seq, home, workspace, captured_at,
	read_through, absorbs, ended_at, end_seq, outcome, delivered, ending, ask_excerpt,
	base_turn, context_tokens, interrupted, retired_by`

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
		interrupted           int
	)
	if err := row.Scan(&s.Handle, &s.Turn, &s.ConversationKey, &s.WorkKey, &s.Harness,
		&s.HarnessVersion, &s.SessionID, &object, &hash, &size, &s.ContentKey,
		&s.ArchiveFormat, &s.LaunchSeq, &s.CaptureSeq, &s.Home, &s.Workspace, &captured,
		&readThrough, &absorbs, &endedAt, &endSeq, &outcome, &delivered, &ending,
		&s.AskExcerpt, &s.BaseTurn, &s.ContextTokens, &interrupted, &s.RetiredBy); err != nil {
		return Session{}, err
	}
	s.Absorbs, s.Interrupted = splitAbsorbs(absorbs), interrupted == 1
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
		var e executorkeep.Ending
		if err := json.Unmarshal([]byte(ending), &e); err != nil {
			return Session{}, fmt.Errorf("executordomain: %s's turn %s's ending: %w",
				s.Handle, s.Turn, err)
		}
		// THE COLUMNS ARE THE ACCOUNT: the outcome as the row was ended
		// with, and whether a reply went out, which a dedupe merges into
		// the row rather than into the ending's bytes.
		e.Outcome, e.Delivered = executorkeep.Outcome(outcome), delivered == 1
		s.Ending = &e
	}
	return s, nil
}

// Head is what a conversation's next turn launches from.
//
// # The head is the turn that read the conversation last
//
// Every turn's first launch continues the head its read named, so a
// conversation's sessions are a LINE, each continuing the one before, and the
// newest point of it is the ended turn whose READ is newest: what every later
// read built on. Never simply the newest END. Two turns of one conversation run
// beside each other whenever one parks on a person's question, which frees its
// seat — and every time for a schedule, whose next fire is never taken as that
// question's answer — so a turn that read the conversation early can end after
// turns that read it later and continued each other. Taken as the head for
// ending last, it would be continued while their line, which its read never
// saw, was told and then absorbed by the next end. So the head is, of the ended
// turns whose rows still name a session, the one that read the conversation
// last, the newest end among turns that read it at one position, and a turn
// that read nothing — its read failed, so it continued nothing and absorbs
// nothing — after every turn that read. A turn that read early and ended late
// is told and offered beside the head, as every other ended turn is, and
// absorbed by the next end.
//
// The seat's budget releases the oldest END first ([seatPlan]), so it can
// release a head before such a turn: the head is then, of the sessions the
// conversation still keeps, the one that read it last, and the released head's
// ending is told as every other ending is.
//
// # What is left of a conversation is what no ended turn was shown
//
// A turn's end absorbs exactly the turns its first launch's read handed out
// ([Head.Absorbs], carried on its captures), so every ended row a conversation
// still holds is one no ended turn was shown: a turn that ended beside the
// head, after it, or before it but past the bound of the read that showed the
// head's turn the rest. So the next turn is shown ALL of them — never only the
// ones that ended after the head, which would leave a turn that ended before
// it, beside a turn parked on a person, shown to nobody, and then absorbed by
// the next end as though it had been.
type Head struct {
	// Session is the conversation's HEAD: of its ended turns whose rows
	// still name a session, the one that read the conversation last, which
	// the next turn continues (see above). Nil where none names one, and the
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
	// headStatement is a conversation's HEAD: of its ended turns that name a
	// session, the one that read the conversation last — the newest end
	// among turns that read it at one position, and a turn that read none
	// (its read failed) after every turn that did ([Head.Session]). AN END'S
	// POSITION IS UNIQUE among the rows that hold one, so the pair is a total
	// order, and the conversation's line index serves it without a sort.
	headStatement = `SELECT ` + sessionColumns + `
		FROM executor_sessions INDEXED BY ` + lineIdx + `
		WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
		  AND object IS NOT NULL
		ORDER BY read_through DESC, end_seq DESC LIMIT 1`

	// endingsStatement is a conversation's other ended turns, the newest
	// first, and endingsCountStatement how many there are. AN END'S
	// POSITION IS UNIQUE among the rows that hold one, so it alone orders
	// them, and the conversation's index serves the order.
	endingsStatement = `SELECT ` + sessionColumns + `
		FROM executor_sessions INDEXED BY ` + conversationIdx + `
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
		SELECT conversation_key FROM executor_sessions INDEXED BY ` + sessionKey + `
		WHERE agent_handle = ? AND turn_id = ?`

	// sessionStatement is one turn's row, whole.
	sessionStatement = `SELECT ` + sessionColumns + `
		FROM executor_sessions INDEXED BY ` + sessionKey + ` WHERE agent_handle = ? AND turn_id = ?`
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

// SessionRead is one turn's row as [Reader.Session] answers it, and where it
// was read: what a read surface states beside the row, as every answer states
// the point its rows were derived through.
type SessionRead struct {
	Session Session

	// Head is the head of the row's conversation as the same read saw it —
	// the turn whose session that conversation's next turn would continue —
	// empty where the conversation has none, or the row has no conversation:
	// so whether the row is its conversation's head is decided from the
	// snapshot the row was read in, never from a second read.
	Head string

	// Position is the checkpoint the row was read at, and Complete the
	// read's own word on whether a record this node could not decode may be
	// missing from it — which a point read the ladder served never is: such
	// a record covering the turn refuses the read instead. Carried as the
	// read reported it rather than stated here, so the two cannot disagree.
	Position statelog.Position
	Complete bool
}

// Session is one turn's row, at the level fresh asks for, and where it was
// read: what a relaunch of the turn reads to continue its own session, and a
// person's view of one session. A turn with no row is [ErrNotFound].
//
// ITS SCOPE IS READ OFF THE ROW, inside the answer's own transaction, because
// only the row says which conversation the turn is in; a turn this node holds
// no row of reads over its whole seat, since a capture this node could not
// decode is exactly a row it does not hold.
func (r *Reader) Session(ctx context.Context, handle, turn string,
	fresh statelog.Freshness) (SessionRead, error) {

	if err := checkSeat(handle, ""); err != nil {
		return SessionRead{}, err
	}
	if !canonicalTurn(turn) {
		return SessionRead{}, invalid("the turn id %q is not a run id in its canonical spelling", turn)
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
	var (
		out  *Session
		head string
	)
	answer, err := r.log.Read(ctx, fresh.Resolved(resolve, false), func(tx *sql.Tx) (readErr error) {
		head = ""
		if out, readErr = oneSession(ctx, tx, sessionStatement, handle, turn); readErr != nil || out == nil {
			return readErr
		}
		heads, readErr := headsOf(ctx, tx, handle, []Session{*out})
		head = heads[out.ConversationKey]
		return readErr
	})
	switch {
	case err != nil:
		return SessionRead{}, err
	case out == nil:
		return SessionRead{}, fmt.Errorf("%w: %s's turn %s", ErrNotFound, handle, turn)
	}
	return SessionRead{Session: *out, Head: head, Position: answer.Position,
		Complete: answer.Complete}, nil
}

// headsOf is the head of every distinct conversation sessions name, keyed by
// the conversation's key: the turn the conversation's head statement answers
// ([Reader.Head]'s own), or "" where the conversation has none. A row with no
// conversation names none, and is not keyed.
//
// ONE SEEK PER CONVERSATION, in the caller's transaction, after its rows are
// read: never a head chosen among the rows in hand, since the head of a
// conversation a page shows one turn of may be on no page at all.
func headsOf(ctx context.Context, tx *sql.Tx, handle string, sessions []Session) (map[string]string, error) {
	var out map[string]string
	for _, s := range sessions {
		if s.ConversationKey == "" {
			continue
		}
		if _, read := out[s.ConversationKey]; read {
			continue
		}
		head, err := oneSession(ctx, tx, headStatement, handle, s.ConversationKey)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]string{}
		}
		out[s.ConversationKey] = ""
		if head != nil {
			out[s.ConversationKey] = head.Turn
		}
	}
	return out, nil
}

// Page is one page of a seat's sessions.
type Page struct {
	Sessions []Session

	// Heads is the head of every conversation the page's rows name, by the
	// conversation's key, read in the page's own transaction ([headsOf]):
	// "" for a conversation with none, and nil for a page naming none.
	Heads map[string]string

	// Next is the cursor of the page after this one, empty on the last.
	Next string

	// Position is the checkpoint the page was read at, and Complete false
	// where a record this node could not decode may be missing from it.
	Position statelog.Position
	Complete bool
}

// pageStatement is a seat's rows after a cursor, the latest begun first.
const pageStatement = `SELECT ` + sessionColumns + `, first_seq
	FROM executor_sessions INDEXED BY ` + seatIdx + `
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
		var headErr error
		out.Heads, headErr = headsOf(ctx, tx, handle, out.Sessions)
		return headErr
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

	// heldEverywhereStatement is every base a live turn holds, in every
	// seat: a pass over the bases' index, which holds only those rows.
	heldEverywhereStatement = `
		SELECT agent_handle, base_turn FROM executor_sessions INDEXED BY ` + heldIdx + `
		WHERE end_seq IS NULL AND launch_seq = 0 AND base_turn <> ''`
)

// heldTurn is one turn of one seat a live turn holds as its base.
type heldTurn struct{ handle, turn string }

// Expired nominates at most limit things last touched at or before cutoff, the
// longest untouched first: turns whose row or (row-less) tombstone is that old,
// and removed seats whose removal is. It is the retention duty's listing, and
// reads this node's copy at `stale`: it only nominates, and every delete is
// decided again in its own snapshot and at the apply.
//
// A ROW A LIVE TURN HOLDS AS ITS BASE IS LEFT OUT, however old: its delete
// would be refused ([expiredIn]), and listed it would fill the duty's page on
// every pass while it holds, so the rows past it would never be reached. The
// holds are read in the listing's own transaction, and the rows asked for as
// many more as there are holds, so a page the held rows would fill still holds
// limit of the rest — decided here row by row rather than by a subquery of the
// listing's statement.
func (r *Reader) Expired(ctx context.Context, cutoff time.Time, limit int) ([]Expiry, error) {
	if limit <= 0 {
		return nil, nil
	}
	at := store.EncodeTime(cutoff)
	var out []Expiry
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		holds, holdErr := heldEverywhere(ctx, tx)
		if holdErr != nil {
			return holdErr
		}
		for _, query := range []struct {
			statement string
			limit     int
		}{
			{expiredRowsStatement, limit + len(holds)},
			{expiredTombsStatement, limit},
			{expiredRemovalsStatement, limit},
		} {
			rows, queryErr := tx.QueryContext(ctx, query.statement, at, query.limit)
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
				if holds[heldTurn{e.Handle, e.Turn}] {
					continue
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

// heldEverywhere is every turn a live turn holds as its base, in every seat.
func heldEverywhere(ctx context.Context, tx *sql.Tx) (map[heldTurn]bool, error) {
	rows, err := tx.QueryContext(ctx, heldEverywhereStatement)
	if err != nil {
		return nil, err
	}
	out := map[heldTurn]bool{}
	for rows.Next() {
		var h heldTurn
		if scanErr := rows.Scan(&h.handle, &h.turn); scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		out[h] = true
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// unendedStatement is the turns that never ended, the longest untouched
// first, through the index over exactly those rows.
const unendedStatement = `
	SELECT agent_handle, conversation_key, turn_id, last_at
	FROM executor_sessions WHERE end_seq IS NULL AND last_at <= ?
	ORDER BY last_at, agent_handle, turn_id LIMIT ?`

// Unended nominates at most limit turns that never ended and whose last record
// (their newest capture, or the begin of a turn that captured nothing) was at
// or before before, the longest untouched first: the retention duty's listing
// of turns whose end may have been lost. It reads this node's copy at `stale`
// and only nominates: the duty leaves out every turn whose run the fleet still
// holds, and an end it publishes is decided again in its own snapshot, where
// one that landed since refuses it.
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
		Level: statelog.ReadStale, Scope: DomainScope(), Set: true, Floors: []statelog.Position{at},
	}, fn)
	if err != nil {
		return false, err
	}
	return served.Complete, nil
}

// errNoReader is an object estate built without the domain's reader.
var errNoReader = errors.New("executordomain: the object estate has no reader")
