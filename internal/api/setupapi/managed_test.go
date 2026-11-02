package setupapi_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/iam"
)

// tierA is the principal the guard composes for the Tier A token id: its own
// credential, under its whole login, holding every grant and fresh for a
// step-up as every Tier A bearer is.
func tierA(id string) iam.Principal {
	return iam.Principal{
		ID: uuid.NewSHA1(auth.TokenNamespace, []byte(id)), Login: iam.TokenLogin(id),
		Kind: iam.KindMachine, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: time.Now().Add(time.Hour),
	}
}

// managedSetup is a node whose Tier A names the token `operator` as the
// company document's only writer, with the fixture company imported by that
// writer.
func managedSetup(t *testing.T) *surface {
	t.Helper()
	s := newManagedSurface(t, nil, fixtureWebhookBase, "operator")
	res := s.doAs(t, tierA("operator"), http.MethodPut, "/config", companyDoc,
		map[string]string{"X-Summary": "first import"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	return s
}

// connectDatadog is the submission that writes Datadog's pointers.
const connectDatadog = `{
	"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"},
	"generate": ["webhook_token"]
}`

// refusedAsManaged asserts the shared refusal: 403, the code, the writer.
func refusedAsManaged(t *testing.T, res interface {
	Result() *http.Response
}, body string) {
	t.Helper()
	if code := res.Result().StatusCode; code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", code, body)
	}
	if !strings.Contains(body, `"error":"config_managed"`) || !strings.Contains(body, `"managed_by":["operator"]`) {
		t.Errorf("the refusal does not say the document is managed by operator: %s", body)
	}
}

// A CONNECT THAT WOULD CHANGE A MANAGED DOCUMENT IS REFUSED BEFORE ANY VALUE
// IS SEALED, and the listed writer's own connect goes through.
func TestAManagedDocumentRefusesAConnectBeforeSealing(t *testing.T) {
	t.Parallel()
	s := managedSetup(t)

	res := s.doAs(t, tierA("alice"), http.MethodPost, "/setup/integrations/datadog/inputs", connectDatadog, nil)
	refusedAsManaged(t, res, res.Body.String())
	if _, sealed := s.vault.get("DATADOG_WEBHOOK_TOKEN"); sealed {
		t.Error("a refused connect sealed a credential nothing points at")
	}

	res = s.doAs(t, tierA("operator"), http.MethodPost, "/setup/integrations/datadog/inputs", connectDatadog, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("the writer's connect = %d: %s", res.Code, res.Body)
	}
}

// AND A ROTATION IS OPEN TO EVERY CREDENTIAL the authority table admits: the
// pointer is already in the document, so the submission seals the new value
// and reloads the revision unchanged — the break-glass a person keeps when a
// key leaks and the system that manages the document cannot know.
func TestARotationOnAManagedDocumentIsOpen(t *testing.T) {
	t.Parallel()
	s := managedSetup(t)
	if res := s.doAs(t, tierA("operator"), http.MethodPost, "/setup/integrations/datadog/inputs",
		connectDatadog, nil); res.Code != http.StatusCreated {
		t.Fatalf("the writer's connect = %d: %s", res.Code, res.Body)
	}
	res := s.doAs(t, tierA("alice"), http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"values": {"api_key": "dd-api-rotated"}}`, nil)
	if res.Code/100 != 2 {
		t.Fatalf("alice's rotation = %d: %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), `"reloaded":true`) {
		t.Errorf("the rotation did not reload the unchanged document: %s", res.Body)
	}
	if got, _ := s.vault.get("DATADOG_API_KEY"); got != "dd-api-rotated" {
		t.Errorf("the rotated value was not sealed: %q", got)
	}
}

// A DISCONNECT REMOVES A BLOCK, so it is refused before the teardown is
// queued — forced or not — and the writer's goes through.
func TestAManagedDocumentRefusesADisconnect(t *testing.T) {
	t.Parallel()
	s := managedSetup(t)
	if res := s.doAs(t, tierA("operator"), http.MethodPost, "/setup/integrations/datadog/inputs",
		connectDatadog, nil); res.Code != http.StatusCreated {
		t.Fatalf("the writer's connect = %d: %s", res.Code, res.Body)
	}
	for _, body := range []string{``, `{"force": true}`} {
		res := s.doAs(t, tierA("alice"), http.MethodDelete, "/setup/integrations/datadog", body, nil)
		refusedAsManaged(t, res, res.Body.String())
	}
	if rows, err := s.status.LoadIntegrations(t.Context()); err != nil || len(rows) != 0 {
		t.Errorf("a refused disconnect recorded a status row: %v, %v", rows, err)
	}
	res := s.doAs(t, tierA("operator"), http.MethodDelete, "/setup/integrations/datadog", `{"force": true}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("the writer's forced disconnect = %d: %s", res.Code, res.Body)
	}
}

