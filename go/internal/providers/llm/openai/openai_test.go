package openai

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

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/credential"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

func TestMain(m *testing.M) {
	logging.Configure(slog.LevelError, logging.FormatText, io.Discard)
	// The package logger bound its handler at package-var init, which
	// runs before this. Rebind it or every case prints its own log.
	log = logging.Get("llm.openai")
	os.Exit(m.Run())
}

// --- harness -----------------------------------------------------------

type attempt struct {
	authorization string
	body          map[string]any
}

// fakeAPI records what actually reached the endpoint. The translation from
// the neutral request is the part that can be wrong in a way no type checks,
// so it is asserted against the wire.
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
		authorization: r.Header.Get("Authorization"),
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

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func okCompletion(text string) string {
	return fmt.Sprintf(`{
		"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-test",
		"choices":[{"index":0,"finish_reason":"stop",
			"message":{"role":"assistant","content":%q}}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
	}`, text)
}

func apiError(kind string) string {
	return fmt.Sprintf(`{"error":{"message":"nope","type":%q,"code":null,"param":null}}`, kind)
}

func newProvider(t *testing.T, baseURL string, mutate func(*Config)) *Provider {
	t.Helper()
	cfg := Config{
		Model:     "gpt-test",
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
			t.Fatalf("path %v: no key %q", path, key)
		}
	}
	return current
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
		writeJSON(w, 200, okCompletion("hi"))
	})
	t.Setenv(KeyEnv, "from-the-environment")
	p := newProvider(t, url, func(c *Config) {
		c.APIKeys = nil
		c.LookupEnv = nil // exercise the real os.Getenv path
	})
	if _, err := p.Complete(context.Background(), userTurn("hello")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := api.seen()[0].authorization; got != "Bearer from-the-environment" {
		t.Fatalf("Authorization = %q, want the environment fallback", got)
	}
}

// The SDK loads OPENAI_API_KEY at construction and there is no exported way
// to switch that off, so the per-request key MUST win — otherwise a pool that
// rotates its bookkeeping sends the same ambient credential every time.
func TestPerRequestKeyBeatsTheAmbientEnvironment(t *testing.T) {
	api, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, okCompletion("hi"))
	})
	t.Setenv(KeyEnv, "ambient")
	p := newProvider(t, url, func(c *Config) {
		c.APIKeys = []string{"configured"}
		c.LookupEnv = nil
	})
	if _, err := p.Complete(context.Background(), userTurn("hello")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := api.seen()[0].authorization; got != "Bearer configured" {
		t.Fatalf("Authorization = %q, want the configured key", got)
	}
}

// OPENAI_BASE_URL is read by the SDK too. An ambient one silently redirects a
// company's traffic, so the configured endpoint is always sent explicitly.
func TestAmbientBaseURLDoesNotRedirectTraffic(t *testing.T) {
	api, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, okCompletion("hi"))
	})
	_, elsewhere := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 500, apiError("api_error"))
	})
	t.Setenv("OPENAI_BASE_URL", elsewhere)
	p := newProvider(t, url, nil)
	if _, err := p.Complete(context.Background(), userTurn("hello")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if api.count() != 1 {
		t.Fatal("the call did not reach the configured endpoint")
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
	_, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("hi")) })
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
	_, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("hi")) })
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
	api := &fakeAPI{handle: func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("hi")) }}
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

// The control: with the SDK's own default the counter DOES see retries, so a
// passing "exactly one attempt" below is a real observation rather than an
// instrument that cannot count.
func TestHarnessSeesSDKRetriesWhenTheyAreLeftOn(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 500, apiError("api_error"))
	})
	client := sdk.NewClient(option.WithBaseURL(url), option.WithAPIKey("k"))
	_, _ = client.Chat.Completions.New(context.Background(), sdk.ChatCompletionNewParams{
		Model:    "gpt-test",
		Messages: []sdk.ChatCompletionMessageParamUnion{sdk.UserMessage("hi")},
	})
	if got := api.count(); got != 3 {
		t.Fatalf("SDK default made %d attempts, want 1 try plus 2 retries — "+
			"the retry-counting instrument is wrong", got)
	}
}

