package queries

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/executorread"
	"github.com/crewlet/crewlet/internal/learning/memread"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SEAT'S EXECUTOR SESSIONS, read by a person (ADR-0073).
//
// Every turn whose executor captured its harness's session, or read its
// conversation (from its first launch's `begin`), leaves a row in the executor
// log naming the sealed object that holds that session and how the turn ended.
// `executor_sessions` pages a seat's rows newest first; `executor_session` reads
// one turn's transcript as the entries its harness recorded.
// Both are decided per seat by [authz.ActionSeatTrailRead] through
// [Sources.trailSeat], as are `executor_tail` and the turn's phases. A
// transcript hands them MORE than live output (every call's whole input, each
// result's text, each subagent's thread), and redaction matches credential
// shapes, not values: a secret of no known shape is in it as the run read it.

// Rows are read through the estate router ([ExecutorSessions]), so any node
// answers; the object through the fleet's object store ([SessionObjects]), by
// the one reader every surface uses (internal/executorread). A gone
// object, one that does not open under the keyring, a malformed archive (the
// engine's own sentence, [executor.MalformedArchiveError], never a decoder's,
// which would quote what a box packed) and an unreadable format are each an
// ANSWER, never a failure: only a store that did not answer is `unavailable`.
// Pages are held to [executor.TranscriptReadBounds], and the answer says why one
// ends. Entry text, the ask's excerpt and the ending's prose pass through
// [redact.Secrets] at this read (the row by [executordomain.Session.Redacted]).
// A node decodes at most [executor.TranscriptReadsAtOnce] at once (each up to
// [executor.TranscriptReadHeldBytes]); more wait for a slot as long as their caller waits.

// A live turn's row naming a session is `in_progress`: the newest its runs kept,
// which the run may have gone past. An ended row kept up to a session its turn
// went on past, or whose turn was lost, is no account of all the turn did. Each
// is said in the domain's one sentence, asked of the row's shown form, so a
// person is told what the seat's own reads are: `in_progress_note`
// ([executordomain.Session.InProgressNote], the engine's words alone) and
// `kept_note` ([executordomain.Session.WentOnNote], quoting what refused a
// capture, declared so). An ended row whose runs did not continue a session
// says so in `not_continued_note` ([executordomain.Session.NotContinuedNote],
// the engine's sentence of each verdict). The screen words none of them.

// Each row says which launch would continue its session (`continued_by`: a
// relaunch while the turn is live; its conversation's next turn once it has
// ended or is ending, in the schedule's words for a schedule's conversation,
// [nextLaunch]; none if it served no conversation), whether it would
// (`continuable`), where (`continues_in`: any box, or for a relaunch only the
// box the run parked in, while held) and if not why (`not_continuable`). All is
// asked of [sandbox.ReadContinuityOf], THE decision that launch takes
// ([sandbox.ContinuityOf] for a relaunch, [sandbox.BaseContinuityOf] for the
// next turn), never a second copy of any part of it.

// A LIVE ROW: the row as a relaunch reads it, the run record ([SessionRuns])
// and the job it names as the one a relaunch follows, and, where the turn kept
// no session, the one its first launch was handed. AN ENDED ROW: whether it is
// its conversation's HEAD (the last-read of its ended turns still naming a
// session; executordomain.Page.Heads, SessionRead.Head, same snapshot): only the
// head is continued, others name the head the next turn is handed; a row naming
// no session says why, of four reasons. For either, the member and cell the
// launch would land on and its runner are picked as THE NODE HOLDING THE SEAT
// picks them ([RelaunchTargets]), off the chain of the revision it runs on: a
// resumed segment keeps its admitted revision, a conversation's next turn takes
// the live one, which an ask of NO turn names ([sandbox.TargetAsk]). No row says
// what the launch finds out once a box is chosen (release, own box, budget).

