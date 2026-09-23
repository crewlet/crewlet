package stream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// The close codes this socket ends a connection with, on top of the ones the
// WebSocket standard already defines.
//
// # Why a close code at all, when the handshake already answers 401
//
// It cannot. A close code rides a close FRAME, so a handshake that never
// completed has none — see [Handler], where a refused credential is answered
// 401 BEFORE the upgrade and the browser reports 1006. These two are for the
// opposite case: a socket that is OPEN, whose credential stopped answering
// while it was — which the socket's own revalidation finds (revalidate.go). A
// handshake decision alone would leave a revoked session's tab receiving the
// company's state for as long as it stayed open.
//
// TWO CODES RATHER THAN ONE, because they call for different repairs and the
// dashboard's own recovery branches on exactly that difference: 4401 means
// "this credential names nobody now" (re-dial with the cookie the browser
// holds, and sign in if that is refused too), 4403 means "it names somebody
// whose seat is gone" (stop — signing in again reaches the same person).
// Collapsed into one, a tab whose cookie merely expired and a person who was
// offboarded get the same dead end.
//
// In the 4000–4999 range, which the standard reserves for an application and
// which no intermediary rewrites. They deliberately ECHO the HTTP statuses
// they mean, so an operator reading a close code and an operator reading a
// REST refusal are reading one number.
//
// NOTHING ELSE CLOSES THIS SOCKET FOR A FAULT. A node that cannot serve keeps
// its socket open and degrades it instead — see [FrameDegraded].
const (
	// CloseUnauthenticated ends a socket whose credential no longer
	// resolves to anybody: its session ended, expired or was revoked, or
	// its token is not one this node accepts — found by the socket's own
	// revalidation (see revalidate.go) — or a frame that needs a caller
	// arrived on a socket that has none.
	CloseUnauthenticated websocket.StatusCode = 4401

	// CloseUnauthorized ends a socket whose credential still resolves but
	// may not act: the person's seat is gone from the chart.
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
// WHO IS ASKING TRAVELS IN THE CONTEXT, which the guard resolved before this
// handler ran — never as an argument beside it, because two identities on one
// call are two chances to answer about different people. A query the caller
// may not ask returns [ErrUnauthorized] rather than deciding for itself what
// to do about it, so the refusal reaches the client as a code it handles.
type Query func(ctx context.Context, what string, params map[string]any) (any, error)

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
// reconnecting. Every socket now authenticates at the handshake — [Handler]
// refuses one that presents nothing — so there is no socket for a frame
// credential to upgrade, and the machinery around it (a refused token closing
// an anonymous socket, a good one upgrading exactly one query, an authenticated
// socket ignoring both) was three rules about a state that can no longer occur.
// The dashboard sends no such field, and an unknown field on a frame is ignored
// by both ends, which is what makes this removal additive on the wire.

// Handler serves the dashboard's live socket.
//
// The credential is ?token= on the URL, because browsers cannot set headers on
// a WebSocket constructor, or the session cookie a signed-in browser sends on
// its own. Non-browser clients may send Authorization instead, and should: a
// query string appears in proxy logs.
//
// # Who is calling is the GUARD's answer, read once
//
// It used to be a second answer: this handler re-read the credential through
// the guard's Tier A arm alone, so a person signed in with a session cookie —
// which is every browser once `/auth` exists — was refused here while every
// REST route beside it served them. The guard resolves both shapes, the
// middleware has already run it for this path, and the handler reads the
// result from the context like every other surface does.
func Handler(guard *auth.Guard, svc *Service, query Query) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, refusal := resolved(guard, w, r)
		principal, how := iam.From(r.Context())
		if refusal == nil && how == iam.Unknown {
			// 503 AND NEVER 401, for the guard's own reason: a browser
			// reads 401 as "sign in again" and discards the cookie, and
			// this node merely could not read its identity estate.
			httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable,
				auth.RetryIdentitySeconds)
			return
		}
		if refusal != nil {
			// A PERSON WHOSE SEAT IS GONE is resolved and still refused,
			// with the guard's own code and detail — a socket is a
			// surface that acts as the seat.
			httpjson.FailWith(w, refusal.Status, refusal.Code,
				map[string]string{"detail": refusal.Detail})
			return
		}
		if how != iam.Resolved {
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
		if !principal.Can(iam.GrantStateRead) {
			// A RESOLVED CALLER WHO MAY READ NONE OF IT. Every push
			// this socket carries is a `state:read` question's answer
			// or an `audit:read` one's, so a caller holding neither
			// would be accepted to receive nothing — and one holding
			// only `audit:read` would receive the event feed without
			// the roster it names. Refused before the upgrade, where
			// the status is still visible to the page's HTTP re-ask.
			httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeUnauthorized,
				map[string]string{"detail": "the live socket needs " +
					string(iam.GrantStateRead)})
			return
		}
		// THE BUDGET IS THE PRINCIPAL'S, keyed on its ID: the same
		// person across their tabs and across a rename, and never shared
		// between two. Not the login, which is a name somebody changes —
		// see budget.go.
		budgetKey := budgetKeyOf(principal)
		who := &asking{principal: principal}
		check := checkerFor(guard, r)

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
		serveSocket(r.Context(), conn, svc, query, budgetKey, who, check)
	})
}

