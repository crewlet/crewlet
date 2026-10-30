package gitlab_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// glSite is a stand-in GitLab holding accounts, memberships and tokens, and
// answering exactly the routes per-seat access uses.
type glSite struct {
	server *httptest.Server
	mu     sync.Mutex

	admin    bool // the provisioning token is an instance administrator's
	users    map[int]*glUser
	group    map[int]gitlab.Access
	projects map[string]map[int]gitlab.Access
	tokens   map[int][]*glToken
	roles    string // the custom-role listing's body; empty is a 404
	refuse   map[string]glRefusal
	// unusable makes a freshly minted token fail its probe.
	unusable bool

	calls   []string
	nextID  int
	nextTok int
}

type glUser struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	State    string `json:"state"`
}

type glToken struct {
	id      int
	name    string
	scopes  []string
	revoked bool
	value   string
	owner   int
}

type glRefusal struct {
	status  int
	message string
}

const glGroupID = 9

func newGLSite(t *testing.T) *glSite {
	t.Helper()
	g := &glSite{
		users: map[int]*glUser{}, group: map[int]gitlab.Access{},
		projects: map[string]map[int]gitlab.Access{"acme/api": {}, "acme/infra": {}},
		tokens:   map[int][]*glToken{}, refuse: map[string]glRefusal{},
		nextID: 100, nextTok: 1,
	}
	g.server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.server.Close)
	return g
}

var (
	reUser         = regexp.MustCompile(`^/users/(\d+)$`)
	reBlock        = regexp.MustCompile(`^/users/(\d+)/(block|unblock)$`)
	reGroupMember  = regexp.MustCompile(`^/groups/9/members/(\d+)$`)
	reProjMembers  = regexp.MustCompile(`^/projects/([^/]+/[^/]+)/members$`)
	reProjMember   = regexp.MustCompile(`^/projects/([^/]+/[^/]+)/members/(\d+)$`)
	reGroupTokens  = regexp.MustCompile(`^/groups/9/service_accounts/(\d+)/personal_access_tokens$`)
	reGroupToken   = regexp.MustCompile(`^/groups/9/service_accounts/(\d+)/personal_access_tokens/(\d+)$`)
	reGroupAccount = regexp.MustCompile(`^/groups/9/service_accounts/(\d+)$`)
	reUserTokens   = regexp.MustCompile(`^/users/(\d+)/personal_access_tokens$`)
	reInstToken    = regexp.MustCompile(`^/personal_access_tokens/(\d+)$`)
	reInstAccount  = regexp.MustCompile(`^/service_accounts/(\d+)$`)
)

