package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// A draining node keeps its listener for its probes and its reads, and refuses
// every request that would start work. These cases pin both halves of that
// against one app whose runtime can be told it is draining, because a gate
// that refused everything would pass the refusal half and a gate that refused
// nothing would pass the other.

// drainingApp is an authenticated, configured node with the webhook edge
// mounted on a real queue, draining or not.
func drainingApp(t *testing.T, draining bool) *api.App {
	t.Helper()
	return drainingAppOn(t, &fakeRuntime{state: api.RuntimeState{
		ShuttingDown: draining, Posture: "serve",
	}})
}

// drainingAppOn is [drainingApp] beside a runtime of the case's own, for a case
// that decides when the drain begins.
func drainingAppOn(t *testing.T, rt api.NodeRuntime) *api.App {
	t.Helper()
	b := closedPosture()
	b.API.Auth.AllowedOrigins = []string{"https://ops.example.com"}
	a := newApp(t, api.Options{
		Bootstrap: &b,
		Runtime:   rt,
		Inbound: api.Inbound{
			Secrets:   func() webhooks.Secrets { return webhooks.Secrets{GitHub: "gh-secret"} },
			Publisher: memory.New(),
		},
		// A CODE HOST WHOSE EVERY HANDLER ANSWERS `served`, on a node of
		// every role: so a git request the gate lets through reaches a
		// handler, and one it refuses is told from a route that is absent.
		Code:    fixedCode{git: fakeGitRoutes{}, reads: &fakeCodeReads{}},
		Sources: queries.Sources{Company: active(t)},
	})
	return a
}

// send runs one authenticated request.
func send(t *testing.T, a *api.App, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, r)
	return rec
}

// refusedForDraining reports whether a response is the drain gate's refusal:
// the API's JSON, or — on a git route — the text/plain form git prints.
func refusedForDraining(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		return false
	}
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		return strings.HasPrefix(rec.Body.String(), "this node is draining")
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return false
	}
	return body["error"] == string(httpjson.CodeDraining)
}

// startsWork is every kind of request the gate refuses, one of each route
// family: the webhook edge (a delivery, and both of its GET landings, one of
// which acts and the other of which is refused with it), the config and
// credential writes, the setup pass, the deployment's controls (the backup,
// the retention gestures and the fleet broker's membership), a project's file
// bytes, the operator writes, the operator MCP surface, a new git fetch — its
// advertisement, which would begin one — a git push, its advertisement as well
// as its upload, and a verify of the node's git cache, which may rebuild it.
//
// WHETHER THIS FIXTURE MOUNTS THE ROUTE IS DECLARED, because the gate is
// middleware and runs BEFORE the mux: it refuses on path and method alone, so
// a refusal proves the rule whether or not a handler exists behind it. That is
// what makes the refusal case above meaningful for every entry — and it is
// also what would let an entry naming a path nothing serves sit here for ever
// looking exactly like one that works. [TestNothingIsRefusedForDrainingBeforeADrain]
// holds the declaration in BOTH directions for that reason, in the idiom
// [solo]'s roster guard uses: a mounted entry that starts answering 404 has
// drifted from the routes, and an unmounted one that stops is a fixture that
// grew a surface and left its declaration behind.
//
// [drainingApp] is a node with no store, no coordination store and no company,
// so the three surfaces that need one (/config, /secrets, /setup) and the ones
// that need a tracker, a knowledge base or an object store (the write surface
// under /worklet/items and /doclet/pages, a project's file bytes under
// /worklet/files, /operator/mcp and /operator/act/{tool}) are not mounted on
// it. Its git routes and the verify are, over a host that answers every
// request it is handed.
var startsWork = []struct {
	method, path string
	mounted      bool
}{
	{http.MethodPost, "/webhooks/github", true},
	{http.MethodPost, "/webhooks/slack/ceo", true},
	{http.MethodGet, "/webhooks/github-app?code=c&state=s", true},
	{http.MethodGet, "/webhooks/slack-oauth?code=c", true},
	{http.MethodPut, "/config", false},
	{http.MethodPatch, "/config", false},
	{http.MethodPost, "/config/reload", false},
	{http.MethodPost, "/config/revisions/r1/revert", false},
	{http.MethodPut, "/secrets/GITHUB_TOKEN", false},
	{http.MethodDelete, "/secrets/GITHUB_TOKEN", false},
	{http.MethodPost, "/setup/integrations/github/provision", false},
	{http.MethodPost, "/backup", true},
	{http.MethodPost, "/worklet/retention/ack", true},
	{http.MethodPost, "/fleet/broker/remove/node-c", true},
	{http.MethodPost, "/fleet/broker/remove-peer/abcdefgh", true},
	{http.MethodPut, "/worklet/files/ENG/report.md", false},
	{http.MethodDelete, "/worklet/files/ENG/report.md", false},
	{http.MethodPost, "/worklet/items/ENG-1/purge", false},
	{http.MethodPatch, "/worklet/items/ENG-1", false},
	{http.MethodPost, "/doclet/pages/p-1/restore", false},
	{http.MethodPost, "/operator/mcp", false},
	{http.MethodPost, "/operator/act/create_work_item", false},
	{http.MethodGet, "/git/r.git/info/refs?service=git-upload-pack", true},
	{http.MethodGet, "/git/r.git/info/refs?service=git-receive-pack", true},
	{http.MethodPost, "/git/r.git/git-receive-pack", true},
	{http.MethodPost, "/code/repos/web/verify", true},
}

