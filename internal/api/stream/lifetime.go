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
// CREDENTIAL, at no other time: it stays open for hours, so only the events
// below decide it again, never a periodic re-check.
//
// A MOVE THAT NAMES ITS CREDENTIAL ([Service.CredentialsMoved]) is a committed
// identity batch that ended its session (by lineage), moved its person's row
// (by id: grants, stage, revocation epoch, seat binding, removal, a machine
// token revoked) or the directory row a Tier A token is bound through (by
// login, `token:<id>`). Only the named sockets are decided again, each on its
// own goroutine, by [auth.Guard.ResolveOpen]. Every node serving the API
// applies the identity log, so each decides its own sockets; nothing crosses
// nodes.

// A MOVE THAT NAMES NOBODY (the fleet session generation, an unreadable
// retained identity record, an adopted estate, or this node ceasing to vouch
// for its identity rows: internal/engine's identityvouch.go) closes EVERY
// socket [CloseUndecided] WITHOUT READING: the reconnect's handshake decides
// (401 signs in, 503 says not yet). A read per socket would queue one identity
// read per open tab on the store's one reserved identity connection, ahead of
// every request's authentication.
//
// A PUBLISHED COMPANY ([Service.CompanyPublished]) is decided IN MEMORY: a
// seat it no longer holds as a human seat closes [CloseUnauthorized]; an
// unseen seat, or a changed AUDIENCE ([Service.AudienceFor]), [CloseUndecided];
// and every watch is decided again ([watching.recheck]).

// A credential's own end ([auth.Lifetime]: a session's absolute deadline, a
// machine token's expiry) writes no record, so a timer per socket decides it
// by the guard. Each socket is also decided ONCE as it is registered: the
// handshake preceded the listening, so a record committed between would be
// heard by nobody.
//
// A guard decision resolved as the same principal leaves the socket open and
// decides its watch again. As SOMEBODY ELSE, or UNKNOWN, it closes
// [CloseUndecided] and the reconnect builds everything for whoever it is now:
// kept open, the socket answered the new principal while the dashboard named
// the old one. A seat refusal or a second-factor-only session closes
// [CloseUnauthorized]; ANONYMOUS, [CloseUnauthenticated].

// A session's IDLE deadline is moved by a REST re-issue a socket never gets,
// so a guard decision sets it aside (an open live view is activity); the
// absolute deadline still ends it. No permission closes a socket 4403
// (ADR-0033: it carries the overview); a moved principal is [CloseUndecided].
//
// A node BEHIND its identity log serves open sockets on their last decision
// inside [statelog.StallGrace]; crossing it is a move naming nobody. During a
// rolling upgrade such a move fires per batch an older node retains, so tabs
// reconnect once per batch: the price of never reading. The published-company
// decision finds the seat by its immutable handle (ADR-0013); a seat not yet
// held (a hire inside the publish's coalescing window) is kept by the next
// publish that holds it.

// CloseUndecided ends a socket whose credential this node will not vouch for
// now: a move that names nobody, a credential the guard could not decide, or a
// seat the published company has never been seen to hold. It is the
// standard's 1013, "try again later", rather than an application code, because
// what it asks of a client is what any close outside the 4000 range asks:
// reconnect on a backoff. The handshake that follows says why, over plain HTTP
// where the status is visible.
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

	// applied is a TEST OBSERVATION POINT, with no production behaviour:
	// nil wherever the engine serves a socket, and never read for a
	// decision. When a test sets it, the lifetime loop calls it after each
	// decision has been acted on (the principal set, or the socket closed),
	// whether or not the socket stays open. A test waits on it because the
	// guard answering a decision is not the socket applying it, and a
	// question asked between the two is asked as the principal before.
	applied func()
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
	// Human is whether it is a human seat, the only kind a person or a
	// credential bound to one may act as.
	Human bool
}

