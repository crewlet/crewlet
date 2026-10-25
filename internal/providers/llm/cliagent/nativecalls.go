package cliagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/toolmarkup"
	"github.com/crewlet/crewlet/internal/textcut"
)

// THE VENDORS' OWN CALL FORMS, read as the envelope's calls ([ParseEnvelope]).
//
// Two families. JSON documents in a vendor's wire shape (an Anthropic tool_use
// block or a message of content blocks, an OpenAI function call) are read
// from the same candidates the envelope is, after it; markup ([toolmarkup])
// is read from the prose that is left, since a model that writes a call as
// markup usually does so in place of the envelope, and sometimes inside its
// message.

// nativeEnvelope reads the reply's candidate documents as a vendor's own call
// shape. It reports false when none is one, and an envelope carrying a
// [Envelope.ProtocolError] when one is but names a tool not offered.
func nativeEnvelope(reply string, tools []llm.ToolDef) (Envelope, bool) {
	for _, candidate := range envelopeCandidates(reply) {
		doc, ok := decodeJSONValue(candidate)
		if !ok {
			continue
		}
		message, calls, ok := nativeDocument(doc)
		if !ok {
			continue
		}
		if why := unoffered(calls, tools); why != "" {
			return Envelope{Message: reply, ProtocolError: why}, true
		}
		return Envelope{Message: message, ToolCalls: calls, Parsed: true}, true
	}
	return Envelope{}, false
}

// nativeDocument reads one decoded document as a vendor's call shape: the
// prose it carries and its calls, or false when it is not one.
//
//   - `{"type": "tool_use", "name": ..., "input": {...}}`, Anthropic's block;
//   - `{"type": "function", "function": {"name": ..., "arguments": "..."}}`,
//     OpenAI's tool call, and `{"function_call": {...}}`, its older form;
//   - `{"content": [blocks]}`, an Anthropic message, its text blocks the
//     prose and its tool_use blocks the calls;
//   - a list of any of these.
//
// A document is one only when it holds at least one call: a message of text
// blocks alone is a JSON answer, handed back as prose by the caller.
func nativeDocument(doc any) (string, []EnvelopeCall, bool) {
	switch typed := doc.(type) {
	case []any:
		var prose []string
		var calls []EnvelopeCall
		for _, item := range typed {
			text, isText := textBlock(item)
			if isText {
				prose = append(prose, text)
				continue
			}
			call, ok := nativeCall(item)
			if !ok {
				return "", nil, false
			}
			calls = append(calls, call)
		}
		return strings.TrimSpace(strings.Join(prose, "\n\n")), calls, len(calls) > 0
	case map[string]any:
		if blocks, ok := typed["content"].([]any); ok {
			return nativeDocument(blocks)
		}
		call, ok := nativeCall(typed)
		if !ok {
			return "", nil, false
		}
		return "", []EnvelopeCall{call}, true
	default:
		return "", nil, false
	}
}

// nativeCall reads one vendor-shaped call object.
func nativeCall(raw any) (EnvelopeCall, bool) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return EnvelopeCall{}, false
	}
	if legacy, isObject := obj["function_call"].(map[string]any); isObject {
		return readCall(legacy)
	}
	switch obj["type"] {
	case "tool_use", "function":
		return readCall(obj)
	}
	return EnvelopeCall{}, false
}

// textBlock reads an Anthropic text block, `{"type": "text", "text": ...}`.
func textBlock(raw any) (string, bool) {
	obj, ok := raw.(map[string]any)
	if !ok || obj["type"] != "text" {
		return "", false
	}
	text, ok := obj["text"].(string)
	return text, ok
}

