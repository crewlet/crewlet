package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/credential"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

func TestMain(m *testing.M) {
	logging.Configure(slog.LevelError, logging.FormatText, io.Discard)
	// The package logger bound its handler at package-var init, which
	// runs before this. Rebind it or every case prints its own log.
	log = logging.Get("llm.anthropic")
	os.Exit(m.Run())
}

// --- harness -----------------------------------------------------------

// attempt is one request the fake Anthropic saw.
type attempt struct {
	apiKey string
	// authorization is recorded because ANTHROPIC_AUTH_TOKEN reaches the
	// SDK through a DIFFERENT header than the pool's key does, and a bearer
	// token that arrived alongside a correct x-api-key would answer every
	// call while the pool rotated keys nothing was using.
	authorization string
	path          string
	body          map[string]any
}

// fakeAPI is an Anthropic endpoint that records what reached it. Everything
// in this file is asserted against the WIRE, because the translation from the
// neutral request is the part that can be wrong in a way no type checks.
type fakeAPI struct {
	mu       sync.Mutex
	attempts []attempt
	handle   func(w http.ResponseWriter, n int)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	f.mu.Lock()
	f.attempts = append(f.attempts, attempt{
		apiKey:        r.Header.Get("X-Api-Key"),
		authorization: r.Header.Get("Authorization"),
		path:          r.URL.Path,
		body:          body,
	})
	n := len(f.attempts)
	f.mu.Unlock()

	f.handle(w, n)
}

func (f *fakeAPI) seen() []attempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]attempt(nil), f.attempts...)
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.attempts)
}

func serve(t *testing.T, handle func(w http.ResponseWriter, n int)) (*fakeAPI, string) {
	t.Helper()
	api := &fakeAPI{handle: handle}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return api, srv.URL
}

