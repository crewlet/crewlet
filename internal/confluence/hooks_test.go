package confluence_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/integration"
)

// cloudSite is a Confluence Cloud webhook administration endpoint behaving
// exactly as the real one was measured to: it stores the URL verbatim, it
// swallows "secret" and every other field it does not know, it validates no
// event names, and it reports a hook's id only as the tail of `self`.
type cloudSite struct {
	mu sync.Mutex
	*httptest.Server
	hooks map[string]map[string]any
	next  int
	// refuse names an event whose registration this site rejects, so a
	// partial hook-up is representable.
	refuse string
	// writes counts every mutation, for the converged-pass-writes-nothing
	// property.
	writes int
}

func newCloudSite(t *testing.T) *cloudSite {
	t.Helper()
	s := &cloudSite{hooks: map[string]map[string]any{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *cloudSite) serve(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.Lock()
	defer s.mu.Unlock()

	path := req.URL.Path
	switch {
	case strings.HasSuffix(path, "/rest/api/user/current"):
		_, _ = w.Write([]byte(`{"accountId":"acct-org"}`))
	case strings.HasSuffix(path, "/rest/webhooks/1.0/webhook") && req.Method == http.MethodGet:
		out := make([]map[string]any, 0, len(s.hooks))
		for _, h := range s.hooks {
			out = append(out, h)
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasSuffix(path, "/rest/webhooks/1.0/webhook") && req.Method == http.MethodPost:
		var in map[string]any
		_ = json.NewDecoder(req.Body).Decode(&in)
		if events, ok := in["events"].([]any); ok && s.refuse != "" &&
			len(events) == 1 && events[0] == s.refuse {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"this token may not register that event"}`))
			return
		}
		s.writes++
		s.next++
		id := itoa(s.next)
		// STORED VERBATIM and every unknown field dropped, as measured.
		h := map[string]any{
			"name": in["name"], "url": in["url"], "events": in["events"],
			"enabled": true, "excludeBody": false, "filters": map[string]any{},
			"self": s.URL + "/wiki/rest/webhooks/1.0/webhook/" + id,
		}
		s.hooks[id] = h
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(h)
	case strings.Contains(path, "/rest/webhooks/1.0/webhook/") && req.Method == http.MethodPut:
		s.writes++
		id := path[strings.LastIndex(path, "/")+1:]
		var in map[string]any
		_ = json.NewDecoder(req.Body).Decode(&in)
		h := s.hooks[id]
		h["name"], h["url"], h["events"] = in["name"], in["url"], in["events"]
		// An update re-asserts enabled, which is what both writers send.
		h["enabled"] = true
		_ = json.NewEncoder(w).Encode(h)
	case strings.Contains(path, "/rest/webhooks/1.0/webhook/") && req.Method == http.MethodDelete:
		s.writes++
		delete(s.hooks, path[strings.LastIndex(path, "/")+1:])
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *cloudSite) urls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, h := range s.hooks {
		out = append(out, h["url"].(string))
	}
	return out
}

// disableAll turns every registered hook off, which is what an administrator
// clicking Disable at the instance does.
func (s *cloudSite) disableAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.hooks {
		h["enabled"] = false
	}
}

// snapshot is what the site currently holds, copied under the lock.
func (s *cloudSite) snapshot() map[string]map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]map[string]any, len(s.hooks))
	for id, h := range s.hooks {
		copied := make(map[string]any, len(h))
		for k, v := range h {
			copied[k] = v
		}
		out[id] = copied
	}
	return out
}

// seed puts a hook the engine did not register into the site.
func (s *cloudSite) seed(hook map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := itoa(s.next)
	hook["self"] = s.URL + "/rest/webhooks/1.0/webhook/" + id
	s.hooks[id] = hook
}

// retarget rewrites every hook's stored address, standing in for a site that
// kept what it was given verbatim.
func (s *cloudSite) retarget(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.hooks {
		h["url"] = url
	}
}

func (s *cloudSite) mutations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// recordingSink remembers what a run minted.
type recordingSink struct {
	mu   sync.Mutex
	vals map[string]string
}

func newSink() *recordingSink { return &recordingSink{vals: map[string]string{}} }

func (s *recordingSink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[name] = value
	return nil
}
func (s *recordingSink) Discard(context.Context) error { return nil }
func (s *recordingSink) Flush(context.Context) error   { return nil }
func (s *recordingSink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vals[name]
	return v, ok, nil
}
func (s *recordingSink) Describe() string { return "the test sink" }
func (s *recordingSink) NextStep() string { return "restart the test" }

// cloudClient builds a client the site will classify as Cloud, which is
// decided by the HOST rather than by anything the test server can answer.
// The httptest address is 127.0.0.1, so the deployment is forced.
func cloudClient(t *testing.T, site *cloudSite) *confluence.Client {
	t.Helper()
	c, err := confluence.NewClient(confluence.ClientOptions{
		URL: site.URL + "/wiki", Email: "org@example.com", Token: "org-token",
		Deployment: confluence.Cloud,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func run(t *testing.T, site *cloudSite, sink *recordingSink, env map[string]string) *confluence.Result {
	t.Helper()
	res, err := confluence.Reconcile(context.Background(), confluence.Options{
		Client: cloudClient(t, site),
		Config: &config.Confluence{URL: site.URL + "/wiki", Token: "t", WebhookToken: "${CONFLUENCE_WEBHOOK_TOKEN}"},
		Value: func(v string) string {
			if v == "${CONFLUENCE_WEBHOOK_TOKEN}" {
				return env["CONFLUENCE_WEBHOOK_TOKEN"]
			}
			return v
		},
		Sink:        sink,
		WebhookBase: "https://engine.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// ONE HOOK PER EVENT, because a Cloud payload names no event and the
// registered path is the only thing that knows which one fired.
func TestCloudRegistersOneHookPerEvent(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()

	res := run(t, site, sink, nil)

	if len(res.Hooks) != len(confluence.WebhookEvents) {
		t.Fatalf("registered %d hooks, want one per event (%d)", len(res.Hooks), len(confluence.WebhookEvents))
	}
	for _, h := range res.Hooks {
		if !h.Hooked() || !h.Created {
			t.Errorf("%s: %+v", h.Event, h)
		}
		if !strings.Contains(h.URL, "/webhooks/confluence/"+h.Event+"?token=") {
			t.Errorf("%s registered at %q, which does not name its event and carry the token", h.Event, h.URL)
		}
	}
}

// THE TOKEN IS MINTED ONCE, into the config's own ${VAR}, and carried in
// every URL. It is the whole authentication, so a run that minted a different
// one per hook would leave the engine able to verify at most one of them.
func TestCloudMintsOneTokenIntoTheVariable(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()

	run(t, site, sink, nil)

	token, ok, _ := sink.Value(context.Background(), "CONFLUENCE_WEBHOOK_TOKEN")
	if !ok || token == "" {
		t.Fatal("no token was minted into CONFLUENCE_WEBHOOK_TOKEN")
	}
	for _, u := range site.urls() {
		if !strings.HasSuffix(u, "?token="+token) {
			t.Errorf("hook %q does not carry the minted token", u)
		}
	}
}

// A RE-RUN WRITES NOTHING. This is the steady state and the one a loop would
// spend its life in; a re-run that re-registered would rotate the token every
// pass and invalidate the one the running engine holds.
func TestCloudRerunIsQuiet(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()

	run(t, site, sink, nil)
	token, _, _ := sink.Value(context.Background(), "CONFLUENCE_WEBHOOK_TOKEN")
	before := site.mutations()

	// The second run sees the token the first one minted, as the engine
	// would after a restart.
	res := run(t, site, sink, map[string]string{"CONFLUENCE_WEBHOOK_TOKEN": token})

	if site.mutations() != before {
		t.Fatalf("a re-run over a converged site made %d write(s)", site.mutations()-before)
	}
	for _, h := range res.Hooks {
		if h.Created {
			t.Errorf("%s was reported created on a re-run", h.Event)
		}
	}
}

// A HOOK POINTING ELSEWHERE IS RE-POINTED, not duplicated. The engine's own
// hooks are found by name, so a deployment that moved host converges rather
// than accumulating one hook per address it ever had.
func TestCloudRepointsAMovedHook(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()
	run(t, site, sink, nil)
	token, _, _ := sink.Value(context.Background(), "CONFLUENCE_WEBHOOK_TOKEN")

	// Simulate the operator having moved the engine: same token, new base.
	res, err := confluence.Reconcile(context.Background(), confluence.Options{
		Client: cloudClient(t, site),
		Config: &config.Confluence{URL: site.URL + "/wiki", Token: "t", WebhookToken: "${CONFLUENCE_WEBHOOK_TOKEN}"},
		Value:  func(v string) string { return map[string]string{"${CONFLUENCE_WEBHOOK_TOKEN}": token}[v] },
		Sink:   sink, WebhookBase: "https://moved.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	urls := site.urls()
	if len(urls) != len(confluence.WebhookEvents) {
		t.Fatalf("site holds %d hooks after a move, want %d (re-pointed, not duplicated)", len(urls), len(confluence.WebhookEvents))
	}
	for _, u := range urls {
		if !strings.HasPrefix(u, "https://moved.example.com/") {
			t.Errorf("hook %q was not re-pointed", u)
		}
	}
	for _, h := range res.Hooks {
		if h.Created {
			t.Errorf("%s was created rather than re-pointed", h.Event)
		}
	}
}

// A HOOK THE OPERATOR MADE BY HAND IS NEVER TOUCHED. Only hooks named under
// the engine's prefix are its to converge.
func TestCloudLeavesForeignHooksAlone(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	site.mu.Lock()
	site.hooks["99"] = map[string]any{"name": "ops-audit", "url": "https://ops.example.com/x",
		"events": []any{"page_updated"}, "enabled": true, "self": site.URL + "/wiki/rest/webhooks/1.0/webhook/99"}
	site.mu.Unlock()

	run(t, site, newSink(), nil)

	found := false
	for _, u := range site.urls() {
		if u == "https://ops.example.com/x" {
			found = true
		}
	}
	if !found {
		t.Fatal("a hook the operator registered by hand was removed or re-pointed")
	}
}

// No base registers nothing and says so, rather than guessing a host: a hook
// pointing at the wrong address is worse than no hook.
func TestNoBaseRegistersNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	res, err := confluence.Reconcile(context.Background(), confluence.Options{
		Client: cloudClient(t, site), Config: &config.Confluence{Token: "t"},
		Value: func(v string) string { return v },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hooks) != 0 || site.mutations() != 0 {
		t.Fatalf("a run with no base registered %d hooks and wrote %d times", len(res.Hooks), site.mutations())
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "public_base_url") {
		t.Fatalf("the operator is not told how to get a hook registered: %v", res.Notes)
	}
	// And the findings say nothing about ingress: a run that was never
	// asked to register cannot report a missing hook honestly.
	if f := res.Findings(); len(f) != 0 {
		t.Fatalf("a run with no base reported %+v", f)
	}
}

// The target parser and the registered URL agree on what "the same hook" is,
// so a base written with a trailing slash does not re-register every pass.
func TestSameTargetTolerates(t *testing.T) {
	t.Parallel()
	want := confluence.CloudWebhookTarget("https://e.example.com", "tok", "page_updated")
	for _, registered := range []string{
		want,
		"https://e.example.com/webhooks/confluence/page_updated/?token=tok",
	} {
		if !confluence.SameTarget(registered, want) {
			t.Errorf("%q was not recognised as %q", registered, want)
		}
	}
	for _, other := range []string{
		"https://e.example.com/webhooks/confluence/page_updated?token=other",
		"https://other.example.com/webhooks/confluence/page_updated?token=tok",
		"https://e.example.com/webhooks/confluence/page_created?token=tok",
	} {
		if confluence.SameTarget(other, want) {
			t.Errorf("%q was wrongly recognised as %q", other, want)
		}
	}
}

// Every hook that could not be established is a finding, and none that could
// is.
func TestFindingsNameTheUnhookedEvents(t *testing.T) {
	t.Parallel()
	res := &confluence.Result{Hooks: []confluence.HookState{
		{Event: "page_created", URL: "https://e/webhooks/confluence/page_created?token=t"},
		{Event: "comment_created", Detail: "the instance refused"},
	}}
	f := res.Findings()
	if len(f) != 1 || f[0].Kind != integration.FindingIngressBlocked || f[0].Subject != "comment_created" {
		t.Fatalf("findings = %+v", f)
	}
}

// A HOOK THE INSTANCE DISABLED IS NOT CONVERGED.
//
// Webhook.Enabled is parsed off the wire on every pass and was read nowhere,
// so a hook at the right address for the right event that Confluence had
// disabled was stamped as hooked and the integration reported Ready — the one
// field that says whether it delivers anything, fetched every pass and thrown
// away at the only point a decision was made.
func TestADisabledCloudHookIsReEnabled(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()
	env := map[string]string{"CONFLUENCE_WEBHOOK_TOKEN": "EXAMPLECONFLUENCETOKEN0000"}

	run(t, site, sink, env)
	before := site.mutations()
	site.disableAll()

	run(t, site, sink, env)
	if got := site.mutations(); got == before {
		t.Fatal("a pass over disabled hooks wrote nothing, so they stay disabled " +
			"and the integration reports ready while delivering nothing")
	}
	for id, hook := range site.snapshot() {
		if enabled, _ := hook["enabled"].(bool); !enabled {
			t.Errorf("hook %s is still disabled after a pass", id)
		}
	}
}

// ONE EVENT'S REFUSAL IS NOT THE PASS'S.
//
// The first refusal returned an error, so every event after it was skipped
// and Findings() was never called at all — which made HookState.Detail dead
// and the FindingIngressBlocked branch unreachable. The two answers are
// opposite to the loop: an error is a fault it waits on, an ingress block is
// degraded and owed by somebody who can grant the permission.
func TestOneRefusedCloudEventDoesNotStopTheOthers(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	site.refuse = "page_created"
	sink := newSink()

	res, err := confluence.Reconcile(context.Background(), confluence.Options{
		Client: cloudClient(t, site),
		Config: &config.Confluence{
			URL: site.URL + "/wiki", Token: "t",
			WebhookToken: "${CONFLUENCE_WEBHOOK_TOKEN}",
		},
		Value: func(v string) string {
			if v == "${CONFLUENCE_WEBHOOK_TOKEN}" {
				return "EXAMPLECONFLUENCETOKEN0000"
			}
			return v
		},
		Sink:        sink,
		WebhookBase: "https://engine.example.com",
	})
	if err != nil {
		t.Fatalf("one refused event failed the whole pass: %v", err)
	}

	var blocked, hooked int
	for _, hook := range res.Hooks {
		if hook.Hooked() {
			hooked++
			continue
		}
		blocked++
		if hook.Event != "page_created" {
			t.Errorf("%s was not hooked and was not the refused event", hook.Event)
		}
		if hook.Detail == "" {
			t.Errorf("%s was not hooked and says nothing about why", hook.Event)
		}
	}
	if blocked != 1 || hooked != len(confluence.WebhookEvents)-1 {
		t.Fatalf("%d hooked and %d blocked, want every event but one registered",
			hooked, blocked)
	}
	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingIngressBlocked {
		t.Fatalf("findings = %+v, want one ingress block", findings)
	}
	if findings[0].Subject != "page_created" {
		t.Errorf("the finding names %q rather than the refused event", findings[0].Subject)
	}
}

// dataCenterClient builds a client the reconcile drives down its Data Center
// branch. That branch had NO test in this package at all — `DataCenter`
// appeared in no test file — which is how it came to match a raw URL string
// where every other half of this integration matches the hook's name.
func dataCenterClient(t *testing.T, site *cloudSite) *confluence.Client {
	t.Helper()
	c, err := confluence.NewClient(confluence.ClientOptions{
		URL: site.URL, Token: "org-token", Deployment: confluence.DataCenter,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func runDataCenter(
	t *testing.T, site *cloudSite, sink *recordingSink, base string,
) *confluence.Result {
	t.Helper()
	res, err := confluence.Reconcile(context.Background(), confluence.Options{
		Client: dataCenterClient(t, site),
		Config: &config.Confluence{
			URL: site.URL, Token: "t",
			WebhookSecret: "${CONFLUENCE_WEBHOOK_SECRET}",
		},
		Value: func(v string) string {
			if v == "${CONFLUENCE_WEBHOOK_SECRET}" {
				stored, _, _ := sink.Value(context.Background(), "CONFLUENCE_WEBHOOK_SECRET")
				return stored
			}
			return v
		},
		Sink:        sink,
		WebhookBase: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A DATA CENTER HOOK THAT MOVED IS RE-POINTED, not duplicated.
//
// The address was compared with != and the name never looked at, so a hook
// whose target had moved read as "not mine": the pass created a second
// registration and left the first enabled, still carrying a valid signing
// secret, delivering to an address that no longer answers.
func TestADataCenterHookThatMovedIsRepointed(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()

	runDataCenter(t, site, sink, "https://old-tunnel.example.com")
	if got := len(site.snapshot()); got != 1 {
		t.Fatalf("the first pass registered %d hooks, want 1", got)
	}

	runDataCenter(t, site, sink, "https://engine.example.com")
	hooks := site.snapshot()
	if len(hooks) != 1 {
		t.Fatalf("a moved deployment left %d registrations behind: %v",
			len(hooks), site.urls())
	}
	for _, hook := range hooks {
		if hook["url"] != "https://engine.example.com/webhooks/confluence" {
			t.Errorf("the surviving hook points at %v", hook["url"])
		}
	}
}

// AND A HOOK SOMEBODY ELSE REGISTERED AT THAT ADDRESS IS NOT ADOPTED.
//
// Matching the URL alone renamed it to crewlet:all, re-subscribed it to this
// engine's events and re-keyed it with this engine's secret — taking over a
// registration the operator made by hand.
func TestADataCenterHookThisEngineDidNotMakeIsLeftAlone(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()
	site.seed(map[string]any{
		"name": "ops-team-hook", "enabled": true,
		"url":    "https://engine.example.com/webhooks/confluence",
		"events": []any{"page_created"},
	})

	runDataCenter(t, site, sink, "https://engine.example.com")

	var theirs, ours int
	for _, hook := range site.snapshot() {
		if hook["name"] == "ops-team-hook" {
			theirs++
			if events, _ := hook["events"].([]any); len(events) != 1 {
				t.Errorf("somebody else's hook was re-subscribed: %v", hook["events"])
			}
			continue
		}
		ours++
	}
	if theirs != 1 {
		t.Error("a hook this engine never registered was taken over")
	}
	if ours != 1 {
		t.Errorf("this engine registered %d hooks of its own, want 1", ours)
	}
}

// A CONVERGED DATA CENTER PASS WRITES NOTHING, which the Cloud half has been
// asserting since it was written and this half had no branch for: it re-sent
// the name, the address, all eight events and the signing secret on every
// pass, for the life of the deployment.
func TestADataCenterRerunIsQuiet(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()

	runDataCenter(t, site, sink, "https://engine.example.com")
	before := site.mutations()

	runDataCenter(t, site, sink, "https://engine.example.com")
	if got := site.mutations(); got != before {
		t.Errorf("a converged pass made %d write(s) at the instance", got-before)
	}
}

// AND A TRAILING SLASH IS NOT A DIFFERENT ADDRESS. Confluence stores what it
// was given verbatim, so a raw string comparison re-created the hook on every
// pass over a base written with one.
func TestADataCenterHookStoredWithATrailingSlashIsConverged(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()

	runDataCenter(t, site, sink, "https://engine.example.com")
	site.retarget("https://engine.example.com/webhooks/confluence/")
	before := site.mutations()

	runDataCenter(t, site, sink, "https://engine.example.com")
	if got := site.mutations(); got != before {
		t.Errorf("a hook stored with a trailing slash was rewritten (%d write(s))",
			got-before)
	}
}
