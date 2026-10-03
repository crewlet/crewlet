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
	"github.com/crewlet/crewlet/internal/iam"
)

// A SOCKET IS AUTHENTICATED AT ITS HANDSHAKE AND ENDED BY WHAT ENDS ITS
// CREDENTIAL.
//
// A REST request is decided once and is over. A socket is decided at its
// handshake and then stays open for as long as the tab does — hours, often a
// day — pushing the company's state and answering questions the whole time.
// Decided only at the handshake, a session revoked, a person suspended, a seat
// taken away or a grant narrowed would change nothing for a socket already
// open: the revocation this engine arbitrates across a fleet in under a second
// would stop at the one surface a browser keeps open.
//
// So the socket is decided again — by the guard, over the request it was
// opened with ([auth.Guard.ResolveOpen]) — exactly when something could have
// changed the answer, and at no other time:
//
//   - THE IDENTITY ESTATE MOVED ITS CREDENTIAL ([Service.CredentialsMoved]): a
//     committed identity batch ended its session, moved the row of the person
//     it acts for (their grants, stage, revocation epoch, seat binding,
//     removal, or a machine token of theirs revoked), or moved something no
//     list can name — the fleet-wide session generation, a release, or a
//     record this node RETAINED rather than applied (a newer build's, or one
//     signed under a key it does not hold), whose person is inside a payload
//     it cannot read. Every node applies the identity log, so every node hears
//     this from its own applier and decides its own sockets; nothing is sent
//     between nodes.
//   - THIS NODE STOPPED VOUCHING FOR ITS IDENTITY ROWS, which arrives on the
//     same path as a move naming everyone: see "A node behind its identity
//     log" below.
//   - A COMPANY WAS PUBLISHED ([Service.CompanyPublished]): the org chart a
//     seat binding resolves through and a watch is decided by may have moved —
//     a seat removed, a lead moved off a team.
//   - ITS CREDENTIAL ENDS ON ITS OWN ([auth.Lifetime]): a session's absolute
//     deadline, a machine token's expiry. No record is written at that
//     instant, so a timer per socket is the only thing that can notice it.
//   - AND ONCE, AS SOON AS IT IS REGISTERED to hear the first two. The
//     handshake was decided before the socket was listening, so a record
//     committed in between — a sign-out a second after the page loaded — would
//     otherwise be heard by nobody; read after registering, the rows already
//     hold it.
//
// There is NO PERIODIC RE-CHECK. One cost a directory read per open tab per
// minute and still let a revocation stand for up to that minute; what decides
// a socket now is the record itself, delivered when it applies.
//
// AND A DECISION IS TAKEN IN THE NODE'S TURN, once per credential: several of
// these events decide every socket at once, every decision is a read on the
// one connection the store reserves for identity work — the one every REST
// request authenticates on — and sockets opened with one credential are
// resolved alike. See decisions.go.
//
// # What each answer does
//
//   - RESOLVED: the socket carries on, and the principal every later query is
//     asked as is the one just resolved. The pushes follow too: the client's
//     [Audience] is the new grants, and a changed one is sent a fresh snapshot
//     built for it. The seat the socket watches is decided again as well.
//   - A SEAT REFUSAL (the person's seat is gone from the chart), or a
//     principal that no longer holds `state:read`: closed [CloseUnauthorized],
//     because the credential is fine and what it may do is not.
//   - ANONYMOUS (the session ended, expired or was revoked; the token is no
//     longer one this node accepts): closed [CloseUnauthenticated], because the
//     remedy is to become somebody again.
//   - UNKNOWN (this node could not read what decides it): closed
//     [CloseUndecided], so the tab reconnects and its handshake decides — which
//     answers `503 identity_unavailable` for as long as this node cannot read
//     its identity estate, before any snapshot is built. Kept open instead, a
//     socket whose credential moved in a way this node cannot read would go on
//     receiving the company's state on a decision the record may have
//     reversed.
//
// # The idle deadline does not end an open socket
//
// A session's idle deadline is moved by a re-issue on a REST response, and a
// socket can never receive one: the bearer it holds is the one its handshake
// presented. An open live view is activity, so a re-decision sets that
// deadline aside and decides the session on its rows ([auth.Guard.ResolveOpen])
// — while its ABSOLUTE deadline, which no re-issue moves, still ends it.
//
// # A record this node retained
//
// A record this node cannot read is retained, not applied, and its applier
// moves past it — so the node is NOT behind, and nothing about it is a stall.
// What changed is what the node can vouch for: every person in the bucket the
// record's scope covers reads as unknown, and the REST guard answers them 503.
// The identity applier says so as a move naming everyone, because no list can
// name whose record it is; decided again, a socket in that bucket answers
// UNKNOWN and closes [CloseUndecided] — its reconnect's handshake answering 503
// as REST does — and every other socket is decided exactly as before. When the
// node can read the record (an upgrade, a key added and a restart), it applies
// it and the ordinary move follows.
//
// # A node behind its identity log
//
// A node hears no event for a record it has not applied, and an applier that
// HALTED on one — a removal or a company-wide invalidation signed under a key
// the node was not restarted with, a record that fails its signature, a
// recreated stream — or that is frozen behind a broker it cannot reach
// commits nothing at all. The record it halted on is often the very one meant
// to end these sockets. What does move is the log's lag: its checkpoint stops
// while it owes records, and past [statelog.StallGrace] every identity read
// on the node answers unknown, so REST is 503. So that crossing is an event
// too: the engine watches the same lag against the same grace
// (internal/engine's identityvouch.go) and, the moment it crosses, hands the
// identity listener a move naming everyone. Every socket is decided again,
// reads what a request reads, and closes [CloseUndecided]; its reconnect's
// handshake answers 503. Inside the grace the guard still serves the rows it
// has, and so does an open socket — the bound a lagging node already has on
// REST, and no longer an unbounded one on the one surface a browser keeps
// open. The socket used to re-check itself on a minute's timer and degrade;
// that cost a periodic read on every healthy node to cover the one that is
// alarmed, and the crossing is one in-memory read a second on each node.

