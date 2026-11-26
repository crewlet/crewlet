package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/presence"
)

// The close codes this socket ends a connection with, beyond the standard's.
//
// A close code rides a close FRAME, so a handshake refused before the upgrade
// has none (see [Handler]: 401, and the browser reports 1006). These are for
// an OPEN socket whose credential stopped answering (lifetime.go).
//
// Two codes because the dashboard's recovery differs: 4401 "names nobody now"
// (re-dial with the cookie, sign in if refused), 4403 "names somebody whose
// seat is gone" (stop). They echo HTTP statuses in the 4000-4999 application
// range. The dashboard's `CLOSE_UNAUTHENTICATED` and `CLOSE_FORBIDDEN` must
// equal them; a gate in this suite reads its source. A node that cannot serve
// degrades its socket ([FrameDegraded]) rather than closing it.
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

// The error codes a query answer can carry: THE SOCKET'S SUBSET OF
// [httpjson.Code], the engine's one refusal vocabulary.
// TestTheSocketAndRestShareOneTable holds each to an entry on that table.
//
// Untyped, because the REST twin answers the same six codes as strings.
//
// Codes, not prose: a sentence in the frame would make every rewording a case
// the client does not handle. The sentence belongs to the code
// ([httpjson.Code.Message]), which REST answers with.
const (
	CodeUnknownQuery = "unknown_query"
	CodeUnauthorized = "unauthorized"
	CodeQueryFailed  = "query_failed"

	// CodeNotFound is a question this node understood, about a record it
	// does not hold. DISTINCT FROM query_failed, because a client acts on
	// them differently: "no such item" is a dead link to show the person,
	// and "the query failed" is a retry.
	CodeNotFound = "not_found"

	// CodeBadParams is a question this node understood and REFUSED: a parameter
	// missing, malformed, or outside the accepted set.
	//
	// Distinct from query_failed because the fault is the caller's: a
	// query_failed is retried, a bad_params never succeeds however often sent,
	// and collapsed it was a client bug shown as an engine fault, retried on
	// every poll and logged as a WARNING.
	//
	// The reason does not travel (codes, not prose, as above); the refusal's own
	// message, naming the field and the accepted values, is logged at DEBUG.
	CodeBadParams = "bad_params"

	// CodeUnavailable is a question this node understood and cannot answer HERE:
	// a projection catching up, a coordination store not reached, or a state log
	// refusal (a node behind its log, or holding a record it cannot decode or
	// whose log is full, which no wait clears). Whether waiting helps is the
	// frame's `retry_after` to say ([Unavailable], by [statelog.RetryAfter]'s
	// rule); its zero sends a client to another node or an operator. A surface
	// this process lacks is CodeUnknownQuery: configuration, not state.
	//
	// It must never be flattened into an empty result: "this company has no work"
	// is an answer a person acts on, so a node still reconciling must say "ask me
	// in a moment" rather than "there is nothing".
	CodeUnavailable = "unavailable"

	// CodeArtifactTrashed is a question about an artifact's CONTENT — its
	// datastore, its room — asked by somebody the artifact is disclosed to,
	// while it is in the trash. The REST routes' `409 artifact_trashed`,
	// under the same code: the artifact's state, which its owner's restore
	// changes. DISTINCT FROM not_found, which is what the same question is
	// answered with for a caller the artifact is answered as absent to: told
	// not_found, a client stopped asking about content a restore brings back
	// whole, and a screen called a binned sheet one that does not exist.
	CodeArtifactTrashed = "artifact_trashed"

	// The five answers to a question about a configuration revision this
	// node cannot open, under the REST routes' own codes and in their words
	// (internal/api/configapi's UnreadableRevisionError): a revision this
	// node keeps sealed under a key its keyring lost, one it keeps stored
	// without a seal, the fleet's revision met only through its pointer,
	// sealed under a key this node was never given or put there without a
	// seal, and a revision of either origin sealed in a form this build does
	// not read. The frame's `detail` says what happened and what fixes it;
	// each cause is fixed in a different place, which is why they are five
	// codes and not one. Answered `query_failed`, the config screen called a
	// missing key a failure nobody could explain.
	CodeUnreadableRevision      = "unreadable_revision"
	CodeUnsealedRevision        = "unsealed_revision"
	CodeUnreadableFleetRevision = "unreadable_fleet_revision"
	CodeUnsealedFleetRevision   = "unsealed_fleet_revision"
	CodeUnknownSealRevision     = "unknown_seal_revision"
)

