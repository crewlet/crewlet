package atlassian_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/org"
)

// The Atlassian half of [integration.Reconciler]'s safety contract, certified
// against the reconcile the loop actually runs.
//
// # Why this file exists at all
//
// integrationtest states the contract in seven clauses and its package doc
// calls the write-counting hook REQUIRED, because that clause is "the one most
// likely to be wrong". Its only caller was a stub whose pass returned
// (nil, nil), so "a converged pass writes nothing" held because nothing
// happened — a green test over a world nobody had built and a reconciler
// nobody had run. This points the same seven cases at [atlassian.Reconcile],
// over an organization that has already been brought into line, and counts
// what that organization and this deployment's own sealed store receive.
//
// It found the clause false. The pass sent the product-access invite once per
// seat on every tick for ever, and re-sealed each seat's account address
// beside it. See the grant in reconcile.go for what replaced it.
//
// # What counts as a write here, and what does not
//
// BY ROUTE, never by method, which for this vendor is the difference between
// a suite that can be satisfied and one that cannot. Atlassian models its
// workspace listing as a POST ([atlassian.AdminBaseURL] + /admin/v2/orgs/…/
// workspaces), and that request is made on every pass by design: it is how the
// site is discovered rather than typed into a form. A counter keyed on "not a
// GET" would count it, no pass could ever reach zero, and the only way out
// would be to weaken the clause.
//
// So four routes are counted, and they are counted because a person would
// have to undo each one:
//
//   - POST …/service-accounts — an identity created at Atlassian.
//   - POST …/service-accounts/invite — product access granted on one.
//   - POST /users/{id}/manage/api-tokens — a live credential minted, which
//     is the write that matters most: Atlassian shows a token once, so
//     minting rotates the one every running seat is authenticating with.
//   - POST /users/{id}/manage/lifecycle/delete — an account destroyed. A
//     teardown route, never reached by a pass, counted so that a regression
//     that reached it would be loud rather than invisible.
//
// And the fifth write is not an HTTP call at all: every [provision.TokenSink]
// Record is counted too. The sealed store is the company's, shared by the
// whole fleet and stamped with an author and a time, so a pass re-sealing the
// same address every tick is writing exactly as surely as one that POSTs.

const (
	// testOrgID and testCloudID name the organization and the site this
	// fake serves. Exact route matching below is built from them, so a
	// client that addressed a different organization would fall through to
	// the unexpected-route arm rather than be quietly answered.
	testOrgID   = "org-nimbus"
	testCloudID = "cloud-nimbus-1"
)

// serviceAccounts is the collection route this fake serves, spelled out
// rather than derived from the package's own constant.
//
// DELIBERATELY A SECOND COPY. The constant is unexported, and a fake built
// from the same expression as the client would answer whatever the client
// asked for — including a route that had silently moved to a service
// Atlassian does not serve it on. Written out, it is an assertion.
const serviceAccounts = "/admin/account-management/v1/orgs/" + testOrgID + "/service-accounts"

