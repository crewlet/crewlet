package agentaccess

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/atlassianhost"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// atlSite is a stand-in for an Atlassian organization and its site: the
// admin APIs, Jira's project roles and Confluence's space access, all on one
// server because their paths do not overlap.
type atlSite struct {
	server *httptest.Server
	mu     sync.Mutex

	accounts map[string]*atlassian.ServiceAccount
	grants   map[string][]string // account -> product keys granted
	tokens   map[string][]*atlToken
	// actors is each project role's accounts, by "PROJECT/roleID".
	actors map[string]map[string]bool
	// rolesMode is the Confluence role mode; empty answers 404 (no beta).
	rolesMode string
	// spaceRoles is each space's role per account; spacePerms each space's
	// permissions per account.
	spaceRoles map[string]map[string]string
	spacePerms map[string]map[string][]confluence.SpacePermission
	refuse     map[string]int

	calls    []string
	nextID   int
	nextTok  int
	nextPerm int
}

type atlToken struct {
	atlassian.APIToken
	scopes []string
	value  string
}

func newAtlSite(t *testing.T) *atlSite {
	t.Helper()
	f := &atlSite{
		accounts: map[string]*atlassian.ServiceAccount{}, grants: map[string][]string{},
		tokens: map[string][]*atlToken{}, actors: map[string]map[string]bool{},
		spaceRoles: map[string]map[string]string{"1001": {}, "1002": {}},
		spacePerms: map[string]map[string][]confluence.SpacePermission{"1001": {}, "1002": {}},
		refuse:     map[string]int{}, nextID: 1, nextTok: 1, nextPerm: 1,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

const (
	orgAccounts = "/admin/account-management/v1/orgs/ORG/service-accounts"
	orgInvite   = orgAccounts + "/invite"
)

var (
	reTokens    = regexp.MustCompile(`^/users/([^/]+)/manage/api-tokens$`)
	reToken     = regexp.MustCompile(`^/users/([^/]+)/manage/api-tokens/([^/]+)$`)
	reDelete    = regexp.MustCompile(`^/users/([^/]+)/manage/lifecycle/delete$`)
	reRole      = regexp.MustCompile(`^/rest/api/3/project/([^/]+)/role/(\d+)$`)
	reAssign    = regexp.MustCompile(`^/api/v2/spaces/(\d+)/role-assignments$`)
	rePerms     = regexp.MustCompile(`^/api/v2/spaces/(\d+)/permissions$`)
	reAddPerm   = regexp.MustCompile(`^/rest/api/space/([^/]+)/permission$`)
	reDropPerm  = regexp.MustCompile(`^/rest/api/space/([^/]+)/permission/([^/]+)$`)
	spaceIDByKy = map[string]string{"ENG": "1001", "OPS": "1002"}
)

func (f *atlSite) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	call := r.Method + " " + path
	f.calls = append(f.calls, call)
	reply := func(status int, v any) {
		w.WriteHeader(status)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	if status, ok := f.refuse[call]; ok {
		reply(status, map[string]string{"message": "refused by the stand-in"})
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	switch {
	case call == "GET "+orgAccounts:
		items := []atlassian.ServiceAccount{}
		for _, a := range f.accounts {
			items = append(items, *a)
		}
		reply(http.StatusOK, map[string]any{"items": items, "links": map[string]any{}})
	case call == "POST "+orgAccounts:
		id := fmt.Sprintf("acc-%d", f.nextID)
		f.nextID++
		a := &atlassian.ServiceAccount{ID: id, DisplayName: body["displayName"].(string),
			Email: id + "@serviceaccount.atlassian.com"}
		f.accounts[id] = a
		reply(http.StatusCreated, a)
	case call == "POST "+orgInvite:
		for _, id := range body["userIds"].([]any) {
			for _, rule := range body["permissionRules"].([]any) {
				res := rule.(map[string]any)["resource"].(string)
				product := strings.TrimSuffix(strings.TrimPrefix(res, "ari:cloud:"), "::site/cloud-1")
				f.grants[id.(string)] = append(f.grants[id.(string)], product)
			}
		}
		reply(http.StatusOK, map[string]any{})
	case call == "POST /admin/v2/orgs/ORG/workspaces":
		reply(http.StatusOK, map[string]any{"data": []map[string]any{{
			"id":         "ari:cloud:jira::site/cloud-1",
			"attributes": map[string]string{"type": "Jira", "hostUrl": "https://acme.atlassian.net"},
		}}})
	case reTokens.MatchString(path):
		account := reTokens.FindStringSubmatch(path)[1]
		if r.Method == http.MethodGet {
			list := []atlassian.APIToken{}
			for _, t := range f.tokens[account] {
				list = append(list, t.APIToken)
			}
			reply(http.StatusOK, list)
			return
		}
		var scopes []string
		for _, s := range body["scopes"].([]any) {
			scopes = append(scopes, s.(string))
		}
		t := &atlToken{
			APIToken: atlassian.APIToken{ID: fmt.Sprintf("tok-%d", f.nextTok), Label: body["label"].(string),
				Expiry: body["expiry"].(string)},
			scopes: scopes, value: fmt.Sprintf("fake-atl-token-%d", f.nextTok),
		}
		f.nextTok++
		f.tokens[account] = append(f.tokens[account], t)
		reply(http.StatusOK, map[string]string{"id": t.ID, "label": t.Label, "token": t.value})
	case reToken.MatchString(path) && r.Method == http.MethodDelete:
		m := reToken.FindStringSubmatch(path)
		before := len(f.tokens[m[1]])
		f.tokens[m[1]] = slices.DeleteFunc(f.tokens[m[1]], func(t *atlToken) bool { return t.ID == m[2] })
		if len(f.tokens[m[1]]) == before {
			reply(http.StatusNotFound, map[string]string{"message": "no such token"})
			return
		}
		reply(http.StatusNoContent, nil)
	case reDelete.MatchString(path):
		delete(f.accounts, reDelete.FindStringSubmatch(path)[1])
		reply(http.StatusNoContent, nil)
	case call == "GET /rest/api/3/role":
		reply(http.StatusOK, []map[string]any{
			{"id": 10002, "name": "Administrators"},
			{"id": 10101, "name": "Developers"},
			{"id": 10102, "name": "Viewers"},
		})
	case reRole.MatchString(path):
		m := reRole.FindStringSubmatch(path)
		key := m[1] + "/" + m[2]
		if f.actors[key] == nil {
			f.actors[key] = map[string]bool{}
		}
		switch r.Method {
		case http.MethodGet:
			actors := []map[string]any{}
			for id := range f.actors[key] {
				actors = append(actors, map[string]any{
					"type": "atlassian-user-role-actor", "actorUser": map[string]string{"accountId": id},
				})
			}
			reply(http.StatusOK, map[string]any{"actors": actors})
		case http.MethodPost:
			for _, id := range body["user"].([]any) {
				f.actors[key][id.(string)] = true
			}
			reply(http.StatusOK, map[string]any{})
		case http.MethodDelete:
			delete(f.actors[key], r.URL.Query().Get("user"))
			reply(http.StatusNoContent, nil)
		}
	case call == "GET /api/v2/space-role-mode":
		if f.rolesMode == "" {
			reply(http.StatusNotFound, nil)
			return
		}
		reply(http.StatusOK, map[string]string{"mode": f.rolesMode})
	case call == "GET /api/v2/space-roles":
		reply(http.StatusOK, map[string]any{"results": []map[string]any{
			{"id": "r-viewer", "name": "Viewer", "type": "SYSTEM", "spacePermissions": []string{"read/space"}},
			{"id": "r-collab", "name": "Collaborator", "type": "SYSTEM",
				"spacePermissions": []string{"read/space", "create/page"}},
			{"id": "r-admin", "name": "Admin", "type": "SYSTEM",
				"spacePermissions": []string{"read/space", "administer/space"}},
		}, "_links": map[string]any{}})
	case call == "GET /api/v2/spaces":
		key := r.URL.Query().Get("keys")
		results := []map[string]string{}
		if id, ok := spaceIDByKy[key]; ok {
			results = append(results, map[string]string{"id": id, "key": key})
		}
		reply(http.StatusOK, map[string]any{"results": results})
	case reAssign.MatchString(path):
		space := reAssign.FindStringSubmatch(path)[1]
		if r.Method == http.MethodGet {
			account := r.URL.Query().Get("principal-id")
			results := []map[string]any{}
			if role := f.spaceRoles[space][account]; role != "" {
				results = append(results, map[string]any{
					"principal": map[string]string{"principalType": "USER", "principalId": account},
					"roleId":    role,
				})
			}
			reply(http.StatusOK, map[string]any{"results": results, "_links": map[string]any{}})
			return
		}
		var entries []map[string]any
		_ = json.Unmarshal(raw, &entries)
		for _, e := range entries {
			account := e["principal"].(map[string]any)["principalId"].(string)
			if role, ok := e["roleId"].(string); ok {
				f.spaceRoles[space][account] = role
			} else {
				delete(f.spaceRoles[space], account)
			}
		}
		reply(http.StatusOK, map[string]any{"results": []any{}})
	case rePerms.MatchString(path):
		space := rePerms.FindStringSubmatch(path)[1]
		results := []map[string]any{}
		for account, perms := range f.spacePerms[space] {
			for _, p := range perms {
				results = append(results, map[string]any{
					"id":        p.ID,
					"principal": map[string]string{"type": "user", "id": account},
					"operation": map[string]string{"key": p.Operation.Key, "targetType": p.Operation.Target},
				})
			}
		}
		reply(http.StatusOK, map[string]any{"results": results, "_links": map[string]any{}})
	case reAddPerm.MatchString(path) && r.Method == http.MethodPost:
		space := spaceIDByKy[reAddPerm.FindStringSubmatch(path)[1]]
		account := body["subject"].(map[string]any)["identifier"].(string)
		op := body["operation"].(map[string]any)
		f.spacePerms[space][account] = append(f.spacePerms[space][account], confluence.SpacePermission{
			ID:        fmt.Sprintf("p%d", f.nextPerm),
			Operation: confluence.Operation{Key: op["key"].(string), Target: op["target"].(string)},
		})
		f.nextPerm++
		f.calls[len(f.calls)-1] += " " + op["key"].(string) + ":" + op["target"].(string)
		reply(http.StatusOK, map[string]any{})
	case reDropPerm.MatchString(path) && r.Method == http.MethodDelete:
		m := reDropPerm.FindStringSubmatch(path)
		space := spaceIDByKy[m[1]]
		for account, perms := range f.spacePerms[space] {
			for _, p := range perms {
				if p.ID != m[2] {
					continue
				}
				f.calls[len(f.calls)-1] += " " + p.Operation.ID()
				if p.Operation == confluence.OperationRead {
					// Confluence removes every permission with View.
					delete(f.spacePerms[space], account)
				} else {
					f.spacePerms[space][account] = slices.DeleteFunc(perms,
						func(q confluence.SpacePermission) bool { return q.ID == p.ID })
				}
				reply(http.StatusNoContent, nil)
				return
			}
		}
		reply(http.StatusNotFound, nil)
	default:
		reply(http.StatusNotFound, map[string]string{"message": "no route " + call})
	}
}

func (f *atlSite) locked(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// testSession is a session against the stand-in, with the seat's document
// blocks as given.
func testSession(t *testing.T, f *atlSite, doc seatDoc, jiraIn, confIn []string) *session {
	t.Helper()
	jc, err := jira.NewClient(jira.ClientOptions{
		URL: f.server.URL, Email: "admin@example.com", Token: "fake-site-token", Declared: atlassianhost.Cloud,
	})
	if err != nil {
		t.Fatal(err)
	}
	cc, err := confluence.NewClient(confluence.ClientOptions{
		URL: f.server.URL, Email: "admin@example.com", Token: "fake-site-token", Declared: atlassianhost.Cloud,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	return &session{
		org: atlassian.NewClient(atlassian.ClientOptions{BaseURL: f.server.URL}), key: "fake-org-key",
		orgID: "ORG", jira: jc, conf: cc, seats: map[string]seatDoc{"sre": doc},
		jiraIn: jiraIn, confIn: confIn, cache: newCache(),
		now: func() time.Time { clock = clock.Add(time.Second); return clock },
	}
}

func onJira(role string, projects ...string) *config.RoleJira {
	v := true
	return &config.RoleJira{Enabled: &v, ProjectRole: role, Projects: projects}
}

func onConfluence(role string, spaces ...string) *config.RoleConfluence {
	v := true
	return &config.RoleConfluence{Enabled: &v, SpaceRole: role, Spaces: spaces}
}

var sre = seataccess.SeatRef{Handle: "sre", Name: "SRE"}

// run takes every step an op draws on a lane, the way the sync does, and
// fails the test on any error.
func run(t *testing.T, s *session, lane seataccess.Lane, op seataccess.Op, want seataccess.Desired,
	have seataccess.Observed, rec *seataccess.Record,
) []seataccess.StepResult {
	t.Helper()
	results, err := runErr(s, lane, op, want, have, rec)
	if err != nil {
		t.Fatalf("%s %s: %v", lane, op, err)
	}
	return results
}

func runErr(s *session, lane seataccess.Lane, op seataccess.Op, want seataccess.Desired,
	have seataccess.Observed, rec *seataccess.Record,
) ([]seataccess.StepResult, error) {
	var out []seataccess.StepResult
	for _, step := range (Vendor{}).Steps(sre, lane, op, want, have) {
		res, err := s.Apply(context.Background(), seataccess.ApplyInput{
			Seat: sre, Lane: lane, Step: step, Want: want, Have: have, Record: rec,
		})
		if err != nil {
			return out, fmt.Errorf("step %d (%s): %w", step.Index, step.Text, err)
		}
		out = append(out, res)
	}
	return out, nil
}

func mintedOf(results []seataccess.StepResult) map[string]seataccess.Minted {
	out := map[string]seataccess.Minted{}
	for _, r := range results {
		for _, m := range r.Minted {
			out[m.Field] = m
		}
	}
	return out
}

func observe(t *testing.T, s *session, rec *seataccess.Record) map[seataccess.Lane]seataccess.Observed {
	t.Helper()
	records := map[string]seataccess.Record{}
	if rec != nil {
		records["sre"] = *rec
	}
	// A NEW PASS, as the next one would be: nothing read by the last.
	s.cache = newCache()
	obs, err := s.Observe(context.Background(), seataccess.ObserveInput{
		Seats: []seataccess.SeatRef{sre}, Records: records,
	})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return obs["sre"]
}

func TestTheAtlassianVendorHasTwoLanesOverOneAccount(t *testing.T) {
	t.Parallel()
	v, ok := seataccess.Lookup(integration.KindAtlassian)
	if !ok {
		t.Fatal("Atlassian is not registered")
	}
	cat := v.Catalog()
	if !slices.Equal(cat.Credentials, []string{FieldToken, FieldEmail}) ||
		len(cat.Lanes) != 2 || cat.Lanes[0].Lane != seataccess.LaneJira || cat.Lanes[1].Lane != seataccess.LaneConfluence {
		t.Fatalf("catalog = %+v", cat)
	}
}

// THE LABEL CARRIES THE LANES, because Atlassian lists a token's label and not
// its scopes; a token minted before lanes existed carried both products.
func TestATokensLabelSaysWhichProductsItReaches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		label string
		want  lanes
		own   bool
	}{
		{tokenLabel("sre", lanes{jira: true}) + "-1700000000", lanes{jira: true}, true},
		{tokenLabel("sre", lanes{confluence: true}) + "-1700000000", lanes{confluence: true}, true},
		{tokenLabel("sre", lanes{true, true}) + "-1700000000", lanes{true, true}, true},
		{"sre-1700000000", lanes{true, true}, true},
		{tokenLabel("sre-lead", lanes{jira: true}) + "-1700000000", lanes{}, false},
		{"hand-made", lanes{}, false},
		{tokenLabel("sre", lanes{jira: true}), lanes{}, false},
	} {
		got, _, own := labelLanes(tc.label, "sre")
		if own != tc.own || got != tc.want {
			t.Errorf("labelLanes(%q) = %+v, %v; want %+v, %v", tc.label, got, own, tc.want, tc.own)
		}
	}
}

func TestOpenRefusesACompanyWithoutAnOrganizationKey(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]*config.Company{
		"no document": nil,
		"no block":    {},
		"no key":      {Integrations: config.Integrations{Atlassian: &config.Atlassian{OrgID: "ORG"}}},
	} {
		_, err := Vendor{}.Open(context.Background(), seataccess.OpenInput{Config: cfg})
		if !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: err = %v, want ErrNotConfigured", name, err)
		}
	}
	s, err := Vendor{}.Open(context.Background(), seataccess.OpenInput{Config: &config.Company{
		Integrations: config.Integrations{Atlassian: &config.Atlassian{OrgID: "ORG", APIKey: "fake-key"}},
	}})
	if err != nil {
		t.Fatalf("a company with only the organization block: %v", err)
	}
	// NO JIRA BLOCK, NO JIRA ROLES: the lane says so rather than guessing.
	_, err = s.Options(context.Background(), seataccess.LaneJira)
	var refusal *seataccess.Refusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Said, "Jira is not connected") {
		t.Fatalf("Jira options without Jira = %v", err)
	}
}

func TestOptionsAreTheSitesOwnRolesAndPermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAtlSite(t)
	s := testSession(t, f, seatDoc{}, []string{"GOP"}, nil)
	jiraOpts, err := s.Options(ctx, seataccess.LaneJira)
	if err != nil {
		t.Fatal(err)
	}
	admins, _, _ := jiraOpts.Level("10002")
	devs, _, _ := jiraOpts.Level("10101")
	if !jiraOpts.Live || len(jiraOpts.Levels) != 4 || !admins.Administers || devs.Administers ||
		!slices.Equal(jiraOpts.Scope, []string{"GOP"}) {
		t.Fatalf("Jira options = %+v", jiraOpts)
	}
	// WITHOUT THE ROLE BETA, Confluence's space permissions under its names.
	perms, err := s.Options(ctx, seataccess.LaneConfluence)
	if err != nil {
		t.Fatal(err)
	}
	view, _, _ := perms.Level("read:space")
	admin, _, _ := perms.Level("administer:space")
	if view.Name != "View" || !admin.Administers || !perms.ScopeOpen {
		t.Fatalf("Confluence permission options = %+v", perms)
	}
	f.locked(func() { f.rolesMode = "ROLES" })
	s = testSession(t, f, seatDoc{}, nil, []string{"ENG"})
	roles, err := s.Options(ctx, seataccess.LaneConfluence)
	if err != nil {
		t.Fatal(err)
	}
	adminRole, _, _ := roles.Level("r-admin")
	collab, _, _ := roles.Level("r-collab")
	if len(roles.Levels) != 4 || !adminRole.Administers || collab.Administers || collab.Name != "Collaborator" {
		t.Fatalf("Confluence role options = %+v", roles)
	}
}

