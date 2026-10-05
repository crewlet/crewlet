package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
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
	"github.com/crewlet/crewlet/internal/providers/llm/anthropic/claudemodel"
	"github.com/crewlet/crewlet/internal/providers/llm/httpapi"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
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
	// raw is the body as it arrived, for the assertions that are about
	// BYTES — a replayed turn is held to what the API sent, and a decoded
	// map would hide every difference a vendor's history check can see.
	raw []byte
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
		raw:           raw,
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
		Model:   "claude-test",
		APIKeys: []string{"k1"},
		BaseURL: baseURL,
		Timeout: 5 * time.Second,
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
	out := slices.Collect(maps.Keys(m))
	return out
}

// --- construction ------------------------------------------------------

func TestNewRequiresAModel(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted a provider with no model")
	}
}

// AN EMPTY BAG READS NO VARIABLE. Which key an entry that names none runs on
// is a configuration rule (config.LLMProvider.Keys), decided where the secret
// store is in reach; a provider that read the process environment itself
// would also run an entry whose every reference resolved to nothing on the
// conventional key — another account's credential, with nothing saying so.
func TestAnEmptyBagSendsNoAmbientKey(t *testing.T) {
	api, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, okMessage("hi"))
	})
	t.Setenv("ANTHROPIC_API_KEY", "from-the-environment")
	p := newProvider(t, url, func(c *Config) {
		c.APIKeys = nil
	})
	if _, err := p.Complete(context.Background(), userTurn("hello")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := api.seen()[0].apiKey; got == "from-the-environment" {
		t.Fatalf("wire key = %q: the ambient variable reached the wire", got)
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
			t.Setenv("ANTHROPIC_API_KEY", tc.key)
			t.Setenv("ANTHROPIC_AUTH_TOKEN", tc.token)
			p := newProvider(t, url, func(c *Config) {
				c.APIKeys = []string{"configured"}
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

func TestASuppliedHTTPClientIsTheOneUsed(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("hi")) })
	// A POOL OF ITS OWN, not the nil-Transport default: an
	// &http.Client{} is http.DefaultTransport under another spelling,
	// and every httptest.Server.Close in this binary sweeps it — see
	// [github.com/crewlet/crewlet/internal/httpx/httpxtest]. This
	// package's own comment already records that flake biting it once.
	counter := &countingTransport{inner: httpxtest.Pool(t)}
	p := newProvider(t, url, func(c *Config) { c.HTTPClient = counter })
	if _, err := p.Complete(context.Background(), userTurn("hi")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if counter.count() != 1 {
		t.Fatalf("the supplied client saw %d calls, want 1", counter.count())
	}
	if _, err := p.Complete(context.Background(), userTurn("hi")); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	if counter.count() != 2 {
		t.Fatalf("the supplied client saw %d calls, want 2", counter.count())
	}
}

// THE CONNECTION IS REUSED, which needs a server that can see connections
// rather than only requests.
//
// This is the whole reason internal/httpx exists: on http.DefaultTransport's
// two idle connections per host the second call past the cap re-handshakes,
// and against a self-hosted HTTP/1.1 endpoint that is a full round trip on the
// hot path of every phase.
//
// It used to go on to assert that Close dropped the connection. That method is
// gone: the client here is on the engine's ONE shared transport, so what it
// closed was every other client's connections too — see httpapi.NewHTTPClient.
// The flake this test showed under load WAS that bug, one parallel test's
// Close dropping this one's warm connection.
func TestTheConnectionIsReusedAcrossCalls(t *testing.T) {
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

// Every pair is one the API documents (its errors page lists the status and
// the type together), so on a status response the type and the status agree
// and either would do. The rows WITHOUT a type are the fallback: a gateway's
// own HTML 503, or a type this build does not know, is still classified — by
// the status, as it always was. The case where the two DISAGREE is the
// stream's, below.
func TestStatusClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		typ    string // "" sends a body naming no type
		want   llm.ErrorKind
	}{
		{400, "invalid_request_error", llm.KindFatal},
		{401, "authentication_error", llm.KindAuth},
		{402, "billing_error", llm.KindRateLimit},
		{403, "permission_error", llm.KindAuth},
		{404, "not_found_error", llm.KindFatal},
		{413, "request_too_large", llm.KindFatal},
		{429, "rate_limit_error", llm.KindRateLimit},
		{500, "api_error", llm.KindServer},
		{504, "timeout_error", llm.KindTimeout},
		{529, "overloaded_error", llm.KindServer},

		{400, "", llm.KindFatal},
		{401, "", llm.KindAuth},
		{408, "", llm.KindTimeout},
		{429, "", llm.KindRateLimit},
		{503, "", llm.KindServer},
	} {
		t.Run(fmt.Sprint(tc.status, "/", tc.typ), func(t *testing.T) {
			t.Parallel()
			_, url := serve(t, func(w http.ResponseWriter, _ int) {
				body := apiError(tc.typ)
				if tc.typ == "" {
					body = `<html>bad gateway</html>`
				}
				writeJSON(w, tc.status, body)
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

// countBreakpoints counts every cache_control marker anywhere in a body.
func countBreakpoints(v any) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		for key, child := range x {
			if key == "cache_control" {
				n++
			}
			n += countBreakpoints(child)
		}
	case []any:
		for _, child := range x {
			n += countBreakpoints(child)
		}
	}
	return n
}

// A TOOL LOOP'S CONVERSATION IS CACHED, NOT ONLY ITS PREFIX. The system and
// tool breakpoints cache the static prefix and nothing after it, so without
// the request's own automatic breakpoint every round re-billed the whole
// history at the full input price. It is set only where a next round exists
// — a call offering tools — because a one-shot call's tail is written at a
// premium and never read. Three breakpoints, inside the API's cap of four,
// all on the default TTL: an automatic entry may not outlive a marker ahead
// of it, and the API refuses the request when it does.
func TestAToolLoopCachesItsConversation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		tools []llm.ToolDef
		want  int // breakpoints in the body
	}{
		{"a tool loop's round", []llm.ToolDef{{Name: "a"}, {Name: "b"}}, 3},
		{"a one-shot call", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
			p := newProvider(t, url, nil)
			_, err := p.Complete(context.Background(), llm.Request{Tools: tc.tools, Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: "frame"},
				{Role: llm.RoleUser, Content: "do it"},
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "a"}}},
				{Role: llm.RoleTool, ToolCallID: "c1", Name: "a", Content: "done"},
			}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			body := api.seen()[0].body
			top, present := body["cache_control"]
			if want := len(tc.tools) > 0; present != want {
				t.Fatalf("top-level cache_control present = %v, want %v", present, want)
			}
			if present {
				marker := top.(map[string]any)
				if marker["type"] != "ephemeral" {
					t.Fatalf("top-level cache_control = %v, want the automatic ephemeral breakpoint", marker)
				}
				if ttl, set := marker["ttl"]; set {
					t.Fatalf("top-level ttl = %v, want the default the other markers carry", ttl)
				}
			}
			if got := countBreakpoints(body); got != tc.want || got > 4 {
				t.Fatalf("%d breakpoints, want %d (the API allows four)", got, tc.want)
			}
		})
	}
}