func TestADrainRefusesEveryRequestThatWouldStartWork(t *testing.T) {
	t.Parallel()
	a := drainingApp(t, true)
	for _, req := range startsWork {
		rec := send(t, a, req.method, req.path)
		if !refusedForDraining(t, rec) {
			t.Errorf("%s %s answered %d %s during a drain, want the draining 503",
				req.method, req.path, rec.Code, rec.Body.String())
			continue
		}
		// A RETRY HINT, because the refusal is the one failure here that
		// is certain to pass: the caller belongs on another node.
		want := strconv.Itoa(int(api.DrainRetryAfter.Seconds()))
		if got := rec.Header().Get("Retry-After"); got != want {
			t.Errorf("%s %s: Retry-After = %q, want %q", req.method, req.path, got, want)
		}
	}
}

func TestNothingIsRefusedForDrainingBeforeADrain(t *testing.T) {
	t.Parallel()
	// The counterfactual. Nothing above is refused for draining on a node
	// that is not draining: the gate is the drain's, and a gate shut all
	// the time would pass the case above.
	//
	// And the declaration is held in both directions, so neither half of
	// that can go quiet. A route this fixture mounts must reach its handler
	// — whatever the handler then says — because an entry that silently
	// started answering 404 would pass this case and the refusal case
	// alike, the gate being middleware that never consults the mux. An
	// entry declared unmounted must still be unmounted, so a fixture that
	// grows a surface cannot leave a stale `false` behind saying the route
	// is untested when it is not.
	a := drainingApp(t, false)
	for _, req := range startsWork {
		rec := send(t, a, req.method, req.path)
		if refusedForDraining(t, rec) {
			t.Errorf("%s %s was refused for draining on a node that is not draining",
				req.method, req.path)
			continue
		}
		switch routed := rec.Code != http.StatusNotFound; {
		case req.mounted && !routed:
			t.Errorf("%s %s answered 404 on a serving node: this entry is "+
				"declared mounted, so either the route moved or the fixture "+
				"stopped mounting it, and the refusal case would not notice "+
				"either", req.method, req.path)
		case !req.mounted && routed:
			t.Errorf("%s %s now reaches a handler (%d): flip its `mounted` to "+
				"true, so this case starts holding the route rather than "+
				"reporting it untested", req.method, req.path, rec.Code)
		}
	}
}

