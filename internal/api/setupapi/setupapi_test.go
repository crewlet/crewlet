package setupapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/store"
)

var pinned = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

// A company with no Datadog block at all: the state an operator pressing
// Connect for the first time is in.
const companyDoc = `{
  "name": "Acme",
  "providers": {"llm": {"zulu": {"type": "anthropic", "model": "claude-sonnet-5", "api_keys": ["${K}"]}}},
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu"},
    {"name": "CTO", "handle": "cto", "llm": "zulu"}
  ]
}`

// vault is the sealed store, standing in for the fleet's. It records what was
// written and, deliberately, offers no way to read a value back: the surface
// under test must never have one.
type vault struct {
	mu     sync.Mutex
	values map[string]string
	order  []string
	fail   error
}

func (v *vault) Set(_ context.Context, name, value, by, source string, _ time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.fail != nil {
		return v.fail
	}
	if v.values == nil {
		v.values = map[string]string{}
	}
	v.values[name] = value
	v.order = append(v.order, name+"|"+by+"|"+source)
	return nil
}

func (v *vault) get(name string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.values[name]
	return value, ok
}

type surface struct {
	mux     *http.ServeMux
	config  *configapi.Service
	vault   *vault
	company func() *config.Company
	configs *store.Configs
}

func newSurface(t *testing.T) *surface {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "c.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := configapi.New(configapi.Options{
		Store: db, Plane: coordmemory.NewFleet(),
		Now: func() time.Time { return pinned },
	})
	v := &vault{}
	s := &surface{mux: http.NewServeMux(), config: cfg, vault: v, configs: db.Configs()}
	// THE ACTIVE DOCUMENT, read fresh on every call, the same way the
	// engine hands it over: a screen bound to the company this process
	// booted on would describe one that is no longer running.
	s.company = func() *config.Company {
		doc, err := cfg.Document(t.Context())
		if err != nil {
			return nil
		}
		return doc
	}
	setupapi.New(setupapi.Options{
		Company: s.company, Config: cfg, Secrets: v,
		// The resolution chain: what the vault holds is what resolved.
		Resolve: v.get,
		Now:     func() time.Time { return pinned },
	}).Routes(s.mux)
	cfg.Routes(s.mux)
	return s
}

