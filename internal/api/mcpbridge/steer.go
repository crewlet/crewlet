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
// A coding run is its CLI's own loop, so the engine has no round of it to open
// with a note. What the CLI has is boundaries of its own — Claude Code's hooks
// after a batch of tool calls and as it is about to finish, OpenCode's next
// step — and a way to ask for something there. So a run's session carries the
// run's note INBOX ([Session.Steer]), and the bridge serves it two ways, both
// behind the run's own token and nothing else:
//
//   - THE steer_take TOOL ([SteerTool]) on the run's existing MCP server, which
//     a hook calls. It is NEVER LISTED, so no CLI offers it to its model, and
//     it is answered by the server's own receiving middleware, so the call never
//     reaches the run's surface and is never recorded on its ledger: it is the
//     CLI asking, not the model. Its answer is ALWAYS text — empty for nothing
//     — never an error: measured on Claude Code, a hook's call that failed made
//     the CLI drop and re-initialise the whole bridge, telling the model its
//     tools were still connecting.
//   - THE NOTES ROUTE ([SteerPattern]) a plugin inside the CLI polls: GET
//     `notes?after=<seq>` holds for a note for up to [steerHold] and answers the
//     ones waiting past the cursor, rendered; POST `read` records the notes the
//     CLI reported putting in front of its model.
//
// THE BRIDGE DECIDES NOTHING ABOUT NOTES. What waits, what fits, what is read
// and what is recorded is the inbox's (internal/engine, runsteer.go); the
// bridge routes a call to the session that holds the inbox — relayed to the
// seat's holder like every bridged call ([Bridge.Join]) — keeps a hook's call to
// the boundaries the main agent's loop makes, and bounds how long either is
// held.

// SteerTool is the tool a coding CLI's hook calls on the bridge for its turn's
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
	// in the shape the CLI reads, or empty for nothing. Never an error — a
	// note that could not be taken waits for the next step.
	Take(ctx context.Context, event string) string

	// Wait is the notes waiting past the cursor after, oldest first, and a
	// channel closed when the inbox's view of them next changes or the inbox
	// closes. A PEEK: nothing is recorded read.
	Wait(ctx context.Context, after int) ([]SteerNote, <-chan struct{})

	// Read records the notes named read at the run's next step — the CLI
	// reported putting them in front of its model. A note no longer waiting
	// is skipped; an error is a record that could not be written, and the
	// notes wait.
	Read(ctx context.Context, ids []string) error

	// Close ends the inbox: its watch stops, every channel [SteerInbox.Wait]
	// handed out closes, and every later call answers nothing.
	Close()
}

// SteerNote is one waiting note as the notes route answers it.
type SteerNote struct {
	// Seq is the note's place among the turn's notes: what a poller passes
	// back as its cursor.
	Seq int `json:"seq"`

	// ID is the note's identity, which a read names it by.
	ID string `json:"id"`

	// Inbox is the id the CLI admits the note under, unique to the note and
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
// engine hands a run's CLI beside the endpoint itself.
func SteerURL(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	return endpoint + steerSuffix
}

// steerTakeBudget bounds the inbox's answer to one hook: FIVE SECONDS, half
// the ten a hook that calls steer_take is given. A CLI waits on its hook before
// its next request, at every batch, so the hook's own timeout is short, and
// half of it answers a note the inbox took before the CLI gives up on the
// hook, unless the network itself stalls past five seconds. Measured on Claude
// Code: at its timeout the CLI cancels the call (notifications/cancelled) and
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
// CLI polls, answered from memory.
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
func (b *Bridge) serveSteer(w http.ResponseWriter, r *http.Request, opened *binding) {
	inbox := opened.inbox()
	if inbox == nil {
		// A SESSION WITH NO INBOX — a run with nothing to steer, the
		// assistant's deep work — has no notes route at all.
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
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
// are handed to peers, and a direct box goes on dialling its loopback (ADR-0057)
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
// away with: the pace the bridge's own relay reads a seat's lease again at
// while it waits for the seat's next holder ([holderRetryCeiling]), because
// that holder is what serves the session's next opening — a node that is
// going hands its seats on, and an opening that ended was replaced, moved
// with its seat, or ended with its run, whose route then answers 401.
const steerPollRetry = holderRetryCeiling

func writeNotes(w http.ResponseWriter, notes []SteerNote) {
	if notes == nil {
		notes = []SteerNote{}
	}
	httpjson.Write(w, http.StatusOK, steerNotes{Notes: notes})
}

// readNotes records the notes a read names — none for a run whose tools are
// closed, which takes nothing more from its notes (see [Bridge.pollNotes]); its
// read is answered as done, because there is nothing for the poller to retry.
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
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := inbox.Read(r.Context(), body.IDs); err != nil {
		log.WarnContext(r.Context(), "mcp_bridge_steer_read_failed", "error", err.Error(),
			"detail", "the notes wait; the poller confirms them again, and the review "+
				"reads any it never does")
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(time.Second))
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
// agent whose loop it fired in, each substituted by the CLI.
type steerArgs struct {
	Event string `json:"event"`
	Agent string `json:"agent"`
}

// takeSteer is steer_take's answer: the inbox's, for a boundary of the main
// agent's loop, and nothing for any other.
func (o *binding) takeSteer(ctx context.Context, raw json.RawMessage) string {
	inbox := o.inbox()
	if inbox == nil || o.over() {
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
		// THE CLI DID NOT SUBSTITUTE IT: whose loop fired the hook cannot be
		// told, so it is answered as a subagent's would be — the note waits
		// for a boundary that can be told apart. Said once per session.
		o.unsubstituted.Do(func() {
			log.WarnContext(o.ctx, "mcp_bridge_steer_unsubstituted", "run_id", o.session.RunID,
				"seat", o.session.Handle, "agent", args.Agent,
				"detail", "the run's coding CLI did not fill in the hook's agent id, so no "+
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
