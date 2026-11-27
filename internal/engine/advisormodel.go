package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/advisor/modelpass"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// AdvisorModels is what the Advisor's model pass runs on (ADR-0164): the
// running company's `advisor.model`, every round recorded as background spend
// on no seat and charged to the company's windows alone, and this process's
// tool host.
func AdvisorModels(e *Engine) modelpass.Models { return advisorModels{e: e} }

type advisorModels struct{ e *Engine }

// Assigned reports whether the company this node runs assigns the Advisor a model.
func (m advisorModels) Assigned() bool {
	c := m.e.Company()
	return c != nil && c.Config != nil && c.Config.Advisor.Model != ""
}

// Deps resolves the model off the company this node runs now, as the helper's
// run of it with the harness's own tools off (config.LLMProvider.HelperOverrides);
// the document a pass reads is the redacted copy, which names the same entry.
func (m advisorModels) Deps(ctx context.Context, _ *config.Company) (modelpass.Deps, error) {
	c := m.e.Company()
	if c == nil || c.Config == nil || c.Models == nil {
		return modelpass.Deps{}, errors.New("engine: this node runs no company with models yet")
	}
	ref, err := c.Config.AdvisorModel()
	if err != nil {
		return modelpass.Deps{}, err
	}
	picked, err := helperPick(c, "advisor.model", ref, "advisor", true)
	if err != nil {
		return modelpass.Deps{}, err
	}
	use := auxspend.Use{Stage: types.AuxStageBackground, Purpose: types.AuxAdvisorModel}
	provider, err := picked.called(func(key string, inner llm.Provider) llm.Provider {
		return recordedProvider{inner: inner, key: key, use: use, ledger: m.e.auxSpend,
			zone: basisOf(c, nil).zone, now: m.e.now, fleet: picked.fleet, begin: picked.inFlight(m.e.ledger, key)}
	})
	if err != nil {
		return modelpass.Deps{}, err
	}
	deps := modelpass.Deps{Provider: provider, ProviderKey: picked.key, Host: m.e.sessions, Now: m.e.now}
	if b := m.e.backends; b != nil && b.Fleet != nil {
		meter := &meter{budgets: m.e.budgets(), basis: basisOf(c, nil), now: m.e.now}
		// A FULL WINDOW the tool loop refuses before the first round, from
		// what observe reads; an UNREADABLE one observe passes over, so it
		// is refused here, as every optional auxiliary pass refuses one
		// ([Engine.auxGate]): the pass waits for its next run rather than
		// spend against a ceiling nobody could read.
		if meter.basis.capped() {
			if _, _, err := meter.refusing(ctx); err != nil {
				m.e.metrics.Add(metrics.AuxiliaryDeclined, 1,
					metrics.Attrs{"pass": "advisor", "reason": learning.SkipBudgetUnknown})
				return modelpass.Deps{}, fmt.Errorf("the company's token budget could not be read, "+
					"so the pass does not start until its next run: %w", err)
			}
		}
		meter.observe(ctx)
		deps.Budget = companyMeter{meter: meter, org: b.Fleet}
	}
	return deps, nil
}
