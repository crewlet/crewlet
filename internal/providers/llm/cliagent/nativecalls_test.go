package cliagent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/toolmarkup"
)

// offeredTool is a request's catalogue for the tests that only need the
// envelope read at all.
var offeredTool = []llm.ToolDef{{Name: "read"}}

// assistantTools are the Dashboard Assistant's write and read tools as their
// schemas declare them: what decides how a markup parameter's text is typed.
var assistantTools = []llm.ToolDef{{
	Name: "propose_api_change",
	Parameters: map[string]any{"type": "object", "properties": map[string]any{
		"route": map[string]any{"type": "string"},
		"path":  map[string]any{"type": "object"},
		"body":  map[string]any{"type": "object"},
	}},
}, {
	Name: "call_api",
	Parameters: map[string]any{"type": "object", "properties": map[string]any{
		"route": map[string]any{"type": "string"},
		"path":  map[string]any{"type": "object"},
		"limit": map[string]any{"type": "integer"},
	}},
}, {
	Name: "describe_api",
}}

// liveInvokeReply is the final reply of the measured Dashboard Assistant turn
// that showed the person raw markup as its answer (Claude Code, text mode),
// its body shortened.
const liveInvokeReply = `<invoke name="propose_api_change">` +
	`<parameter name="route">PUT /config/units/{id}</parameter>` +
	`<parameter name="path">{"id": "core"}</parameter>` +
	`<parameter name="body">{"key": "core", "name": "Core", "roles": ["engineer", "crewlet-swe"]}</parameter>` +
	`</invoke>`

// A call a model writes in its vendor's own form, naming a tool it was
// offered, is the call it asked for: the round runs it rather than ending
// with the markup as its answer.
func TestACallWrittenInAVendorsFormRunsWhenItNamesAnOfferedTool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		reply   string
		message string
		calls   []string
	}{{
		name:  "the measured live reply",
		reply: liveInvokeReply,
		calls: []string{"propose_api_change"},
	}, {
		name: "inside function_calls, with prose before it",
		reply: "I will add the role now.\n\n<function_calls>\n" + liveInvokeReply +
			"\n</function_calls>",
		message: "I will add the role now.",
		calls:   []string{"propose_api_change"},
	}, {
		name: "namespaced tags, two calls in one block",
		reply: "<ns:function_calls>\n<ns:invoke name=\"describe_api\">\n</ns:invoke>\n" +
			"<ns:invoke name=\"call_api\">\n<ns:parameter name=\"route\">GET /agents</ns:parameter>\n" +
			"</ns:invoke>\n</ns:function_calls>",
		calls: []string{"describe_api", "call_api"},
	}, {
		name:  "a Hermes tool_call tag",
		reply: "<tool_call>\n{\"name\": \"call_api\", \"arguments\": {\"route\": \"GET /agents\"}}\n</tool_call>",
		calls: []string{"call_api"},
	}, {
		name:  "the coder models' function form",
		reply: "<function=call_api>\n<parameter=route>\nGET /agents\n</parameter>\n</function>",
		calls: []string{"call_api"},
	}, {
		name:  "an Anthropic tool_use block",
		reply: `{"type": "tool_use", "id": "toolu_01", "name": "call_api", "input": {"route": "GET /agents"}}`,
		calls: []string{"call_api"},
	}, {
		name: "an Anthropic message of content blocks",
		reply: "```json\n" + `{"role": "assistant", "content": [{"type": "text", "text": "Looking."},` +
			` {"type": "tool_use", "name": "call_api", "input": {"route": "GET /agents"}}]}` + "\n```",
		message: "Looking.",
		calls:   []string{"call_api"},
	}, {
		name: "an OpenAI tool call list",
		reply: `[{"id": "call_1", "type": "function", "function": {"name": "call_api", ` +
			`"arguments": "{\"route\": \"GET /agents\"}"}}]`,
		calls: []string{"call_api"},
	}, {
		name: "OpenAI calls inside the envelope's own list",
		reply: `{"message": "", "tool_calls": [{"type": "function", "function": {"name": "call_api", ` +
			`"arguments": "{\"route\": \"GET /agents\"}"}}]}`,
		calls: []string{"call_api"},
	}, {
		name:    "markup inside the envelope's message",
		reply:   `{"message": "Here it is. ` + strings.ReplaceAll(liveInvokeReply, `"`, `\"`) + `", "tool_calls": []}`,
		message: "Here it is.",
		calls:   []string{"propose_api_change"},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := ParseEnvelope(tc.reply, assistantTools)
			if env.ProtocolError != "" {
				t.Fatalf("ProtocolError = %q, want the calls read", env.ProtocolError)
			}
			if !env.Parsed || env.Message != tc.message {
				t.Errorf("Parsed, Message = %v, %q; want true, %q", env.Parsed, env.Message, tc.message)
			}
			var names []string
			for _, c := range env.ToolCalls {
				names = append(names, c.Name)
			}
			if strings.Join(names, ",") != strings.Join(tc.calls, ",") {
				t.Errorf("calls = %v, want %v", names, tc.calls)
			}
		})
	}
}