func TestSDKRetriesAreDisabled(t *testing.T) {
	t.Parallel()
	for _, status := range []int{429, 500, 503, 408} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			api, url := serve(t, func(w http.ResponseWriter, _ int) {
				writeJSON(w, status, apiError("api_error"))
			})
			p := newProvider(t, url, nil)
			if _, err := p.Complete(context.Background(), userTurn("hi")); err == nil {
				t.Fatal("Complete succeeded against a failing endpoint")
			}
			if got := api.count(); got != 1 {
				t.Fatalf("status %d produced %d attempts, want exactly 1", status, got)
			}
		})
	}
}

// --- classification and rotation --------------------------------------

func TestStatusClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		want   llm.ErrorKind
	}{
		{400, llm.KindFatal},
		{401, llm.KindAuth},
		{402, llm.KindRateLimit},
		{403, llm.KindAuth},
		{404, llm.KindFatal},
		{408, llm.KindTimeout},
		{429, llm.KindRateLimit},
		{500, llm.KindServer},
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
		})
	}
}

func TestRotatesToTheNextKeyWithinOneCall(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, n int) {
		if n == 1 {
			w.Header().Set("x-ratelimit-reset-requests", "6m0s")
			writeJSON(w, 429, apiError("rate_limit_exceeded"))
			return
		}
		writeJSON(w, 200, okCompletion("from the second key"))
	})
	p := newProvider(t, url, func(c *Config) {
		c.APIKeys = []string{"k1", "k2"}
		c.Cooldowns = credential.Policy{RateLimit: time.Hour}
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
		t.Fatalf("made %d attempts", len(seen))
	}
	if seen[0].authorization != "Bearer k1" || seen[1].authorization != "Bearer k2" {
		t.Fatalf("wire keys %q then %q", seen[0].authorization, seen[1].authorization)
	}
	// The reset header shortened the bench from the hour-long policy TTL.
	// Python's rstrip("s") could not read "6m0s" at all.
	// Slack for the real clock ticking between the bench and the read.
	if got := p.Pool().Stats()[0].Cooling; got > 6*time.Minute || got < 6*time.Minute-time.Second {
		t.Fatalf("bench = %v, want the header's six minutes (not the %v policy TTL)",
			got, time.Hour)
	}
}

func TestEveryKeyBenchedReportsAnExhaustedPool(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 401, apiError("invalid_api_key"))
	})
	p := newProvider(t, url, func(c *Config) { c.APIKeys = []string{"k1", "k2"} })
	_, err := p.Complete(context.Background(), userTurn("hi"))
	if api.count() != 2 {
		t.Fatalf("made %d attempts, want one per key", api.count())
	}
	if !errors.Is(err, credential.ErrExhausted) {
		t.Fatalf("err = %v, want it to answer errors.Is(ErrExhausted)", err)
	}
	if got := llm.KindOf(err); got != llm.KindAuth {
		t.Fatalf("classified %s, want the kind that did the benching", got)
	}
}

// --- request translation ----------------------------------------------

