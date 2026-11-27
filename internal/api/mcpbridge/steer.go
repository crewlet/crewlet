package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A NOTE INBOX: a person's notes to the run's turn, at the run's own steps.
// A run's session carries its note inbox ([Session.Steer]), which the bridge
// serves two ways behind the run's own token: the steer_take tool
// ([SteerTool]) a hook calls, never listed and always answered text, and the
// notes route ([SteerPattern]) a plugin polls and confirms reads on. The
// bridge decides nothing about notes; the package doc's "A note inbox" has
// the argument and what was measured.

// SteerTool is the tool a harness's hook calls on the bridge for its turn's
// notes.
const SteerTool = "steer_take"

// The hook events steer_take answers: Claude Code's boundaries where every call
// has its answer — after a batch of tool calls, and as the run is about to
// finish. Any other event is answered with nothing.
const (
	SteerEventToolBatch = "PostToolBatch"
	SteerEventFinish    = "Stop"
)

// SteerInbox is a run's notes as its bridge session serves them. Declared here,
// by the consumer; the engine implements it.
type SteerInbox interface {
	// Take answers one hook of the run's main agent, at event
	// ([SteerEventToolBatch] or [SteerEventFinish]): the hook's whole answer,
	// in the shape the harness reads, or empty for nothing. Every note
	// answered is recorded SERVED to the run's job first, and read only once
	// the job's session shows it. Never an error: a note that could not be
	// served waits for the next step. call is the hook's call as the box sent
	// it, nil where the bridge could not read it; ctx ends with the hook's
	// request, cancelled where its harness gave the call up.
	Take(ctx context.Context, event string, call *sandbox.NoteCall) string

	// Cancelled says the box's harness gave up on the hook call named:
	// whatever it was answered, it never took it. Never blocks on the store.
	Cancelled(ctx context.Context, call sandbox.NoteCall)

	// Wait is the notes waiting past the cursor after, oldest first, and a
	// channel closed when the inbox's view of them next changes or the inbox
	// closes. Nothing is recorded READ; every note answered is recorded SERVED
	// to the run's job first, and one that cannot be is not answered, so a
	// note the harness may deliver is never one the record says nothing of.
	Wait(ctx context.Context, after int) ([]SteerNote, <-chan struct{})

	// Read records the notes named read at the run's next step — the harness
	// reported putting them in front of its model — and reports each read
	// there. A note no longer waiting is not recorded again, and one an
	// earlier read of the same job recorded is reported again, as the same
	// record; an error is a read that could not be confirmed written, and its
	// notes wait, or, where the write landed, are reported when the harness
	// confirms them again. An inbox that serves no job, not yet bound or
	// closed, takes no read and answers [ErrNoteUntaken], and so does one
	// whose job the run's record takes no read of any more, for a note named
	// that still waits. So does one that cannot open a note named that still
	// waits for the job, its node not holding the key the note is sealed
	// under, once it has taken every other note named.
	Read(ctx context.Context, ids []string) error

	// Again grants the job's harness the second admission of each note named
	// that a compaction folded: the id to admit it under, by note id, spending
	// one of its serves; and spent, the notes at their budget, which the
	// harness gives up. A note in neither is never admitted again (a wake, a
	// run's message, one the job was not served). Turned away as a read is
	// ([ErrNoteUntaken]); an error grants nothing it says.
	Again(ctx context.Context, ids []string) (granted map[string]string, spent []string, err error)

	// Folded says the notes named, which the job's harness gave up once a
	// compaction folded each and it could not be admitted again: never
	// confirmed, each waits for the review. It records nothing, and takes
	// only a note the job holds; never blocks on the store.
	Folded(ctx context.Context, ids []string)

	// Close ends the inbox: its watch stops, every channel [SteerInbox.Wait]
	// handed out closes, every later Take and Wait answers nothing, and every
	// later Read [ErrNoteUntaken].
	Close()
}

// ErrNoteUntaken is what a [SteerInbox] answers a [SteerInbox.Read] with
// where it left a note the read names untaken that the read made again can
// have taken: an inbox that serves no job that takes notes (closed, not yet
// bound, or bound to a job the run's record takes no read of any more, a
// named note still waiting there), or one whose node does not hold the key a
// named note is sealed under, which recorded every other note named. Declared
// here, by the consumer, which tells it from a write that could not be
// confirmed: the read is turned away to ask again ([Bridge.serveSteer]), its
// next try reaching the inbox that serves the job now, the run that goes on
// once a claim is handed back, or a node that holds the key. The package
// doc's "A note inbox" has each case.
var ErrNoteUntaken = errors.New("mcpbridge: the run's note inbox left a note the read names untaken, for the read made again")