// A markup parameter is typed by the tool's schema: a declared string is the
// text as written, anything else its JSON value where it holds one.
func TestAMarkupParameterIsTypedByTheToolsSchema(t *testing.T) {
	t.Parallel()
	env := ParseEnvelope(liveInvokeReply, assistantTools)
	if len(env.ToolCalls) != 1 {
		t.Fatalf("calls = %d, want 1 (%q)", len(env.ToolCalls), env.ProtocolError)
	}
	args := env.ToolCalls[0].Arguments
	if args["route"] != "PUT /config/units/{id}" {
		t.Errorf("route = %#v, want the string as written", args["route"])
	}
	path, ok := args["path"].(map[string]any)
	if !ok || path["id"] != "core" {
		t.Errorf("path = %#v, want the object it holds", args["path"])
	}
	body, ok := args["body"].(map[string]any)
	if !ok || body["key"] != "core" {
		t.Errorf("body = %#v, want the object it holds", args["body"])
	}

	// A string the schema declares stays a string even when it reads as
	// JSON, and an undeclared number keeps every digit.
	env = ParseEnvelope(`<invoke name="call_api"><parameter name="route">42</parameter>`+
		`<parameter name="limit">12345678901234567890</parameter></invoke>`, assistantTools)
	args = env.ToolCalls[0].Arguments
	if args["route"] != "42" {
		t.Errorf("route = %#v, want the string \"42\"", args["route"])
	}
	if n, ok := args["limit"].(json.Number); !ok || n.String() != "12345678901234567890" {
		t.Errorf("limit = %#v, want the number with every digit", args["limit"])
	}
}

// Markup that cannot run is a protocol fault the round runs nothing for,
// never prose: a tool not offered, a call cut off, envelope calls and markup
// together.
func TestMarkupThatCannotRunIsAProtocolErrorAndRunsNothing(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"a tool not offered":                `<invoke name="Bash"><parameter name="command">ls</parameter></invoke>`,
		"a call cut off":                    `<invoke name="call_api"><parameter name="route">GET /agents`,
		"a stray close":                     `I am done.</invoke>`,
		"no name":                           `<invoke><parameter name="route">GET /agents</parameter></invoke>`,
		"a tool_use naming no offered tool": `{"type": "tool_use", "name": "Bash", "input": {"command": "ls"}}`,
		"envelope calls and markup together": `{"message": "` +
			strings.ReplaceAll(liveInvokeReply, `"`, `\"`) +
			`", "tool_calls": [{"name": "describe_api"}]}`,
		"a JSON tag body naming nothing": "<tool_call>{\"arguments\": {}}</tool_call>",
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := ParseEnvelope(reply, assistantTools)
			if env.ProtocolError == "" {
				t.Fatalf("ProtocolError is empty for %q; the markup would be shown as prose", reply)
			}
			if len(env.ToolCalls) != 0 {
				t.Errorf("calls = %v, want none run from a reply that cannot be read whole", env.ToolCalls)
			}
		})
	}
}

