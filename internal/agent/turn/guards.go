// Package turn drives one agent turn: Plan, Execute, Review, and the loop
// between them.
//
// THE DECISIONS ARE SEPARATED FROM THE I/O. The loop here owns what happens
// between phases — the delivery overrides, the stall guard, the iteration
// ledger, when a turn is done and when it has failed — and reaches the model
// only through the [Phases] interface. Inline, this is one enormous method
// with dozens of injected dependencies, and the engine's most consequential
// rules can then only be exercised by standing up a whole company. Here they
// are exercised with a fake.
//
// A TURN RUNS UNDER ONE CONFIG SNAPSHOT, taken as a value at Run. Reading each
// of ~18 settings from a live cell on every access lets a hot reload landing
// mid-turn run Plan under one round cap and Execute under another, or size a
// sub-agent's budget from a fraction the parent never saw — and then needs a
// context-local "pin" to paper over it. Passing the snapshot makes
// the bug unrepresentable, so there is nothing to pin.
package turn

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// DepthError reports a delegation chain that has hit its cap.
//
// The always-on backstop against runaway and circular delegation: it is
// checked at the top of every turn however the turn was triggered, and an
// agent-to-agent ask propagates the chain so the recipient inherits the
// accumulated depth.
type DepthError struct{ Depth, Limit int }

func (e *DepthError) Error() string {
	return fmt.Sprintf("delegation depth %d reached the limit of %d", e.Depth, e.Limit)
}

// CheckDepth returns a *DepthError when depth has reached limit.
// A limit of 0 or less disables the cap.
func CheckDepth(depth, limit int) error {
	if limit <= 0 || depth < limit {
		return nil
	}
	return &DepthError{Depth: depth, Limit: limit}
}

// ArtifactHash is the stable hash the stall guard compares.
//
// THE WHOLE DIGEST. It was cut to 16 hex characters, and nothing wanted it
// short: the value is never displayed, never keyed on width, and is only ever
// compared for equality. Cutting 256 bits to 64 turned an exact comparison
// into a probabilistic one, so the guard's answer to "did this round produce
// the same artifact" carried a collision chance it did not need — and the
// consequence of a false match is a turn aborted for stalling when it was
// making progress.
func ArtifactHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// StallDetector aborts a turn when repeated self_iterate rounds produce an
// unchanged artifact.
//
// Observe is called with each round's final artifact whenever Review chose
// self_iterate. Once Threshold consecutive rounds hash the same, the turn is
// looping without making progress and is terminated as failed rather than
// spending its remaining iterations re-deriving the same output.
//
// The threshold is a CONSTANT, not a knob. It was a field the loop's settings
// were meant to fill and never did, so every turn ran on the fallback anyway —
// and the value is not an operator preference: two identical rounds is the
// earliest point at which "unchanged" is a fact rather than a single sample,
// and the round cap already bounds how long a turn that IS changing may run.
type StallDetector struct {
	history []string
}

// stallThreshold is how many consecutive identical artifacts end a turn.
const stallThreshold = 2

// Observe records one round's artifact.
func (s *StallDetector) Observe(artifact string) {
	s.history = append(s.history, ArtifactHash(artifact))
}

// ShouldAbort reports whether the last [stallThreshold] observations are
// identical.
func (s *StallDetector) ShouldAbort() bool {
	n := stallThreshold
	if len(s.history) < n {
		return false
	}
	tail := s.history[len(s.history)-n:]
	for _, h := range tail[1:] {
		if h != tail[0] {
			return false
		}
	}
	return true
}

// Reset clears the history. A turn that changed its artifact has made
// progress, so the run of identical rounds starts over.
func (s *StallDetector) Reset() { s.history = nil }

// BreachKind names why a guard ended a turn. Carried on the breach the caller
// publishes, so a dashboard can tell a loop that gave up from one that never
// moved.
type BreachKind string

const (
	// BreachStall — repeated rounds produced the same artifact.
	BreachStall BreachKind = "stall"
	// BreachMaxIterations — the loop ran out of rounds without reaching done.
	BreachMaxIterations BreachKind = "max_iter"
	// BreachDepth — the delegation chain hit its cap.
	BreachDepth BreachKind = "depth"
	// BreachScheduledTimeout — the turn ran past the wall-clock cap its
	// trigger carried. Only a scheduled fire sets one, which is why the
	// operator-facing string is the one docs/concepts/scheduling.md and
	// [types.GuardScheduledTimeout] already name.
	BreachScheduledTimeout BreachKind = "scheduled_timeout"
)

// Breach is a guard firing. Returned on the result rather than published from
// inside the loop: the loop has no event queue, which is what keeps its rules
// testable without one.
type Breach struct {
	Kind   BreachKind
	Detail string
}
