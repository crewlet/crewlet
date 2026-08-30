package atlassian_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
)

// agentClient is a reader for one product, as one seeded account.
func agentClient(t *testing.T, fake *fakeOrg, account *fakeAccount, product atlassian.Product) *atlassian.ProductClient {
	t.Helper()
	value := fake.mint(account, atlassian.TokenLabel("swe")+"-1")
	client, err := atlassian.NewProductClient(fake.srv.URL, product, fakeCloudID,
		atlassian.Credential{Token: value, Email: account.Email}, fake.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestASpacePermissionGrantedToAGroupIsTheAgentsToo(t *testing.T) {
	t.Parallel()
	// THE BUG A NAIVE REIMPLEMENTATION SHIPS. Confluence has no
	// mypermissions, so the space's own grant list is read and filtered to
	// this agent — and almost every real grant is made to a GROUP rather
	// than to an account. Matching only the account id reports an agent
	// that works perfectly as having no access at all, and the operator's
	// next move is to grant permissions it already had.
	fake := newFakeOrg(t)
	account := fake.seed("Crewlet Tech Writer", atlassian.ProductConfluence)
	fake.spaceGroups["ENG"] = []string{"confluence-users"}
	fake.groupsOf[account.AtlassianID] = []string{"confluence-users"}

	client := agentClient(t, fake, account, atlassian.ProductConfluence)
	held, err := client.PermissionsIn(context.Background(), "ENG", account.AtlassianID)
	if err != nil {
		t.Fatal(err)
	}
	missing, excess := atlassian.Classify(atlassian.ProductConfluence, held)
	if len(missing) > 0 || len(excess) > 0 {
		t.Fatalf("a group-granted space reported missing=%v excess=%v", missing, excess)
	}
}

func TestASpacePermissionGrantedToSomebodyElsesGroupIsNotTheAgents(t *testing.T) {
	t.Parallel()
	// The other half of the same filter. The space lists EVERY principal's
	// grants, so a colleague's permission must not be credited to this
	// agent — that would report an agent that can do nothing as fully
	// provisioned, which is the reassuring answer and the wrong one.
	fake := newFakeOrg(t)
	account := fake.seed("Crewlet Tech Writer", atlassian.ProductConfluence)
	fake.spaceGroups["ENG"] = []string{"some-other-team"}
	fake.groupsOf[account.AtlassianID] = []string{"confluence-users"}

	client := agentClient(t, fake, account, atlassian.ProductConfluence)
	held, err := client.PermissionsIn(context.Background(), "ENG", account.AtlassianID)
	if err != nil {
		t.Fatal(err)
	}
	missing, _ := atlassian.Classify(atlassian.ProductConfluence, held)
	if !slices.Equal(missing, atlassian.Allowed(atlassian.ProductConfluence)) {
		t.Fatalf("missing = %v, want every contract permission", missing)
	}
}

func TestASpaceGroupWalkNeedsTheAccountItIsAbout(t *testing.T) {
	t.Parallel()
	// The memberof endpoint answers 400 without an account id and rejects
	// the older username and userkey spellings outright, so a caller with
	// no account id would fail the whole check rather than the group half
	// — and the failure would name the space rather than the omission.
	fake := newFakeOrg(t)
	account := fake.seed("Crewlet Tech Writer", atlassian.ProductConfluence)
	client := agentClient(t, fake, account, atlassian.ProductConfluence)

	_, err := client.PermissionsIn(context.Background(), "ENG", "")
	if err == nil {
		t.Fatal("a space check with no account id was accepted")
	}
}

func TestAJiraCheckAsksForBothHalvesOfTheContract(t *testing.T) {
	t.Parallel()
	fake := newFakeOrg(t)
	account := fake.seed("Crewlet Agent SWE", atlassian.ProductJira)
	held := map[string]bool{}
	for _, name := range atlassian.Allowed(atlassian.ProductJira) {
		held[name] = true
	}
	held["ASSIGNABLE_USER"] = false
	held["ADMINISTER_PROJECTS"] = true
	fake.grants(account, atlassian.ProductJira, "ENG", held)

	client := agentClient(t, fake, account, atlassian.ProductJira)
	got, err := client.PermissionsIn(context.Background(), "ENG", account.AtlassianID)
	if err != nil {
		t.Fatal(err)
	}
	missing, excess := atlassian.Classify(atlassian.ProductJira, got)
	if !slices.Equal(missing, []string{"ASSIGNABLE_USER"}) {
		t.Errorf("missing = %v", missing)
	}
	// EXCESS IS ONLY FOUND BECAUSE IT WAS ASKED FOR. A query that carried
	// the allowed half alone would report this container clean, and an
	// agent that can administer the project would look correctly scoped.
	if !slices.Equal(excess, []string{"ADMINISTER_PROJECTS"}) {
		t.Errorf("excess = %v", excess)
	}
}

func TestTheIdentityCheckIsAskedPerProduct(t *testing.T) {
	t.Parallel()
	// On Cloud both products answer the same account id. It is asked of
	// both anyway, because a credential minted with Jira scopes only is
	// REFUSED by Confluence — and that refusal is the one signal that a
	// seat's products have grown since its credential was minted. Token
	// scopes cannot be read back from Atlassian at all.
	fake := newFakeOrg(t)
	fake.unlicensedIsRefused = true
	account := fake.seed("Crewlet Agent SWE", atlassian.ProductJira)
	value := fake.mint(account, atlassian.TokenLabel("swe")+"-1")
	cred := atlassian.Credential{Token: value, Email: account.Email}

	jira, err := atlassian.NewProductClient(fake.srv.URL, atlassian.ProductJira, fakeCloudID, cred, fake.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if who, err := jira.Me(context.Background()); err != nil || who != account.AtlassianID {
		t.Fatalf("jira identity = %q, err = %v", who, err)
	}

	wiki, err := atlassian.NewProductClient(fake.srv.URL, atlassian.ProductConfluence, fakeCloudID, cred, fake.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wiki.Me(context.Background()); !errors.Is(err, atlassian.ErrCredentialRefused) {
		t.Fatalf("confluence identity err = %v, want ErrCredentialRefused", err)
	}
}

func TestAContainerNobodyCanSeeIsNamedAsSuch(t *testing.T) {
	t.Parallel()
	// Both products hide what a caller may not read, so a container that
	// does not exist and one the agent has no access to answer alike —
	// and they are the same problem to an operator anyway.
	fake := newFakeOrg(t)
	account := fake.seed("Crewlet Agent SWE", atlassian.ProductJira)
	client, err := atlassian.NewProductClient(fake.srv.URL, atlassian.ProductJira, "wrong-cloud",
		atlassian.Credential{Token: fake.mint(account, "x"), Email: account.Email}, fake.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Me(context.Background()); !errors.Is(err, atlassian.ErrContainerNotVisible) {
		t.Fatalf("err = %v, want ErrContainerNotVisible", err)
	}
}

func TestTheSettingsLinkNamesTheScreenAndItsStyle(t *testing.T) {
	t.Parallel()
	// The route differs per project type and a wrong guess lands somebody
	// on an error page having been told the link was the fix. The fake
	// answers `next-gen`, which is what Atlassian still calls a
	// team-managed project on the wire years after renaming it everywhere
	// a person can see.
	fake := newFakeOrg(t)
	account := fake.seed("Crewlet Agent SWE", atlassian.ProductJira)
	jira := agentClient(t, fake, account, atlassian.ProductJira)

	got, err := jira.SettingsFor(context.Background(), "https://acme.atlassian.net/", "ENG")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://acme.atlassian.net/jira/software/projects/ENG/settings/access" {
		t.Errorf("jira settings URL = %q", got.URL)
	}
	if got.Style != atlassian.StyleTeamManaged {
		t.Errorf("style = %q, want team-managed for a next-gen project", got.Style)
	}

	wiki := agentClient(t, fake, account, atlassian.ProductConfluence)
	got, err = wiki.SettingsFor(context.Background(), "https://acme.atlassian.net", "ENG")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://acme.atlassian.net/wiki/spaces/ENG/settings/permissions" ||
		got.Style != atlassian.StyleSpace {
		t.Errorf("confluence settings = %+v", got)
	}

	// NO SITE URL, NO LINK. The API gateway is not a place a browser can
	// go, so a link built from it looks right and opens nothing — the
	// report names the container instead.
	got, err = wiki.SettingsFor(context.Background(), "", "ENG")
	if err != nil || got.URL != "" {
		t.Errorf("settings with no site url = %+v, err = %v", got, err)
	}
}

func TestAProductClientRefusesAnAddressItCannotBuild(t *testing.T) {
	t.Parallel()
	cred := atlassian.Credential{Token: "t", Email: "e@example.com"}
	if _, err := atlassian.NewProductClient("https://x", atlassian.Product("bitbucket"), "c", cred, nil); err == nil {
		t.Error("an unserved product was accepted")
	}
	if _, err := atlassian.NewProductClient("https://x", atlassian.ProductJira, " ", cred, nil); err == nil {
		t.Error("a client with no cloud id was accepted")
	}
}
