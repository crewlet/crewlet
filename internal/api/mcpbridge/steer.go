package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// A NOTE INBOX: a person's notes to the run's turn, at the run's own steps.
//
// An executor is its harness's own loop, so the engine has no round of it to open
// with a note. What the harness has is boundaries of its own — Claude Code's hooks
// after a batch of tool calls and as it is about to finish, OpenCode's next
// step — and a way to ask for something there. So a run's session carries the
// run's note INBOX ([Session.Steer]), and the bridge serves it two ways, both
// behind the run's own token and nothing else:
//
//   - THE steer_take TOOL ([SteerTool]) on the run's existing MCP server, which
//     a hook calls. It is NEVER LISTED, so no harness offers it to its model, and
//     it is answered by the server's own receiving middleware, so the call never
//     reaches the run's surface and is never recorded on its ledger: it is the
//     harness asking, not the model. Its answer is ALWAYS text — empty for nothing
//     — never an error: measured on Claude Code, a hook's call that failed made
//     the harness drop and re-initialise the whole bridge, telling the model its
//     tools were still connecting. And the opening's end waits for that answer
//     before it closes the box's sessions, for as long as the box waits for it
//     ([binding.holdHook]): an answer the close refused would fail the call all
//     the same, and a take's notes would be recorded handed over to a step that
//     never had them.
//   - THE NOTES ROUTE ([SteerPattern]) a plugin inside the harness polls: GET
//     `notes?after=<seq>` holds for a note for up to [steerHold] and answers the
//     ones waiting past the cursor, rendered, each recorded served to the run's
//     job before it is answered (the inbox's [SteerInbox.Wait]); POST `read`
//     records the notes the harness reported putting in front of its model.
//
// THE BRIDGE DECIDES NOTHING ABOUT NOTES. What waits, what fits, what is read
// and what is recorded is the inbox's (internal/engine, runsteer.go); the
// bridge routes a call to the session that holds the inbox — relayed to the
// seat's holder like every bridged call ([Bridge.Join]) — keeps a hook's call to
// the boundaries the main agent's loop makes, and bounds how long either is
// held.

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
	// in the shape the harness reads, or empty for nothing. Never an error — a
	// note that could not be taken waits for the next step.
	Take(ctx context.Context, event string) string

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
	// closed, takes no read and answers [ErrInboxNotServing], and so does one
	// whose job the run's record takes no read of any more, for a note named
	// that still waits.
	Read(ctx context.Context, ids []string) error

	// Close ends the inbox: its watch stops, every channel [SteerInbox.Wait]
	// handed out closes, every later Take and Wait answers nothing, and every
	// later Read [ErrInboxNotServing].
	Close()
}

// ErrInboxNotServing is what a [SteerInbox] answers a [SteerInbox.Read] with
// where it serves no job that takes notes: closed, the opening that served it
// having ended; not yet bound to its job, which no opening serves (the engine
// binds a run's inbox before the bridge files the opening that serves it, so a
// box that reaches the opening the moment it is filed finds the inbox
// serving); or bound to a job the run's record takes no read of any more, a
// note the read names still waiting there: the job's completion claimed the
// run while its opening still serves, the run's ending is decided, or a later
// job carries it on. It recorded nothing. Declared here, by the consumer,
// which tells it from a write that could not be confirmed: the read is turned
// away to ask again, as one reaching its opening as it ends is
// ([Bridge.serveSteer]), and its next try reaches the inbox that serves the
// job now, or the run that goes on once a claim is handed back.
var ErrInboxNotServing = errors.New("mcpbridge: the run's note inbox serves no job that takes notes, and recorded nothing")

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

// steerHold is how long a poll for notes is held when none waits.
//
// TEN SECONDS, under the tightest common default between a box and the bridge:
// Envoy's route timeout, fifteen seconds when unset — a gateway in front of the
// bridge cuts a request held longer, and a poll cut mid-hold is a retry and a
// log line for nothing. Inside the fleet relay's idle bound
// (internal/httprelay's Idle, three heartbeats) for a poll relayed to the
// seat's holder. Its cost is one request every ten seconds per idle run whose
// harness polls, answered from memory.
const steerHold = 10 * time.Second

// steerReadBody bounds a read's body: a read names at most a turn's notes
// ([steer.MaxTurnNotes]), a few hundred bytes, and 64 KiB is far past that
// while far inside what the relay carries (internal/httprelay's MaxBody).
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
func (b *Bridge) SteerHandler() http.Handler { return b.routed(b.serveSteer) }

