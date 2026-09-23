package stream

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/iam"
)

// A SOCKET IS A LONG-LIVED CREDENTIAL CHECK, and the check has to be repeated.
//
// A REST request is decided once and is over. A socket is decided at its
// handshake and then stays open for as long as the tab does — hours, often a
// day — pushing the company's state and answering questions the whole time.
// Decided only at the handshake, a session revoked, a person suspended, a seat
// taken away or a grant narrowed would change nothing for a socket already
// open: the revocation this engine arbitrates across a fleet in under a second
// would stop at the one surface a browser keeps open.
//
// So every socket re-runs the guard against the credential it was opened with,
// on [RevalidateEvery], and the answer decides the socket:
//
//   - RESOLVED: the socket carries on, and the principal every later query is
//     asked as is the one just resolved — a narrowed grant takes effect here
//     within one interval rather than at the next reconnect.
//   - A SEAT REFUSAL (the person's seat is gone from the chart): closed
//     [CloseUnauthorized], because the credential is fine and what it acts as
//     is not.
//   - ANONYMOUS (the session ended, expired or was revoked; the token is no
//     longer one this node accepts): closed [CloseUnauthenticated], because the
//     remedy is to become somebody again.
//   - UNKNOWN (this node cannot read its identity estate, or is behind it):
//     NOT closed. The socket is DEGRADED instead — pushes stop, questions are
//     answered [CodeUnavailable] — and told so on a [KindIdentity] frame, and
//     the next interval checks again. Closing here would put every socket on a
//     node whose applier stalled into a reconnect loop against the same node,
//     each reconnect a full-company snapshot, which is the stampede the
//     degraded posture exists to prevent.
//
// # The credential is the one the socket was OPENED with
//
// A browser rotates its session cookie on REST requests, and a socket cannot
// receive a Set-Cookie, so the bytes this socket holds age while the tab stays
// in use. That is what the session's rotation rule already allows for — a
// LAGGING rotation index is served, not called theft (internal/iam/session) —
// and the idle deadline it carries is the one limit that can close a socket
// whose person is still active. When it does, the close is 4401 and the tab
// reconnects with the cookie the browser holds NOW.

// RevalidateEvery is how often an open socket's credential is checked again.
//
// SIXTY SECONDS, and it is not a number of its own: it is
// [statelog.StallGrace], the time a node may already serve identity it has not
// caught up on before it is alarmed and every request it serves is refused.
// A socket that re-checked less often would outlive a revocation by longer
// than the fleet tolerates any node doing; one that re-checked much more often
// would put a directory read per open tab on a timer for a horizon the rest of
// the engine does not offer. TestTheRevalidationIntervalIsTheStallGrace holds
// the two together.
const RevalidateEvery = 60 * time.Second

// identityState is the payload of a [KindIdentity] frame.
type identityState struct {
	// State is `verified` or `unverifiable`.
	State string `json:"state"`
	// RetryAfterSeconds is when the next check runs, on `unverifiable`.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

// The two identity states a socket reports.
const (
	identityVerified     = "verified"
	identityUnverifiable = "unverifiable"
)

// checkFunc re-runs the guard's resolution against this socket's credential.
type checkFunc func(ctx context.Context) (*http.Request, *auth.Refusal)

// checkerFor is the check an open socket repeats: the guard's own Resolve over
// the ORIGINAL handshake request, answered into a writer nobody reads.
//
// A WRITER NOBODY READS, because the handshake response is long gone — the
// connection was hijacked for the socket — and a resolution that clears or
// rotates a session cookie has nowhere to send it. The browser's own REST
// traffic carries those; this check only needs the answer.
func checkerFor(guard *auth.Guard, handshake *http.Request) checkFunc {
	return func(ctx context.Context) (*http.Request, *auth.Refusal) {
		return guard.Resolve(discardWriter{header: http.Header{}},
			handshake.Clone(ctx))
	}
}

// discardWriter is an [http.ResponseWriter] whose output goes nowhere.
type discardWriter struct{ header http.Header }

func (d discardWriter) Header() http.Header         { return d.header }
func (d discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d discardWriter) WriteHeader(int)             {}

// asking is the principal this socket's questions are asked as, moved by each
// revalidation.
//
// A LOCK RATHER THAN AN ATOMIC because it carries a whole principal and every
// query reads it while the revalidation writes it; a torn read would ask one
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

// context is ctx carrying the current principal.
func (a *asking) context(ctx context.Context) context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return iam.WithPrincipal(ctx, a.principal)
}

// revalidate runs until ctx ends or the socket is closed for a credential,
// re-checking every interval. See the file head for what each answer does.
//
// resync is what a released hold sends: the snapshot, because every push the
// hold swallowed is gone and a tab that resumed from where it stopped would
// show a company that moved without it — the same thing a reconnect fetches.
func revalidate(ctx context.Context, conn *websocket.Conn, client *Client,
	check checkFunc, who *asking, every time.Duration, now func() time.Time,
	resync func() any) {

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		r, refusal := check(ctx)
		if ctx.Err() != nil {
			return
		}
		principal, how := iam.From(r.Context())
		switch {
		case refusal != nil:
			// RESOLVED AND STILL REFUSED — the person's seat is gone.
			log.InfoContext(ctx, "stream_closed_seat_refused",
				"code", string(refusal.Code), "detail", refusal.Detail)
			_ = conn.Close(CloseUnauthorized, string(refusal.Code))
			return
		case how == iam.Unknown:
			if client.HoldIdentity() {
				log.WarnContext(ctx, "stream_identity_unverifiable",
					"hint", "this node cannot read its identity estate or is "+
						"behind it; the socket stays open, degraded, and is "+
						"checked again on the next interval")
			}
			client.Reply(Push(KindIdentity, identityState{
				State:             identityUnverifiable,
				RetryAfterSeconds: int(every / time.Second),
			}, now()))
		case how != iam.Resolved:
			log.InfoContext(ctx, "stream_closed_credential_ended")
			_ = conn.Close(CloseUnauthenticated, "credential no longer accepted")
			return
		default:
			who.set(principal)
			if client.ReleaseIdentity() {
				client.Reply(Push(KindIdentity,
					identityState{State: identityVerified}, now()))
				client.Reply(Push(KindSnapshot, resync(), now()))
			}
		}
	}
}
