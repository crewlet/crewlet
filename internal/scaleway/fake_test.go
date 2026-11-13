package scaleway_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/scaleway"
)

// The fake's fixed identities. Obviously fake values throughout.
const (
	testOrg        = "11111111-2222-4333-8444-555555555555"
	otherOrg       = "99999999-2222-4333-8444-555555555555"
	projectWeb     = "aaaaaaaa-0000-4000-8000-000000000001"
	projectData    = "aaaaaaaa-0000-4000-8000-000000000002"
	projectUnnamed = "aaaaaaaa-0000-4000-8000-000000000003"
	provAccess     = "SCWPROVISIONERKEY001"
	provSecret     = "00000000-0000-4000-8000-00000000c0de"
	provAppID      = "bbbbbbbb-0000-4000-8000-000000000001"
)

// fakeApp, fakePolicy and fakeKey are what the fake organization holds.
type fakeApp struct {
	ID, Name, Description, Org string
	CreatedAt                  time.Time
}

type fakePolicy struct {
	ID, Name, AppID string
	Rules           []scaleway.Rule
	CreatedAt       time.Time
}

type fakeKey struct {
	Access, Secret, AppID, UserID, Description, DefaultProject string
	CreatedAt, ExpiresAt                                       time.Time
}

// fakeScaleway is one organization at Scaleway: its applications, policies,
// keys, permission sets and projects, behind IAM's paging and errors.
type fakeScaleway struct {
	mu sync.Mutex

	apps     map[string]*fakeApp
	policies map[string]*fakePolicy
	keys     map[string]*fakeKey
	sets     map[string]string
	projects []scaleway.Project
	// projectFields is what every project's row carries beside its ID and
	// name: the fields a read of the projects never decodes.
	projectFields map[string]any
	seq           int
	// now is the fake's clock, which a test moves through [fakeScaleway.edit].
	now time.Time

	// maxLifetime refuses a key whose expiry is further away; zero is none.
	maxLifetime time.Duration
	// quotaApps refuses a new application beyond this many; zero is none.
	quotaApps int
	// refuse answers a route with a status, by route key.
	refuse map[string]int
	// refuseDirectKey answers 403 to a read of a key by its access key.
	refuseDirectKey bool
	// limited answers a route 429 this many times first.
	limited map[string]int
	// noSecret answers a key create with no secret.
	noSecret bool

	writes   []string
	requests []string
	rw       *httpxtest.Rewriter
}

func newFakeScaleway() *fakeScaleway {
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	f := &fakeScaleway{
		apps: map[string]*fakeApp{}, policies: map[string]*fakePolicy{}, keys: map[string]*fakeKey{},
		sets: map[string]string{}, refuse: map[string]int{}, limited: map[string]int{},
		projects: []scaleway.Project{
			{ID: projectWeb, Name: "web"}, {ID: projectData, Name: "data"}, {ID: projectUnnamed},
		},
		now: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
	}
	for _, level := range scaleway.Levels() {
		sets, _ := scaleway.ProjectSets(level)
		for _, name := range sets {
			f.sets[name] = "projects"
		}
	}
	for _, name := range append(scaleway.OrgSets(), "IAMApplicationManager", "IAMPolicyManager",
		"IAMManager", "OrganizationManager", "BillingManager") {
		f.sets[name] = "organization"
	}
	f.apps[provAppID] = &fakeApp{ID: provAppID, Name: "crewlet-provisioner", Org: testOrg, CreatedAt: created}
	org := testOrg
	f.policies["pol-provisioner"] = &fakePolicy{
		ID: "pol-provisioner", Name: "crewlet-provisioner", AppID: provAppID, CreatedAt: created,
		Rules: []scaleway.Rule{{
			PermissionSetNames: []string{"IAMApplicationManager", "IAMPolicyManager", "ProjectReadOnly"},
			OrganizationID:     &org,
		}},
	}
	f.keys[provAccess] = &fakeKey{Access: provAccess, Secret: provSecret, AppID: provAppID,
		Description: "provisioner", CreatedAt: created, ExpiresAt: created.Add(365 * 24 * time.Hour)}
	return f
}

// server starts the fake and returns a client transport that reaches it in
// place of api.scaleway.com.
func (f *fakeScaleway) server(t testing.TB) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.rw = httpxtest.Rewrite(t, srv)
	return &http.Client{Transport: f.rw}
}

