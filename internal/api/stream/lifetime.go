package stream

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
)

// A SOCKET IS AUTHENTICATED AT ITS HANDSHAKE AND ENDED BY WHAT ENDS ITS
// CREDENTIAL.
//
// A REST request is decided once and is over. A socket is decided at its
// handshake and then stays open for as long as the tab does — hours, often a
// day. Decided only at the handshake, a session revoked, a person suspended, a
// seat taken away or a grant narrowed would change nothing for a socket
// already open. So an open socket is ended, or decided again, by the event
// that could have changed its answer, and at no other time — there is no
// periodic re-check:
//
//   - A MOVE THAT NAMES ITS CREDENTIAL ([Service.CredentialsMoved]): a
//     committed identity batch ended its session (by lineage), moved the row
//     of the person it acts for (by id: their grants, stage, revocation
//     epoch, seat binding, removal, or a machine token of theirs revoked), or
//     moved the directory row a Tier A token is bound through (by that row's
//     login, `token:<id>`). Only the sockets a move names are decided again,
//     each on its own goroutine, by the guard over the request it was opened
//     with ([auth.Guard.ResolveOpen]) — a direct read, and there are few.
//     Every node applies the identity log, so every node hears this from its
//     own applier and decides its own sockets; nothing is sent between nodes.
//   - A MOVE THAT NAMES NOBODY: the fleet-wide session generation moved, this
//     node retained an identity record it cannot read (whose person is inside
//     the payload), an adoption replaced the estate, or this node stopped
//     vouching for its identity rows (internal/engine's identityvouch.go).
//     EVERY socket closes [CloseUndecided] at once, WITHOUT reading anything:
//     the client reconnects on its backoff and the handshake decides — 401
//     sends it to sign in, 503 says this node cannot tell yet, and anybody
//     the move did not touch is back on a fresh snapshot. Deciding every
//     socket by a read instead would put one identity read per open tab on
//     the store's one reserved identity connection at once, ahead of every
//     request's own authentication.
//   - A PUBLISHED COMPANY ([Service.CompanyPublished]): the org chart a seat
//     binding resolves through and a watch is decided by may have moved. It
//     is decided IN MEMORY against the company just published, with no
//     identity read: a socket whose principal acts as a seat that company no
//     longer holds as a human seat closes [CloseUnauthorized]; one whose seat
//     now answers to another handle closes [CloseUndecided], so its handshake
//     resolves the binding afresh; and every watch is decided again
//     ([watching.recheck]).
//   - ITS CREDENTIAL ENDS ON ITS OWN ([auth.Lifetime]): a session's absolute
//     deadline, a machine token's expiry. No record is written at that
//     instant, so a timer per socket notices it, and decides the credential
//     by the guard.
//   - AND ONCE, AS SOON AS IT IS REGISTERED. The handshake was decided before
//     the socket was listening, so a record committed in between — a
//     sign-out a second after the page loaded — would otherwise be heard by
//     nobody; read after registering, the rows already hold it.
//
// # What a decision by the guard does
//
//   - RESOLVED: the socket carries on, later questions are asked as the
//     principal just resolved, and the pushes follow its grants: a changed
//     [Audience] is sent a fresh snapshot built for it. The watch is decided
//     again too.
//   - A SEAT REFUSAL (the seat is gone), or a principal that no longer holds
//     `state:read`: closed [CloseUnauthorized], because the credential is fine
//     and what it may do is not.
//   - ANONYMOUS (the session ended, expired or was revoked; the token is no
//     longer accepted): closed [CloseUnauthenticated].
//   - UNKNOWN (this node could not read what decides it): closed
//     [CloseUndecided], and the reconnect's handshake answers
//     `503 identity_unavailable` before any snapshot is built.
//
// # What an open socket does not end on
//
// A session's IDLE deadline is moved by a re-issue on a REST response, which a
// socket can never receive. An open live view is activity, so a decision by
// the guard sets that deadline aside ([auth.Guard.ResolveOpen]) and the
// absolute deadline still ends it.
//
// A node BEHIND its identity log hears no record it has not applied, so inside
// [statelog.StallGrace] it serves an open socket on its last decision, as its
// REST routes serve the rows they have. Past the grace it stops vouching, and
// that crossing is a move naming nobody (above).
//
// # Residual
//
// A move naming nobody closes every socket on the node, and one of them is a
// batch that RETAINED a record — which, during a rolling upgrade, is every
// batch an older node applies in a bucket a newer peer's record already holds
// there. Its open tabs reconnect once per such batch until it is upgraded;
// each reconnect is one handshake, which is the price of never reading on a
// move that names nobody.
//
// The published-company decision finds the seat by the identity the company
// gave it when this socket first saw it there. A socket opened on a seat the
// published company did not yet hold (a hire inside the publish's coalescing
// window) learns that identity at the next publish, by handle — so a seat
// renamed and its old handle given to a new seat inside that one window would
// be taken for the new seat. Every other path re-decides by the guard.