// Close codes beside [CloseUnauthenticated] and [CloseUnauthorized].
const (
	// CloseUndecided ends a socket whose credential this node could not
	// decide again — see the file head. It is the standard's 1013, "try
	// again later", rather than an application code, because what it asks
	// of a client is what any close outside the 4000 range asks: reconnect
	// on a backoff. The handshake that follows is what says why, over plain
	// HTTP where the status is visible.
	CloseUndecided = websocket.StatusTryAgainLater
)

// Moved is what the identity estate moved that an open socket's credential may
// depend on — the stream's own reading of the identity applier's post-commit
// value, which internal/api translates.
//
// IT NAMES WHOSE, NEVER WHAT: a socket it names is decided again from the
// rows, which the batch has already committed.
type Moved struct {
	// People are the persons whose row moved, by id.
	People []string

	// Sessions are the session lineages a record ended.
	Sessions []string

	// Everyone is a move no list can name, which decides every socket
	// again.
	Everyone bool
}

// opened is what a socket's credential is, as the identity estate names it —
// what [Moved] is matched against.
type opened struct {
	// lineage is the session a cookie was issued for, empty for a bearer.
	lineage string

	// person is the directory person the credential acts FOR: a session's
	// person, a machine token's owner. Empty for a credential composed from
	// configuration.
	person string

	// configured is a principal composed from a Tier A entry — the bearer,
	// or a session exchanged from it — whose seat binding is the directory
	// row under the token's login, and that row's id is one this socket
	// never learns: so any person's movement decides it again. They are
	// the deployment's own few credentials, and a re-read costs one lookup.
	configured bool
}

// openedWith is what the handshake's resolution says the socket was opened
// with.
func openedWith(ctx context.Context, p iam.Principal) opened {
	var o opened
	if lineage, ok := strings.CutPrefix(p.Via, iam.SessionPrefix); ok {
		o.lineage = lineage
	}
	if _, tierA := auth.TierA(ctx); tierA {
		o.configured = true
	} else if p.ID != uuid.Nil {
		o.person = p.ID.String()
	}
	return o
}

// movedBy reports whether m may have moved this credential.
func (o opened) movedBy(m Moved) bool {
	switch {
	case m.Everyone:
		return true
	case o.lineage != "" && slices.Contains(m.Sessions, o.lineage):
		return true
	case o.configured && len(m.People) > 0:
		return true
	case o.person != "" && slices.Contains(m.People, o.person):
		return true
	}
	return false
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

	// key names the credential the handshake presented
	// ([auth.Guard.PresentedKey]): every socket opened with an equal key
	// shares one decision per move — see decisions.go.
	key string
}

// deciderFor is the decision an open socket repeats: the guard's
// [auth.Guard.ResolveOpen] over the ORIGINAL handshake request, on the
// socket's own context.
func deciderFor(guard *auth.Guard, handshake *http.Request) decideFunc {
	return func(ctx context.Context) (*http.Request, *auth.Refusal) {
		return guard.ResolveOpen(handshake.Clone(ctx))
	}
}

// asking is the principal this socket's questions are asked as, moved by each
// re-decision.
//
// A LOCK RATHER THAN AN ATOMIC because it carries a whole principal and every
// query reads it while a re-decision writes it; a torn read would ask one
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

// listener is one open socket as the service's registry holds it: what it was
// opened with, and the signal that decides it again.
type listener struct {
	opened opened

	// wake holds at most one pending re-decision. A burst of moves
	// collapses into the one decision that follows it, which reads the
	// rows when it starts and so sees everything committed before the
	// last signal.
	wake chan struct{}
}

// newListener builds a listener with one re-decision already pending — the
// one that covers the window between its handshake and its registration (see
// the file head).
func newListener(o opened) *listener {
	l := &listener{opened: o, wake: make(chan struct{}, 1)}
	l.signal()
	return l
}

// signal asks for a re-decision, never blocking: its callers are the identity
// apply loop and the publish path.
func (l *listener) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// listeners is the service's registry of open sockets, by what decides them.
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

