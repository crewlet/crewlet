package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The base URLs Google documents for its OpenAI-compatible endpoints: the
// Gemini API's, and Vertex AI's on a regional host.
const (
	geminiAPI = "https://generativelanguage.googleapis.com/v1beta/openai/"
	vertexAI  = "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/endpoints/openapi"
)

// standIn is a call's extra_content carrying the value Google documents for a
// function call it did not generate.
const standIn = `{"google":{"thought_signature":"skip_thought_signature_validator"}}`

// providerAt is a provider BUILT for baseURL and model — so its binding and
// what it knows of the host are baseURL's — whose transport reaches a recorder
// that answers every call with answer.
func providerAt(t *testing.T, baseURL, model string, answer func(w http.ResponseWriter, n int)) (*Provider, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{handle: answer}
	recorder := httptest.NewServer(api)
	t.Cleanup(recorder.Close)
	return newProvider(t, baseURL, func(c *Config) {
		c.Model = model
		c.HTTPClient = &http.Client{Transport: httpxtest.Rewrite(t, recorder)}
	}), api
}

func answerOK(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("ok")) }

// callsOf is the tool calls an assistant message carried, each as an object.
func callsOf(t *testing.T, msg map[string]any) []map[string]any {
	t.Helper()
	raw, _ := msg["tool_calls"].([]any)
	calls := make([]map[string]any, len(raw))
	for i, c := range raw {
		calls[i] = c.(map[string]any)
	}
	return calls
}