// SteerNote is one waiting note as the notes route answers it.
type SteerNote struct {
	// Seq is the note's place among the turn's notes: what a poller passes
	// back as its cursor.
	Seq int `json:"seq"`

	// ID is the note's identity, which a read names it by.
	ID string `json:"id"`

	// Inbox is the id the harness admits the note under, unique to the note and
	// the job, so a poller that admits it twice admits it once.
	Inbox string `json:"inbox"`

	// Message is the note as the model reads it.
	Message string `json:"message"`
}

// SteerPattern is the notes route's mux pattern, under the bridge's own prefix
// so every gate that covers the bridge — the exemption from the API's
// credential guard, the public listener, serving under drain, the access log's
// redaction of the token — covers it by prefix.
const SteerPattern = PathPrefix + "{token}/steer/{verb}"

// steerSuffix joins a job's endpoint to its notes route.
const steerSuffix = "/steer"

// SteerURL is the notes route of the job whose endpoint is given — what the
// engine hands a run's harness beside the endpoint itself.
func SteerURL(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	return endpoint + steerSuffix
}

// steerTakeBudget bounds the inbox's answer to one hook: FIVE SECONDS, half
// the ten a hook that calls steer_take is given. A harness waits on its hook before
// its next request, at every batch, so the hook's own timeout is short, and
// half of it answers a note the inbox took before the harness gives up on the
// hook, unless the network itself stalls past five seconds. Measured on Claude
// Code: at its timeout the harness cancels the call (notifications/cancelled) and
// discards a late answer, and a take still in flight is cancelled with it.
const steerTakeBudget = 5 * time.Second

// pollHold is how long a held poll is held: a poll for notes while none waits,
// and a browser's poll until its answer moves ([Bridge.pollBrowser]).
//
// TEN SECONDS, under the tightest common default between a box and the bridge:
// Envoy's route timeout, fifteen seconds when unset — a gateway in front of the
// bridge cuts a request held longer, and a poll cut mid-hold is a retry and a
// log line for nothing. Inside the fleet relay's idle bound
// (internal/httprelay's Idle, three heartbeats) for a poll relayed to the
// seat's holder. Its cost is one request every ten seconds per idle run whose
// harness or browser polls, answered from memory.
const pollHold = 10 * time.Second

// steerReadIDs is the most notes one read names: everything one job can be
// owed, a turn's notes ([steer.MaxTurnNotes]) and the job's own items, its late
// calls' ([sandbox.MaxJobItems]) and its wakes' ([sandbox.MaxJobWakes]).
// OpenCode's plugin names every note it owes in one read and sends it again on
// every retry, so a read refused for its count is refused again forever and
// confirms nothing more for the rest of the run.
const steerReadIDs = steer.MaxTurnNotes + sandbox.MaxJobItems + sandbox.MaxJobWakes

// steerReadBody bounds a read's body: a read names at most [steerReadIDs]
// notes, each a short id, a couple of kilobytes in all, and 64 KiB is far past
// that while far inside what the relay carries (internal/httprelay's MaxBody).
const steerReadBody = 64 << 10

// steerRead is a read's body.
type steerRead struct {
	IDs []string `json:"ids"`
}

// steerNotes is a poll's answer.
type steerNotes struct {
	Notes []SteerNote `json:"notes"`
}

// SteerHandler serves the notes route ([SteerPattern]): the same routing as
// [Bridge.Handler] — served here by the node holding the job's opening, and
// relayed to it by any other — with the notes served by the opening's inbox.
func (b *Bridge) SteerHandler() http.Handler { return b.routed(b.serveSteer, isNotesPoll) }

