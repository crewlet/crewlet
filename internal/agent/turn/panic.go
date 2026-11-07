package turn

import (
	"errors"
	"fmt"
	"path"
	"runtime"
	"runtime/debug"
	"strings"
)

// PanicError is a panic recovered while handling a turn.
//
// A PANIC IS A DEFECT IN THIS PROCESS, not a failure of anything it talks to,
// and that is why it gets its own type rather than an ordinary error: the two
// are acted on in opposite directions. A provider that did not answer is worth
// asking again. A nil map written from a tool handler is not, because a
// redelivery runs the same code on the same input and panics in the same place
// having repeated whatever the turn did before it.
//
// Before this type existed nothing between the broker and a seat's turn
// recovered anything, so a panic unwound all the way into the queue backend's
// own handler guard. That guard turns any panic into a NAK, which is the right
// default for a handler it knows nothing about and the wrong one here: the turn
// was redelivered up to the whole 25-delivery budget, each attempt spending a
// model round and replaying the writes of the rounds before it, and the seat
// was left rendered as `working` because nothing closed the turn.
type PanicError struct {
	// Value is what was passed to panic.
	Value any

	// Stack is the panicking goroutine's stack, captured at the recovery
	// point. It reaches the LOG, never an event: a stack names source paths
	// and argument values, and an event is readable by anyone the
	// dashboard serves.
	Stack string

	// Function is the engine function that panicked, as the runtime spells
	// it, "" where the stack holds none of the module's. A compile-time name
	// from the public source and nothing of the input: what an issue report
	// and a guard breach may carry where the stack may not.
	Function string

	// File is the base name of the file Function is in, "" with it. Never the
	// frame's path, which on a build without -trimpath names the build machine.
	File string
}

// Recovered builds the error for a recovered panic value, or nil when there
// was no panic.
//
// It must be called from the deferred function that called recover(): the
// stack it records is the goroutine's at that moment, which still holds the
// frames that panicked only until the deferred call returns.
func Recovered(value any) *PanicError {
	if value == nil {
		return nil
	}
	function, file := panicked()
	return &PanicError{Value: value, Stack: string(debug.Stack()), Function: function, File: file}
}

// panicked is the first of the module's functions under the ORIGINAL panic, on
// the stack [Recovered] was called on, and its file's base name.
//
// THE LAST `runtime.gopanic` IN THIS WALK IS THE ORIGINAL ONE. A deferred function
// that recovers and panics again (a store transaction rolling back) runs on top of
// the frames that first panicked, so each re-raise adds a gopanic above them, and
// the first one met from the top names whoever re-raised rather than the defect.
func panicked() (function, file string) {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	under := false
	for {
		frame, more := frames.Next()
		switch {
		case frame.Function == "runtime.gopanic":
			under = true
		case under && strings.HasPrefix(frame.Function, "github.com/crewlet/crewlet/"):
			function, file = frame.Function, path.Base(frame.File)
			under = false
		}
		if !more {
			return function, file
		}
	}
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// The reasons [Abandon] gives, as constants so a caller that logs or records
// one says the same sentence every other caller does.
const (
	// AbandonedActed is a turn that broke after its own record proved it had
	// already reached outside the engine.
	AbandonedActed = "the turn broke after writing outside the engine"

	// AbandonedPanicked is a turn that panicked.
	AbandonedPanicked = "the turn panicked, and a redelivery would run the " +
		"same defect on the same input"
)

// Abandon reports why a turn that returned err must NOT be run again from its
// trigger, or false when a redelivery may run it cleanly.
//
// ONE ANSWER FOR BOTH PATHS a turn arrives by. The dispatcher asks it about a
// delivery and the run's collection asks it about a completion, and each used to
// carry its own copy of the rule; a panic added to one and forgotten in the
// other is exactly the drift one function makes impossible.
//
// Two cases, the first outranking the second because it is the more specific
// account of what happened:
//
//   - a PANIC is never retried. The premise a retry rests on is that the
//     turn's own record proves nothing reached outside, and a panic destroys
//     the record of the round it happened in, so that premise cannot be
//     established at all. Even when it could, the redelivery runs the same
//     defect again.
//   - a turn whose record PROVES an outward write is not retried either,
//     because the retry would repeat the write. See [Result.Acted].
//
// Everything else keeps its retry: a provider that did not answer, a runner
// that could not be built, a refused budget. None of them wrote anything and
// all of them may succeed next time.
func Abandon(res Result, err error) (reason string, abandon bool) {
	if err == nil {
		return "", false
	}
	var panicked *PanicError
	switch {
	case errors.As(err, &panicked):
		return AbandonedPanicked, true
	case res.Acted:
		return AbandonedActed, true
	}
	return "", false
}
