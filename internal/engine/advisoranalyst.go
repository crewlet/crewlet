package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/advisor/analyst"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// AnalystModels is what the advisor's analyst runs on (ADR-0156): the running
// company's `advisor.analyst.llm`, every round recorded as background spend on
// no seat and charged to the company's windows alone, and this process's tool host.
func AnalystModels(e *Engine) analyst.Models { return analystModels{e: e} }

type analystModels struct{ e *Engine }

// Deps resolves the chain off the company this node runs now; the document a
// run reads is the redacted copy, which names the same entries.
func (m analystModels) Deps(ctx context.Context, _ *config.Company) (analyst.Deps, error) {
	c := m.e.Company()
	if c == nil || c.Config == nil || c.Models == nil {
		return analyst.Deps{}, errors.New("engine: this node runs no company with models yet")
	}
	keys, err := c.Config.AnalystChain()
	if err != nil {
		return analyst.Deps{}, err
	}
	use := auxspend.Use{Stage: types.AuxStageBackground, Purpose: types.AuxAdvisorAnalysis}
	zone := basisOf(c, nil).zone
	members := make([]chain.Member, 0, len(keys))
	for _, key := range keys {
		provider, ok := c.Models.Provider(key)
		if !ok {
			return analyst.Deps{}, fmt.Errorf("engine: advisor.analyst.llm names %q, which the "+
				"running company does not build", key)
		}
		members = append(members, chain.Member{Key: key, Provider: recordedProvider{
			inner: provider, key: key, use: use, ledger: m.e.auxSpend, zone: zone, now: m.e.now}})
	}
	built, err := chain.New(members, chain.Options{OnFallback: func(f chain.Fallback) {
		log.Warn("advisor_analysis_model_fallback", "from", f.From, "to", f.To, "kind", f.Kind.String(),
			"error", f.Err)
	}})
	if err != nil {
		return analyst.Deps{}, fmt.Errorf("engine: analyst chain: %w", err)
	}
	var provider llm.Provider = built
	deps := analyst.Deps{Provider: provider, ProviderKey: keys[0], Host: m.e.sessions, Now: m.e.now}
	if b := m.e.backends; b != nil && b.Fleet != nil {
		meter := &meter{budgets: m.e.budgets(), basis: basisOf(c, nil), now: m.e.now}
		meter.observe(ctx)
		deps.Budget = companyMeter{meter: meter, org: b.Fleet}
	}
	return deps, nil
}