// serveSteer answers one call of the notes route on the opening that holds it.
//
// NO ADMISSION OF ITS OWN: the token is judged by the routing it shares with
// the MCP endpoint ([Bridge.routed]) before it is reached, or by the relay's
// admission for a relayed call ([Bridge.admitRelayed]).
//
// A CALL THAT REACHES ITS OPENING AS IT ENDS IS TURNED AWAY TO ASK AGAIN, a
// 503 with a Retry-After, as a poll the opening can no longer hold is
// ([Bridge.pollNotes]), and so is a read the end overtakes ([readNotes]):
// NEVER THE 404 of a session with no inbox, which ends the OpenCode plugin's
// asking for the rest of its run (package doc, "A note inbox").
func (b *Bridge) serveSteer(w http.ResponseWriter, r *http.Request, opened *binding) {
	if opened.session.Steer == nil {
		// A SESSION WITH NO INBOX has no notes route at all.
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	}
	inbox := opened.inbox()
	if inbox == nil {
		// THE OPENING ENDED, which took the inbox its session has.
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(askAgainRetry))
		return
	}
	switch verb := r.PathValue("verb"); {
	case verb == "notes" && r.Method == http.MethodGet:
		b.pollNotes(w, r, opened, inbox)
	case verb == "read" && r.Method == http.MethodPost:
		readNotes(w, r, opened, inbox)
	case verb == "again" && r.Method == http.MethodPost:
		againNotes(w, r, opened, inbox)
	case verb == "folded" && r.Method == http.MethodPost:
		foldedNotes(w, r, inbox)
	case verb == "notes" || verb == "read" || verb == "again" || verb == "folded":
		httpjson.Fail(w, http.StatusMethodNotAllowed, httpjson.CodeMethodNotAllowed)
	default:
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
	}
}

// pollNotes answers the notes waiting past the poll's cursor, holding the poll
// for up to [pollHold] while none does. It returns at once on a change, the
// opening's end, the bridge's shutdown and the poller going away.
//
// A RUN WHOSE TOOLS ARE CLOSED is answered as one with nothing waiting, as its
// steer_take is ([binding.takeSteer]); asked at every pass, since the fence
// closes while a poll is held. A POLL THIS OPENING CANNOT HOLD IS REFUSED, never answered as an empty one:
// once the bridge shuts down or the opening ends, a held poll is let go and
// later ones get a 503 and Retry-After ([askAgainRetry]). Answered 200 with
// nothing, a poller whose next poll answers its last (the OpenCode plugin's)
// would ask again at once for most of a graceful stop. A failure is what a
// poller pauses on; the notes wait on the run's record for the next opening.
func (b *Bridge) pollNotes(w http.ResponseWriter, r *http.Request, opened *binding, inbox SteerInbox) {
	after := 0
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidQuery)
			return
		}
		after = n
	}
	ctx := r.Context()
	hold := time.NewTimer(b.hold)
	defer hold.Stop()
	for {
		switch {
		case b.shuttingDown():
			httpjson.Unavailable(w, httpjson.CodeDraining, httpjson.RetrySeconds(askAgainRetry))
			return
		case opened.over():
			httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(askAgainRetry))
			return
		}
		notes, changed := inbox.Wait(ctx, after)
		if fence(opened.session) != nil {
			notes = nil
		}
		if len(notes) > 0 {
			writeNotes(w, notes)
			return
		}
		select {
		case <-changed:
		case <-hold.C:
			writeNotes(w, nil)
			return
		case <-b.shutdown:
			// Refused at the top of the next pass.
		case <-ctx.Done():
			return
		}
	}
}

// isNotesPoll reports whether r is a poll of the notes route ([SteerPattern]):
// the one call of that route that WAITS for something to happen, rather than
// for something to be done.
func isNotesPoll(r *http.Request) bool {
	return r.Method == http.MethodGet && r.PathValue("verb") == "notes"
}

// forwardPoll relays a poll this node holds no opening for to the session's
// holder ([Bridge.forward]), a notes poll or a browser's — and lets go of it
// the moment this bridge shuts down, answering it as a poll held here is
// answered then: a 503 the poller pauses on ([Bridge.pollNotes]).
//
// A RELAYED POLL IS HELD BY TWO NODES, the holder and this one waiting on its
// answer, and only the holder's shutdown released it. A stopping node relays
// its polls longest (its seats go to peers, a direct box keeps dialling its
// loopback, ADR-0065), and one it kept waiting on held its listener for the
// whole grace and was then cut, which a poller reads as a network error, not
// the answer to pause on. Only the poll: a relayed tool call or read may
// already be running on the holder, and is left to finish.
func (b *Bridge) forwardPoll(w http.ResponseWriter, r *http.Request, sub subject, f *fleet) {
	if b.shuttingDown() {
		httpjson.Unavailable(w, httpjson.CodeDraining, httpjson.RetrySeconds(askAgainRetry))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		select {
		case <-b.shutdown:
			cancel()
		case <-ctx.Done():
		}
	}()
	answered := &statusWriter{ResponseWriter: w}
	// THE RELAY TELLS THE HOLDER THE EXCHANGE IS OVER when its context ends
	// (internal/httprelay), so the holder lets go of its side as well.
	b.forward(answered, r.WithContext(ctx), sub, f)
	if !answered.wrote && b.shuttingDown() {
		httpjson.Unavailable(w, httpjson.CodeDraining, httpjson.RetrySeconds(askAgainRetry))
	}
}

