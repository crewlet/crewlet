package atlassian_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
)

// A fake Atlassian organization: the admin plane and both product planes,
// from one server.
//
// # It is adversarial where the real one is
//
// Every place Atlassian's own behaviour has a trap in it, this fake has the
// trap too, so a caller that gets it wrong fails here rather than in
// production: the account listing PAGES and a caller that never follows
// links.next comes back short; the licence grant answers a just-created
// account with the directory 404 that reads like "no such account"; the mint
// route rejects `expiresAt` the way the real one does and can answer 200 with
// no token value; a scoped key is refused with a flat 403; and every product
// read demands Basic auth as the account it is about, because the org key is
// rejected on that plane outright.

const (
	fakeOrgID   = "org-1"
	fakeCloudID = "cloud-1"
	fakeOrgKey  = "unscoped-org-key"
)

// fakeAccount is one service account the organization holds.
type fakeAccount struct {
	ID          string
	AtlassianID string
	Email       string
	DisplayName string
	// tokens are the credentials on it, by id, holding their label.
	tokens map[string]string
	// values are the token VALUES, by id. Atlassian never returns one
	// again after minting; this fake keeps them so a product read can
	// recognise which account a credential authenticates as.
	values map[string]string
	// scopes are what each credential may do, by token id. Atlassian will
	// not tell a caller a token's scopes — the listing returns an id and a
	// label and nothing else — but it enforces them, and a credential
	// whose scopes do not cover a product is refused there with the same
	// flat 401 a wrong credential gets. That indistinguishability is the
	// whole reason a provisioner detects scope drift by exercising the
	// credential rather than by remembering what it asked for.
	scopes map[string][]string
	// licensed is the products this account holds a licence for.
	licensed map[atlassian.Product]bool
	// visibleAfter is how many more invite attempts answer the directory
	// 404 before the account becomes grantable.
	visibleAfter int
}

// fakeOrg is the server and its state.
type fakeOrg struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	accounts map[string]*fakeAccount
	nextID   int
	// permissions is what a product reports the caller holds, keyed
	// account/product/container. Absent means the contract's Allowed set,
	// which is the healthy company.
	permissions map[string]map[string]bool
	// spaceGroups grants a space's permissions to a GROUP rather than to
	// an account, and groupsOf says which groups an account is in. The two
	// together are how almost every real Confluence grant is made, and
	// matching only the account name is the bug that reports a working
	// agent as having no access at all.
	spaceGroups map[string][]string
	groupsOf    map[string][]string
	// unlicensedIsRefused makes a product read 401 for an account with no
	// licence, which is what Atlassian does.
	unlicensedIsRefused bool
	// mintReturnsNothing makes the mint answer 200 with no token value.
	mintReturnsNothing bool
	// mintReturnsNoID makes it answer with no id either, which is the
	// credential a rollback has no handle on at all: it exists on the
	// account, nothing recorded its value, and the only thing left to find
	// it by is the label the run sent.
	mintReturnsNoID bool
	// createReturnsNoID makes the create answer 200 having MADE the account
	// and without the atlassianId everything downstream keys on. The
	// account is real, billable and unusable — which is why the caller has
	// to be handed it along with the refusal.
	createReturnsNoID bool
	// createDropsResponse makes the create MAKE the account and then drop
	// the connection, which is the one failure a caller cannot tell from a
	// request that never arrived.
	createDropsResponse bool
	// licenceNeverLands accepts the invite and applies nothing, which is
	// the window Atlassian leaves open: the grant is asynchronous and has
	// taken minutes, so a permission check right after one legitimately
	// finds the agent still unlicensed.
	licenceNeverLands bool
	// containers are the projects and spaces this site actually has. A key
	// that is not here answers 404, the way a mistyped project does —
	// without which every container check passes and the report's one
	// permanent failure can never be exercised.
	containers map[string]bool
	// identityStatus makes every product read answer this status, for the
	// "could not reach the product" case that must never be read as a
	// refusal.
	identityStatus int
	// quotaFull refuses every create and grant with the org-is-full status.
	quotaFull bool
	// fullAfter lets this many accounts be created and refuses the rest,
	// which is the real shape of an allowance: a run does not hit the wall
	// on seat one, it hits it part way through.
	fullAfter int
	// mintStatus makes the mint route REFUSE with this status — proof that
	// nothing was created, as against an answer that went missing.
	mintStatus int
	// revokeFails refuses every token DELETE, so retirement fails on a run
	// whose real work all succeeded.
	revokeFails bool
	// created counts the accounts this server has made, for fullAfter.
	created int
	// requests records every path this server was asked for, in order.
	requests []string
	// bodies records the decoded body of every write, by path.
	bodies map[string][]map[string]any
	// sites is what DiscoverSite finds.
	sites []map[string]any
}

