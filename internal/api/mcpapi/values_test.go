package mcpapi_test

import (
	"context"
	"crypto/sha256"
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
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/secrets"
)

// theValue is the value every case stores: long enough to be withheld from
// any sentence, and plainly not a credential anybody issued.
const theValue = "Bearer lin_api_not-a-real-key-0001"

// valueStore is the fleet's secret store over a memory fleet and one key.
func valueStore(t *testing.T) *fleetsecrets.Store {
	t.Helper()
	sum := sha256.Sum256([]byte("k1"))
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": sum[:]}})
	if err != nil {
		t.Fatal(err)
	}
	return fleetsecrets.New(coordmem.NewFleet(), cipher)
}

// valuesDoc declares a remote server whose key reads nothing yet, one whose
// key already reads its own variable, and a per-agent server granted to dev.
const valuesDoc = `
name: Acme
mcp_servers:
  - {name: linear, transport: http, url: "https://mcp.linear.app/mcp", headers: {Authorization: "${LINEAR_KEY}"}}
  - {name: sentry, transport: http, url: "https://mcp.sentry.dev/mcp"}
  - {name: tracker, command: tracker-mcp, shared: false}
roles:
  - name: Dev
    handle: dev
    mcp_env:
      tracker: {REGION: "eu"}
`

// fakeDocuments is the configuration surface over one company: a dry run
// validates the pointed copy, a write points the company itself.
type fakeDocuments struct {
	managed bool
	refuse  error

	mu       sync.Mutex
	company  *config.Company
	pointed  []string
	reloaded []string
	checked  int
}

func newDocuments(t *testing.T) *fakeDocuments {
	t.Helper()
	company, err := config.ParseCompany([]byte(valuesDoc))
	if err != nil {
		t.Fatal(err)
	}
	return &fakeDocuments{company: company}
}

func (f *fakeDocuments) Authorize(by iam.Actor) error {
	if f.managed {
		return &configapi.ManagedError{By: by, Writers: []string{"gitops"}}
	}
	return nil
}

func (f *fakeDocuments) ActiveCompany(context.Context) (*config.Company, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := json.Marshal(f.company)
	if err != nil {
		return nil, "", err
	}
	copied, err := config.DecodeCompany(raw)
	return copied, "rev-1", err
}

func (f *fakeDocuments) CheckMCPValue(ctx context.Context, slot config.MCPValueSlot, _ iam.Actor) (*config.Company, error) {
	f.mu.Lock()
	f.checked++
	f.mu.Unlock()
	copied, _, err := f.ActiveCompany(ctx)
	if err != nil {
		return nil, err
	}
	if err = copied.PointMCPValue(slot); err != nil {
		return nil, err
	}
	return copied, copied.Validate()
}

func (f *fakeDocuments) PointMCPValue(_ context.Context, slot config.MCPValueSlot, summary string,
	_ iam.Actor) (configapi.Applied, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		return configapi.Applied{}, f.refuse
	}
	if err := f.company.PointMCPValue(slot); err != nil {
		return configapi.Applied{}, err
	}
	f.pointed = append(f.pointed, summary)
	return configapi.Applied{RevisionID: "rev-2", Epoch: 2}, nil
}

func (f *fakeDocuments) Reload(_ context.Context, summary string, _ iam.Actor, _ string) (configapi.Applied, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloaded = append(f.reloaded, summary)
	return configapi.Applied{RevisionID: "rev-2", Epoch: 2}, nil
}

// valueChecker answers a test of a value with the outcome a case set, and
// records what it was asked.
type valueChecker struct {
	fakeChecker
	fail *mcp.Failure
}

