package confluence_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
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
	// refuse names an event this site will not let this credential
	// register: any registration SUBSCRIBING to it is answered 403, so both
	// a partially hooked-up Cloud site and a Data Center instance that
	// refuses its one all-events hook are representable. See
	// [cloudSite.refuses].
	refuse string
	// refuseDelete makes the site refuse to REMOVE a hook, which the event
	// knob above cannot express: a delete carries no body, so there is no
	// event list to match it on. It is the credential that may list and
	// register but not remove — the shape that decides whether a -recreate
	// run leaves one registration or two.
	refuseDelete bool
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
	case strings.HasSuffix(path, "/rest/api/user/current") && req.Method == http.MethodGet:
		_, _ = w.Write([]byte(`{"accountId":"acct-org"}`))
	case strings.HasSuffix(path, "/rest/api/user/current"):
		// THE METHOD CHECK IS PART OF THE COUNTER, and this arm is where
		// it is kept honest.
		//
		// [cloudSite.writes] counts by ROUTE, which is the rule
		// integrationtest's package doc sets: some third-party apps model a
		// listing as a POST, so a method-keyed counter makes the
		// converged-pass-writes-nothing clause impossible to satisfy. The
		// price of that rule is that a route declared read-only has to
		// actually BE read-only — this one matched on the path alone, so a
		// mutating call here would have been served as a read and counted as
		// nothing at all, which is that same hole inverted.
		//
		// Nothing in the pass does that today (it only GETs here), and a 405
		// is what keeps it that way: the day somebody reaches this route with
		// a write, they get a failure to read rather than a silent pass.
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"message":"the identity route is read-only"}`))
	case strings.HasSuffix(path, "/rest/webhooks/1.0/webhook") && req.Method == http.MethodGet:
		out := make([]map[string]any, 0, len(s.hooks))
		for _, h := range s.hooks {
			out = append(out, h)
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasSuffix(path, "/rest/webhooks/1.0/webhook") && req.Method == http.MethodPost:
		var in map[string]any
		_ = json.NewDecoder(req.Body).Decode(&in)
		if s.refuses(in) {
			s.refusal(w)
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
		id := path[strings.LastIndex(path, "/")+1:]
		var in map[string]any
		_ = json.NewDecoder(req.Body).Decode(&in)
		// REFUSED BEFORE IT IS COUNTED, and refusable at all because an
		// instance that will not let this credential register an event will
		// not let it re-point one either. A site that refused only the POST
		// would make a re-pointed hook succeed where a fresh one was
		// forbidden, which no real permission model does.
		if s.refuses(in) {
			s.refusal(w)
			return
		}
		s.writes++
		h := s.hooks[id]
		h["name"], h["url"], h["events"] = in["name"], in["url"], in["events"]
		// An update re-asserts enabled, which is what both writers send.
		h["enabled"] = true
		_ = json.NewEncoder(w).Encode(h)
	case strings.Contains(path, "/rest/webhooks/1.0/webhook/") && req.Method == http.MethodDelete:
		if s.refuseDelete {
			s.refusal(w)
			return
		}
		s.writes++
		delete(s.hooks, path[strings.LastIndex(path, "/")+1:])
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// refuses reports a registration this site will not accept: one that
// subscribes to [cloudSite.refuse], whether on its own or among others.
//
// BY MEMBERSHIP RATHER THAN BY EQUALITY, which is what makes the two
// deployments refusable by one knob. A Cloud registration names exactly one
// event, so the two rules agree there; Data Center registers every event in a
// single call, and an equality check made that call unrefusable — which left
// that branch with no world in which it reports anything at all, so the whole
// findings half of the contract was certified over Cloud alone.
func (s *cloudSite) refuses(in map[string]any) bool {
	if s.refuse == "" {
		return false
	}
	events, ok := in["events"].([]any)
	if !ok {
		return false
	}
	return slices.Contains(events, any(s.refuse))
}

// refusal is what the instance answers a registration it will not accept: the
// 403 an account without the permission to administer webhooks gets.
func (*cloudSite) refusal(w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"message":"this token may not register that event"}`))
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

