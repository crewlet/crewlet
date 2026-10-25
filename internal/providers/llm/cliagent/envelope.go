package cliagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Envelope is what a reply parses into: the prose the operator sees, and the
// tool calls the loop runs.
type Envelope struct {
	Message   string
	ToolCalls []EnvelopeCall
	// Parsed is false when nothing in the reply was an envelope. The
	// caller then hands the whole reply back as assistant content with no
	// tool calls, and the tool loop's finishing corrective takes over in
	// every phase that finishes by a call, naming its submission: a
	// malformed reply costs one round, it never fails a turn.
	Parsed bool
	// ProtocolError is why the reply's tool calls could not be read, when it
	// asked for some in a form that is not the envelope and could not be
	// converted to it: markup naming a tool the request did not offer, markup
	// that is cut off or malformed, or the envelope's calls and markup in
	// one reply. ToolCalls is then EMPTY, since a reply whose calls cannot
	// all be read runs none of them, and Message holds the reply as it was,
	// for the conversation only: the tool loop never shows it
	// ([llm.Completion.ProtocolError]).
	ProtocolError string
	// Refused are the calls a reply with a ProtocolError named, wherever it
	// wrote them: never run, kept so the refusal is on the record
	// ([llm.Completion.RefusedCalls]).
	Refused []EnvelopeCall
}

