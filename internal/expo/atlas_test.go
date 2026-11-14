package expo_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/setup"
)

// atlasNow is the instant every atlas read here is made at.
var atlasNow = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

// Values the fake carries in fields the read never asks for.
const (
	canaryText   = "atlas-canary-undecoded"
	canarySecret = "atlas-canary-p4ssw0rd"
)

// The keys of the fixture's entries.
const (
	accountKey  = "expo/" + testAccountName
	shopProject = "expo.dev/" + testAccountName + "/shop"
)

// atlasProject is one project as Expo answers it, with what a wider query
// would also be sent.
func atlasProject(id, slug, name string, builds map[string]string, repo ...string) map[string]any {
	p := map[string]any{"id": id, "slug": slug, "name": name, "fullName": "@" + testAccountName + "/" + slug,
		"description": canaryText, "android": []any{}, "ios": []any{},
		"environmentSecrets": []any{map[string]any{"name": "API_KEY", "value": canarySecret}},
	}
	for platform, status := range builds {
		p[strings.ToLower(platform)] = []any{map[string]any{"platform": platform, "status": status,
			"message": canaryText, "artifacts": map[string]any{"buildUrl": "https://" + canaryText + ".example/a.ipa"}}}
	}
	if len(repo) == 2 {
		p["githubRepository"] = map[string]any{"githubRepositoryUrl": "https://" + canaryText + ".example",
			"metadata": map[string]any{"githubRepoOwnerName": repo[0], "githubRepoName": repo[1],
				"githubRepoDescription": canaryText}}
	}
	return p
}

// atlasWorld is the account as the atlas reads it: a Viewer robot made for
// the atlas, an agent's robot beside it, and three projects.
type atlasWorld struct {
	*fakeExpo
	atlas, agent *fakeToken
	atlasRobot   *fakeActor
}

func newAtlasWorld() *atlasWorld {
	f := newFakeExpo()
	robot, token := f.robot(expo.AtlasRobotName, expo.RoleViewOnly)
	_, agent := f.robot("crewlet-acme-nova", expo.RoleDeveloper)
	f.projects = []map[string]any{
		atlasProject("app-1", "shop", "Shop", map[string]string{"ANDROID": "FINISHED", "IOS": "ERRORED"}, "Acme", "shop-app"),
		atlasProject("app-2", "kiosk", "Kiosk [internal]", map[string]string{"IOS": "IN_PROGRESS"}),
		atlasProject("app-3", "docs", "Docs", nil),
	}
	return &atlasWorld{fakeExpo: f, atlas: token, agent: agent, atlasRobot: robot}
}

func (w *atlasWorld) input(t *testing.T, edit ...func(*config.Company, config.MapSource)) collect.Input {
	t.Helper()
	on := true
	c := &config.Company{Name: "Acme"}
	c.Integrations.Expo = &config.Expo{Enabled: true,
		Provisioning: &config.ExpoProvisioning{AccountName: testAccountName},
		Atlas:        &config.ExpoAtlas{Token: "${EXPO_ATLAS_TOKEN}"}}
	c.Roles = []config.Role{{Name: "Nova", Handle: "nova",
		Integrations: config.RoleIntegrations{Expo: &config.RoleExpo{Enabled: &on}}}}
	env := config.MapSource{"EXPO_ATLAS_TOKEN": w.atlas.value, "EXPO_TOKEN_NOVA": w.agent.value}
	for _, e := range edit {
		e(c, env)
	}
	return collect.Input{Config: c, Now: atlasNow, Env: config.NewResolver(env), HTTP: w.server(t)}
}

func expoSource(t *testing.T) collect.Source {
	t.Helper()
	src, ok := collect.Lookup(integration.KindExpo)
	if !ok {
		t.Fatal("no Expo source is registered")
	}
	return src
}

func (w *atlasWorld) read(t *testing.T) collect.Read {
	t.Helper()
	read, err := expoSource(t).Collect(t.Context(), w.input(t))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return read
}

// atlasView is claims as the atlas folds them.
func atlasView(claims []atlas.Claim) atlas.View {
	return atlas.Join(atlas.Fold(claims, atlasNow), atlas.Joined{}, atlasNow)
}

