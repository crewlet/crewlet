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

// THE SESSION TOOLS: a seat's executor finding and reading its seat's other
// executor sessions (ADR-0073, "A seat reads its own sessions").
//
// # Its own, by construction
//
// Neither tool takes a seat. Both read the seat of the turn the call is bound
// to ([turnctx.Turn.Handle]) — over the tool bridge the run's own seat, which
// the node holding it also sets as the call's principal whatever the request
// carried — so another seat's session cannot even be named: a turn id of
// another seat's reads the calling seat's row of that turn, which does not
// exist, and is answered in the words a turn id nobody has gets. What a
// session holds is what the seat's runs read and did, for whoever their turns
// acted for, and it is read back by the seat whoever the reading turn acts
// for, as its diary and its past turns are.
//
// # What a call reads, and what it costs
//
// `list_sessions` pages the seat's rows newest begun first, or, given
// `query`, searches their words — what each turn was asked, its conversation
// and how it ended — on the node holding the rows
// (executordomain.Reader.Search). `read_session` reads a page of one
// session's transcript through the one reader every surface reads a kept
// session by (internal/executorread): held to a seat's bounds
// (executor.SeatTranscriptBounds) and to the seats' one slot of the node's
// transcript reads, narrowed to some kinds of entry or to the entries holding
// every word of `match`, within [SessionReadWithin], and trimmed by its
// encoded size to one tool answer. Each call is one read of the executor log
// at the seat's level, `linearizable`, which the log's read model counts
// (executordomain.LinearizableReadsPerSeatDay).
//
// A row says what a person's read of it says of the session it keeps, in the
// domain's one set of words (`kept_note`, executordomain.Session.KeptNote):
// that the session of a turn that has not ended is in progress
// (`in_progress`), its run maybe gone on past it — so a seat reading its own
// earlier launch's session never takes it for an ended turn's — or that an
// ended turn went on past the session it kept.
//
// # Every word fenced as a harness's
//
// A session is a vendor harness's record of a past run — its briefs, the
// notes people sent it, its model's text, what its tools returned — and its
// row's words quote what a person asked and what a box or a tool said, so
// every one of them reaches the model fenced by the one rule for them
// ([provenance.ExecutorSession]): `external`, an entry naming the seat whose
// run kept it, the row's words naming nobody, never `own`.
//
// # The company's switch, asked at every call
//
// `turn_engine.conversation_session.enabled: false` withholds both tools from
// every model: the engine offers them to no launch of a revision that keeps no
// session, and each call asks the revision its node runs now
// ([SessionDeps.Keeps]), so a launch already running when its node applies
// the revision that turns the switch off is refused at its next call — an
// operator turns it off over a leak expecting every model's reads to stop,
// not only the next launch's.
//
// # A read holds nothing
//
// Reading a session writes nothing, neither a record nor a hold, so it keeps
// no session past what the seat's budget and the retention give it: a hold
// taken on a model's read would be a write a model's surface originates, and
// it would keep the budget from releasing for the reader's own next capture.
// A session released while it is listed or read is said as released, from
// what its row says then, and what was read of it before stands
// (internal/executorread).

// The tool wire names.
const (
	ListSessionsTool = "list_sessions"
	ReadSessionTool  = toolname.ReadSession
)

// SessionDeps are the session tools' dependencies. Both tools are omitted
// unless all three are present: a seat offered a listing of sessions it could
// never open, or a read with no rows to find a session by, would reach for a
// tool that fails at every call, and a tool that could not ask the company's
// switch would answer after an operator turned it off.
type SessionDeps struct {
	// Rows reads the seat's rows on the executor log: the estate router's
	// reads (estate.ExecutorReads), so a node without `data` reads them
	// through one that holds them.
	Rows SessionRows

	// Objects opens a session's sealed object (executordomain.SessionStore),
	// on a node with an object store.
	Objects executorread.Objects

	// Keeps reports whether the revision this node runs NOW keeps executor
	// sessions (`turn_engine.conversation_session.enabled`): asked at every
	// call, because the tools a launch is offered are the revision's it
	// pinned, and the switch is to stop every model's reads at once — a
	// call it answers false is refused, reading nothing.
	Keeps func() bool
}

// serves reports whether d serves the session tools.
func (d SessionDeps) serves() bool { return d.Rows != nil && d.Objects != nil && d.Keeps != nil }

