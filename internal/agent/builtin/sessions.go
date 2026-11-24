package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/executorread"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/textindex"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
)

// THE SESSION TOOLS: a seat's executor lists and reads its own seat's other
// executor sessions (ADR-0073, "A seat reads its own sessions").
//
// Neither takes a seat: both read the bound turn's ([turnctx.Turn.Handle]),
// which the tool bridge sets whatever the request carried, so another seat's
// turn id reads a row that does not exist and is answered as one nobody has.
// The seat reads them whoever the reading turn acts for, as it does its diary.
// Each call is one linearizable read of the executor log at the seat's level,
// which executordomain.LinearizableReadsPerSeatDay counts. A read writes no
// record and no hold: a hold would be a write a model originates, and would keep
// the seat's budget from releasing for its own next capture. A session released
// while read is said as released (internal/executorread).

// The tool wire names.
const (
	ListSessionsTool = "list_sessions"
	ReadSessionTool  = toolname.ReadSession
)

// SessionDeps are the session tools' dependencies. Both tools are omitted
// unless all three are present: without one a tool fails at every call, or,
// without Keeps, answers after an operator turned the switch off.
type SessionDeps struct {
	// Rows reads the seat's rows on the executor log through the estate
	// router (estate.ExecutorReads), so a node without `data` can serve.
	Rows SessionRows

	// Objects opens a session's sealed object (executordomain.SessionStore),
	// on a node with an object store.
	Objects executorread.Objects

	// Keeps reports whether the revision this node runs NOW keeps executor
	// sessions (`turn_engine.conversation_session.enabled`). Asked at every
	// call, not only at launch: an operator turning it off over a leak expects
	// a launch already running to stop reading too. False refuses the call.
	Keeps func() bool
}

// serves reports whether d serves the session tools.
func (d SessionDeps) serves() bool { return d.Rows != nil && d.Objects != nil && d.Keeps != nil }

// switchedOff refuses a call while the company keeps no executor session
// ([SessionDeps.Keeps]): forbidden, since no permission or retry lifts it.
func switchedOff(tool string) tools.Result {
	return refused(tools.RefusalForbidden, tool+": reading your sessions is turned off — the company "+
		"keeps no executor session now (turn_engine.conversation_session.enabled is false), and no "+
		"model reads the ones kept before. Work from what this turn was told.")
}

// SessionRows is the executor log's reads the session tools make, each at the
// level asked (estate.ExecutorReads). Over the domain's own types, so the row
// stays one shape.
type SessionRows interface {
	Sessions(ctx context.Context, handle, cursor string, limit int,
		fresh statelog.Freshness) (executordomain.Page, error)
	Search(ctx context.Context, handle string, ask executordomain.SessionSearch,
		fresh statelog.Freshness) (executordomain.SearchPage, error)
	Session(ctx context.Context, handle, turn string,
		fresh statelog.Freshness) (executordomain.SessionRead, error)
}

// SessionReadWithin bounds one `read_session` call: the wait for a slot of the
// node's transcript reads, the object's fetch and its decode up to the page.
//
// TWO MINUTES: at the object store's slowest accepted pace (objstore.ReadBase
// and MiBPace: a minute plus thirty seconds a MiB) a session of a MiB or two
// arrives within it; one at its 48 MiB bound would take about twenty-five and a
// half, which no call should wait out, and two is far inside the ten a harness
// waits on a call. Past it the call is answered unavailable, naming the
// session's size, never as a session that holds nothing. A decode at its bounds
// of what a harness writes takes up to 58 s, within it; one queued behind other
// such reads, or of a value none writes (`pwd=x` lines), can miss it, and fails
// closed so ([executor.PersonTranscriptReadWorst]).
const SessionReadWithin = 2 * time.Minute

// The listing's page.
const (
	// listedSessionsDefault is a page where the call names no limit: the
	// most a page holds, a few days of a seat's work, where a session worth
	// rereading usually is.
	listedSessionsDefault = listedSessionsMax

	// listedSessionsMax is the most a page holds, and a larger limit is
	// clamped to it: NINE, the most rows whose worst case (every field any row
	// can carry at its record's bound, prose at [listedProseBytes] with its
	// fence lines; 6,510 bytes a row when set) fits one 64 KiB tool answer
	// ([ToolAnswerBytes]) with the note and envelope.
	// TestAPageOfSessionsAtItsWorstFitsOneAnswer fails on a page of nine
	// refused and on room for a tenth. A page JSON escaping pushes past it
	// anyway is refused naming `limit`, never cut short silently: its cursor
	// is its last row.
	listedSessionsMax = 9

	// listedProseBytes cuts each of a listed row's words (its ask, its
	// ending's accounts, its went-on note), on a rune boundary and marked:
	// THREE HUNDRED BYTES, about three lines, enough to tell sessions apart.
	// A read's first page shows the row whole, and the in-progress sentence,
	// the engine's and bounded by it, is always whole.
	listedProseBytes = 300
)

