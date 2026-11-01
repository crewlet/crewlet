package configapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
)

// serverRemovalDoc is a company whose server `linear` is named every way a
// removal has to clear: pm's own block and box entry, dev's block, and the eng
// unit's block. Its values are read thus: LINEAR_ORG by the server alone,
// PM_LINEAR and DEV_LINEAR by one seat's block each, and SHARED_TOKEN by the
// unit's block AND by `search`, which stays.
const serverRemovalDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    default:
      model: claude-opus-5-5
mcp_servers:
  - name: linear
    transport: http
    url: https://mcp.linear.example/mcp
    shared: false
    headers: {X-Org: "${LINEAR_ORG}"}
  - name: search
    command: search-mcp
    env: {SEARCH_KEY: "${SEARCH_KEY}", SHARED_TOKEN: "${SHARED_TOKEN}"}
roles:
  - name: PM
    handle: pm
    mcp_env:
      linear: {Authorization: "Bearer ${PM_LINEAR}"}
    sandbox: {mcp: {servers: [linear, search]}}
units:
  - name: Eng
    id: eng
    mcp_env:
      linear: {X-Team: "${SHARED_TOKEN}"}
    roles:
      - name: Dev
        handle: dev
        mcp_env:
          linear: {Authorization: "Bearer ${DEV_LINEAR}"}
