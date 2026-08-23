// Package turnctx carries what a turn IS, as an explicit argument.
//
// The Python engine threaded five separate ambient channels through a turn —
// the work key, the config pin, the LLM scope, the phase recorder and the log
// fields — each a contextvar, and each justified the same way: "the consumer is
// a leaf and the intermediate frames have no business knowing". The justification
// is sound for a leaf value and wrong for everything else, and in Go it is worse
// than wrong: a contextvars.Context is COPIED into a task at creation, while a
// goroutine shares whatever it captured. The same pattern that merely obscured
// a dependency in Python is a live data race here.
//
// So (rewrite/decisions/401) a turn's inputs are an argument. The type is named
// TurnContext there; here it is [Turn], because turnctx.TurnContext stutters and
// the package name already says what it is.
//
// Two things stay in context.Context, because their consumers genuinely are
// leaves called from code with no turn concept at all: the work key
// (internal/workkey, read by store writers) and the log fields. Both are
// immutable, and both fail SAFE when absent — an empty work key means "a turn
// with no ledgerable trigger", which is exactly the case that skips the
// duplicate guard. Nothing branches on their presence to decide correctness.
//
// The config pin is deliberately NOT one of them: a turn reading config through
// an ambient channel is how a mid-turn reload gets observed halfway, which is
// the failure immutable epochs exist to remove (rewrite/decisions/404).
package turnctx

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/org"
)

// Turn is everything a turn's own code needs, and nothing else.
//
// IMMUTABLE after construction. Derive a new one rather than mutating it — a
// tool that could rewrite the seat it runs as would make every authorization
// decision downstream a suggestion.
//
// A goroutine that captures a Turn and outlives the turn is a bug, and the one
// no linter can see. The rule that makes it checkable: a Turn is PASSED, never
// stored in a struct field that outlives a turn. Anything needing turn state
// afterwards takes a copy of the values it wants.
type Turn struct {
	// ID is the work key — what identifies the unit of work this turn did.
	// NOT a per-node turn id: two nodes completing one trigger mint two,
	// and anything keyed on one records the duplicate instead of
	// collapsing it.
	ID string

	// Seat is who is acting. THE authorization fact: a tool that speaks
	// for a seat — asking a colleague, marking an onboarding step, writing
	// a diary entry — reads it from here and never from its arguments,
	// which the model controls.
	Seat *org.Role

	// Org is the company this turn is running in, pinned. Read from the
	// epoch the turn started under, so a colleague lookup mid-turn cannot
	// resolve against a roster that changed underneath it.
	Org *org.Organization

	// Depth is the delegation depth this turn inherited, and Chain is who
	// it came through. Both travel so a sub-agent or an A2A ask can refuse
	// past the cap rather than discovering the loop at runtime.
	Depth int
	Chain []string
}

// Handle is the acting seat's handle, or "" when there is no seat.
//
// The nil check is not defensive noise: a tool surface built outside a turn —
// a validate command, a test driving a runner directly — legitimately has no
// seat, and a tool that speaks for one must refuse rather than panic.
func (t *Turn) Handle() string {
	if t == nil || t.Seat == nil {
		return ""
	}
	return t.Seat.Handle()
}

// Role is the acting seat's role name, or "".
func (t *Turn) Role() string {
	if t == nil || t.Seat == nil {
		return ""
	}
	return t.Seat.Name
}

// ErrNoSeat is what a seat-scoped tool returns when it was called outside a
// turn. Its own error rather than a string, so a caller can tell "this tool is
// unusable here" from "this tool ran and failed".
var ErrNoSeat = fmt.Errorf("turnctx: no acting seat")

// RequireSeat reports the acting seat, or ErrNoSeat.
func (t *Turn) RequireSeat() (*org.Role, error) {
	if t == nil || t.Seat == nil {
		return nil, ErrNoSeat
	}
	return t.Seat, nil
}

// ForSubagent derives the context an ephemeral sub-agent runs under.
//
// It KEEPS the org (a sub-agent must see the same company its parent does) and
// EXTENDS the delegation chain, refusing past the cap. The seat becomes the
// child's own: a sub-agent acting as its parent would make the delegation cap
// unenforceable, because nothing downstream could tell the two apart.
func (t *Turn) ForSubagent(seat *org.Role, limit int) (*Turn, error) {
	if t == nil {
		return nil, ErrNoSeat
	}
	depth := t.Depth + 1
	if limit > 0 && depth > limit {
		return nil, fmt.Errorf("turnctx: delegation depth %d exceeds the limit of %d "+
			"(chain: %v)", depth, limit, t.Chain)
	}
	// Copied, not appended in place: append can share a backing array, and
	// two sub-agents derived from one parent would then write over each
	// other's chain.
	chain := make([]string, len(t.Chain), len(t.Chain)+1)
	copy(chain, t.Chain)
	if h := t.Handle(); h != "" {
		chain = append(chain, h)
	}
	return &Turn{ID: t.ID, Seat: seat, Org: t.Org, Depth: depth, Chain: chain}, nil
}
