package googlecloud_test

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// The fake's fixed identities. Obviously fake values throughout.
const (
	testCompany  = "Acme"
	identity     = "acme-crewlet-id"
	provisioner  = "crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com"
	testProvider = "projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine"
	projectWeb   = "acme-web"
	projectData  = "acme-data"
	projectHalf  = "acme-half"
	projectOther = "acme-other"
)

// googleHosts are the only hosts a client may address.
var googleHosts = []string{
	"https://sts.googleapis.com", "https://iamcredentials.googleapis.com", "https://iam.googleapis.com",
	"https://cloudresourcemanager.googleapis.com", "https://serviceusage.googleapis.com",
}

// testClock is a world's time, which a wait moves forward rather than spends.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *testClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.advance(d)
	return nil
}

// fakeKey is a test federation key and its PEM.
func fakeKey(t testing.TB) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// fakeAccount is one service account of the identity project.
type fakeAccount struct {
	ID, Email, UniqueID, DisplayName, Description string
	Disabled                                      bool
	Policy                                        googlecloud.Policy
}

// fakeProject is one project the provisioner may read.
type fakeProject struct {
	ID, Name, State string
	// Parent is the organization or the folder the project sits under.
	Parent   string
	Policy   googlecloud.Policy
	Services map[string]bool
}

// fakeToken is a token the fake issued, and to whom.
type fakeToken struct {
	principal string
	expires   time.Time
}

// fakeGoogle is Google as the provisioner sees it: STS, impersonation, the
// identity project's accounts, the projects' policies, roles and services,
// holding a write to the provisioner's own conditions as Google does and
// answering read-backs stale for as long as a test asks.
type fakeGoogle struct {
	mu    sync.Mutex
	pub   *ecdsa.PublicKey
	clock *testClock

	tokens   map[string]fakeToken
	accounts map[string]*fakeAccount
	projects map[string]*fakeProject
	roles    map[string]googlecloud.Role
	seq      int

	// lag is how many reads of a resource answer it as it stood before a
	// write, set on each resource as the write lands.
	lag          int
	accountPast  map[string]*fakeAccount
	accountLag   map[string]int
	policyPast   map[string]googlecloud.Policy
	policyLag    map[string]int
	notYet       map[string]int
	notYetFirst  int
	conflicts    map[string]int
	refuse       map[string]int
	quota        int
	mintRefusals int
	// page is how many projects one search page holds; zero is all.
	page int
	// limited answers a route 429 this many times first, by method and path.
	limited map[string]int
	// retryAfter is the wait a 429 names, in seconds; zero is seven.
	retryAfter int
	// unmapped is what every project and service row carries beside the
	// fields a client decodes.
	unmapped map[string]any
	// requests is every request served, by method and path.
	requests []string

	writes   []string
	versions []int
	exchange int
	rw       *httpxtest.Rewriter
}

// newFakeGoogle is a company whose web and data projects are set up for
// Crewlet, whose half project lacks Administer's condition, and whose other
// project is nobody's.
func newFakeGoogle(t testing.TB, pub *ecdsa.PublicKey, clock *testClock) *fakeGoogle {
	t.Helper()
	f := &fakeGoogle{
		pub: pub, clock: clock,
		tokens: map[string]fakeToken{}, accounts: map[string]*fakeAccount{}, projects: map[string]*fakeProject{},
		roles:       map[string]googlecloud.Role{},
		accountPast: map[string]*fakeAccount{}, accountLag: map[string]int{},
		policyPast: map[string]googlecloud.Policy{}, policyLag: map[string]int{},
		notYet: map[string]int{}, conflicts: map[string]int{}, refuse: map[string]int{}, quota: 100,
	}
	f.accounts["100000000000000000001"] = &fakeAccount{
		ID: "crewlet-provisioner", Email: provisioner, UniqueID: "100000000000000000001",
		DisplayName: "Crewlet provisioner",
	}
	for role := range googlecloud.PredefinedRoles() {
		f.roles[role] = googlecloud.Role{Name: role, Title: "Title of " + role, Stage: "GA",
			Permissions: []string{strings.TrimPrefix(role, "roles/") + ".get", "resourcemanager.projects.get"}}
	}
	f.addProject(projectWeb, "Acme Web", 3, true)
	f.addProject(projectData, "Acme Data", 3, true)
	f.addProject(projectHalf, "Acme Half", 2, true)
	f.addProject(projectOther, "Other", 0, false)
	f.projects[identity] = &fakeProject{ID: identity, Name: "Acme Crewlet identities", State: "ACTIVE",
		Services: services("iam", "iamcredentials", "sts", "cloudresourcemanager", "serviceusage")}
	return f
}

