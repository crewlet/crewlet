package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/anthropic"
	"github.com/crewlet/crewlet/internal/providers/llm/openai"
)

// storeSource stands in for the secret store: it answers by NAME, and nothing
// it holds is exported into the process environment. That is what makes these
// tests prove the resolver was consulted rather than os.Getenv — a backend
// falling back to the process environment answers "unset" for every one.
type storeSource map[string]string

func (s storeSource) Lookup(name string) (string, bool) {
	v, ok := s[name]
	return v, ok
}

// tierB is the Tier B chain: the store first, the environment behind it.
func tierB(values map[string]string) *config.Resolver {
	return config.NewResolver(storeSource(values), config.EnvSource{})
}

// chatCompletion is the smallest response the OpenAI SDK will decode.
const chatCompletion = `{
	"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-test",
	"choices":[{"index":0,"finish_reason":"stop",
		"message":{"role":"assistant","content":"ok"}}],
	"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
}`

// TestBuildProviderResolvesBaseURLAndConventionalKey pins the two halves of
// how an entry that names neither an endpoint literally nor a key at all
// still reaches the right server with the right credential.
//
// Both were unwired. base_url was handed to the backend verbatim, so
// `base_url: "${LLM_BASE_URL}"` — the reference an openai-compatible entry is
// documented to carry (docs/reference/environment-variables.md) — sent every
// request to a URL that was the literal reference. And the conventional-key
// fallback read the process environment, so it could not see a value
// `crewlet secrets set OPENAI_API_KEY` had put in the store — the case the
// resolver exists for, and which config.LLMProvider.Keys now reads through.
func TestBuildProviderResolvesBaseURLAndConventionalKey(t *testing.T) {
	t.Parallel()
	var gotAuth string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatCompletion)
	}))
	defer srv.Close()

	r := tierB(map[string]string{
		"LLM_BASE_URL":   srv.URL,
		"OPENAI_API_KEY": "sk-from-the-store",
	})

	// No api_keys: the entry leans on the conventional name entirely.
	p, _, err := buildProvider("gateway", config.LLMProvider{
		Type:    config.LLMOpenAICompatible,
		Model:   "gpt-test",
		BaseURL: "${LLM_BASE_URL}",
	}, r)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}

	if _, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if calls != 1 {
		t.Fatalf("test server saw %d calls, want 1 — the resolved base_url "+
			"never reached the backend", calls)
	}
	if want := "Bearer sk-from-the-store"; gotAuth != want {
		t.Errorf("Authorization = %q, want %q — the conventional-key "+
			"fallback did not consult the secret store", gotAuth, want)
	}
}

// TestBuildProviderResolvesAnthropicBaseURLAndConventionalKey is the same
// invariant on the other HTTP backend. Anthropic sends its credential on
// X-Api-Key rather than Authorization, and anthropic.Config reads no variable
// of its own — its APIKeys are the whole bag — so the conventional key reaches
// it only if the engine resolved it through the store.
func TestBuildProviderResolvesAnthropicBaseURLAndConventionalKey(t *testing.T) {
	t.Parallel()
	var gotKey string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotKey = r.Header.Get("X-Api-Key")
		// The backend streams every call, so the gateway answers as the
		// vendor does: an SSE stream that ends at message_stop.
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`{"type":"message_stop"}`,
		} {
			kind := strings.SplitN(strings.TrimPrefix(event, `{"type":"`), `"`, 2)[0]
			_, _ = io.WriteString(w, "event: "+kind+"\ndata: "+event+"\n\n")
		}
	}))
	defer srv.Close()

	r := tierB(map[string]string{
		"LLM_BASE_URL":      srv.URL,
		"ANTHROPIC_API_KEY": "sk-ant-from-the-store",
	})

	p, _, err := buildProvider("gateway", config.LLMProvider{
		Type:    config.LLMAnthropic,
		Model:   "claude-test",
		BaseURL: "${LLM_BASE_URL}",
	}, r)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}

	if _, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if calls != 1 {
		t.Fatalf("test server saw %d calls, want 1 — the resolved base_url "+
			"never reached the backend", calls)
	}
	if want := "sk-ant-from-the-store"; gotKey != want {
		t.Errorf("X-Api-Key = %q, want %q — the conventional-key fallback "+
			"did not consult the secret store", gotKey, want)
	}
}

