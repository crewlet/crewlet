package jira_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/jira"
)

// recorder is an instance that records what it was asked.
type recorder struct {
	*httptest.Server
	paths   []string
	methods []string
	auth    []string
	bodies  []map[string]any
	reply   func(path string) (int, string)
}

func newRecorder(t *testing.T, reply func(path string) (int, string)) *recorder {
	t.Helper()
	r := &recorder{reply: reply}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.paths = append(r.paths, req.URL.Path)
		r.methods = append(r.methods, req.Method)
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.bodies = append(r.bodies, body)

		status, payload := r.reply(req.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(r.Close)
	return r
}

// THE DEPLOYMENT IS DERIVED FROM THE ADDRESS, because the honest answer is
// already in the address an operator gave — a knob for it would be a second
// place to get it wrong.
func TestTheDeploymentIsDerivedFromTheAddress(t *testing.T) {
	t.Parallel()
	for address, want := range map[string]jira.Deployment{
		"https://acme.atlassian.net":              jira.Cloud,
		"https://api.atlassian.com/ex/jira/cloud": jira.Cloud,
		"https://ACME.ATLASSIAN.NET/browse":       jira.Cloud,
		"https://jira.example.com":                jira.DataCenter,
		"https://jira.internal:8443":              jira.DataCenter,
		"not a url at all":                        jira.DataCenter,
	} {
		if got := jira.DeploymentOf(address); got != want {
			t.Errorf("%s → %s, want %s", address, got, want)
		}
	}
}

// THE TWO DEPLOYMENTS SPEAK DIFFERENT REST VERSIONS. v3 is a 404 on Data
// Center; v2 on Cloud is a deprecated surface that answers with different
// identity fields, which is a silent misroute rather than a loud miss.
func TestEachDeploymentCallsItsOwnRESTVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		deploy jira.Deployment
		want   string
	}{
		{jira.Cloud, "/rest/api/3/myself"},
		{jira.DataCenter, "/rest/api/2/myself"},
	} {
		srv := newRecorder(t, func(string) (int, string) {
			return 200, `{"accountId":"acct-1"}`
		})
		client, err := jira.NewClient(jira.ClientOptions{
			URL: srv.URL, Token: "t", Deployment: tc.deploy,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Me(context.Background()); err != nil {
			t.Fatal(err)
		}
		if srv.paths[0] != tc.want {
			t.Errorf("%s called %s, want %s", tc.deploy, srv.paths[0], tc.want)
		}
	}
}

// THE EMAIL IS THE WHOLE DISCRIMINATOR BETWEEN THE TWO AUTH SCHEMES.
//
// Cloud rejects a bare bearer API token and Data Center rejects Basic with
// an empty user: the same credential, refused purely on which header carried
// it.
func TestAnEmailSwitchesTheAuthenticationScheme(t *testing.T) {
	t.Parallel()
	basic := clientFor(t, jira.ClientOptions{Email: "ops@example.com", Token: "tok"})
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("ops@example.com:tok"))
	if basic.auth[0] != want {
		t.Errorf("with an email the header was %q", basic.auth[0])
	}
	bearer := clientFor(t, jira.ClientOptions{Token: "tok"})
	if bearer.auth[0] != "Bearer tok" {
		t.Errorf("with no email the header was %q", bearer.auth[0])
	}
}

// A CLIENT WITH NOTHING TO TALK TO IS REFUSED AT CONSTRUCTION, rather than
// pointing every call at "" and failing with a much less useful message.
func TestAClientNeedsAnAddressAndACredential(t *testing.T) {
	t.Parallel()
	if _, err := jira.NewClient(jira.ClientOptions{Token: "t"}); err == nil {
		t.Error("a client with no url was built")
	}
	if _, err := jira.NewClient(jira.ClientOptions{URL: "https://x"}); err == nil {
		t.Error("a client with no token was built")
	}
}