// statusWriter is a response writer that says whether a status was written
// through it — the relay's pump writes one only once the holder answers.
type statusWriter struct {
	http.ResponseWriter
	wrote bool
}

func (s *statusWriter) WriteHeader(status int) {
	s.wrote = true
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// Unwrap hands [http.ResponseController] the writer beneath, so the relay's
// flushes reach the client.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// askAgainRetry is the Retry-After a call is turned away with to ask again: a
// poll the bridge cannot hold, a notes or browser call that reached its
// opening as it ended ([Bridge.serveSteer], [Bridge.serveBrowser]), and a
// read its inbox did not take ([ErrNoteUntaken]) or that a run whose tools are
// closed takes no more ([readNotes]): the pace the bridge's own relay reads a
// seat's lease again at while it waits for the seat's next holder
// ([holderRetryCeiling]), because that holder is what serves the session's
// next opening: a node that is going hands its seats on, and an opening that
// ended was replaced, moved with its seat, or ended with its run, whose route
// then answers 401. A box's browser relay waits it out; OpenCode's notes
// plugin reads no Retry-After and pauses on a doubling of its own.
const askAgainRetry = holderRetryCeiling

func writeNotes(w http.ResponseWriter, notes []SteerNote) {
	if notes == nil {
		notes = []SteerNote{}
	}
	httpjson.Write(w, http.StatusOK, steerNotes{Notes: notes})
}

// readNotes records the notes a read names through the opening's inbox.
//
// A READ IS RECORDED OR TURNED AWAY TO ASK AGAIN, NEVER ANSWERED DONE WITH
// NOTHING RECORDED, since the plugin would never confirm its notes again: as
// a read reaching the opening after its end is ([Bridge.serveSteer]), at
// either of its last two steps. A RUN WHOSE TOOLS ARE CLOSED records no read
// (see [Bridge.pollNotes]), its opening ended or not, and AN INBOX THAT
// SERVES NO JOB, whose job takes no read any more, or that lacks a named
// note's key says it took the read nowhere ([ErrNoteUntaken]). The package
// doc's "A note inbox" has why each is turned away.
func readNotes(w http.ResponseWriter, r *http.Request, opened *binding, inbox SteerInbox) {
	body, ok := noteIDs(w, r)
	if !ok {
		return
	}
	if fence(opened.session) != nil {
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(askAgainRetry))
		return
	}
	switch err := inbox.Read(r.Context(), body.IDs); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrNoteUntaken):
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(askAgainRetry))
	default:
		log.WarnContext(r.Context(), "mcp_bridge_steer_read_failed", "error", err.Error(),
			"detail", "the notes wait; the poller confirms them again, and the review "+
				"reads any it never does")
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(time.Second))
	}
}

// noteIDs is a read's or a fold's body, its ids 1 to [steerReadIDs]; false
// where it answered the request a refusal.
func noteIDs(w http.ResponseWriter, r *http.Request) (steerRead, bool) {
	var body steerRead
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, steerReadBody))
	if err := dec.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpjson.Fail(w, http.StatusRequestEntityTooLarge, httpjson.CodeBodyTooLarge)
			return steerRead{}, false
		}
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return steerRead{}, false
	}
	if len(body.IDs) == 0 || len(body.IDs) > steerReadIDs {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return steerRead{}, false
	}
	return body, true
}

// steerAgain is an ask's answer: each note granted its second admission, by
// id, with the id to admit it under, and the notes at their budget.
type steerAgain struct {
	Again map[string]string `json:"again"`
	Spent []string          `json:"spent"`
}

// againNotes answers an ask for the second admission of notes a compaction
// folded ([SteerInbox.Again]): ids in and ids out, behind the same token and
// bounds as a read, and turned away to ask again as a read is.
func againNotes(w http.ResponseWriter, r *http.Request, opened *binding, inbox SteerInbox) {
	body, ok := noteIDs(w, r)
	if !ok {
		return
	}
	if fence(opened.session) != nil {
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(askAgainRetry))
		return
	}
	granted, spent, err := inbox.Again(r.Context(), body.IDs)
	switch {
	case errors.Is(err, ErrNoteUntaken):
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(askAgainRetry))
		return
	case err != nil:
		log.WarnContext(r.Context(), "mcp_bridge_steer_again_failed", "error", err.Error(),
			"detail", "nothing was granted; the poller asks again")
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(time.Second))
		return
	}
	if granted == nil {
		granted = map[string]string{}
	}
	if spent == nil {
		spent = []string{}
	}
	httpjson.Write(w, http.StatusOK, steerAgain{Again: granted, Spent: spent})
}

