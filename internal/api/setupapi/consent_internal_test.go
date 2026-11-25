package setupapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
)

// askedConsents counts what reached the engine.
type askedConsents struct{ asked int }

func (a *askedConsents) SignInCallback() string { return "" }

func (a *askedConsents) StartConsent(context.Context, integration.Kind, iam.Actor) (ConsentSignIn, error) {
	a.asked++
	return ConsentSignIn{}, nil
}

func (a *askedConsents) ConsentSignIn(context.Context, uuid.UUID) (ConsentSignIn, error) {
	return ConsentSignIn{}, nil
}

func (a *askedConsents) SignOutConsent(context.Context, integration.Kind, iam.Actor) (ConsentSignOut, error) {
	a.asked++
	return ConsentSignOut{}, nil
}

// A CONSENT SIGN-IN AND SIGN-OUT ARE CREDENTIAL WRITES: a caller the guard
// refuses secrets.write is answered 403 and the engine is asked nothing.
//
// Mutation: drop mayWriteCredentials from either handler, and the engine is
// asked.
func TestAConsentGestureIsRefusedACallerWithoutSecretsWrite(t *testing.T) {
	t.Parallel()
	engine := &askedConsents{}
	s := &Service{consents: engine, guard: func(_ *http.Request, p authz.Policy) authz.Decision {
		return authz.Decision{Allowed: p.Action != authz.ActionSecretWrite, Reason: authz.ReasonEverybody}
	}}
	for _, c := range []struct {
		method string
		serve  func(http.ResponseWriter, *http.Request)
	}{{http.MethodPost, s.startConsent}, {http.MethodDelete, s.signOutConsent}} {
		r := httptest.NewRequest(c.method, "/setup/integrations/adsense/consent", nil)
		r.SetPathValue("kind", "adsense")
		w := httptest.NewRecorder()
		c.serve(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s without secrets.write = %d %s, want 403", c.method, w.Code, w.Body)
		}
	}
	if engine.asked != 0 {
		t.Errorf("the engine was asked %d times by a caller without secrets.write", engine.asked)
	}
}
