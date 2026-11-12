package github_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// atlasNow is the instant every atlas read here is made at.
var atlasNow = time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)

// atlasKey is one app key every seat here signs with: a real one, since the
// assertion is really signed, made once because making one is slow.
var atlasKey = sync.OnceValue(func() string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
})

// Values the fake carries in fields the read never decodes.
const (
	canaryPerson  = "atlas-canary-person"
	canaryEmail   = "atlas-canary-person@acme.example"
	canaryPayload = "atlas-canary-deployment-payload"
	canaryText    = "atlas-canary-description"
)

// ghInstall is one agent's installation at the fake.
type ghInstall struct {
	login     string
	perms     map[string]string
	suspended bool
	repos     []string
}

// ghRepo is one repository at the fake.
type ghRepo struct {
	row         map[string]any
	workflows   []map[string]any
	runs        []map[string]any
	deployments []map[string]any
	statuses    map[int64][]map[string]any
}

// forge is a GitHub organization as the atlas reads it. A path named in
// refuse answers that status, the path in limit answers GitHub's rate limit,
// and a token is served only what it was minted to read.
type forge struct {
	installs map[int64]*ghInstall
	repos    map[string]*ghRepo
	refuse   map[string]int
	limit    string

	mu      sync.Mutex
	calls   []string
	minted  map[string]map[string]string
	revoked []string
}

var (
	installRoute   = regexp.MustCompile(`^/app/installations/(\d+)$`)
	tokenRoute     = regexp.MustCompile(`^/app/installations/(\d+)/access_tokens$`)
	workflowsRoute = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/actions/workflows$`)
	runsRoute      = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/actions/runs$`)
	deploysRoute   = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/deployments$`)
	statusesRoute  = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/deployments/(\d+)/statuses$`)
)

