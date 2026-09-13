package atlassian_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// The Atlassian half of [integration.Reconciler]'s safety contract, certified
// against the reconcile the loop actually runs.
//
// # Why this file exists at all
//
// integrationtest states the contract in nine clauses and its package doc
// calls the write-counting hook REQUIRED, because that clause is "the one most
// likely to be wrong". Its only caller was a stub whose pass returned
// (nil, nil), so "a converged pass writes nothing" held because nothing
// happened — a green test over a world nobody had built and a reconciler
// nobody had run. This points the same cases at [atlassian.Reconcile], over
// organizations the pass itself has brought into line, and counts what those
// organizations and this deployment's own sealed store receive.
//
// It found the clause false. The pass sent the product-access invite once per
// seat on every tick for ever, and re-sealed each seat's account address
// beside it. See the grant in reconcile.go for what replaced it.
//
// # TWO WORLDS, and the second one is not decoration
//
// A converged pass reports NO findings — that is what converged means — so
// every case that walks a findings list walks an empty one and certifies
// whatever this vendor happens to do. Three of the nine clauses were in that
// state here. So each row below stands up a pair: an organization this pass
// has converged, and one where something is genuinely wrong IN A WAY A PERSON
// HAS TO ACT ON. The two rows break in DIFFERENT ways on purpose — a scoped
// organization key that cannot create an account, and a node with no keyring
// to seal a token into — because between them they are the only worlds in
// this package that put all four of this vendor's finding kinds through the
// suite. Each outstanding world pins the EXACT kinds it reports, so it cannot
// quietly converge and go back to certifying nothing.
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
// The fifth write is not an HTTP call at all: every [provision.TokenSink]
// Record is counted too. The sealed store is the company's, shared by the
// whole fleet and stamped with an author and a time, so a pass re-sealing the
// same address every tick is writing exactly as surely as one that POSTs.
//
// And the sixth is a request this fake does NOT serve — see
// [atlassianOrg.mutations] for why a route nobody matched counts as a write
// rather than as a curiosity.
//
// [provision.TokenSink.Flush] is deliberately NOT counted. A write-through
// sink makes nothing newly durable there, and the loop's own sink rebuilds
// its `${VAR}` snapshot only when the run actually sealed something — so a
// converged pass completes its run, seals nothing, and rebuilds nothing. That
// the pass reaches the completion AT ALL is pinned in reconcile_test.go,
// because it is a claim about a path this suite's converged world never takes.
//
// # One clause this vendor does not make falsifiable, said out loud
//
// "a cancelled pass reports a fault rather than health" passes here whatever
// [atlassian.Reconcile] does at the top of a pass: with the guard removed, the
// site discovery's own request fails on the dead context and the pass raises
// anyway. What that guard actually protects — not accusing
// integrations.atlassian.api_key when a node is merely draining — is pinned by
// TestACancelledPassRaisesRatherThanBlamingTheOrganizationCredential, and by
// nothing in this file. Nobody should read the clause's green tick as
// coverage of it.

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

	// scopedKey is an organization whose API key was created WITH scopes,
	// which is the misconfiguration this vendor punishes most quietly.
	// Everything here authenticates with it except the one call that
	// creates an account: account-management refuses a scoped key with 403
	// whatever scopes it holds (see client.go's serviceAccountsPath). So
	// the site is discovered, the listing answers, and every agent is left
	// without an identity — which is why the connect form says "create it
	// without scopes" and why this is the outstanding world worth having.
	scopedKey bool

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