// resubscribe rewrites one named hook's event list, standing in for an
// administrator editing the subscription at the instance.
//
// It is the third thing a listing reports and the only one neither the name
// nor the address covers, so it is the only way to stand up a hook that is
// this engine's, at the right address, and subscribed to the wrong thing.
func (s *cloudSite) resubscribe(name string, events ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.hooks {
		if h["name"] != name {
			continue
		}
		list := make([]any, 0, len(events))
		for _, event := range events {
			list = append(list, event)
		}
		h["events"] = list
		return true
	}
	return false
}

// eventsOf reads one named hook's subscription back.
func (s *cloudSite) eventsOf(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.hooks {
		if h["name"] != name {
			continue
		}
		raw, _ := h["events"].([]any)
		out := make([]string, 0, len(raw))
		for _, event := range raw {
			out = append(out, event.(string))
		}
		return out
	}
	return nil
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
	// forgotten is what a teardown asked this sink to delete.
	forgotten []string

	mu   sync.Mutex
	vals map[string]string
	// flushes counts completions. It is what a value being VISIBLE to a
	// running engine costs: the sink the engine hands in rebuilds the
	// resolver's snapshot inside Flush and nowhere else, so a minted value
	// that was never flushed is sealed and unreadable.
	flushes int
}

func newSink() *recordingSink { return &recordingSink{vals: map[string]string{}} }

func (s *recordingSink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[name] = value
	return nil
}
func (s *recordingSink) Discard(context.Context) error { return nil }

// Forget implements [provision.TokenSink]: it records what a teardown
// asked to be deleted, so a case can assert the deletion happened.
func (s *recordingSink) Forget(_ context.Context, names ...string) error {
	s.forgotten = append(s.forgotten, names...)
	return nil
}
func (s *recordingSink) Flush(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushes++
	return nil
}

func (s *recordingSink) flushed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushes
}
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

// cuttingTransport delivers the instance's answers and then CANCELS the
// pass's own context, which is what a node shutting down — or the lease
// deadline the loop bounds a pass with — does to a pass mid-walk.
//
// THE RESPONSE IS BUFFERED BEFORE THE CANCELLATION. A cancellation reaches an
// in-flight body read, so cutting the context before the caller has read the
// listing would fail the READ, and the scenario worth standing up is the
// opposite one: the pass got its answer, and everything it does afterwards is
// on a context that is already dead.
type cuttingTransport struct {
	base      http.RoundTripper
	remaining atomic.Int64
	cancel    context.CancelFunc
	// inFlight cuts the context as the request goes OUT rather than once
	// it has been answered, which is the other half of what a deadline
	// does: it expires wherever the pass happens to be, and half the time
	// that is inside a registration rather than between two of them.
	inFlight bool
	// attempts counts every request the pass STARTED, including the ones a
	// dead context refuses before they reach the instance. The site itself
	// cannot see those, and they are the whole evidence of a walk carrying
	// on past the point where it could do anything.
	attempts atomic.Int64
}

func (c *cuttingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.attempts.Add(1)
	if c.inFlight && c.remaining.Add(-1) == 0 {
		c.cancel()
	}
	resp, err := c.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if !c.inFlight && c.remaining.Add(-1) == 0 {
		c.cancel()
	}
	return resp, nil
}

// cut says WHEN the pass's context dies: once the instance has answered
// `after` requests, or — with inFlight — as the `after`th request goes out.
//
// Both happen, and they are different code paths through the walk: one is
// noticed before an event is looked at, the other comes back as that event's
// own write failing.
type cut struct {
	after    int
	inFlight bool
}

// cancellableSink refuses to complete on a context that has already been
// cancelled, exactly as a store write does.
//
// It is the only way to tell a flush that happened from one that happened on
// a context which could not carry it: [recordingSink] takes no notice of the
// context at all, so without this a pass could "flush" a minted value into a
// call that a real sealed store would have refused.
type cancellableSink struct{ *recordingSink }

