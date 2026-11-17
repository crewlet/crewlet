// Package analyst runs the setup advisor's analyst: one model run over the
// week's records, served as read tools, that submits findings the engine then
// grounds ([advisor.Ground]) before anything is listed (ADR-0157).
//
// # What the model reads
//
// Only the tools in tools.go, each over [Data], the same reads `GET /advisor`
// makes: numbers, closed words, times, handles and record ids. No comment,
// message, title or transcript is served, so words somebody wrote cannot steer
// what the administrator is told; a person opens the records to read them.
//
// # What bounds a run
//
// The company's own model entries (`advisor.analyst.llm`), a token ceiling of
// its own ([ceilingMeter]) inside the company's budget, never a seat's, a
// bounded number of rounds, and two submissions.
package analyst

import (
	"context"
	"errors"
	"fmt"
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
	Company  *config.Company
	Evidence advisor.Evidence
	// Rules are the rule-based findings of the same week, which it never repeats.
	Rules   []advisor.Finding
	Answers advisor.Answers
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

// MaxRounds bounds a run's model requests: the nine reads, room to read
// again, and the two submissions.
const MaxRounds = 16

// CeilingScope names the analyst's own ceiling in a budget refusal.
const CeilingScope = "analyst"

// Run is one run over d, as its record reads: its status, what it spent, and
// the findings the engine grounded.
func Run(ctx context.Context, deps Deps, d Data, ceiling int) advisor.Analysis {
	out := advisor.Analysis{Status: advisor.StatusDone, Model: deps.ProviderKey, Ceiling: ceiling,
		Findings: []advisor.Finding{}, Dropped: map[advisor.Drop]int{}, Unread: append([]string{}, d.Unread...)}
	surf := &surface{data: d, served: advisor.NewServed()}
	meter := &ceilingMeter{inner: deps.Budget, ceiling: ceiling}
	res, err := toolloop.Run(ctx, toolloop.Config{
		Provider: deps.Provider, ProviderKey: deps.ProviderKey, Surface: surf, Host: deps.Host,
		ServerTitle: "advisor", Messages: opening(), MaxRounds: MaxRounds, OwnPrompt: true,
		TerminateAfter: []string{SubmitTool}, Budget: meter, Now: deps.Now,
	})
	if res != nil {
		out.InputTokens, out.OutputTokens = res.InputTokens, res.OutputTokens
	}
	submitted, accepted := surf.result()
	var budget *toolloop.BudgetError
	switch {
	case errors.As(err, &budget) && budget.Scope == CeilingScope:
		out.Status = advisor.StatusCeiling
		out.Reason = fmt.Sprintf("The run stopped at its ceiling of %d tokens, so this list may be short.", ceiling)
		if !accepted {
			return out
		}
	case errors.As(err, &budget):
		out.Status, out.Reason = advisor.StatusFailed, "The company's token budget is spent for now, so the run stopped."
		return out
	case err != nil:
		out.Status, out.Reason = advisor.StatusFailed, "The run's model did not answer: "+err.Error()
		return out
	case !accepted:
		out.Status = advisor.StatusRefused
		out.Reason = "The model declined, or did not submit findings the engine could read, so nothing it found is listed."
		return out
	}
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	out.Findings, out.Dropped = advisor.Ground(submitted, surf.served, advisor.Grounding{
		Company: d.Company, Rules: d.Rules, DryRun: deps.DryRun, Answered: d.Answers.Holding(now())})
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
