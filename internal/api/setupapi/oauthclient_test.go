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

// THE ONE SIGN-IN CLIENT IS ASKED ONCE, ON GOOGLE CLOUD'S ANSWER: its redirect
// URI (the engine's own callback and the setting it is built on) just before
// its ID, all in the sign-in group, written under google_cloud.sign_in_client;
// no consent product asks a client or a redirect of its own, only its
// sign-in; and an engine with no public address answers the URI empty.
//
// Mutation: drop FillRedirects, the group, or append the client to a product
// again, and a case goes red.
func TestTheOneSignInClientIsAskedOnceWithTheEnginesRedirectURI(t *testing.T) {
	t.Parallel()
	s := consentSurface(t, testCallback)
	s.seed(t)
	cloud := toolState(t, s, integration.KindGoogleCloud)
	r := redirectOf(cloud)
	switch {
	case r == nil:
		t.Fatal("Google Cloud's answer shows no redirect URI for the sign-in client")
	case r.Redirect == nil || r.Redirect.URL != testCallback || r.Redirect.Key != "api.public.external_url":
		t.Errorf("the redirect URI = %+v, want %s on api.public.external_url", r.Redirect, testCallback)
	case indexOf(cloud, "sign_in_client_id") != indexOf(cloud, "redirect_uri")+1:
		t.Error("the redirect URI is not asked just before the client's ID")
	}
	for _, field := range []string{"redirect_uri", "sign_in_client_id", "sign_in_client_secret"} {
		i := indexOf(cloud, field)
		if i < 0 || cloud.Requirements[i].Group != setup.GroupSignIn {
			t.Errorf("%s is not asked in the sign-in group", field)
		}
	}
	if got := cloud.Requirements[indexOf(cloud, "sign_in_client_id")].ConfigPath; got !=
		"integrations.google_cloud.sign_in_client.client_id" {
		t.Errorf("the client ID is written to %q", got)
	}
	var products int
	for _, kind := range integration.Kinds {
		if _, ok := googleauth.ConsentOf(kind); !ok {
			continue
		}
		products++
		state := toolState(t, s, kind)
		if redirectOf(state) != nil || indexOf(state, "client_id") >= 0 || indexOf(state, "client_secret") >= 0 {
			t.Errorf("%s asks a client of its own", kind)
		}
		if i := indexOf(state, "google_sign_in"); i < 0 || state.Requirements[i].Kind != setup.KindConsent {
			t.Errorf("%s does not ask its sign-in", kind)
		}
	}
	if products < 4 {
		t.Fatalf("%d consent products, and AdMob, AdSense, Google Analytics and Google Play are four", products)
	}

	none := consentSurface(t, "")
	none.seed(t)
	if r := redirectOf(toolState(t, none, integration.KindGoogleCloud)); r == nil || r.Redirect == nil ||
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
