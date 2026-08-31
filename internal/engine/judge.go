package engine

import (
	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
)

// The round-cap extension judge, wired.
//
// [extension.Judge] is an interface, and until this file there was no
// implementation of it anywhere: every RunnerInput left Judge nil,
// extension.Consider rescued with "no_judge" on every exhaustion, and the
// whole mechanism below it — the per-phase ceilings, the round step, the
// enable switch and the llm_judge model role — was configuration nothing
// read. A phase that ran out of rounds while making obvious progress was
// rescued exactly like one that was thrashing.

// judgeFor builds this seat's extension judge, or nil.
//
// PER TURN, like the budget meter beside it, because the model it asks is
// resolved from the SEAT's chain: a company can put a cheap judge on one role
// and none on another, and an engine-wide judge could honour neither.
//
// Nil is an ordinary answer and not a failure — a company with no models
// configured for the seat, or a turn on a build with no epoch — and
// extension.Consider already reads a nil judge as "do not ask", which is the
// same rescue this would otherwise have to invent.
func (e *Engine) judgeFor(c *Company, handle string) extension.Judge {
	if c == nil || c.Org == nil || c.Models == nil {
		return nil
	}
	seat := c.Org.AgentSeatByHandle(handle)
	if seat == nil {
		return nil
	}
	// HEAD, not the chain. The judge is an optimisation on a phase that
	// has already run out of rounds: falling a failed judge call through
	// to a second model would spend twice to answer a question whose
	// refusal costs nothing, and Consider turns any error into the rescue
	// that was already the alternative.
	member, err := c.Models.Head(seat, phase.Judge)
	if err != nil {
		log.Debug("extension_judge_unavailable", "seat", handle, "error", err.Error())
		return nil
	}
	// NewLLMJudge returns nil for a nil provider, which is what makes the
	// nil-judge path above and this one the same path.
	judge := extension.NewLLMJudge(member.Provider, member.Key)
	if judge == nil {
		return nil
	}
	return judge
}
