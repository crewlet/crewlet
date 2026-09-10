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
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/provision"
)

// The Datadog half of [integration.Reconciler]'s safety contract, certified
// against the reconcile the loop actually runs.
//
// # Why this file exists at all
//
// integrationtest states the contract in seven clauses and its package doc
// calls the write-counting hook REQUIRED, because that clause is "the one
// most likely to be wrong". Its only caller was a stub whose pass returned
// (nil, nil), so "a converged pass writes nothing" held because nothing
// happened: a green test over a world nobody had built and a reconciler
// nobody had run. This points the same seven cases at [datadog.Reconcile],
// over a Datadog organization that has already been brought into line, and
// counts what that organization and this deployment's own sealed store
// receive.
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
// The sealed store is the other half. A pass that re-sealed a seat's
// application key on every converged run would write nothing at Datadog and
// still be rotating the credential every running agent authenticates with, so
// [sealedStore] counts every Record and every Discard. Flush is NOT counted,
// and that is not an omission: both sinks that reach this pass in production
// make it a no-op — provision.SecretStoreSink.Flush returns nil, and the
// engine's refreshingSink rebuilds the resolver snapshot only when something
// was actually sealed — so counting it would make the clause unsatisfiable
// for a reason nobody could act on.

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
	t        *testing.T
	roleID   string
	roleName string

	mu       sync.Mutex
	accounts []*serviceAccount
	keys     map[string][]datadog.AppKey
	hook     *datadog.Webhook
	writes   int
	next     int
}

func newAlertingOrg(t *testing.T, roleName string) *alertingOrg {
	t.Helper()
	return &alertingOrg{
		t: t, roleID: "role-1", roleName: roleName,
		keys: map[string][]datadog.AppKey{},
	}
}

// mutations is every write this organization has received, by route.
func (o *alertingOrg) mutations() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.writes
}