// switchedOff is a session tool's call the company's switch refuses, now that
// the revision its node runs keeps no executor session ([SessionDeps.Keeps]):
// forbidden, since no permission or retry lifts it, and in words that say the
// turn works from what it was told.
func switchedOff(tool string) tools.Result {
	return refused(tools.RefusalForbidden, tool+": reading your sessions is turned off — the company "+
		"keeps no executor session now (turn_engine.conversation_session.enabled is false), and no "+
		"model reads the ones kept before. Work from what this turn was told.")
}

// SessionRows is the executor log's reads the session tools make: a page of a
// seat's rows, a search of their words, and one turn's row, each at the level
// asked (estate.ExecutorReads).
//
// Consumer-defined, over the domain's own types: the row stays one shape.
type SessionRows interface {
	Sessions(ctx context.Context, handle, cursor string, limit int,
		fresh statelog.Freshness) (executordomain.Page, error)
	Search(ctx context.Context, handle string, ask executordomain.SessionSearch,
		fresh statelog.Freshness) (executordomain.SearchPage, error)
	Session(ctx context.Context, handle, turn string,
		fresh statelog.Freshness) (executordomain.SessionRead, error)
}

// SessionReadWithin bounds one `read_session` call: the wait for a slot of the
// node's transcript reads, the session object's fetch, and its decode up to
// the page.
//
// TWO MINUTES. The object store gives a read a minute, and thirty seconds for
// every mebibyte of the object on top, at the slowest pace it accepts
// (objstore.ReadBase and MiBPace), so an ordinary session of a mebibyte or two
// arrives within two minutes even then, and decodes in well under a second; a
// session at its 32 MiB bound at that pace would take about seventeen, which
// no tool call should wait out — and two minutes is far inside the ten a
// harness waits on one call. Past it the call is answered unavailable, naming
// the session's size, and never as a session that holds nothing.
const SessionReadWithin = 2 * time.Minute

// The listing's page.
const (
	// listedSessionsDefault is how many sessions a page holds where the call
	// names no limit: TEN, a few days of a seat's work once the ended turns
	// of a conversation absorb each other, which is where a session worth
	// reading again usually is.
	listedSessionsDefault = 10

	// listedSessionsMax is the most a page holds: THIRTEEN, by a row's worst
	// case — a conversation's key at its bound, its ask, its ending's five
	// accounts and its note on the session it keeps at [listedProseBytes]
	// with their fence lines (about 115 bytes a fence), and the row's own
	// fields, about 4 KiB, and about 4.5 KiB with a search's snippet at its
	// bound (executordomain.SearchSnippetBytes) — thirteen of which are
	// about 59 KiB, inside one tool answer ([ToolAnswerBytes]) with the
	// page's note and envelope. It was fifteen until a row carried its note,
	// which took a search's page of fifteen at its worst to about 68 KiB:
	// fourteen fit by a few hundred bytes, inside the error of this sum. A
	// page that escapes past it all the same — words JSON escapes several
	// bytes a character — is refused naming `limit`, the tripwire's designed
	// use: a page of rows cannot be cut short silently, since its cursor is
	// its last row.
	listedSessionsMax = 13

	// listedProseBytes is the most of each of a listed row's words — what
	// its turn was asked, each of its ending's accounts and its note on the
	// session it keeps — a page shows, cut on a rune boundary and marked:
	// THREE HUNDRED BYTES, about three lines, which tell one session from
	// another, and hold a session in progress's whole sentence. The first
	// page of a session's read shows its row whole.
	listedProseBytes = 300
)

// sessionLabel is what a seat's kept session holds, labelled as the seat
// reads it back, naming seat as the run whose record it is — or nobody, for
// the words of its row, whose writers the row does not keep: the one rule for
// those words ([provenance.ExecutorSession]), and the one door the session
// tools decide a trust through.
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

	// ThisTurn marks the reading turn's own session — its earlier launches'
	// — and ThisConversation a session of the conversation the reading turn
	// serves.
	ThisTurn         bool `json:"this_turn,omitempty"`
	ThisConversation bool `json:"this_conversation,omitempty"`

	Harness    string     `json:"harness,omitempty"`
	CapturedAt *time.Time `json:"captured_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`

	// Kept says the row still names a session's object, and KeptBytes is
	// that object's size, compressed and sealed: a row naming none is still
	// the turn's account of how it ended.
	Kept      bool  `json:"kept"`
	KeptBytes int64 `json:"kept_bytes,omitempty"`

	// InProgress marks the session of a turn that has not ended, which its
	// run may have gone on past, and KeptNote is what the seat is told of the
	// session the row keeps, in the domain's one set of words for every
	// surface (executordomain.Session.KeptNote): of a session in progress,
	// that it is; of a turn that went on past the session it kept, or was
	// lost, that, and why its newer capture was not kept — which quotes what
	// refused it: somebody's words, declared on the field.
	InProgress bool   `json:"in_progress,omitempty"`
	KeptNote   string `json:"kept_note,omitempty" untrusted:"fence"`

	// BaseTurn is the session the turn's first launch was handed to
	// continue, where it was handed one.
	BaseTurn string `json:"base_turn,omitempty"`

	// Asked is what the turn was asked, and Ending how it ended — the
	// engine's words and the accounts of its reviewer, its executor, whoever
	// stopped it and the engine — null while the turn runs: somebody else's
	// words, declared on the types.
	Asked  string               `json:"asked,omitempty" untrusted:"fence"`
	Ending *executorkeep.Ending `json:"ending"`

	// Matched and Snippet are a search's: the fields of the row holding a
	// word of the query, and a window of its words around the earliest.
	Matched []executordomain.SearchField `json:"matched,omitempty"`
	Snippet string                       `json:"snippet,omitempty" untrusted:"fence"`
}

