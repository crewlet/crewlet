package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"
)

// AN OPEN CONNECTION'S CREDENTIAL, decided again on the connection's behalf.
//
// A REST request is decided once and is over. A connection held open — the
// dashboard's live socket — is decided at its handshake and then stays open
// for as long as the tab does, and what ends it is not a timer but the record
// that ended its credential: the holder of the connection decides it again
// when the identity estate says that credential moved, and at the instant the
// credential ends on its own ([Lifetime]). Both are answered here, by the
// guard's own rules, so a connection and a request presenting the same
// credential are never told two different things.

// ResolveOpen answers who the credential an OPEN CONNECTION was opened with is
// NOW, by [Guard.Resolve]'s rules over the connection's handshake request, with
// ONE difference: a session's IDLE deadline is set aside.
//
// # Why the idle deadline does not end an open connection
//
// The idle deadline is the bearer's own, moved by a re-issue on a REST
// response — and a connection can never receive one: its bearer is the one
// its handshake presented, so its idle deadline is the handshake's plus
// [session.Idle] however busy the tab has been since. Decided on that, every
// dashboard left open past it would be closed by the first identity event
// anywhere in the company, while its person was at the screen. An open live
// view is activity; the session is decided on its ROWS instead — ended,
// revoked, its person suspended or removed, the company's generation moved —
// and its ABSOLUTE deadline, which no re-issue moves, still ends it.
//
// NOTHING IS WRITTEN: there is no response to write a cookie to, the
// connection having been hijacked at its handshake, and the browser's own REST
// traffic carries every re-issue. And nothing is RECORDED as a use — see
// audit.go: a re-decision is the connection's, not somebody presenting a
// token.
func (g *Guard) ResolveOpen(r *http.Request) (*http.Request, *Refusal) {
	return g.resolve(discardWriter{header: http.Header{}}, r, true)
}

// PresentedKey names the credential r presents to this guard, as a digest: its
// bearer when it carries one, its session cookie when it does not, and the
// absence of both otherwise — the same choice, in the same order, that
// [Guard.Resolve] makes.
//
// # What it is for
//
// Two requests with equal keys present the same credential, and
// [Guard.ResolveOpen] reads nothing else off a request to decide it: so ONE
// decision answers for every connection opened with that credential. The
// dashboard's live socket decides every open connection again whenever the
// identity estate or the company moves, and a person with several tabs open
// holds several connections on one cookie; asked once per connection, every
// move read their rows once per tab, all at once, on the one connection the
// store reserves for identity reads — ahead of every REST request's own.
//
// A DIGEST AND NEVER THE VALUE, because a key outlives the request it was
// taken from in whatever map holds it, and a credential copied into one is a
// second place it can leak from. SHA-256 over a kind-prefixed value, so a
// bearer and a cookie of the same bytes are two credentials, as the guard
// reads them.
func (g *Guard) PresentedKey(r *http.Request) string {
	sum := sha256.New()
	switch bearer := g.Credential(r); {
	case bearer != "":
		sum.Write([]byte("bearer\x00" + bearer))
	case g.sessions != nil && g.sessions.cookieOf(r) != "":
		sum.Write([]byte("cookie\x00" + g.sessions.cookieOf(r)))
	default:
		// NOTHING PRESENTED, which the guard resolves the same way for
		// every request: the development principal where one is
		// configured, anonymous everywhere else.
		sum.Write([]byte("none\x00"))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// lifetimeKey carries the instant the credential a request presented ends on
// its own.
type lifetimeKey struct{}

// withLifetime marks ctx with the instant its credential ends on its own; the
// zero instant marks nothing.
func withLifetime(ctx context.Context, at time.Time) context.Context {
	if at.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, lifetimeKey{}, at.UTC())
}

// Lifetime is the instant the credential a resolved request presented ends ON
// ITS OWN, whatever any record says — a session's absolute deadline, a machine
// token's expiry — and false for a credential with none: a Tier A token,
// which ends when the configuration stops naming it, and the development
// principal.
//
// A CONNECTION HELD PAST THE REQUEST is what asks: nothing else will say when
// that instant passes, because no record is written at it.
func Lifetime(ctx context.Context) (time.Time, bool) {
	at, ok := ctx.Value(lifetimeKey{}).(time.Time)
	return at, ok && !at.IsZero()
}

// discardWriter is an [http.ResponseWriter] whose output goes nowhere — the
// writer [Guard.ResolveOpen] resolves into.
type discardWriter struct{ header http.Header }

func (d discardWriter) Header() http.Header         { return d.header }
func (d discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d discardWriter) WriteHeader(int)             {}