// TOOLS ARE OFFERED, NEVER FORCED. With tools present the API's default
// choice is auto, so none is sent — and a forced `any` is a 400 on Opus 5.5,
// Sonnet 5.5, Fable 5.1 and Mythos 5.1, and on every Claude model while it is
// thinking, which the fallback chain does not retry.
func TestToolsAreOfferedWithNoToolChoice(t *testing.T) {
	t.Parallel()
	for _, tools := range [][]llm.ToolDef{nil, {{Name: "t"}}} {
		api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
		p := newProvider(t, url, nil)
		req := userTurn("hi")
		req.Tools = tools
		if _, err := p.Complete(context.Background(), req); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		body := api.seen()[0].body
		if got, present := body["tool_choice"]; present {
			t.Errorf("%d tools: tool_choice = %v, want the field absent", len(tools), got)
		}
		if _, present := body["tools"]; present != (len(tools) > 0) {
			t.Errorf("%d tools: tools present = %v", len(tools), present)
		}
	}
}

// A TURN THIS BACKEND DID NOT WRITE IS REBUILT FROM THE NEUTRAL VIEW, and
// rebuilt WITHOUT ITS THINKING. Here it is another backend's turn carrying
// signed blocks: another vendor's reasoning has no Anthropic signature, and a
// block rebuilt from the neutral view is not the block that was signed — its
// order and the text around it are the rebuild's — so sending it is the edit
// the vendor's history check rejects. A turn with no thinking is what any
// non-Claude turn looks like, and is always accepted.
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
			Origin:         llm.Origin{Provider: "openai", Model: "gpt-test"},
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
	if len(blocks) != 2 {
		t.Fatalf("assistant blocks = %v, want text then tool_use and no thinking", blocks)
	}
	if blocks[0].(map[string]any)["text"] != "working" {
		t.Fatalf("text block = %v", blocks[0])
	}
	use := blocks[1].(map[string]any)
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

// --- replay ------------------------------------------------------------

// sentBlocks is the content of message i of a request body, one block per
// element, as the bytes that crossed the wire.
func sentBlocks(t *testing.T, raw []byte, i int) []json.RawMessage {
	t.Helper()
	var body struct {
		Messages []struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if i >= len(body.Messages) {
		t.Fatalf("the request carries %d messages, want message %d", len(body.Messages), i)
	}
	if body.Messages[i].Role != "assistant" {
		t.Fatalf("message %d is %q, want the assistant turn", i, body.Messages[i].Role)
	}
	return body.Messages[i].Content
}

// encoded is a block in the one form every request body puts it in: the SDK
// encodes a body with Go's JSON encoder, which drops the whitespace BETWEEN
// tokens and writes <, > and & inside a string as \u escapes. Neither changes
// a token — the same keys in the same order, every number in the digits it was
// written in, every string the same string — and what the vendor compares is
// the tokens, so this is the strongest equality a request can be held to and
// still be sent through the SDK.
func encoded(t *testing.T, block []byte) string {
	t.Helper()
	var compact, escaped bytes.Buffer
	if err := json.Compact(&compact, block); err != nil {
		t.Fatalf("block %s is not JSON: %v", block, err)
	}
	json.HTMLEscape(&escaped, compact.Bytes())
	return escaped.String()
}

// replayedAsWritten fails unless the assistant turn at message i of a request
// is exactly the blocks the model wrote, in the order it wrote them.
func replayedAsWritten(t *testing.T, raw []byte, i int, written []string) {
	t.Helper()
	sent := sentBlocks(t, raw, i)
	if len(sent) != len(written) {
		t.Fatalf("replayed %d blocks, want the %d the model wrote:\n%s", len(sent), len(written), raw)
	}
	for j := range written {
		if got, want := encoded(t, sent[j]), encoded(t, []byte(written[j])); got != want {
			t.Errorf("block %d replayed as\n  %s\nwant it as the model wrote it\n  %s", j, got, want)
		}
	}
}

// interleaved is a turn the neutral view cannot rebuild: two thinking blocks
// with text between them, then a call. Every block is written the way a
// re-encoder would NOT write it — keys out of the SDK's order, a field this
// engine does not model (`citations`, `caller`), a number past 2^53, a 1.0, and
// markup a string escaper touches — so a replay that went through a decode and
// an encode anywhere fails on at least one of them.
var interleaved = []string{
	`{"signature":"sig-A","type":"thinking","thinking":"Weighing <it> & the rest.\n  Indented."}`,
	`{"type":"text","text":"  First, <this>.  ","citations":null}`,
	`{"type":"thinking","thinking":"Now the call.","signature":"sig-B"}`,
	`{"type":"tool_use","id":"toolu_1","name":"lookup",` +
		`"input":{"z":1.0,"id":12345678901234567890,"q":"a<b>"},"caller":{"type":"direct"}}`,
}

func messageOf(blocks []string, stop string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test",` +
		`"content":[` + strings.Join(blocks, ",") + `],"stop_reason":"` + stop + `",` +
		`"usage":{"input_tokens":10,"output_tokens":5}}`
}

// AN ASSISTANT TURN IS REPLAYED AS THE MODEL WROTE IT. Opus 5.5, Sonnet 5.5
// and Fable 5.1 bind each thinking block to everything before it, byte for
// byte, and an earlier turn that comes back different invalidates every block
// after it — a 400 the chain does not retry, on an account the vendor
// enforces. The rebuild this replaced put both thinking blocks first, joined
// the texts, re-encoded the call's input from a decoded map and dropped every
// field it did not model.
func TestAnAssistantTurnIsReplayedAsTheModelWroteIt(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, n int) {
		if n == 1 {
			writeJSON(w, 200, messageOf(interleaved, "tool_use"))
			return
		}
		writeJSON(w, 200, okMessage("done"))
	})
	p := newProvider(t, url, nil)
	out, err := p.Complete(context.Background(), userTurn("look it up"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// The neutral view is still the engine's to read.
	if len(out.ToolCalls) != 1 || len(out.ThinkingBlocks) != 2 || out.Content != "  First, <this>.  " {
		t.Fatalf("neutral view = %+v", out)
	}
	turn := out.Message()
	if turn.Origin != (llm.Origin{Provider: "anthropic", Model: "claude-test"}) {
		t.Fatalf("origin = %+v, want this backend and its model", turn.Origin)
	}

	if _, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "look it up"},
		turn,
		{Role: llm.RoleTool, ToolCallID: "toolu_1", Name: "lookup", Content: "found"},
	}}); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	replayedAsWritten(t, api.seen()[1].raw, 1, interleaved)
}

// A STREAMED TURN IS REPLAYED AS THE MODEL WROTE IT TOO. A streamed block
// arrives as a start and a run of deltas, and the copy kept for the replay is
// the FINISHED block: the thinking with its streamed signature, the call with
// the input exactly as its fragments spelled it.
func TestAStreamedTurnIsReplayedAsTheModelWroteIt(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, n int) {
		if n > 1 {
			writeJSON(w, 200, okMessage("done"))
			return
		}
		writeStream(w, streamOf(
			streamStart(),
			sseEvent{"content_block_start", `{"type":"content_block_start","index":0,` +
				`"content_block":{"type":"thinking","thinking":"","signature":""}}`},
			sseEvent{"content_block_delta", `{"type":"content_block_delta","index":0,` +
				`"delta":{"type":"thinking_delta","thinking":"Weighing it."}}`},
			sseEvent{"content_block_delta", `{"type":"content_block_delta","index":0,` +
				`"delta":{"type":"signature_delta","signature":"sig-1"}}`},
			sseEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			textBlock(1, "Hel", "lo"),
			sseEvent{"content_block_start", `{"type":"content_block_start","index":2,` +
				`"content_block":{"type":"thinking","thinking":"","signature":""}}`},
			sseEvent{"content_block_delta", `{"type":"content_block_delta","index":2,` +
				`"delta":{"type":"thinking_delta","thinking":"Then the call."}}`},
			sseEvent{"content_block_delta", `{"type":"content_block_delta","index":2,` +
				`"delta":{"type":"signature_delta","signature":"sig-2"}}`},
			sseEvent{"content_block_stop", `{"type":"content_block_stop","index":2}`},
			sseEvent{"content_block_start", `{"type":"content_block_start","index":3,` +
				`"content_block":{"type":"tool_use","id":"tu_1","name":"lookup","input":{}}}`},
			sseEvent{"content_block_delta", `{"type":"content_block_delta","index":3,` +
				`"delta":{"type":"input_json_delta","partial_json":"{\"id\": 1234567890"}}`},
			sseEvent{"content_block_delta", `{"type":"content_block_delta","index":3,` +
				`"delta":{"type":"input_json_delta","partial_json":"123456789, \"q\":\"x\"}"}}`},
			sseEvent{"content_block_stop", `{"type":"content_block_stop","index":3}`},
			streamEnd("tool_use"),
		)...)
	})
	p := newProvider(t, url, nil)
	var got []llm.Delta
	out, err := p.Complete(context.Background(), streamingTurn("hi", &got))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		out.Message(),
		{Role: llm.RoleTool, ToolCallID: "tu_1", Content: "found"},
	}}); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	replayedAsWritten(t, api.seen()[1].raw, 1, []string{
		`{"type":"thinking","thinking":"Weighing it.","signature":"sig-1"}`,
		`{"type":"text","text":"Hello"}`,
		`{"type":"thinking","thinking":"Then the call.","signature":"sig-2"}`,
		`{"type":"tool_use","id":"tu_1","name":"lookup","input":{"id": 1234567890123456789, "q":"x"}}`,
	})
}

