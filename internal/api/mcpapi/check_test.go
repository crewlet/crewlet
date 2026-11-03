package mcpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/secrets"
)

// fakeChecker records each test it is asked for, refuses the ones refusals
// names, and finds every other server serving one tool on node-a.
type fakeChecker struct {
	refusals map[string]error

	mu        sync.Mutex
	asked     []mcpapi.CheckRequest
	addresses []string
}

func (f *fakeChecker) CheckServer(_ context.Context, req mcpapi.CheckRequest) (mcpapi.Check, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, req)
	if err, refused := f.refusals[req.Server+"/"+req.Seat]; refused {
		return mcpapi.Check{}, err
	}
	return mcpapi.Check{Server: req.Server, Seat: req.Seat, Draft: req.Draft, By: req.By.Name,
		At: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), Node: "node-a", OK: true,
		Nodes: []mcpapi.CheckedNode{servingNode("node-a"), {Node: "node-b", Reason: "did not answer within 4m15s"}}}, nil
}

func (f *fakeChecker) CheckAddress(_ context.Context, address string) mcp.AddressCheck {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addresses = append(f.addresses, address)
	return mcp.AddressCheck{Test: servingNode("").Report, Auth: catalogue.AuthOAuth,
		OAuth: &mcp.OAuthDiscovery{ResourceMetadata: "https://mcp.example.com/.well-known/oauth-protected-resource",
			Resource: "https://mcp.example.com/mcp", AuthorizationServers: []string{"https://auth.example.com"},
			Issuer: "https://auth.example.com", Scopes: []string{"read"}, Client: catalogue.ClientDCR, PKCE: true}}
}

func (f *fakeChecker) requests() []mcpapi.CheckRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// servingNode is a node that found the server serving two tools, one refused.
func servingNode(node string) mcpapi.CheckedNode {
	return mcpapi.CheckedNode{Node: node, Answered: true, Report: mcp.TestReport{
		Steps: []mcp.TestStep{
			{Stage: mcp.StageLaunch, State: mcp.StepPassed, Took: 40 * time.Millisecond, Detail: "the process started"},
			{Stage: mcp.StageDiscover, State: mcp.StepPassed, Took: 90 * time.Millisecond, Detail: "tracker-mcp 1.2.0"},
			{Stage: mcp.StageList, State: mcp.StepPassed, Took: 30 * time.Millisecond, Detail: "2 tools"},
		},
		Identity: mcp.Identity{Name: "tracker-mcp", Version: "1.2.0", Protocol: "2026-07-28"},
		Tools: []mcp.TestedTool{
			{Name: "tracker_search", Raw: "search", Description: "Searches.", Annotations: mcp.Annotations{ReadOnly: mcp.Yes}},
			{Name: "tracker_" + strings.Repeat("x", 60), Raw: strings.Repeat("x", 60), Refused: mcp.NameTooLong},
		},
		Took: 160 * time.Millisecond,
	}}
}

// fakeDrafts holds a draft to a dry run that refuses the bodies refused
// names, and otherwise answers a company holding the draft as written.
type fakeDrafts struct {
	refused map[string]error

	mu    sync.Mutex
	calls []string
}

func (f *fakeDrafts) CheckEntity(_ context.Context, kind, id string, body []byte, create bool,
	_ iam.Actor) (*config.Company, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, kind+"/"+id+map[bool]string{true: " create", false: " edit"}[create])
	if err, ok := f.refused[id]; ok {
		return nil, err
	}
	var server config.MCPServer
	if err := json.Unmarshal(body, &server); err != nil {
		return nil, &configapi.EntityError{Err: err}
	}
	return &config.Company{Name: "Acme", MCPServers: []config.MCPServer{server}}, nil
}

