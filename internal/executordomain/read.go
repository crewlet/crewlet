package executordomain

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
// Every read goes through [statelog.Reader], so its refusal ladder holds and an
// undecodable record is reported, never answered around. A launch reads its
// conversation's head at `linearizable`: the previous turn's `end` may have been
// published by another node, and an older head continues a session missing the
// last turn. A refused read is the caller's to fail open on: a launch starts
// fresh, saying it could not read its history.
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

	// Entry is the providers.llm entry the session ran under ([Capture.Entry]),
	// "" where its capture named none.
	Entry string

	// Object is the sealed session, its Key zero on a row that names none:
	// an ending-only row, or one whose session the seat's budget released or
	// a launch's spending it retired ([Session.RetiredBy]).
	Object        objstore.Object
	ContentKey    string
	ArchiveFormat int

	// Media is every media object the session's frames name ([Capture.Media]),
	// in digest order, none for a session kept whole: filled by every read of
	// a row naming an object ([withMedia]), so every opener splices it whole.
	Media []executorkeep.MediaObject `json:",omitempty"`

	LaunchSeq  int64
	CaptureSeq int64

	Home      string
	Workspace string

	// ContextTokens and Interrupted are the newest capture's
	// ([Capture.ContextTokens], [Capture.Interrupted]); zero on a row nothing
	// was captured into.
	ContextTokens int64
	Interrupted   bool

	// Trimmed is the newest capture's ([Capture.Trimmed]): its session was cut
	// to what its harness's resume loads, said by [Session.TrimmedNote].
	Trimmed bool

	// CapturedAt is the broker's instant for the newest capture, zero on a
	// row nothing was captured into.
	CapturedAt time.Time

	// ReadThrough is where the turn's first launch read its conversation's
	// head, zero where it read none, and Absorbs the turns that read handed
	// out in full, which the turn's end absorbs.
	ReadThrough statelog.Position
	Absorbs     []string

	// BaseTurn is the ended turn whose session the turn's first read froze to
	// continue, as its `begin` recorded it ([Begin]); empty where the read
	// named none, the base moved before the begin landed, or none landed.
	// While the row is live and names no capture it HOLDS that turn's row
	// against every release but a retirement (package doc, "A turn's first
	// read holds its base").
	BaseTurn string

	// RetiredBy is the turn a launch of which spent this row's session, whose end
	// took it off the row ([applyEnd]), and RetiredLoss how, as that end kept it
	// ([executorkeep.Ending.SpentSessions]); both empty where none did.
	RetiredBy   string
	RetiredLoss executorkeep.Loss

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

// MediaBytes is the sealed size of the media objects the row names, each
// counted here however many other rows name it too.
func (s Session) MediaBytes() int64 {
	var n int64
	for _, m := range s.Media {
		n += m.Object.Size
	}
	return n
}

// Unkept is the turn's newest capture its end named as not kept
// ([executorkeep.Ending.Unkept]), WHERE THAT STILL STANDS ON THE ROW: nil where
// the end named none, the turn has not ended, or the named capture is no newer
// than the row's place.
//
// Decided on the row's own place, as [applyEnd] decides: an end is decided off
// the run's record, which can name as unkept a capture the log took anyway
// (sent before the end was decided), and the row's place is then that capture's.
// One NEWER than the place stands whether or not the row still names a session:
// an end below [wentOnVersion] took the older one off, one at or above kept it.
// [Session.NotKept], [Session.WentOnNote] and [Session.Redacted] all read the
// unkept capture through this one rule, so none calls a kept session unkept.
func (s Session) Unkept() *executorkeep.Unkept {
	if s.Ending == nil || s.Ending.Unkept == nil ||
		!unkeptSupersedes(*s.Ending.Unkept, s.LaunchSeq, s.CaptureSeq) {
		return nil
	}
	unkept := *s.Ending.Unkept
	return &unkept
}