// ONE ACCOUNT, TWO LANES: Jira creates it with a Jira-only token; turning
// Confluence on grants that product, gives the space role, and replaces the
// token with one carrying both products' scopes; turning Jira off removes its
// roles and narrows the token to Confluence; turning Confluence off revokes
// it; and only then can the account be removed.
func TestASeatsWholeLifeAcrossJiraAndConfluence(t *testing.T) {
	t.Parallel()
	f := newAtlSite(t)
	f.rolesMode = "ROLES"
	doc := seatDoc{jira: onJira("10101", "GOP")}
	s := testSession(t, f, doc, []string{"GOP", "OPS"}, []string{"ENG"})

	jiraWant := seataccess.Desired{Enabled: true, Level: "10101", Scope: []string{"GOP"}}
	created := run(t, s, seataccess.LaneJira, seataccess.OpCreate, jiraWant, seataccess.Observed{}, nil)
	account := created[0].AccountID
	m := mintedOf(created)
	f.locked(func() {
		if f.accounts[account].DisplayName != "SRE (crewlet:sre)" ||
			!slices.Equal(f.grants[account], []string{"jira-software"}) || !f.actors["GOP/10101"][account] {
			t.Fatalf("after create: account %+v grants %v", f.accounts[account], f.grants[account])
		}
		live := f.tokens[account]
		if len(live) != 1 || !slices.Equal(live[0].scopes, atlassian.JiraScopes) ||
			m[FieldToken].Value != live[0].value || m[FieldEmail].Value != account+"@serviceaccount.atlassian.com" {
			t.Fatalf("token %+v, minted %+v", live, m)
		}
	})

	rec := &seataccess.Record{
		Account:    &seataccess.Account{ID: account, State: seataccess.AccountActive},
		Credential: &seataccess.Credential{Variable: "ATLASSIAN_TOKEN_SRE"},
		Lanes:      map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneJira: {Applied: &jiraWant}},
	}
	obs := observe(t, s, rec)
	if j, c := obs[seataccess.LaneJira], obs[seataccess.LaneConfluence]; !j.Enabled || j.Level != "10101" ||
		!slices.Equal(j.Scope, []string{"GOP"}) || j.Credential == nil || c.Enabled || !c.Exists ||
		c.DisabledBy != seataccess.DisabledByCrewlet {
		t.Fatalf("observed after Jira = %+v / %+v", j, c)
	}

	doc.confluence = onConfluence("r-collab", "ENG")
	s.seats["sre"] = doc
	confWant := seataccess.Desired{Enabled: true, Level: "r-collab", Scope: []string{"ENG"}}
	run(t, s, seataccess.LaneConfluence, seataccess.OpEnable, confWant, obs[seataccess.LaneConfluence], rec)
	f.locked(func() {
		live := f.tokens[account]
		if !slices.Contains(f.grants[account], "confluence") || f.spaceRoles["1001"][account] != "r-collab" ||
			len(live) != 1 || !slices.Equal(live[0].scopes, atlassian.ScopesFor(true, true)) {
			t.Fatalf("after Confluence on: grants %v, tokens %+v", f.grants[account], live)
		}
	})
	rec.Lanes[seataccess.LaneConfluence] = seataccess.LaneRecord{Applied: &confWant}
	obs = observe(t, s, rec)
	if c := obs[seataccess.LaneConfluence]; !c.Enabled || c.Level != "r-collab" || !slices.Equal(c.Scope, []string{"ENG"}) {
		t.Fatalf("observed Confluence = %+v", c)
	}

	doc.jira = &config.RoleJira{Projects: []string{"GOP"}}
	s.seats["sre"] = doc
	off := run(t, s, seataccess.LaneJira, seataccess.OpDisable, seataccess.Desired{}, obs[seataccess.LaneJira], rec)
	if off[0].Disabled {
		t.Fatal("the account was marked disabled while Confluence is still on")
	}
	if !strings.Contains(off[0].Did, "keeps its Jira product access") ||
		off[1].Did != "Minted an API token with the scopes of Confluence only" {
		t.Fatalf("Jira off said %q / %q", off[0].Did, off[1].Did)
	}
	f.locked(func() {
		live := f.tokens[account]
		if f.actors["GOP/10101"][account] || len(live) != 1 || !slices.Equal(live[0].scopes, atlassian.ConfluenceScopes) {
			t.Fatalf("after Jira off: actors %v, tokens %+v", f.actors, live)
		}
	})

	if _, err := runErr(s, seataccess.LaneJira, seataccess.OpRemove, seataccess.Desired{}, seataccess.Observed{}, rec); err == nil {
		t.Fatal("the account was removed while Confluence is still on")
	}

	doc.confluence = &config.RoleConfluence{Spaces: []string{"ENG"}}
	s.seats["sre"] = doc
	last := run(t, s, seataccess.LaneConfluence, seataccess.OpDisable, seataccess.Desired{}, obs[seataccess.LaneConfluence], rec)
	if !last[0].Disabled || len(mintedOf(last)) != 0 {
		t.Fatalf("last lane off = %+v, want the account disabled and no token minted", last)
	}
	// THE TOKEN IS REVOKED ONCE, after the last lane's disable.
	revoked := run(t, s, seataccess.LaneConfluence, seataccess.OpRevoke, seataccess.Desired{}, obs[seataccess.LaneConfluence], rec)
	if !slices.Equal(revoked[0].Revoked, []string{FieldToken}) {
		t.Fatalf("revoke = %+v", revoked)
	}
	f.locked(func() {
		if len(f.tokens[account]) != 0 || f.spaceRoles["1001"][account] != "" {
			t.Fatalf("after both off: tokens %+v, space role %q", f.tokens[account], f.spaceRoles["1001"][account])
		}
	})

	removed := run(t, s, seataccess.LaneJira, seataccess.OpRemove, seataccess.Desired{}, seataccess.Observed{}, rec)
	if !removed[0].Gone {
		t.Fatalf("remove = %+v", removed)
	}
	f.locked(func() {
		if _, ok := f.accounts[account]; ok {
			t.Fatal("the account is still there")
		}
	})
}

