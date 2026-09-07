package setupapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"errors"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
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
	// setup is the service itself, kept so a test can attach the app flow
	// the engine attaches after construction.
	setup *setupapi.Service
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
	s.setup = setupapi.New(setupapi.Options{
		Company: s.company, Config: cfg, Secrets: v,
		// The resolution chain: what the vault holds is what resolved.
		Resolve: v.get,
		Now:     func() time.Time { return pinned },
	})
	s.setup.Routes(s.mux)
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
	for _, field := range []string{"enabled", "webhook_token", "route_to"} {
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
	// EVERY FIELD ON A CONNECT FORM IS REQUIRED, which is the control
	// plane's own contract: its Datadog connect takes site, api_key and
	// app_key and rejects a request missing any of them. The optional ones
	// were optional to the INTEGRATION and read as optional to the person
	// filling the form in, who then connected an app that could do half of
	// what they asked for.
	for _, field := range []string{"site", "api_key", "app_key"} {
		if reqs[field]["required"] != true {
			t.Errorf("%s is reported as optional", field)
		}
	}
	// NO CREDENTIAL CARRIES A VALUE, ever. Requirement.Stored can hold a
	// literal key on a company that wrote one instead of a ${VAR}, so the
	// value field a form opens on must never appear on a secret.
	for field, r := range reqs {
		if r["kind"] != "secret" {
			continue
		}
		if _, leaked := r["value"]; leaked {
			t.Errorf("%s is a credential and carries a value field", field)
		}
	}
	// And a plain setting does, because a form has to open on what this
	// company already answered rather than blank over it.
	if reqs["enabled"]["value"] != "false" {
		t.Errorf("enabled value = %v, want the toggle's own state", reqs["enabled"]["value"])
	}
}

// The base every inbound third-party app is built on is answered ONCE, not repeated in
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
		"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"},
		"generate": ["webhook_token"]
	}`, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", res.Code, res.Body)
	}
	body := decode(t, res)

	// The secret is sealed, under the name the third-party app declared, and the
	// answer names it without carrying it.
	names, _ := body["wrote_secrets"].([]any)
	// Every credential the submission carried, named and not carried. The
	// provisioning keys are part of connecting Datadog now, so a complete
	// submission seals three.
	if len(names) != 3 || !slices.Contains(names, any("DATADOG_WEBHOOK_TOKEN")) {
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
// third-party app's own API token would seal a value it has never heard of
// and report the integration connected.
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
		"values": {"site": "datadoghq.com", "enabled": "true"},
		"generate": ["webhook_token"]
	}`, nil)
	// The block comes into existence with a token and a region and no
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
		"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"},
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
	// third-party app's own error string would have carried straight through.
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
		`{"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"}, "generate": ["webhook_token"]}`, nil)
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

