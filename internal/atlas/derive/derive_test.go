package derive_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/build"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/config"
)

var now = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// acmeGitlet is what Gitlet lists to a reader of acme's repositories: api, a
// mirror the settings declare; tools, one a seat made, read in detail; and
// the checks api's default branch runs.
func acmeGitlet() ([]derive.GitletRepo, []derive.GitletCheck) {
	return []derive.GitletRepo{
		{Name: "api", Status: "active", DefaultBranch: "main", Visibility: "company", OwnerKind: "unit", Owner: "platform"},
		{Name: "tools", Status: "active", DefaultBranch: "main", Visibility: "company", OwnerKind: "seat", Owner: "sre",
			ImportedFrom: "https://gitlab.acme.example/platform/tools.git"},
		{Name: "old", Status: "deleted", DefaultBranch: "main", Visibility: "company"},
	}, []derive.GitletCheck{
		{Repo: "api", Name: "test", State: "passed", Required: true},
		{Repo: "api", Name: "deploy", State: "failed", External: true},
	}
}

func read(c *config.Company) atlas.View {
	s := derive.FromSettings(c, "r1", derive.AcmeDeployment)
	repos, checks := acmeGitlet()
	claims := append(s.Claims(now).Claims, s.Gitlet(repos, checks, now).Claims...)
	return atlas.Fold(claims, now)
}

func find(t *testing.T, v atlas.View, k atlas.Kind, key string) atlas.Entry {
	t.Helper()
	e, ok := v.Find(atlas.MustRef(k, key).ID())
	if !ok {
		t.Fatalf("%s %s is not in the atlas", k, key)
	}
	return e
}

func factOf(e atlas.Entry, f atlas.Fact) []string {
	var out []string
	for _, v := range e.Facts {
		if v.Fact == f {
			out = append(out, v.Value.Text)
		}
	}
	return out
}

func related(v atlas.View, e atlas.Entry, r atlas.Relation) []string {
	var out []string
	for _, ed := range e.Relations {
		if ed.Relation != r {
			continue
		}
		if ed.External != "" {
			out = append(out, string(ed.External)+":"+ed.Name)
			continue
		}
		if to, ok := v.Find(ed.To); ok {
			out = append(out, to.Ref.Key)
		}
	}
	return out
}

// NO SECRET REACHES THE ATLAS: a canary in every secret position of the
// settings appears in no claim, no withheld note and no folded entry.
func TestNoSecretReachesTheAtlas(t *testing.T) {
	t.Parallel()
	c := derive.AcmeCompany()
	derive.PlantSecrets(c)
	s := derive.FromSettings(c, "r1", derive.AcmeDeployment)
	repos, checks := acmeGitlet()
	claims, gitlet := s.Claims(now), s.Gitlet(repos, checks, now)
	all := append(slices.Clone(claims.Claims), gitlet.Claims...)
	if len(claims.Claims) < 100 {
		t.Fatalf("the planted settings made %d claims: the test reads too little to prove anything", len(claims.Claims))
	}
	for name, v := range map[string]any{"settings": claims, "gitlet": gitlet, "view": atlas.Fold(all, now)} {
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if i := strings.Index(string(out), derive.Canary); i >= 0 {
			t.Errorf("a secret reached the atlas through the %s: %s", name, out[i:min(len(out), i+100)])
		}
	}
}

