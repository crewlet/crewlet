package queries

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SEAT'S EXECUTOR SESSIONS, read by a person (ADR-0073).
//
// Every turn whose executor captured its harness's session leaves a row in the
// executor log naming the sealed object that holds the session its harness
// kept when its run ended — the whole record of what the run read, called and
// wrote — and how the turn ended. Two questions read it: `executor_sessions`,
// a seat's rows newest first, a page at a time, each with how its turn ended
// and whether its session is still kept; and `executor_session`, one turn's
// transcript, its object opened and read as the entries its harness recorded.
//
// # The seat's trail, decided as the run's live output is
//
// Both questions are decided per seat by [authz.ActionSeatTrailRead] — the
// seat itself, whoever leads it, or an admin — through [Sources.trailSeat], the
// one place every question about a seat's trail decides it: exactly who reads
// the run's live output (`executor_tail`) and its turn's phases with their
// prompts and responses. They reach nobody those do not.
//
// WHAT a transcript hands those readers is MORE than the live output did: the
// live output shows a call's subject on one line and a failed result's first
// line, and never a successful result's body; a transcript shows every call's
// whole input and each result's text — the contents of the files the run read
// and what its commands printed — to an entry's bound, and each subagent's
// thread. The redaction at this read matches the credential shapes it knows,
// not values: a secret of no recognisable shape that a run read is in its
// transcript as it read it.
//
// # From any node, and never a 500 for an object that is gone
//
// The rows are read through the estate router ([ExecutorSessions]), so a node
// without `data` answers as a data node does, and the object through the
// fleet's one object store ([SessionObjects]). An object the store no longer
// holds, one that does not open under this fleet's keyring, an archive that is
// not one its format describes, and one this build does not read are each an
// ANSWER saying so — the row is still the turn's account of how it ended —
// never a failure: only a store that did not answer is `unavailable`. What the
// answer says of an archive that is not what its format describes is the
// engine's own sentence ([executor.MalformedArchiveError]), never a decoder's,
// which would quote what a box's own agent packed.
//
// # Bounded, and redacted at this read
//
// A transcript is read a page at a time, held to the bounds the runner states
// ([executor.TranscriptReadBounds]) — entries and bytes per page, each entry's
// text, and the archive's bytes one read takes — and the answer says why its
// page ends where it does. Every entry's text, the ask's excerpt and the
// ending's prose are passed through [redact.Secrets] here, whatever the capture
// did: a session is everything a run read and printed.
//
// And a node decodes at most [executor.TranscriptReadsAtOnce] transcripts at
// once, whichever socket or REST caller asks, since one read can hold on the
// order of a hundred mebibytes: a read past them waits for a slot, its object
// not yet opened, for as long as its caller waits, and ends with the caller's
// own error where the caller leaves first.
//
// # Whether a relaunch continues it, decided as a relaunch decides it
//
// Each row says whether a relaunch of its turn would continue its session
// (`continuable`), where (`continues_in`: from the turn's kept capture in
// whichever box the relaunch gets, or only in the box the turn's run parked
// in, while that box is held), and where it would not, why
// (`not_continuable`): asked of [sandbox.ReadContinuityOf] — THE decision a
// relaunch takes, [sandbox.ContinuityOf], and beside it the launch's own test
// of a box it continues in — over what a relaunch reads: the turn's row on the
// executor log at the level a relaunch reads it ([sandbox.RelaunchRead]), the
// turn's run record in the coordination store ([SessionRuns]) and the job that
// record names as the one a relaunch would follow, and the member and the
// runner a relaunch of the seat's turn would be launched on, by the engine's
// one pick of them ([RelaunchTargets]) — and never a second copy of any part
// of it. As far as the engine can know before a box is chosen: whether the box
// a relaunch gets runs a harness release that takes the session up, and
// whether the seat's budget can carry it, are the launch's to find out, and no
// row says them. An ended turn's session is never continuable here: a
// conversation's next turn continues no session yet.
//
// A PAGE READS THE COORDINATION STORE ONCE for every run it lists: one listing
// of the seat's live runs ([SessionRuns.ListActiveForSeat]), which the fleet's
// concurrent runs bound, none at all where every row on the page has ended,
// which the log's row alone decides, and one turn's record alone for a live
// row the listing does not hold — a record of a status this build does not
// know, or one it could not decode — which a session read alone decides the
// same way. And A LIVE ROW IS DECIDED ON THE LOG AS A RELAUNCH READS IT: read
// again at the relaunch's level where the page was read below it, since a copy
// of the log that has not applied the capture the run's record keeps would read
// a session a relaunch continues as one the log no longer names.
//
// AN UNKNOWN IS NEVER A NO: `continuable` is null, and `not_continuable` says
// why, where the turn's run record or its row at the relaunch's level could not
// be read, and where a relaunch would not decide now either — its job still
// working or being collected, its capture still being kept, every member of
// the seat's chain sitting out its plan. A no would tell a person a session
// will start over that may well be continued; and a store that does not
// answer never fails the page, whose rows the log answered whole.