func (s *cancellableSink) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.New("the sink was flushed on a context that had already been cancelled")
	}
	return s.recordingSink.Flush(ctx)
}

// cuttingClient talks to the site through the transport that ends the pass.
func cuttingClient(
	t *testing.T, site *cloudSite, deployment confluence.Deployment,
	transport *cuttingTransport,
) *confluence.Client {
	t.Helper()
	base, email := site.URL, ""
	if deployment == confluence.Cloud {
		base, email = site.URL+"/wiki", "org@example.com"
	}
	c, err := confluence.NewClient(confluence.ClientOptions{
		URL: base, Email: email, Token: "org-token", Deployment: deployment,
		HTTP: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// cutPass runs one pass whose context dies where `when` says, and returns
// what that pass reported — which is exactly what the engine's own adapter
// hands the loop — together with the transport that counted its requests.
func cutPass(
	t *testing.T, site *cloudSite, sink provision.TokenSink,
	deployment confluence.Deployment, when cut,
) (*confluence.Result, *cuttingTransport, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := &cuttingTransport{
		base: site.Client().Transport, cancel: cancel, inFlight: when.inFlight,
	}
	transport.remaining.Store(int64(when.after))

	cfg := &config.Confluence{
		URL: site.URL, Token: "t", WebhookSecret: "${CONFLUENCE_WEBHOOK_SECRET}",
	}
	variable := "CONFLUENCE_WEBHOOK_SECRET"
	if deployment == confluence.Cloud {
		cfg = &config.Confluence{
			URL: site.URL + "/wiki", Token: "t",
			WebhookToken: "${CONFLUENCE_WEBHOOK_TOKEN}",
		}
		variable = "CONFLUENCE_WEBHOOK_TOKEN"
	}
	res, err := confluence.Reconcile(ctx, confluence.Options{
		Client: cuttingClient(t, site, deployment, transport),
		Config: cfg,
		Value: func(v string) string {
			if v != "${"+variable+"}" {
				return v
			}
			held, _, _ := sink.Value(context.Background(), variable)
			return held
		},
		Sink:        sink,
		WebhookBase: "https://engine.example.com",
	})
	return res, transport, err
}

// A CLOUD PASS CUT SHORT REPORTS A FAULT, NOT A CONVERGED SITE.
//
// This is the failure that hides behind the steady state. Over a converged
// site the walk makes no request at all, so a context that died after the
// listing produced no error and no findings — and those two together are the
// loop's word for "this integration is ready". A node shutting down recorded
// every Confluence as healthy on its way out, and the next node to hold the
// duty trusted that for a full settled interval.
func TestACancelledCloudPassOverAConvergedSiteReportsAFaultRatherThanHealth(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()
	run(t, site, sink, nil)

	// Cut after the identity probe and the listing, which is every request
	// a converged pass makes.
	res, _, err := cutPass(t, site, sink, confluence.Cloud, cut{after: 2})
	if err == nil && len(res.Findings()) == 0 {
		t.Fatal("a pass whose context died after the listing reported a converged " +
			"site it had stopped reading; the loop records that as ready")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the fault does not name the cancellation behind it: %v", err)
	}
}

// AND IT DOES NOT BLAME THE INSTANCE FOR ITS OWN CANCELLATION.
//
// A per-event refusal is recorded and the walk carries on, which is right
// when Confluence refused that registration. A dead context refuses all eight
// identically, and recording it eight times reports FindingIngressBlocked —
// degraded, owed by an ADMINISTRATOR — so a node shutting down sent somebody
// to grant a permission that was never missing.
func TestACancelledCloudPassDoesNotBlameTheInstance(t *testing.T) {
	t.Parallel()
	for _, when := range []struct {
		name string
		cut  cut
		// requests is every request the pass may start: the identity
		// probe, the listing, and — where the cut lands inside one — the
		// registration that carried it.
		requests int64
	}{
		{"between two events", cut{after: 2}, 2},
		{"while a registration is in flight", cut{after: 3, inFlight: true}, 3},
	} {
		t.Run(when.name, func(t *testing.T) {
			t.Parallel()
			site := newCloudSite(t)
			sink := newSink()

			// Nothing is registered yet, so every one of the eight
			// events is a write this pass will not get to make.
			res, transport, err := cutPass(t, site, sink, confluence.Cloud, when.cut)
			if err == nil {
				t.Fatalf("a pass whose context died before it could register "+
					"anything returned no error; it reported %+v", res.Findings())
			}
			for _, finding := range res.Findings() {
				if finding.Kind == integration.FindingIngressBlocked {
					t.Errorf("the cancellation was reported as the instance "+
						"blocking %q: %s", finding.Subject, finding.Detail)
				}
			}
			// AND IT STOPPED WORKING, rather than walking the remaining
			// events into a context that refuses every one of them.
			if extra := transport.attempts.Load() - when.requests; extra > 0 {
				t.Errorf("the pass started %d more request(s) after its context died",
					extra)
			}
		})
	}
}

// THE DATA CENTER HALF OF THE SAME CLAUSE, which is its own code: the branch
// answers a converged instance by returning from inside the listing walk, so
// nothing below it could have noticed the context either.
func TestACancelledDataCenterPassOverAConvergedInstanceReportsAFault(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()
	runDataCenter(t, site, sink, "https://engine.example.com")

	res, _, err := cutPass(t, site, sink, confluence.DataCenter, cut{after: 2})
	if err == nil && len(res.Findings()) == 0 {
		t.Fatal("a pass whose context died after the listing reported a converged " +
			"instance it had stopped reading; the loop records that as ready")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the fault does not name the cancellation behind it: %v", err)
	}
}

// A VALUE THIS PASS MINTED IS FLUSHED EVEN WHEN THE PASS THEN FAILS.
//
// The token is sealed BEFORE the hooks that carry it are registered, and the
// sink the engine hands in rebuilds the resolver's snapshot only inside
// Flush. Returning from the failure without one left the value sealed and
// INVISIBLE: the next pass resolved nothing, minted a second value, failed at
// the same place, and did that for as long as the failure lasted — which is
// the exact runaway the refreshing sink exists to prevent.
//
// And flushed on a context that can carry it, because the failure being
// cleaned up after is often the cancellation itself.
func TestAValueMintedBeforeAFailureIsStillFlushed(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := &cancellableSink{recordingSink: newSink()}

	// Cut after the identity probe: the pass mints, and then the listing
	// it needs before it can register anything fails.
	_, _, err := cutPass(t, site, sink, confluence.Cloud, cut{after: 1})
	if err == nil {
		t.Fatal("the pass did not fail, so there is no failure path here to test")
	}
	token, ok, _ := sink.Value(context.Background(), "CONFLUENCE_WEBHOOK_TOKEN")
	if !ok || token == "" {
		t.Fatal("nothing was minted, so this is not the mint-then-fail path")
	}
	if sink.flushed() == 0 {
		t.Error("a minted token was left sealed and unflushed, so the running " +
			"engine still holds the snapshot from before it and the next pass " +
			"mints another one")
	}
	if strings.Contains(err.Error(), "already been cancelled") {
		t.Errorf("the sink was flushed on the pass's own dead context: %v", err)
	}
}

// A DATA CENTER REFUSAL IS A BLOCKED INGRESS, NOT A PASS FAULT.
//
// Every write on this branch returned an error, so an instance that refused
// the registration — a 403, which is what an org account without the
// Confluence Administrator global permission gets here — came out of
// Reconcile as a fault. The loop reads a fault as the engine still working on
// it and retries it on the waiting backoff for ever; FindingIngressBlocked is
// degraded and owed by the ADMINISTRATOR who can grant that permission. So on
// the deployment where missing admin rights are most likely, the one person
// who could fix it was never told. The Cloud half of this same file has
// answered correctly since it was written.
func TestARefusedDataCenterRegistrationIsBlockedIngressRatherThanAFault(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	site.refuse = "page_created"
	sink := newSink()

	res, err := confluence.Reconcile(context.Background(), confluence.Options{
		Client: dataCenterClient(t, site),
		Config: &config.Confluence{
			URL: site.URL, Token: "t",
			WebhookSecret: "${CONFLUENCE_WEBHOOK_SECRET}",
		},
		Value:       func(v string) string { return v },
		Sink:        sink,
		WebhookBase: "https://engine.example.com",
	})
	if err != nil {
		t.Fatalf("the instance refusing one registration failed the whole pass, "+
			"so the loop waits on it instead of telling an administrator: %v", err)
	}
	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingIngressBlocked {
		t.Fatalf("findings = %+v, want one ingress block", findings)
	}
	if _, actor := findings[0].Kind.Verdict(); !actor.WaitsOnAPerson() {
		t.Errorf("the refusal is owed by %s rather than by a person", actor)
	}
	// AND IT SAYS WHAT WAS LOST. This branch's single hook carries every
	// event, so a refusal is total rather than one event class.
	if !strings.Contains(findings[0].Detail, "NO Confluence event") {
		t.Errorf("the finding does not say that nothing reaches the engine: %q",
			findings[0].Detail)
	}
	if !strings.Contains(findings[0].Detail, "403") {
		t.Errorf("the finding does not carry the instance's own refusal: %q",
			findings[0].Detail)
	}
}

// AND THE PASS THAT MADE IT IS STILL QUIET ABOUT THE REST.
//
// A refusal recorded on the state must not also leave a half-written
// registration behind, and two passes over the same refusing instance must
// report the same thing — a finding that churned would flap the reported
// phase and reset the backoff every few minutes.
func TestTwoPassesOverARefusingDataCenterInstanceAgree(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	site.refuse = "page_created"
	sink := newSink()

	first := runDataCenter(t, site, sink, "https://engine.example.com")
	second := runDataCenter(t, site, sink, "https://engine.example.com")

	if got := len(site.snapshot()); got != 0 {
		t.Errorf("a refused registration left %d hook(s) at the instance", got)
	}
	if !slices.Equal(first.Findings(), second.Findings()) {
		t.Fatalf("two passes disagree:\n first: %+v\nsecond: %+v",
			first.Findings(), second.Findings())
	}
}

// A PASS WITH NO BASE IS A FAULT ON A DEAD CONTEXT, like every other exit.
//
// The empty-WebhookBase return was the ONE path that did not fold ctx.Err():
// it answered (no findings, no error) — the loop's word for ready — from a
// pass that had already run out of context. [Client.Me] running first makes a
// CANCELLATION unreachable there and says nothing about a DEADLINE, which is
// what actually bounds a pass: the lease that protects it. So the context is
// cut with a deadline that has already expired by the time the pass returns
// from the identity probe.
func TestAPassWithNoBaseOnADeadContextIsAFaultRatherThanHealth(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := &cuttingTransport{base: site.Client().Transport, cancel: cancel}
	// After the identity probe — the only request a pass with no base
	// makes — and before it returns.
	transport.remaining.Store(1)

	res, err := confluence.Reconcile(ctx, confluence.Options{
		Client:      cuttingClient(t, site, confluence.Cloud, transport),
		Config:      &config.Confluence{URL: site.URL + "/wiki", Token: "t"},
		Value:       func(v string) string { return v },
		WebhookBase: "",
	})
	if err == nil {
		t.Fatalf("a pass whose context died reported a company with no public "+
			"base URL as ready: %+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the fault does not name the cancellation behind it: %v", err)
	}
}

// A CLOUD HOOK SUBSCRIBED TO THE WRONG EVENT IS RE-SUBSCRIBED.
//
// The event list is the third thing a listing reports, and it is the one that
// decides what actually arrives. The engine registers ONE HOOK PER EVENT
// because the Cloud payload names none — the route stamps the event back onto
// the body from the PATH — so a hook at the crewlet:page_created address
// subscribed to comment_created does not merely deliver the wrong thing: it
// delivers comment payloads STAMPED as page creations, and every seat woken by
// one is woken about work that does not exist. A pass that compared only the
// name and the address called that converged.
//
// Both shapes, because the converged predicate makes two claims: exactly one
// event, and that it is this one.
func TestACloudHookSubscribedToTheWrongEventsIsCorrected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		events []string
	}{
		{"a different event", []string{"comment_created"}},
		{"its own event and another", []string{"page_created", "comment_created"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			site := newCloudSite(t)
			sink := newSink()
			env := map[string]string{"CONFLUENCE_WEBHOOK_TOKEN": "EXAMPLECONFLUENCETOKEN0000"}
			run(t, site, sink, env)

			hook := confluence.HookName("page_created")
			if !site.resubscribe(hook, tc.events...) {
				t.Fatalf("%s was never registered, so there is nothing to edit", hook)
			}
			before := site.mutations()

			run(t, site, sink, env)
			if site.mutations() == before {
				t.Fatal("a pass over a hook subscribed to the wrong event wrote " +
					"nothing, so the engine goes on stamping whatever arrives " +
					"there as a page creation")
			}
			if got := site.eventsOf(hook); !slices.Equal(got, []string{"page_created"}) {
				t.Errorf("%s is subscribed to %v after a pass", hook, got)
			}
		})
	}
}

// A DISABLED DATA CENTER HOOK IS RE-ENABLED, which is the Cloud half's
// TestADisabledCloudHookIsReEnabled over the branch that had no such test.
//
// Webhook.Enabled is parsed off the wire on every pass, and this branch's
// converged check reads it — but nothing asserted that. A hook at the right
// address subscribed to the right events that the administrator had disabled
// would be stamped as hooked and the integration reported Ready, delivering
// nothing, and here that is the WHOLE integration: Data Center registers one
// hook for every event.
func TestADisabledDataCenterHookIsReEnabled(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()

	runDataCenter(t, site, sink, "https://engine.example.com")
	before := site.mutations()
	site.disableAll()

	runDataCenter(t, site, sink, "https://engine.example.com")
	if got := site.mutations(); got == before {
		t.Fatal("a pass over the disabled hook wrote nothing, so it stays " +
			"disabled and the integration reports ready while no Confluence " +
			"event reaches the engine at all")
	}
	for id, hook := range site.snapshot() {
		if enabled, _ := hook["enabled"].(bool); !enabled {
			t.Errorf("hook %s is still disabled after a pass", id)
		}
	}
}

// A DATA CENTER HOOK MISSING ONE OF THIS ENGINE'S EVENTS IS RE-SUBSCRIBED.
//
// [confluence.WebhookEvents] is exactly the set the parser routes, so an event
// this hook is not subscribed to is an event class that silently never
// arrives — no error, no finding, just a category of work the company stops
// hearing about. The pass compares the set for that reason and nothing
// asserted it.
//
// EXTRAS ARE STILL LEFT ALONE, which is the other half of the same predicate:
// an operator who added an event wanted it, and taking it away every pass is
// the opposite of converging what this engine needs.
func TestADataCenterHookMissingAnEventIsResubscribed(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()
	runDataCenter(t, site, sink, "https://engine.example.com")
	hook := confluence.HookName("all")

	if !site.resubscribe(hook, "page_created") {
		t.Fatalf("%s was never registered, so there is nothing to edit", hook)
	}
	before := site.mutations()

	runDataCenter(t, site, sink, "https://engine.example.com")
	if site.mutations() == before {
		t.Fatal("a pass over a hook subscribed to one event of eight wrote " +
			"nothing, so seven event classes never arrive and nothing says so")
	}
	for _, event := range confluence.WebhookEvents {
		if !slices.Contains(site.eventsOf(hook), event) {
			t.Errorf("%s is not subscribed to %s after a pass", hook, event)
		}
	}

	// And an operator's own extra event survives the next pass untouched.
	site.resubscribe(hook, append(slices.Clone(confluence.WebhookEvents), "label_added")...)
	before = site.mutations()
	runDataCenter(t, site, sink, "https://engine.example.com")
	if site.mutations() != before {
		t.Error("a pass rewrote a subscription that already carried every event " +
			"this engine needs, taking away one the operator added")
	}
}

// AND A VALUE MINTED BY A PASS THAT SUCCEEDED IS FLUSHED TOO.
//
// The failure path has its own test above, and it is the rarer half. The
// ordinary way out of a minting pass is success, and the consequence of not
// flushing is identical and more common: the sink the engine hands in rebuilds
// the resolver's snapshot only inside Flush, so the running engine goes on
// resolving the ${VAR} to nothing, the next pass mints a second value, and
// every hook the last one registered is now carrying a token the engine will
// not verify.
func TestAValueMintedByASuccessfulPassIsFlushed(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()

	run(t, site, sink, nil)

	if token, ok, _ := sink.Value(context.Background(), "CONFLUENCE_WEBHOOK_TOKEN"); !ok || token == "" {
		t.Fatal("nothing was minted, so this is not the mint-then-succeed path")
	}
	if sink.flushed() == 0 {
		t.Error("a pass that minted a token and then succeeded left it sealed " +
			"and unflushed, so the running engine still holds the snapshot from " +
			"before it and the next pass mints another one")
	}
}

// A REFUSED DATA CENTER RE-POINT IS A BLOCKED INGRESS TOO.
//
// The create path has its own test above; this is the same claim over the
// other write this branch makes, and the state it leaves behind is worse. The
// registration is still there — enabled, signed with a key the engine still
// holds — pointing at an address that no longer answers, so an administrator
// looking at the instance sees a healthy-looking hook. That is precisely when
// the finding has to name the refusal rather than the loop waiting on a fault
// nobody is told about.
func TestARefusedDataCenterRepointIsBlockedIngressRatherThanAFault(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()
	runDataCenter(t, site, sink, "https://old-tunnel.example.com")

	// The permission is withdrawn between the two passes, and the engine
	// moves: the pass now has to re-point a hook it may no longer write.
	site.mu.Lock()
	site.refuse = "page_created"
	site.mu.Unlock()
	res := runDataCenter(t, site, sink, "https://engine.example.com")

	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingIngressBlocked {
		t.Fatalf("findings = %+v, want one ingress block", findings)
	}
	if !strings.Contains(findings[0].Detail, "points at this engine") {
		t.Errorf("the finding claims nothing is registered, where what is wrong "+
			"is that the registration still points at the old address: %q",
			findings[0].Detail)
	}
	// AND NOTHING WAS DUPLICATED: the refused update left one registration,
	// not a second one beside it.
	if got := len(site.snapshot()); got != 1 {
		t.Errorf("the instance holds %d registrations after a refused re-point", got)
	}
}

// A -RECREATE RUN THAT CANNOT REMOVE THE OLD DATA CENTER HOOK DOES NOT MAKE
// A SECOND ONE.
//
// Recreate is delete-then-create, and the create carries a FRESH signing
// secret. Falling through to it after a failed delete leaves the instance
// with two enabled registrations at the same address: the old one signing
// with a key the engine has just replaced, so half the deliveries fail
// verification at the edge and nothing at the instance looks wrong. The pass
// reports the block instead and leaves the working hook alone.
func TestADataCenterRecreateThatCannotDeleteDoesNotRegisterASecondHook(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()
	runDataCenter(t, site, sink, "https://engine.example.com")
	site.mu.Lock()
	site.refuseDelete = true
	site.mu.Unlock()

	res, err := confluence.Reconcile(context.Background(), confluence.Options{
		Client: dataCenterClient(t, site),
		Config: &config.Confluence{
			URL: site.URL, Token: "t",
			WebhookSecret: "${CONFLUENCE_WEBHOOK_SECRET}",
		},
		Value: func(v string) string {
			if v != "${CONFLUENCE_WEBHOOK_SECRET}" {
				return v
			}
			stored, _, _ := sink.Value(context.Background(), "CONFLUENCE_WEBHOOK_SECRET")
			return stored
		},
		Sink:        sink,
		WebhookBase: "https://engine.example.com",
		Recreate:    true,
	})
	if err != nil {
		t.Fatalf("a refused delete failed the whole pass: %v", err)
	}
	if got := len(site.snapshot()); got != 1 {
		t.Fatalf("the instance holds %d registrations, want the one that could "+
			"not be removed and no second one beside it", got)
	}
	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingIngressBlocked {
		t.Fatalf("findings = %+v, want one ingress block naming the refusal", findings)
	}
}

// A FINDING NEVER ENDS IN A DANGLING COLON.
//
// Every path inside this package records a reason before it reports a hook as
// unestablished, so the fallback is about the EXPORTED surface: Result and
// HookState are both exported and Findings() is what the engine calls on
// whatever it is handed. A state arriving without a detail rendered
// "…reach nobody: " — a sentence addressed to an administrator that names
// nothing to do, which is the one thing this finding kind exists to avoid.
func TestAnUnhookedEventWithNoDetailStillSaysWhatToDo(t *testing.T) {
	t.Parallel()
	res := &confluence.Result{Hooks: []confluence.HookState{
		{Event: "page_created"},
		{Event: "all"},
	}}
	findings := res.Findings()
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want one per unhooked event", findings)
	}
	for _, finding := range findings {
		detail := strings.TrimSpace(finding.Detail)
		if detail == "" || strings.HasSuffix(detail, ":") {
			t.Errorf("%s: the finding ends where the reason should start: %q",
				finding.Subject, finding.Detail)
		}
	}
}