// services is each named API enabled.
func services(names ...string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[n+".googleapis.com"] = true
	}
	return out
}

// condition is a level list as the owner's Cloud Shell block writes it.
func condition(roles []string) *googlecloud.Condition {
	quoted := make([]string, len(roles))
	for i, r := range roles {
		quoted[i] = "'" + r + "'"
	}
	return &googlecloud.Condition{Title: "crewlet level", Expression: "api.getAttribute(" +
		"'iam.googleapis.com/modifiedGrantsByRole', []).hasOnly([" + strings.Join(quoted, ", ") + "])"}
}

// addProject is a project holding the provisioner's first lists conditions,
// its Service Usage and Role Viewer, and crewletOperator where custom.
func (f *fakeGoogle) addProject(id, name string, lists int, custom bool) {
	p := &fakeProject{ID: id, Name: name, State: "ACTIVE", Services: services("cloudresourcemanager", "compute")}
	p.Policy.Version = 3
	p.Policy.Bindings = append(p.Policy.Bindings, googlecloud.Binding{Role: "roles/owner",
		Members: []string{"user:owner@acme.example"}})
	for _, list := range googlecloud.Lists(id)[:lists] {
		p.Policy.Bindings = append(p.Policy.Bindings, googlecloud.Binding{
			Role: "roles/resourcemanager.projectIamAdmin", Members: []string{"serviceAccount:" + provisioner},
			Condition: condition(list),
		})
	}
	if lists > 0 {
		for _, role := range []string{"roles/serviceusage.serviceUsageViewer", "roles/iam.roleViewer"} {
			p.Policy.Bindings = append(p.Policy.Bindings, googlecloud.Binding{Role: role,
				Members: []string{"serviceAccount:" + provisioner}})
		}
	}
	p.Policy.Etag = "e0"
	f.projects[id] = p
	if custom {
		f.roles[googlecloud.CustomRole(id)] = googlecloud.Role{Name: googlecloud.CustomRole(id),
			Title: "Crewlet Operator", Stage: "GA", Permissions: googlecloud.CustomRolePermissions()}
	}
}

// server starts the fake and returns a client transport that reaches it in
// place of Google's hosts.
func (f *fakeGoogle) server(t testing.TB) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.rw = httpxtest.Rewrite(t, srv)
	return &http.Client{Transport: f.rw}
}

// onlyGoogle fails a test whose client addressed anything but Google's API
// hosts, which is where the provisioner's token may go.
func (f *fakeGoogle) onlyGoogle(t testing.TB) {
	t.Helper()
	for _, host := range f.rw.Hosts() {
		if !slices.Contains(googleHosts, host) {
			t.Errorf("the client addressed %s", host)
		}
	}
}

