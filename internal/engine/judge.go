package engine

import (
	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/auxspend"
)

// The round-cap extension judge: the one [extension.Judge] implementation,
// so a phase that runs out of rounds while making progress is told apart
// from one that is thrashing.

// judgeFor builds this seat's extension judge, or nil.
//
// Per turn, because the model it asks comes from the SEAT's chain: a company
// can put a cheap judge on one role and none on another. Nil (no models for
// the seat, no epoch) is not a failure: extension.Consider reads it as "do
// not ask".
//
// use is the turn's attribution: the judge's evidence rewrites are filed
// under it, and its Budget meters the judge's own call, held only after the
// evidence is rendered, since a rewrite can fill the window the call would be
// refused in ([extension.LLMJudge.WithHold]).
func (e *Engine) judgeFor(c *Company, handle string, use auxspend.Use) extension.Judge {
	if c == nil || c.Org == nil || c.Models == nil {
		return nil
	}
	seat := c.Org.AgentSeatByHandle(handle)
	if seat == nil {
		return nil
	}
	// HEAD, not the chain: a failed judge call falling through to a second
	// model spends twice on a question whose refusal costs nothing, since
	// Consider turns any error into the rescue anyway.
	member, err := c.Models.Take(seat, phase.Judge)
	if err != nil {
		log.Debug("extension_judge_unavailable", "seat", handle, "error", err.Error())
		return nil
	}
	// NewLLMJudge returns nil for a nil provider, the same nil-judge path.
	// The seat's compactor rewrites evidence past its budget for the judge
	// rather than cutting it or carrying it whole.
	key := member.Key
	inFlight := inFlightProvider{inner: member.Provider,
		begin: func() func() { return c.Models.Begin(seat, phase.Judge, key) }}
	judge := extension.NewLLMJudge(inFlight, member.Key).
		WithCompactor(e.seatCompactor(c, handle, use))
	if use.Budget != nil {
		judge = judge.WithHold(use.Budget)
	}
	if judge == nil {
		return nil
	}
	return judge
}