// TEARDOWN REMOVES THIS ENGINE'S HOOKS AND NOTHING ELSE.
//
// Teardown had no test in this package at all, and it is the one function
// here whose failure mode is destruction rather than drift: it walks every
// registration the instance holds and deletes. What confines it to this
// engine's own is the [confluence.HookNamePrefix] check — the same namespace
// rule the two converge halves follow — and with that check wrong, a
// disconnect silently removes every webhook the Confluence administrator ever
// registered, for tools that have nothing to do with Crewlet. Nothing at the
// instance records what they were.
//
// SAFE TO REPEAT is the other half of its doc, and it is what a retried
// disconnect depends on: a hook already gone is not an error, and a pass that
// finds none has finished rather than failed.
func TestTeardownRemovesOnlyThisEnginesHooksAndIsSafeToRepeat(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	sink := newSink()
	run(t, site, sink, nil)
	site.seed(map[string]any{
		"name": "ops-audit", "enabled": true,
		"url":    "https://ops.example.com/x",
		"events": []any{"page_updated"},
	})

	opts := confluence.Options{Client: cloudClient(t, site)}
	if err := confluence.Teardown(context.Background(), opts); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	left := site.snapshot()
	if len(left) != 1 {
		t.Fatalf("the instance holds %d hook(s) after a teardown, want only the "+
			"one this engine never registered: %v", len(left), site.urls())
	}
	for _, hook := range left {
		if hook["name"] != "ops-audit" {
			t.Errorf("teardown left %v and removed the operator's own hook", hook["name"])
		}
	}

	// Repeated, which is what a retried disconnect does.
	if err := confluence.Teardown(context.Background(), opts); err != nil {
		t.Errorf("a second teardown over an instance holding none of this "+
			"engine's hooks failed, so a retried disconnect can never finish: %v", err)
	}
}