// endWentPast reports whether the row's end says its turn went on past THE
// SESSION THIS ROW NAMES: [executorkeep.Ending.WentOn] is set, and its
// [executorkeep.Ending.LastWord], where it names one, is newer than the row's
// place. THE ONE READING OF WENT_ON: [Session.WentOn], [Session.WentOnNote],
// [Session.Kept] and [Session.Redacted] all decide on it, so none disagree.
//
// Held to the row's place, as [Session.Unkept] is: the run's record can hold
// refused a capture the log took anyway, and where that is the last word the
// row holds all the turn did. An end naming no last word went past whatever the
// row names. Read at read time, it is right whichever side of the end a late
// record landed: taken before the end, it is the row's place; after, the end's
// tombstone refused it.
func (s Session) endWentPast() bool {
	if s.Ending == nil || !s.Ending.WentOn {
		return false
	}
	w := s.Ending.LastWord
	return w == nil || newer(w.LaunchSeq, w.CaptureSeq, s.LaunchSeq, s.CaptureSeq)
}

// WentOn reports a row naming a session its turn WENT ON PAST, or may have: an
// ended row naming an object whose end went past it ([Session.endWentPast]),
// or whose turn ended LOST ([executorkeep.OutcomeLost]), so its session may not
// hold the last of what it did. The next turn still continues such a row, told
// which of the two it is ([Session.Kept]); it is only never read as all the
// turn did. A row holding the last word its end named is one only if lost.
func (s Session) WentOn() bool {
	return s.HasObject() && s.Ending != nil &&
		(s.endWentPast() || s.Ending.Outcome == executorkeep.OutcomeLost)
}

// WentOnNote is a row its turn went on past ([Session.WentOn]) in a reader's
// words, "" of any other: that it is kept up to a session the turn went on past
// (or, of a lost turn, may have), and which newer capture [Session.Unkept]
// stands and why. The one sentence of it, for every surface that shows a row in
// words.
//
// It quotes somebody else's words (the unkept capture's reason), so a reader
// asks it of the shown form ([Session.Redacted]), whose reason is redacted.
func (s Session) WentOnNote() string {
	if !s.WentOn() {
		return ""
	}
	note := "kept up to a session taken while the turn still worked; the turn went on past it, and what " +
		"it did after is not in it"
	if !s.endWentPast() {
		note = "kept up to a session its turn may have gone on past: the turn was lost, so the session may " +
			"not hold the last of what it did"
	}
	if unkept := s.Unkept(); unkept != nil {
		note += fmt.Sprintf("; its newer capture (launch %d, capture %d) was not kept: %s", unkept.LaunchSeq,
			unkept.CaptureSeq, unkeptReason(*unkept))
	}
	return note
}

// InProgress reports a row naming the session of a turn that HAS NOT ENDED:
// the newest session its runs have kept, which a run may have gone on past and
// whose last call may have no result yet. It is read as the turn's session,
// never an ended turn's. Whether a relaunch continues it is the relaunch's own
// decision (internal/sandbox's ContinuityOf): it continues only the newest kept
// capture of the job it follows, often not the row's.
//
// Read off the row alone: a reader reads no run record, and the log does not
// say what kind of capture landed a row. So [Session.InProgressNote] never says
// the session is saved as its run works, false of a harness or seat whose runs
// keep nothing while they work.
func (s Session) InProgress() bool { return !s.Ended() && s.HasObject() }

// InProgressNote is a row in progress ([Session.InProgress]) in a reader's
// words, "" of any other: the ONE SENTENCE OF IT, for every surface that shows
// a row in words. It holds only the engine's words and the row's own instant,
// so the held row and its shown form ([Session.Redacted]) say it alike and a
// surface never fences it, unlike [Session.WentOnNote], which is of an ended
// row and so never meets it.
func (s Session) InProgressNote() string {
	if !s.InProgress() {
		return ""
	}
	note := "This turn has not ended: this is the newest session its runs have kept"
	// No instant where the row holds none, as prompts.BuildNextTurn: the zero
	// instant read as a date would be a capture nobody took.
	if !s.CapturedAt.IsZero() {
		note += ", captured at " + s.CapturedAt.UTC().Format(time.RFC3339)
	}
	return note + "; its run may have gone on past it, and its last call may have no result yet."
}