func TestConversationTranslation(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("ok")) })
	p := newProvider(t, url, nil)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "be brief"},
		{Role: llm.RoleUser, Content: "do it", Name: "asker"},
		{
			Role:    llm.RoleAssistant,
			Content: "working",
			// Reasoning must NOT go back: this endpoint has no field for
			// it, and the vendors that emit it reject it on input.
			ReasoningContent: "secret thoughts",
			ThinkingBlocks:   []llm.ThinkingBlock{{Type: "thinking", Thinking: "hmm"}},
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "run", Arguments: map[string]any{"a": 1}},
			},
		},
		{Role: llm.RoleTool, ToolCallID: "call_1", Name: "run", Content: "done"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	messages := api.seen()[0].body["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("sent %d messages, want 4", len(messages))
	}

	system := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "be brief" {
		t.Fatalf("system = %v", system)
	}
	user := messages[1].(map[string]any)
	if user["role"] != "user" || user["name"] != "asker" {
		t.Fatalf("user = %v", user)
	}

	assistant := messages[2].(map[string]any)
	if assistant["content"] != "working" {
		t.Fatalf("assistant content = %v", assistant["content"])
	}
	for _, banned := range []string{"reasoning_content", "reasoning", "thinking_blocks"} {
		if _, present := assistant[banned]; present {
			t.Fatalf("assistant turn carried %q back to an endpoint with no such field", banned)
		}
	}
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" {
		t.Fatalf("tool call = %v", call)
	}
	if dig(t, call, "function", "name") != "run" {
		t.Fatalf("tool call function = %v", call["function"])
	}
	// Arguments travel as a JSON *string* on this wire format.
	if dig(t, call, "function", "arguments") != `{"a":1}` {
		t.Fatalf("arguments = %v", dig(t, call, "function", "arguments"))
	}

	tool := messages[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "done" {
		t.Fatalf("tool message = %v", tool)
	}
}

// json.Marshal spells a nil map "null", and an assistant turn replaying
// `"arguments": "null"` is a message the model wrote turning into one it did
// not.
func TestToolCallWithNoArgumentsSendsAnEmptyObject(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("ok")) })
	p := newProvider(t, url, nil)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c", Name: "ping"}}},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	call := api.seen()[0].body["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0]
	if got := dig(t, call.(map[string]any), "function", "arguments"); got != "{}" {
		t.Fatalf("arguments = %q, want an empty object", got)
	}
}

// Arguments the caller handed back that cannot be JSON are a plumbing fault.
// Swapping them for empty arguments would rewrite the conversation and blame
// the model, so the call is refused before it reaches the network.
func TestUnserialisableArgumentsAreRefusedBeforeTheCall(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("ok")) })
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

func TestToolsAndToolChoice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		choice string
		want   any // nil means the field must be absent
	}{
		{"", "auto"},
		{"auto", "auto"},
		{"required", "required"},
		{"none", "none"},
		{"nonsense", nil},
	} {
		t.Run("choice="+tc.choice, func(t *testing.T) {
			t.Parallel()
			api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("ok")) })
			p := newProvider(t, url, nil)
			req := userTurn("hi")
			req.ToolChoice = tc.choice
			req.Tools = []llm.ToolDef{
				{Name: "with_schema", Description: "d", Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"x": map[string]any{"type": "string"}},
					"required":   []any{"x"},
				}},
				{Name: "no_schema"},
			}
			if _, err := p.Complete(context.Background(), req); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			body := api.seen()[0].body

			got, present := body["tool_choice"]
			if tc.want == nil {
				if present {
					t.Fatalf("tool_choice = %v, want the field absent", got)
				}
			} else if got != tc.want {
				t.Fatalf("tool_choice = %v, want %v", got, tc.want)
			}

			tools := body["tools"].([]any)
			if len(tools) != 2 {
				t.Fatalf("sent %d tools", len(tools))
			}
			first := tools[0].(map[string]any)
			if first["type"] != "function" || dig(t, first, "function", "name") != "with_schema" {
				t.Fatalf("first tool = %v", first)
			}
			if _, ok := dig(t, first, "function", "parameters", "properties").(map[string]any); !ok {
				t.Fatalf("schema lost: %v", first)
			}
			// A tool with no schema still declares an object: several
			// compatible endpoints reject a function whose parameters are
			// missing or untyped.
			second := tools[1].(map[string]any)
			if dig(t, second, "function", "parameters", "type") != "object" {
				t.Fatalf("empty schema = %v", second)
			}
		})
	}
}

