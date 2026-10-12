package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// --- the replay harness --------------------------------------------------
//
// Every case here runs the round trip a tool loop runs: a first call whose
// response the host shaped, the turn it becomes (llm.Completion.Message), and
// a second call carrying that turn back — asserted on the wire, field by
// field, because what a host refuses is a request body.

// unaryAnswer is a whole completion whose one message is message.
func unaryAnswer(message string) string {
	return fmt.Sprintf(`{
		"id":"c","object":"chat.completion","created":1,"model":"m",
		"choices":[{"index":0,"finish_reason":"tool_calls","message":%s}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
	}`, message)
}

// streamAnswer writes deltas as a chat-completions stream, the last chunk
// carrying the finish reason, then the usage chunk and [DONE].
func streamAnswer(w http.ResponseWriter, deltas ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for i, delta := range deltas {
		finish := ""
		if i == len(deltas)-1 {
			finish = `,"finish_reason":"tool_calls"`
		}
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,"model":"m",`+
			`"choices":[{"index":0,"delta":`+delta+finish+`}]}`+"\n\n")
	}
	_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,"model":"m",`+
		`"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// roundTrip has an endpoint answer the first call with first, and returns
// the turn that answer became and the assistant message the second call — a
// tool result later, on the same provider — carried back.
func roundTrip(t *testing.T, stream bool, first func(w http.ResponseWriter)) (*llm.Completion, map[string]any) {
	t.Helper()
	api, url := serve(t, func(w http.ResponseWriter, n int) {
		if n == 1 {
			first(w)
			return
		}
		writeJSON(w, 200, okCompletion("done"))
	})
	p := newProvider(t, url, nil)
	turn := complete(t, p, stream, userTurn("go"))
	next := userTurn("go")
	next.Messages = append(next.Messages, turn.Message(), llm.Message{
		Role: llm.RoleTool, ToolCallID: firstCallID(turn), Content: "result",
	})
	if _, err := p.Complete(context.Background(), next); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	return turn, assistantSent(t, api.seen()[1].body)
}

func complete(t *testing.T, p *Provider, stream bool, req llm.Request) *llm.Completion {
	t.Helper()
	if stream {
		req.OnDelta = func(llm.Delta) {}
	}
	out, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return out
}

func firstCallID(c *llm.Completion) string {
	if len(c.ToolCalls) == 0 {
		return ""
	}
	return c.ToolCalls[0].ID
}

// assistantSent is the one assistant message a request carried.
func assistantSent(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	found := assistantsSent(body)
	if len(found) != 1 {
		t.Fatalf("the request carried %d assistant messages, want 1: %v", len(found), body["messages"])
	}
	return found[0]
}

// assistantsSent is every assistant message a request carried, in order.
func assistantsSent(body map[string]any) []map[string]any {
	var found []map[string]any
	for _, m := range body["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "assistant" {
			found = append(found, msg)
		}
	}
	return found
}

// sameJSON asserts got is the JSON value want spells — compared as values, so
// a key order or a space is not a difference, and a dropped field, a changed
// signature or a re-ordered block is.
func sameJSON(t *testing.T, what string, got any, want string) {
	t.Helper()
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("%s: the expectation is not JSON: %v", what, err)
	}
	if !reflect.DeepEqual(got, wantValue) {
		encoded, _ := json.Marshal(got)
		t.Fatalf("%s went back as\n  %s\nwant\n  %s", what, encoded, want)
	}
}

// --- what goes back, per host shape --------------------------------------

// Kimi K3 is trained to read the reasoning of every earlier assistant message
// and cannot be told otherwise: Moonshot documents reasoning_content as
// "required in multi-turn conversations and tool-call loops", and DeepSeek
// answers a tool loop without it with a 400. The turn goes back with it, in the
// field it came in, whether the round arrived whole or streamed.
func TestReasoningContentGoesBackWithItsToolCall(t *testing.T) {
	t.Parallel()
	const trace = "The user wants the weather; I need a date first."
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			turn, sent := roundTrip(t, stream, func(w http.ResponseWriter) {
				if !stream {
					writeJSON(w, 200, unaryAnswer(`{"role":"assistant","content":"",
						"reasoning_content":"`+trace+`",
						"tool_calls":[{"id":"call_1","type":"function",
							"function":{"name":"get_date","arguments":"{}"}}]}`))
					return
				}
				streamAnswer(w,
					`{"role":"assistant","reasoning_content":"The user wants the weather; "}`,
					`{"reasoning_content":"I need a date first."}`,
					`{"tool_calls":[{"index":0,"id":"call_1","type":"function",`+
						`"function":{"name":"get_date","arguments":"{}"}}]}`)
			})
			if turn.ReasoningContent != trace {
				t.Fatalf("ReasoningContent = %q, want %q", turn.ReasoningContent, trace)
			}
			if sent["reasoning_content"] != trace {
				t.Fatalf("reasoning_content went back as %v, want %q", sent["reasoning_content"], trace)
			}
			if _, invented := sent["reasoning"]; invented {
				t.Fatalf("a field the host never sent went back: %v", sent)
			}
			call := sent["tool_calls"].([]any)[0].(map[string]any)
			if call["id"] != "call_1" || dig(t, call, "function", "name") != "get_date" {
				t.Fatalf("the tool call went back as %v", call)
			}
		})
	}
}

// OpenRouter answers with `reasoning` beside `reasoning_details` — readable
// text with its signature, an encrypted payload — and the details' sequence
// "must match the outputs generated by the model during the original request".
// Both go back exactly as they came, encrypted data and signatures included.
func TestOpenRouterReasoningDetailsGoBackVerbatim(t *testing.T) {
	t.Parallel()
	const details = `[
		{"type":"reasoning.text","text":"Let me check the date.","signature":"c2lnLTE=",
		 "id":"rs_1","format":"anthropic-claude-v1","index":0},
		{"type":"reasoning.encrypted","data":"ZW5jcnlwdGVk","id":"rs_2",
		 "format":"openai-responses-v1","index":1}
	]`
	turn, sent := roundTrip(t, false, func(w http.ResponseWriter) {
		writeJSON(w, 200, unaryAnswer(`{"role":"assistant","content":null,"refusal":null,
			"reasoning":"Let me check the date.","reasoning_details":`+details+`,
			"tool_calls":[{"id":"call_1","type":"function",
				"function":{"name":"get_date","arguments":"{}"}}]}`))
	})
	if turn.ReasoningContent != "Let me check the date." {
		t.Fatalf("ReasoningContent = %q", turn.ReasoningContent)
	}
	sameJSON(t, "reasoning_details", sent["reasoning_details"], details)
	if sent["reasoning"] != "Let me check the date." {
		t.Fatalf("reasoning went back as %v", sent["reasoning"])
	}
	// Only the reasoning: a response-only field of the host's (refusal,
	// null here) is not echoed into the request.
	if _, echoed := sent["refusal"]; echoed {
		t.Fatalf("a response-only field went back: %v", sent)
	}
}

// A STREAM sends the same details in fragments, and what goes back is what a
// whole response would have carried: one block per block the model wrote — a
// text block's fragments joined, the signature that closes it landing on it
// even in a fragment of its own — and an encrypted block whole.
func TestAStreamedTurnsDetailsGoBackAsOneBlockEach(t *testing.T) {
	t.Parallel()
	turn, sent := roundTrip(t, true, func(w http.ResponseWriter) {
		streamAnswer(w,
			`{"role":"assistant","reasoning":"Let me ","reasoning_details":[{"type":"reasoning.text",`+
				`"text":"Let me ","signature":null,"id":"rs_1","format":"anthropic-claude-v1","index":0}]}`,
			`{"reasoning":"check.","reasoning_details":[{"type":"reasoning.text","text":"check.",`+
				`"signature":null,"id":"rs_1","format":"anthropic-claude-v1","index":0}]}`,
			`{"reasoning_details":[{"type":"reasoning.text","signature":"c2lnLTE=",`+
				`"format":"anthropic-claude-v1","index":0}]}`,
			`{"reasoning_details":[{"type":"reasoning.text","text":"A second thought.",`+
				`"signature":null,"format":"anthropic-claude-v1","index":1}]}`,
			`{"reasoning_details":[{"type":"reasoning.encrypted","data":"ZW5j","index":2}]}`,
			`{"reasoning_details":[{"type":"reasoning.encrypted","data":"cnlwdGVk","index":3}]}`,
			// Two summary parts, unsigned: only their numbering tells them
			// apart, and a whole response lists them apart.
			`{"reasoning_details":[{"type":"reasoning.summary","summary":"Part one","index":4}]}`,
			`{"reasoning_details":[{"type":"reasoning.summary","summary":".","index":4}]}`,
			`{"reasoning_details":[{"type":"reasoning.summary","summary":"Part two.","index":5}]}`,
			`{"tool_calls":[{"index":0,"id":"call_1","type":"function",`+
				`"function":{"name":"get_date","arguments":"{}"}}]}`)
	})
	if turn.ReasoningContent != "Let me check." {
		t.Fatalf("ReasoningContent = %q", turn.ReasoningContent)
	}
	sameJSON(t, "reasoning_details", sent["reasoning_details"], `[
		{"type":"reasoning.text","text":"Let me check.","signature":"c2lnLTE=",
		 "id":"rs_1","format":"anthropic-claude-v1","index":0},
		{"type":"reasoning.text","text":"A second thought.","signature":null,
		 "format":"anthropic-claude-v1","index":1},
		{"type":"reasoning.encrypted","data":"ZW5j","index":2},
		{"type":"reasoning.encrypted","data":"cnlwdGVk","index":3},
		{"type":"reasoning.summary","summary":"Part one.","index":4},
		{"type":"reasoning.summary","summary":"Part two.","index":5}
	]`)
	if sent["reasoning"] != "Let me check." {
		t.Fatalf("reasoning went back as %v", sent["reasoning"])
	}
}

// Gemini 3 behind Google's OpenAI-compatible endpoint signs a function call,
// on the call itself (`extra_content.google.thought_signature`), and answers a
// step without it with a 400; only the first of parallel calls carries one.
// LiteLLM carries the same signature as the call's `provider_specific_fields`.
// Each goes back on its own call, and a call that had none gets none.
func TestAToolCallsSignatureGoesBackOnThatCall(t *testing.T) {
	t.Parallel()
	const google = `{"google":{"thought_signature":"PFNJR05BVFVSRV9BPg=="}}`
	const litellm = `{"thought_signature":"Q3BjSEFkSHRpbTk="}`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			_, sent := roundTrip(t, stream, func(w http.ResponseWriter) {
				if !stream {
					writeJSON(w, 200, unaryAnswer(`{"role":"assistant","tool_calls":[
						{"id":"call_1","type":"function","extra_content":`+google+`,
						 "function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}},
						{"id":"call_2","type":"function",
						 "function":{"name":"weather","arguments":"{\"city\":\"London\"}"}},
						{"id":"call_3","type":"function","provider_specific_fields":`+litellm+`,
						 "function":{"name":"weather","arguments":"{\"city\":\"Rome\"}"}}]}`))
					return
				}
				streamAnswer(w,
					`{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function",`+
						`"extra_content":`+google+`,"function":{"name":"weather","arguments":""}}]}`,
					`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}]}`,
					`{"tool_calls":[{"index":1,"id":"call_2","type":"function",`+
						`"function":{"name":"weather","arguments":"{\"city\":\"London\"}"}}]}`,
					`{"tool_calls":[{"index":2,"id":"call_3","type":"function","provider_specific_fields":`+
						litellm+`,"function":{"name":"weather","arguments":"{\"city\":\"Rome\"}"}}]}`)
			})
			calls := sent["tool_calls"].([]any)
			if len(calls) != 3 {
				t.Fatalf("%d tool calls went back, want 3: %v", len(calls), calls)
			}
			sameJSON(t, "call_1's extra_content", calls[0].(map[string]any)["extra_content"], google)
			if _, invented := calls[1].(map[string]any)["extra_content"]; invented {
				t.Fatalf("a call that had no signature went back with one: %v", calls[1])
			}
			sameJSON(t, "call_3's provider_specific_fields",
				calls[2].(map[string]any)["provider_specific_fields"], litellm)
		})
	}
}