// mutations is every request that changed this organization — plus every
// request it does not serve at all.
//
// THE SECOND HALF IS NOT BOOKKEEPING, AND IT IS THE HOLE THIS HARNESS SHIPPED
// WITH. `unexpected` was recorded by [atlassianOrg.refuse] and then looked at
// in exactly one place: after the pass that converges a world. So a write the
// pass made ONLY once that world was converged, to a route this fake does not
// answer, was refused with a 404, remembered, and read by nobody — and the one
// clause that exists to catch exactly that went green. It is reachable: a
// stray call whose error the pass swallows leaves no other trace at all.
//
// A route nobody matched is not a request this harness knows to be harmless.
// It is a request whose effect at the real Atlassian nothing here can vouch
// for, and for a clause about writes the safe direction is to count it.
func (o *atlassianOrg) mutations() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.writes + len(o.unexpected)
}

// strayRoutes is what this organization was asked for and does not serve.
//
// Read where [atlassianOrg.mutations] cannot see it: before the suite takes
// its baseline — the pass that converges a world runs first and its writes are
// deliberately uncounted — and over the outstanding world, where nothing
// samples the counter at all.
func (o *atlassianOrg) strayRoutes() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.unexpected)
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
		if o.scopedKey {
			// NOT COUNTED: nothing changed. The same rule the refused mint
			// below follows — counting a refusal would let it mask a real
			// write, and this route is refused on every pass by design in
			// the world that sets this.
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"The API key is scoped. Creating a ` +
				`service account requires an organization API key created ` +
				`without scopes."}`))
			return
		}
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
// every seat before the first request, and a harness whose CONVERGED world was
// built on one would certify a pass that never ran. That posture is a world in
// its own right here, and it is the second row's outstanding one.
type sealedStore struct {
	// forgotten is what a teardown asked this sink to delete.
	forgotten []string

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

// Forget implements [provision.TokenSink]: it records what a teardown
// asked to be deleted, so a case can assert the deletion happened.
func (s *sealedStore) Forget(_ context.Context, names ...string) error {
	s.forgotten = append(s.forgotten, names...)
	return nil
}

// Flush is NOT counted. A write-through sink makes nothing newly durable
// completing a run, and the loop's own sink rebuilds its `${VAR}` snapshot
// only where this one incremented writes. See this file's header.
func (s *sealedStore) Flush(context.Context) error { return nil }
func (s *sealedStore) Describe() string            { return "a sealed store this test counts" }
func (s *sealedStore) NextStep() string            { return "" }

// world is one Atlassian organization, this deployment's own sealed store, and
// the pass over both as [integration.Reconciler] sees it.
type world struct {
	vendor *atlassianOrg
	store  *sealedStore
	opts   atlassian.Options
}

// Kind names the surface. A constant rather than anything derived, which is
// what makes the suite's stability clause an assertion about the adapter
// rather than about a field.
func (*world) Kind() integration.Kind { return integration.KindAtlassian }

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
func (w *world) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := atlassian.Reconcile(ctx, w.opts)
	if err != nil {
		return nil, err
	}
	return res.Findings(), nil
}

// mutations is every write since this world was built: at Atlassian, and into
// this deployment's own sealed store.
func (w *world) mutations() int { return w.vendor.mutations() + w.store.mutations() }

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