func (f *forge) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+path)
	w.Header().Set("Content-Type", "application/json")
	answer := func(status int, body any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	if f.limit == path {
		w.Header().Set("X-RateLimit-Remaining", "0")
		answer(http.StatusForbidden, map[string]string{"message": "API rate limit exceeded"})
		return
	}
	if status := f.refuse[path]; status != 0 {
		answer(status, map[string]string{"message": "Resource not accessible by integration"})
		return
	}
	q := r.URL.Query()
	paged := func(rows []map[string]any) []map[string]any {
		size, _ := strconv.Atoi(q.Get("per_page"))
		n, _ := strconv.Atoi(q.Get("page"))
		start := (max(n, 1) - 1) * size
		if start >= len(rows) {
			return []map[string]any{}
		}
		return rows[start:min(len(rows), start+size)]
	}
	// may reports a token minted to read key, and refuses the request
	// otherwise, as GitHub does.
	may := func(key string) bool {
		if level := f.minted[bearer][key]; level == "read" || level == "write" {
			return true
		}
		answer(http.StatusForbidden, map[string]string{"message": "Resource not accessible by integration"})
		return false
	}
	switch {
	case installRoute.MatchString(path):
		id, _ := strconv.ParseInt(installRoute.FindStringSubmatch(path)[1], 10, 64)
		in, ok := f.installs[id]
		if !ok {
			answer(http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		body := map[string]any{"id": id, "permissions": in.perms, "repository_selection": "selected",
			"account": map[string]any{"login": in.login, "type": "Organization", "email": canaryEmail}}
		if in.suspended {
			body["suspended_at"] = "2026-11-01T00:00:00Z"
			body["suspended_by"] = map[string]any{"login": canaryPerson, "type": "User"}
		}
		answer(http.StatusOK, body)
	case tokenRoute.MatchString(path):
		id := tokenRoute.FindStringSubmatch(path)[1]
		var asked struct {
			Permissions map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&asked)
		n, _ := strconv.ParseInt(id, 10, 64)
		for key := range asked.Permissions {
			if key != "metadata" && f.installs[n].perms[key] == "" {
				answer(http.StatusUnprocessableEntity, map[string]string{"message": "not granted"})
				return
			}
		}
		token := "installation-token-" + id
		f.minted[token] = asked.Permissions
		answer(http.StatusCreated, map[string]any{"token": token, "expires_at": "2026-11-12T08:00:00Z",
			"permissions": asked.Permissions})
	case path == "/installation/token" && r.Method == http.MethodDelete:
		f.revoked = append(f.revoked, bearer)
		delete(f.minted, bearer)
		w.WriteHeader(http.StatusNoContent)
	case path == "/installation/repositories":
		if !may("metadata") {
			return
		}
		id, _ := strconv.ParseInt(strings.TrimPrefix(bearer, "installation-token-"), 10, 64)
		var rows []map[string]any
		for _, name := range f.installs[id].repos {
			rows = append(rows, f.repos[name].row)
		}
		answer(http.StatusOK, map[string]any{"total_count": len(rows), "repositories": paged(rows)})
	case workflowsRoute.MatchString(path):
		if !may("actions") {
			return
		}
		rows := f.repos[workflowsRoute.FindStringSubmatch(path)[1]].workflows
		answer(http.StatusOK, map[string]any{"total_count": len(rows), "workflows": paged(rows)})
	case runsRoute.MatchString(path):
		if !may("actions") {
			return
		}
		repo := f.repos[runsRoute.FindStringSubmatch(path)[1]]
		if q.Get("status") != "completed" || q.Get("branch") != repo.row["default_branch"] ||
			q.Get("exclude_pull_requests") != "true" {
			answer(http.StatusBadRequest, map[string]string{"message": "the read asked for the wrong runs"})
			return
		}
		answer(http.StatusOK, map[string]any{"total_count": len(repo.runs), "workflow_runs": paged(repo.runs)})
	case deploysRoute.MatchString(path):
		if !may("deployments") {
			return
		}
		answer(http.StatusOK, paged(f.repos[deploysRoute.FindStringSubmatch(path)[1]].deployments))
	case statusesRoute.MatchString(path):
		if !may("deployments") {
			return
		}
		m := statusesRoute.FindStringSubmatch(path)
		id, _ := strconv.ParseInt(m[2], 10, 64)
		answer(http.StatusOK, paged(f.repos[m[1]].statuses[id]))
	default:
		answer(http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func (f *forge) sent(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

// person is what GitHub nests in almost every row: who did it.
func person() map[string]any {
	return map[string]any{"login": canaryPerson, "email": canaryEmail, "name": canaryPerson}
}

// acme is an organization one agent's installation covers: a repository with
// a failing deploy workflow, a production environment whose newest deployment
// failed and one more environment, a repository with nothing, and an
// archived one.
func acme() *forge {
	return &forge{
		installs: map[int64]*ghInstall{
			7: {login: "Acme", repos: []string{"Acme/API", "acme/web", "acme/old"},
				perms: map[string]string{"metadata": "read", "contents": "write", "actions": "write", "deployments": "read"}},
		},
		repos: map[string]*ghRepo{
			"Acme/API": {
				row: map[string]any{"full_name": "Acme/API", "default_branch": "main", "private": true,
					"visibility": "private", "description": canaryText, "owner": person()},
				workflows: []map[string]any{
					{"id": 2, "name": "[deploy] Production", "path": ".github/workflows/deploy.yml", "state": "active"},
					{"id": 1, "name": "CI", "path": ".github/workflows/ci.yml", "state": "active"},
					{"id": 3, "name": "Nightly", "path": ".github/workflows/nightly.yml", "state": "disabled_inactivity"},
					{"id": 4, "name": "pages-build-deployment", "path": "dynamic/pages/pages-build-deployment", "state": "active"},
					{"id": 5, "name": "Gone", "path": ".github/workflows/gone.yml", "state": "deleted"},
				},
				runs: []map[string]any{
					{"workflow_id": 2, "conclusion": "failure", "actor": person(), "triggering_actor": person(),
						"head_commit": map[string]any{"message": canaryText, "author": person()}},
					{"workflow_id": 1, "conclusion": "success", "actor": person()},
					{"workflow_id": 1, "conclusion": "failure"},
				},
				deployments: []map[string]any{
					{"id": 30, "environment": "Production", "creator": person(), "description": canaryText,
						"payload": map[string]any{"secret": canaryPayload}},
					{"id": 29, "environment": "pr-12", "transient_environment": true},
					{"id": 28, "environment": "Production"},
					{"id": 27, "environment": "eu west/1"},
				},
				statuses: map[int64][]map[string]any{
					30: {{"state": "failure", "creator": person(), "description": canaryText,
						"log_url":         "https://ci.acme.example/logs/" + canaryText,
						"environment_url": "https://deploy:hunter2@api.acme.example/?token=" + canaryPayload + "#frag"},
						{"state": "in_progress"}},
					27: {{"state": "success", "environment_url": "https://203.0.113.9:8443/"}},
				},
			},
			"acme/web": {row: map[string]any{"full_name": "acme/web", "default_branch": "main", "visibility": "public"}},
			// An older Enterprise Server sends no word, only the flag.
			"acme/old": {row: map[string]any{"full_name": "acme/old", "default_branch": "master", "archived": true,
				"private": true}},
		},
		refuse: map[string]int{},
		minted: map[string]map[string]string{},
	}
}

// agent is one agent seat with its own app, installed as installation.
func agent(handle string, appID, installation int64) config.Role {
	on := true
	return config.Role{Name: strings.ToUpper(handle), Handle: handle,
		Integrations: config.RoleIntegrations{GitHub: &config.RoleGitHub{
			Enabled: &on, AppID: appID, AppSlug: "acme-" + handle, InstallationID: installation,
			PrivateKey: "${GITHUB_APP_KEY_" + strings.ToUpper(handle) + "}",
		}}}
}

func (f *forge) input(t *testing.T, roles ...config.Role) collect.Input {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	if len(roles) == 0 {
		roles = []config.Role{agent("sre", 41, 7)}
	}
	company := &config.Company{Name: "Acme", Roles: roles}
	company.Integrations.GitHub = &config.GitHub{Enabled: true,
		Provisioning: &config.GitHubProvisioning{Org: "Acme"}}
	env := config.MapSource{}
	for _, role := range roles {
		env["GITHUB_APP_KEY_"+strings.ToUpper(role.Handle)] = atlasKey()
	}
	return collect.Input{Config: company, Now: atlasNow, Env: config.NewResolver(env),
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, srv)}}
}

func githubSource(t *testing.T) collect.Source {
	t.Helper()
	src, ok := collect.Lookup(integration.KindGitHub)
	if !ok {
		t.Fatal("no GitHub source is registered")
	}
	return src
}

func (f *forge) read(t *testing.T, roles ...config.Role) collect.Read {
	t.Helper()
	read, err := githubSource(t).Collect(t.Context(), f.input(t, roles...))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return read
}

func atlasEntry(t *testing.T, v atlas.View, k atlas.Kind, key string) atlas.Entry {
	t.Helper()
	e, ok := v.Find(atlas.MustRef(k, key).ID())
	if !ok {
		t.Fatalf("%s:%s is not in the read", k, key)
	}
	return e
}

// related is the keys of the entries e relates to through r, sorted.
func related(v atlas.View, e atlas.Entry, r atlas.Relation) []string {
	var out []string
	for _, ed := range e.Relations {
		if ed.Relation != r {
			continue
		}
		if target, ok := v.Find(ed.To); ok {
			out = append(out, target.Ref.Key)
		}
	}
	slices.Sort(out)
	return out
}

func atlasFact(e atlas.Entry, f atlas.Fact) string {
	for _, fv := range e.Facts {
		if fv.Fact == f {
			return fv.Value.Text
		}
	}
	return ""
}

func signalsOf(e atlas.Entry) []atlas.SignalKind {
	var out []atlas.SignalKind
	for _, s := range e.Signals {
		out = append(out, s.Kind)
	}
	return out
}

// atlasView is claims as the atlas folds them, with their signals joined.
func atlasView(claims []atlas.Claim) atlas.View {
	return atlas.Join(atlas.Fold(claims, atlasNow), atlas.Joined{}, atlasNow)
}

// The keys of the fixture's entries.
const (
	accountKey    = "github/github.com/acme"
	apiKey        = "github.com/acme/api"
	ciKey         = apiKey + "#workflow:ci.yml"
	deployKey     = apiKey + "#workflow:deploy.yml"
	nightlyKey    = apiKey + "#workflow:nightly.yml"
	productionKey = apiKey + "#environment:Production"
	euKey         = apiKey + "#environment:eu~20west~2f1"
)

// THE READ CLAIMS THE ORGANIZATION, EVERY REPOSITORY, EACH DECLARED WORKFLOW
// WITH ITS LAST RESULT ON THE DEFAULT BRANCH, AND EACH LASTING ENVIRONMENT
// WITH ITS NEWEST DEPLOYMENT, and relates what a deployment put its code
// behind to the environment by exact keys. It is made under a token carrying
// Read-only of what the installation holds, which it revokes. Nothing a
// payload carries beside the mapped fields reaches the read: no login, no
// email, no description, no payload, no log address.
//
// Mutation: decode a run's actor or a deployment's creator into a claim, or
// keep the address's user information, and a canary reaches the read.
func TestTheAtlasReadsRepositoriesWorkflowsAndEnvironments(t *testing.T) {
	t.Parallel()
	f := acme()
	read := f.read(t)
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{canaryPerson, canaryEmail, canaryPayload, canaryText, "hunter2", "frag",
		"installation-token", "pr-12", "pages-build-deployment", "gone.yml"} {
		if strings.Contains(string(out), canary) {
			t.Errorf("%q, which the read never maps, reached it: %s", canary, out)
		}
	}
	if len(read.Withheld) != 0 || len(read.Unread) != 0 {
		t.Errorf("the read withheld %+v and left unread %+v", read.Withheld, read.Unread)
	}
	// The installation, its token, its repositories and the token's revoke;
	// one repository's workflows, runs, deployments and two statuses; the
	// second's workflows and deployments; and nothing of the archived one.
	if read.Calls != 11 {
		t.Errorf("the read made %d calls, want 11: %v", read.Calls, f.calls)
	}
	want := map[string]string{"metadata": "read", "actions": "read", "deployments": "read"}
	if len(f.revoked) != 1 || len(f.minted) != 0 {
		t.Errorf("the read revoked %v and left %v live", f.revoked, f.minted)
	}
	if f.sent("GET /repos/acme/old/actions/workflows") != 0 || f.sent("GET /repos/acme/web/actions/runs") != 0 {
		t.Errorf("the read asked for what it has no use for: %v", f.calls)
	}
	// What the token was minted with is gone with the token, so mint again
	// to read what the read asks for.
	again := acme()
	again.refuse["/installation/token"] = http.StatusInternalServerError
	again.read(t)
	if got := again.minted["installation-token-7"]; !mapsEqual(got, want) {
		t.Errorf("the read's token carries %v, want read-only of what the installation holds: %v", got, want)
	}

	v := atlasView(read.Claims)
	account := atlasEntry(t, v, atlas.KindAccount, accountKey)
	api := atlasEntry(t, v, atlas.KindRepository, apiKey)
	if atlasFact(api, atlas.FactDefaultBranch) != "main" || atlasFact(api, atlas.FactStatus) != "active" {
		t.Errorf("the repository: %+v", api.Facts)
	}
	if got := related(v, api, atlas.RelationBelongsTo); !slices.Equal(got, []string{account.Ref.Key}) {
		t.Errorf("the repository belongs to %v", got)
	}
	if old := atlasEntry(t, v, atlas.KindRepository, "github.com/acme/old"); atlasFact(old, atlas.FactStatus) != "archived" ||
		atlasFact(old, atlas.FactVisibility) != "private" {
		t.Errorf("the archived repository: %+v", old.Facts)
	}
	if web := atlasEntry(t, v, atlas.KindRepository, "github.com/acme/web"); atlasFact(api, atlas.FactVisibility) != "private" ||
		atlasFact(web, atlas.FactVisibility) != "public" {
		t.Errorf("the repositories' visibilities: %+v and %+v", api.Facts, web.Facts)
	}
	for key, want := range map[string]struct{ name, state, result string }{
		ciKey:      {"CI", atlas.PipelineActive, atlas.ResultPassed},
		deployKey:  {"(deploy) Production", atlas.PipelineActive, atlas.ResultFailed},
		nightlyKey: {"Nightly", atlas.PipelineDisabled, ""},
	} {
		w := atlasEntry(t, v, atlas.KindPipeline, key)
		if w.Name != want.name || atlasFact(w, atlas.FactPipelineState) != want.state ||
			atlasFact(w, atlas.FactLastResult) != want.result {
			t.Errorf("the workflow %s: %q %+v", key, w.Name, w.Facts)
		}
		if got := related(v, w, atlas.RelationBuiltFrom); !slices.Equal(got, []string{apiKey}) {
			t.Errorf("the workflow %s is built from %v", key, got)
		}
	}
	production := atlasEntry(t, v, atlas.KindPipeline, productionKey)
	if production.Name != "Production" || atlasFact(production, atlas.FactLastResult) != atlas.ResultFailed ||
		atlasFact(production, atlas.FactEnvironment) != "production" {
		t.Errorf("the production environment: %q %+v", production.Name, production.Facts)
	}
	// A name with a space and a slash still has a key, exact both ways.
	eu := atlasEntry(t, v, atlas.KindPipeline, euKey)
	if eu.Name != "eu west/1" || atlasFact(eu, atlas.FactLastResult) != atlas.ResultPassed ||
		atlasFact(eu, atlas.FactEnvironment) != "" {
		t.Errorf("the second environment: %q %+v", eu.Name, eu.Facts)
	}
	// The address without its credentials, query and fragment, and its
	// name; an address by number is an address and no name.
	for key, kind := range map[string]atlas.Kind{
		"https://api.acme.example/": atlas.KindEndpoint, "api.acme.example": atlas.KindHostname,
	} {
		if got := related(v, atlasEntry(t, v, kind, key), atlas.RelationDeployedBy); !slices.Equal(got, []string{productionKey}) {
			t.Errorf("%s is deployed by %v", key, got)
		}
	}
	if got := related(v, atlasEntry(t, v, atlas.KindEndpoint, "https://203.0.113.9:8443/"), atlas.RelationDeployedBy); !slices.Equal(got, []string{euKey}) {
		t.Errorf("the numbered address is deployed by %v", got)
	}
	// Only what lives at GitHub is said to exist.
	for _, c := range read.Claims {
		owned := c.Subject.Kind == atlas.KindAccount || c.Subject.Kind == atlas.KindRepository ||
			c.Subject.Kind == atlas.KindPipeline
		if c.Facet.Kind == atlas.FacetExists && !owned {
			t.Errorf("the read says %s exists, which does not live at GitHub", c.Subject)
		}
	}
	// The account, three repositories, three workflows, two environments,
	// two addresses and one name, in exactly the claims counted here: a
	// field mapped by a later change shows as one more.
	if len(v.Entries) != 12 || len(read.Claims) != 47 {
		t.Errorf("the read folds %d claims to %d entries, want 47 and 12", len(read.Claims), len(v.Entries))
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A FAILING PIPELINE IS THE HEALTH OF WHAT IT DEPLOYS: the environment whose
// newest deployment failed needs a look, and so does the name it deployed,
// once a source that holds the name reports it. A workflow that failed needs
// one too; what passed, and what the failing ones do not deploy, is quiet.
//
// Mutation: fold a failed deployment's state to nothing, or relate the name
// to the repository rather than the environment, and the name stays quiet.
func TestAFailingDeploymentIsTheHealthOfWhatItDeploys(t *testing.T) {
	t.Parallel()
	read := acme().read(t)
	dns := atlas.IntegrationSource("cloudflare")
	name, err := atlas.Exists(atlas.MustRef(atlas.KindHostname, "api.acme.example"), dns, atlasNow)
	if err != nil {
		t.Fatal(err)
	}
	v := atlasView(append(slices.Clone(read.Claims), name))
	failing := []atlas.SignalKind{atlas.SignalPipelineFailing}
	for key, kind := range map[string]atlas.Kind{
		productionKey: atlas.KindPipeline, deployKey: atlas.KindPipeline, "api.acme.example": atlas.KindHostname,
	} {
		e := atlasEntry(t, v, kind, key)
		if !slices.Equal(signalsOf(e), failing) || e.Health() != atlas.HealthNeedsLook {
			t.Errorf("%s carries %v and is %s", key, signalsOf(e), e.Health())
		}
	}
	for key, kind := range map[string]atlas.Kind{
		ciKey: atlas.KindPipeline, euKey: atlas.KindPipeline, apiKey: atlas.KindRepository,
		"https://203.0.113.9:8443/": atlas.KindEndpoint,
	} {
		if e := atlasEntry(t, v, kind, key); len(e.Signals) != 0 {
			t.Errorf("%s, which nothing failing deploys, carries %v", key, signalsOf(e))
		}
	}
}

// A REPOSITORY GITLET MIRRORS IS ONE ENTRY: the repository GitHub lists and
// the Gitlet repository whose pull mirror names its address merge on that
// exact key, so the map shows one repository with both hosts' facts.
//
// Mutation: key the repository by anything but its host and its path, and
// the atlas shows the mirror and its source as two. Fold visibility as one
// fact of the entry, and every mirror waits in Review.
func TestARepositoryGitletMirrorsIsOneEntry(t *testing.T) {
	t.Parallel()
	read := acme().read(t)
	gitlet := atlas.RecordSource(atlas.RecordGitlet)
	mirror := atlas.MustRef(atlas.KindRepository, "gitlet/api")
	exists, err := atlas.Exists(mirror, gitlet, atlasNow)
	if err != nil {
		t.Fatal(err)
	}
	from, err := atlas.Says(mirror, atlas.FactPullMirrorOf, "https://GitHub.com/Acme/API.git", gitlet, atlasNow)
	if err != nil {
		t.Fatal(err)
	}
	company, err := atlas.Says(mirror, atlas.FactVisibility, "company", gitlet, atlasNow)
	if err != nil {
		t.Fatal(err)
	}
	v := atlasView(append(slices.Clone(read.Claims), exists, from, company))
	here, ok := v.Find(mirror.ID())
	there, found := v.Find(atlas.MustRef(atlas.KindRepository, apiKey).ID())
	if !ok || !found || here.ID != there.ID || len(here.Members) != 2 {
		t.Fatalf("the mirror and its source are %+v and %+v", here.Members, there.Members)
	}
	if atlasFact(here, atlas.FactDefaultBranch) != "main" {
		t.Errorf("the merged repository: %+v", here.Facts)
	}
	// PRIVATE ON GITHUB AND THE WHOLE COMPANY'S ON GITLET, both true: each
	// host's visibility is its own fact, and the mirror waits in no Review.
	seen := map[string]string{}
	for _, f := range here.Facts {
		if f.Fact == atlas.FactVisibility {
			seen[f.Of.Key] = f.Value.Text
		}
	}
	if here.Review || len(seen) != 2 || seen[apiKey] != "private" || seen["gitlet/api"] != "company" {
		t.Errorf("the mirror's visibilities: review %v, %v", here.Review, seen)
	}
	repositories := 0
	for _, e := range v.Entries {
		if e.Ref.Kind == atlas.KindRepository {
			repositories++
		}
	}
	if repositories != 3 {
		t.Errorf("the atlas shows %d repositories, want the three GitHub lists", repositories)
	}
}

// A PART NO INSTALLATION MAY READ IS A FINDING AND RETIRES NOTHING: an
// installation without Actions leaves the workflows unread and reads the
// rest, a listing GitHub refuses one repository leaves that repository's
// part unread, and neither withdraws, misses or retires what an earlier read
// recorded, however long it lasts. The read never asks for what it may not.
//
// Mutation: read a part the installation does not hold as an empty listing,
// and a changed permission retires every workflow a day later.
func TestAPartNoInstallationMayReadIsAFindingAndRetiresNothing(t *testing.T) {
	t.Parallel()
	whole := acme().read(t)
	f := acme()
	delete(f.installs[7].perms, "actions")
	f.refuse["/repos/Acme/API/deployments"] = http.StatusNotFound
	read := f.read(t)
	if len(read.Unread) != 2 || read.Unread[0].Part != "workflows" || read.Unread[1].Part != "deployments" ||
		!strings.Contains(read.Unread[0].Detail, "Actions (Read-only)") ||
		!strings.Contains(read.Unread[1].Detail, "Deployments (Read-only)") {
		t.Fatalf("the refused parts: %+v", read.Unread)
	}
	if strings.Contains(read.Unread[1].Detail, "not accessible") {
		t.Errorf("the finding repeats the vendor's answer: %q", read.Unread[1].Detail)
	}
	if f.sent("GET /repos/Acme/API/actions/workflows") != 0 {
		t.Errorf("the read asked for workflows no installation may read: %v", f.calls)
	}
	v := atlasView(read.Claims)
	if e := atlasEntry(t, v, atlas.KindRepository, apiKey); atlasFact(e, atlas.FactStatus) != "active" {
		t.Errorf("the repositories were not read beside the refused parts: %+v", e.Facts)
	}
	status := collect.Status{Kind: integration.KindGitHub}
	for _, at := range []time.Time{atlasNow.Add(time.Hour), atlasNow.Add(30 * time.Hour), atlasNow.Add(60 * time.Hour)} {
		plan := collect.Decide(whole.Claims, read, status, at)
		if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 {
			t.Fatalf("at %s the refused parts recorded %+v and missed %+v", at, plan.Records, plan.Status.Missing)
		}
		if len(plan.Status.Unread) != 2 {
			t.Fatalf("the standing's findings: %+v", plan.Status.Unread)
		}
		status = plan.Status
	}
	// The same read, were its parts whole and empty, retires every pipeline.
	empty := read
	empty.Unread = nil
	first := collect.Decide(whole.Claims, empty, collect.Status{Kind: integration.KindGitHub}, atlasNow.Add(time.Hour))
	if plan := collect.Decide(whole.Claims, empty, first.Status, atlasNow.Add(26*time.Hour)); plan.Status.Retired != 5 {
		t.Errorf("the control retired %d pipelines, want 5", plan.Status.Retired)
	}
}

// AN INSTALLATION THE READ CANNOT BE MADE AS CARRIES WHAT ONLY IT COVERED: a
// suspended installation, one that is gone and a seat with no installation
// are each a finding, the repositories another agent's installation covers
// are read, and everything of a repository this read did not list stands. A
// seat switched off is not read through and is no finding.
//
// Mutation: skip a suspended installation in silence, and the repositories
// only it covers are retired a day after somebody suspends it.
func TestAnInstallationThatCannotBeReadCarriesWhatOnlyItCovered(t *testing.T) {
	t.Parallel()
	both := func() *forge {
		f := acme()
		f.installs[7].repos = []string{"Acme/API"}
		f.installs[8] = &ghInstall{login: "acme", repos: []string{"acme/web", "acme/old"},
			perms: map[string]string{"metadata": "read"}}
		return f
	}
	off := agent("zed", 44, 10)
	*off.Integrations.GitHub.Enabled = false
	roles := []config.Role{agent("sre", 41, 7), agent("ops", 42, 8), agent("new", 43, 0), off}
	whole := both().read(t, roles[0], roles[1])
	f := both()
	f.installs[8].suspended = true
	read := f.read(t, roles...)
	if len(read.Unread) != 1 || read.Unread[0].Part != "installations" ||
		!strings.HasPrefix(read.Unread[0].Detail, "2 of 3 agents' GitHub Apps") {
		t.Fatalf("the installations that could not be read: %+v", read.Unread)
	}
	if f.sent("GET /app/installations/10") != 0 || f.sent("POST /app/installations/8/access_tokens") != 0 {
		t.Errorf("the read asked as a seat it may not be made as: %v", f.calls)
	}
	v := atlasView(read.Claims)
	if _, ok := v.Find(atlas.MustRef(atlas.KindRepository, "github.com/acme/web").ID()); ok {
		t.Error("a repository only the suspended installation covers was read")
	}
	if e := atlasEntry(t, v, atlas.KindPipeline, deployKey); atlasFact(e, atlas.FactLastResult) != atlas.ResultFailed {
		t.Errorf("the other installation's repository was not read: %+v", e.Facts)
	}
	status := collect.Status{Kind: integration.KindGitHub}
	for _, at := range []time.Time{atlasNow.Add(time.Hour), atlasNow.Add(30 * time.Hour)} {
		plan := collect.Decide(whole.Claims, read, status, at)
		if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 {
			t.Fatalf("at %s a suspended installation recorded %+v and missed %+v", at, plan.Records, plan.Status.Missing)
		}
		status = plan.Status
	}
	// Every installation closed: still a read, of the organization alone.
	f.installs[7].suspended = true
	none := f.read(t, roles...)
	if len(none.Unread) != 1 || len(none.Claims) != 1 {
		t.Errorf("a read no installation opened holds %+v and %+v", none.Claims, none.Unread)
	}
	if plan := collect.Decide(whole.Claims, none, collect.Status{Kind: integration.KindGitHub}, atlasNow.Add(48*time.Hour)); len(plan.Records) != 0 {
		t.Errorf("a read no installation opened recorded %+v", plan.Records)
	}
}

// AN ENVIRONMENT NOTHING DEPLOYS TO ANY MORE RETIRES ITSELF AND NOTHING IT
// DEPLOYED: after two complete reads a day apart the environment is denied,
// and the name and the address its deployment named have that relation
// withdrawn, with no denial of either.
//
// Mutation: claim the deployed name with Ref rather than Names, and the read
// denies a name the DNS provider still lists.
func TestAnEnvironmentThatIsGoneRetiresNothingItDeployed(t *testing.T) {
	t.Parallel()
	whole := acme().read(t)
	f := acme()
	f.repos["Acme/API"].deployments = slices.DeleteFunc(f.repos["Acme/API"].deployments,
		func(d map[string]any) bool { return d["environment"] == "Production" })
	read := f.read(t)
	first := collect.Decide(whole.Claims, read, collect.Status{Kind: integration.KindGitHub}, atlasNow.Add(time.Hour))
	if len(first.Records) != 0 || len(first.Status.Missing) == 0 {
		t.Fatalf("the first miss recorded %+v and missed %+v", first.Records, first.Status.Missing)
	}
	second := collect.Decide(whole.Claims, read, first.Status, atlasNow.Add(26*time.Hour))
	environment := atlas.MustRef(atlas.KindPipeline, productionKey)
	named := map[atlas.Ref]bool{
		atlas.MustRef(atlas.KindHostname, "api.acme.example"):          false,
		atlas.MustRef(atlas.KindEndpoint, "https://api.acme.example/"): false,
	}
	for _, rec := range second.Records {
		switch _, deployed := named[rec.Entry]; {
		case rec.Entry == environment:
			if !rec.Retires || len(rec.Claims) != 1 || !rec.Claims[0].Denied {
				t.Errorf("the environment that is gone: %+v", rec)
			}
		case deployed:
			named[rec.Entry] = true
			if rec.Retires || len(rec.Claims) != 0 || len(rec.Withdrawn) != 1 {
				t.Errorf("what the environment deployed: %+v", rec)
			}
		default:
			t.Errorf("an environment that is gone recorded %s", rec.Entry)
		}
	}
	if second.Status.Retired != 1 || slices.Contains(slices.Collect(mapValues(named)), false) {
		t.Errorf("retired %d and released %v", second.Status.Retired, named)
	}
}

func mapValues(m map[atlas.Ref]bool) func(func(bool) bool) {
	return func(yield func(bool) bool) {
		for _, v := range m {
			if !yield(v) {
				return
			}
		}
	}
}

// NO CREDENTIAL SHAPE REACHES THE ATLAS FROM GITHUB: every shape the
// redaction pass knows, planted in every field the read maps and in the ones
// it never reads, leaves no piece of itself in the read, and what the mapped
// fields refused is counted.
//
// Mutation: map a deployment's payload or a run's actor into a claim, cut a
// name before the builder judges it, or skip the builder's grammar on a key
// or a value, and a shape reaches the read.
func TestNoCredentialShapeReachesTheAtlasFromGitHub(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		// A name long enough that cutting it to a label's length would cut
		// the shape short of its end, where no rule knows it any more.
		long := strings.Repeat("m", max(1, atlas.MaxLabelRunes-len(s)+len(s)/4)) + " " + s
		who := map[string]any{"login": s, "email": s, "name": s, "avatar_url": s}
		f := &forge{
			installs: map[int64]*ghInstall{7: {login: "acme", repos: []string{"acme/api", "acme/planted"},
				perms: map[string]string{"metadata": "read", "actions": "read", "deployments": "read"}}},
			repos: map[string]*ghRepo{
				"acme/planted": {row: map[string]any{"full_name": "acme/" + s, "default_branch": s, "description": s,
					"visibility": s,
					"homepage":   s, "owner": who, "topics": []string{s}, "clone_url": "https://x:" + s + "@github.com/acme/api.git"}},
				"acme/api": {
					row: map[string]any{"full_name": "acme/api", "default_branch": "main", "description": s, "owner": who,
						"visibility": s},
					workflows: []map[string]any{
						{"id": 1, "name": s, "path": ".github/workflows/" + s, "state": s, "badge_url": s},
						{"id": 2, "name": long, "path": ".github/workflows/ci.yml", "state": "active"},
						{"id": 3, "name": "Deploy", "path": ".github/workflows/deploy.yml", "state": "active"},
					},
					runs: []map[string]any{
						{"workflow_id": 2, "conclusion": s, "actor": who, "triggering_actor": who, "name": s,
							"display_title": s, "head_branch": s, "head_commit": map[string]any{"message": s, "author": who}},
						{"workflow_id": 3, "conclusion": "success", "actor": who},
					},
					deployments: []map[string]any{
						{"id": 30, "environment": s, "creator": who, "description": s, "task": s, "ref": s,
							"payload": map[string]any{"secret": s}},
						{"id": 29, "environment": long},
						{"id": 28, "environment": "production", "payload": s},
						{"id": 27, "environment": "staging"},
						{"id": 26, "environment": "preview"},
					},
					statuses: map[int64][]map[string]any{
						30: {{"state": s, "environment_url": s, "description": s, "log_url": s, "target_url": s, "creator": who}},
						28: {{"state": "success", "creator": who, "description": s,
							"environment_url": "https://ops:" + s + "@www.acme.example/ok?token=" + s + "#" + s}},
						27: {{"state": "success", "environment_url": "https://" + s + ".acme.example/"}},
						26: {{"state": "success", "environment_url": "https://api.acme.example/" + s}},
					},
				},
			},
			refuse: map[string]int{},
			minted: map[string]map[string]string{},
		}
		read := f.read(t)
		out, err := json.Marshal(read)
		if err != nil {
			t.Fatal(err)
		}
		// The shape's second half, and a piece of its body near its start,
		// which is what a name cut short of the shape's end would keep.
		for _, piece := range []string{s[len(s)/2:], s[len(s)/4 : len(s)/4+12]} {
			if strings.Contains(string(out), piece) {
				at := strings.Index(string(out), piece)
				t.Errorf("%s: a piece of the shape reached the read near %q", shape.Name,
					string(out)[max(0, at-120):at])
			}
		}
		// A repository's name, a workflow's file, a workflow's name, two
		// environments' names and three deployed addresses are each
		// refused where they sat.
		if len(read.Withheld) < 8 {
			t.Errorf("%s: %d values withheld, want every planted mapped field: %+v", shape.Name,
				len(read.Withheld), read.Withheld)
		}
		// What sat beside a credential is still read: an address is kept
		// without the user information and the query that carried one. A
		// shape with a space in it makes the address no address at all.
		if strings.ContainsAny(s, " \n") {
			continue
		}
		v := atlasView(read.Claims)
		e, ok := v.Find(atlas.MustRef(atlas.KindEndpoint, "https://www.acme.example/ok").ID())
		if !ok || !slices.Equal(related(v, e, atlas.RelationDeployedBy), []string{apiKey + "#environment:production"}) {
			t.Errorf("%s: the address beside the credential was not read: %+v", shape.Name, e)
		}
	}
}

// A LISTING THAT DID NOT FINISH IS NO READ AT ALL: a listing that fails for
// any reason but a refusal, GitHub's rate limit included, fails the whole
// read, so nothing is retired on a listing cut short; and the token the read
// minted is revoked all the same.
//
// Mutation: read a rate-limited listing as a refusal, and a busy hour is
// recorded as a missing permission.
func TestAListingThatFailsOrIsRateLimitedFailsTheRead(t *testing.T) {
	t.Parallel()
	for name, fault := range map[string]func(*forge){
		"a server fault":   func(f *forge) { f.refuse["/repos/Acme/API/actions/runs"] = http.StatusBadGateway },
		"the rate limit":   func(f *forge) { f.limit = "/repos/Acme/API/deployments/30/statuses" },
		"the repositories": func(f *forge) { f.refuse["/installation/repositories"] = http.StatusForbidden },
		"an installation":  func(f *forge) { f.refuse["/app/installations/7"] = http.StatusInternalServerError },
	} {
		f := acme()
		fault(f)
		read, err := githubSource(t).Collect(t.Context(), f.input(t))
		if err == nil || errors.Is(err, integration.ErrNotConfigured) || len(read.Claims) != 0 {
			t.Errorf("%s: the read answered %d claims and %v", name, len(read.Claims), err)
		}
		if name != "an installation" && len(f.revoked) != 1 {
			t.Errorf("%s: the read left its token live: revoked %v", name, f.revoked)
		}
	}
}

// A LISTING IS WALKED TO ITS END: repositories and workflows past a first
// page are read, and a repository's deployments are read newest first for
// three pages and no further.
func TestTheListingsAreWalkedToTheirBounds(t *testing.T) {
	t.Parallel()
	f := acme()
	api := f.repos["Acme/API"]
	api.workflows, api.runs, api.statuses = nil, nil, map[int64][]map[string]any{}
	for i := range 130 {
		api.workflows = append(api.workflows, map[string]any{"id": 100 + i, "name": "W" + strconv.Itoa(i),
			"path": ".github/workflows/w" + strconv.Itoa(i) + ".yml", "state": "active"})
	}
	api.deployments = nil
	for i := range 350 {
		env := "preview"
		if i == 320 {
			env = "long-ago"
		}
		api.deployments = append(api.deployments, map[string]any{"id": 1000 - i, "environment": env})
	}
	for i := range 120 {
		name := "acme/r" + strconv.Itoa(i)
		f.repos[name] = &ghRepo{row: map[string]any{"full_name": name, "default_branch": "main", "archived": true}}
		f.installs[7].repos = append(f.installs[7].repos, name)
	}
	v := atlasView(f.read(t).Claims)
	repositories, workflows := 0, 0
	for _, e := range v.Entries {
		switch {
		case e.Ref.Kind == atlas.KindRepository:
			repositories++
		case strings.Contains(e.Ref.Key, "#workflow:"):
			workflows++
		}
	}
	if repositories != 123 || workflows != 130 {
		t.Errorf("the read listed %d repositories and %d workflows, want 123 and 130", repositories, workflows)
	}
	if f.sent("GET /repos/Acme/API/deployments") != 3 {
		t.Errorf("the deployments were walked %d pages, want 3", f.sent("GET /repos/Acme/API/deployments"))
	}
	if _, ok := v.Find(atlas.MustRef(atlas.KindPipeline, apiKey+"#environment:long-ago").ID()); ok {
		t.Error("an environment last deployed past the walk's bound was read")
	}
	atlasEntry(t, v, atlas.KindPipeline, apiKey+"#environment:preview")
}

// A COMPANY THAT DOES NOT CONNECT GITHUB, NAMES NO ORGANIZATION OR HAS NO
// AGENT'S APP SWITCHED ON IS NOT CONFIGURED, which the duty reads as nothing
// to read rather than a fault.
func TestAnUnconnectedGitHubIsNotConfigured(t *testing.T) {
	t.Parallel()
	off := agent("sre", 41, 7)
	*off.Integrations.GitHub.Enabled = false
	for name, edit := range map[string]func(*config.Company){
		"no block":        func(c *config.Company) { c.Integrations.GitHub = nil },
		"switched off":    func(c *config.Company) { c.Integrations.GitHub.Enabled = false },
		"no organization": func(c *config.Company) { c.Integrations.GitHub.Provisioning = nil },
		"no app":          func(c *config.Company) { c.Roles = []config.Role{off, {Name: "Plain", Handle: "plain"}} },
	} {
		f := acme()
		in := f.input(t)
		edit(in.Config)
		if _, err := githubSource(t).Collect(t.Context(), in); !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: %v, want not configured", name, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s: the read called GitHub: %v", name, f.calls)
		}
	}
	if _, err := githubSource(t).Collect(t.Context(), collect.Input{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no document: %v, want not configured", err)
	}
}