// The failures a query surface reports precisely; everything else is a
// query_failed.
var (
	ErrUnknownQuery = errors.New("stream: unknown query")
	// ErrUnauthorized is what a [RefusedError] is to [errors.Is]. Only a
	// RefusedError reaches a client as a refusal: a bare one names no
	// decision.
	ErrUnauthorized = errors.New("stream: this caller may not ask that")
	ErrNotFound     = errors.New("stream: no such record")
	ErrBadParams    = errors.New("stream: query refused")
	ErrUnavailable  = errors.New("stream: not available on this node")

	// ErrArtifactTrashed is a question about the content of an artifact in
	// the trash, answered [CodeArtifactTrashed].
	ErrArtifactTrashed = errors.New("stream: the artifact is in the trash")
)

// RefusedError is [ErrUnauthorized] carrying WHY: the code the authority
// table's mapping gives the refusal, and the rule's reason and the grants that
// would have admitted the caller, which the error frame answers with beside
// it. Built by [RefusedFor] and by nothing else.
//
// An error rather than a field on the answer, because the query surface
// reports a refusal the way it reports every failure — as the error — and the
// frame is built from the error in exactly one place ([runQuery]).
type RefusedError struct {
	// What is what was refused, for the log.
	What string
	// Code is the frame's code, as [authz.RefusalFields] gives it.
	Code httpjson.Code
	Refused
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("%v: %s refused (%s)", ErrUnauthorized, e.What, e.Reason)
}

// Unwrap is what keeps every errors.Is(err, ErrUnauthorized) arm answering it.
func (e *RefusedError) Unwrap() error { return ErrUnauthorized }

// errUnspokenRefusal is a refusal on authority the mapping words in a code, or
// with a field, this socket's vocabulary does not have — see [RefusedFor].
var errUnspokenRefusal = errors.New("stream: a refusal this socket cannot say")

// RefusedFor answers one refusal the authority table made, for a question or
// a watch alike: the code and fields of its ONE MAPPING
// ([authz.RefusalFields]), as the REST twin does.
//
//   - `unauthorized` is a [RefusedError] with the rule's reason, the grants
//     that would admit the caller and, below the verb's artifact role, the
//     role it needs.
//   - `not_found` (NO RELATION to an artifact) is [ErrNotFound], byte for byte
//     the frame for an id nothing holds.
//
// A code or field this socket lacks (`step_up_required`, `token_refused`) is a
// fault, never a frame: reaching one is this build's mistake, `query_failed`.
func RefusedFor(what string, d authz.Decision) error {
	_, code, detail := authz.RefusalFields(d)
	switch code {
	case CodeNotFound:
		if len(detail) == 0 {
			return fmt.Errorf("%w: %s", ErrNotFound, what)
		}
	case CodeUnauthorized:
		if refused, ok := frameRefusal(detail); ok {
			return &RefusedError{What: what, Code: code, Refused: refused}
		}
	}
	return fmt.Errorf("%w: %s was refused %s (%s)", errUnspokenRefusal, what, code, d.Reason)
}

// frameRefusal is the mapping's fields as an `unauthorized` frame carries
// them, and whether the frame carries every one: a reason and a grants list
// always, a role where the mapping gave one, and nothing else.
func frameRefusal(detail httpjson.Detail) (Refused, bool) {
	var refused Refused
	for key, value := range detail {
		ok := false
		switch key {
		case authz.DetailReason:
			refused.Reason, ok = value.(string)
		case authz.DetailGrants:
			refused.Grants, ok = value.([]string)
		case authz.DetailRole:
			refused.Role, ok = value.(string)
		}
		if !ok {
			return Refused{}, false
		}
	}
	return refused, refused.Reason != "" && refused.Grants != nil
}

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

// RevisionError is a question about a configuration revision this node cannot
// open, carrying the code and the sentence the REST route answers it with: one
// of the five Code…Revision codes, and the detail that names what fixes it.
//
// A TYPE FOR [BadParamsError]'s REASON: the frame has to carry something the
// code alone cannot, and here it is which key or which document to restore.
type RevisionError struct {
	What   string
	Code   httpjson.Code
	Detail string
}

func (e *RevisionError) Error() string {
	return fmt.Sprintf("stream: %s: %s: %s", e.What, e.Code, e.Detail)
}

// Query answers one client question.
//
// WHO IS ASKING TRAVELS IN THE CONTEXT, which the guard resolved before this
// handler ran — never as an argument beside it, because two identities on one
// call are two chances to answer about different people. A query the caller
// may not ask returns what [RefusedFor] makes of the decision that refused it,
// rather than deciding for itself what to do about it, so the refusal reaches
// the client as a code it handles, in the words every surface uses.
type Query func(ctx context.Context, what string, params map[string]any) (any, error)