// serviceAccount is one agent's identity as this fake organization holds it.
type serviceAccount struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`

	// grants is the product access the invite route applied, and tokens is
	// how many API tokens the account holds. Both are STATE rather than a
	// canned answer: the world is converged by running the real pass
	// against it, so what the second pass reads has to be what the first
	// pass actually did.
	grants []atlassian.PermissionRule
	tokens int
}

// atlassianOrg is an Atlassian organization with state, and a counter for
// every request that changed it.
type atlassianOrg struct {
	mu       sync.Mutex
	accounts []*serviceAccount
	next     int

	// writes counts the requests that MUTATED this organization, keyed by
	// route. See this file's header for the four and why.
	writes int

	// unexpected names every route this fake was asked for and does not
	// serve. Answered 404 rather than 200 `{}`: a fake with a permissive
	// default is how the invite POST stayed invisible for as long as it
	// did, since the one request nobody had thought to match was answered
	// exactly like the ones that had been.
	unexpected []string
}

func (o *atlassianOrg) mutations() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.writes
}

func (o *atlassianOrg) serve(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path

	switch {
	// ---- queries, whatever the method they are made with -------------- //

	// THE WORKSPACE LISTING IS A POST AND IS NOT A MUTATION. It changes
	// nothing and it is how the site is discovered rather than asked for,
	// so it is made on every pass by design.
	case r.Method == http.MethodPost && path == "/admin/v2/orgs/"+testOrgID+"/workspaces":
		_, _ = w.Write([]byte(`{"data":[{"id":"ari:cloud:jira::site/` + testCloudID + `",` +
			`"attributes":{"type":"JiraSoftware",` +
			`"hostUrl":"https://nimbus.atlassian.invalid"}}]}`))

	case r.Method == http.MethodGet && path == serviceAccounts:
		// One page and no next link: the client follows next links, and a
		// fake that always offered one would run it into its own page cap.
		_ = json.NewEncoder(w).Encode(map[string]any{"items": o.accounts})

	case r.Method == http.MethodGet && strings.HasSuffix(path, "/manage/api-tokens"):
		account := o.find(accountIn(path, "/manage/api-tokens"))
		if account == nil {
			o.refuse(w, r, path)
			return
		}
		tokens := make([]map[string]string, account.tokens)
		for i := range tokens {
			tokens[i] = map[string]string{"id": fmt.Sprintf("token-%d", i)}
		}
		_ = json.NewEncoder(w).Encode(tokens)

	// ---- the four mutations ------------------------------------------- //

	case r.Method == http.MethodPost && path == serviceAccounts:
		o.writes++
		var body struct {
			DisplayName string `json:"displayName"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		o.next++
		account := &serviceAccount{
			ID:          fmt.Sprintf("acct-%d", o.next),
			DisplayName: body.DisplayName,
			// ATLASSIAN INVENTS THE ADDRESS, which is the whole reason a
			// seat has an EmailVar: nobody can write it down in advance.
			Email: fmt.Sprintf("acct-%d@nimbus.invalid", o.next),
		}
		o.accounts = append(o.accounts, account)
		_ = json.NewEncoder(w).Encode(account)

	case r.Method == http.MethodPost && path == serviceAccounts+"/invite":
		o.writes++
		var body struct {
			UserIDs         []string                   `json:"userIds"`
			PermissionRules []atlassian.PermissionRule `json:"permissionRules"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, id := range body.UserIDs {
			if account := o.find(id); account != nil {
				account.grants = body.PermissionRules
			}
		}
		_, _ = w.Write([]byte(`{}`))

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/manage/api-tokens"):
		account := o.find(accountIn(path, "/manage/api-tokens"))
		if account == nil {
			// Counted only once something actually changed: a mint aimed at
			// an account that does not exist mutated nothing, and counting
			// it would let a refused write mask a real one.
			o.refuse(w, r, path)
			return
		}
		o.writes++
		account.tokens++
		_, _ = fmt.Fprintf(w, `{"token":"minted-for-%s-%d"}`, account.ID, account.tokens)

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/manage/lifecycle/delete"):
		o.writes++
		_, _ = w.Write([]byte(`{}`))

	default:
		o.refuse(w, r, path)
	}
}

// refuse answers a route this organization does not serve, and remembers it.
func (o *atlassianOrg) refuse(w http.ResponseWriter, r *http.Request, path string) {
	o.unexpected = append(o.unexpected, r.Method+" "+path)
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"message":"this organization serves no such route"}`))
}

func (o *atlassianOrg) find(id string) *serviceAccount {
	for _, account := range o.accounts {
		if account.ID == id {
			return account
		}
	}
	return nil
}

// accountIn pulls the account id out of a /users/{id}/… route.
func accountIn(path, suffix string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), suffix)
}

// sealedStore is this deployment's own sealed store, and the second place a
// pass can write.
//
// IT HAS NO Mints METHOD, so [provision.CanMint] answers true — which is what
// makes the pass do its real work. A sink that cannot mint short-circuits
// every seat before the first request, and a harness built on one would
// certify a pass that never ran.
type sealedStore struct {
	mu     sync.Mutex
	values map[string]string
	writes int
}

func (s *sealedStore) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[name] = value
	s.writes++
	return nil
}

func (s *sealedStore) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, held := s.values[name]
	return value, held, nil
}

func (s *sealedStore) held(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[name]
}

func (s *sealedStore) mutations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func (s *sealedStore) Discard(context.Context) error { return nil }
func (s *sealedStore) Flush(context.Context) error   { return nil }
func (s *sealedStore) Describe() string              { return "a sealed store this test counts" }
func (s *sealedStore) NextStep() string              { return "" }

// convergedOrg is an Atlassian organization this pass has already converged,
// plus the pass itself as [integration.Reconciler] sees it.
type convergedOrg struct {
	vendor *atlassianOrg
	store  *sealedStore
	opts   atlassian.Options
}

// Kind names the surface. A constant rather than anything derived, which is
// what makes the suite's stability clause an assertion about the adapter
// rather than about a field.
func (*convergedOrg) Kind() integration.Kind { return integration.KindAtlassian }

