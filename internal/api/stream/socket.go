package stream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// The close codes this socket ends a connection with, on top of the ones the
// WebSocket standard already defines.
//
// # Why a close code at all, when the handshake already answers 401
//
// It cannot. A close code rides a close FRAME, so a handshake that never
// completed has none — see [Handler], where a refused credential is answered
// 401 BEFORE the upgrade and the browser reports 1006. These two are for the
// opposite case: a socket that is OPEN, whose client then offers or omits a
// credential on a frame. That channel exists because a browser cannot set a
// header on a WebSocket constructor, so it is the only way an open socket's
// identity is ever asserted — and it is the only place a close is the right
// answer.
//
// TWO CODES RATHER THAN ONE, because they call for different repairs and the
// dashboard's own recovery branches on exactly that difference: 4401 means
// "become somebody" (a credential is needed and none was offered), 4403 means
// "that credential is not one this node knows" (forget it and ask for
// another). Collapsed into one, a reader who has never been asked for a token
// and a reader holding a revoked one get the same dead end.
//
// In the 4000–4999 range, which the standard reserves for an application and
// which no intermediary rewrites. They deliberately ECHO the HTTP statuses
// they mean, so an operator reading a close code and an operator reading a
// REST refusal are reading one number.
//
// NOTHING ELSE CLOSES THIS SOCKET FOR A FAULT. A node that cannot serve keeps
// its socket open and degrades it instead — see [FrameDegraded].
const (
	// CloseUnauthenticated is a client frame that requires an operator on
	// a socket that has none and offered none.
	CloseUnauthenticated websocket.StatusCode = 4401

	// CloseUnauthorized is a credential this node REFUSES, offered on a
	// frame by a socket with no identity of its own.
	CloseUnauthorized websocket.StatusCode = 4403
)

// The error codes a query answer can carry — THE SOCKET'S SUBSET OF
// [httpjson.Code], which is the engine's one refusal vocabulary.
//
// Each value is an entry on that table, and TestTheSocketAndRestShareOneTable
// walks this package's own source to hold it there: a code declared here with
// no entry over there is a refusal the socket calls one thing and REST another,
// which is precisely what the dashboard's `QueryErrorCode` union drifted into
// before anything pinned it.
//
// UNTYPED, because the same six codes are answered over plain HTTP by the
// query surface's REST twin, which writes them as strings. Typing them here
// would say nothing the walk does not, and would make the twin's answer a
// conversion at every call site.
//
// CODES, not prose: the client switches on the value, and a frame carrying a
// sentence would make every rewording a case nobody handles. The sentence
// belongs to the code rather than to the occurrence — [httpjson.Code.Message]
// holds one per code, which is what a REST refusal of the same question
// answers with and what the dashboard's own per-code line is keyed on.
const (
	CodeUnknownQuery = "unknown_query"
	CodeUnauthorized = "unauthorized"
	CodeQueryFailed  = "query_failed"

	// CodeNotFound is a question this node understood, about a record it
	// does not hold. DISTINCT FROM query_failed, because a client acts on
	// them differently: "no such item" is a dead link to show the person,
	// and "the query failed" is a retry.
	CodeNotFound = "not_found"

	// CodeBadParams is a question this node understood and REFUSED: a
	// parameter missing, malformed, or outside the set the field accepts.
	//
	// DISTINCT FROM query_failed because the fault is the caller's, and
	// the two are acted on in opposite directions — a query_failed is
	// retried, a bad_params never succeeds however many times it is sent.
	// Collapsed into query_failed it was a client bug rendered to a person
	// as an engine fault, retried on every poll, and logged as a WARNING
	// by the node being asked wrong.
	//
	// The reason still does not travel — this envelope carries codes and
	// not prose, for the reason stated above — so the refusal's own
	// message, which names the field and the values it would have
	// accepted, is logged at DEBUG instead: available to whoever is
	// debugging the screen, absent from the operator's log when nobody is.
	// What the client renders is its own line for the code; the
	// engine's sentence for that same code is what the REST surface
	// answers with, and both are keyed on the one vocabulary.
	CodeBadParams = "bad_params"

	// CodeUnavailable is a question this node understood and cannot answer
	// YET: a projection still catching up, or a coordination store that
	// could not be reached. A surface this process does not have at all is
	// CodeUnknownQuery instead, because waiting never changes that answer.
	//
	// It is the code that must never be flattened into an empty result.
	// "This company has no work" is an answer a person acts on: they file
	// the duplicate, they conclude the migration failed. A node whose
	// boot reconcile is still running has to be able to say "ask me in a
	// moment" rather than "there is nothing".
	CodeUnavailable = "unavailable"
)