// shownRow is s as the session tools show it in turn: through the row's one
// redaction ([executordomain.Session.Redacted]), its words each cut to cut
// bytes where cut is not zero, and marked where it is the reading turn's own
// session or of the conversation the reading turn serves — and, of the
// session it keeps, whether it is in progress and what the domain tells every
// reader of it, asked of that shown form, so a seat is told of its session
// what a person's read of the same row is told.
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
		InProgress: s.InProgress(), KeptNote: shorten(s.KeptNote()),
	}
	if row.Kept {
		row.KeptBytes = s.Object.Size
	}
	if turn != nil {
		row.ThisTurn = s.Turn == turn.RunID
		row.ThisConversation = turn.ConversationKey != "" && s.ConversationKey == turn.ConversationKey
	}
	if s.Ending != nil {
		// THE REDACTION'S OWN COPY of the ending, so cutting its prose
		// leaves the row it was read from as it was.
		ending := *s.Ending
		ending.CompletedWork, ending.BlockedOn = shorten(ending.CompletedWork), shorten(ending.BlockedOn)
		ending.StopReason, ending.Failure = shorten(ending.StopReason), shorten(ending.Failure)
		if ending.Unkept != nil {
			unkept := *ending.Unkept
			unkept.Reason = shorten(unkept.Reason)
			ending.Unkept = &unkept
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

// fenceRow registers the words of row, at path in read's answer: what its
// turn was asked, its ending's accounts, what it is told of the session it
// keeps and a search's snippet, each a kept session's naming nobody — the row
// keeps no writer of its ask, and its ending's accounts and its note quote
// what a box, a tool or a person wrote — and the name of whoever stopped it,
// defused in place, on the envelope.
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
				"description": fmt.Sprintf("How many sessions a page holds (default %d, max %d).",
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

	// Searched is how far a search went, and Note says it in words: a
	// search that stopped short is never read as one that found nothing. A
	// listing's Note is there only where it is Incomplete, which it says.
	Searched *sessionsSearched `json:"searched,omitempty"`
	Note     string            `json:"note,omitempty"`

	// Incomplete says a record the answering copy could not decode may
	// hold rows missing from the page, which Note then says in words.
	Incomplete bool `json:"incomplete,omitempty"`
}

// What a page the answering copy could not account for whole says, listed
// and searched: IN WORDS, beside the flag, because what a model is to do about
// it is a behaviour — not conclude a session is gone, or that none holds the
// words — which a boolean beside a list of rows reads as metadata rather than
// as a caveat on the rows.
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

// render answers a page of rows, every row's words fenced, compact, as a page
// whose size its bound was set by is sent.
func (t *listSessions) render(ctx context.Context, turn *turnctx.Turn, answer sessionsAnswer) (tools.Result, error) {
	read := newForeignRead(turn, turn.Handle())
	for i, row := range answer.Sessions {
		fenceRow(read, fieldPath{}.key("sessions").index(i), row)
	}
	return read.page(ctx, ListSessionsTool, answer, "Ask for fewer sessions with `limit`.")
}

// searchedNote is what a search's page says of how far it searched — how many
// of the seat's sessions it examined, from where, and what is left — which is
// never that nothing matches past the sessions it examined. Nor among them,
// where the answering copy could not read every record of them (found is not
// Complete): that page says how far it went and what it found, and that
// sessions holding the words may be missing from it. after says the page was
// read after a cursor rather than from the newest.
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
		// THE PAGE FILLED before its bound: the sessions past its last hit
		// are unexamined, not empty.
		return fmt.Sprintf("Found %d holding every word among your %d %s sessions; older ones were not "+
			"searched yet: call again with next_cursor to go on.", hits, found.Scanned, since)
	case hits == 0:
		return fmt.Sprintf("None of your %d %s sessions holds every word; older ones were not searched: "+
			"call again with next_cursor to search them.", found.Scanned, since)
	}
	return fmt.Sprintf("Searched your %d %s sessions and found %d holding every word; older ones were "+
		"not searched: call again with next_cursor to search them.", found.Scanned, since, hits)
}