// okMessage is a minimal successful response.
func okMessage(text string) string {
	return fmt.Sprintf(`{
		"id":"msg_1","type":"message","role":"assistant","model":"claude-test",
		"content":[{"type":"text","text":%q}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":10,"output_tokens":5}
	}`, text)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func apiError(kind string) string {
	return fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"nope"}}`, kind)
}

func newProvider(t *testing.T, baseURL string, mutate func(*Config)) *Provider {
	t.Helper()
	cfg := Config{
		Model:     "claude-test",
		APIKeys:   []string{"k1"},
		BaseURL:   baseURL,
		Timeout:   5 * time.Second,
		LookupEnv: func(string) string { return "" },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func userTurn(text string) llm.Request {
	return llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: text}}}
}

// dig walks a decoded JSON body.
func dig(t *testing.T, body map[string]any, path ...string) any {
	t.Helper()
	var current any = body
	for _, key := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not an object (%T)", path, key, current)
		}
		current, ok = obj[key]
		if !ok {
			t.Fatalf("path %v: no key %q (have %v)", path, key, keysOf(obj))
		}
	}
	return current
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- construction ------------------------------------------------------

func TestNewRequiresAModel(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted a provider with no model")
	}
}

func TestKeysFallBackToTheConventionalVariable(t *testing.T) {
	api, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, okMessage("hi"))
	})
	t.Setenv(KeyEnv, "from-the-environment")
	p := newProvider(t, url, func(c *Config) {
		c.APIKeys = nil
		c.LookupEnv = nil // exercise the real os.Getenv path
	})
	if _, err := p.Complete(context.Background(), userTurn("hello")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := api.seen()[0].apiKey; got != "from-the-environment" {
		t.Fatalf("wire key = %q, want the environment fallback", got)
	}
}

// The SDK loads ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN and ANTHROPIC_BASE_URL
// of its own accord. Every one of those is a credential source the pool does
// not know about — an ambient one would answer every call while the pool
// rotated keys nothing was using.
//
// BOTH rows matter, and only the second one pins the check that prevents this.
// The SDK's env autoload RETURNS EARLY at ANTHROPIC_API_KEY, so with that
// variable set the auth-token branch never runs and removing
// WithoutEnvironmentDefaults changes nothing observable — a mutation proved
// exactly that. It is the auth-token-only case that reaches the branch, and
// there the per-request WithAPIKey cannot save us: a bearer token rides a
// DIFFERENT header, so the request would carry a correct x-api-key and an
// ambient Authorization, and the server would honour the bearer.
func TestAmbientEnvironmentDoesNotShadowTheConfiguredKey(t *testing.T) {
	for _, tc := range []struct{ name, key, token string }{
		{"both ambient", "ambient", "ambient-token"},
		{"only an ambient auth token", "", "ambient-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, url := serve(t, func(w http.ResponseWriter, _ int) {
				writeJSON(w, 200, okMessage("hi"))
			})
			// An empty value reads as unset to the SDK's autoload, which
			// tests `ok && v != ""`.
			t.Setenv(KeyEnv, tc.key)
			t.Setenv("ANTHROPIC_AUTH_TOKEN", tc.token)
			p := newProvider(t, url, func(c *Config) {
				c.APIKeys = []string{"configured"}
				c.LookupEnv = nil
			})
			if _, err := p.Complete(context.Background(), userTurn("hello")); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			seen := api.seen()[0]
			if seen.apiKey != "configured" {
				t.Fatalf("wire key = %q, want the configured key", seen.apiKey)
			}
			if seen.authorization != "" {
				t.Fatalf("Authorization = %q, want no ambient credential on the wire",
					seen.authorization)
			}
		})
	}
}

// countingTransport is a caller-supplied HTTP client. It proves the field is
// wired at all — a Config field the constructor quietly ignores looks
// identical to one that works, right up to the deployment behind a proxy.
type countingTransport struct {
	mu    sync.Mutex
	calls int
	inner *http.Client
}

func (c *countingTransport) Do(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.Do(r)
}

func (c *countingTransport) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestASuppliedHTTPClientIsUsedAndNotClosed(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("hi")) })
	counter := &countingTransport{inner: &http.Client{}}
	p := newProvider(t, url, func(c *Config) { c.HTTPClient = counter })
	if _, err := p.Complete(context.Background(), userTurn("hi")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if counter.count() != 1 {
		t.Fatalf("the supplied client saw %d calls, want 1", counter.count())
	}
	// A transport the caller supplied may still be serving somebody else.
	p.Close()
	if _, err := p.Complete(context.Background(), userTurn("hi")); err != nil {
		t.Fatalf("Complete after Close: %v", err)
	}
	if counter.count() != 2 {
		t.Fatalf("the supplied client saw %d calls after Close, want 2", counter.count())
	}
}

func TestCloseOnAProviderOwnedTransportIsSafe(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("hi")) })
	p := newProvider(t, url, nil)
	if _, err := p.Complete(context.Background(), userTurn("hi")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	p.Close()
	p.Close() // idempotent: a config swap may close what shutdown closes again
	// Closing idle connections is not closing the client; the provider still
	// works, it just re-dials.
	if _, err := p.Complete(context.Background(), userTurn("hi")); err != nil {
		t.Fatalf("Complete after Close: %v", err)
	}
}

// Close's actual effect — dropping the idle connections this provider holds —
// needs a server that can see connections, not just requests. Without this the
// method could be an empty body and every other test would still pass.
func TestCloseDropsIdleConnections(t *testing.T) {
	t.Parallel()
	var conns atomic.Int64
	api := &fakeAPI{handle: func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("hi")) }}
	srv := httptest.NewUnstartedServer(api)
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	p := newProvider(t, srv.URL, nil)
	for range 2 {
		if _, err := p.Complete(context.Background(), userTurn("hi")); err != nil {
			t.Fatalf("Complete: %v", err)
		}
	}
	if got := conns.Load(); got != 1 {
		t.Fatalf("two sequential calls opened %d connections, want the second to reuse the first", got)
	}

	p.Close()
	if _, err := p.Complete(context.Background(), userTurn("hi")); err != nil {
		t.Fatalf("Complete after Close: %v", err)
	}
	if got := conns.Load(); got != 2 {
		t.Fatalf("opened %d connections after Close, want the idle one to have been dropped", got)
	}
}

// --- SDK retries -------------------------------------------------------

// The control for the test below: with the SDK's own default the counter DOES
// see retries, so a passing "exactly one attempt" is a real observation and
// not an instrument that cannot count.
func TestHarnessSeesSDKRetriesWhenTheyAreLeftOn(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 500, apiError("api_error"))
	})
	client := sdk.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(url),
		option.WithAPIKey("k"),
		// No WithMaxRetries: the SDK's documented default is two.
	)
	_, _ = client.Messages.New(context.Background(), sdk.MessageNewParams{
		Model:     "claude-test",
		MaxTokens: 16,
		Messages:  []sdk.MessageParam{sdk.NewUserMessage(sdk.NewTextBlock("hi"))},
	})
	if got := api.count(); got != 3 {
		t.Fatalf("SDK default made %d attempts, want 1 try plus 2 retries — "+
			"the retry-counting instrument is wrong", got)
	}
}

// Retrying inside a provider hides the one signal the layers above need. The
// SDK retries on exactly the statuses the pool cares about, and reports the
// last failure as a timeout.
func TestSDKRetriesAreDisabled(t *testing.T) {
	t.Parallel()
	for _, status := range []int{429, 500, 503, 408} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			api, url := serve(t, func(w http.ResponseWriter, _ int) {
				writeJSON(w, status, apiError("api_error"))
			})
			p := newProvider(t, url, nil)
			_, err := p.Complete(context.Background(), userTurn("hi"))
			if err == nil {
				t.Fatal("Complete succeeded against a failing endpoint")
			}
			if got := api.count(); got != 1 {
				t.Fatalf("status %d produced %d attempts, want exactly 1", status, got)
			}
		})
	}
}

// --- classification ----------------------------------------------------

func TestStatusClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		want   llm.ErrorKind
	}{
		{400, llm.KindFatal},
		{401, llm.KindAuth},
		{403, llm.KindAuth},
		{404, llm.KindFatal},
		{408, llm.KindTimeout},
		{429, llm.KindRateLimit},
		{500, llm.KindServer},
		{529, llm.KindServer},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			t.Parallel()
			_, url := serve(t, func(w http.ResponseWriter, _ int) {
				writeJSON(w, tc.status, apiError("api_error"))
			})
			p := newProvider(t, url, nil)
			_, err := p.Complete(context.Background(), userTurn("hi"))
			if got := llm.KindOf(err); got != tc.want {
				t.Fatalf("status %d classified %s, want %s (err: %v)", tc.status, got, tc.want, err)
			}
			var classified *llm.Error
			if !errors.As(err, &classified) {
				t.Fatalf("error is not an *llm.Error: %v", err)
			}
			if classified.Provider != providerName || classified.Model != "claude-test" {
				t.Fatalf("error names %s/%s", classified.Provider, classified.Model)
			}
		})
	}
}

func TestATimeoutIsATimeoutNotACredentialFailure(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, 200, okMessage("late"))
	})
	p := newProvider(t, url, func(c *Config) { c.Timeout = 20 * time.Millisecond })
	_, err := p.Complete(context.Background(), userTurn("hi"))
	if got := llm.KindOf(err); got != llm.KindTimeout {
		t.Fatalf("classified %s, want timeout (err: %v)", got, err)
	}
	for _, s := range p.Pool().Stats() {
		if s.Cooling != 0 {
			t.Fatal("a transport timeout benched the credential")
		}
	}
}

func TestCancellationIsNotAProviderFailure(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, 200, okMessage("late"))
	})
	p := newProvider(t, url, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := p.Complete(ctx, userTurn("hi"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to answer errors.Is(context.Canceled)", err)
	}
	if llm.KindOf(err).Retryable() {
		t.Fatal("a cancelled call was reported as worth retrying on another model")
	}
	for _, s := range p.Pool().Stats() {
		if s.Cooling != 0 {
			t.Fatal("a cancelled call benched the credential")
		}
	}
}

// --- credential rotation ----------------------------------------------

func TestRotatesToTheNextKeyWithinOneCall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"rate limit", 429},
		{"quota", 402},
		{"auth", 401},
		{"forbidden", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api, url := serve(t, func(w http.ResponseWriter, n int) {
				if n == 1 {
					writeJSON(w, tc.status, apiError("error"))
					return
				}
				writeJSON(w, 200, okMessage("from the second key"))
			})
			p := newProvider(t, url, func(c *Config) {
				c.APIKeys = []string{"k1", "k2", "k3"}
			})
			out, err := p.Complete(context.Background(), userTurn("hi"))
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if out.Content != "from the second key" {
				t.Fatalf("Content = %q", out.Content)
			}
			seen := api.seen()
			if len(seen) != 2 {
				t.Fatalf("made %d attempts, want to stop at the first live key", len(seen))
			}
			// The key actually changed ON THE WIRE. A pool that rotates
			// its bookkeeping while every request carries the same
			// credential is the failure this pins.
			if seen[0].apiKey != "k1" || seen[1].apiKey != "k2" {
				t.Fatalf("wire keys %q then %q, want k1 then k2", seen[0].apiKey, seen[1].apiKey)
			}
			stats := p.Pool().Stats()
			if stats[0].Cooling == 0 {
				t.Fatal("the refusing key was not benched")
			}
			if stats[1].Cooling != 0 || stats[2].Cooling != 0 {
				t.Fatal("a key that never failed was benched")
			}
		})
	}
}

func TestAFatalErrorDoesNotRotate(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 400, apiError("invalid_request_error"))
	})
	p := newProvider(t, url, func(c *Config) { c.APIKeys = []string{"k1", "k2"} })
	_, err := p.Complete(context.Background(), userTurn("hi"))
	if got := llm.KindOf(err); got != llm.KindFatal {
		t.Fatalf("classified %s, want fatal", got)
	}
	if api.count() != 1 {
		t.Fatalf("made %d attempts; a 400 is a 400 on every key", api.count())
	}
	for _, s := range p.Pool().Stats() {
		if s.Cooling != 0 {
			t.Fatal("a malformed request benched a credential")
		}
	}
}

func TestEveryKeyBenchedReportsAnExhaustedPool(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 429, apiError("rate_limit_error"))
	})
	p := newProvider(t, url, func(c *Config) { c.APIKeys = []string{"k1", "k2"} })
	_, err := p.Complete(context.Background(), userTurn("hi"))
	if api.count() != 2 {
		t.Fatalf("made %d attempts, want one per key", api.count())
	}
	if !errors.Is(err, credential.ErrExhausted) {
		t.Fatalf("err = %v, want it to answer errors.Is(ErrExhausted)", err)
	}
	// Retryable, so the seat's chain moves to another model rather than
	// failing the phase.
	if got := llm.KindOf(err); got != llm.KindRateLimit || !got.Retryable() {
		t.Fatalf("classified %s (retryable %v)", got, got.Retryable())
	}
}

func TestServerRetryHintShortensTheBench(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		header [2]string
		want   time.Duration
	}{
		{"retry-after seconds", [2]string{"Retry-After", "20"}, 20 * time.Second},
		{"anthropic reset", [2]string{"anthropic-ratelimit-requests-reset", ""}, 45 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			value := tc.header[1]
			if value == "" {
				value = time.Now().UTC().Add(45 * time.Second).Format(time.RFC3339)
			}
			_, url := serve(t, func(w http.ResponseWriter, _ int) {
				w.Header().Set(tc.header[0], value)
				writeJSON(w, 429, apiError("rate_limit_error"))
			})
			p := newProvider(t, url, func(c *Config) {
				c.Cooldowns = credential.Policy{RateLimit: time.Hour}
			})
			_, _ = p.Complete(context.Background(), userTurn("hi"))
			got := p.Pool().Stats()[0].Cooling
			// A second of slack: the RFC 3339 case is measured against
			// the wall clock the header was written on.
			if got > tc.want || got < tc.want-2*time.Second {
				t.Fatalf("bench = %v, want about %v (not the %v policy TTL)",
					got, tc.want, time.Hour)
			}
		})
	}
}

// --- request translation ----------------------------------------------

func TestSystemTurnsBecomeTheTopLevelParameterWithACacheBreakpoint(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "first"},
		{Role: llm.RoleUser, Content: "question"},
		{Role: llm.RoleSystem, Content: "second"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	body := api.seen()[0].body
	system, ok := body["system"].([]any)
	if !ok || len(system) != 1 {
		t.Fatalf("system = %v, want one block", body["system"])
	}
	block := system[0].(map[string]any)
	if block["text"] != "first\nsecond" {
		t.Fatalf("system text = %q, want both turns joined", block["text"])
	}
	if dig(t, block, "cache_control", "type") != "ephemeral" {
		t.Fatal("the system block carries no cache breakpoint")
	}
	messages := body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %v, want the system turns lifted out", messages)
	}
}

func TestToolsCarryTheirSchemaAndACacheBreakpointOnTheLast(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	req := userTurn("hi")
	req.Tools = []llm.ToolDef{
		{Name: "first", Description: "a", Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
			"required":   []any{"x"},
			// An extra keyword must survive: dropping it silently weakens
			// the contract the tool advertises.
			"additionalProperties": false,
		}},
		{Name: "second", Description: "b"},
	}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	tools := api.seen()[0].body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("sent %d tools", len(tools))
	}
	first := tools[0].(map[string]any)
	if first["name"] != "first" || first["description"] != "a" {
		t.Fatalf("first tool = %v", first)
	}
	schema := first["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v, want object", schema["type"])
	}
	if _, ok := dig(t, schema, "properties", "x").(map[string]any); !ok {
		t.Fatalf("properties lost: %v", schema["properties"])
	}
	if fmt.Sprint(schema["required"]) != "[x]" {
		t.Fatalf("required = %v", schema["required"])
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("an extra schema keyword was dropped: %v", schema)
	}
	if _, breakpoint := first["cache_control"]; breakpoint {
		t.Fatal("a cache breakpoint on a non-final tool")
	}

	second := tools[1].(map[string]any)
	if dig(t, second, "cache_control", "type") != "ephemeral" {
		t.Fatal("the final tool carries no cache breakpoint")
	}
	// A tool with no parameters still declares an object schema; several
	// endpoints reject a function whose schema has no declared type.
	if dig(t, second, "input_schema", "type") != "object" {
		t.Fatalf("empty schema = %v", second["input_schema"])
	}
}

func TestToolChoiceMapping(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		choice string
		want   string // "" means the field must be absent
	}{
		{"", "auto"},
		{"auto", "auto"},
		{"required", "any"}, // Anthropic spells it `any`
		{"none", "none"},
		{"nonsense", ""},
	} {
		t.Run("choice="+tc.choice, func(t *testing.T) {
			t.Parallel()
			api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
			p := newProvider(t, url, nil)
			req := userTurn("hi")
			req.Tools = []llm.ToolDef{{Name: "t"}}
			req.ToolChoice = tc.choice
			if _, err := p.Complete(context.Background(), req); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			body := api.seen()[0].body
			got, present := body["tool_choice"]
			if tc.want == "" {
				if present {
					t.Fatalf("tool_choice = %v, want the field absent", got)
				}
				return
			}
			if !present {
				t.Fatal("tool_choice missing")
			}
			if kind := got.(map[string]any)["type"]; kind != tc.want {
				t.Fatalf("tool_choice type = %v, want %v", kind, tc.want)
			}
		})
	}
}

func TestToolChoiceIsOmittedWithoutTools(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	req := userTurn("hi")
	req.ToolChoice = "required"
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, present := api.seen()[0].body["tool_choice"]; present {
		t.Fatal("tool_choice sent with no tools to choose from")
	}
}

func TestConversationTranslation(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "do it"},
		{
			Role:           llm.RoleAssistant,
			Content:        "working",
			ThinkingBlocks: []llm.ThinkingBlock{{Type: "thinking", Thinking: "hmm", Signature: "sig"}},
			ToolCalls:      []llm.ToolCall{{ID: "call_1", Name: "run", Arguments: map[string]any{"a": 1}}},
		},
		{Role: llm.RoleTool, ToolCallID: "call_1", Name: "run", Content: "done"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	messages := api.seen()[0].body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("sent %d messages, want 3", len(messages))
	}

	assistant := messages[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("role = %v", assistant["role"])
	}
	blocks := assistant["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("assistant blocks = %v, want thinking, text, tool_use", blocks)
	}
	// Thinking goes back FIRST and verbatim, signature included:
	// Anthropic validates it against the turn it belongs to.
	thinking := blocks[0].(map[string]any)
	if thinking["type"] != "thinking" || thinking["thinking"] != "hmm" || thinking["signature"] != "sig" {
		t.Fatalf("thinking block = %v", thinking)
	}
	if blocks[1].(map[string]any)["text"] != "working" {
		t.Fatalf("text block = %v", blocks[1])
	}
	use := blocks[2].(map[string]any)
	if use["type"] != "tool_use" || use["id"] != "call_1" || use["name"] != "run" {
		t.Fatalf("tool_use block = %v", use)
	}
	if fmt.Sprint(use["input"]) != "map[a:1]" {
		t.Fatalf("tool_use input = %v", use["input"])
	}

	// A tool result is a USER turn carrying a tool_result block.
	result := messages[2].(map[string]any)
	if result["role"] != "user" {
		t.Fatalf("tool result role = %v, want user", result["role"])
	}
	block := result["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_result" || block["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result block = %v", block)
	}
}

func TestRedactedThinkingIsHandedBackOpaquely(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{
			Role:           llm.RoleAssistant,
			ThinkingBlocks: []llm.ThinkingBlock{{Type: "redacted_thinking", Data: "opaque"}},
			ToolCalls:      []llm.ToolCall{{ID: "c", Name: "t"}},
		},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	messages := api.seen()[0].body["messages"].([]any)
	block := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if block["type"] != "redacted_thinking" || block["data"] != "opaque" {
		t.Fatalf("redacted block = %v", block)
	}
}

// Anthropic rejects an empty content block, and each of these is a shape the
// tool loop can genuinely produce.
func TestEmptyContentIsHandledRatherThanSent(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: ""},             // dropped
		{Role: llm.RoleUser, Content: "   "},               // dropped
		{Role: llm.RoleTool, ToolCallID: "c", Content: ""}, // substituted
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	messages := api.seen()[0].body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("sent %d messages, want the two empty turns dropped: %v", len(messages), messages)
	}
	// The SDK renders a tool_result's string content as a one-element text
	// block array, so the substitution is asserted where it actually lands.
	block := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	inner := block["content"].([]any)[0].(map[string]any)
	if inner["text"] != emptyToolResultContent {
		t.Fatalf("empty tool result rendered as %v, want %q", block["content"], emptyToolResultContent)
	}
}

// Arguments the caller handed back that cannot be JSON are a plumbing fault.
// The SDK would surface it from inside its encoder, naming no tool; refusing
// here names the offending one and costs no round trip.
func TestUnserialisableArgumentsAreRefusedBeforeTheCall(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "c", Name: "deliver", Arguments: map[string]any{"n": math.Inf(1)}},
		}},
	}})
	if got := llm.KindOf(err); got != llm.KindFatal {
		t.Fatalf("classified %s, want fatal", got)
	}
	if !strings.Contains(err.Error(), "deliver") {
		t.Fatalf("error %v does not name the offending tool", err)
	}
	if api.count() != 0 {
		t.Fatal("an unsendable request still reached the network")
	}
}

// Dropping every message would produce a 400 about a field. Refusing here
// names the actual problem, and costs no round trip.
func TestARequestWithNothingToSayIsRefusedBeforeTheCall(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "only a system prompt"},
	}})
	if got := llm.KindOf(err); got != llm.KindFatal {
		t.Fatalf("classified %s, want fatal", got)
	}
	if api.count() != 0 {
		t.Fatal("an unsendable request still reached the network")
	}
}

func TestReasoningSendsAThinkingBudgetAndPinsTemperature(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, func(c *Config) {
		c.Reasoning = true
		c.ThinkingBudget = 4096
		c.MaxTokens = 1000
	})
	if _, err := p.Complete(context.Background(), userTurn("hi")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	body := api.seen()[0].body
	if dig(t, body, "thinking", "type") != "enabled" {
		t.Fatalf("thinking = %v", body["thinking"])
	}
	if dig(t, body, "thinking", "budget_tokens") != float64(4096) {
		t.Fatalf("budget = %v", dig(t, body, "thinking", "budget_tokens"))
	}
	// Anthropic requires max_tokens strictly greater than the budget and
	// rejects any temperature but 1 while thinking.
	if body["max_tokens"] != float64(4096+1000) {
		t.Fatalf("max_tokens = %v, want the budget plus the cap", body["max_tokens"])
	}
	if body["temperature"] != float64(1) {
		t.Fatalf("temperature = %v, want 1", body["temperature"])
	}
}

func TestThinkingBudgetIsRaisedToTheVendorMinimum(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, func(c *Config) {
		c.Reasoning = true
		c.ThinkingBudget = 10 // below Anthropic's floor of 1024
	})
	if _, err := p.Complete(context.Background(), userTurn("hi")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := dig(t, api.seen()[0].body, "thinking", "budget_tokens"); got != float64(minThinkingBudget) {
		t.Fatalf("budget = %v, want it raised to %d", got, minThinkingBudget)
	}
}

func TestTemperatureAndMaxTokensDefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		configure       func(*Config)
		request         llm.Request
		wantTemperature float64
		wantMaxTokens   float64
	}{
		{
			// The tool loop sends neither field on any call it makes, so
			// "unset" is what the whole engine runs on: a nil temperature
			// must reach the provider's configured default, not 0.0.
			name: "request says nothing", request: userTurn("hi"),
			wantTemperature: DefaultTemperature, wantMaxTokens: DefaultMaxTokens,
		},
		{
			name:            "config overrides the default",
			configure:       func(c *Config) { c.Temperature = 0.2; c.MaxTokens = 512 },
			request:         userTurn("hi"),
			wantTemperature: 0.2, wantMaxTokens: 512,
		},
		{
			name:            "request overrides the config",
			request:         llm.Request{Messages: userTurn("hi").Messages, Temperature: llm.Temp(0.9), MaxTokens: 77},
			configure:       func(c *Config) { c.Temperature = 0.2; c.MaxTokens = 512 },
			wantTemperature: 0.9, wantMaxTokens: 77,
		},
		{
			// The whole reason Temperature is a pointer. A judge asking
			// for a reproducible answer says 0.0 and MUST get it; a
			// backend testing `> 0` silently substitutes its default and
			// the judge is non-deterministic with nothing to show for it.
			name:            "an explicit zero reaches the wire",
			request:         llm.Request{Messages: userTurn("hi").Messages, Temperature: llm.Temp(0)},
			configure:       func(c *Config) { c.Temperature = 0.2 },
			wantTemperature: 0, wantMaxTokens: DefaultMaxTokens,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
			p := newProvider(t, url, tc.configure)
			if _, err := p.Complete(context.Background(), tc.request); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			body := api.seen()[0].body
			if body["temperature"] != tc.wantTemperature {
				t.Fatalf("temperature = %v, want %v", body["temperature"], tc.wantTemperature)
			}
			if body["max_tokens"] != tc.wantMaxTokens {
				t.Fatalf("max_tokens = %v, want %v", body["max_tokens"], tc.wantMaxTokens)
			}
		})
	}
}

// --- response translation ---------------------------------------------

func TestResponseTranslation(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, `{
			"id":"msg_1","type":"message","role":"assistant","model":"claude-test",
			"content":[
				{"type":"thinking","thinking":"first thought","signature":"sig1"},
				{"type":"redacted_thinking","data":"opaque"},
				{"type":"text","text":"hello "},
				{"type":"text","text":"world"},
				{"type":"tool_use","id":"call_1","name":"run","input":{"path":"/tmp"}}
			],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":10,"output_tokens":5,
			         "cache_read_input_tokens":100,"cache_creation_input_tokens":7}
		}`)
	})
	p := newProvider(t, url, nil)
	out, err := p.Complete(context.Background(), userTurn("hi"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out.Content != "hello world" {
		t.Fatalf("Content = %q, want the text blocks joined", out.Content)
	}
	if out.ReasoningContent != "first thought" {
		t.Fatalf("ReasoningContent = %q", out.ReasoningContent)
	}
	if len(out.ThinkingBlocks) != 2 {
		t.Fatalf("ThinkingBlocks = %v, want the redacted one carried too", out.ThinkingBlocks)
	}
	if out.ThinkingBlocks[0].Signature != "sig1" || out.ThinkingBlocks[1].Data != "opaque" {
		t.Fatalf("ThinkingBlocks = %+v", out.ThinkingBlocks)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].ID != "call_1" ||
		out.ToolCalls[0].Arguments["path"] != "/tmp" {
		t.Fatalf("ToolCalls = %+v", out.ToolCalls)
	}
	if out.FinishReason != "tool_use" {
		t.Fatalf("FinishReason = %q", out.FinishReason)
	}
	// The per-model token breakdown is built from completions, so every
	// answer has to name the model that produced it. An empty one files the
	// call's tokens under no model at all.
	if out.Model != "claude-test" {
		t.Fatalf("Model = %q, want the configured model id", out.Model)
	}
}

// The CONFIGURED id, not the one the response echoes: a vendor alias resolving
// to a dated snapshot would re-key the breakdown the day the alias moves,
// splitting one model's spend across two names the config never mentions.
func TestTheCompletionNamesTheConfiguredModelNotTheEcho(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, `{"id":"m","type":"message","role":"assistant",
			"model":"claude-test-20990101","content":[{"type":"text","text":"x"}],
			"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	p := newProvider(t, url, nil)
	out, err := p.Complete(context.Background(), userTurn("hi"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out.Model != "claude-test" {
		t.Fatalf("Model = %q, want the configured id", out.Model)
	}
}

// input_tokens counts only the UNCACHED remainder. The vendor's own field
// doc: "Total input tokens in a request is the summation of input_tokens,
// cache_creation_input_tokens and cache_read_input_tokens." Getting this
// wrong under-bills every cached round, which is most of them.
func TestInputTokensSumTheCacheComponents(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                   string
		base, read, write, out int
		wantInput, wantTotal   int
	}{
		{"no cache", 10, 0, 0, 5, 10, 15},
		{"cache read", 10, 100, 0, 5, 110, 115},
		{"cache write", 10, 0, 7, 5, 17, 22},
		{"both", 10, 100, 7, 5, 117, 122},
		{"everything cached", 0, 900, 0, 5, 900, 905},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, url := serve(t, func(w http.ResponseWriter, _ int) {
				writeJSON(w, 200, fmt.Sprintf(`{
					"id":"m","type":"message","role":"assistant","model":"claude-test",
					"content":[{"type":"text","text":"x"}],"stop_reason":"end_turn",
					"usage":{"input_tokens":%d,"output_tokens":%d,
					         "cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}
				}`, tc.base, tc.out, tc.read, tc.write))
			})
			p := newProvider(t, url, nil)
			got, err := p.Complete(context.Background(), userTurn("hi"))
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got.InputTokens != tc.wantInput {
				t.Fatalf("InputTokens = %d, want %d (base %d + read %d + write %d)",
					got.InputTokens, tc.wantInput, tc.base, tc.read, tc.write)
			}
			if got.OutputTokens != tc.out {
				t.Fatalf("OutputTokens = %d, want %d", got.OutputTokens, tc.out)
			}
			// The breakdown is for cost reporting; adding it to
			// InputTokens again would double-count.
			if got.CacheRead != tc.read || got.CacheWrite != tc.write {
				t.Fatalf("cache breakdown = %d/%d, want %d/%d",
					got.CacheRead, got.CacheWrite, tc.read, tc.write)
			}
			if got.TotalTokens() != tc.wantTotal {
				t.Fatalf("TotalTokens = %d, want %d", got.TotalTokens(), tc.wantTotal)
			}
		})
	}
}