// serveSteer answers one call of the notes route on the opening that holds it.
//
// NO ADMISSION OF ITS OWN: the token, and for a session held to loopback the
// address and the Host ([loopbackRefusal]), are judged by the routing it
// shares with the MCP endpoint ([Bridge.routed]) before it is reached, and a
// relayed call for a held session never reaches it ([Bridge.admitRelayed]).
//
// A CALL THAT REACHES ITS OPENING AS IT ENDS IS TURNED AWAY TO ASK AGAIN, as a
// poll the opening can no longer hold is ([Bridge.pollNotes]): a 503 with a
// Retry-After. The routing found the opening before its end took it out of
// the bridge's table, and the end took its inbox before the call was served:
// the run ended, the seat left this node, or a recovery opened the job again.
// The call's next try is routed afresh, to the opening that serves the box
// now, here or on the seat's next holder, or refused where the run is over.
// NEVER THE 404 of a session that never had an inbox, which says the run has
// no notes route at all: the OpenCode plugin reads that as it reads a refused
// token, and stops asking for the rest of its run, so a box that goes on
// would never be handed a note between its steps again. And the same holds
// for a read whose opening the end overtakes after this check, on its way to
// the inbox ([readNotes]).
func (b *Bridge) serveSteer(w http.ResponseWriter, r *http.Request, opened *binding) {
	if opened.session.Steer == nil {
		// A SESSION WITH NO INBOX (a run with nothing to steer, the
		// assistant's deep work) has no notes route at all.
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	}
	inbox := opened.inbox()
	if inbox == nil {
		// THE OPENING ENDED, which took the inbox its session has.
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(steerPollRetry))
		return
	}
	switch verb := r.PathValue("verb"); {
	case verb == "notes" && r.Method == http.MethodGet:
		b.pollNotes(w, r, opened, inbox)
	case verb == "read" && r.Method == http.MethodPost:
		readNotes(w, r, opened, inbox)
	case verb == "notes" || verb == "read":
		httpjson.Fail(w, http.StatusMethodNotAllowed, httpjson.CodeMethodNotAllowed)
	default:
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
	}
}

// pollNotes answers the notes waiting past the poll's cursor, holding the poll
// for up to [steerHold] while none does. It returns at once on a change, the
// opening's end, the bridge's shutdown and the poller going away.
//
// A RUN WHOSE TOOLS ARE CLOSED is answered as one with nothing waiting, as its
// steer_take is ([binding.takeSteer]): the seat moved or a person stopped it,
// and its notes wait for whatever reads the turn next. Asked at every pass,
// because the fence closes while a poll is held.
//
// A POLL THIS OPENING CANNOT HOLD IS REFUSED, never answered as an empty one:
// once the bridge is shutting down or the opening has ended, a held poll is let
// go and every later one turned away with a 503 and a Retry-After
// ([steerPollRetry]). Answered 200 with nothing, a poller whose next poll is the
// answer to its last — the OpenCode plugin's — asked again at once, and kept
// asking for as long as this node went on serving its route: from the bridge's
// shutdown to the node leaving the relay is most of a graceful stop. A failure
// is what a poller pauses on; the notes wait on the run's record for the
// session's next opening, here or on the seat's next holder.
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
			httpjson.Unavailable(w, httpjson.CodeDraining, httpjson.RetrySeconds(steerPollRetry))
			return
		case opened.over():
			httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(steerPollRetry))
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
// the one call under the bridge's prefix that WAITS for something to happen,
// rather than for something to be done.
func isNotesPoll(r *http.Request) bool {
	return r.Method == http.MethodGet && r.PathValue("verb") == "notes"
}