// ANOTHER CLAUDE MODEL'S TURN IS REPLAYED WHOLE, thinking included. A chain's
// fallback member, or a parked run resumed after the entry's model changed,
// hands this model turns another one wrote; the vendor decides which model
// reads which block and drops the ones it cannot, unbilled and without failing
// the call. Stripping them here would remove blocks from the MIDDLE of the
// conversation's sequence — the one removal that fails every block after it
// once the conversation is back on the model that wrote them.
func TestAnotherClaudeModelsTurnIsReplayedWithItsThinking(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, n int) {
		if n == 1 {
			writeJSON(w, 200, messageOf(interleaved, "tool_use"))
			return
		}
		writeJSON(w, 200, okMessage("ok"))
	})
	// The turn is the primary's; the next round is the fallback's, over
	// the same tools and the same system prompt.
	primary := newProvider(t, url, func(c *Config) { c.Model = "claude-opus-5-5" })
	fallback := newProvider(t, url, func(c *Config) { c.Model = "claude-sonnet-5-5" })
	tools := []llm.ToolDef{{Name: "lookup", Description: "Look a thing up."}}
	out, err := primary.Complete(context.Background(), llm.Request{Tools: tools, Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "look it up"},
	}})
	if err != nil {
		t.Fatalf("primary Complete: %v", err)
	}
	if _, err := fallback.Complete(context.Background(), llm.Request{Tools: tools, Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "look it up"},
		out.Message(),
		{Role: llm.RoleTool, ToolCallID: "toolu_1", Content: "found"},
	}}); err != nil {
		t.Fatalf("fallback Complete: %v", err)
	}
	replayedAsWritten(t, api.seen()[1].raw, 1, interleaved)
}

// RAW IS REPLAYED ONLY BY THE BACKEND WHOSE FORMAT IT IS. A turn some other
// backend recorded blocks for, or one with blocks and no origin at all, is
// rebuilt from the neutral view — never sent as Anthropic content it is not.
func TestOnlyThisBackendsBlocksAreReplayed(t *testing.T) {
	t.Parallel()
	for name, origin := range map[string]llm.Origin{
		"another backend": {Provider: "openai", Model: "gpt-test"},
		"no origin":       {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
			p := newProvider(t, url, nil)
			if _, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
				{Role: llm.RoleUser, Content: "hi"},
				{
					Role: llm.RoleAssistant, Content: "working",
					ToolCalls: []llm.ToolCall{{ID: "c", Name: "t"}},
					Origin:    origin,
					Raw:       []json.RawMessage{json.RawMessage(`{"type":"foreign","x":1}`)},
				},
				{Role: llm.RoleTool, ToolCallID: "c", Content: "done"},
			}}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			var types []string
			for _, b := range sentBlocks(t, api.seen()[0].raw, 1) {
				var head struct{ Type string }
				_ = json.Unmarshal(b, &head)
				types = append(types, head.Type)
			}
			if !slices.Equal(types, []string{"text", "tool_use"}) {
				t.Fatalf("assistant blocks = %v, want the neutral rebuild", types)
			}
		})
	}
}

// Redacted thinking and a block type this engine has never heard of are both
// replayed exactly as they came: neither has anything the neutral view could
// rebuild them from, and leaving either out is an edit.
func TestOpaqueBlocksAreHandedBackAsTheyCame(t *testing.T) {
	t.Parallel()
	written := []string{
		`{"type":"redacted_thinking","data":"opaque"}`,
		`{"type":"from_a_later_api","anything":{"at":["all"]}}`,
		`{"type":"tool_use","id":"c","name":"t","input":{}}`,
	}
	api, url := serve(t, func(w http.ResponseWriter, n int) {
		if n == 1 {
			writeJSON(w, 200, messageOf(written, "tool_use"))
			return
		}
		writeJSON(w, 200, okMessage("ok"))
	})
	p := newProvider(t, url, nil)
	out, err := p.Complete(context.Background(), userTurn("hi"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		out.Message(),
		{Role: llm.RoleTool, ToolCallID: "c", Content: "done"},
	}}); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	replayedAsWritten(t, api.seen()[1].raw, 1, written)
}

// A TEXT BLOCK OF NOTHING BUT WHITESPACE IS LEFT OUT of a replayed turn. A
// model writes one — a newline between its thinking and its call — and the
// API refuses one on input; the vendor's history check ignores them by rule,
// so leaving one out is not an edit. A turn that held nothing else is dropped
// whole, as any turn with nothing in it is.
func TestWhitespaceTextIsLeftOutOfAReplayedTurn(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	blocks := func(b ...string) []json.RawMessage {
		out := make([]json.RawMessage, len(b))
		for i := range b {
			out[i] = json.RawMessage(b[i])
		}
		return out
	}
	thinking := `{"type":"thinking","thinking":"t","signature":"s"}`
	call := `{"type":"tool_use","id":"c","name":"t","input":{}}`
	origin := llm.Origin{Provider: "anthropic", Model: "claude-test"}
	// Written under this request's own (empty) system prompt and tools, so
	// the thinking is the request's to replay.
	bound := boundTo(t, "", nil)
	if _, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Origin: origin, Binding: bound, Raw: blocks(`{"type":"text","text":" \n\t"}`)},
		{Role: llm.RoleUser, Content: "go on"},
		{
			Role: llm.RoleAssistant, Origin: origin, Binding: bound, ToolCalls: []llm.ToolCall{{ID: "c", Name: "t"}},
			Raw: blocks(thinking, `{"type":"text","text":"\n\n"}`, call),
		},
		{Role: llm.RoleTool, ToolCallID: "c", Content: "done"},
	}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	raw := api.seen()[0].raw
	if n := len(api.seen()[0].body["messages"].([]any)); n != 3 {
		t.Fatalf("sent %d messages, want the whitespace-only turn dropped and the two user turns joined: %s", n, raw)
	}
	replayedAsWritten(t, raw, 1, []string{thinking, call})
}