// THE SETTINGS DECLARE WHAT THE COMPANY RUNS ON: the account of every
// integration, the zones and the host names in them, the servers, the tool
// servers and the models, each confirmed by the settings. The sandbox it
// declares is no account: its boxes run on the engine's own nodes.
func TestTheSettingsDeclareWhatTheCompanyRunsOn(t *testing.T) {
	t.Parallel()
	v := read(derive.AcmeCompany())
	accounts := 0
	for _, e := range v.Entries {
		if e.Ref.Kind == atlas.KindAccount {
			accounts++
		}
		if e.State != atlas.StateConfirmed || e.Word != atlas.WordConfirmed {
			t.Errorf("%s is %s, %s: what the settings declare is confirmed", e.Ref, e.State, e.Word)
		}
	}
	if accounts != 14 {
		t.Errorf("%d accounts, want one for each of the 13 integrations that name one (Jira and Confluence "+
			"share a site) and Gitlet's, and none for the sandbox", accounts)
	}
	cf := find(t, v, atlas.KindAccount, "cloudflare/0123456789abcdef0123456789abcdef")
	if got := factOf(cf, atlas.FactCapability); !slices.Equal(got, []string{"cdn", "dns"}) {
		t.Errorf("Cloudflare's capabilities: %v", got)
	}
	zone := find(t, v, atlas.KindDomain, "acme-status.example")
	if got := related(v, zone, atlas.RelationServedBy); !slices.Equal(got, []string{cf.Ref.Key}) {
		t.Errorf("acme-status.example is served by %v", got)
	}
	host := find(t, v, atlas.KindHostname, "crewlet.acme.example")
	if got := related(v, host, atlas.RelationInZone); !slices.Equal(got, []string{"acme.example"}) {
		t.Errorf("the deployment's host name sits in %v", got)
	}
	site := find(t, v, atlas.KindAccount, "atlassian/acme.atlassian.net")
	if got := factOf(site, atlas.FactCapability); !slices.Equal(got, []string{"knowledge", "tracker"}) {
		t.Errorf("Jira and Confluence on one site: %v, members %v", got, site.Members)
	}
	db := find(t, v, atlas.KindHost, "ssh/db-1")
	if got := db.Facts; len(factOf(db, atlas.FactAddress)) != 1 || !got[slices.IndexFunc(got, func(f atlas.FactView) bool {
		return f.Fact == atlas.FactAddress
	})].Value.Private {
		t.Error("db-1's address is not marked internal")
	}
	main := find(t, v, atlas.KindModel, "llm/main")
	for f, want := range map[atlas.Fact]string{atlas.FactVendor: "anthropic", atlas.FactChainPosition: "1",
		atlas.FactSignIn: "subscription", atlas.FactModel: "claude-opus-5-5"} {
		if got := factOf(main, f); !slices.Equal(got, []string{want}) {
			t.Errorf("the main model's %s is %v, want %s", f, got, want)
		}
	}
	find(t, v, atlas.KindToolServer, "mcp/tickets")
	aws := find(t, v, atlas.KindAccount, "aws/123456789012")
	if got := factOf(aws, atlas.FactRegion); !slices.Equal(got, []string{"eu-west-1", "us-east-2"}) {
		t.Errorf("the AWS account's regions: %v", got)
	}
}

// A REPOSITORY THE SETTINGS NAME HAS THE KEY ITS FORGE'S ADDRESS GIVES IT, on
// a forge served under a path of its host as well, so the settings, the
// forge's own read and a mirror's address name one entry.
//
// Mutation: key it by the forge's host and the path alone, and an instance
// under /gitlab shows each named project twice once GitLab is read.
func TestANamedRepositoryIsKeyedByItsForgesAddress(t *testing.T) {
	t.Parallel()
	c := derive.AcmeCompany()
	c.Integrations.GitLab.URL = "https://git.acme.example/gitlab/"
	c.Integrations.GitLab.Provisioning.Group = "Acme"
	c.Integrations.GitLab.Provisioning.Projects = []string{"API", "partners/Shared"}
	v := read(c)
	account := find(t, v, atlas.KindAccount, "gitlab/git.acme.example/acme")
	for _, key := range []string{"git.acme.example/gitlab/acme/api", "git.acme.example/gitlab/partners/shared"} {
		if got := related(v, find(t, v, atlas.KindRepository, key), atlas.RelationBelongsTo); !slices.Equal(got, []string{account.Ref.Key}) {
			t.Errorf("%s belongs to %v", key, got)
		}
	}
}

// A DIGITALOCEAN TEAM IS KEYED BY ITS NAME'S SLUG: a team's name may hold a
// space, which an account's key may not, and the read of the team keys it
// the same way, so the two name one entry.
//
// Mutation: key the account by the name lowercased, and a team named with a
// space is withheld, so the company's DigitalOcean account is no entry.
func TestADigitalOceanTeamWithASpaceIsAnAccount(t *testing.T) {
	t.Parallel()
	c := derive.AcmeCompany()
	c.Integrations.DigitalOcean.Provisioning.Team = "Acme Labs"
	v := read(c)
	account := find(t, v, atlas.KindAccount, "digitalocean/acme-labs")
	if account.Name != "DigitalOcean" {
		t.Errorf("the team's account is named %q", account.Name)
	}
}