// A TOKEN THAT CANNOT BE SEALED IS REVOKED through the revoke its mint handed
// over; the address grants nothing and carries none.
func TestAMintedTokenThatCannotBeSealedIsRevokedAtAtlassian(t *testing.T) {
	t.Parallel()
	f := newAtlSite(t)
	s := testSession(t, f, seatDoc{jira: onJira("10101", "GOP")}, []string{"GOP"}, nil)
	want := seataccess.Desired{Enabled: true, Level: "10101", Scope: []string{"GOP"}}
	results := run(t, s, seataccess.LaneJira, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	m := mintedOf(results)
	if m[FieldEmail].Revoke != nil || m[FieldToken].Revoke == nil {
		t.Fatalf("minted = %+v", m)
	}
	if err := m[FieldToken].Revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.locked(func() {
		if n := len(f.tokens[results[0].AccountID]); n != 0 {
			t.Fatalf("%d token(s) still live", n)
		}
	})
}

// A ROLE ADDED BY HAND IS DRIFT: two roles in one project can never equal a
// stored level, so the plan restores the document's.
func TestASecondRoleAddedByHandIsObservedAsDrift(t *testing.T) {
	t.Parallel()
	f := newAtlSite(t)
	s := testSession(t, f, seatDoc{jira: onJira("10101", "GOP")}, []string{"GOP"}, nil)
	want := seataccess.Desired{Enabled: true, Level: "10101", Scope: []string{"GOP"}}
	account := run(t, s, seataccess.LaneJira, seataccess.OpCreate, want, seataccess.Observed{}, nil)[0].AccountID
	f.locked(func() { f.actors["GOP/10002"] = map[string]bool{account: true} })
	rec := &seataccess.Record{
		Account: &seataccess.Account{ID: account, State: seataccess.AccountActive},
		Lanes:   map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneJira: {Applied: &want}},
	}
	j := observe(t, s, rec)[seataccess.LaneJira]
	if j.Level == want.Level {
		t.Fatalf("drift into Administrators observed as %q", j.Level)
	}
	run(t, s, seataccess.LaneJira, seataccess.OpLevel, want, j, rec)
	f.locked(func() {
		if f.actors["GOP/10002"][account] || !f.actors["GOP/10101"][account] {
			t.Fatalf("after restore: %v", f.actors)
		}
	})
}

