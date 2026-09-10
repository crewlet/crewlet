package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// Certifying this third-party app against the one suite every reconciler passes.
//
// # Why the harness is here and not at the loop
//
// integrationtest states [integration.Reconciler]'s safety contract and calls
// its write-counting hook required, because that clause is "the one most
// likely to be wrong". Its only caller drove a stub whose pass returned
// (nil, nil), so "a converged pass writes nothing" was true because nothing
// happened, and no reconciler in this tree was certified at all.
//
// The clause cannot be certified anywhere but here. What the suite needs is a
// WORLD that is already converged and a count of what a pass wrote to it, and
// only the third-party app's own package can stand one up: the engine seam
// hands out a live client and a live company, and a suite driven through it
// would be certifying the adapter again.
//
// # What is restated, and what that costs
//
// A GitHub pass is TWO halves — [github.Reconcile] over the organization's
// credential and webhooks, and [github.ReconcileSeatApps] over each agent's
// own app — and engine.githubPass.Run is what composes them. This file
// composes them the same way, which means the composition itself is the one
// thing here that is a copy rather than the original: a change to the order
// or to which findings are appended would pass here and be wrong there. The
// two halves, which are all of the behaviour, are the real ones.
//
// # Two worlds, because the second is where the hole was
//
// `integrations.github.token` is optional and the engine hands a nil client
// for an empty one, so a company with no organization credential is an
// ordinary supported deployment — and it was the one shape whose pass could
// not fail. A harness that only ever configured a token would have seen every
// case green while a cancelled pass over the other shape reported a converged
// integration. Both are certified, and the case that separates them has its
// own named test below.

// The converged world's fixed points. Constants rather than literals scattered
// through the handler, because the fixture only proves anything while the
// address the hook points at is byte-identical to the one the pass computes.
const (
	convergedOrgName = "acme"
	convergedRepo    = "acme/api"
	convergedBase    = "https://engine.example.com"
	orgToken         = "ghp_engine"
	seatToken        = "ghp_sre-lead"
)

// convergedWorld is a GitHub that already matches the company, and the log of
// everything a pass wrote to it.
//
// A LOG RATHER THAN A COUNTER, because the number is what fails the suite and
// the route is what a person then has to find. "a pass made 1 write(s)" sends
// somebody reading the whole reconcile; "PATCH /orgs/acme/hooks/1" names the
// branch.
//
// It spans BOTH surfaces a pass can write to, and the second is the one that
// costs an outage. GitHub's own writes rewrite a delivery path; the engine's
// writes — an installation recorded onto the company document, a stale app
// forgotten, a freshly minted webhook secret sealed — rewrite the company and
// rotate the key every running deployment is verifying deliveries with. A
// harness counting only the HTTP half would certify a pass that re-sealed a
// credential every few minutes for ever.
type convergedWorld struct {
	t *testing.T

	// logins is what each bearer token authenticates as, built before the
	// server starts and never written again: the seat walk is concurrent,
	// so a map a handler could write would be this package's next -race
	// failure.
	logins map[string]string

	// hooks is the webhook listing every hook route answers with. A field
	// rather than a constant so the same world can be stood up NOT
	// converged, which is what proves the counter can fail.
	hooks []byte

	// installations answers /app/installations/{id} per seat.
	installations map[string]string

	// ONE LOCK OVER BOTH LOGS, because the seat walk is concurrent and a
	// pull request's participants are read on two goroutines: a slice
	// appended to straight from a handler closure is this package's
	// documented -race failure.
	mu     sync.Mutex
	writes []string
	reads  []string
}

// wrote records one write, from whichever goroutine made it.
func (w *convergedWorld) wrote(what string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, what)
}

// read records one route the pass asked for.
//
// A GREEN SUITE OVER A WORLD NOBODY TOUCHED IS THE FAILURE THIS EXISTS FOR.
// Every clause the suite checks — no writes, no findings, two passes agreeing
// — is satisfied perfectly by a pass that returned before making a single
// request, which is exactly what the stub this harness replaces did. So what
// was READ is recorded beside what was written, and
// [TestTheConvergedWorldIsActuallyRead] asserts on it.
func (w *convergedWorld) read(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reads = append(w.reads, path)
}