// edit runs change under the fake's lock.
func (f *fakeGoogle) edit(change func(*fakeGoogle)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// wrote is every write the fake took, in order.
func (f *fakeGoogle) wrote() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// account is the account with email, and false where there is none.
func (f *fakeGoogle) account(email string) (fakeAccount, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.byAccount(email); a != nil {
		return *a, true
	}
	return fakeAccount{}, false
}

// policy is a project's policy now.
func (f *fakeGoogle) policy(project string) googlecloud.Policy {
	f.mu.Lock()
	defer f.mu.Unlock()
	return clonePolicy(f.projects[project].Policy)
}

func clonePolicy(p googlecloud.Policy) googlecloud.Policy {
	out := p
	out.Bindings = make([]googlecloud.Binding, len(p.Bindings))
	for i, b := range p.Bindings {
		out.Bindings[i] = googlecloud.Binding{Role: b.Role, Members: slices.Clone(b.Members), Condition: b.Condition}
	}
	return out
}

// byAccount is the account named by its email or unique ID.
func (f *fakeGoogle) byAccount(account string) *fakeAccount {
	for _, a := range f.accounts {
		if a.Email == account || a.UniqueID == account {
			return a
		}
	}
	return nil
}

func (f *fakeGoogle) fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": status, "status": code, "message": message,
	}})
}

func (f *fakeGoogle) answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// issue mints a token for principal good for life.
func (f *fakeGoogle) issue(principal string, life time.Duration) (string, time.Time) {
	f.seq++
	value := fmt.Sprintf("ya29.fake-%s-%d", strings.NewReplacer("@", "-", ":", "-").Replace(principal), f.seq)
	expires := f.clock.now().Add(life)
	f.tokens[value] = fakeToken{principal: principal, expires: expires}
	return value, expires
}

// caller is the principal r's bearer token was issued to, "" for none or an
// expired one.
func (f *fakeGoogle) caller(r *http.Request) string {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	tok, ok := f.tokens[value]
	if !ok || !f.clock.now().Before(tok.expires) {
		return ""
	}
	return tok.principal
}

func (f *fakeGoogle) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	key := r.Method + " " + path
	f.requests = append(f.requests, key)
	if n := f.limited[key]; n > 0 {
		f.limited[key] = n - 1
		w.Header().Set("Retry-After", strconv.Itoa(cmp.Or(f.retryAfter, 7)))
		f.fail(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "Quota exceeded for quota metric 'Read requests'.")
		return
	}
	if status, ok := f.refuse[key]; ok {
		f.fail(w, status, http.StatusText(status), "refused by the test")
		return
	}
	if path == "/v1/token" && r.Method == http.MethodPost {
		f.sts(w, r)
		return
	}
	who := f.caller(r)
	if who == "" {
		f.fail(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Request had invalid authentication credentials.")
		return
	}
	switch {
	case strings.HasSuffix(path, ":generateAccessToken"):
		f.impersonate(w, r, who, strings.TrimSuffix(strings.TrimPrefix(path, "/v1/projects/-/serviceAccounts/"),
			":generateAccessToken"))
	case who != provisioner:
		f.fail(w, http.StatusForbidden, "PERMISSION_DENIED", "only the provisioner calls these")
	case path == "/v3/projects:search":
		f.search(w, r)
	case strings.HasPrefix(path, "/v1/projects/"+identity+"/serviceAccounts"):
		f.accountRoute(w, r, strings.TrimPrefix(path, "/v1/projects/"+identity+"/serviceAccounts"))
	case strings.HasPrefix(path, "/v1/roles/") || strings.Contains(path, "/roles/"):
		f.role(w, strings.TrimPrefix(path, "/v1/"))
	case strings.HasSuffix(path, "/services"):
		f.services(w, strings.TrimSuffix(strings.TrimPrefix(path, "/v1/projects/"), "/services"))
	case strings.HasPrefix(path, "/v1/projects/") && strings.Contains(path, ":"):
		project, method, _ := strings.Cut(strings.TrimPrefix(path, "/v1/projects/"), ":")
		f.projectRoute(w, r, project, method)
	default:
		f.fail(w, http.StatusNotFound, "NOT_FOUND", "no route "+key)
	}
}