// A streamed call the host did not number is still the call the SDK assembles,
// and its signature goes back on it. Google's OpenAI-compatible endpoint has
// been reported streaming tool calls with no `index` (and Bedrock numbers a
// lone call -1); the SDK's accumulator files either at position 0, so the
// fields kept beside it are read by the same rule or they are matched to
// nothing — while the same response, whole, kept them.
func TestAnUnnumberedStreamedCallKeepsItsSignature(t *testing.T) {
	t.Parallel()
	const google = `{"google":{"thought_signature":"PFNJR05BVFVSRV9BPg=="}}`
	for _, tc := range []struct{ name, index string }{
		{"no index", ``},
		{"index -1", `"index":-1,`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			turn, sent := roundTrip(t, true, func(w http.ResponseWriter) {
				streamAnswer(w, `{"role":"assistant","tool_calls":[{`+tc.index+`"id":"call_1","type":"function",`+
					`"extra_content":`+google+`,"function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]}`)
			})
			if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].ID != "call_1" {
				t.Fatalf("the stream assembled %+v, want call_1", turn.ToolCalls)
			}
			sameJSON(t, "call_1's extra_content", sent["tool_calls"].([]any)[0].(map[string]any)["extra_content"], google)
		})
	}
}

// A LiteLLM proxy fronting Claude answers with `thinking_blocks`, signed, and
// Anthropic refuses a tool result whose call came back without them. Streamed,
// the block's fragments and the signature that closes it are one block again.
func TestThinkingBlocksGoBackSigned(t *testing.T) {
	t.Parallel()
	turn, sent := roundTrip(t, true, func(w http.ResponseWriter) {
		streamAnswer(w,
			`{"role":"assistant","reasoning_content":"Three cities, ",`+
				`"thinking_blocks":[{"type":"thinking","thinking":"Three cities, ","signature":""}]}`,
			`{"reasoning_content":"three calls.",`+
				`"thinking_blocks":[{"type":"thinking","thinking":"three calls.","signature":""}]}`,
			`{"thinking_blocks":[{"type":"thinking","thinking":"","signature":"RXFvQkNrZ0lB"}]}`,
			// A second block of the same type, numbered by nothing: the
			// signature above closed the first, so this one is new.
			`{"thinking_blocks":[{"type":"thinking","thinking":"Then Paris.","signature":""}]}`,
			`{"thinking_blocks":[{"type":"thinking","thinking":"","signature":"U2Vjb25k"}]}`,
			`{"thinking_blocks":[{"type":"redacted_thinking","data":"cmVkYWN0ZWQ="}]}`,
			`{"tool_calls":[{"index":0,"id":"toolu_1","type":"function",`+
				`"function":{"name":"weather","arguments":"{}"}}]}`)
	})
	if turn.ReasoningContent != "Three cities, three calls." {
		t.Fatalf("ReasoningContent = %q", turn.ReasoningContent)
	}
	sameJSON(t, "thinking_blocks", sent["thinking_blocks"], `[
		{"type":"thinking","thinking":"Three cities, three calls.","signature":"RXFvQkNrZ0lB"},
		{"type":"thinking","thinking":"Then Paris.","signature":"U2Vjb25k"},
		{"type":"redacted_thinking","data":"cmVkYWN0ZWQ="}
	]`)
	if sent["reasoning_content"] != "Three cities, three calls." {
		t.Fatalf("reasoning_content went back as %v", sent["reasoning_content"])
	}
}