// mutations is the count the suite reads, and the routes are kept for the
// failure message.
func (w *convergedWorld) mutations() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.writes)
}

// writeLog and readLog are copies, so a caller reading one cannot race the
// handler still appending to it.
func (w *convergedWorld) writeLog() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.writes...)
}

func (w *convergedWorld) readLog() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.reads...)
}

// mutationAt names what a request CHANGED at GitHub, or "" for a read.
//
// THE ROUTE DECIDES, not the method, and the difference is not academic: some
// hosts model a listing as a POST — Atlassian's workspace discovery is one —
// so a counter that read every non-GET as a write would make the suite's
// load-bearing clause impossible to pass rather than hard to pass, and an
// impossible clause is what gets a suite weakened instead of a third-party app
// fixed. The route is also what a person needs when the count moves: "1
// write" sends somebody reading the whole reconcile, "the delivery path" names
// the branch.
//
// GitHub has no listing behind a POST, which is why the method still appears:
// it serves the hook LISTING and the hook CREATE from one path, so on a write
// route the method is what separates the two. The default arm is the backstop
// for a route a later build adds — an unrecognised non-GET counts, because a
// counter that silently stops seeing the write it exists to see is worse than
// no counter at all.
func mutationAt(method, path string) string {
	if method == http.MethodGet {
		return ""
	}
	switch {
	case strings.HasSuffix(path, "/access_tokens"):
		// A SEAT'S CREDENTIAL. Not on the reconcile path at all — it is
		// minted where a token is needed — and counted because a pass
		// that started minting one per tick is the failure the suite's
		// package doc opens with.
		return "a seat's credential"
	case strings.Contains(path, "/hooks"):
		// THE DELIVERY PATH: create, rewrite or remove, on the
		// organization or on one repository (internal/github/hooks.go).
		return "the delivery path"
	case strings.HasPrefix(path, "/app/installations/"):
		// DELETE here uninstalls a seat's app, which is what actually
		// revokes its access.
		return "a seat's installation"
	default:
		return "a route this counter has not been told about"
	}
}

// serve answers the converged world.
func (w *convergedWorld) serve(rw http.ResponseWriter, r *http.Request) {
	if what := mutationAt(r.Method, r.URL.Path); what != "" {
		w.wrote(r.Method + " " + r.URL.Path + " (" + what + ")")
		// COUNTED AND ANSWERED, never also failed here: the count is this
		// suite's assertion, and a handler that raised as well would
		// report one write as two and bury "a converged pass wrote"
		// under "unexpected route".
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"id":1}`))
		return
	}

	w.read(r.URL.Path)
	rw.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/user":
		login := w.logins[bearerOf(r)]
		if login == "" {
			// Errorf, NOT Fatal: this is the server's goroutine, where
			// Fatal ends the goroutine instead of the test and the
			// caller sees an unexplained EOF.
			w.t.Errorf("a pass authenticated at /user with a credential this " +
				"world does not know, so the fixture is not the one the pass " +
				"was built against")
			rw.WriteHeader(http.StatusUnauthorized)
			_, _ = rw.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		_, _ = fmt.Fprintf(rw, `{"login":%q}`, login)

	case r.URL.Path == "/orgs/"+convergedOrgName+"/hooks",
		r.URL.Path == "/repos/"+convergedRepo+"/hooks":
		_, _ = rw.Write(w.hooks)

	case r.URL.Path == "/repos/"+convergedRepo:
		_, _ = rw.Write([]byte(`{"full_name":"` + convergedRepo + `",` +
			`"archived":false,"private":false,` +
			`"permissions":{"admin":true,"push":true,"pull":true}}`))

	default:
		if body, ok := w.installations[r.URL.Path]; ok {
			_, _ = rw.Write([]byte(body))
			return
		}
		// EVERY OTHER ROUTE MEANS THE WORLD IS NOT CONVERGED, and each
		// one is a route the pass reaches only when something is
		// outstanding: GET /app/installations is the discovery walk a
		// seat with no recorded installation makes, and GET /app is the
		// probe that runs after a 404. Both end in an engine-side write,
		// so a fixture that quietly answered them would certify a pass
		// that rewrites the company document every tick.
		w.t.Errorf("a pass over a converged world asked for %s %s, which it "+
			"reaches only when something is outstanding", r.Method, r.URL.Path)
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"message":"Not Found"}`))
	}
}