// sts verifies the engine's token as the provider does and exchanges it.
func (f *fakeGoogle) sts(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	audience := "//iam.googleapis.com/" + testProvider
	if r.Form.Get("audience") != audience || r.Form.Get("subject_token_type") != "urn:ietf:params:oauth:token-type:jwt" {
		w.WriteHeader(http.StatusBadRequest)
		f.answer(w, map[string]string{"error": "invalid_target", "error_description": "wrong audience"})
		return
	}
	claims, ok := f.verify(r.Form.Get("subject_token"))
	now := f.clock.now().Unix()
	if !ok || claims.Iss != googlecloud.Issuer(testCompany) || claims.Sub != googlecloud.Subject(testCompany) ||
		claims.Aud != "https:"+audience || claims.Exp <= now || claims.Iat > now+60 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant",
			"error_description": "The token " + r.Form.Get("subject_token") + " is not trusted"})
		return
	}
	f.exchange++
	value, _ := f.issue("federated:"+claims.Sub, time.Hour)
	f.answer(w, map[string]any{"access_token": value, "expires_in": 3600, "token_type": "Bearer"})
}

type jwtClaims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// verify checks an ES256 token against the uploaded public key, which it
// names by the key's RFC 7638 thumbprint as Google matches it.
func (f *fakeGoogle) verify(token string) (jwtClaims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}, false
	}
	dec := base64.RawURLEncoding.DecodeString
	rawHeader, err := dec(parts[0])
	var header struct {
		Kid string `json:"kid"`
	}
	if err != nil || json.Unmarshal(rawHeader, &header) != nil || header.Kid != thumbprint(f.pub) {
		return jwtClaims{}, false
	}
	sig, err := dec(parts[2])
	if err != nil || len(sig) != 64 {
		return jwtClaims{}, false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(f.pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return jwtClaims{}, false
	}
	raw, err := dec(parts[1])
	if err != nil {
		return jwtClaims{}, false
	}
	var claims jwtClaims
	return claims, json.Unmarshal(raw, &claims) == nil
}

// thumbprint is pub's RFC 7638 thumbprint, worked out here rather than by the
// code under test, so a kid the engine got wrong is refused.
func thumbprint(pub *ecdsa.PublicKey) string {
	enc := base64.RawURLEncoding.EncodeToString
	x, y := coordinates(pub)
	canonical := `{"crv":"P-256","kty":"EC","x":"` + enc(x) + `","y":"` + enc(y) + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return enc(sum[:])
}

// coordinates are pub's X and Y, 32 big-endian bytes each, read off its
// uncompressed encoding (0x04, X, then Y), or none for a key that has none.
func coordinates(pub *ecdsa.PublicKey) (x, y []byte) {
	raw, err := pub.Bytes()
	if err != nil || len(raw) != 65 {
		return nil, nil
	}
	return raw[1:33], raw[33:]
}

