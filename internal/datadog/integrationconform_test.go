package datadog_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
)

// The Datadog half of [integration.Reconciler]'s safety contract, certified
// against the reconcile the loop actually runs.
//
// # Why this file exists at all
//
// integrationtest states the contract in nine clauses and its package doc
// calls the write-counting hook REQUIRED, because that clause is "the one
// most likely to be wrong". Its only caller was a stub whose pass returned
// (nil, nil), so "a converged pass writes nothing" held because nothing
// happened: a green test over a world nobody had built and a reconciler
// nobody had run. This points the same nine cases at [datadog.Reconcile],
// over a Datadog organization that has already been brought into line, and
// counts what that organization and this deployment's own sealed store
// receive.
//
// # Two worlds, because one of them certifies nothing on its own
//
// A converged pass reports NO findings — that is what converged means — so
// every clause that walks the findings list walks an empty one and passes
// whatever this package does. Three of them did: the kinds are known, a
// person-owed finding says what to do, and two passes over a world with
// findings agree. [outstandingDatadog] is the other world, and its own doc
// says which two faults it holds and why those two.
//
// # What counts as a write here, and why it is not "not a GET"
//
// integrationtest's own doc says to count by ROUTE and to write down which
// routes those are, because some vendors model a listing as a POST and a
// counter keyed on the method makes the clause impossible to satisfy. Datadog
// is not one of those vendors TODAY — every route this client reads through
// is a GET — but that is a fact about the five routes below rather than a
// property of the API, and Datadog does serve POST-shaped search elsewhere.
// So [alertingOrg] classifies by route and takes the safe default: the five
// reads are named, and ANYTHING ELSE the organization receives counts as a
// write. A route added to the client later is counted until somebody decides
// it is a query, which is the direction that fails loudly rather than
// silently.
//
// The pass seals nothing: each agent's account and key are the per-seat
// access sync's, which has its own tests. So the organization is the whole of
// what a write here can reach.

// Every route this pass can land on, in the shape [routeOf] reduces one to.
//
// `hookCollection` is the webhook collection and is spelled once for the whole
// package, in hooks_test.go.
const (
	orgRoute      = "GET /api/v1/org"
	rolesRoute    = "GET /api/v2/roles"
	usersRoute    = "GET /api/v2/users"
	accountsRoute = "/api/v2/service_accounts"
	appKeysRoute  = accountsRoute + "/{account}/application_keys"
	oneKeyRoute   = appKeysRoute + "/{key}"
	oneHookRoute  = hookCollection + "/{name}"
	oneUserRoute  = "/api/v2/users/{user}"
)

// datadogQueries is every route a pass may ask for without having changed
// anything. Everything else [alertingOrg] receives is a write.
//
// Named one at a time rather than derived from the method, so a route the
// client grows later is counted as a write until somebody looks at it — see
// this file's opening comment for why that direction is the safe one.
//
//nolint:gochecknoglobals // an immutable set, not state
var datadogQueries = map[string]bool{
	orgRoute:              true,
	rolesRoute:            true,
	usersRoute:            true,
	"GET " + appKeysRoute: true,
	"GET " + oneHookRoute: true,
}

// routeOf is the ROUTE a request landed on, with the identifiers that vary
// from run to run put back as placeholders.
//
// Without this a counter keyed on the raw path cannot tell one route from
// another at all: a minted key's id is in the path of the call that revokes
// it, so every revocation would look like a route nobody had classified.
func routeOf(method, path string) string {
	shape := path
	switch {
	case strings.HasPrefix(path, hookCollection+"/"):
		shape = oneHookRoute
	case strings.HasPrefix(path, accountsRoute+"/"):
		_, tail, _ := strings.Cut(strings.TrimPrefix(path, accountsRoute+"/"), "/")
		switch {
		case tail == "application_keys":
			shape = appKeysRoute
		case strings.HasPrefix(tail, "application_keys/"):
			shape = oneKeyRoute
		}
	case strings.HasPrefix(path, "/api/v2/users/"):
		shape = oneUserRoute
	}
	return method + " " + shape
}

