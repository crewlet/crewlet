package turn

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/panics"
)

// PanicError is a panic recovered while handling a turn.
//
// A PANIC IS A DEFECT IN THIS PROCESS, not a failure of anything it talks to,
// so it gets its own type: the two are acted on in opposite directions. A
// provider that did not answer is worth asking again; a nil map written from a
// tool handler is not, because a redelivery panics in the same place having
// repeated whatever the turn did before it.
//
// Unrecovered, a panic would reach the queue backend's handler guard, which
// NAKs it: the turn is redelivered up to the whole 25-delivery budget, each
// attempt spending a model round and replaying earlier writes, and the seat
// stays rendered as `working` because nothing closed the turn.
type PanicError struct {
	// Value is what was passed to panic.
	Value any

	// Stack is the panicking goroutine's stack, captured at the recovery
	// point. It reaches the LOG, never an event: a stack names source paths
	// and argument values, and an event is readable by anyone the
	// dashboard serves.
	Stack string

	failure error
}

// Recovered builds the error for a recovered panic value, or nil when there
// was no panic, and tells [panics] of it, which names the function that broke.
//
// It must be called from the deferred function that called recover(): the
// frames that panicked are on the goroutine's stack only until that deferred
// call returns.
func Recovered(scope panics.Scope, value any) *PanicError {
	p := panics.Recovered(scope, value)
	if p == nil {
		return nil
	}
	return &PanicError{Value: p.Value, Stack: p.Stack, failure: p.Err()}
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// Unwrap is the contained panic as a [panics.Failure], never its value: a
// panic(context.Canceled) is a panic, not a cancellation.
func (e *PanicError) Unwrap() error { return e.failure }

// The reasons [Abandon] gives, as constants so a caller that logs or records
// one says the same sentence every other caller does.
const (
	// AbandonedActed is a turn that broke after its own record showed it had
	// already reached outside the engine ([Acted]).
	AbandonedActed = "the turn broke after writing outside the engine"

	// AbandonedPanicked is a turn that panicked.
	AbandonedPanicked = "the turn panicked, and a redelivery would run the " +
		"same defect on the same input"

	// AbandonedPermanent is a turn whose phase refused to run for a reason a
	// redelivery would meet again ([ErrPermanent]). It says only that the
	// refusal is final: why is the error's to say, which every account of an
	// abandoned turn prints beside its reason, and which says a retry would be
	// refused the same way.
	AbandonedPermanent = "the turn was refused for good"
)

// ErrPermanent marks a phase that refused to run for a reason a redelivery
// would meet again, wrapped beside the error that says why (internal/agent/
// runner's ErrRoundLost). [Abandon] gives such a turn up.
//
// A MARK THIS PACKAGE DECLARES AND THE REFUSER WRAPS: this package imports
// neither package that decides a refusal is final. As an ordinary broken phase
// it was refused again on every announcement, holding the seat for half an hour.
var ErrPermanent = errors.New("a retry would be refused the same way")

// Abandon reports why a turn that returned err must NOT be run again from its
// trigger, or false when a redelivery may run it cleanly. One answer for both
// paths a turn arrives by (delivery, run collection); each case outranks the next:
//
//   - a PANIC: it destroys its round's record, so "nothing reached outside"
//     cannot be established, and a retry runs the same defect.
//   - a refusal a retry would MEET AGAIN ([ErrPermanent]).
//   - an outward write ([Acted]): the retry would repeat it.
//
// Everything else keeps its retry (provider silent, runner not built, refused
// budget): none wrote anything.
func Abandon(res Result, err error) (reason string, abandon bool) {
	if err == nil {
		return "", false
	}
	var panicked *PanicError
	switch {
	case errors.As(err, &panicked):
		return AbandonedPanicked, true
	case errors.Is(err, ErrPermanent):
		return AbandonedPermanent, true
	case res.Acted:
		return AbandonedActed, true
	}
	return "", false
}