// A FORCED DISCONNECT REMOVES THE BLOCK AND NAMES WHAT IS ORPHANED, without
// deleting it. A credential an operator may be sharing with another
// deployment is not something a disconnect button decides about on its own.
//
// Forced, because that is the path that still writes synchronously: the
// ordinary one records the intent and lets the loop remove what the third-party app
// holds before the block goes.
func TestDisconnectRemovesTheBlockAndNamesTheOrphans(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	connect := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"}, "generate": ["webhook_token"]}`, nil)
	if connect.Code != http.StatusCreated {
		t.Fatalf("connect = %d: %s", connect.Code, connect.Body)
	}

	res := s.do(t, http.MethodDelete, "/setup/integrations/datadog", `{"force": true}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["removed"] != true {
		t.Fatalf("removed = %v", body["removed"])
	}
	orphans, _ := body["orphaned_secrets"].([]any)
	if !slices.Contains(orphans, any("DATADOG_WEBHOOK_TOKEN")) {
		t.Errorf("orphaned_secrets = %v, want the sealed token named", orphans)
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

// --- the provisioning pass --------------------------------------------------- //

// recordingPass stands in for a third-party app's Reconcile. It records what it was
// given, which is the whole question these tests ask: does the route hand a
// pass the two things that let it write, and only when it should.
type recordingPass struct {
	mu       sync.Mutex
	calls    []setup.PassInput
	findings []integration.Finding
	err      error
	release  chan struct{}
	needs    *setup.Requirement
	// watchCtx, when set, receives the context's error at the moment the
	// pass stops waiting: nil if it was released normally.
	watchCtx chan error
}

func (*recordingPass) Kind() integration.Kind      { return integration.KindGitHub }
func (p *recordingPass) Needs() *setup.Requirement { return p.needs }

func (p *recordingPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	p.mu.Lock()
	p.calls = append(p.calls, in)
	release, watch := p.release, p.watchCtx
	p.mu.Unlock()
	if watch != nil {
		// Report what the context did rather than what it was: a pass
		// that writes at a third-party app cares only whether it was cut off.
		select {
		case <-ctx.Done():
			watch <- ctx.Err()
		case <-release:
			watch <- nil
		}
		return p.findings, p.err
	}
	if release != nil {
		<-release
	}
	return p.findings, p.err
}

func (p *recordingPass) last() (setup.PassInput, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		return setup.PassInput{}, 0
	}
	return p.calls[len(p.calls)-1], len(p.calls)
}

// statusStore is the fleet row a pass writes to.
type statusStore struct {
	mu     sync.Mutex
	states map[integration.Kind]integration.State
	forgot []integration.Kind
}

func (s *statusStore) SaveIntegration(_ context.Context, state integration.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = map[integration.Kind]integration.State{}
	}
	s.states[state.Kind] = state
	return nil
}

func (s *statusStore) ForgetIntegration(_ context.Context, kind integration.Kind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgot = append(s.forgot, kind)
	delete(s.states, kind)
	return nil
}

func (s *statusStore) LoadIntegrations(context.Context) ([]integration.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]integration.State, 0, len(s.states))
	for _, state := range s.states {
		out = append(out, state)
	}
	return out, nil
}

func (s *statusStore) get(kind integration.Kind) (integration.State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[kind]
	return state, ok
}

// withPass rebuilds the surface with a provisioning pass wired in.
func (s *surface) withPass(t *testing.T, pass *recordingPass) (*statusStore, *setup.Runner) {
	t.Helper()
	status := &statusStore{}
	runner := setup.NewRunner([]setup.Pass{pass}, nil, func() time.Time { return pinned })
	s.mux = http.NewServeMux()
	setupapi.New(setupapi.Options{
		Company: s.company, Config: s.config, Secrets: s.vault,
		Resolve: s.vault.get,
		Passes:  runner,
		Sink: func(operator string) (provision.TokenSink, error) {
			return provision.NewSecretStoreSink(sinkStore{s.vault}, operator), nil
		},
		Status: status,
		Now:    func() time.Time { return pinned },
	}).Routes(s.mux)
	s.config.Routes(s.mux)
	return status, runner
}

// sinkStore adapts the vault to what a secret-store sink needs.
type sinkStore struct{ v *vault }

func (s sinkStore) Set(ctx context.Context, name, value, by, source string, at time.Time) error {
	return s.v.Set(ctx, name, value, by, source, at)
}

func (s sinkStore) Get(_ context.Context, name string) (string, error) {
	value, ok := s.v.get(name)
	if !ok {
		return "", secrets.ErrNotFound
	}
	return value, nil
}

func (s sinkStore) Unset(_ context.Context, name string) (bool, error) {
	s.v.mu.Lock()
	defer s.v.mu.Unlock()
	_, ok := s.v.values[name]
	delete(s.v.values, name)
	return ok, nil
}

// seedGitHub puts a complete GitHub block in place, with a public base.
func (s *surface) seedGitHub(t *testing.T) {
	t.Helper()
	res := s.do(t, http.MethodPatch, "/config", `{"integrations":{
		"public_base_url":"https://engine.example.com",
		"github":{"enabled":true,"webhook_secret":"${GH_SECRET}",
		"provisioning":{"org":"acme"}}}}`,
		map[string]string{
			"Content-Type": "application/merge-patch+json",
			"X-Summary":    "github by hand",
		})
	if res.Code != http.StatusCreated {
		t.Fatalf("seed github = %d: %s", res.Code, res.Body)
	}
	// The secret has to RESOLVE, or the requirement is outstanding and the
	// pass is refused before it runs, which is a different test.
	if err := s.vault.Set(t.Context(), "GH_SECRET", "s", "test", "test", pinned); err != nil {
		t.Fatal(err)
	}
}

// THE PASS IS HANDED THE TWO THINGS THE LOOP WITHHOLDS. A base is permission
// to register a hook and a sink is permission to mint a credential, and a
// person pressing the button is what supplies both.
func TestAProvisionPassGetsASinkAndABase(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{}
	status, _ := s.withPass(t, pass)
	s.seedGitHub(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	in, calls := pass.last()
	if calls != 1 {
		t.Fatalf("the pass ran %d times", calls)
	}
	if in.Sink == nil {
		t.Error("the pass got no sink, so it can mint nothing")
	}
	if in.WebhookBase != "https://engine.example.com" {
		t.Errorf("webhook base = %q", in.WebhookBase)
	}
	// And the outcome landed on the fleet row the loop reads.
	if _, ok := status.get(integration.KindGitHub); !ok {
		t.Error("the pass wrote no status, so the screen would not update")
	}
}

// A CHECK IS THE SAME PASS WITH NEITHER. It answers "is it working now"
// without the engine writing anything at the third-party app.
func TestACheckRunsTheSamePassReadOnly(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{}
	s.withPass(t, pass)
	s.seedGitHub(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/github/check", `{}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	in, calls := pass.last()
	if calls != 1 {
		t.Fatalf("the pass ran %d times", calls)
	}
	if in.Sink != nil || in.WebhookBase != "" {
		t.Fatalf("a check was given permission to write: sink=%v base=%q",
			in.Sink != nil, in.WebhookBase)
	}
}

// A PASS IS REFUSED AGAINST A HALF-CONFIGURED INTEGRATION, naming what is
// missing. It writes at the third-party app, so running it on a guess is worse than
// not running it.
func TestAPassIsRefusedWhileSomethingIsOutstanding(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{}
	s.withPass(t, pass)
	// enabled, and with a secret that resolves to nothing.
	res := s.do(t, http.MethodPatch, "/config",
		`{"integrations":{"public_base_url":"https://engine.example.com",
		  "github":{"enabled":true,"webhook_secret":"${GH_MISSING}"}}}`,
		map[string]string{
			"Content-Type": "application/merge-patch+json",
			"X-Summary":    "half",
		})
	if res.Code != http.StatusCreated {
		t.Fatalf("seed = %d: %s", res.Code, res.Body)
	}

	got := s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil)
	if got.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", got.Code, got.Body)
	}
	body := decode(t, got)
	if body["error"] != "requirements_outstanding" {
		t.Fatalf("error = %v", body["error"])
	}
	fields, _ := body["fields"].([]any)
	if len(fields) == 0 {
		t.Error("the refusal names nothing to fix")
	}
	if _, calls := pass.last(); calls != 0 {
		t.Error("a refused pass ran anyway")
	}
}