// foreignStep is an assistant turn with one tool call per id that the
// ANTHROPIC backend wrote: nothing in it is any OpenAI-wire host's to read.
func foreignStep(ids ...string) []llm.Message {
	turn := llm.Message{
		Role: llm.RoleAssistant, ReasoningContent: "claude's thinking",
		Origin: llm.Origin{Provider: "anthropic", Model: "claude-opus-5-5"},
		Raw:    []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"claude's thinking","signature":"s"}`)},
	}
	results := make([]llm.Message, 0, len(ids))
	for _, id := range ids {
		turn.ToolCalls = append(turn.ToolCalls, llm.ToolCall{ID: id, Name: "weather", Arguments: map[string]any{"city": id}})
		results = append(results, llm.Message{Role: llm.RoleTool, ToolCallID: id, Content: "sun"})
	}
	return append([]llm.Message{turn}, results...)
}

// A fallback onto Gemini in the middle of a tool loop hands it steps it did
// not write, and Gemini 3 refuses a step of the current turn whose first call
// has no thought signature — fatally, so the chain tries nothing else. Google
// documents one value for a call it did not generate,
// skip_thought_signature_validator, and every such step carries it on its
// first call: a parallel step's other calls carry nothing, as the model's own
// do. Every step, including the ones before a user message that may or may not
// start a new turn as Google counts them. A step ANOTHER Google endpoint signed
// goes with the stand-in too, never with that endpoint's signature.
func TestAStepGoogleDidNotWriteGoesWithTheDocumentedStandIn(t *testing.T) {
	t.Parallel()
	for _, base := range []string{geminiAPI, vertexAI} {
		t.Run(base, func(t *testing.T) {
			t.Parallel()
			p, api := providerAt(t, base, "gemini-3.5-flash", answerOK)
			other := vertexAI
			if base == vertexAI {
				other = geminiAPI
			}
			signedElsewhere := llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_v", Name: "weather", Arguments: map[string]any{}}},
				Origin:    llm.Origin{Provider: wireName, Model: "gemini-3.5-flash"},
				Raw: []json.RawMessage{json.RawMessage(`{"tool_calls":[{"id":"call_v",` +
					`"extra_content":{"google":{"thought_signature":"T1RIRVJfRU5EUE9JTlQ="}}}]}`)},
				Binding: endpointBinding(other),
			}
			conversation := userTurn("go")
			conversation.Messages = append(conversation.Messages, foreignStep("call_1", "call_2")...)
			conversation.Messages = append(conversation.Messages, signedElsewhere,
				llm.Message{Role: llm.RoleTool, ToolCallID: "call_v", Content: "rain"},
				// A person's note to the running turn, after tool results.
				llm.Message{Role: llm.RoleUser, Content: "check Rome too"})
			conversation.Messages = append(conversation.Messages, foreignStep("call_3")...)
			if _, err := p.Complete(context.Background(), conversation); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			sent := assistantsSent(api.seen()[0].body)
			if len(sent) != 3 {
				t.Fatalf("%d assistant messages went, want 3", len(sent))
			}
			for i, msg := range sent {
				calls := callsOf(t, msg)
				sameJSON(t, fmt.Sprintf("step %d's first call's extra_content", i), calls[0]["extra_content"], standIn)
				for _, call := range calls[1:] {
					if extra, set := call["extra_content"]; set {
						t.Fatalf("step %d: a parallel call after the first carried extra_content %v", i, extra)
					}
				}
			}
		})
	}
}

// A step the endpoint signed goes back with the model's own signature, which
// the stand-in never replaces — the model's own reasoning is what the
// signature brings back. Only the step it did not write is given one.
func TestAStepGoogleSignedKeepsItsOwnSignature(t *testing.T) {
	t.Parallel()
	const google = `{"google":{"thought_signature":"PFNJR05BVFVSRV9BPg=="}}`
	p, api := providerAt(t, geminiAPI, "gemini-3.5-flash", func(w http.ResponseWriter, n int) {
		if n > 1 {
			answerOK(w, n)
			return
		}
		writeJSON(w, 200, unaryAnswer(`{"role":"assistant","tool_calls":[
			{"id":"call_1","type":"function","extra_content":`+google+`,
			 "function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}},
			{"id":"call_2","type":"function",
			 "function":{"name":"weather","arguments":"{\"city\":\"London\"}"}}]}`))
	})
	conversation := userTurn("go")
	own := complete(t, p, false, conversation)
	conversation.Messages = append(conversation.Messages, own.Message(),
		llm.Message{Role: llm.RoleTool, ToolCallID: "call_1", Content: "sun"},
		llm.Message{Role: llm.RoleTool, ToolCallID: "call_2", Content: "rain"})
	conversation.Messages = append(conversation.Messages, foreignStep("call_3")...)
	complete(t, p, false, conversation)

	sent := assistantsSent(api.seen()[1].body)
	if len(sent) != 2 {
		t.Fatalf("%d assistant messages went, want 2", len(sent))
	}
	mine := callsOf(t, sent[0])
	sameJSON(t, "the signed step's first call's extra_content", mine[0]["extra_content"], google)
	if extra, set := mine[1]["extra_content"]; set {
		t.Fatalf("the signed step's second call went with extra_content %v", extra)
	}
	sameJSON(t, "the foreign step's extra_content", callsOf(t, sent[1])[0]["extra_content"], standIn)
}

// Which requests carry the stand-in is a fact about the host and the model
// in front of it. Google's own endpoints, known from the URL — the request
// that needs it is the first one after a fallback, when the endpoint has
// written nothing — and a Gemini of generation 3 or later, which is what holds
// a step to its signature; a model id is read by its last segment, and one
// naming no generation is the current one. Never a host that documents no
// extra_content, nor a model Google does not check.
func TestOnlyAGoogleEndpointServingGemini3GetsTheStandIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		base, model string
		want        bool
	}{
		{geminiAPI, "gemini-3.5-flash", true},
		{geminiAPI, "models/gemini-3.1-pro-preview", true},
		{geminiAPI, "gemini-flash-latest", true},
		{geminiAPI, "gemini-4-pro", true},
		{"https://aiplatform.googleapis.com/v1/projects/p/locations/global/endpoints/openapi",
			"google/gemini-3.1-pro-preview", true},
		{"HTTPS://Europe-West4-AIPlatform.googleapis.com/v1beta1/projects/p/locations/europe-west4/endpoints/openapi",
			"google/gemini-3.5-flash", true},
		// Vertex AI's jurisdictional multi-region endpoints, which serve
		// only Gemini 3 and later, and the private names of the same.
		{"https://aiplatform.us.rep.googleapis.com/v1/projects/p/locations/us/endpoints/openapi",
			"google/gemini-3.5-flash", true},
		{"https://aiplatform.eu.rep.googleapis.com/v1/projects/p/locations/eu/endpoints/openapi",
			"google/gemini-3.1-pro-preview", true},
		{"https://aiplatform.us.p.rep.googleapis.com/v1/projects/p/locations/us/endpoints/openapi",
			"google/gemini-3.5-flash", true},
		// A Private Service Connect endpoint's name for Vertex AI.
		{"https://aiplatform-genai1.p.googleapis.com/v1/projects/p/locations/global/endpoints/openapi",
			"google/gemini-3.5-flash", true},
		// A host written as a fully qualified name is the same host.
		{"https://generativelanguage.googleapis.com./v1beta/openai/", "gemini-3.5-flash", true},
		{geminiAPI, "gemini-2.5-flash", false},
		{geminiAPI, "models/gemini-2.0-flash-001", false},
		{geminiAPI, "gemini-1.5-pro", false},
		{geminiAPI, "gemma-4-27b-it", false},
		{vertexAI, "meta/llama-4-maverick-17b-128e-instruct-maas", false},
		{"https://api.openai.com/v1", "gpt-test", false},
		// A gateway carries Google's signature in a field of its own and
		// answers for the steps it has none for itself.
		{"https://openrouter.ai/api/v1", "google/gemini-3.5-flash", false},
		{"https://generativelanguage.googleapis.com.example/v1beta/openai/", "gemini-3.5-flash", false},
		{"https://aiplatform.us.rep.googleapis.com.example/v1/projects/p/locations/us/endpoints/openapi",
			"google/gemini-3.5-flash", false},
		// Another Google service's names, and names no grammar of Google's
		// spells, serve no Gemini.
		{"https://storage.us.rep.googleapis.com/v1/projects/p/locations/us/endpoints/openapi",
			"google/gemini-3.5-flash", false},
		{"https://storage-genai1.p.googleapis.com/v1/projects/p/locations/global/endpoints/openapi",
			"google/gemini-3.5-flash", false},
		{"https://aiplatform.rep.googleapis.com/v1/projects/p/locations/us/endpoints/openapi",
			"google/gemini-3.5-flash", false},
		{"https://aiplatform.p.googleapis.com/v1/projects/p/locations/global/endpoints/openapi",
			"google/gemini-3.5-flash", false},
	} {
		t.Run(tc.base+" "+tc.model, func(t *testing.T) {
			t.Parallel()
			p, api := providerAt(t, tc.base, tc.model, answerOK)
			conversation := userTurn("go")
			conversation.Messages = append(conversation.Messages, foreignStep("call_1")...)
			complete(t, p, false, conversation)
			extra, set := callsOf(t, assistantSent(t, api.seen()[0].body))[0]["extra_content"]
			if set != tc.want {
				t.Fatalf("the foreign step's call carried extra_content = %v (%v), want carried = %v", set, extra, tc.want)
			}
			if set {
				sameJSON(t, "the foreign step's extra_content", extra, standIn)
			}
		})
	}
}

// An endpoint whose URL is not Google's but whose turns carry Google's
// signature — a gateway passing Google's fields through — has shown it holds
// steps to one, and from then on a step it did not write carries the stand-in.
// The evidence is what the endpoint WROTE: without it nothing is added, and a
// Gemini from before generation 3, which Google does not hold to the rule, is
// sent nothing whatever its host signed.
func TestAnEndpointThatSignsTheGoogleWayGetsTheStandInForTheRest(t *testing.T) {
	t.Parallel()
	const google = `{"google":{"thought_signature":"PFNJR05BVFVSRV9BPg=="}}`
	for _, tc := range []struct {
		name, model, extra string
		want               bool
	}{
		{"a signed step", "gemini-3.5-flash", `,"extra_content":` + google, true},
		{"no signed step", "gemini-3.5-flash", ``, false},
		{"a signed step, from an older Gemini", "gemini-2.5-pro", `,"extra_content":` + google, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api, url := serve(t, func(w http.ResponseWriter, n int) {
				if n > 1 {
					answerOK(w, n)
					return
				}
				writeJSON(w, 200, unaryAnswer(`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function"`+
					tc.extra+`,"function":{"name":"weather","arguments":"{}"}}]}`))
			})
			p := newProvider(t, url, func(c *Config) { c.Model = tc.model })
			conversation := userTurn("go")
			own := complete(t, p, false, conversation)
			conversation.Messages = append(conversation.Messages, own.Message(),
				llm.Message{Role: llm.RoleTool, ToolCallID: "call_1", Content: "sun"})
			conversation.Messages = append(conversation.Messages, foreignStep("call_2")...)
			complete(t, p, false, conversation)

			sent := assistantsSent(api.seen()[1].body)
			extra, set := callsOf(t, sent[1])[0]["extra_content"]
			if set != tc.want {
				t.Fatalf("the foreign step's call carried extra_content = %v (%v), want carried = %v", set, extra, tc.want)
			}
			if set {
				sameJSON(t, "the foreign step's extra_content", extra, standIn)
			}
		})
	}
}

