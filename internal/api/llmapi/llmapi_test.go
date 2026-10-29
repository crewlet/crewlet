package llmapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/llmapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/secrets"
)

var clock = time.Date(2026, 10, 28, 22, 0, 0, 0, time.UTC)

// company is a document with an API-key model, a token model whose sign-in
// and login directory default shares with aux, and a codex entry.
const company = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    default:
      model: claude-opus-5-5
      state_dir: /var/lib/crewlet/claude
      auth: {token: "${CLAUDE_CODE_OAUTH_TOKEN}"}
    aux:
      model: haiku
      state_dir: /var/lib/crewlet/claude
      auth: {token: "${CLAUDE_CODE_OAUTH_TOKEN}"}
    sonnet:
      model: sonnet
      auth: {mode: api-key, api_key: "${ANTHROPIC_API_KEY_SONNET}"}
    codex-review:
      agent: codex
      model: gpt-6.1-sol
roles:
  - name: QA
    handle: qa
    llm: default
`

// fakeEngine plays the engine: what a check answers, and what it was asked.
type fakeEngine struct {
	mu      sync.Mutex
	check   llmapi.Check
	checks  []checked
	retry   llmapi.Retried
	retErr  error
	login   coord.HostLogin
	logErr  error
	device  coord.DeviceSignIn
	devErr  error
	running int
}

type checked struct {
	key    string
	draft  *config.LLMProvider
	values map[string]string
	smoke  bool
}

func (f *fakeEngine) CheckModel(_ context.Context, key string, draft *config.LLMProvider,
	values map[string]string, smoke bool) (llmapi.Check, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, checked{key, draft, values, smoke})
	return f.check, nil
}

func (f *fakeEngine) RetryModel(context.Context, string, coord.ModelRetryMode, iam.Actor) (llmapi.Retried, error) {
	return f.retry, f.retErr
}

func (f *fakeEngine) CheckHostLogin(context.Context, string) (coord.HostLogin, error) {
	return f.login, f.logErr
}

func (f *fakeEngine) StartDeviceSignIn(context.Context, string, iam.Actor) (coord.DeviceSignIn, error) {
	return f.device, f.devErr
}

func (f *fakeEngine) CancelDeviceSignIn(_ context.Context, id uuid.UUID, _ iam.Actor) (coord.DeviceSignIn, error) {
	if id != f.device.ID {
		return coord.DeviceSignIn{}, llmapi.ErrNoSuchSignIn
	}
	return f.device, nil
}

func (f *fakeEngine) RunningRunsOn(context.Context, []string) (int, error) { return f.running, nil }

// fakeConfig is the config surface over one document: a draft is spliced into
// a copy of it and the whole document validated, as the entity route's dry
// run does.
type fakeConfig struct {
	mu      sync.Mutex
	doc     *config.Company
	reloads []string
	expects []string
}

func (c *fakeConfig) ActiveCompany(context.Context) (*config.Company, string, error) {
	return c.doc, "r-1", nil
}

func (c *fakeConfig) CheckEntity(_ context.Context, _, id string, body []byte, _ bool,
	_ iam.Actor) (*config.Company, error) {
	var entry config.LLMProvider
	if err := json.Unmarshal(body, &entry); err != nil {
		return nil, &configapi.EntityError{Err: err}
	}
	raw, _ := json.Marshal(c.doc)
	next, err := config.DecodeCompany(raw)
	if err != nil {
		return nil, err
	}
	next.Providers.LLM[id] = entry
	if err := next.Validate(); err != nil {
		return nil, &configapi.ValidationError{Err: err}
	}
	return next, nil
}

func (c *fakeConfig) Reads(_ context.Context, names []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, ref := range c.doc.VariablesRead() {
		if slices.Contains(names, ref.Name) {
			out[ref.Name] = append(out[ref.Name], ref.Path)
		}
	}
	return out, nil
}

func (c *fakeConfig) Reload(_ context.Context, summary string, _ iam.Actor, expect string) (configapi.Applied, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reloads = append(c.reloads, summary)
	c.expects = append(c.expects, expect)
	return configapi.Applied{RevisionID: "r-2", Epoch: 42}, nil
}

type stored struct {
	value  string
	source secrets.Source
}

type fakeSecrets struct {
	mu   sync.Mutex
	rows map[string]stored
}

func (s *fakeSecrets) Describe(_ context.Context, name string) (secrets.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rows[name]
	return secrets.Record{Name: name}, ok, nil
}

func (s *fakeSecrets) Set(_ context.Context, name, value string, _ secrets.Author,
	source secrets.Source, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[name] = stored{value, source}
	return nil
}

type audit struct {
	mu   sync.Mutex
	rows []types.ModelActed
}

func (a *audit) Publish(_ context.Context, _ string, ev *events.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if row, ok := ev.Data.(*types.ModelActed); ok {
		a.rows = append(a.rows, *row)
	}
	return nil
}

func (a *audit) last(t *testing.T) types.ModelActed {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.rows) == 0 {
		t.Fatal("no model_acted was published")
	}
	return a.rows[len(a.rows)-1]
}

// rig is the surface over fakes, served as an operator holding every grant
// and a fresh proof.
type rig struct {
	h       http.Handler
	engine  *fakeEngine
	config  *fakeConfig
	secrets *fakeSecrets
	audit   *audit
	leases  coord.Backend
}

func newRig(t *testing.T) *rig {
	t.Helper()
	doc, err := config.ParseCompany([]byte(company))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{engine: &fakeEngine{check: llmapi.Check{At: clock, Node: "node-a", OK: true,
		Elapsed: 3400 * time.Millisecond, Hints: map[string]string{"ANTHROPIC_API_KEY_SONNET": "sk-ant-api03-…7d2K"},
		Lines: []cliagent.Check{{Name: cliagent.CheckBinary, OK: true, Detail: "claude at /usr/local/bin/claude"}}},
		running: 1},
		config: &fakeConfig{doc: doc}, secrets: &fakeSecrets{rows: map[string]stored{}},
		audit: &audit{}, leases: coordmemory.New()}
	svc, err := llmapi.New(llmapi.Options{Engine: r.engine, Config: r.config, Secrets: r.secrets,
		Leases: r.leases, Audit: r.audit, Node: "node-a", Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	caller := iam.Principal{ID: uuid.NewSHA1(auth.TokenNamespace, []byte("ops")),
		Login: iam.TokenLogin("ops"), Kind: iam.KindMachine, Stage: iam.StageActive,
		Grants: iam.AllGrants, ReauthAt: time.Now().Add(time.Hour)}
	r.h = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mux.ServeHTTP(w, req.WithContext(iam.WithPrincipal(req.Context(), caller)))
	})
	return r
}

func (r *rig) do(t *testing.T, method, path, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	out := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s answered non-JSON %d: %s", method, path, rec.Code, rec.Body)
	}
	return rec.Code, out
}

func problemAt(t *testing.T, body map[string]any) (string, string) {
	t.Helper()
	problems, _ := body["problems"].([]any)
	if len(problems) == 0 {
		t.Fatalf("no problems in %v", body)
	}
	p, _ := problems[0].(map[string]any)
	segs, _ := json.Marshal(p["segments"])
	return string(segs), p["message"].(string)
}

const sonnetKey = `{"values": {"ANTHROPIC_API_KEY_SONNET": "sk-ant-api03-not-a-real-key-7d2K"}}`

// A CREDENTIAL THE VENDOR REFUSES IS NEVER STORED.
//
// The value is handed to the entry's own CLI first; a refusal answers 422 at
// the value with the vendor's sentence, nothing is sealed or re-activated, and
// the model goes on signing in with what it had. The refusal is audited.
func TestACredentialIsTestedBeforeItIsStored(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.engine.check = llmapi.Check{At: clock, Node: "node-a", Failure: "auth",
		Said: "401 invalid x-api-key"}

	code, body := r.do(t, http.MethodPut, "/llm/providers/sonnet/credential", sonnetKey, nil)
	if code != http.StatusUnprocessableEntity || body["error"] != "credential_rejected" {
		t.Fatalf("refused credential = %d %v", code, body)
	}
	segs, message := problemAt(t, body)
	if segs != `["values","ANTHROPIC_API_KEY_SONNET"]` ||
		!strings.Contains(message, "401 invalid x-api-key") ||
		!strings.Contains(message, "Nothing was stored, and the current credential still signs the model in") {
		t.Errorf("problem at %s: %q", segs, message)
	}
	if len(r.secrets.rows) != 0 || len(r.config.reloads) != 0 {
		t.Errorf("a refused credential was stored (%v) or re-activated (%v)", r.secrets.rows, r.config.reloads)
	}
	row := r.audit.last(t)
	if row.Action != types.ModelCredential || row.Outcome != types.AuditRefused ||
		row.Refusal != "credential_rejected" || !slices.Equal(row.Names, []string{"ANTHROPIC_API_KEY_SONNET"}) {
		t.Errorf("audit = %+v", row)
	}
	if raw, _ := json.Marshal(r.audit.rows); strings.Contains(string(raw), "not-a-real-key") {
		t.Errorf("the audit carries the value: %s", raw)
	}

	r.engine.check = llmapi.Check{At: clock, Node: "node-a", Failure: "timeout"}
	if code, body := r.do(t, http.MethodPut, "/llm/providers/sonnet/credential", sonnetKey, nil); code != http.StatusBadGateway ||
		body["error"] != "credential_untested" || len(r.secrets.rows) != 0 {
		t.Errorf("an unanswered test = %d %v, stored %v", code, body, r.secrets.rows)
	}
}

// A CREDENTIAL THAT SIGNS IN IS SEALED, AND THE REVISION THAT READS IT
// RE-ACTIVATED, held to what the caller read.
//
// The answer names every entry that reads the name, the epoch, the runs still
// on the old value and the entries this node had sitting out on the sign-in
// it replaced.
func TestACredentialThatSignsInIsSealedAndReactivated(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if _, _, err := r.leases.TryAcquire(t.Context(), coord.NodeResource("node-a"), coord.AcquireOptions{
		Owner: "node-a:1", TTL: time.Minute, Meta: map[string]any{coord.StatusKey: coord.NodeStatus{
			Models: []coord.ModelStatus{{Key: "default", SignIn: "token", Resolves: true,
				SittingOut: &coord.ModelSitOut{Until: clock.Add(time.Hour), Window: "session"}}},
		}.Meta()},
	}); err != nil {
		t.Fatal(err)
	}
	code, body := r.do(t, http.MethodPut, "/llm/providers/default/credential",
		`{"values": {"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-a-new-token-Qm4x"}}`,
		map[string]string{"If-Match": `"r-1"`})
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %v", code, body)
	}
	if got := r.secrets.rows["CLAUDE_CODE_OAUTH_TOKEN"]; got.value != "sk-ant-oat01-a-new-token-Qm4x" ||
		got.source != secrets.SourceModels {
		t.Errorf("sealed %+v, want the value under the models source", got)
	}
	if len(r.config.reloads) != 1 || r.config.expects[0] != `"r-1"` {
		t.Errorf("reloads %q expecting %q, want one held to If-Match", r.config.reloads, r.config.expects)
	}
	if reload, _ := body["reload"].(map[string]any); reload["epoch"] != float64(42) {
		t.Errorf("reload = %v", body["reload"])
	}
	if entries, _ := json.Marshal(body["entries"]); string(entries) != `["default","aux"]` {
		t.Errorf("entries = %s, want both that read the token", entries)
	}
	if cleared, _ := json.Marshal(body["cleared"]); string(cleared) != `["default"]` {
		t.Errorf("cleared = %s, want the entry this node had sitting out", cleared)
	}
	if body["running_runs"] != float64(1) || !strings.Contains(body["verified"].(string), "3.4 s") {
		t.Errorf("running_runs %v verified %v", body["running_runs"], body["verified"])
	}
	// THE TEST CARRIED THE CANDIDATE, and only it.
	if c := r.engine.checks[0]; c.draft != nil || c.values["CLAUDE_CODE_OAUTH_TOKEN"] == "" || c.smoke {
		t.Errorf("the test was asked %+v", c)
	}
	if row := r.audit.last(t); row.Outcome != types.AuditApplied || row.RevisionID != "r-2" {
		t.Errorf("audit = %+v", row)
	}

	// A 429 PROVES THE CREDENTIAL AUTHENTICATED.
	r.engine.check = llmapi.Check{At: clock, Failure: "rate_limit",
		Limited: &coord.ModelSitOut{Until: clock.Add(time.Hour), Window: "weekly"}}
	code, body = r.do(t, http.MethodPut, "/llm/providers/sonnet/credential", sonnetKey, nil)
	if limited, _ := body["limited"].(map[string]any); code != http.StatusOK || limited["window"] != "weekly" {
		t.Errorf("a limited test = %d %v, want it stored and said", code, body)
	}
}

// A DRY RUN TESTS AND STORES NOTHING; A CREATE NEVER OVERWRITES.
func TestADryRunStoresNothingAndACreateNeverOverwrites(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	code, body := r.do(t, http.MethodPut, "/llm/providers/sonnet/credential?dry_run=true", sonnetKey, nil)
	if code != http.StatusOK || body["ok"] != true || body["hints"] == nil || body["check"] == nil {
		t.Fatalf("dry run = %d %v", code, body)
	}
	if len(r.secrets.rows) != 0 || len(r.config.reloads) != 0 {
		t.Errorf("a dry run stored %v or re-activated %v", r.secrets.rows, r.config.reloads)
	}
	if row := r.audit.last(t); !row.DryRun {
		t.Errorf("audit = %+v, want it marked a dry run", row)
	}

	r.secrets.rows["ANTHROPIC_API_KEY_SONNET"] = stored{value: "held"}
	code, body = r.do(t, http.MethodPut, "/llm/providers/sonnet/credential", sonnetKey,
		map[string]string{"If-None-Match": "*"})
	if code != http.StatusPreconditionFailed || body["error"] != "already_exists" {
		t.Fatalf("a create over a stored name = %d %v", code, body)
	}
	if names, _ := json.Marshal(body["names"]); string(names) != `["ANTHROPIC_API_KEY_SONNET"]` {
		t.Errorf("names = %s", names)
	}
	if r.secrets.rows["ANTHROPIC_API_KEY_SONNET"].value != "held" || len(r.engine.checks) != 1 {
		t.Errorf("a refused create overwrote or tested: %v, %d tests", r.secrets.rows, len(r.engine.checks))
	}
}

// A VALUE THE MODEL DOES NOT READ, OR OF THE WRONG KIND, IS REFUSED AT THE
// VALUE before any call is made.
func TestAValueTheModelCannotUseIsRefusedAtTheValue(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	code, body := r.do(t, http.MethodPut, "/llm/providers/sonnet/credential",
		`{"values": {"ANTHROPIC_API_KEY": "sk-ant-api03-whatever-it-is-1234"}}`, nil)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("an unread name = %d %v", code, body)
	}
	if segs, message := problemAt(t, body); segs != `["values","ANTHROPIC_API_KEY"]` ||
		!strings.Contains(message, "ANTHROPIC_API_KEY is not read by this model: it signs in with ANTHROPIC_API_KEY_SONNET") {
		t.Errorf("problem at %s: %q", segs, message)
	}
	code, body = r.do(t, http.MethodPut, "/llm/providers/default/credential",
		`{"values": {"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-api03-this-is-a-key-1234"}}`, nil)
	if segs, message := problemAt(t, body); code != http.StatusUnprocessableEntity ||
		segs != `["values","CLAUDE_CODE_OAUTH_TOKEN"]` || !strings.HasPrefix(message, "That is an API key") {
		t.Errorf("a key in a token slot = %d at %s: %q", code, segs, message)
	}
	if len(r.engine.checks) != 0 {
		t.Errorf("a refused value was tested %d times", len(r.engine.checks))
	}
	if code, _ := r.do(t, http.MethodPut, "/llm/providers/nope/credential", sonnetKey, nil); code != http.StatusNotFound {
		t.Errorf("an absent model with no draft = %d, want 404", code)
	}
}

// A DRAFT IS TESTED AS THE DOCUMENT WOULD RUN IT: a create, validated whole,
// and its test handed the draft.
func TestADraftIsValidatedAndTested(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	body := `{"values": {"OPUS_KEY": "sk-ant-api03-a-brand-new-key-0000"},
		"entity": {"agent": "claude-code", "model": "opus",
		"auth": {"mode": "api-key", "api_key": "${OPUS_KEY}"}}}`
	code, got := r.do(t, http.MethodPut, "/llm/providers/opus/credential?dry_run=true", body, nil)
	if code != http.StatusOK {
		t.Fatalf("a draft's test = %d %v", code, got)
	}
	if c := r.engine.checks[0]; c.draft == nil || c.draft.Model != "opus" {
		t.Errorf("the test was not handed the draft: %+v", c)
	}
	code, got = r.do(t, http.MethodPut, "/llm/providers/opus/credential",
		`{"values": {"OPUS_KEY": "x"}, "entity": {"agent": "claude-code",
		"auth": {"mode": "api-key", "api_key": "${OPUS_KEY}", "token": "${T}"}}}`, nil)
	if code != http.StatusUnprocessableEntity || got["error"] != "validation_error" || got["problems"] == nil {
		t.Errorf("an invalid draft = %d %v, want 422 with problems", code, got)
	}
}

// A RETRY, A CHECK AND A HOST LOGIN READ ANSWER IN THEIR SHAPES, AND REFUSE IN
// THEIR WORDS.
func TestTheRuntimeVerbsAnswerInTheirShapes(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.engine.retry = llmapi.Retried{Cleared: false, Node: "node-a",
		Still: &coord.ModelSitOut{Until: clock.Add(time.Hour), Window: "session", Said: "You've hit your session limit"}}
	code, body := r.do(t, http.MethodPost, "/llm/providers/default/retry", `{"mode": "probe"}`, nil)
	if still, _ := body["still"].(map[string]any); code != http.StatusOK || body["cleared"] != false ||
		still["window"] != "session" || body["node"] != "node-a" {
		t.Errorf("probe = %d %v", code, body)
	}
	r.engine.retry = llmapi.Retried{Node: "node-a", Failed: "401 authentication_failed"}
	if _, body := r.do(t, http.MethodPost, "/llm/providers/default/retry", `{"mode": "probe"}`, nil); body["failed"] != "401 authentication_failed" || body["cleared"] != false {
		t.Errorf("a probe that failed otherwise = %v, want it said in failed", body)
	}
	r.engine.retErr = llmapi.ErrNotSittingOut
	if code, body := r.do(t, http.MethodPost, "/llm/providers/default/retry", `{"mode": "probe"}`, nil); code != http.StatusConflict ||
		body["error"] != "not_sitting_out" {
		t.Errorf("a probe of a model nothing sits out = %d %v", code, body)
	}
	r.engine.retErr = nil
	r.do(t, http.MethodPost, "/llm/providers/default/retry", `{"mode": "clear"}`, nil)
	if row := r.audit.last(t); row.Action != types.ModelClear {
		t.Errorf("a clear audited as %s", row.Action)
	}
	if code, _ := r.do(t, http.MethodPost, "/llm/providers/default/retry", `{"mode": "force"}`, nil); code != http.StatusBadRequest {
		t.Errorf("an unknown mode = %d", code)
	}

	code, body = r.do(t, http.MethodPost, "/llm/providers/default/check", `{"smoke": true}`, nil)
	if lines, _ := body["lines"].([]any); code != http.StatusOK || body["ok"] != true || len(lines) != 1 {
		t.Errorf("check = %d %v", code, body)
	}
	if c := r.engine.checks[len(r.engine.checks)-1]; !c.smoke || c.values != nil {
		t.Errorf("the check was asked %+v, want a smoke run and no value", c)
	}

	r.engine.login = coord.HostLogin{State: coord.HostSignedIn, Method: "claude.ai", CheckedAt: clock}
	code, body = r.do(t, http.MethodPost, "/llm/providers/default/host-login/check", "", nil)
	if shares, _ := json.Marshal(body["shares_login_with"]); code != http.StatusOK ||
		body["state"] != "signed_in" || body["command"] != "crewlet llm login default" || string(shares) != `["aux"]` {
		t.Errorf("host login = %d %v", code, body)
	}
	r.engine.logErr = llmapi.ErrNoHostLogin
	if code, body := r.do(t, http.MethodPost, "/llm/providers/sonnet/host-login/check", "", nil); code != http.StatusUnprocessableEntity ||
		body["error"] != "no_host_login" {
		t.Errorf("a model with no login = %d %v", code, body)
	}
}

// A DEVICE SIGN-IN ANSWERS ITS CODE, ONE AT A TIME PER MODEL, AND IS CANCELLED
// BY ITS ID.
func TestADeviceSignInIsOneAtATime(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.engine.device = coord.DeviceSignIn{ID: uuid.New(), Entry: "codex-review", Node: "node-a",
		URL: "https://auth.openai.com/codex/device", Code: "QXJM-7KPD", Status: coord.DevicePending,
		StartedAt: clock, ExpiresAt: clock.Add(15 * time.Minute)}
	code, body := r.do(t, http.MethodPost, "/llm/providers/codex-review/device-sign-in", "{}", nil)
	if code != http.StatusCreated || body["code"] != "QXJM-7KPD" || body["status"] != "pending" {
		t.Fatalf("start = %d %v", code, body)
	}
	r.engine.devErr = &llmapi.SignInInProgressError{Running: r.engine.device}
	if code, body := r.do(t, http.MethodPost, "/llm/providers/codex-review/device-sign-in", "{}", nil); code != http.StatusConflict ||
		body["error"] != "sign_in_in_progress" || body["running"] == nil {
		t.Errorf("a second start = %d %v", code, body)
	}
	r.engine.devErr = &llmapi.DeviceSignInUnavailableError{Line: "error: device code login is disabled"}
	if code, body := r.do(t, http.MethodPost, "/llm/providers/codex-review/device-sign-in", "{}", nil); code != http.StatusBadGateway ||
		!strings.Contains(body["detail"].(string), "disabled") {
		t.Errorf("no code = %d %v", code, body)
	}
	code, body = r.do(t, http.MethodDelete, "/llm/device-sign-ins/"+r.engine.device.ID.String(), "", nil)
	if code != http.StatusOK || body["status"] != "cancelled" {
		t.Errorf("cancel = %d %v", code, body)
	}
	if row := r.audit.last(t); row.ProviderKey != "codex-review" || row.Action != types.ModelDeviceSignIn {
		t.Errorf("a cancel audited as %+v", row)
	}
	if code, _ := r.do(t, http.MethodDelete, "/llm/device-sign-ins/"+uuid.NewString(), "", nil); code != http.StatusNotFound {
		t.Errorf("an unknown sign-in = %d", code)
	}
}

// recordingMux keeps the policy each route was mounted with.
type recordingMux struct {
	*http.ServeMux
	policies map[string]authz.Policy
}

func (m *recordingMux) MountedWith(pattern string, p authz.Policy) { m.policies[pattern] = p }

// EVERY ROUTE IS DECIDED ON ITS OWN VERB: a credential and a sign-in are a
// secret's write, a check and a host login read the models' check, a retry
// the models' retry.
func TestEveryModelRouteTakesItsVerb(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	svc, err := llmapi.New(llmapi.Options{Engine: r.engine, Config: r.config, Secrets: r.secrets,
		Leases: r.leases, Audit: r.audit, Node: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	mux := &recordingMux{ServeMux: http.NewServeMux(), policies: map[string]authz.Policy{}}
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	want := map[string]authz.Action{
		"PUT /llm/providers/{key}/credential":        authz.ActionSecretWrite,
		"POST /llm/providers/{key}/check":            authz.ActionModelsCheck,
		"POST /llm/providers/{key}/retry":            authz.ActionModelsRetry,
		"POST /llm/providers/{key}/host-login/check": authz.ActionModelsCheck,
		"POST /llm/providers/{key}/device-sign-in":   authz.ActionSecretWrite,
		"DELETE /llm/device-sign-ins/{id}":           authz.ActionSecretWrite,
	}
	for pattern, action := range want {
		if got, ok := mux.policies[pattern]; !ok || got.Action != action {
			t.Errorf("%s is mounted with %+v, want %s", pattern, got, action)
		}
	}
	if len(mux.policies) != len(want) {
		t.Errorf("mounted %d routes, want %d", len(mux.policies), len(want))
	}
}
