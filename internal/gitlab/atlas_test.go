package gitlab_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// Values the fake carries in fields the read never decodes.
const (
	canaryPerson = "atlas-canary-person"
	canaryEmail  = "atlas-canary-person@acme.example"
	canarySecret = "atlas-canary-runner-token"
	canaryText   = "atlas-canary-description"
)

// The two credentials a company may hold, and the variables they resolve from.
const (
	atlasReadToken  = "read-token-value"
	atlasAdminToken = "admin-token-value"
)

// glProject is one project at the fake. files is every file of its default
// branch's tree, and links the symbolic links beside them, by path.
type glProject struct {
	row       map[string]any
	pipelines []map[string]any
	envs      []map[string]any
	head      string
	files     map[string]string
	links     map[string]string
}

// hub is a GitLab group as the atlas reads it. A path named in refuse answers
// that status, and a request under any credential but token is refused as
// GitLab refuses a dead one.
type hub struct {
	token    string
	group    string
	projects []*glProject
	refuse   map[string]int

	mu    sync.Mutex
	calls []string
}

var (
	groupRoute       = regexp.MustCompile(`^/api/v4/groups/([^/]+)$`)
	groupProjects    = regexp.MustCompile(`^/api/v4/groups/(\d+)/projects$`)
	projectRoute     = regexp.MustCompile(`^/api/v4/projects/([^/]+)$`)
	pipelinesRoute   = regexp.MustCompile(`^/api/v4/projects/(\d+)/pipelines$`)
	environmentsList = regexp.MustCompile(`^/api/v4/projects/(\d+)/environments$`)
	branchRoute      = regexp.MustCompile(`^/api/v4/projects/(\d+)/repository/branches/([^/]+)$`)
	treeRoute        = regexp.MustCompile(`^/api/v4/projects/(\d+)/repository/tree$`)
	blobRoute        = regexp.MustCompile(`^/api/v4/projects/(\d+)/repository/blobs/([0-9a-f]+)/raw$`)
	environmentRoute = regexp.MustCompile(`^/api/v4/projects/(\d+)/environments/(\d+)$`)
)

const groupID = 9

func (h *hub) project(id string) *glProject {
	for _, p := range h.projects {
		if strconv.Itoa(p.row["id"].(int)) == id {
			return p
		}
	}
	return nil
}

