package cliagent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Envelope is what a reply parses into: the prose the operator sees, and the
// tool calls the loop runs.
type Envelope struct {
	Message   string
	ToolCalls []EnvelopeCall
	// Parsed is false when nothing in the reply was an envelope. The
	// caller then hands the whole reply back as assistant content with no
	// tool calls, and the tool loop's own tool_choice="required"
	// corrective re-prompt takes over: a malformed reply costs one round,
	// it never fails a turn.
	Parsed bool
}

// EnvelopeCall is one requested tool call.
type EnvelopeCall struct {
	Name      string
	Arguments map[string]any
}

// messageKeys are the synonyms accepted for the prose field.
//
// Four of them because models reach for all four under a contract that names
// one, and refusing three of them buys nothing: the contract is a request, not
// a schema the vendor enforces. Ordered by how strongly each implies "the
// note to the operator" rather than "the whole answer".
var messageKeys = []string{"message", "content", "text", "response"}

// callKeys are the synonyms accepted for the tool-call list.
var callKeys = []string{"tool_calls", "toolCalls", "tools", "calls"}

// argumentKeys are the synonyms accepted for one call's arguments.
var argumentKeys = []string{"arguments", "args", "input", "parameters"}

// nameKeys are the synonyms accepted for one call's tool name.
var nameKeys = []string{"name", "tool", "tool_name", "function"}

// ParseEnvelope reads a CLI's reply into an [Envelope].
//
// Deliberately forgiving, in a specific direction: it accepts every shape a
// model plausibly produces under the contract, and it NEVER errors. The
// alternative — a strict parser that fails a turn on a stray prose sentence
// before the fence — trades a one-round correction for an incident, and the
// tool loop already has the corrective re-prompt this leans on.
//
// The order matters. A fenced block is tried first and the LAST one wins,
// because a model that reasons in prose and then answers puts the answer at
// the end; taking the first would return its worked example.
func ParseEnvelope(reply string) Envelope {
	for _, candidate := range envelopeCandidates(reply) {
		var doc map[string]any
		if err := json.Unmarshal([]byte(candidate), &doc); err != nil {
			continue
		}
		env, ok := fromDocument(doc)
		if ok {
			return env
		}
	}
	return Envelope{Message: reply}
}

// envelopeCandidates yields the JSON documents in a reply, most likely first:
// every fenced block from last to first, then the reply's own outermost
// braces, then the reply verbatim.
func envelopeCandidates(reply string) []string {
	var out []string
	blocks := fencedBlocks(reply)
	for i := len(blocks) - 1; i >= 0; i-- {
		out = append(out, blocks[i])
	}
	if bare, ok := outermostObject(reply); ok {
		out = append(out, bare)
	}
	trimmed := strings.TrimSpace(reply)
	if trimmed != "" {
		out = append(out, trimmed)
	}
	return out
}

// fencedBlocks returns the contents of every ``` fence, in order.
//
// The info string is ignored rather than required to be "json": models label
// these ```json, ```JSON, ```jsonc and sometimes not at all, and a fence whose
// body is a JSON object is an envelope whatever the label says.
func fencedBlocks(reply string) []string {
	var out []string
	rest := reply
	for {
		open := strings.Index(rest, "```")
		if open < 0 {
			return out
		}
		rest = rest[open+3:]
		// Skip the info string: everything to the end of that line.
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		} else {
			return out
		}
		end := strings.Index(rest, "```")
		if end < 0 {
			// An unterminated fence is the common truncation shape, and
			// its body is still the best candidate in the reply.
			out = append(out, strings.TrimSpace(rest))
			return out
		}
		out = append(out, strings.TrimSpace(rest[:end]))
		rest = rest[end+3:]
	}
}

// outermostObject returns the span from the first '{' to the last '}'.
func outermostObject(reply string) (string, bool) {
	start := strings.IndexByte(reply, '{')
	end := strings.LastIndexByte(reply, '}')
	if start < 0 || end <= start {
		return "", false
	}
	return reply[start : end+1], true
}

