package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

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
// while it was — which the record that ended it, or the credential's own end,
// decides (lifetime.go). A handshake decision alone would leave a revoked
// session's tab receiving the company's state for as long as it stayed open.
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
// THE DASHBOARD HOLDS THE SAME TWO NUMBERS, as `CLOSE_UNAUTHENTICATED` and
// `CLOSE_FORBIDDEN` in its socket module, and a gate in this package's suite
// reads them from its source: each must equal the constant here, and every
// application close code declared here must have one there. They were two
// literals with nothing between them, and renumbering one side would have left
// the dashboard reconnecting for ever against a withdrawn grant.
//
// ONE MORE CLOSE IS NOT THE APPLICATION'S: [CloseUndecided], the standard's
// "try again later", for a credential this node could not decide again — see
// lifetime.go. Nothing else closes this socket for a fault: a node that cannot
// serve keeps its socket open and degrades it instead — see [FrameDegraded].
const (
	// CloseUnauthenticated ends a socket whose credential no longer
	// resolves to anybody: its session ended, expired or was revoked, or
	// its token is not one this node accepts — found when the socket was
	// decided again (see lifetime.go) — or a frame that needs a caller
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
	// HERE: a projection still catching up, a coordination store that could
	// not be reached, or a refusal by its state log — a node behind its log,
	// and also one holding a record it cannot decode or whose log is full,
	// which no wait clears. WHETHER WAITING HELPS is not this code's to say:
	// the frame's `retry_after` carries it ([Unavailable], by
	// [statelog.RetryAfter]'s rule), and its zero sends a client to another
	// node or an operator rather than back here. A surface this process does
	// not have at all is CodeUnknownQuery instead: that is this node's
	// configuration rather than its state, and no wait changes it.
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
	ErrUnauthorized = errors.New("stream: this caller may not ask that")
	ErrNotFound     = errors.New("stream: no such record")
	ErrBadParams    = errors.New("stream: query refused")
	ErrUnavailable  = errors.New("stream: not available on this node")
)

// RefusedError is [ErrUnauthorized] carrying WHY: the rule's reason and the
// grants that would have admitted the caller, which the error frame answers
// with beside the code.
//
// An error rather than a field on the answer, because the query surface
// reports a refusal the way it reports every failure — as the error — and the
// frame is built from the error in exactly one place ([runQuery]).
type RefusedError struct {
	// What is what was refused, for the log.
	What string
	Refused
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("%v: %s refused (%s)", ErrUnauthorized, e.What, e.Reason)
}

// Unwrap is what keeps every errors.Is(err, ErrUnauthorized) arm answering it.
func (e *RefusedError) Unwrap() error { return ErrUnauthorized }

// BadParamsError is [ErrBadParams] carrying the refusal's own sentence.
//
// A TYPE FOR [RefusedError]'s REASON: the frame has to carry something the code
// alone cannot, and here it is the sentence. A `bad_params` refusal is the one
// failure of a question whose text is written FOR the caller — it names the
// parameter to change and the values it accepts ("days=91, and a spend window
// is 1 to 90 company days — ask for at most 90") — and it used to reach the
// debug log and nothing else, so a person who picked a window past the spend
// history was told only that "something it needs was missing". Every other
// failure keeps its text off the wire: a query failure can carry a database
// path, and none of the rest has a reader.
type BadParamsError struct {
	What string
	// Detail is the refusal's sentence with no sentinel prefix in front of
	// it — what a screen shows beside the code.
	Detail string
}

func (e *BadParamsError) Error() string {
	return fmt.Sprintf("%v: %s: %s", ErrBadParams, e.What, e.Detail)
}