// SeatOfFunc answers what the company this node has published says about the
// seat a handle names, and false for a seat it does not hold. In memory: it
// reads no identity row.
type SeatOfFunc func(handle string) (SeatState, bool)

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
// rewatch decides the watched seat again, as whoever is asking now. seatOf is
// the published company's word on a seat. audienceMoved reports whether the
// principal's audience on the company now published differs from the one the
// socket was opened with ([Service.AudienceFor]); nil asks nothing. now is the
// service's clock, which the credential's end is measured against — the one
// the guard deciding it reads, rather than a second, private reading of the
// wall clock: a node whose guard was handed a clock this timer did not read
// re-decided every socket once a second, its deadline long past by one clock
// and hours off by the other.
func keepDecided(ctx context.Context, conn *websocket.Conn, l *listener,
	cred credential, seatOf SeatOfFunc, now func() time.Time,
	audienceMoved func(context.Context, iam.Principal) bool,
	rewatch func(context.Context)) {

	// ONE TIMER, ARMED AT THE HANDSHAKE'S READING: a credential's own end is
	// fixed when it is issued, so the instant to wake at is known from the
	// start.
	//
	// AND ARMED AGAIN WHEN THE GUARD STILL SERVES IT, because the timer runs
	// on the monotonic clock and the guard compares the WALL clock: a wall
	// clock stepped back after the arming (an NTP correction, a VM resumed)
	// fires the timer while the guard still sees the credential live, and a
	// timer dropped there left the socket with no deadline at all. Re-armed
	// for that clock's own distance, floored at [expiryRetry].
	var timer *time.Timer
	var expiry <-chan time.Time
	if !cred.ends.IsZero() {
		timer = time.NewTimer(cred.ends.Sub(now()))
		defer timer.Stop()
		expiry = timer.C
	}
	// WHETHER THE PUBLISHED COMPANY HAS BEEN SEEN TO HOLD the seat the
	// principal acts as — what tells a seat removed from one not yet in it.
	//
	// LEARNED ONCE, AT THE HANDSHAKE'S SEAT, and never again from a
	// decision: one that moves the seat closes the socket, and one that
	// keeps it learned whatever company was published as it ended — one
	// that had just dropped the seat left this socket believing it had never
	// been held, and the publish that followed closed it [CloseUndecided]
	// rather than `seat_unavailable`.
	seen := holds(seatOf, cred.who.current().Seat)
	decide := func() bool {
		r, refusal := cred.decide(ctx)
		open := decideOnce(ctx, conn, cred.who, r, refusal, rewatch)
		if cred.applied != nil {
			cred.applied()
		}
		return open
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.drop:
			log.InfoContext(ctx, "stream_closed_undecided",
				"hint", "an identity move this node cannot name anybody by; "+
					"the tab reconnects and its handshake decides")
			_ = conn.Close(CloseUndecided, "Sign-in changed. Reconnecting.")
			return
		case <-l.published:
			if !keepSeated(ctx, conn, cred.who, seatOf, &seen) {
				return
			}
			if audienceMoved != nil && audienceMoved(ctx, cred.who.current()) {
				// WHOSE TRAIL THIS READER READS MOVED with the company:
				// the reconnect builds the audience it has now, its
				// snapshot included, rather than this socket pushing a
				// lead's new team's bodies into a tab built without them
				// — or the old team's into one that may no longer read
				// them.
				log.InfoContext(ctx, "stream_closed_audience_moved",
					"login", cred.who.current().Login)
				_ = conn.Close(CloseUndecided, "Your access changed. "+
					"Reconnecting.")
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
			timer.Reset(max(cred.ends.Sub(now()), expiryRetry))
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

// holds reports whether the published company holds a seat under handle.
func holds(seatOf SeatOfFunc, handle string) bool {
	if handle == "" {
		return false
	}
	_, ok := seatOf(handle)
	return ok
}

// keepSeated decides an open socket against a company just published, in
// memory, and reports whether it is still open — see the file head. seen is
// whether a published company has held the seat the principal acts as,
// learned here when it was not.
func keepSeated(ctx context.Context, conn *websocket.Conn, who *asking,
	seatOf SeatOfFunc, seen *bool) bool {

	p := who.current()
	if p.Seat == "" {
		return true
	}
	s, found := seatOf(p.Seat)
	switch {
	case found && s.Human:
		*seen = true
		return true
	case *seen:
		// A SEAT THIS SOCKET SAW THE COMPANY HOLD, removed or no longer a
		// human seat: what the guard refuses `seat_unavailable`.
		log.InfoContext(ctx, "stream_closed_seat_gone", "seat", p.Seat)
		_ = conn.Close(CloseUnauthorized, string(httpjson.CodeSeatUnavailable))
		return false
	}
	// A SEAT THIS SOCKET HAS NOT YET SEEN THE PUBLISHED COMPANY HOLD: the
	// handshake resolves the binding.
	log.InfoContext(ctx, "stream_closed_seat_moved", "seat", p.Seat)
	_ = conn.Close(CloseUndecided, "Your seat changed. Reconnecting.")
	return false
}

// decideOnce acts on one decision of the socket's credential by the guard —
// its resolution r and its refusal — and reports whether the socket is still
// open.
func decideOnce(ctx context.Context, conn *websocket.Conn, who *asking,
	r *http.Request, refusal *auth.Refusal, rewatch func(context.Context)) bool {

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
			"hint", "Couldn't read sign-in data for this socket. The tab "+
				"reconnects and gets a 503 until it can.")
		_ = conn.Close(CloseUndecided, "Can't check your sign-in right now. "+
			"Reconnecting.")
		return false
	case how != iam.Resolved:
		log.InfoContext(ctx, "stream_closed_credential_ended")
		_ = conn.Close(CloseUnauthenticated, "Your session ended. Sign in "+
			"again.")
		return false
	}
	if before := who.current(); !sameActor(before, principal) {
		// SOMEBODY ELSE NOW — see the file head. The reconnect's handshake
		// decides the new principal, builds the snapshot for its audience
		// and is asked "who am I" again.
		log.InfoContext(ctx, "stream_closed_principal_moved",
			"login", principal.Login, "was", before.Login,
			"seat", principal.Seat, "seat_was", before.Seat)
		_ = conn.Close(CloseUndecided, "Your account changed. Reconnecting.")
		return false
	}
	who.set(principal)
	// AND THE SEAT IT WATCHES, as whoever this decision resolved.
	if rewatch != nil {
		rewatch(who.context(ctx))
	}
	return true
}

// sameActor reports whether two resolutions of one credential act as the same
// principal, as a tab knows it: the same login, seat and permissions. The
// rest — a step-up deadline, the credential's own name — moves under a socket
// without changing who it is.
//
// THE PERMISSIONS AS CARRIED, which the guard composed under this node's admin
// posture ([iam.AdminPosture.Effective]): admin expanded into every app and
// served or clamped, so two resolutions carrying the same effective set are
// the same reader whatever their rows spelled.
func sameActor(a, b iam.Principal) bool {
	ga, gb := slices.Clone(a.Grants), slices.Clone(b.Grants)
	slices.Sort(ga)
	slices.Sort(gb)
	return a.Login == b.Login && a.Seat == b.Seat &&
		slices.Equal(slices.Compact(ga), slices.Compact(gb))
}