// The seats this company has.
//
// Spelled once because BOTH worlds are built from them and they must not
// drift: [outstandingDatadog] decommissions seatOne at Datadog and leaves
// seatTwo converged beside it, so a pass that reported every seat broken
// would not satisfy that world's own test either.
const (
	seatOne = "sre"
	seatTwo = "oncall"
)

// serviceAccount is one identity this organization holds.
type serviceAccount struct {
	id       string
	email    string
	name     string
	person   bool
	disabled bool
}

// alertingOrg is a Datadog organization a real pass can be run against, and
// the ledger of what that pass did to it.
//
// STATEFUL RATHER THAN A FIXTURE OF CANNED ANSWERS, because the world here is
// converged BY RUNNING THE PASS — see [convergedDatadog] — and a pass can only
// converge a world that remembers what it was told.
type alertingOrg struct {
	t integrationtest.TB

	mu       sync.Mutex
	accounts []*serviceAccount
	hook     *datadog.Webhook
	writes   int
}

// newAlertingOrg builds one, reporting to the CASE that drives it.
//
// [integrationtest.TB] rather than the *testing.T that owns the world, and
// the difference shows the moment a fixture complains: every Errorf in the
// handlers below is a statement that THIS pass did something a pass must not
// do — disabled a user, asked for a route the organization does not serve —
// and attributed to the parent it names a test with nine subtests rather than
// the clause that caught it.
//
// The SERVER's lifetime still hangs off the parent, because TB deliberately
// has no Cleanup: the suite's own tests drive its cases with a recorder, so
// the interface is the narrow one they can satisfy. That split is safe in the
// one way it has to be — every request this organization ever serves
// originates from a Reconcile call made synchronously inside a case, so no
// handler can still be running, and reporting into a case that has finished,
// once that case returns.
func newAlertingOrg(t integrationtest.TB) *alertingOrg {
	t.Helper()
	return &alertingOrg{t: t}
}

// mutations is every write this organization has received, by route.
func (o *alertingOrg) mutations() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.writes
}