func atlasEntry(t *testing.T, v atlas.View, k atlas.Kind, key string) atlas.Entry {
	t.Helper()
	e, ok := v.Find(atlas.MustRef(k, key).ID())
	if !ok {
		t.Fatalf("%s %s is not in the read", k, key)
	}
	return e
}

func atlasFact(e atlas.Entry, f atlas.Fact) string {
	for _, fv := range e.Facts {
		if fv.Fact == f {
			return fv.Value.Text
		}
	}
	return ""
}

// relates reports whether e relies through rel on the entry k and key name.
func relates(e atlas.Entry, rel atlas.Relation, v atlas.View, k atlas.Kind, key string) bool {
	to, ok := v.Find(atlas.MustRef(k, key).ID())
	return ok && slices.ContainsFunc(e.Relations, func(ed atlas.Edge) bool {
		return ed.Relation == rel && ed.To == to.ID
	})
}

// asked is every field name a query asks for, its arguments and enum values
// aside.
var asked = regexp.MustCompile(`\([^)]*\)|[A-Za-z_]+`)

// THE READ CLAIMS THE ACCOUNT'S PROJECTS AND THE LATEST BUILD OF EACH ON
// EACH PLATFORM: a project is the service its slug names, built from the
// repository linked to it, and each platform it was built for is a pipeline
// carrying that build's result. The one query names every field it is sent,
// none of them a secret, an environment value, a message or an artifact.
//
// Mutation: ask a build for its message or a project for its environment,
// and the query's field list no longer matches; decode one into a claim, and
// a canary reaches the read.
func TestTheAtlasReadsTheAccountsProjectsAndBuilds(t *testing.T) {
	t.Parallel()
	w := newAtlasWorld()
	read := w.read(t)
	w.onlyExpo(t)
	if got := w.operations(); !slices.Equal(got, []string{"CrewletViewer", "CrewletRole", "CrewletAtlasProjects"}) ||
		read.Calls != 3 || len(w.writes()) != 0 {
		t.Fatalf("the read made %v and counted %d calls", got, read.Calls)
	}
	var fields []string
	for _, m := range asked.FindAllString(strings.SplitN(w.queries["CrewletAtlasProjects"], "{", 2)[1], -1) {
		if !strings.HasPrefix(m, "(") && !slices.Contains(fields, m) {
			fields = append(fields, m)
		}
	}
	slices.Sort(fields)
	if want := []string{"account", "android", "appsPaginated", "builds", "byName", "edges", "endCursor",
		"githubRepoName", "githubRepoOwnerName", "githubRepository", "hasNextPage", "id", "ios", "metadata",
		"name", "node", "pageInfo", "platform", "slug", "status"}; !slices.Equal(fields, want) {
		t.Fatalf("the listing asks for %v, want exactly %v", fields, want)
	}
	if len(read.Withheld) != 0 || len(read.Unread) != 0 {
		t.Fatalf("withheld %+v, unread %+v", read.Withheld, read.Unread)
	}
	v := atlasView(read.Claims)
	account := atlasEntry(t, v, atlas.KindAccount, accountKey)
	if atlasFact(account, atlas.FactAccountName) != testAccountName {
		t.Errorf("the account: %+v", account.Facts)
	}
	shop := atlasEntry(t, v, atlas.KindService, "shop")
	if shop.Name != "Shop" || !relates(shop, atlas.RelationBelongsTo, v, atlas.KindAccount, accountKey) ||
		!relates(shop, atlas.RelationDependsOn, v, atlas.KindAccount, accountKey) {
		t.Errorf("the project: %q %+v", shop.Name, shop.Relations)
	}
	android := atlasEntry(t, v, atlas.KindPipeline, shopProject+"#android")
	ios := atlasEntry(t, v, atlas.KindPipeline, shopProject+"#ios")
	if android.Name != "Shop Android build" || atlasFact(android, atlas.FactLastResult) != atlas.ResultPassed ||
		atlasFact(android, atlas.FactRunner) != "external" ||
		ios.Name != "Shop iOS build" || atlasFact(ios, atlas.FactLastResult) != atlas.ResultFailed ||
		!relates(shop, atlas.RelationDeployedBy, v, atlas.KindPipeline, shopProject+"#android") ||
		!relates(shop, atlas.RelationDeployedBy, v, atlas.KindPipeline, shopProject+"#ios") ||
		!relates(ios, atlas.RelationBelongsTo, v, atlas.KindAccount, accountKey) {
		t.Errorf("the builds: android %q %+v %+v, ios %q %+v", android.Name, android.Facts, android.Relations,
			ios.Name, ios.Facts)
	}
	// A repository and a project's own address are named, never claimed.
	var built []string
	for _, c := range read.Claims {
		if c.Subject.Kind == atlas.KindRepository {
			t.Errorf("the read claims a repository: %+v", c)
		}
		if c.Facet.Relation == atlas.RelationBuiltFrom {
			built = append(built, c.Subject.Key+" from "+c.Facet.Target.Entry.Key)
		}
	}
	slices.Sort(built)
	if want := []string{shopProject + "#android from github.com/acme/shop-app",
		shopProject + "#ios from github.com/acme/shop-app", "shop from github.com/acme/shop-app"}; !slices.Equal(built, want) {
		t.Errorf("built from: %v", built)
	}
	// A project built for one platform has one pipeline, and one never built none.
	kiosk := atlasEntry(t, v, atlas.KindService, "kiosk")
	running := atlasEntry(t, v, atlas.KindPipeline, "expo.dev/"+testAccountName+"/kiosk#ios")
	if kiosk.Name != "Kiosk (internal)" || atlasFact(running, atlas.FactLastResult) != atlas.ResultRunning {
		t.Errorf("the kiosk: %q, its build %+v", kiosk.Name, running.Facts)
	}
	pipelines := 0
	for _, e := range v.Entries {
		if e.Ref.Kind == atlas.KindPipeline {
			pipelines++
		}
	}
	if atlasEntry(t, v, atlas.KindService, "docs"); pipelines != 3 {
		t.Errorf("%d pipelines, want the three platforms that were built", pipelines)
	}
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{canaryText, canarySecret, w.atlas.value, "owner@example.com"} {
		if strings.Contains(string(out), canary) {
			t.Errorf("%q reached the read", canary)
		}
	}
}

