// Package structured is how a phase gives a typed answer.
//
// A phase ends by CALLING A TOOL rather than by writing prose the engine then
// parses. The tool does nothing: it validates its arguments against a decoder,
// records the result, and reports back. What the phase decided is the
// arguments it was called with, so the ordinary tool loop carries the answer
// out and nothing needs a side channel — which is also why a phase whose
// answer never arrived is distinguishable from one that answered badly.
//
// Three rules travel with the shape, and each is a decision the callers must
// not make differently:
//
//   - A REJECTED SUBMISSION GOES BACK TO THE MODEL. Invalid arguments are the
//     one tool failure a model reliably fixes, and ending the phase over one
//     throws away everything it already did.
//   - LAST WRITE WINS. A model that submits twice has corrected itself;
//     refusing the second leaves the engine acting on the draft it replaced.
//   - AN ABSENT SUBMISSION IS NOT A VALUE. [Tool.Value] reports whether one
//     arrived, so a caller can tell "the phase decided this" from "nothing
//     decided anything" — the distinction every rescue path in the turn
//     engine turns on.
//
// The decoder is the caller's, because what a valid answer IS belongs to the
// phase asking. This package owns only the shape and the three rules.
package structured

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/tools"
)

// Tool is a structured-output tool: a schema, a decoder, and the value the
// model submitted.
//
// Generic over the payload so a caller gets its own type back rather than a
// map it has to re-check at every use.
type Tool[T any] struct {
	name   string
	desc   string
	schema map[string]any
	decode func(map[string]any) (T, error)

	value  T
	called bool
}

// New builds a structured-output tool.
//
// The decoder both parses and VALIDATES: an error from it is shown to the
// model as a failed tool call, so its message is written for the model that
// has to correct it rather than for a log.
func New[T any](name, description string, schema map[string]any,
	decode func(map[string]any) (T, error),
) *Tool[T] {
	return &Tool[T]{name: name, desc: description, schema: schema, decode: decode}
}

// Name is the tool's wire name, which is also what a terminator matches on.
func (t *Tool[T]) Name() string { return t.name }

// Description is what the model is told this tool is for.
func (t *Tool[T]) Description() string { return t.desc }

// Parameters is the JSON Schema the model fills in. It IS the answer's shape:
// a phase that asked for prose and parsed it would be validating twice, once
// in the model's head and once in a decoder, and only one of those reports a
// mistake the model can act on.
func (t *Tool[T]) Parameters() map[string]any { return t.schema }

// Call decodes one submission and records it.
func (t *Tool[T]) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	v, err := t.decode(args)
	if err != nil {
		// A failed validation goes BACK TO THE MODEL rather than ending
		// the phase. It is the one tool failure the model can reliably
		// fix, and refusing the turn over a malformed submission throws
		// away everything the phase already did.
		//nolint:nilerr // Deliberate: see the paragraph above.
		return tools.Result{Output: "Invalid submission: " + err.Error(), Failed: true}, nil
	}
	// LAST WRITE WINS, and the call is still accepted. A model that
	// submits twice has corrected itself; rejecting the second submission
	// leaves the engine acting on the draft the model just replaced.
	t.value = v
	t.called = true
	return tools.Result{Output: "submitted"}, nil
}

// Value returns the captured submission and whether one arrived.
func (t *Tool[T]) Value() (T, bool) { return t.value, t.called }

// Remarshal moves a decoded argument map into a typed struct.
//
// Via JSON rather than field-by-field because the arguments already came off
// the wire as JSON and the struct tags are the schema — one definition of the
// mapping instead of two that can disagree. json.Number survives the round
// trip, so a large id in a submission's own arguments stays exact.
func Remarshal(args map[string]any, into any) error {
	blob, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("arguments could not be re-encoded: %w", err)
	}
	if err := json.Unmarshal(blob, into); err != nil {
		return fmt.Errorf("arguments do not match the schema: %w", err)
	}
	return nil
}