func newFakeOrg(t *testing.T) *fakeOrg {
	t.Helper()
	f := &fakeOrg{
		t:           t,
		accounts:    map[string]*fakeAccount{},
		permissions: map[string]map[string]bool{},
		spaceGroups: map[string][]string{},
		groupsOf:    map[string][]string{},
		bodies:      map[string][]map[string]any{},
		// ENG is the project and the space every fixture company names. A
		// test that needs another one adds it.
		containers: map[string]bool{"ENG": true},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.route))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOrg) admin(t *testing.T) *atlassian.AdminClient {
	t.Helper()
	client, err := atlassian.NewAdminClient(atlassian.AdminOptions{
		BaseURL: f.srv.URL, Key: fakeOrgKey, HTTP: f.srv.Client(),
	})
	if err != nil {
		t.Fatalf("admin client: %v", err)
	}
	return client
}

// seed adds an account the organization already had, as though an operator
// created it by hand or a previous run left it.
func (f *fakeOrg) seed(displayName string, products ...atlassian.Product) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createLocked(displayName, products...)
}

func (f *fakeOrg) createLocked(displayName string, products ...atlassian.Product) *fakeAccount {
	f.nextID++
	account := &fakeAccount{
		ID:          fmt.Sprintf("acc-%d", f.nextID),
		AtlassianID: fmt.Sprintf("aid-%d", f.nextID),
		Email:       fmt.Sprintf("svc-%d@serviceaccount.atlassian.com", f.nextID),
		DisplayName: displayName,
		tokens:      map[string]string{},
		values:      map[string]string{},
		scopes:      map[string][]string{},
		licensed:    map[atlassian.Product]bool{},
	}
	for _, product := range products {
		account.licensed[product] = true
	}
	f.accounts[account.ID] = account
	return account
}

// mint adds a credential to an account and returns its value, for seeding a
// sink with a credential the fake will recognise.
//
// With no products named it carries every scope, which is the credential a
// healthy company already has. Naming some is how a test seeds the drift
// case: a seat whose products have grown since its credential was minted.
func (f *fakeOrg) mint(account *fakeAccount, label string, products ...atlassian.Product) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(products) == 0 {
		products = atlassian.Products
	}
	id := fmt.Sprintf("tok-%d", len(account.tokens)+1)
	value := "value-" + account.AtlassianID + "-" + id
	account.tokens[id] = label
	account.values[id] = value
	account.scopes[id] = atlassian.Scopes(products)
	return value
}

func (f *fakeOrg) byDisplayName(name string) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, account := range f.accounts {
		if atlassian.NormalizeName(account.DisplayName) == atlassian.NormalizeName(name) {
			return account
		}
	}
	return nil
}

func (f *fakeOrg) asked(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	for _, path := range f.requests {
		if strings.Contains(path, substr) {
			n++
		}
	}
	return n
}

// askedExact counts requests whose method and path match exactly, so a
// prefix that also covers a sibling route (the invite lives under the
// collection) cannot make an assertion vacuous.
func (f *fakeOrg) askedExact(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	for _, seen := range f.requests {
		if seen == method+" "+path {
			n++
		}
	}
	return n
}

