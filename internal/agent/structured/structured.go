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
	"reflect"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/jsonform"
	"github.com/crewlet/crewlet/internal/tools"
)

// Tool is a structured-output tool: a schema, a decoder, and the value the
// model submitted.
//
// Generic over the payload so a caller gets its own type back rather than a
// map it has to re-check at every use.
//
// SAFE FOR CONCURRENT USE. A coding CLI makes a response's calls at once, and
// an executor run's bridge hands each one to the surface on its own goroutine
// with nothing in front of them that runs a submission alone — so two
// submissions, or a submission and the read of one, can meet here. The decoder
// runs outside the lock: it is the caller's, it may read the surface this tool
// sits on, and nothing it does decides the captured value.
type Tool[T any] struct {
	name   string
	desc   string
	schema map[string]any
	decode func(map[string]any) (T, error)

	mu     sync.Mutex
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
		return tools.Result{
			Output: "Invalid submission: " + err.Error(), Failed: true,
			Cause: tools.Classify(tools.RefusalInvalid, err),
		}, nil
	}
	// LAST WRITE WINS, and the call is still accepted. A model that
	// submits twice has corrected itself; rejecting the second submission
	// leaves the engine acting on the draft the model just replaced. Of two
	// made at once, the one that lands last is the answer: the order a
	// model wrote two concurrent calls in is not one their arrival keeps.
	t.mu.Lock()
	t.value, t.called = v, true
	t.mu.Unlock()
	return tools.Result{Output: "submitted"}, nil
}

// Value returns the captured submission and whether one arrived.
func (t *Tool[T]) Value() (T, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.value, t.called
}

// Remarshal moves a decoded argument map into a typed struct.
//
// Via JSON rather than field-by-field because the arguments already came off
// the wire as JSON and the struct tags are the schema — one definition of the
// mapping instead of two that can disagree. json.Number survives the round
// trip, so a large id in a submission's own arguments stays exact.
//
// A FIELD THE STRUCT DOES NOT HAVE IS REFUSED, at any depth, naming it. A
// plain unmarshal DROPS it, and a dropped field is not a no-op: a model that
// misspelt an optional one — a delegated task's `afer`, a reviewer's
// `completed_work` — had its submission accepted as though it had said less,
// so the task ran without the input it was told to wait for and the next
// round was never told what had already landed, and nothing told the model
// which field it got wrong. A refusal goes back to the model, which is the
// one failure it reliably fixes.
// It is also what lets a test hold a hand-written schema to its struct at
// all: while a property the struct lacked decoded cleanly, the walk that
// submits every published property could not fail on the drift it names.
func Remarshal(args map[string]any, into any) error {
	blob, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("arguments could not be re-encoded: %w", err)
	}
	// ACCEPTED EXACTLY AS BEFORE, every fault named (ADR-0059): what
	// encoding/json with DisallowUnknownFields takes is what a submission
	// is, and when it refuses, the walk names every fault in the one
	// answer, so the model fixes them all in one attempt rather than one
	// per attempt.
	form := jsonform.OfType(reflect.TypeOf(into).Elem())
	d := jsonform.Stored(jsonform.UntilEOF)(blob, form,
		jsonform.Subject{File: "the submission", Format: "the submission"}, into)
	if len(d.Faults) == 0 {
		return nil
	}
	reasons := make([]string, len(d.Faults))
	for i, f := range d.Faults {
		reasons[i] = f.Message
	}
	return fmt.Errorf("arguments do not match the schema: %s", strings.Join(reasons, "; "))
}
