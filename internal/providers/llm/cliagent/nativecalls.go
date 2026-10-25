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

// namedCalls is every call the reply writes, wherever it writes it, and why
// the reply is a protocol fault when one of its envelopes has a call key that
// does not read as calls ([readCalls]). [ParseEnvelope] holds the calls it
// picked to this list: one it did not pick is a call that would otherwise be
// lost without a word.
//
// Read from every place a call can be:
//   - every candidate document ([envelopeCandidates]) that is an envelope,
//     whatever tool it names, since the envelope is the contract's own form;
//   - every candidate that is a vendor's call document, naming an offered
//     tool ([nativeDocument]);
//   - every whole markup call outside code ([toolmarkup.Read]);
//   - every whole markup call inside a fenced block that names an offered
//     tool. A fenced block is code, and markup in code is an example as far
//     as the prose a person reads is concerned ([toolmarkup]); but a complete
//     call to a tool this very request offers, in a reply whose envelope does
//     not make it, is the model making the call in the wrong place, and
//     reading it as an example ended a turn on a promise nothing kept.
//
// Markup that does not read as a whole call is not on this list: a fragment
// in a code block is an example, and a fragment outside code is a fault the
// reading of the reply's own prose already reports ([withMarkupCalls]).
func namedCalls(reply string, tools []llm.ToolDef) ([]EnvelopeCall, string) {
	var calls []EnvelopeCall
	fault := ""
	for _, candidate := range envelopeCandidates(reply) {
		if doc, ok := decodeEnvelopeObject(candidate); ok {
			if env, isEnvelope := fromDocument(doc); isEnvelope {
				calls = append(calls, env.ToolCalls...)
				if fault == "" {
					fault = env.ProtocolError
				}
			}
		}
		if doc, ok := decodeJSONValue(candidate); ok {
			if _, native, isNative := nativeDocument(doc); isNative {
				calls = append(calls, offeredOnly(native, tools)...)
			}
		}
	}
	calls = append(calls, markupCalls(reply, tools, false)...)
	for _, block := range fencedBlocks(reply) {
		calls = append(calls, markupCalls(block, tools, true)...)
	}
	return distinctCalls(calls), fault
}

// markupCalls are the whole markup calls in text, those naming an offered
// tool only when offeredOnly is set.
func markupCalls(text string, tools []llm.ToolDef, onlyOffered bool) []EnvelopeCall {
	// The blocks read before a fault are whole calls; the fault itself is
	// reported where the markup is the reply's prose ([withMarkupCalls]).
	blocks, _ := toolmarkup.Read(text)
	var calls []EnvelopeCall
	for _, b := range blocks {
		call, ok := markupCall(b, tools)
		if !ok {
			continue
		}
		calls = append(calls, call)
	}
	if onlyOffered {
		return offeredOnly(calls, tools)
	}
	return calls
}

// offeredOnly is the calls that name a tool in tools.
func offeredOnly(calls []EnvelopeCall, tools []llm.ToolDef) []EnvelopeCall {
	var out []EnvelopeCall
	for _, c := range calls {
		if unoffered([]EnvelopeCall{c}, tools) == "" {
			out = append(out, c)
		}
	}
	return out
}

// callIdentity is what makes two calls the same call: the tool and its
// arguments, compared as canonical JSON, since the same call written as an
// envelope and as markup decodes to equal values by different routes.
func callIdentity(c EnvelopeCall) string {
	return c.Name + "\x00" + canonicalJSON(c.Arguments) + "\x00" + c.ArgumentsError
}

// canonicalJSON is a decoded value as JSON with its keys in order, the form
// two readings of one value agree on.
func canonicalJSON(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		// Every value here was decoded from text, so it encodes; one that
		// does not still compares as itself, never as another value.
		return fmt.Sprintf("%#v", v)
	}
	return string(out)
}

// distinctCalls is calls with each call kept once, in the order first written.
func distinctCalls(calls []EnvelopeCall) []EnvelopeCall {
	seen := map[string]bool{}
	var out []EnvelopeCall
	for _, c := range calls {
		if id := callIdentity(c); !seen[id] {
			seen[id] = true
			out = append(out, c)
		}
	}
	return out
}

// missingFrom is every call in named that picked does not account for.
//
// A call is accounted for by a picked call to the same tool whose arguments
// hold every argument it writes, with the same value. Written twice, a call
// is often written less completely once: Claude Code in text mode opens with
// an empty `<invoke name="propose_api_change"></invoke>` and then writes the
// whole call as the envelope (measured), which is one request, not two. A
// call the picked set does not hold that way (another tool, another value,
// an argument the picked call lacks) is a second request, and refused.
func missingFrom(named, picked []EnvelopeCall) []EnvelopeCall {
	var out []EnvelopeCall
	for _, c := range named {
		if !slices.ContainsFunc(picked, func(p EnvelopeCall) bool {
			return p.Name == c.Name && holdsArguments(p.Arguments, c.Arguments)
		}) {
			out = append(out, c)
		}
	}
	return out
}

// holdsArguments reports whether whole has every argument of part, equal.
func holdsArguments(whole, part map[string]any) bool {
	for key, value := range part {
		held, ok := whole[key]
		if !ok || canonicalJSON(held) != canonicalJSON(value) {
			return false
		}
	}
	return true
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