// With no tools offered, nothing a reply writes is a call: a vendor-shaped
// document is a protocol fault, and plain prose stays prose.
func TestNoToolIsRunThatTheRequestDidNotOffer(t *testing.T) {
	t.Parallel()
	if env := ParseEnvelope(liveInvokeReply, nil); env.ProtocolError == "" || len(env.ToolCalls) > 0 {
		t.Errorf("a call to an unoffered tool read as %+v", env)
	}
	if env := ParseEnvelope("Plain prose, no calls.", assistantTools); env.ProtocolError != "" ||
		env.Message != "Plain prose, no calls." {
		t.Errorf("prose read as %+v", env)
	}
}

// Markup a model shows as code is an example, not a call, and not a fault.
func TestMarkupInCodeIsNeitherACallNorAFault(t *testing.T) {
	t.Parallel()
	for _, reply := range []string{
		"A call looks like this:\n```xml\n" + liveInvokeReply + "\n```",
		"Write `<invoke name=\"call_api\">` to call it.",
	} {
		env := ParseEnvelope(reply, assistantTools)
		if env.ProtocolError != "" || len(env.ToolCalls) != 0 || env.Message != reply {
			t.Errorf("%q read as %+v, want the prose verbatim", reply, env)
		}
	}
}

// A JSON answer that merely has a type field is not a call.
func TestAJSONAnswerWithATypeFieldIsNotACall(t *testing.T) {
	t.Parallel()
	reply := `{"type": "summary", "name": "call_api", "count": 3}`
	env := ParseEnvelope(reply, assistantTools)
	if env.Parsed || env.ProtocolError != "" || len(env.ToolCalls) != 0 {
		t.Errorf("%q read as %+v, want the JSON answer as prose", reply, env)
	}
}

// FuzzParseEnvelope holds the parser to its invariants on any reply: it never
// panics, it never runs a tool the request did not offer, a protocol error
// runs nothing, and the message of a reply it read calls from never carries
// markup a reader would be shown.
func FuzzParseEnvelope(f *testing.F) {
	for _, seed := range []string{
		liveInvokeReply, "plain prose", `{"message":"hi","tool_calls":[]}`,
		"<function_calls>" + liveInvokeReply, "<tool_call>{\"name\":\"read\"}</tool_call>",
		`<invoke name="read"><parameter name="p">x</parameter></invoke> and </invoke>`,
		"```\n<invoke name=\"read\">\n```", `[{"type":"tool_use","name":"read","input":{}}]`,
		`<ns:invoke name="read"><ns:parameter name="p"><invoke name="x"></ns:parameter></ns:invoke>`,
	} {
		f.Add(seed)
	}
	tools := append([]llm.ToolDef{{Name: "read"}}, assistantTools...)
	f.Fuzz(func(t *testing.T, reply string) {
		env := ParseEnvelope(reply, tools)
		offered := map[string]bool{}
		for _, tool := range tools {
			offered[tool.Name] = true
		}
		if env.ProtocolError != "" && len(env.ToolCalls) > 0 {
			t.Fatalf("a protocol error ran %d calls", len(env.ToolCalls))
		}
		for _, c := range env.ToolCalls {
			if c.Arguments == nil {
				t.Fatalf("call %q has nil arguments", c.Name)
			}
			// Only the strict envelope may name a tool that was not
			// offered (the tool loop answers that call as unknown); a
			// vendor's own form never runs one.
			if _, strict := strictEnvelope(reply); !offered[c.Name] && !strict {
				t.Fatalf("call %q, not offered, was read from a vendor's form", c.Name)
			}
		}
		if env.ProtocolError == "" && len(env.ToolCalls) > 0 && toolmarkup.Index(env.Message) >= 0 {
			t.Fatalf("message %q still carries markup", env.Message)
		}
	})
}