// Unwrap makes a BadParamsError an [ErrBadParams] to errors.Is.
func (e *BadParamsError) Unwrap() error { return ErrBadParams }

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

	// Seat is whose record a `watch` frame asks to be a recipient for —
	// a seat's handle, or a login, which is resolved to the record its
	// holder's notices are kept under — and empty clears the
	// subscription. See [watching] and [Hub.Watch].
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
// The credential is the session cookie a signed-in browser sends on its own —
// a browser cannot set a header on a WebSocket constructor, so the cookie is
// the whole of what the dashboard presents — or an Authorization bearer from
// any other client. Nothing is read off the URL: a query string is written
// into every proxy's access log ([auth.Guard.Credential]).
//
// # Who is calling is the GUARD's answer, read once
//
// It used to be a second answer: this handler re-read the credential through
// the guard's Tier A arm alone, so a person signed in with a session cookie —
// which is every browser once `/auth` exists — was refused here while every
// REST route beside it served them. The guard resolves both shapes, the
// middleware has already run it for this path, and the handler reads the
// result from the context like every other surface does.
func Handler(guard *auth.Guard, origins CrossSite, svc *Service, query Query) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, refusal := resolved(guard, w, r)
		// THE RESOLVED REQUEST'S CONTEXT IS THIS HANDLER'S OWN, carrying
		// the guard's answer: see [auth.Guard.Middleware], which reads it
		// the same way and says why contextcheck cannot see that. The
		// socket's lifetime below is that same context.
		//nolint:contextcheck // derived from r.Context(); see the paragraph above
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
		// WHAT THE SOCKET WAS OPENED WITH, which is what decides it again
		// for as long as it is open — see lifetime.go.
		//nolint:contextcheck // the resolved request's; see where it is resolved
		ends, _ := auth.Lifetime(r.Context())
		cred := credential{
			who: &asking{principal: principal},
			//nolint:contextcheck // the resolved request's, as above
			opened: openedWith(r.Context(), principal),
			ends:   ends,
			decide: deciderFor(guard, r),
			key:    guard.PresentedKey(r),
		}

		// THE HANDSHAKE'S ORIGIN IS JUDGED BY THE WRITES' RULE, and only
		// a handshake's: see [auth.CSRF.RefuseCrossSite] for why the
		// library's own check is switched off below, and why the plain
		// GET the dashboard re-asks this path with is not judged.
		if upgrading(r) && origins.RefuseCrossSite(w, r) {
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// JUDGED ABOVE, against api.external_url and every
			// api.auth.allowed_origins entry. The library's own check
			// compares Origin with this request's Host header, which a
			// second hostname and a Host-rewriting proxy both fail.
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Debug("stream_accept_failed", "error", err)
			return
		}
		//nolint:contextcheck // the resolved request's; see where it is resolved
		serveSocket(r.Context(), conn, svc, query, cred)
	})
}

// CrossSite judges a handshake's `Origin` and answers the refusal itself. The
// engine hands in the same [auth.CSRF] every write is judged by, so the socket
// and the routes beside it cannot disagree about where this deployment is
// reached.
type CrossSite interface {
	RefuseCrossSite(w http.ResponseWriter, r *http.Request) bool
}

// upgrading reports whether a request asks to become a WebSocket, which is
// what a browser's handshake carries and the dashboard's plain re-ask of this
// path does not.
func upgrading(r *http.Request) bool {
	for _, value := range r.Header.Values("Upgrade") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "websocket") {
				return true
			}
		}
	}
	return false
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
		// NO REFUSAL CAN BE PENDING HERE: the middleware writes a
		// resolved caller's refusal itself on every route that refusal
		// applies to — a lost seat and an enrolment-only session both
		// reach the socket — so a request it answered and passed on is
		// one it did not refuse.
		return r, nil
	}
	return guard.Resolve(w, r)
}

