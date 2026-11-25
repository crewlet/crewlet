package setupapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// testCallback is the engine's sign-in callback a case's fake answers.
const testCallback = "https://hooks.example.com/webhooks/mcp-oauth"

// analyticsCloud is a company with Google Cloud on through its provisioning
// block, its federation key named; a case resolves it or not.
const analyticsCloud = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {
    "google_cloud": {"enabled": true, "provisioning": {
      "identity_project": "acme-crewlet-id",
      "provider": "projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine",
      "federation_key": "${GOOGLE_CLOUD_FEDERATION_KEY}"}}},
  "roles": [{"name": "CTO", "handle": "cto", "llm": "zulu"}]
}`

// consentSurface is a surface whose engine comes back to callback, its address
// read from the public listener's key.
func consentSurface(t *testing.T, callback string) *surface {
	t.Helper()
	s := newSurface(t)
	s.setup = newService(t, setupapi.Options{Config: s.config, Secrets: s.vault, Resolve: s.vault.get,
		Status: s.status, WebhookBase: fixtureWebhookBase, WebhookBaseKey: "api.public.external_url",
		Consents: &fakeConsents{id: uuid.New(), callback: callback}, Now: func() time.Time { return pinned }})
	s.mux = http.NewServeMux()
	mount(t, s.mux, s.setup, s.config)
	return s
}

func toolState(t *testing.T, s *surface, kind integration.Kind) setupapi.ToolState {
	t.Helper()
	res := s.do(t, http.MethodGet, "/setup/integrations/"+string(kind), "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("%s: status = %d: %s", kind, res.Code, res.Body)
	}
	var state setupapi.ToolState
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func redirectOf(state setupapi.ToolState) *setup.Requirement {
	for i, r := range state.Requirements {
		if r.Kind == setup.KindRedirect {
			return &state.Requirements[i]
		}
	}
	return nil
}

// EVERY CONSENT PRODUCT'S FORM SHOWS THE REDIRECT URI ITS CLIENT REGISTERS:
// the engine's own sign-in callback and the setting it is built on, before
// the client's ID, for every kind the consent registry holds; and an engine
// with no public address answers it empty, for the form to say so.
//
// Mutation: drop FillRedirects, or a product's redirect row, and a case goes red.
func TestAConsentProductsFormShowsTheEnginesRedirectURI(t *testing.T) {
	t.Parallel()
	s := consentSurface(t, testCallback)
	s.seed(t)
	var products int
	for _, kind := range integration.Kinds {
		if _, ok := googleauth.ConsentOf(kind); !ok {
			continue
		}
		products++
		state := toolState(t, s, kind)
		r := redirectOf(state)
		switch {
		case r == nil:
			t.Errorf("%s: the form shows no redirect URI for its OAuth client", kind)
		case r.Redirect == nil || r.Redirect.URL != testCallback || r.Redirect.Key != "api.public.external_url":
			t.Errorf("%s: the redirect URI = %+v, want %s on api.public.external_url", kind, r.Redirect, testCallback)
		case state.Requirements[indexOf(state, "client_id")-1].Kind != setup.KindRedirect:
			t.Errorf("%s: the redirect URI is not asked just before the client's ID", kind)
		}
	}
	if products < 4 {
		t.Fatalf("%d consent products, and AdMob, AdSense, Google Analytics and Google Play are four", products)
	}

	none := consentSurface(t, "")
	none.seed(t)
	if r := redirectOf(toolState(t, none, integration.KindGoogleAnalytics)); r == nil || r.Redirect == nil ||
		r.Redirect.URL != "" || r.Redirect.Key == "" {
		t.Errorf("with no public address the redirect URI = %+v, want empty with its setting", r)
	}
}

func indexOf(state setupapi.ToolState, field string) int {
	for i, r := range state.Requirements {
		if r.Field == field {
			return i
		}
	}
	return -1
}

// A STEP THAT CONNECTS ANOTHER INTEGRATION FIRST SAYS WHETHER IT IS: Google
// Analytics' first step answers Google Cloud not connected until Google Cloud
// is on with nothing it requires outstanding.
//
// Mutation: answer Connected from the block's presence alone, and the
// unresolved key reads connected.
func TestAStepNeedingGoogleCloudSaysWhetherItIsConnected(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, doc string
		key       bool
		want      bool
	}{
		{"no Google Cloud", companyDoc, false, false},
		{"its key unresolved", analyticsCloud, false, false},
		{"connected", analyticsCloud, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := consentSurface(t, testCallback)
			if c.key {
				s.vault.put(t, "GOOGLE_CLOUD_FEDERATION_KEY", "-----BEGIN PRIVATE KEY-----")
			}
			s.seedDocument(t, c.doc)
			steps := toolState(t, s, integration.KindGoogleAnalytics).Prepare
			if len(steps) == 0 || steps[0].Needs != integration.KindGoogleCloud {
				t.Fatalf("the first step = %+v, want one needing Google Cloud", steps)
			}
			if got := steps[0].Connected; got == nil || *got != c.want {
				t.Errorf("Google Cloud connected = %v, want %v", got, c.want)
			}
			for _, step := range steps[1:] {
				if step.Connected != nil {
					t.Errorf("a step needing nothing answers connected: %+v", step)
				}
			}
		})
	}
}
