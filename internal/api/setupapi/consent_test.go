package setupapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
)

// fakeConsents signs in the kind "adsense" alone, recording who asked, and
// comes back to callback.
type fakeConsents struct {
	mu       sync.Mutex
	by       []iam.Actor
	id       uuid.UUID
	callback string
}

func (f *fakeConsents) SignInCallback() string { return f.callback }

func (f *fakeConsents) StartConsent(_ context.Context, kind integration.Kind, by iam.Actor) (setupapi.ConsentSignIn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.by = append(f.by, by)
	switch kind {
	case "adsense":
		return setupapi.ConsentSignIn{ID: f.id, Kind: string(kind), Status: "pending", StartedAt: pinned,
			ExpiresAt: pinned.Add(15 * time.Minute), AuthorizeURL: "https://accounts.example.com/auth?state=s"}, nil
	case "admob":
		return setupapi.ConsentSignIn{}, fmt.Errorf("%w: set api.external_url", setupapi.ErrNoConsentAddress)
	case "turned_off":
		return setupapi.ConsentSignIn{}, &setupapi.ConsentRefusal{Status: http.StatusConflict,
			Detail: "the integration is turned off. Turn it on, then sign in"}
	}
	return setupapi.ConsentSignIn{}, setupapi.ErrNoConsent
}

func (f *fakeConsents) ConsentSignIn(_ context.Context, id uuid.UUID) (setupapi.ConsentSignIn, error) {
	if id != f.id {
		return setupapi.ConsentSignIn{}, setupapi.ErrNoConsentSignIn
	}
	return setupapi.ConsentSignIn{ID: id, Kind: "adsense", Status: "approved", StartedAt: pinned,
		ExpiresAt: pinned.Add(15 * time.Minute)}, nil
}

func (f *fakeConsents) SignOutConsent(_ context.Context, kind integration.Kind, _ iam.Actor) (setupapi.ConsentSignOut, error) {
	if kind != "adsense" {
		return setupapi.ConsentSignOut{}, setupapi.ErrConsentNotSignedIn
	}
	return setupapi.ConsentSignOut{Kind: string(kind), Revocation: "shared", Said: "Crewlet deleted its copy."}, nil
}

// A CONSENT SIGN-IN IS STARTED, READ AND GIVEN UP THROUGH THE SETUP SURFACE: a
// start answers the address the browser goes to and records who asked; each
// engine refusal answers its own status and code; a sign-out says what Google
// was asked; and a surface wired with no consents answers 501.
//
// Mutation: map the missing-address refusal to 503, or answer the start 200,
// and a case goes red.
func TestAConsentSignInIsStartedReadAndGivenUpThroughSetup(t *testing.T) {
	t.Parallel()
	fake := &fakeConsents{id: uuid.New()}
	s := newSurface(t)
	s.setup = newService(t, setupapi.Options{Config: s.config, Secrets: s.vault, Status: s.status,
		WebhookBase: fixtureWebhookBase, WebhookBaseKey: oneListener, Consents: fake,
		Now: func() time.Time { return pinned }})
	s.mux = http.NewServeMux()
	mount(t, s.mux, s.setup, s.config)

	res := s.do(t, http.MethodPost, "/setup/integrations/adsense/consent", "{}", nil)
	body := decode(t, res)
	if res.Code != http.StatusCreated || body["authorize_url"] != "https://accounts.example.com/auth?state=s" ||
		body["status"] != "pending" {
		t.Fatalf("a start = %d %v", res.Code, body)
	}
	if len(fake.by) != 1 || fake.by[0].Name == "" {
		t.Fatalf("the engine was told the start was by %+v", fake.by)
	}
	for _, c := range []struct {
		kind, code string
		status     int
	}{
		{"admob", "no_external_url", http.StatusConflict},
		{"turned_off", "vendor_refused", http.StatusConflict},
		{"gitlab", "unknown_kind", http.StatusNotFound},
	} {
		res := s.do(t, http.MethodPost, "/setup/integrations/"+c.kind+"/consent", "{}", nil)
		if got := decode(t, res); res.Code != c.status || got["error"] != c.code {
			t.Errorf("a start of %s = %d %v, want %d %s", c.kind, res.Code, got, c.status, c.code)
		}
	}
	if res := s.do(t, http.MethodGet, "/setup/consent-sign-ins/"+fake.id.String(), "", nil); res.Code != http.StatusOK ||
		decode(t, res)["status"] != "approved" {
		t.Errorf("the poll = %d %s", res.Code, res.Body)
	}
	if res := s.do(t, http.MethodGet, "/setup/consent-sign-ins/"+uuid.NewString(), "", nil); res.Code != http.StatusNotFound {
		t.Errorf("a poll of an unknown sign-in = %d", res.Code)
	}
	res = s.do(t, http.MethodDelete, "/setup/integrations/adsense/consent", "", nil)
	if got := decode(t, res); res.Code != http.StatusOK || got["revocation"] != "shared" {
		t.Errorf("a sign-out = %d %v", res.Code, got)
	}
	if res := s.do(t, http.MethodDelete, "/setup/integrations/admob/consent", "", nil); res.Code != http.StatusNotFound {
		t.Errorf("a sign-out nobody signed in to = %d", res.Code)
	}

	unwired := newSurface(t)
	if res := unwired.do(t, http.MethodPost, "/setup/integrations/adsense/consent", "{}", nil); res.Code != http.StatusNotImplemented ||
		!strings.Contains(res.Body.String(), "consent") {
		t.Errorf("an unwired start = %d %s", res.Code, res.Body)
	}
}