// WITHOUT A PUBLIC BASE A PASS REGISTERS NOTHING, so it is refused by name
// rather than run to report success having done nothing.
func TestAProvisionPassNeedsAPublicBase(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{}
	s.withPass(t, pass)
	res := s.do(t, http.MethodPatch, "/config",
		`{"integrations":{"github":{"enabled":true,"webhook_secret":"${GH_SECRET}","provisioning":{"org":"acme"}}}}`,
		map[string]string{
			"Content-Type": "application/merge-patch+json", "X-Summary": "no base",
		})
	if res.Code != http.StatusCreated {
		t.Fatalf("seed = %d: %s", res.Code, res.Body)
	}
	if err := s.vault.Set(t.Context(), "GH_SECRET", "s", "test", "test", pinned); err != nil {
		t.Fatal(err)
	}

	got := s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil)
	if got.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", got.Code, got.Body)
	}
	if decode(t, got)["error"] != "no_public_base_url" {
		t.Errorf("error = %v", decode(t, got)["error"])
	}
	if _, calls := pass.last(); calls != 0 {
		t.Error("a pass ran with no base")
	}
}

// A SECOND PASS IS REFUSED WHILE ONE RUNS. Minting twice is not something a
// retry should paper over, so the second caller is told rather than queued.
func TestASecondPassIsRefusedWhileOneRuns(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{release: make(chan struct{})}
	s.withPass(t, pass)
	s.seedGitHub(t)

	done := make(chan int, 1)
	go func() {
		done <- s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil).Code
	}()
	// Wait for the first to be inside the pass.
	for range 200 {
		if _, calls := pass.last(); calls == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	second := s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil)
	if second.Code != http.StatusConflict {
		t.Fatalf("second pass = %d, want 409: %s", second.Code, second.Body)
	}
	if decode(t, second)["error"] != "pass_in_flight" {
		t.Errorf("error = %v", decode(t, second)["error"])
	}
	close(pass.release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("the first pass = %d", code)
	}
}