// sessionLabel labels what a seat's kept session holds as the seat reads it
// back: naming seat as the run that kept it, or nobody ("") for the row's
// words, whose writers the row does not keep ([provenance.ExecutorSession]).
// Never `own`: a session is a vendor harness's record, quoting briefs, people's
// notes and tools' output. It is the one door the session tools decide a trust
// through.
func sessionLabel(seat string) provenance.Label { return provenance.ExecutorSession(seat) }

// fenceSession registers the string at path in read's answer as text label
// says is a kept session's.
func fenceSession(read *foreignRead, path fieldPath, label provenance.Label, text string) {
	read.fenceAt(path, label.Source, label.Trust, label.Authors, text)
}

// sessionRow is one of a seat's sessions as the session tools show it.
type sessionRow struct {
	Turn         string `json:"turn_id"`
	Conversation string `json:"conversation,omitempty"`

	// ThisTurn marks the reading turn's own session (its earlier launches'),
	// ThisConversation one of the conversation the reading turn serves.
	ThisTurn         bool `json:"this_turn,omitempty"`
	ThisConversation bool `json:"this_conversation,omitempty"`

	Harness    string     `json:"harness,omitempty"`
	CapturedAt *time.Time `json:"captured_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`

	// Kept says the row still names a session's object, KeptBytes its size
	// compressed and sealed; a row naming none still says how its turn ended.
	Kept      bool  `json:"kept"`
	KeptBytes int64 `json:"kept_bytes,omitempty"`

	// SummaryKept says the row keeps the newest summary its harness wrote of
	// the session, apart from it, and SummaryBytes the bytes kept of it: what
	// `read_session` reads of a row that keeps no session.
	SummaryKept  bool  `json:"summary_kept,omitempty"`
	SummaryBytes int64 `json:"summary_bytes,omitempty"`

	// InProgress marks the session of a turn that has not ended, which its
	// run may have gone on past, so a seat never takes it for an ended turn's.
	// InProgressNote is executordomain.Session.InProgressNote: the engine's
	// words alone, never fenced.
	InProgress     bool   `json:"in_progress,omitempty"`
	InProgressNote string `json:"in_progress_note,omitempty"`

	// KeptNote is executordomain.Session.WentOnNote, on a turn that went on
	// past the session it kept, or lost it: it quotes what refused the newer
	// capture, so it is somebody's words.
	KeptNote string `json:"kept_note,omitempty" untrusted:"fence"`

	// BaseTurn is the session the turn's first launch was handed to
	// continue, where it was handed one.
	BaseTurn string `json:"base_turn,omitempty"`

	// Asked is what the turn was asked, and Ending how it ended (null while
	// it runs): somebody else's words, declared on the types.
	Asked  string               `json:"asked,omitempty" untrusted:"fence"`
	Ending *executorkeep.Ending `json:"ending"`

	// Matched and Snippet are a search's: the fields of the row holding a
	// word of the query, and a window of its words around the earliest.
	Matched []executordomain.SearchField `json:"matched,omitempty"`
	Snippet string                       `json:"snippet,omitempty" untrusted:"fence"`
}

// shownRow is s as shown to turn: redacted ([executordomain.Session.Redacted]),
// its words cut to cut bytes unless cut is 0, and marked when it is turn's own
// session or conversation. In-progress and went-on are asked of the redacted
// form, so a seat is told what a person's read of the row is told: the
// in-progress sentence whole, the went-on note cut like the row's other words.
func shownRow(s executordomain.Session, turn *turnctx.Turn, cut int) sessionRow {
	s = s.Redacted()
	shorten := func(text string) string {
		if cut == 0 {
			return text
		}
		return textcut.Within(text, cut)
	}
	row := sessionRow{
		Turn: s.Turn, Conversation: s.ConversationKey, Harness: s.Harness,
		CapturedAt: sessionInstant(s.CapturedAt), EndedAt: sessionInstant(s.EndedAt),
		Kept: s.HasObject(), BaseTurn: s.BaseTurn, Asked: shorten(s.AskExcerpt),
		InProgress: s.InProgress(), InProgressNote: s.InProgressNote(),
		KeptNote: shorten(s.WentOnNote()),
	}
	if row.Kept {
		row.KeptBytes = s.Object.Size
	}
	if s.Summary != nil {
		row.SummaryKept, row.SummaryBytes = true, s.Summary.Kept
	}
	if turn != nil {
		row.ThisTurn = s.Turn == turn.RunID
		row.ThisConversation = turn.ConversationKey != "" && s.ConversationKey == turn.ConversationKey
	}
	if s.Ending != nil {
		// A COPY, so cutting its prose leaves the row read untouched.
		ending := *s.Ending
		ending.CompletedWork, ending.BlockedOn = shorten(ending.CompletedWork), shorten(ending.BlockedOn)
		ending.StopReason, ending.Failure = shorten(ending.StopReason), shorten(ending.Failure)
		if ending.Unkept != nil {
			unkept := *ending.Unkept
			unkept.Reason = shorten(unkept.Reason)
			ending.Unkept = &unkept
		}
		if len(ending.NotContinued) > 0 {
			said := slices.Clone(ending.NotContinued)
			for i := range said {
				said[i].Detail = shorten(said[i].Detail)
			}
			ending.NotContinued = said
		}
		row.Ending = &ending
	}
	return row
}

