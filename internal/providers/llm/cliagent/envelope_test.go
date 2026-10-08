package cliagent

import (
	"encoding/json"
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

// THE CONTRACT NEVER DEMANDS A CALL. A request cannot force one, so a phase
// that must end in a call names it in the conversation and the tool loop asks
// again; a contract telling the model an empty list is unacceptable would be a
// second, stricter protocol that only this backend speaks.
func TestTheContractNeverDemandsAToolCall(t *testing.T) {
	t.Parallel()
	contract := RenderContract()
	if strings.Contains(contract, "MUST") {
		t.Errorf("the contract demands a tool call:\n%s", contract)
	}
	if !strings.Contains(contract, "empty tool_calls list when no tool is needed") {
		t.Errorf("the contract does not say a call may be omitted:\n%s", contract)
	}
}

// A call list nothing could be read from is NOT an envelope.
//
// The distinction is what happens next. A document that is not an envelope
// becomes assistant prose and, in a phase that has to end in a call, the tool
// loop's corrective asks again — one round, and the model reliably fixes it. Accepted as an envelope
// instead, the same reply reported that the model asked for NO tools when it
// had asked for several, so the turn ended on a confident message with nothing
// delivered.
func TestACallListNothingCouldBeReadFromIsNotAnEnvelope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, reply string }{
		{"entries are strings, not objects",
			`{"message":"posting it now","tool_calls":["mattermost_post_message"]}`},
		{"every entry is nameless",
			`{"message":"posting it now","tool_calls":[{"arguments":{"channel":"c"}}]}`},
		// A call key holding something that is neither a list nor an
		// object. Skipped, it left the message to decide the verdict and
		// the reply parsed as an envelope that asked for nothing.
		{"the call key holds a string",
			`{"message":"posting it now","tool_calls":"mattermost_post_message"}`},
		{"the call key holds a number",
			`{"message":"posting it now","tool_calls":1}`},
		{"the call key holds one nameless object",
			`{"message":"posting it now","tool_calls":{"arguments":{"channel":"c"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := ParseEnvelope(tc.reply)
			if env.Parsed {
				t.Errorf("parsed as an envelope with %d calls — the request was dropped "+
					"and the message reported as a final answer", len(env.ToolCalls))
			}
			if env.Message != tc.reply {
				t.Errorf("the reply did not fall through as prose: %q", env.Message)
			}
		})
	}
}

// An EMPTY list is an ordinary final answer — the model saying "no calls, here
// is my note" — and must keep parsing.
func TestAnEmptyCallListIsStillAnEnvelope(t *testing.T) {
	t.Parallel()
	env := ParseEnvelope(`{"message":"nothing to do here","tool_calls":[]}`)
	if !env.Parsed {
		t.Fatal("an explicit no-calls answer was refused")
	}
	if env.Message != "nothing to do here" || len(env.ToolCalls) != 0 {
		t.Errorf("message = %q calls = %d", env.Message, len(env.ToolCalls))
	}
}

// ONE CALL WRITTEN WITHOUT ITS LIST IS THAT CALL. A model asked for a list of
// one drops the brackets often enough, and the object names a runnable tool —
// so dropping it would report a model that asked for nothing when it asked for
// exactly one thing, the very shape the unreadable-list rule refuses.
func TestASingleCallObjectIsReadAsAListOfOne(t *testing.T) {
	t.Parallel()
	env := ParseEnvelope(
		`{"message":"posting it now","tool_calls":{"name":"slack_post","arguments":{"channel":"C1"}}}`)
	if !env.Parsed {
		t.Fatal("a single call object was refused")
	}
	if len(env.ToolCalls) != 1 || env.ToolCalls[0].Name != "slack_post" ||
		env.ToolCalls[0].Arguments["channel"] != "C1" {
		t.Errorf("calls = %+v, want the one slack_post call", env.ToolCalls)
	}
	if env.Message != "posting it now" {
		t.Errorf("message = %q", env.Message)
	}
}

// A NULL CALL LIST IS AN EMPTY ONE: the model saying "no calls", which is an
// ordinary final answer exactly as `[]` is.
func TestANullCallListIsAnEmptyOne(t *testing.T) {
	t.Parallel()
	env := ParseEnvelope(`{"message":"nothing to do here","tool_calls":null}`)
	if !env.Parsed || env.Message != "nothing to do here" || len(env.ToolCalls) != 0 {
		t.Errorf("env = %+v, want a parsed envelope with no calls", env)
	}
}

// One unreadable entry beside a readable one keeps the readable one: the
// tool loop's corrective is for a reply that requested nothing this build
// could run, not for a reply with a stray element in its list.
func TestAPartiallyReadableCallListKeepsWhatItCanRun(t *testing.T) {
	t.Parallel()
	env := ParseEnvelope(
		`{"tool_calls":[{"nope":1},{"name":"submit_work","arguments":{"outcome":"delivered"}}]}`)
	if !env.Parsed {
		t.Fatal("a list with one good call was refused")
	}
	if len(env.ToolCalls) != 1 || env.ToolCalls[0].Name != "submit_work" {
		t.Errorf("calls = %+v", env.ToolCalls)
	}
}

// A WIDE ID IS EXECUTED, not displayed, so rounding it runs the call against
// the wrong row with nothing anywhere reporting an error. Every other decoder
// on an argument path in this engine reads through json.Number for that
// reason; this was the one that did not.
func TestAWideArgumentIDSurvivesTheEnvelope(t *testing.T) {
	for _, reply := range []string{
		`{"tool_calls": [{"name": "get_issue", "arguments": {"id": 1234567890123456789}}]}`,
		// The STRING form of an argument list takes the same path, and it is
		// the form a model that learned OpenAI's wire format reproduces.
		`{"tool_calls": [{"name": "get_issue", "arguments": "{\"id\": 1234567890123456789}"}]}`,
	} {
		env := ParseEnvelope(reply)
		if len(env.ToolCalls) != 1 {
			t.Fatalf("ParseEnvelope(%s) gave %d calls, want 1", reply, len(env.ToolCalls))
		}
		raw, err := json.Marshal(env.ToolCalls[0].Arguments)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(raw) != `{"id":1234567890123456789}` {
			t.Errorf("arguments re-encoded as %s, want the id unchanged", raw)
		}
	}
}

// A TAIL IS NOT A DOCUMENT, on the one path where a model's raw text reaches
// the decoder whole: the STRING form of an argument list, which is what a
// model that learned OpenAI's wire format reproduces. A Decoder reads one
// value and stops, so `{"a":1}garbage` would decode clean and the suffix be
// dropped in silence — where the `json.Unmarshal` this replaced refused it and
// the call fell back to no arguments. Refusing is the honest answer: half a
// model's arguments is not its request.
func TestAnArgumentListWithATailIsRefused(t *testing.T) {
	env := ParseEnvelope(`{"tool_calls": [{"name": "get_issue", "arguments": "{\"id\": 1}garbage"}]}`)
	if len(env.ToolCalls) != 1 {
		t.Fatalf("ParseEnvelope gave %d calls, want 1", len(env.ToolCalls))
	}
	if got := env.ToolCalls[0].Arguments; len(got) != 0 {
		t.Errorf("Arguments = %v, want none — the text had a tail", got)
	}
	if env.ToolCalls[0].ArgumentsError == "" {
		t.Error("the refused arguments carry no reason, so the call would run with none")
	}
}

// ARGUMENTS THAT DO NOT READ ARE A REASON, NOT AN EMPTY MAP.
//
// The call is kept — it is what the model asked for — but an empty map
// standing in for arguments it wrote and nobody could read is a search over
// everything or a post with no body. The reason travels instead, and the tool
// loop answers the call with it rather than running it.
func TestUnreadableArgumentsKeepTheCallAndSayWhy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, reply, want string }{
		{"truncated JSON string", `{"tool_calls":[{"name":"search","arguments":"{\"query\": \"x"}]}`, "JSON object"},
		{"a number", `{"tool_calls":[{"name":"search","arguments":42}]}`, "number"},
		{"a list", `{"tool_calls":[{"name":"search","arguments":["x"]}]}`, "list"},
		{"a boolean", `{"tool_calls":[{"name":"search","args":true}]}`, "boolean"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := ParseEnvelope(tc.reply)
			if len(env.ToolCalls) != 1 {
				t.Fatalf("got %d calls, want the one the model asked for", len(env.ToolCalls))
			}
			call := env.ToolCalls[0]
			if !strings.Contains(call.ArgumentsError, tc.want) {
				t.Errorf("ArgumentsError = %q, want it to name a %s", call.ArgumentsError, tc.want)
			}
			if len(call.Arguments) != 0 {
				t.Errorf("Arguments = %v, want none", call.Arguments)
			}
		})
	}
	// And a synonym that DOES read wins over one that does not, so a reply
	// carrying both is the call it reads as, with no error.
	env := ParseEnvelope(`{"tool_calls":[{"name":"search","arguments":42,"input":{"query":"x"}}]}`)
	if len(env.ToolCalls) != 1 || env.ToolCalls[0].ArgumentsError != "" ||
		env.ToolCalls[0].Arguments["query"] != "x" {
		t.Errorf("calls = %+v, want the readable synonym's arguments and no error", env.ToolCalls)
	}
	// Absent, null and empty arguments are NO arguments, not unreadable ones.
	for _, reply := range []string{
		`{"tool_calls":[{"name":"a"}]}`,
		`{"tool_calls":[{"name":"a","arguments":null}]}`,
		`{"tool_calls":[{"name":"a","arguments":""}]}`,
	} {
		if got := ParseEnvelope(reply).ToolCalls[0].ArgumentsError; got != "" {
			t.Errorf("%s: ArgumentsError = %q, want none", reply, got)
		}
	}
}