// Mistral's reasoning models put the thinking IN the content, as a list of
// typed chunks, and ask for "the full assistant message (including
// ThinkChunk)" back. Read as the string OpenAI's schema promises, that list
// arrived as its own JSON text — the model's thinking printed as its answer.
// The visible text is the text chunks, the reasoning is the thinking chunk, and
// the list goes back as the list.
func TestAChunkedContentIsReadAndGoesBackAsChunks(t *testing.T) {
	t.Parallel()
	const chunks = `[
		{"type":"thinking","thinking":[{"type":"text","text":"Seventeen times twenty-three."}]},
		{"type":"text","text":"It is 391."}
	]`
	t.Run("whole", func(t *testing.T) {
		t.Parallel()
		turn, sent := roundTrip(t, false, func(w http.ResponseWriter) {
			writeJSON(w, 200, unaryAnswer(`{"role":"assistant","content":`+chunks+`,
				"tool_calls":[{"id":"c1","type":"function","function":{"name":"check","arguments":"{}"}}]}`))
		})
		if turn.Content != "It is 391." || turn.ReasoningContent != "Seventeen times twenty-three." {
			t.Fatalf("read as content %q, reasoning %q", turn.Content, turn.ReasoningContent)
		}
		sameJSON(t, "content", sent["content"], chunks)
	})
	t.Run("streamed", func(t *testing.T) {
		t.Parallel()
		var shown, thought strings.Builder
		api, url := serve(t, func(w http.ResponseWriter, n int) {
			if n > 1 {
				writeJSON(w, 200, okCompletion("done"))
				return
			}
			// Mistral's stream: the thinking as chunk lists, then one list
			// closing it and opening the answer, then plain strings.
			streamAnswer(w,
				`{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"Seventeen "}]}]}`,
				`{"content":[{"type":"thinking","thinking":[{"type":"text","text":"times twenty-three."}]}]}`,
				`{"content":[{"type":"thinking","thinking":[],"closed":true},{"type":"text","text":"It is "}]}`,
				`{"content":"391."}`,
				`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"check","arguments":"{}"}}]}`)
		})
		p := newProvider(t, url, nil)
		req := userTurn("go")
		req.OnDelta = func(d llm.Delta) {
			shown.WriteString(d.Content)
			thought.WriteString(d.Reasoning)
		}
		turn, err := p.Complete(context.Background(), req)
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if turn.Content != "It is 391." || turn.ReasoningContent != "Seventeen times twenty-three." {
			t.Fatalf("read as content %q, reasoning %q", turn.Content, turn.ReasoningContent)
		}
		if shown.String() != "It is 391." || thought.String() != "Seventeen times twenty-three." {
			t.Fatalf("the live view showed content %q, reasoning %q — a chunk list's JSON is not text",
				shown.String(), thought.String())
		}
		next := userTurn("go")
		next.Messages = append(next.Messages, turn.Message(),
			llm.Message{Role: llm.RoleTool, ToolCallID: "c1", Content: "ok"})
		if _, err := p.Complete(context.Background(), next); err != nil {
			t.Fatalf("second Complete: %v", err)
		}
		sameJSON(t, "content", assistantSent(t, api.seen()[1].body)["content"], `[
			{"type":"thinking","thinking":[{"type":"text","text":"Seventeen times twenty-three."}],"closed":true},
			{"type":"text","text":"It is 391."}
		]`)
	})
}