func (s *surface) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// seed imports the company so there is something to patch.
func (s *surface) seed(t *testing.T) {
	t.Helper()
	res := s.do(t, http.MethodPut, "/config", companyDoc,
		map[string]string{"X-Summary": "first import"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
}

func decode(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", res.Body, err)
	}
	return body
}

// requirements pulls one tool's requirement list out of a state answer.
func requirements(t *testing.T, state map[string]any) map[string]map[string]any {
	t.Helper()
	rows, _ := state["requirements"].([]any)
	out := map[string]map[string]any{}
	for _, row := range rows {
		r, _ := row.(map[string]any)
		out[r["field"].(string)] = r
	}
	return out
}

// --- reading --------------------------------------------------------------- //

// AN UNCONFIGURED VENDOR STILL ANSWERS ITS WHOLE LIST. Answering an empty one
// would leave the screen a Connect button that opens a dialog collecting
// nothing.
func TestAnUnconfiguredVendorSaysWhatItNeeds(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodGet, "/setup/integrations/datadog", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	state := decode(t, res)
	if state["configured"] != false || state["satisfied"] != false {
		t.Fatalf("an absent block reports configured=%v satisfied=%v",
			state["configured"], state["satisfied"])
	}
	reqs := requirements(t, state)
	for _, field := range []string{"enabled", "webhook_token", "route_to", "handle_tag"} {
		if _, ok := reqs[field]; !ok {
			t.Errorf("the list omits %q", field)
		}
	}
	// THE TOKEN IS MINTABLE, so a person is never asked to invent a
	// password, and it says which finding it clears so a reconcile row can
	// offer exactly this field.
	if reqs["webhook_token"]["mintable"] != true {
		t.Error("the shared token is not mintable")
	}
	if reqs["webhook_token"]["blocks"] != "credential_missing" {
		t.Errorf("blocks = %v", reqs["webhook_token"]["blocks"])
	}
	// The optional one is optional, and still listed.
	if reqs["handle_tag"]["required"] != false {
		t.Error("the owner tag key is reported as required")
	}
	// NOTHING CARRIES A VALUE. Not present, not resolved, not a default.
	for field, r := range reqs {
		if _, leaked := r["value"]; leaked {
			t.Errorf("%s carries a value field", field)
		}
	}
}

// The base every inbound vendor is built on is answered ONCE, not repeated in
// each tool: it is one setting, and asking for it seven times would ask the
// operator to keep seven copies consistent.
func TestThePublicBaseIsAnsweredOnce(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	body := decode(t, s.do(t, http.MethodGet, "/setup/integrations", "", nil))
	base, _ := body["public_base_url"].(map[string]any)
	if base == nil {
		t.Fatal("the listing carries no public base")
	}
	if base["present"] != false {
		t.Errorf("present = %v on a company that names none", base["present"])
	}
	if base["config_path"] != "integrations.public_base_url" {
		t.Errorf("config_path = %v", base["config_path"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("the listing serves no tools")
	}
	for _, tool := range tools {
		if _, repeated := tool.(map[string]any)["public_base_url"]; repeated {
			t.Error("a tool repeats the public base")
		}
	}
}

func TestAnUnknownKindIsRefusedByName(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodGet, "/setup/integrations/pagerduty", "", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", res.Code, res.Body)
	}
	if got := decode(t, res)["error"]; got != "unknown_kind" {
		t.Errorf("error = %v", got)
	}
}

// --- writing --------------------------------------------------------------- //

// THE WHOLE SPINE, end to end: an empty config, a submission, and an
// integration that reports itself satisfied. What it proves is that the
// secret was sealed, the config got a POINTER rather than the value, the
// revision activated, and the requirement's resolution flipped.
func TestASubmissionSealsTheSecretAndPointsTheConfigAtIt(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", `{
		"values": {"route_to": "sre-lead", "enabled": "true"},
		"generate": ["webhook_token"]
	}`, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", res.Code, res.Body)
	}
	body := decode(t, res)

	// The secret is sealed, under the name the vendor declared, and the
	// answer names it without carrying it.
	names, _ := body["wrote_secrets"].([]any)
	if len(names) != 1 || names[0] != "DATADOG_WEBHOOK_TOKEN" {
		t.Fatalf("wrote_secrets = %v", names)
	}
	minted, ok := s.vault.get("DATADOG_WEBHOOK_TOKEN")
	if !ok || minted == "" {
		t.Fatal("nothing was sealed")
	}
	if strings.Contains(res.Body.String(), minted) {
		t.Fatal("the answer carries the minted credential")
	}

	// THE CONFIG HOLDS A POINTER, never the value. This is what makes a
	// GET /config safe to render and a rotation a change to the store.
	doc := s.do(t, http.MethodGet, "/config", "", nil)
	if strings.Contains(doc.Body.String(), minted) {
		t.Fatal("the company document holds the credential itself")
	}
	if !strings.Contains(doc.Body.String(), "${DATADOG_WEBHOOK_TOKEN}") {
		t.Fatalf("the document does not point at the secret: %s", doc.Body)
	}

	// And the state flipped: written down AND resolved.
	state, _ := body["state"].(map[string]any)
	if state["satisfied"] != true {
		t.Fatalf("satisfied = %v after a complete submission", state["satisfied"])
	}
	token := requirements(t, state)["webhook_token"]
	if token["present"] != true || token["resolved"] != true {
		t.Errorf("present/resolved = %v/%v", token["present"], token["resolved"])
	}
}

// A MINT AND A VALUE ARE TWO DIFFERENT REQUESTS. A client that could send
// both under one key would eventually send a weak token by accident.
func TestAFieldCannotBeBothSuppliedAndGenerated(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", `{
		"values": {"webhook_token": "mine", "route_to": "sre-lead", "enabled": "true"},
		"generate": ["webhook_token"]
	}`, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.Code, res.Body)
	}
	if _, sealed := s.vault.get("DATADOG_WEBHOOK_TOKEN"); sealed {
		t.Error("a refused submission sealed a value")
	}
}

// Only a MINTABLE field can be generated. Asking the engine to invent a
// vendor's own API token would seal a value the vendor has never heard of and
// report the integration connected.
func TestOnlyAMintableFieldCanBeGenerated(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"generate": ["route_to"]}`, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), "route_to") {
		t.Errorf("the refusal does not name the field: %s", res.Body)
	}
}