// request is one client-to-server frame.
type request struct {
	Kind string `json:"kind"`
	// ID is a `query` frame's own, which its answer echoes, and what a
	// `query_cancel` frame names the question it gives up on by.
	ID     int64          `json:"id"`
	What   string         `json:"what"`
	Params map[string]any `json:"params"`

	// WaitMS is how long a `query` frame's client waits for its answer once
	// it sent the frame, in milliseconds, and zero where it names no wait: the
	// question is answered within it or not at all ([request.waited]).
	WaitMS int64 `json:"wait_ms"`

	// Seat is whose record a `watch` frame asks to be a recipient for —
	// a seat's handle, or a login, which is resolved to the record its
	// holder's notices are kept under — and empty clears the
	// subscription. See [watching] and [Hub.Watch].
	Seat string `json:"seat"`

	// Artifact is the artifact an `artifact_join`, `artifact_leave` or
	// artifact `presence` frame is about, by its id. Since is a join's
	// catch-up point: the `through` the socket last held, zero for none.
	Artifact string `json:"artifact"`
	Since    int64  `json:"since"`

	// Room is the kind of room a `presence` frame is about, which names the
	// field its room is named by: `artifact` for an artifact's room
	// (Artifact, joined first with `artifact_join`), and `chat` for a chat
	// room's (ChannelID, which the frame itself joins: a tab looks at one
	// room at a time, and an empty ChannelID leaves the one it was in). A
	// frame naming no kind this build keeps is dropped.
	Room presence.RoomKind `json:"room"`

	// Activity and Focus are a `presence` frame's: what the socket is doing
	// in the room and where it is looking. Who is doing it is never the
	// frame's to say: it is the principal the socket resolved.
	Activity presence.Activity `json:"activity"`
	Focus    presence.Focus    `json:"focus"`

	// ChannelID is the chat room a chat `presence` frame is about. A field
	// on the shared request rather than a second decoder, because a frame
	// is decoded before its kind is known.
	ChannelID string `json:"channel_id"`
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
// The credential is the session cookie (a browser cannot set a header on a
// WebSocket constructor) or an Authorization bearer. None is read off the URL,
// which proxies log ([auth.Guard.Credential]); the tab's READER is no
// credential, and the guard has refused a handshake made for anybody else
// ([auth.ReaderParam]). Who is calling is the GUARD's answer, read once from
// the context.
//
// chat pushes the company's own chat ([ChatHub]); a NIL ONE IS A NODE THAT
// SERVES NO CHATLET and every method is a no-op, so the socket needs no
// branch. A parameter because the hub is the process's, not this socket's.
func Handler(guard *auth.Guard, origins CrossSite, svc *Service, query Query,
	chat *ChatHub) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admitted, ok := Admit(guard, origins, w, r) //nolint:contextcheck // decided on the request's own context
		if !ok {
			return
		}
		conn, err := admitted.Accept(w)
		if err != nil {
			log.Debug("stream_accept_failed", "error", err)
			return
		}
		//nolint:contextcheck // the resolved request's; see [Admit]
		serveSocket(admitted.r.Context(), conn, svc, query, chat, admitted.cred)
	})
}

// Admitted is a socket handshake every engine socket's rules let through:
// resolved to a principal whose stage may act, from an Origin the CSRF rule
// accepts. /ws/stream and /ws/dictation are both admitted here, so the two
// cannot come to disagree about who may open a socket.
type Admitted struct {
	r    *http.Request
	cred credential
}

// Principal is who the handshake resolved to.
func (a *Admitted) Principal() iam.Principal { return a.cred.who.current() }

// Request is the resolved handshake: its context carries the principal.
func (a *Admitted) Request() *http.Request { return a.r }