// The failures a query surface reports precisely; everything else is a
// query_failed.
var (
	ErrUnknownQuery = errors.New("stream: unknown query")
	ErrUnauthorized = errors.New("stream: query requires an operator")
	ErrNotFound     = errors.New("stream: no such record")
	ErrBadParams    = errors.New("stream: query refused")
	ErrUnavailable  = errors.New("stream: not available on this node yet")
)

// Query answers one client question.
//
// operatorID is empty for an unauthenticated socket. A query that needs one
// returns [ErrUnauthorized] rather than deciding for itself what to do about
// it, so the refusal reaches the client as a code it already handles.
type Query func(ctx context.Context, what string, params map[string]any, operatorID string) (any, error)

// request is one client-to-server frame.
type request struct {
	Kind   string         `json:"kind"`
	ID     int64          `json:"id"`
	What   string         `json:"what"`
	Params map[string]any `json:"params"`

	// Seat is which seat a `watch` frame asks to be a recipient for, and
	// empty clears the subscription. See [Hub.Watch].
	Seat string `json:"seat"`
}

// THE PER-FRAME CREDENTIAL IS GONE, and it went with `allow_anonymous_read`.
//
// A `token` used to ride the FRAME rather than the handshake, so that a socket
// opened for anonymous reads could ask one operator-only question without
// reconnecting. Every socket now authenticates at the handshake — [authenticate]
// refuses one that presents nothing — so there is no socket for a frame
// credential to upgrade, and the machinery around it (a refused token closing
// an anonymous socket, a good one upgrading exactly one query, an authenticated
// socket ignoring both) was three rules about a state that can no longer occur.
// The dashboard sends no such field, and an unknown field on a frame is ignored
// by both ends, which is what makes this removal additive on the wire.

// Handler serves the dashboard's live socket.
//
// The credential is ?token= on the URL, because browsers cannot set headers on
// a WebSocket constructor. Non-browser clients may send Authorization instead,
// and should: a query string appears in proxy logs.
func Handler(guard *auth.Guard, svc *Service, query Query) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operatorID, ok := authenticate(guard, r)
		if !ok {
			// REFUSED BEFORE THE UPGRADE. Accepting a credential this
			// node rejects, purely to close it politely a moment later,
			// would let anyone open a socket here.
			//
			// It is NOT "close(1008) before accept", which is what this
			// comment used to claim and what the dashboard was written
			// against. A close code rides a close FRAME, so a handshake
			// that never completed cannot carry one: the browser reports
			// 1006 — indistinguishable from a stopped engine — and hides
			// the status, so a page cannot port-scan with a socket.
			//
			// The client therefore cannot learn this from the socket at
			// all. It re-asks over plain HTTP, where the status is
			// visible, and this route answers a GET without an Upgrade
			// header 401 (refused) or 426 (accepted, wrong protocol).
			// That pairing is load-bearing for the dashboard's token
			// gate — see `probeRefusal` in dashboard/src/protocol/socket.ts.
			// Real JSON, not http.Error: that sets text/plain AND
			// nosniff, so a JSON literal handed to it is the one
			// combination guaranteed to stop a strict client parsing
			// the body it is being sent. The refusal envelope is the
			// same one every REST route answers with, which is what
			// lets the dashboard's HTTP re-ask read one shape.
			httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
			return
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// The dashboard is served by this same process, so the
			// socket is same-origin. Anything else is a page the
			// operator did not open.
			OriginPatterns: nil,
		})
		if err != nil {
			log.Debug("stream_accept_failed", "error", err)
			return
		}
		serveSocket(r.Context(), conn, svc, query, operatorID)
	})
}

// authenticate resolves the socket's operator, or refuses it.
//
// The socket is guarded exactly as the equivalent HTTP read is, which now
// means: always. A credential that is absent and one that is present and wrong
// are refused alike — the first used to open an anonymous socket, and what
// that socket could read is every LLM transcript this company has produced.
// The credential is read by the guard's own rule, the same one the middleware
// applied a moment earlier, so the two can never disagree about where a
// socket's token may ride.
//
// IT STILL ASKS [auth.Unguarded] rather than assuming the answer. The
// exemption list is that package's to state, and a socket path that somebody
// later declares unguarded must open here rather than being refused by a
// second, private copy of the rule.
func authenticate(guard *auth.Guard, r *http.Request) (string, bool) {
	operatorID, authenticated := guard.Presented(r)
	if authenticated {
		return operatorID, true
	}
	// A CREDENTIAL THAT IS PRESENT AND WRONG IS REFUSED EVEN IF THIS PATH
	// WERE EXEMPT, which is where this parts company with the HTTP
	// middleware: there an unguarded route serves a bad token as nobody,
	// and here it must not. A socket is a long-lived subscription rather
	// than one answer, and a reader whose token is stale would sit on it
	// for hours getting whatever an exempt socket serves, never told that
	// the credential they typed is wrong.
	//
	// Both arms answer false today, because the socket path is guarded —
	// they are kept apart because only one of them may change if it ever
	// stops being.
	if guard.Credential(r) != "" {
		return "", false
	}
	return "", auth.Unguarded(auth.SocketPath)
}