// signal asks every listener whose credential matches to decide it again.
func (ls *listeners) signal(matches func(opened) bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for l := range ls.open {
		if matches(l.opened) {
			l.signal()
		}
	}
}

// CredentialsMoved decides again every open socket whose credential m may have
// moved, and no other.
//
// NEVER BLOCKS: its caller is the identity applier's post-commit hook, on the
// apply loop's own goroutine with the next batch waiting behind it. Each socket
// is woken on its own goroutine and decided in the node's turn — see
// decisions.go.
func (s *Service) CredentialsMoved(m Moved) {
	s.wake(func(o opened) bool { return o.movedBy(m) })
}

// wake counts a move and then signals every listener matches names: counted
// FIRST, so a socket the signal wakes needs a decision whose read began after
// it — see [decisions].
func (s *Service) wake(matches func(opened) bool) {
	s.decisions.moved()
	s.listeners.signal(matches)
}

// keepDecided decides this socket's credential again on every signal and at
// its credential's own end, until ctx ends or a decision closes the socket.
// See the file head for what each answer does.
//
// resync is what a changed audience is sent: the snapshot, built for the
// audience the decision resolved, because a narrowed grant stops the pushes it
// no longer covers from the next one on and the screen has to lose what it was
// already showing under the old grant rather than keep it until a reload.
// rewatch decides the watched seat again, as whoever the decision resolved.
//
// Each decision is asked of d, in the node's turn and shared with every socket
// opened with the same credential — see decisions.go: a wake needs one whose
// read began after the signal, and the credential's own end one of its own.
func keepDecided(ctx context.Context, conn *websocket.Conn, client *Client,
	l *listener, cred credential, d *decisions, now func() time.Time,
	resync func(Audience) map[string]any, rewatch func(context.Context)) {

	held, release := d.hold(cred.key)
	defer release()

	// ONE TIMER, ARMED AT THE HANDSHAKE'S READING: a credential's own end is
	// fixed when it is issued — no re-issue moves an absolute deadline, and
	// a machine token's expiry is minted with it — so the instant to wake
	// at is known from the start.
	//
	// AND ARMED AGAIN WHEN THE GUARD STILL SERVES IT, because the timer and
	// the guard read two different clocks: the timer runs on the
	// monotonic clock from the wall-clock difference measured when it was
	// armed, and the guard compares the WALL clock against the deadline.
	// A wall clock stepped back after the arming (an NTP correction, a
	// hypervisor resuming a VM) fires the timer while the guard still
	// sees the credential live — and a timer dropped there left the
	// socket with no deadline at all, open past the one limit no record
	// will ever state, until something unrelated happened to decide it.
	// Re-armed for the wall clock's own distance to the deadline, a clock
	// that is behind is asked again when it reaches it; floored at
	// [expiryRetry], a guard whose clock disagrees costs one decision per
	// interval rather than a loop against the same instant.
	var timer *time.Timer
	var expiry <-chan time.Time
	if !cred.ends.IsZero() {
		timer = time.NewTimer(time.Until(cred.ends))
		defer timer.Stop()
		expiry = timer.C
	}
	for {
		var need uint64
		fired := false
		select {
		case <-ctx.Done():
			return
		case <-l.wake:
			need = d.woken()
		case <-expiry:
			fired = true
			need = d.expired()
		}
		r, refusal, ok := d.decide(ctx, held, need, cred.decide)
		if !ok || !decideOnce(ctx, conn, client, cred.who, r, refusal, now,
			resync, rewatch) {
			return
		}
		if fired {
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
// its deadline a socket can be served to the same small figure. Shorter buys
// nothing a person could see; longer serves an ended credential longer.
const expiryRetry = time.Second

// decideOnce acts on one decision of the socket's credential — the guard's
// resolution r and its refusal — and reports whether the socket is still open.
//
// THE DECISION MAY BE ANOTHER SOCKET'S, read over its own handshake: two
// handshakes presenting one credential are resolved alike, so what r carries
// is who this socket is too — see decisions.go.
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
		// RESOLVED, AND MAY NO LONGER READ what this socket carries: the
		// grant the handshake required was withdrawn. 4403 rather than
		// 4401, because signing in again reaches the same person with the
		// same grants.
		log.InfoContext(ctx, "stream_closed_grant_withdrawn",
			"login", principal.Login, "grant", string(iam.GrantStateRead))
		_ = conn.Close(CloseUnauthorized, "grant withdrawn: "+
			string(iam.GrantStateRead))
		return false
	}
	who.set(principal)
	// AND THE SEAT IT WATCHES, as whoever this decision resolved: a lead
	// moved off a team stops following a former report's inbox here
	// rather than at the next reconnect.
	if rewatch != nil {
		rewatch(who.context(ctx))
	}
	audience := AudienceOf(principal.Grants)
	if client.SetAudience(audience) {
		client.Reply(Push(KindSnapshot, resync(audience), now()))
	}
	return true
}
