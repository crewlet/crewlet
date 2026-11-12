package cloudflare_test

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

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// The fake's fixed identities. Obviously fake values throughout.
const (
	testAccount     = "0123456789abcdef0123456789abcdef"
	provisioner     = "fake-provisioner-token"
	provisionerID   = "prov-token-id"
	zoneExampleID   = "zone-example-id"
	zoneOtherID     = "zone-other-id"
	zoneUnlistedID  = "zone-unlisted-id"
	zoneExampleName = "example.com"
	zoneOtherName   = "example.org"
)

// fakeEnvelope is a refusal as Cloudflare writes one, chained errors included.
type fakeEnvelope struct {
	status int
	errors []cloudflare.Message
}

// malformedToken is Cloudflare's answer to an Authorization header holding no
// token it can read.
var malformedToken = fakeEnvelope{status: http.StatusBadRequest, errors: []cloudflare.Message{{
	Code: 6003, Message: "Invalid request headers",
	Chain: []cloudflare.Message{{Code: 6111, Message: "Invalid format for Authorization header"}},
}}}

// fakeGroup is one permission group the fake account lists.
type fakeGroup struct {
	ID, Name   string
	Scopes     []string
	Selectable *bool
}

// fakeToken is one account token the fake holds.
type fakeToken struct {
	ID, Name, Status, Value string
	ExpiresOn               string
	Policies                []cloudflare.Policy
}

// fakeCloudflare is one account at Cloudflare: its permission groups, zones and
// account tokens, behind the v4 API's envelope, paging and errors.
type fakeCloudflare struct {
	mu sync.Mutex

	groups []fakeGroup
	zones  []cloudflare.Zone
	tokens map[string]*fakeToken
	seq    int

	provisionerStatus  string
	provisionerExpires string
	provisionerOwn     []cloudflare.Policy

	// refuse answers a route with a status and an envelope error, by route
	// key (verify, groups, zones, create, get, update, roll, delete).
	refuse map[string]int
	// refuseID answers any route naming one token with a status.
	refuseID map[string]int
	// envelope answers a route with this status and error envelope, by route
	// key, as Cloudflare answers a token it cannot read.
	envelope map[string]fakeEnvelope
	// limited answers a route 429 this many times first, with retryAfter.
	limited    map[string]int
	retryAfter string
	// emptyValue answers a create or a roll with no value.
	emptyValue bool

	writes   []string
	requests []string
	pageSize []int
	rw       *httpxtest.Rewriter
}

// zoneGroups and accountGroups are the scopes of every group a level or the
// provisioner names.
var (
	zoneGroups = []string{
		"Zone Read", "Zone Settings Read", "Analytics Read", "DNS Read", "Cache Settings Read",
		"Firewall Services Read", "Zone WAF Read", "SSL and Certificates Read", "Cache Purge",
		"Cache Settings Write", "Zone Settings Write", "Response Compression Write", "Zone WAF Write",
		"Zone Transform Rules Read", "Zone Transform Rules Write",
	}
	accountGroups = []string{
		"Account Analytics Read", "Account Settings Read", "Account API Tokens Write", "Account API Tokens Read",
		"Account Rule Lists Read", "Account Rule Lists Write",
	}
)

func groupID(name string) string { return "pg-" + strings.ReplaceAll(strings.ToLower(name), " ", "-") }

func newFakeCloudflare() *fakeCloudflare {
	f := &fakeCloudflare{
		tokens: map[string]*fakeToken{}, refuse: map[string]int{}, refuseID: map[string]int{},
		envelope:          map[string]fakeEnvelope{},
		limited:           map[string]int{},
		provisionerStatus: cloudflare.StatusActive, retryAfter: "0",
		zones: []cloudflare.Zone{
			{ID: zoneExampleID, Name: zoneExampleName, Status: "active"},
			{ID: zoneOtherID, Name: zoneOtherName, Status: "active"},
			{ID: zoneUnlistedID, Name: "example.net", Status: "active"},
		},
	}
	no := false
	for _, name := range zoneGroups {
		f.groups = append(f.groups, fakeGroup{ID: groupID(name), Name: name,
			Scopes: []string{"com.cloudflare.api.account.zone"}})
	}
	for _, name := range accountGroups {
		f.groups = append(f.groups, fakeGroup{ID: groupID(name), Name: name,
			Scopes: []string{"com.cloudflare.api.account"}})
	}
	// A group a token cannot be granted, carrying a name a level uses: it is
	// skipped rather than granted.
	f.groups = append(f.groups, fakeGroup{ID: "pg-legacy-cache-purge", Name: "Cache Purge",
		Scopes: []string{"com.cloudflare.api.account.zone"}, Selectable: &no})
	// The provisioner holds every group it needs.
	var all []cloudflare.GroupRef
	for _, g := range f.groups {
		if g.Selectable == nil {
			all = append(all, cloudflare.GroupRef{ID: g.ID})
		}
	}
	f.provisionerOwn = []cloudflare.Policy{{Effect: "allow", PermissionGroups: all,
		Resources: map[string]any{"com.cloudflare.api.account." + testAccount: "*"}}}
	return f
}