// CloseUndecided ends a socket whose credential this node will not vouch for
// now: a move that names nobody, a credential the guard could not decide, or a
// seat that answers to another handle. It is the standard's 1013, "try again
// later", rather than an application code, because what it asks of a client is
// what any close outside the 4000 range asks: reconnect on a backoff. The
// handshake that follows is what says why, over plain HTTP where the status is
// visible.
const CloseUndecided = websocket.StatusTryAgainLater

// Moved is what the identity estate moved that an open socket's credential may
// depend on — the stream's own reading of the identity applier's post-commit
// value, which internal/api translates.
//
// IT NAMES WHOSE, NEVER WHAT: a socket it names is decided again from the
// rows, which the batch has already committed.
type Moved struct {
	// People are the persons whose row moved, by id.
	People []string

	// Logins are the logins those people held, and every login a claim
	// gave or took: what a Tier A token's binding is named by.
	Logins []string

	// Sessions are the session lineages a record ended.
	Sessions []string

	// Everyone is a move no list can name, which closes every socket
	// [CloseUndecided] without reading anything.
	Everyone bool
}

// opened is what a socket's credential is, as the identity estate names it —
// what [Moved] is matched against.
type opened struct {
	// lineage is the session a cookie was issued for, empty for a bearer.
	lineage string

	// person is the principal's id: a session's person, a machine token's
	// owner. A Tier A token's derived id, which no move names.
	person string

	// login is the principal's login. For a Tier A token it is
	// `token:<id>`, the login of the directory row its seat binding is
	// read from — whose id the socket never learns, so this is the one
	// name a move can reach it by.
	login string
}

// openedWith is what the handshake's resolution says the socket was opened
// with.
func openedWith(p iam.Principal) opened {
	o := opened{login: p.Login}
	if lineage, ok := strings.CutPrefix(p.Via, iam.SessionPrefix); ok {
		o.lineage = lineage
	}
	if p.ID != uuid.Nil {
		o.person = p.ID.String()
	}
	return o
}

// namedBy reports whether m names this credential.
func (o opened) namedBy(m Moved) bool {
	return o.lineage != "" && slices.Contains(m.Sessions, o.lineage) ||
		o.person != "" && slices.Contains(m.People, o.person) ||
		o.login != "" && slices.Contains(m.Logins, o.login)
}

// decideFunc decides a socket's credential again: the guard's resolution of
// the handshake request, on ctx.
type decideFunc func(ctx context.Context) (*http.Request, *auth.Refusal)

// credential is a socket's credential as its handshake decided it, and how to
// decide it again.
type credential struct {
	// who is the principal this socket's questions are asked as.
	who *asking

	// opened is what the identity estate names the credential by.
	opened opened

	// ends is when the credential ends on its own, zero for never.
	ends time.Time

	// decide is the guard, over the handshake request.
	decide decideFunc
}

// deciderFor is the decision an open socket repeats: the guard's
// [auth.Guard.ResolveOpen] over the ORIGINAL handshake request.
func deciderFor(guard *auth.Guard, handshake *http.Request) decideFunc {
	return func(ctx context.Context) (*http.Request, *auth.Refusal) {
		return guard.ResolveOpen(handshake.Clone(ctx))
	}
}

// asking is the principal this socket's questions are asked as, moved by each
// decision.
//
// A LOCK RATHER THAN AN ATOMIC because it carries a whole principal and every
// query reads it while a decision writes it; a torn read would ask one
// question as half of two people.
type asking struct {
	mu        sync.Mutex
	principal iam.Principal
}

// set replaces the principal later questions are asked as.
func (a *asking) set(p iam.Principal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.principal = p
}

// current is the principal later questions are asked as.
func (a *asking) current() iam.Principal {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.principal
}

// context is ctx carrying the current principal.
func (a *asking) context(ctx context.Context) context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return iam.WithPrincipal(ctx, a.principal)
}

// SeatState is what a published company says about a seat.
type SeatState struct {
	// Origin is the handle the seat was created under — its identity,
	// which the chart never issues to another seat.
	Origin string

	// Handle is the handle it answers to now.
	Handle string

	// Human is whether it is a human seat, the only kind a person or a
	// credential bound to one may act as.
	Human bool
}

// SeatOfFunc answers what the company this node has published says about the
// seat a name addresses — its live handle, the handle it was created under, or
// a handle it gave up — and false for a seat it does not hold. In memory: it
// reads no identity row.
type SeatOfFunc func(name string) (SeatState, bool)