// bearerOf is the credential a request carried.
func bearerOf(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// convergedSeatApps is the roster engine.Engine.githubSeatApps reads out of
// the company document, already handle-sorted the way SeatsFrom leaves it.
//
// Two seats on different tiers, because [github.Shortfall] compares what the
// installation holds against what the TIER asks for: one seat would let a
// fixture that answered every installation with one permission map pass while
// the tier was ignored entirely.
func convergedSeatApps(pem string) []github.SeatApp {
	return github.SeatsFrom([]github.SeatApp{
		{
			Handle: "eng-lead", Name: "Engineering Lead", Tier: github.TierFullAccess,
			AppID: 11, Slug: "acme-eng-lead", InstallationID: 101, Key: pem,
		},
		{
			Handle: "sre-lead", Name: "SRE Lead", Tier: github.TierReview,
			AppID: 12, Slug: "acme-sre-lead", InstallationID: 102, Key: pem,
		},
	})
}

// convergedOrgChart is the company these apps belong to.
//
// THE MODERN SHAPE, deliberately: `eng-lead` holds no `mcp_env.github`
// credential at all, because identity on this host is each agent's own app
// and a company on the current design has no personal access token anywhere.
// A fixture that handed every seat a token to make the pass quiet would have
// been testing a shape nobody runs — and would have hidden the finding that
// parked exactly that company in Degraded for ever.
//
// `sre-lead` holds one anyway, which is the mid-migration shape and the only
// way the credential walk gets exercised. The founder is a HUMAN seat, which
// holds no tool credential and must not be walked as though it did.
func convergedOrgChart() *org.Organization {
	agent := func(name, handle, token string) *org.Role {
		role := &org.Role{Name: name, DeclaredHandle: handle}
		if token != "" {
			role.MCPEnv = map[string]map[string]string{
				github.SeatEnv: {"GITHUB_TOKEN": token},
			}
		}
		return role
	}
	founder := agent("Founder", "founder", "")
	founder.Kind = org.KindHuman
	return &org.Organization{
		Name:  "Acme",
		Roles: []*org.Role{agent("Engineering Lead", "eng-lead", ""), founder},
		Units: []*org.Unit{{
			Name:  "Platform",
			Roles: []*org.Role{agent("SRE Lead", "sre-lead", seatToken)},
		}},
	}
}

// newConvergedWorld stands the whole thing up: a GitHub with nothing
// outstanding, and the two option sets a pass runs against it.
//
// `withOrgToken` picks which of the two supported shapes this is. False
// leaves [github.Options.Client] nil, which is what the engine hands a
// company whose `integrations.github.token` resolves empty.
// The RSA key arrives from the caller rather than being generated here,
// because [testKey] reports through *testing.T and this runs on the suite's
// own goroutine — and because one keygen per test beats one per case.
func newConvergedWorld(
	t *testing.T, tb integrationtest.TB, pem string, withOrgToken bool,
) (*convergedWorld, github.Options, github.SeatAppOptions) {
	// tb IS THE REPORTER, not t. New is called on the goroutine t.Run
	// started for one case, and (*testing.T).Fatal from a goroutine that is
	// not that test's own ends the wrong one — so what can fail here says
	// so through the TB the suite handed in, and t is used only for the
	// two things that are safe from anywhere: Cleanup, and the handler's
	// Errorf.
	tb.Helper()

	seats := convergedSeatApps(pem)

	// THE HOOK THE PASS WOULD WRITE, read back as though it were already
	// there. Both halves matter and neither is visible from the URL: a
	// disabled hook delivers nothing, and one missing an event delivers
	// everything but that one. The events are marshalled from the exported
	// slice rather than spelled out, so adding one to the parser moves the
	// fixture with it instead of leaving a converged pass rewriting a hook
	// on every tick.
	events, err := json.Marshal(github.WebhookEvents)
	if err != nil {
		tb.Fatalf("marshalling the webhook event list: %v", err)
	}
	world := &convergedWorld{
		t: t,
		logins: map[string]string{
			orgToken:  "crewlet-engine",
			seatToken: "sre-lead-bot",
		},
		hooks: fmt.Appendf(nil,
			`[{"id":1,"active":true,"events":%s,"config":{"url":%q}}]`,
			events, convergedBase+"/webhooks/github"),
		installations: map[string]string{},
	}
	for _, seat := range seats {
		// EXACTLY WHAT THE TIER ASKS FOR: less is a Shortfall finding,
		// and anything in github.Denied is an Excess one. Built from
		// Tier.Permissions so a tier that gains a permission does not
		// leave this fixture quietly reporting a shortfall for ever.
		perms, permErr := json.Marshal(seat.Tier.Permissions())
		if permErr != nil {
			tb.Fatalf("marshalling %s's tier permissions: %v", seat.Handle, permErr)
		}
		world.installations["/app/installations/"+strconv.FormatInt(seat.InstallationID, 10)] =
			fmt.Sprintf(`{"id":%d,"account":{"login":%q,"type":"Organization"},`+
				`"repository_selection":"all","suspended_at":null,"permissions":%s}`,
				seat.InstallationID, convergedOrgName, perms)
	}

	server := httptest.NewServer(http.HandlerFunc(world.serve))
	t.Cleanup(server.Close)

	cfg := &config.GitHub{
		Enabled: true, WebhookSecret: "s3cret",
		Provisioning: &config.GitHubProvisioning{
			Org:        convergedOrgName,
			Repos:      []string{convergedRepo},
			OrgWebhook: config.ContainerWebhookAuto,
		},
	}
	opts := github.Options{
		Config: cfg, Org: convergedOrgChart(),
		Value: func(v string) string { return v },
		// THE LOOP'S OWN SINK, wrapped rather than replaced. A node with
		// no keyring gets provision.ReadOnly, and a converged pass must
		// not reach it at all: a Record here is a webhook secret minted,
		// which invalidates the key every other deployment of this
		// company is verifying with.
		Sink:        countingSink{TokenSink: provision.ReadOnly(), world: world},
		WebhookBase: convergedBase,
		// NEVER TRUE. The loop never sets it, and true forces a delete
		// and a create on every target — a converged pass that could not
		// possibly be converged.
		RecreateWebhooks: false,
	}
	if withOrgToken {
		cfg.Token = orgToken
		client, clientErr := github.NewClient(github.ClientOptions{
			APIBase: server.URL, WebBase: server.URL, Token: orgToken,
		})
		if clientErr != nil {
			tb.Fatalf("building the org client: %v", clientErr)
		}
		opts.Client = client
	}

	seatOpts := github.SeatAppOptions{
		APIBase: server.URL, WebBase: server.URL, Org: convergedOrgName,
		Seats: seats,
		// COUNTED AND REFUSED. These are the pass's writes onto the
		// COMPANY DOCUMENT, and each one is reached only from a route
		// the converged fixture does not serve — so a call here is
		// either a fixture that drifted or a pass that adopts an
		// installation it already has recorded, every few minutes, for
		// ever.
		Record: func(_ context.Context, handle string, id int64) error {
			world.wrote(fmt.Sprintf("company document: record installation %d for %s",
				id, handle))
			return nil
		},
		Forget: func(_ context.Context, handle string) error {
			world.wrote("company document: forget the app for " + handle)
			return nil
		},
	}
	return world, opts, seatOpts
}

// countingSink is the loop's read-only sink with every Record counted.
//
// The sink is as much a surface this suite counts writes at as GitHub is:
// what it records is a credential, and one recorded on a converged world is a
// credential rotated on a timer — the exact failure integrationtest's package
// doc names.
type countingSink struct {
	provision.TokenSink
	world *convergedWorld
}

func (s countingSink) Record(ctx context.Context, name, value string) error {
	s.world.wrote("secret store: seal " + name)
	return s.TokenSink.Record(ctx, name, value)
}

// githubReconciler is the pass the loop runs, composed the way
// engine.githubPass.Run composes it.
type githubReconciler struct {
	opts  github.Options
	seats github.SeatAppOptions
}

func (*githubReconciler) Kind() integration.Kind { return integration.KindGitHub }

func (r *githubReconciler) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := github.Reconcile(ctx, r.opts)
	if err != nil {
		return nil, fmt.Errorf("github pass: %w", err)
	}
	findings := res.Findings()
	if len(r.seats.Seats) == 0 {
		return findings, nil
	}
	apps, appsErr := github.ReconcileSeatApps(ctx, r.seats)
	if appsErr != nil {
		return nil, fmt.Errorf("github pass: %w", appsErr)
	}
	return append(findings, apps.Findings...), nil
}