// A DRAINING NODE FINISHES A FETCH AND BEGINS NO GIT WORK. A fetch is git
// processes, and a sync from the object store before them, so a draining node
// begins none: a fetch's advertisement is refused, which is where every new
// one starts. What it serves is the upload-pack RPC, which continues a fetch
// already begun — here or on a peer, since git keeps no state between an
// exchange's requests — and a refusal there would break a fetch half done.
// A push is refused at every request of it, its advertisement included,
// because the GET is where git prints a refusal's reason — on the POST it
// prints only `RPC failed; HTTP 503`. Every refusal is in git's own form,
// text/plain, since git prints the API's JSON as nothing at all. A coding
// box's git, classified as a person's, is drained by the same rules.
func TestADrainingNodeFinishesAFetchAndBeginsNoGitWork(t *testing.T) {
	t.Parallel()
	continues := []struct{ method, path string }{
		{http.MethodPost, "/git/r.git/git-upload-pack"},
		{http.MethodPost, "/box/git/r.git/git-upload-pack"},
	}
	begins := []struct{ method, path string }{
		// A fetch's advertisement, however it asks.
		{http.MethodGet, "/git/r.git/info/refs?service=git-upload-pack"},
		{http.MethodGet, "/git/r.git/info/refs"},
		{http.MethodHead, "/git/r.git/info/refs?service=git-upload-pack"},
		// Every request of a push.
		{http.MethodGet, "/git/r.git/info/refs?service=git-receive-pack"},
		{http.MethodPost, "/git/r.git/git-receive-pack"},
		// A coding box's advertisement and push.
		{http.MethodGet, "/box/git/r.git/info/refs?service=git-upload-pack"},
		{http.MethodPost, "/box/git/r.git/git-receive-pack"},
	}
	for _, draining := range []bool{true, false} {
		a := drainingApp(t, draining)
		for _, req := range continues {
			// Past the gate, to the code host behind it, which answers
			// every request it is handed.
			rec := send(t, a, req.method, req.path)
			if refusedForDraining(t, rec) || rec.Code != http.StatusOK {
				t.Errorf("draining=%v: %s %s answered %d %q, want it past the gate",
					draining, req.method, req.path, rec.Code, rec.Body.String())
			}
		}
		for _, req := range begins {
			rec := send(t, a, req.method, req.path)
			if !draining {
				if refusedForDraining(t, rec) || rec.Code != http.StatusOK {
					t.Errorf("%s %s answered %d %q on a node that is not draining, want it past the gate",
						req.method, req.path, rec.Code, rec.Body.String())
				}
				continue
			}
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s answered %d during a drain, want 503", req.method, req.path, rec.Code)
				continue
			}
			if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("%s %s refused as %q: git prints a refusal only when it is text/plain",
					req.method, req.path, got)
			}
			want := strconv.Itoa(int(api.DrainRetryAfter.Seconds()))
			if got := rec.Header().Get("Retry-After"); got != want {
				t.Errorf("%s %s: Retry-After = %q, want %q", req.method, req.path, got, want)
			}
			const sentence = "this node is draining for a shutdown and begins no new fetch or push: " +
				"retry in 30 s, against another node or once it has restarted\n"
			if got := rec.Body.String(); got != sentence {
				t.Errorf("%s %s said %q, want %q", req.method, req.path, got, sentence)
			}
		}
	}
}

// holdThroughGate sends one request through a's drain gate into a handler that
// holds it until the returned release is called, and returns once the handler
// holds it — or, for a request the gate refuses, once the refusal is written.
// ended is closed when the request has been answered.
func holdThroughGate(t *testing.T, a *api.App, method, path string) (release func(), ended <-chan struct{}) {
	t.Helper()
	held, let := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	h := a.ThroughDrainGate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(held)
		<-let
	}))
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
	}()
	select {
	case <-held:
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s %s never reached the handler", method, path)
	}
	var once sync.Once
	release = func() { once.Do(func() { close(let) }) }
	t.Cleanup(release)
	return release, done
}

// drainGit runs a.DrainGit on its own goroutine and hands back its answer.
func drainGit(ctx context.Context, a *api.App) <-chan error {
	out := make(chan error, 1)
	go func() { out <- a.DrainGit(ctx) }()
	return out
}

