package derive_test

import (
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
// servers, the models and the sandbox, each confirmed by the settings.
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
	if accounts != 15 {
		t.Errorf("%d accounts, want one for each of the 13 integrations that name one (Jira and Confluence "+
			"share a site), the sandbox's and Gitlet's", accounts)
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
	find(t, v, atlas.KindAccount, "e2b")
	find(t, v, atlas.KindToolServer, "mcp/tickets")
	aws := find(t, v, atlas.KindAccount, "aws/123456789012")
	if got := factOf(aws, atlas.FactRegion); !slices.Equal(got, []string{"eu-west-1", "us-east-2"}) {
		t.Errorf("the AWS account's regions: %v", got)
	}
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
			"internal/queue", "internal/code", "net/http"} {
			if strings.Contains(imp, banned) {
				t.Errorf("internal/atlas/derive imports %s", imp)
			}
		}
	}
}
