package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/tracker"
)

// What the operator surface's `answer_knowledge` needs from the engine: the
// model a person's question runs on, the company's windows it is judged and
// charged against, and where this node's corpus is applied through. See
// internal/agent/builtin/answerknowledge.go for the tool.

// AnswerModels resolves a person's answer model off the CURRENT epoch, per
// call, because a config apply replaces the registry and the operator surface
// is built once.
//
// UNMETERED on purpose, unlike [Engine.meteredModelsFor]: that wrapper charges
// a SEAT's counter, and a person is not an agent seat — the tool charges the
// company's windows itself, through [AnswerBudget].
func AnswerModels(e *Engine) builtin.AnswerModels { return answerModels{engine: e} }

type answerModels struct{ engine *Engine }

func (m answerModels) Head(role *org.Role, ph phase.Phase) (chain.Member, error) {
	c := m.engine.Company()
	if c == nil {
		return chain.Member{}, phase.ErrNoProviders
	}
	return c.Models.Head(role, ph)
}

// AnswerBudget is the company's windows, as a person's answer is gated and
// charged against them — or nil where there is no fleet to count on, which
// omits the tool rather than serving answers no counter hears about.
//
// THE COMPANY'S ALONE: a person has no seat budget, so the gate reads the
// org's counter and the charge is [coord.Budgets.PostChargeOrg].
func AnswerBudget(e *Engine) builtin.AnswerBudget {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return answerBudget{engine: e, budgets: e.backends.Fleet, now: time.Now}
}

// orgCounter is the slice of the fleet's counters an answer uses: the
// [budgetCounter] a [meter] reads the company's windows through — so the gate
// is the very rule the budget park applies, not a copy of it — and the
// after-the-fact charge. Nothing here calls Charge: an answer's size is known
// only from its reply, so it is never put through the gate.
type orgCounter interface {
	budgetCounter
	PostChargeOrg(ctx context.Context, tokens int, windows coord.Windows) (coord.Usage, error)
}

type answerBudget struct {
	engine  *Engine
	budgets orgCounter
	now     func() time.Time
}

// basis is the company's ceilings and clock off the current epoch.
func (b answerBudget) basis() budgetBasis { return basisOf(b.engine.Company(), nil) }

// Refusing reports the company window that turns an answer away, by the same
// rule the budget park and the live meter use ([windowRefuses]): a window
// whose gate refused a charge and has admitted none since, or one with no
// room for a single token.
func (b answerBudget) Refusing(ctx context.Context) (builtin.BudgetRefusal, bool, error) {
	m := &meter{budgets: b.budgets, basis: b.basis(), now: b.now}
	r, found, err := m.refusing(ctx)
	if err != nil || !found {
		return builtin.BudgetRefusal{}, false, err
	}
	return builtin.BudgetRefusal{
		Period: string(r.Window.Period), Window: r.Window.Label,
		ResetsAt: r.Window.End, Used: r.Used, Limit: r.Limit,
	}, true, nil
}

// Charge records an answer's tokens in the company's current windows.
func (b answerBudget) Charge(ctx context.Context, tokens int) error {
	windows := coord.WindowsAt(b.now(), b.basis().zone)
	if _, err := b.budgets.PostChargeOrg(ctx, tokens, windows); err != nil {
		return fmt.Errorf("engine: charge an answer to the company: %w", err)
	}
	return nil
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
// FALSE FOR A COMPANY WHOSE KNOWLEDGE BASE IS NOT NATIVE: an external wiki
// changes without this node hearing, so a position that covered only the
// tracker would keep serving an answer whose pages had moved.
func (e *Engine) KnowledgeCorpus() (string, bool) {
	if e.NativeSearcher() == nil || e.native == nil || e.native.log == nil {
		return "", false
	}
	parts := make([]string, 0, 3)
	for _, name := range []string{
		tracker.Domain{}.Name(), pages.Domain{}.Name(), search.Domain{}.Name(),
	} {
		running := e.native.log.Domain(name)
		if running == nil || running.runner == nil {
			return "", false
		}
		parts = append(parts, running.runner.Committed().String())
	}
	return strings.Join(parts, ","), true
}