func TestAMissingStopReasonGetsADefault(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, `{"id":"m","type":"message","role":"assistant",
			"model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	p := newProvider(t, url, nil)
	out, err := p.Complete(context.Background(), userTurn("hi"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out.FinishReason != "end_turn" {
		t.Fatalf("FinishReason = %q, want the default", out.FinishReason)
	}
}

// A model can emit a number no float64 holds. encoding/json half-decodes it,
// and keeping the half would put a value in the conversation that cannot be
// serialised on the NEXT round.
func TestUnparseableToolArgumentsDoNotPoisonTheConversation(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, `{"id":"m","type":"message","role":"assistant",
			"model":"claude-test","stop_reason":"tool_use",
			"content":[{"type":"tool_use","id":"c","name":"run","input":{"n":1e1000}}],
			"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	p := newProvider(t, url, nil)
	out, err := p.Complete(context.Background(), userTurn("hi"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %v", out.ToolCalls)
	}
	if len(out.ToolCalls[0].Arguments) != 0 {
		t.Fatalf("Arguments = %v, want the half-decode discarded", out.ToolCalls[0].Arguments)
	}
	if _, err := json.Marshal(out.ToolCalls[0].Arguments); err != nil {
		t.Fatalf("the surviving arguments cannot be re-serialised: %v", err)
	}
}

func TestConcurrentCompletesShareOnePoolSafely(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, n int) {
		if n%4 == 0 {
			writeJSON(w, 429, apiError("rate_limit_error"))
			return
		}
		writeJSON(w, 200, okMessage("ok"))
	})
	p := newProvider(t, url, func(c *Config) {
		c.APIKeys = []string{"k1", "k2", "k3"}
		c.Cooldowns = credential.Policy{RateLimit: time.Millisecond, Auth: time.Millisecond}
	})
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Complete(context.Background(), userTurn("hi"))
		}()
	}
	wg.Wait()
	for _, s := range p.Pool().Stats() {
		if s.InFlight != 0 {
			t.Fatalf("key %s left %d leases in flight", s.Hint, s.InFlight)
		}
	}
}

func TestModelAndStringIdentity(t *testing.T) {
	t.Parallel()
	p := newProvider(t, "https://example.invalid", nil)
	if p.Model() != "claude-test" {
		t.Fatalf("Model() = %q", p.Model())
	}
	if !strings.Contains(p.String(), "anthropic/claude-test") {
		t.Fatalf("String() = %q", p.String())
	}
}