// sessionInstant is an instant of a row, or nothing for one it has not got.
func sessionInstant(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	at = at.UTC()
	return &at
}

// fenceRow fences row's words at path in read's answer as a kept session's
// naming nobody (the row keeps no writer, and its ending and note quote a box,
// a tool or a person), and whoever stopped it as a name defused in place, on
// the envelope. Never the in-progress sentence, which is the engine's own.
func fenceRow(read *foreignRead, path fieldPath, row sessionRow) {
	nobody := sessionLabel("")
	fenceSession(read, path.key("asked"), nobody, row.Asked)
	fenceSession(read, path.key("kept_note"), nobody, row.KeptNote)
	fenceSession(read, path.key("snippet"), nobody, row.Snippet)
	e := row.Ending
	if e == nil {
		return
	}
	ending := path.key("ending")
	fenceSession(read, ending.key("completed_work"), nobody, e.CompletedWork)
	fenceSession(read, ending.key("blocked_on"), nobody, e.BlockedOn)
	fenceSession(read, ending.key("stop_reason"), nobody, e.StopReason)
	fenceSession(read, ending.key("failure"), nobody, e.Failure)
	if e.Unkept != nil {
		fenceSession(read, ending.key("unkept").key("reason"), nobody, e.Unkept.Reason)
	}
	for i, n := range e.NotContinued {
		fenceSession(read, ending.key("not_continued").index(i).key("detail"), nobody, n.Detail)
	}
	if e.StoppedBy != "" {
		read.adopt(nobody)
	}
}

// sentenceAfter is what err says after sentinel's own words, which name a
// package rather than what happened.
func sentenceAfter(err, sentinel error) string {
	text := err.Error()
	if at := strings.LastIndex(text, sentinel.Error()+": "); at >= 0 {
		return text[at+len(sentinel.Error())+2:]
	}
	return text
}

// sizeWords is n bytes as a reader is told a session's size.
func sizeWords(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", kib(int(n)))
}

// --- list_sessions ------------------------------------------------------- //

// listSessions lists the calling seat's own sessions, a page at a time, or
// searches their words.
type listSessions struct{ deps SessionDeps }

var _ tools.SeatCallable = (*listSessions)(nil)

func (t *listSessions) Name() string { return ListSessionsTool }

func (t *listSessions) Description() string {
	return "Your own executor sessions: the transcripts your harness kept of your past turns, " +
		"newest first. Each says what its turn was asked, how it ended, and whether its " +
		"transcript is still kept. `query` keeps the sessions whose ask, conversation or " +
		"ending holds every word. Read one with `" + ReadSessionTool + "`."
}

func (t *listSessions) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type": "string",
				"description": "Optional: words. Only the sessions whose ask, conversation or ending " +
					"holds every one of them are listed: whole words, any case.",
			},
			"cursor": map[string]any{
				"type":        "string",
				"description": "The `next_cursor` of the previous answer, to read on from.",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many sessions a page holds (default %d, max %d: the most "+
					"whose page fits one answer with every row at its largest).",
					listedSessionsDefault, listedSessionsMax),
			},
		},
	}
}

// sessionsAnswer is a page of the seat's sessions.
type sessionsAnswer struct {
	Sessions []sessionRow `json:"sessions"`

	// Next is the cursor the next page is read from, absent on the last.
	Next string `json:"next_cursor,omitempty"`

	// Searched is how far a search went, and Note says it in words, so a
	// search that stopped short is never read as one that found nothing. A
	// listing has a Note only when Incomplete.
	Searched *sessionsSearched `json:"searched,omitempty"`
	Note     string            `json:"note,omitempty"`

	// Incomplete says the answering copy could not decode a record that may
	// hold rows missing from the page.
	Incomplete bool `json:"incomplete,omitempty"`
}