// A GOOGLE ANALYTICS PROPERTY IS AN ACCOUNT, keyed by its ID, with the
// analytics capability; a company that names none holds the vendor alone.
func TestAGoogleAnalyticsPropertyIsAnAccount(t *testing.T) {
	t.Parallel()
	c := derive.AcmeCompany()
	c.Integrations.GoogleAnalytics = &config.GoogleAnalytics{Enabled: true, Properties: []string{"123456789"}}
	account := find(t, read(c), atlas.KindAccount, "google/analytics/123456789")
	if account.Name != "Google Analytics" || !slices.Contains(factOf(account, atlas.FactCapability), "analytics") {
		t.Errorf("the property's account is %+v", account)
	}
	c.Integrations.GoogleAnalytics.Properties = nil
	find(t, read(c), atlas.KindAccount, "google/analytics")
}

// AN ADMOB PUBLISHER ACCOUNT IS AN ACCOUNT, keyed by its ID, with the
// monetization capability; a company that names none holds the vendor alone.
func TestAnAdMobPublisherAccountIsAnAccount(t *testing.T) {
	t.Parallel()
	c := derive.AcmeCompany()
	c.Integrations.AdMob = &config.AdMob{Enabled: true, Accounts: []string{"pub-1234567890123456"}}
	account := find(t, read(c), atlas.KindAccount, "google/admob/pub-1234567890123456")
	if account.Name != "AdMob" || !slices.Contains(factOf(account, atlas.FactCapability), "monetization") {
		t.Errorf("the publisher account is %+v", account)
	}
	c.Integrations.AdMob.Accounts = nil
	find(t, read(c), atlas.KindAccount, "google/admob")
}

// AN ADSENSE PUBLISHER ACCOUNT IS AN ACCOUNT, keyed by its ID, with the
// monetization capability; a company that names none holds the vendor alone.
func TestAnAdSensePublisherAccountIsAnAccount(t *testing.T) {
	t.Parallel()
	c := derive.AcmeCompany()
	c.Integrations.AdSense = &config.AdSense{Enabled: true, Accounts: []string{"pub-1234567890123456"}}
	account := find(t, read(c), atlas.KindAccount, "google/adsense/pub-1234567890123456")
	if account.Name != "AdSense" || !slices.Contains(factOf(account, atlas.FactCapability), "monetization") {
		t.Errorf("the publisher account is %+v", account)
	}
	c.Integrations.AdSense.Accounts = nil
	find(t, read(c), atlas.KindAccount, "google/adsense")
}

// A GOOGLE ADS ACCOUNT IS AN ACCOUNT, keyed by its customer ID, with the
// advertising capability; a company that names none holds the vendor alone.
func TestAGoogleAdsAccountIsAnAccount(t *testing.T) {
	t.Parallel()
	c := derive.AcmeCompany()
	c.Integrations.GoogleAds = &config.GoogleAds{Enabled: true, Customers: []config.GoogleAdsCustomer{{ID: "1234567890"}}}
	account := find(t, read(c), atlas.KindAccount, "google/ads/1234567890")
	if account.Name != "Google Ads" || !slices.Contains(factOf(account, atlas.FactCapability), "advertising") {
		t.Errorf("the customer's account is %+v", account)
	}
	c.Integrations.GoogleAds.Customers = nil
	find(t, read(c), atlas.KindAccount, "google/ads")
}