// A replayed block that is not JSON is a parked conversation corrupted in
// storage. Refused naming the message, before the network, rather than left
// to fail inside the SDK's encoder with an error naming nothing.
func TestACorruptReplayedBlockIsRefusedBeforeTheCall(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{
			Role: llm.RoleAssistant, Origin: llm.Origin{Provider: "anthropic"},
			Raw: []json.RawMessage{json.RawMessage(`{"type":"thinking"`)},
		},
		{Role: llm.RoleUser, Content: "go on"},
	}})
	if got := llm.KindOf(err); got != llm.KindFatal {
		t.Fatalf("classified %s (%v), want fatal", got, err)
	}
	if !strings.Contains(err.Error(), "message 1") {
		t.Fatalf("error %v does not name the message", err)
	}
	if api.count() != 0 {
		t.Fatal("an unsendable request still reached the network")
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

// --- the request shape, per model --------------------------------------

// shapeFacts is what the API reference says one model accepts, written out
// HERE rather than read from claudemodel: the test holds the wire to the
// reference, so a table row edited away from it goes red as a body that
// changed rather than passing because both sides moved together.
type shapeFacts struct {
	name        string
	model       string
	claudeModel string
	adaptive    bool     // false: the budget generation
	efforts     []string // nil: takes no effort at all
	sampling    bool
	maxOutput   float64
}

var (
	everyLevel = []string{"low", "medium", "high", "xhigh", "max"}
	noXHigh    = []string{"low", "medium", "high", "max"}
)

var shapeModels = []shapeFacts{
	{"fable 5.1", "claude-fable-5-1", "", true, everyLevel, false, 128000},
	{"mythos 5.1", "claude-mythos-5-1", "", true, everyLevel, false, 128000},
	{"fable 5", "claude-fable-5", "", true, everyLevel, false, 128000},
	{"opus 5.5", "claude-opus-5-5", "", true, everyLevel, false, 128000},
	{"opus 5", "claude-opus-5", "", true, everyLevel, false, 128000},
	{"opus 4.8", "claude-opus-4-8", "", true, everyLevel, false, 128000},
	{"opus 4.7", "claude-opus-4-7", "", true, everyLevel, false, 128000},
	{"opus 4.6", "claude-opus-4-6", "", true, noXHigh, true, 128000},
	{"sonnet 5.5", "claude-sonnet-5-5", "", true, everyLevel, false, 128000},
	{"sonnet 5", "claude-sonnet-5", "", true, everyLevel, false, 128000},
	{"sonnet 4.6", "claude-sonnet-4-6", "", true, noXHigh, true, 128000},
	{"haiku 4.5", "claude-haiku-4-5", "", false, nil, true, 64000},
	{"haiku 4.5 snapshot", "claude-haiku-4-5-20251001", "", false, nil, true, 64000},
	// An id the table has never seen is a model released after it: the
	// current generation's shape, at the smallest current output cap.
	{"unknown id", "claude-opus-5-7", "", true, everyLevel, false, 64000},
	// The deployment spellings shape as the model they spell.
	{"bedrock", "anthropic.claude-opus-5-5", "", true, everyLevel, false, 128000},
	{"bedrock profile", "us.anthropic.claude-sonnet-4-5-20250929-v1:0", "", false, nil, true, 64000},
	{"vertex", "claude-opus-4-5@20251101", "", false, []string{"low", "medium", "high"}, true, 64000},
	// A gateway alias for an OLDER model, the one case Modern gets wrong,
	// named by claude_model.
	{"alias with claude_model", "gateway-fast", "claude-haiku-4-5", false, nil, true, 64000},
}

// wireShape is every field of a request body the model decides.
type wireShape struct {
	ThinkingType    string // "" when the field is absent
	ThinkingDisplay string
	Budget          float64
	Effort          string // "" when output_config is absent
	Temperature     *float64
	MaxTokens       float64
	ToolChoice      bool
}

func (w wireShape) String() string {
	temp := "absent"
	if w.Temperature != nil {
		temp = fmt.Sprint(*w.Temperature)
	}
	return fmt.Sprintf("thinking=%q display=%q budget=%v effort=%q temperature=%s max_tokens=%v tool_choice=%v",
		w.ThinkingType, w.ThinkingDisplay, w.Budget, w.Effort, temp, w.MaxTokens, w.ToolChoice)
}

func shapeOf(t *testing.T, body map[string]any) wireShape {
	t.Helper()
	var w wireShape
	if thinking, ok := body["thinking"].(map[string]any); ok {
		w.ThinkingType, _ = thinking["type"].(string)
		w.ThinkingDisplay, _ = thinking["display"].(string)
		w.Budget, _ = thinking["budget_tokens"].(float64)
	}
	if out, ok := body["output_config"].(map[string]any); ok {
		w.Effort, _ = out["effort"].(string)
		if w.Effort == "" {
			t.Fatalf("output_config sent with no effort: %v", out)
		}
	}
	if temp, ok := body["temperature"].(float64); ok {
		w.Temperature = &temp
	}
	w.MaxTokens, _ = body["max_tokens"].(float64)
	_, w.ToolChoice = body["tool_choice"]
	return w
}

// TestRequestShapeForEveryModel is the whole of what this backend decides per
// model, asserted on the WIRE for every id the reference names, an id it does
// not, the Bedrock and Vertex spellings and an alias that names its model —
// across the entry's effort and budget and the call's effort, temperature and
// output cap. Every expectation below is derived from shapeFacts alone:
//
//   - thinking is {adaptive, summarized} on every call to an adaptive model,
//     {enabled, budget_tokens} on a budget-era one only when the entry gives
//     a budget, and absent otherwise;
//   - effort is the entry's level (high when it names none) lowered to the
//     call's, and absent on a model that takes none;
//   - a temperature is sent only when the model samples, the call is not
//     thinking and the caller named one;
//   - max_tokens is the model's ceiling, and a caller's own cap only on a
//     call that is not thinking;
//   - tool_choice is never sent.
func TestRequestShapeForEveryModel(t *testing.T) {
	t.Parallel()
	type scenario struct {
		name    string
		effort  llm.Effort
		budget  int
		request func(llm.Request) llm.Request
	}
	scenarios := []scenario{
		{name: "defaults", request: func(r llm.Request) llm.Request { return r }},
		{name: "entry xhigh, call low", effort: llm.EffortXHigh,
			request: func(r llm.Request) llm.Request { r.Effort = llm.EffortLow; return r }},
		{name: "caller temperature 0 and cap 400",
			request: func(r llm.Request) llm.Request {
				r.Temperature, r.MaxTokens = llm.Temp(0), 400
				return r
			}},
		{name: "budget 2048 with temperature 0 and cap 400", budget: 2048,
			request: func(r llm.Request) llm.Request {
				r.Temperature, r.MaxTokens = llm.Temp(0), 400
				return r
			}},
	}
	for _, m := range shapeModels {
		for _, sc := range scenarios {
			t.Run(m.name+"/"+sc.name, func(t *testing.T) {
				t.Parallel()
				api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
				p, err := New(Config{
					Model: m.model, ClaudeModel: m.claudeModel, APIKeys: []string{"k"},
					BaseURL: url, Timeout: 5 * time.Second,
					Effort: sc.effort, ThinkingBudget: sc.budget,
				})

				// The entry settings the model refuses are refused at
				// construction, by name, rather than sent as a 400 on
				// every call.
				switch {
				case sc.effort != "" && !slices.Contains(m.efforts, string(sc.effort)):
					if err == nil || !strings.Contains(err.Error(), "Effort") {
						t.Fatalf("New with effort %s on %s = %v, want a refusal naming Effort",
							sc.effort, m.model, err)
					}
					return
				case sc.budget > 0 && m.adaptive:
					if !errors.Is(err, claudemodel.ErrTakesNoBudget) || !strings.Contains(err.Error(), "ThinkingBudget") {
						t.Fatalf("New with a budget on adaptive %s = %v, want ErrTakesNoBudget naming ThinkingBudget",
							m.model, err)
					}
					return
				case err != nil:
					t.Fatalf("New: %v", err)
				}

				req := sc.request(llm.Request{
					Messages: userTurn("hi").Messages,
					Tools:    []llm.ToolDef{{Name: "ok", Parameters: map[string]any{"type": "object"}}},
				})
				if _, err := p.Complete(context.Background(), req); err != nil {
					t.Fatalf("Complete: %v", err)
				}

				thinking := m.adaptive || sc.budget > 0
				want := wireShape{MaxTokens: m.maxOutput}
				switch {
				case m.adaptive:
					want.ThinkingType, want.ThinkingDisplay = "adaptive", "summarized"
				case sc.budget > 0:
					want.ThinkingType, want.Budget = "enabled", float64(sc.budget)
				}
				if m.efforts != nil {
					want.Effort = "high"
					if sc.effort != "" {
						want.Effort = string(sc.effort)
					}
					if req.Effort != "" {
						want.Effort = string(req.Effort) // every case lowers
					}
				}
				if req.Temperature != nil && m.sampling && !thinking {
					want.Temperature = req.Temperature
				}
				if req.MaxTokens > 0 && !thinking {
					want.MaxTokens = float64(req.MaxTokens)
				}

				got := shapeOf(t, api.seen()[0].body)
				if got.String() != want.String() {
					t.Fatalf("%s\n got  %s\n want %s", m.model, got, want)
				}
				if body := api.seen()[0].body; body["model"] != m.model {
					t.Fatalf("model sent as %v, want %q as configured", body["model"], m.model)
				}
			})
		}
	}
}

// A call's ceiling may fall on a level the model skips — `xhigh` on Opus 4.6,
// `max` on Opus 4.5 — and lowering it to the next level the model takes is
// the only reading that neither raises the effort nor sends a 400.
func TestAnEffortTheModelSkipsIsLoweredToOneItTakes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		model         string
		entry, call   llm.Effort
		wantOnTheWire string
	}{
		{"claude-opus-4-6", llm.EffortMax, llm.EffortXHigh, "high"},
		{"claude-opus-4-6", llm.EffortMax, "", "max"},
		{"claude-opus-4-5", llm.EffortHigh, llm.EffortMax, "high"},
		{"claude-opus-4-5", "", llm.EffortMedium, "medium"},
		// A ceiling ABOVE the entry never raises it.
		{"claude-opus-5-5", llm.EffortMedium, llm.EffortMax, "medium"},
	} {
		api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
		p := newProvider(t, url, func(c *Config) { c.Model, c.Effort = tc.model, tc.entry })
		req := userTurn("hi")
		req.Effort = tc.call
		if _, err := p.Complete(context.Background(), req); err != nil {
			t.Fatalf("%s: Complete: %v", tc.model, err)
		}
		if got := dig(t, api.seen()[0].body, "output_config", "effort"); got != tc.wantOnTheWire {
			t.Errorf("%s entry %q call %q: effort = %v, want %q",
				tc.model, tc.entry, tc.call, got, tc.wantOnTheWire)
		}
	}
}