// What an incomplete page says, IN WORDS beside the flag: what a model is to do
// (not conclude a session is gone, or that none holds the words) is a
// behaviour, and a bare boolean beside rows reads as metadata, not a caveat.
const (
	incompleteSessions = "The node that answered holds records of your sessions it could not read, " +
		"so sessions may be missing from this page: do not conclude a session is not kept."
	incompleteSearch = "The node that answered holds records of your sessions it could not read, so " +
		"sessions holding every word may be missing from this search: do not conclude none does."
)

// sessionsSearched is how far a search's page went: how many sessions it
// examined, and whether they reached the seat's oldest.
type sessionsSearched struct {
	Sessions int  `json:"sessions"`
	ToTheEnd bool `json:"to_the_end"`
}

func (t *listSessions) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *listSessions) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	seat := turn.Handle()
	if seat == "" {
		return refused(tools.RefusalForbidden, ListSessionsTool+" can only be called during a turn, on "+
			"behalf of a seat: it reads the calling seat's own sessions."), nil
	}
	if !t.deps.Keeps() {
		return switchedOff(ListSessionsTool), nil
	}
	limit := clampInt(argInt(args, "limit", listedSessionsDefault), 1, listedSessionsMax)
	cursor := strings.TrimSpace(argString(args, "cursor"))
	query := strings.TrimSpace(argString(args, "query"))
	answer := sessionsAnswer{Sessions: []sessionRow{}}
	if query == "" {
		page, err := t.deps.Rows.Sessions(ctx, seat, cursor, limit, seatRead)
		if err != nil {
			return sessionsFailure(ctx, err, false), nil
		}
		for _, s := range page.Sessions {
			answer.Sessions = append(answer.Sessions, shownRow(s, turn, listedProseBytes))
		}
		answer.Next, answer.Incomplete = page.Next, !page.Complete
		if answer.Incomplete {
			answer.Note = incompleteSessions
		}
		return t.render(ctx, turn, answer)
	}
	if len(textindex.Terms(query)) == 0 {
		return failed(ListSessionsTool + ": `query` holds no word to search by: a word is two or more " +
			"letters or digits."), nil
	}
	found, err := t.deps.Rows.Search(ctx, seat,
		executordomain.SessionSearch{Query: query, Cursor: cursor, Limit: limit}, seatRead)
	if err != nil {
		return sessionsFailure(ctx, err, true), nil
	}
	for _, hit := range found.Hits {
		row := shownRow(hit.Session, turn, listedProseBytes)
		row.Matched, row.Snippet = hit.Matched, hit.Snippet
		answer.Sessions = append(answer.Sessions, row)
	}
	answer.Next, answer.Incomplete = found.Next, !found.Complete
	answer.Searched = &sessionsSearched{Sessions: found.Scanned, ToTheEnd: found.Exhausted}
	answer.Note = searchedNote(found, cursor != "")
	return t.render(ctx, turn, answer)
}

// render answers a page with every row's words fenced, compact: the encoding
// listedSessionsMax was sized by.
func (t *listSessions) render(ctx context.Context, turn *turnctx.Turn, answer sessionsAnswer) (tools.Result, error) {
	read := newForeignRead(turn, turn.Handle())
	for i, row := range answer.Sessions {
		fenceRow(read, fieldPath{}.key("sessions").index(i), row)
	}
	return read.page(ctx, ListSessionsTool, answer, "Ask for fewer sessions with `limit`.")
}