// listener is one open socket as the service's registry holds it: what it was
// opened with, and the signals that reach it. Each holds at most one pending
// signal, so a burst collapses into the one decision that follows it.
type listener struct {
	opened opened

	// wake asks for a decision by the guard: a move named this credential.
	wake chan struct{}

	// drop closes the socket [CloseUndecided]: a move named nobody.
	drop chan struct{}

	// published asks for the in-memory decision against a company just
	// published.
	published chan struct{}
}

// newListener builds a listener with one decision by the guard already
// pending — the one that covers the window between its handshake and its
// registration (see the file head).
func newListener(o opened) *listener {
	l := &listener{opened: o, wake: make(chan struct{}, 1),
		drop: make(chan struct{}, 1), published: make(chan struct{}, 1)}
	signal(l.wake)
	return l
}

// signal leaves one pending signal on c, never blocking: its callers are the
// identity apply loop and the publish path.
func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// listeners is the service's registry of open sockets.
type listeners struct {
	mu   sync.Mutex
	open map[*listener]struct{}
}

// add registers l and answers its removal.
func (ls *listeners) add(l *listener) func() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.open == nil {
		ls.open = map[*listener]struct{}{}
	}
	ls.open[l] = struct{}{}
	return func() {
		ls.mu.Lock()
		defer ls.mu.Unlock()
		delete(ls.open, l)
	}
}

// each calls fn for every open socket, under the registry's lock: fn only
// signals.
func (ls *listeners) each(fn func(*listener)) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for l := range ls.open {
		fn(l)
	}
}

// CredentialsMoved acts on every open socket an identity move reaches: a move
// naming nobody closes them all [CloseUndecided], and any other decides again
// exactly the sockets it names.
//
// NEVER BLOCKS: its caller is the identity applier's post-commit hook, on the
// apply loop's own goroutine with the next batch waiting behind it. Each
// socket acts on its own goroutine.
func (s *Service) CredentialsMoved(m Moved) {
	s.listeners.each(func(l *listener) {
		switch {
		case m.Everyone:
			signal(l.drop)
		case l.opened.namedBy(m):
			signal(l.wake)
		}
	})
}

// keepDecided acts on this socket's signals and its credential's own end,
// until ctx ends or the socket is closed. See the file head.
//
// resync is what a changed audience is sent: the snapshot, built for the
// audience the decision resolved, because the screen has to lose what it was
// showing under a narrowed grant rather than keep it until a reload. rewatch
// decides the watched seat again, as whoever is asking now. seatOf is the
// published company's word on a seat.
func keepDecided(ctx context.Context, conn *websocket.Conn, client *Client,
	l *listener, cred credential, seatOf SeatOfFunc, now func() time.Time,
	resync func(Audience) map[string]any, rewatch func(context.Context)) {

	// ONE TIMER, ARMED AT THE HANDSHAKE'S READING: a credential's own end is
	// fixed when it is issued, so the instant to wake at is known from the
	// start.
	//
	// AND ARMED AGAIN WHEN THE GUARD STILL SERVES IT, because the timer runs
	// on the monotonic clock and the guard compares the WALL clock: a wall
	// clock stepped back after the arming (an NTP correction, a VM resumed)
	// fires the timer while the guard still sees the credential live, and a
	// timer dropped there left the socket with no deadline at all. Re-armed
	// for the wall clock's own distance, floored at [expiryRetry].
	var timer *time.Timer
	var expiry <-chan time.Time
	if !cred.ends.IsZero() {
		timer = time.NewTimer(time.Until(cred.ends))
		defer timer.Stop()
		expiry = timer.C
	}
	// THE IDENTITY OF THE SEAT the principal acts as, as the published
	// company names it — what a published company is decided by.
	origin := identify(seatOf, cred.who.current().Seat)
	decide := func() bool {
		r, refusal := cred.decide(ctx)
		if !decideOnce(ctx, conn, client, cred.who, r, refusal, now, resync,
			rewatch) {
			return false
		}
		origin = identify(seatOf, cred.who.current().Seat)
		return true
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.drop:
			log.InfoContext(ctx, "stream_closed_undecided",
				"hint", "an identity move this node cannot name anybody by; "+
					"the tab reconnects and its handshake decides")
			_ = conn.Close(CloseUndecided, "credential to be decided again")
			return
		case <-l.published:
			if !keepSeated(ctx, conn, cred.who, seatOf, &origin) {
				return
			}
			if rewatch != nil {
				rewatch(cred.who.context(ctx))
			}
		case <-l.wake:
			if !decide() {
				return
			}
		case <-expiry:
			if !decide() {
				return
			}
			timer.Reset(max(time.Until(cred.ends), expiryRetry))
		}
	}
}