func (f *valueChecker) CheckServer(ctx context.Context, req mcpapi.CheckRequest) (mcpapi.Check, error) {
	done, err := f.fakeChecker.CheckServer(ctx, req)
	if err != nil || f.fail == nil {
		return done, err
	}
	done.OK = false
	done.Nodes = []mcpapi.CheckedNode{{Node: "node-a", Answered: true, Report: mcp.TestReport{
		Steps: []mcp.TestStep{
			{Stage: mcp.StageAddress, State: mcp.StepPassed},
			{Stage: mcp.StageResolve, State: mcp.StepPassed},
			{Stage: mcp.StageConnect, State: mcp.StepPassed},
			{Stage: mcp.StageTLS, State: mcp.StepPassed},
			{Stage: mcp.StageDiscover, State: mcp.StepFailed, Detail: f.fail.Said},
			{Stage: mcp.StageList, State: mcp.StepNotReached},
		},
		Failure: f.fail,
	}}}
	return done, nil
}

// valueSurface is the surface over a checker and a company's documents,
// auditing into audit and sealing into store.
func valueSurface(t *testing.T, checker mcpapi.Checker, docs *fakeDocuments, store *fleetsecrets.Store,
	audit *recordedAudit) *mcpapi.Service {
	t.Helper()
	svc, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return docs.company },
		Registry: &fakeRegistry{}, Detail: noDetail, Restarter: &fakeRestarter{}, Checker: checker,
		Documents: docs, Values: store, SignIns: &fakeSignIns{}, Audit: audit,
		Now: func() time.Time { return time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// put stores body under path as a principal, answering the status and the raw
// answer.
func put(t *testing.T, svc *mcpapi.Service, as iam.Principal, path, body string) (int, string) {
	t.Helper()
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(iam.WithPrincipal(t.Context(), as), http.MethodPut, path,
		strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func decoded(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("not JSON: %q", raw)
	}
	return out
}

func valueBody(value string) string {
	raw, _ := json.Marshal(map[string]string{"value": value})
	return string(raw)
}

// A value is sealed only after its test passes (plan, A8): a failed test stores
// and points nothing. Mutation: seal before the test, and this is red.
func TestAValueIsSealedOnlyAfterItsTestPasses(t *testing.T) {
	t.Parallel()
	checker := &valueChecker{fail: &mcp.Failure{Class: mcp.FailCredentialRejected,
		Said: "the server answered 401 to the credential it was sent"}}
	docs, store, audit := newDocuments(t), valueStore(t), &recordedAudit{}
	svc := valueSurface(t, checker, docs, store, audit)
	admin := proved(time.Minute)

	code, raw := put(t, svc, admin, "/mcp-servers/sentry/values/Authorization", valueBody(theValue))
	body := decoded(t, raw)
	if code != http.StatusUnprocessableEntity || body["error"] != "value_test_failed" ||
		body["stage"] != string(mcp.StageDiscover) || body["class"] != string(mcp.FailCredentialRejected) {
		t.Fatalf("a refused value = %d %v, want 422 value_test_failed at discover", code, body)
	}
	if rows, _ := store.List(t.Context()); len(rows) != 0 {
		t.Fatalf("a value whose test failed was sealed: %+v", rows)
	}
	if len(docs.pointed) != 0 || len(docs.reloaded) != 0 {
		t.Fatalf("a value whose test failed changed the company: %v %v", docs.pointed, docs.reloaded)
	}
	if row := audit.last(t); row.Action != types.MCPServerStoreValue || row.Outcome != types.AuditRefused ||
		row.Refusal != "value_test_failed" || row.Variable != "MCP_SENTRY_AUTHORIZATION" {
		t.Errorf("the refusal audited %+v", row)
	}

	checker.fail = nil
	code, raw = put(t, svc, admin, "/mcp-servers/sentry/values/Authorization", valueBody("  "+theValue+"\n"))
	body = decoded(t, raw)
	if code != http.StatusOK || body["variable"] != "MCP_SENTRY_AUTHORIZATION" || body["tested"] != true ||
		body["pointed"] != true {
		t.Fatalf("a value that works = %d %v", code, body)
	}
	asked := checker.requests()
	if last := asked[len(asked)-1]; len(last.Values) != 1 || last.Values[0].Key != "Authorization" ||
		last.Values[0].Variable != "MCP_SENTRY_AUTHORIZATION" || last.Values[0].Value != theValue || last.Seat != "" {
		t.Fatalf("tested %+v, want the trimmed value read through the variable it is sealed under", last)
	}
	row, found, err := store.Describe(t.Context(), "MCP_SENTRY_AUTHORIZATION")
	if err != nil || !found || !row.WriteOnly || row.Source != secrets.SourceMCP || row.UpdatedBy != "jane.doe" {
		t.Fatalf("the sealed row = %+v (%v, %v), want it write-only from mcp by jane.doe", row, found, err)
	}
	all, err := store.All(t.Context())
	if err != nil || all["MCP_SENTRY_AUTHORIZATION"] != theValue {
		t.Fatalf("the resolver does not read the sealed value: %v", err)
	}
	if got := docs.company.MCPServers[1].Headers["Authorization"]; got != "${MCP_SENTRY_AUTHORIZATION}" ||
		!slices.Equal(docs.pointed, []string{"store Authorization of MCP server sentry as MCP_SENTRY_AUTHORIZATION"}) {
		t.Fatalf("the key reads %q after %v", got, docs.pointed)
	}
	if row := audit.last(t); row.Outcome != types.AuditApplied || row.Key != "Authorization" {
		t.Errorf("the store audited %+v", row)
	}
}

// No answer or audit record carries the value (plan, A8), and the answer has
// exactly its keys. Mutation: echo the value in a refusal, and this is red.
func TestTheAnswerNeverCarriesTheValue(t *testing.T) {
	t.Parallel()
	checker := &valueChecker{}
	docs, audit := newDocuments(t), &recordedAudit{}
	svc := valueSurface(t, checker, docs, valueStore(t), audit)
	admin := proved(time.Minute)

	answers := map[string]string{}
	code, raw := put(t, svc, admin, "/mcp-servers/linear/values/Authorization", valueBody(theValue))
	if code != http.StatusOK {
		t.Fatalf("store = %d %s", code, raw)
	}
	answers["a store"] = raw
	body := decoded(t, raw)
	if got := keysOf(body); !slices.Equal(got, []string{"check", "key", "pointed", "reload", "seat", "server",
		"tested", "values", "variable"}) {
		t.Errorf("the answer's keys = %v", got)
	}
	values, _ := body["values"].([]any)
	if len(values) != 1 || !slices.Equal(keysOf(values[0]), []string{"as", "bound_by", "key", "optional", "set",
		"stored_at", "stored_by", "variable"}) {
		t.Fatalf("values = %v, want one row in exactly the value state's keys", body["values"])
	}
	state := values[0].(map[string]any)
	if state["variable"] != "LINEAR_KEY" || state["set"] != true || state["as"] != "header" ||
		state["stored_by"] != "jane.doe" || state["stored_at"] != "2026-11-02T09:00:00Z" {
		t.Errorf("the value's state = %v", state)
	}
	checker.fail = &mcp.Failure{Class: mcp.FailCredentialRejected, Said: "401 for the credential [withheld]"}
	_, answers["a failed test"] = put(t, svc, admin, "/mcp-servers/linear/values/Authorization", valueBody(theValue))
	_, answers["a key no server reads"] = put(t, svc, admin, "/mcp-servers/linear/values/Bad%20Key", valueBody(theValue))
	_, answers["a server nobody declares"] = put(t, svc, admin, "/mcp-servers/absent/values/X", valueBody(theValue))
	for what, raw := range answers {
		if strings.Contains(raw, "lin_api") {
			t.Errorf("%s carries the value: %s", what, raw)
		}
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	for _, row := range audit.rows {
		if encoded, _ := json.Marshal(row); strings.Contains(string(encoded), "lin_api") {
			t.Errorf("an audit record carries the value: %s", encoded)
		}
	}
}

// A key already reading its own variable is rotated in place and re-activated.
func TestAKeyThatReadsItsOwnVariableIsReactivatedNotRepointed(t *testing.T) {
	t.Parallel()
	docs, store := newDocuments(t), valueStore(t)
	svc := valueSurface(t, &valueChecker{}, docs, store, &recordedAudit{})
	code, raw := put(t, svc, proved(time.Minute), "/mcp-servers/linear/values/authorization?test=false",
		valueBody(theValue))
	body := decoded(t, raw)
	if code != http.StatusOK || body["pointed"] != false || body["key"] != "Authorization" ||
		body["variable"] != "LINEAR_KEY" || body["tested"] != false || body["check"] != nil {
		t.Fatalf("a rotation = %d %v", code, body)
	}
	if reload, _ := body["reload"].(map[string]any); reload["revision_id"] != "rev-2" {
		t.Errorf("reload = %v, want the re-activation", body["reload"])
	}
	if len(docs.pointed) != 0 || len(docs.reloaded) != 1 || docs.checked != 0 {
		t.Errorf("pointed %v, reloaded %v, dry runs %d: want one re-activation alone",
			docs.pointed, docs.reloaded, docs.checked)
	}
	if _, err := store.Get(t.Context(), "LINEAR_KEY"); !errors.Is(err, secrets.ErrWriteOnly) {
		t.Errorf("the rotated value reads back: %v", err)
	}
}

// A stale proof, the assistant and a managed document are refused before the
// body is read. Mutation: drop the config.write admission, and this is red.
func TestAValueIsDecidedBeforeItIsRead(t *testing.T) {
	t.Parallel()
	checker := &valueChecker{}
	docs, store, audit := newDocuments(t), valueStore(t), &recordedAudit{}
	svc := valueSurface(t, checker, docs, store, audit)

	code, raw := put(t, svc, proved(2*time.Hour), "/mcp-servers/sentry/values/Authorization", "not even json")
	if body := decoded(t, raw); code != http.StatusForbidden || body["error"] != "step_up_required" {
		t.Fatalf("a stale proof = %d %v, want 403 step_up_required", code, body)
	}
	assistant := iam.Principal{ID: uuid.New(), Login: "ana.admin", Kind: iam.KindPerson,
		Seat: "ana", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour),
		Via:      iam.AssistName(uuid.Must(uuid.NewV7())), AssistScope: iam.AssistScopeExec}
	code, raw = put(t, svc, assistant, "/mcp-servers/sentry/values/Authorization", valueBody(theValue))
	if code != http.StatusForbidden {
		t.Fatalf("the assistant = %d %s, want 403", code, raw)
	}
	// CONFIG.WRITE ON ITS OWN: today the same rows admit both verbs, so a
	// guard refusing config.write alone is what tells them apart.
	noConfigWrite, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return docs.company },
		Registry: &fakeRegistry{}, Detail: noDetail, Restarter: &fakeRestarter{}, Checker: checker,
		Documents: docs, Values: store, SignIns: &fakeSignIns{}, Audit: audit,
		Guard: func(r *http.Request, p authz.Policy) authz.Decision {
			if p.Action == authz.ActionConfigWrite {
				return authz.Decision{Allowed: false}
			}
			return authz.ContextGuard(authz.NoChart{})(r, p)
		}})
	if err != nil {
		t.Fatal(err)
	}
	if code, raw = put(t, noConfigWrite, proved(time.Minute), "/mcp-servers/sentry/values/Authorization",
		"not even json"); code != http.StatusForbidden {
		t.Fatalf("no config.write = %d %s, want 403 before the body is read", code, raw)
	}
	docs.managed = true
	code, raw = put(t, svc, proved(time.Minute), "/mcp-servers/sentry/values/Authorization", "not even json")
	if body := decoded(t, raw); code != http.StatusForbidden || body["error"] != "config_managed" {
		t.Fatalf("a managed document = %d %v, want 403 config_managed", code, body)
	}
	if len(checker.requests()) != 0 || docs.checked != 0 {
		t.Fatal("a refused value was tested or planned")
	}
	if rows, _ := store.List(t.Context()); len(rows) != 0 {
		t.Fatalf("a refused value was sealed: %+v", rows)
	}
	if row := audit.last(t); row.Action != types.MCPServerStoreValue || row.Outcome != types.AuditRefused {
		t.Errorf("the refusal audited %+v", row)
	}
}