// A step THIS endpoint wrote goes back with the record it kept, whatever the
// model — and a round an older Gemini wrote on the same base_url before the
// chain moved to Gemini 3 may have kept fields on its calls but no signature.
// It is as unsigned as a step another member wrote, so its first call goes
// with the stand-in too, added to what the record put back on that call
// rather than replacing it, and the step's other calls go exactly as they
// came.
func TestAStepThisEndpointWroteWithoutASignatureGetsTheStandInBesideItsFields(t *testing.T) {
	t.Parallel()
	const (
		first   = `{"google":{"thought":false}}`
		signed  = `{"google":{"thought":false,"thought_signature":"skip_thought_signature_validator"}}`
		second  = `{"google":{"thought":true}}`
		gateway = `{"note":"kept"}`
	)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			api := &fakeAPI{handle: func(w http.ResponseWriter, n int) {
				if n > 1 {
					answerOK(w, n)
					return
				}
				if !stream {
					writeJSON(w, 200, unaryAnswer(`{"role":"assistant","tool_calls":[
						{"id":"call_1","type":"function","extra_content":`+first+`,"provider_specific_fields":`+gateway+`,
						 "function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}},
						{"id":"call_2","type":"function","extra_content":`+second+`,
						 "function":{"name":"weather","arguments":"{\"city\":\"London\"}"}}]}`))
					return
				}
				streamAnswer(w,
					`{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function",`+
						`"extra_content":`+first+`,"provider_specific_fields":`+gateway+
						`,"function":{"name":"weather","arguments":""}}]}`,
					`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}]}`,
					`{"tool_calls":[{"index":1,"id":"call_2","type":"function","extra_content":`+second+
						`,"function":{"name":"weather","arguments":"{\"city\":\"London\"}"}}]}`)
			}}
			recorder := httptest.NewServer(api)
			t.Cleanup(recorder.Close)
			at := func(model string) *Provider {
				return newProvider(t, geminiAPI, func(c *Config) {
					c.Model = model
					c.HTTPClient = &http.Client{Transport: httpxtest.Rewrite(t, recorder)}
				})
			}
			older, newer := at("gemini-2.5-flash"), at("gemini-3.5-flash")

			conversation := userTurn("go")
			own := complete(t, older, stream, conversation)
			conversation.Messages = append(conversation.Messages, own.Message(),
				llm.Message{Role: llm.RoleTool, ToolCallID: "call_1", Content: "sun"},
				llm.Message{Role: llm.RoleTool, ToolCallID: "call_2", Content: "rain"})
			complete(t, newer, false, conversation)

			calls := callsOf(t, assistantSent(t, api.seen()[1].body))
			if len(calls) != 2 {
				t.Fatalf("%d tool calls went, want 2: %v", len(calls), calls)
			}
			sameJSON(t, "the first call's extra_content", calls[0]["extra_content"], signed)
			sameJSON(t, "the first call's provider_specific_fields", calls[0]["provider_specific_fields"], gateway)
			sameJSON(t, "the second call's extra_content", calls[1]["extra_content"], second)
		})
	}
}