// TestBuildProviderResolvesModel pins the third scalar a Tier B document may
// write a reference into. An unresolved model is not a 401 an operator can
// read: it is the vendor rejecting a model literally named "${LLM_MODEL}".
func TestBuildProviderResolvesModel(t *testing.T) {
	t.Parallel()
	r := tierB(map[string]string{"LLM_MODEL": "gpt-4o-mini"})

	for _, kind := range []config.LLMProviderType{
		config.LLMOpenAI, config.LLMAnthropic,
	} {
		t.Run(string(kind), func(t *testing.T) {
			p, _, err := buildProvider("cheap", config.LLMProvider{
				Type:    kind,
				Model:   "${LLM_MODEL}",
				APIKeys: []string{"sk-test"},
			}, r)
			if err != nil {
				t.Fatalf("buildProvider: %v", err)
			}
			if got := p.Model(); got != "gpt-4o-mini" {
				t.Errorf("Model() = %q, want %q", got, "gpt-4o-mini")
			}
		})
	}
}

// TestBuildCLIAgentResolvesModel is the same for the subscription backend,
// whose model becomes the CLI's --model argv rather than a request field.
func TestBuildCLIAgentResolvesModel(t *testing.T) {
	t.Parallel()
	r := tierB(map[string]string{"CLI_MODEL": "sonnet"})

	p, _, err := buildProvider("subscription", config.LLMProvider{
		Type:  config.LLMCLIAgent,
		Model: "${CLI_MODEL}",
		CLI: &config.CLIAgent{
			Agent:    "claude-code",
			StateDir: t.TempDir(),
		},
	}, r)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	if got := p.Model(); got != "sonnet" {
		t.Errorf("Model() = %q, want %q", got, "sonnet")
	}
}

