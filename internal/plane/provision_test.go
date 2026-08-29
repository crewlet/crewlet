package plane_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/plane"
	"github.com/crewlet/crewlet/internal/provision"
)

// instance is a Plane workspace that remembers what was done to it.
//
// A stand-in rather than a mock: what the reconcile has to get right is the
// SEQUENCE — probe, enumerate, resolve, create — and its idempotence across
// runs. Asserting call-by-call would pin the sequence this was written with
// rather than the properties it must hold.
type instance struct {
	mu sync.Mutex

	// accounts is username -> the member row the workspace serves.
	accounts map[string]*plane.Account
	// tokens is account id -> its token rows.
	tokens   map[string][]*plane.Token
	projects []plane.Project
	// member is project:account -> joined.
	member map[string]bool
	hooks  []*plane.Webhook

	// The switches that reach the paths an operator actually meets.
	noServiceAccounts bool // stock Community: the route is not there
	noTokenLifecycle  bool // service accounts, but no token routes
	noWebhooks        bool
	notAdmin          bool // members/ refuses
	badWorkspace      bool // the slug does not resolve
	noUsernames       bool // member rows carry no username
	badCredential     bool // the API key does not authenticate
	// identityFails makes the identity route answer 500 for a SEAT's
	// key, which is "cannot tell" rather than "this credential is bad".
	identityFails     bool
	secretlessWebhook bool // the create response carries no secret_key
	// duplicateAs is the status a repeat membership is refused with. The
	// fork improved the message to a 409 on some paths and stock CE maps
	// the constraint violation to a generic 400 — both are "already a
	// member" and a run has to survive either.
	duplicateAs int
	// rejectMembership refuses every membership with a 400 that is NOT a
	// duplicate, which must abort rather than read as success.
	rejectMembership bool
	// crashMembership answers with a 500 whose body NAMES a duplicate,
	// which is what an unhandled integrity error looks like — the
	// membership may or may not exist, so it is not success.
	crashMembership bool
	renameTo        string
	mintEmpty       bool
	failMintFor     string
	failDelete      bool
	dropPageEntity  bool

	calls  []string
	nextID int
	// now is the instant token expiry is judged against, so a test can
	// age a token without waiting.
	now time.Time
	// pages is the workspace's page store, for the import tests.
	pages map[string]*pageRow
	// failPageNamed refuses to write the page with this name, to reach
	// the isolated-failure path.
	failPageNamed string
	// refuseDelete makes a page delete 403 after its archive landed,
	// which is the state a failed prune has to undo.
	refuseDelete bool
	// bodies are the write payloads, so a test can assert what was SENT
	// rather than what the fake chose to remember about it.
	bodies []recorded
	// inactive marks a project:member row Plane is keeping deactivated —
	// what "remove from project" in the UI actually does.
	inactive map[string]bool
	// noProjectMemberList refuses the membership list, the degraded case
	// the inactive check has to survive without failing a run.
	noProjectMemberList bool
	// noDisplayNames drops display_name from the member listing, which is
	// the build the drift check must not read as "every name has moved".
	noDisplayNames bool
	// noRoles drops role from the member listing. Plane's role ints are
	// 20/15/5, so an unserved column decodes as 0 — a value that is not a
	// role, and must not be read as a demotion.
	noRoles bool
}

// pageRow is a page as the workspace holds it.
type pageRow struct {
	ID             string
	Project        string
	Name           string
	HTML           string
	ExternalID     string
	ExternalSource string
	Archived       bool
}

// recorded is one write, as it arrived.
type recorded struct {
	method string
	path   string
	body   map[string]any
}

