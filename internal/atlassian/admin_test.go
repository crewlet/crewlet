package atlassian_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
)

func TestTheAccountListingFollowsItsOwnPages(t *testing.T) {
	t.Parallel()
	// A listing that stopped at the first page would report accounts that
	// EXIST as missing — and the repair for "missing" is to create a
	// second identity on top of a live one.
	fake := newFakeOrg(t)
	for i := range 205 {
		fake.seed(fmt.Sprintf("Crewlet Agent %03d", i))
	}
	accounts, err := fake.admin(t).ServiceAccounts(context.Background(), fakeOrgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 205 {
		t.Fatalf("listed %d accounts, want 205 — a page was dropped", len(accounts))
	}
	seen := map[string]bool{}
	for _, account := range accounts {
		if seen[account.ID] {
			t.Fatalf("account %s was listed twice", account.ID)
		}
		seen[account.ID] = true
	}
}

func TestAScopedKeyIsRefusedWithTheFixRatherThanAStatus(t *testing.T) {
	t.Parallel()
	// THE WALL EVERY FIRST RUN HITS. account-management refuses a scoped
	// organization key with a flat 403, which reads as "you do not have
	// permission" when the truth is "this key has the wrong shape".
	fake := newFakeOrg(t)
	client, err := atlassian.NewAdminClient(atlassian.AdminOptions{
		BaseURL: fake.srv.URL, Key: "a-scoped-key", HTTP: fake.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ServiceAccounts(context.Background(), fakeOrgID)
	if !errors.Is(err, atlassian.ErrUnscopedKeyRequired) {
		t.Fatalf("err = %v, want ErrUnscopedKeyRequired", err)
	}
}

func TestAMintSendsExpiryRatherThanExpiresAt(t *testing.T) {
	t.Parallel()
	// `expiresAt` is the name Atlassian uses on other surfaces and fails
	// here with INVALID_EXPIRY — which reads as though no expiry had been
	// sent at all, so the failure names the wrong thing.
	fake := newFakeOrg(t)
	account := fake.seed("Crewlet Agent SWE")
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	label := atlassian.TokenLabel("swe") + "-" + fmt.Sprint(now.Unix())
	token, err := fake.admin(t).MintToken(context.Background(), account.AtlassianID,
		label, atlassian.Scopes([]atlassian.Product{atlassian.ProductJira}),
		300*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if token.Token == "" {
		t.Fatal("no token value")
	}
	body := fake.wrote("/users/" + account.AtlassianID + "/manage/api-tokens")
	if len(body) != 1 {
		t.Fatalf("%d mint request(s)", len(body))
	}
	if _, wrong := body[0]["expiresAt"]; wrong {
		t.Error("the mint sent expiresAt, which Atlassian rejects")
	}
	if got := str(body[0]["expiry"]); got != "2026-12-26T12:00:00Z" {
		t.Errorf("expiry = %q, want the lifetime added to the given clock", got)
	}
	// THE CALLER'S LABEL IS SENT VERBATIM. It is the only handle a rollback
	// has on a credential whose id was never returned, so a label this
	// client decorated for itself would differ from the one the run
	// remembers — and the cleanup would match nothing while reporting that
	// it had revoked everything.
	if got := str(body[0]["label"]); got != label {
		t.Errorf("label sent = %q, want the caller's %q", got, label)
	}
}

func TestAMintThatReturnsNoValueIsNamedRatherThanSilentlyEmpty(t *testing.T) {
	t.Parallel()
	// The value exists ONLY in the creation response, so there is nothing
	// to retry against — and the credential may nonetheless exist on the
	// account. Named rather than returned as an empty string, because a
	// caller that recorded "" would report the seat as provisioned and
	// leave a live credential nobody can account for.
	fake := newFakeOrg(t)
	fake.mintReturnsNothing = true
	account := fake.seed("Crewlet Agent SWE")

	token, err := fake.admin(t).MintToken(context.Background(), account.AtlassianID,
		atlassian.TokenLabel("swe")+"-1", nil, time.Hour, time.Now())
	if !errors.Is(err, atlassian.ErrTokenNotReturned) {
		t.Fatalf("err = %v, want ErrTokenNotReturned", err)
	}
	// THE TOKEN COMES BACK WITH THE ERROR. Its id is the direct handle on
	// a credential that now exists and cannot be used; a caller given only
	// the error would have to hunt for it by label instead.
	if token == nil || token.ID == "" {
		t.Fatalf("token = %+v, want the id of the credential that was created", token)
	}
}

func TestAMintWithNoIDLeavesTheLabelAsTheOnlyHandle(t *testing.T) {
	t.Parallel()
	// Neither a value nor an id: everything that manages a credential
	// afterwards keys on the id, and [retirePrevious] in particular skips
	// the token it was told to KEEP by id — so an empty one would make the
	// fresh credential match the retire prefix like every other and be
	// revoked seconds after it was recorded. Refused here so the caller
	// rolls the seat back by the label it sent instead.
	fake := newFakeOrg(t)
	fake.mintReturnsNothing = true
	fake.mintReturnsNoID = true
	account := fake.seed("Crewlet Agent SWE")

	_, err := fake.admin(t).MintToken(context.Background(), account.AtlassianID,
		atlassian.TokenLabel("swe")+"-1", nil, time.Hour, time.Now())
	if !errors.Is(err, atlassian.ErrTokenNotReturned) {
		t.Fatalf("err = %v, want the missing VALUE named first", err)
	}

	fake.mintReturnsNothing = false
	_, err = fake.admin(t).MintToken(context.Background(), account.AtlassianID,
		atlassian.TokenLabel("swe")+"-2", nil, time.Hour, time.Now())
	if !errors.Is(err, atlassian.ErrUnexpected) {
		t.Fatalf("err = %v, want a credential with no id refused", err)
	}
}

func TestAFullOrganizationStopsTheRunWithOneAnswer(t *testing.T) {
	t.Parallel()
	// Atlassian answers a full organization with a payment status or a
	// conflict depending on WHY it is full, and both mean the same thing
	// to an operator: free a seat or raise the allowance.
	fake := newFakeOrg(t)
	fake.quotaFull = true

	_, err := fake.admin(t).CreateServiceAccount(context.Background(), fakeOrgID, "Crewlet Agent", "why")
	if !errors.Is(err, atlassian.ErrQuotaExceeded) {
		t.Fatalf("create err = %v, want ErrQuotaExceeded", err)
	}
	err = fake.admin(t).GrantLicence(context.Background(), fakeOrgID, fakeCloudID, "aid-1", atlassian.ProductJira)
	if !errors.Is(err, atlassian.ErrQuotaExceeded) {
		t.Fatalf("grant err = %v, want ErrQuotaExceeded", err)
	}
}

func TestAJustCreatedAccountIsNotReadyRatherThanMissing(t *testing.T) {
	t.Parallel()
	// Atlassian answers a grant against a just-created account with a 404
	// that reads exactly like "no such account". Naming it is what lets
	// the caller WAIT instead of concluding the account failed to create
	// and making a second one.
	fake := newFakeOrg(t)
	client := fake.admin(t)
	account, err := client.CreateServiceAccount(context.Background(), fakeOrgID, "Crewlet Agent SWE", "why")
	if err != nil {
		t.Fatal(err)
	}
	err = client.GrantLicence(context.Background(), fakeOrgID, fakeCloudID, account.AtlassianID, atlassian.ProductJira)
	if !errors.Is(err, atlassian.ErrAccountNotReady) {
		t.Fatalf("err = %v, want ErrAccountNotReady", err)
	}
	// The next attempt lands, and the licence is real.
	if err := client.GrantLicence(context.Background(), fakeOrgID, fakeCloudID, account.AtlassianID, atlassian.ProductJira); err != nil {
		t.Fatalf("second grant: %v", err)
	}
	if !fake.byDisplayName("Crewlet Agent SWE").licensed[atlassian.ProductJira] {
		t.Fatal("the licence did not land")
	}
}

func TestALicenceIsGrantedWithTheExactARIs(t *testing.T) {
	t.Parallel()
	// The fake matches the ARI exactly, because the real endpoint accepts
	// the plain `jira` ARI and grants nothing — a run that reports success
	// and agents that can do nothing, with no error anywhere.
	fake := newFakeOrg(t)
	account := fake.seed("Crewlet Agent SWE")
	for _, product := range atlassian.Products {
		if err := fake.admin(t).GrantLicence(context.Background(), fakeOrgID,
			fakeCloudID, account.AtlassianID, product); err != nil {
			t.Fatalf("%s: %v", product, err)
		}
		if !account.licensed[product] {
			t.Fatalf("%s: the licence did not land, so the ARI was wrong", product)
		}
	}
}

func TestASiteIsDiscoveredAndAnAmbiguousOrganizationIsRefused(t *testing.T) {
	t.Parallel()
	fake := newFakeOrg(t)
	site, err := fake.admin(t).DiscoverSite(context.Background(), fakeOrgID)
	if err != nil {
		t.Fatal(err)
	}
	if site.CloudID != fakeCloudID || site.URL != "https://acme.atlassian.net" {
		t.Fatalf("site = %+v", site)
	}

	// ONE SITE RUNNING BOTH PRODUCTS IS ONE SITE. It is the ordinary
	// arrangement, and counting it twice would refuse it as ambiguous.
	fake.sites = []map[string]any{
		{"id": "ari:cloud:jira::site/" + fakeCloudID, "attributes": map[string]any{
			"typeKey": "jira", "status": "online", "hostUrl": "https://acme.atlassian.net"}},
		{"id": "ari:cloud:confluence::site/" + fakeCloudID, "attributes": map[string]any{
			"typeKey": "confluence", "status": "online", "hostUrl": "https://acme.atlassian.net"}},
	}
	if _, err := fake.admin(t).DiscoverSite(context.Background(), fakeOrgID); err != nil {
		t.Fatalf("one site running two products was refused: %v", err)
	}

	// TWO sites is refused rather than guessed: picking would silently
	// point every agent at a place the operator did not choose, and the
	// symptom is an agent that authenticates perfectly into an empty
	// instance.
	fake.sites = append(fake.sites, map[string]any{
		"id": "ari:cloud:jira::site/cloud-2", "attributes": map[string]any{
			"typeKey": "jira", "status": "online", "hostUrl": "https://other.atlassian.net"}})
	if _, err := fake.admin(t).DiscoverSite(context.Background(), fakeOrgID); !errors.Is(err, atlassian.ErrManySites) {
		t.Fatalf("err = %v, want ErrManySites", err)
	}

	// An organization with nothing online has nothing to provision into.
	fake.sites = []map[string]any{}
	if _, err := fake.admin(t).DiscoverSite(context.Background(), fakeOrgID); !errors.Is(err, atlassian.ErrNoSite) {
		t.Fatalf("err = %v, want ErrNoSite", err)
	}
}

func TestAnAccountCreatedWithoutAnAtlassianIDIsRefused(t *testing.T) {
	t.Parallel()
	// Everything downstream keys on that field — the token routes, the
	// licence grant, the account id a webhook names the agent by — so an
	// account without one is unusable, and discovering it at the mint
	// would report the wrong failure.
	fake := newFakeOrg(t)
	client := fake.admin(t)
	account, err := client.CreateServiceAccount(context.Background(), fakeOrgID, "Crewlet Agent", "why")
	if err != nil || account.AtlassianID == "" {
		t.Fatalf("account = %+v, err = %v", account, err)
	}
}

func TestTheDefaultTokenLifetimeStaysUnderAtlassiansCap(t *testing.T) {
	t.Parallel()
	// Atlassian will not mint a credential that outlives 365 days, and
	// nothing in Crewlet renews one on a schedule — so the default is
	// deliberately short of the cap rather than at it.
	if atlassian.DefaultTokenExpiryDays >= atlassian.MaxTokenExpiryDays {
		t.Fatalf("default %d is not under the cap %d",
			atlassian.DefaultTokenExpiryDays, atlassian.MaxTokenExpiryDays)
	}
	if atlassian.MaxTokenExpiryDays != 365 {
		t.Fatalf("cap = %d, want Atlassian's own 365", atlassian.MaxTokenExpiryDays)
	}
}

func TestAReadThatAnswers404AlsoNamesTheUnscopedKey(t *testing.T) {
	t.Parallel()
	// The collection exists for any credential that may SEE it, so a 404
	// on a read means the key cannot reach the service rather than that
	// the organization holds no accounts. Read the other way, an operator
	// with a scoped key is told their organization is empty and goes on to
	// create a duplicate identity for every seat they already have.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	client, err := atlassian.NewAdminClient(atlassian.AdminOptions{
		BaseURL: srv.URL, Key: "scoped-key", HTTP: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ServiceAccounts(context.Background(), fakeOrgID); !errors.Is(err, atlassian.ErrUnscopedKeyRequired) {
		t.Fatalf("err = %v, want ErrUnscopedKeyRequired", err)
	}
}

func TestASiteThatIsNotOnlineIsNotOfferedAsTheCompanysOwn(t *testing.T) {
	t.Parallel()
	// A sandbox being provisioned, a site mid-migration, one that has been
	// deactivated: pointing every agent at it produces exactly the symptom
	// this discovery exists to prevent — a seat that authenticates
	// perfectly into an instance with nothing in it.
	fake := newFakeOrg(t)
	fake.sites = []map[string]any{
		{
			"id": "ari:cloud:jira::site/" + fakeCloudID,
			"attributes": map[string]any{
				"name": "acme", "typeKey": "jira", "status": "online",
				"hostUrl": "https://acme.atlassian.net",
			},
		},
		{
			"id": "ari:cloud:jira::site/cloud-2",
			"attributes": map[string]any{
				"name": "acme-sandbox", "typeKey": "jira", "status": "provisioning",
				"hostUrl": "https://acme-sandbox.atlassian.net",
			},
		},
	}
	site, err := fake.admin(t).DiscoverSite(context.Background(), fakeOrgID)
	if err != nil {
		// Counting the offline one would make this organization ambiguous
		// and refuse a company that has exactly one usable site.
		t.Fatalf("a site that is not online made the organization ambiguous: %v", err)
	}
	if site.CloudID != fakeCloudID {
		t.Fatalf("site = %+v, want the online one", site)
	}
}

func TestACreateWhoseAnswerIsLostNamesTheAccountItMayHaveMade(t *testing.T) {
	t.Parallel()
	// The one failure a caller cannot tell apart from a request that never
	// arrived. If it DID land, the account exists, this process holds no id
	// for it, and no rollback can reach it — a later run then matches it by
	// display name and adopts an identity with no credential. Naming it is
	// all that can be done, and it turns an invisible orphan into one
	// search in admin.atlassian.com.
	fake := newFakeOrg(t)
	fake.set(func(f *fakeOrg) { f.createDropsResponse = true })

	_, err := fake.admin(t).CreateServiceAccount(context.Background(), fakeOrgID,
		"Crewlet Agent SWE", "why")
	if err == nil {
		t.Fatal("a dropped create answered as a success")
	}
	if !strings.Contains(err.Error(), "Crewlet Agent SWE") {
		t.Fatalf("the error does not name what to go looking for: %v", err)
	}
	// A REFUSAL IS NOT DECORATED: Atlassian answered, so nothing was
	// created and there is nothing to search for.
	fake.set(func(f *fakeOrg) { f.createDropsResponse, f.quotaFull = false, true })
	_, err = fake.admin(t).CreateServiceAccount(context.Background(), fakeOrgID,
		"Crewlet Agent QA", "why")
	if !errors.Is(err, atlassian.ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
	if strings.Contains(err.Error(), "now exists") {
		t.Errorf("a refusal was reported as a possible orphan: %v", err)
	}
}