func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	// An instance may be served under a path of its host.
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/gitlab")
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, r.Method+" "+path)
	w.Header().Set("Content-Type", "application/json")
	answer := func(status int, body any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	if r.Method != http.MethodGet {
		answer(http.StatusMethodNotAllowed, map[string]string{"message": "the read wrote"})
		return
	}
	if r.Header.Get("PRIVATE-TOKEN") != h.token {
		answer(http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
		return
	}
	if status := h.refuse[path]; status != 0 {
		answer(status, map[string]string{"message": "403 Forbidden - " + canaryText})
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
	wrong := func(what string) {
		answer(http.StatusBadRequest, map[string]string{"message": "the read asked for the wrong " + what})
	}
	switch {
	case groupProjects.MatchString(path):
		if q.Has("archived") || q.Get("with_shared") != "false" || q.Get("include_subgroups") != "true" ||
			q.Get("order_by") != "id" || q.Get("sort") != "asc" {
			wrong("projects")
			return
		}
		var rows []map[string]any
		for _, p := range h.projects {
			if p.row["listed"] != false {
				rows = append(rows, p.row)
			}
		}
		answer(http.StatusOK, paged(rows))
	case groupRoute.MatchString(path):
		name, _ := url.PathUnescape(groupRoute.FindStringSubmatch(path)[1])
		if !strings.EqualFold(name, h.group) {
			answer(http.StatusNotFound, map[string]string{"message": "404 Group Not Found"})
			return
		}
		answer(http.StatusOK, map[string]any{"id": groupID, "full_path": h.group, "name": canaryText,
			"description": canaryText, "runners_token": canarySecret})
	case pipelinesRoute.MatchString(path):
		p := h.project(pipelinesRoute.FindStringSubmatch(path)[1])
		if q.Get("scope") != "finished" || q.Get("ref") != p.row["default_branch"] || q.Get("per_page") != "1" {
			wrong("pipelines")
			return
		}
		answer(http.StatusOK, paged(p.pipelines))
	case environmentsList.MatchString(path):
		if q.Get("states") != "available" {
			wrong("environments")
			return
		}
		var rows []map[string]any
		for _, e := range h.project(environmentsList.FindStringSubmatch(path)[1]).envs {
			if e["state"] != "stopped" {
				rows = append(rows, e)
			}
		}
		answer(http.StatusOK, paged(rows))
	case environmentRoute.MatchString(path):
		m := environmentRoute.FindStringSubmatch(path)
		for _, e := range h.project(m[1]).envs {
			if strconv.Itoa(e["id"].(int)) == m[2] {
				answer(http.StatusOK, e)
				return
			}
		}
		answer(http.StatusNotFound, map[string]string{"message": "404 Not found"})
	case branchRoute.MatchString(path):
		m := branchRoute.FindStringSubmatch(path)
		p := h.project(m[1])
		if name, _ := url.PathUnescape(m[2]); name != p.row["default_branch"] {
			answer(http.StatusNotFound, map[string]string{"message": "404 Branch Not Found"})
			return
		}
		answer(http.StatusOK, map[string]any{"name": m[2], "commit": map[string]any{"id": p.head,
			"message": canaryText, "author_name": canaryPerson, "author_email": canaryEmail}})
	case treeRoute.MatchString(path):
		p := h.project(treeRoute.FindStringSubmatch(path)[1])
		if q.Get("ref") != p.head || q.Get("recursive") != "true" || q.Get("pagination") != "keyset" {
			wrong("tree")
			return
		}
		// BY KEYSET: the page after a token, and a link to the next.
		rows := p.tree()
		size, _ := strconv.Atoi(q.Get("per_page"))
		start, _ := strconv.Atoi(q.Get("page_token"))
		end := min(len(rows), start+size)
		if end < len(rows) {
			next := *r.URL
			nq := next.Query()
			nq.Set("page_token", strconv.Itoa(end))
			next.RawQuery = nq.Encode()
			w.Header().Set("Link", "<https://gitlab.com"+next.String()+`>; rel="next"`)
		}
		answer(http.StatusOK, rows[start:end])
	case blobRoute.MatchString(path):
		m := blobRoute.FindStringSubmatch(path)
		for _, body := range h.project(m[1]).files {
			if gitBlob(body) == m[2] {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write([]byte(body))
				return
			}
		}
		for _, target := range h.project(m[1]).links {
			if gitBlob(target) == m[2] {
				_, _ = w.Write([]byte(target))
				return
			}
		}
		answer(http.StatusNotFound, map[string]string{"message": "404 Blob Not Found"})
	case projectRoute.MatchString(path):
		name, _ := url.PathUnescape(projectRoute.FindStringSubmatch(path)[1])
		for _, p := range h.projects {
			if strings.EqualFold(p.row["path_with_namespace"].(string), name) {
				answer(http.StatusOK, p.row)
				return
			}
		}
		answer(http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
	default:
		answer(http.StatusNotFound, map[string]string{"message": "404 Not Found"})
	}
}

func (h *hub) sent(call string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c == call {
			n++
		}
	}
	return n
}

// person is what GitLab nests in almost every row: who did it.
func person() map[string]any {
	return map[string]any{"username": canaryPerson, "email": canaryEmail, "name": canaryPerson}
}

// deployment is an environment's newest deployment as GitLab nests it.
func deployment(status string) map[string]any {
	return map[string]any{"status": status, "user": person(), "ref": canaryText,
		"deployable": map[string]any{"name": canaryText, "user": person(),
			"commit": map[string]any{"message": canaryText, "author_email": canaryEmail}}}
}

// acme is a group the read credential sees whole: a project whose pipeline
// and newest production deployment failed, with one more environment and a
// stopped review one; a project in a subgroup; an archived project; one with
// its pipelines switched off; and an empty one.
func acme() *hub {
	return &hub{
		token: atlasReadToken, group: "Acme",
		projects: []*glProject{
			{
				row: map[string]any{"id": 1, "path_with_namespace": "Acme/API", "default_branch": "main",
					"visibility": "private", "jobs_enabled": true, "description": canaryText, "owner": person(),
					"runners_token":        canarySecret,
					"import_url":           "https://mirror:" + canarySecret + "@git.acme.example/api.git",
					"service_desk_address": canaryEmail, "topics": []string{canaryText}},
				pipelines: []map[string]any{
					{"id": 90, "status": "failed", "user": person(), "name": canaryText, "ref": "main"},
					{"id": 89, "status": "success"},
				},
				envs: []map[string]any{
					{"id": 27, "name": "eu west/1", "tier": "other", "state": "available",
						"external_url": "https://203.0.113.9:8443/", "last_deployment": deployment("success")},
					{"id": 29, "name": "review/pr-12", "tier": "development", "state": "stopped",
						"external_url": "https://pr-12.acme.example/", "last_deployment": deployment("success")},
					{"id": 30, "name": "Production", "tier": "production", "state": "available",
						"description":     canaryText,
						"external_url":    "https://deploy:hunter2@api.acme.example/?token=" + canarySecret + "#frag",
						"last_deployment": deployment("failed")},
				},
			},
			{
				row: map[string]any{"id": 2, "path_with_namespace": "acme/platform/web", "default_branch": "main",
					"visibility": "public", "builds_access_level": "enabled"},
				pipelines: []map[string]any{{"id": 70, "status": "success"}},
			},
			{row: map[string]any{"id": 3, "path_with_namespace": "acme/old", "default_branch": "master",
				"visibility": "internal", "archived": true}},
			{row: map[string]any{"id": 4, "path_with_namespace": "acme/docs", "default_branch": "main",
				"visibility": "private", "builds_access_level": "disabled", "environments_access_level": "disabled"}},
			{row: map[string]any{"id": 5, "path_with_namespace": "acme/empty", "visibility": "private",
				"empty_repo": true}},
		},
		refuse: map[string]int{},
	}
}

// company is a company connected to the fake's group, holding both
// credentials unless edited.
func company() *config.Company {
	c := &config.Company{Name: "Acme"}
	c.Integrations.GitLab = &config.GitLab{Enabled: true, URL: "https://gitlab.com", Token: "${GITLAB_TOKEN}",
		Provisioning: &config.GitLabProvisioning{Group: "Acme", AdminToken: "${GITLAB_ADMIN_TOKEN}"}}
	return c
}

func (h *hub) input(t *testing.T, edit ...func(*config.Company)) collect.Input {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(srv.Close)
	c := company()
	for _, e := range edit {
		e(c)
	}
	env := config.MapSource{"GITLAB_TOKEN": atlasReadToken, "GITLAB_ADMIN_TOKEN": atlasAdminToken}
	return collect.Input{Config: c, Now: atlasNow, Env: config.NewResolver(env),
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, srv)}}
}

