package digitalocean

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// fakeToken is an obviously fake personal access token, distinct per seed: the
// seed's hex digits, repeated.
func fakeToken(seed byte) string {
	return "dop_v1_" + strings.Repeat(hex.EncodeToString([]byte{seed}), 32)
}

// profile is what one fake token is: its user, team and scopes.
type profile struct {
	name, team, teamUUID, status string
	scopes                       []string
	revoked                      bool
	refuseRevoke                 bool
}

// fakeDO answers DigitalOcean's API as the live run measured it: 403 for a
// scope a token lacks, and for one it holds 200 to a read, 404 to an id that
// cannot exist, 400 to a malformed body and 422 to an empty one.
type fakeDO struct {
	mu       sync.Mutex
	tokens   map[string]*profile
	routes   []route
	requests []string
	revoked  []string
	rw       *httpxtest.Rewriter
	// answer overrides one route's status where set, by scope.
	answer map[string]int
	// revokeDown answers every revoke 503, as an outage would.
	revokeDown bool
	// inv is the team's resources as the listings answer them; nil has none.
	inv *inventory
}

// route is one request shape the checks send, with the scope it needs.
type route struct {
	method string
	path   *regexp.Regexp
	scope  string
}

var (
	uuidPattern    = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	invalidPattern = regexp.MustCompile(`crewlet-check-[0-9a-f]{12}\.invalid`)
)

// routesOf is every check's request shape, its random ids matched by pattern.
func routesOf(sets ...[]check) []route {
	var out []route
	for _, set := range sets {
		for _, ch := range set {
			path, _, _ := strings.Cut(ch.path(), "?")
			path = uuidPattern.ReplaceAllString(path, "UUIDX")
			path = invalidPattern.ReplaceAllString(path, "DOMAINX")
			quoted := regexp.QuoteMeta(path)
			quoted = strings.ReplaceAll(quoted, "UUIDX", uuidPattern.String())
			quoted = strings.ReplaceAll(quoted, "DOMAINX", invalidPattern.String())
			out = append(out, route{method: ch.method, path: regexp.MustCompile("^" + quoted + "$"), scope: ch.scope})
		}
	}
	return out
}

func newFakeDO() *fakeDO {
	return &fakeDO{
		tokens: map[string]*profile{},
		routes: routesOf(readChecks, separatorChecks, forbiddenChecks, administerChecks),
		answer: map[string]int{},
	}
}

// add registers a token for the team "Example Team".
func (f *fakeDO) add(token string, scopes []string) *profile {
	p := &profile{name: "Owner", team: "Example Team", teamUUID: "11111111-2222-4333-a444-555555555555",
		status: "active", scopes: scopes}
	f.tokens[token] = p
	return p
}

// server starts the fake and returns a client transport that reaches it in
// place of DigitalOcean.
func (f *fakeDO) server(t testing.TB) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.rw = httpxtest.Rewrite(t, srv)
	return &http.Client{Transport: f.rw}
}

// onlyDigitalOcean fails a test whose client addressed anything but
// DigitalOcean's API and OAuth hosts.
func (f *fakeDO) onlyDigitalOcean(t testing.TB) {
	t.Helper()
	for _, host := range f.rw.Hosts() {
		if host != "https://api.digitalocean.com" && host != "https://cloud.digitalocean.com" {
			t.Errorf("the client addressed %s", host)
		}
	}
}

func (f *fakeDO) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	p, ok := f.tokens[token]
	if r.URL.Path == "/v1/oauth/revoke" {
		form, _ := url.ParseQuery(string(body))
		switch {
		case f.revokeDown:
			fail(w, http.StatusServiceUnavailable, "service_unavailable")
		case !ok || p.revoked || form.Get("token") != token:
			fail(w, http.StatusUnauthorized, "unauthorized")
		case p.refuseRevoke:
			fail(w, http.StatusBadRequest, "invalid_request")
		default:
			p.revoked = true
			f.revoked = append(f.revoked, token)
			_, _ = w.Write([]byte("{}"))
		}
		return
	}
	if !ok || p.revoked {
		fail(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v2/account" {
		if !slices.Contains(p.scopes, "account:read") {
			fail(w, http.StatusForbidden, "Forbidden")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{
			"name": p.name, "email": "owner@example.com", "uuid": "u-1", "status": p.status,
			"team": map[string]any{"uuid": p.teamUUID, "name": p.team}}})
		return
	}
	if f.inv != nil && r.Method == http.MethodGet && f.inv.serve(w, r, p) {
		return
	}
	for _, rt := range f.routes {
		if rt.method != r.Method || !rt.path.MatchString(r.URL.Path) {
			continue
		}
		if code, set := f.answer[rt.scope]; set {
			w.WriteHeader(code)
			return
		}
		if !slices.Contains(p.scopes, rt.scope) {
			fail(w, http.StatusForbidden, "Forbidden")
			return
		}
		switch {
		case string(body) == "{":
			fail(w, http.StatusBadRequest, "bad_request")
		case string(body) == "{}":
			fail(w, http.StatusUnprocessableEntity, "unprocessable_entity")
		case r.Method == http.MethodGet && !uuidPattern.MatchString(r.URL.Path):
			_, _ = w.Write([]byte("{}"))
		default:
			fail(w, http.StatusNotFound, "not_found")
		}
		return
	}
	fail(w, http.StatusNotFound, "not_found")
}

func fail(w http.ResponseWriter, status int, id string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "message": "refused by the fake"})
}

// scopesOf is a level's grants, plus extra.
func scopesOf(level string, extra ...string) []string {
	return append(Grants(level), extra...)
}