// A SEARCH CONSOLE SITE IS AN ACCOUNT, with the analytics capability: a
// domain property keyed as Search Console writes it, and each URL prefix its
// own account by its host and a digest of the site, which it carries as its
// site, so three properties on one host are three accounts whose references
// join no other Google account the host names; nothing is withheld, and a
// company that names no site holds the vendor alone.
//
// Mutation: key a URL prefix by its host alone, and the three properties are
// one account whose reference is the bare host.
func TestASearchConsoleSiteIsAnAccount(t *testing.T) {
	t.Parallel()
	c := derive.AcmeCompany()
	prefixes := []string{"https://www.example.com/", "https://www.example.com/blog/", "http://www.example.com/"}
	c.Integrations.SearchConsole = &config.SearchConsole{Enabled: true,
		Sites: append([]string{"sc-domain:example.com"}, prefixes...)}
	d := derive.FromSettings(c, "r1", derive.AcmeDeployment).Claims(now)
	for _, w := range d.Withheld {
		if strings.HasPrefix(w.Where, "integrations.search_console") {
			t.Errorf("%s was withheld: %s", w.Where, w.Reason)
		}
	}
	v := read(c)
	domain := find(t, v, atlas.KindAccount, "google/search-console/sc-domain:example.com")
	if !slices.Equal(factOf(domain, atlas.FactAccountRef), []string{"sc-domain:example.com"}) ||
		!slices.Contains(factOf(domain, atlas.FactCapability), "analytics") {
		t.Errorf("the domain property is %+v", domain)
	}
	refs := map[string]bool{}
	for _, site := range prefixes {
		sum := sha256.Sum256([]byte(site))
		ref := "www.example.com/" + hex.EncodeToString(sum[:6])
		account := find(t, v, atlas.KindAccount, "google/search-console/"+ref)
		said, err := atlas.Parse(atlas.TypeURL, site, nil)
		if err != nil {
			t.Fatal(err)
		}
		if account.Name != "Search Console" || !slices.Equal(factOf(account, atlas.FactSite), []string{said.Text}) ||
			!slices.Equal(factOf(account, atlas.FactAccountRef), []string{ref}) {
			t.Errorf("%s is %+v", site, account)
		}
		refs[ref] = true
	}
	if len(refs) != len(prefixes) {
		t.Errorf("three properties on one host are %d accounts", len(refs))
	}
	c.Integrations.SearchConsole.Sites = nil
	find(t, read(c), atlas.KindAccount, "google/search-console")
}

// A GOOGLE PLAY APP IS AN ACCOUNT, keyed by its package name, with the
// app_delivery capability; a company that names none holds the vendor alone.
func TestAGooglePlayAppIsAnAccount(t *testing.T) {
	t.Parallel()
	c := derive.AcmeCompany()
	c.Integrations.GooglePlay = &config.GooglePlay{Enabled: true, Apps: []string{"com.acme.app"}}
	account := find(t, read(c), atlas.KindAccount, "google/play/com.acme.app")
	if account.Name != "Google Play" || !slices.Contains(factOf(account, atlas.FactCapability), "app_delivery") {
		t.Errorf("the app's account is %+v", account)
	}
	c.Integrations.GooglePlay.Apps = nil
	find(t, read(c), atlas.KindAccount, "google/play")
}

// A TOOL SERVER IS KNOWN BY ITS COMMAND, NEVER ITS ARGUMENTS: an argument is
// where a person passes a token by mistake, and it is no secret position.
func TestAToolServerIsKnownByItsCommandNotItsArguments(t *testing.T) {
	t.Parallel()
	c := derive.AcmeCompany()
	c.MCPServers[1].Args = []string{"--token", "atlas-arg-canary"}
	v := read(c)
	files := find(t, v, atlas.KindToolServer, "mcp/files")
	if got := factOf(files, atlas.FactCommand); !slices.Equal(got, []string{"files-mcp"}) {
		t.Errorf("the files server's command: %v", got)
	}
	if out, _ := json.Marshal(v); strings.Contains(string(out), "atlas-arg-canary") {
		t.Error("a tool server's argument reached the atlas")
	}
}