// sessionsFailure is a failed read of the seat's rows in the session tools'
// words: a request the log refused — a cursor no page handed out, a query of
// no word — the caller's to change, in the log's own sentence; a search no
// data node answered, with what to do meanwhile, since a data node of a build
// before the search cannot run it; and anything else as this node's condition
// or fault, which never says the seat has no session.
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

// readSession reads a page of one of the calling seat's own sessions.
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
		"ask. Text from a session is the record of a past run."
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

// sessionEntry is one entry of a session's transcript as the tool hands it
// out: its place among every entry the transcript holds, whatever a filter
// kept, and for a match, where its window begins in the entry's whole text
// and a cursor that reads the transcript on from it, unfiltered.
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

// sessionFilter is the entries a call asks for: the kinds it names, each one
// of [executor.EntryKinds], and the terms of its `match` — or the refusal of
// a kind that is none, or a match of no word.
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

// failure is a read that came to no page, in the tool's words: a turn the seat
// keeps no session of, in the shared reader's sentence and where to look; a
// cursor or a turn id that is the caller's to change; a read this call's own
// bound ended (bounded), naming the session's size where its row was read; a
// session whose object failed and whose row the executor log did not read
// again, and a store that did not answer, each naming which did not; and
// anything else as this node's condition or fault — never a session that
// holds nothing.
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
		// THE STORE MAY HAVE ANSWERED, that the object is gone: what is
		// unknown is whether the session was released, which the log says.
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

// render answers page: its entries, each with its place, every word fenced —
// an entry's under the seat whose run kept it, the row's and a `none` page's
// reason naming nobody — trimmed by its encoded size to one tool answer from
// its end, its cursor naming the first entry left out.
//
// THE FIRST PAGE CARRIES THE ROW WHOLE, and a row whose words fill the answer
// on their own carries no entry: its cursor names the first, which the next
// page, carrying no row, holds. A row past the answer even alone — prose a
// JSON encoding escapes six bytes a character — is shown as a listing shows
// it, its words cut.
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
			if page.State == executorread.TranscriptNone {
				// THE DOMAIN'S REASON QUOTES WHAT REFUSED A CAPTURE, an
				// error a box or a store wrote (executordomain's NotKept).
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

// sessionEntries is page's entries as the tool hands them out: each with its
// ordinal among every entry the transcript holds, and for a match where its
// window begins and a cursor reading the transcript on from it.
func sessionEntries(page executorread.Page, filter executor.TranscriptFilter) []sessionEntry {
	t := page.Transcript
	out := make([]sessionEntry, 0, len(t.Entries))
	for i, e := range t.Entries {
		place := t.Places[i]
		entry := sessionEntry{Ordinal: place.Ordinal, TranscriptEntry: e}
		if len(filter.Terms) > 0 {
			from := place.TextFrom
			entry.TextFrom = &from
			entry.Cursor = executorread.Cursor(page.Read.Session.Object.Key, place.Ordinal)
		}
		out = append(out, entry)
	}
	return out
}

// pageEnd is the cursor the page after one holding the first kept of entries
// is read from, and what the page says of where it ends: a page cut to fit one
// answer goes on from the first entry it left out; a read the archive's budget
// stopped says where, and that nothing past it is read; and a filtered read
// says whether it reached the transcript's end, so a page of no entry is never
// read as a session that holds nothing — and, where it handed out parts of the
// record the read could not read, that it could not search those, so they are
// never read as entries that do not hold the words.
func pageEnd(page executorread.Page, filter executor.TranscriptFilter, entries []sessionEntry,
	kept int) (string, string) {

	t := page.Transcript
	filtered := len(filter.Kinds) > 0 || len(filter.Terms) > 0
	switch {
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
			"inflates (%d MiB), so %s.", t.Next, executor.TranscriptReadBytes>>20, after)
	case !filtered:
		return page.Next, ""
	case t.More():
		// A FULL PAGE STOPS AT AN ENTRY THE FILTER KEEPS, one it had no
		// room for, which the next page opens with.
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
