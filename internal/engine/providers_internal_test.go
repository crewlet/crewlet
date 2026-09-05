package engine

import (
	"strings"

	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm"
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
// `base_url: "${LLM_BASE_URL}"` — which examples/nimbus.company.yaml ships
// and docs/reference/environment-variables.md documents — sent every request
// to a URL that was the literal reference. And LookupEnv was never passed, so
// the conventional-key fallback read the process environment and could not
// see a value `crewlet secrets set OPENAI_API_KEY` had put in the store,
// which is the case config.Resolver.LookupOK exists for.
func TestBuildProviderResolvesBaseURLAndConventionalKey(t *testing.T) {
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
	p, err := buildProvider("gateway", config.LLMProvider{
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
// X-Api-Key rather than Authorization, and anthropic.Config.LookupEnv's own
// doc promises "the engine passes a secret-store-aware resolver" — which it
// did not.
func TestBuildProviderResolvesAnthropicBaseURLAndConventionalKey(t *testing.T) {
	var gotKey string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant",
			"model":"claude-test","content":[{"type":"text","text":"ok"}],
			"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	r := tierB(map[string]string{
		"LLM_BASE_URL":      srv.URL,
		"ANTHROPIC_API_KEY": "sk-ant-from-the-store",
	})

	p, err := buildProvider("gateway", config.LLMProvider{
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
	r := tierB(map[string]string{"LLM_MODEL": "gpt-4o-mini"})

	for _, kind := range []config.LLMProviderType{
		config.LLMOpenAI, config.LLMAnthropic,
	} {
		t.Run(string(kind), func(t *testing.T) {
			p, err := buildProvider("cheap", config.LLMProvider{
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
	r := tierB(map[string]string{"CLI_MODEL": "sonnet"})

	p, err := buildProvider("subscription", config.LLMProvider{
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
			p, err := buildProvider("vllm", tc.spec, r)
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
	_, err := buildProvider("default", config.LLMProvider{
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
	_, err := buildProvider("gateway", config.LLMProvider{
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
	if _, err := buildProvider("gpt", config.LLMProvider{
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
	if _, err := buildProvider("default", config.LLMProvider{
		Type: config.LLMOpenAICompatible, Model: "llama-3",
		BaseURL: "https://gateway.example.com/v1",
		APIKeys: []string{"${NOBODY_SET_THIS}"},
	}, r); err != nil {
		t.Fatalf("a provider with no credential was refused: %v", err)
	}
}