// onlyScaleway fails a test whose client addressed anything but Scaleway's
// API host, which is where the provisioner key may go.
func (f *fakeScaleway) onlyScaleway(t testing.TB) {
	t.Helper()
	for _, host := range f.rw.Hosts() {
		if host != "https://api.scaleway.com" {
			t.Errorf("the client addressed %s", host)
		}
	}
}

func (f *fakeScaleway) fail(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": kind, "message": message})
}

func (f *fakeScaleway) ok(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// page answers one page of rows under field, with the collection's total.
func (f *fakeScaleway) page(w http.ResponseWriter, r *http.Request, field string, rows []any) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		f.fail(w, http.StatusBadRequest, "invalid_arguments", "page_size must be between 1 and 100")
		return
	}
	start := min((page-1)*size, len(rows))
	end := min(start+size, len(rows))
	f.ok(w, map[string]any{field: rows[start:end], "total_count": len(rows)})
}

// route is a request's route key and the ID it names.
func route(method, path string) (string, string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/account/v3/projects" && method == http.MethodGet:
		return "projects", ""
	case len(parts) < 3 || parts[0] != "iam" || parts[1] != "v1alpha1":
		return "", ""
	}
	id := ""
	if len(parts) == 4 {
		id = parts[3]
	}
	switch parts[2] + " " + method {
	case "api-keys GET":
		if id != "" {
			return "key", id
		}
		return "keys", ""
	case "api-keys POST":
		return "create key", ""
	case "api-keys PATCH":
		return "patch key", id
	case "api-keys DELETE":
		return "delete key", id
	case "applications GET":
		if id != "" {
			return "app", id
		}
		return "apps", ""
	case "applications POST":
		return "create app", ""
	case "applications DELETE":
		return "delete app", id
	case "policies GET":
		return "policies", ""
	case "policies POST":
		return "create policy", ""
	case "policies DELETE":
		return "delete policy", id
	case "rules GET":
		return "rules", ""
	case "rules PUT":
		return "set rules", ""
	case "permission-sets GET":
		return "sets", ""
	}
	return "", ""
}