// A content that changes shape mid-stream keeps everything it said: text sent
// as a plain string before the first list is the list's first text chunk, not
// dropped when the list takes over.
func TestAContentThatTurnsIntoChunksKeepsItsEarlierText(t *testing.T) {
	t.Parallel()
	turn, sent := roundTrip(t, true, func(w http.ResponseWriter) {
		streamAnswer(w,
			`{"role":"assistant","content":"Checking. "}`,
			`{"content":[{"type":"thinking","thinking":[{"type":"text","text":"which city?"}]},`+
				`{"type":"text","text":"Paris."}]}`,
			`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`)
	})
	if turn.Content != "Checking. Paris." || turn.ReasoningContent != "which city?" {
		t.Fatalf("read as content %q, reasoning %q", turn.Content, turn.ReasoningContent)
	}
	sameJSON(t, "content", sent["content"], `[
		{"type":"text","text":"Checking. "},
		{"type":"thinking","thinking":[{"type":"text","text":"which city?"}]},
		{"type":"text","text":"Paris."}
	]`)
}

// A stream that ends its chunk list with an empty string — a fragment carrying
// only a tool call or the finish, as streams do — adds nothing to it: the
// content goes back as the list a whole response would have held, not with an
// empty text chunk the model never wrote.
func TestAnEmptyFragmentAddsNoChunk(t *testing.T) {
	t.Parallel()
	const thinking = `{"type":"thinking","thinking":[{"type":"text","text":"Which city?"}]}`
	turn, sent := roundTrip(t, true, func(w http.ResponseWriter) {
		streamAnswer(w,
			`{"role":"assistant","content":[`+thinking+`]}`,
			`{"content":"","tool_calls":[{"index":0,"id":"c1","type":"function",`+
				`"function":{"name":"weather","arguments":"{}"}}]}`,
			`{"content":""}`)
	})
	if turn.Content != "" || turn.ReasoningContent != "Which city?" {
		t.Fatalf("read as content %q, reasoning %q", turn.Content, turn.ReasoningContent)
	}
	sameJSON(t, "content", sent["content"], `[`+thinking+`]`)
}