func gitlabSource(t *testing.T) collect.Source {
	t.Helper()
	src, ok := collect.Lookup(integration.KindGitLab)
	if !ok {
		t.Fatal("no GitLab source is registered")
	}
	return src
}

func (h *hub) read(t *testing.T, edit ...func(*config.Company)) collect.Read {
	t.Helper()
	read, err := gitlabSource(t).Collect(t.Context(), h.input(t, edit...))
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
	accountKey    = "gitlab/gitlab.com/acme"
	apiKey        = "gitlab.com/acme/api"
	webKey        = "gitlab.com/acme/platform/web"
	apiPipeline   = apiKey + "#pipeline"
	webPipeline   = webKey + "#pipeline"
	productionKey = apiKey + "#environment:Production"
	euKey         = apiKey + "#environment:eu~20west~2f1"
)

// THE READ CLAIMS THE GROUP, EVERY PROJECT AS A REPOSITORY, THE PIPELINE OF
// EACH PROJECT THAT HAS RUN ONE ON ITS DEFAULT BRANCH, AND EACH AVAILABLE
// ENVIRONMENT WITH ITS NEWEST DEPLOYMENT, and relates what an environment
// serves to it by exact keys. It is made under the read credential, with GET
// alone. Nothing a payload carries beside the mapped fields reaches the read:
// no person, no email, no description, no runner token, no import address.
//
// Mutation: decode a project's runners_token or a deployment's user into a
// claim, or keep the address's user information, and a canary reaches the read.
func TestTheAtlasReadsProjectsPipelinesAndEnvironments(t *testing.T) {
	t.Parallel()
	h := acme()
	read := h.read(t)
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{canaryPerson, canaryEmail, canarySecret, canaryText, "hunter2", "frag",
		atlasReadToken, atlasAdminToken, "pr-12"} {
		if strings.Contains(string(out), canary) {
			t.Errorf("%q, which the read never maps, reached it: %s", canary, out)
		}
	}
	if len(read.Withheld) != 0 || len(read.Unread) != 0 {
		t.Errorf("the read withheld %+v and left unread %+v", read.Withheld, read.Unread)
	}
	// The group and its projects; one project's pipelines, environments and
	// two environments' deployments; the second's pipelines and
	// environments; and nothing of the archived one, the one with its
	// pipelines off or the empty one.
	if read.Calls != 8 {
		t.Errorf("the read made %d calls, want 8: %v", read.Calls, h.calls)
	}
	for _, id := range []string{"3", "4", "5"} {
		if h.sent("GET /api/v4/projects/"+id+"/pipelines") != 0 || h.sent("GET /api/v4/projects/"+id+"/environments") != 0 {
			t.Errorf("the read asked project %s for what it has no use for: %v", id, h.calls)
		}
	}
	if h.sent("GET /api/v4/projects/1/environments/29") != 0 {
		t.Errorf("the read asked for a stopped environment: %v", h.calls)
	}

	v := atlasView(read.Claims)
	account := atlasEntry(t, v, atlas.KindAccount, accountKey)
	api := atlasEntry(t, v, atlas.KindRepository, apiKey)
	if atlasFact(api, atlas.FactDefaultBranch) != "main" || atlasFact(api, atlas.FactStatus) != "active" ||
		atlasFact(api, atlas.FactVisibility) != "private" {
		t.Errorf("the repository: %+v", api.Facts)
	}
	if got := related(v, api, atlas.RelationBelongsTo); !slices.Equal(got, []string{account.Ref.Key}) {
		t.Errorf("the repository belongs to %v", got)
	}
	if old := atlasEntry(t, v, atlas.KindRepository, "gitlab.com/acme/old"); atlasFact(old, atlas.FactStatus) != "archived" ||
		atlasFact(old, atlas.FactVisibility) != "internal" {
		t.Errorf("the archived repository: %+v", old.Facts)
	}
	// A project in a subgroup is keyed by its whole path.
	if web := atlasEntry(t, v, atlas.KindRepository, webKey); atlasFact(web, atlas.FactVisibility) != "public" {
		t.Errorf("the subgroup's repository: %+v", web.Facts)
	}
	for key, want := range map[string]struct{ name, result, from string }{
		apiPipeline: {"API pipeline", atlas.ResultFailed, apiKey},
		webPipeline: {"web pipeline", atlas.ResultPassed, webKey},
	} {
		p := atlasEntry(t, v, atlas.KindPipeline, key)
		if p.Name != want.name || atlasFact(p, atlas.FactLastResult) != want.result {
			t.Errorf("the pipeline %s: %q %+v", key, p.Name, p.Facts)
		}
		if got := related(v, p, atlas.RelationBuiltFrom); !slices.Equal(got, []string{want.from}) {
			t.Errorf("the pipeline %s is built from %v", key, got)
		}
	}
	production := atlasEntry(t, v, atlas.KindPipeline, productionKey)
	if production.Name != "Production" || atlasFact(production, atlas.FactLastResult) != atlas.ResultFailed ||
		atlasFact(production, atlas.FactEnvironment) != "production" {
		t.Errorf("the production environment: %q %+v", production.Name, production.Facts)
	}
	// A name with a space and a slash still has a key, exact both ways, and
	// a tier GitLab could not place suggests no environment.
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
	// Only what lives at GitLab is said to exist.
	for _, c := range read.Claims {
		owned := c.Subject.Kind == atlas.KindAccount || c.Subject.Kind == atlas.KindRepository ||
			c.Subject.Kind == atlas.KindPipeline
		if c.Facet.Kind == atlas.FacetExists && !owned {
			t.Errorf("the read says %s exists, which does not live at GitLab", c.Subject)
		}
	}
	// The group, five repositories, two pipelines, two environments, two
	// addresses and one name, in exactly the claims counted here: a field
	// mapped by a later change shows as one more.
	if len(v.Entries) != 13 || len(read.Claims) != 49 {
		t.Errorf("the read folds %d claims to %d entries, want 49 and 13", len(read.Claims), len(v.Entries))
	}
}