// TestOpenAICompatibleNamesItsEndpoint pins what a failure on a gateway says.
//
// openai.Config.Name's doc promises "an openai-compatible entry passes its
// own so a chain's telemetry says which endpoint answered"; the engine passed
// none, so every gateway, aggregator and local vLLM in a fallback chain
// reported itself as "openai" and two of them were indistinguishable in the
// log line naming which one failed.
func TestOpenAICompatibleNamesItsEndpoint(t *testing.T) {
	t.Parallel()
	r := tierB(nil)

	cases := []struct {
		name string
		spec config.LLMProvider
		want string
	}{{
		name: "openai-compatible takes the config key",
		spec: config.LLMProvider{
			Type: config.LLMOpenAICompatible, Model: "llama-3",
			BaseURL: "https://vllm.example/v1", APIKeys: []string{"sk-test"},
		},
		want: "vllm/llama-3",
	}, {
		name: "plain openai keeps the vendor name",
		spec: config.LLMProvider{
			Type: config.LLMOpenAI, Model: "gpt-4o",
			APIKeys: []string{"sk-test"},
		},
		want: "openai/gpt-4o",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, err := buildProvider("vllm", tc.spec, r)
			if err != nil {
				t.Fatalf("buildProvider: %v", err)
			}
			s, ok := p.(fmt.Stringer)
			if !ok {
				t.Fatalf("provider %T does not name itself", p)
			}
			if got := s.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// AN UNRESOLVED ${VAR} IS NOT A MISSING FIELD, and the two need different
// sentences.
//
// A provider entry that plainly reads `model: "${LLM_MODEL}"` and is refused
// with "Model is required" sends an operator to look at a field they can see
// is filled in. Naming the variable sends them to the one thing they have to
// change — which is the rule every error in this tree is held to.
func TestAnUnresolvedModelNamesTheVariable(t *testing.T) {
	t.Parallel()
	r := tierB(nil)
	_, _, err := buildProvider("default", config.LLMProvider{
		Type: config.LLMOpenAICompatible, Model: "${LLM_MODEL}",
		BaseURL: "https://gateway.example.com/v1",
	}, r)
	if err == nil {
		t.Fatal("a provider with no model built")
	}
	for _, want := range []string{"default", "${LLM_MODEL}", "LLM_MODEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// AN OPENAI-COMPATIBLE ENTRY WITH NO ENDPOINT IS REFUSED, not defaulted.
//
// This one is not a message fix. `openai-compatible` means "not OpenAI" by
// definition — the type exists to point somewhere else — and an empty base
// URL takes the openai backend's own default. So a company whose
// ${LLM_BASE_URL} was unset would send every request, under a key that is not
// an OpenAI key, to api.openai.com: the company's whole model traffic
// silently misrouted to a third party, with a 401 naming a vendor the
// operator never configured as the only symptom.
func TestACompatibleEntryWithNoEndpointIsRefusedRatherThanSentToOpenAI(t *testing.T) {
	t.Parallel()
	r := tierB(nil)
	_, _, err := buildProvider("gateway", config.LLMProvider{
		Type: config.LLMOpenAICompatible, Model: "llama-3",
		BaseURL: "${LLM_BASE_URL}",
	}, r)
	if err == nil {
		t.Fatal("an openai-compatible entry with no endpoint built, so its " +
			"traffic would go to api.openai.com")
	}
	for _, want := range []string{"gateway", "LLM_BASE_URL", "api.openai.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// A PLAIN OPENAI ENTRY STILL DEFAULTS, which is the whole difference between
// the two types: `openai` means OpenAI, so an absent base_url is the ordinary
// case rather than a misroute.
func TestAPlainOpenAIEntryNeedsNoEndpoint(t *testing.T) {
	t.Parallel()
	r := tierB(map[string]string{"OPENAI_API_KEY": "sk-test"})
	if _, _, err := buildProvider("gpt", config.LLMProvider{
		Type: config.LLMOpenAI, Model: "gpt-4o",
	}, r); err != nil {
		t.Fatalf("a plain openai entry with no base_url was refused: %v", err)
	}
}

// AND A MISSING CREDENTIAL STILL BUILDS. The asymmetry is deliberate: every
// call then comes back a clean 401 that names the provider, which is far
// easier to diagnose than a boot that died over one key — where a missing
// model or endpoint has no such tell.
func TestAMissingCredentialStillBuilds(t *testing.T) {
	t.Parallel()
	r := tierB(nil)
	if _, _, err := buildProvider("default", config.LLMProvider{
		Type: config.LLMOpenAICompatible, Model: "llama-3",
		BaseURL: "https://gateway.example.com/v1",
		APIKeys: []string{"${NOBODY_SET_THIS}"},
	}, r); err != nil {
		t.Fatalf("a provider with no credential was refused: %v", err)
	}
}

// An anthropic entry's request shape follows its MODEL, and the entry's dials
// reach the backend: reasoning_effort as output_config.effort, claude_model as
// the row an alias is shaped from, reasoning_budget_tokens as a budget-era
// model's thinking. Asserted on the wire, because the wiring is the part that
// can be dropped with everything still compiling.
func TestAnAnthropicEntryIsShapedFromItsModel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		spec config.LLMProvider
		want map[string]any // body field -> its JSON
	}{
		{
			name: "a referenced current model at the entry's effort",
			spec: config.LLMProvider{Model: "${LLM_MODEL}", ReasoningEffort: config.EffortXHigh},
			want: map[string]any{
				"thinking":      map[string]any{"type": "adaptive", "display": "summarized"},
				"output_config": map[string]any{"effort": "xhigh"},
				"max_tokens":    float64(128000),
			},
		},
		{
			name: "an alias named by claude_model, thinking on its budget",
			spec: config.LLMProvider{Model: "gw-fast", ClaudeModel: "claude-haiku-4-5", ReasoningBudgetTokens: 2048},
			want: map[string]any{
				"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(2048)},
				"max_tokens": float64(64000),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant",
					"model":"m","content":[{"type":"text","text":"ok"}],
					"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer srv.Close()
			spec := tc.spec
			spec.Type, spec.BaseURL, spec.APIKeys = config.LLMAnthropic, srv.URL, []string{"sk-ant-test"}
			p, _, err := buildProvider("claude", spec, tierB(map[string]string{"LLM_MODEL": "claude-sonnet-5-5"}))
			if err != nil {
				t.Fatalf("buildProvider: %v", err)
			}
			if _, err := p.Complete(context.Background(), llm.Request{
				Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
			}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			for field, want := range tc.want {
				if got := fmt.Sprint(body[field]); got != fmt.Sprint(want) {
					t.Errorf("%s = %s, want %s", field, got, fmt.Sprint(want))
				}
			}
			if _, sent := body["temperature"]; sent {
				t.Errorf("temperature sent on a thinking call: %v", body["temperature"])
			}
		})
	}
}

// A model written as a `${VAR}` is judged only once it resolves, so the
// backend's refusal is the one an operator reads: it names the provider, the
// model it resolved to and the field to change.
func TestADialTheResolvedModelRefusesFailsTheBuildByName(t *testing.T) {
	t.Parallel()
	_, _, err := buildProvider("claude", config.LLMProvider{
		Type: config.LLMAnthropic, Model: "${LLM_MODEL}", ReasoningBudgetTokens: 4096,
	}, tierB(map[string]string{"LLM_MODEL": "claude-sonnet-5-5"}))
	if err == nil {
		t.Fatal("built a budget on an adaptive model, which every call would answer with a 400")
	}
	for _, want := range []string{`"claude"`, `"claude-sonnet-5-5"`, "reasoning_budget_tokens", "ThinkingBudget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// The config's levels are the contract's, so a level the config admits is one
// a backend can compare and lower.
func TestEveryConfigEffortIsAContractEffort(t *testing.T) {
	t.Parallel()
	for _, level := range config.ReasoningEfforts {
		if e := llm.Effort(level); e == "" || !e.Valid() {
			t.Errorf("config admits reasoning_effort %q, which llm.Effort does not know", level)
		}
	}
}

// ONE TIMEOUT DEFAULT. The engine builds every HTTP backend with the config's
// resolved timeout, and each backend keeps a default of its own for a Config
// built without one; the two were "matched" by a comment, which is how one
// moves and the other does not. The config's is the one that runs and the one
// the docs state, so the backends' must equal it, and an entry naming its own
// value must get exactly that.
func TestTheLLMTimeoutDefaultIsOneNumber(t *testing.T) {
	t.Parallel()
	unset := (&config.LLMProvider{}).Timeout()
	if unset != 600 {
		t.Fatalf("an entry naming no timeout_seconds gets %v s, want 600", unset)
	}
	for name, d := range map[string]time.Duration{
		"anthropic": anthropic.DefaultTimeout,
		"openai":    openai.DefaultTimeout,
	} {
		if d != time.Duration(unset*float64(time.Second)) {
			t.Errorf("%s.DefaultTimeout = %v, want the config's %v s", name, d, unset)
		}
	}
	if got := (&config.LLMProvider{TimeoutSeconds: 45}).Timeout(); got != 45 {
		t.Errorf("an entry naming 45 s gets %v", got)
	}
}

// THE DOCTOR'S PROVIDER IS THE ENGINE'S. `crewlet llm doctor` builds an
// anthropic entry through BuildAnthropic, so it must resolve what buildProvider
// resolves — a `${VAR}` model and endpoint included — and refuse what it
// refuses; one that built its own would certify a request no seat sends.
func TestBuildAnthropicIsTheBuildASeatGets(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	r := tierB(map[string]string{"LLM_MODEL": "claude-opus-4-1", "LLM_BASE_URL": srv.URL})
	spec := config.LLMProvider{Type: config.LLMAnthropic, Model: "${LLM_MODEL}", BaseURL: "${LLM_BASE_URL}"}
	built, err := BuildAnthropic("claude", spec, r)
	if err != nil {
		t.Fatalf("BuildAnthropic: %v", err)
	}
	seat, _, err := buildProvider("claude", spec, r)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	if built.Model() != "claude-opus-4-1" || built.Model() != seat.Model() {
		t.Errorf("doctor builds %q, a seat %q", built.Model(), seat.Model())
	}
	if d := built.Diagnose(context.Background(), anthropic.DiagnoseOptions{Key: "claude"}); d.Endpoint != srv.URL {
		t.Errorf("endpoint = %q, want the resolved base_url", d.Endpoint)
	}

	// The refusals too: a reference that resolved to nothing, and a dial
	// the resolved model would answer with a 400.
	if _, err := BuildAnthropic("claude", config.LLMProvider{
		Type: config.LLMAnthropic, Model: "${LLM_MODEL}",
	}, tierB(nil)); err == nil || !strings.Contains(err.Error(), "LLM_MODEL resolved to nothing") {
		t.Errorf("an unresolved model: %v", err)
	}
	if _, err := BuildAnthropic("claude", config.LLMProvider{
		Type: config.LLMAnthropic, Model: "claude-opus-5-5", ReasoningBudgetTokens: 4096,
	}, r); err == nil || !strings.Contains(err.Error(), "reasoning_budget_tokens") {
		t.Errorf("a budget on an adaptive model: %v", err)
	}
	if _, err := BuildAnthropic("gpt", config.LLMProvider{Type: config.LLMOpenAI, Model: "gpt-5"}, r); err == nil {
		t.Error("built an openai entry as an anthropic provider")
	}
}

// A RESOLVED base_url IS HELD TO BOTH HALVES OF ITS RULE, by class. A `${VAR}`
// is judged at validation only around its references, so what the VARIABLE
// holds is first seen here. One no call can be sent on is REFUSED, naming the
// provider and the variable, never the value. One whose query or fragment the
// SDK drops BUILDS — every call is sent, only without the part, and the rule
// that refuses one on a write is an admission rule a later build may relax —
// and is warned about as provider_base_url_unsent, naming the provider, the
// part and the variable and never the value, since a query is where a key
// gets written. The embeddings provider is held to the same two.
//
// Mutation: refuse a revealed part in config.resolveBaseURL, and the query
// cases fail to build; drop warnUnsentBaseURL from resolveEndpoint or from
// embeddingsBaseURL, and that half logs nothing.
func TestAResolvedBaseURLIsRefusedOnlyWhenNoCallCanBeSent(t *testing.T) {
	t.Parallel()
	const secret = "sk-in-the-query"
	r := config.NewResolver(storeSource{
		"LLM_BASE_URL":   "https://gw.example.com/v1?key=" + secret,
		"LLM_NO_SCHEME":  "gw.example.com/v1?key=" + secret,
		"EMBED_BASE_URL": "https://embed.example.com/v1#" + secret,
		"EMBED_NO_HOST":  "https:///v1#" + secret,
	})
	for _, kind := range []config.LLMProviderType{config.LLMAnthropic, config.LLMOpenAI, config.LLMOpenAICompatible} {
		spec := config.LLMProvider{Type: kind, Model: "claude-opus-5-5", BaseURL: "${LLM_NO_SCHEME}"}
		_, _, err := buildProvider("gw", spec, r)
		if err == nil {
			t.Errorf("%s: built over a base_url with no scheme", kind)
		} else {
			for _, named := range []string{`"gw"`, "LLM_NO_SCHEME", "http://"} {
				if !strings.Contains(err.Error(), named) {
					t.Errorf("%s: %q does not name %s", kind, err, named)
				}
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "gw.example.com") {
				t.Errorf("%s: the refusal repeats the resolved value: %v", kind, err)
			}
		}

		var logged bytes.Buffer
		spec.BaseURL = "${LLM_BASE_URL}"
		e, err := resolveEndpoint(slog.New(slog.NewJSONHandler(&logged, nil)), "gw", spec, r)
		if err != nil {
			t.Errorf("%s: a base_url whose variable holds a query was refused: %v", kind, err)
			continue
		}
		if e.baseURL != "https://gw.example.com/v1?key="+secret {
			t.Errorf("%s: the backend is handed %q", kind, e.baseURL)
		}
		assertUnsentWarning(t, logged.String(), "gw", "query", "LLM_BASE_URL", secret)
		if _, _, err := buildProvider("gw", spec, r); err != nil {
			t.Errorf("%s: buildProvider refused what resolveEndpoint built: %v", kind, err)
		}
	}

	embedder := func(variable string) *config.EmbeddingProvider {
		return &config.EmbeddingProvider{
			Type: config.EmbeddingOpenAICompatible, Model: "text-embedding-3-small",
			APIKey: "sk-embed", BaseURL: "${" + variable + "}",
		}
	}
	var logged bytes.Buffer
	base, err := embeddingsBaseURL(slog.New(slog.NewJSONHandler(&logged, nil)), embedder("EMBED_BASE_URL"), r)
	if err != nil || base != "https://embed.example.com/v1#"+secret {
		t.Errorf("an embeddings base_url whose variable holds a fragment = %q, %v; want it built", base, err)
	}
	assertUnsentWarning(t, logged.String(), "embeddings", "fragment", "EMBED_BASE_URL", secret)
	_, err = embeddingsBaseURL(slog.New(slog.NewJSONHandler(io.Discard, nil)), embedder("EMBED_NO_HOST"), r)
	if err == nil || !strings.Contains(err.Error(), "EMBED_NO_HOST") || strings.Contains(err.Error(), secret) {
		t.Errorf("an embeddings base_url with no host: %v, want a refusal naming EMBED_NO_HOST "+
			"and not the value", err)
	}
}

// assertUnsentWarning holds a captured log to exactly one
// provider_base_url_unsent line naming provider, part and variable, and
// carrying nothing of the value.
func assertUnsentWarning(t *testing.T, logged, provider, part, variable, secret string) {
	t.Helper()
	if strings.Contains(logged, secret) {
		t.Errorf("the warning repeats the value: %s", logged)
	}
	var line struct {
		Msg       string   `json:"msg"`
		Provider  string   `json:"provider"`
		Parts     []string `json:"parts"`
		Variables []string `json:"variables"`
	}
	if trimmed := strings.TrimSpace(logged); strings.Contains(trimmed, "\n") ||
		json.Unmarshal([]byte(trimmed), &line) != nil {
		t.Fatalf("want one JSON warning, logged %q", logged)
	}
	if line.Msg != "provider_base_url_unsent" || line.Provider != provider ||
		len(line.Parts) != 1 || line.Parts[0] != part ||
		len(line.Variables) != 1 || line.Variables[0] != variable {
		t.Errorf("warning = %+v, want provider_base_url_unsent naming %s, %s and %s",
			line, provider, part, variable)
	}
}

// WHAT IS JUDGED IS WHAT IS SENT. A stored base_url with surrounding space or a
// trailing CR (`crewlet secrets set` strips only one newline) passed a check
// that trimmed and was then handed to the SDK as written, which failed every
// call with the whole URL — userinfo included — in its parse error. The
// endpoint is trimmed once, and the call reaches the server.
//
// Mutation: drop the trim from config.resolveBaseURL, and the build is
// refused over a value that was always sendable.
func TestAResolvedBaseURLIsSentAsItWasJudged(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatCompletion)
	}))
	defer srv.Close()
	r := config.NewResolver(storeSource{"LLM_BASE_URL": " " + srv.URL + "\r"})
	p, _, err := buildProvider("gw", config.LLMProvider{
		Type: config.LLMOpenAICompatible, Model: "gpt-test", BaseURL: "${LLM_BASE_URL}",
		APIKeys: []string{"sk-test"},
	}, r)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	if _, err := p.Complete(t.Context(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil || calls.Load() != 1 {
		t.Fatalf("Complete over a CR-terminated base_url: %v (%d calls), want one call answered", err, calls.Load())
	}
}

// WHERE A BASE_URL'S USERINFO IS SENT, measured on every backend: the fact the
// admission rule on userinfo (config.BaseURLUnsent) rests on. Go's HTTP client
// sends a URL's userinfo as Basic credentials only on a request carrying no
// Authorization of its own, and openai-go sends any key it holds as
// `Authorization: Bearer` — so on the openai backend and the embedder the
// userinfo reaches the gateway ONLY on an entry with no key, while the
// Anthropic SDK, whose key travels as x-api-key, always sends it.
func TestWhereABaseURLsUserinfoIsSent(t *testing.T) {
	t.Parallel()
	const basic = "Basic Z3ctdXNlcjpndy1wYXNz" // gw-user:gw-pass
	type seen struct{ authorization, apiKey string }
	server := func(t *testing.T, answer func(http.ResponseWriter)) (*httptest.Server, func() seen) {
		var (
			mu  sync.Mutex
			got seen
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			got = seen{r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")}
			mu.Unlock()
			answer(w)
		}))
		t.Cleanup(srv.Close)
		return srv, func() seen { mu.Lock(); defer mu.Unlock(); return got }
	}
	withUserinfo := func(srv *httptest.Server) string {
		return strings.Replace(srv.URL, "http://", "http://gw-user:gw-pass@", 1)
	}
	ask := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}

	for _, tc := range []struct {
		name string
		keys []string
		want seen
	}{
		{"an openai entry with a key", []string{"sk-test"}, seen{authorization: "Bearer sk-test"}},
		{"an openai entry with no key", nil, seen{authorization: basic}},
	} {
		srv, got := server(t, func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, chatCompletion)
		})
		p, _, err := buildProvider("gw", config.LLMProvider{
			Type: config.LLMOpenAICompatible, Model: "gpt-test", BaseURL: withUserinfo(srv), APIKeys: tc.keys,
		}, config.NewResolver(storeSource{}))
		if err != nil {
			t.Fatalf("%s: buildProvider: %v", tc.name, err)
		}
		if _, err := p.Complete(t.Context(), ask); err != nil {
			t.Fatalf("%s: Complete: %v", tc.name, err)
		}
		if got() != tc.want {
			t.Errorf("%s: the gateway saw %+v, want %+v", tc.name, got(), tc.want)
		}
	}

	srv, got := server(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`{"type":"message_stop"}`,
		} {
			kind := strings.SplitN(strings.TrimPrefix(event, `{"type":"`), `"`, 2)[0]
			_, _ = io.WriteString(w, "event: "+kind+"\ndata: "+event+"\n\n")
		}
	})
	p, _, err := buildProvider("claude", config.LLMProvider{
		Type: config.LLMAnthropic, Model: "claude-test", BaseURL: withUserinfo(srv), APIKeys: []string{"sk-ant-test"},
	}, config.NewResolver(storeSource{}))
	if err != nil {
		t.Fatalf("anthropic: buildProvider: %v", err)
	}
	if _, err := p.Complete(t.Context(), ask); err != nil {
		t.Fatalf("anthropic: Complete: %v", err)
	}
	if want := (seen{authorization: basic, apiKey: "sk-ant-test"}); got() != want {
		t.Errorf("an anthropic entry with a key: the gateway saw %+v, want %+v", got(), want)
	}

	for _, tc := range []struct {
		name, key string
		want      seen
	}{
		{"an embedder with a key", "sk-embed", seen{authorization: "Bearer sk-embed"}},
		{"an embedder with no key", "", seen{authorization: basic}},
	} {
		srv, got := server(t, func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			vector := strings.TrimSuffix(strings.Repeat("0.5,", 1536), ",")
			fmt.Fprintf(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[%s]}],"model":"m"}`, vector)
		})
		cfg := &config.EmbeddingProvider{
			Type: config.EmbeddingOpenAICompatible, Model: "text-embedding-3-small",
			APIKey: tc.key, BaseURL: withUserinfo(srv),
		}
		base, err := embeddingsBaseURL(slog.New(slog.NewJSONHandler(io.Discard, nil)), cfg, config.NewResolver(storeSource{}))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		limits := cfg.Limits()
		embedder, err := embeddings.New(embeddings.Config{
			Model: cfg.Model, Dimensions: cfg.Width(), APIKey: tc.key, BaseURL: base,
			Limits: embeddings.Limits{
				InputBytes: limits.InputBytes, BatchInputs: limits.BatchInputs,
				BatchBytes: limits.BatchBytes, InputOverhead: limits.InputOverhead,
			},
		})
		if err != nil {
			t.Fatalf("%s: embeddings.New: %v", tc.name, err)
		}
		if _, err := embedder.Embed(t.Context(), "hello"); err != nil {
			t.Fatalf("%s: Embed: %v", tc.name, err)
		}
		if got() != tc.want {
			t.Errorf("%s: the gateway saw %+v, want %+v", tc.name, got(), tc.want)
		}
	}
}