// THE CONTRACT, AGAINST THE REAL RECONCILER, over a company that holds an
// organization credential.
func TestGitHubMeetsTheReconcilerContract(t *testing.T) {
	t.Parallel()
	runGitHubContract(t, true)
}

// AND OVER ONE THAT DOES NOT, which is a supported deployment rather than a
// degraded one — `integrations.github.token` is optional, and each agent's own
// app answers the question it used to. It is also the shape whose pass could
// not fail: see [TestACancelledPassWithNoOrgCredentialRaisesRatherThanReportingHealth].
func TestGitHubMeetsTheReconcilerContractWithNoOrgCredential(t *testing.T) {
	t.Parallel()
	runGitHubContract(t, false)
}

func runGitHubContract(t *testing.T, withOrgToken bool) {
	t.Helper()
	// A WORLD PER CASE. The suite calls New once per case and reads
	// Mutations around a single pass, so one world shared across the seven
	// would let case 3 count the requests case 2 made. The pointer is
	// atomic because New and Mutations are called from different
	// goroutines — t.Run runs each case in its own.
	// ONE KEY FOR THE WHOLE TEST. Every case stands up its own world, and
	// generating a 2048-bit RSA key fourteen times over proves nothing the
	// first one does not.
	_, pem := testKey(t)

	var current atomic.Pointer[convergedWorld]
	integrationtest.Run(t, integrationtest.Reconciler{
		New: func(tb integrationtest.TB) integration.Reconciler {
			tb.Helper()
			world, opts, seats := newConvergedWorld(t, tb, pem, withOrgToken)
			current.Store(world)
			return &githubReconciler{opts: opts, seats: seats}
		},
		Mutations: func() int {
			world := current.Load()
			if world == nil {
				t.Fatal("Mutations was read before a world was stood up")
			}
			if n := world.mutations(); n > 0 {
				// THE ROUTES, not just the count: the suite's own
				// message says how many writes there were, and the
				// branch that made them is what somebody then has to
				// find.
				t.Logf("writes at this world: %v", world.writeLog())
			}
			return world.mutations()
		},
	})
}