// A DRAIN FINISHES THE GIT REQUESTS IT IS SERVING. A clone or a push cut by the
// listener's close is not resumed: git begins it again, from nothing, on a
// peer, and the fleet pays its pack, its transfer slot and its sync from the
// object store twice, at every rolling restart. So the node's shutdown waits,
// once the turns are done, for every git request it is serving — a fetch
// continued during the drain, and a push or a fetch begun before it — and
// then for the quiet an exchange's next request would break.
func TestADrainFinishesTheGitRequestsItIsServing(t *testing.T) {
	t.Parallel()
	const gap = 100 * time.Millisecond
	for _, tc := range []struct {
		name         string
		draining     bool
		method, path string
	}{
		// The RPC a draining node goes on serving.
		{"a fetch continued during the drain", true, http.MethodPost, "/git/r.git/git-upload-pack"},
		// What was running when the drain began, a push included: it
		// passed the gate before the gate refused pushes.
		{"a push begun before the drain", false, http.MethodPost, "/git/r.git/git-receive-pack"},
		{"a clone's advertisement begun before the drain", false, http.MethodGet,
			"/git/r.git/info/refs?service=git-upload-pack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := drainingApp(t, tc.draining)
			api.SetGitExchangeGap(a, gap)
			release, ended := holdThroughGate(t, a, tc.method, tc.path)
			drained := drainGit(t.Context(), a)

			// Held: the drain waits however long the request takes.
			select {
			case err := <-drained:
				t.Fatalf("the drain ended (%v) while %s %s was still being served",
					err, tc.method, tc.path)
			case <-time.After(5 * gap):
			}

			released := time.Now()
			release()
			<-ended
			select {
			case err := <-drained:
				if err != nil {
					t.Fatalf("the drain gave up: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the drain never ended after the last git request did")
			}
			// AND THEN THE QUIET: an exchange's next request follows
			// the last answer by a round trip, so the drain listens a
			// gap longer before it lets the listener close under one.
			if waited := time.Since(released); waited < gap {
				t.Errorf("the drain ended %v after the last git request, inside the %v an "+
					"exchange's next request is waited for", waited, gap)
			}
		})
	}
}

// THE NEXT REQUEST OF AN EXCHANGE KEEPS THE DRAIN WAITING. Between two requests
// of one fetch the node serves nothing of it — v2's ls-refs and then fetch,
// v0's negotiation one round a POST — so a drain that ended the instant
// nothing was running would close the listener under a fetch half done.
func TestADrainWaitsForAnExchangesNextRequest(t *testing.T) {
	t.Parallel()
	// Room for the scheduler: the case starts the second request at once,
	// and a gap it could miss would fail the case rather than the code.
	const gap = 2 * time.Second
	a := drainingApp(t, true)
	api.SetGitExchangeGap(a, gap)

	releaseFirst, firstEnded := holdThroughGate(t, a, http.MethodPost, "/git/r.git/git-upload-pack")
	drained := drainGit(t.Context(), a)
	releaseFirst()
	<-firstEnded
	// The exchange's next round, inside the gap.
	releaseNext, nextEnded := holdThroughGate(t, a, http.MethodPost, "/git/r.git/git-upload-pack")

	select {
	case err := <-drained:
		t.Fatalf("the drain ended (%v) under an exchange's next request", err)
	case <-time.After(gap + gap/2):
	}
	released := time.Now()
	releaseNext()
	<-nextEnded
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("the drain gave up: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the drain never ended after the exchange's last request did")
	}
	if waited := time.Since(released); waited < gap {
		t.Errorf("the drain ended %v after the exchange's last request, inside its %v gap", waited, gap)
	}
}

// A NODE SERVING NO GIT HAS NONE TO FINISH. The wait is for git requests the gate
// let through: one it refused started nothing, and every other request — a
// dashboard's socket, a coding run's bridge session — is the listener's to
// end, so none of them holds the shutdown here.
func TestADrainHasNoGitToFinishOnANodeServingNone(t *testing.T) {
	t.Parallel()
	a := drainingApp(t, true)
	// Long enough that a wait for it is a failure rather than a delay.
	api.SetGitExchangeGap(a, time.Hour)

	// Refused by the drain: started nothing, so it is nothing to wait for.
	_, refused := holdThroughGate(t, a, http.MethodPost, "/git/r.git/git-receive-pack")
	<-refused
	// Served and held, but no git request.
	holdThroughGate(t, a, http.MethodPost, "/mcp/run-token")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := <-drainGit(ctx, a); err != nil {
		t.Errorf("the drain waited for something on a node serving no git: %v", err)
	}
}