// Reconcile runs the real pass and maps its answer the way the loop's own
// adapter does.
//
// IT MIRRORS internal/engine's atlassianPass.Run, with one part left out and
// named rather than quietly dropped: that pass also records the site it
// discovered into the company's jira and confluence blocks, which is a write
// to the CONFIG PLANE through a writer only the engine holds. It converges
// after one apply and is not this package's to certify; nothing here can
// stand it up, and pretending otherwise would be the vacuous shape this file
// exists to replace.
func (w *convergedOrg) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := atlassian.Reconcile(ctx, w.opts)
	if err != nil {
		return nil, err
	}
	return res.Findings(), nil
}

// mutations is every write since this world was built: at Atlassian, and into
// this deployment's own sealed store.
func (w *convergedOrg) mutations() int { return w.vendor.mutations() + w.store.mutations() }

// nimbus is the company, as the engine's own pass reads it — through
// [atlassian.PlanFor] over a real organization rather than a hand-built plan.
//
// TWO SEATS, because every write this pass made on a converged organization
// was per-seat: one seat counts 1 where the bug's shape is N, and a harness
// cannot tell "wrote once" from "wrote once per seat" with one of them.
func nimbus() *org.Organization {
	return &org.Organization{
		Name: "Nimbus",
		Roles: []*org.Role{
			{Name: "SRE Lead", MCPEnv: map[string]map[string]string{
				"jira": {
					"JIRA_API_TOKEN": "${ATLASSIAN_TOKEN_SRE_LEAD}",
					"JIRA_USERNAME":  "${ATLASSIAN_EMAIL_SRE_LEAD}",
				},
			}},
			{Name: "CTO", MCPEnv: map[string]map[string]string{
				"atlassian": {
					"ATLASSIAN_API_TOKEN": "${ATLASSIAN_TOKEN_CTO}",
					"ATLASSIAN_EMAIL":     "${ATLASSIAN_EMAIL_CTO}",
				},
			}},
		},
	}
}

// convergedAtlassian stands the organization up and converges it BY RUNNING
// THE PASS.
//
// # Converged by the pass rather than by a fixture
//
// The alternative is a hand-written fixture that seeds the accounts, their
// grants and their tokens. It is the tempting one and it is weaker in exactly
// the direction that matters: it encodes what the harness author BELIEVES a
// converged organization looks like, so a pass whose idea of converged has
// drifted from that belief writes on every run and the fixture calls it
// converged anyway. This vendor punishes that harder than most — a display
// name one character off [atlassian.AccountName] makes every pass create a
// second identity, and an empty token listing makes every pass mint over a
// live credential — and both would look like a passing test. Running the real
// pass makes the world converged by definition, and leaves only the question
// worth asking: whether the SECOND pass writes.
//
// The seeding pass's own writes are not counted: [integrationtest.Run] takes
// its baseline from Mutations() after New returns.
//
// BOTH t AND tb, deliberately. [integrationtest.TB] is the narrow interface
// the suite's own tests drive its cases with and it has no Cleanup, so the
// case's own reporter is tb while the server's lifetime hangs off the real
// *testing.T of the subtest that owns this world.
func convergedAtlassian(
	t *testing.T, tb integrationtest.TB, tune func(*org.Organization), wantNotes int,
) *convergedOrg {
	t.Helper()
	vendor := &atlassianOrg{}
	srv := httptest.NewServer(http.HandlerFunc(vendor.serve))
	t.Cleanup(srv.Close)

	company := nimbus()
	if tune != nil {
		tune(company)
	}
	plan, err := atlassian.PlanFor(company)
	if err != nil {
		tb.Fatalf("PlanFor: %v", err)
	}
	store := &sealedStore{}
	world := &convergedOrg{vendor: vendor, store: store, opts: atlassian.Options{
		Client: atlassian.NewClient(atlassian.ClientOptions{BaseURL: srv.URL}),
		OrgID:  testOrgID, Key: "an-organization-api-key", Plan: plan, Sink: store,
		// PINNED, so nothing here depends on the wall clock: the mint
		// stamps its label and its expiry with this.
		Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}}

	findings, err := world.Reconcile(context.Background())
	if err != nil {
		tb.Fatalf("the pass that converges the world failed: %v", err)
	}
	for _, f := range findings {
		// A NOTE ABOUT A SEAT THE COMPANY PROVISIONS BY HAND SURVIVES
		// CONVERGENCE — no pass can clear it — and nothing else may.
		if f.Kind != integration.FindingGrantShort {
			tb.Fatalf("the world is not converged: the seeding pass reported %+v", findings)
		}
	}
	// AND THE COUNT IS PINNED, so the two clauses about findings cannot go
	// vacuous without saying so. Both of them walk the pass's findings, so a
	// world that reported none would satisfy each by having nothing to
	// check — which is the same silent weakening as a suite driven by a stub.
	if len(findings) != wantNotes {
		tb.Fatalf("the seeding pass reported %d finding(s), want %d: %+v",
			len(findings), wantNotes, findings)
	}
	// AND IT ACTUALLY DID THE WORK. A seeding pass that wrote nothing would
	// leave every case below true about an empty organization, which is the
	// exact vacuous pass this file exists to replace.
	if vendor.mutations() == 0 || store.mutations() == 0 {
		tb.Fatalf("the seeding pass made %d write(s) at Atlassian and %d into the "+
			"store, so there is no converged world here to certify a second pass "+
			"against", vendor.mutations(), store.mutations())
	}
	vendor.mu.Lock()
	defer vendor.mu.Unlock()
	if len(vendor.unexpected) > 0 {
		tb.Fatalf("the pass called routes this organization does not serve: %v",
			vendor.unexpected)
	}
	if len(vendor.accounts) != len(plan.Seats) {
		tb.Fatalf("the organization holds %d account(s) for %d planned seat(s)",
			len(vendor.accounts), len(plan.Seats))
	}
	for _, account := range vendor.accounts {
		// THE GRANT AND THE TOKEN ARE BOTH LOAD-BEARING for what comes
		// next. The second pass reads the token count as its evidence that
		// this account was granted, so an account that reached converged
		// with no token would make it grant again and the clause below
		// would fail for a reason that has nothing to do with the bug.
		if len(account.grants) != len(atlassian.Products) {
			tb.Fatalf("%s holds %d product grant(s), want %d",
				account.DisplayName, len(account.grants), len(atlassian.Products))
		}
		if account.tokens != 1 {
			tb.Fatalf("%s holds %d API token(s), want exactly one",
				account.DisplayName, account.tokens)
		}
	}
	for _, seat := range plan.Seats {
		if store.held(seat.TokenVar) == "" {
			tb.Fatalf("%s holds no token after the seeding pass, so the second "+
				"pass would mint rather than keep", seat.TokenVar)
		}
		if seat.EmailVar != "" && store.held(seat.EmailVar) == "" {
			tb.Fatalf("%s holds no address after the seeding pass, so the second "+
				"pass would re-seal rather than keep", seat.EmailVar)
		}
	}
	return world
}

