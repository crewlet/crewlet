package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// openRig is a signed-in person whose session outlives its idle deadline, on a
// clock a case moves: the shape of a dashboard tab left open overnight.
type openRig struct {
	*signedIn
	clock    *time.Time
	absolute time.Time
}

func newOpenRig(t *testing.T) *openRig {
	t.Helper()
	s := newSignedIn(t)
	clock := s.at
	signer, err := session.New(session.Options{
		Material: sessionKeyring(),
		Now:      func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("build a signer: %v", err)
	}
	// AN ABSOLUTE DEADLINE PAST THE IDLE ONE, so the idle deadline is the
	// one a tab open past it meets first.
	absolute := s.at.Add(session.Idle + 12*time.Hour)
	cookie, err := signer.Mint(session.Mint{
		Lineage: s.lineage, Person: sessionPerson, Epoch: 3, Generation: 1,
		StartPosition: sessionStart, AbsoluteExpiresAt: absolute,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	s.signer, s.cookie = signer, cookie
	return &openRig{signedIn: s, clock: &clock, absolute: absolute}
}

// advance moves the rig's clock, the signer's and the arm's together.
func (o *openRig) advance(by time.Duration) {
	*o.clock = o.clock.Add(by)
	o.at = *o.clock
}

// handshake is the request a socket was opened with: this rig's cookie.
func (o *openRig) handshake() *http.Request {
	r := httptest.NewRequest(http.MethodGet, auth.SocketPath, nil)
	o.withCookie(r)
	return r
}

// AN OPEN CONNECTION IS NOT ENDED BY ITS IDLE DEADLINE, and is by everything
// else that ends its session.
//
// A socket holds the bearer its handshake presented and can never be re-issued
// one, so its idle deadline is the handshake's plus [session.Idle] however busy
// the tab has been since. Decided on that, a dashboard left open past it would
// be closed by the first identity event anywhere in the company, while its
// person was at the screen. The control is the same cookie presented as a
// REQUEST at the same instant, which the idle deadline does end — so the
// served answer is the open connection's rule and not a bearer that was still
// live. And the rows still decide (a session a record ended), and so does the
// absolute deadline, which no re-issue moves.
func TestAnOpenConnectionIsNotEndedByItsIdleDeadline(t *testing.T) {
	t.Parallel()
	o := newOpenRig(t)
	g := o.guard()
	o.advance(session.Idle + time.Hour)

	if r, _ := g.Resolve(httptest.NewRecorder(), o.handshake()); func() bool {
		_, how := iam.From(r.Context())
		return how != iam.Anonymous
	}() {
		t.Fatal("the control: a request presenting a bearer past its idle " +
			"deadline was served, so the case below proves nothing")
	}
	r, refusal := g.ResolveOpen(o.handshake())
	principal, how := iam.From(r.Context())
	if how != iam.Resolved || refusal != nil || principal.Login != "sarah.chen" {
		t.Fatalf("an open connection past its idle deadline resolved %v %+v "+
			"(refusal %v), want its person: a tab left open is activity", how,
			principal, refusal)
	}
	if at, ok := auth.Lifetime(r.Context()); !ok || !at.Equal(o.absolute) {
		t.Errorf("its lifetime is %v (%v), want the absolute deadline %v", at, ok,
			o.absolute)
	}

	o.dir.identity.Session.Ended = true
	r, _ = g.ResolveOpen(o.handshake())
	if _, how := iam.From(r.Context()); how != iam.Anonymous {
		t.Errorf("an open connection whose session a record ended resolved %v, "+
			"want anonymous: the rows still decide", how)
	}

	o.dir.identity.Session.Ended = false
	o.advance(o.absolute.Sub(*o.clock))
	r, _ = g.ResolveOpen(o.handshake())
	if _, how := iam.From(r.Context()); how != iam.Anonymous {
		t.Errorf("an open connection at its absolute deadline resolved %v, "+
			"want anonymous: no re-issue moves that one", how)
	}
}

// A CREDENTIAL'S LIFETIME IS ITS OWN END, and a credential with none has none.
//
// A connection held past its handshake learns when its credential ends on its
// own from here, because no record is written at that instant: a session's
// absolute deadline, a machine token's expiry — and nothing for a Tier A
// token, which ends when the configuration stops naming it, where a lifetime
// read as the zero instant would close every break-glass socket at once.
func TestACredentialsLifetimeIsItsOwnEnd(t *testing.T) {
	t.Parallel()
	s := newSignedIn(t)
	g := s.guard()

	cookie := httptest.NewRequest(http.MethodGet, auth.SocketPath, nil)
	s.withCookie(cookie)
	r, _ := g.Resolve(httptest.NewRecorder(), cookie)
	if at, ok := auth.Lifetime(r.Context()); !ok || !at.Equal(s.at.Add(8*time.Hour)) {
		t.Errorf("a session's lifetime is %v (%v), want its absolute deadline", at, ok)
	}

	tierA := httptest.NewRequest(http.MethodGet, auth.SocketPath, nil)
	tierA.Header.Set("Authorization", "Bearer a-tier-a-token")
	r, _ = g.Resolve(httptest.NewRecorder(), tierA)
	if _, how := iam.From(r.Context()); how != iam.Resolved {
		t.Fatalf("the Tier A token did not resolve (%v), so its lifetime says nothing", how)
	}
	if at, ok := auth.Lifetime(r.Context()); ok {
		t.Errorf("a Tier A token has a lifetime of %v: it ends when the "+
			"configuration stops naming it, and no sooner", at)
	}

	m := newMachineRig(t)
	pat := httptest.NewRequest(http.MethodGet, auth.SocketPath, nil)
	pat.Header.Set("Authorization", "Bearer "+m.presented.Value())
	r, _ = m.guard(nil).Resolve(httptest.NewRecorder(), pat)
	if at, ok := auth.Lifetime(r.Context()); !ok || !at.Equal(m.row.ExpiresAt) {
		t.Errorf("a machine token's lifetime is %v (%v), want its expiry %v", at, ok,
			m.row.ExpiresAt)
	}
}