// Admit decides a socket handshake, answering a refusal itself (503 while
// identity cannot be read, the guard's own refusal, 401, the stage refusal,
// the Origin refusal) and reporting false.
func Admit(guard *auth.Guard, origins CrossSite, w http.ResponseWriter,
	r *http.Request) (*Admitted, bool) {

	r, refusal := resolved(guard, w, r)
	// THE RESOLVED REQUEST'S CONTEXT IS THE SOCKET'S OWN, carrying the
	// guard's answer: see [auth.Guard.Middleware], which reads it the same
	// way and says why contextcheck cannot see that. The socket's lifetime
	// is that same context.
	//nolint:contextcheck // derived from r.Context(); see the paragraph above
	principal, how := iam.From(r.Context())
	if refusal == nil && how == iam.Unknown {
		// 503 AND NEVER 401, for the guard's own reason: a browser
		// reads 401 as "sign in again" and discards the cookie, and
		// this node merely could not read its identity estate.
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable,
			auth.RetryIdentitySeconds)
		return nil, false
	}
	if refusal != nil {
		// A PERSON WHOSE SEAT IS GONE is resolved and still refused,
		// with the guard's own code and detail: a socket is a
		// surface that acts as the seat.
		httpjson.FailWith(w, refusal.Status, refusal.Code,
			map[string]string{"detail": refusal.Detail})
		return nil, false
	}
	if how != iam.Resolved {
		// REFUSED BEFORE THE UPGRADE: accepting a credential this node rejects, only
		// to close it a moment later, would let anyone open a socket here.
		//
		// A close code rides a close FRAME, so a handshake that never completed
		// cannot carry one: the browser reports 1006 (like a stopped engine) and hides
		// the status. The client re-asks over plain HTTP, where this route answers a
		// GET without an Upgrade header 401 (refused) or 426 (accepted); the
		// dashboard's token gate relies on that pairing (`probeRefusal` in
		// dashboard/src/protocol/socket.ts).
		//
		// Real JSON, not http.Error: that sets text/plain AND nosniff, which stops a
		// strict client parsing the body. The envelope is every REST route's.
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return nil, false
	}
	// EVERY RESOLVED PRINCIPAL WHOSE STAGE MAY ACT OPENS THE SOCKET (ADR-0033):
	// what it carries is the overview, everybody's; what a reader may not read (a
	// seat's body, the audit trail, an inbox without Worklet) is withheld per kind
	// and payload by the audience built below, never by refusing the socket.
	//
	// The STAGE is the authority table's first check and the query registry's:
	// this holds the socket to the rule every other surface states. Refused
	// before the upgrade, where the page's HTTP re-ask can see the status.
	if !principal.Stage.MayAct() {
		authz.EnvelopeRefusal(w, r, authz.Policy{},
			authz.Decision{Reason: authz.ReasonStage})
		return nil, false
	}
	// WHAT THE SOCKET WAS OPENED WITH, which is what decides it again
	// for as long as it is open (lifetime.go).
	//nolint:contextcheck // the resolved request's; see where it is resolved
	ends, _ := auth.Lifetime(r.Context())
	cred := credential{
		who:    &asking{principal: principal},
		opened: openedWith(principal),
		ends:   ends,
		decide: deciderFor(guard, r),
	}

	// THE HANDSHAKE'S ORIGIN IS JUDGED BY THE WRITES' RULE, and only
	// a handshake's: see [auth.CSRF.RefuseCrossSite] for why the
	// library's own check is switched off below, and why the plain
	// GET the dashboard re-asks this path with is not judged.
	if auth.WebSocketUpgrade(r) && origins.RefuseCrossSite(w, r) {
		return nil, false
	}
	return &Admitted{r: r, cred: cred}, true
}

// Accept upgrades an admitted handshake. A plain GET, which a dashboard
// re-asks a socket's path with to read why a handshake failed, is answered
// 426 here.
func (a *Admitted) Accept(w http.ResponseWriter) (*websocket.Conn, error) {
	return websocket.Accept(w, a.r, &websocket.AcceptOptions{
		// JUDGED IN [Admit], against api.external_url and every
		// api.auth.allowed_origins entry. The library's own check
		// compares Origin with this request's Host header, which a
		// second hostname and a Host-rewriting proxy both fail.
		InsecureSkipVerify: true,
	})
}

// KeepDecided decides an admitted socket's credential again for as long as it
// is open, closing it as /ws/stream's are (lifetime.go: 4401, 4403, 1013), and
// returns when ctx ends or it closed the socket. For a socket other than this
// package's own, which has no audience or watch to decide again.
func (s *Service) KeepDecided(ctx context.Context, conn *websocket.Conn, a *Admitted) {
	l := newListener(a.cred.opened)
	defer s.listeners.add(l)()
	defer containPanic(ctx, "socket_decide", func() {
		_ = conn.Close(CloseUndecided, "Sign-in could not be decided again. Reconnecting.")
	})
	keepDecided(ctx, conn, l, a.cred, s.seatOf, s.now, nil, nil)
}