// THE WAIT IS THE CALLER'S TO BOUND. Like the turns', it has no deadline of its
// own — the supervisor's kill grace or a second interrupt is the bound — so it
// ends when its context does, and says so.
func TestADrainOfGitEndsWithItsContext(t *testing.T) {
	t.Parallel()
	a := drainingApp(t, true)
	holdThroughGate(t, a, http.MethodPost, "/git/r.git/git-upload-pack")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	select {
	case err := <-drainGit(ctx, a):
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the drain answered %v when its context ended, want the context's error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not end with its context")
	}
}

// drainsWhileAsked is a node whose drain begins WHILE A REQUEST IS ASKING
// whether it has: the first answer is "not draining", and before that answer
// is returned the drain begins — every later read says so — and during runs,
// standing for everything the shutdown does next: the turns' drain, then the
// git wait ([api.App.DrainGit]).
type drainsWhileAsked struct {
	fakeRuntime
	begun  atomic.Bool
	once   sync.Once
	during func()
}

func (d *drainsWhileAsked) ShuttingDown() bool {
	answer := d.begun.Load()
	d.once.Do(func() {
		d.begun.Store(true)
		d.during()
	})
	return answer
}

// Snapshot answers the same fact ShuttingDown does, as [fakeRuntime]'s does.
func (d *drainsWhileAsked) Snapshot(ctx context.Context) api.RuntimeState {
	s := d.fakeRuntime.Snapshot(ctx)
	s.ShuttingDown = d.begun.Load()
	return s
}

// A GIT REQUEST THE GATE LETS THROUGH IS ONE THE DRAIN WAITS FOR, however the
// two interleave. The most hostile interleaving is the whole of a drain falling
// in the instant after a push has been told the node is not draining: the turns
// finish and the git wait looks before the push takes its next step. Counted
// only after that answer, the push was invisible to the look, so the wait found
// nothing running and ended, and the listener closed under a push the gate had
// let through. The gate counts a git request BEFORE it asks, so whichever comes
// first, the wait sees it or the gate sees the drain.
func TestAGitRequestTheGateLetsThroughIsOneTheDrainWaitsFor(t *testing.T) {
	t.Parallel()
	rt := &drainsWhileAsked{fakeRuntime: fakeRuntime{state: api.RuntimeState{Posture: "serve"}}}
	a := drainingAppOn(t, rt)
	var waited error
	rt.during = func() {
		// Short, because a wait that saw the push waits until the push
		// has been answered, which is after this returns.
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		waited = a.DrainGit(ctx)
	}
	served := false
	h := a.ThroughDrainGate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/git/r.git/git-receive-pack", nil))

	if !served {
		t.Fatal("the push was refused, though the node was not draining when it asked")
	}
	if !errors.Is(waited, context.DeadlineExceeded) {
		t.Errorf("the drain's git wait answered %v with a push it had let through still to be "+
			"served, want it still waiting when its context ended", waited)
	}
}

func TestADrainingNodeStillServesItsProbesAndItsReads(t *testing.T) {
	t.Parallel()
	a := drainingApp(t, true)

	// Liveness stays 200, which is the whole reason the listener outlives
	// the drain: an orchestrator that could not reach it would kill the
	// node in the middle of the turns the drain exists to finish.
	if rec := send(t, a, http.MethodGet, "/health"); rec.Code != http.StatusOK {
		t.Errorf("/health answered %d during a drain, want 200", rec.Code)
	} else if !strings.Contains(rec.Body.String(), api.StatusShuttingDown) {
		t.Errorf("/health does not say the node is shutting down: %s", rec.Body.String())
	}

	// Readiness answers from its own handler, with the reason, rather than
	// from the gate: it is a read, and its 503 is the one that steers
	// traffic.
	rec := send(t, a, http.MethodGet, "/ready")
	var ready map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ready)
	if rec.Code != http.StatusServiceUnavailable || ready["reason"] != api.ReasonDraining {
		t.Errorf("/ready answered %d %v during a drain, want 503 naming the drain",
			rec.Code, ready)
	}

	// Every other read keeps answering, which is how an operator watches
	// the drain: the dashboard, the REST reads (the code reads among them)
	// and the query surface.
	for _, path := range []string{"/agents", "/org", "/dashboard", "/query/viewer", "/code/repos"} {
		if rec := send(t, a, http.MethodGet, path); rec.Code != http.StatusOK {
			t.Errorf("GET %s answered %d during a drain, want 200", path, rec.Code)
		}
	}
	// And the reads of a guarded surface, and a preflight, are reads too —
	// a verification's outcome among them, which an operator who began one
	// before the drain reads while the node drains.
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/config"},
		{http.MethodOptions, "/config"},
		{http.MethodGet, "/code/repos/web/verify/0190d2f2-0000-7000-8000-000000000000"},
	} {
		if rec := send(t, a, req.method, req.path); refusedForDraining(t, rec) {
			t.Errorf("%s %s was refused for draining, and it starts nothing",
				req.method, req.path)
		}
	}
}