// A ROBOT THAT CAN CHANGE ANYTHING READS NOTHING: the atlas proves before
// every read that its token is a robot's and that the robot is a Viewer on
// the account, and anything else fails the read before the projects are
// asked for. So does a token Expo no longer knows.
//
// Mutation: read with whatever token is sealed, and a Developer robot, or a
// person's own token, lists the account under the atlas's name.
func TestARobotThatIsNoViewerReadsNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		edit func(*atlasWorld, config.MapSource)
		says string
		dead bool
	}{
		"a developer": {edit: func(w *atlasWorld, _ config.MapSource) { w.setRole(w.atlasRobot.id, expo.RoleDeveloper) },
			says: "holds the Developer role"},
		"a runner": {edit: func(w *atlasWorld, _ config.MapSource) { w.setRole(w.atlasRobot.id, expo.RoleRunner) },
			says: "holds the Runner role"},
		"an admin": {edit: func(w *atlasWorld, _ config.MapSource) { w.setRole(w.atlasRobot.id, expo.RoleAdmin) },
			says: "holds the Admin role"},
		"a person": {edit: func(w *atlasWorld, env config.MapSource) {
			env["EXPO_ATLAS_TOKEN"] = w.person("Sam Example", expo.RoleViewOnly).value
		}, says: "belongs to the person"},
		"no role here": {edit: func(w *atlasWorld, _ config.MapSource) { delete(w.atlasRobot.roles, testAccountName) },
			says: "holds no role"},
		"a revoked token": {edit: func(w *atlasWorld, _ config.MapSource) { w.revoke(w.atlas) },
			says: "did not accept", dead: true},
	} {
		w := newAtlasWorld()
		in := w.input(t, func(_ *config.Company, env config.MapSource) { tc.edit(w, env) })
		read, err := expoSource(t).Collect(t.Context(), in)
		if err == nil || errors.Is(err, integration.ErrNotConfigured) || len(read.Claims) != 0 ||
			!strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: the read answered %d claims and %v", name, len(read.Claims), err)
		}
		if errors.Is(err, integration.ErrCredentialRejected) != tc.dead {
			t.Errorf("%s: a rejected credential is %v, want %v: %v", name, !tc.dead, tc.dead, err)
		}
		if slices.Contains(w.operations(), "CrewletAtlasProjects") {
			t.Errorf("%s: the refused token listed the projects", name)
		}
	}
}