// CrossSite judges a handshake's `Origin` and answers the refusal itself. The
// engine hands in the same [auth.CSRF] every write is judged by, so the socket
// and the routes beside it cannot disagree about where this deployment is
// reached.
type CrossSite interface {
	RefuseCrossSite(w http.ResponseWriter, r *http.Request) bool
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
	svc *Service, query Query, chat *ChatHub, cred credential,
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	who := cred.who

	// THE AUDIENCE IS DECIDED ONCE, here, from the principal the
	// handshake resolved and the company this node runs — whose seats'
	// trail it reads included — and never moved while the socket is open
	// (see [Client.audience] and lifetime.go).
	client := NewClient(svc.AudienceFor(ctx, who.current()))
	// REGISTERED BEFORE THE SNAPSHOT. See Hub.Register: the overlap is
	// deduped by the client, and the gap the other order leaves is not.
	//
	// THROUGH THE SERVICE rather than the hub directly, because the node's
	// posture is what this client must be registered AT: a tab that
	// connects to a shedding node between two health ticks would otherwise
	// be served live for the rest of the interval. See [Service.Join].
	svc.Join(client)
	// LEAVING EVERY ROOM, ITS PRESENCE INCLUDED, on every path out: a tab
	// that closed must not stay in an artifact's room for its peers to see.
	defer svc.leave(client)
	// AND JOINED TO CHAT, as the seat this socket's principal acts as and
	// holds Chatlet through ([chatViewerOf]) — the SAME principal the
	// audience above was built from, decided once: a principal that moves
	// closes this socket (lifetime.go), so the seat is never resolved
	// again. A principal that may not be shown chat is not joined at all.
	// Left before the queue closes, so the socket is out of every room's
	// presence first.
	chat.join(client, chatViewerOf(who.current()))
	defer chat.leave(client)
	seats := &watching{hub: svc.Hub(), client: client, chart: svc.chart,
		holders: svc.holders, now: svc.now}
	// THE SOCKET'S ROOM FRAMES, in the order it sent them, on a goroutine of
	// their own: a join reads the artifact and its datastore, and the read
	// loop must not wait on that to answer a ping. One goroutine rather than
	// one per frame, so a leave can never overtake the join it undoes.
	rooms := &socketRooms{ops: make(chan request, roomOpsDepth)}
	var roomWorker sync.WaitGroup
	roomWorker.Go(func() {
		for req := range rooms.ops {
			svc.roomOp(who.context(ctx), client, chat, req)
		}
	})

	var writer sync.WaitGroup
	writer.Go(func() {
		defer cancel()
		defer containPanic(ctx, "socket_write", func() {})
		writeLoop(ctx, conn, client)
	})

	// Stamped by the SERVICE's clock, as every push after it is: a
	// wall-clock stamp here dated the first frame in another time than the
	// rest under a pinned or simulated clock (Options.Now).
	client.Reply(Push(KindSnapshot, svc.Snapshot(client.Audience()), svc.now()))

	// THE CREDENTIAL IS ENDED OR DECIDED AGAIN FOR AS LONG AS THE SOCKET
	// LIVES, on what can change it and at nothing else. See lifetime.go for
	// why a handshake decision is not enough and what each answer does to
	// this socket. REGISTERED with one decision pending, so a record that
	// landed between the handshake and this line is not missed.
	l := newListener(cred.opened)
	defer svc.listeners.add(l)()
	var deciding sync.WaitGroup
	deciding.Go(func() {
		// A SOCKET NOBODY DECIDES AGAIN IS CLOSED, so its tab's handshake decides.
		defer containPanic(ctx, "socket_decide", func() {
			_ = conn.Close(CloseUndecided, "Sign-in could not be decided again. Reconnecting.")
		})
		keepDecided(ctx, conn, l, cred, svc.seatOf, svc.now,
			func(ctx context.Context, p iam.Principal) bool {
				return !svc.AudienceFor(ctx, p).Equal(client.Audience())
			}, func(ctx context.Context) {
				// THE WATCH AND EVERY ROOM, decided again together as
				// whoever this decision resolved.
				seats.recheck(ctx)
				svc.recheckRooms(ctx, client)
			})
	})
	defer deciding.Wait()

	code, reason := readFrames(ctx, conn, seats, client, query, who, svc.interval, rooms)

	// THE ROOM FRAMES FIRST, so no join lands after the leave below.
	close(rooms.ops)
	roomWorker.Wait()
	// Leaving closes the client's queue, which is what ends the writer.
	svc.leave(client)
	writer.Wait()
	_ = conn.Close(code, reason)
}

// roomOpsDepth is how many room frames one socket may have waiting for its room
// goroutine before its read loop waits for room.
//
// SIXTEEN: presence frames are held to [presence.ClientRate] a second
// before they are queued and joins are rare, so this is more than a second of
// an honest tab's frames, and the read loop waits only behind a join that is
// slow to decide, which is backpressure on that one socket.
const roomOpsDepth = 16

// socketRooms is one socket's room frames on their way to its room goroutine,
// and its presence allowance.
type socketRooms struct {
	ops      chan request
	presence presenceFrames
}

// queue hands one room frame to the socket's room goroutine, waiting for room
// in its queue no longer than the socket lives.
func (r *socketRooms) queue(ctx context.Context, req request) {
	select {
	case r.ops <- req:
	case <-ctx.Done():
	}
}