func (f *fakeOrg) wrote(path string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[path]
}

// route dispatches one request, admin plane or product plane.
func (f *fakeOrg) route(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	if strings.HasPrefix(r.URL.Path, "/ex/") {
		f.product(w, r)
		return
	}
	// The admin plane takes the org key as a BEARER. A scoped key — or any
	// other credential — is refused with a flat 403, which is the same
	// answer as no permission at all and is why the client maps it to a
	// message naming the fix.
	if r.Header.Get("Authorization") != "Bearer "+fakeOrgKey {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/workspaces"):
		f.discover(w)
	case strings.HasSuffix(r.URL.Path, "/service-accounts/invite"):
		f.invite(w, r)
	case strings.Contains(r.URL.Path, "/manage/api-tokens"):
		f.tokens(w, r)
	case strings.Contains(r.URL.Path, "/service-accounts"):
		f.serviceAccounts(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeOrg) discover(w http.ResponseWriter) {
	sites := f.sites
	if sites == nil {
		sites = []map[string]any{{
			"id": "ari:cloud:jira::site/" + fakeCloudID,
			"attributes": map[string]any{
				"name": "acme", "typeKey": "jira", "status": "online",
				"hostUrl": "https://acme.atlassian.net",
			},
		}}
	}
	writeJSON(w, map[string]any{"data": sites})
}

func (f *fakeOrg) serviceAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		f.listAccounts(w, r)
	case http.MethodPost:
		f.createAccount(w, r)
	case http.MethodDelete:
		f.deleteAccount(w, r)
	default:
		http.NotFound(w, r)
	}
}

// listAccounts PAGES. Atlassian caps the page at 80 and answers a larger
// limit with a 400 rather than clamping, so a caller that never follows
// links.next comes back short — and an account it did not see is one it
// creates a second identity on top of.
func (f *fakeOrg) listAccounts(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit > 80 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if limit == 0 {
		limit = 80
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	f.mu.Lock()
	all := make([]atlassian.ServiceAccount, 0, len(f.accounts))
	for _, account := range f.accounts {
		all = append(all, atlassian.ServiceAccount{
			ID: account.ID, AtlassianID: account.AtlassianID,
			Email: account.Email, DisplayName: account.DisplayName,
			Status: "active",
		})
	}
	f.mu.Unlock()
	// Sorted so a page boundary is stable across runs.
	sortAccounts(all)

	end := min(offset+limit, len(all))
	page := map[string]any{"items": all[min(offset, len(all)):end], "total": len(all)}
	if end < len(all) {
		// ABSOLUTE, the way Atlassian's is: a client that used the whole
		// URL would leave this server and hand the organization's key to
		// api.atlassian.com.
		page["links"] = map[string]any{
			"next": "https://api.atlassian.com" + r.URL.Path +
				"?limit=" + strconv.Itoa(limit) + "&offset=" + strconv.Itoa(end),
		}
	}
	writeJSON(w, page)
}

func (f *fakeOrg) createAccount(w http.ResponseWriter, r *http.Request) {
	body := f.readBody(w, r)
	if body == nil {
		return
	}
	f.mu.Lock()
	full := f.quotaFull || (f.fullAfter > 0 && f.created >= f.fullAfter)
	f.mu.Unlock()
	if full {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte("organization is full"))
		return
	}
	f.mu.Lock()
	f.created++
	account := f.createLocked(str(body["displayName"]))
	// A NEW ACCOUNT IS NOT GRANTABLE YET. Atlassian takes a moment to put
	// it in the directory and answers a grant before then with a 404 that
	// reads exactly like "no such account".
	account.visibleAfter = 1
	noID, dropped := f.createReturnsNoID, f.createDropsResponse
	f.mu.Unlock()
	if dropped {
		// The account is MADE and the answer never arrives. Aborting the
		// handler closes the connection without a response, which is what
		// the client sees when a real one is lost in transit.
		panic(http.ErrAbortHandler)
	}
	out := atlassian.ServiceAccount{
		ID: account.ID, AtlassianID: account.AtlassianID,
		Email: account.Email, DisplayName: account.DisplayName, Status: "active",
	}
	if noID {
		// THE ACCOUNT WAS STILL MADE. It is in the listing, it is
		// billable, and only the identity everything downstream keys on
		// is missing.
		out.AtlassianID = ""
	}
	writeJSON(w, out)
}