// THE ATLAS NEVER READS AS AN AGENT'S ROBOT: a robot holding a token sealed
// for a seat is refused as the atlas's, whichever of its tokens was pasted,
// so Expo's own log never shows an agent making the atlas's reads.
//
// Mutation: compare the pasted value alone, and a second token made on an
// agent's Viewer robot reads the account hourly as the agent.
func TestAnAgentsRobotIsNeverTheAtlass(t *testing.T) {
	t.Parallel()
	w := newAtlasWorld()
	agentRobot := w.tokens[w.agent.value].owner
	w.setRole(agentRobot, expo.RoleViewOnly)
	second := w.mint(agentRobot)
	in := w.input(t, func(_ *config.Company, env config.MapSource) { env["EXPO_ATLAS_TOKEN"] = second.value })
	_, err := expoSource(t).Collect(t.Context(), in)
	if err == nil || !strings.Contains(err.Error(), "nova's own robot") ||
		slices.Contains(w.operations(), "CrewletAtlasProjects") {
		t.Fatalf("an agent's robot read as the atlas: %v, after %v", err, w.operations())
	}
}

// NO CREDENTIAL'S SHAPE REACHES THE ATLAS FROM EXPO, whichever field a
// person typed it into: a slug, a name or a repository shaped like one is
// withheld where it sat, and what sat beside it is still read.
//
// Mutation: skip the builder's grammar on a key or a value, and a shape
// reaches the read.
func TestNoCredentialShapeReachesTheAtlasFromExpo(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		long := strings.Repeat("m", max(1, atlas.MaxLabelRunes-len(s)+len(s)/4)) + " " + s
		w := newAtlasWorld()
		w.projects = append(w.projects,
			atlasProject("app-4", s, s, map[string]string{"ANDROID": "FINISHED"}, s, s),
			atlasProject("app-5", "named", long, map[string]string{"IOS": s}, "acme", s),
			atlasProject(s, "beside", "Beside", map[string]string{"ANDROID": "FINISHED"}))
		read := w.read(t)
		out, err := json.Marshal(read)
		if err != nil {
			t.Fatal(err)
		}
		for _, piece := range []string{s[len(s)/2:], s[len(s)/4 : len(s)/4+12]} {
			if strings.Contains(string(out), piece) {
				at := strings.Index(string(out), piece)
				t.Errorf("%s: a piece of the shape reached the read near %q", shape.Name,
					string(out)[max(0, at-120):at])
			}
		}
		// The slug, the long name and the repository are refused where they sat.
		if len(read.Withheld) < 3 {
			t.Errorf("%s: %d values withheld, want every planted mapped field: %+v", shape.Name,
				len(read.Withheld), read.Withheld)
		}
		v := atlasView(read.Claims)
		if beside := atlasEntry(t, v, atlas.KindService, "beside"); beside.Name != "Beside" {
			t.Errorf("%s: the project beside the credential was not read: %+v", shape.Name, beside)
		}
		// A project whose name was refused is still an entry, by its slug.
		if named := atlasEntry(t, v, atlas.KindService, "named"); named.Name != "named" {
			t.Errorf("%s: the project whose name was refused is named %q", shape.Name, named.Name)
		}
	}
}