// Every combination that would be a 400 on every call is refused when the
// provider is built, naming the field — the config tier refuses the same
// combinations for a model written literally, and this is what catches one
// written as a `${VAR}` that only resolved at build.
func TestNewRefusesASettingTheModelWouldAnswerWithA400(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		cfg   Config
		field string
		is    error
	}{
		{"claude_model not in the table",
			Config{Model: "gw", ClaudeModel: "claude-haiku-4"}, "ClaudeModel", claudemodel.ErrNotInTable},
		{"claude_model beside a model the table reads",
			Config{Model: "claude-opus-5-5", ClaudeModel: "claude-haiku-4-5"}, "ClaudeModel", claudemodel.ErrOverrideNotNeeded},
		{"effort on a model that takes none",
			Config{Model: "claude-haiku-4-5", Effort: llm.EffortLow}, "Effort", claudemodel.ErrTakesNoEffort},
		{"xhigh before Opus 4.7",
			Config{Model: "claude-sonnet-4-6", Effort: llm.EffortXHigh}, "Effort", claudemodel.ErrEffortLevel},
		{"an effort that is not a level",
			Config{Model: "claude-opus-5-5", Effort: "extreme"}, "Effort", claudemodel.ErrEffortLevel},
		{"a budget on an adaptive model",
			Config{Model: "claude-opus-5-5", ThinkingBudget: 4096}, "ThinkingBudget", claudemodel.ErrTakesNoBudget},
		{"a budget on an unknown id",
			Config{Model: "gw", ThinkingBudget: 4096}, "ThinkingBudget", claudemodel.ErrTakesNoBudget},
		{"a budget below the minimum",
			Config{Model: "claude-haiku-4-5", ThinkingBudget: 10}, "ThinkingBudget", claudemodel.ErrBudgetRange},
		{"a budget the output cap cannot hold",
			Config{Model: "claude-haiku-4-5", ThinkingBudget: 64000}, "ThinkingBudget", claudemodel.ErrBudgetRange},
		{"a negative budget",
			Config{Model: "claude-haiku-4-5", ThinkingBudget: -1}, "ThinkingBudget", claudemodel.ErrBudgetRange},
	} {
		_, err := New(tc.cfg)
		if !errors.Is(err, tc.is) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: New = %v, want %v naming %s", tc.name, err, tc.is, tc.field)
		}
	}
	// The controls: the same fields where the model takes them.
	for _, cfg := range []Config{
		{Model: "gw", ClaudeModel: "claude-haiku-4-5", ThinkingBudget: 1024},
		{Model: "claude-haiku-4-5", ThinkingBudget: 63999},
		{Model: "claude-opus-4-7", Effort: llm.EffortXHigh},
		{Model: "claude-opus-4-5", Effort: llm.EffortHigh, ThinkingBudget: 2048},
	} {
		if _, err := New(cfg); err != nil {
			t.Errorf("New(%+v) = %v, want it built", cfg, err)
		}
	}
}

func TestAnInvalidCallEffortIsRefusedBeforeTheCall(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	req := userTurn("hi")
	req.Effort = "extreme"
	_, err := p.Complete(context.Background(), req)
	if llm.KindOf(err) != llm.KindFatal || !strings.Contains(err.Error(), `"extreme"`) {
		t.Fatalf("Complete = %v, want a fatal error naming the level", err)
	}
	if api.count() != 0 {
		t.Fatal("a request with an invalid effort still reached the network")
	}
}

// NO PREFILL: a conversation ending on the assistant's turn is a 400 on every
// model from 4.6 on. Nothing in the engine sends one, and this keeps it so.
func TestAPrefillIsRefusedBeforeTheCall(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "start"},
		{Role: llm.RoleAssistant, Content: "Sure, here is"},
	}})
	if !errors.Is(err, ErrPrefill) || llm.KindOf(err) != llm.KindFatal {
		t.Fatalf("Complete = %v, want ErrPrefill", err)
	}
	if api.count() != 0 {
		t.Fatal("a prefill still reached the network")
	}
	// The control: the same assistant turn followed by the user's answer.
	if _, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "start"},
		{Role: llm.RoleAssistant, Content: "Sure, here is"},
		{Role: llm.RoleUser, Content: "go on"},
	}}); err != nil {
		t.Fatalf("a conversation ending on the user: %v", err)
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
	if out.StopReason != llm.StopToolUse {
		t.Fatalf("StopReason = %q, want tool_use", out.StopReason)
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

// A MISSING STOP REASON IS READ FROM THE RESPONSE, never invented as a
// truncation: a gateway that omits the field has not said the response was cut
// short, so a round with calls stopped for them and one without ended.
func TestAMissingStopReasonIsReadFromTheResponse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		content string
		want    llm.StopReason
	}{
		{"no calls", `[]`, llm.StopEnd},
		{"a call", `[{"type":"tool_use","id":"c","name":"run","input":{}}]`, llm.StopToolUse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, url := serve(t, func(w http.ResponseWriter, _ int) {
				writeJSON(w, 200, `{"id":"m","type":"message","role":"assistant",
					"model":"claude-test","content":`+tc.content+`,
					"usage":{"input_tokens":1,"output_tokens":1}}`)
			})
			out, err := newProvider(t, url, nil).Complete(context.Background(), userTurn("hi"))
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if out.StopReason != tc.want {
				t.Fatalf("StopReason = %q, want %q", out.StopReason, tc.want)
			}
		})
	}
}

