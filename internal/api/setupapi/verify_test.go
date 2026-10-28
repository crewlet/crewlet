package setupapi_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/atlassianhost"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
)

// inertProbes is a probe for every kind that answers works and asks nothing,
// for a case that is not about checking a credential.
func inertProbes() map[integration.Kind]setup.Probe {
	out := make(map[integration.Kind]setup.Probe, len(integration.Kinds))
	for _, kind := range integration.Kinds {
		out[kind] = func(context.Context, setup.Check) (setup.Verdict, error) {
			return setup.Works("inert"), nil
		}
	}
	return out
}

// seenCheck is what one probe call was handed, read out of the Value
// function while the call was live.
type seenCheck struct {
	field    string
	values   map[string]string
	declared atlassianhost.Deployment
}

// recordingProbe answers a fixed verdict and remembers every check.
type recordingProbe struct {
	mu      sync.Mutex
	verdict setup.Verdict
	err     error
	// read are the fields each call reads through Value.
	read  []string
	calls []seenCheck
}

func (p *recordingProbe) probe(_ context.Context, check setup.Check) (setup.Verdict, error) {
	seen := seenCheck{field: check.Field, values: map[string]string{}, declared: check.Declared}
	for _, field := range append([]string{check.Field}, p.read...) {
		seen.values[field] = check.Value(field)
	}
	p.mu.Lock()
	p.calls = append(p.calls, seen)
	p.mu.Unlock()
	return p.verdict, p.err
}

func (p *recordingProbe) seen(t *testing.T) seenCheck {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 1 {
		t.Fatalf("the probe was asked %d times, want once", len(p.calls))
	}
	return p.calls[0]
}

// withProbe rebuilds the surface's service with probe answering kind and the
// inert one answering every other.
func (s *surface) withProbe(t *testing.T, kind integration.Kind, probe setup.Probe) {
	t.Helper()
	probes := inertProbes()
	probes[kind] = probe
	s.mux = http.NewServeMux()
	s.setup = newService(t, setupapi.Options{
		Config: s.config, Secrets: s.vault,
		Resolve:        s.vault.get,
		Status:         s.status,
		Probes:         probes,
		Now:            func() time.Time { return pinned },
		WebhookBase:    s.webhookBase,
		WebhookBaseKey: s.webhookBaseKey,
	})
	mount(t, s.mux, s.setup, s.config)
}

// snapshot is every value the vault holds.
func (v *vault) snapshot() map[string]string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return maps.Clone(v.values)
}

func (v *vault) put(t *testing.T, name, value string) {
	t.Helper()
	if err := v.Set(t.Context(), name, value, testAuthor, secrets.SourceAPI, pinned); err != nil {
		t.Fatal(err)
	}
}