// A ROW BEGINS AT ITS TURN'S FIRST LAUNCH where the turn read its conversation
// (`begin` holds the handed session, `base_turn`), else at its first capture,
// and GOES WITH THE LATER TURN THAT WAS HANDED IT once that turn lands a capture
// of its own and ends (an end deletes the rows its read handed out). It also
// goes past the retention and with its seat, so a session read after its row
// went is `not_found`, said as one that may have gone any of these ways.

// A PAGE READS THE COORDINATION STORE ONCE: one listing of the seat's live runs
// ([SessionRuns.ListActiveForSeat], bounded by the fleet's concurrent runs),
// none where every row has ended, and one turn's record for a live row the
// listing lacks (a status this build does not know, or undecodable). It ASKS
// THE NODE HOLDING THE SEAT ONCE for every row a member decides (live rows
// neither over nor ending, ended heads), not at all if none, at the cost of one
// lease read naming that node ([RelaunchTargets]).

// A LIVE ROW IS DECIDED ON THE LOG AS A RELAUNCH READS IT: re-read at the
// relaunch's level ([executordomain.RelaunchRead]) only where the page was read
// below it and the row names another capture than the record keeps, or the
// record keeps none or no newer one of the followed job ([sandbox.RereadsRow]),
// since a copy behind the record would show continuable a session the log no
// longer names; elsewhere not, since that read appends a barrier to the log. An
// ended row decides on its page's snapshot (`read_level`). A transcript's first
// page, read with no cursor, says it; later pages leave it null, saying so.