func (g *glSite) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4")
	path = strings.ReplaceAll(path, "%2F", "/")
	call := r.Method + " " + path
	g.calls = append(g.calls, call)
	if ref, ok := g.refuse[call]; ok {
		w.WriteHeader(ref.status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": ref.message})
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	reply := func(status int, v any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	missing := func() { reply(http.StatusNotFound, map[string]string{"message": "404 Not Found"}) }
	token := r.Header.Get("PRIVATE-TOKEN")

	switch {
	case call == "GET /groups/acme":
		reply(http.StatusOK, map[string]any{"id": glGroupID, "full_path": "acme"})
	case call == "GET /user":
		if token == "admin-token" {
			out := map[string]any{"id": 1, "username": "owner"}
			if g.admin {
				out["is_admin"] = true
			}
			reply(http.StatusOK, out)
			return
		}
		for _, list := range g.tokens {
			for _, t := range list {
				if t.value == token && !t.revoked && !g.unusable {
					reply(http.StatusOK, g.users[t.owner])
					return
				}
			}
		}
		reply(http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
	case call == "GET /users":
		var out []*glUser
		for _, u := range g.users {
			if u.Username == r.URL.Query().Get("username") {
				out = append(out, u)
			}
		}
		reply(http.StatusOK, out)
	case r.Method == http.MethodGet && reUser.MatchString(path):
		if u, ok := g.users[atoi(reUser.FindStringSubmatch(path)[1])]; ok {
			reply(http.StatusOK, u)
			return
		}
		missing()
	case call == "POST /groups/9/service_accounts" || call == "POST /service_accounts":
		g.nextID++
		u := &glUser{ID: g.nextID, Username: str(body["username"]), Name: str(body["name"]), State: "active"}
		g.users[u.ID] = u
		reply(http.StatusCreated, u)
	case r.Method == http.MethodPost && reBlock.MatchString(path):
		m := reBlock.FindStringSubmatch(path)
		if !g.admin {
			reply(http.StatusForbidden, map[string]string{"message": "403 Forbidden"})
			return
		}
		u, ok := g.users[atoi(m[1])]
		if !ok {
			missing()
			return
		}
		u.State = map[string]string{"block": "blocked", "unblock": "active"}[m[2]]
		reply(http.StatusCreated, true)
	case call == "GET /groups/9/members":
		reply(http.StatusOK, renderMembers(g.users, g.group))
	case call == "POST /groups/9/members":
		g.group[int(body["user_id"].(float64))] = accessOf(body)
		reply(http.StatusCreated, map[string]any{})
	case reGroupMember.MatchString(path):
		id := atoi(reGroupMember.FindStringSubmatch(path)[1])
		if _, ok := g.group[id]; !ok {
			missing()
			return
		}
		if r.Method == http.MethodDelete {
			delete(g.group, id)
		} else {
			g.group[id] = accessOf(body)
		}
		reply(http.StatusOK, map[string]any{})
	case reProjMembers.MatchString(path):
		project := reProjMembers.FindStringSubmatch(path)[1]
		members, ok := g.projects[project]
		if !ok {
			missing()
			return
		}
		if r.Method == http.MethodGet {
			reply(http.StatusOK, renderMembers(g.users, members))
			return
		}
		members[int(body["user_id"].(float64))] = accessOf(body)
		reply(http.StatusCreated, map[string]any{})
	case reProjMember.MatchString(path):
		m := reProjMember.FindStringSubmatch(path)
		members := g.projects[m[1]]
		id := atoi(m[2])
		if _, ok := members[id]; !ok {
			missing()
			return
		}
		if r.Method == http.MethodDelete {
			delete(members, id)
		} else {
			members[id] = accessOf(body)
		}
		reply(http.StatusOK, map[string]any{})
	case call == "GET /groups/9/member_roles":
		if g.roles == "" {
			missing()
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(g.roles))
	case call == "GET /member_roles":
		missing()
	case call == "GET /personal_access_tokens" || reGroupTokens.MatchString(path) || reUserTokens.MatchString(path):
		var owner int
		switch {
		case call == "GET /personal_access_tokens":
			owner = atoi(r.URL.Query().Get("user_id"))
		case reGroupTokens.MatchString(path):
			owner = atoi(reGroupTokens.FindStringSubmatch(path)[1])
		default:
			owner = atoi(reUserTokens.FindStringSubmatch(path)[1])
		}
		if r.Method == http.MethodGet {
			var out []map[string]any
			for _, t := range g.tokens[owner] {
				out = append(out, map[string]any{
					"id": t.id, "name": t.name, "revoked": t.revoked, "active": !t.revoked,
					"scopes": t.scopes,
				})
			}
			reply(http.StatusOK, out)
			return
		}
		var scopes []string
		for _, s := range body["scopes"].([]any) {
			scopes = append(scopes, s.(string))
		}
		t := &glToken{id: g.nextTok, name: str(body["name"]), scopes: scopes,
			value: fmt.Sprintf("glpat-fake-%d", g.nextTok), owner: owner}
		g.nextTok++
		g.tokens[owner] = append(g.tokens[owner], t)
		reply(http.StatusCreated, map[string]any{"id": t.id, "name": t.name, "token": t.value, "scopes": scopes})
	case (reGroupToken.MatchString(path) || reInstToken.MatchString(path)) && r.Method == http.MethodDelete:
		id := 0
		if m := reGroupToken.FindStringSubmatch(path); m != nil {
			id = atoi(m[2])
		} else {
			id = atoi(reInstToken.FindStringSubmatch(path)[1])
		}
		for _, list := range g.tokens {
			for _, t := range list {
				if t.id == id {
					t.revoked = true
					reply(http.StatusNoContent, nil)
					return
				}
			}
		}
		missing()
	case reInstAccount.MatchString(path) && r.Method == http.MethodDelete:
		id := atoi(reInstAccount.FindStringSubmatch(path)[1])
		if _, ok := g.users[id]; !ok {
			missing()
			return
		}
		delete(g.users, id)
		reply(http.StatusNoContent, nil)
	case reGroupAccount.MatchString(path) && r.Method == http.MethodDelete:
		id := atoi(reGroupAccount.FindStringSubmatch(path)[1])
		if _, ok := g.users[id]; !ok {
			missing()
			return
		}
		delete(g.users, id)
		delete(g.group, id)
		reply(http.StatusNoContent, nil)
	default:
		missing()
	}
}

func accessOf(body map[string]any) gitlab.Access {
	a := gitlab.Access{Level: int(body["access_level"].(float64))}
	if id, ok := body["member_role_id"].(float64); ok {
		a.MemberRoleID = int(id)
	}
	return a
}

func renderMembers(users map[int]*glUser, members map[int]gitlab.Access) []map[string]any {
	var out []map[string]any
	for id, access := range members {
		row := map[string]any{"id": id, "access_level": access.Level}
		if u, ok := users[id]; ok {
			row["username"] = u.Username
		}
		if access.MemberRoleID != 0 {
			row["member_role"] = map[string]any{"id": access.MemberRoleID}
		}
		out = append(out, row)
	}
	return out
}

func (g *glSite) liveTokens(owner int) []*glToken {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []*glToken
	for _, t := range g.tokens[owner] {
		if !t.revoked {
			out = append(out, t)
		}
	}
	return out
}

func (g *glSite) userNamed(username string) *glUser {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, u := range g.users {
		if u.Username == username {
			return u
		}
	}
	return nil
}

// glCompany is a company provisioning GitLab into group acme, with one agent
// seat whose block is given.
func glCompany(url string, projects []string, block *config.RoleGitLab) *config.Company {
	return &config.Company{
		Roles: []config.Role{{
			Name:         "SRE",
			Integrations: config.RoleIntegrations{GitLab: block},
		}},
		Integrations: config.Integrations{GitLab: &config.GitLab{
			Enabled: true, URL: url,
			Provisioning: &config.GitLabProvisioning{
				Group: "acme", AdminToken: "admin-token", Projects: projects,
			},
		}},
	}
}

func on() *bool { v := true; return &v }

func openGL(t *testing.T, g *glSite, projects []string) seataccess.Session {
	t.Helper()
	s, err := gitlab.Vendor{}.Open(context.Background(), seataccess.OpenInput{
		Config: glCompany(g.server.URL, projects, &config.RoleGitLab{Enabled: on(), AccessLevel: 20}),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

var sre = seataccess.SeatRef{Handle: "sre", Name: "SRE"}

// runOp takes every step an op draws, the way the sync does.
func runOp(t *testing.T, s seataccess.Session, op seataccess.Op, want seataccess.Desired,
	have seataccess.Observed, rec *seataccess.Record,
) []seataccess.StepResult {
	t.Helper()
	var out []seataccess.StepResult
	for _, step := range (gitlab.Vendor{}).Steps(sre, seataccess.LaneGitLab, op, want, have) {
		res, err := s.Apply(context.Background(), seataccess.ApplyInput{
			Seat: sre, Lane: seataccess.LaneGitLab, Step: step, Want: want, Have: have, Record: rec,
		})
		if err != nil {
			t.Fatalf("%s step %d (%s): %v", op, step.Index, step.Text, err)
		}
		out = append(out, res)
	}
	return out
}

func minted(results []seataccess.StepResult) []seataccess.Minted {
	var out []seataccess.Minted
	for _, r := range results {
		out = append(out, r.Minted...)
	}
	return out
}

func TestTheGitLabVendorIsRegisteredWithOneTokenAndOneLane(t *testing.T) {
	t.Parallel()
	v, ok := seataccess.Lookup(integration.KindGitLab)
	if !ok {
		t.Fatal("GitLab is not registered")
	}
	cat := v.Catalog()
	if !slices.Equal(cat.Credentials, []string{gitlab.FieldToken}) || len(cat.Lanes) != 1 ||
		cat.Lanes[0].Lane != seataccess.LaneGitLab || !cat.Lanes[0].Rotatable {
		t.Fatalf("catalog = %+v", cat)
	}
}

func TestAStoredLevelRoundTripsAndRefusesWhatGitLabIsNotOffered(t *testing.T) {
	t.Parallel()
	for _, a := range []gitlab.Access{{Level: 30}, {Level: 20, MemberRoleID: 7}} {
		level, role, err := gitlab.ParseLevel(gitlab.LevelID(a))
		if err != nil || (role == 0 && level != a.Level) || role != a.MemberRoleID {
			t.Errorf("%+v round trip = %d, %d, %v", a, level, role, err)
		}
	}
	for _, bad := range []string{"", "5", "25", "developer", "member_role:", "member_role:-1"} {
		if _, _, err := gitlab.ParseLevel(bad); err == nil {
			t.Errorf("ParseLevel(%q) accepted", bad)
		}
	}
}

// THE STEPS ARE DRAWN IN GITLAB'S WORDS, AND A ROLE CHANGE RE-MINTS ONLY WHERE
// THE SCOPES CHANGE: Reporter to Developer gains `api`, Developer to
// Maintainer does not.
func TestStepsAreDrawnInGitLabsWordsAndRemintOnlyWhenScopesChange(t *testing.T) {
	t.Parallel()
	v := gitlab.Vendor{}
	verbs := func(steps []seataccess.Step) []seataccess.StepVerb {
		var out []seataccess.StepVerb
		for i, s := range steps {
			if s.Index != i || s.By != seataccess.ByCrewlet {
				t.Fatalf("step %d = %+v", i, s)
			}
			if strings.Contains(s.Text+s.Note, "—") {
				t.Fatalf("step text carries an em dash: %q", s.Text+s.Note)
			}
			out = append(out, s.Verb)
		}
		return out
	}
	create := v.Steps(sre, seataccess.LaneGitLab, seataccess.OpCreate,
		seataccess.Desired{Enabled: true, Level: "20", Scope: []string{"acme/api"}}, seataccess.Observed{})
	if got := verbs(create); !slices.Equal(got, []seataccess.StepVerb{
		seataccess.VerbCreate, seataccess.VerbGrant, seataccess.VerbMint, seataccess.VerbRevoke}) {
		t.Fatalf("create = %v", got)
	}
	if !strings.Contains(create[1].Text, "acme/api as Reporter") ||
		!strings.Contains(create[2].Text, "read_api and read_repository") {
		t.Fatalf("create text = %q / %q", create[1].Text, create[2].Text)
	}
	raise := v.Steps(sre, seataccess.LaneGitLab, seataccess.OpLevel,
		seataccess.Desired{Enabled: true, Level: "30"}, seataccess.Observed{Level: "20"})
	if got := verbs(raise); !slices.Equal(got, []seataccess.StepVerb{
		seataccess.VerbRerole, seataccess.VerbMint, seataccess.VerbRevoke}) {
		t.Fatalf("Reporter to Developer = %v", got)
	}
	if !strings.Contains(raise[0].Text, "from Reporter to Developer") {
		t.Fatalf("rerole text = %q", raise[0].Text)
	}
	same := v.Steps(sre, seataccess.LaneGitLab, seataccess.OpLevel,
		seataccess.Desired{Enabled: true, Level: "40"}, seataccess.Observed{Level: "30"})
	if got := verbs(same); !slices.Equal(got, []seataccess.StepVerb{seataccess.VerbRerole}) {
		t.Fatalf("Developer to Maintainer = %v", got)
	}
	off := v.Steps(sre, seataccess.LaneGitLab, seataccess.OpDisable, seataccess.Desired{}, seataccess.Observed{})
	if got := verbs(off); !slices.Equal(got, []seataccess.StepVerb{seataccess.VerbDisable}) {
		t.Fatalf("disable = %v", got)
	}
	revoke := v.Steps(sre, seataccess.LaneGitLab, seataccess.OpRevoke, seataccess.Desired{}, seataccess.Observed{})
	if got := verbs(revoke); !slices.Equal(got, []seataccess.StepVerb{seataccess.VerbRevoke}) {
		t.Fatalf("revoke = %v", got)
	}
	if v.Steps(sre, seataccess.LaneJira, seataccess.OpCreate, seataccess.Desired{}, seataccess.Observed{}) != nil {
		t.Fatal("GitLab drew steps for another vendor's lane")
	}
}

func TestOpenRefusesACompanyThatDoesNotProvisionGitLab(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]*config.Company{
		"no document": nil,
		"no block":    {},
		"disabled": {Integrations: config.Integrations{GitLab: &config.GitLab{
			Provisioning: &config.GitLabProvisioning{Group: "acme", AdminToken: "x"}}}},
		"no admin token": {Integrations: config.Integrations{GitLab: &config.GitLab{
			Enabled: true, URL: "https://gitlab.example.com",
			Provisioning: &config.GitLabProvisioning{Group: "acme"}}}},
	} {
		_, err := gitlab.Vendor{}.Open(context.Background(), seataccess.OpenInput{Config: cfg})
		if !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: err = %v, want ErrNotConfigured", name, err)
		}
	}
}

// GITLAB'S ROLES, UNDER GITLAB'S NAMES, plus the custom roles read live, and
// the company's project list as the reach.
func TestOptionsAreGitLabsRolesPlusLiveCustomRoles(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	g.roles = `[{"id": 7, "name": "Incident responder", "base_access_level": 40}]`
	s := openGL(t, g, []string{"acme/api"})
	opts, err := s.Options(context.Background(), seataccess.LaneGitLab)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Live || len(opts.Levels) != 7 || !slices.Equal(opts.Scope, []string{"acme/api"}) || opts.ScopeOpen {
		t.Fatalf("options = %+v", opts)
	}
	maintainer, _, _ := opts.Level("40")
	reporter, _, _ := opts.Level("20")
	custom, _, ok := opts.Level("member_role:7")
	if maintainer.Name != "Maintainer" || !maintainer.Administers || reporter.Administers {
		t.Fatalf("default roles = %+v %+v", maintainer, reporter)
	}
	if !ok || !custom.Custom || !custom.Administers || custom.Name != "Incident responder" {
		t.Fatalf("custom role = %+v", custom)
	}
}

// AN ACCOUNT MADE BEFORE THE LEDGER IS ADOPTED, and the tokens an earlier
// engine minted on it are revoked once the new one is sealed.
func TestACreateAdoptsAnExistingAccountAndRetiresItsEarlierTokens(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	g.seed(&glUser{ID: 500, Username: "crewlet-sre", State: "active"})
	g.mu.Lock()
	g.tokens[500] = []*glToken{{id: 77, name: "crewlet-sre", owner: 500, value: "old", scopes: []string{"api"}}}
	// GitLab's token ids grow instance-wide, so the one minted next is
	// higher than any already there.
	g.nextTok = 78
	g.mu.Unlock()
	s := openGL(t, g, nil)
	results := runOp(t, s, seataccess.OpCreate, seataccess.Desired{Enabled: true, Level: "30"}, seataccess.Observed{}, nil)
	live := g.liveTokens(500)
	if results[0].AccountID != "500" || len(live) != 1 || live[0].id == 77 || g.called("POST /groups/9/service_accounts") {
		t.Fatalf("adoption: results %+v, live %+v", results, live)
	}
}

// THE WHOLE LIFE OF A SEAT: created narrowed to one project as Reporter with
// read scopes, raised to Developer (new token with api, old one revoked),
// widened to the whole group, turned off, turned back on, and removed. Each
// state is read back the way the next pass reads it.
func TestASeatsWholeLifeAtGitLab(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	g.admin = true
	s := openGL(t, g, []string{"acme/api", "acme/infra"})

	want := seataccess.Desired{Enabled: true, Level: "20", Scope: []string{"acme/api"}}
	results := runOp(t, s, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	user := g.userNamed("crewlet-sre")
	if user == nil || results[0].AccountID != strconv.Itoa(user.ID) || results[0].Account != "crewlet-sre" {
		t.Fatalf("create = %+v, user %+v", results[0], user)
	}
	if _, inGroup := g.inGroup(user.ID); inGroup || g.inProject("acme/api", user.ID).Level != 20 {
		t.Fatal("the seat is not a Reporter of acme/api alone")
	}
	mint := minted(results)
	live := g.liveTokens(user.ID)
	if len(mint) != 1 || mint[0].Field != gitlab.FieldToken || len(live) != 1 ||
		mint[0].Value != live[0].value || !slices.Equal(live[0].scopes, []string{"read_api", "read_repository"}) {
		t.Fatalf("minted %+v, live %+v", mint, live)
	}

	rec := &seataccess.Record{
		Kind: integration.KindGitLab, Handle: "sre",
		Account:    &seataccess.Account{ID: results[0].AccountID, State: seataccess.AccountActive},
		Credential: &seataccess.Credential{Variable: "GITLAB_TOKEN_SRE"},
		Lanes:      map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneGitLab: {Applied: &want}},
	}
	have := observeOne(t, s, rec)
	if !have.Exists || !have.Enabled || have.Level != "20" || !slices.Equal(have.Scope, []string{"acme/api"}) ||
		have.Credential == nil || have.Account != "crewlet-sre" {
		t.Fatalf("observed after create = %+v", have)
	}

	raised := seataccess.Desired{Enabled: true, Level: "30", Scope: []string{"acme/api"}}
	runOp(t, s, seataccess.OpLevel, raised, have, rec)
	live = g.liveTokens(user.ID)
	if g.inProject("acme/api", user.ID).Level != 30 || len(live) != 1 || !slices.Equal(live[0].scopes, []string{"api"}) {
		t.Fatalf("after the raise: live %+v", live)
	}

	whole := seataccess.Desired{Enabled: true, Level: "30"}
	runOp(t, s, seataccess.OpScope, whole, seataccess.Observed{Exists: true, Level: "30", Scope: []string{"acme/api"}}, rec)
	if access, _ := g.inGroup(user.ID); access.Level != 30 || g.projectSize("acme/api") != 0 {
		t.Fatal("widening did not move the seat to the whole group")
	}
	if have = observeOne(t, s, rec); len(have.Scope) != 0 || have.Level != "30" {
		t.Fatalf("observed whole group = %+v", have)
	}

	off := runOp(t, s, seataccess.OpDisable, seataccess.Desired{}, have, rec)
	off = append(off, runOp(t, s, seataccess.OpRevoke, seataccess.Desired{}, have, rec)...)
	if !off[0].Disabled || !slices.Equal(off[1].Revoked, []string{gitlab.FieldToken}) ||
		g.userNamed("crewlet-sre").State != "blocked" || len(g.liveTokens(user.ID)) != 0 {
		t.Fatalf("off = %+v, user %+v", off, g.userNamed("crewlet-sre"))
	}
	rec.Account.State = seataccess.AccountDisabled
	if have = observeOne(t, s, rec); have.Enabled || have.DisabledBy != seataccess.DisabledByCrewlet || have.Credential != nil {
		t.Fatalf("observed off = %+v", have)
	}

	back := runOp(t, s, seataccess.OpEnable, whole, have, rec)
	if g.userNamed("crewlet-sre").State != "active" || len(minted(back)) != 1 || len(g.liveTokens(user.ID)) != 1 {
		t.Fatalf("back on = %+v", back)
	}

	removed := runOp(t, s, seataccess.OpRemove, seataccess.Desired{}, have, rec)
	if !removed[0].Gone || g.userNamed("crewlet-sre") != nil {
		t.Fatalf("remove = %+v", removed)
	}
	if !g.called("DELETE /groups/9/service_accounts/" + strconv.Itoa(user.ID)) {
		t.Fatal("the group service-account route was not the one used")
	}
	if have = observeOne(t, s, rec); have.Exists {
		t.Fatalf("a removed account observed as %+v", have)
	}
}

func (g *glSite) inGroup(id int) (gitlab.Access, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.group[id]
	return a, ok
}

func (g *glSite) inProject(project string, id int) gitlab.Access {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.projects[project][id]
}

func (g *glSite) projectSize(project string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.projects[project])
}

func (g *glSite) seed(u *glUser) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.users[u.ID] = u
}

func (g *glSite) member(project string, id int, a gitlab.Access) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.projects[project][id] = a
}

func (g *glSite) called(call string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Contains(g.calls, call)
}

func observeOne(t *testing.T, s seataccess.Session, rec *seataccess.Record) seataccess.Observed {
	t.Helper()
	records := map[string]seataccess.Record{}
	if rec != nil {
		records["sre"] = *rec
	}
	obs, err := s.Observe(context.Background(), seataccess.ObserveInput{
		Seats: []seataccess.SeatRef{sre}, Records: records,
	})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return obs["sre"][seataccess.LaneGitLab]
}

// A GROUP OWNER CANNOT BLOCK, so off strips every membership and token, and
// the account reads as off by Crewlet only because the ledger says so.
func TestOffWithAGroupOwnerTokenStripsMembershipsAndReadsBackAsOffByCrewlet(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	s := openGL(t, g, []string{"acme/api"})
	want := seataccess.Desired{Enabled: true, Level: "30", Scope: []string{"acme/api"}}
	results := runOp(t, s, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	rec := &seataccess.Record{Account: &seataccess.Account{ID: results[0].AccountID, State: seataccess.AccountActive}}
	runOp(t, s, seataccess.OpDisable, seataccess.Desired{}, seataccess.Observed{Scope: want.Scope}, rec)
	runOp(t, s, seataccess.OpRevoke, seataccess.Desired{}, seataccess.Observed{Scope: want.Scope}, rec)
	id := atoi(results[0].AccountID)
	if g.userNamed("crewlet-sre").State != "active" || g.projectSize("acme/api") != 0 || len(g.liveTokens(id)) != 0 {
		t.Fatal("off with a group Owner token did not strip the memberships and tokens")
	}
	if g.called("POST /users/" + results[0].AccountID + "/block") {
		t.Fatal("a group Owner token was sent to the administrator's block route")
	}
	if have := observeOne(t, s, rec); !have.Enabled {
		t.Fatalf("with the ledger still active the account reads as %+v", have)
	}
	rec.Account.State = seataccess.AccountDisabled
	if have := observeOne(t, s, rec); have.Enabled || have.DisabledBy != seataccess.DisabledByCrewlet {
		t.Fatalf("observed off = %+v", have)
	}
}

// AN ACCOUNT BLOCKED BY SOMEBODY ELSE IS READ AS DISABLED BY HAND (D3), which
// the plan never turns back on unasked.
func TestABlockByHandReadsAsDisabledByHand(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	s := openGL(t, g, nil)
	g.seed(&glUser{ID: 500, Username: "crewlet-sre", State: "blocked"})
	rec := &seataccess.Record{Account: &seataccess.Account{ID: "500", State: seataccess.AccountActive}}
	if have := observeOne(t, s, rec); !have.Exists || have.Enabled || have.DisabledBy != seataccess.DisabledByHand {
		t.Fatalf("observed = %+v", have)
	}
}

// A ROLE CHANGED AT GITLAB IS READ BACK AS IT IS, so the plan restores it.
func TestDriftInRoleIsObserved(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	s := openGL(t, g, []string{"acme/api", "acme/infra"})
	g.seed(&glUser{ID: 500, Username: "crewlet-sre", State: "active"})
	g.member("acme/api", 500, gitlab.Access{Level: 20})
	g.member("acme/infra", 500, gitlab.Access{Level: 40})
	applied := seataccess.Desired{Enabled: true, Level: "20", Scope: []string{"acme/api", "acme/infra"}}
	rec := &seataccess.Record{
		Account: &seataccess.Account{ID: "500", State: seataccess.AccountActive},
		Lanes:   map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneGitLab: {Applied: &applied}},
	}
	if have := observeOne(t, s, rec); have.Level != "40" {
		t.Fatalf("observed level = %q, want the drifted Maintainer", have.Level)
	}
}

// A CUSTOM ROLE IS WRITTEN WITH ITS BASE LEVEL AND READ BACK BY ITS ID, and
// its token scopes follow the base level.
func TestACustomRoleIsHeldWithItsBaseLevel(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	g.roles = `[{"id": 7, "name": "Auditor", "base_access_level": 20}]`
	s := openGL(t, g, []string{"acme/api"})
	want := seataccess.Desired{Enabled: true, Level: "member_role:7", Scope: []string{"acme/api"}}
	results := runOp(t, s, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	id := atoi(results[0].AccountID)
	if got := g.inProject("acme/api", id); !got.Equal(gitlab.Access{Level: 20, MemberRoleID: 7}) {
		t.Fatalf("membership = %+v", got)
	}
	if live := g.liveTokens(id); !slices.Equal(live[0].scopes, []string{"read_api", "read_repository"}) {
		t.Fatalf("scopes = %v", live[0].scopes)
	}
	rec := &seataccess.Record{Account: &seataccess.Account{ID: results[0].AccountID, State: seataccess.AccountActive}}
	if have := observeOne(t, s, rec); have.Level != "member_role:7" {
		t.Fatalf("observed level = %q", have.Level)
	}
}

func TestACustomRoleGitLabNoLongerHasIsARefusal(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	s := openGL(t, g, []string{"acme/api"})
	want := seataccess.Desired{Enabled: true, Level: "member_role:7", Scope: []string{"acme/api"}}
	create := runOpUntil(t, s, seataccess.OpCreate, want, 1)
	var refusal *seataccess.Refusal
	if !errors.As(create, &refusal) || !strings.Contains(refusal.Said, "custom role 7") ||
		refusal.Actor != seataccess.ByYou {
		t.Fatalf("err = %v", create)
	}
}

// runOpUntil takes an op's steps through index last and returns that step's
// error.
func runOpUntil(t *testing.T, s seataccess.Session, op seataccess.Op, want seataccess.Desired, last int) error {
	t.Helper()
	steps := (gitlab.Vendor{}).Steps(sre, seataccess.LaneGitLab, op, want, seataccess.Observed{})
	for i, step := range steps[:last+1] {
		_, err := s.Apply(context.Background(), seataccess.ApplyInput{
			Seat: sre, Lane: seataccess.LaneGitLab, Step: step, Want: want,
		})
		if i == last {
			return err
		}
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	return nil
}

// A REFUSAL NAMES THE STEP'S FAILURE IN GITLAB'S OWN WORDS, the remedy, who
// acts and where; a 5xx is a fault the sync retries.
func TestARefusalCarriesGitLabsWordsTheRemedyAndWhereToAct(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	g.refuse["POST /projects/acme/api/members"] = glRefusal{http.StatusForbidden, "403 Forbidden - not an Owner"}
	s := openGL(t, g, []string{"acme/api"})
	want := seataccess.Desired{Enabled: true, Level: "20", Scope: []string{"acme/api"}}
	err := runOpUntil(t, s, seataccess.OpCreate, want, 1)
	var refusal *seataccess.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if !strings.Contains(refusal.Said, "not an Owner") || !strings.Contains(refusal.Remedy, "Owner role on acme") ||
		refusal.Actor != seataccess.ByYou || !strings.HasSuffix(refusal.ActionURL, "/groups/acme/-/group_members") {
		t.Fatalf("refusal = %+v", refusal)
	}

	g2 := newGLSite(t)
	g2.refuse["POST /projects/acme/api/members"] = glRefusal{http.StatusBadGateway, "bad gateway"}
	err = runOpUntil(t, openGL(t, g2, []string{"acme/api"}), seataccess.OpCreate, want, 1)
	if err == nil || errors.As(err, &refusal) {
		t.Fatalf("a 502 = %v, want a fault", err)
	}
}

// A TOKEN THAT CANNOT BE SEALED IS REVOKED THROUGH THE REVOKE THE MINT HANDED
// OVER, which is the only copy of its id.
func TestAMintedTokenThatCannotBeSealedIsRevokedAtGitLab(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	s := openGL(t, g, nil)
	want := seataccess.Desired{Enabled: true, Level: "30"}
	results := runOp(t, s, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	id := atoi(results[0].AccountID)
	m := minted(results)
	if len(m) != 1 || m[0].Revoke == nil {
		t.Fatalf("minted = %+v", m)
	}
	if err := m[0].Revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if live := g.liveTokens(id); len(live) != 0 {
		t.Fatalf("the unsealed token is still live: %+v", live)
	}
}

// AN ACCOUNT GITLAB WILL NOT LET AUTHENTICATE gets no token: the fresh one is
// revoked and the refusal says the account is the problem, which is what
// stops a pass minting for ever.
func TestAnAccountThatCannotAuthenticateKeepsNoToken(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	g.unusable = true
	s := openGL(t, g, nil)
	want := seataccess.Desired{Enabled: true, Level: "30"}
	err := runOpUntil(t, s, seataccess.OpCreate, want, 2)
	var refusal *seataccess.Refusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Said, "cannot authenticate") {
		t.Fatalf("err = %v", err)
	}
	if u := g.userNamed("crewlet-sre"); len(g.liveTokens(u.ID)) != 0 {
		t.Fatal("a token was left on an account that cannot use it")
	}
}

// A ROTATION MINTS, AND ONLY THE STEP AFTER THE SEAL REVOKES THE OLD ONE; an
// administrator's own token on the account is never touched.
func TestARotationRevokesOnlyTheEarlierTokenCrewletMinted(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	s := openGL(t, g, nil)
	want := seataccess.Desired{Enabled: true, Level: "30"}
	results := runOp(t, s, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	id := atoi(results[0].AccountID)
	g.mu.Lock()
	g.tokens[id] = append(g.tokens[id], &glToken{id: 900, name: "hand-made", owner: id, value: "x"})
	g.mu.Unlock()
	rotated := runOp(t, s, seataccess.OpRotate, want, seataccess.Observed{}, nil)
	live := g.liveTokens(id)
	names := []string{}
	for _, tok := range live {
		names = append(names, tok.name)
	}
	if len(minted(rotated)) != 1 || len(live) != 2 || !slices.Contains(names, "hand-made") ||
		live[len(live)-1].value != minted(rotated)[0].Value {
		t.Fatalf("after rotation live = %v", names)
	}
}

// THE SHARED PLAN DRAWS GITLAB'S OFF AS ITS OWN STEPS: the account is turned
// off, then the credential is revoked once, and a person sees both.
func TestThePlanTurnsASeatOffThenRevokesItsToken(t *testing.T) {
	t.Parallel()
	plan, err := seataccess.Plan(gitlab.Vendor{}, seataccess.SeatInput{
		Seat: sre,
		Want: map[seataccess.Lane]seataccess.Desired{seataccess.LaneGitLab: {}},
		Have: map[seataccess.Lane]seataccess.Observed{seataccess.LaneGitLab: {
			Exists: true, Enabled: true, Level: "30",
			Credential: &seataccess.Credential{Variable: "GITLAB_TOKEN_SRE"},
		}},
		Record: &seataccess.Record{Credential: &seataccess.Credential{Variable: "GITLAB_TOKEN_SRE"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ops []seataccess.Op
	for _, step := range plan[seataccess.LaneGitLab] {
		ops = append(ops, step.Op)
	}
	if !slices.Equal(ops, []seataccess.Op{seataccess.OpDisable, seataccess.OpRevoke}) {
		t.Fatalf("plan = %v", ops)
	}
}

// AN INSTANCE-OWNED ACCOUNT A GROUP OWNER CANNOT CREATE NAMES THE CREDENTIAL
// THE MODE NEEDS, because the same 403 means a different remedy in each mode.
func TestACreateRefusalNamesTheCredentialTheModeNeeds(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	g.refuse["POST /service_accounts"] = glRefusal{http.StatusForbidden, "403 Forbidden"}
	cfg := glCompany(g.server.URL, nil, &config.RoleGitLab{Enabled: on(), AccessLevel: 30})
	cfg.Integrations.GitLab.Provisioning.Mode = config.GitLabModeInstance
	s, err := gitlab.Vendor{}.Open(context.Background(), seataccess.OpenInput{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	err = runOpUntil(t, s, seataccess.OpCreate, seataccess.Desired{Enabled: true, Level: "30"}, 0)
	var refusal *seataccess.Refusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Remedy, "instance administrator") ||
		!strings.Contains(refusal.Remedy, "provisioning.mode: group") {
		t.Fatalf("err = %v", err)
	}
}

// A STEP NAMES THE ROLE AS GITLAB NAMES IT: a custom role by the name the
// lane's Options read live, and one GitLab does not have by its id marked as
// missing, never a bare id.
func TestStepsNameACustomRoleAsGitLabDoes(t *testing.T) {
	t.Parallel()
	want := seataccess.Desired{Enabled: true, Level: "member_role:7", Scope: []string{"acme/api"}}
	in := seataccess.StepsInput{Seat: sre, Lane: seataccess.LaneGitLab, Op: seataccess.OpCreate, Want: want,
		Options: seataccess.Options{Levels: []seataccess.Level{{ID: "member_role:7", Name: "Incident responder", Custom: true}}}}
	named := gitlab.Vendor{}.StepsFor(in)
	if !strings.Contains(named[1].Text, "acme/api as Incident responder") {
		t.Fatalf("named = %q", named[1].Text)
	}
	in.Options = seataccess.Options{}
	missing := gitlab.Vendor{}.StepsFor(in)
	if !strings.Contains(missing[1].Text, "member_role:7 (not found at GitLab)") {
		t.Fatalf("unresolved = %q", missing[1].Text)
	}
	if len(named) != len(missing) {
		t.Fatal("Options changed which steps there are, not only their words")
	}
}

// TURNED BACK ON AT THE DOCUMENT'S ROLE, with a token only where the seat
// holds none.
func TestBackOnMintsOnlyWhereTheSeatHoldsNoToken(t *testing.T) {
	t.Parallel()
	v := gitlab.Vendor{}
	want := seataccess.Desired{Enabled: true, Level: "30"}
	without := v.Steps(sre, seataccess.LaneGitLab, seataccess.OpEnable, want, seataccess.Observed{Exists: true})
	with := v.Steps(sre, seataccess.LaneGitLab, seataccess.OpEnable, want, seataccess.Observed{
		Exists: true, Credential: &seataccess.Credential{Variable: "GITLAB_TOKEN_SRE"},
	})
	if len(without) != 4 || without[2].Verb != seataccess.VerbMint || len(with) != 2 ||
		with[1].Verb != seataccess.VerbGrant {
		t.Fatalf("without a token %d steps, with one %d", len(without), len(with))
	}
}

// THE DOCUMENT'S MODE DECIDES THE ROUTE, for every operation on an account:
// an instance-owned account is created, minted on and removed through the
// instance's own routes. The group delete answers 404 as success for an
// account the group does not own, so a removal sent down the wrong route
// would report the account gone while it stays live.
func TestAnInstanceOwnedAccountIsWorkedOnThroughTheInstanceRoutes(t *testing.T) {
	t.Parallel()
	g := newGLSite(t)
	g.admin = true
	cfg := glCompany(g.server.URL, nil, &config.RoleGitLab{Enabled: on(), AccessLevel: 30})
	cfg.Integrations.GitLab.Provisioning.Mode = config.GitLabModeInstance
	s, err := gitlab.Vendor{}.Open(context.Background(), seataccess.OpenInput{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	want := seataccess.Desired{Enabled: true, Level: "30"}
	results := runOp(t, s, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	id := results[0].AccountID
	rec := &seataccess.Record{Account: &seataccess.Account{ID: id, State: seataccess.AccountActive}}
	runOp(t, s, seataccess.OpRemove, seataccess.Desired{}, seataccess.Observed{Exists: true}, rec)
	for _, call := range []string{
		"POST /service_accounts", "POST /users/" + id + "/personal_access_tokens",
		"DELETE /service_accounts/" + id,
	} {
		if !g.called(call) {
			t.Errorf("the instance route %q was not used", call)
		}
	}
	for _, call := range []string{"POST /groups/9/service_accounts", "DELETE /groups/9/service_accounts/" + id} {
		if g.called(call) {
			t.Errorf("an instance-owned account was sent down the group route %q", call)
		}
	}
}