func (f *fakeDrafts) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// checkSurface is the surface over checker and drafts, auditing into audit,
// in a company that declares `tracker`.
func checkSurface(t *testing.T, checker mcpapi.Checker, drafts mcpapi.DraftChecker, audit *recordedAudit) *mcpapi.Service {
	t.Helper()
	company, err := config.ParseCompany([]byte("name: Acme\nmcp_servers:\n  - name: tracker\n    command: tracker-mcp\n"))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return company },
		Registry: &fakeRegistry{}, Detail: noDetail, Restarter: &fakeRestarter{}, Checker: checker,
		Drafts: drafts, Values: valueStore(t), SignIns: &fakeSignIns{}, Audit: audit})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// send makes one request as a principal, answering its status and body.
func send(t *testing.T, svc *mcpapi.Service, as iam.Principal, path, body string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(iam.WithPrincipal(t.Context(), as), http.MethodPost, path,
		strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s answered %d with %q", path, w.Code, w.Body.String())
	}
	return w.Code, out
}

// proved is an admin whose proof is d old: a step-up's window is an hour.
func proved(d time.Duration) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: iam.AllGrants, ReauthAt: time.Now().Add(-d).Add(time.Hour)}
}

// keysOf is a JSON object's keys, sorted.
func keysOf(v any) []string {
	m, _ := v.(map[string]any)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// A SAVED SERVER IS TESTED ON THE WORD OF AN ADMIN, AT ANY RECENCY, AND EVERY
// CALL IS AUDITED. An admin whose proof is old tests a shared server with an
// empty body, or a per-agent one for a seat; a member is refused by the
// guard; each refusal of the engine's is answered with what the person can
// change; and every call is one `mcp_server_acted` naming the server and the
// seat. The answer is exactly the keys the page reads.
//
// Mutation: mount the route on config.write, or answer a refusal as a
// success, and this is red.
func TestASavedServerIsTestedAndEveryCallAudited(t *testing.T) {
	t.Parallel()
	checker := &fakeChecker{refusals: map[string]error{
		"gone/":         mcpapi.ErrNoSuchServer,
		"off/":          mcpapi.ErrServerOff,
		"tracker/ghost": mcpapi.ErrNoSuchSeat,
		"tracker/cto":   mcpapi.ErrSeatNotGranted,
		"split/":        mcpapi.ErrSeatRequired,
		"broken/":       errors.New("the broker is down"),
	}}
	audit := &recordedAudit{}
	svc := checkSurface(t, checker, &fakeDrafts{}, audit)
	admin := proved(3 * time.Hour)

	code, body := send(t, svc, admin, "/mcp-servers/tracker/check", "")
	if code != http.StatusOK || body["ok"] != true || body["server"] != "tracker" || body["draft"] != false {
		t.Fatalf("a test of a saved server = %d %v", code, body)
	}
	if got := keysOf(body); !slices.Equal(got, []string{"at", "by", "draft", "node", "nodes", "ok", "seat",
		"server", "server_info", "tools", "warnings"}) {
		t.Errorf("the answer's keys = %v", got)
	}
	nodes, _ := body["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %v, want both nodes asked", body["nodes"])
	}
	if got := keysOf(nodes[0]); !slices.Equal(got, []string{"answered", "failure", "node", "ok", "reason",
		"refused", "server_info", "steps", "took_ms", "tools"}) {
		t.Errorf("a node's keys = %v", got)
	}
	steps, _ := nodes[0].(map[string]any)["steps"].([]any)
	if len(steps) != 3 || !slices.Equal(keysOf(steps[0]), []string{"detail", "stage", "state", "took_ms"}) {
		t.Errorf("steps = %v", steps)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 2 || !slices.Equal(keysOf(tools[0]), []string{"annotations", "description", "external",
		"name", "raw", "refused", "risk", "risk_declared", "starting_level", "title"}) {
		t.Fatalf("tools = %v", tools)
	}
	first, second := tools[0].(map[string]any), tools[1].(map[string]any)
	if first["starting_level"] != "allow" || second["starting_level"] != "approve" || second["refused"] != "too_long" {
		t.Errorf("a read-only tool starts at %v, an unannotated one at %v (refused %v)",
			first["starting_level"], second["starting_level"], second["refused"])
	}
	if warnings, _ := body["warnings"].([]any); len(warnings) != 1 || !strings.Contains(warnings[0].(string), "not offered") {
		t.Errorf("warnings = %v, want the refused tool said", body["warnings"])
	}
	if row := audit.last(t); row.Action != types.MCPServerCheck || row.Server != "tracker" || row.Seat != "" ||
		row.Draft || row.Outcome != types.AuditApplied {
		t.Errorf("audited %+v", row)
	}

	if code, _ := send(t, svc, admin, "/mcp-servers/tracker/check", `{"seat":"ceo"}`); code != http.StatusOK {
		t.Errorf("a test for a seat answered %d", code)
	}
	if got := checker.requests(); got[len(got)-1].Seat != "ceo" || got[len(got)-1].Entry != nil {
		t.Errorf("asked %+v, want the saved server for the seat", got[len(got)-1])
	}
	for _, tc := range []struct {
		path, body string
		code       int
		error      string
	}{
		{"/mcp-servers/gone/check", "", http.StatusNotFound, "not_found"},
		{"/mcp-servers/off/check", "", http.StatusConflict, "conflict"},
		{"/mcp-servers/tracker/check", `{"seat":"ghost"}`, http.StatusNotFound, "no_such_seat"},
		{"/mcp-servers/tracker/check", `{"seat":"cto"}`, http.StatusConflict, "seat_not_granted"},
		{"/mcp-servers/split/check", "", http.StatusBadRequest, "invalid_body"},
		{"/mcp-servers/broken/check", "", http.StatusServiceUnavailable, "unavailable"},
		{"/mcp-servers/tracker/check", `{"seat":"ceo","force":true}`, http.StatusBadRequest, "invalid_body"},
	} {
		code, body := send(t, svc, admin, tc.path, tc.body)
		if code != tc.code || body["error"] != tc.error {
			t.Errorf("%s %s = %d %v, want %d %s", tc.path, tc.body, code, body, tc.code, tc.error)
		}
		if row := audit.last(t); row.Outcome == types.AuditApplied {
			t.Errorf("%s %s audited as applied", tc.path, tc.body)
		}
	}

	before := len(checker.requests())
	member := caller(slices.DeleteFunc(slices.Clone(iam.AllGrants), func(g iam.Grant) bool { return g == iam.GrantAdmin })...)
	code, body = send(t, svc, member, "/mcp-servers/tracker/check", "")
	if code != http.StatusForbidden || len(checker.requests()) != before {
		t.Errorf("a member's test = %d %v, want 403 and nothing asked", code, body)
	}
	if row := audit.last(t); row.Outcome != types.AuditRefused || row.Server != "tracker" {
		t.Errorf("the guard's refusal audited %+v", row)
	}
}

// A DRAFT IS DECIDED AS THE WRITE IT STANDS FOR, BEFORE ITS BODY IS READ: an
// admin whose proof is old is refused step_up_required with nothing held to
// the dry run and nothing tested, whatever the body says; a fresh one has the
// draft held to the configuration's dry run (a create for a new name, an edit
// for one the company declares), a refused draft answered as its write would
// be, and an admitted one tested as the dry run left it, never recorded.
func TestADraftIsDecidedAsTheWriteItStandsForBeforeItIsRead(t *testing.T) {
	t.Parallel()
	checker := &fakeChecker{}
	drafts := &fakeDrafts{refused: map[string]error{
		"bad": &configapi.ValidationError{Err: errors.New("mcp_servers[0].url: is plain http to a public host")},
	}}
	audit := &recordedAudit{}
	svc := checkSurface(t, checker, drafts, audit)

	code, body := send(t, svc, proved(2*time.Hour), "/mcp-checks", `not even json`)
	if code != http.StatusForbidden || body["error"] != "step_up_required" {
		t.Fatalf("a draft by a stale proof = %d %v, want 403 step_up_required", code, body)
	}
	if len(drafts.seen()) != 0 || len(checker.requests()) != 0 {
		t.Fatal("a refused draft was read or tested")
	}
	if row := audit.last(t); row.Action != types.MCPServerCheck || !row.Draft || row.Outcome != types.AuditRefused {
		t.Errorf("the refusal audited %+v", row)
	}

	fresh := proved(time.Minute)
	code, body = send(t, svc, fresh, "/mcp-checks", `{"entity":{"name":"wiki","command":"wiki-mcp"}}`)
	if code != http.StatusOK || body["draft"] != true || body["server"] != "wiki" {
		t.Fatalf("a new draft = %d %v", code, body)
	}
	got := checker.requests()
	if len(got) != 1 || !got[0].Draft || got[0].Entry == nil || got[0].Entry.Command != "wiki-mcp" {
		t.Fatalf("asked %+v, want the draft as the dry run left it", got)
	}
	if code, _ := send(t, svc, fresh, "/mcp-checks", `{"entity":{"name":"tracker","command":"tracker-mcp-2"}}`); code != http.StatusOK {
		t.Errorf("an edit's draft answered %d", code)
	}
	if seen := drafts.seen(); !slices.Equal(seen, []string{"mcp-servers/wiki create", "mcp-servers/tracker edit"}) {
		t.Errorf("held to the dry run as %v, want a create then an edit", seen)
	}
	if row := audit.last(t); !row.Draft || row.Server != "tracker" || row.Outcome != types.AuditApplied {
		t.Errorf("audited %+v", row)
	}
	code, body = send(t, svc, fresh, "/mcp-checks", `{"entity":{"name":"bad","transport":"http","url":"http://x.example"}}`)
	if code != http.StatusUnprocessableEntity || body["error"] != "validation_error" {
		t.Errorf("a draft the dry run refused = %d %v, want 422", code, body)
	}
	if code, body := send(t, svc, fresh, "/mcp-checks", `{"seat":"ceo"}`); code != http.StatusBadRequest {
		t.Errorf("a draft with no entity = %d %v, want 400", code, body)
	}
	if len(checker.requests()) != 2 {
		t.Errorf("a refused draft was tested: %+v", checker.requests())
	}
}

// AN ADDRESS IS CHECKED ONLY BY WHO MAY MAKE THE ENGINE DIAL IT, AND THE
// TRAIL NAMES IT WITH NO CREDENTIAL: a stale proof is refused before the body
// is read, and a fresh one is answered the walk, the sign-in the server asks
// for and the curated entry, the address's key withheld in the answer and the
// audit alike.
func TestAnAddressIsCheckedOnlyByWhoMayMakeTheEngineDialIt(t *testing.T) {
	t.Parallel()
	checker := &fakeChecker{}
	audit := &recordedAudit{}
	svc := checkSurface(t, checker, &fakeDrafts{}, audit)
	const address = "https://mcp.example.com/mcp?api_key=opaque-key-value-42"

	if code, body := send(t, svc, proved(2*time.Hour), "/mcp-probe", `{"url":"`+address+`"}`); code != http.StatusForbidden {
		t.Fatalf("a probe by a stale proof = %d %v, want 403", code, body)
	}
	if len(checker.addresses) != 0 {
		t.Fatal("a refused probe dialled")
	}
	code, body := send(t, svc, proved(time.Minute), "/mcp-probe", `{"url":"`+address+`"}`)
	if code != http.StatusOK || body["auth"] != "oauth" || body["reachable"] != true {
		t.Fatalf("a probe = %d %v", code, body)
	}
	if got := keysOf(body); !slices.Equal(got, []string{"address", "auth", "catalogue", "check", "oauth",
		"reachable", "title", "tools", "vendor"}) {
		t.Errorf("the answer's keys = %v", got)
	}
	if got := keysOf(body["oauth"]); !slices.Equal(got, []string{"authorization_servers", "client", "failure",
		"issuer", "pkce", "resource", "resource_metadata", "scopes"}) {
		t.Errorf("oauth's keys = %v", got)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "opaque-key-value-42") {
		t.Errorf("the answer writes the address's key: %s", raw)
	}
	row := audit.last(t)
	if row.Action != types.MCPServerProbe || row.Outcome != types.AuditApplied || row.Address == "" ||
		strings.Contains(row.Address, "opaque-key-value-42") {
		t.Errorf("audited %+v, want the address with its key withheld", row)
	}
	if code, _ := send(t, svc, proved(time.Minute), "/mcp-probe", `{}`); code != http.StatusBadRequest {
		t.Errorf("a probe with no address answered %d", code)
	}
}

// A SURFACE WITH NO CHECKER IS NOT BUILT: a test is asked of the nodes that
// run a server through the engine, and nothing else can.
func TestASurfaceNeedsAChecker(t *testing.T) {
	t.Parallel()
	if _, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return nil },
		Detail: noDetail, Restarter: &fakeRestarter{}, Audit: &recordedAudit{}}); err == nil {
		t.Error("a surface with no checker was built")
	}
}

