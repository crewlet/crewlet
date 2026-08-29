package cliagent

import (
	"strings"
	"testing"
)

// The parser's contract is that it never errors and never loses the answer.
// Each case here is a shape a model actually produces under the response
// contract; the property being protected is that a malformed reply costs one
// corrective round rather than a failed turn.
func TestParseEnvelopeAcceptsTheShapesModelsProduce(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		reply     string
		message   string
		callNames []string
		parsed    bool
	}{{
		name:      "a fenced block, the shape the contract asks for",
		reply:     "```json\n{\"message\":\"on it\",\"tool_calls\":[{\"name\":\"read\",\"arguments\":{\"p\":\"a\"}}]}\n```",
		message:   "on it",
		callNames: []string{"read"},
		parsed:    true,
	}, {
		name:      "a bare object with no fence",
		reply:     `{"message":"hi","tool_calls":[]}`,
		message:   "hi",
		callNames: nil,
		parsed:    true,
	}, {
		name:      "prose before and after the fence",
		reply:     "Let me think.\n\n```json\n{\"message\":\"\",\"tool_calls\":[{\"name\":\"ls\"}]}\n```\n\nDone.",
		message:   "",
		callNames: []string{"ls"},
		parsed:    true,
	}, {
		name: "the LAST fence wins, because a worked example comes first",
		reply: "Here is the format:\n```json\n{\"message\":\"example\",\"tool_calls\":[{\"name\":\"wrong\"}]}\n```\n" +
			"And my answer:\n```json\n{\"message\":\"real\",\"tool_calls\":[{\"name\":\"right\"}]}\n```",
		message:   "real",
		callNames: []string{"right"},
		parsed:    true,
	}, {
		name:      "arguments as a JSON string, the OpenAI wire convention",
		reply:     "```json\n{\"message\":\"\",\"tool_calls\":[{\"name\":\"grep\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}]}\n```",
		message:   "",
		callNames: []string{"grep"},
		parsed:    true,
	}, {
		name:    "content as a synonym for message",
		reply:   `{"content":"spoken","tool_calls":[]}`,
		message: "spoken",
		parsed:  true,
	}, {
		name:    "an unterminated fence, the common truncation shape",
		reply:   "```json\n{\"message\":\"cut off\",\"tool_calls\":[]}",
		message: "cut off",
		parsed:  true,
	}, {
		name:    "an unlabelled fence",
		reply:   "```\n{\"message\":\"plain\",\"tool_calls\":[]}\n```",
		message: "plain",
		parsed:  true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := ParseEnvelope(tc.reply)
			if env.Parsed != tc.parsed {
				t.Fatalf("Parsed = %v, want %v", env.Parsed, tc.parsed)
			}
			if env.Message != tc.message {
				t.Errorf("Message = %q, want %q", env.Message, tc.message)
			}
			var got []string
			for _, call := range env.ToolCalls {
				got = append(got, call.Name)
			}
			if strings.Join(got, ",") != strings.Join(tc.callNames, ",") {
				t.Errorf("tool calls = %v, want %v", got, tc.callNames)
			}
		})
	}
}

// The failure direction that matters: a reply that is not an envelope must
// come back as the assistant's prose, whole, so the corrective re-prompt has
// something to correct. Losing it would turn a formatting slip into a turn
// that saw an empty answer.
func TestAnUnparseableReplyBecomesContentRatherThanAnError(t *testing.T) {
	t.Parallel()
	for _, reply := range []string{
		"I cannot do that.",
		"```python\nprint('hi')\n```",
		`{"answer": 42}`,
		"",
	} {
		env := ParseEnvelope(reply)
		if env.Parsed {
			t.Errorf("ParseEnvelope(%q).Parsed = true, want false", reply)
		}
		if env.Message != reply {
			t.Errorf("ParseEnvelope(%q).Message = %q, want the reply verbatim", reply, env.Message)
		}
		if len(env.ToolCalls) != 0 {
			t.Errorf("ParseEnvelope(%q) produced %d tool calls", reply, len(env.ToolCalls))
		}
	}
}

// A JSON object that answers a question is NOT an envelope. Accepting it
// would silently drop the answer and hand the turn an empty message.
func TestAJSONAnswerIsNotMistakenForAnEnvelope(t *testing.T) {
	t.Parallel()
	env := ParseEnvelope("```json\n{\"name\":\"Ada\",\"age\":36}\n```")
	if env.Parsed {
		t.Fatalf("a JSON answer parsed as an envelope: %+v", env)
	}
	if !strings.Contains(env.Message, "Ada") {
		t.Errorf("the answer was lost: %q", env.Message)
	}
}

// A call with no name cannot be run, and letting it through would fail one
// layer later with a message that no longer names the reply it came from.
func TestACallWithoutANameIsDropped(t *testing.T) {
	t.Parallel()
	env := ParseEnvelope(`{"message":"","tool_calls":[{"arguments":{"a":1}},{"name":"good"}]}`)
	if len(env.ToolCalls) != 1 || env.ToolCalls[0].Name != "good" {
		t.Fatalf("tool calls = %+v, want only the named one", env.ToolCalls)
	}
}

// Arguments are never nil, so the tool loop can index into them without a
// check at every call site.
func TestArgumentsAreAlwaysAMap(t *testing.T) {
	t.Parallel()
	for _, reply := range []string{
		`{"tool_calls":[{"name":"a"}]}`,
		`{"tool_calls":[{"name":"a","arguments":null}]}`,
		`{"tool_calls":[{"name":"a","arguments":""}]}`,
	} {
		env := ParseEnvelope(reply)
		if len(env.ToolCalls) != 1 {
			t.Fatalf("%s: got %d calls", reply, len(env.ToolCalls))
		}
		if env.ToolCalls[0].Arguments == nil {
			t.Errorf("%s: Arguments is nil", reply)
		}
	}
}

// The corrective re-prompt has to SAY it is corrective, or the model answers
// with prose a second time and the round is wasted.
func TestTheRequiredContractDemandsAToolCall(t *testing.T) {
	t.Parallel()
	if strings.Contains(RenderContract(false), "MUST") {
		t.Error("the permissive contract demands a tool call")
	}
	if !strings.Contains(RenderContract(true), "MUST") {
		t.Error("the required contract does not demand a tool call")
	}
}
