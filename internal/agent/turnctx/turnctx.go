// Package turnctx carries what a turn IS, as an explicit argument.
//
// A turn has five values that want to travel ambiently — the work key, the
// config pin, the LLM seat, the phase recorder and the log fields — and each
// makes the same case for itself: "the consumer is a leaf and the intermediate
// frames have no business knowing". That case is sound for a leaf value and
// wrong for everything else, and in Go it is worse than wrong: a goroutine
// SHARES whatever it captured, for as long as it runs, including after the
// turn that created it has finished. There is no copy-on-spawn to bound it, so
// what would elsewhere merely obscure a dependency is a live data race.
//
// So a turn's inputs are an argument: [Turn], which the package name already
// qualifies.
//
// # What is still allowed to be ambient, and the bar it clears
//
// Three values, each read by a genuine leaf called from code with no turn
// concept at all, each IMMUTABLE, and each failing SAFE when absent — nothing
// branches on their presence to decide correctness:
//
//   - the work key (internal/workkey), read by store writers. Absent means "a
//     turn with no ledgerable trigger", which is exactly the case that skips
//     the duplicate guard.
//   - the log fields, which decorate a line or do not.
//   - the seat handle a model call belongs to (llm.WithSeat). Absent resolves
//     to a named "shared" rather than an empty string, because the value
//     becomes a home directory and auxiliary work — summarisation, the
//     relevance filter — legitimately arrives unbound.
//
// The config pin is deliberately NOT one of them: a turn reading config through
// an ambient channel is how a mid-turn reload gets observed halfway, which is
// the failure immutable epochs exist to remove. Neither is the phase recorder,
// whose whole job is to attribute spend to the phase that incurred it — an
// ambient one attributes it to whichever phase last wrote the context.
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