func (f *fakeOrg) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.accounts[id]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	delete(f.accounts, id)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeOrg) invite(w http.ResponseWriter, r *http.Request) {
	body := f.readBody(w, r)
	if body == nil {
		return
	}
	if f.quotaFull {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("no seats left"))
		return
	}
	ids, _ := body["userIds"].([]any)
	if len(ids) != 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	account := f.byAtlassianIDLocked(str(ids[0]))
	if account == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if account.visibleAfter > 0 {
		account.visibleAfter--
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"user not found in the directory"}`))
		return
	}
	if f.licenceNeverLands {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	rules, _ := body["permissionRules"].([]any)
	for _, raw := range rules {
		rule, _ := raw.(map[string]any)
		resource := str(rule["resource"])
		for _, product := range atlassian.Products {
			// EXACT ARI MATCH. The plain `jira` ARI is accepted by the
			// real endpoint and grants nothing, so a fake that matched
			// loosely would let that regression through.
			if resource == atlassian.SiteResourceARI(product, fakeCloudID) &&
				str(rule["role"]) == atlassian.MemberRoleARI(product) {
				account.licensed[product] = true
			}
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func (f *fakeOrg) tokens(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/users/")
	atlassianID, tail, _ := strings.Cut(rest, "/manage/api-tokens")
	f.mu.Lock()
	account := f.byAtlassianIDLocked(atlassianID)
	f.mu.Unlock()
	if account == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodGet:
		f.mu.Lock()
		out := make([]atlassian.AgentToken, 0, len(account.tokens))
		for id, label := range account.tokens {
			out = append(out, atlassian.AgentToken{ID: id, Label: label})
		}
		f.mu.Unlock()
		sortTokens(out)
		writeJSON(w, out)
	case r.Method == http.MethodDelete:
		id := strings.TrimPrefix(tail, "/")
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.revokeFails {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if _, ok := account.tokens[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(account.tokens, id)
		delete(account.values, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost:
		f.mintToken(w, r, account)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeOrg) mintToken(w http.ResponseWriter, r *http.Request, account *fakeAccount) {
	body := f.readBody(w, r)
	if body == nil {
		return
	}
	f.mu.Lock()
	status := f.mintStatus
	f.mu.Unlock()
	if status != 0 {
		// REFUSED, which is proof no credential was created — the fact a
		// rollback needs and could not previously tell from silence.
		w.WriteHeader(status)
		return
	}
	// THE FIELD IS `expiry`. Sending `expiresAt`, which is the name
	// Atlassian uses on other surfaces, fails with INVALID_EXPIRY and
	// reads as though no expiry had been sent at all.
	if _, wrong := body["expiresAt"]; wrong {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"INVALID_EXPIRY"}`))
		return
	}
	expiry, err := time.Parse(time.RFC3339, str(body["expiry"]))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"INVALID_EXPIRY"}`))
		return
	}
	label := str(body["label"])
	f.mu.Lock()
	id := fmt.Sprintf("tok-%d", len(account.tokens)+1)
	value := "value-" + account.AtlassianID + "-" + id
	account.tokens[id] = label
	account.values[id] = value
	account.scopes[id] = scopeList(body["scopes"])
	nothing, noID := f.mintReturnsNothing, f.mintReturnsNoID
	f.mu.Unlock()

	out := map[string]any{"id": id, "label": label, "expiry": expiry}
	if !nothing {
		out["token"] = value
	}
	if noID {
		delete(out, "id")
	}
	writeJSON(w, out)
}

// product serves both products' REST surfaces, as the AGENT.
func (f *fakeOrg) product(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/ex/")
	host, rest, _ := strings.Cut(rest, "/")
	cloudID, path, _ := strings.Cut(rest, "/")
	product := atlassian.ProductJira
	if host == "confluence" {
		product = atlassian.ProductConfluence
	}
	if cloudID != fakeCloudID {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	account, scopes := f.accountFor(r.Header.Get("Authorization"))
	if account == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	// A credential whose SCOPES do not cover this product is refused, and
	// so — when the test asks for it — is one whose account holds no
	// licence. Both answer the same flat 401 the real product does, which
	// is exactly why the provisioner cannot tell them apart and re-mints
	// on either.
	refused := !covers(scopes, product) ||
		(f.unlicensedIsRefused && !account.licensed[product])
	status := f.identityStatus
	f.mu.Unlock()
	if refused {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if status != 0 {
		// A 5xx or an outage: NOT a rejection. Re-minting on "cannot
		// tell" destroys a credential that works.
		w.WriteHeader(status)
		return
	}

	switch {
	case strings.HasSuffix(path, "/myself"), strings.HasSuffix(path, "/user/current"):
		writeJSON(w, map[string]any{"accountId": account.AtlassianID})
	case strings.Contains(path, "/mypermissions"):
		f.jiraPermissions(w, r, account)
	case strings.Contains(path, "/user/memberof"):
		f.confluenceGroups(w, r, account)
	case strings.Contains(path, "/space/"):
		key := path[strings.LastIndex(path, "/")+1:]
		if !f.knows(key) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.spacePermissions(w, path, account)
	case strings.Contains(path, "/project/"):
		if !f.knows(path[strings.LastIndex(path, "/")+1:]) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"projectTypeKey": "software", "style": "next-gen"})
	default:
		http.NotFound(w, r)
	}
}

// accountFor resolves the account a Basic header authenticates as.
//
// It matches the EMAIL AND the token together, because that is the whole of
// what Cloud checks: a token presented under the wrong address is refused,
// and a credential minted for one account never authenticates as another.
func (f *fakeOrg) accountFor(header string) (*fakeAccount, []string) {
	raw, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, nil
	}
	email, token, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return nil, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, account := range f.accounts {
		if account.Email != email {
			continue
		}
		for id, value := range account.values {
			if value == token {
				return account, account.scopes[id]
			}
		}
	}
	return nil, nil
}

// covers reports a scope list that reaches a product at all.
func covers(scopes []string, product atlassian.Product) bool {
	for _, want := range atlassian.Scopes([]atlassian.Product{product}) {
		if slices.Contains(scopes, want) {
			return true
		}
	}
	return false
}

// scopeList reads the scopes off a mint request body.
func scopeList(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		out = append(out, str(item))
	}
	return out
}

func (f *fakeOrg) jiraPermissions(w http.ResponseWriter, r *http.Request, account *fakeAccount) {
	container := r.URL.Query().Get("projectKey")
	if !f.knows(container) {
		// A MISTYPED PROJECT KEY, which is what the report's one permanent
		// finding is almost always about. Jira answers the check with a
		// 404 rather than an empty permission set.
		w.WriteHeader(http.StatusNotFound)
		return
	}
	held := f.heldBy(account, atlassian.ProductJira, container)
	out := map[string]any{}
	for _, name := range strings.Split(r.URL.Query().Get("permissions"), ",") {
		out[name] = map[string]any{"havePermission": held[name]}
	}
	writeJSON(w, map[string]any{"permissions": out})
}

func (f *fakeOrg) confluenceGroups(w http.ResponseWriter, r *http.Request, account *fakeAccount) {
	if r.URL.Query().Get("accountId") == "" {
		// The endpoint answers 400 without one, and rejects the older
		// username and userkey spellings outright.
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	groups := f.groupsOf[account.AtlassianID]
	f.mu.Unlock()
	results := make([]map[string]any, 0, len(groups))
	for _, name := range groups {
		results = append(results, map[string]any{"name": name})
	}
	writeJSON(w, map[string]any{"results": results, "size": len(results)})
}

// spacePermissions answers the way Confluence does: the whole space's grant
// list, per principal, with the READ spelling (operation/targetType) rather
// than the write one (key/target).
func (f *fakeOrg) spacePermissions(w http.ResponseWriter, path string, account *fakeAccount) {
	key := path[strings.LastIndex(path, "/")+1:]
	held := f.heldBy(account, atlassian.ProductConfluence, key)

	f.mu.Lock()
	viaGroup := f.spaceGroups[key]
	f.mu.Unlock()

	permissions := []map[string]any{}
	for name, ok := range held {
		if !ok {
			continue
		}
		operation, target, _ := strings.Cut(name, ":")
		entry := map[string]any{
			"operation": map[string]any{"operation": operation, "targetType": target},
		}
		switch {
		case len(viaGroup) > 0:
			// GRANTED TO A GROUP, which is how almost every real space
			// permission is granted. An implementation that matched only
			// the account id reports a perfectly working agent as having
			// no access at all.
			groups := make([]map[string]any, 0, len(viaGroup))
			for _, group := range viaGroup {
				groups = append(groups, map[string]any{"name": group})
			}
			entry["subjects"] = map[string]any{
				"group": map[string]any{"results": groups},
				"user":  map[string]any{"results": []map[string]any{}},
			}
		default:
			entry["subjects"] = map[string]any{
				"user": map[string]any{"results": []map[string]any{
					{"accountId": account.AtlassianID},
				}},
				"group": map[string]any{"results": []map[string]any{}},
			}
		}
		permissions = append(permissions, entry)
	}
	writeJSON(w, map[string]any{"key": key, "permissions": permissions})
}

// heldBy is what a product reports this account holds in a container.
//
// The default is the contract's Allowed set and nothing forbidden, which is
// the healthy company — a test that wants a finding says so explicitly.
func (f *fakeOrg) heldBy(account *fakeAccount, product atlassian.Product, container string) map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, ok := f.permissions[permKey(account.AtlassianID, product, container)]; ok {
		return held
	}
	held := map[string]bool{}
	for _, name := range atlassian.Allowed(product) {
		held[name] = true
	}
	return held
}

// grants overrides what one account holds in one container.
func (f *fakeOrg) grants(account *fakeAccount, product atlassian.Product, container string, held map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permissions[permKey(account.AtlassianID, product, container)] = held
}

func permKey(atlassianID string, product atlassian.Product, container string) string {
	return atlassianID + "/" + string(product) + "/" + container
}

// set changes a knob under the server's own lock.
//
// Needed wherever a knob is changed after a request has been served: the
// aborted-handler case leaves a goroutine still unwinding, and an unguarded
// write beside its guarded read is a data race the suite reports on a machine
// that schedules them differently.
func (f *fakeOrg) set(change func(*fakeOrg)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// knows reports a container this site actually has.
func (f *fakeOrg) knows(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers[key]
}

func (f *fakeOrg) byAtlassianIDLocked(id string) *fakeAccount {
	for _, account := range f.accounts {
		if account.AtlassianID == id {
			return account
		}
	}
	return nil
}

func (f *fakeOrg) readBody(w http.ResponseWriter, r *http.Request) map[string]any {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil
	}
	f.mu.Lock()
	f.bodies[r.URL.Path] = append(f.bodies[r.URL.Path], body)
	f.mu.Unlock()
	return body
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func sortAccounts(in []atlassian.ServiceAccount) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j].ID < in[j-1].ID; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

func sortTokens(in []atlassian.AgentToken) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j].ID < in[j-1].ID; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