// THE CONTRACT IS CERTIFIED AGAINST THE REAL RECONCILER.
func TestTheAtlassianReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	for _, world := range []struct {
		name string
		tune func(*org.Organization)
		// notes is how many findings a converged pass over this world still
		// reports. See the count pinned in convergedAtlassian.
		notes int
	}{
		// Every seat opted in, which is the shape the loop spends its life
		// on: two accounts, two grants, two tokens and two addresses, all
		// already there, and nothing left to say about any of it.
		{name: "every seat provisioned"},

		// And a company that manages some of Atlassian by hand. Both of
		// these are CONVERGED — no pass can do anything more for either
		// seat — and both leave a note behind, which is what makes the
		// suite's two clauses about findings say something here instead of
		// running over an empty slice.
		{name: "seats the company provisions by hand", notes: 2, tune: func(o *org.Organization) {
			o.Roles = append(o.Roles,
				// A literal where a ${VAR} belongs: nowhere to write a
				// minted token, so the plan leaves the seat out entirely.
				&org.Role{Name: "Data Analyst", MCPEnv: map[string]map[string]string{
					"jira": {"JIRA_API_TOKEN": "a-token-somebody-pasted-in"},
				}},
				// And a token slot with no address slot beside it. This one
				// IS provisioned — it gets an account, a grant and a token —
				// and the address Atlassian assigned has nowhere to go, so
				// the seat authenticates as nobody until a person adds one.
				&org.Role{Name: "Release Manager", MCPEnv: map[string]map[string]string{
					"confluence": {"CONFLUENCE_API_TOKEN": "${ATLASSIAN_TOKEN_RELEASE_MANAGER}"},
				}},
			)
		}},
	} {
		t.Run(world.name, func(t *testing.T) {
			t.Parallel()
			// Assigned by New and read by Mutations, which the suite calls
			// in that order within one sequential case.
			var current *convergedOrg
			integrationtest.Run(t, integrationtest.Reconciler{
				New: func(tb integrationtest.TB) integration.Reconciler {
					current = convergedAtlassian(t, tb, world.tune, world.notes)
					return current
				},
				Mutations: func() int { return current.mutations() },
			})
		})
	}
}