func newInstance() *instance {
	return &instance{
		accounts: map[string]*plane.Account{},
		tokens:   map[string][]*plane.Token{},
		member:   map[string]bool{},
		inactive: map[string]bool{},
		pages:    map[string]*pageRow{},
		projects: []plane.Project{{ID: "p-eng", Identifier: "ENG", Name: "Engineering"}},
		now:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// deactivateMembership flips every membership of a project to inactive,
// which is what "remove from project" in the Plane UI does to the row.
func (f *instance) deactivateMembership(project string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.member {
		if owner, _, _ := strings.Cut(key, ":"); owner == project {
			f.inactive[key] = true
		}
	}
}

// renameAccount edits a display name the way a workspace admin would.
func (f *instance) renameAccount(username, display string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.accounts[username]; a != nil {
		a.DisplayName = display
	}
}

func (f *instance) displayName(username string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.accounts[username]; a != nil {
		return a.DisplayName
	}
	return ""
}

// setRole promotes or demotes an account the way a workspace admin would.
func (f *instance) setRole(username string, role int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.accounts[username]; a != nil {
		a.Role = role
	}
}

func (f *instance) roleOf(username string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.accounts[username]; a != nil {
		return a.Role
	}
	return 0
}

func (f *instance) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func (f *instance) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	f.calls = append(f.calls, r.Method+" "+path)
	w.Header().Set("Content-Type", "application/json")
	const ws = "/workspaces/nimbus"

	var body map[string]any
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if len(raw) > 0 {
			json.Unmarshal(raw, &body)
			f.bodies = append(f.bodies, recorded{r.Method, path, body})
		}
	}

	switch {
	case path == "/users/me/":
		if f.badCredential {
			deny(w, http.StatusUnauthorized, "invalid api key")
			return
		}
		// WHOEVER PRESENTED THE KEY. The re-run check takes the value a
		// variable holds and asks the instance who it is, so a fake that
		// answered "admin" for every key would prove nothing.
		key := r.Header.Get("X-API-Key")
		if f.identityFails && key != adminKey {
			deny(w, http.StatusInternalServerError, "upstream is unwell")
			return
		}
		if key == adminKey {
			json.NewEncoder(w).Encode(map[string]any{"id": "admin", "username": "root"})
			return
		}
		owner, live := f.owner(key)
		if !live {
			deny(w, http.StatusUnauthorized, "invalid api key")
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": owner})

	case strings.HasSuffix(path, "/pages/") && r.Method == http.MethodGet:
		project := strings.TrimSuffix(strings.TrimPrefix(path, ws+"/projects/"), "/pages/")
		out := make([]map[string]any, 0, len(f.pages))
		for _, page := range f.pages {
			if page.Project != project {
				continue
			}
			row := map[string]any{
				"id": page.ID, "name": page.Name, "project": page.Project,
				"description_html":     page.HTML,
				"description_stripped": plane.HTMLToText(page.HTML),
				"external_id":          page.ExternalID,
				"external_source":      page.ExternalSource,
			}
			if page.Archived {
				row["archived_at"] = "2026-01-01T00:00:00Z"
			}
			// THE PROJECTION IS HONOURED, because the import
			// enumeration asks for a narrow one and a fake that served
			// every field would hide a caller reading something it
			// never requested.
			if fields := r.URL.Query().Get("fields"); fields != "" {
				want := map[string]bool{}
				for _, name := range strings.Split(fields, ",") {
					want[strings.TrimSpace(name)] = true
				}
				for name := range row {
					if !want[name] {
						delete(row, name)
					}
				}
			}
			out = append(out, row)
		}
		// DELIBERATELY UNSORTED: Plane guarantees no order here, and a
		// fake that sorted would hide every place the importer depends
		// on one.
		json.NewEncoder(w).Encode(map[string]any{"results": out})

	case strings.HasSuffix(path, "/pages/") && r.Method == http.MethodPost:
		project := strings.TrimSuffix(strings.TrimPrefix(path, ws+"/projects/"), "/pages/")
		name := fmt.Sprint(body["name"])
		if name == f.failPageNamed {
			deny(w, http.StatusForbidden, "this page is locked")
			return
		}
		page := &pageRow{
			ID: f.id("page"), Project: project, Name: name,
			HTML:           fmt.Sprint(body["description_html"]),
			ExternalID:     fmt.Sprint(body["external_id"]),
			ExternalSource: fmt.Sprint(body["external_source"]),
		}
		f.pages[page.ID] = page
		json.NewEncoder(w).Encode(map[string]any{
			"id": page.ID, "name": page.Name,
			"external_id": page.ExternalID, "external_source": page.ExternalSource,
		})

	case strings.Contains(path, "/pages/") && strings.HasSuffix(path, "/archive/"):
		id := pageIDIn(path, "/archive/")
		page := f.pages[id]
		if page == nil {
			deny(w, http.StatusNotFound, "gone")
			return
		}
		if page.Archived {
			// Plane refuses to archive what is already archived.
			deny(w, http.StatusBadRequest, "page is already archived")
			return
		}
		page.Archived = true
		w.WriteHeader(http.StatusNoContent)

	case strings.Contains(path, "/pages/") && strings.HasSuffix(path, "/unarchive/"):
		id := pageIDIn(path, "/unarchive/")
		if page := f.pages[id]; page != nil {
			page.Archived = false
		}
		w.WriteHeader(http.StatusNoContent)

	case strings.Contains(path, "/pages/") && r.Method == http.MethodPatch:
		id := pageIDIn(path, "")
		page := f.pages[id]
		if page == nil {
			deny(w, http.StatusNotFound, "gone")
			return
		}
		if fmt.Sprint(body["name"]) == f.failPageNamed {
			deny(w, http.StatusForbidden, "this page is locked")
			return
		}
		page.Name = fmt.Sprint(body["name"])
		page.HTML = fmt.Sprint(body["description_html"])
		page.ExternalID = fmt.Sprint(body["external_id"])
		page.ExternalSource = fmt.Sprint(body["external_source"])
		w.WriteHeader(http.StatusNoContent)

	case strings.Contains(path, "/pages/") && r.Method == http.MethodDelete:
		id := pageIDIn(path, "")
		page := f.pages[id]
		if page != nil && !page.Archived {
			// ARCHIVE IS THE PRECONDITION. Plane will not delete a live
			// page, which is why the prune archives first — and why a
			// prune that stops between the two has to undo the archive.
			deny(w, http.StatusBadRequest, "archive the page before deleting it")
			return
		}
		if f.refuseDelete {
			deny(w, http.StatusForbidden, "only a project admin may delete a page")
			return
		}
		delete(f.pages, id)
		w.WriteHeader(http.StatusNoContent)

	case path == ws+"/projects/" && r.Method == http.MethodPost:
		project := plane.Project{
			ID: f.id("proj"), Name: fmt.Sprint(body["name"]),
			Identifier: fmt.Sprint(body["identifier"]),
		}
		f.projects = append(f.projects, project)
		json.NewEncoder(w).Encode(project)

	case path == ws+"/projects/" && r.Method == http.MethodGet:
		if f.badWorkspace {
			deny(w, http.StatusForbidden, "no such workspace")
			return
		}
		json.NewEncoder(w).Encode(f.projects)

	case path == ws+"/members/" && r.Method == http.MethodGet:
		if f.notAdmin {
			deny(w, http.StatusForbidden, "not an admin")
			return
		}
		out := make([]map[string]any, 0, len(f.accounts))
		for _, a := range f.accounts {
			row := map[string]any{"id": a.ID, "is_bot": a.IsBot}
			if !f.noRoles {
				row["role"] = a.Role
			}
			if !f.noUsernames {
				row["username"] = a.Username
			}
			if !f.noDisplayNames {
				row["display_name"] = a.DisplayName
			}
			out = append(out, row)
		}
		json.NewEncoder(w).Encode(out)

	case path == ws+"/service-accounts/":
		if f.noServiceAccounts {
			deny(w, http.StatusNotFound, "no such route")
			return
		}
		if r.Method != http.MethodPost {
			// The route is registered POST-only, so the capability
			// probe's GET is refused for its METHOD — which is the
			// presence signal the preflight reads.
			deny(w, http.StatusMethodNotAllowed, `method "GET" not allowed`)
			return
		}
		username, _ := body["username"].(string)
		if f.renameTo != "" {
			// An instance carrying only the upstream service-accounts
			// API ignores a caller-chosen username and generates one.
			username = f.renameTo
		}
		account := &plane.Account{
			ID: f.id("acct"), Username: username, IsBot: true,
			DisplayName: fmt.Sprint(body["display_name"]),
			Role:        roleInt(fmt.Sprint(body["role"])),
			Token:       f.id("plane_api_"),
		}
		f.accounts[username] = account
		if f.mintEmpty {
			account.Token = ""
		}
		f.tokens[account.ID] = []*plane.Token{{
			ID: f.id("tok"), Label: "initial", Active: true, Value: account.Token,
		}}
		json.NewEncoder(w).Encode(account)

	case strings.HasPrefix(path, ws+"/service-accounts/") && strings.HasSuffix(path, "/tokens/"):
		if f.noTokenLifecycle {
			deny(w, http.StatusNotFound, "no such route")
			return
		}
		account := strings.TrimSuffix(strings.TrimPrefix(path, ws+"/service-accounts/"), "/tokens/")
		switch r.Method {
		case http.MethodPatch:
			// No token route allows PATCH; the probe relies on it.
			deny(w, http.StatusMethodNotAllowed, `method "PATCH" not allowed`)
		case http.MethodGet:
			json.NewEncoder(w).Encode(f.tokens[account])
		case http.MethodPost:
			label, _ := body["label"].(string)
			if f.failMintFor != "" && strings.HasSuffix(label, f.failMintFor) {
				deny(w, http.StatusForbidden, "not permitted")
				return
			}
			token := &plane.Token{ID: f.id("tok"), Label: label, Active: true}
			token.Value = "plane_api_" + token.ID
			if raw, ok := body["expired_at"].(string); ok {
				token.ExpiresAt, _ = time.Parse(time.RFC3339, raw)
			}
			// The token EXISTS on the instance whether or not the
			// response carries its value — which is what makes an
			// empty response the worst answer available.
			f.tokens[account] = append(f.tokens[account], token)
			value := token.Value
			if f.mintEmpty {
				value = ""
			}
			json.NewEncoder(w).Encode(map[string]any{
				"id": token.ID, "label": label, "is_active": true, "token": value,
			})
			_ = token
		default:
			deny(w, http.StatusMethodNotAllowed, "no")
		}

	case strings.Contains(path, "/tokens/") && r.Method == http.MethodDelete:
		rest := strings.TrimPrefix(path, ws+"/service-accounts/")
		account, tokenID, _ := strings.Cut(rest, "/tokens/")
		f.deactivate(account, strings.Trim(tokenID, "/"))
		w.WriteHeader(http.StatusNoContent)

	case strings.HasPrefix(path, ws+"/service-accounts/") && r.Method == http.MethodDelete:
		if f.failDelete {
			deny(w, http.StatusInternalServerError, "boom")
			return
		}
		id := strings.Trim(strings.TrimPrefix(path, ws+"/service-accounts/"), "/")
		// A real delete CASCADES: every token goes with the account.
		delete(f.tokens, id)
		for name, a := range f.accounts {
			if a.ID == id {
				delete(f.accounts, name)
			}
		}
		w.WriteHeader(http.StatusNoContent)

	case strings.HasPrefix(path, ws+"/projects/") &&
		strings.HasSuffix(path, "/members/") && r.Method == http.MethodGet:
		project := strings.TrimSuffix(strings.TrimPrefix(path, ws+"/projects/"), "/members/")
		if f.noProjectMemberList {
			deny(w, http.StatusNotFound, "no such route")
			return
		}
		out := make([]map[string]any, 0, len(f.member))
		for key := range f.member {
			owner, member, _ := strings.Cut(key, ":")
			if owner != project {
				continue
			}
			out = append(out, map[string]any{
				"id": "pm-" + member, "member": member,
				"is_active": !f.inactive[key],
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"results": out})

	case strings.HasSuffix(path, "/members/") && r.Method == http.MethodPost:
		project := strings.TrimSuffix(strings.TrimPrefix(path, ws+"/projects/"), "/members/")
		if f.crashMembership {
			deny(w, http.StatusInternalServerError,
				"duplicate key value violates unique constraint")
			return
		}
		if f.rejectMembership {
			deny(w, http.StatusBadRequest, "INVALID_MEMBER: not in the workspace")
			return
		}
		key := project + ":" + fmt.Sprint(body["member"])
		if f.member[key] {
			// A duplicate add violates a unique constraint the view maps
			// to a GENERIC 400 — which a reconcile has to read as
			// "already a member" or no second run can ever finish.
			if f.duplicateAs == http.StatusConflict {
				deny(w, http.StatusConflict, "member already exists")
			} else {
				deny(w, http.StatusBadRequest, "The payload is not valid: already exists")
			}
			return
		}
		f.member[key] = true
		json.NewEncoder(w).Encode(map[string]any{"id": f.id("pm")})

	case path == ws+"/webhooks/":
		if f.noWebhooks {
			deny(w, http.StatusNotFound, "no such route")
			return
		}
		switch r.Method {
		case http.MethodGet:
			out := make([]map[string]any, 0, len(f.hooks))
			for _, h := range f.hooks {
				// THE SECRET IS STRUCTURALLY ABSENT from every read: it
				// is served once, in the create response.
				out = append(out, map[string]any{
					"id": h.ID, "url": h.URL, "is_active": h.IsActive, "page": h.Page,
				})
			}
			json.NewEncoder(w).Encode(out)
		case http.MethodPost:
			hook := &plane.Webhook{
				ID: f.id("wh"), URL: fmt.Sprint(body["url"]), IsActive: true,
				SecretKey: "plane_wh_" + f.id("s"),
				// An instance without page-webhook support DROPS the
				// unknown field rather than refusing it, so the only
				// evidence is the echo.
				Page: !f.dropPageEntity,
			}
			f.hooks = append(f.hooks, hook)
			if f.secretlessWebhook {
				hook.SecretKey = ""
			}
			json.NewEncoder(w).Encode(map[string]any{
				"id": hook.ID, "url": hook.URL, "secret_key": hook.SecretKey,
				"is_active": true, "page": hook.Page,
			})
		default:
			deny(w, http.StatusMethodNotAllowed, "no")
		}

	case strings.HasPrefix(path, ws+"/webhooks/") && r.Method == http.MethodPatch:
		id := strings.Trim(strings.TrimPrefix(path, ws+"/webhooks/"), "/")
		for _, h := range f.hooks {
			if h.ID == id {
				h.Page = !f.dropPageEntity
				json.NewEncoder(w).Encode(map[string]any{
					"id": h.ID, "url": h.URL, "is_active": true, "page": h.Page,
				})
				return
			}
		}
		deny(w, http.StatusNotFound, "gone")

	case strings.HasPrefix(path, ws+"/webhooks/") && r.Method == http.MethodDelete:
		id := strings.Trim(strings.TrimPrefix(path, ws+"/webhooks/"), "/")
		kept := f.hooks[:0]
		for _, h := range f.hooks {
			if h.ID != id {
				kept = append(kept, h)
			}
		}
		f.hooks = kept
		w.WriteHeader(http.StatusNoContent)

	default:
		deny(w, http.StatusNotFound, "no such route")
	}
}

func (f *instance) deactivate(account, tokenID string) {
	for _, t := range f.tokens[account] {
		if t.ID == tokenID {
			t.Active = false
		}
	}
}

// adminKey is the operator credential the fixture's client presents.
const adminKey = "admin-key"

// owner resolves a token value to the account it belongs to, and reports
// whether it would still authenticate.
func (f *instance) owner(value string) (string, bool) {
	for account, tokens := range f.tokens {
		for _, token := range tokens {
			if token.Value == value {
				live := token.Active &&
					(token.ExpiresAt.IsZero() || token.ExpiresAt.After(f.now))
				return account, live
			}
		}
	}
	return "", false
}

// pageIDIn pulls the page id out of a page-scoped path.
func pageIDIn(path, suffix string) string {
	_, rest, _ := strings.Cut(path, "/pages/")
	rest = strings.TrimSuffix(rest, suffix)
	return strings.Trim(rest, "/")
}

func deny(w http.ResponseWriter, status int, detail string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, detail)
}

func roleInt(name string) int {
	switch name {
	case "admin":
		return plane.RoleAdmin
	case "guest":
		return plane.RoleGuest
	default:
		return plane.RoleMember
	}
}

// liveTokens counts the credentials that would still authenticate.
func (f *instance) liveTokens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, tokens := range f.tokens {
		for _, t := range tokens {
			if t.Active {
				n++
			}
		}
	}
	return n
}