// expiryRetry is the shortest interval at which a socket whose credential's
// own end has passed is decided again, while the guard still serves it.
//
// ONE SECOND, because it is what a disagreement between this node's clocks
// costs and nothing else: the decision is a keyed read of one person's rows,
// and the disagreement lasts as long as the wall clock's step — seconds after
// an NTP correction — so a second bounds both the read rate and how long past
// its deadline a socket can be served to the same small figure.
const expiryRetry = time.Second

// identify is the identity the published company gives the seat handle answers
// to now, or "" when it holds no seat under that live handle.
func identify(seatOf SeatOfFunc, handle string) string {
	if handle == "" {
		return ""
	}
	if s, ok := seatOf(handle); ok && s.Handle == handle {
		return s.Origin
	}
	return ""
}

// keepSeated decides an open socket against a company just published, in
// memory, and reports whether it is still open — see the file head. origin is
// the identity of the seat the principal acts as, learned here when it was
// not known.
func keepSeated(ctx context.Context, conn *websocket.Conn, who *asking,
	seatOf SeatOfFunc, origin *string) bool {

	p := who.current()
	if p.Seat == "" {
		return true
	}
	ref := *origin
	if ref == "" {
		ref = p.Seat
	}
	s, found := seatOf(ref)
	switch {
	case found && s.Human && s.Handle == p.Seat:
		*origin = s.Origin
		return true
	case *origin != "" && (!found || !s.Human):
		// A SEAT THIS SOCKET SAW THE COMPANY HOLD, removed or no longer a
		// human seat: what the guard refuses `seat_unavailable`.
		log.InfoContext(ctx, "stream_closed_seat_gone", "seat", p.Seat)
		_ = conn.Close(CloseUnauthorized, string(httpjson.CodeSeatUnavailable))
		return false
	}
	// RENAMED, or a seat this socket has not yet seen the published company
	// hold: the handshake resolves the binding by its identity.
	log.InfoContext(ctx, "stream_closed_seat_moved", "seat", p.Seat)
	_ = conn.Close(CloseUndecided, "seat to be resolved again")
	return false
}

// decideOnce acts on one decision of the socket's credential by the guard —
// its resolution r and its refusal — and reports whether the socket is still
// open.
func decideOnce(ctx context.Context, conn *websocket.Conn, client *Client,
	who *asking, r *http.Request, refusal *auth.Refusal, now func() time.Time,
	resync func(Audience) map[string]any, rewatch func(context.Context)) bool {

	if ctx.Err() != nil {
		return false
	}
	// THE DECISION'S REQUEST CARRIES ITS RESOLUTION: [deciderFor] clones
	// the handshake onto a context and the guard resolves into it.
	// contextcheck follows a context through a returned request no further
	// than the call.
	//nolint:contextcheck // the resolution's own; see the paragraph above
	principal, how := iam.From(r.Context())
	switch {
	case refusal != nil && refusal.Applies(r):
		// RESOLVED AND STILL REFUSED — the person's seat is gone, or
		// their session may only enrol a second factor.
		log.InfoContext(ctx, "stream_closed_refused",
			"code", string(refusal.Code), "detail", refusal.Detail)
		_ = conn.Close(CloseUnauthorized, string(refusal.Code))
		return false
	case how == iam.Unknown:
		log.WarnContext(ctx, "stream_closed_undecided",
			"hint", "this node could not read what decides this socket's "+
				"credential; the tab reconnects, and its handshake answers "+
				"503 until the node can say")
		_ = conn.Close(CloseUndecided, "credential could not be decided")
		return false
	case how != iam.Resolved:
		log.InfoContext(ctx, "stream_closed_credential_ended")
		_ = conn.Close(CloseUnauthenticated, "credential no longer accepted")
		return false
	case !principal.Can(iam.GrantStateRead):
		// RESOLVED, AND MAY NO LONGER READ what this socket carries. 4403
		// rather than 4401, because signing in again reaches the same
		// person with the same grants.
		log.InfoContext(ctx, "stream_closed_grant_withdrawn",
			"login", principal.Login, "grant", string(iam.GrantStateRead))
		_ = conn.Close(CloseUnauthorized, "grant withdrawn: "+
			string(iam.GrantStateRead))
		return false
	}
	who.set(principal)
	// AND THE SEAT IT WATCHES, as whoever this decision resolved.
	if rewatch != nil {
		rewatch(who.context(ctx))
	}
	audience := AudienceOf(principal.Grants)
	if client.SetAudience(audience) {
		client.Reply(Push(KindSnapshot, resync(audience), now()))
	}
	return true
}