// A FAILED PASS IS A FACT ABOUT THE INTEGRATION, recorded as the loop records
// one: activating, actor engine, findings dropped. Hiding it would leave the
// screen showing the last good answer under a fresh timestamp.
func TestAFailedPassIsRecordedAsAFault(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{err: errors.New("github refused the credential")}
	status, _ := s.withPass(t, pass)
	s.seedGitHub(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", res.Code, res.Body)
	}
	state, ok := status.get(integration.KindGitHub)
	if !ok {
		t.Fatal("a failed pass recorded nothing")
	}
	if state.Report.Phase != integration.PhaseActivating || state.Report.Actor != integration.ActorEngine {
		t.Errorf("phase/actor = %v/%v, want the fault shape the loop writes",
			state.Report.Phase, state.Report.Actor)
	}
	if state.LastError == "" {
		t.Error("the fault carries no error")
	}
	if len(state.Findings) != 0 {
		t.Error("a failed pass kept findings it never observed")
	}
}

// A third-party app with no pass says so rather than answering 404 or running nothing.
func TestAVendorWithNoPassSaysSo(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	s.withPass(t, &recordingPass{})

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/provision", `{}`, nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", res.Code, res.Body)
	}
	if decode(t, res)["error"] != "not_provisionable" {
		t.Errorf("error = %v", decode(t, res)["error"])
	}
}

// A PASS THAT NEEDS AN ADMINISTRATOR CREDENTIAL SAYS SO, so the screen can
// collect it rather than starting a run that will report nothing useful.
func TestAPassDeclaresTheCredentialItNeeds(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	s.withPass(t, &recordingPass{needs: &setup.Requirement{
		Field: "operator_credential", Label: "Group Owner token",
		Kind: setup.KindSecret, Required: true,
	}})
	s.seedGitHub(t)

	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/github", "", nil))
	needs, _ := state["needs_operator"].(map[string]any)
	if needs == nil {
		t.Fatal("the state does not declare the credential its pass needs")
	}
	if needs["label"] != "Group Owner token" {
		t.Errorf("label = %v", needs["label"])
	}
	if state["can_provision"] != true {
		t.Error("can_provision is false on a third-party app with a pass")
	}
}