func (o *alertingOrg) id(prefix string) string {
	o.next++
	return prefix + strconv.Itoa(o.next)
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
	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/roles":
		o.write(w, rows([]any{o.row(o.roleID, map[string]any{"name": o.roleName})}))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/users":
		o.listUsers(w)
	case r.Method == http.MethodPost && r.URL.Path == accountsRoute:
		o.createAccount(w, r)
	case strings.HasPrefix(r.URL.Path, accountsRoute+"/"):
		o.serveKeys(w, r)
	case strings.HasPrefix(r.URL.Path, hookCollection):
		o.serveWebhook(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v2/users/"):
		// A DECOMMISSION, ON THE RECONCILE PATH. Datadog models
		// disabling a user as a DELETE, and a pass that reached it would
		// be tearing down the seat it is meant to be keeping.
		o.t.Errorf("a reconcile pass disabled %s; that is a teardown gesture "+
			"and it takes an agent's identity away", r.URL.Path)
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

func (o *alertingOrg) createAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Data struct {
			Attributes struct {
				Email string `json:"email"`
				Name  string `json:"name"`
			} `json:"attributes"`
			Relationships struct {
				Roles struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"roles"`
			} `json:"relationships"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		o.refuse(w, http.StatusBadRequest, "undecodable body")
		return
	}
	// THE ROLE COMES WITH THE CREATE, which is the one thing a fake can
	// check that the client's own test cannot: an account that exists for
	// a moment without one inherits the organization's default.
	roles := body.Data.Relationships.Roles.Data
	if len(roles) != 1 || roles[0].ID != o.roleID {
		o.t.Errorf("an account was created holding %v, want the configured role %q",
			roles, o.roleID)
	}
	account := &serviceAccount{
		id: o.id("u"), email: body.Data.Attributes.Email,
		name: body.Data.Attributes.Name,
	}
	o.accounts = append(o.accounts, account)
	o.write(w, map[string]any{"data": o.row(account.id, map[string]any{
		"email": account.email, "name": account.name,
	})})
}

// serveKeys is the application-key half: one account's keys, without their
// values, and the mint and revoke that move them.
func (o *alertingOrg) serveKeys(w http.ResponseWriter, r *http.Request) {
	account, tail, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, accountsRoute+"/"), "/")
	keyID := strings.TrimPrefix(tail, "application_keys/")
	switch {
	case r.Method == http.MethodGet && tail == "application_keys":
		out := make([]any, 0, len(o.keys[account]))
		for _, key := range o.keys[account] {
			out = append(out, o.row(key.ID, map[string]any{"name": key.Name}))
		}
		o.write(w, rows(out))
	case r.Method == http.MethodPost && tail == "application_keys":
		var body struct {
			Data struct {
				Attributes struct {
					Name string `json:"name"`
				} `json:"attributes"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		key := datadog.AppKey{
			ID: o.id("k"), Name: body.Data.Attributes.Name,
			// THE VALUE IS SERVED ONCE, which is Datadog's own rule and
			// the reason the pass may not mint on a converged world.
			Key: "dd-app-key-" + account,
		}
		o.keys[account] = append(o.keys[account], key)
		o.write(w, map[string]any{"data": o.row(key.ID, map[string]any{
			"key": key.Key, "name": key.Name,
		})})
	case r.Method == http.MethodDelete && keyID != "" && keyID != tail:
		kept := o.keys[account][:0]
		for _, key := range o.keys[account] {
			if key.ID != keyID {
				kept = append(kept, key)
			}
		}
		o.keys[account] = kept
		w.WriteHeader(http.StatusNoContent)
	default:
		o.t.Errorf("the pass asked for %s %s on an account's keys",
			r.Method, r.URL.Path)
		o.refuse(w, http.StatusNotFound, "no route")
	}
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
// that is already correct on every tick, for ever. The two places the engine
// deliberately compares by VALUE are both exercised here — `custom_headers`
// is a JSON object Datadog stores as a string and re-encodes at its own
// discretion, and `encode_as` comes back in whatever case it feels like — so
// a comparison that tightened to bytes fails this fixture rather than
// production.
func served(hook datadog.Webhook) datadog.Webhook {
	hook.EncodeAs = strings.ToUpper(hook.EncodeAs)
	var headers map[string]string
	if json.Unmarshal([]byte(hook.CustomHeaders), &headers) == nil {
		if reencoded, err := json.MarshalIndent(headers, "", "  "); err == nil {
			hook.CustomHeaders = string(reencoded)
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

// sealedStore is this deployment's own half of "a write".
//
// The suite's package doc is explicit that a write is anything a person would
// have to undo and that it is WIDER than a request to the vendor: a pass that
// re-seals a seat's key on every converged run rotates the credential every
// running agent is authenticating with, and nothing at Datadog would show it.
type sealedStore struct {
	*sink
	writes int
}

func newSealedStore() *sealedStore { return &sealedStore{sink: newSink()} }

// Record counts the ATTEMPT rather than the success, because a pass hammering
// a store that refuses every write is the same fault as one hammering a store
// that accepts them.
func (s *sealedStore) Record(ctx context.Context, name, value string) error {
	s.writes++
	return s.sink.Record(ctx, name, value)
}

func (s *sealedStore) Discard(ctx context.Context) error {
	s.writes++
	return s.sink.Discard(ctx)
}

// convergedOrg is a Datadog this pass has already converged, plus the pass
// itself as [integration.Reconciler] sees it.
type convergedOrg struct {
	org  *alertingOrg
	keys *sealedStore
	opts datadog.Options
}

// Kind names the surface. [integration.KindDatadog] is a constant rather than
// anything derived, which is what makes the suite's stability clause an
// assertion about the adapter rather than about a field.
func (*convergedOrg) Kind() integration.Kind { return integration.KindDatadog }

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
func (w *convergedOrg) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := datadog.Reconcile(ctx, w.opts)
	if err != nil {
		return nil, err
	}
	return res.Findings(), nil
}

// mutations is every write since this world was built, at Datadog and in the
// sealed store.
func (w *convergedOrg) mutations() int { return w.org.mutations() + w.keys.writes }

// convergedDatadog stands the organization up and converges it BY RUNNING THE
// PASS.
//
// # Converged by the pass rather than by a fixture
//
// The alternative is a hand-written fixture holding the accounts, the keys
// and the webhook definition a converged company has. It is the tempting one
// and it is weaker in the direction that matters: it encodes what the harness
// author BELIEVES converged looks like, so a pass whose idea of converged has
// drifted from that belief writes on every run and the fixture calls it
// converged anyway. Datadog makes that especially easy to get wrong — the
// webhook definition has to match on four fields including a JSON-encoded
// header, and a users row without `service_account: true` is dropped as a
// real person, which turns a converged world into one that creates. Running
// the real pass makes the world converged by definition, and the only
// question left is the one worth asking: whether the SECOND pass writes.
//
// The seeding pass's own writes are not counted: [integrationtest.Run] takes
// its baseline from Mutations() after New returns.
//
// TWO SEATS, because every write this pass makes on the identity half is
// per-seat. One seat counts 1 where the bug's shape is N, and a harness
// cannot tell "wrote once" from "wrote once per seat" with one of them.
//
// BOTH t AND tb, deliberately. [integrationtest.TB] is the narrow interface
// the suite's own tests drive its cases with and it has no Cleanup, so the
// case's own reporter is tb while the server's lifetime hangs off the real
// *testing.T that owns this world.
func convergedDatadog(t *testing.T, tb integrationtest.TB) *convergedOrg {
	t.Helper()
	cfg := cfgWith()
	org := newAlertingOrg(t, datadog.RoleName(cfg.Provisioning))
	// A REAL PERSON UNDER THE SAME DOMAIN, because Datadog's user filter
	// is a free-text substring match on the address and one is what the
	// service-account check exists to drop. Without it every row in the
	// listing is a seat, and a fixture where nothing has to be filtered
	// cannot notice a pass that stopped filtering.
	org.accounts = append(org.accounts, &serviceAccount{
		id: "person-1", email: "someone@agents.test.invalid",
		name: "A Colleague", person: true,
	})

	srv := httptest.NewServer(http.HandlerFunc(org.serve))
	t.Cleanup(srv.Close)

	// The client is pointed at the fake by rewriting the host on the way
	// out: the endpoint is built from the SITE, which is the whole point
	// of the region check. Over the SERVER'S OWN transport rather than the
	// shared default, so the connection pool dies with the server.
	client, err := datadog.NewClient(datadog.ClientOptions{
		Site: cfg.Provisioning.Site,
		HTTP: &http.Client{Transport: rewriteHost{
			to: srv.Listener.Addr().String(), next: srv.Client().Transport,
		}},
	})
	if err != nil {
		tb.Fatalf("NewClient: %v", err)
	}

	handles := []string{"sre", "oncall"}
	plan := &provision.Plan{}
	for _, handle := range handles {
		plan.Add(provision.Seat{
			Handle: handle, Role: strings.ToUpper(handle),
			TokenVar: strings.ToUpper(handle) + "_DD_KEY",
			// FROM THE PACKAGE'S OWN DERIVATION rather than spelled out
			// here. The address is Datadog's key for a seat's identity,
			// and a harness that wrote its own would certify a pass
			// against accounts the real plan would never find.
			Email: datadog.AccountEmail(cfg.Provisioning, handle),
		})
	}

	keys := newSealedStore()
	world := &convergedOrg{org: org, keys: keys, opts: datadog.Options{
		Client: client, Config: cfg, Plan: plan, Creds: pair, Sink: keys,
		// BOTH OF THESE THE WAY THE ENGINE SETS THEM. Either one empty
		// makes ensureWebhook return before it reads anything, so the
		// inbound half — half of what this pass does, and the half that
		// writes on every tick when it gets convergence wrong — would
		// never be exercised and the converged clause would pass for the
		// wrong reason.
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
	for _, handle := range handles {
		if keys.held[strings.ToUpper(handle)+"_DD_KEY"] == "" {
			tb.Fatalf("%s holds no application key after the seeding pass, so "+
				"the second pass would mint rather than keep", handle)
		}
	}
	org.mu.Lock()
	defer org.mu.Unlock()
	if len(org.accounts) != len(handles)+1 {
		tb.Fatalf("the organization holds %d users, want one per seat beside "+
			"the person that was already there", len(org.accounts))
	}
	if org.hook == nil {
		tb.Fatalf("%s", "no webhook was registered, so the inbound half of the "+
			"pass is not being exercised")
	}
	return world
}

// THE CONTRACT IS CERTIFIED AGAINST THE REAL RECONCILER.
func TestTheDatadogReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	// Assigned by New and read by Mutations, which the suite calls in that
	// order within one sequential case.
	var world *convergedOrg
	integrationtest.Run(t, integrationtest.Reconciler{
		New: func(tb integrationtest.TB) integration.Reconciler {
			world = convergedDatadog(t, tb)
			return world
		},
		Mutations: func() int { return world.mutations() },
	})
}

// AND THE COUNTER SEES THE WRITES IT IS THERE TO SEE.
//
// A Mutations() that answered 0 whatever happened would make the clause above
// vacuous in exactly the way the stub it replaced was, and nothing in the
// suite can tell the difference: it only ever compares the number to itself.
// So both halves are exercised here — a write at Datadog, and a write into
// this deployment's own sealed store — by asking a converged world for a
// second seat's identity, which is the pass doing its job rather than a
// contrivance.
func TestTheWriteCounterCountsBothHalves(t *testing.T) {
	t.Parallel()
	world := convergedDatadog(t, t)
	before, vendorBefore, storeBefore := world.mutations(), world.org.mutations(), world.keys.writes

	world.opts.Plan.Add(provision.Seat{
		Handle: "dba", Role: "DBA", TokenVar: "DBA_DD_KEY",
		Email: datadog.AccountEmail(world.opts.Config.Provisioning, "dba"),
	})
	if _, err := world.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}

	if world.org.mutations() <= vendorBefore {
		t.Errorf("a seat with no account was provisioned without a single "+
			"write reaching Datadog: %d", world.org.mutations()-vendorBefore)
	}
	if world.keys.writes <= storeBefore {
		t.Errorf("a key was minted and the sealed store recorded nothing: %d",
			world.keys.writes-storeBefore)
	}
	if world.mutations() <= before {
		t.Errorf("Mutations() did not move over a pass that provisioned a seat")
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