`

// storedValues are the values the store holds for serverRemovalDoc: every one
// it reads but DEV_LINEAR, which a node takes from its environment. Each is a
// fake, and no answer may carry one.
var storedValues = map[string]string{
	"LINEAR_ORG": "fake-org-0001", "PM_LINEAR": "fake-pm-0002",
	"SHARED_TOKEN": "fake-shared-0003", "SEARCH_KEY": "fake-search-0004",
}

// serverSurface is a surface running serverRemovalDoc with its values stored.
func serverSurface(t *testing.T) *surface {
	t.Helper()
	s := newSurface(t)
	s.seed(t, serverRemovalDoc)
	for name, value := range storedValues {
		if err := s.secrets.Set(t.Context(), name, value, secrets.Author{Name: "test"},
			secrets.SourceAPI, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func removeServer(t *testing.T, s *surface, name, query, body string,
	headers map[string]string) (int, map[string]any) {

	t.Helper()
	res := s.do(t, http.MethodDelete, "/config/mcp-servers/"+name+query, body, headers)
	for _, value := range storedValues {
		if strings.Contains(res.Body.String(), value) {
			t.Fatalf("the answer carries a stored value: %s", res.Body)
		}
	}
	return res.Code, decode(t, res)
}

// stored reports whether the credential store holds a value under name.
func (s *surface) stored(t *testing.T, name string) bool {
	t.Helper()
	_, held, err := s.secrets.Describe(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// jsonOf is v as compact JSON, for comparing a part of an answer.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// normal is the JSON text as [jsonOf] spells a decoded answer: keys sorted.
func normal(t *testing.T, text string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	return jsonOf(t, v)
}

const wantDependants = `{"mcp_env":[` +
	`{"kind":"seat","key":"pm","name":"PM","segments":["roles",0,"mcp_env","linear"]},` +
	`{"kind":"seat","key":"dev","name":"Dev","segments":["units",0,"roles",0,"mcp_env","linear"]},` +
	`{"kind":"unit","key":"eng","name":"Eng","segments":["units",0,"mcp_env","linear"]}],` +
	`"sandbox":[{"seat":"pm","name":"PM","segments":["roles",0,"sandbox","mcp","servers",0]}],` +
	`"secrets":["LINEAR_ORG","PM_LINEAR"]}`

// A REMOVAL CLEARS EVERY DEPENDANT IN ONE REVISION OR IS REFUSED NAMING ALL OF
// THEM.
//
// Asked without `clear`, the refusal is the list: each field that names the
// server, at its field, and what the removal would take with it, including the
// stored values only it read (never DEV_LINEAR, which the store does not
// hold, nor SHARED_TOKEN, which `search` reads). With `clear`, the server and
// every one of those fields go in the same revision, and nothing else does.
//
// Mutation: drop the refusal without clear, or clear the unit's block, and
// this fails.
func TestAServerRemovalClearsEveryDependantInOneRevisionOrNamesThemAll(t *testing.T) {
	t.Parallel()
	s := serverSurface(t)
	before, _, err := s.active(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	code, body := removeServer(t, s, "linear", "?dry_run=true", "", nil)
	if code != http.StatusUnprocessableEntity || body["error"] != "validation_error" {
		t.Fatalf("a removal leaving dependants = %d %v, want 422 validation_error", code, body)
	}
	if got, want := jsonOf(t, body["dependants"]), normal(t, wantDependants); got != want {
		t.Errorf("dependants:\n got %s\nwant %s", got, want)
	}
	var segments []string
	for _, p := range body["problems"].([]any) {
		problem := p.(map[string]any)
		if problem["kind"] != "conflict" || !strings.Contains(problem["message"].(string), "send clear") {
			t.Errorf("problem = %v, want a conflict telling the caller to send clear", problem)
		}
		segments = append(segments, jsonOf(t, problem["segments"]))
	}
	if want := []string{`["roles",0,"mcp_env","linear"]`, `["roles",0,"sandbox","mcp","servers",0]`,
		`["units",0,"roles",0,"mcp_env","linear"]`, `["units",0,"mcp_env","linear"]`}; !slices.Equal(segments, want) {
		t.Errorf("problems at %v, want one at every field that names the server %v", segments, want)
	}

	code, body = removeServer(t, s, "linear", "?dry_run=true", `{"clear": true}`, nil)
	if code != http.StatusOK || body["valid"] != true {
		t.Fatalf("a dry run that clears = %d %v", code, body)
	}
	if got, want := jsonOf(t, body["dependants"]), normal(t, wantDependants); got != want {
		t.Errorf("the dry run's dependants:\n got %s\nwant %s", got, want)
	}
	if after, _, _ := s.active(t.Context()); after.ID != before.ID {
		t.Fatal("a dry run stored a revision")
	}

	code, body = removeServer(t, s, "linear", "", `{"clear": true}`,
		map[string]string{"X-Summary": "remove linear"})
	if code != http.StatusCreated || body["removed"] != "linear" || body["revision_id"] == "" {
		t.Fatalf("the removal = %d %v, want 201 naming the server and the revision", code, body)
	}
	if got, want := jsonOf(t, body["cleared"]),
		normal(t, wantDependants[:strings.Index(wantDependants, `,"secrets"`)]+"}"); got != want {
		t.Errorf("cleared:\n got %s\nwant %s", got, want)
	}
	if got := jsonOf(t, body["secrets"]); got != `[]` {
		t.Errorf("secrets = %s, want none deleted when none was named", got)
	}
	after, _, err := s.active(t.Context())
	if err != nil || after.ParentID != before.ID {
		t.Fatalf("the removal is revision %s on %s (%v), want one revision on %s",
			after.ID, after.ParentID, err, before.ID)
	}
	c := activeCompany(t, s)
	if len(c.MCPServers) != 1 || c.MCPServers[0].Name != "search" {
		t.Errorf("servers = %+v, want search alone", c.MCPServers)
	}
	if refs := c.MCPServerReferences("linear"); len(refs) != 0 {
		t.Errorf("linear is still named: %+v", refs)
	}
	if pm := c.Roles[0]; pm.MCPEnv != nil || !slices.Equal(pm.Sandbox.MCP.Servers, []string{"search"}) {
		t.Errorf("pm = %v %+v, want its block gone and search kept in its box", pm.MCPEnv, pm.Sandbox)
	}
	if c.Units[0].MCPEnv != nil || c.Units[0].Roles[0].MCPEnv != nil {
		t.Errorf("eng's and dev's blocks stay: %v %v", c.Units[0].MCPEnv, c.Units[0].Roles[0].MCPEnv)
	}
	for name := range storedValues {
		if !s.stored(t, name) {
			t.Errorf("%s was deleted, and the removal named no value to delete", name)
		}
	}
}

// ONLY A VALUE NOTHING ELSE READS MAY GO WITH THE SERVER.
//
// SHARED_TOKEN is read by `search` as well, so deleting it would sign search
// out: refused at its index, naming who reads it at the path the caller's
// document has now. LINEAR_ORGG, a typo, is read by nothing at all. The two
// linear alone read are deleted once the removal is written,
// and DEV_LINEAR, which no row holds, is said to be absent rather than
// refused. The answers carry names and never a value.
//
// Mutation: accept any name read before the removal, and SHARED_TOKEN is
// deleted from under search.
func TestOnlyAValueNothingElseReadsMayGoWithTheServer(t *testing.T) {
	t.Parallel()
	s := serverSurface(t)

	code, body := removeServer(t, s, "linear", "?dry_run=true",
		`{"clear": true, "delete_secrets": ["LINEAR_ORG", "SHARED_TOKEN", "LINEAR_ORGG"]}`, nil)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("deleting values others read = %d %v, want 422", code, body)
	}
	problems := body["problems"].([]any)
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want SHARED_TOKEN's and LINEAR_ORGG's", problems)
	}
	shared, foreign := problems[0].(map[string]any), problems[1].(map[string]any)
	if jsonOf(t, shared["segments"]) != `["delete_secrets",1]` || shared["kind"] != "conflict" ||
		!strings.Contains(shared["message"].(string), "still read by mcp_servers[1].env.SHARED_TOKEN:") {
		t.Errorf("SHARED_TOKEN's problem = %v", shared)
	}
	if jsonOf(t, foreign["segments"]) != `["delete_secrets",2]` || foreign["kind"] != "unknown_value" ||
		!strings.Contains(foreign["message"].(string), `is not read by "linear"`) {
		t.Errorf("LINEAR_ORGG's problem = %v", foreign)
	}

	code, body = removeServer(t, s, "linear", "",
		`{"clear": true, "delete_secrets": ["LINEAR_ORG", "PM_LINEAR", "DEV_LINEAR"],
		  "_summary": "remove linear and its credentials"}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("the removal = %d %v", code, body)
	}
	want := `[{"name":"LINEAR_ORG","outcome":"deleted"},{"name":"PM_LINEAR","outcome":"deleted"},` +
		`{"name":"DEV_LINEAR","outcome":"absent"}]`
	if got := jsonOf(t, body["secrets"]); got != want {
		t.Errorf("secrets:\n got %s\nwant %s", got, want)
	}
	for name, held := range map[string]bool{"LINEAR_ORG": false, "PM_LINEAR": false,
		"SHARED_TOKEN": true, "SEARCH_KEY": true} {
		if s.stored(t, name) != held {
			t.Errorf("%s stored = %v, want %v", name, !held, held)
		}
	}
}