// A per-agent server's value is a seat's, tested as that seat; its own value
// is stored only untested.
func TestAPerAgentValueIsTestedAsItsSeat(t *testing.T) {
	t.Parallel()
	checker := &valueChecker{}
	docs, store := newDocuments(t), valueStore(t)
	svc := valueSurface(t, checker, docs, store, &recordedAudit{})
	admin := proved(time.Minute)

	code, raw := put(t, svc, admin, "/mcp-servers/tracker/values/TOKEN?seat=dev", valueBody(theValue))
	if body := decoded(t, raw); code != http.StatusOK || body["variable"] != "MCP_TRACKER_TOKEN_DEV" ||
		body["seat"] != "dev" {
		t.Fatalf("dev's value = %d %v", code, body)
	}
	if last := checker.requests()[0]; last.Seat != "dev" || len(last.Values) != 1 {
		t.Fatalf("tested %+v, want dev's instance", last)
	}
	if got := docs.company.Roles[0].MCPEnv["tracker"]; got["TOKEN"] != "${MCP_TRACKER_TOKEN_DEV}" || got["REGION"] != "eu" {
		t.Fatalf("dev's block = %v", got)
	}
	code, raw = put(t, svc, admin, "/mcp-servers/tracker/values/TOKEN", valueBody(theValue))
	if body := decoded(t, raw); code != http.StatusBadRequest || body["error"] != "invalid_query" {
		t.Fatalf("a per-agent server's own value, tested = %d %v, want 400", code, body)
	}
	if code, raw = put(t, svc, admin, "/mcp-servers/tracker/values/TOKEN?test=false", valueBody(theValue)); code != http.StatusOK {
		t.Fatalf("a per-agent server's own value, untested = %d %s", code, raw)
	}
	for path, want := range map[string]int{
		"/mcp-servers/tracker/values/TOKEN?seat=nobody": http.StatusNotFound,
		"/mcp-servers/linear/values/TOKEN?seat=dev":     http.StatusBadRequest,
	} {
		if code, raw := put(t, svc, admin, path, valueBody(theValue)); code != want {
			t.Errorf("%s = %d %s, want %d", path, code, raw, want)
		}
	}
}