// foldedNotes hands the inbox the notes a fold names ([SteerInbox.Folded]):
// ids only, behind the same token as a read, and answered 204 whatever the
// inbox makes of them, since it records nothing the plugin could post again
// for.
func foldedNotes(w http.ResponseWriter, r *http.Request, inbox SteerInbox) {
	body, ok := noteIDs(w, r)
	if !ok {
		return
	}
	inbox.Folded(r.Context(), body.IDs)
	w.WriteHeader(http.StatusNoContent)
}

// Shutdown releases every poll this bridge holds, for notes or a browser's: one
// it serves from its own opening, and one it is relaying to the session's
// holder ([Bridge.forwardPoll]). It turns away every one that arrives after it,
// at once and as a failure the poller pauses on ([Bridge.pollNotes]); and it ends
// every call waiting for a holder to take it ([Bridge.forward]), answered that
// this node is draining. So a graceful stop never waits out a hold. Safe to
// call more than once.
func (b *Bridge) Shutdown() {
	if b == nil {
		return
	}
	b.shutdownOnce.Do(func() { close(b.shutdown) })
}

// shuttingDown reports whether [Bridge.Shutdown] has run.
func (b *Bridge) shuttingDown() bool {
	select {
	case <-b.shutdown:
		return true
	default:
		return false
	}
}

// steerMiddleware answers tools/call of [SteerTool] on an opening's MCP server,
// and hands every other request on: the tool is never listed, never reaches
// the run's surface and is never recorded — see the package's note inbox.
func (o *binding) steerMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		call, ok := req.(*mcp.CallToolRequest)
		if method != "tools/call" || !ok || call.Params == nil || call.Params.Name != SteerTool {
			return next(ctx, method, req)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: o.takeSteer(ctx, call.Params.Arguments,
				o.noteCall(ctx, call))}},
		}, nil
	}
}

// noteCall is the hook's call as the bridge's front read it off its POST
// ([Bridge.steerFront]): the MCP session it came on and the id the front set
// in [steerCallHeader]. Nil where the front read none, said once an opening
// (mcp_bridge_steer_call_unread): only the job's session then settles what
// the call is served.
func (o *binding) noteCall(ctx context.Context, call *mcp.CallToolRequest) *sandbox.NoteCall {
	var id string
	if call.Extra != nil {
		id = call.Extra.Header.Get(steerCallHeader)
	}
	if id == "" || call.Session == nil {
		o.callUnread.Do(func() {
			log.WarnContext(ctx, "mcp_bridge_steer_call_unread", "run_id", o.session.RunID,
				"seat", o.session.Handle, "detail", "a hook's steer_take came with a JSON-RPC id the bridge "+
					"could not read, so a cancel of it cannot be matched: what it is served is settled by the "+
					"job's session alone, at its collection")
		})
		return nil
	}
	return &sandbox.NoteCall{Session: call.Session.ID(), ID: id}
}

// steerArgs is what a hook passes steer_take: the event that fired it and the
// agent whose loop it fired in, each substituted by the harness.
type steerArgs struct {
	Event string `json:"event"`
	Agent string `json:"agent"`
}