func TestReasoningChangesTheTokenCapAndDropsTemperature(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("ok")) })
	p := newProvider(t, url, func(c *Config) {
		c.Reasoning = true
		c.ReasoningEffort = "high"
		c.MaxTokens = 500
	})
	if _, err := p.Complete(context.Background(), userTurn("hi")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	body := api.seen()[0].body
	if body["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", body["reasoning_effort"])
	}
	// The o-series rejects max_tokens outright. Python sent it anyway,
	// which 400s every reasoning call the moment a caller sets a cap.
	if _, present := body["max_tokens"]; present {
		t.Fatalf("max_tokens sent to a reasoning model: %v", body)
	}
	if body["max_completion_tokens"] != float64(500) {
		t.Fatalf("max_completion_tokens = %v", body["max_completion_tokens"])
	}
	if _, present := body["temperature"]; present {
		t.Fatalf("temperature sent to a reasoning model: %v", body["temperature"])
	}
}

func TestTemperatureAndMaxTokensDefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		configure       func(*Config)
		request         llm.Request
		wantTemperature float64
		wantMaxTokens   any // nil means absent
	}{
		{
			// The tool loop sends neither field on any call it makes, so
			// "unset" is what the whole engine runs on: a nil temperature
			// must reach the provider's configured default, not 0.0.
			name: "request says nothing", request: userTurn("hi"),
			wantTemperature: DefaultTemperature, wantMaxTokens: nil,
		},
		{
			name:            "config supplies a cap",
			configure:       func(c *Config) { c.Temperature = 0.2; c.MaxTokens = 512 },
			request:         userTurn("hi"),
			wantTemperature: 0.2, wantMaxTokens: float64(512),
		},
		{
			name:            "request overrides the config",
			configure:       func(c *Config) { c.Temperature = 0.2; c.MaxTokens = 512 },
			request:         llm.Request{Messages: userTurn("hi").Messages, Temperature: llm.Temp(0.9), MaxTokens: 77},
			wantTemperature: 0.9, wantMaxTokens: float64(77),
		},
		{
			// The whole reason Temperature is a pointer. A judge asking
			// for a reproducible answer says 0.0 and MUST get it; a
			// backend testing `> 0` silently substitutes its default and
			// the judge is non-deterministic with nothing to show for it.
			name:            "an explicit zero reaches the wire",
			configure:       func(c *Config) { c.Temperature = 0.2 },
			request:         llm.Request{Messages: userTurn("hi").Messages, Temperature: llm.Temp(0)},
			wantTemperature: 0, wantMaxTokens: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("ok")) })
			p := newProvider(t, url, tc.configure)
			if _, err := p.Complete(context.Background(), tc.request); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			body := api.seen()[0].body
			if body["temperature"] != tc.wantTemperature {
				t.Fatalf("temperature = %v, want %v", body["temperature"], tc.wantTemperature)
			}
			got, present := body["max_tokens"]
			if tc.wantMaxTokens == nil {
				if present {
					t.Fatalf("max_tokens = %v, want the field absent", got)
				}
				return
			}
			if got != tc.wantMaxTokens {
				t.Fatalf("max_tokens = %v, want %v", got, tc.wantMaxTokens)
			}
		})
	}
}

// --- response translation ---------------------------------------------