func (f *fakeScaleway) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	now := f.now
	if !f.authenticated(r.Header.Get("X-Auth-Token"), now) {
		f.fail(w, http.StatusUnauthorized, "denied_authentication", "authentication is denied")
		return
	}
	name, id := route(r.Method, r.URL.Path)
	if name == "" {
		f.fail(w, http.StatusNotFound, "not_found", "no route to "+r.URL.Path)
		return
	}
	if n := f.limited[name]; n > 0 {
		f.limited[name] = n - 1
		w.Header().Set("Retry-After", "0")
		f.fail(w, http.StatusTooManyRequests, "too_many_requests", "slow down")
		return
	}
	if status := f.refuse[name]; status != 0 {
		f.fail(w, status, "permissions_denied", "insufficient permissions")
		return
	}
	q := r.URL.Query()
	if slices.Contains([]string{"keys", "apps", "policies", "sets", "projects"}, name) && q.Get("organization_id") != testOrg {
		f.fail(w, http.StatusBadRequest, "invalid_arguments", "organization_id is required")
		return
	}
	switch name {
	case "key":
		if f.refuseDirectKey {
			f.fail(w, http.StatusForbidden, "permissions_denied", "insufficient permissions")
			return
		}
		k, ok := f.keys[id]
		if !ok {
			f.fail(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		f.ok(w, k.wire(false))
	case "keys":
		var rows []any
		for _, access := range slices.Sorted(maps.Keys(f.keys)) {
			k := f.keys[access]
			if b := q.Get("bearer_id"); b != "" && k.AppID != b {
				continue
			}
			if a := q["access_keys"]; len(a) > 0 && !slices.Contains(a, k.Access) {
				continue
			}
			rows = append(rows, k.wire(false))
		}
		f.page(w, r, "api_keys", rows)
	case "create key":
		var body struct {
			ApplicationID    string `json:"application_id"`
			Description      string `json:"description"`
			DefaultProjectID string `json:"default_project_id"`
			ExpiresAt        string `json:"expires_at"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := f.apps[body.ApplicationID]; !ok {
			f.fail(w, http.StatusNotFound, "not_found", "application not found")
			return
		}
		expires, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil {
			f.fail(w, http.StatusBadRequest, "invalid_arguments", "expires_at is not a time")
			return
		}
		if f.maxLifetime > 0 && expires.Sub(now) > f.maxLifetime {
			f.fail(w, http.StatusBadRequest, "invalid_arguments",
				"expires_at is beyond the maximum API key expiration duration of the organization")
			return
		}
		f.seq++
		k := &fakeKey{
			Access: fmt.Sprintf("SCWFAKE%013d", f.seq), Secret: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq),
			AppID: body.ApplicationID, Description: body.Description, DefaultProject: body.DefaultProjectID,
			CreatedAt: now.Add(time.Duration(f.seq) * time.Second), ExpiresAt: expires,
		}
		f.keys[k.Access] = k
		f.writes = append(f.writes, "create key "+k.Access)
		f.ok(w, k.wire(!f.noSecret))
	case "patch key":
		k, ok := f.keys[id]
		if !ok {
			f.fail(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		var body struct {
			DefaultProjectID string `json:"default_project_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		k.DefaultProject = body.DefaultProjectID
		f.writes = append(f.writes, "patch key "+id)
		f.ok(w, k.wire(false))
	case "delete key":
		if _, ok := f.keys[id]; !ok {
			f.fail(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		delete(f.keys, id)
		f.writes = append(f.writes, "delete key "+id)
		w.WriteHeader(http.StatusNoContent)
	case "app":
		a, ok := f.apps[id]
		if !ok {
			f.fail(w, http.StatusNotFound, "not_found", "application not found")
			return
		}
		f.ok(w, a.wire())
	case "apps":
		var rows []any
		for _, id := range slices.Sorted(maps.Keys(f.apps)) {
			rows = append(rows, f.apps[id].wire())
		}
		f.page(w, r, "applications", rows)
	case "create app":
		if f.quotaApps > 0 && len(f.apps) >= f.quotaApps {
			f.fail(w, http.StatusForbidden, "quotas_exceeded", "quota exceeded for applications")
			return
		}
		var body struct {
			Name, Description string
			OrganizationID    string `json:"organization_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.seq++
		a := &fakeApp{ID: fmt.Sprintf("cccccccc-0000-4000-8000-%012d", f.seq), Name: body.Name,
			Description: body.Description, Org: body.OrganizationID, CreatedAt: now}
		f.apps[a.ID] = a
		f.writes = append(f.writes, "create app "+a.Name)
		f.ok(w, a.wire())
	case "delete app":
		if _, ok := f.apps[id]; !ok {
			f.fail(w, http.StatusNotFound, "not_found", "application not found")
			return
		}
		delete(f.apps, id)
		// SCALEWAY TAKES THE APPLICATION'S KEYS WITH IT.
		for access, k := range f.keys {
			if k.AppID == id {
				delete(f.keys, access)
			}
		}
		f.writes = append(f.writes, "delete app "+id)
		w.WriteHeader(http.StatusNoContent)
	case "policies":
		var rows []any
		for _, id := range slices.Sorted(maps.Keys(f.policies)) {
			p := f.policies[id]
			if a := q.Get("application_ids"); a != "" && p.AppID != a {
				continue
			}
			rows = append(rows, map[string]any{"id": p.ID, "name": p.Name, "application_id": p.AppID,
				"created_at": p.CreatedAt.Format(time.RFC3339)})
		}
		f.page(w, r, "policies", rows)
	case "create policy":
		var body struct {
			Name          string              `json:"name"`
			ApplicationID string              `json:"application_id"`
			Rules         []scaleway.RuleSpec `json:"rules"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if bad := f.badRules(body.Rules); bad != "" {
			f.fail(w, http.StatusBadRequest, "invalid_arguments", bad)
			return
		}
		f.seq++
		p := &fakePolicy{ID: fmt.Sprintf("pol-%d", f.seq), Name: body.Name, AppID: body.ApplicationID,
			Rules: rulesOf(body.Rules), CreatedAt: now.Add(time.Duration(f.seq) * time.Second)}
		f.policies[p.ID] = p
		f.writes = append(f.writes, "create policy "+p.Name)
		f.ok(w, map[string]any{"id": p.ID, "name": p.Name, "application_id": p.AppID})
	case "delete policy":
		if _, ok := f.policies[id]; !ok {
			f.fail(w, http.StatusNotFound, "not_found", "policy not found")
			return
		}
		delete(f.policies, id)
		f.writes = append(f.writes, "delete policy "+id)
		w.WriteHeader(http.StatusNoContent)
	case "rules":
		p, ok := f.policies[q.Get("policy_id")]
		if !ok {
			f.fail(w, http.StatusNotFound, "not_found", "policy not found")
			return
		}
		rows := make([]any, 0, len(p.Rules))
		for _, rule := range p.Rules {
			rows = append(rows, rule)
		}
		f.page(w, r, "rules", rows)
	case "set rules":
		var body struct {
			PolicyID string              `json:"policy_id"`
			Rules    []scaleway.RuleSpec `json:"rules"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		p, ok := f.policies[body.PolicyID]
		if !ok {
			f.fail(w, http.StatusNotFound, "not_found", "policy not found")
			return
		}
		if bad := f.badRules(body.Rules); bad != "" {
			f.fail(w, http.StatusBadRequest, "invalid_arguments", bad)
			return
		}
		p.Rules = rulesOf(body.Rules)
		f.writes = append(f.writes, "set rules "+p.ID)
		f.ok(w, map[string]any{"rules": p.Rules})
	case "sets":
		var rows []any
		for _, name := range slices.Sorted(maps.Keys(f.sets)) {
			rows = append(rows, map[string]any{"name": name, "scope_type": f.sets[name]})
		}
		f.page(w, r, "permission_sets", rows)
	case "projects":
		rows := make([]any, 0, len(f.projects))
		for _, p := range f.projects {
			row := map[string]any{"id": p.ID, "name": p.Name}
			maps.Copy(row, f.projectFields)
			rows = append(rows, row)
		}
		f.page(w, r, "projects", rows)
	}
}

// authenticated reports a token that is a live key's secret.
func (f *fakeScaleway) authenticated(token string, now time.Time) bool {
	for _, k := range f.keys {
		if k.Secret == token && (k.ExpiresAt.IsZero() || k.ExpiresAt.After(now)) {
			return true
		}
	}
	return false
}

// badRules is what Scaleway says of rules naming a set it does not list at
// the scope they grant it at, or a rule with both scopes or neither.
func (f *fakeScaleway) badRules(rules []scaleway.RuleSpec) string {
	for _, r := range rules {
		scope := "projects"
		if r.OrganizationID != "" {
			scope = "organization"
		}
		if (r.OrganizationID != "") == (len(r.ProjectIDs) > 0) {
			return "a rule names projects or the organization"
		}
		for _, name := range r.PermissionSetNames {
			if f.sets[name] != scope {
				return "permission set " + name + " is not available at " + scope + " scope"
			}
		}
	}
	return ""
}

func rulesOf(specs []scaleway.RuleSpec) []scaleway.Rule {
	out := make([]scaleway.Rule, 0, len(specs))
	for _, s := range specs {
		r := scaleway.Rule{PermissionSetNames: slices.Clone(s.PermissionSetNames), ProjectIDs: slices.Clone(s.ProjectIDs)}
		if s.OrganizationID != "" {
			org := s.OrganizationID
			r.OrganizationID = &org
		}
		out = append(out, r)
	}
	return out
}

func (k *fakeKey) wire(secret bool) map[string]any {
	out := map[string]any{
		"access_key": k.Access, "description": k.Description, "default_project_id": k.DefaultProject,
		"created_at": k.CreatedAt.Format(time.RFC3339),
	}
	if k.AppID != "" {
		out["application_id"] = k.AppID
	}
	if k.UserID != "" {
		out["user_id"] = k.UserID
	}
	if !k.ExpiresAt.IsZero() {
		out["expires_at"] = k.ExpiresAt.Format(time.RFC3339)
	}
	if secret {
		out["secret_key"] = k.Secret
	}
	return out
}

func (a *fakeApp) wire() map[string]any {
	return map[string]any{"id": a.ID, "name": a.Name, "description": a.Description,
		"organization_id": a.Org, "created_at": a.CreatedAt.Format(time.RFC3339)}
}

// wrote is every write the fake took, in order.
func (f *fakeScaleway) wrote() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// appNamed is the application named name, if one is.
func (f *fakeScaleway) appNamed(name string) (fakeApp, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.apps {
		if a.Name == name {
			return *a, true
		}
	}
	return fakeApp{}, false
}

// policiesOf is every policy on the application, oldest first.
func (f *fakeScaleway) policiesOf(appID string) []fakePolicy {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakePolicy
	for _, p := range f.policies {
		if p.AppID == appID {
			out = append(out, *p)
		}
	}
	slices.SortFunc(out, func(a, b fakePolicy) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// keysOf is every key on the application, oldest first.
func (f *fakeScaleway) keysOf(appID string) []fakeKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeKey
	for _, k := range f.keys {
		if k.AppID == appID {
			out = append(out, *k)
		}
	}
	slices.SortFunc(out, func(a, b fakeKey) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// edit changes one of the fake's records under its lock, as somebody at
// Scaleway would.
func (f *fakeScaleway) edit(fn func(f *fakeScaleway)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}