// WATCHERS COME BACK IN EITHER DEPLOYMENT'S IDENTITY FIELD.
func TestWatchersReadWhicheverIdentityTheInstanceUses(t *testing.T) {
	t.Parallel()
	srv := newRecorder(t, func(string) (int, string) {
		return 200, `{"watchers":[
			{"accountId":"acct-1","displayName":"Ana"},
			{"name":"bob","displayName":"Bob"},
			{"displayName":"Nobody"}]}`
	})
	client, err := jira.NewClient(jira.ClientOptions{URL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.WatchersOf(context.Background(), "ENG-42")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "acct-1,bob" {
		t.Fatalf("watchers = %v", got)
	}
	if !strings.HasSuffix(srv.paths[0], "/issue/ENG-42/watchers") {
		t.Errorf("called %s", srv.paths[0])
	}
}

// A REFUSAL IS TYPED, so a caller deciding what it MEANS does not
// substring-match a message whose wording differs by Jira version and locale.
func TestARefusalCarriesItsStatus(t *testing.T) {
	t.Parallel()
	srv := newRecorder(t, func(string) (int, string) {
		return 404, `{"errorMessages":["Issue does not exist"]}`
	})
	client, err := jira.NewClient(jira.ClientOptions{URL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.WatchersOf(context.Background(), "ENG-42")
	var apiErr *jira.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *jira.APIError", err)
	}
	if apiErr.Status != 404 || !strings.Contains(apiErr.Detail, "does not exist") {
		t.Errorf("error = %+v", apiErr)
	}
}

// A PROJECT READ CARRIES JIRA'S OWN IDEA OF WHO LEADS IT, which is what the
// reconcile compares against the org chart.
func TestAProjectReadReportsItsLead(t *testing.T) {
	t.Parallel()
	srv := newRecorder(t, func(string) (int, string) {
		return 200, `{"id":"10000","key":"ENG","name":"Engineering",
			"lead":{"accountId":"acct-lead","displayName":"Ana Ruiz"}}`
	})
	client, err := jira.NewClient(jira.ClientOptions{URL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ProjectOf(context.Background(), "ENG")
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "ENG" || got.Name != "Engineering" ||
		got.Lead != "acct-lead" || got.LeadName != "Ana Ruiz" {
		t.Fatalf("project = %+v", got)
	}
}

// THE HOOK ADMINISTRATION SURFACE IS NOT UNDER THE VERSIONED API TREE, and
// a client that put it there would 404 on every registration.
func TestWebhookAdministrationUsesItsOwnPrefix(t *testing.T) {
	t.Parallel()
	srv := newRecorder(t, func(string) (int, string) {
		return 200, `[{"self":"https://jira.example.com/rest/webhooks/1.0/webhook/7",
			"name":"crewlet","url":"https://engine.example.com/webhooks/jira",
			"events":["jira:issue_created"],"enabled":true}]`
	})
	client, err := jira.NewClient(jira.ClientOptions{URL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	hooks, err := client.Webhooks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if srv.paths[0] != "/rest/webhooks/1.0/webhook" {
		t.Fatalf("called %s", srv.paths[0])
	}
	// The id is only in `self` on Data Center, so it is parsed from there
	// rather than read from a field that does not exist.
	if len(hooks) != 1 || hooks[0].ID != "7" {
		t.Fatalf("hooks = %+v", hooks)
	}
}

// A REGISTERED HOOK CARRIES THE SECRET AND THE WHOLE BODY.
//
// excludeBody true delivers an event with no issue at all, which the parser
// reads as an event naming nobody — an integration that looks configured and
// routes nothing.
func TestARegisteredHookIsSignedAndCarriesItsBody(t *testing.T) {
	t.Parallel()
	srv := newRecorder(t, func(string) (int, string) {
		return 201, `{"self":"https://jira.example.com/rest/webhooks/1.0/webhook/9"}`
	})
	client, err := jira.NewClient(jira.ClientOptions{URL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateWebhook(context.Background(),
		"crewlet", "https://engine.example.com/webhooks/jira", "s3cret"); err != nil {
		t.Fatal(err)
	}
	body := srv.bodies[0]
	if body["secret"] != "s3cret" {
		t.Errorf("the hook was registered unsigned: %v", body)
	}
	if body["excludeBody"] != false {
		t.Errorf("the hook excludes its body: %v", body)
	}
	events, _ := body["events"].([]any)
	if len(events) != len(jira.WebhookEvents) {
		t.Errorf("the hook subscribes to %d events, the parser routes %d",
			len(events), len(jira.WebhookEvents))
	}
}

// THE HOOK SUBSCRIBES TO EXACTLY WHAT THE PARSER ROUTES. More is bandwidth
// and an audit row per irrelevant change; less is an event class that
// silently never arrives.
func TestTheHookSubscribesToWhatTheParserRoutes(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	for _, event := range jira.WebhookEvents {
		w := issue(event, acctLead, map[string]any{"assignee": person(acctSWE)},
			map[string]any{"comment": map[string]any{
				"author": person(acctLead), "body": "hello",
			}})
		copies, err := parser(t, nil).Parse(context.Background(), w, reg)
		if err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		if len(copies) == 0 {
			t.Errorf("the hook subscribes to %s and the parser routes none of it", event)
		}
	}
}

func clientFor(t *testing.T, opts jira.ClientOptions) *recorder {
	t.Helper()
	srv := newRecorder(t, func(string) (int, string) { return 200, `{"accountId":"a"}` })
	opts.URL = srv.URL
	client, err := jira.NewClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	return srv
}
