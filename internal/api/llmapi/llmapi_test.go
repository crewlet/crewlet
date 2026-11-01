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
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
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
      harness: codex
      model: gpt-6.1-sol
    gateway:
      model: sonnet
      auth: {mode: api-key, api_key: "${GATEWAY_KEY}"}
      env: {ANTHROPIC_BASE_URL: "${GATEWAY_URL}"}
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
	// onCheck runs as a check returns: a test cancels the request there.
	onCheck func()
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
	if f.onCheck != nil {
		f.onCheck()
	}
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

func (c *fakeConfig) Reload(ctx context.Context, summary string, _ iam.Actor, expect string) (configapi.Applied, error) {
	if err := ctx.Err(); err != nil {
		return configapi.Applied{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reloads = append(c.reloads, summary)
	c.expects = append(c.expects, expect)
	return configapi.Applied{RevisionID: "r-2", Epoch: 42}, nil
}

type stored struct {
	value  string
	source secrets.Source
	// op is the operation key of the create that sealed the row, and
	// sealed when, by the service's clock.
	op     string
	sealed time.Time
}

type fakeSecrets struct {
	mu   sync.Mutex
	rows map[string]stored
	// refuse is what a seal of each name answers instead of storing it.
	refuse map[string]error
	// reclaimAnyAge makes Reclaim re-seal a row whatever its seal time, so a
	// case pins the route's own window check alone.
	reclaimAnyAge bool
}

func (s *fakeSecrets) Describe(_ context.Context, name string) (secrets.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rows[name]
	return secrets.Record{Name: name}, ok, nil
}

func (s *fakeSecrets) Create(ctx context.Context, name, value string, _ secrets.Author,
	source secrets.Source, now time.Time, opID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refuse[name]; err != nil {
		return false, err
	}
	if _, held := s.rows[name]; held {
		return false, nil
	}
	s.rows[name] = stored{value, source, opID, now}
	return true, nil
}

func (s *fakeSecrets) OpOf(_ context.Context, name string) (string, time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[name]
	return row.op, row.sealed, ok, nil
}

func (s *fakeSecrets) Reclaim(_ context.Context, name, value string, _ secrets.Author,
	source secrets.Source, now time.Time, opID string, notBefore time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refuse[name]; err != nil {
		return false, err
	}
	row, ok := s.rows[name]
	if !ok || opID == "" || row.op != opID || (!s.reclaimAnyAge && row.sealed.Before(notBefore)) {
		return false, nil
	}
	s.rows[name] = stored{value, source, opID, now}
	return true, nil
}

func (s *fakeSecrets) Set(ctx context.Context, name, value string, _ secrets.Author,
	source secrets.Source, _ time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refuse[name]; err != nil {
		return err
	}
	s.rows[name] = stored{value: value, source: source}
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
	guard   *recordingGuard
}

// recordingGuard is the engine's guard, recording every policy it is asked
// and refusing the actions in deny.
type recordingGuard struct {
	mu    sync.Mutex
	asked []authz.Action
	deny  map[authz.Action]bool
}

func (g *recordingGuard) decide(r *http.Request, p authz.Policy) authz.Decision {
	g.mu.Lock()
	g.asked = append(g.asked, p.Action)
	denied := g.deny[p.Action]
	g.mu.Unlock()
	if denied {
		return authz.Decision{Allowed: false}
	}
	return authz.ContextGuard(authz.NoChart{})(r, p)
}

func (g *recordingGuard) wasAsked(action authz.Action) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Contains(g.asked, action)
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return newRigAs(t, iam.Principal{ID: uuid.NewSHA1(auth.TokenNamespace, []byte("ops")),
		Login: iam.TokenLogin("ops"), Kind: iam.KindMachine, Stage: iam.StageActive,
		Grants: iam.AllGrants, ReauthAt: time.Now().Add(time.Hour)})
}

// newRigAs is [newRig] served as caller.
func newRigAs(t *testing.T, caller iam.Principal) *rig {
	t.Helper()
	return newRigOver(t, caller, &fakeSecrets{rows: map[string]stored{}})
}

// newRigOver is [newRigAs] over the secret store store, which another rig may
// share.
func newRigOver(t *testing.T, caller iam.Principal, store *fakeSecrets) *rig {
	t.Helper()
	doc, err := config.ParseCompany([]byte(company))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{engine: &fakeEngine{check: llmapi.Check{At: clock, Node: "node-a", OK: true,
		Elapsed: 3400 * time.Millisecond, Hints: map[string]string{"ANTHROPIC_API_KEY_SONNET": "sk-ant-api03-…7d2K"},
		Lines: []harness.Check{{Name: harness.CheckBinary, OK: true, Detail: "claude at /usr/local/bin/claude"}}},
		running: 1},
		config: &fakeConfig{doc: doc}, secrets: store,
		audit: &audit{}, leases: coordmemory.New(), guard: &recordingGuard{deny: map[authz.Action]bool{}}}
	svc, err := llmapi.New(llmapi.Options{Engine: r.engine, Config: r.config, Secrets: r.secrets,
		Leases: r.leases, Audit: r.audit, Node: "node-a", Now: func() time.Time { return clock },
		Guard: r.guard.decide})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	r.h = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mux.ServeHTTP(w, req.WithContext(iam.WithPrincipal(req.Context(), caller)))
	})
	return r
}