// fromDocument reads a decoded object as an envelope, reporting whether it
// looked like one at all.
//
// A JSON object that carries neither a message synonym nor a call synonym is
// NOT an envelope — it is the model answering a question with JSON — so it is
// rejected here and the reply is handed back as prose. Accepting it would
// turn every JSON answer into an empty envelope and silently drop the answer.
func fromDocument(doc map[string]any) (Envelope, bool) {
	env := Envelope{Parsed: true}
	found := false

	for _, key := range messageKeys {
		v, ok := doc[key]
		if !ok {
			continue
		}
		if s, isString := v.(string); isString {
			env.Message = s
			found = true
			break
		}
	}

	for _, key := range callKeys {
		v, ok := doc[key]
		if !ok {
			continue
		}
		calls, isList := v.([]any)
		if !isList {
			continue
		}
		found = true
		for _, raw := range calls {
			call, ok := readCall(raw)
			if ok {
				env.ToolCalls = append(env.ToolCalls, call)
			}
		}
		break
	}

	return env, found
}

// readCall reads one tool call.
func readCall(raw any) (EnvelopeCall, bool) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return EnvelopeCall{}, false
	}
	var call EnvelopeCall
	for _, key := range nameKeys {
		if s, isString := obj[key].(string); isString && s != "" {
			call.Name = s
			break
		}
	}
	if call.Name == "" {
		// A call with no name is not runnable and the tool loop would
		// reject it one layer later with a worse message.
		return EnvelopeCall{}, false
	}
	for _, key := range argumentKeys {
		v, ok := obj[key]
		if !ok {
			continue
		}
		args, ok := readArguments(v)
		if ok {
			call.Arguments = args
			break
		}
	}
	if call.Arguments == nil {
		call.Arguments = map[string]any{}
	}
	return call, true
}

// readArguments reads one call's arguments, accepting both an object and a
// JSON string holding one.
//
// The string form is not a model quirk to tolerate grudgingly: it is what the
// OpenAI tool-call wire format uses, so a model that has seen that format
// reproduces it faithfully. Rejecting it would fail the calls from the models
// that had learned the convention best.
func readArguments(v any) (map[string]any, bool) {
	switch typed := v.(type) {
	case map[string]any:
		return typed, true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return map[string]any{}, true
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(trimmed), &args); err == nil {
			return args, true
		}
		return nil, false
	case nil:
		return map[string]any{}, true
	default:
		return nil, false
	}
}

// RenderContract is the response contract appended to a prompt that offers
// tools. A call with NO tools gets no contract at all: auxiliary work
// (summarisation, the relevance filter) sends a plain prompt and reads a plain
// answer, with no envelope to get wrong.
func RenderContract(required bool) string {
	var b strings.Builder
	b.WriteString("## Response contract\n\n")
	b.WriteString("Reply with ONE fenced json block and nothing outside it:\n\n")
	b.WriteString("```json\n")
	b.WriteString(`{"message": "a short note for the operator, or an empty string", `)
	b.WriteString(`"tool_calls": [{"name": "tool_name", "arguments": {"argument": "value"}}]}`)
	b.WriteString("\n```\n\n")
	if required {
		b.WriteString("You MUST request at least one tool call. " +
			"An empty tool_calls list is not an acceptable answer to this turn.\n")
	} else {
		b.WriteString("Use an empty tool_calls list when no tool is needed.\n")
	}
	return b.String()
}

// RenderTools is the tool catalogue a prompt carries in place of a native
// tool-call channel.
func RenderTools(tools []toolSpec) (string, error) {
	if len(tools) == 0 {
		return "", nil
	}
	encoded, err := json.MarshalIndent(tools, "", "  ")
	if err != nil {
		return "", fmt.Errorf("cli-agent: encoding the tool catalogue: %w", err)
	}
	var b strings.Builder
	b.WriteString("## Available tools\n\n```json\n")
	b.Write(encoded)
	b.WriteString("\n```\n")
	return b.String(), nil
}

// toolSpec is one tool as the catalogue renders it.
type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}