// writes returns every payload sent to paths with this suffix.
func (f *instance) writes(method, suffix string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, rec := range f.bodies {
		if rec.method == method && strings.HasSuffix(rec.path, suffix) {
			out = append(out, rec.body)
		}
	}
	return out
}

// mintBodies is every token-mint payload this instance received.
func (f *instance) mintBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, rec := range f.bodies {
		if rec.method == http.MethodPost && strings.HasSuffix(rec.path, "/tokens/") {
			out = append(out, rec.body)
		}
	}
	return out
}

// revokeAll deactivates every token, as an administrator would.
func (f *instance) revokeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tokens := range f.tokens {
		for _, t := range tokens {
			t.Active = false
		}
	}
}

// expireAll stamps a past expiry on every token while leaving is_active
// true, which is exactly how the instance serves a lapsed one.
func (f *instance) expireAll(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tokens := range f.tokens {
		for _, t := range tokens {
			t.ExpiresAt = at
		}
	}
	// The instance's own clock moves past the expiry too — otherwise the
	// token still authenticates and the run is right to keep it.
	f.now = at.Add(time.Hour)
}

// forget clears the call log, so a test can count what ONE run did.
func (f *instance) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// tokenRevokes counts the token deletions this instance has been asked for.
func (f *instance) tokenRevokes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if strings.HasPrefix(call, http.MethodDelete+" ") && strings.Contains(call, "/tokens/") {
			n++
		}
	}
	return n
}