// prompt_tokens is ALREADY the full prompt count and cached_tokens is a
// SUBSET of it. This is the mirror image of the Anthropic backend and the
// same invariant: InputTokens is the full prompt count, and the two vendors
// report it differently. Adding the cache figures here double-bills every
// cached round.
func TestInputTokensAreNotSummedWithTheCacheBreakdown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		prompt, completion int
		cached, written    int
	}{
		{"no cache", 100, 20, 0, 0},
		{"mostly cached", 100, 20, 90, 0},
		{"cache written", 100, 20, 0, 40},
		{"both", 100, 20, 60, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, url := serve(t, func(w http.ResponseWriter, _ int) {
				writeJSON(w, 200, fmt.Sprintf(`{
					"id":"c","object":"chat.completion","created":1,"model":"gpt-test",
					"choices":[{"index":0,"finish_reason":"stop",
						"message":{"role":"assistant","content":"x"}}],
					"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,
						"prompt_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d}}
				}`, tc.prompt, tc.completion, tc.prompt+tc.completion, tc.cached, tc.written))
			})
			p := newProvider(t, url, nil)
			got, err := p.Complete(context.Background(), userTurn("hi"))
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got.InputTokens != tc.prompt {
				t.Fatalf("InputTokens = %d, want prompt_tokens unchanged (%d)",
					got.InputTokens, tc.prompt)
			}
			if got.OutputTokens != tc.completion {
				t.Fatalf("OutputTokens = %d, want %d", got.OutputTokens, tc.completion)
			}
			if got.CacheRead != tc.cached || got.CacheWrite != tc.written {
				t.Fatalf("cache breakdown = %d/%d, want %d/%d",
					got.CacheRead, got.CacheWrite, tc.cached, tc.written)
			}
			if got.TotalTokens() != tc.prompt+tc.completion {
				t.Fatalf("TotalTokens = %d, want %d", got.TotalTokens(), tc.prompt+tc.completion)
			}
		})
	}
}

func TestReasoningTraceIsReadOffTheRawMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		message string
		want    string
	}{
		{"neither", `{"role":"assistant","content":"x"}`, ""},
		{"reasoning_content", `{"role":"assistant","content":"x","reasoning_content":"deepseek style"}`, "deepseek style"},
		{"bare reasoning", `{"role":"assistant","content":"x","reasoning":"nebius style"}`, "nebius style"},
		{"both prefers reasoning_content",
			`{"role":"assistant","content":"x","reasoning_content":"older","reasoning":"newer"}`, "older"},
		// A structured summary must yield nothing rather than fail a call.
		{"reasoning as an object", `{"role":"assistant","content":"x","reasoning":{"summary":"s"}}`, ""},
		{"reasoning null", `{"role":"assistant","content":"x","reasoning":null}`, ""},
		{"empty reasoning_content falls through to reasoning",
			`{"role":"assistant","content":"x","reasoning_content":"","reasoning":"used"}`, "used"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, url := serve(t, func(w http.ResponseWriter, _ int) {
				writeJSON(w, 200, fmt.Sprintf(`{
					"id":"c","object":"chat.completion","created":1,"model":"gpt-test",
					"choices":[{"index":0,"finish_reason":"stop","message":%s}],
					"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
				}`, tc.message))
			})
			p := newProvider(t, url, nil)
			got, err := p.Complete(context.Background(), userTurn("hi"))
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got.ReasoningContent != tc.want {
				t.Fatalf("ReasoningContent = %q, want %q", got.ReasoningContent, tc.want)
			}
		})
	}
}