// A TEST THAT CANNOT FAIL IS WORSE THAN NO TEST, and that applies first to
// the counter the whole suite leans on.
//
// The fixture above is converged, so every case passes with a counter that is
// wired to nothing at all — an atomic.Int64 nobody increments reads zero
// exactly as reliably as a correct one. This stands up the same world with
// one field changed, the field that is invisible from the delivery address: a
// hook that is present, at the right URL, carrying every event, and DISABLED.
// The pass must rewrite it, and this harness must see that.
func TestTheWriteCounterSeesAWriteWhenThereIsOneToSee(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	world, opts, seats := newConvergedWorld(t, t, pem, true)

	// The one difference from converged. A disabled hook delivers nothing,
	// which is why converged() looks at more than the URL.
	events, err := json.Marshal(github.WebhookEvents)
	if err != nil {
		t.Fatal(err)
	}
	world.hooks = fmt.Appendf(nil,
		`[{"id":1,"active":false,"events":%s,"config":{"url":%q}}]`,
		events, convergedBase+"/webhooks/github")

	rec := &githubReconciler{opts: opts, seats: seats}
	if _, err := rec.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if world.mutations() == 0 {
		t.Fatal("a pass that had to re-enable a disabled hook was counted as " +
			"writing nothing, so the clause the whole suite exists for would " +
			"pass over any third-party app at all")
	}
	if got := world.writeLog(); !strings.Contains(got[0], "/hooks/1") {
		t.Errorf("the write was recorded as %q, which does not name the hook "+
			"route a person then has to find", got[0])
	}
}