// THE READ CREDENTIAL IS USED WHERE THE COMPANY GAVE ONE, AND THE
// PROVISIONING TOKEN ONLY WHERE IT DID NOT: a token that may write is on the
// read's path only when nothing narrower exists.
//
// Mutation: prefer the provisioning token, and the fake, which accepts the
// read credential alone, refuses the first read.
func TestTheReadCredentialIsUsedBeforeTheProvisioningToken(t *testing.T) {
	t.Parallel()
	acme().read(t)
	h := acme()
	h.token = atlasAdminToken
	read := h.read(t, func(c *config.Company) { c.Integrations.GitLab.Token = "" })
	if len(read.Claims) == 0 || len(read.Unread) != 0 {
		t.Errorf("a company with the provisioning token alone read %d claims, unread %+v", len(read.Claims), read.Unread)
	}
	// A finding names the setting the credential it is about is held in.
	h = acme()
	h.token = atlasAdminToken
	h.refuse["/api/v4/projects/1/pipelines"] = http.StatusForbidden
	read = h.read(t, func(c *config.Company) { c.Integrations.GitLab.Token = "" })
	if len(read.Unread) != 1 || !strings.Contains(read.Unread[0].Detail, "integrations.gitlab.provisioning.admin_token") {
		t.Errorf("the finding under the provisioning token: %+v", read.Unread)
	}
}

// A FAILING PIPELINE IS THE HEALTH OF WHAT IT DEPLOYS: the environment whose
// newest deployment failed needs a look, and so does the name it serves, once
// a source that holds the name reports it. A project whose pipeline failed
// needs one too; what passed, and what the failing ones do not deploy, is
// quiet.
//
// Mutation: fold a failed deployment's status to nothing, or relate the name
// to the repository rather than the environment, and the name stays quiet.
func TestAFailingGitLabDeploymentIsTheHealthOfWhatItDeploys(t *testing.T) {
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
		productionKey: atlas.KindPipeline, apiPipeline: atlas.KindPipeline, "api.acme.example": atlas.KindHostname,
	} {
		e := atlasEntry(t, v, kind, key)
		if !slices.Equal(signalsOf(e), failing) || e.Health() != atlas.HealthNeedsLook {
			t.Errorf("%s carries %v and is %s", key, signalsOf(e), e.Health())
		}
	}
	for key, kind := range map[string]atlas.Kind{
		webPipeline: atlas.KindPipeline, euKey: atlas.KindPipeline, apiKey: atlas.KindRepository,
		"https://203.0.113.9:8443/": atlas.KindEndpoint,
	} {
		if e := atlasEntry(t, v, kind, key); len(e.Signals) != 0 {
			t.Errorf("%s, which nothing failing deploys, carries %v", key, signalsOf(e))
		}
	}
}