func (o *alertingOrg) serve(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !datadogQueries[routeOf(r.Method, r.URL.Path)] {
		o.writes++
	}
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/org":
		o.write(w, map[string]any{"orgs": []any{
			map[string]any{"name": "Acme", "public_id": "p1"},
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/users":
		o.listUsers(w)
	case strings.HasPrefix(r.URL.Path, hookCollection):
		o.serveWebhook(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v2/users/"):
		// A DECOMMISSION, ON THE COMPANY'S PASS. Datadog models
		// disabling a user as a DELETE, and every agent's account is the
		// access sync's: this pass reaching it would be taking an agent's
		// identity away behind the sync's back.
		o.t.Errorf("the company's pass disabled %s; agents' accounts are the "+
			"access sync's", r.URL.Path)
		o.refuse(w, http.StatusInternalServerError, "not a reconcile route")
	default:
		// FAIL LOUD. An unserved route answered with a plain 404 is read
		// by the client as an ordinary Datadog refusal and folded into a
		// per-seat finding, so a fixture missing a route would leave the
		// converged clause passing over a pass that never read anything.
		o.t.Errorf("the pass asked for %s %s, which this organization does "+
			"not serve", r.Method, r.URL.Path)
		o.refuse(w, http.StatusNotFound, "no route")
	}
}

func (o *alertingOrg) listUsers(w http.ResponseWriter) {
	out := make([]any, 0, len(o.accounts))
	for _, account := range o.accounts {
		out = append(out, o.row(account.id, map[string]any{
			"email":           account.email,
			"name":            account.name,
			"disabled":        account.disabled,
			"service_account": !account.person,
		}))
	}
	o.write(w, rows(out))
}

// serveWebhook is the inbound half, out of ONE definition, so a case asserts
// what a pass DID rather than which calls it happened to make.
func (o *alertingOrg) serveWebhook(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, hookCollection), "/")
	decode := func() (datadog.Webhook, bool) {
		var body datadog.Webhook
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			o.refuse(w, http.StatusBadRequest, "undecodable body")
			return datadog.Webhook{}, false
		}
		return body, true
	}
	switch {
	case name == "" && r.Method == http.MethodPost:
		if body, ok := decode(); ok {
			o.hook = &body
			w.WriteHeader(http.StatusCreated)
			o.write(w, o.hook)
		}
	case name == "":
		// DATADOG SERVES NO LISTING, which is why the pass addresses a
		// definition by name. Answering anything else here would let a
		// pass that diffed a set look correct against a Datadog that has
		// no set to diff.
		o.refuse(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	case r.Method == http.MethodGet:
		if o.hook == nil || o.hook.Name != name {
			o.refuse(w, http.StatusNotFound, "Webhook does not exist")
			return
		}
		o.write(w, served(*o.hook))
	case r.Method == http.MethodPut:
		if body, ok := decode(); ok {
			o.hook = &body
			o.write(w, o.hook)
		}
	case r.Method == http.MethodDelete:
		if o.hook == nil || o.hook.Name != name {
			o.refuse(w, http.StatusNotFound, "Webhook does not exist")
			return
		}
		o.hook = nil
		w.WriteHeader(http.StatusNoContent)
	default:
		o.t.Errorf("the pass asked for %s %s on a webhook", r.Method, r.URL.Path)
		o.refuse(w, http.StatusNotFound, "no route")
	}
}

// served is a definition as Datadog HANDS IT BACK, which is not byte-for-byte
// what was written.
//
// A fake that echoes the request body makes the convergence comparison pass
// for the wrong reason: byte equality would satisfy it, and then the first
// real region that re-encodes anything has the pass rewriting a definition
// that is already correct on every tick, for ever — an edit per tick in
// somebody's audit log, invisible from inside this engine because nothing
// here re-reads what it wrote.
//
// So EVERY FIELD THE ENGINE COMPARES IS PERTURBED WITHOUT CHANGING WHAT IT
// MEANS, and the engine's own comparison decides. `encode_as` comes back in
// whatever case Datadog feels like. `custom_headers` and `payload` are JSON
// documents Datadog stores as strings and re-encodes at its own discretion,
// so one is re-indented and the other collapsed — opposite directions on
// purpose, so a comparison that tightened to bytes on either field fails
// here rather than in production. The payload was the one echoed verbatim,
// and it is the larger document of the two: [datadog.WebhookPayload] is ten
// keys of literal JSON.
func served(hook datadog.Webhook) datadog.Webhook {
	hook.EncodeAs = strings.ToUpper(hook.EncodeAs)
	var headers map[string]string
	if json.Unmarshal([]byte(hook.CustomHeaders), &headers) == nil {
		if reencoded, err := json.MarshalIndent(headers, "", "  "); err == nil {
			hook.CustomHeaders = string(reencoded)
		}
	}
	var payload any
	if json.Unmarshal([]byte(hook.Payload), &payload) == nil {
		if reencoded, err := json.Marshal(payload); err == nil {
			hook.Payload = string(reencoded)
		}
	}
	return hook
}

func (o *alertingOrg) row(id string, attributes map[string]any) map[string]any {
	return map[string]any{"id": id, "attributes": attributes}
}

func rows(data []any) map[string]any { return map[string]any{"data": data} }

func (o *alertingOrg) write(w http.ResponseWriter, body any) {
	if err := json.NewEncoder(w).Encode(body); err != nil {
		o.t.Errorf("encode the answer: %v", err)
	}
}

func (o *alertingOrg) refuse(w http.ResponseWriter, status int, detail string) {
	w.WriteHeader(status)
	o.write(w, map[string]any{"errors": []string{detail}})
}

// datadogWorld is a Datadog organization, plus the pass that runs against it
// as [integration.Reconciler] sees it.
//
// ONE TYPE FOR BOTH WORLDS. [convergedDatadog] builds the converged one and
// [outstandingDatadog] builds the other by breaking that one, so the second
// differs from the first by exactly the faults its own doc names and by
// nothing else.
type datadogWorld struct {
	org  *alertingOrg
	opts datadog.Options
}

// Kind names the surface. [integration.KindDatadog] is a constant rather than
// anything derived, which is what makes the suite's stability clause an
// assertion about the adapter rather than about a field.
func (*datadogWorld) Kind() integration.Kind { return integration.KindDatadog }

// Reconcile runs the real pass and maps its answer the way the loop's own
// adapter does.
//
// IT IS THINNER THAN internal/engine's datadogPass.Run DELIBERATELY, and the
// difference is worth naming. Every arm this leaves out is a PRE-FLIGHT read
// of the company document — a block that is absent or switched off, a region
// that is not one Datadog serves, an organization key that did not resolve —
// and each of them answers without a client at all. None is reachable over a
// converged world, so mirroring them here would add branches no case can
// take. What that adapter does on top of the pass is scored where it lives.
func (w *datadogWorld) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := datadog.Reconcile(ctx, w.opts)
	if err != nil {
		return nil, err
	}
	return res.Findings(), nil
}