// searchedNote says how far a search's page went (how many sessions it
// examined, from where, what is left), never that nothing matches past them,
// nor among them when found is not Complete. after says the page was read
// after a cursor rather than from the newest.
func searchedNote(found executordomain.SearchPage, after bool) string {
	hits, since := len(found.Hits), "most recently begun"
	if after {
		since = "most recently begun before the cursor"
	}
	if !found.Complete {
		var went string
		switch {
		case found.Exhausted && !after:
			went = fmt.Sprintf("Searched to your oldest session, examining %d, and found %d holding every "+
				"word.", found.Scanned, hits)
		case found.Exhausted:
			went = fmt.Sprintf("Searched the sessions begun before the cursor to your oldest, examining %d, "+
				"and found %d holding every word.", found.Scanned, hits)
		default:
			went = fmt.Sprintf("Searched your %d %s sessions and found %d holding every word; older ones "+
				"were not searched: call again with next_cursor to search them.", found.Scanned, since, hits)
		}
		return went + " " + incompleteSearch
	}
	switch {
	case found.Exhausted && !after && hits == 0:
		return fmt.Sprintf("Searched all %d of your sessions: none holds every word.", found.Scanned)
	case found.Exhausted && !after:
		return fmt.Sprintf("Searched all %d of your sessions.", found.Scanned)
	case found.Exhausted && hits == 0:
		return fmt.Sprintf("Searched the %d of your sessions begun before the cursor, to your oldest: "+
			"none holds every word.", found.Scanned)
	case found.Exhausted:
		return fmt.Sprintf("Searched the %d of your sessions begun before the cursor, to your oldest.",
			found.Scanned)
	case found.Scanned < executordomain.SearchScanRows:
		// THE PAGE FILLED before its bound: sessions past its last hit are
		// unexamined, not empty.
		return fmt.Sprintf("Found %d holding every word among your %d %s sessions; older ones were not "+
			"searched yet: call again with next_cursor to go on.", hits, found.Scanned, since)
	case hits == 0:
		return fmt.Sprintf("None of your %d %s sessions holds every word; older ones were not searched: "+
			"call again with next_cursor to search them.", found.Scanned, since)
	}
	return fmt.Sprintf("Searched your %d %s sessions and found %d holding every word; older ones were "+
		"not searched: call again with next_cursor to search them.", found.Scanned, since, hits)
}

// sessionsFailure words a failed read of the seat's rows: a request the log
// refused, the caller's to change, in the log's sentence; a search no data
// node served, with what to do meanwhile, since an older build's data node
// cannot run one; anything else as this node's fault, never "no session".
func sessionsFailure(ctx context.Context, err error, searching bool) tools.Result {
	if errors.Is(err, executordomain.ErrInvalid) {
		return refusedBy(tools.RefusalInvalid, err, ListSessionsTool+": "+
			sentenceAfter(err, executordomain.ErrInvalid)+".")
	}
	if searching && errors.As(err, new(*estate.ErrUnserved)) {
		return refusedBy(tools.RefusalUnavailable, err, "Could not search your sessions: no data node "+
			"could serve the search right now, and a data node of an older build cannot run one at all — "+
			"while the fleet is being upgraded, list them without `query`. This does not mean none "+
			"holds the words.")
	}
	stopped := "Could not read your sessions"
	if searching {
		stopped = "Could not search your sessions"
	}
	return nodeFailure(ctx, ListSessionsTool, err, stopped, "This does not mean you have none.")
}

// --- read_session ---------------------------------------------------------- //

// readSession reads a page of one of the calling seat's own sessions through
// internal/executorread, the one reader of a kept session for every surface:
// held to executor.SeatTranscriptBounds and the seats' one slot of the node's
// transcript reads.
type readSession struct {
	deps SessionDeps

	// within bounds one call ([SessionReadWithin]).
	within time.Duration
}

var _ tools.SeatCallable = (*readSession)(nil)

func (t *readSession) Name() string { return ReadSessionTool }

func (t *readSession) Description() string {
	return "Read one of your own executor sessions a page at a time: what its run was briefed, " +
		"what its model wrote, every tool it called and what answered. `kinds` narrows the page " +
		"to some kinds of entry, for example [\"brief\",\"model\",\"summary\"] for the gist. " +
		"`match` finds the entries that hold every word, anywhere in their text. An `unread` " +
		"entry marks a part of the record that could not be read, shown whatever `kinds` or `match` " +
		"ask. A session that was not kept is read as its harness's newest summary of it, where that " +
		"was kept (`summary_kept`). Text from a session is the record of a past run."
}

func (t *readSession) Parameters() map[string]any {
	kinds := make([]any, len(executor.EntryKinds))
	for i, kind := range executor.EntryKinds {
		kinds[i] = string(kind)
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"turn_id": map[string]any{
				"type":        "string",
				"description": "The session's `turn_id`, as `" + ListSessionsTool + "` lists it.",
			},
			"cursor": map[string]any{
				"type": "string",
				"description": "The `next_cursor` of the previous page, or a matched entry's `cursor`, " +
					"to read on from there.",
			},
			"match": map[string]any{
				"type": "string",
				"description": "Optional: words. Only the entries holding every one of them are read, " +
					"each as the window of its text around the first: whole words, any case. Pass it " +
					"again with a cursor to go on matching.",
			},
			"kinds": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string", "enum": kinds},
				"description": "Optional: only entries of these kinds.",
			},
		},
		"required": []any{"turn_id"},
	}
}