// roomOp carries out one room frame, as whoever the latest decision of the
// socket's credential resolved: an artifact's room on this service, a chat
// room's on the chat arm, which decides who may be in a chat room as it
// decides who is shown what is said there.
func (s *Service) roomOp(ctx context.Context, client *Client, chat *ChatHub, req request) {
	defer containPanic(ctx, req.Kind, func() {
		if req.Kind == "artifact_join" {
			client.Reply(Envelope{Kind: KindError, ID: req.ID, What: artifactJoinWhat, Error: CodeQueryFailed,
				Artifact: req.Artifact})
		}
	})
	switch req.Kind {
	case "artifact_join":
		s.joinArtifact(ctx, client, req)
	case "artifact_leave":
		s.leaveArtifact(client, req)
	case "presence":
		switch req.Room {
		case presence.RoomArtifact:
			s.presenceFrame(ctx, client, req)
		case presence.RoomChat:
			if client.Posture().ServesQueries() {
				chat.presenceFrame(ctx, client, req.ChannelID, req.Activity, req.Focus)
			}
		}
	}
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
			// A DEADLINE PER WRITE. A peer that vanished without a FIN (lid closed, NAT
			// entry dropped, network handoff) leaves Write blocked for minutes, holding
			// the goroutine, the queue and the hub slot on every broadcast.
			//
			// THIRTY SECONDS, not a few: QueueDepth above decides a slow tab must not be
			// disconnected, so this tells GONE from SLOW and nothing else; a tighter one
			// would sever a mobile tab mid-snapshot and contradict that.
			writeCtx, cancelWrite := context.WithTimeout(ctx, writeTimeout)
			err := conn.Write(writeCtx, websocket.MessageText, raw)
			cancelWrite()
			if err != nil {
				return
			}
		}
	}
}

// readFrames is [readLoop], a panic in it contained ([containPanic]) and ending
// the socket as an internal error, so the socket's own teardown runs: escaped,
// it would wait on the decider that only that teardown ends.
func readFrames(ctx context.Context, conn *websocket.Conn,
	seats *watching, client *Client, query Query, who *asking,
	healthEvery time.Duration, rooms *socketRooms,
) (code websocket.StatusCode, reason string) {
	defer containPanic(ctx, "socket_read", func() {
		code, reason = websocket.StatusInternalError, "The node failed reading a frame. Reconnecting."
	})
	return readLoop(ctx, conn, seats, client, query, who, healthEvery, rooms)
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
func readLoop(ctx context.Context, conn *websocket.Conn,
	seats *watching, client *Client, query Query, who *asking,
	healthEvery time.Duration, rooms *socketRooms,
) (websocket.StatusCode, string) {
	// THIS SOCKET'S CONCURRENCY BOUND — see [MaxInFlightQueries] for why
	// four, and why per socket. Queries run on their own goroutines so a
	// store scan cannot stall the live feed, and a burst past the bound
	// queues here rather than piling into the engine's connection pool.
	slots := make(chan struct{}, MaxInFlightQueries)
	var running sync.WaitGroup
	defer running.Wait()
	questions := askedQuestions{byID: map[int64]*askedQuestion{}}

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
		case "artifact_join", "artifact_leave":
			rooms.queue(ctx, req)
		case "presence":
			// HELD TO ITS ALLOWANCE HERE, on the read loop, so a socket
			// that does not throttle itself fills no queue — a chat
			// room's typing frames under the same allowance as an
			// artifact's cursor, since both are this one frame.
			if rooms.presence.take(seats.now()) {
				rooms.queue(ctx, req)
			}
		case "query_cancel":
			// A QUESTION ITS CLIENT GAVE UP ON STOPS NOW, not once its wait has
			// passed: a screen left behind a transcript page's read held this
			// socket's slot and the node's for minutes, for nobody. Nothing is
			// answered for it, as for one whose wait passed.
			questions.cancel(req.ID)
		case "query":
			// A DEGRADED NODE REFUSES rather than answers: its copy of the company is
			// wrong rather than behind, and "there is no such work item" out of it is
			// something a person acts on. The refusal is a DIRECT frame, so it reaches
			// the client whatever the posture.
			//
			// `retry_after` is never omitted, as on every `unavailable` frame (a frame
			// without it is what a node too old to say sends). The posture is re-derived
			// on the shared health tick, so one tick is the soonest this node could
			// answer differently; the health frame says why. No state-log refusal is
			// behind it, so none is named.
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
			// rather than a paused reader. The client's wait starts as
			// the frame is read, before the slot, as its own clock does.
			asked, stop := req.waited(ctx)
			mine := questions.add(req.ID, stop)
			running.Add(1)
			slots <- struct{}{}
			go func() {
				defer running.Done()
				defer func() { <-slots }()
				defer questions.done(req.ID, mine)
				defer stop()
				defer containPanic(asked, req.What, func() {
					if asked.Err() == nil {
						client.Reply(queryError(req, CodeQueryFailed))
					}
				})
				// AS WHOEVER THE LAST DECISION SAID, not whoever opened
				// the socket: a permission taken away an hour ago must
				// not still answer here. See lifetime.go.
				runQuery(who.context(asked), client, query, req)
			}()
		default:
			// Unknown kinds are ignored, which is what makes new ones
			// additive on both ends.
		}
	}
}

// askedQuestions is one socket's questions in flight, by the id each `query`
// frame named, so a `query_cancel` frame stops the one its client gave up on.
type askedQuestions struct {
	mu   sync.Mutex
	byID map[int64]*askedQuestion
}