// mutations is every write since this world was built, at Datadog.
func (w *datadogWorld) mutations() int { return w.org.mutations() }

// convergedDatadog stands the organization up and converges it BY RUNNING THE
// PASS.
//
// # Converged by the pass rather than by a fixture
//
// The alternative is a hand-written fixture holding the webhook definition a
// converged company has. It is the tempting one and it is weaker in the
// direction that matters: it encodes what the harness author BELIEVES
// converged looks like, so a pass whose idea of converged has drifted from
// that belief writes on every run and the fixture calls it converged anyway.
// The webhook definition has to match on four fields including a JSON-encoded
// header. Running the real pass makes the world converged by definition, and
// the only question left is the one worth asking: whether the SECOND pass
// writes.
//
// The seeding pass's own writes are not counted: [integrationtest.Run] takes
// its baseline from Mutations() after New returns.
//
// TWO AGENT ACCOUNTS the access sync manages, and a PERSON under the same
// domain. The accounts are what the orphan advisory must not report; the
// person is what the service-account check exists to drop, since Datadog's
// user filter is a free-text substring match on the address.
//
// BOTH t AND tb, deliberately. [integrationtest.TB] is the narrow interface
// the suite's own tests drive its cases with and it has no Cleanup, so the
// case's own reporter is tb while the server's lifetime hangs off the real
// *testing.T that owns this world.
func convergedDatadog(t *testing.T, tb integrationtest.TB) *datadogWorld {
	t.Helper()
	cfg := cfgWith()
	org := newAlertingOrg(tb)
	var managed []string
	for i, handle := range []string{seatOne, seatTwo} {
		id := "agent-" + strconv.Itoa(i+1)
		org.accounts = append(org.accounts, &serviceAccount{
			id: id, email: datadog.AccountEmail(cfg.Provisioning, handle),
			name: strings.ToUpper(handle) + " (Crewlet)",
		})
		managed = append(managed, id)
	}
	org.accounts = append(org.accounts, &serviceAccount{
		id: "person-1", email: "someone@agents.test.invalid",
		name: "A Colleague", person: true,
	})

	srv := httptest.NewServer(http.HandlerFunc(org.serve))
	t.Cleanup(srv.Close)

	// The client is pointed at the fake by rewriting the host on the way
	// out: the endpoint is built from the SITE, which is the whole point
	// of the region check.
	client, err := datadog.NewClient(datadog.ClientOptions{
		Site: cfg.Provisioning.Site,
		HTTP: &http.Client{Transport: httpxtest.Rewrite(tb, srv)},
	})
	if err != nil {
		tb.Fatalf("NewClient: %v", err)
	}

	world := &datadogWorld{org: org, opts: datadog.Options{
		Client: client, Config: cfg, Creds: pair, Managed: managed,
		// BOTH OF THESE THE WAY THE ENGINE SETS THEM. Either one empty
		// makes ensureWebhook return before it reads anything, so the
		// inbound half would never be exercised and the converged clause
		// would pass for the wrong reason.
		WebhookBase: base, WebhookToken: token,
	}}

	findings, err := world.Reconcile(context.Background())
	if err != nil {
		tb.Fatalf("the pass that converges the world failed: %v", err)
	}
	if len(findings) != 0 {
		tb.Fatalf("the world is not converged: the seeding pass reported %+v", findings)
	}
	// AND IT ACTUALLY DID THE WORK. A seeding pass that wrote nothing
	// would leave every case below true about an empty Datadog, which is
	// the exact shape of vacuous pass this file exists to replace.
	if world.mutations() == 0 {
		tb.Fatalf("%s", "the seeding pass wrote nothing, so there is no "+
			"converged world here to certify a second pass against")
	}
	org.mu.Lock()
	defer org.mu.Unlock()
	if org.hook == nil {
		tb.Fatalf("%s", "no webhook was registered, so the inbound half of the "+
			"pass is not being exercised")
	}
	return world
}