// standUp builds one Atlassian organization, the company that wants identities
// in it, and the pass over both. NO PASS HAS RUN YET — what each caller does
// with this world is what makes it converged or outstanding.
//
// BOTH t AND tb, deliberately. [integrationtest.TB] is the narrow interface
// the suite's own tests drive its cases with and it has no Cleanup, so the
// case's own reporter is tb while the server's lifetime hangs off the real
// *testing.T of the subtest that owns this world.
func standUp(
	t *testing.T, tb integrationtest.TB,
	vendor *atlassianOrg, tune func(*org.Organization), keyring bool,
) *world {
	t.Helper()
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

	// A NODE WITH NO KEYRING IS HANDED [provision.ReadOnly], WHICH IS NOT
	// NIL. nil is the command line's check; this is a running node that has
	// been told it may seal nothing, and the pass has to tell the two apart
	// — see [atlassian.Options.Sink]. The store is still built either way so
	// the write counter reads the same on both, and stays at zero on this
	// one, which is the point.
	var sink provision.TokenSink = store
	if !keyring {
		sink = provision.ReadOnly()
	}

	return &world{vendor: vendor, store: store, opts: atlassian.Options{
		Client: atlassian.NewClient(atlassian.ClientOptions{BaseURL: srv.URL}),
		OrgID:  testOrgID, Key: "an-organization-api-key", Plan: plan, Sink: sink,
		// PINNED, so nothing here depends on the wall clock: the mint
		// stamps its label and its expiry with this.
		Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}}
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
// worth asking: whether a pass over a world that needs nothing writes.
//
// Nothing this function does is counted: [integrationtest.Run] takes its
// baseline from Mutations() after Converged returns, so the seeding pass's
// writes — and the steady-state probe below it — are inside the baseline. The
// probe does not blunt the clause: the writes it is about are made once per
// pass, so a pass that writes still writes on the one the suite measures. The
// only thing it changes is that the measured pass is the third rather than the
// second, over a world that is if anything more converged.
func convergedAtlassian(
	t *testing.T, tb integrationtest.TB, tune func(*org.Organization), wantNotes int,
) *world {
	t.Helper()
	vendor := &atlassianOrg{}
	w := standUp(t, tb, vendor, tune, true)
	plan := w.opts.Plan

	// MORE THAN ONE PASS, because this vendor genuinely takes more than one.
	//
	// Granting product access is accepted immediately and applied over the
	// next minute, so the pass that MAKES the grant reports a wait rather
	// than a finished seat ([atlassian.SeatResult.Granted]) — and it is
	// right to: for that minute the products refuse the credential it just
	// minted. A single pass therefore does not converge this world, and this
	// helper's own doc used to say it did.
	//
	// BOUNDED AND SILENT ON SUCCESS. Looping until nothing changes would
	// hide a vendor that never settles, which is the one thing this whole
	// suite is for, so the bound is small and the assertion below is what
	// reports a world still moving when it runs out.
	var findings []integration.Finding
	for range 3 {
		var err error
		findings, err = w.Reconcile(context.Background())
		if err != nil {
			tb.Fatalf("the pass that converges the world failed: %v", err)
		}
		if !slices.ContainsFunc(findings, func(f integration.Finding) bool {
			return f.Kind != integration.FindingGrantShort
		}) {
			break
		}
	}
	for _, f := range findings {
		// A NOTE ABOUT A SEAT THE COMPANY PROVISIONS BY HAND SURVIVES
		// CONVERGENCE — no pass can clear it — and nothing else may.
		if f.Kind != integration.FindingGrantShort {
			tb.Fatalf("the world is not converged: the seeding pass reported %+v", findings)
		}
	}
	// AND THE COUNT IS PINNED, so a converged world cannot quietly grow a
	// finding nobody expected — or lose the ones the second row is built to
	// carry through the pass.
	if len(findings) != wantNotes {
		tb.Fatalf("the seeding pass reported %d finding(s), want %d: %+v",
			len(findings), wantNotes, findings)
	}
	// AND CONVERGING IT CHANGED NOTHING ABOUT WHAT IS REPORTED.
	//
	// Everything above is about the pass that BUILT this world, and the
	// suite's own two clauses about findings now run over the outstanding
	// twin — so a finding this vendor reports only once a world is converged
	// is seen by nothing at all. Nor by `two passes over an unchanged world
	// agree`: that compares two steady-state passes with each other and is
	// satisfied by any two identical wrong answers. This compares the steady
	// state with what the pass that converged it said, which is the
	// comparison that an integration reporting degraded on every tick of a
	// company that is fine actually fails — and the state an operator
	// watching a dashboard is in for the life of the deployment.
	steady, err := w.Reconcile(context.Background())
	if err != nil {
		tb.Fatalf("the second pass over the converged world failed: %v", err)
	}
	if !integration.SameAll(steady, findings) {
		tb.Fatalf("converging this world changed what the pass reports about it:\n"+
			"seeding: %+v\n steady: %+v", findings, steady)
	}
	// AND IT ACTUALLY DID THE WORK. A seeding pass that wrote nothing would
	// leave every case below true about an empty organization, which is the
	// exact vacuous pass this file exists to replace.
	if vendor.mutations() == 0 || w.store.mutations() == 0 {
		tb.Fatalf("the seeding pass made %d write(s) at Atlassian and %d into the "+
			"store, so there is no converged world here to certify a second pass "+
			"against", vendor.mutations(), w.store.mutations())
	}
	// STRAY ROUTES ON THE SEEDING PASS, which is the ONE window
	// [atlassianOrg.mutations] cannot see: the suite takes its baseline after
	// this returns, so everything here — including a 404 nobody looked at —
	// is already inside it. A stray call made only on the create-and-mint
	// path shows up here and nowhere else.
	if stray := vendor.strayRoutes(); len(stray) > 0 {
		tb.Fatalf("the pass that converges the world called routes this "+
			"organization does not serve: %v", stray)
	}
	vendor.mu.Lock()
	defer vendor.mu.Unlock()
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
		if w.store.held(seat.TokenVar) == "" {
			tb.Fatalf("%s holds no token after the seeding pass, so the second "+
				"pass would mint rather than keep", seat.TokenVar)
		}
		if seat.EmailVar != "" && w.store.held(seat.EmailVar) == "" {
			tb.Fatalf("%s holds no address after the seeding pass, so the second "+
				"pass would re-seal rather than keep", seat.EmailVar)
		}
	}
	return w
}