// ExecutorSessions is the seats' executor sessions as this surface reads them:
// the two reads of the domain's reader a person asks, answered through the
// estate router so every node answers (estate.ExecutorReads).
//
// Consumer-defined like every other seam here, and over the domain's own row
// type for the reason [SeatMemory] is over memread's: the row stays one shape.
type ExecutorSessions interface {
	Sessions(ctx context.Context, handle, cursor string, limit int,
		fresh statelog.Freshness) (executordomain.Page, error)
	Session(ctx context.Context, handle, turn string,
		fresh statelog.Freshness) (executordomain.SessionRead, error)
}

// SessionObjects opens a session's sealed object, streaming, its associated
// data binding it to the seat, the turn and the object's key
// (executordomain.SessionStore): what the transcript is read from.
type SessionObjects interface {
	Open(ctx context.Context, session executordomain.Session) (io.ReadCloser, error)
}

// SessionRuns is the turns' run records in the fleet's coordination store, as
// whether a session can be continued is decided off them
// ([sandbox.ReadContinuityOf]): one seat's live runs for a page of its
// sessions, in one read however many rows the page holds, and one turn's for a
// session read alone — or for a live row of a page the listing does not hold.
// The store a relaunch reads its own turn's record from (sandbox.CoordStore),
// so the two decide on one record.
type SessionRuns interface {
	ListActiveForSeat(ctx context.Context, handle string) ([]sandbox.PendingRun, error)
	Get(ctx context.Context, turnID string) (sandbox.PendingRun, bool, error)
}

// RelaunchTargets is what a relaunch of a seat's turn would be launched on, as
// the company this node runs picks it ([sandbox.RelaunchTarget]): the member of
// the seat's executor chain the relaunch takes, the cell its run lands in and
// the runner it takes a carried session up with — the ENGINE'S ONE PICK, which
// the relaunch's own probe asks too, so a row is decided by the runner a
// relaunch would take, never by the harness that wrote the session.
type RelaunchTargets interface {
	RelaunchTarget(handle string) sandbox.RelaunchTarget
}

// SessionRow is one turn's executor session as this surface shows it.
type SessionRow struct {
	Turn            string `json:"turn_id"`
	ConversationKey string `json:"conversation_key"`
	WorkKey         string `json:"work_key"`

	// Harness is the harness that wrote the session, and HarnessVersion its
	// release as the box's harness printed it — the box's words.
	Harness        string `json:"harness"`
	HarnessVersion string `json:"harness_version" untrusted:"name"`

	// CapturedAt is when the turn's newest capture was recorded, null where
	// nothing was captured; EndedAt when the turn's end was, null while the
	// turn runs.
	CapturedAt *time.Time `json:"captured_at"`
	EndedAt    *time.Time `json:"ended_at"`

	// Ending is the engine's account of how the turn ended — its outcome,
	// the reviewer's decision, whether a reply reached anybody, who stopped
	// it, what failed — its model- and person-written fields declared
	// somebody's words on the type itself; null while the turn runs.
	Ending *executordomain.Ending `json:"ending"`

	// AskExcerpt is the opening of what the turn was asked, redacted.
	AskExcerpt string `json:"ask_excerpt" untrusted:"fence"`

	// Kept says the row still names a session's object, and ObjectBytes is
	// that object's sealed size: zero where the turn captured nothing, or
	// the seat's budget released it.
	Kept        bool  `json:"kept"`
	ObjectBytes int64 `json:"object_bytes"`

	// Continuable says whether a relaunch of the turn would continue this
	// session, as far as the engine can know before a box is chosen, decided
	// as a relaunch decides it ([sandbox.ReadContinuityOf]), and ContinuesIn
	// where it would: from the turn's kept capture in whichever box the
	// relaunch gets, or only in the box the turn's run parked in, while that
	// box is held. NotContinuable, in the engine's own words, redacted, is why
	// it would not; or, where it would only in the box the run parked in, why
	// no other box continues it. NULL where it cannot be decided now, which
	// NotContinuable then says: an unknown is never sent as a no. See the file
	// comment.
	Continuable    *bool               `json:"continuable"`
	ContinuesIn    sandbox.ContinuesIn `json:"continues_in,omitempty"`
	NotContinuable string              `json:"not_continuable,omitempty"`
}