// server starts the fake and returns a client transport that reaches it in
// place of api.cloudflare.com.
func (f *fakeCloudflare) server(t testing.TB) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.rw = httpxtest.Rewrite(t, srv)
	return &http.Client{Transport: f.rw}
}

// onlyCloudflare fails a test whose client addressed anything but
// Cloudflare's API host, which is where the provisioner token may go.
func (f *fakeCloudflare) onlyCloudflare(t testing.TB) {
	t.Helper()
	for _, host := range f.rw.Hosts() {
		if host != "https://api.cloudflare.com" {
			t.Errorf("the client addressed %s", host)
		}
	}
}

func (f *fakeCloudflare) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	f.requests = append(f.requests, r.Method+" "+path)
	if r.Header.Get("Authorization") != "Bearer "+provisioner {
		f.fail(w, http.StatusUnauthorized, 10000, "Authentication error")
		return
	}
	route, id := f.route(r.Method, path)
	if route == "" {
		f.fail(w, http.StatusNotFound, 7003, "Could not route to "+path)
		return
	}
	if n := f.limited[route]; n > 0 {
		f.limited[route] = n - 1
		w.Header().Set("Retry-After", f.retryAfter)
		f.fail(w, http.StatusTooManyRequests, 971, "Please wait and consider throttling your request speed")
		return
	}
	if e, ok := f.envelope[route]; ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(e.status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false, "errors": e.errors, "messages": []any{}, "result": nil,
		})
		return
	}
	if status := f.refuse[route]; status != 0 {
		f.fail(w, status, 9109, "Unauthorized to access requested resource")
		return
	}
	if status := f.refuseID[id]; id != "" && status != 0 {
		f.fail(w, status, 10001, "Internal error")
		return
	}
	switch route {
	case "verify":
		f.ok(w, map[string]any{"id": provisionerID, "status": f.provisionerStatus,
			"expires_on": f.provisionerExpires}, nil)
	case "groups":
		rows := make([]any, 0, len(f.groups))
		for _, g := range f.groups {
			row := map[string]any{"id": g.ID, "name": g.Name, "scopes": g.Scopes}
			if g.Selectable != nil {
				row["is_selectable"] = *g.Selectable
			}
			rows = append(rows, row)
		}
		f.page(w, r, rows)
	case "zones":
		if r.URL.Query().Get("account.id") != testAccount {
			f.fail(w, http.StatusBadRequest, 1001, "account.id is required")
			return
		}
		rows := make([]any, 0, len(f.zones))
		for _, z := range f.zones {
			rows = append(rows, map[string]any{"id": z.ID, "name": z.Name, "status": z.Status})
		}
		f.page(w, r, rows)
	case "list":
		ids := slices.Sorted(maps.Keys(f.tokens))
		rows := []any{map[string]any{"id": provisionerID, "name": "crewlet-acme-provisioner",
			"status": f.provisionerStatus, "policies": f.provisionerOwn}}
		for _, id := range ids {
			rows = append(rows, f.tokens[id].wire(false))
		}
		f.page(w, r, rows)
	case "get":
		if id == provisionerID {
			f.ok(w, map[string]any{"id": provisionerID, "name": "provisioner", "status": f.provisionerStatus,
				"policies": f.provisionerOwn}, nil)
			return
		}
		tok, ok := f.tokens[id]
		if !ok {
			f.fail(w, http.StatusNotFound, 1003, "Token not found")
			return
		}
		f.ok(w, tok.wire(false), nil)
	case "create":
		spec, bad := f.decodeSpec(r)
		if bad != "" {
			f.fail(w, http.StatusBadRequest, 1001, bad)
			return
		}
		f.seq++
		tok := &fakeToken{
			ID: fmt.Sprintf("tok-%d", f.seq), Name: spec.Name, Status: cloudflare.StatusActive,
			ExpiresOn: spec.ExpiresOn, Policies: spec.Policies, Value: f.value(),
		}
		if spec.Status != "" {
			tok.Status = spec.Status
		}
		f.tokens[tok.ID] = tok
		f.writes = append(f.writes, "create "+tok.ID)
		f.ok(w, tok.wire(true), nil)
	case "update":
		tok, ok := f.tokens[id]
		if !ok {
			f.fail(w, http.StatusNotFound, 1003, "Token not found")
			return
		}
		spec, bad := f.decodeSpec(r)
		if bad != "" {
			f.fail(w, http.StatusBadRequest, 1001, bad)
			return
		}
		tok.Name, tok.Policies, tok.ExpiresOn = spec.Name, spec.Policies, spec.ExpiresOn
		if spec.Status != "" {
			tok.Status = spec.Status
		}
		f.writes = append(f.writes, "update "+id)
		f.ok(w, tok.wire(false), nil)
	case "roll":
		tok, ok := f.tokens[id]
		if !ok {
			f.fail(w, http.StatusNotFound, 1003, "Token not found")
			return
		}
		tok.Value = f.value()
		f.writes = append(f.writes, "roll "+id)
		f.ok(w, tok.Value, nil)
	case "delete":
		if _, ok := f.tokens[id]; !ok {
			f.fail(w, http.StatusNotFound, 1003, "Token not found")
			return
		}
		delete(f.tokens, id)
		f.writes = append(f.writes, "delete "+id)
		f.ok(w, map[string]any{"id": id}, nil)
	}
}