// broken is what is wrong in one row's OUTSTANDING world.
//
// Each shape is a real misconfiguration this vendor produces, chosen because
// A PERSON — never the engine, never Atlassian — is the only thing that ends
// it. The suite's anti-vacuity clause asks for at least one finding and at
// least one somebody owes; kinds asks for more than that, because "at least
// one" is satisfied by a world that has drifted into reporting something
// else entirely.
type broken struct {
	// what this world is, for the failure messages.
	what string

	// scopedKey makes the account create refuse with the 403 Atlassian
	// answers a scoped organization key. See [atlassianOrg.scopedKey].
	scopedKey bool

	// keyring is whether this node can seal a credential at all. False
	// hands the pass [provision.ReadOnly], which is what the reconcile loop
	// hands a node with no secrets.keys.
	keyring bool

	// kinds is EXACTLY what a pass over this world reports, in order. Pinned
	// rather than counted: a world that starts answering identity_missing
	// where it answered identity_failed has stopped being about a person,
	// and every clause downstream would go on passing.
	kinds []integration.FindingKind
}

// outstandingAtlassian stands up a world where something is genuinely wrong.
//
// NO CONVERGING PASS RUNS HERE, and that is the difference from the twin
// above: this world is wrong when it is built and stays wrong however many
// times the suite reconciles it, which is what makes the clauses that walk a
// findings list mean something. A pass over it is allowed to write — it has
// work to do — and nothing samples the counter.
func outstandingAtlassian(
	t *testing.T, tb integrationtest.TB, tune func(*org.Organization), b broken,
) *world {
	t.Helper()
	vendor := &atlassianOrg{scopedKey: b.scopedKey}
	w := standUp(t, tb, vendor, tune, b.keyring)

	// THE WORLD PROVES ITSELF BEFORE THE SUITE SEES IT. The suite's own
	// anti-vacuity clause would catch a world that reported nothing at all;
	// it would not catch one that reported the wrong thing, and the two
	// clauses after it — every finding is a known kind, a person's finding
	// says what to do — would then certify whatever this world happened to
	// drift into.
	findings, err := w.Reconcile(context.Background())
	if err != nil {
		tb.Fatalf("a pass over the outstanding world (%s) raised rather than "+
			"reporting: %v", b.what, err)
	}
	got := make([]integration.FindingKind, 0, len(findings))
	for _, f := range findings {
		got = append(got, f.Kind)
	}
	if !slices.Equal(got, b.kinds) {
		tb.Fatalf("the outstanding world (%s) reported %v, want exactly %v: %+v",
			b.what, got, b.kinds, findings)
	}
	// AND NOT BY REACHING FOR ANYTHING THIS ORGANIZATION DOES NOT SERVE.
	// Nothing samples [atlassianOrg.mutations] over this world, so a stray
	// route made only on a failing path would be recorded here and read
	// nowhere at all.
	if stray := vendor.strayRoutes(); len(stray) > 0 {
		tb.Fatalf("the pass over the outstanding world (%s) called routes this "+
			"organization does not serve: %v", b.what, stray)
	}
	return w
}