// SessionsAnswer is one page of a seat's executor sessions, the latest begun
// first.
type SessionsAnswer struct {
	Handle   string       `json:"handle"`
	Sessions []SessionRow `json:"sessions"`

	// Next is the cursor of the page after this one, absent on the last: the
	// key every cursor-paged answer of a state-log domain names it by.
	Next string `json:"next_cursor,omitempty"`

	// ReadLevel is the level the page was read at, Position where, and
	// Complete false where a record of the log the answering copy could not
	// decode may hold rows missing from it.
	ReadLevel statelog.ReadLevel `json:"read_level"`
	Position  statelog.Position  `json:"position"`
	Complete  bool               `json:"complete"`
}

// TranscriptState is what reading a session's object came to.
type TranscriptState string

const (
	// TranscriptRead is a session read: the answer holds its entries.
	TranscriptRead TranscriptState = "read"

	// TranscriptNone is a row naming no object: its turn captured nothing,
	// or the seat's session budget released it.
	TranscriptNone TranscriptState = "none"

	// TranscriptMissing is an object the store no longer holds.
	TranscriptMissing TranscriptState = "missing"

	// TranscriptUnreadable is an object that does not open: sealed under a
	// key the keyring no longer holds, or bytes that are not the ones sealed
	// for this turn.
	TranscriptUnreadable TranscriptState = "unreadable"

	// TranscriptMalformed is an object that opened and is not an archive its
	// format describes.
	TranscriptMalformed TranscriptState = "malformed"

	// TranscriptUnsupported is an archive of a harness or a format this build
	// does not read.
	TranscriptUnsupported TranscriptState = "unsupported"
)

// TranscriptStates are every state a transcript's read can come to.
var TranscriptStates = []TranscriptState{
	TranscriptRead, TranscriptNone, TranscriptMissing, TranscriptUnreadable,
	TranscriptMalformed, TranscriptUnsupported,
}

// Valid reports whether a state off the wire is one this build answers.
func (s TranscriptState) Valid() bool { return slices.Contains(TranscriptStates, s) }

// SessionAnswer is one turn's executor session and a page of its transcript.
type SessionAnswer struct {
	Handle  string     `json:"handle"`
	Session SessionRow `json:"session"`

	// State is what reading the session's object came to, and Why says it in
	// the engine's own words wherever it is not `read`.
	State TranscriptState `json:"state"`
	Why   string          `json:"why,omitempty"`

	// Entries are the transcript's entries from From on, in the order the
	// harness recorded them; empty wherever State is not `read`.
	Entries []executor.TranscriptEntry `json:"entries"`
	From    int                        `json:"from"`

	// Cut is why Entries ends before the transcript does, empty where it
	// does not; Next is the cursor a read of the entries after them takes,
	// absent where none follow.
	Cut  executor.TranscriptCut `json:"cut,omitempty"`
	Next string                 `json:"next_cursor,omitempty"`

	// Bounds are what every read of a transcript is held to.
	Bounds executor.TranscriptBounds `json:"bounds"`

	// ReadLevel is the level the row was read at, Position where, and
	// Complete the read's own word that no record the answering copy could
	// not decode is missing from it — as every answer of a state-log domain
	// says beside its rows.
	ReadLevel statelog.ReadLevel `json:"read_level"`
	Position  statelog.Position  `json:"position"`
	Complete  bool               `json:"complete"`
}