// EnvelopeCall is one requested tool call.
type EnvelopeCall struct {
	Name      string
	Arguments map[string]any
	// ArgumentsError is why the arguments the model wrote could not be read
	// — a string holding no single JSON object, a number, a list — and
	// empty when they could, or when it wrote none. Arguments is then
	// EMPTY, and the call is KEPT rather than dropped: it is what the model
	// asked for, and [llm.ToolCall.ArgumentsError] is how the tool loop
	// answers it with this reason instead of running it with nothing.
	ArgumentsError string
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

// ParseEnvelope reads a CLI's reply into an [Envelope], against the tools the
// request offered.
//
// Deliberately forgiving, in a specific direction: it accepts every shape a
// model plausibly produces under the contract, and it NEVER errors. The
// alternative — a strict parser that fails a turn on a stray prose sentence
// before the fence — trades a one-round correction for an incident, and the
// tool loop already has the corrective re-prompt this leans on.
//
// The order matters. A fenced block is tried first and the LAST one wins,
// because a model that reasons in prose and then answers puts the answer at
// the end; taking the first would return its worked example. Its MESSAGE
// wins, that is: a worked example that asks for a call is a second set of
// calls, which refuses the reply (below).
//
// THE ENVELOPE FIRST, THEN THE VENDORS' OWN FORMS. A model trained on its
// vendor's native tool channel sometimes writes a call in that channel's form
// instead: a JSON tool_use block, an OpenAI function call, or markup
// ([toolmarkup]). Each is read as the call it is when it names a tool in
// tools, so the round runs rather than ending with the markup as its answer;
// one that names any other tool, or does not read as a whole call, is the
// envelope's [Envelope.ProtocolError] instead, and runs nothing. The gate is
// the catalogue because nothing else separates a call from a model answering
// a question about calls: a document naming an offered tool is a request to
// run it, and anything else in that shape is a protocol fault the tool loop
// asks again about, never prose shown to a person.
//
// ONE REPLY, ONE SET OF CALLS. The reading above picks one source (the last
// envelope, a vendor document, or markup), and every call the reply writes
// ANYWHERE ([namedCalls]) must be accounted for by the ones it picked
// ([missingFrom]). A call that is not (an earlier envelope's, markup beside
// the envelope, a whole call to an offered tool written in a code block)
// makes the reply a protocol fault that runs nothing and names every call as
// refused. Measured on Claude Code in text mode: the proposal written as
// markup, then an envelope asking for nothing. Read by the envelope alone it
// was a round with no call, the model believed its proposal made, and the
// turn completed on a message saying so, with no proposal and no step
// saying why.
func ParseEnvelope(reply string, tools []llm.ToolDef) Envelope {
	env, ok := strictEnvelope(reply)
	if !ok {
		env, ok = nativeEnvelope(reply, tools)
	}
	if !ok {
		env = Envelope{Message: reply}
	}
	env = withMarkupCalls(env, tools)
	named, fault := namedCalls(reply, tools)
	if env.ProtocolError == "" {
		env.ProtocolError = fault
	}
	if env.ProtocolError == "" {
		if stray := missingFrom(named, env.ToolCalls); len(stray) > 0 {
			env.ProtocolError = fmt.Sprintf("it wrote a call to %q outside the envelope it "+
				"answered with (in a second envelope, as markup or in a code block), and one "+
				"reply carries exactly one envelope", stray[0].Name)
		}
	}
	if env.ProtocolError != "" {
		return Envelope{Message: reply, Parsed: env.Parsed, ProtocolError: env.ProtocolError,
			Refused: distinctCalls(append(env.ToolCalls, named...))}
	}
	return env
}

// strictEnvelope is the envelope the contract asks for, read from the reply's
// candidate documents in order.
func strictEnvelope(reply string) (Envelope, bool) {
	for _, candidate := range envelopeCandidates(reply) {
		doc, ok := decodeEnvelopeObject(candidate)
		if !ok {
			continue
		}
		if env, ok := fromDocument(doc); ok {
			if env.ProtocolError != "" {
				// None of its calls runs; [ParseEnvelope] names the ones
				// it could read as refused.
				return Envelope{Message: reply, Parsed: true, ProtocolError: env.ProtocolError}, true
			}
			return env, true
		}
	}
	return Envelope{}, false
}

// decodeEnvelopeObject reads one candidate document, THROUGH json.Number.
//
// The number rule is not a display concern here: these arguments are
// EXECUTED. Decoded into an `any` a number becomes a float64, so a call naming
// a 19-digit id — a Jira issue id, a Slack timestamp, a GitHub node id — is
// re-encoded as 1234567890123456800 and runs against a DIFFERENT row, with
// nothing anywhere reporting an error.
// [github.com/crewlet/crewlet/internal/tools.ReadArgs] holds a record this
// engine wrote to the same rule.
//
// Nothing else read out of the envelope is a number — [fromDocument] and
// [readCall] take strings and lists — so the whole document can be decoded
// this way rather than the arguments alone, which is what the object form of
// an argument list needs: it is already inside this map by the time
// [readArguments] sees it.
//
// NOT exec.go's `decodeObject`, which reads a CLI's own STREAM events — a
// different document, with a leading-brace guard this one must not have and
// numbers nobody executes against. Sharing one would give the envelope that
// guard and give the stream a rule it has no use for.
func decodeEnvelopeObject(text string) (map[string]any, bool) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, false
	}
	// EXACTLY ONE DOCUMENT. A Decoder reads one value and stops, so
	// `{"message":"hi"}and then some prose` would decode clean where the
	// `json.Unmarshal` this replaced refused it — and refusing it is what
	// makes the candidate order mean anything: a prefix that happens to
	// parse must not win over the fenced block further down that IS the
	// answer. Trailing whitespace is not a tail.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return doc, doc != nil
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
// NOT an envelope: it is the model answering a question with JSON, so it is
// rejected here and the reply is handed back as prose. Accepting it would
// turn every JSON answer into an empty envelope and silently drop the answer.
//
// A CALL KEY THAT DOES NOT READ AS CALLS IS A PROTOCOL FAULT, never a smaller
// set of calls and never prose. The envelope carries the readable calls in
// ToolCalls and the reason in ProtocolError, and its caller runs none of them.
// Read the forgiving way, an entry with no name was dropped and the rest ran,
// or a list nothing could be read from turned the whole reply into the
// answer: either way the model had asked for a call that neither ran nor was
// refused, and a turn could end on a message like "I'll post it now" with
// nothing delivered and nothing to say why. A NULL or EMPTY list is not a
// fault: it is the model saying "no calls, here is my note".
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
		env.ToolCalls, env.ProtocolError = readCalls(key, v)
		found = true
		break
	}

	return env, found
}