// TrimmedNote is a row whose kept session was trimmed ([Session.Trimmed]) in a
// reader's words, "" of any other: the ONE SENTENCE OF IT, for every surface
// that shows a row in words. The engine's words alone, as
// [Session.InProgressNote] is, so a surface never fences it.
func (s Session) TrimmedNote() string {
	if !s.Trimmed || !s.HasObject() {
		return ""
	}
	return TrimmedSentence
}

// TrimmedSentence is what [Session.TrimmedNote] says.
const TrimmedSentence = "The history before this session's newest compaction was trimmed to keep it " +
	"within the bound a kept session is held to: its transcript begins there."

// NotContinuedNote is what the turn's runs did not continue, in the engine's
// sentence of each verdict its end names ([executorkeep.NotContinued.Sentence]):
// its first and its newest. "" where its runs lost nothing, or it has not
// ended. The engine's words alone: a verdict's detail is never in it.
func (s Session) NotContinuedNote() string {
	if s.Ending == nil {
		return ""
	}
	said := make([]string, 0, len(s.Ending.NotContinued))
	for _, n := range s.Ending.NotContinued {
		said = append(said, n.Sentence())
	}
	return strings.Join(said, " ")
}

// Kept is the row as continuing its turn's kept session reads it
// ([executorkeep.TurnRow]): the ONE conversion of a row into the sandbox's
// view of it, made by the engine's restore and the read surface alike, so a
// reader of whether a relaunch would continue the session decides on the
// relaunch's own fields.
//
// It carries the two facts [Session.WentOn] joins apart: the end went past the
// row's session ([Session.endWentPast]), of which the next turn is told where
// the session ends; and the turn was lost, of which it is told only that the
// session may not hold the last of what the turn did.
func (s Session) Kept() executorkeep.TurnRow {
	return executorkeep.TurnRow{
		Handle: s.Handle, Turn: s.Turn,
		Harness: s.Harness, HarnessVersion: s.HarnessVersion, Entry: s.Entry, SessionID: s.SessionID,
		ArchiveFormat: s.ArchiveFormat, LaunchSeq: s.LaunchSeq, CaptureSeq: s.CaptureSeq,
		Object: s.Object, Media: slices.Clone(s.Media), Ended: s.Ended(),
		ConversationKey: s.ConversationKey, NotKept: s.NotKept(),
		ContentKey: s.ContentKey, Home: s.Home, Workspace: s.Workspace,
		ContextTokens: s.ContextTokens, Interrupted: s.Interrupted, CapturedAt: s.CapturedAt,
		WentOn:  s.HasObject() && s.endWentPast(),
		Lost:    s.Ending != nil && s.Ending.Outcome == executorkeep.OutcomeLost,
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

// RelaunchRead is the level a relaunch reads its turn's row at, and so the
// level every reader of whether a relaunch would continue the turn's session
// decides on (internal/sandbox's ReadContinuityOf): the capture it continues
// may have been recorded by another node's keep, which this node's copy may
// not have applied yet.
func RelaunchRead() statelog.Freshness { return statelog.Freshness{Level: statelog.ReadLinearizable} }

// BindRead is the level a minted session's bind asks whether its id is any
// turn's at ([Reader.SessionHeld]): the row naming it may be another node's
// capture, which this node's copy may not have applied yet.
func BindRead() statelog.Freshness { return statelog.Freshness{Level: statelog.ReadLinearizable} }

// NotKept is why the row names no session's object, in a reader's words, ""
// where it names one. Told apart off the row alone, it is one of four: a launch
// spent the session ([Session.RetiredBy]); nothing was captured
// (yet, on a live row); the end named the newest capture unkept
// ([Session.Unkept]) and no older one was kept, or an end below [wentOnVersion]
// released it ([applyEnd]); or the seat's budget released it ([seatPlan]).
//
// Only the budget releases a session its end kept (an end with
// [executorkeep.Ending.WentOn] keeps it beside the unkept capture), so this
// asks the end's went_on, not [Session.endWentPast]: they part only where the
// row holds the last word, budget-released either way. The unkept reason
// quotes an error a box or a store wrote, so a reader redacts this.
func (s Session) NotKept() string {
	unkept := s.Unkept()
	keptByItsEnd := s.Ending != nil && s.Ending.WentOn && !s.CapturedAt.IsZero()
	switch {
	case s.HasObject():
		return ""
	case s.RetiredBy != "":
		return executorkeep.SpentWhy(s.SessionID, s.RetiredBy, s.RetiredLoss) + "; how the turn ended is kept"
	case unkept != nil && !keptByItsEnd:
		return fmt.Sprintf("the turn's newest capture of its session (launch %d, capture %d) was not kept: "+
			"%s; how the turn ended is kept", unkept.LaunchSeq, unkept.CaptureSeq, unkeptReason(*unkept))
	case s.CapturedAt.IsZero() && !s.Ended():
		return "nothing of this turn's session has been captured yet"
	case s.CapturedAt.IsZero():
		return "nothing of this turn's session was captured: its run ended before its harness packed one, " +
			"its harness carries none, or the company kept no session"
	}
	released := "the seat's session budget released this session's object to make room for newer ones"
	if unkept != nil {
		released += fmt.Sprintf(" — its turn had gone on past it, and its newer capture (launch %d, capture %d) "+
			"was not kept: %s", unkept.LaunchSeq, unkept.CaptureSeq, unkeptReason(*unkept))
	}
	return released + "; how the turn ended is kept"
}

// unkeptReason is why u was not kept, as a reader is told it: its reason, or
// that none was recorded.
func unkeptReason(u executorkeep.Unkept) string {
	if u.Reason == "" {
		return "no reason was recorded"
	}
	return u.Reason
}

// rowScanner is one row, from a query or a query row.
type rowScanner interface{ Scan(dest ...any) error }

// sessionColumns is every column a [Session] is read from, in [scanSession]'s
// order.
const sessionColumns = `agent_handle, turn_id, conversation_key, work_key, harness,
	harness_version, session_id, object, object_hash, object_size, content_key,
	archive_format, launch_seq, capture_seq, home, workspace, captured_at,
	read_through, absorbs, ended_at, end_seq, outcome, delivered, ending, ask_excerpt,
	base_turn, context_tokens, interrupted, retired_by, retired_loss, entry, trimmed`

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
		interrupted, trimmed  int
	)
	if err := row.Scan(&s.Handle, &s.Turn, &s.ConversationKey, &s.WorkKey, &s.Harness,
		&s.HarnessVersion, &s.SessionID, &object, &hash, &size, &s.ContentKey,
		&s.ArchiveFormat, &s.LaunchSeq, &s.CaptureSeq, &s.Home, &s.Workspace, &captured,
		&readThrough, &absorbs, &endedAt, &endSeq, &outcome, &delivered, &ending,
		&s.AskExcerpt, &s.BaseTurn, &s.ContextTokens, &interrupted, &s.RetiredBy, &s.RetiredLoss,
		&s.Entry, &trimmed); err != nil {
		return Session{}, err
	}
	s.Absorbs, s.Interrupted, s.Trimmed = splitAbsorbs(absorbs), interrupted == 1, trimmed == 1
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
		// The columns win over the ending's bytes: a dedupe merges the
		// outcome and the delivery into the row, not into the bytes.
		e.Outcome, e.Delivered = executorkeep.Outcome(outcome), delivered == 1
		s.Ending = &e
	}
	return s, nil
}