// outstandingDatadog is the same organization with two things genuinely
// wrong, one in each half of the pass, and both of them a PERSON's to settle.
//
// # Why a second world exists at all
//
// [integrationtest.Reconciler.Outstanding]'s doc states it: a converged pass
// reports no findings, so every clause that walks the findings list walks an
// empty one and certifies nothing. This world is what those clauses are run
// against, and it has to prove itself first: the suite's anti-vacuity clause
// fails if it reports nothing, or nothing a person owes.
//
// # Why THESE two faults
//
// Each is STABLE across passes, which the two-passes-agree clause needs and
// which rules out every fault the pass would simply fix on the first tick:
//
//   - THIS DEPLOYMENT HAS NO PUBLIC BASE URL, so there is no address to point
//     a webhook at and every monitor this company has fires into nothing. It
//     reads as ingress_blocked on the field somebody has to set.
//   - AN ENABLED SERVICE ACCOUNT AT THIS COMPANY'S DOMAIN THAT NOTHING
//     MANAGES, the kind a live organization accumulates. It is reported and
//     never touched, which is what makes it stable here. It reads as
//     registration_orphaned, owed by an admin.
//
// Neither fault makes this pass write, which is incidental rather than
// required: the suite counts mutations only over the converged world.
func outstandingDatadog(t *testing.T, tb integrationtest.TB) *datadogWorld {
	t.Helper()
	world := convergedDatadog(t, tb)
	world.opts.WebhookBase = ""
	world.org.mu.Lock()
	defer world.org.mu.Unlock()
	world.org.accounts = append(world.org.accounts, &serviceAccount{
		id: "stray-1", email: "crewlet-left-over@agents.test.invalid", name: "Left over",
	})
	return world
}

// THE CONTRACT IS CERTIFIED AGAINST THE REAL RECONCILER, OVER BOTH WORLDS.
func TestTheDatadogReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	// Assigned by Converged and read by Mutations, which the suite calls in
	// that order within one sequential case. Outstanding deliberately does
	// not touch it: nothing counts mutations over that world, and sharing
	// the variable would let a later case's world answer for an earlier
	// case's baseline.
	var converged *datadogWorld
	integrationtest.Run(t, integrationtest.Reconciler{
		Converged: func(tb integrationtest.TB) integration.Reconciler {
			converged = convergedDatadog(t, tb)
			return converged
		},
		Outstanding: func(tb integrationtest.TB) integration.Reconciler {
			return outstandingDatadog(t, tb)
		},
		Mutations: func() int { return converged.mutations() },
	})
}