// --- where it goes back, and where it does not ---------------------------

// A field one host emits is one another may refuse — OpenAI's own endpoint
// documents none of these as an input, and a 400 is fatal and not retried —
// so a turn's reasoning goes back to the ENDPOINT that wrote it and to no
// other: not to another host a chain falls back to, whatever the model.
// To the same endpoint it goes back whatever the model and whatever the key,
// and however the base URL is spelled.
func TestReasoningGoesBackOnlyToTheEndpointThatWroteIt(t *testing.T) {
	t.Parallel()
	_, writer := serve(t, func(w http.ResponseWriter, _ int) {
		writeJSON(w, 200, unaryAnswer(`{"role":"assistant","reasoning_content":"mine",
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}`))
	})
	turn := complete(t, newProvider(t, writer, nil), false, userTurn("go"))
	conversation := userTurn("go")
	conversation.Messages = append(conversation.Messages, turn.Message(),
		llm.Message{Role: llm.RoleTool, ToolCallID: "call_1", Content: "r"})

	for _, tc := range []struct {
		name    string
		baseURL string
		model   string
		key     string
		want    bool
	}{
		{"the same endpoint", writer, "gpt-test", "k1", true},
		{"the same endpoint, another model", writer, "another-model", "k1", true},
		{"the same endpoint, another key", writer, "gpt-test", "k2", true},
		{"the same endpoint, spelled with a slash", writer + "/", "gpt-test", "k1", true},
		{"another endpoint", "", "gpt-test", "k1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := &fakeAPI{handle: func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("ok")) }}
			recorder := httptest.NewServer(api)
			t.Cleanup(recorder.Close)
			p := newProvider(t, recorder.URL, func(c *Config) {
				c.Model, c.APIKeys = tc.model, []string{tc.key}
				if tc.baseURL != "" {
					// The provider is BUILT for the writer's endpoint
					// and only its transport reaches the recorder, so
					// the binding is the writer's and the request is
					// still observable.
					c.BaseURL = tc.baseURL
					c.HTTPClient = &http.Client{Transport: httpxtest.Rewrite(t, recorder)}
				}
			})
			if _, err := p.Complete(context.Background(), conversation); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			sent := assistantSent(t, api.seen()[0].body)
			_, carried := sent["reasoning_content"]
			if carried != tc.want {
				t.Fatalf("reasoning_content carried = %v, want %v: %v", carried, tc.want, sent)
			}
			// The turn itself goes back either way: only its reasoning is
			// the endpoint's to read.
			if len(sent["tool_calls"].([]any)) != 1 {
				t.Fatalf("the turn's call did not go back: %v", sent)
			}
		})
	}
}