// THE CONTRACT IS CERTIFIED AGAINST THE REAL RECONCILER.
func TestTheAtlassianReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		tune func(*org.Organization)
		// notes is how many findings a converged pass over this world still
		// reports. See the count pinned in convergedAtlassian.
		notes int
		// broken is the same company with something genuinely wrong with it.
		broken broken
	}{
		// Every seat opted in, which is the shape the loop spends its life
		// on: two accounts, two grants, two tokens and two addresses, all
		// already there, and nothing left to say about any of it.
		{
			name: "every seat provisioned",
			broken: broken{
				// THE MISCONFIGURATION THIS VENDOR HIDES BEST. A scoped
				// organization API key authenticates for everything here
				// except creating an account, which is refused 403 whatever
				// scopes it holds — so the site is discovered, the listing
				// answers, and every agent silently has no identity. Only a
				// person can fix it, at admin.atlassian.com, by issuing an
				// unscoped key.
				what: "an organization API key created with scopes", scopedKey: true, keyring: true,
				kinds: []integration.FindingKind{
					integration.FindingIdentityFailed,
					integration.FindingIdentityFailed,
				},
			},
		},

		// And a company that manages some of Atlassian by hand. Both of
		// these are CONVERGED — no pass can do anything more for either
		// seat — and both leave a note behind, which is what makes the
		// suite's two clauses about findings say something here instead of
		// running over an empty slice.
		{
			name:  "seats the company provisions by hand",
			notes: 2,
			tune: func(o *org.Organization) {
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
			},
			broken: broken{
				// THE OTHER END OF THE SAME PASS, and the one no retry ever
				// clears. A node handed [provision.ReadOnly] has nowhere to
				// seal a minted token, so it must not create the account
				// either — and it stays that way until somebody sets
				// secrets.keys in the bootstrap configuration. The three
				// seats report as having no account because they genuinely
				// have none, and the two notes survive from the plan.
				what: "a node with no keyring", keyring: false,
				kinds: []integration.FindingKind{
					integration.FindingCredentialMissing,
					integration.FindingIdentityMissing,
					integration.FindingIdentityMissing,
					integration.FindingIdentityMissing,
					integration.FindingGrantShort,
					integration.FindingGrantShort,
				},
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			// Assigned by Converged and read by Mutations, which the suite
			// calls in that order within one sequential case. OUTSTANDING
			// DOES NOT TOUCH IT: nothing counts writes over a world that has
			// work to do, and letting one built there land here would point
			// the load-bearing clause at the wrong organization.
			var current *world
			integrationtest.Run(t, integrationtest.Reconciler{
				Converged: func(tb integrationtest.TB) integration.Reconciler {
					current = convergedAtlassian(t, tb, row.tune, row.notes)
					return current
				},
				Outstanding: func(tb integrationtest.TB) integration.Reconciler {
					return outstandingAtlassian(t, tb, row.tune, row.broken)
				},
				Mutations: func() int { return current.mutations() },
			})
		})
	}
}