// A LISTING THAT DID NOT FINISH IS NO READ AT ALL: a fault on any page fails
// the read whole, so no project is retired on a listing cut short, and a
// listing longer than a page is read to its end.
//
// Mutation: stop at the first page, and an account's fifty-first project is
// retired a day later.
func TestAProjectListingIsReadToItsEndOrNotAtAll(t *testing.T) {
	t.Parallel()
	down := newAtlasWorld()
	down.refuse["CrewletAtlasProjects"] = fakeRefusal{status: 502, message: "bad gateway"}
	if read, err := expoSource(t).Collect(t.Context(), down.input(t)); err == nil || len(read.Claims) != 0 ||
		errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("a failed listing answered %d claims and %v", len(read.Claims), err)
	}
	paged := newAtlasWorld()
	paged.pageCap = 2
	for i := range 4 {
		paged.projects = append(paged.projects, atlasProject("app-9"+strconv.Itoa(i), "extra-"+strconv.Itoa(i), "Extra", nil))
	}
	read := paged.read(t)
	v := atlasView(read.Claims)
	for i := range 4 {
		atlasEntry(t, v, atlas.KindService, "extra-"+strconv.Itoa(i))
	}
	// Seven projects two a page are four pages, after the two that prove the robot.
	if read.Calls != 6 {
		t.Errorf("the read made %d calls, want 6", read.Calls)
	}
}

// A COMPANY THAT DOES NOT CONNECT EXPO, OR GAVE THE ATLAS NO TOKEN, IS NOT
// CONFIGURED, which the duty reads as nothing to read rather than a fault:
// the token is optional, and nothing is asked of Expo without it.
func TestAnExpoWithNoAtlasTokenIsNotConfigured(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*config.Company, config.MapSource){
		"no block":     func(c *config.Company, _ config.MapSource) { c.Integrations.Expo = nil },
		"switched off": func(c *config.Company, _ config.MapSource) { c.Integrations.Expo.Enabled = false },
		"no account":   func(c *config.Company, _ config.MapSource) { c.Integrations.Expo.Provisioning = nil },
		"no atlas":     func(c *config.Company, _ config.MapSource) { c.Integrations.Expo.Atlas = nil },
		"no token":     func(_ *config.Company, env config.MapSource) { delete(env, "EXPO_ATLAS_TOKEN") },
	} {
		w := newAtlasWorld()
		if _, err := expoSource(t).Collect(t.Context(), w.input(t, edit)); !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: %v, want not configured", name, err)
		}
		if len(w.operations()) != 0 {
			t.Errorf("%s: the read called Expo: %v", name, w.operations())
		}
	}
	if _, err := expoSource(t).Collect(t.Context(), collect.Input{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no document: %v, want not configured", err)
	}
}

// THE CONNECT FORM OFFERS THE ATLAS'S ROBOT AS AN OPTION AND TESTS ITS TOKEN
// AS THE READ PROVES IT: a Viewer robot works, and any other is refused with
// what to do. Nothing else of the form is a credential.
func TestTheConnectFormTestsTheAtlasRobot(t *testing.T) {
	t.Parallel()
	none := func(string) (string, bool) { return "", false }
	var field *setup.Requirement
	for _, r := range expo.Requirements(nil, none) {
		if r.Kind == setup.KindSecret || r.Checkable {
			if r.Field != expo.AtlasField || r.Required || r.Connect || !strings.HasPrefix(r.Hint, "See more in Atlas") {
				t.Errorf("%s = %+v", r.Field, r)
			}
			field = &r
		}
	}
	if field == nil || field.ConfigPath != "integrations.expo.atlas.token" {
		t.Fatalf("the form offers no atlas token: %+v", field)
	}
	check := func(w *atlasWorld, token string) setup.Verdict {
		values := map[string]string{expo.AtlasField: token, "account_name": testAccountName}
		v, err := expo.Probe(w.server(t))(context.Background(), setup.Check{Field: expo.AtlasField,
			Value: func(field string) string { return values[field] }})
		if err != nil {
			t.Fatalf("the probe: %v", err)
		}
		return v
	}
	w := newAtlasWorld()
	if v := check(w, w.atlas.value); v.Outcome != setup.VerifyWorks || len(v.Warnings) != 0 {
		t.Errorf("a Viewer robot: %+v", v)
	}
	if v := check(w, w.agent.value); v.Outcome != setup.VerifyRefused || !strings.Contains(v.Summary, "Developer") ||
		strings.Contains(v.Summary, w.agent.value) {
		t.Errorf("a Developer robot: %+v", v)
	}
	if len(w.writes()) != 0 || slices.Contains(w.operations(), "CrewletAtlasProjects") {
		t.Errorf("the check did more than prove the robot: %v", w.operations())
	}
}