// AND THE COUNTER AGREES WITH WHAT THE PACKAGE CAN ACTUALLY DO.
//
// The rule is that the ROUTE decides what a request changed, because a host
// that models a listing as a POST would make the load-bearing clause
// impossible to pass under a method-only counter — and the label is what a
// person reads when the count moves. Both are claims about routes this
// package reaches, so both are pinned here, route by route, rather than left
// as prose above the function.
//
// The reads are as much of the point as the writes: a counter that called the
// hook LISTING a mutation would fail case 3 over a converged world and send
// somebody looking for a write that never happened.
func TestTheWriteCounterClassifiesEveryRouteAPassCanReach(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		method, path string
		want         string
	}{
		// EVERY READ THIS PACKAGE MAKES, including the two a converged
		// pass must not reach — they are still reads when it does.
		"the credential probe":     {http.MethodGet, "/user", ""},
		"the org hook listing":     {http.MethodGet, "/orgs/acme/hooks", ""},
		"the repo hook listing":    {http.MethodGet, "/repos/acme/api/hooks", ""},
		"reading a repository":     {http.MethodGet, "/repos/acme/api", ""},
		"reading an installation":  {http.MethodGet, "/app/installations/101", ""},
		"the installation listing": {http.MethodGet, "/app/installations", ""},
		"the app probe":            {http.MethodGet, "/app", ""},

		// AND EVERY WRITE IT CAN MAKE.
		"creating the org hook":  {http.MethodPost, "/orgs/acme/hooks", "the delivery path"},
		"rewriting the org hook": {http.MethodPatch, "/orgs/acme/hooks/1", "the delivery path"},
		"removing the org hook":  {http.MethodDelete, "/orgs/acme/hooks/1", "the delivery path"},
		"creating a repo hook":   {http.MethodPost, "/repos/acme/api/hooks", "the delivery path"},
		"rewriting a repo hook": {
			http.MethodPatch, "/repos/acme/api/hooks/1", "the delivery path"},
		"removing a repo hook": {
			http.MethodDelete, "/repos/acme/api/hooks/1", "the delivery path"},
		"minting a seat's token": {
			http.MethodPost, "/app/installations/101/access_tokens", "a seat's credential"},
		"uninstalling a seat app": {
			http.MethodDelete, "/app/installations/101", "a seat's installation"},

		// AND A WRITE ROUTE THIS BUILD DOES NOT HAVE still counts, which
		// is the whole reason the default arm is not "not a mutation".
		"a route added later": {
			http.MethodPut, "/orgs/acme/somewhere-new",
			"a route this counter has not been told about"},
	} {
		if got := mutationAt(tc.method, tc.path); got != tc.want {
			t.Errorf("%s (%s %s) was classified as %q, want %q",
				name, tc.method, tc.path, got, tc.want)
		}
	}
}