// takeSteer is steer_take's answer: the inbox's, for a boundary of the main
// agent's loop, and nothing for any other. Whatever it answers, the opening's
// end waits for that answer to be out before it closes the box's sessions
// ([binding.holdHook]), so a take whose write landed is never answered into
// the close while its box still waits for it: a box that gave the call up, or
// whose connection failed, lets the hold go before any write, and reads no
// answer.
func (o *binding) takeSteer(ctx context.Context, raw json.RawMessage, call *sandbox.NoteCall) string {
	inbox := o.holdHook(ctx.Done())
	if inbox == nil {
		return ""
	}
	var args steerArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return ""
		}
	}
	switch {
	case strings.HasPrefix(args.Agent, "${"):
		// THE HARNESS DID NOT SUBSTITUTE IT: whose loop fired the hook cannot be
		// told, so it is answered as a subagent's would be — the note waits
		// for a boundary that can be told apart. Said once per session.
		o.unsubstituted.Do(func() {
			log.WarnContext(o.ctx, "mcp_bridge_steer_unsubstituted", "run_id", o.session.RunID,
				"seat", o.session.Handle, "agent", args.Agent,
				"detail", "the run's harness did not fill in the hook's agent id, so no "+
					"note is taken between its steps; its notes reach the run's review instead")
		})
		return ""
	case args.Agent != "":
		// A SUBAGENT'S BATCH. Its loop is not the run's: a note handed to it
		// would bind one delegated piece of work and be lost with it, so it
		// waits for the main agent's next boundary.
		return ""
	case args.Event != SteerEventToolBatch && args.Event != SteerEventFinish:
		return ""
	}
	if fence(o.session) != nil {
		// A RUN WHOSE TOOLS ARE CLOSED reads nothing more: the seat moved, or
		// a person stopped it. The note waits for whatever reads the turn
		// next.
		return ""
	}
	takeCtx, cancel := context.WithTimeout(ctx, steerTakeBudget)
	defer cancel()
	return inbox.Take(takeCtx, args.Event, call)
}

// steerCallHeader carries a hook's steer_take call's JSON-RPC id, as canonical
// JSON text, from the bridge's front to its steer middleware
// ([Bridge.steerFront], [binding.noteCall]): the middleware cannot learn the
// id of the call it answers, which the SDK keeps under an unexported key.
// Always the front's own: a box-sent one is dropped.
const steerCallHeader = "Crewlet-Steer-Call"

// steerPeekBytes is how much of a POST's body the front reads to tell a hook's
// call or a cancel: FOUR KIB, past the largest either is (a cancel's id and
// reason, a steer_take's two short arguments, each well under one KiB), and
// small beside what the SDK reads of every POST anyway. A longer body is no
// hook's, and is handed on unread.
const steerPeekBytes = 4 << 10

// steerFront is what the bridge reads of every POST before the SDK does, the
// package doc's "A hook's cancel is heard before the SDK": it drops a box-sent
// [steerCallHeader]; it peeks at most [steerPeekBytes] of the body, handing
// the SDK those bytes and the rest; and of one JSON-RPC message, decoded as
// the SDK decodes it (jsonrpc.DecodeMessage, keys matched exactly, [rpcParam])
// so the two never read one body two ways, it sets the header to a steer_take
// call's id, or hands a cancel to the opening's inbox ([binding.cancelInbox]),
// which never blocks. Either id is named as a serve keeps it
// ([sandbox.NoteCallID]). Anything else goes straight on.
func (b *Bridge) steerFront(r *http.Request, opened *binding) {
	r.Header.Del(steerCallHeader)
	if r.Method != http.MethodPost || r.Body == nil || r.Body == http.NoBody {
		return
	}
	peek := make([]byte, steerPeekBytes+1)
	n, err := io.ReadFull(r.Body, peek)
	peek = peek[:n]
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), r.Body), r.Body}
	if n > steerPeekBytes || (err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)) {
		return
	}
	msg, err := jsonrpc.DecodeMessage(peek)
	if err != nil {
		return
	}
	req, isRequest := msg.(*jsonrpc.Request)
	if !isRequest {
		return
	}
	switch req.Method {
	case "tools/call":
		if name, _ := rpcParam(req.Params, "name").(string); name != SteerTool {
			return
		}
		if id, ok := sandbox.NoteCallID(req.ID.Raw()); ok {
			r.Header.Set(steerCallHeader, id)
		}
	case "notifications/cancelled":
		requested, idErr := jsonrpc.MakeID(rpcParam(req.Params, "requestId"))
		if idErr != nil {
			return
		}
		id, ok := sandbox.NoteCallID(requested.Raw())
		session := r.Header.Get(sessionHeader)
		if !ok || session == "" {
			return
		}
		if inbox := opened.cancelInbox(); inbox != nil {
			inbox.Cancelled(r.Context(), sandbox.NoteCall{Session: session, ID: id})
		}
	}
}

// rpcParam is the value under key in a JSON-RPC message's params, decoded as
// into an any (a number a float64, as the SDK reads a request id), or nil
// where there is none: the key matched exactly, as the SDK matches it, never
// case-insensitively as encoding/json matches a struct's fields.
func rpcParam(params json.RawMessage, key string) any {
	var fields map[string]json.RawMessage
	if json.Unmarshal(params, &fields) != nil {
		return nil
	}
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}
