package executor

import (
	"bytes"
	"encoding/json"
)

// The Gemini API's content parts, as every harness built on it records a
// conversation's turns: Gemini CLI's session file and Qwen Code's alike name a
// turn's parts in the API's own vocabulary (text, a thought, a function call,
// a function's response), whatever record each wraps them in. One decode for
// both, so a harness's record parser reads its own record and hands the parts
// here.

// geminiPart is one part of a Gemini API content, each field raw where a
// release may retype it and read loosely ([geminiParts]).
type geminiPart struct {
	// Text is the part's text, nil for a part with none.
	Text *string
	// Thought marks a text part that is the model's reasoning, never its
	// words.
	Thought bool
	// Call is the part's function call, Response a function's response.
	Call     *geminiFunction
	Response *geminiFunction
}

// geminiFunction is a function call or a function's response: the id the
// harness gave the call, the function's name, and its arguments or its
// response as JSON.
type geminiFunction struct {
	ID, Name string
	Body     json.RawMessage
}

// geminiPartWire is one part as the API spells it, each value raw.
type geminiPartWire struct {
	Text             json.RawMessage `json:"text"`
	Thought          json.RawMessage `json:"thought"`
	FunctionCall     json.RawMessage `json:"functionCall"`
	FunctionResponse json.RawMessage `json:"functionResponse"`
}

// geminiFunctionWire is a call or a response as the API spells it.
type geminiFunctionWire struct {
	ID       json.RawMessage `json:"id"`
	Name     json.RawMessage `json:"name"`
	Args     json.RawMessage `json:"args"`
	Response json.RawMessage `json:"response"`
}

// geminiParts is content's parts: a JSON array of parts, or a string, which
// is one text part (Gemini CLI writes a model turn's words so). A part that
// does not decode is passed over, as are content of any other kind and a part
// that is no object.
func geminiParts(content json.RawMessage) []geminiPart {
	content = bytes.TrimSpace(content)
	if s := looseString(content); s != "" {
		return []geminiPart{{Text: &s}}
	}
	if len(content) == 0 || content[0] != '[' {
		return nil
	}
	var raws []json.RawMessage
	if wireDecode(content, &raws) != nil {
		return nil
	}
	parts := make([]geminiPart, 0, len(raws))
	for _, raw := range raws {
		var w geminiPartWire
		if !wireObject(raw) || wireAlias(raw, &w) != nil {
			continue
		}
		var part geminiPart
		if len(w.Text) > 0 && w.Text[0] == '"' {
			text := looseString(w.Text)
			part.Text = &text
		}
		part.Thought = looseTrue(w.Thought)
		part.Call = geminiFunctionOf(w.FunctionCall, false)
		part.Response = geminiFunctionOf(w.FunctionResponse, true)
		parts = append(parts, part)
	}
	return parts
}

// geminiFunctionOf is raw as a call (its args) or, where response is set, a
// function's response (its response); nil where raw is no object.
func geminiFunctionOf(raw json.RawMessage, response bool) *geminiFunction {
	var w geminiFunctionWire
	if !wireObject(raw) || wireAlias(raw, &w) != nil {
		return nil
	}
	f := &geminiFunction{ID: looseString(w.ID), Name: looseString(w.Name), Body: bytes.Clone(w.Args)}
	if response {
		f.Body = bytes.Clone(w.Response)
	}
	return f
}

// decodes reports whether decode reads raw into v. The Gemini-API readers pass
// over a line or a field that does not decode, as Claude Code's reader does,
// and answer nothing for it rather than an error.
func decodes(decode func([]byte, any) error, raw []byte, v any) bool { return decode(raw, v) == nil }