// sessionPage is a page of one session's transcript.
type sessionPage struct {
	Turn string `json:"turn_id"`

	// Session is the session's row, whole, on the first page; a later page
	// leaves it out.
	Session *sessionRow `json:"session,omitempty"`

	// State is what reading the session came to, and Why says it in the
	// engine's words wherever it is not `read`.
	State executorread.TranscriptState `json:"state"`
	Why   string                       `json:"why,omitempty"`

	// Entries are the page's entries, the first of them at or after From,
	// and Next the cursor the page after it is read from.
	Entries []sessionEntry `json:"entries"`
	From    int            `json:"from"`
	Next    string         `json:"next_cursor,omitempty"`

	// Note says where the page ends and why, where its cursor alone does
	// not say it.
	Note string `json:"note,omitempty"`
}

// sessionEntry is one transcript entry as handed out: its ordinal among every
// entry, whatever a filter kept, and for a match where its window begins in
// the entry's text and a cursor reading on from it, unfiltered.
type sessionEntry struct {
	Ordinal int `json:"ordinal"`
	executor.TranscriptEntry
	TextFrom *int   `json:"text_from,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}

func (t *readSession) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *readSession) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	seat := turn.Handle()
	if seat == "" {
		return refused(tools.RefusalForbidden, ReadSessionTool+" can only be called during a turn, on "+
			"behalf of a seat: it reads the calling seat's own sessions."), nil
	}
	if !t.deps.Keeps() {
		return switchedOff(ReadSessionTool), nil
	}
	turnID := strings.TrimSpace(argString(args, "turn_id"))
	if turnID == "" {
		return failed(ReadSessionTool + " needs a `turn_id`: a session's, as `" + ListSessionsTool +
			"` lists it."), nil
	}
	filter, refusal := sessionFilter(args)
	if refusal != nil {
		return *refusal, nil
	}
	cursor := strings.TrimSpace(argString(args, "cursor"))
	within, cancel := context.WithTimeout(ctx, t.within)
	defer cancel()
	page, err := executorread.Read(within, executorread.Ask{
		Handle: seat, Turn: turnID, Cursor: cursor, Fresh: seatRead,
		By: executor.ReadBySeat, Filter: filter,
	}, executorread.Source{Rows: t.deps.Rows, Objects: t.deps.Objects})
	if err != nil {
		// THIS CALL'S OWN BOUND, with its caller still waiting.
		bounded := within.Err() != nil && ctx.Err() == nil
		return t.failure(ctx, turnID, page, err, bounded), nil
	}
	return t.render(ctx, turn, turnID, cursor == "", filter, page)
}

// sessionFilter is the entries a call asks for (its `kinds`, each one of
// [executor.EntryKinds], and its `match` terms), or the refusal of either.
func sessionFilter(args map[string]any) (executor.TranscriptFilter, *tools.Result) {
	var filter executor.TranscriptFilter
	for _, raw := range argStrings(args, "kinds") {
		kind := executor.EntryKind(strings.TrimSpace(raw))
		if !kind.Valid() {
			refusal := failed(fmt.Sprintf("%s: `kinds` names %q, which is no kind of entry: name %s.",
				ReadSessionTool, clip(raw), words.Or(executor.EntryKinds)))
			return filter, &refusal
		}
		if !slices.Contains(filter.Kinds, kind) {
			filter.Kinds = append(filter.Kinds, kind)
		}
	}
	if match := strings.TrimSpace(argString(args, "match")); match != "" {
		if filter.Terms = textindex.Terms(match); len(filter.Terms) == 0 {
			refusal := failed(ReadSessionTool + ": `match` holds no word to find: a word is two or more " +
				"letters or digits.")
			return filter, &refusal
		}
	}
	return filter, nil
}