// The endpoint's identity is its URL as a host reads it: case-folded scheme
// and host, no trailing slash, no credentials — and still a digest, because a
// base URL may carry a key and the binding rides every parked conversation.
func TestAnEndpointIsItsURLAsAHostReadsIt(t *testing.T) {
	t.Parallel()
	base := endpointBinding("https://openrouter.ai/api/v1")
	for _, same := range []string{
		"https://openrouter.ai/api/v1/",
		"HTTPS://OpenRouter.AI/api/v1",
		" https://openrouter.ai/api/v1 ",
		"https://user:secret@openrouter.ai/api/v1",
	} {
		if endpointBinding(same) != base {
			t.Errorf("%q is another endpoint than https://openrouter.ai/api/v1", same)
		}
	}
	for _, other := range []string{
		"https://api.moonshot.ai/v1",
		"https://openrouter.ai/api/v2",
		"http://openrouter.ai/api/v1",
	} {
		if endpointBinding(other) == base {
			t.Errorf("%q is the same endpoint as https://openrouter.ai/api/v1", other)
		}
	}
	if strings.Contains(endpointBinding("https://gw.example/v1?key=s3cret"), "s3cret") {
		t.Error("the binding carries the URL's credential")
	}
}

// A turn another BACKEND wrote — Anthropic's content blocks in Raw — is rebuilt
// from the neutral view with no reasoning, as it always was: its blocks are in
// no shape an OpenAI-wire host reads.
func TestATurnAnotherBackendWroteGoesBackWithoutItsReasoning(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("ok")) })
	p := newProvider(t, url, nil)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "go"},
		{
			Role: llm.RoleAssistant, Content: "checking", ReasoningContent: "claude's thinking",
			ToolCalls: []llm.ToolCall{{ID: "toolu_1", Name: "f", Arguments: map[string]any{}}},
			Origin:    llm.Origin{Provider: "anthropic", Model: "claude-opus-5-5"},
			Raw: []json.RawMessage{
				json.RawMessage(`{"type":"thinking","thinking":"claude's thinking","signature":"s"}`),
				json.RawMessage(`{"type":"tool_use","id":"toolu_1","name":"f","input":{}}`),
			},
			// The same digest by accident must change nothing: the
			// origin decides whose record Raw is.
			Binding: p.binding,
		},
		{Role: llm.RoleTool, ToolCallID: "toolu_1", Content: "r"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	sent := assistantSent(t, api.seen()[0].body)
	for key := range sent {
		switch key {
		case "role", "content", "tool_calls":
		default:
			t.Fatalf("the rebuilt turn carried %q: %v", key, sent)
		}
	}
}

// A response that carried no reasoning keeps nothing, and its turn goes back
// exactly as a turn always did: OpenAI's own endpoint, which returns none of
// these fields, sees the requests it always saw. A null is how JSON says a
// field is absent, and hosts that print every field they have print nulls.
func TestAResponseWithNothingBesideTheSchemaKeepsNoRecord(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			turn, sent := roundTrip(t, stream, func(w http.ResponseWriter) {
				if !stream {
					writeJSON(w, 200, unaryAnswer(`{"role":"assistant","content":"calling","refusal":null,
						"reasoning":null,"reasoning_content":null,"reasoning_details":null,"annotations":[],
						"tool_calls":[{"id":"call_1","type":"function","extra_content":null,
							"function":{"name":"f","arguments":"{}"}}]}`))
					return
				}
				streamAnswer(w,
					`{"role":"assistant","content":"calling","reasoning":null,"reasoning_content":null}`,
					`{"reasoning_details":null,"tool_calls":[{"index":0,"id":"call_1","type":"function",`+
						`"extra_content":null,"function":{"name":"f","arguments":"{}"}}]}`)
			})
			if turn.Raw != nil || turn.Binding != "" {
				t.Fatalf("kept %s under %q from a response that carried nothing", turn.Raw, turn.Binding)
			}
			for key := range sent {
				switch key {
				case "role", "content", "tool_calls":
				default:
					t.Fatalf("the turn went back carrying %q: %v", key, sent)
				}
			}
			if _, extra := sent["tool_calls"].([]any)[0].(map[string]any)["extra_content"]; extra {
				t.Fatalf("a null field went back: %v", sent)
			}
		})
	}
}