// A PROJECT GITLET MIRRORS IS ONE ENTRY, on an instance served under a path
// of its host as well: the repository GitLab lists and the Gitlet repository
// whose pull mirror names its address merge on that exact key, with each
// host's own visibility and no Review.
//
// Mutation: key the project by the instance's host and its path alone, and
// an instance under /gitlab shows every mirror and its source as two.
func TestAProjectGitletMirrorsIsOneEntry(t *testing.T) {
	t.Parallel()
	for web, key := range map[string]string{
		"https://gitlab.com":              apiKey,
		"https://git.acme.example/gitlab": "git.acme.example/gitlab/acme/api",
	} {
		read := acme().read(t, func(c *config.Company) { c.Integrations.GitLab.URL = web })
		gitlet := atlas.RecordSource(atlas.RecordGitlet)
		mirror := atlas.MustRef(atlas.KindRepository, "gitlet/api")
		exists, err := atlas.Exists(mirror, gitlet, atlasNow)
		if err != nil {
			t.Fatal(err)
		}
		from, err := atlas.Says(mirror, atlas.FactPullMirrorOf, web+"/Acme/API.git", gitlet, atlasNow)
		if err != nil {
			t.Fatal(err)
		}
		company, err := atlas.Says(mirror, atlas.FactVisibility, "company", gitlet, atlasNow)
		if err != nil {
			t.Fatal(err)
		}
		v := atlasView(append(slices.Clone(read.Claims), exists, from, company))
		here, ok := v.Find(mirror.ID())
		there, found := v.Find(atlas.MustRef(atlas.KindRepository, key).ID())
		if !ok || !found || here.ID != there.ID || len(here.Members) != 2 {
			t.Fatalf("%s: the mirror and its source are %+v and %+v", web, here.Members, there.Members)
		}
		seen := map[string]string{}
		for _, f := range here.Facts {
			if f.Fact == atlas.FactVisibility {
				seen[f.Of.Key] = f.Value.Text
			}
		}
		if here.Review || len(seen) != 2 || seen[key] != "private" || seen["gitlet/api"] != "company" {
			t.Errorf("%s: the mirror's visibilities: review %v, %v", web, here.Review, seen)
		}
		repositories := 0
		for _, e := range v.Entries {
			if e.Ref.Kind == atlas.KindRepository {
				repositories++
			}
		}
		if repositories != 5 {
			t.Errorf("%s: the atlas shows %d repositories, want the five GitLab lists", web, repositories)
		}
	}
}

// A PART GITLAB REFUSES IS A FINDING AND RETIRES NOTHING: a project whose
// pipelines the credential may not read leaves that part unread and reads
// the rest, as does one whose environments it may not, and neither withdraws,
// misses or retires what an earlier read recorded, however long it lasts.
//
// Mutation: read a refused part as an empty listing, and a changed role
// retires every pipeline a day later.
func TestAPartGitLabRefusesIsAFindingAndRetiresNothing(t *testing.T) {
	t.Parallel()
	whole := acme().read(t)
	h := acme()
	h.refuse["/api/v4/projects/1/pipelines"] = http.StatusForbidden
	h.refuse["/api/v4/projects/2/pipelines"] = http.StatusForbidden
	h.refuse["/api/v4/projects/1/environments/30"] = http.StatusNotFound
	read := h.read(t)
	if len(read.Unread) != 2 || read.Unread[0].Part != "pipelines" || read.Unread[1].Part != "environments" ||
		!strings.HasPrefix(read.Unread[0].Detail, "The pipelines of 2 projects") ||
		!strings.Contains(read.Unread[0].Detail, "Reporter") ||
		!strings.Contains(read.Unread[0].Detail, "integrations.gitlab.token") ||
		!strings.HasPrefix(read.Unread[1].Detail, "The environments of 1 projects") {
		t.Fatalf("the refused parts: %+v", read.Unread)
	}
	if strings.Contains(read.Unread[0].Detail, canaryText) {
		t.Errorf("the finding repeats the vendor's answer: %q", read.Unread[0].Detail)
	}
	v := atlasView(read.Claims)
	if e := atlasEntry(t, v, atlas.KindRepository, apiKey); atlasFact(e, atlas.FactStatus) != "active" {
		t.Errorf("the repositories were not read beside the refused parts: %+v", e.Facts)
	}
	// A refusal part way through a project's environments leaves no half.
	if _, ok := v.Find(atlas.MustRef(atlas.KindPipeline, euKey).ID()); ok {
		t.Error("an environment was claimed from a part GitLab refused part way")
	}
	status := collect.Status{Kind: integration.KindGitLab}
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
	first := collect.Decide(whole.Claims, empty, collect.Status{Kind: integration.KindGitLab}, atlasNow.Add(time.Hour))
	if plan := collect.Decide(whole.Claims, empty, first.Status, atlasNow.Add(26*time.Hour)); plan.Status.Retired != 4 {
		t.Errorf("the control retired %d pipelines, want 4", plan.Status.Retired)
	}
}