// A DRAFT IS TESTED WITH THE VALUES TYPED FOR IT, each read through the
// variable the values route would store it under in the company the draft
// makes (the key's own where only it reads one, MCP_<SERVER>_<KEY> otherwise),
// and no answer, refusal or audit row carries one. Mutation: drop the values
// from the request, or choose the variable anywhere but the slot, and this is
// red.
func TestADraftIsTestedWithTheValuesTypedForIt(t *testing.T) {
	t.Parallel()
	checker := &fakeChecker{}
	audit := &recordedAudit{}
	svc := checkSurface(t, checker, &fakeDrafts{}, audit)
	fresh := proved(time.Minute)
	const typed = "Bearer typed-value-not-real"
	draft := `{"entity":{"name":"wiki","transport":"http","url":"https://wiki.example.com/mcp",` +
		`"headers":{"X-Team":"${WIKI_TEAM}"}},"values":{"X-Team":"team-value-not-real","Authorization":"  ` + typed + `  "}}`

	code, body := send(t, svc, fresh, "/mcp-checks", draft)
	if code != http.StatusOK || body["draft"] != true {
		t.Fatalf("a draft with typed values = %d %v", code, body)
	}
	asked := checker.requests()
	if len(asked) != 1 || !slices.Equal(asked[0].Values, []mcpapi.ValueTry{
		{Key: "Authorization", Variable: "MCP_WIKI_AUTHORIZATION", Value: typed},
		{Key: "X-Team", Variable: "WIKI_TEAM", Value: "team-value-not-real"},
	}) {
		t.Fatalf("tested %+v, want each trimmed value under the variable it would be stored under", asked)
	}
	raw, _ := json.Marshal(body)
	row, _ := json.Marshal(audit.last(t))
	for _, said := range []string{string(raw), string(row)} {
		if strings.Contains(said, "not-real") {
			t.Errorf("a value under test was written back: %s", said)
		}
	}

	for name, tc := range map[string]struct {
		values string
		code   int
	}{
		"a value over two lines":  {`{"Authorization":"Bearer one-not-real\nBearer two"}`, http.StatusBadRequest},
		"an empty value":          {`{"Authorization":"   "}`, http.StatusBadRequest},
		"a key no value may take": {`{"Authorization: x":"Bearer one-not-real"}`, http.StatusBadRequest},
		"values over one stored value's bound": {
			`{"A":"` + strings.Repeat("a", secrets.MaxValueBytes/2+1) + `","B":"` +
				strings.Repeat("b", secrets.MaxValueBytes/2+1) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		code, body := send(t, svc, fresh, "/mcp-checks",
			`{"entity":{"name":"wiki","transport":"http","url":"https://wiki.example.com/mcp"},"values":`+tc.values+`}`)
		said, _ := json.Marshal(body)
		if code != tc.code || strings.Contains(string(said), "not-real") || strings.Contains(string(said), "aaaa") {
			t.Errorf("%s = %d %s, want %d naming no value", name, code, said, tc.code)
		}
	}
	if len(checker.requests()) != 1 {
		t.Errorf("a refused draft was tested: %+v", checker.requests())
	}
}