// A GITHUB APP IS RECORDED ON ITS SEAT, so a managed document refuses its
// creation at the BEGIN route — before GitHub creates an app whose key is
// issued once — and again at the callback, before the exchange, for a state
// begun by a credential this node does not let write.
func TestAManagedDocumentRefusesAGitHubAppBeforeGitHubIsAsked(t *testing.T) {
	t.Parallel()
	s := newManagedSurface(t, nil, fixtureWebhookBase, "operator")
	if res := s.doAs(t, tierA("operator"), http.MethodPut, "/config", githubAppsDoc,
		map[string]string{"X-Summary": "a company mid-rollout"}); res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	res := s.doAs(t, tierA("alice"), http.MethodPost, "/setup/integrations/github/app", `{"seat":"sre-lead"}`, nil)
	refusedAsManaged(t, res, res.Body.String())
	if strings.Contains(res.Body.String(), `"state"`) {
		t.Error("a refused begin minted a state")
	}

	var asked atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		http.Error(w, "this fake answers nothing", http.StatusTeapot)
	}))
	t.Cleanup(server.Close)
	if res := s.doAs(t, tierA("operator"), http.MethodPatch, "/config",
		`{"integrations":{"github":{"enabled":true,"url":"`+server.URL+`","webhook_secret":"org",`+
			`"provisioning":{"org":"acme"}}}}`,
		map[string]string{"Content-Type": "application/merge-patch+json", "X-Summary": "github"},
	); res.Code != http.StatusCreated {
		t.Fatalf("point at github = %d: %s", res.Code, res.Body)
	}
	// A STATE SEALED FOR A CREDENTIAL THE LIST DOES NOT NAME — begun on a
	// node whose Tier A said otherwise, or before a restart that changed it.
	state, err := s.setup.AppFlow().MintState("sre-lead", iam.ActorFor(tierA("alice")))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, err = s.setup.AppFlow().Complete(t.Context(), "one-time-code", state)
	var managed *configapi.ManagedError
	if !errors.As(err, &managed) {
		t.Fatalf("a callback begun by alice = %v, want the managed refusal", err)
	}
	// AND THE PERSON IS TOLD WHERE THE APP IS ADDED, not which token the
	// config surface refused.
	toldAs(t, "a managed document", err, "managed outside Crewlet")
	if asked.Load() != 0 {
		t.Error("the callback exchanged GitHub's one-time code before refusing")
	}
}

// THE LISTED WRITER'S OWN APP LANDS ON A MANAGED DOCUMENT, recorded as written
// by the credential that began it, which the sealed state carries — the
// callback carries none of its own, and the managed check at the callback is
// asked of that party.
func TestAGitHubAppIsRecordedAsTheOperatorWhoBeganIt(t *testing.T) {
	t.Parallel()
	s := newManagedSurface(t, nil, fixtureWebhookBase, "operator")
	writer := tierA("operator")
	if res := s.doAs(t, writer, http.MethodPut, "/config", githubAppsDoc,
		map[string]string{"X-Summary": "a company mid-rollout"}); res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	server := fakeGitHub(t, map[string]any{
		"id": 91, "slug": "acme-sre-lead", "name": "Acme sre-lead",
		"pem": "-----BEGIN RSA PRIVATE KEY-----\nk\n-----END RSA PRIVATE KEY-----",
	})
	if res := s.doAs(t, writer, http.MethodPatch, "/config",
		`{"integrations":{"github":{"enabled":true,"url":"`+server.URL+`","webhook_secret":"org",`+
			`"provisioning":{"org":"acme"}}}}`,
		map[string]string{"Content-Type": "application/merge-patch+json", "X-Summary": "github"},
	); res.Code != http.StatusCreated {
		t.Fatalf("point at github = %d: %s", res.Code, res.Body)
	}
	begin := decode(t, s.doAs(t, writer, http.MethodPost, "/setup/integrations/github/app",
		`{"seat":"sre-lead"}`, nil))
	state, _ := begin["state"].(string)
	if state == "" {
		t.Fatalf("the writer's begin minted no state: %v", begin)
	}
	if _, err := s.setup.AppFlow().Complete(t.Context(), "one-time-code", state); err != nil {
		t.Fatalf("the writer's callback: %v", err)
	}
	active := s.active(t)
	by := iam.ActorFor(writer)
	if active.CreatedBy != by.Name || active.OperatorID != by.OperatorID ||
		!strings.Contains(active.Summary, "GitHub App") {
		t.Errorf("the app's revision is by %q through %q (%q), want %+v, who began it",
			active.CreatedBy, active.OperatorID, active.Summary, by)
	}
}