// askedQuestion is one question in flight: what stops it.
type askedQuestion struct{ stop context.CancelFunc }

// add holds id's question, stopped by stop, until [askedQuestions.done]. An id
// a client names twice cancels the latest question it named.
func (q *askedQuestions) add(id int64, stop context.CancelFunc) *askedQuestion {
	one := &askedQuestion{stop: stop}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.byID[id] = one
	return one
}

// done lets go of id's question, one, unless a later one took its id.
func (q *askedQuestions) done(id int64, one *askedQuestion) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.byID[id] == one {
		delete(q.byID, id)
	}
}

// cancel stops id's question, where one is in flight.
func (q *askedQuestions) cancel(id int64) {
	q.mu.Lock()
	one := q.byID[id]
	q.mu.Unlock()
	if one != nil {
		one.stop()
	}
}

// watching is one socket's subscription to a seat's frames, and the authority
// that decides it. A watch installs a routing row in the hub and is a READ of
// somebody's inbox, so it is decided as the `inbox` question is,
// [authz.ActionInbox], WITH NO APP: a frame announces Gitlet's notices too.
//
// Three answers, none closing the socket: ALLOWED installs it; REFUSED
// installs nothing and sends `unauthorized` (not a 4403, which the dashboard
// reads as "no live channel at all"); UNDECIDABLE (chart unread) installs
// nothing and sends `unavailable` ([watchAnswer.frame]): installing would
// hand frames to somebody not shown to be allowed. Either drops the previous
// watch ([Hub.Watch]). The name is resolved by [iam.OwnerOf], as `work_inbox`
// is, after [watching.mayLook], so no directory read for a caller who leads nobody.
type watching struct {
	hub     *Hub
	client  *Client
	chart   authz.Chart
	holders iam.Holders

	// now is the service's clock ([Options.Now]), which every decision
	// here is taken at: the guard that resolved this socket's caller and
	// the timer that ends its credential read the same one (lifetime.go),
	// and a watch decided on a private wall-clock reading would judge a
	// recency window by a different clock from the guard that proved it.
	now func() time.Time
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
			"Sign in to watch a seat."
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
// name as typed, which nobody leads, so only an admin learns it names
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
		d := authz.Decide(ctx, principal, authz.ActionInbox, authz.Object{
			Kind: authz.KindPerson, Owner: login, Unresolved: true,
		}, w.chart, w.now())
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
		return refusedWatch(ctx, d)
	}
}

// refusedWatch is a watch the authority table refused, answered through
// [RefusedFor] — the frame a query refused on the same decision carries — or,
// for a refusal this socket cannot say, as the fault [RefusedFor] makes it.
func refusedWatch(ctx context.Context, d authz.Decision) *watchAnswer {
	err := RefusedFor(watchWhat, d)
	var refused *RefusedError
	switch {
	case errors.As(err, &refused):
		return &watchAnswer{code: refused.Code, refused: &refused.Refused}
	case errors.Is(err, ErrNotFound):
		return &watchAnswer{code: CodeNotFound}
	}
	log.ErrorContext(ctx, "stream_watch_refusal_unspoken", "error", err)
	return &watchAnswer{code: CodeQueryFailed}
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
// An `unavailable` one says when to ask again, like every other: a
// `retry_after` ([HealthInterval] where nothing better is known), ZERO where
// waiting will not help (a log full or a record this node cannot decode). Sent
// bare it made the dashboard re-ask at its own interval a watch refused until
// an operator acted.
//
// The hint is [UnavailableOf]'s, taken whole: it is the one reading every
// query's `unavailable` takes, and a copy of its arithmetic would drift. Only
// the hint, never the code or words: they stay in the log ([watching.resolve])
// because what the directory says about a login names the seat bound to it.
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

	d := authz.Decide(ctx, principal, authz.ActionInbox,
		authz.Object{Kind: authz.KindPerson, Owner: seat}, w.chart, w.now())
	switch {
	case d.Unknown():
		log.InfoContext(ctx, "stream_watch_undecidable", "login", principal.Login,
			"seat", seat, "error", d.Err)
		return &watchAnswer{code: CodeUnavailable, cause: d.Err}
	case !d.Allowed:
		log.InfoContext(ctx, "stream_watch_refused", "login", principal.Login,
			"seat", seat, "reason", string(d.Reason))
		return refusedWatch(ctx, d)
	}
	return nil
}