// impersonate is generateAccessToken: the federated subject may become the
// provisioner, and the provisioner an account whose own policy names it
// Token Creator.
func (f *fakeGoogle) impersonate(w http.ResponseWriter, r *http.Request, who, account string) {
	var body struct {
		Lifetime string `json:"lifetime"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	seconds, err := strconv.Atoi(strings.TrimSuffix(body.Lifetime, "s"))
	if err != nil || seconds <= 0 || seconds > 3600 {
		f.fail(w, http.StatusBadRequest, "INVALID_ARGUMENT", "lifetime out of range")
		return
	}
	target := f.byAccount(account)
	switch {
	case target == nil:
		f.fail(w, http.StatusNotFound, "NOT_FOUND", "Not found; Gaia id not found for email "+account)
		return
	case who == "federated:"+googlecloud.Subject(testCompany) && target.Email == provisioner:
	case who == provisioner && f.delegates(target):
		if f.mintRefusals > 0 {
			f.mintRefusals--
			f.fail(w, http.StatusForbidden, "PERMISSION_DENIED", "Permission 'iam.serviceAccounts.getAccessToken' denied")
			return
		}
	default:
		f.fail(w, http.StatusForbidden, "PERMISSION_DENIED", "Permission 'iam.serviceAccounts.getAccessToken' denied")
		return
	}
	if target.Disabled {
		f.fail(w, http.StatusBadRequest, "FAILED_PRECONDITION", "The service account is disabled.")
		return
	}
	value, expires := f.issue(target.Email, time.Duration(seconds)*time.Second)
	f.answer(w, map[string]any{"accessToken": value, "expireTime": expires.Format(time.RFC3339)})
}

// delegates reports an account whose own policy lets the provisioner mint for it.
func (f *fakeGoogle) delegates(a *fakeAccount) bool {
	plain, _ := googlecloud.HeldBy(a.Policy, "serviceAccount:"+provisioner)
	return slices.Contains(plain, "roles/iam.serviceAccountTokenCreator")
}

func (f *fakeGoogle) search(w http.ResponseWriter, r *http.Request) {
	var out []map[string]any
	for _, id := range slices.Sorted(mapKeys(f.projects)) {
		p := f.projects[id]
		row := map[string]any{"projectId": p.ID, "displayName": p.Name, "state": p.State}
		if p.Parent != "" {
			row["parent"] = p.Parent
		}
		for k, v := range f.unmapped {
			row[k] = v
		}
		out = append(out, row)
	}
	from, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
	if f.page == 0 || from+f.page >= len(out) {
		f.answer(w, map[string]any{"projects": out[from:]})
		return
	}
	f.answer(w, map[string]any{"projects": out[from : from+f.page], "nextPageToken": strconv.Itoa(from + f.page)})
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// accountRoute serves the identity project's accounts: rest is what follows
// `/serviceAccounts` in the path.
func (f *fakeGoogle) accountRoute(w http.ResponseWriter, r *http.Request, rest string) {
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			var out []fakeAccountWire
			for _, id := range slices.Sorted(mapKeys(f.accounts)) {
				out = append(out, wire(f.accounts[id]))
			}
			f.answer(w, map[string]any{"accounts": out})
		case http.MethodPost:
			f.create(w, r)
		}
		return
	}
	account, method, _ := strings.Cut(strings.TrimPrefix(rest, "/"), ":")
	a := f.byAccount(account)
	if method == "" && r.Method == http.MethodGet || method == "getIamPolicy" {
		if past, stale := f.staleAccount(a, account); stale {
			a = past
		}
	}
	if a == nil {
		f.fail(w, http.StatusNotFound, "NOT_FOUND", "Unknown service account")
		return
	}
	switch {
	case method == "" && r.Method == http.MethodGet:
		f.answer(w, wire(a))
	case method == "" && r.Method == http.MethodDelete:
		f.wroteAccount(a, "delete "+a.Email)
		delete(f.accounts, a.UniqueID)
		f.answer(w, map[string]any{})
	case method == "disable" || method == "enable":
		f.wroteAccount(a, method+" "+a.Email)
		a.Disabled = method == "disable"
		f.answer(w, map[string]any{})
	case method == "getIamPolicy":
		f.answer(w, a.Policy)
	case method == "setIamPolicy":
		var body struct {
			Policy googlecloud.Policy `json:"policy"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Policy.Etag != a.Policy.Etag {
			f.fail(w, http.StatusConflict, "ABORTED", "There were concurrent policy changes.")
			return
		}
		f.wroteAccount(a, "set the policy of "+a.Email)
		body.Policy.Etag = fmt.Sprintf("a%d", f.seqNext())
		a.Policy = body.Policy
		f.answer(w, a.Policy)
	default:
		f.fail(w, http.StatusNotFound, "NOT_FOUND", "no route")
	}
}

func (f *fakeGoogle) seqNext() int { f.seq++; return f.seq }

type fakeAccountWire struct {
	Name        string `json:"name"`
	ProjectID   string `json:"projectId"`
	UniqueID    string `json:"uniqueId"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Description string `json:"description,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
}