// forwardPoll relays a poll this node holds no opening for to the session's
// holder ([Bridge.forward]) — and lets go of it the moment this bridge shuts
// down, answering it as a poll held here is answered then: a 503 the poller
// pauses on ([Bridge.pollNotes]).
//
// A RELAYED POLL IS HELD BY TWO NODES, the holder that waits for a note and this
// one, which waits for the holder's answer, and only the holder's shutdown
// released it. A node that is stopping relays its polls the longest — its seats
// are handed to peers, and a direct box goes on dialling its loopback (ADR-0065)
// — and one it went on waiting on kept its listener busy for the whole grace of
// a graceful stop and was then cut, which a poller reads as a network error
// rather than the answer to pause on. Only the poll: a relayed tool call or a
// read may already be running on the holder, and is left to finish.
func (b *Bridge) forwardPoll(w http.ResponseWriter, r *http.Request, sub subject, f *fleet) {
	if b.shuttingDown() {
		httpjson.Unavailable(w, httpjson.CodeDraining, httpjson.RetrySeconds(steerPollRetry))
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
		httpjson.Unavailable(w, httpjson.CodeDraining, httpjson.RetrySeconds(steerPollRetry))
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

// steerPollRetry is the Retry-After a poll the bridge cannot hold is turned
// away with, and a read that reached its opening as it ended, or that a run
// whose tools are closed takes no more ([Bridge.serveSteer], [readNotes]): the
// pace the bridge's own relay reads a seat's lease again at while it waits for
// the seat's next holder ([holderRetryCeiling]), because that holder is what
// serves the session's next opening: a node that is going hands its seats on,
// and an opening that ended was replaced, moved with its seat, or ended with
// its run, whose route then answers 401.
const steerPollRetry = holderRetryCeiling

func writeNotes(w http.ResponseWriter, notes []SteerNote) {
	if notes == nil {
		notes = []SteerNote{}
	}
	httpjson.Write(w, http.StatusOK, steerNotes{Notes: notes})
}

// readNotes records the notes a read names through the opening's inbox.
//
// A READ IS RECORDED OR TURNED AWAY TO ASK AGAIN, NEVER ANSWERED DONE WITH
// NOTHING RECORDED: the plugin would never confirm its notes again, and the
// record would keep each served and unread, for the run's ending to report
// unconfirmed though OpenCode delivered it, and a later launch to tell again
// as one the session may already show. It is turned away as a read that
// reached the opening after its end took the inbox is ([Bridge.serveSteer]),
// at either of its last two steps:
//
//   - A RUN WHOSE TOOLS ARE CLOSED takes nothing more from its notes (see
//     [Bridge.pollNotes]), and records no read either, whether or not its
//     opening has ended yet. A seat leaving this node leaves its grants,
//     which closes the fence, before its release ends the opening, and its
//     box goes on with the seat's next holder, which the read's next try
//     reaches and which records it. A person's stop closes the fence too, and
//     the run's end then refuses the read's next try. Answered done, the read
//     was recorded nowhere, and the seat's next holder was never asked.
//   - AN INBOX THAT SERVES NO JOB says it took the read nowhere
//     ([ErrInboxNotServing]): the end closed it after the route found it
//     serving. The inbox decides that under its own lock, which its close
//     takes, so a read either found it serving and is recorded through the
//     store, which a later close does not touch, or did not and is not
//     recorded at all. And so does an inbox whose job the run's record takes
//     no read of any more, though it still serves: the job's completion
//     claimed the run before the reclaim closes its session, so the read
//     made again is recorded where the claim is handed back and the run goes
//     on in its box, and refused once the run is over.
func readNotes(w http.ResponseWriter, r *http.Request, opened *binding, inbox SteerInbox) {
	var body steerRead
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, steerReadBody))
	if err := dec.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpjson.Fail(w, http.StatusRequestEntityTooLarge, httpjson.CodeBodyTooLarge)
			return
		}
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}
	if len(body.IDs) == 0 || len(body.IDs) > steer.MaxTurnNotes {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}
	if fence(opened.session) != nil {
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(steerPollRetry))
		return
	}
	switch err := inbox.Read(r.Context(), body.IDs); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrInboxNotServing):
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(steerPollRetry))
	default:
		log.WarnContext(r.Context(), "mcp_bridge_steer_read_failed", "error", err.Error(),
			"detail", "the notes wait; the poller confirms them again, and the review "+
				"reads any it never does")
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(time.Second))
	}
}

// Shutdown releases every poll for notes this bridge holds — one it serves from
// its own opening, and one it is relaying to the session's holder
// ([Bridge.forwardPoll]) — and turns away every one that arrives after it, at
// once and as a failure the poller pauses on ([Bridge.pollNotes]), so a graceful
// stop never waits out a hold. Safe to call more than once.
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
			Content: []mcp.Content{&mcp.TextContent{Text: o.takeSteer(ctx, call.Params.Arguments)}},
		}, nil
	}
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
func (o *binding) takeSteer(ctx context.Context, raw json.RawMessage) string {
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
	return inbox.Take(takeCtx, args.Event)
}
