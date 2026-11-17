package engine

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/worklet"
)

// What the operator surface's `answer_knowledge` needs from the engine: the
// model a person's question runs on, the company's windows it is judged
// against, and where this node's corpus is applied through. See
// internal/agent/builtin/answerknowledge.go for the tool.

// AnswerModels resolves a person's answer model off the CURRENT epoch, per
// call: a config apply replaces the registry, and the operator surface
// outlives it. It goes through the auxiliary seam like every other auxiliary
// call, which on the operator stage charges the COMPANY's windows alone (a
// person has no seat budget) and records the spend for the person.
func AnswerModels(e *Engine) builtin.AnswerModels { return answerModels{engine: e} }

type answerModels struct{ engine *Engine }

func (m answerModels) Auxiliary(role *org.Role, use auxspend.Use) (chain.Member, error) {
	seam := m.engine.auxiliaryFor(m.engine.Company())
	if seam == nil {
		return chain.Member{}, phase.ErrNoProviders
	}
	return seam.Auxiliary(role, use)
}

// AnswerBudget is the company's windows a person's answer is gated against
// (a person has no seat budget), or nil where there is no fleet to count on,
// which omits the tool rather than serving answers no counter hears about.
// The charge is the seam's ([AnswerModels]).
func AnswerBudget(e *Engine) builtin.AnswerBudget {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return answerBudget{engine: e, budgets: e.budgets()}
}

// answerBudget reads the company's windows through the [budgetCounter] a
// [meter] reads them through, so the gate is the very rule the budget park
// applies, not a copy of it — and cuts them at the engine's instant
// ([Engine.now]), the gate's, so an answer is judged in the window a seat's
// next round is charged in, never on a clock of its own.
type answerBudget struct {
	engine  *Engine
	budgets budgetCounter
}

// basis is the company's ceilings and clock off the current epoch.
func (b answerBudget) basis() budgetBasis { return basisOf(b.engine.Company(), nil) }

// Refusing reports the company window that turns an answer away (one with no
// room for a single token, as a window that refused a round is, since the
// refused round is counted), by the budget park's and the live meter's rule
// ([windowRefuses]), and records the refusal in the window's `refused_at`
// ([meter.turnAway]): the tool asks this immediately before an answer's first
// call and refuses on yes, so a yes is the gate turning that call away.
func (b answerBudget) Refusing(ctx context.Context) (builtin.BudgetRefusal, bool, error) {
	m := &meter{budgets: b.budgets, basis: b.basis(), now: b.engine.now}
	r, found, err := m.refusing(ctx)
	if err != nil || !found {
		return builtin.BudgetRefusal{}, false, err
	}
	m.turnAway(ctx, r)
	return builtin.BudgetRefusal{
		Period: string(r.Window.Period), Window: r.Window.Label,
		ResetsAt: r.Window.End, Used: r.Used, Limit: r.Limit,
	}, true, nil
}

// KnowledgeCorpus is where this node's knowledge corpus last changed — the
// tracker's, the pages' and the vectors' rows, joined — and false where there
// is no such place to name.
//
// Where the rows last changed ([statelog.Runner.Changed]), never the applied
// position: every linearizable read appends a barrier that changes no row, so
// a key on the position is retired by the answer's own reads. Those three logs
// because an answer is written from all three; not chat's, which is outside
// the corpus (ADR-0071) and would retire every answer whenever anybody spoke.
// False when the knowledge base is not Doclet (an external wiki changes
// unheard) and on a node without `data`, which runs no log to read.
func (e *Engine) KnowledgeCorpus() (string, bool) {
	c := e.core.Load()
	if e.DocletSearcher() == nil || c == nil || c.log == nil {
		return "", false
	}
	parts := make([]string, 0, 3)
	for _, name := range []string{
		worklet.Domain{}.Name(), doclet.Domain{}.Name(), search.Domain{}.Name(),
	} {
		running := c.log.Domain(name)
		if running == nil || running.runner == nil {
			return "", false
		}
		parts = append(parts, running.runner.Changed().String())
	}
	return strings.Join(parts, ","), true
}

// Rewrites is the cache every compaction this node makes is kept in, for the
// surfaces outside a turn that condense text the same way — a person's
// question answered from pages too long to read whole.
func (e *Engine) Rewrites() *compact.Cache { return e.rewrites }
