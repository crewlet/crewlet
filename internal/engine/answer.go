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
// call, because a config apply replaces the registry and the operator surface
// is built once.
//
// THROUGH THE AUXILIARY SEAM like every other auxiliary call, which on the
// operator stage the tool states charges the COMPANY's windows alone — a
// person has no seat budget — and records the spend for the person. It used
// to resolve off the bare registry and leave the charge to the tool, which
// made the answer the one auxiliary call outside the seam, charged by a copy
// of its rule and recorded nowhere.
func AnswerModels(e *Engine) builtin.AnswerModels { return answerModels{engine: e} }

type answerModels struct{ engine *Engine }

func (m answerModels) Auxiliary(role *org.Role, use auxspend.Use) (chain.Member, error) {
	seam := m.engine.auxiliaryFor(m.engine.Company())
	if seam == nil {
		return chain.Member{}, phase.ErrNoProviders
	}
	return seam.Auxiliary(role, use)
}

// AnswerBudget is the company's windows, as a person's answer is gated against
// them — or nil where there is no fleet to count on, which omits the tool
// rather than serving answers no counter hears about. The CHARGE is the seam's
// ([AnswerModels]).
//
// THE COMPANY'S ALONE: a person has no seat budget, so the gate reads the
// org's counter.
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

// Refusing reports the company window that turns an answer away, by the same
// rule the budget park and the live meter use ([windowRefuses]): a window with
// no room for a single token, which every window the gate refused a round in
// has, since the refused round is counted.
//
// AND RECORDS THE REFUSAL ([meter.turnAway]): the tool asks this immediately
// before an answer's first call and refuses the question on yes, so a yes is
// the gate turning that call away, and the company's window says so in its
// `refused_at`. A company whose day a person's answers, an executor launch or a
// background pass had filled used to refuse every question asked of it while
// every screen said it had refused nothing.
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

// KnowledgeCorpus is where this node's knowledge corpus is applied through —
// the tracker's, the pages' and the vectors' committed positions, joined — and
// false where there is no such place to name.
//
// THREE LOGS, because an answer is written from all three: a page's body, a
// work item's description, and the vectors that ranked them by meaning. A
// write to any of them can change what an answer would say, and moves the
// position, which is what retires every answer cached at the old one.
//
// AND NOT CHAT'S, deliberately: a conversation is outside the knowledge corpus
// (ADR-0071) — an answer is drawn from the knowledge seam, which searches no
// room — so a message changes nothing an answer would say. Keyed on chat's log
// as well, every cached answer would be retired each time anybody spoke, which
// on a busy company is every answer, all the time.
//
// FALSE FOR A COMPANY WHOSE KNOWLEDGE BASE IS NOT DOCLET: an external wiki
// changes without this node hearing, so a position that covered only the
// tracker would keep serving an answer whose pages had moved.
//
// THE CORE'S LOGS, which every data node runs whatever its company keeps: the
// native half holds only the searcher over them. A node without `data` runs
// no log to read a position off, so it answers false and caches nothing.
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
		parts = append(parts, running.runner.Committed().String())
	}
	return strings.Join(parts, ","), true
}

// Rewrites is the cache every compaction this node makes is kept in, for the
// surfaces outside a turn that condense text the same way — a person's
// question answered from pages too long to read whole.
func (e *Engine) Rewrites() *compact.Cache { return e.rewrites }