// DELETING A VALUE ASKS `secrets.write` ON TOP OF THE ROUTE'S VERB.
//
// The Dashboard Assistant may take `config.write` on a person's approval and
// never `secrets.write`, so through it a server may be removed and its values
// may not: a removal naming one is refused before anything is written, its
// check included, so the card that would ask is never drawn.
//
// Mutation: drop the admission, and the assistant deletes the value.
func TestDeletingAServersValuesAsksSecretsWrite(t *testing.T) {
	t.Parallel()
	s := serverSurface(t)
	assistant := iam.Principal{ID: uuid.New(), Login: "ana.admin", Kind: iam.KindPerson,
		Seat: "ana", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour),
		Via:      iam.AssistName(uuid.Must(uuid.NewV7())), AssistScope: iam.AssistScopeExec}
	s.caller = &assistant
	deleting := `{"clear": true, "delete_secrets": ["LINEAR_ORG"], "_summary": "remove linear"}`

	for _, query := range []string{"?dry_run=true", ""} {
		code, body := removeServer(t, s, "linear", query, deleting, nil)
		if code != http.StatusForbidden || body["error"] != "unauthorized" ||
			body["reason"] != string(authz.ReasonAssistRefused) {
			t.Errorf("the assistant deleting a value (%q) = %d %v, want 403 unauthorized "+
				"for the assistant", query, code, body)
		}
	}
	if !s.stored(t, "LINEAR_ORG") || len(activeCompany(t, s).MCPServers) != 2 {
		t.Fatal("a refused removal deleted a value or the server")
	}
	code, body := removeServer(t, s, "linear", "", `{"clear": true, "_summary": "remove linear"}`, nil)
	if code != http.StatusCreated {
		t.Errorf("the assistant removing the server alone = %d %v, want 201", code, body)
	}
}