// A round that reasoned nothing still says so, and what it said goes back.
// Moonshot refuses an assistant tool-call message whose reasoning_content is
// missing while thinking is on ("thinking is enabled but reasoning_content is
// missing"), the empty value included, so an empty field the host sent is its
// answer for that round — whole, or as fragments that never carried a byte.
func TestAnEmptyReasoningGoesBackEmpty(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			turn, sent := roundTrip(t, stream, func(w http.ResponseWriter) {
				if !stream {
					writeJSON(w, 200, unaryAnswer(`{"role":"assistant","content":"",
						"reasoning_content":"","reasoning_details":[],
						"tool_calls":[{"id":"call_1","type":"function",
							"function":{"name":"get_date","arguments":"{}"}}]}`))
					return
				}
				streamAnswer(w,
					`{"role":"assistant","content":"","reasoning_content":""}`,
					`{"reasoning_details":[],"tool_calls":[{"index":0,"id":"call_1","type":"function",`+
						`"function":{"name":"get_date","arguments":"{}"}}]}`)
			})
			if turn.ReasoningContent != "" {
				t.Fatalf("ReasoningContent = %q, want none", turn.ReasoningContent)
			}
			if got, carried := sent["reasoning_content"]; !carried || got != "" {
				t.Fatalf("reasoning_content went back as %v (carried %v), want the empty string: %v",
					got, carried, sent)
			}
			sameJSON(t, "reasoning_details", sent["reasoning_details"], `[]`)
		})
	}
}