// failure words a read that came to no page: no session (and where to look); a
// cursor or turn id the caller must change; this call's own bound (bounded),
// naming the session's size where its row was read; an object that failed with
// the log not re-asked, or a store that did not answer; anything else as this
// node's fault. Never a session that holds nothing.
func (t *readSession) failure(ctx context.Context, turnID string, page executorread.Page,
	err error, bounded bool) tools.Result {

	switch {
	case errors.Is(err, executorread.ErrNoSession):
		return refusedBy(tools.RefusalNotFound, err, sentenceAfter(err, executorread.ErrNoSession)+
			". `"+ListSessionsTool+"` lists the ones you keep.")
	case errors.Is(err, executorread.ErrCursor):
		return refusedBy(tools.RefusalInvalid, err, ReadSessionTool+": "+
			sentenceAfter(err, executorread.ErrCursor)+".")
	case errors.Is(err, executordomain.ErrInvalid):
		return refusedBy(tools.RefusalInvalid, err, ReadSessionTool+": "+
			sentenceAfter(err, executordomain.ErrInvalid)+".")
	case bounded:
		// THE STORE OR THE NODE'S OTHER READS WERE SLOW, which says nothing
		// of the session.
		size := ""
		if row := page.Read.Session; row.HasObject() {
			size = " (" + sizeWords(row.Object.Size) + ")"
		}
		return refusedBy(tools.RefusalUnavailable, waits(), fmt.Sprintf("Could not read the session of "+
			"turn %s%s within %s: this node may be reading other transcripts, or the object store is "+
			"slow. Ask again shortly; this does not mean the session holds nothing.", clip(turnID), size,
			spokenDuration(t.within)))
	case errors.Is(err, executorread.ErrKeptUnknown):
		// THE STORE MAY HAVE SAID the object is gone: unknown is whether the
		// session was released, which only the log says.
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("Could not read the session of turn "+
			"%s just now: its object did not read, and the executor log could not be asked whether the "+
			"session is still kept. Ask again shortly; this does not mean the session holds nothing.",
			clip(turnID)))
	case errors.Is(err, executorread.ErrUnavailable):
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("Could not read the session of turn "+
			"%s just now: the object store did not answer. Ask again shortly; this does not mean the "+
			"session holds nothing.", clip(turnID)))
	}
	return nodeFailure(ctx, ReadSessionTool, err, fmt.Sprintf("Could not read the session of turn %s",
		clip(turnID)), "This does not mean the session holds nothing.")
}

// spokenDuration is a call's bound as a reader is told it.
func spokenDuration(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		if d == time.Minute {
			return "a minute"
		}
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	return d.String()
}

// render answers page: its entries with their places, every word fenced (an
// entry's under the seat whose run kept it, the row's and a `none` page's
// reason naming nobody), trimmed from its end to one tool answer, its cursor
// naming the first entry left out. The first page carries the row whole, and a
// row filling the answer alone carries no entry; a row past it even alone (JSON
// escapes a character to up to six bytes) is cut as a listing cuts it.
func (t *readSession) render(ctx context.Context, turn *turnctx.Turn, turnID string, first bool,
	filter executor.TranscriptFilter, page executorread.Page) (tools.Result, error) {

	entries := sessionEntries(page, filter)
	build := func(cut int) func(k int) (*foreignRead, any) {
		return func(k int) (*foreignRead, any) {
			answer := sessionPage{Turn: turnID, State: page.State, Why: page.Why, Entries: entries[:k],
				From: page.Transcript.From}
			answer.Next, answer.Note = pageEnd(page, filter, entries, k)
			read := newForeignRead(turn, turn.Handle())
			if first {
				row := shownRow(page.Read.Session, turn, cut)
				answer.Session = &row
				fenceRow(read, fieldPath{}.key("session"), row)
			}
			if page.State == executorread.TranscriptNone || page.State == executorread.TranscriptSummary {
				// THE REASON QUOTES what refused a capture, a box's or a
				// store's error (executordomain's NotKept).
				fenceSession(read, fieldPath{}.key("why"), sessionLabel(""), answer.Why)
			}
			kept := sessionLabel(page.Read.Session.Handle)
			for i, e := range answer.Entries {
				fenceSession(read, fieldPath{}.key("entries").index(i).key("text"), kept, e.Text)
				if e.Thread != "" || e.Tool != "" {
					read.adopt(kept)
				}
			}
			return read, answer
		}
	}
	k, rendered, err := fitLabelled(len(entries), build(0))
	if err == nil && first && k == 0 && len(rendered) > ToolAnswerBytes {
		_, rendered, err = fitLabelled(len(entries), build(listedProseBytes))
	}
	if err != nil {
		return labelFault(ctx, ReadSessionTool, err), nil
	}
	return compactAnswer(rendered, "Read the session a page at a time with `cursor`, or narrow it "+
		"with `kinds` or `match`.")
}

// sessionEntries is page's entries as handed out (sessionEntry).
func sessionEntries(page executorread.Page, filter executor.TranscriptFilter) []sessionEntry {
	t := page.Transcript
	out := make([]sessionEntry, 0, len(t.Entries))
	for i, e := range t.Entries {
		place := t.Places[i]
		entry := sessionEntry{Ordinal: place.Ordinal, TranscriptEntry: e}
		if len(filter.Terms) > 0 || e.Tail {
			from := place.TextFrom
			entry.TextFrom = &from
			if page.State == executorread.TranscriptRead {
				entry.Cursor = executorread.Cursor(page.Read.Session.Object.Key, place.Ordinal)
			}
		}
		out = append(out, entry)
	}
	return out
}