// SPACE PERMISSIONS BY OPERATION: View goes in first, and comes out last,
// because Confluence requires it for every other one and removes them all
// with it.
func TestSpacePermissionsAreGrantedViewFirstAndRemovedViewLast(t *testing.T) {
	t.Parallel()
	f := newAtlSite(t)
	doc := seatDoc{confluence: &config.RoleConfluence{Enabled: new(bool), Spaces: []string{"ENG"}}}
	*doc.confluence.Enabled = true
	s := testSession(t, f, doc, nil, []string{"ENG"})
	want := seataccess.Desired{Enabled: true, Level: "create:page read:space", Scope: []string{"ENG"}}
	results := run(t, s, seataccess.LaneConfluence, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	account := results[0].AccountID
	var adds []string
	f.locked(func() {
		for _, c := range f.calls {
			if strings.HasPrefix(c, "POST /rest/api/space/ENG/permission ") {
				adds = append(adds, strings.Fields(c)[2])
			}
		}
	})
	if !slices.Equal(adds, []string{"read:space", "create:page"}) {
		t.Fatalf("adds = %v, want View first", adds)
	}
	rec := &seataccess.Record{Account: &seataccess.Account{ID: account, State: seataccess.AccountActive}}
	if c := observe(t, s, rec)[seataccess.LaneConfluence]; c.Level != "create:page read:space" {
		t.Fatalf("observed level = %q", c.Level)
	}
	doc.confluence.Enabled = new(bool)
	s.seats["sre"] = doc
	run(t, s, seataccess.LaneConfluence, seataccess.OpDisable, seataccess.Desired{},
		seataccess.Observed{Scope: []string{"ENG"}}, rec)
	var drops []string
	f.locked(func() {
		for _, c := range f.calls {
			if strings.HasPrefix(c, "DELETE /rest/api/space/ENG/permission/") {
				drops = append(drops, strings.Fields(c)[2])
			}
		}
	})
	if !slices.Equal(drops, []string{"create:page", "read:space"}) {
		t.Fatalf("drops = %v, want View last", drops)
	}
}

// A REFUSAL NAMES THE PRODUCT'S OWN WORDS, THE REMEDY AND WHERE; a 5xx is a
// fault the sync retries.
func TestARefusalCarriesTheProductsWordsTheRemedyAndWhere(t *testing.T) {
	t.Parallel()
	f := newAtlSite(t)
	f.refuse["POST /rest/api/3/project/GOP/role/10101"] = http.StatusForbidden
	s := testSession(t, f, seatDoc{jira: onJira("10101", "GOP")}, []string{"GOP"}, nil)
	want := seataccess.Desired{Enabled: true, Level: "10101", Scope: []string{"GOP"}}
	_, err := runErr(s, seataccess.LaneJira, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	var refusal *seataccess.Refusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Said, "refused by the stand-in") ||
		!strings.Contains(refusal.Remedy, "Administer Projects") || refusal.Actor != seataccess.ByYou ||
		refusal.ActionURL != f.server.URL {
		t.Fatalf("err = %v", err)
	}

	f2 := newAtlSite(t)
	f2.refuse["POST "+orgAccounts] = http.StatusUnauthorized
	s2 := testSession(t, f2, seatDoc{jira: onJira("10101", "GOP")}, []string{"GOP"}, nil)
	_, err = runErr(s2, seataccess.LaneJira, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Remedy, "integrations.atlassian.api_key") ||
		refusal.ActionURL != adminConsoleURL {
		t.Fatalf("org refusal = %v", err)
	}

	f3 := newAtlSite(t)
	f3.refuse["POST "+orgInvite] = http.StatusServiceUnavailable
	s3 := testSession(t, f3, seatDoc{jira: onJira("10101", "GOP")}, []string{"GOP"}, nil)
	_, err = runErr(s3, seataccess.LaneJira, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	if err == nil || errors.As(err, &refusal) {
		t.Fatalf("a 503 = %v, want a fault", err)
	}
}

func TestAProjectRoleTheSiteNoLongerHasIsARefusal(t *testing.T) {
	t.Parallel()
	f := newAtlSite(t)
	s := testSession(t, f, seatDoc{jira: onJira("99999", "GOP")}, []string{"GOP"}, nil)
	want := seataccess.Desired{Enabled: true, Level: "99999", Scope: []string{"GOP"}}
	_, err := runErr(s, seataccess.LaneJira, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	var refusal *seataccess.Refusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Said, "99999") {
		t.Fatalf("err = %v", err)
	}
}

// A ROTATION MINTS WITH THE LANES THE SEAT IS ON AND REVOKES ONLY CREWLET'S
// EARLIER TOKEN; one made by hand on the account is left alone.
func TestARotationKeepsAHandMadeToken(t *testing.T) {
	t.Parallel()
	f := newAtlSite(t)
	s := testSession(t, f, seatDoc{jira: onJira("10101", "GOP")}, []string{"GOP"}, nil)
	want := seataccess.Desired{Enabled: true, Level: "10101", Scope: []string{"GOP"}}
	account := run(t, s, seataccess.LaneJira, seataccess.OpCreate, want, seataccess.Observed{}, nil)[0].AccountID
	f.locked(func() {
		f.tokens[account] = append(f.tokens[account], &atlToken{APIToken: atlassian.APIToken{ID: "hand", Label: "ci-bot"}})
	})
	rotated := run(t, s, seataccess.LaneJira, seataccess.OpRotate, want, seataccess.Observed{}, nil)
	f.locked(func() {
		var labels []string
		for _, tok := range f.tokens[account] {
			labels = append(labels, tok.Label)
		}
		if len(labels) != 2 || !slices.Contains(labels, "ci-bot") ||
			mintedOf(rotated)[FieldToken].Value != f.tokens[account][1].value {
			t.Fatalf("after rotation: %v", labels)
		}
	})
}

// ONE ACCOUNT, PLANNED ONCE: a new seat on both products is created on the
// Jira lane, and the Confluence lane grants its product on the same account
// rather than creating a second one.
func TestThePlanCreatesOneAccountForBothProducts(t *testing.T) {
	t.Parallel()
	want := seataccess.Desired{Enabled: true, Level: "10101"}
	plan, err := seataccess.Plan(Vendor{}, seataccess.SeatInput{
		Seat: sre,
		Want: map[seataccess.Lane]seataccess.Desired{
			seataccess.LaneJira: want, seataccess.LaneConfluence: {Enabled: true, Level: "r-viewer"},
		},
		Have: map[seataccess.Lane]seataccess.Observed{},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := func(lane seataccess.Lane) seataccess.Op {
		if len(plan[lane]) == 0 {
			return ""
		}
		return plan[lane][0].Op
	}
	if first(seataccess.LaneJira) != seataccess.OpCreate || first(seataccess.LaneConfluence) != seataccess.OpEnable {
		t.Fatalf("plan = jira %v, confluence %v", plan[seataccess.LaneJira], plan[seataccess.LaneConfluence])
	}
	for _, step := range plan[seataccess.LaneConfluence] {
		if step.Verb == seataccess.VerbCreate {
			t.Fatalf("the Confluence lane creates a second account: %+v", step)
		}
	}
}

// A STEP NAMES THE ACCESS AS THE PRODUCT NAMES IT: a project role from the
// lane's Options, Confluence's permissions by their own names, and a role the
// site does not have by its id marked as missing.
func TestStepsNameTheAccessAsTheProductDoes(t *testing.T) {
	t.Parallel()
	v := Vendor{}
	jira := seataccess.StepsInput{Seat: sre, Lane: seataccess.LaneJira, Op: seataccess.OpLevel,
		Want: seataccess.Desired{Enabled: true, Level: "10101", Scope: []string{"GOP"}},
		Have: seataccess.Observed{Exists: true, Enabled: true, Level: "10102"},
		Options: seataccess.Options{Levels: []seataccess.Level{
			{ID: "10101", Name: "Developers"}, {ID: "10102", Name: "Viewers"},
		}},
	}
	if got := v.StepsFor(jira)[0].Text; got != "Change it from the Viewers project role to the Developers project role in GOP" {
		t.Fatalf("named = %q", got)
	}
	jira.Options = seataccess.Options{}
	if got := v.StepsFor(jira)[0].Text; !strings.Contains(got, "the project role 10101 (not found at Jira)") {
		t.Fatalf("unresolved = %q", got)
	}
	perms := seataccess.StepsInput{Seat: sre, Lane: seataccess.LaneConfluence, Op: seataccess.OpScope,
		Want: seataccess.Desired{Enabled: true, Level: "create:page read:space", Scope: []string{"ENG"}}}
	if got := v.StepsFor(perms)[0].Text; got != "Give it the space permissions View, Add pages in ENG and nowhere else" {
		t.Fatalf("permissions = %q", got)
	}
}

// PRODUCT ACCESS ONLY IS OFFERED FIRST, least access, with the sentence that
// says what it means, on Jira and on both kinds of Confluence site.
func TestProductAccessOnlyIsOfferedFirstWithWhatItMeans(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAtlSite(t)
	s := testSession(t, f, seatDoc{}, []string{"GOP"}, []string{"ENG"})
	check := func(lane seataccess.Lane, mention string) {
		t.Helper()
		opts, err := s.Options(ctx, lane)
		if err != nil {
			t.Fatal(err)
		}
		first := opts.Levels[0]
		if first.ID != "" || first.Name != "Product access only" || !strings.Contains(first.Note, mention) ||
			first.Administers {
			t.Fatalf("%s first level = %+v", lane, first)
		}
		if _, ok := opts.LevelsOf(""); !ok {
			t.Fatalf("%s does not offer the empty level", lane)
		}
	}
	check(seataccess.LaneJira, "permission scheme")
	check(seataccess.LaneConfluence, "permissions decide")
	f.locked(func() { f.rolesMode = "ROLES" })
	s = testSession(t, f, seatDoc{}, []string{"GOP"}, []string{"ENG"})
	check(seataccess.LaneConfluence, "permissions decide")
}

// A SEAT CREATED WITH PRODUCT ACCESS ONLY gets the account, the product and a
// token, and no project role anywhere; read back, it is converged.
func TestASeatCreatedWithProductAccessOnlyHoldsNoRole(t *testing.T) {
	t.Parallel()
	f := newAtlSite(t)
	s := testSession(t, f, seatDoc{jira: onJira("")}, []string{"GOP", "OPS"}, nil)
	want := seataccess.Desired{Enabled: true}
	steps := (Vendor{}).Steps(sre, seataccess.LaneJira, seataccess.OpCreate, want, seataccess.Observed{})
	if !strings.Contains(steps[2].Text, "product access only: no project roles in any project") {
		t.Fatalf("role step = %q", steps[2].Text)
	}
	results := run(t, s, seataccess.LaneJira, seataccess.OpCreate, want, seataccess.Observed{}, nil)
	account := results[0].AccountID
	f.locked(func() {
		if !slices.Equal(f.grants[account], []string{"jira-software"}) || len(f.tokens[account]) != 1 {
			t.Fatalf("grants %v, tokens %d", f.grants[account], len(f.tokens[account]))
		}
		for key, members := range f.actors {
			if members[account] {
				t.Fatalf("product access only holds the role %s", key)
			}
		}
	})
	rec := &seataccess.Record{
		Account:    &seataccess.Account{ID: account, State: seataccess.AccountActive},
		Credential: &seataccess.Credential{Variable: "ATLASSIAN_TOKEN_SRE"},
		Lanes:      map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneJira: {Applied: &want}},
	}
	have := observe(t, s, rec)[seataccess.LaneJira]
	plan, err := seataccess.Plan(Vendor{}, seataccess.SeatInput{
		Seat: sre, Record: rec,
		Want: map[seataccess.Lane]seataccess.Desired{seataccess.LaneJira: want},
		Have: map[seataccess.Lane]seataccess.Observed{seataccess.LaneJira: have},
	})
	if err != nil || len(plan[seataccess.LaneJira]) != 0 {
		t.Fatalf("a converged product-access-only seat plans %+v, %v", plan[seataccess.LaneJira], err)
	}
}

// FROM A ROLE TO PRODUCT ACCESS ONLY removes the roles and keeps the product;
// AND BACK gives the role again in the listed project.
func TestMovingBetweenARoleAndProductAccessOnly(t *testing.T) {
	t.Parallel()
	f := newAtlSite(t)
	doc := seatDoc{jira: onJira("10101", "GOP")}
	s := testSession(t, f, doc, []string{"GOP", "OPS"}, nil)
	role := seataccess.Desired{Enabled: true, Level: "10101", Scope: []string{"GOP"}}
	account := run(t, s, seataccess.LaneJira, seataccess.OpCreate, role, seataccess.Observed{}, nil)[0].AccountID
	rec := &seataccess.Record{
		Account: &seataccess.Account{ID: account, State: seataccess.AccountActive},
		Lanes:   map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneJira: {Applied: &role}},
	}
	have := observe(t, s, rec)[seataccess.LaneJira]

	only := seataccess.Desired{Enabled: true}
	doc.jira = onJira("")
	s.seats["sre"] = doc
	plan, err := seataccess.Plan(Vendor{}, seataccess.SeatInput{
		Seat: sre, Record: rec,
		Want: map[seataccess.Lane]seataccess.Desired{seataccess.LaneJira: only},
		Have: map[seataccess.Lane]seataccess.Observed{seataccess.LaneJira: have},
		Options: map[seataccess.Lane]seataccess.Options{seataccess.LaneJira: {Levels: []seataccess.Level{
			{ID: "", Name: "Product access only"}, {ID: "10101", Name: "Developers"},
		}}},
	})
	if err != nil || len(plan[seataccess.LaneJira]) == 0 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if got := plan[seataccess.LaneJira][0].Text; got != "Change it from the Developers project role to Jira "+
		"product access only, removing its project roles" {
		t.Fatalf("step = %q", got)
	}
	for _, step := range plan[seataccess.LaneJira] {
		if _, err := s.Apply(context.Background(), seataccess.ApplyInput{
			Seat: sre, Lane: seataccess.LaneJira, Step: step, Want: only, Have: have, Record: rec,
		}); err != nil {
			t.Fatalf("%s: %v", step.Text, err)
		}
	}
	f.locked(func() {
		if f.actors["GOP/10101"][account] || !slices.Contains(f.grants[account], "jira-software") {
			t.Fatalf("after product access only: actors %v, grants %v", f.actors, f.grants[account])
		}
	})
	if back := observe(t, s, rec)[seataccess.LaneJira]; back.Level != "" || len(back.Scope) != 0 || !back.Enabled {
		t.Fatalf("observed product access only = %+v", back)
	}

	doc.jira = onJira("10101", "GOP")
	s.seats["sre"] = doc
	run(t, s, seataccess.LaneJira, seataccess.OpLevel, role, seataccess.Observed{Exists: true, Enabled: true}, rec)
	f.locked(func() {
		if !f.actors["GOP/10101"][account] {
			t.Fatalf("back to the role: actors %v", f.actors)
		}
	})
}

// CONFLUENCE TOO, on both kinds of site: a space role, or a set of space
// permissions, gives way to product access only.
func TestConfluenceGivesWayToProductAccessOnly(t *testing.T) {
	t.Parallel()
	for _, rolesMode := range []string{"ROLES", ""} {
		t.Run("mode "+rolesMode, func(t *testing.T) {
			t.Parallel()
			f := newAtlSite(t)
			f.rolesMode = rolesMode
			level := "r-collab"
			if rolesMode == "" {
				level = "create:page read:space"
			}
			doc := seatDoc{confluence: onConfluence(level, "ENG")}
			s := testSession(t, f, doc, nil, []string{"ENG"})
			held := seataccess.Desired{Enabled: true, Level: level, Scope: []string{"ENG"}}
			account := run(t, s, seataccess.LaneConfluence, seataccess.OpCreate, held, seataccess.Observed{}, nil)[0].AccountID
			doc.confluence = onConfluence("")
			s.seats["sre"] = doc
			s.cache = newCache()
			run(t, s, seataccess.LaneConfluence, seataccess.OpLevel, seataccess.Desired{Enabled: true},
				seataccess.Observed{Exists: true, Enabled: true, Level: level, Scope: []string{"ENG"}}, nil)
			f.locked(func() {
				if f.spaceRoles["1001"][account] != "" || len(f.spacePerms["1001"][account]) != 0 {
					t.Fatalf("after product access only: role %q, permissions %+v",
						f.spaceRoles["1001"][account], f.spacePerms["1001"][account])
				}
			})
		})
	}
}

// A TOKEN REVOKED, EXPIRED OR INSIDE ITS RENEWAL WINDOW IS GONE: the vendor
// says so over the ledger's sealed record, so the plan mints a replacement
// while a lane is on, before an agent goes quiet.
func TestATokenGoneOrDueForRenewalIsMintedAgainAtAtlassian(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	label := tokenLabel("sre", lanes{jira: true}) + "-1700000000"
	for _, tc := range []struct {
		name   string
		tokens []*atlToken
		gone   bool
	}{
		{"revoked", nil, true},
		{"expired", []*atlToken{{APIToken: atlassian.APIToken{ID: "t1", Label: label,
			Expiry: clock.Add(-time.Hour).Format(time.RFC3339)}}}, true},
		{"expiring in a week", []*atlToken{{APIToken: atlassian.APIToken{ID: "t1", Label: label,
			Expiry: clock.Add(7 * 24 * time.Hour).Format(time.RFC3339)}}}, true},
		{"live for months", []*atlToken{{APIToken: atlassian.APIToken{ID: "t1", Label: label,
			Expiry: clock.Add(200 * 24 * time.Hour).Format(time.RFC3339)}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAtlSite(t)
			f.accounts["acc-9"] = &atlassian.ServiceAccount{ID: "acc-9", DisplayName: "SRE (crewlet:sre)"}
			f.tokens["acc-9"] = tc.tokens
			s := testSession(t, f, seatDoc{jira: onJira("10101", "GOP")}, []string{"GOP"}, nil)
			s.now = func() time.Time { return clock }
			want := seataccess.Desired{Enabled: true, Level: "10101", Scope: []string{"GOP"}}
			rec := &seataccess.Record{
				Account:    &seataccess.Account{ID: "acc-9", State: seataccess.AccountActive},
				Credential: &seataccess.Credential{Variable: "ATLASSIAN_TOKEN_SRE"},
				Lanes:      map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneJira: {Applied: &want}},
			}
			obs := observe(t, s, rec)
			if j := obs[seataccess.LaneJira]; j.CredentialGone != tc.gone || (j.Credential == nil) != tc.gone {
				t.Fatalf("observed gone %v, credential %+v; want gone %v", j.CredentialGone, j.Credential, tc.gone)
			}
			plan, err := seataccess.Plan(Vendor{}, seataccess.SeatInput{
				Seat: sre, Record: rec,
				Want: map[seataccess.Lane]seataccess.Desired{seataccess.LaneJira: want},
				Have: obs,
			})
			if err != nil {
				t.Fatal(err)
			}
			minted := slices.ContainsFunc(plan[seataccess.LaneJira], func(s seataccess.Step) bool {
				return s.Op == seataccess.OpMint
			})
			if minted != tc.gone {
				t.Fatalf("plan mints %v, want %v: %+v", minted, tc.gone, plan[seataccess.LaneJira])
			}
		})
	}
}

// OFF SAYS WHAT HAPPENED WHEN THE PRODUCT IS NOT CONNECTED: no role could be
// read or removed, and the token losing the product's scopes is the off.
func TestOffSaysSoWhenTheProductIsNotConnected(t *testing.T) {
	t.Parallel()
	f := newAtlSite(t)
	f.accounts["acc-9"] = &atlassian.ServiceAccount{ID: "acc-9", DisplayName: "SRE (crewlet:sre)"}
	s := testSession(t, f, seatDoc{jira: &config.RoleJira{}}, []string{"GOP"}, nil)
	s.jira, s.jiraMissing = nil, "jira: no instance url"
	rec := &seataccess.Record{Account: &seataccess.Account{ID: "acc-9", State: seataccess.AccountActive}}
	off := run(t, s, seataccess.LaneJira, seataccess.OpDisable, seataccess.Desired{},
		seataccess.Observed{Exists: true, Enabled: true}, rec)
	if !strings.HasPrefix(off[0].Did, "Jira is not connected") || strings.Contains(off[0].Did, "Removed") {
		t.Fatalf("off said %q", off[0].Did)
	}
}

// A PERSONAL SPACE'S KEY KEEPS ITS CASE, because Confluence matches `~` keys
// case-sensitively; a global space's key is upper-cased.
func TestAPersonalSpaceKeyKeepsItsCase(t *testing.T) {
	t.Parallel()
	got := normalKeys([]string{" ~alice ", "eng", "~557058:AbC"})
	if !slices.Equal(got, []string{"ENG", "~557058:AbC", "~alice"}) {
		t.Fatalf("normalKeys = %v", got)
	}
}