// serveSocket runs one connection until it closes.
func serveSocket(ctx context.Context, conn *websocket.Conn,
	svc *Service, query Query, operatorID string,
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// THE BUDGET IS ACQUIRED BEFORE ANY FRAME IS READ and released when
	// this socket is done, so a person's tabs share one allowance for
	// exactly as long as they are open. See budget.go.
	slots, releaseBudget := svc.budgets.acquire(operatorID)
	defer releaseBudget()

	client := NewClient()
	// REGISTERED BEFORE THE SNAPSHOT. See Hub.Register: the overlap is
	// deduped by the client, and the gap the other order leaves is not.
	//
	// THROUGH THE SERVICE rather than the hub directly, because the node's
	// posture is what this client must be registered AT: a tab that
	// connects to a shedding node between two health ticks would otherwise
	// be served live for the rest of the interval. See [Service.Join].
	svc.Join(client)
	defer svc.Hub().Unregister(client)

	var writer sync.WaitGroup
	writer.Go(func() {
		defer cancel()
		writeLoop(ctx, conn, client)
	})

	client.Reply(Push(KindSnapshot, svc.Snapshot(), time.Now().UTC()))
	code, reason := readLoop(ctx, conn, svc.Hub(), client, query, operatorID, slots)

	// Unregister closes the client's queue, which is what ends the writer.
	svc.Hub().Unregister(client)
	writer.Wait()
	_ = conn.Close(code, reason)
}

// writeLoop drains this client's queue onto the socket.
//
// One writer per connection, and it is the ONLY thing that writes: a WebSocket
// connection permits one concurrent writer, so a query answered on its own
// goroutine goes through this queue rather than to the socket directly.
func writeLoop(ctx context.Context, conn *websocket.Conn, client *Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame, open := <-client.Out():
			if !open {
				return
			}
			// ALREADY ENCODED, and that is the point of [Frame]: the
			// bytes were marshalled once for this client's whole
			// posture (see [Hub.Broadcast]) and every writer in that
			// posture writes the same slice. This loop used to marshal
			// the envelope again for itself, so one push on a company
			// with N tabs open cost N encodes of identical JSON.
			raw := frame.Raw()
			// A DEADLINE PER WRITE. A TCP peer that has vanished
			// without a FIN — a laptop lid closed, a NAT entry
			// dropped, a mobile network handing off — leaves this
			// Write blocked until the kernel gives up, which is
			// minutes with default keepalives. Until then the
			// goroutine, the client's queue and the hub registration
			// all stay live, so a page nobody is reading holds a slot
			// on every broadcast.
			//
			// THIRTY SECONDS, not a few: QueueDepth above decides
			// deliberately that a slow tab must not be disconnected
			// ("a visible failure for a reader who did nothing
			// wrong"), so this deadline is here to tell GONE from
			// SLOW and nothing else. A tighter one would sever a
			// mobile tab mid-snapshot and contradict that decision.
			writeCtx, cancelWrite := context.WithTimeout(ctx, writeTimeout)
			err := conn.Write(writeCtx, websocket.MessageText, raw)
			cancelWrite()
			if err != nil {
				return
			}
		}
	}
}