// The stand-in is merged into the extra_content a call already carries:
// every other key kept, Google's own included, and a value that is no object
// — which holds nothing Google reads — replaced.
func TestTheStandInKeepsWhatTheCallAlreadyCarries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, extra, want string }{
		{"nothing", ``, standIn},
		{"another google field", `{"google":{"thought":false}}`,
			`{"google":{"thought":false,"thought_signature":"skip_thought_signature_validator"}}`},
		{"another vendor's key", `{"other":{"k":1}}`,
			`{"other":{"k":1},"google":{"thought_signature":"skip_thought_signature_validator"}}`},
		{"an empty signature", `{"google":{"thought_signature":""}}`, standIn},
		{"not an object", `"x"`, standIn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := withStandIn(json.RawMessage(tc.extra))
			if err != nil {
				t.Fatalf("withStandIn: %v", err)
			}
			var value any
			if err := json.Unmarshal(got, &value); err != nil {
				t.Fatalf("withStandIn wrote %s, which is not JSON: %v", got, err)
			}
			sameJSON(t, "the call's extra_content", value, tc.want)
		})
	}
}

// DeepSeek and Moonshot refuse an assistant message without
// reasoning_content, and the first request after a fallback onto either holds
// only turns another member wrote — nothing that could show the host speaks
// the field. Their own APIs are known from the URL, so every assistant turn
// carries the empty string from that first request on, never another backend's
// reading of its own turn. A host whose URL says nothing of the kind —
// OpenAI's, a gateway serving the same models — is sent nothing it did not
// show it reads.
func TestDeepSeekAndMoonshotGetReasoningContentFromTheFirstRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		base, model string
		want        bool
	}{
		{"https://api.deepseek.com", "deepseek-flash", true},
		{"https://api.deepseek.com/v1", "deepseek-pro", true},
		{"https://api.moonshot.ai/v1", "kimi-k3", true},
		{"https://API.Moonshot.CN/v1/", "kimi-k2.7-code", true},
		{"https://api.deepseek.com./v1", "deepseek-flash", true},
		{"https://openrouter.ai/api/v1", "moonshotai/kimi-k3", false},
		{"https://api.together.xyz/v1", "moonshotai/Kimi-K3", false},
		{"https://api.openai.com/v1", "gpt-test", false},
		{"https://api.deepseek.com.example/v1", "deepseek-flash", false},
	} {
		t.Run(tc.base+" "+tc.model, func(t *testing.T) {
			t.Parallel()
			p, api := providerAt(t, tc.base, tc.model, answerOK)
			conversation := userTurn("go")
			conversation.Messages = append(conversation.Messages, foreignStep("call_1")...)
			complete(t, p, false, conversation)
			sent := assistantSent(t, api.seen()[0].body)
			got, carried := sent["reasoning_content"]
			if carried != tc.want {
				t.Fatalf("reasoning_content carried = %v (%v), want carried = %v", carried, got, tc.want)
			}
			if carried && got != "" {
				t.Fatalf("a turn another backend wrote went with reasoning_content %q, want the empty string", got)
			}
		})
	}
}
