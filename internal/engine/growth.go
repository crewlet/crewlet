package engine

import (
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleads"
	"github.com/crewlet/crewlet/internal/googleanalytics"
)

var (
	_ builtin.GrowthProduct = (*googleanalytics.Product)(nil)
	_ builtin.GrowthProduct = (*googleads.Product)(nil)
)

// growthProducts are the growth products this engine's tools reach, each on
// the running document, its resolver, the engine's tokens and its access log.
func (e *Engine) growthProducts() []builtin.GrowthProduct {
	return []builtin.GrowthProduct{
		googleanalytics.New(e.runningConfig, e.resolver, e, e.GrowthAudit(), nil),
		googleads.New(e.runningConfig, e.resolver, e, e.GrowthAudit(), spendLeases{engine: e}, e.GrowthAudit(), nil),
	}
}

// runningConfig is the running document, nil before the first epoch.
func (e *Engine) runningConfig() *config.Company {
	if c := e.Company(); c != nil {
		return c.Config
	}
	return nil
}

// GrowthToolDeps is what the growth tools act through on this engine: every
// one of them, the access log a held call records its ask on included, since
// the tools are registered only with all of them.
func GrowthToolDeps(e *Engine) builtin.GrowthDeps {
	if e == nil {
		return builtin.GrowthDeps{}
	}
	return builtin.GrowthDeps{Products: e.growthProducts(), Requesters: turnRequesters{engine: e},
		Chart: ChartAuthorityOf(e), Audit: e.GrowthAudit()}
}