// resolved is the request carrying the guard's answer, and the guard's
// refusal if it made one.
//
// THE MIDDLEWARE HAS NORMALLY ALREADY ANSWERED, and then this is a read: the
// socket path is guarded, so in the engine every request reaching this handler
// carries a resolution — and resolving a second time would read the identity
// estate twice and could write the session cookie twice on one response.
//
// A CONTEXT NOBODY RESOLVED is this handler mounted bare, which is how its own
// suite and any embedding that skips the middleware reach it. It is not a
// wiring fault here the way it is for a query — this handler is HANDED the
// guard, so it can answer the question itself, once, by the same rule.
func resolved(guard *auth.Guard, w http.ResponseWriter,
	r *http.Request) (*http.Request, *auth.Refusal) {

	if !errors.Is(iam.Reason(r.Context()), iam.ErrUnresolved) {
		// NO REFUSAL CAN BE PENDING HERE: the middleware writes a seat
		// refusal itself on every path outside `/auth/`, so a request
		// it answered and passed on is one it did not refuse.
		return r, nil
	}
	return guard.Resolve(w, r)
}

// serveSocket runs one connection until it closes.
func serveSocket(ctx context.Context, conn *websocket.Conn,
	svc *Service, query Query, budgetKey uuid.UUID, who *asking, check checkFunc,
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// THE BUDGET IS ACQUIRED BEFORE ANY FRAME IS READ and released when
	// this socket is done, so a person's tabs share one allowance for
	// exactly as long as they are open. See budget.go.
	slots, releaseBudget := svc.budgets.acquire(budgetKey)
	defer releaseBudget()

	client := NewClient(AudienceOf(who.current().Grants))
	// REGISTERED BEFORE THE SNAPSHOT. See Hub.Register: the overlap is
	// deduped by the client, and the gap the other order leaves is not.
	//
	// THROUGH THE SERVICE rather than the hub directly, because the node's
	// posture is what this client must be registered AT: a tab that
	// connects to a shedding node between two health ticks would otherwise
	// be served live for the rest of the interval. See [Service.Join].
	svc.Join(client)
	defer svc.Hub().Unregister(client)
	seats := &watching{hub: svc.Hub(), client: client, chart: svc.chart}

	var writer sync.WaitGroup
	writer.Go(func() {
		defer cancel()
		writeLoop(ctx, conn, client)
	})

	client.Reply(Push(KindSnapshot, svc.Snapshot(client.Audience()), time.Now().UTC()))

	// THE CREDENTIAL IS CHECKED AGAIN FOR AS LONG AS THE SOCKET LIVES. See
	// revalidate.go for why a handshake decision is not enough and what
	// each answer does to this socket.
	var checking sync.WaitGroup
	checking.Go(func() {
		revalidate(ctx, conn, client, check, who, svc.revalidateEvery,
			svc.now, svc.Snapshot, seats.recheck)
	})
	defer checking.Wait()

	code, reason := readLoop(ctx, conn, seats, client, query, who, slots)

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
	seats *watching, client *Client, query Query, who *asking,
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
			if code, reason := seats.watch(who.context(ctx), req); code != 0 {
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
				// AS WHOEVER THE LAST CHECK SAID, not whoever opened
				// the socket: a grant narrowed an hour ago must not
				// still answer here. See revalidate.go.
				runQuery(who.context(ctx), client, query, req)
			}()
		default:
			// Unknown kinds are ignored, which is what makes new ones
			// additive on both ends.
		}
	}
}