func TestToolCallsAreTranslated(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, `{
			"id":"c","object":"chat.completion","created":1,"model":"gpt-test",
			"choices":[{"index":0,"finish_reason":"tool_calls","message":{
				"role":"assistant","content":null,
				"tool_calls":[
					{"id":"call_1","type":"function",
					 "function":{"name":"run","arguments":"{\"path\":\"/tmp\"}"}},
					{"id":"call_2","type":"function",
					 "function":{"name":"broken","arguments":"{\"n\":1e1000}"}},
					{"id":"call_3","type":"function",
					 "function":{"name":"empty","arguments":""}}
				]}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	})
	p := newProvider(t, url, nil)
	out, err := p.Complete(context.Background(), userTurn("hi"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(out.ToolCalls) != 3 {
		t.Fatalf("ToolCalls = %+v", out.ToolCalls)
	}
	if out.ToolCalls[0].Arguments["path"] != "/tmp" {
		t.Fatalf("first call = %+v", out.ToolCalls[0])
	}
	// The property is that the arguments can go back onto the wire. A
	// number no float64 holds is decoded exactly and round-trips; keeping a
	// half-decoded +Inf would put a value in the conversation that fails to
	// serialise a round later and takes the rest of the turn with it.
	for _, i := range []int{1, 2} {
		if _, err := json.Marshal(out.ToolCalls[i].Arguments); err != nil {
			t.Fatalf("call %d arguments cannot be re-serialised: %v", i, err)
		}
	}
	if out.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q", out.FinishReason)
	}
	// The per-model token breakdown is built from completions, so every
	// answer has to name the model that produced it.
	if out.Model != "gpt-test" {
		t.Fatalf("Model = %q, want the configured model id", out.Model)
	}
}

// The CONFIGURED id, not the one the response echoes: a vendor alias resolving
// to a dated snapshot would re-key the breakdown the day the alias moves.
func TestTheCompletionNamesTheConfiguredModelNotTheEcho(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, `{"id":"c","object":"chat.completion","created":1,
			"model":"gpt-test-20990101",
			"choices":[{"index":0,"finish_reason":"stop",
				"message":{"role":"assistant","content":"x"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	p := newProvider(t, url, nil)
	out, err := p.Complete(context.Background(), userTurn("hi"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out.Model != "gpt-test" {
		t.Fatalf("Model = %q, want the configured id", out.Model)
	}
}

func TestACustomToolCallIsSkippedRatherThanFaked(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, `{
			"id":"c","object":"chat.completion","created":1,"model":"gpt-test",
			"choices":[{"index":0,"finish_reason":"tool_calls","message":{
				"role":"assistant","content":null,
				"tool_calls":[
					{"id":"call_1","type":"custom","custom":{"name":"freeform","input":"do a thing"}},
					{"id":"call_2","type":"function","function":{"name":"run","arguments":"{}"}}
				]}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	})
	p := newProvider(t, url, nil)
	out, err := p.Complete(context.Background(), userTurn("hi"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != "run" {
		t.Fatalf("ToolCalls = %+v, want only the function call", out.ToolCalls)
	}
}

// Python returned an empty completion with finish_reason "error" here, which
// the tool loop reads as a clean finish: the phase produces nothing and
// reports success.
func TestNoChoicesIsAServerFailureNotAnEmptyAnswer(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, `{"id":"c","object":"chat.completion","created":1,
			"model":"gpt-test","choices":[],
			"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
	})
	p := newProvider(t, url, nil)
	out, err := p.Complete(context.Background(), userTurn("hi"))
	if out != nil {
		t.Fatalf("Complete returned a completion for an empty response: %+v", out)
	}
	if got := llm.KindOf(err); got != llm.KindServer {
		t.Fatalf("classified %s, want server (retryable, no credential benched)", got)
	}
	for _, s := range p.Pool().Stats() {
		if s.Cooling != 0 {
			t.Fatal("a malformed response benched the credential")
		}
	}
}

func TestAMissingFinishReasonGetsADefault(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, `{"id":"c","object":"chat.completion","created":1,"model":"gpt-test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"x"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	p := newProvider(t, url, nil)
	out, err := p.Complete(context.Background(), userTurn("hi"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want the default", out.FinishReason)
	}
}

func TestConcurrentCompletesShareOnePoolSafely(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, n int) {
		if n%4 == 0 {
			writeJSON(w, 429, apiError("rate_limit_exceeded"))
			return
		}
		writeJSON(w, 200, okCompletion("ok"))
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

func TestNameLabelsAnOpenAICompatibleEndpoint(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 400, apiError("invalid_request_error"))
	})
	p := newProvider(t, url, func(c *Config) { c.Name = "openai-compatible" })
	_, err := p.Complete(context.Background(), userTurn("hi"))
	var classified *llm.Error
	if !errors.As(err, &classified) {
		t.Fatalf("err = %v", err)
	}
	if classified.Provider != "openai-compatible" {
		t.Fatalf("error names %q, want the configured label", classified.Provider)
	}
	if !strings.Contains(p.String(), "openai-compatible/gpt-test") {
		t.Fatalf("String() = %q", p.String())
	}
}