// EVERY STOP REASON THE API DOCUMENTS IS MAPPED, onto the one vocabulary the
// tool loop decides on — and an unknown one is an ordinary end, because a word
// newer than this build is not a truncation this build can name.
func TestEveryStopReasonIsMapped(t *testing.T) {
	t.Parallel()
	for raw, want := range map[sdk.StopReason]llm.StopReason{
		sdk.StopReasonEndTurn:                    llm.StopEnd,
		sdk.StopReasonStopSequence:               llm.StopEnd,
		sdk.StopReasonToolUse:                    llm.StopToolUse,
		sdk.StopReasonMaxTokens:                  llm.StopMaxTokens,
		sdk.StopReasonRefusal:                    llm.StopRefusal,
		sdk.StopReasonModelContextWindowExceeded: llm.StopContextExceeded,
		sdk.StopReasonPauseTurn:                  llm.StopPaused,
		"a_reason_from_the_future":               llm.StopEnd,
	} {
		if got := stopReason(raw, false); got != want {
			t.Errorf("stopReason(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A REFUSAL IS AN ERROR, NOT AN ANSWER — and it is the API's answer, not a
// failure of the key. It arrives with 200, so the call succeeded: the key is
// not benched, the chain is not asked to try another model (KindRefusal is not
// retryable), and the refused completion rides the error so the frame that
// meters spend still sees what the call cost.
func TestARefusalIsAClassifiedErrorThatBenchesNoKey(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, `{"id":"m","type":"message","role":"assistant",
			"model":"claude-test","content":[{"type":"text","text":"I can"}],
			"stop_reason":"refusal",
			"stop_details":{"type":"refusal","category":"cyber","explanation":"exploit development"},
			"usage":{"input_tokens":40,"output_tokens":3}}`)
	})
	p := newProvider(t, url, func(c *Config) { c.APIKeys = []string{"k1", "k2"} })
	out, err := p.Complete(context.Background(), userTurn("hi"))
	if err == nil {
		t.Fatalf("Complete answered %+v — a refusal must not read as an answer", out)
	}
	if llm.KindOf(err) != llm.KindRefusal {
		t.Fatalf("kind = %s, want refusal", llm.KindOf(err))
	}
	var refusal *llm.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("no *llm.Refusal under %v", err)
	}
	if refusal.Category != "cyber" || refusal.Explanation != "exploit development" {
		t.Fatalf("refusal = %+v, want the stop_details", refusal)
	}
	if c := refusal.Completion; c == nil || c.InputTokens != 40 || c.OutputTokens != 3 ||
		c.StopReason != llm.StopRefusal || c.Model != "claude-test" {
		t.Fatalf("refused completion = %+v, want its usage, model and stop reason", refusal.Completion)
	}
	if n := api.count(); n != 1 {
		t.Fatalf("%d attempts, want 1 — a refusal is not rotated onto the next key", n)
	}
	for _, s := range p.Pool().Stats() {
		if s.Cooling != 0 {
			t.Fatalf("key %+v is cooling — a refusal benched a healthy key", s)
		}
	}
}

// A TOOL CALL THE OUTPUT CAP CUT OFF ARRIVES LOOKING WHOLE, and only the stop
// reason says otherwise. Streamed, the arguments arrive in fragments and a
// max_tokens stop leaves half an object behind — which the SDK's accumulator
// REPLACES with `{}` so the block marshals, so nothing in the call itself is
// left to notice. That is why the round's stop reason has to reach the tool
// loop: it is the one signal that this call was never finished, and running
// it is a write with no arguments the model never asked for.
func TestAToolCallCutByTheCapIsReportedByItsStopReason(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeStream(w, streamOf(
			streamStart(),
			sseEvent{"content_block_start", `{"type":"content_block_start","index":0,` +
				`"content_block":{"type":"tool_use","id":"tu_1","name":"post","input":{}}}`},
			sseEvent{"content_block_delta", `{"type":"content_block_delta","index":0,` +
				`"delta":{"type":"input_json_delta","partial_json":"{\"body\":\"Refunds are"}}`},
			sseEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			streamEnd("max_tokens"),
		)...)
	})
	var got []llm.Delta
	out, err := newProvider(t, url, nil).Complete(context.Background(), streamingTurn("hi", &got))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out.StopReason != llm.StopMaxTokens {
		t.Fatalf("StopReason = %q, want max_tokens", out.StopReason)
	}
	// The premise the stop reason is load-bearing for: the call itself
	// looks complete. If the SDK ever starts handing the fragment through,
	// DecodeArgs will name it instead and this premise check says so.
	if len(out.ToolCalls) != 1 || len(out.ToolCalls[0].Arguments) != 0 ||
		out.ToolCalls[0].ArgumentsError != "" {
		t.Fatalf("tool calls = %+v, want the cut call emptied to {} by the accumulator", out.ToolCalls)
	}
}

// A FAILED TOOL RESULT SAYS SO IN THE API'S OWN FIELD. The content carries the
// sentence; is_error is the structured flag beside it, and a result that did
// not fail must not carry it.
func TestAFailedToolResultIsSentAsAnError(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	req := llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "go"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "a", Name: "run", Arguments: map[string]any{}},
			{ID: "b", Name: "run", Arguments: map[string]any{}},
		}},
		{Role: llm.RoleTool, ToolCallID: "a", Name: "run", Content: "refused", Failed: true},
		{Role: llm.RoleTool, ToolCallID: "b", Name: "run", Content: "fine"},
	}}
	if _, err := newProvider(t, url, nil).Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	msgs := dig(t, api.seen()[0].body, "messages").([]any)
	results := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v, want both answers in one user turn", results)
	}
	if got := results[0].(map[string]any)["is_error"]; got != true {
		t.Errorf("failed result is_error = %v, want true", got)
	}
	if got := results[1].(map[string]any)["is_error"]; got == true {
		t.Errorf("successful result is_error = %v, want it absent or false", got)
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
	// The property is that the arguments can go back onto the wire, not
	// that they were discarded: decoded with UseNumber, a number no float64
	// holds survives EXACTLY and round-trips. Without that the map holds
	// +Inf, the assistant turn replaying this tool call fails to encode,
	// and every subsequent round of the turn fails with it.
	blob, err := json.Marshal(out.ToolCalls[0].Arguments)
	if err != nil {
		t.Fatalf("the surviving arguments cannot be re-serialised: %v", err)
	}
	if string(blob) != `{"n":1e1000}` {
		t.Fatalf("arguments round-tripped as %s, want the value unchanged", blob)
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
		wg.Go(func() {
			_, _ = p.Complete(context.Background(), userTurn("hi"))
		})
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

// A USER MESSAGE AFTER TOOL RESULTS — a person's note to a running turn, sent
// straight after the round's results (internal/agent/steer) — is ONE user
// turn: every tool_result first, in call order, then the note as text. The
// API requires a tool_use's results in the user turn that follows it and first
// within it; sending the note as a turn of its own leaves that to a merge the
// server performs as a courtesy.
func TestAUserMessageAfterToolResultsJoinsTheirTurnAfterThem(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("ok")) })
	p := newProvider(t, url, nil)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "do it"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "a", Name: "first"}, {ID: "b", Name: "second"},
		}},
		{Role: llm.RoleTool, ToolCallID: "a", Name: "first", Content: "one"},
		{Role: llm.RoleTool, ToolCallID: "b", Name: "second", Content: "two"},
		{Role: llm.RoleUser, Content: "a note from the founder"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	messages := api.seen()[0].body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("sent %d messages, want the opening, the calls, and ONE user turn "+
			"answering them: %v", len(messages), messages)
	}
	turn := messages[2].(map[string]any)
	blocks := turn["content"].([]any)
	if turn["role"] != "user" || len(blocks) != 3 {
		t.Fatalf("the answering turn is %v", turn)
	}
	for i, id := range []string{"a", "b"} {
		b := blocks[i].(map[string]any)
		if b["type"] != "tool_result" || b["tool_use_id"] != id {
			t.Errorf("block %d is %v, want the result for %s", i, b, id)
		}
	}
	if note := blocks[2].(map[string]any); note["type"] != "text" || note["text"] != "a note from the founder" {
		t.Errorf("the note is %v, want it last, as text", note)
	}
}