// watching is one socket's subscription to a seat's frames, and the authority
// that decides it.
//
// # A subscription is a WRITE, and it is a READ of somebody's inbox
//
// A watch installs a row in the hub's routing index, on this node, on behalf of
// a caller, and it stays there until the socket goes away. What it buys is the
// `inbox_changed` frames for that seat — which say whose inbox moved, when and
// under which reason — so it is decided exactly as the `work_inbox` question
// about that seat is: [authz.ActionPersonRead], which admits the seat's own
// holder, whoever leads it through the chart, and the admin grant. Any
// resolved caller could watch any seat before, which made the routing index a
// way to follow somebody else's inbox that the question itself refused.
//
// THREE ANSWERS, and none of them closes the socket:
//
//   - ALLOWED: the watch is installed.
//   - REFUSED: nothing is installed, the socket watches NOTHING, and a
//     `unauthorized` error frame says so. Not a 4403 close, which is what the
//     dashboard reads as "this browser may not have the live channel at all"
//     — it stops reconnecting and says access was withdrawn — when all that
//     was refused is one seat's frames on a socket that is otherwise fine.
//   - UNDECIDABLE (the chart could not be read): nothing is installed either,
//     and an `unavailable` error frame asks the client to try again. It is the
//     only answer that is not a guess: installing would hand a seat's frames
//     to somebody this node could not show was allowed them, and a refusal
//     would tell a lead they lead nobody because this node is behind — the
//     mistake authz's three-valued chart exists to make unrepresentable.
//
// WHY THE PREVIOUS WATCH IS DROPPED on a refused or undecidable one: a watch
// is a screen saying where it now is ([Hub.Watch] keeps one seat per socket
// for that reason), so the seat it asked to leave is not one it still wants.
type watching struct {
	hub    *Hub
	client *Client
	chart  authz.Chart
}

// watchWhat is what a watch refusal's error frame names in `what`, which is
// how a client tells it from a query's.
const watchWhat = "watch"

// watch handles one `watch` frame, or reports the close code that refuses it.
//
// THE CREDENTIAL CHECK IS KEPT THOUGH EVERY SOCKET IS NOW AUTHENTICATED, and
// it is not belt-and-braces: [resolved] is what decides a socket's caller, it
// asks the auth package's exemption list, and that list is somebody else's to
// change. A watch reaching here with nobody resolved would mean the socket path
// had become exempt — and installing routing state for a caller nobody can name
// is the one outcome that must not follow silently from that. That arm is a
// close, because there is nobody left to answer.
func (w *watching) watch(ctx context.Context, req request) (websocket.StatusCode, string) {
	// WHO IS WATCHING IS THE GUARD'S ANSWER, read from the context rather
	// than from an id passed alongside — this socket reached the handler
	// past the guard, so a principal it could not resolve never gets here.
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return CloseUnauthenticated,
			"watching a seat needs a credential this node can resolve"
	}
	seat := strings.TrimSpace(req.Seat)
	if seat == "" {
		// CLEARING IS ALWAYS ALLOWED: it withdraws routing, and needs
		// nobody's authority to stop receiving something.
		w.hub.Watch(w.client, "")
		return 0, ""
	}
	if code, ok := w.decide(ctx, principal, seat); !ok {
		w.hub.Watch(w.client, "")
		w.client.Reply(Envelope{Kind: KindError, ID: req.ID, What: watchWhat,
			Error: code})
		return 0, ""
	}
	w.hub.Watch(w.client, seat)
	log.DebugContext(ctx, "stream_watch", "login", principal.Login, "seat", seat)
	return 0, ""
}

// decide asks the table whether principal may watch seat, answering the error
// code a refusal carries.
func (w *watching) decide(ctx context.Context, principal iam.Principal,
	seat string) (httpjson.Code, bool) {

	d := authz.Decide(ctx, principal, authz.ActionPersonRead,
		authz.Object{Kind: authz.KindPerson, Owner: seat}, w.chart)
	switch {
	case d.Unknown():
		log.InfoContext(ctx, "stream_watch_undecidable", "login", principal.Login,
			"seat", seat, "error", d.Err)
		return CodeUnavailable, false
	case !d.Allowed:
		log.InfoContext(ctx, "stream_watch_refused", "login", principal.Login,
			"seat", seat, "reason", string(d.Reason))
		return CodeUnauthorized, false
	}
	return "", true
}

// recheck re-decides the seat this socket watches, as whoever the latest
// revalidation resolved — the watch's own half of revalidate.go's argument: a
// decision taken once and kept for the life of the socket would let a lead
// moved off a team go on following a former report's inbox.
//
// A REFUSAL withdraws the watch and says so. AN UNDECIDABLE ANSWER KEEPS IT,
// which is the opposite of what a new watch gets and deliberately so: nothing
// has been learned against a decision this node already made, and a chart
// blip that silently unsubscribed every lead's screen would be a notification
// outage caused by the node being behind.
func (w *watching) recheck(ctx context.Context) {
	seat := w.client.Seat()
	if seat == "" {
		return
	}
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return
	}
	code, ok := w.decide(ctx, principal, seat)
	if ok || code == CodeUnavailable {
		return
	}
	w.hub.Watch(w.client, "")
	w.client.Reply(Envelope{Kind: KindError, What: watchWhat, Error: code})
}

// runQuery answers one question onto the client's own queue.
func runQuery(ctx context.Context, client *Client, query Query, req request) {
	data, err := query(ctx, req.What, req.Params)
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
		// carry a database path or a driver's own message, and holding the
		// grant a question needs does not make a reader somebody that path
		// is meant for.
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