// A VALUE A WRITE LANDING MEANWHILE READS IS KEPT.
//
// The values go after the removal's revision is written, and a write can land
// between the two: one that has `search` read LINEAR_ORG too. Each value is
// asked again of the fleet's revision just before it goes, so LINEAR_ORG is
// kept, said with the path that now reads it, while PM_LINEAR goes.
//
// Mutation: delete on the removal's own decision alone, and LINEAR_ORG is
// deleted from under search.
func TestAValueAWriteMeanwhileReadsIsKept(t *testing.T) {
	t.Parallel()
	meanwhile := &afterActivation{}
	s := newSurfaceWith(t, func(o *configapi.Options) {
		meanwhile.Plane = o.Plane
		o.Plane = meanwhile
	})
	s.seed(t, serverRemovalDoc)
	for _, name := range []string{"LINEAR_ORG", "PM_LINEAR"} {
		if err := s.secrets.Set(t.Context(), name, storedValues[name], secrets.Author{Name: "test"},
			secrets.SourceAPI, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	// WHAT THE COMPANY IS ONCE linear IS GONE, with search reading the
	// organisation's value too: the write a colleague lands meanwhile.
	readsOrg := `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    default:
      model: claude-opus-5-5
mcp_servers:
  - name: search
    command: search-mcp
    env: {SEARCH_KEY: "${SEARCH_KEY}", SHARED_TOKEN: "${SHARED_TOKEN}", ORG: "${LINEAR_ORG}"}
roles:
  - name: PM
    handle: pm
    sandbox: {mcp: {servers: [search]}}
units:
  - name: Eng
    id: eng
    roles:
      - name: Dev
        handle: dev
`
	write := func() {
		if res := s.do(t, http.MethodPut, "/config", readsOrg,
			map[string]string{"X-Summary": "search reads the org"}); res.Code != http.StatusCreated {
			t.Errorf("the write meanwhile = %d %s", res.Code, res.Body)
		}
	}
	// ARMED BY THE REMOVAL'S OWN ACTIVATION, not the seed's.
	meanwhile.activated.Store(false)
	meanwhile.armed.Store(&write)

	code, body := removeServer(t, s, "linear", "", `{"clear": true,
		"delete_secrets": ["LINEAR_ORG", "PM_LINEAR"], "_summary": "remove linear"}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("the removal = %d %v", code, body)
	}
	if meanwhile.armed.Load() != nil {
		t.Fatal("the write meanwhile never ran: the case proves nothing")
	}
	want := `[{"name":"LINEAR_ORG","outcome":"kept","read_by":["mcp_servers[0].env.ORG"]},` +
		`{"name":"PM_LINEAR","outcome":"deleted"}]`
	if got := jsonOf(t, body["secrets"]); got != want {
		t.Errorf("secrets:\n got %s\nwant %s", got, want)
	}
	if !s.stored(t, "LINEAR_ORG") || s.stored(t, "PM_LINEAR") {
		t.Error("want LINEAR_ORG kept for search and PM_LINEAR deleted")
	}
}

// afterActivation is a fleet on which, once an activation has landed, the next
// read of the pointer first runs the write armed: another write landing just
// after the request's own, before anything else the request reads.
type afterActivation struct {
	coord.Plane
	activated atomic.Bool
	armed     atomic.Pointer[func()]
}

func (p *afterActivation) Activate(ctx context.Context,
	req coord.ActivationRequest) (coord.Activation, error) {

	a, err := p.Plane.Activate(ctx, req)
	if err == nil {
		p.activated.Store(true)
	}
	return a, err
}

func (p *afterActivation) Target(ctx context.Context) (coord.Activation, bool, error) {
	if p.activated.Load() {
		if write := p.armed.Swap(nil); write != nil {
			(*write)()
		}
	}
	return p.Plane.Target(ctx)
}

// A VALUE THE STORE DID NOT DELETE IS SAID, beside a removal that happened.
//
// The revision is written before any value goes, so a store that fails the
// delete leaves the server removed and the value where it was: 201, with that
// value's outcome `failed`, never a 500 that reads as though nothing changed.
func TestAValueTheStoreDidNotDeleteIsSaid(t *testing.T) {
	t.Parallel()
	s := newSurfaceWith(t, func(o *configapi.Options) { o.Secrets = failingUnset{o.Secrets} })
	s.seed(t, serverRemovalDoc)
	if err := s.secrets.Set(t.Context(), "LINEAR_ORG", storedValues["LINEAR_ORG"],
		secrets.Author{Name: "test"}, secrets.SourceAPI, time.Now()); err != nil {
		t.Fatal(err)
	}
	code, body := removeServer(t, s, "linear", "",
		`{"clear": true, "delete_secrets": ["LINEAR_ORG"], "_summary": "remove linear"}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("the removal = %d %v, want 201", code, body)
	}
	if got := jsonOf(t, body["secrets"]); got != `[{"name":"LINEAR_ORG","outcome":"failed"}]` {
		t.Errorf("secrets = %s, want LINEAR_ORG failed", got)
	}
	if len(activeCompany(t, s).MCPServers) != 1 || !s.stored(t, "LINEAR_ORG") {
		t.Error("want the server removed and the value still stored")
	}
}

// failingUnset is a credential store whose deletes fail.
type failingUnset struct{ configapi.Secrets }

func (failingUnset) Unset(context.Context, string) (bool, error) {
	return false, errors.New("the store is between leaders")
}

// A SERVER'S REMOVAL IS A WRITE LIKE ANY OTHER: held to what its caller read,
// summarised, strict about its body, and a 404 that says there is nothing to
// remove rather than how to add one.
func TestAServerRemovalIsAWriteLikeAnyOther(t *testing.T) {
	t.Parallel()
	s := serverSurface(t)
	clear := `{"clear": true}`

	code, body := removeServer(t, s, "nope", "", clear, nil)
	if code != http.StatusNotFound || !strings.Contains(body["hint"].(string), "nothing to remove") ||
		strings.Contains(body["hint"].(string), "If-None-Match") {
		t.Errorf("an absent server = %d %v, want 404 saying there is nothing to remove", code, body)
	}
	if code, body := removeServer(t, s, "linear", "", clear,
		map[string]string{"If-Match": `"mcp-servers-0000"`}); code != http.StatusPreconditionFailed {
		t.Errorf("a stale If-Match = %d %v, want 412", code, body)
	}
	if code, body := removeServer(t, s, "linear", "", clear, nil); code != http.StatusBadRequest ||
		body["error"] != "summary_required" {
		t.Errorf("no summary = %d %v, want 400 summary_required", code, body)
	}
	for _, bad := range []string{`{"clear": true, "repoint": []}`,
		`{"delete_secrets": ["A", "A"]}`, `{"delete_secrets": [" "]}`, `{"clear": "yes"}`} {
		if code, body := removeServer(t, s, "linear", "?dry_run=true", bad, nil); code != http.StatusBadRequest ||
			body["error"] != "invalid_body" {
			t.Errorf("%s = %d %v, want 400 invalid_body", bad, code, body)
		}
	}
	tag := s.do(t, http.MethodGet, "/config/mcp-servers/linear", "", nil).Header().Get("ETag")
	if code, body := removeServer(t, s, "linear", "", clear,
		map[string]string{"If-Match": tag, "X-Summary": "remove linear"}); code != http.StatusCreated {
		t.Errorf("with the entry's own tag = %d %v", code, body)
	}
}

// A SERVER'S REMOVAL IS MOUNTED AS A CONFIG WRITE: `config.write`, checked by
// `?dry_run=true` and summarised, as a model's is. Deleting a value asks
// `secrets.write` in the handler, on top.
func TestAServerRemovalIsMountedAsAConfigWrite(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	mux := &policyRecorder{ServeMux: http.NewServeMux(), policies: map[string]authz.Policy{}}
	if err := s.service().Routes(mux); err != nil {
		t.Fatal(err)
	}
	policy, ok := mux.policies["DELETE /config/"+configapi.EntityMCPServers+"/{id}"]
	if !ok {
		t.Fatal("the removal route is not mounted")
	}
	if policy.Action != authz.ActionConfigWrite || !policy.DryRun || !policy.Summary {
		t.Errorf("the removal is mounted with %+v, want config.write, checked and summarised", policy)
	}
}

// EVERY ENUM OF THE ANSWER KNOWS ITS OWN VALUES, and only those.
func TestTheRemovalAnswersEnumsAreClosed(t *testing.T) {
	t.Parallel()
	for _, k := range []configapi.MCPHolderKind{configapi.MCPHolderSeat, configapi.MCPHolderUnit} {
		if !k.Valid() {
			t.Errorf("%q is not valid", k)
		}
	}
	for _, o := range []configapi.MCPSecretOutcome{configapi.MCPSecretDeleted,
		configapi.MCPSecretAbsent, configapi.MCPSecretKept, configapi.MCPSecretFailed} {
		if !o.Valid() {
			t.Errorf("%q is not valid", o)
		}
	}
	if configapi.MCPHolderKind("team").Valid() || configapi.MCPSecretOutcome("gone").Valid() {
		t.Error("a value off the wire nobody declared is valid")
	}
}