// A value that cannot be sent as written is refused before any test, unquoted.
func TestAValueTheServerCannotBeSentIsRefused(t *testing.T) {
	t.Parallel()
	checker := &valueChecker{}
	svc := valueSurface(t, checker, newDocuments(t), valueStore(t), &recordedAudit{})
	admin := proved(time.Minute)
	for name, tc := range map[string]struct{ path, body string }{
		"no value":           {"/mcp-servers/sentry/values/X-Org", `{}`},
		"an empty value":     {"/mcp-servers/sentry/values/X-Org", valueBody("   ")},
		"a line break":       {"/mcp-servers/sentry/values/X-Org", valueBody("Bearer lin_api_x\r\nX-Evil: 1")},
		"another field":      {"/mcp-servers/sentry/values/X-Org", `{"value": "x", "name": "y"}`},
		"another param":      {"/mcp-servers/sentry/values/X-Org?dry_run=true", valueBody(theValue)},
		"a test not bool":    {"/mcp-servers/sentry/values/X-Org?test=maybe", valueBody(theValue)},
		"a test given twice": {"/mcp-servers/sentry/values/X-Org?test=true&test=false", valueBody(theValue)},
	} {
		code, raw := put(t, svc, admin, tc.path, tc.body)
		if code != http.StatusBadRequest || strings.Contains(raw, "lin_api") || strings.Contains(raw, "Evil") {
			t.Errorf("%s = %d %s, want 400 quoting nothing", name, code, raw)
		}
	}
	if len(checker.requests()) != 0 {
		t.Errorf("a refused value was tested: %+v", checker.requests())
	}
}