func (r *rig) do(t *testing.T, method, path, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	return r.doCtx(t.Context(), t, method, path, body, headers)
}

// doCtx is [rig.do] under ctx.
func (r *rig) doCtx(ctx context.Context, t *testing.T, method, path, body string,
	headers map[string]string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
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
// The value is handed to the entry's own harness first; a refusal answers 422 at
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
		message != "The vendor refused this API key: it is wrong, expired, or revoked. Paste a current one. "+
			"Nothing was stored, and the model still uses its current API key." {
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
		"entity": {"harness": "claude-code", "model": "opus",
		"auth": {"mode": "api-key", "api_key": "${OPUS_KEY}"}}}`
	code, got := r.do(t, http.MethodPut, "/llm/providers/opus/credential?dry_run=true", body, nil)
	if code != http.StatusOK {
		t.Fatalf("a draft's test = %d %v", code, got)
	}
	if c := r.engine.checks[0]; c.draft == nil || c.draft.Model != "opus" {
		t.Errorf("the test was not handed the draft: %+v", c)
	}
	code, got = r.do(t, http.MethodPut, "/llm/providers/opus/credential",
		`{"values": {"OPUS_KEY": "x"}, "entity": {"harness": "claude-code",
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
	// A CANCEL IS ITS OWN GESTURE in the trail, never read as a start.
	if row := r.audit.last(t); row.ProviderKey != "codex-review" || row.Action != types.ModelDeviceSignInCancel {
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
	// A CREDENTIAL'S DRY RUN IS NO CHECK (ADR-0055): it runs the candidate,
	// and a draft's harness, so it is decided as the write it tests, never as a
	// read grant's check.
	if got := mux.policies["PUT /llm/providers/{key}/credential"]; got.DryRun {
		t.Errorf("the credential route declares its dry run a check: %+v", got)
	}
	if len(mux.policies) != len(want) {
		t.Errorf("mounted %d routes, want %d", len(mux.policies), len(want))
	}
}

// draftBody is a check of a draft entry: a model the company does not run yet.
const draftBody = `{"smoke": false, "entity": {"harness": "claude-code", "model": "opus",
	"auth": {"mode": "api-key", "api_key": "${ANTHROPIC_API_KEY_SONNET}"}}}`

// A DRAFT IS RUN ONLY BY WHO MAY WRITE WHAT IT STANDS FOR.
//
// A draft chooses its harness, its binary, its overrides and its env, and resolves
// its ${VAR}s through the secret store, so checking one runs what a config
// write could make the company run with the company's credentials. It is asked
// as `config.write` and `secrets.write`: a person whose proof is stale is told
// to step up, and the assistant, which never takes `secrets.write`, is refused.
// A check of the entry the company already runs stays `models.check`'s.
func TestADraftIsCheckedOnlyByWhoMayWriteIt(t *testing.T) {
	t.Parallel()
	person := func(proved time.Duration) iam.Principal {
		return iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
			Stage: iam.StageActive, Grants: iam.AllGrants, ReauthAt: time.Now().Add(-proved).Add(time.Hour)}
	}
	assistant := person(time.Minute)
	assistant.Via, assistant.AssistScope = iam.AssistName(uuid.New()), iam.AssistScopeExec

	for _, tc := range []struct {
		name   string
		caller iam.Principal
		error  string
	}{
		{"a stale proof", person(2 * time.Hour), "step_up_required"},
		{"the assistant", assistant, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRigAs(t, tc.caller)
			code, body := r.do(t, http.MethodPost, "/llm/providers/opus/check", draftBody, nil)
			if code != http.StatusForbidden || (tc.error != "" && body["error"] != tc.error) {
				t.Errorf("a draft check = %d %v, want 403 %s", code, body, tc.error)
			}
			code, body = r.do(t, http.MethodPut, "/llm/providers/opus/credential?dry_run=true",
				`{"values": {"ANTHROPIC_API_KEY_SONNET": "sk-ant-api03-a-candidate-key-0000"},
				"entity": {"harness": "claude-code", "model": "opus",
				"auth": {"mode": "api-key", "api_key": "${ANTHROPIC_API_KEY_SONNET}"}}}`, nil)
			if code != http.StatusForbidden {
				t.Errorf("a draft credential test = %d %v, want 403", code, body)
			}
			if len(r.engine.checks) != 0 {
				t.Errorf("a refused draft was run: %+v", r.engine.checks)
			}
		})
	}

	// THE ENTRY THE COMPANY RUNS stays models.check's, for the same stale
	// proof and for the assistant on approval.
	for _, caller := range []iam.Principal{person(2 * time.Hour), assistant} {
		r := newRigAs(t, caller)
		if code, body := r.do(t, http.MethodPost, "/llm/providers/default/check", `{"smoke": false}`, nil); code != http.StatusOK {
			t.Errorf("a saved entry's check as %s = %d %v, want 200", caller.Login, code, body)
		}
	}
	// AND A FRESH ADMIN RUNS A DRAFT.
	r := newRig(t)
	if code, body := r.do(t, http.MethodPost, "/llm/providers/opus/check", draftBody, nil); code != http.StatusOK ||
		len(r.engine.checks) != 1 || r.engine.checks[0].draft == nil {
		t.Errorf("a fresh admin's draft check = %d %v", code, body)
	}
}

// A CREDENTIAL'S DRAFT ASKS `config.write` OF ITS OWN, and its write asks it
// before a re-activation.
//
// The route is mounted at `secrets.write`, which is admin's with a recent
// proof as `config.write` is, so no caller tells the two apart: a guard that
// refuses `config.write` alone does. A draft is refused before it runs, and a
// write whose name a revision reads is refused before anything is tested or
// sealed; a saved entry's dry run never asks it.
func TestACredentialAsksConfigWriteWhereItWritesTheConfig(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.guard.deny[authz.ActionConfigWrite] = true

	code, body := r.do(t, http.MethodPut, "/llm/providers/opus/credential?dry_run=true",
		`{"values": {"ANTHROPIC_API_KEY_SONNET": "sk-ant-api03-a-candidate-key-0000"},
		"entity": {"harness": "claude-code", "model": "opus",
		"auth": {"mode": "api-key", "api_key": "${ANTHROPIC_API_KEY_SONNET}"}}}`, nil)
	if code != http.StatusForbidden || len(r.engine.checks) != 0 {
		t.Errorf("a draft without config.write = %d %v, ran %d checks", code, body, len(r.engine.checks))
	}

	code, body = r.do(t, http.MethodPut, "/llm/providers/sonnet/credential", sonnetKey, nil)
	if code != http.StatusForbidden || len(r.engine.checks) != 0 || len(r.secrets.rows) != 0 ||
		len(r.config.reloads) != 0 {
		t.Errorf("a write re-activating without config.write = %d %v: checks %d, sealed %v, reloads %v",
			code, body, len(r.engine.checks), r.secrets.rows, r.config.reloads)
	}

	fresh := newRig(t)
	if code, body := fresh.do(t, http.MethodPut, "/llm/providers/sonnet/credential?dry_run=true",
		sonnetKey, nil); code != http.StatusOK || fresh.guard.wasAsked(authz.ActionConfigWrite) {
		t.Errorf("a saved entry's dry run = %d %v, asked config.write %v", code, body,
			fresh.guard.wasAsked(authz.ActionConfigWrite))
	}
}

// A CREDENTIAL THAT PASSED ITS TEST IS SEALED AND RE-ACTIVATED EVEN WHEN ITS
// CALLER LEAVES.
//
// The test takes up to two minutes; a person who closes the page then would
// otherwise leave the request's context cancelled under the seal and the
// re-activation, and a two-value write half stored.
func TestACredentialFinishesWhenItsCallerLeaves(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.engine.onCheck = cancel
	code, body := r.doCtx(ctx, t, http.MethodPut, "/llm/providers/sonnet/credential", sonnetKey, nil)
	if code != http.StatusOK || r.secrets.rows["ANTHROPIC_API_KEY_SONNET"].value == "" ||
		len(r.config.reloads) != 1 {
		t.Errorf("a write whose caller left = %d %v: sealed %v, reloads %v", code, body,
			r.secrets.rows, r.config.reloads)
	}
}

// A SEAL THAT STOPS PART WAY SAYS WHAT IT STORED, and re-activates nothing.
func TestACredentialSealThatFailsSaysWhatItStored(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.secrets.refuse = map[string]error{"ANTHROPIC_API_KEY_SONNET": context.DeadlineExceeded}
	code, body := r.do(t, http.MethodPut, "/llm/providers/sonnet/credential", sonnetKey, nil)
	sealed, ok := body["sealed"].([]any)
	if code != http.StatusInternalServerError || !ok || len(sealed) != 0 ||
		!strings.Contains(body["detail"].(string), "Nothing was stored") || len(r.config.reloads) != 0 {
		t.Errorf("a refused seal = %d %v, reloads %v, want 500 naming no value sealed", code, body,
			r.config.reloads)
	}
}

// A NAME THAT IS NOT ONE NEVER REACHES THE AUDIT: it may be the key itself,
// pasted where the name goes.
func TestARefusedNameIsNotAudited(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	code, body := r.do(t, http.MethodPut, "/llm/providers/sonnet/credential",
		`{"values": {"sk-ant-api03-pasted-into-the-name-7d2K": "x"}}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("a key as a name = %d %v, want 400", code, body)
	}
	if raw, _ := json.Marshal(r.audit.rows); strings.Contains(string(raw), "pasted-into-the-name") {
		t.Errorf("the audit carries the refused name: %s", raw)
	}
}

// A TEST'S 429 KNOWS NO END: `limited.until` is null rather than the zero
// time, which a page drew as the year 1.
func TestALimitedCandidateSaysNoEnd(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.engine.check = llmapi.Check{At: clock, Failure: "rate_limit",
		Limited: &coord.ModelSitOut{Window: "unknown", Said: "429 rate limited"}}
	code, body := r.do(t, http.MethodPut, "/llm/providers/sonnet/credential?dry_run=true", sonnetKey, nil)
	limited, _ := body["limited"].(map[string]any)
	if code != http.StatusOK || limited == nil {
		t.Fatalf("a limited candidate = %d %v", code, body)
	}
	if until, present := limited["until"]; !present || until != nil {
		t.Errorf("limited.until = %v, want null", until)
	}
}

// A CREATE NEVER SEALS OVER A VALUE STORED WHILE IT WAS TESTED.
//
// `If-None-Match: *` is answered early, before the test, and then decided
// again at the seal in one step of the store's: another writer that stored
// the name during the test keeps its value, and nothing is re-activated.
func TestACreateNeverSealsOverAValueStoredDuringItsTest(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.engine.onCheck = func() {
		r.secrets.mu.Lock()
		r.secrets.rows["ANTHROPIC_API_KEY_SONNET"] = stored{value: "theirs", source: secrets.SourceCLI}
		r.secrets.mu.Unlock()
	}
	code, body := r.do(t, http.MethodPut, "/llm/providers/sonnet/credential", sonnetKey,
		map[string]string{"If-None-Match": "*"})
	if code != http.StatusPreconditionFailed || body["error"] != "already_exists" {
		t.Errorf("a create that lost the name = %d %v, want 412 already_exists", code, body)
	}
	if got := r.secrets.rows["ANTHROPIC_API_KEY_SONNET"].value; got != "theirs" || len(r.config.reloads) != 0 {
		t.Errorf("the stored value is %q, reloads %v: the create sealed over it", got, r.config.reloads)
	}
}

// A CALL THE GUARD TURNS AWAY IS IN THE TRAIL TOO.
//
// The router refuses a member's call, and a stale proof's, before the handler
// runs, so the handler's record never saw them: the trail held every call on
// a model but the ones somebody was not allowed to make.
func TestACallTheGuardRefusesIsAudited(t *testing.T) {
	t.Parallel()
	member := iam.Principal{ID: uuid.New(), Login: "sam.member", Kind: iam.KindPerson,
		Stage: iam.StageActive, ReauthAt: time.Now().Add(time.Hour)}
	r := newRigAs(t, member)
	if code, body := r.do(t, http.MethodPut, "/llm/providers/sonnet/credential", sonnetKey, nil); code != http.StatusForbidden {
		t.Fatalf("a member's credential write = %d %v", code, body)
	}
	if row := r.audit.last(t); row.Action != types.ModelCredential || row.ProviderKey != "sonnet" ||
		row.Outcome != types.AuditRefused || row.Refusal != "unauthorized" || row.ActorName == "" {
		t.Errorf("the refused call is audited as %+v", row)
	}

	stale := iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: iam.AllGrants, ReauthAt: time.Now().Add(-time.Hour)}
	r = newRigAs(t, stale)
	if code, _ := r.do(t, http.MethodDelete, "/llm/device-sign-ins/"+uuid.NewString(), "", nil); code != http.StatusForbidden {
		t.Fatalf("a stale proof's cancel = %d", code)
	}
	if row := r.audit.last(t); row.Action != types.ModelDeviceSignInCancel ||
		row.Outcome != types.AuditRefused || row.Refusal != "step_up_required" {
		t.Errorf("the step-up refusal is audited as %+v", row)
	}
}

// gatewayCreate is a create of both values the gateway entry reads.
const gatewayCreate = `{"values": {"GATEWAY_KEY": "sk-ant-api03-a-gateway-key-0000",
	"GATEWAY_URL": "https://gw.example.com"}}`

// createAs is a create-only credential write of body for the gateway entry, with
// the operation key key ("" for none).
func createAs(t *testing.T, r *rig, body, key string) (int, map[string]any) {
	t.Helper()
	headers := map[string]string{"If-None-Match": "*"}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	return r.do(t, http.MethodPut, "/llm/providers/gateway/credential", body, headers)
}

// A CREATE THAT STOPPED PART WAY IS FINISHED BY ITS OWN RETRY, KNOWN BY ITS KEY.
//
// A create of several names seals them one at a time; one that stopped after
// the first left it stored, and a retry of the same body was refused 412 on
// what that very request sealed. The rows a create seals carry its operation
// key, and the same request sent again under that key re-seals them with the
// values it sends now, never reading what is stored: a changed value lands,
// and nothing about a stored value is answered. Without the key, or under
// another, the names are another's and refused.
func TestARetriedCreateFinishesWhatItsKeySealed(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.secrets.refuse = map[string]error{"GATEWAY_URL": context.DeadlineExceeded}
	code, body := createAs(t, r, gatewayCreate, "")
	if sealed, _ := json.Marshal(body["sealed"]); code != http.StatusInternalServerError ||
		string(sealed) != `["GATEWAY_KEY"]` {
		t.Fatalf("the first attempt = %d %v, want it stopped after GATEWAY_KEY", code, body)
	}
	key, _ := body["op_id"].(string)
	if key == "" {
		t.Fatalf("the stopped attempt names no op_id to retry with: %v", body)
	}
	r.secrets.refuse = nil

	if code, body := createAs(t, r, gatewayCreate, ""); code != http.StatusPreconditionFailed {
		t.Errorf("a retry with no key = %d %v, want 412", code, body)
	}
	if code, body := createAs(t, r, gatewayCreate, statelog.NewOpID(clock, "")); code != http.StatusPreconditionFailed {
		t.Errorf("a retry under another key = %d %v, want 412", code, body)
	}

	// THE SAME KEY, A CHANGED VALUE: the end state is the retry's.
	changed := `{"values": {"GATEWAY_KEY": "sk-ant-api03-a-corrected-key-1111",
		"GATEWAY_URL": "https://gw.example.com"}}`
	code, body = createAs(t, r, changed, key)
	if code != http.StatusOK {
		t.Fatalf("the retry under its key = %d %v, want it to finish", code, body)
	}
	if got := r.secrets.rows["GATEWAY_KEY"].value; got != "sk-ant-api03-a-corrected-key-1111" {
		t.Errorf("GATEWAY_KEY is %q, want the retried request's value", got)
	}
	if got := r.secrets.rows["GATEWAY_URL"].value; got != "https://gw.example.com" {
		t.Errorf("GATEWAY_URL is %q", got)
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), key) {
		t.Errorf("a successful answer carries the operation key: %s", raw)
	}
	if raw, _ := json.Marshal(r.audit.rows); strings.Contains(string(raw), key) {
		t.Errorf("the audit carries the operation key: %s", raw)
	}
}

// A GUESS AT ANOTHER REQUEST'S VALUE LEARNS NOTHING.
//
// A create naming a secret another request sealed is 412 whether the value it
// sends equals what is stored or not, with the same answer, and the stored
// value is untouched: a create that told the two apart would let a caller
// test guesses against a write-only secret.
func TestACreateNeverAnswersWhetherAGuessMatches(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if code, body := createAs(t, r, gatewayCreate, ""); code != http.StatusOK {
		t.Fatalf("the first create = %d %v", code, body)
	}
	wrong := `{"values": {"GATEWAY_KEY": "sk-ant-api03-a-wrong-guess-9999",
		"GATEWAY_URL": "https://gw.example.com"}}`
	var answers []string
	for _, guess := range []string{gatewayCreate, wrong} {
		code, body := createAs(t, r, guess, "")
		if code != http.StatusPreconditionFailed {
			t.Errorf("a guess = %d %v, want 412 whether or not it matches", code, body)
		}
		delete(body, "op_id")
		raw, _ := json.Marshal(body)
		answers = append(answers, string(raw))
	}
	if answers[0] != answers[1] {
		t.Errorf("a matching and a wrong guess are answered apart:\n%s\n%s", answers[0], answers[1])
	}
	if got := r.secrets.rows["GATEWAY_KEY"].value; got != "sk-ant-api03-a-gateway-key-0000" {
		t.Errorf("a guess changed the stored value: %q", got)
	}
}

// A KEY IS ITS PRINCIPAL'S, AND ITS DAY'S. Another principal sending the same
// key, and a key past [llmapi.CreateRetryWindow], finish nothing.
func TestACreatesKeyIsItsPrincipalsAndExpires(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.secrets.refuse = map[string]error{"GATEWAY_URL": context.DeadlineExceeded}
	_, body := createAs(t, r, gatewayCreate, "")
	key, _ := body["op_id"].(string)
	r.secrets.refuse = nil

	other := newRigOver(t, iam.Principal{ID: uuid.New(), Login: iam.TokenLogin("other"),
		Kind: iam.KindMachine, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: time.Now().Add(time.Hour)}, r.secrets)
	if code, body := createAs(t, other, gatewayCreate, key); code != http.StatusPreconditionFailed {
		t.Errorf("another principal under the same key = %d %v, want 412", code, body)
	}

	old := newRig(t)
	stale := statelog.NewOpID(clock.Add(-llmapi.CreateRetryWindow-time.Hour), "")
	old.secrets.refuse = map[string]error{"GATEWAY_URL": context.DeadlineExceeded}
	_, body = createAs(t, old, gatewayCreate, stale)
	staleKey, _ := body["op_id"].(string)
	old.secrets.refuse = nil
	if code, body := createAs(t, old, gatewayCreate, staleKey); code != http.StatusPreconditionFailed {
		t.Errorf("a key past the retry window = %d %v, want 412", code, body)
	}
}

// A CREATE'S RETRY WINDOW RUNS FROM THE SEAL, BY THE ENGINE'S CLOCK.
//
// A caller mints its own key, and a UUIDv7 carries whatever instant the
// caller wrote into it, so the window cannot be measured from the key: a key
// dated a year ahead stamps rows its retry could take for a day a year later.
// Rows sealed past the window are refused to the same key whatever its own
// instant says: a key that looks fresh now (minted "a year ahead" of rows
// sealed a year ago) and a key minted an hour ago over rows sealed a day and
// an hour ago alike, and the stored values are untouched.
func TestACreatesRetryWindowRunsFromItsSeal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		minted time.Time
		sealed time.Time
	}{
		{"a key dated ahead of rows sealed a year ago", clock, clock.Add(-365 * 24 * time.Hour)},
		{"a key from an hour ago over rows sealed a day and an hour ago", clock.Add(-time.Hour),
			clock.Add(-llmapi.CreateRetryWindow - time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.secrets.refuse = map[string]error{"GATEWAY_URL": context.DeadlineExceeded}
			_, body := createAs(t, r, gatewayCreate, statelog.NewOpID(tc.minted, ""))
			key, _ := body["op_id"].(string)
			if key == "" {
				t.Fatalf("the first attempt names no op_id: %v", body)
			}
			r.secrets.refuse = nil
			r.secrets.mu.Lock()
			row := r.secrets.rows["GATEWAY_KEY"]
			row.sealed = tc.sealed
			r.secrets.rows["GATEWAY_KEY"] = row
			r.secrets.mu.Unlock()

			code, body := createAs(t, r, gatewayCreate, key)
			if code != http.StatusPreconditionFailed {
				t.Errorf("a retry over a row sealed outside the window = %d %v, want 412", code, body)
			}
			if got := r.secrets.rows["GATEWAY_KEY"]; got.value != "sk-ant-api03-a-gateway-key-0000" ||
				!got.sealed.Equal(tc.sealed) {
				t.Errorf("the stale row was re-sealed: %+v", got)
			}
		})
	}
}

// THE ROUTE ASKS THE SEAL'S TIME OF ITS OWN, before any re-seal: with a store
// that would re-seal a row of any age, a retry over rows sealed outside the
// window is still refused, by the early check, and nothing is written.
func TestACreatesRetryIsRefusedByTheRouteBeforeTheStore(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.secrets.refuse = map[string]error{"GATEWAY_URL": context.DeadlineExceeded}
	_, body := createAs(t, r, gatewayCreate, "")
	key, _ := body["op_id"].(string)
	r.secrets.refuse = nil
	r.secrets.mu.Lock()
	row := r.secrets.rows["GATEWAY_KEY"]
	row.sealed = clock.Add(-llmapi.CreateRetryWindow - time.Hour)
	r.secrets.rows["GATEWAY_KEY"] = row
	r.secrets.reclaimAnyAge = true
	r.secrets.mu.Unlock()

	code, body := createAs(t, r, gatewayCreate, key)
	if names, _ := json.Marshal(body["names"]); code != http.StatusPreconditionFailed ||
		string(names) != `["GATEWAY_KEY"]` {
		t.Errorf("a retry over a row sealed outside the window = %d %v, want the route's 412 naming it", code, body)
	}
	if got := r.secrets.rows["GATEWAY_KEY"]; !got.sealed.Equal(row.sealed) {
		t.Errorf("the stale row was re-sealed: %+v", got)
	}
}

// A VALUE WITH A SPACE OR AN INVISIBLE CHARACTER INSIDE IS REFUSED AT THE
// VALUE, BEFORE ANY HARNESS RUNS. A terminal that wraps `claude setup-token`
// output turns the break into spaces when it is copied, and Claude Code then
// ignores the token and says it is not logged in, which read as a refusal to
// log in again. The whitespace around a value is only trimmed.
func TestAValueWithStrayCharactersIsRefusedBeforeItsTest(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	for _, tc := range []struct {
		name, value, want string
	}{
		{"a wrapped token", `sk-ant-oat01-abc  def-AA`,
			"This token contains 2 spaces (characters 17 and 18). A token is one unbroken line, " +
				"so copy it again or remove them."},
		{"a line break", `sk-ant-oat01-abc\ndef-AA`,
			"This token contains 1 line break (character 17). A token is one unbroken line, " +
				"so copy it again or remove it."},
		{"a zero-width space", `sk-ant-oat01-abc\u200bdef-AA`,
			"This token contains 1 invisible character (character 17)."},
		{"many", `sk-ant-oat01- a b c d e`,
			"This token contains 5 spaces (characters 14, 16, 18 and 2 more)."},
	} {
		code, body := r.do(t, http.MethodPut, "/llm/providers/default/credential",
			`{"values": {"CLAUDE_CODE_OAUTH_TOKEN": "`+tc.value+`"}}`, nil)
		segs, message := problemAt(t, body)
		if code != http.StatusUnprocessableEntity || segs != `["values","CLAUDE_CODE_OAUTH_TOKEN"]` ||
			!strings.HasPrefix(message, tc.want) {
			t.Errorf("%s = %d at %s: %q, want it to open %q", tc.name, code, segs, message, tc.want)
		}
	}
	code, body := r.do(t, http.MethodPut, "/llm/providers/sonnet/credential",
		`{"values": {"ANTHROPIC_API_KEY_SONNET": "sk-ant-api03-abc def"}}`, nil)
	if _, message := problemAt(t, body); code != http.StatusUnprocessableEntity ||
		!strings.HasPrefix(message, "This API key contains 1 space (character 17). An API key is one unbroken line") {
		t.Errorf("a key with a space = %d: %q", code, message)
	}
	if len(r.engine.checks) != 0 {
		t.Fatalf("a value with stray characters was tested %d times", len(r.engine.checks))
	}

	if code, body := r.do(t, http.MethodPut, "/llm/providers/default/credential?dry_run=true",
		`{"values": {"CLAUDE_CODE_OAUTH_TOKEN": "  sk-ant-oat01-abc-def-AA\n"}}`, nil); code != http.StatusOK {
		t.Fatalf("a token with whitespace around it = %d %v", code, body)
	}
	if got := r.engine.checks[0].values["CLAUDE_CODE_OAUTH_TOKEN"]; got != "sk-ant-oat01-abc-def-AA" {
		t.Errorf("the test was handed %q, want the token trimmed", got)
	}
}

// A REFUSED TEST SAYS WHAT THE HARNESS'S WORDS MEANT, never the harness's own advice:
// a token the harness never used is named as one the vendor never saw, a refused
// one with the command that makes a new one, and neither vouches for the
// stored credential, which may have expired or met its limit.
func TestARefusedTestSaysWhatItMeant(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	failed := func(meaning harnessprofile.AuthMeaning) llmapi.Check {
		return llmapi.Check{At: clock, Node: "node-a", Failure: "auth",
			Said: "Not logged in · Please run /login",
			Lines: []harness.Check{{Name: harness.CheckSmoke, Kind: "auth", Meaning: meaning,
				Detail: "failed: auth: Not logged in · Please run /login"}}}
	}
	for meaning, want := range map[harnessprofile.AuthMeaning]string{
		harnessprofile.AuthUnread: "Claude Code did not use this token, so the vendor never saw it. Copy the " +
			"whole token again as one unbroken line starting with sk-ant-oat01-. Nothing was stored, " +
			"and the model still uses its current token.",
		harnessprofile.AuthRefused: "The vendor refused this token: it is wrong, expired, or revoked. Make a " +
			"new one with claude setup-token and paste it here. Nothing was stored, and the model " +
			"still uses its current token.",
	} {
		r.engine.check = failed(meaning)
		code, body := r.do(t, http.MethodPut, "/llm/providers/default/credential",
			`{"values": {"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-abc-def-AA"}}`, nil)
		if _, message := problemAt(t, body); code != http.StatusUnprocessableEntity ||
			body["error"] != "credential_rejected" || message != want || body["detail"] != want {
			t.Errorf("%s = %d %v: %q", meaning, code, body["error"], message)
		}
		check, _ := body["check"].(map[string]any)
		lines, _ := check["lines"].([]any)
		if len(lines) != 1 || lines[0].(map[string]any)["meaning"] != string(meaning) {
			t.Errorf("%s: the check's line carries %v", meaning, lines)
		}
	}
	if len(r.secrets.rows) != 0 {
		t.Errorf("a refused token was stored: %v", r.secrets.rows)
	}
}