// AND THE WORLD IS ACTUALLY READ, which no clause of the suite can check.
//
// "No writes", "no findings" and "two passes agree" are all satisfied
// perfectly by a pass that returns before making one request — which is what
// the stub this harness replaces did, and why a green suite over it meant
// nothing at all. So the routes the fixture served are asserted directly:
// each one is a read the pass has to make before it may conclude anything,
// and a fixture that stopped being reached would otherwise still be green.
func TestTheConvergedWorldIsActuallyRead(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	world, opts, seats := newConvergedWorld(t, t, pem, true)

	res, err := github.Reconcile(t.Context(), opts)
	if err != nil {
		t.Fatalf("Reconcile over a converged world: %v", err)
	}
	apps, err := github.ReconcileSeatApps(t.Context(), seats)
	if err != nil {
		t.Fatalf("ReconcileSeatApps over a converged world: %v", err)
	}

	got := world.readLog()
	for _, want := range []string{
		// THE CREDENTIAL PROBE, which is what the whole pass is ordered
		// around: read before write, so a dead credential never leaves
		// GitHub delivering to an engine that cannot enrich anything.
		"/user",
		// THE ORG HOOK, whose convergence is what stops the repository
		// walk one line later.
		"/orgs/" + convergedOrgName + "/hooks",
		// AND EACH SEAT'S INSTALLATION, read back rather than assumed:
		// the operator can revoke it at GitHub and tell nobody.
		"/app/installations/101",
		"/app/installations/102",
	} {
		if !containsRoute(got, want) {
			t.Errorf("a converged pass never asked for %s, so the fixture is "+
				"not what made this suite green — the routes read were %v",
				want, got)
		}
	}

	// AND IT CONCLUDED THE THING A CONVERGED WORLD MEANS. Zero findings is
	// half of it and the weaker half: a pass that read nothing reports zero
	// too. Both seats READY is the half only a pass that read the
	// installations back can reach.
	if findings := append(res.Findings(), apps.Findings...); len(findings) != 0 {
		t.Fatalf("a converged world produced %+v", findings)
	}
	if len(apps.Ready) != 2 {
		t.Errorf("Ready = %v, want both seats: a converged installation on the "+
			"tier its seat is set to is the only state an agent can act from",
			apps.Ready)
	}
	// AND NOTHING WAS ADOPTED OR FORGOTTEN, which are the two engine-side
	// writes. Counted above as well; asserted here because the count says
	// only that the total did not move.
	if len(apps.Adopted) != 0 || len(apps.Forgotten) != 0 {
		t.Errorf("a converged pass adopted %v and forgot %v", apps.Adopted, apps.Forgotten)
	}
}

// containsRoute reports a path the world served.
func containsRoute(log []string, want string) bool {
	for _, path := range log {
		if path == want {
			return true
		}
	}
	return false
}

// A CANCELLED PASS REPORTS A FAULT RATHER THAN HEALTH — the suite's seventh
// clause, and the two root causes that used to break it on this host.
//
// The two answers are opposite claims to the loop: an error is a fault it
// retries, and an empty findings list is a statement that everything is fine,
// trusted for a full settled interval. A node shutting down went through both
// paths below and came out of each with (no findings, nil error).

// THE FIRST: no organization credential, which is a supported deployment.
//
// [github.Reconcile]'s nil-client arm is the ONE way out of that function
// that touches no network, so it was the one a dead context could not fail on
// its own — and it is reached by exactly the companies that never set
// `integrations.github.token`, which is optional and which each agent's own
// app has made unnecessary. With a token the pass fails at the credential
// probe and this was invisible, which is why the harness above certifies both
// shapes rather than the convenient one.
func TestACancelledPassWithNoOrgCredentialRaisesRatherThanReportingHealth(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := github.Reconcile(ctx, github.Options{
		Config: &config.GitHub{Enabled: true}, WebhookBase: convergedBase,
	})
	if err == nil {
		t.Fatalf("a cancelled pass answered %+v with no error, which the loop "+
			"reads as a converged integration", res.Findings())
	}
	// THE SENTINEL SURVIVES, because the loop's own backoff and every
	// caller deciding what a failure MEANS reads it rather than the string.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the refusal does not carry the cancellation: %v", err)
	}
}