// serveSocket runs one connection until it closes.
func serveSocket(ctx context.Context, conn *websocket.Conn,
	svc *Service, query Query, cred credential,
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	who := cred.who

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
	seats := &watching{hub: svc.Hub(), client: client, chart: svc.chart,
		holders: svc.holders}

	var writer sync.WaitGroup
	writer.Go(func() {
		defer cancel()
		writeLoop(ctx, conn, client)
	})

	client.Reply(Push(KindSnapshot, svc.Snapshot(client.Audience()), time.Now().UTC()))

	// THE CREDENTIAL IS DECIDED AGAIN FOR AS LONG AS THE SOCKET LIVES, on
	// what can change it and at nothing else. See lifetime.go for why a
	// handshake decision is not enough and what each answer does to this
	// socket. REGISTERED with one decision pending, so a record that
	// landed between the handshake and this line is not missed.
	l := newListener(cred.opened)
	defer svc.listeners.add(l)()
	var deciding sync.WaitGroup
	deciding.Go(func() {
		keepDecided(ctx, conn, client, l, cred, svc.decisions, svc.now, svc.Snapshot,
			seats.recheck)
	})
	defer deciding.Wait()

	code, reason := readLoop(ctx, conn, seats, client, query, who, svc.interval,
		svc.queries)

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
//
// healthEvery is the shared tick's cadence, which is when a degraded posture
// can next change — the hint a query refused on one carries.
//
// node is the ceiling every socket on this node shares — see [queryCeiling].
func readLoop(ctx context.Context, conn *websocket.Conn,
	seats *watching, client *Client, query Query, who *asking,
	healthEvery time.Duration, node chan struct{},
) (websocket.StatusCode, string) {
	// THIS SOCKET'S CONCURRENCY BOUND — see [MaxInFlightQueries] for why
	// four, and why per socket. Queries run on their own goroutines so a
	// store scan cannot stall the live feed, and a burst past the bound
	// queues here rather than piling into the engine's connection pool.
	slots := make(chan struct{}, MaxInFlightQueries)
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
			//
			// AND IT SAYS WHEN TO ASK AGAIN, like every other
			// `unavailable` frame: `retry_after` is never omitted,
			// because a frame without it is what a node too old to say
			// sends. The posture is re-derived on the shared health
			// tick — a node shedding or stuck on a configuration — so
			// one tick is the soonest this node could answer
			// differently, and the health frame the tab keeps
			// receiving says why. No state-log refusal is behind it,
			// so none is named.
			if !client.Posture().ServesQueries() {
				env := queryError(req, CodeUnavailable)
				env.unavailable(Unavailable{
					RetryAfter: httpjson.RetrySeconds(healthEvery),
				})
				client.Reply(env)
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
				// AND THE NODE'S CEILING, waited for HERE rather than
				// on the reader: what holds it may be another socket's
				// burst, and this socket's keepalive must not wait on
				// that. A socket that closes while it waits runs
				// nothing — there is nobody left to answer.
				select {
				case node <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-node }()
				// AS WHOEVER THE LAST DECISION SAID, not whoever
				// opened the socket: a grant narrowed an hour ago must
				// not still answer here. See lifetime.go.
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
//     and an `unavailable` error frame says when to try again — its
//     `retry_after`, zero where waiting will not change the answer
//     ([watchAnswer.frame]). It is the only answer that is not a guess:
//     installing would hand a seat's frames to somebody this node could not
//     show was allowed them, and a refusal would tell a lead they lead nobody
//     because this node is behind — the mistake authz's three-valued chart
//     exists to make unrepresentable.
//
// WHY THE PREVIOUS WATCH IS DROPPED on a refused or undecidable one: a watch
// is a screen saying where it now is ([Hub.Watch] keeps one seat per socket
// for that reason), so the seat it asked to leave is not one it still wants.
//
// # The name is RESOLVED, by the one owner function
//
// `inbox_changed` is pushed to the name a person's notices are KEPT under —
// their seat when the identity directory binds them to one — so a watch is
// resolved through [iam.OwnerOf], exactly as the `work_inbox` question about
// the same name is, and installed on what it resolved to. Decided and routed
// on the name as sent, a lead watching their report by login was refused as
// leading nobody called that, and an administrator's watch of a bound person's
// login was installed on the login, where no frame is ever pushed. And the
// directory is asked only once the watch has been decided as far as it can be
// without the record ([watching.mayLook]): a caller who leads nobody is
// refused before anything is looked up, so what the directory says about a
// login is never theirs to learn.
type watching struct {
	hub     *Hub
	client  *Client
	chart   authz.Chart
	holders iam.Holders
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
	name := strings.TrimSpace(req.Seat)
	if name == "" {
		// CLEARING IS ALWAYS ALLOWED: it withdraws routing, and needs
		// nobody's authority to stop receiving something.
		w.hub.Watch(w.client, "")
		return 0, ""
	}
	seat, answer := w.resolve(ctx, principal, name)
	if answer != nil {
		w.hub.Watch(w.client, "")
		w.client.Reply(answer.frame(req.ID))
		return 0, ""
	}
	w.hub.Watch(w.client, seat)
	log.DebugContext(ctx, "stream_watch", "login", principal.Login,
		"asked", name, "seat", seat)
	return 0, ""
}

// resolve is the record a `watch` frame's name addresses, decided: what the
// watch is installed on, or — non-nil — the answer it is refused with.
//
// [iam.OwnerOf] answers the name — the caller's own names are their own
// record, somebody else's login is their holder's, and anything else is read
// as the seat it names — and the watch is decided on what it resolved to. A
// login nobody holds, or one whose holder's seat is gone, is decided on the
// name as typed, which nobody leads, so only the admin grant learns it names
// no record (`not_found`, which no retry changes); a directory this node
// cannot read is `unavailable`, and its own words — which name the seat a
// login is bound to — go to the log.
func (w *watching) resolve(ctx context.Context, principal iam.Principal,
	name string) (string, *watchAnswer) {

	owner, _, err := iam.OwnerOf(ctx, principal, name, w.holders,
		w.mayLook(principal))
	var looked *iam.LookRefused
	switch {
	case errors.As(err, &looked):
		var answer *watchAnswer
		if errors.As(looked.Err, &answer) {
			return "", answer
		}
		return "", &watchAnswer{code: CodeUnavailable, cause: looked.Err}
	case errors.Is(err, iam.ErrNoHolder), errors.Is(err, iam.ErrHolderUnseated):
		if answer := w.decide(ctx, principal, name); answer != nil {
			return "", answer
		}
		return "", &watchAnswer{code: CodeNotFound}
	case err != nil:
		log.InfoContext(ctx, "stream_watch_unresolvable", "login", principal.Login,
			"asked", name, "error", err.Error())
		return "", &watchAnswer{code: CodeUnavailable, cause: err}
	}
	if answer := w.decide(ctx, principal, owner); answer != nil {
		return "", answer
	}
	return owner, nil
}

// mayLook is the watch's decision BEFORE the identity directory is asked whose
// record somebody else's login is — see [iam.MayLook] and
// [authz.Object.Unresolved]: the same verb [watching.decide] asks, of a record
// nobody has resolved yet. A caller who could be admitted only as the lead of
// whoever holds the login lets the lookup happen, and the watch is decided
// again on the record; every other answer is the frame the watch is answered
// with.
func (w *watching) mayLook(principal iam.Principal) iam.MayLook {
	return func(ctx context.Context, login string) error {
		d := authz.Decide(ctx, principal, authz.ActionPersonRead, authz.Object{
			Kind: authz.KindPerson, Owner: login, Unresolved: true,
		}, w.chart, time.Now())
		switch {
		case d.Allowed, errors.Is(d.Err, authz.ErrUnresolved):
			return nil
		case d.Unknown():
			log.InfoContext(ctx, "stream_watch_undecidable", "login",
				principal.Login, "asked", login, "error", d.Err)
			return &watchAnswer{code: CodeUnavailable, cause: d.Err}
		}
		log.InfoContext(ctx, "stream_watch_refused", "login", principal.Login,
			"asked", login, "reason", string(d.Reason))
		return &watchAnswer{code: CodeUnauthorized,
			refused: NewRefused(d.Reason, d.Grants)}
	}
}

// watchAnswer is a watch that was not installed: the code its error frame
// carries, the refusal on authority behind an `unauthorized` one, and what made
// an `unavailable` one undecidable. It travels through [iam.OwnerOf] as the
// error the gate refused with, so the frame it becomes is the one a decision on
// a seat would have made.
type watchAnswer struct {
	code    httpjson.Code
	refused *Refused

	// cause is what this node could not read behind an `unavailable`
	// answer — the chart the decision asks, or the directory a login
	// resolves through — and it is read for ONE thing, the frame's hint
	// ([watchAnswer.frame]). Its words go to the log and never onto the
	// socket.
	cause error
}

func (a *watchAnswer) Error() string { return "stream: watch refused: " + string(a.code) }

// frame is the error frame the answer is sent as.
//
// AN `unavailable` ONE SAYS WHEN TO ASK AGAIN, like every other `unavailable`
// frame on this socket: a `retry_after` over what could not be read —
// [HealthInterval] where nothing better is known — and ZERO where waiting will
// not change it, a chart or a directory whose log is full or holds a record
// this node cannot decode. It went out bare, which is the frame a node too old
// to say sends, so the dashboard re-asked at its own fixed interval a watch
// this node would refuse until an operator acted, for as long as the tab
// stayed open.
//
// THE HINT IS [UnavailableOf]'s, taken whole rather than worked out here from
// the same two calls. That reading is the one every query's `unavailable`
// takes on both transports, and its own doc says no surface decides the hint
// for itself: a copy of its arithmetic here would agree with it only until
// the reading learned a case of its own — as it did for a node with no
// company — and then a watch and a query refused over one cause would tell
// the same tab two different times to come back.
//
// THE HINT ALONE, never the refusal's code or words. A client does one thing
// with a watch's `unavailable` — decides when to send the watch again — and the
// hint is the whole of that; the words behind it stay in the log
// ([watching.resolve]), because what the directory says about a login names
// the seat it is bound to.
func (a *watchAnswer) frame(id int64) Envelope {
	env := Envelope{Kind: KindError, ID: id, What: watchWhat, Error: a.code,
		Refused: a.refused}
	if a.code == CodeUnavailable {
		env.unavailable(Unavailable{RetryAfter: UnavailableOf(a.cause).RetryAfter})
	}
	return env
}

// decide asks the table whether principal may watch seat: nil when it may, and
// otherwise the answer it is refused with — for a refusal on authority, the
// reason and the grants a query's refusal carries too.
func (w *watching) decide(ctx context.Context, principal iam.Principal,
	seat string) *watchAnswer {

	d := authz.Decide(ctx, principal, authz.ActionPersonRead,
		authz.Object{Kind: authz.KindPerson, Owner: seat}, w.chart, time.Now())
	switch {
	case d.Unknown():
		log.InfoContext(ctx, "stream_watch_undecidable", "login", principal.Login,
			"seat", seat, "error", d.Err)
		return &watchAnswer{code: CodeUnavailable, cause: d.Err}
	case !d.Allowed:
		log.InfoContext(ctx, "stream_watch_refused", "login", principal.Login,
			"seat", seat, "reason", string(d.Reason))
		return &watchAnswer{code: CodeUnauthorized, refused: NewRefused(d.Reason, d.Grants)}
	}
	return nil
}

// recheck re-decides the seat this socket watches, as whoever the latest
// decision of its credential resolved — the watch's own half of lifetime.go's
// argument: a decision taken once and kept for the life of the socket would
// let a lead moved off a team go on following a former report's inbox. It runs
// whenever the credential is decided again, and a published company — the one
// moment a lead can move — is among the things that do it.
//
// A REFUSAL withdraws the watch and says so. AN UNDECIDABLE ANSWER KEEPS IT,
// which is the opposite of what a new watch gets and deliberately so: nothing
// has been learned against a decision this node already made, and a chart
// blip that silently unsubscribed every lead's screen would be a notification
// outage caused by the node being behind.
//
// WITHDRAWN ONLY IF IT IS STILL THE WATCH THAT WAS DECIDED. This runs on the
// decision's goroutine and the read loop installs watches on its own, so a
// refusal of the seat read here must not clear one the read loop allowed in
// the meantime — see [Hub.UnwatchIf]. A watch that moved is the read loop's
// to decide, and the next decision decides it again.
func (w *watching) recheck(ctx context.Context) {
	seat, watch := w.hub.Watching(w.client)
	if seat == "" {
		return
	}
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return
	}
	answer := w.decide(ctx, principal, seat)
	if answer == nil || answer.code == CodeUnavailable {
		return
	}
	if w.hub.UnwatchIf(w.client, watch) {
		w.client.Reply(answer.frame(0))
	}
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
		env := queryError(req, CodeUnauthorized)
		var refused *RefusedError
		if errors.As(err, &refused) {
			env.Refused = &refused.Refused
		}
		client.Reply(env)
	case errors.Is(err, ErrNotFound):
		client.Reply(queryError(req, CodeNotFound))
	case errors.Is(err, ErrBadParams):
		// DEBUG, NOT WARN: it is not this node's failure, and a poll
		// behind a bad request writes a line per tick for as long as the
		// screen is open. The sentence travels on the frame's `detail`
		// (see [BadParamsError]), so the caller reads the fix without
		// anybody turning debug on.
		log.DebugContext(ctx, "stream_query_refused", "what", req.What, "error", err)
		refusal := queryError(req, CodeBadParams)
		var bad *BadParamsError
		if errors.As(err, &bad) {
			refusal.Detail = bad.Detail
		}
		client.Reply(refusal)
	case errors.Is(err, ErrUnavailable):
		// THE REFUSAL, ITS WORDS AND ITS HINT RIDE THE FRAME, because a
		// node catching up and a node that will refuse this read until an
		// operator acts are both `unavailable` — see [Unavailable].
		env := queryError(req, CodeUnavailable)
		env.unavailable(UnavailableOf(err))
		client.Reply(env)
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