// --- streaming ---------------------------------------------------------

// sseEvent is one server-sent event of a Messages stream. The SDK dispatches
// on the `event:` line, so the name matters as much as the data.
type sseEvent struct{ name, data string }

// writeStream answers a request as a Messages stream, flushing each event so
// the client sees them as separate arrivals rather than one buffered body.
func writeStream(w http.ResponseWriter, events ...sseEvent) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, e := range events {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func streamStart() sseEvent {
	return sseEvent{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message",` +
		`"role":"assistant","model":"claude-test","content":[],"stop_reason":null,` +
		`"usage":{"input_tokens":10,"output_tokens":1}}}`}
}

// textBlock is a text content block at index, written in parts.
func textBlock(index int, parts ...string) []sseEvent {
	out := []sseEvent{{"content_block_start", fmt.Sprintf(
		`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, index)}}
	for _, part := range parts {
		out = append(out, sseEvent{"content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, index, part)})
	}
	return append(out, sseEvent{"content_block_stop", fmt.Sprintf(
		`{"type":"content_block_stop","index":%d}`, index)})
}

func streamEnd(stop string) []sseEvent {
	return []sseEvent{
		{"message_delta", fmt.Sprintf(
			`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":5}}`, stop)},
		{"message_stop", `{"type":"message_stop"}`},
	}
}

// streamError is the `error` event the API sends when a response that has
// already begun fails — after the 200, so the type in its body is the only
// thing that says what went wrong.
func streamError(kind string) sseEvent { return sseEvent{"error", apiError(kind)} }

// streamOf joins events and event lists in order.
func streamOf(parts ...any) []sseEvent {
	var out []sseEvent
	for _, part := range parts {
		switch v := part.(type) {
		case sseEvent:
			out = append(out, v)
		case []sseEvent:
			out = append(out, v...)
		}
	}
	return out
}

// streamingTurn is userTurn asking to be streamed, recording every delta.
func streamingTurn(text string, got *[]llm.Delta) llm.Request {
	req := userTurn(text)
	req.OnDelta = func(d llm.Delta) { *got = append(*got, d) }
	return req
}

// A STREAMED ROUND IS SHOWN AS IT IS WRITTEN AND ANSWERS AS A UNARY ONE DOES.
// Text and thinking are forwarded fragment by fragment; a half-written JSON
// argument and a signature are not (neither is readable); and the completion
// is the accumulated message — signature, tool arguments and usage included —
// because one interpretation of a response is all [Provider.completion] has.
func TestAStreamedCallForwardsFragmentsAndStillAnswers(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeStream(w, streamOf(
			streamStart(),
			sseEvent{"content_block_start", `{"type":"content_block_start","index":0,` +
				`"content_block":{"type":"thinking","thinking":"","signature":""}}`},
			sseEvent{"content_block_delta", `{"type":"content_block_delta","index":0,` +
				`"delta":{"type":"thinking_delta","thinking":"Weighing it."}}`},
			sseEvent{"content_block_delta", `{"type":"content_block_delta","index":0,` +
				`"delta":{"type":"signature_delta","signature":"sig-1"}}`},
			sseEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			textBlock(1, "Hel", "lo"),
			sseEvent{"content_block_start", `{"type":"content_block_start","index":2,` +
				`"content_block":{"type":"tool_use","id":"tu_1","name":"lookup","input":{}}}`},
			sseEvent{"content_block_delta", `{"type":"content_block_delta","index":2,` +
				`"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`},
			sseEvent{"content_block_delta", `{"type":"content_block_delta","index":2,` +
				`"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}`},
			sseEvent{"content_block_stop", `{"type":"content_block_stop","index":2}`},
			streamEnd("tool_use"),
		)...)
	})
	p := newProvider(t, url, nil)
	var got []llm.Delta
	out, err := p.Complete(context.Background(), streamingTurn("hi", &got))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if stream := api.seen()[0].body["stream"]; stream != true {
		t.Fatalf("stream = %v, want the request to ask for a stream", stream)
	}
	want := []llm.Delta{{Reasoning: "Weighing it."}, {Content: "Hel"}, {Content: "lo"}}
	if !slices.Equal(got, want) {
		t.Fatalf("deltas = %+v, want %+v — each readable fragment as it arrived, nothing else", got, want)
	}
	if out.Content != "Hello" || out.ReasoningContent != "Weighing it." {
		t.Fatalf("content %q / reasoning %q, want the assembled message", out.Content, out.ReasoningContent)
	}
	if len(out.ThinkingBlocks) != 1 || out.ThinkingBlocks[0].Signature != "sig-1" {
		t.Fatalf("thinking blocks = %+v, want the streamed signature kept for the next round", out.ThinkingBlocks)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].Arguments["q"] != "x" {
		t.Fatalf("tool calls = %+v, want the argument assembled from its fragments", out.ToolCalls)
	}
	if out.InputTokens != 10 || out.OutputTokens != 5 || out.StopReason != llm.StopToolUse {
		t.Fatalf("tokens %d/%d, stop %q", out.InputTokens, out.OutputTokens, out.StopReason)
	}
}

// AN ENDPOINT THAT ANSWERS A STREAMING REQUEST WITHOUT STREAMING STILL ANSWERS,
// on the same key, and is never asked to stream again. "Anthropic-compatible"
// has real variance — a local shim or a proxy may serve the unary route only —
// and failing a phase over that would regress every such deployment.
func TestAnEndpointThatCannotStreamStillAnswers(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okMessage("Hello")) })
	p := newProvider(t, url, nil)
	var got []llm.Delta
	req := streamingTurn("hi", &got)
	out, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out.Content != "Hello" {
		t.Fatalf("content = %q, want the unary answer", out.Content)
	}
	seen := api.seen()
	if len(seen) != 2 || seen[0].body["stream"] != true || seen[1].body["stream"] != nil {
		t.Fatalf("attempts = %d, want one streaming ask and one unary retry", len(seen))
	}
	if seen[1].apiKey != "k1" {
		t.Fatalf("the unary retry went out on %q, want the same key — a capability is not a key failure", seen[1].apiKey)
	}
	for _, s := range p.Pool().Stats() {
		if s.Cooling != 0 {
			t.Fatal("a missing capability benched the credential")
		}
	}

	// LATCHED: discovered once per process, never re-probed.
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	seen = api.seen()
	if len(seen) != 3 || seen[2].body["stream"] != nil {
		t.Fatalf("the second call made %d requests (stream %v), want one unary request",
			len(seen)-2, seen[len(seen)-1].body["stream"])
	}
}

