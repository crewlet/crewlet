package jira_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/org"
)

// instance is a Jira the reconcile can be run against.
//
// Keyed on the CREDENTIAL for /myself, because that is the whole of what the
// seat walk learns: which account a token authenticates as.
type instance struct {
	mu sync.Mutex
	*httptest.Server

	// accounts maps an Authorization header to the account it is.
	accounts map[string]string
	// projects the instance has.
	projects map[string]string
	hooks    []map[string]any
	created  []map[string]any
	updated  []map[string]any
	deleted  []string
}

func newInstance(t *testing.T) *instance {
	t.Helper()
	inst := &instance{
		accounts: map[string]string{},
		projects: map[string]string{},
	}
	inst.Server = httptest.NewServer(http.HandlerFunc(inst.serve))
	t.Cleanup(inst.Close)
	return inst
}

func (i *instance) serve(w http.ResponseWriter, req *http.Request) {
	i.mu.Lock()
	defer i.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := req.URL.Path

	switch {
	case strings.HasSuffix(path, "/myself"):
		account, ok := i.accounts[req.Header.Get("Authorization")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errorMessages":["Client must be authenticated"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"accountId":"` + account + `"}`))

	case strings.Contains(path, "/project/"):
		key := path[strings.LastIndex(path, "/")+1:]
		name, ok := i.projects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorMessages":["No project could be found"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"1","key":"` + key + `","name":"` + name +
			`","lead":{"accountId":"` + acctLead + `","displayName":"Ana"}}`))

	case strings.HasPrefix(path, "/rest/webhooks/1.0/webhook"):
		i.serveHooks(w, req, path)

	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{}`))
	}
}

func (i *instance) serveHooks(w http.ResponseWriter, req *http.Request, path string) {
	id := strings.TrimPrefix(path, "/rest/webhooks/1.0/webhook")
	id = strings.TrimPrefix(id, "/")

	switch req.Method {
	case http.MethodGet:
		body := "["
		for n, hook := range i.hooks {
			if n > 0 {
				body += ","
			}
			body += `{"self":"` + i.URL + `/rest/webhooks/1.0/webhook/` +
				hook["id"].(string) + `","name":"crewlet","url":"` +
				hook["url"].(string) + `","enabled":true}`
		}
		_, _ = w.Write([]byte(body + "]"))
	case http.MethodPost:
		i.created = append(i.created, decode(req))
		_, _ = w.Write([]byte(`{"self":"` + i.URL + `/rest/webhooks/1.0/webhook/99"}`))
	case http.MethodPut:
		body := decode(req)
		body["id"] = id
		i.updated = append(i.updated, body)
		_, _ = w.Write([]byte(`{"self":"` + i.URL + `/rest/webhooks/1.0/webhook/` + id + `"}`))
	case http.MethodDelete:
		i.deleted = append(i.deleted, id)
		w.WriteHeader(http.StatusNoContent)
	}
}

func decode(req *http.Request) map[string]any {
	var body map[string]any
	_ = json.NewDecoder(req.Body).Decode(&body)
	return body
}

// company is an org with one lead, one seat holding a credential and one
// holding none, plus a human seat that must never be looked up as though it
// held one.
func company() *org.Organization {
	o := &org.Organization{
		Name: "nimbus",
		Roles: []*org.Role{
			// The lead's credential is under Atlassian's own combined
			// server name; the SWE's is under a Jira-only one. Both are
			// real configs, and a scan that knew one name would report
			// the other seat as holding nothing.
			{Name: "Eng Lead", DeclaredHandle: "lead", JiraProject: "ENG",
				MCPEnv: map[string]map[string]string{
					"atlassian": {"JIRA_API_TOKEN": "lead-token"},
				}},
			{Name: "SWE", DeclaredHandle: "swe",
				MCPEnv: map[string]map[string]string{
					"jira": {"JIRA_TOKEN": "swe-token"},
				}},
			{Name: "QA", DeclaredHandle: "qa"},
			{Name: "Founder", DeclaredHandle: "founder", Kind: org.KindHuman,
				Contact: &org.HumanContact{AtlassianAccountID: acctFounder}},
		},
	}
	o.Normalize()
	return o
}

func run(t *testing.T, inst *instance, mutate func(*jira.Options)) (*jira.Result, error) {
	t.Helper()
	client, err := jira.NewClient(jira.ClientOptions{
		URL: inst.URL, Token: "org-token", Deployment: jira.DataCenter,
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := jira.Options{
		Client: client,
		Config: &config.Jira{
			URL: inst.URL, Token: "${JIRA_TOKEN}", WebhookSecret: "${JIRA_WEBHOOK_SECRET}",
		},
		Org:   company(),
		Value: func(v string) string { return v },
		Sink:  newSink(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	return jira.Reconcile(context.Background(), opts)
}

// THE SEAT WALK IS THE POINT OF THE COMMAND.
//
// A seat with no account id receives nothing, and nothing else in the engine
// says so: its inbound routing is simply silent.
func TestTheReconcileReportsWhichSeatsCanBeReached(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.accounts["Bearer lead-token"] = acctLead
	// swe-token is a credential the instance refuses — a rotated token
	// that was never re-provisioned, which is the common case and the one
	// that must be visible.

	res, err := run(t, inst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Account != "acct-org" {
		t.Errorf("org account = %q", res.Account)
	}
	byHandle := map[string]jira.SeatIdentity{}
	for _, seat := range res.Seats {
		byHandle[seat.Handle] = seat
	}
	// The HUMAN seat is not in the walk: it holds no tool credential and
	// must never be looked up as though it did.
	if _, present := byHandle["founder"]; present {
		t.Error("a human seat was probed for a tracker credential")
	}
	if got := byHandle["lead"]; got.Account != acctLead {
		t.Errorf("the lead resolved to %q", got.Account)
	}
	if got := byHandle["swe"]; got.Routes() || got.Reason == "" {
		t.Errorf("a refused credential was reported as routing: %+v", got)
	}
	if got := byHandle["qa"]; got.Routes() || !strings.Contains(got.Reason, "mcp_env.atlassian") {
		t.Errorf("a seat with no credential was not told where one goes: %+v", got)
	}
	if res.Routing() != 1 {
		t.Errorf("routing seats = %d, want 1", res.Routing())
	}
}

// A DEAD ORG CREDENTIAL STOPS THE RUN, because nothing else it reported
// would be trustworthy.
func TestARefusedOrgCredentialFailsTheRun(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	if _, err := run(t, inst, nil); err == nil {
		t.Fatal("the run continued past a credential the instance refused")
	}
}

// EVERY PROJECT THE ORG NAMES IS CHECKED. A key with a typo in it is a
// routing gap that produces no error anywhere: the webhook arrives, the key
// matches no lead, and the issue reaches nobody.
func TestTheReconcileChecksEveryDeclaredProject(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.accounts["Bearer lead-token"] = acctLead
	inst.projects["ENG"] = "Engineering"

	o := company()
	o.Units = []*org.Unit{{Name: "Ops", JiraProject: "OPZ"}}
	o.Normalize()

	res, err := run(t, inst, func(opts *jira.Options) { opts.Org = o })
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]jira.ProjectCheck{}
	for _, p := range res.Projects {
		byKey[p.Key] = p
	}
	if len(byKey) != 2 {
		t.Fatalf("checked %v", res.Projects)
	}
	if got := byKey["OPZ"]; got.Exists || got.Detail == "" {
		t.Errorf("a project the instance does not have was reported fine: %+v", got)
	}
	// The lead's own project agrees: the org chart's lead IS the account
	// Jira calls the project lead.
	if got := byKey["ENG"]; !got.Exists || !got.Agrees() {
		t.Errorf("ENG = %+v", got)
	}
}

// A JIRA LEAD WHO IS NOT A SEAT HERE IS A FACT, NOT A FAULT.
//
// A human manager owning the project while an agent triages it is an
// ordinary arrangement, so the two ideas of ownership are reported side by
// side and never failed.
func TestALeadWhoIsNotASeatIsReportedNotRefused(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.projects["ENG"] = "Engineering"

	res, err := run(t, inst, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Projects[0]
	if !got.Exists || got.Agrees() {
		t.Fatalf("ENG = %+v", got)
	}
	if got.OrgLead != "lead" || got.JiraLead != acctLead {
		t.Errorf("the two owners were not both reported: %+v", got)
	}
}

// THE WEBHOOK IS REGISTERED WITH A MINTED SECRET, and the secret is recorded
// where the config points.
func TestTheWebhookIsRegisteredAndItsSecretRecorded(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	sink := newSink()

	res, err := run(t, inst, func(opts *jira.Options) {
		opts.Sink = sink
		opts.WebhookBase = "https://engine.example.com/"
		// The reference resolves to nothing, which is how a run says
		// "there is no secret yet".
		opts.Value = func(v string) string {
			if v == "${JIRA_WEBHOOK_SECRET}" {
				return ""
			}
			return v
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hooked != "https://engine.example.com/webhooks/jira" {
		t.Fatalf("hooked = %q", res.Hooked)
	}
	minted := sink.value("JIRA_WEBHOOK_SECRET")
	if minted == "" {
		t.Fatal("no secret was recorded, so the engine cannot verify a delivery")
	}
	if len(inst.created) != 1 || inst.created[0]["secret"] != minted {
		t.Fatalf("the hook was registered with %v, the sink holds %q",
			inst.created, minted)
	}
}

// A SECRET THAT ALREADY RESOLVES IS USED AS IT IS.
//
// The tempting shape is to mint every run, and it is an outage: the engine
// is running with the OLD secret, so re-registering with a fresh one makes
// the instance sign every delivery with a key nothing holds.
func TestARerunDoesNotReplaceAWorkingSecret(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.hooks = []map[string]any{
		{"id": "7", "url": "https://engine.example.com/webhooks/jira"},
	}
	sink := newSink()

	res, err := run(t, inst, func(opts *jira.Options) {
		opts.Sink = sink
		opts.WebhookBase = "https://engine.example.com"
		opts.Value = func(v string) string {
			if v == "${JIRA_WEBHOOK_SECRET}" {
				return "the-live-secret"
			}
			return v
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hooked == "" || len(inst.created) != 0 || len(inst.updated) != 1 {
		t.Fatalf("created %v, updated %v", inst.created, inst.updated)
	}
	if inst.updated[0]["secret"] != "the-live-secret" {
		t.Errorf("the live secret was replaced: %v", inst.updated[0])
	}
	if sink.value("JIRA_WEBHOOK_SECRET") != "" {
		t.Error("a working secret was re-minted into the sink")
	}
}

// RECREATING IS DESTRUCTIVE AND ASKED FOR: it is the only recovery for a
// secret that was lost, because the value cannot be read back off the hook.
func TestRecreatingTheWebhookMintsAFreshSecret(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.hooks = []map[string]any{
		{"id": "7", "url": "https://engine.example.com/webhooks/jira"},
	}
	sink := newSink()

	_, err := run(t, inst, func(opts *jira.Options) {
		opts.Sink = sink
		opts.WebhookBase = "https://engine.example.com"
		opts.RecreateWebhook = true
		opts.Value = func(v string) string {
			if v == "${JIRA_WEBHOOK_SECRET}" {
				return "the-live-secret"
			}
			return v
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inst.deleted) != 1 || inst.deleted[0] != "7" {
		t.Fatalf("the old hook was not removed: %v", inst.deleted)
	}
	minted := sink.value("JIRA_WEBHOOK_SECRET")
	if minted == "" || minted == "the-live-secret" {
		t.Fatalf("the secret was not rotated: %q", minted)
	}
}

// A FOREIGN HOOK IS NOT THIS RUN'S TO RECONFIGURE. An instance may carry
// hooks somebody else registered, and taking over the first one found would
// break an unrelated integration.
func TestAForeignHookIsLeftAlone(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.hooks = []map[string]any{
		{"id": "1", "url": "https://someone-else.example.com/hook"},
	}
	if _, err := run(t, inst, func(opts *jira.Options) {
		opts.WebhookBase = "https://engine.example.com"
		opts.Value = func(v string) string {
			if v == "${JIRA_WEBHOOK_SECRET}" {
				return "s"
			}
			return v
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(inst.updated) != 0 || len(inst.deleted) != 0 {
		t.Fatalf("a foreign hook was touched: updated %v deleted %v",
			inst.updated, inst.deleted)
	}
	if len(inst.created) != 1 {
		t.Fatalf("the engine's own hook was not registered: %v", inst.created)
	}
}

// CLOUD HAS NO WEBHOOK ENDPOINT FOR AN API TOKEN, and that is not something
// a better credential fixes: on Cloud a dynamic webhook belongs to an app.
// A run that reported a 403 there would send an operator to rotate a token
// that is fine.
func TestCloudSkipsWebhookRegistrationAndSaysWhy(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"

	client, err := jira.NewClient(jira.ClientOptions{
		URL: inst.URL, Token: "org-token", Deployment: jira.Cloud,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := jira.Reconcile(context.Background(), jira.Options{
		Client: client,
		Config: &config.Jira{CloudID: "acme", Token: "t"},
		Org:    company(),
		Value:  func(v string) string { return v },
		Sink:   newSink(),
		// A base IS given: the point is that Cloud skips anyway.
		WebhookBase: "https://engine.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hooked != "" {
		t.Errorf("a Cloud run claimed to register a hook: %q", res.Hooked)
	}
	if len(inst.created) != 0 {
		t.Errorf("a Cloud run posted to the hook endpoint: %v", inst.created)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "/webhooks/forge") {
		t.Errorf("the operator is not told where Cloud events arrive: %v", res.Notes)
	}
}

// WITHOUT A PUBLIC BASE NOTHING IS GUESSED. A hook pointing at the wrong
// host is worse than no hook: the instance then reports a healthy
// integration that delivers into the void.
func TestNoPublicURLRegistersNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"

	res, err := run(t, inst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Hooked != "" || len(inst.created) != 0 {
		t.Fatalf("a hook was registered with no base: %q %v", res.Hooked, inst.created)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "public base URL") {
		t.Errorf("the operator is not told why: %v", res.Notes)
	}
}

// A LITERAL WEBHOOK SECRET THAT RESOLVES TO NOTHING HAS NOWHERE TO MINT
// INTO, and the run says which of the two fixes to apply rather than
// half-configuring the instance.
func TestALiteralSecretWithNoValueIsRefused(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"

	_, err := run(t, inst, func(opts *jira.Options) {
		opts.WebhookBase = "https://engine.example.com"
		opts.Config = &config.Jira{URL: inst.URL, Token: "t", WebhookSecret: ""}
		opts.Value = func(string) string { return "" }
	})
	if err == nil || !strings.Contains(err.Error(), "webhook_secret") {
		t.Fatalf("err = %v", err)
	}
}

// sink is the reconcile's recorder.
type sink struct {
	mu     sync.Mutex
	values map[string]string
}

func newSink() *sink { return &sink{values: map[string]string{}} }

func (s *sink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = value
	return nil
}

func (s *sink) Discard(context.Context) error { return nil }
func (s *sink) Flush(context.Context) error   { return nil }

func (s *sink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[name], s.values[name] != "", nil
}

func (s *sink) Describe() string { return "a test sink" }
func (s *sink) NextStep() string { return "restart the engine" }

func (s *sink) value(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[name]
}
