package engine

import "github.com/crewlet/crewlet/internal/agent/builtin"

// HomeBoardToolDeps is what a person saves their Home board through: the
// coordination store, where every node reads it. Who saves is set per call.
// The zero value where this node has no coordination store.
func HomeBoardToolDeps(e *Engine) builtin.HomeBoardDeps {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return builtin.HomeBoardDeps{}
	}
	return builtin.HomeBoardDeps{Boards: e.backends.Fleet}
}

// InstructionsToolDeps is what a person saves their own Ask Crewlet
// instructions through, beside their Home board ([HomeBoardToolDeps]).
func InstructionsToolDeps(e *Engine) builtin.InstructionsDeps {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return builtin.InstructionsDeps{}
	}
	return builtin.InstructionsDeps{Store: e.backends.Fleet}
}
