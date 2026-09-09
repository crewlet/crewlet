package jira_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
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
	// onLookup, when set, runs on each /myself request OUTSIDE the
	// instance lock, so a test can observe how many are in flight at once.
	onLookup func()
	// projects the instance has.
	projects map[string]string
	// projectStatus makes one key answer a chosen status instead, which is
	// how a read that FAILED is expressed: a 404 is the instance
	// answering, and everything else is it failing to.
	projectStatus map[string]int
	hooks         []map[string]any
	created       []map[string]any
	updated       []map[string]any
	deleted       []string
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

// account reads one credential's identity, holding the lock only for the read.
func (i *instance) account(auth string) (string, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	account, ok := i.accounts[auth]
	return account, ok
}

func (i *instance) serve(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := req.URL.Path

	// /myself is served WITHOUT the instance lock held for the whole
	// handler, because the seat walk is the one concurrent caller here: a
	// handler that serialises every request makes the walk look sequential
	// no matter what it does, so a test of the fan-out's bound could not
	// fail. Everything below is provisioning, which is sequential anyway.
	if strings.HasSuffix(path, "/myself") {
		account, ok := i.account(req.Header.Get("Authorization"))
		if i.onLookup != nil {
			i.onLookup()
		}
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errorMessages":["Client must be authenticated"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"accountId":"` + account + `"}`))
		return
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	switch {
	case strings.Contains(path, "/project/"):
		key := path[strings.LastIndex(path, "/")+1:]
		if status, refuse := i.projectStatus[key]; refuse {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"errorMessages":["nope"]}`))
			return
		}
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
			// THE HOOK'S OWN NAME, because the name is what says whose a
			// hook is. It was hardcoded to crewlet, so this fake could not
			// express somebody else's hook at all and agreed with any
			// matching rule it was asked about.
			name, _ := hook["name"].(string)
			if name == "" {
				name = "crewlet"
			}
			// AND ITS EVENTS, for the same reason: a hook that listed
			// none could not express a CONVERGED registration at all, so
			// a fake with no events agreed that every hook needed
			// rewriting.
			events, _ := hook["events"].([]string)
			if events == nil {
				events = jira.WebhookEvents
			}
			encoded, err := json.Marshal(events)
			if err != nil {
				panic(err)
			}
			body += `{"self":"` + i.URL + `/rest/webhooks/1.0/webhook/` +
				hook["id"].(string) + `","name":"` + name + `","url":"` +
				hook["url"].(string) + `","enabled":true,"events":` +
				string(encoded) + `}`
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
	// A PROJECT THE INSTANCE ANSWERED 404 FOR. A read that FAILED does not
	// reach here at all — it raises, so the loop records it as this
	// surface's fault and retries — which is what tells a company document
	// naming a project that does not exist from an instance that timed
	// out. They want opposite treatment: one is a document an operator
	// must fix, and the other is worth another look in thirty seconds.
	if got := byKey["OPZ"]; got.Exists {
		t.Errorf("a project the instance does not have was not reported as "+
			"absent: %+v", got)
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
	if res.Hooked == "" || len(inst.created) != 0 {
		t.Fatalf("created %v, updated %v", inst.created, inst.updated)
	}
	if sink.value("JIRA_WEBHOOK_SECRET") != "" {
		t.Error("a working secret was re-minted into the sink")
	}
	// AND THE CONVERGED HOOK IS NOT REWRITTEN EITHER. This pass runs every
	// few minutes for the life of the deployment, and an unconditional
	// update was one write per pass, for ever, on a hook that already
	// carried the right address, the right events and a secret this run
	// did not change.
	if len(inst.updated) != 0 {
		t.Errorf("a converged hook was rewritten: %v", inst.updated)
	}
}

// AND A HOOK THAT IS NOT ALREADY CORRECT IS STILL WRITTEN, or the rule above
// would be a way of never converging anything. A disabled hook, or one
// missing an event the parser reads, delivers nothing an operator will miss.
func TestAHookMissingAnEventIsRewritten(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.hooks = []map[string]any{{
		"id":  "7",
		"url": "https://engine.example.com/webhooks/jira",
		// Every event but the comments, which is how a hook somebody
		// edited by hand arrives.
		"events": []string{"jira:issue_created", "jira:issue_updated"},
	}}

	if _, err := run(t, inst, func(opts *jira.Options) {
		opts.Sink = newSink()
		opts.WebhookBase = "https://engine.example.com"
		opts.Value = func(v string) string {
			if v == "${JIRA_WEBHOOK_SECRET}" {
				return "the-live-secret"
			}
			return v
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(inst.updated) != 1 {
		t.Fatalf("a hook missing comment events was left alone: updated %v",
			inst.updated)
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

// A CHANGED ADDRESS MOVES THE HOOK, it does not add one.
//
// Matching on the URL made a hook this engine had registered at a DIFFERENT
// address invisible to it, so changing the public base URL created a second
// hook and left the first. A deployment behind a tunnel accumulated one per
// restart, all live, all delivering to addresses that no longer answer.
func TestAMovedBaseURLUpdatesTheHookRatherThanAddingOne(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.hooks = []map[string]any{
		{"id": "1", "name": "crewlet", "url": "https://the-old-tunnel.example.com/webhooks/jira"},
	}
	if _, err := run(t, inst, func(opts *jira.Options) {
		opts.WebhookBase = "https://the-new-tunnel.example.com"
		opts.Value = func(v string) string {
			if v == "${JIRA_WEBHOOK_SECRET}" {
				return "s"
			}
			return v
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(inst.created) != 0 {
		t.Errorf("a second hook was registered: %v", inst.created)
	}
	if len(inst.updated) != 1 {
		t.Fatalf("the hook was not moved: %v", inst.updated)
	}
	if got := inst.updated[0]["url"]; got != "https://the-new-tunnel.example.com/webhooks/jira" {
		t.Errorf("url = %v, want the address the company is reachable on now", got)
	}
}

// AND DUPLICATES THIS ENGINE ALREADY LEFT ARE CLEANED UP.
//
// Converging on the first match still left every hook a previous address had
// created: this engine's own name on three live registrations, two of them
// delivering to somewhere that no longer answers. Converged has to mean one.
func TestDuplicateHooksOfOurOwnAreRemoved(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.hooks = []map[string]any{
		{"id": "1", "name": "crewlet", "url": "https://one.example.com/webhooks/jira"},
		{"id": "2", "name": "crewlet", "url": "https://two.example.com/webhooks/jira"},
		{"id": "3", "name": "someone-else", "url": "https://theirs.example.com/hook"},
	}
	if _, err := run(t, inst, func(opts *jira.Options) {
		opts.WebhookBase = "https://now.example.com"
		opts.Value = func(v string) string {
			if v == "${JIRA_WEBHOOK_SECRET}" {
				return "s"
			}
			return v
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(inst.deleted) != 1 || inst.deleted[0] != "2" {
		t.Errorf("deleted = %v, want the duplicate and nothing else", inst.deleted)
	}
	if len(inst.updated) != 1 {
		t.Fatalf("the surviving hook was not moved: %v", inst.updated)
	}
	if got := inst.updated[0]["url"]; got != "https://now.example.com/webhooks/jira" {
		t.Errorf("url = %v", got)
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
		{"id": "1", "name": "someone-elses-integration",
			"url": "https://someone-else.example.com/hook"},
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
// CLOUD REGISTERS ITS OWN WEBHOOK, and this test is the inversion of the one
// it replaces.
//
// The reconcile used to skip registration on Cloud and tell the operator to
// install a Forge app, on the premise that "on Cloud a dynamic webhook
// belongs to an app, so this endpoint refuses an API token". That is true of
// /rest/api/3/webhook and false of /rest/webhooks/1.0/webhook, which is the
// endpoint this client actually calls. Verified against a live Cloud site:
// GET answers 200, POST answers 201 with isSigned true, and the app-only
// endpoint answers 403 "Only Connect and OAuth 2.0 apps can use this
// operation".
//
// The cost of the wrong premise was total: a Cloud company got no webhook at
// all from the command whose job is to register one.
func TestCloudRegistersItsOwnWebhook(t *testing.T) {
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
		Client:      client,
		Config:      &config.Jira{CloudID: "acme", Token: "t", WebhookSecret: "${JIRA_WEBHOOK_SECRET}"},
		Org:         company(),
		Value:       func(v string) string { return v },
		Sink:        newSink(),
		WebhookBase: "https://engine.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hooked != "https://engine.example.com/webhooks/jira" {
		t.Errorf("a Cloud run registered %q", res.Hooked)
	}
	if len(inst.created) != 1 {
		t.Fatalf("a Cloud run posted %d hooks, want 1: %v", len(inst.created), inst.created)
	}
	// AND IT IS SIGNED. The secret is the route's only credential, so a
	// hook registered without one is an endpoint that answers 503 to every
	// delivery it would otherwise have routed.
	if got, _ := inst.created[0]["secret"].(string); got == "" {
		t.Errorf("the Cloud hook was registered with no secret: %v", inst.created[0])
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

// TestTheSeatWalkIsBounded is the other half of the walk's concurrency
// contract: it fans out, and the fan-out has a ceiling.
//
// It ran with none. `crewlet jira provision` opened one HTTPS connection per
// credentialled seat, all at once, against one instance. internal/engine's
// three credential resolvers were bounded and this walk was not, which is
// exactly the drift provision.ResolveConcurrently exists to prevent.
func TestTheSeatWalkIsBounded(t *testing.T) {
	t.Parallel()
	// Comfortably more seats than the cap, so an unbounded walk is visible
	// rather than merely possible.
	const seats = provision.IdentityLookups * 4

	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"

	roles := make([]*org.Role, 0, seats+1)
	for i := range seats {
		token := fmt.Sprintf("bot%d-token", i)
		inst.accounts["Bearer "+token] = fmt.Sprintf("acct-%d", i)
		roles = append(roles, &org.Role{
			Name:           fmt.Sprintf("Engineer %d", i),
			DeclaredHandle: fmt.Sprintf("eng%d", i),
			MCPEnv:         map[string]map[string]string{"jira": {"JIRA_TOKEN": token}},
		})
	}
	// A SEAT WITH NO CREDENTIAL, so the index mapping is exercised: the
	// walk resolves a subset of the seats and each answer still has to land
	// in its own row.
	roles = append(roles, &org.Role{Name: "QA", DeclaredHandle: "qa"})

	var (
		mu    sync.Mutex
		inFlt int
		peak  int
	)
	inst.onLookup = func() {
		mu.Lock()
		inFlt++
		peak = max(peak, inFlt)
		mu.Unlock()

		// HELD, so the callers actually overlap. A hook that returns
		// immediately lets each lookup finish before the next begins, and
		// the peak then stays at one whether or not anything bounds it —
		// a test that cannot fail.
		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		inFlt--
		mu.Unlock()
	}

	company := &org.Organization{Name: "nimbus", Roles: roles}
	company.Normalize()
	res, err := run(t, inst, func(o *jira.Options) { o.Org = company })
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	gotPeak := peak
	mu.Unlock()
	if gotPeak > provision.IdentityLookups {
		t.Errorf("peak in-flight lookups = %d, want at most %d",
			gotPeak, provision.IdentityLookups)
	}

	// The bound must not have cost work: every credentialled seat still
	// resolves, to ITS OWN account rather than a neighbour's.
	byHandle := map[string]jira.SeatIdentity{}
	for _, seat := range res.Seats {
		byHandle[seat.Handle] = seat
	}
	for i := range seats {
		handle := fmt.Sprintf("eng%d", i)
		if want := fmt.Sprintf("acct-%d", i); byHandle[handle].Account != want {
			t.Fatalf("%s resolved to %q, want %q — an answer landed in the wrong row",
				handle, byHandle[handle].Account, want)
		}
	}
	if byHandle["qa"].Account != "" {
		t.Errorf("the seat with no credential resolved to %q", byHandle["qa"].Account)
	}
}

// THE FINDINGS ARE WHAT THE RECONCILE LOOP READS, so what the operator sees
// on the dashboard comes from here rather than from the CLI's own printout.
//
// A seat whose credential the instance refuses is the one finding this
// command exists to surface, and it must survive the trip into the shared
// vocabulary rather than being visible only to somebody running the
// subcommand and reading its output.
func TestFindingsReportASeatWithNoAccount(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.accounts["Bearer lead-token"] = acctLead

	res, err := run(t, inst, nil)
	if err != nil {
		t.Fatal(err)
	}
	findings := res.Findings()

	var swe *integration.Finding
	for i, f := range findings {
		if f.Subject == "swe" {
			swe = &findings[i]
		}
	}
	if swe == nil {
		t.Fatalf("no finding names the seat whose credential was refused: %+v", findings)
	}
	if swe.Kind != integration.FindingIdentityFailed {
		t.Errorf("kind is %q, want %q", swe.Kind, integration.FindingIdentityFailed)
	}
	if !strings.Contains(swe.Detail, "swe") {
		t.Errorf("detail %q does not name the seat", swe.Detail)
	}

	// And the report an operator reads names it too, rather than only the
	// findings list behind it.
	report := integration.Classify(findings)
	if report.Phase == integration.PhaseReady {
		t.Fatalf("a company with an unreachable seat classified ready: %+v", report)
	}
}

// A PROJECT THE INSTANCE DOES NOT HAVE is almost always a typo in the company
// document, and a silent one: the webhook arrives, the key matches no lead,
// and the issue reaches nobody.
func TestFindingsReportAProjectTheInstanceDoesNotHave(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.accounts["Bearer lead-token"] = acctLead
	inst.accounts["Bearer swe-token"] = "acct-swe"
	inst.accounts["Bearer qa-token"] = "acct-qa"
	// Deliberately no projects registered, so every declared key is absent.

	res, err := run(t, inst, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range res.Findings() {
		// NOT AN ACCESS TIER. This was FindingUnknownTier, whose own doc
		// defines it as an access tier the company names and the vendor
		// does not have, and whose fallback sentence says exactly that —
		// on a finding about a project key.
		if f.Kind == integration.FindingIngressBlocked && f.Subject != "integrations.public_base_url" {
			found = true
			if f.Subject == "" {
				t.Errorf("a missing project finding names no project: %+v", f)
			}
			// BOTH HALVES OF THE 404: Jira answers it for a project that
			// is not there and for one this credential may not browse,
			// and told only the first half an operator looks for a typo
			// in a key they can see in the UI.
			if !strings.Contains(f.Detail, "Browse Projects") {
				t.Errorf("the finding does not mention the permission half of "+
					"a Jira 404: %q", f.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("no finding reports a declared project the instance lacks: %+v",
			res.Findings())
	}
}

// A PROJECT READ THAT FAILED IS A FAULT, NOT A FINDING.
//
// It became FindingGrantPending — a kind whose closed-set doc says "nobody
// has to act; it resolves on its own" and whose actor is the PROVIDER — so a
// permanent 403 from a token without Browse Projects reported forever that
// somebody else was working on it. The pass returned nil, so State.LastError
// stayed empty and the refusal appeared on no surface at all.
func TestAProjectReadThatFailedIsRaisedRatherThanReported(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.accounts["Bearer lead-token"] = acctLead
	inst.accounts["Bearer swe-token"] = "acct-swe"
	inst.accounts["Bearer qa-token"] = "acct-qa"
	inst.projectStatus = map[string]int{"ENG": http.StatusForbidden}

	res, err := run(t, inst, nil)
	if err == nil {
		t.Fatalf("a refused project read was reported as a finding: %+v",
			res.Findings())
	}
	// AND ROUTED TO THE OPERATOR. A 403 is refused identically on every
	// later pass, so folding it in with the transport faults would report
	// "the engine is working on it" about the one thing that will never
	// happen on its own.
	if !errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("a 403 was not marked as a credential refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "ENG") {
		t.Errorf("the fault does not name the project it failed on: %v", err)
	}
}

// AND A 5XX IS NOT A CREDENTIAL REFUSAL, or every brief outage would send an
// operator to rotate a key that is fine.
func TestAProjectReadThatBrokeIsNotACredentialRefusal(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.accounts["Bearer lead-token"] = acctLead
	inst.accounts["Bearer swe-token"] = "acct-swe"
	inst.accounts["Bearer qa-token"] = "acct-qa"
	inst.projectStatus = map[string]int{"ENG": http.StatusInternalServerError}

	if _, err := run(t, inst, nil); err == nil {
		t.Fatal("a project read that broke was not raised")
	} else if errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("a 500 was reported as a refused credential: %v", err)
	}
}

// A DATA CENTER INSTANCE WITH NOWHERE TO DELIVER TO IS AN INGRESS BLOCK.
//
// The pass registers no hook, which used to be silence — on the reasoning
// that the public base "is not on the integrations block" and ingress belonged
// to the subcommand that had it. It IS on the block, as
// integrations.public_base_url, and the reconcile loop feeds it into every
// pass, so the silence meant a company that never set it saw Jira reported
// Ready while the instance had no address to send anything to.
func TestADataCenterInstanceWithNoPublicBaseReportsIngressBlocked(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.accounts["Bearer lead-token"] = acctLead
	inst.accounts["Bearer swe-token"] = "acct-swe"
	inst.accounts["Bearer qa-token"] = "acct-qa"
	inst.projects["ENG"] = "Engineering"
	inst.projects["QA"] = "Quality"

	res, err := run(t, inst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Hooked != "" {
		t.Fatalf("the premise is wrong: this run registered %q", res.Hooked)
	}
	var found bool
	for _, f := range res.Findings() {
		if f.Kind == integration.FindingIngressBlocked {
			found = true
			if f.Subject != "integrations.public_base_url" {
				t.Errorf("the finding names %q rather than the field to set",
					f.Subject)
			}
		}
	}
	if !found {
		t.Fatalf("an instance with no address to deliver to reported no ingress "+
			"problem: %+v", res.Findings())
	}
}

// AND A CLOUD COMPANY IS STILL SILENT, which is the half that has to survive
// the rule above. A Cloud webhook belongs to an app rather than to an API
// token, and those events arrive through the Forge relay at an address this
// engine never registered — so an empty Hooked there is a working company,
// and reporting it would park one on a block nobody can clear.
func TestACloudCompanySaysNothingAboutIngress(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.accounts["Bearer lead-token"] = acctLead
	inst.accounts["Bearer swe-token"] = "acct-swe"
	inst.accounts["Bearer qa-token"] = "acct-qa"
	inst.projects["ENG"] = "Engineering"
	inst.projects["QA"] = "Quality"

	res, err := run(t, inst, func(o *jira.Options) {
		client, err := jira.NewClient(jira.ClientOptions{
			URL: inst.URL, Token: "org-token", Deployment: jira.Cloud,
		})
		if err != nil {
			t.Fatal(err)
		}
		o.Client = client
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Findings() {
		if f.Kind == integration.FindingIngressBlocked {
			t.Fatalf("a Cloud company reported ingress blocked: %+v", f)
		}
	}
}

// THE ENGINE'S HOOK IS FOUND BY NAME, WHEREVER IT POINTS.
//
// Matching on the address meant a deployment that changed its public base
// created a second hook and left the first: one live orphan per change, each
// enabled, each delivering to somewhere that no longer answers.
func TestAHookRegisteredAtAnOldAddressIsRepointed(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.hooks = []map[string]any{
		{"id": "7", "name": "crewlet", "url": "https://old-tunnel.example.com/webhooks/jira"},
	}

	res, err := run(t, inst, func(opts *jira.Options) {
		opts.Sink = newSink()
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
	if res.Hooked != "https://engine.example.com/webhooks/jira" {
		t.Fatalf("Hooked = %q", res.Hooked)
	}
	if len(inst.created) != 0 {
		t.Errorf("a second hook was created beside the one that moved: %v",
			inst.created)
	}
	if len(inst.updated) != 1 || inst.updated[0]["id"] != "7" {
		t.Errorf("the hook at the old address was not repointed: %v", inst.updated)
	}
}

// AND A HOOK THIS ENGINE NEVER REGISTERED IS NOT ADOPTED.
//
// The name alone would take one: an instance carries hooks other integrations
// registered, and a run that repointed the first thing sharing a name would
// take down somebody else's. Every hook this engine registers ends in
// /webhooks/jira whatever base carries it, so the path is the guard.
func TestAHookSharingTheNameButNotThePathIsLeftAlone(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.hooks = []map[string]any{
		{"id": "7", "name": "crewlet", "url": "https://elsewhere.example.com/their/hook"},
	}

	if _, err := run(t, inst, func(opts *jira.Options) {
		opts.Sink = newSink()
		opts.WebhookBase = "https://engine.example.com"
		opts.Value = func(v string) string {
			if v == "${JIRA_WEBHOOK_SECRET}" {
				return "the-live-secret"
			}
			return v
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(inst.updated) != 0 || len(inst.deleted) != 0 {
		t.Errorf("somebody else's hook was updated %v / deleted %v",
			inst.updated, inst.deleted)
	}
	if len(inst.created) != 1 {
		t.Errorf("this engine did not register its own hook: %v", inst.created)
	}
}

// TWO DEPLOYMENTS WATCHING ONE INSTANCE SET TWO NAMES, and each then
// converges its own hook. With one name each pass repointed the other's and
// only the last deployment to run received anything.
func TestADeploymentWithItsOwnHookNameKeepsItsOwnHook(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.hooks = []map[string]any{
		{"id": "1", "name": "crewlet", "url": "https://prod.example.com/webhooks/jira"},
		{"id": "2", "name": "crewlet-staging", "url": "https://old-staging/webhooks/jira"},
	}

	if _, err := run(t, inst, func(opts *jira.Options) {
		opts.Sink = newSink()
		opts.WebhookBase = "https://staging.example.com"
		opts.Config = &config.Jira{
			URL: inst.URL, Token: "${JIRA_TOKEN}",
			WebhookSecret: "${JIRA_WEBHOOK_SECRET}", WebhookName: "crewlet-staging",
		}
		opts.Value = func(v string) string {
			if v == "${JIRA_WEBHOOK_SECRET}" {
				return "the-live-secret"
			}
			return v
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(inst.updated) != 1 || inst.updated[0]["id"] != "2" {
		t.Fatalf("staging did not converge its own hook: %v", inst.updated)
	}
	if len(inst.deleted) != 0 {
		t.Errorf("staging deleted %v, which is production's hook", inst.deleted)
	}
}

// The default here and the config model's must agree, or the hook the
// reconcile registers is not the one a company that named none is documented
// to get.
func TestTheDefaultHookNameMatchesTheConfigModel(t *testing.T) {
	t.Parallel()
	if got := (&config.Jira{}).WebhookNameOrDefault(); got != jira.DefaultWebhookName {
		t.Errorf("config says %q, jira says %q", got, jira.DefaultWebhookName)
	}
}