// A GROUP OR A NAMED PROJECT THE CREDENTIAL CANNOT SEE CARRIES WHAT AN
// EARLIER READ RECORDED: a group GitLab does not show is a finding and a
// read of the account alone, a project the settings name that GitLab does
// not show is a finding beside the projects it does, and neither retires
// anything.
//
// Mutation: fail the read on a group that is not shown, and the source shows
// a fault where a membership is what is missing; read a hidden project as
// absent, and it is retired a day after somebody leaves it.
func TestAGroupOrANamedProjectTheCredentialCannotSeeCarriesWhatWasRead(t *testing.T) {
	t.Parallel()
	extra := func(c *config.Company) {
		c.Integrations.GitLab.Provisioning.Projects = []string{"partners/shared", "API", "acme/platform/web"}
	}
	shared := &glProject{row: map[string]any{"id": 8, "path_with_namespace": "partners/shared",
		"default_branch": "main", "visibility": "private", "listed": false},
		pipelines: []map[string]any{{"id": 5, "status": "success"}}}
	both := acme()
	both.projects = append(both.projects, shared)
	whole := both.read(t, extra)
	v := atlasView(whole.Claims)
	atlasEntry(t, v, atlas.KindPipeline, "gitlab.com/partners/shared#pipeline")
	// A project the group lists is read once, however the settings spell it.
	if both.sent("GET /api/v4/projects/partners%2Fshared") != 1 || whole.Calls != 11 {
		t.Errorf("the named projects cost %d calls: %v", whole.Calls, both.calls)
	}

	hidden := acme()
	read := hidden.read(t, extra)
	if len(read.Unread) != 1 || read.Unread[0].Part != "projects" ||
		!strings.HasPrefix(read.Unread[0].Detail, "1 of the projects") {
		t.Fatalf("the project GitLab does not show: %+v", read.Unread)
	}
	gone := acme()
	gone.group = "elsewhere"
	none := gone.read(t, extra)
	if len(none.Unread) != 1 || none.Unread[0].Part != "group" || len(none.Claims) != 1 {
		t.Fatalf("a read of a group GitLab does not show holds %+v and %+v", none.Claims, none.Unread)
	}
	for name, read := range map[string]collect.Read{"a hidden project": read, "a hidden group": none} {
		status := collect.Status{Kind: integration.KindGitLab}
		for _, at := range []time.Time{atlasNow.Add(time.Hour), atlasNow.Add(30 * time.Hour)} {
			plan := collect.Decide(whole.Claims, read, status, at)
			if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 {
				t.Fatalf("%s: at %s recorded %+v and missed %+v", name, at, plan.Records, plan.Status.Missing)
			}
			status = plan.Status
		}
	}
}

// AN ENVIRONMENT THAT IS STOPPED RETIRES ITSELF AND NOTHING IT SERVED: after
// two complete reads a day apart the environment is denied, and the name and
// the address it served have that relation withdrawn, with no denial of
// either.
//
// Mutation: claim the served name with Ref rather than Names, and the read
// denies a name the DNS provider still lists.
func TestAnEnvironmentThatIsStoppedRetiresNothingItServed(t *testing.T) {
	t.Parallel()
	whole := acme().read(t)
	h := acme()
	h.projects[0].envs[2]["state"] = "stopped"
	read := h.read(t)
	first := collect.Decide(whole.Claims, read, collect.Status{Kind: integration.KindGitLab}, atlasNow.Add(time.Hour))
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
		switch _, served := named[rec.Entry]; {
		case rec.Entry == environment:
			if !rec.Retires || len(rec.Claims) != 1 || !rec.Claims[0].Denied {
				t.Errorf("the environment that is stopped: %+v", rec)
			}
		case served:
			named[rec.Entry] = true
			if rec.Retires || len(rec.Claims) != 0 || len(rec.Withdrawn) != 1 {
				t.Errorf("what the environment served: %+v", rec)
			}
		default:
			t.Errorf("an environment that is stopped recorded %s", rec.Entry)
		}
	}
	for ref, released := range named {
		if !released {
			t.Errorf("%s was not released", ref)
		}
	}
	if second.Status.Retired != 1 {
		t.Errorf("retired %d entries, want the environment alone", second.Status.Retired)
	}
}