// A pointer refused after the seal says the value is stored; sending it again
// finishes it.
func TestAPointerRefusedAfterTheSealSaysWhatIsStored(t *testing.T) {
	t.Parallel()
	docs, store := newDocuments(t), valueStore(t)
	docs.refuse = &configapi.MCPValueMovedError{Sealed: "MCP_SENTRY_X_ORG", Now: "OTHER"}
	svc := valueSurface(t, &valueChecker{}, docs, store, &recordedAudit{})
	code, raw := put(t, svc, proved(time.Minute), "/mcp-servers/sentry/values/X-Org", valueBody(theValue))
	body := decoded(t, raw)
	if code != http.StatusConflict || body["stored"] != true || body["variable"] != "MCP_SENTRY_X_ORG" {
		t.Fatalf("a pointer refused after the seal = %d %v", code, body)
	}
	if strings.Contains(raw, "lin_api") {
		t.Fatalf("the refusal carries the value: %s", raw)
	}
	docs.refuse = nil
	if code, raw := put(t, svc, proved(time.Minute), "/mcp-servers/sentry/values/X-Org", valueBody(theValue)); code != http.StatusOK {
		t.Fatalf("the same request again = %d %s", code, raw)
	}
}

// A VALUE IS TESTED WITH ITS ENTRY AS THE FLEET'S ACTIVE REVISION STATES IT,
// sent with the test, even where the company this node applied does not have
// the server yet: a value stored straight after its entry's write is not
// refused for a server the node has not started. Mutation: send no entry, or
// the applied company's, and this is red.
func TestAValueIsTestedWithItsEntryAsTheActiveRevisionStatesIt(t *testing.T) {
	t.Parallel()
	checker := &valueChecker{}
	docs := newDocuments(t)
	applied, err := config.ParseCompany([]byte("name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return applied },
		Registry: &fakeRegistry{}, Detail: noDetail, Restarter: &fakeRestarter{}, Checker: checker,
		Documents: docs, Values: valueStore(t), SignIns: &fakeSignIns{}, Audit: &recordedAudit{},
		Now: func() time.Time { return time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	code, raw := put(t, svc, proved(time.Minute), "/mcp-servers/sentry/values/Authorization", valueBody(theValue))
	if code != http.StatusOK {
		t.Fatalf("a value for an entry the node has not applied = %d %s", code, raw)
	}
	asked := checker.requests()
	if len(asked) != 1 || asked[0].Entry == nil || asked[0].Entry.Name != "sentry" ||
		asked[0].Entry.URL != "https://mcp.sentry.dev/mcp" || asked[0].Draft {
		t.Fatalf("tested %+v, want the active revision's sentry entry sent with the test", asked)
	}
	if strings.Contains(raw, theValue) {
		t.Errorf("the answer carries the value: %s", raw)
	}
}

// A VALUE FOR AN AGENT THE SERVER IS NOT GIVEN is refused with its own code and
// the sentence a person reads for it, never a collision's, tested or not.
func TestAValueForAnAgentNotGivenTheServerSaysSo(t *testing.T) {
	t.Parallel()
	company, err := config.ParseCompany([]byte(`
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false, access: {except: [dev]}}
roles:
  - name: Dev
    handle: dev
`))
	if err != nil {
		t.Fatal(err)
	}
	checker := &valueChecker{}
	svc := valueSurface(t, checker, &fakeDocuments{company: company}, valueStore(t), &recordedAudit{})
	for _, path := range []string{
		"/mcp-servers/tracker/values/TOKEN?seat=dev", "/mcp-servers/tracker/values/TOKEN?seat=dev&test=false",
	} {
		code, raw := put(t, svc, proved(time.Minute), path, valueBody(theValue))
		body := decoded(t, raw)
		if code != http.StatusConflict || body["error"] != string(httpjson.CodeSeatNotGranted) ||
			body["message"] != httpjson.CodeSeatNotGranted.Message() || body["detail"] == "" {
			t.Errorf("%s = %d %v, want 409 seat_not_granted with its sentence and why", path, code, body)
		}
	}
	if asked := checker.requests(); len(asked) != 0 {
		t.Errorf("a seat not given the server was tested: %+v", asked)
	}
}