// executorSessions answers a page of a seat's executor sessions, the latest
// begun first, after `cursor`, `limit` to a page (the domain's default and
// ceiling: executordomain.DefaultPageSize and MaxPageSize), at the caller's
// freshness.
func (s Sources) executorSessions(ctx context.Context, p Params) (any, error) {
	// A SEAT'S TRAIL, decided before anything is read — see
	// [Sources.trailSeat]. An absent handle is the caller's own seat.
	seat, err := s.trailSeat(ctx, strings.TrimSpace(p.String("handle")))
	if err != nil {
		return nil, err
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	page, err := s.ExecutorSessions.Sessions(ctx, seat.Handle, strings.TrimSpace(p.String("cursor")),
		p.Int("limit", 0), fresh)
	if err != nil {
		return nil, executorRefusal(err)
	}
	// ONE READ OF THE COORDINATION STORE FOR THE PAGE, and one MEMBER A
	// RELAUNCH OF THE SEAT'S TURNS IS LAUNCHED ON — see the file comment.
	runs, unread := s.liveRuns(ctx, seat.Handle, page.Sessions)
	target := s.relaunchTarget(seat.Handle)
	rows := make([]SessionRow, 0, len(page.Sessions))
	for _, row := range page.Sessions {
		shown := sessionRowOf(row)
		run, runErr := runs[row.Turn], unread
		if run == nil && runErr == nil {
			// A LIVE ROW THE LISTING DOES NOT HOLD is read alone, as a
			// session read alone reads it: the listing passes over a record
			// of a status this build does not know and one it could not
			// decode, and neither is a run that is over.
			run, runErr = s.liveRun(ctx, row)
		}
		shown.Continuable, shown.ContinuesIn, shown.NotContinuable = s.continuity(ctx, row, run, runErr,
			target, fresh)
		rows = append(rows, shown)
	}
	return SessionsAnswer{Handle: seat.Handle, Sessions: rows, Next: page.Next,
		ReadLevel: fresh.Level, Position: page.Position, Complete: page.Complete}, nil
}

// errNoRunRecords is a node wired with no run records to read
// ([Sources.SessionRuns]): every live turn's row is then undecided.
var errNoRunRecords = errors.New("this node reads no run records")

// undecidedContinuity is what a live turn's row says where its run record
// could not be read: whether a relaunch continues its session is unknown, and
// a person is told so rather than told it does not.
const undecidedContinuity = "the turn's run record could not be read; ask again shortly"

// unreadRelaunchRow is what a live turn's row says where its row on the
// executor log could not be read at the level a relaunch reads it.
const unreadRelaunchRow = "the turn's row on the executor log could not be read as a relaunch reads it; " +
	"ask again shortly"

// relaunchTarget is what a relaunch of handle's turn would be launched on, or,
// on a node wired with nothing to pick one by ([Sources.RelaunchTargets]), a
// target a relaunch could not be decided for: every live row of the seat is
// then undecided, as one whose run record could not be read is.
func (s Sources) relaunchTarget(handle string) sandbox.RelaunchTarget {
	if s.RelaunchTargets == nil {
		return sandbox.RelaunchTarget{Waiting: true, Why: "this node picks no member a relaunch is launched " +
			"on; ask a node that runs the company"}
	}
	return s.RelaunchTargets.RelaunchTarget(handle)
}

// liveRuns is the run record of every live turn among rows the listing holds,
// by turn, read in ONE listing of handle's live runs
// ([SessionRuns.ListActiveForSeat]) — and nothing read where every row has
// ended, which the log's row alone decides — or why they could not be read. A
// live turn missing from the map is read alone ([Sources.liveRun]): the
// listing holds only the statuses this build knows active, and only the
// records it could decode.
func (s Sources) liveRuns(ctx context.Context, handle string,
	rows []executordomain.Session) (map[string]*sandbox.PendingRun, error) {

	if !slices.ContainsFunc(rows, func(row executordomain.Session) bool { return !row.Ended() }) {
		return nil, nil
	}
	if s.SessionRuns == nil {
		return nil, errNoRunRecords
	}
	listed, err := s.SessionRuns.ListActiveForSeat(ctx, handle)
	if err != nil {
		log.WarnContext(ctx, "executor_session_runs_unreadable", "handle", handle, "error", err,
			"detail", "whether a relaunch continues each live session on the page cannot be said now")
		return nil, err
	}
	runs := make(map[string]*sandbox.PendingRun, len(listed))
	for i := range listed {
		runs[listed[i].TurnID] = &listed[i]
	}
	return runs, nil
}

// liveRun is the run record of row's turn: nil where its run is over, and
// nothing read where it has ended, which the log's row alone decides — or why
// it could not be read.
func (s Sources) liveRun(ctx context.Context, row executordomain.Session) (*sandbox.PendingRun, error) {
	if row.Ended() {
		return nil, nil
	}
	if s.SessionRuns == nil {
		return nil, errNoRunRecords
	}
	run, found, err := s.SessionRuns.Get(ctx, row.Turn)
	switch {
	case err != nil:
		log.WarnContext(ctx, "executor_session_runs_unreadable", "handle", row.Handle, "turn_id", row.Turn,
			"error", err, "detail", "whether a relaunch continues the session cannot be said now")
		return nil, err
	case !found:
		return nil, nil
	}
	return &run, nil
}

// continuity is a row's `continuable`, `continues_in` and `not_continuable`:
// THE DECISION A RELAUNCH TAKES ([sandbox.ReadContinuityOf]), asked of the
// turn's row on the log as a relaunch reads it ([Sources.relaunchRow]), its run
// record — nil where its run is over — and target, the member a relaunch of the
// seat's turn would be launched on; or undecided, where the turn is live and
// its record could not be read (unread), since the decision would read a
// missing record as a run that is over, and where its row could not be read as
// a relaunch reads it.
func (s Sources) continuity(ctx context.Context, row executordomain.Session, run *sandbox.PendingRun,
	unread error, target sandbox.RelaunchTarget, fresh statelog.Freshness) (*bool, sandbox.ContinuesIn, string) {

	if unread != nil && !row.Ended() {
		return nil, "", undecidedContinuity
	}
	decideOn, err := s.relaunchRow(ctx, row, run, fresh)
	if err != nil {
		log.WarnContext(ctx, "executor_session_row_unreadable", "handle", row.Handle, "turn_id", row.Turn,
			"error", err, "detail", "whether a relaunch continues the session cannot be said now")
		return nil, "", unreadRelaunchRow
	}
	decided := sandbox.ReadContinuityOf(decideOn, run, target, s.clock())
	// REDACTED, as every text of this surface is: a reason can quote a
	// harness's release as its box printed it.
	why := redact.Secrets(decided.Why)
	if !decided.Decided {
		return nil, "", why
	}
	return &decided.Continues, decided.In, why
}

// relaunchRow is the row a live turn's continuity is decided on: the turn's
// row on the executor log read again at the level a relaunch reads it
// ([sandbox.RelaunchRead]) where the caller's read was below it — a copy of the
// log that has not applied the capture the run's record keeps would read a
// session a relaunch continues as one the log no longer names — and row itself
// where it was read at that level, or where its turn has ended or its run is
// over, which the log's row and the record decide at any level.
func (s Sources) relaunchRow(ctx context.Context, row executordomain.Session, run *sandbox.PendingRun,
	fresh statelog.Freshness) (executordomain.Session, error) {

	through := sandbox.RelaunchRead()
	if run == nil || row.Ended() || fresh.Level == through.Level {
		return row, nil
	}
	read, err := s.ExecutorSessions.Session(ctx, row.Handle, row.Turn, through)
	if err != nil {
		return executordomain.Session{}, err
	}
	return read.Session, nil
}

// executorSession answers one turn's session — the turn `turn_id`, as every
// question about a turn names it — and a page of its transcript from `cursor`
// on, the first page where it is absent.
func (s Sources) executorSession(ctx context.Context, p Params) (any, error) {
	seat, err := s.trailSeat(ctx, strings.TrimSpace(p.String("handle")))
	if err != nil {
		return nil, err
	}
	turn := strings.TrimSpace(p.String("turn_id"))
	if turn == "" {
		return nil, badParams("turn_id", "", nil)
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	read, err := s.ExecutorSessions.Session(ctx, seat.Handle, turn, fresh)
	switch {
	case errors.Is(err, executordomain.ErrNotFound):
		return nil, fmt.Errorf("%w: %s keeps no executor session of the turn %s", ErrNotFound,
			seat.Handle, turn)
	case err != nil:
		return nil, executorRefusal(err)
	}
	row := read.Session
	from, err := transcriptFrom(strings.TrimSpace(p.String("cursor")), row)
	if err != nil {
		return nil, err
	}
	shown := sessionRowOf(row)
	run, unread := s.liveRun(ctx, row)
	shown.Continuable, shown.ContinuesIn, shown.NotContinuable = s.continuity(ctx, row, run, unread,
		s.relaunchTarget(seat.Handle), fresh)
	answer, err := s.readTranscript(ctx, row, SessionAnswer{
		Handle: seat.Handle, Session: shown, Entries: []executor.TranscriptEntry{},
		From: from, Bounds: executor.TranscriptReadBounds(),
		ReadLevel: fresh.Level, Position: read.Position, Complete: read.Complete,
	})
	if err != nil {
		return nil, err
	}
	return answer, nil
}

// readTranscript reads a page of row's transcript into out: its object opened
// and read through the runner's own format knowledge, or what kept it from
// being read.
func (s Sources) readTranscript(ctx context.Context, row executordomain.Session,
	out SessionAnswer) (SessionAnswer, error) {

	if !row.HasObject() {
		out.State, out.Why = TranscriptNone, notKept(row)
		return out, nil
	}
	if ok, why := executor.Carries(row.Harness, row.ArchiveFormat); !ok {
		out.State, out.Why = TranscriptUnsupported, why
		return out, nil
	}
	// THE READ OPENS THE OBJECT, once it holds one of the node's transcript
	// slots ([executor.TranscriptReadsAtOnce]), and closes it before it gives
	// the slot back: an object opened here would hold its store's connection
	// for as long as the read waited for one.
	open := func(ctx context.Context) (io.ReadCloser, error) { return s.SessionObjects.Open(ctx, row) }
	page, err := executor.ReadTranscript(ctx, row.Harness, row.ArchiveFormat, open, out.From)
	if err != nil {
		return transcriptUnread(out, row, err)
	}
	out.State, out.Entries, out.Cut = TranscriptRead, page.Entries, page.Cut
	if page.More() {
		out.Next = transcriptCursor(row.Object.Key, page.Next)
	}
	return out, nil
}

// transcriptUnread is what kept a session's object from being read, as the
// answer says it: gone, sealed out of reach, malformed or of a format this
// build does not read are each a state of the answer; a caller that left is
// returned as it left; anything else is a store that did not answer, which a
// caller retries.
func transcriptUnread(out SessionAnswer, row executordomain.Session,
	err error) (SessionAnswer, error) {
	var malformed *executor.MalformedArchiveError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return SessionAnswer{}, err
	// GONE BEFORE UNREADABLE: the store's own absence is wrapped in the
	// domain's unreadable, and a reader acts on the two differently.
	case errors.Is(err, objstore.ErrNotFound):
		out.State, out.Why = TranscriptMissing, "the object store no longer holds this "+
			"session's object: the row named it, and the collector, a retention or a newer "+
			"capture of the turn took it since"
	case errors.Is(err, executordomain.ErrUnreadable):
		out.State, out.Why = TranscriptUnreadable, "this session's object does not open: it "+
			"is sealed under a key this fleet's keyring no longer holds, or its bytes are "+
			"not the ones sealed for this turn"
	case errors.As(err, &malformed):
		// THE ENGINE'S SENTENCE ALONE, never the decoder's beneath it: a
		// decoder quotes what it read, and what it read is a box's.
		out.State, out.Why = TranscriptMalformed, "this session opened, and is not an archive "+
			"of its harness's format: "+malformed.Why
	case errors.Is(err, executor.ErrUnsupportedArchive):
		out.State, out.Why = TranscriptUnsupported, errorTail(err, executor.ErrUnsupportedArchive)
	default:
		return SessionAnswer{}, fmt.Errorf("%w: the object store did not answer for %s's "+
			"session of the turn %s: %w", ErrUnavailable, row.Handle, row.Turn, err)
	}
	out.Entries = []executor.TranscriptEntry{}
	return out, nil
}

// errorTail is what err says after sentinel's own words, which name a package
// rather than what happened.
func errorTail(err, sentinel error) string {
	text := err.Error()
	if at := strings.LastIndex(text, sentinel.Error()+": "); at >= 0 {
		return text[at+len(sentinel.Error())+2:]
	}
	return text
}

// executorRefusal is a failed read of the executor log in this surface's words:
// a request the domain refused — a cursor that is no page's position, a turn
// id that is none — is the caller's to change, told in the domain's own
// sentence; anything else is returned as it came, for the registry to class.
func executorRefusal(err error) error {
	if !errors.Is(err, executordomain.ErrInvalid) {
		return err
	}
	return refusedBy(err, errorTail(err, executordomain.ErrInvalid))
}

// transcriptCursor is the cursor a transcript's next page is read from: the
// object the page was read from, the version of what counts as a transcript's
// entry ([executor.TranscriptVersion]), and the ordinal of the entry after it.
//
// THE OBJECT IS IN IT because a live turn's row moves to a newer object with
// every capture, and THE VERSION because a rolling upgrade serves two builds
// side by side: a page read from one archive, or counted by one build, and
// continued in another would skip or repeat entries with nothing saying so.
func transcriptCursor(key objstore.Key, next int) string {
	return key.String() + "." + strconv.Itoa(executor.TranscriptVersion) + "." + strconv.Itoa(next)
}

// transcriptFrom is the ordinal a page of row's transcript starts at: 0 for no
// cursor, and the cursor's own for one the row's object handed out under this
// build's count of entries. A cursor of an object the row has since replaced
// with a newer capture, or of another count, is refused, saying to read again
// from the start.
//
// A ROW THAT NAMES NO OBJECT ANY MORE IS NOT CAPTURED AGAIN: the seat's
// session budget released its object, which it does to an ended turn alone,
// and an ended turn is never captured again. So its cursor is the page's to
// answer, as the first page is answered — no session kept, and why
// ([Sources.readTranscript]) — where a refusal saying the session was captured
// since, and to read it from its start, said what was false and sent the
// reader to a first page saying something else.
func transcriptFrom(cursor string, row executordomain.Session) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	parts := strings.Split(cursor, ".")
	if len(parts) != 3 {
		return 0, notATranscriptCursor(cursor)
	}
	version, versionErr := strconv.Atoi(parts[1])
	from, fromErr := strconv.Atoi(parts[2])
	switch {
	case versionErr != nil || fromErr != nil || version < 1 || from < 0:
		return 0, notATranscriptCursor(cursor)
	case version != executor.TranscriptVersion:
		return 0, fmt.Errorf("%w: the cursor %q was handed out by a build of the engine that "+
			"counts a transcript's entries otherwise: read the transcript again from its start",
			ErrBadParams, cursor)
	case row.HasObject() && parts[0] != row.Object.Key.String():
		return 0, fmt.Errorf("%w: the turn %s's session was captured again since the cursor "+
			"%q was handed out: read its transcript again from the start", ErrBadParams,
			row.Turn, cursor)
	}
	return from, nil
}

