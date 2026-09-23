package authapi

import (
	"context"
	"errors"
	"github.com/crewlet/crewlet/internal/events/types"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PROVIDER SIGN-IN HANDS ITS REFRESH TOKEN TO CUSTODY, beside the session it
// belongs to — and a sign-in whose token could not be kept is REFUSED.
//
// The deactivation probe is the only thing that notices somebody disabled at
// the provider, and it asks with this token; a session whose token was
// dropped is one no central deactivation ends before its absolute deadline.
// So three things are held here:
//
//  1. The grant names the session the cookie is for, the provider's issuer,
//     and the POSITION the session's record landed at — which is what lets a
//     peer that has not applied that record yet tell "not arrived" from
//     "gone" and keep the grant.
//  2. A custody failure answers 503 with no cookie, and closes the session it
//     had opened, so the sessions screen does not list one nobody holds.
//  3. A sign-in with no provider token hands custody nothing.
func TestAProviderSignInKeepsItsRefreshTokenOrIsRefused(t *testing.T) {
	t.Parallel()
	landed := statelog.Position{Stream: "CREWLET_IAM_LOG", Seq: 42}
	held := iamdomain.Sighting{ID: "018f3a9c-0000-7000-8000-000000000001",
		Login: "sarah.chen"}

	t.Run("kept", func(t *testing.T) {
		t.Parallel()
		svc, writer, custody := keepFixture(t, landed, nil)
		rec := httptest.NewRecorder()
		svc.completeSignIn(rec, signInRequest(), held,
			signIn{method: types.SignInOIDC, refresh: "refresh-1"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", rec.Code)
		}
		grants := custody.held()
		if len(grants) != 1 {
			t.Fatalf("custody holds %d grants, want the one this sign-in obtained",
				len(grants))
		}
		got := grants[0]
		want := iamdomain.RefreshGrant{
			Lineage: writer.opened, Person: held.ID,
			Issuer: "https://idp.example.com", Token: "refresh-1",
			Start: uint64(landed.Packed()),
		}
		if got != want {
			t.Errorf("custody holds %+v, want %+v", got, want)
		}
	})

	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		svc, writer, _ := keepFixture(t, landed,
			errors.New("coordination store: no responders"))
		rec := httptest.NewRecorder()
		svc.completeSignIn(rec, signInRequest(), held,
			signIn{method: types.SignInOIDC, refresh: "refresh-1"})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d with the token unkept, want 503 — admitted, the "+
				"session is one the probe never sees", rec.Code)
		}
		if cookies := rec.Result().Cookies(); len(cookies) != 0 {
			t.Errorf("a refused sign-in set %d cookies", len(cookies))
		}
		if writer.closed != writer.opened || writer.reason != reasonRefreshUnkept {
			t.Errorf("the session it opened was closed as (%q, %q), want (%q, %q)",
				writer.closed, writer.reason, writer.opened, reasonRefreshUnkept)
		}
	})

	t.Run("no provider token", func(t *testing.T) {
		t.Parallel()
		svc, _, custody := keepFixture(t, landed, nil)
		rec := httptest.NewRecorder()
		svc.completeSignIn(rec, signInRequest(), held,
			signIn{method: types.SignInPassword})
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", rec.Code)
		}
		if n := len(custody.held()); n != 0 {
			t.Errorf("a sign-in with no provider token put %d grants in custody", n)
		}
	})
}

// keepFixture is the surface with only what completeSignIn touches.
func keepFixture(t *testing.T, landed statelog.Position, holdErr error) (
	*Service, *openingWriter, *recordingCustody) {

	t.Helper()
	b := config.DefaultBootstrap()
	b.API.ExternalURL = "https://crewlet.example.com"
	signer, err := session.New(session.Options{
		Material: runtoken.Material{
			ActiveID: "k1",
			Keys:     []runtoken.KeyMaterial{{ID: "k1", Material: "a-fixture-signing-key"}},
		},
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	writer := &openingWriter{at: landed}
	custody := &recordingCustody{err: holdErr}
	return &Service{
		boot: &b, writer: writer, signer: signer, custody: custody,
		provider: oidc.NewProvider(oidc.Config{Issuer: "https://idp.example.com"},
			nil, nil),
		now: time.Now,
	}, writer, custody
}

func signInRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil)
}

// openingWriter records the session a sign-in opens and the one it closes.
type openingWriter struct {
	Writer // every other verb panics: a sign-in must not reach it
	at     statelog.Position

	mu             sync.Mutex
	opened, closed string
	reason         string
}

func (w *openingWriter) OpenSession(_ context.Context, in iamdomain.SessionStart) (
	statelog.Position, error) {

	w.mu.Lock()
	defer w.mu.Unlock()
	w.opened = in.Lineage
	return w.at, nil
}

func (w *openingWriter) CloseSession(_ context.Context, lineage, _, reason, _ string) (
	statelog.Position, error) {

	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed, w.reason = lineage, reason
	return w.at, nil
}

// recordingCustody records what it was handed, or refuses it.
type recordingCustody struct {
	err    error
	mu     sync.Mutex
	grants []iamdomain.RefreshGrant
}

func (c *recordingCustody) Hold(_ context.Context, g iamdomain.RefreshGrant, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.grants = append(c.grants, g)
	return nil
}

func (c *recordingCustody) held() []iamdomain.RefreshGrant {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]iamdomain.RefreshGrant(nil), c.grants...)
}