func TestADrainKeepsTheEdgesOfTheRunsItCannotWaitFor(t *testing.T) {
	t.Parallel()
	// The sandbox bridge and the telemetry edge carry the tool calls and
	// the spans of coding runs that started before the drain. A detached
	// run outlives the turn that started it, so the drain never waits on
	// one: refusing these shortens no drain and only breaks a run against
	// the node still holding its bridge session and its receiver.
	a := drainingApp(t, true)
	for _, req := range []struct{ method, path string }{
		{http.MethodPost, "/mcp/run-token"},
		{http.MethodDelete, "/mcp/run-token"},
		{http.MethodGet, "/mcp/run-token/steer/notes"},
		{http.MethodPost, "/mcp/run-token/steer/read"},
		{http.MethodPost, "/otlp/run-token/v1/traces"},
	} {
		if rec := send(t, a, req.method, req.path); refusedForDraining(t, rec) {
			t.Errorf("%s %s was refused for draining: an in-flight run lost its "+
				"edge", req.method, req.path)
		}
	}
}

func TestADrainRefusalCarriesASentenceForThePersonWhoSentIt(t *testing.T) {
	t.Parallel()
	// The CLI and the dashboard both render `detail` and `hint` beside the
	// code, and a bare "draining" tells an operator what happened without
	// telling them what to do about it.
	rec := send(t, drainingApp(t, true), http.MethodPut, "/config")
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["detail"] == "" || body["hint"] == "" {
		t.Errorf("the refusal carries no detail or hint: %v", body)
	}
}

func TestACrossOriginDashboardCanReadADrainRefusal(t *testing.T) {
	t.Parallel()
	// The gate sits INSIDE the browser posture. Outside it, a permitted
	// origin's write would be refused with no allow-origin header, and the
	// browser would report an opaque network failure instead of the drain.
	a := drainingApp(t, true)
	r := httptest.NewRequest(http.MethodPatch, "/config", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Origin", "https://ops.example.com")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, r)
	if !refusedForDraining(t, rec) {
		t.Fatalf("PATCH /config answered %d during a drain", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://ops.example.com" {
		t.Errorf("the refusal permitted origin %q, so the browser cannot read it", got)
	}
}

func TestACredentialIsStillTheFirstQuestionDuringADrain(t *testing.T) {
	t.Parallel()
	// Inside the guard: a write with no token is unauthorized whatever the
	// node is doing, so a drain tells an anonymous caller nothing a guarded
	// route would not.
	a := drainingApp(t, true)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodPut, "/config", strings.NewReader("{}"))))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated write answered %d during a drain, want 401", rec.Code)
	}
}

func TestAProcessWithNoEngineHasNothingToDrain(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b})
	if rec := send(t, a, http.MethodPost, "/backup"); refusedForDraining(t, rec) {
		t.Error("a process with no engine refused a write for draining")
	}
}

// A STOPPING CODE HOST TELLS GIT WHAT A DRAINING NODE TELLS EVERY OTHER
// CALLER: the one wait that takes a stopping node out of a balancer's
// rotation. The code host imports nothing of the API, so its figure is its
// own constant, held to this one here.
func TestTheGitStopRetryIsTheDrainRetry(t *testing.T) {
	if host.StoppingRetryAfter != api.DrainRetryAfter {
		t.Errorf("a stopping code host answers Retry-After %v, a draining node %v: a git client and a balancer "+
			"would be told two waits for one stop", host.StoppingRetryAfter, api.DrainRetryAfter)
	}
}