func (f *instance) accountCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.accounts)
}

// ---- the fixtures ------------------------------------------------------ //

func workspaceClient(t *testing.T, f *instance) *plane.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	// THE SERVER'S OWN CLIENT, whose transport belongs to this server and
	// dies with it. A client over http.DefaultTransport shares one
	// connection pool with every other parallel test, so one server's
	// Close breaks a request in flight against another.
	c, err := plane.NewClient(plane.ClientOptions{
		URL: server.URL, Workspace: "nimbus", APIKey: adminKey,
		HTTP: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func seatOrg(seats ...*org.Role) *org.Organization {
	return &org.Organization{Name: "Nimbus", Roles: seats}
}

func trackerSeat(name, value string) *org.Role {
	return &org.Role{Name: name, MCPEnv: map[string]map[string]string{
		plane.SeatEnv: {plane.SeatKey: value},
	}}
}

func enabledPlane() *config.Plane {
	return &config.Plane{
		Enabled: true, URL: "https://plane.example.com", Workspace: "nimbus",
		WebhookSecret: "${PLANE_WEBHOOK_SECRET}",
		Provisioning:  &config.PlaneProvisioning{Projects: []string{"ENG"}},
	}
}

// withEngineToken points the engine's own credential at a variable, which
// is what makes the run provision the engine account beside the seats. Off
// in the base fixture so a test about a seat is about ONE account.
func withEngineToken(cfg *config.Plane) *config.Plane {
	cfg.Token = "${PLANE_ENGINE_TOKEN}"
	return cfg
}

// trackerSink is a sink that can be made to fail, to reach the rollback.
type trackerSink struct {
	mu       sync.Mutex
	values   map[string]string
	failOn   string
	discards int
	flushes  int
	// holdsErr makes the sink unreadable, which must never be read as
	// "nothing is held" — that would rotate every live credential.
	holdsErr error
	// onRecord fires before a record is judged, so a test can cancel the
	// caller's context at the exact moment the rollback has to survive.
	onRecord func()
}

func newTrackerSink() *trackerSink {
	return &trackerSink{values: map[string]string{}}
}

func (s *trackerSink) Record(_ context.Context, name, value string) error {
	if s.onRecord != nil {
		s.onRecord()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == s.failOn {
		return errors.New("the store is unreachable")
	}
	s.values[name] = value
	return nil
}

func (s *trackerSink) Discard(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discards++
	s.values = map[string]string{}
	return nil
}

func (s *trackerSink) Flush(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushes++
	return nil
}

// Value implements the sink contract — what an EARLIER run recorded, which
// is what a re-run has to see.
// seed puts a value in the sink as an EARLIER run would have.
func (s *trackerSink) seed(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = value
}

func (s *trackerSink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holdsErr != nil {
		return "", false, s.holdsErr
	}
	return s.values[name], s.values[name] != "", nil
}

func (s *trackerSink) Describe() string { return "a test sink" }
func (s *trackerSink) NextStep() string { return "a test next step" }

func (s *trackerSink) recorded(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[name]
}

func run(t *testing.T, f *instance, opts plane.Options) (*plane.Result, error) {
	t.Helper()
	if opts.Client == nil {
		opts.Client = workspaceClient(t, f)
	}
	if opts.Config == nil {
		opts.Config = enabledPlane()
	}
	if opts.Plan == nil {
		o := seatOrg(trackerSeat("SWE", "${PLANE_TOKEN_SWE}"))
		plan, err := plane.PlanFor(o, opts.Config)
		if err != nil {
			t.Fatalf("PlanFor: %v", err)
		}
		opts.Plan = plan
	}
	if opts.Sink == nil {
		opts.Sink = newTrackerSink()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	}
	return plane.Reconcile(context.Background(), opts)
}

var _ provision.TokenSink = (*trackerSink)(nil)