// AND THE OUTSTANDING WORLD REPORTS EVERY FAULT IT WAS BUILT TO HOLD.
//
// The suite's anti-vacuity clause asks for ONE finding and one a person owes,
// which two faults satisfy twice over — so it cannot notice that one of them
// stopped being reported, and the world would quietly go on certifying the
// findings clauses against half of what it was built for. That is the same
// failure this world exists to remove, moved one level up. Each fault is
// named here, so each is pinned on its own rather than by the other.
func TestTheOutstandingWorldReportsEveryFaultItHolds(t *testing.T) {
	t.Parallel()
	world := outstandingDatadog(t, t)
	findings, err := world.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("a pass over the outstanding world: %v", err)
	}

	// The SUBJECT as well as the kind, because the subject is what a status
	// row offers somebody to act on: a finding naming the wrong thing is a
	// person sent to the wrong field.
	want := map[integration.FindingKind]string{
		integration.FindingIngressBlocked:       "api.external_url",
		integration.FindingRegistrationOrphaned: "datadog service accounts",
	}
	for _, f := range findings {
		subject, expected := want[f.Kind]
		if !expected {
			t.Errorf("the outstanding world reported %q on %q, which is neither "+
				"of the two faults it holds: %s", f.Kind, f.Subject, f.Detail)
			continue
		}
		if f.Subject != subject {
			t.Errorf("%s names %q, want %q", f.Kind, f.Subject, subject)
		}
		delete(want, f.Kind)
	}
	for kind, subject := range want {
		t.Errorf("the outstanding world holds a fault it did not report: %s on "+
			"%s — every clause that walks the findings list is then walking a "+
			"shorter list than this world was built to produce", kind, subject)
	}

	// AND THE AGENTS' OWN ACCOUNTS ARE NOT AMONG THE ORPHANS, which is
	// what makes the advisory about the stray account rather than about a
	// pass that reports every account it sees.
	for _, f := range findings {
		for _, subject := range f.Subjects {
			if strings.Contains(subject, seatOne) || strings.Contains(subject, seatTwo) {
				t.Errorf("an agent's own account was reported as an orphan: %s", subject)
			}
		}
	}
}

// AND THE COUNTER SEES THE WRITES IT IS THERE TO SEE.
//
// A Mutations() that answered 0 whatever happened would make the clause above
// vacuous in exactly the way the stub it replaced was, and nothing in the
// suite can tell the difference: it only ever compares the number to itself.
// So a write is made here by moving this deployment's address, which the pass
// answers by rewriting the webhook: the pass doing its job rather than a
// contrivance.
func TestTheWriteCounterCountsTheVendorsWrites(t *testing.T) {
	t.Parallel()
	world := convergedDatadog(t, t)
	before := world.mutations()
	world.opts.WebhookBase = "https://moved.example.com"
	if _, err := world.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if world.mutations() <= before {
		t.Errorf("a moved webhook was rewritten without a single write reaching Datadog")
	}
}

// A QUERY IS NOT A WRITE, AND THE ROUTE IS WHAT SAYS SO.
//
// integrationtest's doc names the trap: some vendors model a listing as a
// POST, so a counter keyed on the HTTP method makes the clause impossible to
// satisfy — "and a clause that cannot be satisfied is one somebody eventually
// weakens". This pins the classification itself, including the safe default,
// which is the half a reader would otherwise have to take on trust.
func TestTheWriteCounterClassifiesByRouteRatherThanMethod(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		method, path string
		query        bool
	}{
		{http.MethodGet, "/api/v1/org", true},
		{http.MethodGet, "/api/v2/users", true},
		{http.MethodGet, "/api/v2/roles", true},
		{http.MethodGet, "/api/v2/service_accounts/u1/application_keys", true},
		{http.MethodGet, hookByName, true},
		{http.MethodPost, "/api/v2/service_accounts", false},
		{http.MethodPost, "/api/v2/service_accounts/u1/application_keys", false},
		{http.MethodDelete, "/api/v2/service_accounts/u1/application_keys/k1", false},
		{http.MethodPost, hookCollection, false},
		{http.MethodPut, hookByName, false},
		{http.MethodDelete, hookByName, false},
		{http.MethodDelete, "/api/v2/users/u1", false},
		// A ROUTE NOBODY HAS CLASSIFIED counts as a write, which is the
		// direction that fails loudly: an uncounted write is a credential
		// rotated on a timer, and an over-counted read is a test somebody
		// reads.
		{http.MethodPost, "/api/v2/logs/events/search", false},
		{http.MethodGet, "/api/v2/logs/events", false},
	} {
		if got := datadogQueries[routeOf(c.method, c.path)]; got != c.query {
			t.Errorf("%s %s counted as a query = %v, want %v (route %q)",
				c.method, c.path, got, c.query, routeOf(c.method, c.path))
		}
	}
}