// readLoop handles client frames until the socket closes, and reports the
// close code the connection should end with.
//
// [websocket.StatusNormalClosure] is the ordinary answer — the client went
// away, or this process is stopping. The two auth codes are the only faults
// that end a socket at all; everything else this loop can go wrong about is
// answered ON the socket and the socket stays open. See [CloseUnauthenticated]
// and [FrameDegraded].
func readLoop(ctx context.Context, conn *websocket.Conn,
	hub *Hub, client *Client, query Query, operatorID string,
	slots chan struct{},
) (websocket.StatusCode, string) {
	// THE CONCURRENCY BOUND ARRIVES FROM THE SERVICE rather than being
	// made here, and that is the whole of the per-principal change: a
	// channel built in this function is one socket's, so a person's second
	// tab got a second full allowance. Queries still run on their own
	// goroutines so a store scan cannot stall the live feed, and a burst
	// past the bound queues here rather than piling into the engine's
	// connection pool — it is now this PERSON's burst that queues rather
	// than this tab's.
	var running sync.WaitGroup
	defer running.Wait()

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return websocket.StatusNormalClosure, ""
		}
		var req request
		if err := decodeRequest(raw, &req); err != nil {
			// Unparseable input from a client is not a reason to drop a
			// socket that is otherwise working.
			log.DebugContext(ctx, "stream_bad_frame", "error", err)
			continue
		}
		switch req.Kind {
		case "ping":
			// THE KEEPALIVE, and it is answered in EVERY posture — a
			// degraded socket is the one that most needs to stay up,
			// because the health frame beside this is how the tab
			// learns why its pushes stopped.
			client.Reply(Envelope{Kind: KindPong})
		case "watch":
			if code, reason := watch(ctx, hub, client, req, operatorID); code != 0 {
				return code, reason
			}
		case "query":
			// A DEGRADED NODE REFUSES rather than answers. Its copy of
			// the company is wrong rather than behind, and an answer
			// out of it — "there is no such work item" — is something a
			// person acts on. The refusal is a DIRECT frame, so it
			// reaches the client whatever the posture.
			if !client.Posture().ServesQueries() {
				client.Reply(queryError(req, CodeUnavailable))
				continue
			}
			if query == nil {
				client.Reply(queryError(req, CodeUnknownQuery))
				continue
			}
			// NOT running.Go: the semaphore acquire has to happen on
			// THIS goroutine, the reader. Moving it inside the spawned
			// one would let the reader keep spawning past the cap and
			// the backpressure would be a queue of blocked goroutines
			// rather than a paused reader.
			running.Add(1)
			slots <- struct{}{}
			go func() {
				defer running.Done()
				defer func() { <-slots }()
				runQuery(ctx, client, query, req, operatorID)
			}()
		default:
			// Unknown kinds are ignored, which is what makes new ones
			// additive on both ends.
		}
	}
}

// watch makes this client a recipient for one seat, or reports the close code
// that refuses it.
//
// # A subscription is a WRITE, so it needs a credential
//
// A watch is not a read: it installs a row in the hub's routing index, on this
// node, on behalf of a caller, and it stays there until the socket goes away.
//
// THE CHECK IS KEPT THOUGH EVERY SOCKET IS NOW AUTHENTICATED, and it is not
// belt-and-braces: [authenticate] is what decides a socket's operator, it asks
// the auth package's exemption list, and that list is somebody else's to
// change. A watch reaching this function with no operator would mean the
// socket path had become exempt — and installing routing state for a caller
// nobody can name is the one outcome that must not follow silently from that.
//
// A close rather than an error frame, because a watch carries no correlation
// id: the socket itself is the only channel the refusal has.
func watch(ctx context.Context, hub *Hub, client *Client,
	req request, operatorID string,
) (websocket.StatusCode, string) {
	if operatorID == "" {
		return CloseUnauthenticated,
			"watching a seat needs an operator credential"
	}
	hub.Watch(client, req.Seat)
	log.DebugContext(ctx, "stream_watch", "operator", operatorID, "seat", req.Seat)
	return 0, ""
}

// runQuery answers one question onto the client's own queue.
func runQuery(ctx context.Context, client *Client, query Query,
	req request, operatorID string,
) {
	data, err := query(ctx, req.What, req.Params, operatorID)
	switch {
	case err == nil:
		client.Reply(Envelope{Kind: KindResult, ID: req.ID, What: req.What, Data: data})
	case errors.Is(err, ErrUnknownQuery):
		client.Reply(queryError(req, CodeUnknownQuery))
	case errors.Is(err, ErrUnauthorized):
		client.Reply(queryError(req, CodeUnauthorized))
	case errors.Is(err, ErrNotFound):
		client.Reply(queryError(req, CodeNotFound))
	case errors.Is(err, ErrBadParams):
		// DEBUG, NOT WARN: it is not this node's failure, and a poll
		// behind a bad request writes a line per tick for as long as the
		// screen is open. The message is worth keeping — it names the
		// field the caller got wrong, which is the whole of the fix —
		// but only to somebody who turned debug on to look for it.
		log.DebugContext(ctx, "stream_query_refused", "what", req.What, "error", err)
		client.Reply(queryError(req, CodeBadParams))
	case errors.Is(err, ErrUnavailable):
		client.Reply(queryError(req, CodeUnavailable))
	default:
		// The reason reaches the LOG, not the client. A query failure can
		// carry a database path or a driver's own message, and the socket
		// is the one surface an unauthenticated reader may be holding.
		log.WarnContext(ctx, "stream_query_failed", "what", req.What, "error", err)
		client.Reply(queryError(req, CodeQueryFailed))
	}
}

func queryError(req request, code httpjson.Code) Envelope {
	return Envelope{Kind: KindError, ID: req.ID, What: req.What, Error: code}
}

// decodeRequest parses one client frame.
//
// Strict about SHAPE and lenient about content: a frame that is not an object
// is not a request, but an unknown field on one that is costs nothing to
// ignore — which is what makes a newer client safe against an older server.
func decodeRequest(raw []byte, req *request) error {
	return json.Unmarshal(raw, req)
}