// THE SECOND: the seat-app half, which could not report a fault at all.
//
// Its "a failed read is not a finding" rule is right for a timeout and wrong
// for a cancellation — one means GitHub was asked and could not say, the
// other that it was never asked — and both landed on the arm that leaves the
// seat exactly as it was. [github.ReconcileSeatApps] then returned nil
// unconditionally, so no cancellation anywhere beneath it could reach a
// caller: a draining node reported every seat as ready on its way out.
func TestACancelledSeatPassRaisesRatherThanLeavingEverySeatAlone(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	_, _, seats := newConvergedWorld(t, t, pem, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := github.ReconcileSeatApps(ctx, seats)
	if err == nil {
		t.Fatalf("a cancelled seat pass returned no error; with Findings %+v "+
			"and Ready %v that is the exact shape the loop reads as ready",
			res.Findings, res.Ready)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the refusal does not carry the cancellation: %v", err)
	}
	// AND IT CLAIMED NOTHING ON THE WAY OUT. A seat marked Ready out of a
	// roster that was never read is the same false "converged" one seat
	// wide, and Ready is what the engine mints tokens against.
	if len(res.Ready) != 0 {
		t.Errorf("a cancelled pass reported %v as ready", res.Ready)
	}
}

// AND A CANCELLATION BETWEEN SEATS STOPS THE WALK, not just one that
// interrupts a request.
//
// The two arms above a seat's first call — no app, and no key — conclude from
// the DOCUMENT alone and make no request at all, so a check placed only where
// a read failed would let a torn-down pass keep walking the roster and report
// on seats it had already stopped being able to look at. The finding it would
// produce is even true; what is false is the pass presenting a roster it did
// not finish as one it did.
func TestACancellationBetweenSeatsStopsTheWalk(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Cancelled by the world itself, once the first seat has been read, so
	// the second seat's turn begins under a dead context having made no
	// request of its own.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":101,"account":{"login":"acme","type":"Organization"},` +
			`"permissions":{"metadata":"read","contents":"read","issues":"read",` +
			`"pull_requests":"read","checks":"read","actions":"read","deployments":"read"}}`))
		cancel()
	}))
	t.Cleanup(server.Close)

	res, err := github.ReconcileSeatApps(ctx, github.SeatAppOptions{
		APIBase: server.URL, WebBase: server.URL, Org: convergedOrgName,
		Seats: []github.SeatApp{
			{Handle: "a-read", AppID: 11, Slug: "acme-a", InstallationID: 101,
				Key: pem, Tier: github.TierReadOnly},
			// NO APP: this seat concludes without a request, which is
			// what the loop-top check exists for.
			{Handle: "z-no-app"},
		},
	})
	if err == nil {
		t.Fatalf("a pass cancelled mid-roster answered %+v with no error", res.Findings)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the refusal does not carry the cancellation: %v", err)
	}
}

// AND A READ GITHUB REFUSED IS STILL LEFT ALONE, which is the boundary the
// change above had to not cross.
//
// A timeout or a 5xx means GitHub was asked and could not say, and the
// package deliberately leaves such a seat exactly as it was: reporting "no
// installation" on it would send an operator to redo a click nobody reversed.
// Testing the ERROR for cancellation rather than the CONTEXT would have
// swallowed that whole rule — net/http gives a Client.Timeout
// context.DeadlineExceeded too, so every slow GitHub would have started
// raising as a torn-down pass.
func TestAReadGitHubRefusedIsStillNotAFinding(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// NOT a 404: that means absent, and absent is a conclusion. This
		// is GitHub failing to answer.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"Server Error"}`))
	}))
	t.Cleanup(server.Close)

	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		APIBase: server.URL, WebBase: server.URL, Org: convergedOrgName,
		Seats: []github.SeatApp{{
			Handle: "sre-lead", AppID: 12, Slug: "acme-sre-lead",
			InstallationID: 102, Key: pem, Tier: github.TierReview,
		}},
	})
	if err != nil {
		t.Fatalf("a read GitHub refused failed the whole pass: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("a seat whose installation could not be READ was reported as "+
			"%+v, which tells an operator to redo a click nobody reversed",
			res.Findings)
	}
	if len(res.Ready) != 0 {
		t.Errorf("a seat whose installation could not be read was reported ready: %v",
			res.Ready)
	}
}