// THE TRANSIENT CREDENTIAL REACHES THE PASS AND NOTHING ELSE. It is not
// sealed, not written into the config, and not echoed back.
func TestTheOperatorCredentialReachesThePassAndIsNotKept(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{}
	s.withPass(t, pass)
	s.seedGitHub(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/github/provision",
		`{"operator_credential":"glpat-owner-token"}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	in, _ := pass.last()
	if in.Operator != "glpat-owner-token" {
		t.Fatalf("the pass got operator %q", in.Operator)
	}
	if strings.Contains(res.Body.String(), "glpat-owner-token") {
		t.Fatal("the answer echoes the administrator credential")
	}
	// And nothing sealed it: it is a grant with an end, not a stored one.
	s.vault.mu.Lock()
	defer s.vault.mu.Unlock()
	for name, value := range s.vault.values {
		if value == "glpat-owner-token" {
			t.Fatalf("the administrator credential was sealed as %s", name)
		}
	}
}

// --- per-seat setup ---------------------------------------------------------- //

// SLACK IS PER SEAT, and the list names every AGENT rather than only the ones
// already configured: the list is what a screen renders a form from, so
// leaving out the seats that have no app yet would leave an operator no way to
// give one to them.
func TestSlackAnswersOneListPerAgentSeat(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/slack", "", nil))
	seats, _ := state["seats"].([]any)
	if len(seats) != 2 {
		t.Fatalf("seats = %v, want one per agent", seats)
	}
	first, _ := seats[0].(map[string]any)
	if first["handle"] != "cto" && first["handle"] != "sre-lead" {
		t.Errorf("handle = %v", first["handle"])
	}
	// Each seat's route is its own: a delivery is addressed to a seat here,
	// which is the whole reason the credentials are per seat too.
	if first["inbound_path"] != "/webhooks/slack/"+first["handle"].(string) {
		t.Errorf("inbound_path = %v", first["inbound_path"])
	}
	reqs, _ := first["requirements"].([]any)
	if len(reqs) != 3 {
		t.Fatalf("a seat declares %d requirements", len(reqs))
	}
	// NOTHING CARRIES A VALUE, per seat as everywhere else.
	for _, row := range reqs {
		if _, leaked := row.(map[string]any)["value"]; leaked {
			t.Error("a per-seat requirement carries a value")
		}
	}
}

// A PER-SEAT SUBMISSION WRITES THROUGH THE SEAT, not through a merge patch: a
// patch replaces an array wholesale, so patching the roster to change one seat
// would delete every other one.
func TestAPerSeatSubmissionLeavesTheOtherSeatsAlone(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/slack/inputs", `{
		"seat": "sre-lead",
		"values": {"bot_token": "xoxb-one", "signing_secret": "sig-one"}
	}`, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	// The credentials are sealed under names that carry the handle, so two
	// seats never share one.
	for _, name := range []string{"SLACK_BOT_TOKEN_SRE_LEAD", "SLACK_SIGNING_SECRET_SRE_LEAD"} {
		if _, ok := s.vault.get(name); !ok {
			t.Errorf("%s was not sealed", name)
		}
	}
	// AND THE OTHER SEAT SURVIVES, which is what a merge patch would have
	// destroyed.
	doc := s.do(t, http.MethodGet, "/config", "", nil)
	if !strings.Contains(doc.Body.String(), `"cto"`) {
		t.Fatalf("the other seat is gone from the document: %s", doc.Body)
	}
	if !strings.Contains(doc.Body.String(), "${SLACK_BOT_TOKEN_SRE_LEAD}") {
		t.Fatalf("the seat does not point at its sealed token: %s", doc.Body)
	}
	if strings.Contains(doc.Body.String(), "xoxb-one") {
		t.Fatal("the document holds the credential itself")
	}
}

// A submission naming a seat that takes no per-seat setup is refused by name
// rather than written somewhere unexpected.
func TestAnUnknownSeatIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/slack/inputs",
		`{"seat": "nobody", "values": {"bot_token": "x"}}`, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", res.Code, res.Body)
	}
}

// A PASS OUTLIVES THE REQUEST THAT ASKED FOR IT.
//
// The pass WRITES AT THE VENDOR: it creates accounts, mints tokens and
// registers webhooks. Run on the request's own context, a browser tab closing
// or a reverse proxy hitting its read timeout cancels it mid-way, and what is
// left behind is an account created with no credential sealed, or a
// credential sealed with no pointer written. The next pass then duplicates
// the account or reports a half-finished integration nobody asked for.
//
// So the handler detaches. This asserts the pass does NOT see the request's
// cancellation, which is the only observable difference.
func TestAPassDoesNotSeeTheRequestBeingCancelled(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{release: make(chan struct{}), watchCtx: make(chan error, 1)}
	s.withPass(t, pass)
	s.seedGitHub(t)

	req := httptest.NewRequest(http.MethodPost,
		"/setup/integrations/github/provision", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	served := make(chan struct{})
	go func() {
		defer close(served)
		s.mux.ServeHTTP(httptest.NewRecorder(), req)
	}()

	// Cancel the request while the pass is mid-flight, then let it finish.
	cancel()
	close(pass.release)

	select {
	case err := <-pass.watchCtx:
		if err != nil {
			t.Fatalf("the pass saw %v from the request's context: a closed tab "+
				"can abandon a run that is creating accounts at the third-party app", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pass never finished")
	}
	<-served
}

// AN ORDINARY DISCONNECT ASKS, IT DOES NOT REMOVE.
//
// The block carries the credential the third-party app teardown authenticates with, so
// dropping it here would strand every webhook and account the integration
// still holds with nothing left to authenticate a second attempt. The intent
// goes on the fleet row and the loop removes both, in that order.
func TestDisconnectRecordsTheIntentRatherThanRemovingTheBlock(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	status, _ := s.withPass(t, &recordingPass{})
	s.seedGitHub(t)

	res := s.do(t, http.MethodDelete, "/setup/integrations/github",
		`{"remove_seats": true}`, nil)
	if res.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["removed"] != false || body["disconnecting"] != true {
		t.Fatalf("body = %v: an asked-for disconnect must not report the block removed", body)
	}

	row, ok := status.get(integration.KindGitHub)
	if !ok {
		t.Fatal("no fleet row was written, so the loop would never act on the disconnect")
	}
	if !row.Disconnecting {
		t.Error("the row does not carry the intent")
	}
	if !row.RemoveSeats {
		t.Error("the checkbox answer did not reach the row the teardown reads it from")
	}
	// DUE NOW, or the disconnect waits out a backoff nobody asked it to
	// serve before anything at the third-party app is touched.
	if !row.Due(pinned) {
		t.Errorf("next attempt is %v, which is not due at %v", row.NextAttemptAt, pinned)
	}

	// And the integration is still configured: the block goes when the
	// teardown finishes, not before.
	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/github", "", nil))
	if state["configured"] != true {
		t.Error("the block was removed before the third-party app teardown ran")
	}
}

// THE CHECKBOX DEFAULTS TO OFF. An account at a third-party app is a colleague with
// history attached, and a disconnect that removed one because the field was
// absent would be inferring the most destructive answer.
func TestDisconnectDoesNotRemoveSeatsUnlessAsked(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	status, _ := s.withPass(t, &recordingPass{})
	s.seedGitHub(t)

	if res := s.do(t, http.MethodDelete, "/setup/integrations/github", "", nil); res.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	row, ok := status.get(integration.KindGitHub)
	if !ok {
		t.Fatal("no fleet row was written")
	}
	if row.RemoveSeats {
		t.Error("a disconnect with no body asked for the accounts to be deleted")
	}
}

// A DISCONNECT NAMES WHAT IS ORPHANED, NOT WHAT COULD HAVE BEEN.
//
// This walked every secret REQUIREMENT rather than every secret STORED, so
// an integration with optional credentials named ones nobody had ever set.
// The list exists for an operator to act on — `crewlet secrets unset` — and
// half of it pointed at nothing.
func TestDisconnectNamesOnlyTheSecretsThatExist(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	// Connect with the webhook token only, leaving the provisioning keys
	// unset. The form asks for them, and a submission that omits one is
	// still a submission: what this pins is that the disconnect names the
	// secrets that EXIST rather than every name the app declares.
	connect := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"values": {"route_to": "sre-lead", "enabled": "true"}, "generate": ["webhook_token"]}`, nil)
	if connect.Code != http.StatusCreated {
		t.Fatalf("connect = %d: %s", connect.Code, connect.Body)
	}

	res := s.do(t, http.MethodDelete, "/setup/integrations/datadog", `{"force": true}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	orphans, _ := decode(t, res)["orphaned_secrets"].([]any)
	if len(orphans) != 1 || orphans[0] != "DATADOG_WEBHOOK_TOKEN" {
		t.Fatalf("orphaned = %v, want only the secret that was actually stored", orphans)
	}
}

// A MISSING KEYRING SAYS SO, rather than internal_error.
//
// There are two ways to have no keyring and only one was caught: a node with
// no secret store WIRED, and a node with one whose bootstrap names no key.
// The second fails at the seal with secrets.ErrNoKeyring — a sentinel that
// exists to be recognised — and fell through to the generic case, so a screen
// that could have said "set secrets.keys" said internal_error and left an
// operator reading engine logs to find a one-line fix.
func TestASealWithNoKeyringSaysWhatToSet(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	s.vault.fail = fmt.Errorf("setup: seal DATADOG_APP_KEY: %w", secrets.ErrNoKeyring)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"}, "generate": ["webhook_token"]}`, nil)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["error"] != "no_keyring" {
		t.Fatalf("error = %v, want no_keyring", body["error"])
	}
	if hint, _ := body["hint"].(string); !strings.Contains(hint, "secrets.keys") {
		t.Errorf("hint = %q, want it to name what to set", hint)
	}
}