// A ROTATION IS A RESTART, and a RATE LIMIT INSIDE A STREAM IS A RATE LIMIT.
// The first key streams half an answer and the API then ends the response
// with a rate_limit_error — after the 200, so only the body's type says so.
// Read by its status that is fatal and the call dies with half an answer;
// read by its type the key is benched, the next key answers, and the consumer
// is told the first half was abandoned rather than shown two halves as one.
func TestARateLimitInsideAStreamRotatesAndRestarts(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, n int) {
		if n == 1 {
			writeStream(w, streamOf(streamStart(),
				sseEvent{"content_block_start", `{"type":"content_block_start","index":0,` +
					`"content_block":{"type":"text","text":""}}`},
				sseEvent{"content_block_delta", `{"type":"content_block_delta","index":0,` +
					`"delta":{"type":"text_delta","text":"Hel"}}`},
				streamError("rate_limit_error"))...)
			return
		}
		writeStream(w, streamOf(streamStart(), textBlock(0, "Hello"), streamEnd("end_turn"))...)
	})
	p := newProvider(t, url, func(c *Config) { c.APIKeys = []string{"k1", "k2"} })
	var got []llm.Delta
	out, err := p.Complete(context.Background(), streamingTurn("hi", &got))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out.Content != "Hello" {
		t.Fatalf("content = %q, want the second key's whole answer, not the halves joined", out.Content)
	}
	want := []llm.Delta{{Content: "Hel"}, {Restart: true, Model: "claude-test"}, {Content: "Hello"}}
	if !slices.Equal(got, want) {
		t.Fatalf("deltas = %+v, want %+v", got, want)
	}
	if seen := api.seen(); len(seen) != 2 || seen[0].apiKey != "k1" || seen[1].apiKey != "k2" {
		t.Fatalf("attempts = %d, want k1 then k2", len(seen))
	}
	if stats := p.Pool().Stats(); stats[0].Cooling == 0 || stats[1].Cooling != 0 {
		t.Fatalf("pool = %+v, want only the rate-limited key benched", stats)
	}
}

// AN OVERLOAD INSIDE A STREAM IS THE SERVER'S, not a fatal refusal of the
// request: the chain may try its next model, and no key is benched for a
// capacity blip. Before the type was read this was a 200 and therefore fatal.
// And what streamed before the failure is NOT an answer — handing it back
// would give the loop a truncated round as though the model had finished.
func TestAnOverloadInsideAStreamIsAServerFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		typ  string
		want llm.ErrorKind
	}{
		{"overloaded_error", llm.KindServer},
		{"api_error", llm.KindServer},
		{"timeout_error", llm.KindTimeout},
		// A type this build does not know, on a response the API had
		// already accepted: it cannot be a request the API refused.
		{"some_future_error", llm.KindServer},
		{"invalid_request_error", llm.KindFatal},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			t.Parallel()
			_, url := serve(t, func(w http.ResponseWriter, _ int) {
				writeStream(w, streamOf(streamStart(),
					sseEvent{"content_block_start", `{"type":"content_block_start","index":0,` +
						`"content_block":{"type":"text","text":""}}`},
					sseEvent{"content_block_delta", `{"type":"content_block_delta","index":0,` +
						`"delta":{"type":"text_delta","text":"half an ans"}}`},
					streamError(tc.typ))...)
			})
			p := newProvider(t, url, nil)
			var got []llm.Delta
			out, err := p.Complete(context.Background(), streamingTurn("hi", &got))
			if out != nil {
				t.Fatalf("a stream that failed mid-body answered %q", out.Content)
			}
			var classified *llm.Error
			if !errors.As(err, &classified) {
				t.Fatalf("err = %v, want a classified failure", err)
			}
			if classified.Kind != tc.want {
				t.Fatalf("classified %s, want %s (status %d, err %v)", classified.Kind, tc.want, classified.Status, err)
			}
			for _, s := range p.Pool().Stats() {
				if s.Cooling != 0 {
					t.Fatal("a failure of the server benched the credential")
				}
			}
		})
	}
}

// writeSlowStream answers as a Messages stream, pausing before every event.
func writeSlowStream(w http.ResponseWriter, gap time.Duration, events ...sseEvent) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, e := range events {
		time.Sleep(gap)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// A STREAMED ROUND IS BOUNDED BY SILENCE, NOT LENGTH. This one runs several
// times the entry's timeout in total and is never silent for as long as it,
// which is what a round thinking at a high effort looks like — the per-attempt
// deadline used to cover the whole body and cut such a round off half-way.
func TestALongStreamedRoundIsNotCutOffByTheTimeout(t *testing.T) {
	t.Parallel()
	const timeout = 200 * time.Millisecond
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeSlowStream(w, timeout/4, streamOf(
			streamStart(), textBlock(0, "a", "b", "c", "d", "e", "f"), streamEnd("end_turn"))...)
	})
	p := newProvider(t, url, func(c *Config) { c.Timeout = timeout })
	var got []llm.Delta
	start := time.Now()
	out, err := p.Complete(context.Background(), streamingTurn("hi", &got))
	if err != nil {
		t.Fatalf("Complete after %v: %v", time.Since(start), err)
	}
	if elapsed := time.Since(start); elapsed < 2*timeout {
		t.Fatalf("the round took %v; it must outlast the %v timeout to prove anything", elapsed, timeout)
	}
	if out.Content != "abcdef" {
		t.Fatalf("content = %q, want the whole round", out.Content)
	}
}

// A STREAM THAT GOES SILENT IS ENDED AFTER THE TIMEOUT, as a TIMEOUT — before
// its first byte or part-way through — so the chain may try its next model
// and no key is benched. Lifting the total bound must not leave a dead
// connection holding a seat for ever.
func TestASilentStreamIsATimeout(t *testing.T) {
	t.Parallel()
	const timeout = 100 * time.Millisecond
	for _, tc := range []struct {
		name   string
		handle func(w http.ResponseWriter)
	}{
		{"before the first byte", func(w http.ResponseWriter) {
			time.Sleep(10 * timeout)
			writeStream(w, streamOf(streamStart(), textBlock(0, "late"), streamEnd("end_turn"))...)
		}},
		{"part-way through", func(w http.ResponseWriter) {
			writeStream(w, streamStart())
			time.Sleep(10 * timeout)
			writeStream(w, streamOf(textBlock(0, "late"), streamEnd("end_turn"))...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, url := serve(t, func(w http.ResponseWriter, _ int) { tc.handle(w) })
			p := newProvider(t, url, func(c *Config) { c.Timeout = timeout })
			var got []llm.Delta
			start := time.Now()
			out, err := p.Complete(context.Background(), streamingTurn("hi", &got))
			if out != nil {
				t.Fatalf("a silent stream answered %q after %v", out.Content, time.Since(start))
			}
			if !errors.Is(err, httpapi.ErrStalled) || llm.KindOf(err) != llm.KindTimeout {
				t.Fatalf("err = %v (kind %s), want a stall classified as a timeout", err, llm.KindOf(err))
			}
			if elapsed := time.Since(start); elapsed > 5*timeout {
				t.Fatalf("gave up after %v, want about the %v bound", elapsed, timeout)
			}
			for _, s := range p.Pool().Stats() {
				if s.Cooling != 0 {
					t.Fatal("a silent stream benched the credential")
				}
			}
		})
	}
}
