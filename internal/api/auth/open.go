package auth

import (
	"context"
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
// ONE difference: a session's IDLE deadline is set aside
// ([session.Signer.ValidateOpen]).
//
// The idle deadline is moved by a re-issue on a REST response, which a
// connection never receives: its idle deadline stays the handshake's plus the
// limit however busy the tab is, so every dashboard left open past it would be
// closed by the first identity event while its person is at the screen. An
// open live view is activity; the session is decided on its ROWS (ended,
// revoked, person suspended or removed, generation moved) and its ABSOLUTE
// deadline, which no re-issue moves.
func (g *Guard) ResolveOpen(r *http.Request) (*http.Request, *Refusal) {
	// NOTHING IS WRITTEN: the connection was hijacked at its handshake, so
	// there is no cookie to set, and REST traffic carries the re-issues. A
	// refusal is not COUNTED (audit.go): a re-decision is the connection's,
	// not a presented credential.
	return g.resolve(discardWriter{header: http.Header{}}, r, true)
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
