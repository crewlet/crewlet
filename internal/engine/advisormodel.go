package engine

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/advisor/modelpass"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
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
	key := picked.key
	use := auxspend.Use{Stage: types.AuxStageBackground, Purpose: types.AuxAdvisorModel}
	deps := modelpass.Deps{
		Provider: recordedProvider{inner: picked.provider, key: key, use: use, ledger: m.e.auxSpend,
			zone: basisOf(c, nil).zone, now: m.e.now, fleet: picked.fleet, begin: picked.inFlight(m.e.ledger)},
		ProviderKey: key, Host: m.e.sessions, Now: m.e.now,
	}
	if b := m.e.backends; b != nil && b.Fleet != nil {
		meter := &meter{budgets: m.e.budgets(), basis: basisOf(c, nil), now: m.e.now}
		meter.observe(ctx)
		deps.Budget = companyMeter{meter: meter, org: b.Fleet}
	}
	return deps, nil
}