// A VALUE SHAPED LIKE A CREDENTIAL IN THE SETTINGS IS WITHHELD: named by where
// it sits and why, and never shown.
func TestAValueShapedLikeACredentialIsWithheld(t *testing.T) {
	t.Parallel()
	c := derive.AcmeCompany()
	secret := "gh" + "p_" + strings.Repeat("Ab3dE6gH9j", 3) + "K2mN5p"
	c.Providers.LLM["main"] = config.LLMProvider{Harness: config.HarnessClaudeCode, Model: secret}
	c.MCPServers[0].URL = "https://mcp.tickets.example/mcp?key=" + secret
	d := derive.FromSettings(c, "r1", derive.AcmeDeployment).Claims(now)
	var where []string
	for _, w := range d.Withheld {
		where = append(where, w.Where)
	}
	for _, want := range []string{"providers.llm.main.model", "mcp_servers[0].url"} {
		if !slices.Contains(where, want) {
			t.Errorf("%s was not withheld: %v", want, where)
		}
	}
	if out, _ := json.Marshal(d); strings.Contains(string(out), secret) {
		t.Error("the refused value reached the claims or the note withholding it")
	}
}

// A REFERENCE IS NEVER RESOLVED: the atlas reads the settings as written, and
// says which values it could not read because they are references.
func TestAReferenceIsNeverResolved(t *testing.T) {
	t.Parallel()
	c := derive.AcmeCompany()
	c.Integrations.GitLab.URL = "${GITLAB_URL}"
	d := derive.FromSettings(c, "r1", derive.AcmeDeployment).Claims(now)
	if !slices.ContainsFunc(d.Withheld, func(w derive.Withheld) bool {
		return w.Where == "integrations.gitlab" && strings.Contains(w.Reason, "reference")
	}) {
		t.Errorf("a reference was not named as one: %+v", d.Withheld)
	}
	if out, _ := json.Marshal(d.Claims); strings.Contains(string(out), "GITLAB_URL") {
		t.Error("a reference reached a claim")
	}
}

// GITLET LISTS ONLY WHAT ITS READ LISTED: a repository the settings declare but
// the reader may not read (its unit's alone) is not in the atlas, nor is its
// push mirror, and a deleted one is gone.
func TestGitletListsOnlyWhatTheReaderMayRead(t *testing.T) {
	t.Parallel()
	v := read(derive.AcmeCompany())
	for _, key := range []string{"gitlet/payroll", "gitlet/old", "github.com/acme/payroll"} {
		if _, ok := v.Find(atlas.MustRef(atlas.KindRepository, key).ID()); ok {
			t.Errorf("%s is in the atlas", key)
		}
	}
	tools := find(t, v, atlas.KindRepository, "gitlet/tools")
	if got := related(v, tools, atlas.RelationOwnedBy); !slices.Equal(got, []string{"seat:sre"}) {
		t.Errorf("tools is owned by %v", got)
	}
	deploy := find(t, v, atlas.KindPipeline, "gitlet/api#deploy")
	for f, want := range map[atlas.Fact]string{atlas.FactLastResult: "failed", atlas.FactRunner: "external",
		atlas.FactRequired: "no"} {
		if got := factOf(deploy, f); !slices.Equal(got, []string{want}) {
			t.Errorf("the deploy pipeline's %s is %v, want %s", f, got, want)
		}
	}
}

// A GITLET MIRROR IS THE REPOSITORY IT MIRRORS: the settings name api's source
// on GitHub, which the GitHub settings name too, and the atlas shows one entry.
func TestAGitletMirrorIsTheRepositoryItMirrors(t *testing.T) {
	t.Parallel()
	v := read(derive.AcmeCompany())
	api, gh := find(t, v, atlas.KindRepository, "gitlet/api"), find(t, v, atlas.KindRepository, "github.com/acme/api")
	if api.ID != gh.ID || len(api.Members) != 2 {
		t.Errorf("the mirror and its source are %s and %s", api.ID, gh.ID)
	}
	if got := related(v, api, atlas.RelationOwnedBy); !slices.Equal(got, []string{"unit:platform"}) {
		t.Errorf("the merged repository is owned by %v", got)
	}
}

// DERIVE IMPORTS NO WRITE PATH: it is pure functions over what it is handed.
func TestDeriveImportsNoWritePath(t *testing.T) {
	t.Parallel()
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		for _, banned := range []string{"internal/api", "internal/store", "internal/statelog", "internal/coord",
			"internal/queue", "internal/gitlet", "internal/codesearch", "net/http"} {
			if strings.Contains(imp, banned) {
				t.Errorf("internal/atlas/derive imports %s", imp)
			}
		}
	}
}