// datadogDoc is a company that has connected Datadog in the EU region, with
// both keys in the sealed store.
const datadogDoc = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {
    "datadog": {"enabled": true, "route_to": "sre-lead", "webhook_token": "${DATADOG_WEBHOOK_TOKEN}",
                "provisioning": {"site": "datadoghq.eu", "api_key": "${DATADOG_API_KEY}",
                                 "app_key": "${DATADOG_APP_KEY}"}}
  },
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu"},
    {"name": "CTO", "handle": "cto", "llm": "zulu"}
  ]
}`

func verifyBody(field, value string, extra string) string {
	body := `{"field": "` + field + `", "value": "` + value + `"`
	if extra != "" {
		body += ", " + extra
	}
	return body + "}"
}

// THE PROBE'S VERDICT IS THE ANSWER, whole, as a 200: a refusal and an
// outage at the vendor are what the check found, not a failure of the route.
func TestAVerifyAnswersTheProbesVerdict(t *testing.T) {
	t.Parallel()
	for _, want := range []setup.Verdict{
		setup.Works("Works. Datadog accepted it for the Acme organization."),
		{Outcome: setup.VerifyRefused, Detail: "Datadog refused it: Forbidden."},
		{Outcome: setup.VerifyUnreachable, Detail: "Datadog did not answer. Nothing was stored."},
	} {
		t.Run(string(want.Outcome), func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t)
			probe := &recordingProbe{verdict: want}
			s.withProbe(t, integration.KindDatadog, probe.probe)
			res := s.do(t, http.MethodPost, "/setup/integrations/datadog/verify",
				verifyBody("api_key", "dd-api", `"values": {"app_key": "dd-app"}`), nil)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", res.Code, res.Body)
			}
			got := decode(t, res)
			if got["outcome"] != string(want.Outcome) || got["detail"] != want.Detail {
				t.Errorf("answered %v, want %+v", got, want)
			}
			if len(got) != 2 {
				t.Errorf("the answer carries more than outcome and detail: %v", got)
			}
		})
	}
}

// NOTHING IS WRITTEN. Not the value, not a pointer, not a revision and not a
// reconcile finding: a check is a read the vendor answers.
func TestAVerifyWritesNothing(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedDocument(t, datadogDoc)
	s.vault.put(t, "DATADOG_APP_KEY", "dd-app")
	probe := &recordingProbe{verdict: setup.Works("Works.")}
	s.withProbe(t, integration.KindDatadog, probe.probe)

	before, err := s.configs.List(t.Context(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	active := s.active(t)
	sealed := s.vault.snapshot()

	for _, body := range []string{
		verifyBody("api_key", "a-new-key", ""),
		verifyBody("webhook_token", "a-typed-token", ""),
	} {
		if res := s.do(t, http.MethodPost, "/setup/integrations/datadog/verify", body, nil); res.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", res.Code, res.Body)
		}
	}

	after, err := s.configs.List(t.Context(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("a check stored a revision: %d before, %d after", len(before), len(after))
	}
	if still := s.active(t); still.ID != active.ID {
		t.Errorf("a check moved the fleet's revision from %s to %s", active.ID, still.ID)
	}
	if got := s.vault.snapshot(); !maps.Equal(got, sealed) {
		t.Errorf("a check changed the sealed store: %v, was %v", got, sealed)
	}
	if _, recorded := s.status.get(integration.KindDatadog); recorded {
		t.Error("a check wrote a reconcile row")
	}
	// AND THE CHECKED VALUE NEVER COMES BACK.
	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/verify",
		verifyBody("api_key", "a-new-key", ""), nil)
	if strings.Contains(res.Body.String(), "a-new-key") {
		t.Errorf("the answer carries the checked value: %s", res.Body)
	}
}

// EVERY FIELD THE PERSON DID NOT TYPE IS READ THE WAY THE WRITE WOULD READ IT:
// what the document holds, resolved through the store, else the default. A
// key changed on a working integration is checked against the region and the
// other key already in force.
func TestAVerifyReadsTheStoredValueOfAnUntypedField(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedDocument(t, datadogDoc)
	s.vault.put(t, "DATADOG_APP_KEY", "stored-app-key")
	probe := &recordingProbe{verdict: setup.Works("Works."), read: []string{"site", "app_key"}}
	s.withProbe(t, integration.KindDatadog, probe.probe)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/verify",
		verifyBody("api_key", "typed-api-key", ""), nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	seen := probe.seen(t)
	for field, want := range map[string]string{
		"api_key": "typed-api-key", "site": "datadoghq.eu", "app_key": "stored-app-key",
	} {
		if seen.values[field] != want {
			t.Errorf("Value(%q) = %q, want %q", field, seen.values[field], want)
		}
	}

	// AND A TYPED VALUE WINS over the stored one.
	typed := newSurface(t)
	typed.seedDocument(t, datadogDoc)
	typed.vault.put(t, "DATADOG_APP_KEY", "stored-app-key")
	again := &recordingProbe{verdict: setup.Works("Works."), read: []string{"site", "app_key"}}
	typed.withProbe(t, integration.KindDatadog, again.probe)
	typed.do(t, http.MethodPost, "/setup/integrations/datadog/verify",
		verifyBody("api_key", "typed-api-key", `"values": {"site": "us5.datadoghq.com", "app_key": "typed-app"}`), nil)
	if got := again.seen(t).values; got["site"] != "us5.datadoghq.com" || got["app_key"] != "typed-app" {
		t.Errorf("the typed values were not what was checked: %v", got)
	}
}

// AND WHERE THE DOCUMENT HOLDS NOTHING, THE FIELD'S DEFAULT: Datadog's region
// is offered as US1, so a first connect is checked there.
func TestAVerifyFallsBackToTheFieldsDefault(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	probe := &recordingProbe{verdict: setup.Works("Works."), read: []string{"site"}}
	s.withProbe(t, integration.KindDatadog, probe.probe)
	s.do(t, http.MethodPost, "/setup/integrations/datadog/verify",
		verifyBody("api_key", "typed", ""), nil)
	if got := probe.seen(t).values["site"]; got != "datadoghq.com" {
		t.Errorf("site = %q, want the field's default", got)
	}
}

// A WHOLE `${NAME}` IS RESOLVED through this node's chain before it is
// checked, and one naming nothing is the request's fault, named: checked as
// its literal text, the vendor would refuse it and the person would rotate a
// key that may be fine.
func TestAVerifyResolvesAReferenceAndRefusesOneNamingNothing(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	s.vault.put(t, "SPARE_DATADOG_KEY", "the-spare-key")
	probe := &recordingProbe{verdict: setup.Works("Works."), read: []string{"app_key"}}
	s.withProbe(t, integration.KindDatadog, probe.probe)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/verify",
		verifyBody("api_key", "${SPARE_DATADOG_KEY}", `"values": {"app_key": "  ${SPARE_DATADOG_KEY} "}`), nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	if got := probe.seen(t).values; got["api_key"] != "the-spare-key" || got["app_key"] != "the-spare-key" {
		t.Errorf("the references were not resolved: %v", got)
	}

	missing := s.do(t, http.MethodPost, "/setup/integrations/datadog/verify",
		verifyBody("api_key", "${NO_SUCH_SECRET}", ""), nil)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", missing.Code, missing.Body)
	}
	body := decode(t, missing)
	if body["error"] != string(httpjson.CodeInvalidInput) ||
		body["detail"] != "There is no secret NO_SUCH_SECRET." {
		t.Errorf("answered %v", body)
	}
}

// A FIELD NOTHING CAN CHECK AHEAD answers its own sentence, and no vendor is
// asked: a signing secret is proved by the first delivery the vendor signs.
func TestAnUncheckableFieldAnswersItsNoteAndAsksNoVendor(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	probe := &recordingProbe{verdict: setup.Works("Works.")}
	s.withProbe(t, integration.KindSlack, probe.probe)

	res := s.do(t, http.MethodPost, "/setup/integrations/slack/verify",
		verifyBody("signing_secret", "a-typed-secret", `"seat": "sre-lead"`), nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	got := decode(t, res)
	if got["outcome"] != string(setup.VerifyUnchecked) ||
		got["detail"] != "Slack proves it with the first event it signs for this agent." {
		t.Errorf("answered %v", got)
	}
	if len(probe.calls) != 0 {
		t.Errorf("a vendor was asked about a field it cannot check: %+v", probe.calls)
	}

	// AND A SEAT'S CHECKABLE FIELD IS ASKED OF THE PROBE.
	res = s.do(t, http.MethodPost, "/setup/integrations/slack/verify",
		verifyBody("bot_token", "xoxb-typed", `"seat": "sre-lead"`), nil)
	if res.Code != http.StatusOK || decode(t, res)["outcome"] != string(setup.VerifyWorks) {
		t.Fatalf("a seat's bot token: %d %s", res.Code, res.Body)
	}
	if seen := probe.seen(t); seen.field != "bot_token" || seen.values["bot_token"] != "xoxb-typed" {
		t.Errorf("the probe was handed %+v", seen)
	}
}

// THE COMPANY'S ATLASSIAN DECLARATION TRAVELS WITH THE CHECK, because Jira's
// and Confluence's REST version and credential scheme follow it.
func TestAVerifyCarriesTheDeclaredAtlassianDeployment(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedDocument(t, `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {
    "atlassian": {"deployment": "data_center"},
    "jira": {"url": "https://jira.acme.internal", "token": "${JIRA_TOKEN}",
             "webhook_secret": "${JIRA_WEBHOOK_SECRET}"}
  },
  "roles": [{"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu"}]
}`)
	probe := &recordingProbe{verdict: setup.Works("Works."), read: []string{"url"}}
	s.withProbe(t, integration.KindJira, probe.probe)
	res := s.do(t, http.MethodPost, "/setup/integrations/jira/verify",
		verifyBody("token", "a-pat", ""), nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	seen := probe.seen(t)
	if seen.declared != atlassianhost.DataCenter || seen.values["url"] != "https://jira.acme.internal" {
		t.Errorf("the probe was handed %+v", seen)
	}
}

// THE REQUEST'S OWN FAULTS, each refused by name before any vendor is asked.
func TestAVerifyRefusesWhatItCannotCheck(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		path   string
		body   string
		status int
		code   httpjson.Code
	}{
		{"an unknown kind", "/setup/integrations/zendesk/verify",
			verifyBody("token", "x", ""), http.StatusNotFound, httpjson.CodeUnknownKind},
		{"a field the kind does not take", "/setup/integrations/datadog/verify",
			verifyBody("password", "x", ""), http.StatusBadRequest, httpjson.CodeInvalidInput},
		{"a field that is not a credential", "/setup/integrations/datadog/verify",
			verifyBody("site", "datadoghq.eu", ""), http.StatusBadRequest, httpjson.CodeInvalidInput},
		{"an empty value", "/setup/integrations/datadog/verify",
			verifyBody("api_key", "  ", ""), http.StatusBadRequest, httpjson.CodeInvalidInput},
		{"no field", "/setup/integrations/datadog/verify",
			`{"value": "x"}`, http.StatusBadRequest, httpjson.CodeInvalidInput},
		{"a form value the kind does not take", "/setup/integrations/datadog/verify",
			verifyBody("api_key", "x", `"values": {"region": "eu"}`),
			http.StatusBadRequest, httpjson.CodeInvalidInput},
		{"a per-seat field asked of the company", "/setup/integrations/slack/verify",
			verifyBody("bot_token", "xoxb-1", ""), http.StatusBadRequest, httpjson.CodeInvalidInput},
		{"a seat that takes no setup", "/setup/integrations/slack/verify",
			verifyBody("bot_token", "xoxb-1", `"seat": "nobody"`), http.StatusNotFound,
			httpjson.CodeInvalidInput},
		{"an unknown key", "/setup/integrations/datadog/verify",
			`{"field": "api_key", "value": "x", "store": true}`,
			http.StatusBadRequest, httpjson.CodeInvalidBody},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t)
			probe := &recordingProbe{verdict: setup.Works("Works.")}
			s.withProbe(t, integration.KindDatadog, probe.probe)
			res := s.do(t, http.MethodPost, c.path, c.body, nil)
			if res.Code != c.status {
				t.Fatalf("status = %d, want %d: %s", res.Code, c.status, res.Body)
			}
			if got := decode(t, res)["error"]; got != string(c.code) {
				t.Errorf("error = %v, want %s", got, c.code)
			}
			if len(probe.calls) != 0 {
				t.Errorf("a vendor was asked about a refused request: %+v", probe.calls)
			}
		})
	}
}

// NO COMPANY, NO CHECK: the form a check is made from is the active
// document's, and there is none.
func TestAVerifyWithNoActiveRevisionIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/verify",
		verifyBody("api_key", "x", ""), nil)
	if res.Code != http.StatusConflict ||
		decode(t, res)["error"] != string(httpjson.CodeNoActiveRevision) {
		t.Fatalf("answered %d %s, want 409 no_active_revision", res.Code, res.Body)
	}
}

// WHAT THE ENGINE DECIDES, as distinct from what the vendor does: a check the
// inputs cannot make is the request's fault with the probe's own sentence, and
// a probe's fault or an outcome this build does not know is the engine's.
func TestAProbesOwnRefusalsAreAnsweredByWhoseFaultTheyAre(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		probe  *recordingProbe
		status int
		detail string
	}{
		{"unusable input", &recordingProbe{err: setup.Unusable(
			"Datadog checks the API key and the application key together.")},
			http.StatusBadRequest, "Datadog checks the API key and the application key together."},
		{"a wiring fault", &recordingProbe{err: setup.NotCheckable("Datadog", "api_key")},
			http.StatusInternalServerError, ""},
		{"an engine fault", &recordingProbe{err: errors.New("boom")},
			http.StatusInternalServerError, ""},
		{"an unknown outcome", &recordingProbe{verdict: setup.Verdict{Outcome: "maybe"}},
			http.StatusInternalServerError, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t)
			s.withProbe(t, integration.KindDatadog, c.probe.probe)
			res := s.do(t, http.MethodPost, "/setup/integrations/datadog/verify",
				verifyBody("api_key", "x", ""), nil)
			if res.Code != c.status {
				t.Fatalf("status = %d, want %d: %s", res.Code, c.status, res.Body)
			}
			if c.detail != "" && decode(t, res)["detail"] != c.detail {
				t.Errorf("detail = %v, want %q", decode(t, res)["detail"], c.detail)
			}
			if strings.Contains(res.Body.String(), "boom") {
				t.Errorf("an engine fault reached the caller: %s", res.Body)
			}
		})
	}
}

// A PROBE SET MISSING A KIND IS REFUSED BY NAME at construction, because the
// Test button on that integration's form would otherwise answer 500.
func TestNewRefusesAProbeSetMissingAKind(t *testing.T) {
	t.Parallel()
	probes := inertProbes()
	delete(probes, integration.KindGitLab)
	cfg := newConfigSurface(t).svc
	v := &vault{}
	_, err := setupapi.New(setupapi.Options{
		Config: cfg, Secrets: v, Resolve: v.get,
		Passes: setup.NewRunner(nil, nil, nil), Probes: probes,
		Sink:   func(iam.Actor) (provision.TokenSink, error) { return nil, nil },
		Status: &statusStore{}, SlackApps: func() map[string]string { return nil },
		StateCipher: testCipher(t), StateClaims: coordmemory.NewFleet(),
		WebhookBaseKey: oneListener,
	})
	if err == nil || !strings.Contains(err.Error(), "gitlab") {
		t.Fatalf("a probe set without gitlab built a surface: %v", err)
	}
}

// EVERY CREDENTIAL A FORM SHOWS IS EITHER CHECKABLE OR SAYS WHEN IT IS
// PROVED. A secret that is neither is a field whose Test button answers a
// blank sentence, and the form cannot tell the person when to expect an
// answer. Walked over what the surface SERVES, for both Atlassian
// deployments (each drops the other's fields), so a field added to any
// vendor's list is held to the same line.
func TestEveryCredentialIsCheckableOrSaysWhenItIsProved(t *testing.T) {
	t.Parallel()
	dataCenter := strings.Replace(companyDoc, `"providers"`,
		`"integrations": {"atlassian": {"deployment": "data_center"}}, "providers"`, 1)
	seen := 0
	for _, doc := range []string{companyDoc, dataCenter} {
		s := newSurface(t)
		s.seedDocument(t, doc)
		for _, kind := range integration.Kinds {
			res := s.do(t, http.MethodGet, "/setup/integrations/"+string(kind), "", nil)
			if res.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", kind, res.Code, res.Body)
			}
			state := decode(t, res)
			lists := [][]any{}
			if rows, ok := state["requirements"].([]any); ok {
				lists = append(lists, rows)
			}
			seats, _ := state["seats"].([]any)
			for _, row := range seats {
				seat, _ := row.(map[string]any)
				if rows, ok := seat["requirements"].([]any); ok {
					lists = append(lists, rows)
				}
			}
			for _, rows := range lists {
				for _, row := range rows {
					r, _ := row.(map[string]any)
					if r["kind"] != string(setup.KindSecret) || r["hidden"] == true {
						continue
					}
					seen++
					checkable := r["checkable"] == true
					note, _ := r["check_note"].(string)
					switch {
					case checkable && note != "":
						t.Errorf("%s.%s is checkable and carries a note saying it is not",
							kind, r["field"])
					case !checkable && strings.TrimSpace(note) == "":
						t.Errorf("%s.%s is neither checkable nor says when it is proved",
							kind, r["field"])
					}
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no credential was walked, so this certified nothing")
	}
}