// recheck re-decides the seat this socket watches, as whoever the latest
// decision of its credential resolved: lifetime.go's argument, as a decision
// kept for the life of the socket would let a lead moved off a team keep
// following a former report's inbox. It runs when the guard decides the
// credential again and when a company is published. A REFUSAL withdraws the
// watch. AN UNDECIDABLE ANSWER KEEPS IT, unlike a new
// watch: nothing was learned against the earlier decision, and a chart blip
// that unsubscribed every lead would be an outage.
//
// Withdrawn only if still the watch decided: this runs on the decision's
// goroutine while the read loop installs its own, so see [Hub.UnwatchIf]; a
// watch that moved is the read loop's to decide.
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
//
// A QUESTION NOBODY WAITS FOR IS DROPPED, its socket closed or its client's
// wait passed ([request.waited]): nobody is left to answer, and the read
// failing on that is no fault of the node's. Read off ctx rather than err, so
// a deadline of the question's own still logs.
func runQuery(ctx context.Context, client *Client, query Query, req request) {
	data, err := query(ctx, req.What, req.Params)
	if ctx.Err() != nil {
		log.DebugContext(ctx, "stream_query_dropped", "what", req.What, "error", err)
		return
	}
	var refused *RefusedError
	var revision *RevisionError
	switch {
	case err == nil:
		client.Reply(Envelope{Kind: KindResult, ID: req.ID, What: req.What, Data: data})
	case errors.Is(err, ErrUnknownQuery):
		client.Reply(queryError(req, CodeUnknownQuery))
	case errors.As(err, &refused):
		// THE MAPPING'S CODE AND FIELDS, carried whole from [RefusedFor]. A
		// bare [ErrUnauthorized] names no decision, so it is answered by the
		// default arm as the fault it is rather than as a refusal nobody
		// could say the reason for.
		env := queryError(req, refused.Code)
		env.Refused = &refused.Refused
		client.Reply(env)
	case errors.Is(err, ErrNotFound):
		client.Reply(queryError(req, CodeNotFound))
	case errors.Is(err, ErrArtifactTrashed):
		client.Reply(queryError(req, CodeArtifactTrashed))
	case errors.As(err, &revision):
		// THE REST ROUTE'S CODE AND WORDS, so a revision this node cannot
		// open is told the same thing on both transports. The keyring's own
		// error went to the log where the question read it.
		refusal := queryError(req, revision.Code)
		refusal.Detail = httpjson.Sentence(revision.Detail)
		client.Reply(refusal)
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
			refusal.Detail = httpjson.Sentence(bad.Detail)
		}
		client.Reply(refusal)
	case errors.Is(err, ErrUnavailable):
		// THE REFUSAL, A PERSON'S SENTENCE FOR IT AND ITS HINT RIDE THE
		// FRAME, because a node catching up and a node that will refuse
		// this read until an operator acts are both `unavailable` — see
		// [Unavailable]. Its own detail goes to the log.
		LogUnavailable(ctx, log, "stream_query_unavailable", err, "what", req.What)
		env := queryError(req, CodeUnavailable)
		env.unavailable(UnavailableOf(err))
		client.Reply(env)
	default:
		// The reason reaches the LOG, not the client. A query failure can
		// carry a database path or a driver's own message, and being
		// admitted to a question does not make a reader somebody that path
		// is meant for.
		LogFailed(ctx, log, "stream_query_failed", err, "what", req.What)
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

// containPanic, deferred on a goroutine a socket's client is served on or a
// loop serving every socket runs on, contains a panic there as net/http
// contains a request handler's: [panics] hears it in [panics.ScopeAPI], the log
// has its stack, and failed answers whoever waited (a query as failed, the
// socket closed where nothing else can). Uncontained, one question's panic
// ended the engine's process, every socket and seat on it. Deferred itself,
// since recover answers only the function a defer calls.
func containPanic(ctx context.Context, what string, failed func()) {
	v := recover()
	if v == nil {
		return
	}
	p := panics.Recovered(panics.ScopeAPI, v)
	log.ErrorContext(ctx, "stream_panicked", "what", what, "panic", fmt.Sprint(v), "stack", p.Stack)
	failed()
}

// contained runs one pass of a loop that serves every socket on this node (a
// health tick, a chat batch, a read-state watch, a room's decision), its panic
// contained ([containPanic]), and reports whether the pass ran to its end. A
// PASS, NOT THE LOOP: a loop ended by one pass's panic would serve no later
// one, every socket on the node going without its pushes until a restart.
func contained(ctx context.Context, what string, pass func()) (ended bool) {
	defer containPanic(ctx, what, func() {})
	pass()
	return true
}

// maxWaitMS is the longest wait a frame names that a [time.Duration] holds;
// a longer one is no deadline a socket outlives.
const maxWaitMS = math.MaxInt64 / int64(time.Millisecond)

// waited is ctx held to the wait r's client gives its answer ([request.WaitMS]),
// and what lets it go. A QUESTION ITS CLIENT GAVE UP ON STOPS: unbounded, a read
// the dashboard stopped waiting for ran on in the socket's slot and the
// engine's (a transcript's, for one), holding both for nobody until it ended.
func (r request) waited(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.WaitMS <= 0 || r.WaitMS > maxWaitMS {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(r.WaitMS)*time.Millisecond)
}