// withMarkupCalls reads any tool-call markup left in env's prose: as calls
// when every one names an offered tool and reads whole, and as the
// envelope's [Envelope.ProtocolError] otherwise.
func withMarkupCalls(env Envelope, tools []llm.ToolDef) Envelope {
	if env.ProtocolError != "" {
		return env
	}
	text := env.Message
	blocks, err := toolmarkup.Read(text)
	if len(blocks) == 0 && err == nil {
		return env
	}
	fault := func(why string) Envelope {
		return Envelope{Message: text, Parsed: env.Parsed, ProtocolError: why}
	}
	switch {
	case err != nil:
		return fault("it wrote a tool call as markup that does not read as a whole call: " + err.Error())
	case len(env.ToolCalls) > 0:
		return fault("it named tool calls in the envelope and wrote more as markup in its message")
	}
	calls := make([]EnvelopeCall, 0, len(blocks))
	for _, b := range blocks {
		call, ok := markupCall(b, tools)
		if !ok {
			return fault("it wrote a tool call as markup that names no tool: " + jsonPreview(b.JSON))
		}
		calls = append(calls, call)
	}
	if why := unoffered(calls, tools); why != "" {
		return fault(why)
	}
	// What the reply said around its calls is its message, as the envelope's
	// would have been; the reply as a whole was never one only when it had
	// no envelope at all.
	return Envelope{Message: toolmarkup.Prose(text, blocks), ToolCalls: calls, Parsed: true}
}

// markupCall is one markup block as a call: a JSON body read by the envelope's
// own call rule, an `<invoke>` by its parameters.
func markupCall(b toolmarkup.Block, tools []llm.ToolDef) (EnvelopeCall, bool) {
	if b.Name == "" {
		doc, ok := decodeEnvelopeObject(b.JSON)
		if !ok {
			return EnvelopeCall{}, false
		}
		return readCall(doc)
	}
	var schema map[string]any
	if i := slices.IndexFunc(tools, func(t llm.ToolDef) bool { return t.Name == b.Name }); i >= 0 {
		schema = tools[i].Parameters
	}
	args := make(map[string]any, len(b.Params))
	for _, p := range b.Params {
		args[p.Name] = paramValue(p.Value, propertyType(schema, p.Name))
	}
	return EnvelopeCall{Name: b.Name, Arguments: args}, true
}

// paramValue is one markup parameter's value: the text as written for a
// parameter the tool declares a string, and otherwise its JSON value where
// the text is exactly one (an object, a list, a number, a boolean), read
// through json.Number for the reason [decodeEnvelopeObject] gives, else the
// text as written.
//
// The schema decides first because the markup carries no types of its own:
// `<parameter name="id">0042</parameter>` is a string id to a tool that says
// so, and `{"id": "core"}` is an object to a tool that takes one.
func paramValue(raw, declared string) any {
	text := strings.Trim(raw, "\r\n")
	if declared == "string" {
		return text
	}
	if v, ok := decodeJSONValue(strings.TrimSpace(text)); ok {
		return v
	}
	return text
}

// propertyType is the JSON schema type a tool's parameters declare for one
// property, or "" where the schema names none (or several).
func propertyType(schema map[string]any, name string) string {
	props, _ := schema["properties"].(map[string]any)
	prop, _ := props[name].(map[string]any)
	t, _ := prop["type"].(string)
	return t
}

// unoffered says why calls cannot run because one names a tool the request
// did not offer, or "" when every one is offered.
func unoffered(calls []EnvelopeCall, tools []llm.ToolDef) string {
	for _, c := range calls {
		if !slices.ContainsFunc(tools, func(t llm.ToolDef) bool { return t.Name == c.Name }) {
			return fmt.Sprintf("it wrote a call to %q in a form other than the envelope, "+
				"and no tool by that name is offered here", c.Name)
		}
	}
	return ""
}

// decodeJSONValue reads text as EXACTLY ONE JSON value of any kind, through
// json.Number, as [decodeEnvelopeObject] reads an object.
func decodeJSONValue(text string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}

// jsonPreviewBytes bounds the body a protocol error quotes: enough to show
// the shape the model wrote, never a whole page body.
const jsonPreviewBytes = 80

// jsonPreview is a JSON body as an error quotes it: short, on one line.
func jsonPreview(body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if len(body) > jsonPreviewBytes {
		body = textcut.Within(body, jsonPreviewBytes)
	}
	if body == "" {
		return "an empty body"
	}
	return body
}