func wire(a *fakeAccount) fakeAccountWire {
	return fakeAccountWire{Name: "projects/" + identity + "/serviceAccounts/" + a.Email, ProjectID: identity,
		UniqueID: a.UniqueID, Email: a.Email, DisplayName: a.DisplayName, Description: a.Description, Disabled: a.Disabled}
}

// staleAccount is the account as it stood before its last write, while that
// write's reads are still answered stale. nil past is an account that was
// not there yet.
func (f *fakeGoogle) staleAccount(a *fakeAccount, account string) (*fakeAccount, bool) {
	uid := account
	if a != nil {
		uid = a.UniqueID
	}
	for id, past := range f.accountPast {
		if past != nil && (past.Email == account || past.UniqueID == account) {
			uid = id
		}
	}
	if f.accountLag[uid] <= 0 {
		return nil, false
	}
	f.accountLag[uid]--
	return f.accountPast[uid], true
}

// wroteAccount records a write to a and the state its stale reads answer.
func (f *fakeGoogle) wroteAccount(a *fakeAccount, what string) {
	f.writes = append(f.writes, what)
	past := *a
	past.Policy = clonePolicy(a.Policy)
	f.accountPast[a.UniqueID] = &past
	f.accountLag[a.UniqueID] = f.lag
}

func (f *fakeGoogle) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID      string `json:"accountId"`
		ServiceAccount struct {
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
		} `json:"serviceAccount"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	email := body.AccountID + "@" + identity + ".iam.gserviceaccount.com"
	if f.byAccount(email) != nil {
		f.fail(w, http.StatusConflict, "ALREADY_EXISTS", "Service account "+body.AccountID+" already exists within project projects/"+identity+".")
		return
	}
	if len(f.accounts) >= f.quota {
		f.fail(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "Maximum number of service accounts on project reached.")
		return
	}
	uid := fmt.Sprintf("1%020d", f.seqNext())
	a := &fakeAccount{ID: body.AccountID, Email: email, UniqueID: uid, DisplayName: body.ServiceAccount.DisplayName,
		Description: body.ServiceAccount.Description, Policy: googlecloud.Policy{Etag: "a0"}}
	f.accounts[uid] = a
	f.writes = append(f.writes, "create "+email)
	f.accountPast[uid] = nil
	f.accountLag[uid] = f.lag
	f.notYet[email] = f.notYetFirst
	f.answer(w, wire(a))
}

func (f *fakeGoogle) role(w http.ResponseWriter, name string) {
	r, ok := f.roles[name]
	if !ok {
		f.fail(w, http.StatusNotFound, "NOT_FOUND", "The role named "+name+" was not found.")
		return
	}
	f.answer(w, r)
}

func (f *fakeGoogle) services(w http.ResponseWriter, project string) {
	p, ok := f.projects[project]
	if !ok {
		f.fail(w, http.StatusForbidden, "PERMISSION_DENIED", "Permission denied on resource project "+project+".")
		return
	}
	var out []map[string]any
	for _, name := range slices.Sorted(mapKeys(p.Services)) {
		row := map[string]any{"config": map[string]any{"name": name, "title": f.unmapped["title"]}, "state": "ENABLED"}
		for k, v := range f.unmapped {
			row[k] = v
		}
		out = append(out, row)
	}
	f.answer(w, map[string]any{"services": out})
}

// projectRoute serves a project's policy and permission checks.
func (f *fakeGoogle) projectRoute(w http.ResponseWriter, r *http.Request, project, method string) {
	p, ok := f.projects[project]
	if !ok {
		f.fail(w, http.StatusForbidden, "PERMISSION_DENIED", "The caller does not have permission")
		return
	}
	switch method {
	case "getIamPolicy":
		if f.policyLag[project] > 0 {
			f.policyLag[project]--
			f.answer(w, f.policyPast[project])
			return
		}
		f.answer(w, p.Policy)
	case "setIamPolicy":
		f.setProjectPolicy(w, r, p)
	case "testIamPermissions":
		var body struct {
			Permissions []string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var held []string
		for _, perm := range body.Permissions {
			if strings.HasPrefix(perm, "iam.serviceAccounts.") && project == identity {
				held = append(held, perm)
			}
		}
		f.answer(w, map[string]any{"permissions": held})
	default:
		f.fail(w, http.StatusNotFound, "NOT_FOUND", "no route")
	}
}

// setProjectPolicy takes a write as Google does: version 3 where conditions
// are kept, the etag current, every new account visible, and every role the
// write changes inside one of the provisioner's conditions.
func (f *fakeGoogle) setProjectPolicy(w http.ResponseWriter, r *http.Request, p *fakeProject) {
	var body struct {
		Policy googlecloud.Policy `json:"policy"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	next := body.Policy
	f.versions = append(f.versions, next.Version)
	if f.conflicts[p.ID] > 0 {
		f.conflicts[p.ID]--
		f.fail(w, http.StatusConflict, "ABORTED", "There were concurrent policy changes.")
		return
	}
	if next.Etag != p.Policy.Etag {
		f.fail(w, http.StatusConflict, "ABORTED", "There were concurrent policy changes.")
		return
	}
	if next.Version != 3 && slices.ContainsFunc(next.Bindings, func(b googlecloud.Binding) bool { return b.Condition != nil }) {
		f.fail(w, http.StatusBadRequest, "INVALID_ARGUMENT", "A policy with conditions requires version 3.")
		return
	}
	for _, b := range next.Bindings {
		for _, m := range b.Members {
			email, isSA := strings.CutPrefix(m, "serviceAccount:")
			if !isSA || f.notYet[email] <= 0 {
				continue
			}
			f.notYet[email]--
			f.fail(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Service account "+email+" does not exist.")
			return
		}
	}
	changed := changedRoles(p.Policy, next)
	if len(changed) > 0 && !f.allows(p.Policy, changed) {
		f.fail(w, http.StatusForbidden, "PERMISSION_DENIED", "Policy update access denied.")
		return
	}
	f.writes = append(f.writes, fmt.Sprintf("set the policy of %s: %s", p.ID, strings.Join(changed, " ")))
	f.policyPast[p.ID] = clonePolicy(p.Policy)
	f.policyLag[p.ID] = f.lag
	next.Etag = fmt.Sprintf("e%d", f.seqNext())
	p.Policy = next
	f.answer(w, next)
}