// route is a request's route key and the token ID it names.
func (f *fakeCloudflare) route(method, path string) (string, string) {
	if path == "/zones" && method == http.MethodGet {
		return "zones", ""
	}
	rest, ok := strings.CutPrefix(path, "/accounts/"+testAccount+"/tokens")
	if !ok {
		return "", ""
	}
	switch {
	case rest == "/verify" && method == http.MethodGet:
		return "verify", ""
	case rest == "/permission_groups" && method == http.MethodGet:
		return "groups", ""
	case rest == "" && method == http.MethodPost:
		return "create", ""
	case rest == "" && method == http.MethodGet:
		return "list", ""
	case strings.HasSuffix(rest, "/value") && method == http.MethodPut:
		return "roll", strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/value")
	case strings.Count(rest, "/") == 1:
		id := strings.TrimPrefix(rest, "/")
		switch method {
		case http.MethodGet:
			return "get", id
		case http.MethodPut:
			return "update", id
		case http.MethodDelete:
			return "delete", id
		}
	}
	return "", ""
}

// spec is a create or update body as Cloudflare reads it.
type spec struct {
	Name      string              `json:"name"`
	Policies  []cloudflare.Policy `json:"policies"`
	ExpiresOn string              `json:"expires_on"`
	Status    string              `json:"status"`
	Condition json.RawMessage     `json:"condition"`
}

// decodeSpec reads a body and refuses what Cloudflare refuses: no name, no
// policy, a policy with no resources, and (this engine's own rule, held
// here) any IP condition.
func (f *fakeCloudflare) decodeSpec(r *http.Request) (spec, string) {
	var s spec
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		return s, "malformed body"
	}
	switch {
	case s.Name == "":
		return s, "name is required"
	case len(s.Policies) == 0:
		return s, "policies are required"
	case len(s.Condition) > 0:
		return s, "the engine must never set an IP condition"
	}
	for _, p := range s.Policies {
		if len(p.Resources) == 0 || len(p.PermissionGroups) == 0 {
			return s, "a policy needs resources and permission groups"
		}
		for _, g := range p.PermissionGroups {
			if !slices.ContainsFunc(f.groups, func(fg fakeGroup) bool {
				return fg.ID == g.ID && (fg.Selectable == nil || *fg.Selectable)
			}) {
				return s, "permission group " + g.ID + " cannot be granted"
			}
		}
	}
	return s, ""
}

func (f *fakeCloudflare) value() string {
	if f.emptyValue {
		return ""
	}
	f.seq++
	return fmt.Sprintf("cfat_fake_value_%d", f.seq)
}

func (t *fakeToken) wire(withValue bool) map[string]any {
	out := map[string]any{"id": t.ID, "name": t.Name, "status": t.Status, "policies": t.Policies}
	if t.ExpiresOn != "" {
		out["expires_on"] = t.ExpiresOn
	}
	if withValue {
		out["value"] = t.Value
	}
	return out
}

// page serves one page of rows as Cloudflare pages a listing.
func (f *fakeCloudflare) page(w http.ResponseWriter, r *http.Request, rows []any) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 20
	}
	f.pageSize = append(f.pageSize, perPage)
	start := min((page-1)*perPage, len(rows))
	end := min(start+perPage, len(rows))
	pages := (len(rows) + perPage - 1) / perPage
	f.ok(w, rows[start:end], map[string]any{
		"page": page, "per_page": perPage, "count": end - start,
		"total_count": len(rows), "total_pages": pages,
	})
}

func (f *fakeCloudflare) ok(w http.ResponseWriter, result any, info map[string]any) {
	body := map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result}
	if info != nil {
		body["result_info"] = info
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeCloudflare) fail(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": false, "errors": []any{map[string]any{"code": code, "message": message}},
		"messages": []any{}, "result": nil,
	})
}

// token is a copy of one token the fake holds.
func (f *fakeCloudflare) token(id string) (fakeToken, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok, ok := f.tokens[id]
	if !ok {
		return fakeToken{}, false
	}
	return *tok, true
}

// wrote is every write the fake took, in order.
func (f *fakeCloudflare) wrote() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// edit changes one token as a person at Cloudflare would.
func (f *fakeCloudflare) edit(id string, change func(*fakeToken)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.tokens[id])
}

// set changes the fake's own state under its lock.
func (f *fakeCloudflare) set(change func(*fakeCloudflare)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// rfc3339 is a time as Cloudflare writes one.
func rfc3339(at time.Time) string { return at.UTC().Format(time.RFC3339) }