// NO CREDENTIAL SHAPE REACHES THE ATLAS FROM GITLAB: every shape the
// redaction pass knows, planted in every field the read maps and in the ones
// it never reads, leaves no piece of itself in the read, and what the mapped
// fields refused is counted.
//
// Mutation: map a project's runners_token or a deployment's user into a
// claim, cut a name before the builder judges it, or skip the builder's
// grammar on a key or a value, and a shape reaches the read.
func TestNoCredentialShapeReachesTheAtlasFromGitLab(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		// A name long enough that cutting it to a label's length would cut
		// the shape short of its end, where no rule knows it any more.
		long := strings.Repeat("m", max(1, atlas.MaxLabelRunes-len(s)+len(s)/4)) + " " + s
		who := map[string]any{"username": s, "email": s, "name": s, "avatar_url": s}
		last := func(status string) map[string]any {
			return map[string]any{"status": status, "user": who, "ref": s, "sha": s,
				"deployable": map[string]any{"name": s, "user": who, "commit": map[string]any{"message": s}}}
		}
		h := &hub{token: atlasReadToken, group: "acme", refuse: map[string]int{}, projects: []*glProject{
			{row: map[string]any{"id": 1, "path_with_namespace": "acme/" + s, "default_branch": s, "visibility": s,
				"description": s, "runners_token": s, "import_url": "https://x:" + s + "@git.acme.example/a.git",
				"owner": who, "topics": []string{s}, "service_desk_address": s, "name": s, "web_url": s}},
			{
				row: map[string]any{"id": 2, "path_with_namespace": "acme/api", "default_branch": "main",
					"visibility": "private", "description": s, "runners_token": s, "owner": who,
					"ci_config_path": s, "name_with_namespace": s},
				pipelines: []map[string]any{{"id": 1, "status": s, "user": who, "name": s, "sha": s, "web_url": s}},
				envs: []map[string]any{
					{"id": 30, "name": s, "tier": s, "external_url": s, "description": s, "slug": s,
						"last_deployment": last(s)},
					{"id": 29, "name": long, "tier": "other", "last_deployment": last("success")},
					{"id": 28, "name": "production", "tier": "production", "last_deployment": last("success"),
						"external_url": "https://ops:" + s + "@www.acme.example/ok?token=" + s + "#" + s},
					{"id": 27, "name": "staging", "tier": "staging", "last_deployment": last("success"),
						"external_url": "https://" + s + ".acme.example/"},
					{"id": 26, "name": "preview", "tier": "development", "last_deployment": last("success"),
						"external_url": "https://api.acme.example/" + s},
				},
			},
		}}
		read := h.read(t)
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
		// A project's path, two environments' names and three served
		// addresses are each refused where they sat.
		if len(read.Withheld) < 6 {
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
// any reason but a refusal, GitLab's rate limit and a credential that stops
// working part way included, fails the whole read, so nothing is retired on
// a listing cut short. A credential GitLab refuses at the group is a rejected
// credential, never a missing permission.
//
// Mutation: read a rate-limited listing as a refusal, and a busy hour is
// recorded as a missing role.
func TestAListingThatFailsOrIsRateLimitedFailsTheGitLabRead(t *testing.T) {
	t.Parallel()
	for name, fault := range map[string]func(*hub){
		"a server fault":       func(h *hub) { h.refuse["/api/v4/projects/1/pipelines"] = http.StatusBadGateway },
		"the rate limit":       func(h *hub) { h.refuse["/api/v4/projects/1/environments/30"] = http.StatusTooManyRequests },
		"a dead credential":    func(h *hub) { h.refuse["/api/v4/projects/2/environments"] = http.StatusUnauthorized },
		"the projects":         func(h *hub) { h.refuse["/api/v4/groups/9/projects"] = http.StatusForbidden },
		"the group, limited":   func(h *hub) { h.refuse["/api/v4/groups/acme"] = http.StatusTooManyRequests },
		"the group, forbidden": func(h *hub) { h.refuse["/api/v4/groups/acme"] = http.StatusForbidden },
		"a rejected token":     func(h *hub) { h.token = "another" },
	} {
		h := acme()
		fault(h)
		read, err := gitlabSource(t).Collect(t.Context(), h.input(t))
		if err == nil || errors.Is(err, integration.ErrNotConfigured) || len(read.Claims) != 0 {
			t.Errorf("%s: the read answered %d claims and %v", name, len(read.Claims), err)
		}
		rejected := name == "a rejected token" || name == "the group, forbidden"
		if errors.Is(err, integration.ErrCredentialRejected) != rejected {
			t.Errorf("%s: a rejected credential is %v, want %v: %v", name, !rejected, rejected, err)
		}
	}
}

// A LISTING IS WALKED TO ITS BOUNDS: projects past a first page are read; of
// a project's environments the twenty nearest production are read, the same
// ones every time, and the rest counted; and a group past the ceiling is a
// finding that reads and retires nothing.
//
// Mutation: take a project's first twenty environments as GitLab lists them,
// and a project with a review environment per branch loses its production.
func TestTheGitLabListingsAreWalkedToTheirBounds(t *testing.T) {
	t.Parallel()
	h := acme()
	api := h.projects[0]
	api.envs = nil
	for i := range 130 {
		api.envs = append(api.envs, map[string]any{"id": 100 + i, "name": "review/b" + strconv.Itoa(1000+i),
			"tier": "development", "state": "available"})
	}
	api.envs = append(api.envs,
		map[string]any{"id": 300, "name": "Production", "tier": "production", "state": "available"},
		map[string]any{"id": 301, "name": "staging", "tier": "staging", "state": "available"})
	for i := range 120 {
		h.projects = append(h.projects, &glProject{row: map[string]any{"id": 10 + i,
			"path_with_namespace": "acme/r" + strconv.Itoa(i), "default_branch": "main", "archived": true}})
	}
	read := h.read(t)
	v := atlasView(read.Claims)
	repositories, environments := 0, 0
	for _, e := range v.Entries {
		switch {
		case e.Ref.Kind == atlas.KindRepository:
			repositories++
		case strings.Contains(e.Ref.Key, "#environment:"):
			environments++
		}
	}
	if repositories != 125 || environments != 20 || len(read.Withheld) != 112 {
		t.Errorf("the read listed %d repositories and %d environments and counted %d past the bound, "+
			"want 125, 20 and 112", repositories, environments, len(read.Withheld))
	}
	if h.sent("GET /api/v4/projects/1/environments") != 2 || h.sent("GET /api/v4/groups/9/projects") != 2 {
		t.Errorf("the listings were not walked to their ends: %v", h.calls[:8])
	}
	atlasEntry(t, v, atlas.KindPipeline, productionKey)
	atlasEntry(t, v, atlas.KindPipeline, apiKey+"#environment:staging")
	atlasEntry(t, v, atlas.KindPipeline, apiKey+"#environment:review~2fb1000")
	if _, ok := v.Find(atlas.MustRef(atlas.KindPipeline, apiKey+"#environment:review~2fb1018").ID()); ok {
		t.Error("an environment past the twenty nearest production was read")
	}

	whole := acme().read(t)
	for i := range 1000 {
		h.projects = append(h.projects, &glProject{row: map[string]any{"id": 1000 + i,
			"path_with_namespace": "acme/x" + strconv.Itoa(i), "archived": true}})
	}
	large := h.read(t)
	if len(large.Unread) != 1 || large.Unread[0].Part != "projects" || len(large.Claims) != 1 ||
		!strings.Contains(large.Unread[0].Detail, "1000") {
		t.Fatalf("a group past the ceiling: %d claims, %+v", len(large.Claims), large.Unread)
	}
	if plan := collect.Decide(whole.Claims, large, collect.Status{Kind: integration.KindGitLab}, atlasNow.Add(48*time.Hour)); len(plan.Records) != 0 || len(plan.Status.Missing) != 0 {
		t.Errorf("a group past the ceiling recorded %+v and missed %+v", plan.Records, plan.Status.Missing)
	}
}

// A COMPANY THAT DOES NOT CONNECT GITLAB, NAMES NO GROUP OR HOLDS NO
// CREDENTIAL TO READ WITH IS NOT CONFIGURED, which the duty reads as nothing
// to read rather than a fault.
func TestAnUnconnectedGitLabIsNotConfigured(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*config.Company){
		"no block":     func(c *config.Company) { c.Integrations.GitLab = nil },
		"switched off": func(c *config.Company) { c.Integrations.GitLab.Enabled = false },
		"no group":     func(c *config.Company) { c.Integrations.GitLab.Provisioning.Group = " " },
		"no provision": func(c *config.Company) { c.Integrations.GitLab.Provisioning = nil },
		"no credential": func(c *config.Company) {
			c.Integrations.GitLab.Token, c.Integrations.GitLab.Provisioning.AdminToken = "", "${GITLAB_UNSET}"
		},
	} {
		h := acme()
		if _, err := gitlabSource(t).Collect(t.Context(), h.input(t, edit)); !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: %v, want not configured", name, err)
		}
		if len(h.calls) != 0 {
			t.Errorf("%s: the read called GitLab: %v", name, h.calls)
		}
	}
	if _, err := gitlabSource(t).Collect(t.Context(), collect.Input{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no document: %v, want not configured", err)
	}
}