// ONCE A HOST HAS SHOWN IT SPEAKS reasoning_content, every assistant turn of
// the request carries one. Moonshot refuses a tool-call message without it
// while thinking is on, and DeepSeek wants every earlier turn's "even for
// turns where the model did not perform a tool call" — but a stream that
// reasoned nothing need never name the field, and a turn another endpoint or
// backend wrote has none this host can read. Each goes as the empty string,
// which is the host's own word for no reasoning; never another backend's
// reading of its own turn.
func TestEveryTurnCarriesReasoningContentOnceItsHostSpeaksIt(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, n int) {
		switch n {
		case 1:
			streamAnswer(w,
				`{"role":"assistant","reasoning_content":"Need the date."}`,
				`{"tool_calls":[{"index":0,"id":"call_1","type":"function",`+
					`"function":{"name":"get_date","arguments":"{}"}}]}`)
		case 2:
			// The round reasoned nothing, and its stream never named
			// the field.
			streamAnswer(w,
				`{"role":"assistant","content":""}`,
				`{"tool_calls":[{"index":0,"id":"call_2","type":"function",`+
					`"function":{"name":"weather","arguments":"{}"}}]}`)
		default:
			writeJSON(w, 200, okCompletion("done"))
		}
	})
	p := newProvider(t, url, nil)
	first := complete(t, p, true, userTurn("go"))
	foreign := llm.Message{
		Role: llm.RoleAssistant, Content: "checking", ReasoningContent: "claude's thinking",
		ToolCalls: []llm.ToolCall{{ID: "toolu_1", Name: "f", Arguments: map[string]any{}}},
		Origin:    llm.Origin{Provider: "anthropic", Model: "claude-opus-5-5"},
		Raw:       []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"claude's thinking","signature":"s"}`)},
	}
	conversation := userTurn("go")
	conversation.Messages = append(conversation.Messages,
		first.Message(), llm.Message{Role: llm.RoleTool, ToolCallID: "call_1", Content: "Monday"},
		foreign, llm.Message{Role: llm.RoleTool, ToolCallID: "toolu_1", Content: "r"})
	conversation.OnDelta = func(llm.Delta) {}
	second, err := p.Complete(context.Background(), conversation)
	if err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	conversation.OnDelta = nil
	conversation.Messages = append(conversation.Messages,
		second.Message(), llm.Message{Role: llm.RoleTool, ToolCallID: "call_2", Content: "sun"})
	if _, err := p.Complete(context.Background(), conversation); err != nil {
		t.Fatalf("third Complete: %v", err)
	}

	seen := api.seen()
	for i, want := range [][]string{
		{"Need the date.", ""},
		{"Need the date.", "", ""},
	} {
		sent := assistantsSent(seen[i+1].body)
		if len(sent) != len(want) {
			t.Fatalf("request %d carried %d assistant messages, want %d", i+2, len(sent), len(want))
		}
		for j, msg := range sent {
			if got, carried := msg["reasoning_content"]; !carried || got != want[j] {
				t.Fatalf("request %d, assistant message %d: reasoning_content = %v (carried %v), want %q: %v",
					i+2, j, got, carried, want[j], msg)
			}
		}
	}
}

// Only reasoning_content is filled in. A host that answers in `reasoning` and
// `reasoning_details` (OpenRouter) documents no rule about a turn without
// them, and an invented empty block in its details is a sequence the model
// never produced — so a turn that came without them goes without them.
func TestNoOtherReasoningFieldIsInvented(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, n int) {
		switch n {
		case 1:
			writeJSON(w, 200, unaryAnswer(`{"role":"assistant","reasoning":"Date first.",
				"reasoning_details":[{"type":"reasoning.text","text":"Date first.","index":0}],
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}`))
		case 2:
			writeJSON(w, 200, unaryAnswer(`{"role":"assistant","reasoning":null,
				"tool_calls":[{"id":"call_2","type":"function","function":{"name":"g","arguments":"{}"}}]}`))
		default:
			writeJSON(w, 200, okCompletion("done"))
		}
	})
	p := newProvider(t, url, nil)
	conversation := userTurn("go")
	for _, id := range []string{"call_1", "call_2"} {
		turn := complete(t, p, false, conversation)
		conversation.Messages = append(conversation.Messages, turn.Message(),
			llm.Message{Role: llm.RoleTool, ToolCallID: id, Content: "r"})
	}
	complete(t, p, false, conversation)
	sent := assistantsSent(api.seen()[2].body)
	if len(sent) != 2 {
		t.Fatalf("%d assistant messages went back, want 2", len(sent))
	}
	if sent[0]["reasoning"] != "Date first." {
		t.Fatalf("the first turn's reasoning went back as %v", sent[0]["reasoning"])
	}
	for key := range sent[1] {
		switch key {
		case "role", "content", "tool_calls":
		default:
			t.Fatalf("a turn with no reasoning went back carrying %q: %v", key, sent[1])
		}
	}
}

// A run that SUSPENDS parks its conversation in a row another node may resume
// days later (internal/agent/execstate). The rounds before the suspension are
// still the conversation K3 is reading, so their reasoning goes back from the
// parked row exactly as it would have from memory.
func TestKeptReasoningSurvivesASuspension(t *testing.T) {
	t.Parallel()
	api, url := serve(t, func(w http.ResponseWriter, n int) {
		if n == 1 {
			writeJSON(w, 200, unaryAnswer(`{"role":"assistant","reasoning":"start the run",
				"reasoning_details":[{"type":"reasoning.text","text":"start the run","signature":"c2ln","index":0}],
				"tool_calls":[{"id":"call_run","type":"function","extra_content":{"google":{"thought_signature":"U0lH"}},
					"function":{"name":"run_sandbox","arguments":"{\"brief\":\"x\"}"}}]}`))
			return
		}
		writeJSON(w, 200, okCompletion("done"))
	})
	p := newProvider(t, url, nil)
	turn := complete(t, p, false, userTurn("go"))

	blob, err := execstate.Encode(execstate.State{
		Messages:        []llm.Message{{Role: llm.RoleUser, Content: "go"}, turn.Message()},
		PendingCallID:   "call_run",
		PendingCallName: "run_sandbox",
	})
	if err != nil {
		t.Fatalf("parking: %v", err)
	}
	parked, ok, err := execstate.Decode(blob)
	if err != nil || !ok {
		t.Fatalf("resuming: %v (found %v)", err, ok)
	}
	resumed := llm.Request{Messages: parked.Answer("the run finished")}
	if _, err := p.Complete(context.Background(), resumed); err != nil {
		t.Fatalf("resumed Complete: %v", err)
	}
	sent := assistantSent(t, api.seen()[1].body)
	sameJSON(t, "reasoning_details", sent["reasoning_details"],
		`[{"type":"reasoning.text","text":"start the run","signature":"c2ln","index":0}]`)
	sameJSON(t, "the call's extra_content",
		sent["tool_calls"].([]any)[0].(map[string]any)["extra_content"], `{"google":{"thought_signature":"U0lH"}}`)
}

// A record that is not what this backend writes can only be a parked row
// corrupted in storage. Sent anyway, the request would carry none of what the
// host asked to see again — so it is refused, naming the message.
func TestACorruptRecordIsRefusedNamingTheMessage(t *testing.T) {
	t.Parallel()
	_, url := serve(t, func(w http.ResponseWriter, _ int) { writeJSON(w, 200, okCompletion("ok")) })
	p := newProvider(t, url, nil)
	for _, raw := range [][]json.RawMessage{
		{json.RawMessage(`"not an object"`)},
		{json.RawMessage(`{"tool_calls":"not a list"}`)},
		{json.RawMessage(`{}`), json.RawMessage(`{}`)},
	} {
		_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "go"},
			{Role: llm.RoleAssistant, Content: "x", Origin: llm.Origin{Provider: wireName}, Raw: raw, Binding: p.binding},
		}})
		if err == nil || !strings.Contains(err.Error(), "message 1") {
			t.Fatalf("a corrupt record %s was sent, or refused without naming the message: %v", raw, err)
		}
		if llm.KindOf(err) != llm.KindFatal {
			t.Fatalf("kind = %v, want fatal: no other member can send it either", llm.KindOf(err))
		}
	}
}