// notATranscriptCursor refuses a cursor no page of a transcript hands out.
func notATranscriptCursor(cursor string) error {
	return fmt.Errorf("%w: the cursor parameter is one a page of this transcript "+
		"handed out, and %q isn't", ErrBadParams, cursor)
}

// sessionRowOf is a row as this surface shows it, the text somebody wrote in it
// redacted.
func sessionRowOf(s executordomain.Session) SessionRow {
	row := SessionRow{
		Turn: s.Turn, ConversationKey: s.ConversationKey, WorkKey: s.WorkKey,
		Harness: s.Harness, HarnessVersion: s.HarnessVersion,
		CapturedAt: sessionInstant(s.CapturedAt), EndedAt: sessionInstant(s.EndedAt),
		AskExcerpt: redact.Secrets(s.AskExcerpt), Kept: s.HasObject(),
	}
	if row.Kept {
		row.ObjectBytes = s.Object.Size
	}
	if s.Ending != nil {
		// A COPY, redacted: the row is the domain's, and the ending's prose
		// is a model's or a person's words.
		ending := *s.Ending
		ending.CompletedWork = redact.Secrets(ending.CompletedWork)
		ending.BlockedOn = redact.Secrets(ending.BlockedOn)
		ending.StopReason = redact.Secrets(ending.StopReason)
		ending.Failure = redact.Secrets(ending.Failure)
		row.Ending = &ending
	}
	return row
}

// notKept is why a row names no object.
func notKept(s executordomain.Session) string {
	if s.CapturedAt.IsZero() {
		return "nothing of this turn's session was captured: its run ended before its harness " +
			"packed one, or its harness carries none"
	}
	return "the seat's session budget released this session's object to make room for newer " +
		"ones; how the turn ended is kept"
}

// sessionInstant is an instant of a row, or null for one it has not got.
func sessionInstant(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	at = at.UTC()
	return &at
}