// AN UNKNOWN IS NEVER A NO: `continuable` is null and `not_continuable` says why
// where the run record or the row at the relaunch's level could not be read, or
// the launch would not decide now: its job working or being collected, its
// capture being kept or its record awaiting the log's answer (a relaunch asks
// again, [sandbox.ReadContinuityOf]), every member of the seat's chain out of its
// plan, no node holding the seat or not answering, a relaunch only its job's
// collection resume decides, a turn whose end is not on the log yet. A no would
// tell a person a session starts over that may be continued; an unanswering
// store never fails the page, whose rows the log answered whole.

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
// (executordomain.SessionStore): what the transcript is read from; and the
// summary its row keeps apart, read where the session is not kept.
type SessionObjects interface {
	Open(ctx context.Context, session executordomain.Session) (io.ReadCloser, error)
	OpenSummary(ctx context.Context, session executordomain.Session) (io.ReadCloser, error)
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

// RelaunchTargets is what a launch of each of a seat's sessions asked about
// would be launched on now — a relaunch of the turn an ask names, or the next
// turn of a conversation, for an ask that names none — one target per ask in
// the order asked, as THE NODE HOLDING THE SEAT picks it
// ([sandbox.RelaunchTarget]): the member of the seat's executor chain the
// launch takes — off the chain of the company revision that launch runs on —
// the cell its run lands in, that node, and whether that member's runner
// restores the ask's archive, so a row is decided by the runner the launch
// would take, never by the harness that wrote the session, nor by a bench of
// spent plans the launching node does not keep.
type RelaunchTargets interface {
	RelaunchTargets(ctx context.Context, seat uuid.UUID, handle string,
		asks []sandbox.RelaunchAsk) []sandbox.RelaunchTarget
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
	Ending *executorkeep.Ending `json:"ending"`

	// AskExcerpt is the opening of what the turn was asked, redacted.
	AskExcerpt string `json:"ask_excerpt" untrusted:"fence"`

	// Kept says the row still names a session's object, and ObjectBytes is
	// that object's sealed size: zero where the turn captured nothing, its
	// newest capture was not kept, or the seat's budget released it.
	Kept        bool  `json:"kept"`
	ObjectBytes int64 `json:"object_bytes"`

	// SummaryKept says the row keeps its session's newest compaction summary
	// apart from the session (executordomain.Session.Summary), SummaryBytes
	// the bytes kept of it: the transcript a row keeping no session reads as
	// ([executorread.TranscriptSummary]).
	SummaryKept  bool  `json:"summary_kept"`
	SummaryBytes int64 `json:"summary_bytes"`

	// InProgress says the row names the session of a turn that has not
	// ended: the newest its runs have kept, which its run may have gone on
	// past (executordomain.Session.InProgress), and InProgressNote says so in
	// the domain's one sentence of it (executordomain.Session.InProgressNote):
	// the engine's words alone, absent on any other row.
	InProgress     bool   `json:"in_progress"`
	InProgressNote string `json:"in_progress_note,omitempty"`

	// KeptNote is what a reader is told of an ended turn's row kept up to a
	// session its turn went on past, or of a turn that was lost, in the
	// domain's one sentence of it (executordomain.Session.WentOnNote): that,
	// and which newer capture was not kept and why where its end names one —
	// somebody's words in it, redacted, as the ending's are. Absent on any
	// other row.
	KeptNote string `json:"kept_note,omitempty" untrusted:"fence"`

	// NotContinuedNote is what an ended turn's runs did not continue, in the
	// engine's sentence of each (executordomain.Session.NotContinuedNote): what
	// was lost, of which turn, and what the run continued instead. The
	// engine's words alone, absent where they lost nothing.
	NotContinuedNote string `json:"not_continued_note,omitempty"`

	// BaseTurn is the ended turn whose session the turn's first launch was
	// handed to continue — its conversation's head as the turn's dispatch
	// read it, which the turn's `begin` held on the log — absent where the
	// read named none, that session had gone before the begin landed, or no
	// begin landed. Whether a launch took it up is its executor records'
	// `brief`.
	BaseTurn string `json:"base_turn,omitempty"`

	// ContinuedBy is the launch the rest is about ([sandbox.ContinuedBy]): a
	// relaunch while the turn is live, its conversation's next turn once it has
	// ended or is ending (`next_fire` for a schedule's), absent where none would
	// continue the session or nothing was decided (a transcript's later page).
	//
	// Continuable is whether that launch would continue this session, decided as
	// it decides ([sandbox.ReadContinuityOf]); ContinuesIn where. NotContinuable
	// is, in the engine's words, redacted, why not (for a relaunch confined to
	// its parked box, why no other box does). NULL where it cannot be decided
	// now: an unknown is never sent as a no (see the file comment).
	// NotContinuable can quote what refused a capture, an error a box or store
	// wrote, so it is declared somebody else's words.
	ContinuedBy    sandbox.ContinuedBy `json:"continued_by,omitempty"`
	Continuable    *bool               `json:"continuable"`
	ContinuesIn    sandbox.ContinuesIn `json:"continues_in,omitempty"`
	NotContinuable string              `json:"not_continuable,omitempty" untrusted:"fence"`
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

// SessionAnswer is one turn's executor session and a page of its transcript.
type SessionAnswer struct {
	Handle  string     `json:"handle"`
	Session SessionRow `json:"session"`

	// State is what reading the session's object came to, and Why says it in
	// the engine's own words wherever it is not `read` — the words every
	// reader of a session is told (internal/executorread), which can quote
	// why a capture was not kept, and so are declared as somebody else's.
	State executorread.TranscriptState `json:"state"`
	Why   string                       `json:"why,omitempty" untrusted:"fence"`

	// Entries are the transcript's entries from From on, in the order the
	// harness recorded them: the newest summary alone where State is
	// `summary`, and empty wherever it is neither that nor `read`.
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
	// says beside its rows. A row read again because the session's object
	// failed was read at `linearizable` ([executorread.ReadAgain]), and the
	// answer says so.
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
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	page, err := s.ExecutorSessions.Sessions(ctx, seat.Handle, strings.TrimSpace(p.String("cursor")),
		p.Int("limit", 0), fresh)
	if err != nil {
		return nil, executorRefusal(err)
	}
	// ONE READ OF THE COORDINATION STORE FOR THE PAGE, and ONE QUESTION TO
	// THE NODE HOLDING THE SEAT — see the file comment.
	runs, unread := s.liveRuns(ctx, seat.Handle, page.Sessions)
	now := s.clock()
	pending := make([]rowContinuity, 0, len(page.Sessions))
	for _, row := range page.Sessions {
		run, runErr := runs[row.Turn], unread
		if run == nil && runErr == nil {
			// A LIVE ROW THE LISTING DOES NOT HOLD is read alone, as a
			// session read alone reads it: the listing passes over a record
			// of a status this build does not know and one it could not
			// decode, and neither is a run that is over.
			run, runErr = s.liveRun(ctx, row)
		}
		// ITS CONVERSATION'S HEAD AS THE PAGE'S OWN SNAPSHOT NAMED IT, so an
		// ended row is decided where its rows were read, never on a second
		// read the page's own could disagree with.
		head := page.Heads[row.ConversationKey]
		pending = append(pending, s.relaunchRow(ctx, row, head, run, runErr, fresh, now))
	}
	targets := s.relaunchTargets(ctx, seat, pending)
	rows := make([]SessionRow, 0, len(pending))
	for i, c := range pending {
		shown := sessionRowOf(c.row)
		shown.ContinuedBy, shown.Continuable, shown.ContinuesIn, shown.NotContinuable = c.decide(targets[i], now)
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

// laterTranscriptPage is what a transcript's later page says of whether a
// relaunch continues the session: its first page says it, once.
const laterTranscriptPage = "said on the transcript's first page, which a later page does not ask again"

// relaunchTargets is what a launch of each row's session would be launched on,
// one per row, as the node holding the seat answers it for the ask the row
// makes ([sandbox.TargetAsk]: a relaunch of a live row's turn, or the next turn
// of the conversation an ended row is the head of) — asked once for every row
// that asks, and not at all where none does, a row that does not deciding by
// it — or, on a node wired with nothing to ask ([Sources.RelaunchTargets]), a
// target that cannot be said: every row that asks is then undecided, as a live
// one whose run record could not be read is.
func (s Sources) relaunchTargets(ctx context.Context, seat memread.Seat,
	rows []rowContinuity) []sandbox.RelaunchTarget {

	targets := make([]sandbox.RelaunchTarget, len(rows))
	var asks []sandbox.RelaunchAsk
	var asking []int
	for i, c := range rows {
		ask, asked := c.ask()
		if !asked {
			continue
		}
		asks = append(asks, ask)
		asking = append(asking, i)
	}
	if len(asks) == 0 {
		return targets
	}
	var answered []sandbox.RelaunchTarget
	if s.RelaunchTargets != nil {
		answered = s.RelaunchTargets.RelaunchTargets(ctx, seat.ID, seat.Handle, asks)
	}
	for j, i := range asking {
		switch {
		case s.RelaunchTargets == nil:
			targets[i] = sandbox.RelaunchTarget{Undecided: true, Why: "this node asks no node holding the seat " +
				"what a launch of the seat's turns is launched on; ask a node that runs the company"}
		case j >= len(answered):
			// AN ASK LEFT UNANSWERED is unknown, never a no.
			targets[i] = sandbox.RelaunchTarget{Undecided: true, Why: "the node holding the seat said nothing " +
				"of what this session's launch is launched on; ask again shortly"}
		default:
			targets[i] = answered[j]
		}
	}
	return targets
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

// rowContinuity is one row of a seat's sessions on its way to its
// `continuable`: the row as its page read it, the row the decision reads — the
// same, or read again as a relaunch reads it ([Sources.relaunchRow]) — its
// conversation's head as the read that answered the row the decision reads
// named it, its turn's run record, nil where its run is over, and why it cannot
// be decided, where it cannot before anything else is asked.
type rowContinuity struct {
	row, decideOn executordomain.Session
	head          string
	run           *sandbox.PendingRun
	undecided     string
}

// ask is what the row asks the node holding the seat ([sandbox.TargetAsk]), and
// false where the row is decided by no member — or cannot be decided at all.
func (c rowContinuity) ask() (sandbox.RelaunchAsk, bool) {
	if c.undecided != "" {
		return sandbox.RelaunchAsk{}, false
	}
	return sandbox.TargetAsk(c.decideOn.Kept(), c.head, c.run)
}

// decide is the row's `continued_by`, `continuable`, `continues_in` and
// `not_continuable`: THE DECISION THE LAUNCH THAT WOULD CONTINUE IT TAKES
// ([sandbox.ReadContinuityOf]), asked of the row that launch reads, its
// conversation's head, its run record and target, the member the node holding
// the seat would launch it on, and which launch continues an ended turn of its
// conversation ([nextLaunch]) — or undecided, where the run record or the row
// could not be read as a relaunch reads it.
func (c rowContinuity) decide(target sandbox.RelaunchTarget,
	now time.Time) (sandbox.ContinuedBy, *bool, sandbox.ContinuesIn, string) {

	if c.undecided != "" {
		return sandbox.ContinuedByRelaunch, nil, "", c.undecided
	}
	decided := sandbox.ReadContinuityOf(c.decideOn.Kept(), c.head, c.run, target,
		nextLaunch(c.decideOn.ConversationKey), now)
	// REDACTED, as every text of this surface is: a reason can quote a
	// harness's release as its box printed it, and why a turn's newest
	// capture was not kept quotes what refused it.
	why := redact.Secrets(decided.Why)
	if !decided.Decided {
		return decided.By, nil, "", why
	}
	return decided.By, &decided.Continues, decided.In, why
}

// nextLaunch is the launch that continues the session an ended turn of the
// conversation key names left, as a reader is told it: its schedule's next
// fire where the conversation is a schedule's ([notify.Scheduled]), and its
// next turn otherwise. Asked here, of the one rule internal/notify holds,
// because internal/sandbox, which decides the row, cannot import notify.
func nextLaunch(conversationKey string) sandbox.ContinuedBy {
	if notify.Scheduled(conversationKey) {
		return sandbox.ContinuedByNextFire
	}
	return sandbox.ContinuedByNextTurn
}

// relaunchRow is row on its way to its continuity, head being its
// conversation's head as the read that answered row named it: undecided where
// the turn is live and its record unread, and otherwise decided on the row as
// a relaunch reads it: read again at the relaunch's level
// ([executordomain.RelaunchRead]) where [sandbox.RereadsRow] says so, and row
// itself elsewhere; undecided where the read again fails. A ROW READ AGAIN IS
// DECIDED ON THE HEAD THAT READ NAMED, never the page's: the turn's end can
// land between the two reads, and the page's older head would name a row the
// end may have absorbed.
func (s Sources) relaunchRow(ctx context.Context, row executordomain.Session, head string,
	run *sandbox.PendingRun, unread error, fresh statelog.Freshness, now time.Time) rowContinuity {

	c := rowContinuity{row: row, decideOn: row, head: head, run: run}
	switch {
	case unread != nil && !row.Ended():
		c.undecided = undecidedContinuity
		return c
	case fresh.Level == executordomain.RelaunchRead().Level || !sandbox.RereadsRow(row.Kept(), run, now):
		return c
	}
	read, err := s.ExecutorSessions.Session(ctx, row.Handle, row.Turn, executordomain.RelaunchRead())
	if err != nil {
		log.WarnContext(ctx, "executor_session_row_unreadable", "handle", row.Handle, "turn_id", row.Turn,
			"error", err, "detail", "whether a relaunch continues the session cannot be said now")
		c.undecided = unreadRelaunchRow
		return c
	}
	c.decideOn, c.head = read.Session, read.Head
	return c
}

// executorSession answers one turn's session — the turn `turn_id`, as every
// question about a turn names it — and a page of its transcript from `cursor`
// on, the first page where it is absent: read by the one reader of a kept
// session (internal/executorread), so a person is told of a session what every
// reader of it is told.
func (s Sources) executorSession(ctx context.Context, p Params) (any, error) {
	seat, err := s.trailSeat(ctx, strings.TrimSpace(p.String("handle")))
	if err != nil {
		return nil, err
	}
	turn := strings.TrimSpace(p.String("turn_id"))
	if turn == "" {
		return nil, badParams("turn_id", "", nil)
	}
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	cursor := strings.TrimSpace(p.String("cursor"))
	page, err := executorread.Read(ctx,
		executorread.Ask{Handle: seat.Handle, Turn: turn, Cursor: cursor, Fresh: fresh,
			By: executor.ReadByPerson},
		executorread.Source{Rows: s.ExecutorSessions, Objects: s.SessionObjects})
	if err != nil {
		return nil, sessionRefusal(err)
	}
	// THE ROW THE PAGE WAS ANSWERED FROM, at the level it was read at: the
	// caller's, or — where the session's object failed and the reader read
	// the turn's row again to say whether it is still kept — that read's.
	read, level := page.Read, fresh
	if page.Reread {
		level = executorread.ReadAgain()
	}
	shown := sessionRowOf(read.Session)
	if cursor == "" {
		// THE FIRST PAGE DECIDES IT, once — see the file comment — on the
		// head the row's own read named.
		now := s.clock()
		run, unread := s.liveRun(ctx, read.Session)
		c := s.relaunchRow(ctx, read.Session, read.Head, run, unread, level, now)
		shown.ContinuedBy, shown.Continuable, shown.ContinuesIn, shown.NotContinuable = c.decide(
			s.relaunchTargets(ctx, seat, []rowContinuity{c})[0], now)
	} else {
		shown.NotContinuable = laterTranscriptPage
	}
	return SessionAnswer{
		Handle: seat.Handle, Session: shown, State: page.State, Why: page.Why,
		Entries: page.Transcript.Entries, From: page.Transcript.From, Cut: page.Transcript.Cut,
		Next: page.Next, Bounds: executor.TranscriptReadBounds(),
		ReadLevel: level.Level, Position: read.Position, Complete: read.Complete,
	}, nil
}

// sessionRefusal is a read of a kept session that came to no page, in this
// surface's words: a turn the seat keeps no row of is not found, and a cursor
// the transcript cannot be continued from the caller's to change, each in the
// reader's own sentence; a store that did not answer is unavailable, which a
// caller retries; a refusal of the log's own is [executorRefusal]'s.
func sessionRefusal(err error) error {
	switch {
	case errors.Is(err, executorread.ErrNoSession):
		return fmt.Errorf("%w: %s", ErrNotFound, errorTail(err, executorread.ErrNoSession))
	case errors.Is(err, executorread.ErrCursor):
		return refusedBy(err, errorTail(err, executorread.ErrCursor))
	case errors.Is(err, executorread.ErrUnavailable):
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return executorRefusal(err)
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

// sessionRowOf is a row as this surface shows it: through the row's one
// redaction ([executordomain.Session.Redacted]), which every reader of a row
// shows it through, so what the turn was asked and the ending's prose are
// redacted here exactly as they are wherever else a row is shown — and what a
// reader is told of the session it keeps is the domain's own sentences of that
// form ([executordomain.Session.InProgressNote],
// [executordomain.Session.WentOnNote]), as a seat's own reads are told them.
func sessionRowOf(s executordomain.Session) SessionRow {
	s = s.Redacted()
	row := SessionRow{
		Turn: s.Turn, ConversationKey: s.ConversationKey, WorkKey: s.WorkKey,
		Harness: s.Harness, HarnessVersion: s.HarnessVersion,
		CapturedAt: sessionInstant(s.CapturedAt), EndedAt: sessionInstant(s.EndedAt),
		AskExcerpt: s.AskExcerpt, Kept: s.HasObject(), BaseTurn: s.BaseTurn, Ending: s.Ending,
		InProgress: s.InProgress(), InProgressNote: s.InProgressNote(), KeptNote: s.WentOnNote(),
		NotContinuedNote: s.NotContinuedNote(),
	}
	if row.Kept {
		row.ObjectBytes = s.Object.Size
	}
	if s.Summary != nil {
		row.SummaryKept, row.SummaryBytes = true, s.Summary.Kept
	}
	return row
}

// sessionInstant is an instant of a row, or null for one it has not got.
func sessionInstant(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	at = at.UTC()
	return &at
}