// readCalls reads the value of an envelope's call key: the calls it could
// read, and why it is a protocol fault when any part of it is not a call.
func readCalls(key string, v any) ([]EnvelopeCall, string) {
	var raws []any
	switch typed := v.(type) {
	case []any:
		raws = typed
	case map[string]any:
		// ONE CALL WRITTEN WITHOUT ITS LIST. A model asked for a list of
		// one routinely drops the brackets, and the object is as runnable
		// as the list holding it, so it is read as that list.
		raws = []any{typed}
	case nil:
		// `null` is the model saying "no calls", the same answer as an
		// empty list, as an argument list's `null` is no arguments in
		// [readArguments].
		return nil, ""
	default:
		return nil, fmt.Sprintf("its %q holds a %s, not a list of tool calls", key, jsonKind(typed))
	}
	// Every readable call is kept, after an unreadable one too: they are the
	// refusal's record.
	var calls []EnvelopeCall
	fault := ""
	for i, raw := range raws {
		call, ok := readCall(raw)
		if !ok {
			if fault == "" {
				fault = fmt.Sprintf("entry %d of its %q is not a tool call with a name", i+1, key)
			}
			continue
		}
		calls = append(calls, call)
	}
	return calls, fault
}

// jsonKind names a decoded JSON value's kind, as a protocol fault quotes it.
func jsonKind(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// readCall reads one tool call.
func readCall(raw any) (EnvelopeCall, bool) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return EnvelopeCall{}, false
	}
	if fn, isObject := obj["function"].(map[string]any); isObject {
		// THE OPENAI TOOL-CALL SHAPE, `{"type": "function", "function":
		// {"name": ..., "arguments": "..."}}`: the call is the inner
		// object, and reading the outer one found no name and dropped it.
		return readCall(fn)
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
	// The first synonym that READS wins. When one is present and none
	// reads, the call is still the model's — but it must not run on an
	// empty map standing in for arguments it never wrote, which would be a
	// search over everything or a post with no body. So the first
	// unreadable synonym's reason is kept and the tool loop answers the call
	// with it, a failed result the model can read and correct.
	var unreadable error
	for _, key := range argumentKeys {
		v, ok := obj[key]
		if !ok {
			continue
		}
		args, err := readArguments(v)
		if err == nil {
			call.Arguments = args
			unreadable = nil
			break
		}
		if unreadable == nil {
			unreadable = fmt.Errorf("%q %w", key, err)
		}
	}
	if unreadable != nil {
		call.ArgumentsError = unreadable.Error()
	}
	if call.Arguments == nil {
		call.Arguments = map[string]any{}
	}
	return call, true
}

// readArguments reads one call's arguments, accepting both an object and a
// JSON string holding one, and says what it found when it is neither.
//
// The string form is not a model quirk to tolerate grudgingly: it is what the
// OpenAI tool-call wire format uses, so a model that has seen that format
// reproduces it faithfully. Rejecting it would fail the calls from the models
// that had learned the convention best.
func readArguments(v any) (map[string]any, error) {
	switch typed := v.(type) {
	case map[string]any:
		return typed, nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return map[string]any{}, nil
		}
		if args, ok := decodeEnvelopeObject(trimmed); ok {
			return args, nil
		}
		return nil, errors.New("is a string that does not hold exactly one complete JSON object")
	case nil:
		return map[string]any{}, nil
	case json.Number:
		// Numbers decode through json.Number (see decodeEnvelopeObject).
		return nil, errors.New("is a number, not a JSON object")
	case []any:
		return nil, errors.New("is a list, not a JSON object")
	case bool:
		return nil, errors.New("is a boolean, not a JSON object")
	default:
		return nil, fmt.Errorf("is a %T, not a JSON object", typed)
	}
}

// RenderContract is the response contract appended to a prompt that offers
// tools. A call with NO tools gets no contract at all: auxiliary work
// (summarisation, the relevance filter) sends a plain prompt and reads a plain
// answer, with no envelope to get wrong.
//
// ONE FORM, the permissive one, because a request never forces a call (see
// [llm.Request.Tools]): a phase that must end in a call names it in the
// conversation, and the tool loop asks again when a round ends without it.
func RenderContract() string {
	var b strings.Builder
	b.WriteString("## Response contract\n\n")
	b.WriteString("Reply with ONE fenced json block and nothing outside it:\n\n")
	b.WriteString("```json\n")
	b.WriteString(`{"message": "a short note for the operator, or an empty string", `)
	b.WriteString(`"tool_calls": [{"name": "tool_name", "arguments": {"argument": "value"}}]}`)
	b.WriteString("\n```\n\n")
	b.WriteString("Use an empty tool_calls list when no tool is needed.\n")
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
