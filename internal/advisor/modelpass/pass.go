// Package modelpass runs the Advisor's model pass (ADR-0164): one run on the model the
// company assigns the Advisor, over tools.go's reads of [Data] (numbers, closed words and
// record ids, never anyone's words), bounded by [ceilingMeter], whose findings
// [advisor.Ground] checks before any is kept, as ADR-0157 decided for the analyst it was.
// The duty schedules it (internal/advisor/duty).
package modelpass

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// Data is what one run reads.
type Data struct {
	Company *config.Company
	// Revision is the document's revision, which a fix the pass builds is built against.
	Revision string
	Evidence advisor.Evidence
	// Rules are the rule-based findings of the same week, which it never repeats.
	Rules   []advisor.Finding
	Answers advisor.Answers
	// Unexplained are the open findings whose verdict no explanation was
	// written for yet, which the pass explains.
	Unexplained []advisor.Explainable
	// Unread names the sources the reads could not read.
	Unread []string
}

// Deps is what a run runs on.
type Deps struct {
	Provider    llm.Provider
	ProviderKey string
	Host        *toolsession.Host
	// Budget is the company's meter; nil on a node that counts nothing.
	Budget toolloop.BudgetMeter
	// DryRun is the entity route's own check of a change, in-process.
	DryRun func(advisor.Change) error
	Now    func() time.Time
}

// MaxRounds bounds a run's model requests: the reads, room to read again,
// the explanations and the findings' submission.
const MaxRounds = 16

// CeilingScope names the pass's own ceiling in a budget refusal.
const CeilingScope = "advisor"

// Ceiling is the most tokens one pass may spend, every round together: the
// measured pass over a busy twelve-agent week reads about 60 to 80 thousand
// tokens in one round, so this leaves room for a second reading and the
// submission. Charged to the company's budget and no seat's.
const Ceiling = 200_000

// Run is one pass over d: its status, what it spent, and the findings the
// engine grounded.
func Run(ctx context.Context, deps Deps, d Data, ceiling int) advisor.Pass {
	out := advisor.Pass{Status: advisor.PassDone, Model: deps.ProviderKey, Ceiling: ceiling,
		Findings: []advisor.Finding{}, Dropped: map[advisor.Drop]int{}, Unread: append([]string{}, d.Unread...)}
	surf := &surface{data: d, served: advisor.NewServed()}
	meter := &ceilingMeter{inner: deps.Budget, ceiling: ceiling}
	res, err := toolloop.Run(ctx, toolloop.Config{
		Provider: deps.Provider, ProviderKey: deps.ProviderKey, Surface: surf, Host: deps.Host,
		ServerTitle: "advisor", Messages: opening(), MaxRounds: MaxRounds, OwnPrompt: true,
		TerminateAfter: []string{SubmitTool}, Budget: meter, Now: deps.Now,
		// NO SEAT TO FENCE: its tools read only this pass's Data, and one pass at a time is the duty's lease.
		Fence: nil,
	})
	if res != nil {
		out.InputTokens, out.OutputTokens = res.InputTokens, res.OutputTokens
	}
	submitted, accepted := surf.result()
	// THE EXPLANATIONS ARE KEPT WHATEVER BECAME OF THE FINDINGS: each is
	// checked against its own finding alone.
	out.Explained = explained(surf.explanations(), d.Unexplained, out.Dropped)
	var budget *toolloop.BudgetError
	switch {
	case errors.As(err, &budget) && budget.Scope == CeilingScope:
		out.Status = advisor.PassCeiling
		out.Reason = fmt.Sprintf("The pass stopped at its ceiling of %d tokens, so this list may be short.", ceiling)
		if !accepted {
			return out
		}
	case errors.As(err, &budget):
		out.Status, out.Reason = advisor.PassFailed, "The company's token budget is spent for now, so the pass stopped."
		return out
	case err != nil:
		out.Status, out.Reason = advisor.PassFailed, "The Advisor's model did not answer: "+err.Error()
		return out
	case !accepted:
		out.Status = advisor.PassDeclined
		out.Reason = "The model declined, or did not submit findings the engine could read, so nothing it found is listed."
		return out
	}
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	findings, dropped := advisor.Ground(submitted, surf.served, advisor.Grounding{
		Company: d.Company, Rules: d.Rules, DryRun: deps.DryRun, Answered: d.Answers.Holding(now())})
	// THE EXPLANATIONS' DROPS STAY COUNTED beside the findings'.
	for drop, n := range out.Dropped {
		dropped[drop] += n
	}
	out.Findings, out.Dropped = findings, dropped
	out.Listed = len(out.Findings)
	return out
}

// ceilingMeter charges the company's meter, then holds the run to its own
// ceiling; a round past it is refused under [CeilingScope].
type ceilingMeter struct {
	inner   toolloop.BudgetMeter
	ceiling int

	mu    sync.Mutex
	spent int
}

var _ toolloop.BudgetMeter = (*ceilingMeter)(nil)

func (m *ceilingMeter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	if m.inner != nil {
		if outcome, err := m.inner.Spend(ctx, tokens); err != nil || !outcome.OK {
			m.add(tokens)
			return outcome, err
		}
	}
	if spent := m.add(tokens); spent > m.ceiling {
		return toolloop.SpendOutcome{Scope: CeilingScope, Used: spent, Limit: m.ceiling}, nil
	}
	return toolloop.SpendOutcome{OK: true}, nil
}

func (m *ceilingMeter) Refused(ctx context.Context) (toolloop.SpendOutcome, bool) {
	if m.inner != nil {
		if outcome, refused := m.inner.Refused(ctx); refused {
			return outcome, true
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.spent >= m.ceiling {
		return toolloop.SpendOutcome{Scope: CeilingScope, Used: m.spent, Limit: m.ceiling}, true
	}
	return toolloop.SpendOutcome{}, false
}

func (m *ceilingMeter) add(tokens int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spent += tokens
	return m.spent
}

// explained is every submitted explanation that names a finding the pass was
// asked to explain and cites only that finding's numbers and records, the
// first for each; every other is counted in dropped.
func explained(submitted []submittedExplanation, asked []advisor.Explainable, dropped map[advisor.Drop]int) []advisor.Explained {
	out := []advisor.Explained{}
	done := map[string]bool{}
	for _, x := range submitted {
		at := slices.IndexFunc(asked, func(e advisor.Explainable) bool { return e.ID == x.ID })
		if at < 0 || done[x.ID] || advisor.GroundExplanation(x.Text, asked[at]) != nil {
			dropped[advisor.DropExplanation]++
			continue
		}
		e := asked[at]
		done[x.ID] = true
		out = append(out, advisor.Explained{ID: e.ID, Key: e.Key, VerdictDigest: e.VerdictDigest,
			Text: strings.TrimSpace(x.Text)})
	}
	return out
}