// A SUBMISSION THAT CLEARS A REQUIRED FIELD IS REFUSED. An empty string is
// almost always a form that rendered a value it never had; writing it would
// disable a working integration and answer 201.
func TestClearingARequiredFieldIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"values": {"route_to": ""}}`, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.Code, res.Body)
	}
	if got := decode(t, res)["error"]; got != "invalid_input" {
		t.Errorf("error = %v", got)
	}
}

// THE WHOLE DOCUMENT IS VALIDATED, so a field that is fine on its own is
// still refused when it leaves the company invalid. Datadog's own rule is
// that an enabled block must name a fallback seat.
func TestASubmissionIsValidatedAsTheWholeCompany(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", `{
		"values": {"handle_tag": "owner", "enabled": "true"},
		"generate": ["webhook_token"]
	}`, nil)
	// The block comes into existence with a token and a tag and no
	// route_to, which the config validator refuses.
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.Code, res.Body)
	}
	if got := decode(t, res)["error"]; got != "validation_error" {
		t.Errorf("error = %v: %s", got, res.Body)
	}
}

// A STALE BASE IS REFUSED BEFORE ANYTHING IS WRITTEN, and the refusal names
// both sides so the caller can re-read and re-derive.
func TestAStaleRevisionIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", `{
		"if_match": "01JSOMETHINGELSE",
		"values": {"route_to": "sre-lead", "enabled": "true"},
		"generate": ["webhook_token"]
	}`, nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["error"] != "revision_advanced" {
		t.Fatalf("error = %v", body["error"])
	}
	if body["current_revision_id"] == nil || body["your_base"] != "01JSOMETHINGELSE" {
		t.Errorf("the refusal does not carry both sides: %v", body)
	}
	if _, sealed := s.vault.get("DATADOG_WEBHOOK_TOKEN"); sealed {
		t.Error("a refused submission sealed a value")
	}
}

// A LITERAL IN THE SLOT IS REFUSED BY PATH. Overwriting it would edit the
// company from a setup form and destroy a credential somebody put there.
func TestALiteralInTheConfigIsRefusedByPath(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	// Put a literal there the way an operator would have: through the
	// config surface itself.
	patch := s.do(t, http.MethodPatch, "/config",
		`{"integrations":{"datadog":{"enabled":true,"webhook_token":"plain-token","route_to":"sre-lead"}}}`,
		map[string]string{
			"Content-Type": "application/merge-patch+json",
			"X-Summary":    "by hand",
		})
	if patch.Code != http.StatusCreated {
		t.Fatalf("seed patch = %d: %s", patch.Code, patch.Body)
	}

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"generate": ["webhook_token"]}`, nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["error"] != "literal_in_config" {
		t.Fatalf("error = %v", body["error"])
	}
	if body["path"] != "integrations.datadog.webhook_token" {
		t.Errorf("path = %v", body["path"])
	}
	// AND THE REFUSAL DOES NOT ECHO THE VALUE, which is the leak a
	// vendor's own error string would have carried straight through.
	if strings.Contains(res.Body.String(), "plain-token") {
		t.Fatal("the refusal echoes the credential it refused to overwrite")
	}
}

// ROTATING A VALUE STILL ADVANCES THE EPOCH. The pointer is already correct,
// so there is no patch to make, and a value written into the store after the
// last apply is invisible to every running seat until something activates.
func TestRotatingASecretPublishesAnyway(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	first := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"values": {"route_to": "sre-lead", "enabled": "true"}, "generate": ["webhook_token"]}`, nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("connect = %d: %s", first.Code, first.Body)
	}
	before, _ := s.vault.get("DATADOG_WEBHOOK_TOKEN")

	second := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"generate": ["webhook_token"]}`, nil)
	if second.Code != http.StatusCreated {
		t.Fatalf("rotate = %d: %s", second.Code, second.Body)
	}
	body := decode(t, second)
	if body["reloaded"] != true {
		t.Fatalf("a rotation did not report the reload: %v", body)
	}
	if body["revision_id"] == decode(t, first)["revision_id"] {
		t.Fatal("the rotation did not advance the pointer, so no seat re-reads the store")
	}
	after, _ := s.vault.get("DATADOG_WEBHOOK_TOKEN")
	if after == before {
		t.Fatal("the rotation minted the same value")
	}
}

// --- disconnect ------------------------------------------------------------- //

// DISCONNECT REMOVES THE BLOCK AND NAMES WHAT IS ORPHANED, without deleting
// it. A credential an operator may be sharing with another deployment is not
// something a disconnect button decides about on its own.
func TestDisconnectRemovesTheBlockAndNamesTheOrphans(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	connect := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"values": {"route_to": "sre-lead", "enabled": "true"}, "generate": ["webhook_token"]}`, nil)
	if connect.Code != http.StatusCreated {
		t.Fatalf("connect = %d: %s", connect.Code, connect.Body)
	}

	res := s.do(t, http.MethodDelete, "/setup/integrations/datadog", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["removed"] != true {
		t.Fatalf("removed = %v", body["removed"])
	}
	orphans, _ := body["orphaned_secrets"].([]any)
	if len(orphans) != 1 || orphans[0] != "DATADOG_WEBHOOK_TOKEN" {
		t.Errorf("orphaned_secrets = %v", orphans)
	}
	// The value is still sealed. Naming it is the point; deleting it is
	// the operator's call.
	if _, ok := s.vault.get("DATADOG_WEBHOOK_TOKEN"); !ok {
		t.Error("disconnect deleted the sealed value")
	}
	// And the block is gone.
	after := decode(t, s.do(t, http.MethodGet, "/setup/integrations/datadog", "", nil))
	if after["configured"] != false {
		t.Errorf("configured = %v after a disconnect", after["configured"])
	}
}

// Disconnecting something that is not connected is not an error: the caller
// wanted a company without this integration and it has one.
func TestDisconnectingWhatIsAbsentIsNotAnError(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodDelete, "/setup/integrations/datadog", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	if decode(t, res)["removed"] != false {
		t.Error("an absent integration reported a removal")
	}
}