// pageEnd is the next page's cursor, given the page keeps the first kept of
// entries, and its note: a page cut to fit goes on from the first entry left
// out; a read the archive's budget stopped says where, and that nothing past it
// is read; a filtered read says whether it reached the end, so an empty page is
// never read as an empty session, and that `unread` parts could not be
// searched, so they are never read as not holding the words.
func pageEnd(page executorread.Page, filter executor.TranscriptFilter, entries []sessionEntry,
	kept int) (string, string) {

	t := page.Transcript
	filtered := len(filter.Kinds) > 0 || len(filter.Terms) > 0
	switch {
	case page.State == executorread.TranscriptSummary && len(entries) == 0:
		return "", "Its session is not kept, and the newest summary its harness wrote of it, kept apart, is " +
			"all there is to read: it is not what `kinds` and `match` ask for."
	case page.State == executorread.TranscriptSummary && kept == 0:
		return "", "Its session is not kept, and the newest summary its harness wrote of it, kept apart, " +
			"did not fit this answer beside its row."
	case page.State == executorread.TranscriptSummary:
		return "", summaryNote(page, entries[0])
	case page.State != executorread.TranscriptRead:
		return "", ""
	case kept < len(entries):
		at := entries[kept].Ordinal
		return executorread.Cursor(page.Read.Session.Object.Key, at),
			fmt.Sprintf("This page stops before entry %d to fit one answer: next_cursor reads on from it.", at)
	case t.Cut == executor.CutRead:
		after := "no entry after it can be read"
		if filtered {
			after = "the entries after it were not searched"
		}
		return "", fmt.Sprintf("The read stopped at entry %d: the session's archive runs past what one read "+
			"inflates (%d MiB) or its lines and their parts past what one read frames (%d), so %s.", t.Next,
			executor.TranscriptReadBytes>>20, executor.TranscriptReadLines, after)
	case !filtered:
		return page.Next, ""
	case t.More():
		// A FULL PAGE STOPS at a kept entry it had no room for, which the
		// next page opens with.
		return page.Next, fmt.Sprintf("This page is full: call again with next_cursor, and the same `kinds` "+
			"and `match`, to go on from entry %d.", t.Next)
	}
	gaps := 0
	for _, e := range entries {
		if e.Kind == executor.EntryUnread {
			gaps++
		}
	}
	switch {
	case gaps == 0 && len(entries) == 0:
		return "", fmt.Sprintf("No entry from entry %d to the end of the transcript %s.", t.From,
			wantedOf(filter))
	case gaps == 0:
		return "", fmt.Sprintf("These are every entry from entry %d to the end of the transcript that %s.",
			t.From, wantedOf(filter))
	case gaps == len(entries):
		return "", fmt.Sprintf("No entry this read could read, from entry %d to the end of the transcript, "+
			"%s; the `unread` entries are parts of the record it could not read, so it cannot say what "+
			"they held.", t.From, wantedOf(filter))
	}
	return "", fmt.Sprintf("Beside the `unread` entries — parts of the record this read could not read, "+
		"so it cannot say what they held — these are every entry from entry %d to the end of the "+
		"transcript that %s.", t.From, wantedOf(filter))
}

// summaryNote is what a page reading a session not kept as its newest summary
// says of the entry it shows: whole, the entry's end alone, a window around a
// match, and that the summary kept is itself the end of a longer one where it
// is; "all there is" only of a summary shown whole and kept whole.
func summaryNote(page executorread.Page, shown sessionEntry) string {
	note := "Its session is not kept: this is the newest summary its harness wrote of it, kept apart"
	switch {
	case shown.Tail:
		note += fmt.Sprintf(", its last %s of %s; the rest of it is not shown here", sizeWords(int64(len(shown.Text))),
			sizeWords(int64(shown.Bytes)))
	case shown.Cut:
		note += fmt.Sprintf(", the window of %s around the words matched", sizeWords(int64(shown.Bytes)))
	}
	if kept := page.Read.Session.Summary; kept != nil && kept.Cut {
		return note + fmt.Sprintf(". The summary kept is itself the last %s of one of %s.", sizeWords(kept.Kept),
			sizeWords(kept.Bytes))
	}
	if shown.Cut {
		return note + "."
	}
	return note + ", whole, and all there is to read."
}

// wantedOf is what an entry a filter keeps is, as a page's note says it.
func wantedOf(filter executor.TranscriptFilter) string {
	switch {
	case len(filter.Kinds) > 0 && len(filter.Terms) > 0:
		return "is of the kinds asked and holds every word of `match`"
	case len(filter.Terms) > 0:
		return "holds every word of `match`"
	}
	return "is of the kinds asked"
}