// mediaStatement is the media objects one turn's row names, by its key, in
// digest order.
const mediaStatement = `
	SELECT digest, object, object_hash, object_size, content_key
	FROM executor_session_media INDEXED BY ` + mediaKey + `
	WHERE agent_handle = ? AND turn_id = ? ORDER BY digest`

// withMedia fills [Session.Media] of every session that names an object, one
// point read of its turn's media each, in the caller's transaction: THE ONE
// LOADER every read of a row goes through, so no reader opens a session's
// frames without the media they splice in. A row naming no object names none.
func withMedia(ctx context.Context, tx *sql.Tx, sessions []Session) error {
	for i := range sessions {
		if !sessions[i].HasObject() {
			continue
		}
		media, err := mediaOf(ctx, tx, sessions[i].Handle, sessions[i].Turn)
		if err != nil {
			return err
		}
		sessions[i].Media = media
	}
	return nil
}

// mediaOf is the media objects one turn's row names.
func mediaOf(ctx context.Context, tx *sql.Tx, handle, turn string) ([]executorkeep.MediaObject, error) {
	rows, err := tx.QueryContext(ctx, mediaStatement, handle, turn)
	if err != nil {
		return nil, fmt.Errorf("executordomain: read %s's media of turn %s: %w", handle, turn, err)
	}
	var out []executorkeep.MediaObject
	for rows.Next() {
		var (
			m         executorkeep.MediaObject
			key, hash string
		)
		if err = rows.Scan(&m.Digest, &key, &hash, &m.Object.Size, &m.ContentKey); err == nil {
			m.Object, err = parsedObject(key, hash, m.Object.Size)
		}
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("executordomain: %s's turn %s names media %s: %w", handle, turn, m.Digest, err)
		}
		out = append(out, m)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// Head is what a conversation's next turn launches from: its head, the turn
// that read it last, never simply the newest end (package doc, "The head is
// the turn that read the conversation last"); and ALL its other ended turns,
// never only those that ended after the head, since each is one no ended turn
// was shown (package doc, "What a conversation still holds is what no ended
// turn was shown").
type Head struct {
	// Session is the conversation's HEAD, which the next turn continues; nil
	// where no ended turn names a session, and the launch starts fresh.
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
	// headStatement is a conversation's HEAD ([Head.Session]) among its own
	// rows, which [headOf] weighs beside the turns it reads as served; a turn
	// that read none sorts after every turn that did. An end's position is unique
	// among the rows holding one, so the pair is a total order, which the
	// line index serves without a sort.
	headStatement = `SELECT ` + sessionColumns + `
		FROM executor_sessions INDEXED BY ` + lineIdx + `
		WHERE agent_handle = ? AND conversation_key = ? AND end_seq IS NOT NULL
		  AND object IS NOT NULL
		ORDER BY read_through DESC, end_seq DESC LIMIT 1`

	// endingsStatement is a conversation's other ended turns of its own, the
	// newest first, and endingsCountStatement how many there are. An end's position
	// is unique, so it alone orders them, served by the conversation's index.
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

// THE TURNS A CONVERSATION READS AS SERVED (package doc, "A turn serves every
// conversation its run read"): each a turn's one row, reached from the served
// rows in the order their ends applied, which the line index serves without a
// sort. A served row is placed at its end, so that one position orders it both
// as a head and as an ending.
var (
	servedSelect = `SELECT ` + servedColumns() + `
		FROM executor_session_serves AS v INDEXED BY ` + servesLineIdx + `
		CROSS JOIN executor_sessions AS s INDEXED BY ` + sessionKey + `
		WHERE v.agent_handle = ? AND v.conversation_key = ?
		  AND s.agent_handle = v.agent_handle AND s.turn_id = v.turn_id AND s.end_seq IS NOT NULL`
	servedCount = `SELECT COUNT(*)
		FROM executor_session_serves AS v INDEXED BY ` + servesLineIdx + `
		CROSS JOIN executor_sessions AS s INDEXED BY ` + sessionKey + `
		WHERE v.agent_handle = ? AND v.conversation_key = ?
		  AND s.agent_handle = v.agent_handle AND s.turn_id = v.turn_id AND s.end_seq IS NOT NULL`

	// servedHeadStatement is the newest of them that names a session.
	servedHeadStatement = servedSelect + ` AND s.object IS NOT NULL
		ORDER BY v.end_seq DESC LIMIT 1`

	// servedEndingsStatement is the others but the head, the newest first,
	// and servedEndingsCountStatement how many; servedSiblingsCountStatement
	// how many of those name a session.
	servedEndingsStatement = servedSelect + ` AND v.turn_id <> ?
		ORDER BY v.end_seq DESC LIMIT ?`
	servedEndingsCountStatement  = servedCount + ` AND v.turn_id <> ?`
	servedSiblingsCountStatement = servedCount + ` AND s.object IS NOT NULL AND v.turn_id <> ?`
)

// servedColumns is [sessionColumns] named under the served reads' alias for
// the turn's row, `s`.
func servedColumns() string {
	fields := strings.Split(sessionColumns, ",")
	for i, f := range fields {
		fields[i] = "s." + strings.TrimSpace(f)
	}
	return strings.Join(fields, ", ")
}

// headOf is a conversation's head: of its own ended turns and those it reads
// as served that name a session, the one that read it last. THE ONE RULE OF
// THE LINE, as [headStatement] orders a conversation's own rows: the newest
// read, then the newest end. A served turn read the conversation where its end
// applied, so it outranks every turn of the conversation that read before that,
// and one that read at or after it outranks it by its later end.
func headOf(ctx context.Context, tx *sql.Tx, handle, conversationKey string) (*Session, error) {
	own, err := oneSession(ctx, tx, headStatement, handle, conversationKey)
	if err != nil {
		return nil, err
	}
	srv, err := oneSession(ctx, tx, servedHeadStatement, handle, conversationKey)
	switch {
	case err != nil:
		return nil, err
	case srv == nil:
		return own, nil
	case own == nil:
		return srv, nil
	}
	at := srv.EndedAtPosition.Packed()
	if read := own.ReadThrough.Packed(); read > at || (read == at && own.EndedAtPosition.Packed() > at) {
		return own, nil
	}
	return srv, nil
}

// endingsOf is a conversation's ended turns but the head, its own and those it
// reads as served, the newest end first, at most [MaxEndings], and how many
// there are.
func endingsOf(ctx context.Context, tx *sql.Tx, handle, conversationKey, head string) ([]Session, int, error) {
	own, ownTotal, err := boundedSessions(ctx, tx, endingsStatement, endingsCountStatement, MaxEndings,
		handle, conversationKey, head)
	if err != nil {
		return nil, 0, err
	}
	srv, srvTotal, err := boundedSessions(ctx, tx, servedEndingsStatement, servedEndingsCountStatement,
		MaxEndings, handle, conversationKey, head)
	if err != nil {
		return nil, 0, err
	}
	all := slices.Concat(own, srv)
	slices.SortFunc(all, func(a, b Session) int {
		return cmp.Compare(b.EndedAtPosition.Packed(), a.EndedAtPosition.Packed())
	})
	return all[:min(len(all), MaxEndings)], ownTotal + srvTotal, nil
}

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
			head, headErr := headOf(ctx, tx, handle, conversationKey)
			if headErr != nil {
				return headErr
			}
			// NO TURN HAS AN EMPTY ID, so with no head nothing is left out.
			var headTurn string
			if head != nil {
				one := []Session{*head}
				if headErr = withMedia(ctx, tx, one); headErr != nil {
					return headErr
				}
				out.Session, headTurn = &one[0], head.Turn
			}
			endings, total, endingsErr := endingsOf(ctx, tx, handle, conversationKey, headTurn)
			if endingsErr == nil {
				endingsErr = withMedia(ctx, tx, endings)
			}
			if endingsErr != nil {
				return endingsErr
			}
			// IN THE ORDER THEY ENDED, which is how the next turn is told
			// them: the query takes the newest, and they read oldest first.
			slices.Reverse(endings)
			out.Endings, out.EndingsOmitted = endings, total-len(endings)
			var withSessions, servedSessions int
			if countErr := tx.QueryRowContext(ctx, siblingsCountStatement,
				handle, conversationKey, headTurn).Scan(&withSessions); countErr != nil {
				return countErr
			}
			if countErr := tx.QueryRowContext(ctx, servedSiblingsCountStatement,
				handle, conversationKey, headTurn).Scan(&servedSessions); countErr != nil {
				return countErr
			}
			withSessions += servedSessions
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

	// Head is the head turn of the row's conversation as the same read saw
	// it, empty where there is none: decided in the row's snapshot, never by
	// a second read.
	Head string

	// Position is the checkpoint the row was read at, and Complete the
	// read's own word on whether an undecodable record may be missing (never
	// for a point read the ladder served: such a record refuses it), carried
	// as reported so the two cannot disagree.
	Position statelog.Position
	Complete bool
}

// Session is one turn's row, at the level fresh asks for, and where it was
// read: what a relaunch of the turn reads to continue its own session, and a
// person's view of one session. A turn with no row is [ErrNotFound].
//
// Its scope is read off the row, in the answer's own transaction, since only
// the row names the turn's conversation; a turn with no row here reads over its
// whole seat, since an undecodable capture is exactly a row this node lacks.
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
		one := []Session{*out}
		if readErr = withMedia(ctx, tx, one); readErr != nil {
			return readErr
		}
		out = &one[0]
		heads, readErr := headsOf(ctx, tx, handle, one)
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
// the conversation's key: the turn [headOf] answers ([Reader.Head]'s own), or
// "" where the conversation has none. A row with no
// conversation names none, and is not keyed.
//
// One seek per conversation, in the caller's transaction: never a head chosen
// among the rows in hand, since a conversation's head may be on no page.
func headsOf(ctx context.Context, tx *sql.Tx, handle string, sessions []Session) (map[string]string, error) {
	var out map[string]string
	for _, s := range sessions {
		if s.ConversationKey == "" {
			continue
		}
		if _, read := out[s.ConversationKey]; read {
			continue
		}
		head, err := headOf(ctx, tx, handle, s.ConversationKey)
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
// as a read surface and a seat's own search list them. A page holds its rows to
// the bytes the read carries ([statelog.RowsOf]), so it may end short of limit
// with a cursor: a caller pages on Next, never on a page's length.
//
// Paged by where each row began, which nothing moves: every other position on a
// row moves with a capture, its end or a release, and a cursor over one would
// skip or repeat a row that moved between two pages.
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
		var (
			read  []Session
			begun []int64
		)
		for rows.Next() {
			var first int64
			s, scanErr := scanSession(trailing{rows, &first})
			if scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			read, begun = append(read, s), append(begun, first)
		}
		if closeErr := errors.Join(rows.Err(), rows.Close()); closeErr != nil {
			return closeErr
		}
		// HELD TO ITS BYTES WITH ITS MEDIA, which are read once the page's
		// rows are closed: a row is what the answer carries of it.
		if mediaErr := withMedia(ctx, tx, read); mediaErr != nil {
			return mediaErr
		}
		held := statelog.RowsOf(ctx)
		for _, s := range read {
			if !held.TakeEncoded(s) {
				break
			}
			out.Sessions = append(out.Sessions, s)
		}
		// A PAGE CUT SHORT BY ITS BYTES goes on from its last kept row, as a
		// full one does. Heads beside them are one entry per conversation of
		// at most MaxPageSize rows, each key a row already carries: small.
		switch {
		case len(out.Sessions) > limit:
			out.Sessions = out.Sessions[:limit]
			out.Next = strconv.FormatInt(begun[limit-1], 10)
		case held.Cut():
			out.Next = strconv.FormatInt(begun[len(out.Sessions)-1], 10)
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
// and removed seats whose removal is. The retention duty's listing, it reads
// at `stale`: it only nominates, and every delete is decided again in its own
// snapshot and at the apply.
//
// A row a live turn holds as its base is left out, however old: its delete
// would be refused ([expiredIn]), and listed it would fill the duty's page on
// every pass while it holds. The holds are read in the same transaction and the
// rows over-fetched by their count, so a page still holds limit of the rest.
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

// sessionHeldStatement is whether any turn's row names a session id, by the
// index over the id ([sessionIdx]).
const sessionHeldStatement = `
	SELECT 1 FROM executor_sessions INDEXED BY ` + sessionIdx + ` WHERE session_id = ? LIMIT 1`

// SessionHeld reports whether any turn's row, of any seat, names the session
// id, at the level fresh asks for: a session a harness mints is bound only
// while it is fresh, and one a row names is another turn's, whose capture and
// carry it would take over. One seek, whatever the table holds.
//
// An answer that may be missing a row is [ErrIncomplete], never false: an
// undecodable record may be exactly the row naming the id.
func (r *Reader) SessionHeld(ctx context.Context, sessionID string, fresh statelog.Freshness) (bool, error) {
	if sessionID == "" {
		return false, invalid("a session id is needed to ask whether a turn holds it")
	}
	held := false
	answer, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		held = false
		var one int
		err := tx.QueryRowContext(ctx, sessionHeldStatement, sessionID).Scan(&one)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		held = true
		return nil
	})
	switch {
	case err != nil:
		return false, fmt.Errorf("executordomain: whether a turn holds session %q: %w", sessionID, err)
	case !held && !answer.Complete:
		return false, fmt.Errorf("%w: whether a turn holds session %q", ErrIncomplete, sessionID)
	}
	return held, nil
}

// handlesStatement is every seat holding a row, in order.
const handlesStatement = `
	SELECT DISTINCT agent_handle FROM executor_sessions ORDER BY agent_handle`

// Handles is every seat that holds a row here, in order, at the level fresh
// asks for: what a seat's retirement matches the departed seat against when
// its record has lost the seat's handle.
//
// An answer that may be missing a seat is [ErrIncomplete], never a shorter
// list: an undecodable record may hold exactly the departed seat's rows, and a
// retirement matched against the rest would remove nothing and say it was done.
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

// handlesPageStatement is every seat holding a row after a handle, in order.
const handlesPageStatement = `
	SELECT DISTINCT agent_handle FROM executor_sessions WHERE agent_handle > ? ORDER BY agent_handle`

// HandlesPage is one page of the seats holding a row: the handles after the
// page before's Next, in order, and Next where more follow.
type HandlesPage struct {
	Handles []string
	Next    string
}

// HandlesPage is [Reader.Handles] a page at a time, after `after` (empty for
// the first), as a node holding no data reads it: one answer each, held to the
// bytes the read carries ([statelog.RowsOf]); its caller pages on Next.
func (r *Reader) HandlesPage(ctx context.Context, after string, fresh statelog.Freshness) (HandlesPage, error) {
	var out HandlesPage
	answer, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = HandlesPage{}
		rows, queryErr := tx.QueryContext(ctx, handlesPageStatement, after)
		if queryErr != nil {
			return queryErr
		}
		held := statelog.RowsOf(ctx)
		for rows.Next() {
			var handle string
			if scanErr := rows.Scan(&handle); scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			if !held.TakeEncoded(handle) {
				out.Next = out.Handles[len(out.Handles)-1]
				break
			}
			out.Handles = append(out.Handles, handle)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	switch {
	case err != nil:
		return HandlesPage{}, fmt.Errorf("executordomain: list the seats holding sessions: %w", err)
	case !answer.Complete:
		return HandlesPage{}, fmt.Errorf("%w: list the seats holding sessions", ErrIncomplete)
	}
	return out, nil
}

// SealedCount is how many session and media objects sit sealed under each
// keyring key: what a keyring rotation waits out before it drops an old key.
//
// Waited out, never moved: an object is sealed once, by the capture that
// uploaded it, and leaves only with its row. So a rotation keeps the old key
// until its count reaches zero; dropped sooner, those sessions read as
// [ErrUnreadable] and their next turns start fresh.
type SealedCount struct {
	Values secrets.KeyCounts `json:"values,omitempty"`
}

// Outside is how many of the sessions are sealed under a key other than
// active.
func (c SealedCount) Outside(active string) int { return c.Values.Outside(active) }

// sealedKeysStatement counts the named objects by the key that sealed them,
// and sealedMediaKeysStatement the media rows likewise: a media object two
// rows name counts twice, which waits out the same key.
const (
	sealedKeysStatement = `
	SELECT content_key, COUNT(*) FROM executor_sessions
	WHERE object IS NOT NULL AND length(content_key) > 0 GROUP BY content_key`
	sealedMediaKeysStatement = `
	SELECT content_key, COUNT(*) FROM executor_session_media
	WHERE length(content_key) > 0 GROUP BY content_key`
)

// SealedKeys counts every session object and media object this node's rows
// name, by the keyring key its row says it is sealed under, fetching none.
func (r *Reader) SealedKeys(ctx context.Context) (SealedCount, error) {
	out := SealedCount{Values: secrets.KeyCounts{}}
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		clear(out.Values)
		for _, statement := range []string{sealedKeysStatement, sealedMediaKeysStatement} {
			rows, queryErr := tx.QueryContext(ctx, statement)
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
			if closeErr := errors.Join(rows.Err(), rows.Close()); closeErr != nil {
				return closeErr
			}
		}
		return nil
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