// allows reports a write changing only roles one of the provisioner's
// conditions lists, as Google's modifiedGrantsByRole holds it.
func (f *fakeGoogle) allows(pol googlecloud.Policy, changed []string) bool {
	for _, b := range pol.Bindings {
		if b.Role != "roles/resourcemanager.projectIamAdmin" || !slices.Contains(b.Members, "serviceAccount:"+provisioner) {
			continue
		}
		if b.Condition == nil {
			return true
		}
		listed, ok := googlecloud.ConditionRoles(b.Condition.Expression)
		if ok && !slices.ContainsFunc(changed, func(r string) bool { return !slices.Contains(listed, r) }) {
			return true
		}
	}
	return false
}

// changedRoles is every role whose bindings differ between two policies.
func changedRoles(before, after googlecloud.Policy) []string {
	members := func(p googlecloud.Policy) map[string][]string {
		out := map[string][]string{}
		for _, b := range p.Bindings {
			cond := ""
			if b.Condition != nil {
				cond = b.Condition.Expression
			}
			for _, m := range b.Members {
				out[b.Role] = append(out[b.Role], m+"|"+cond)
			}
		}
		for role := range out {
			slices.Sort(out[role])
		}
		return out
	}
	a, b := members(before), members(after)
	var out []string
	for role := range a {
		if !slices.Equal(a[role], b[role]) {
			out = append(out, role)
		}
	}
	for role := range b {
		if _, ok := a[role]; !ok {
			out = append(out, role)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
