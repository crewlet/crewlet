package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/tools"
)

// growthToolsCompany has Google Analytics on, one seat at read and one off.
const growthToolsCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  google_cloud:
    enabled: true
    provisioning:
      identity_project: acme-crewlet-id
      provider: ` + gcpProvider + `
      federation_key: "${` + googlecloud.FederationKeySecret + `}"
  google_analytics:
    enabled: true
roles:
  - name: Growth
    handle: growth
    llm: zulu
    integrations:
      google_analytics: {enabled: true, level: read, properties: ["111"]}
  - name: Dev
    handle: dev
    llm: zulu
`

// AN ENGINE OFFERS A GROWTH PRODUCT'S TOOLS TO THE SEATS ITS LANE GRANTS: the
// growth tools are registered only with every dependency they act through,
// the access log included, so the engine's own wiring is what is tested, and
// a seat at read is shown the read tool and not the operate one.
//
// Mutation: drop the access log from GrowthToolDeps, and no growth tool is
// registered.
func TestAnEngineOffersAGrowthProductsToolsToTheSeatsItsLaneGrants(t *testing.T) {
	t.Parallel()
	c, err := NewCompany(parseCompany(t, growthToolsCompany))
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	e := &Engine{}
	e.epoch.current.Store(c)
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Authorize: builtin.Decide(orgchart.Of(c.Org)), Requesters: turnRequesters{engine: e},
		Growth: GrowthToolDeps(e),
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup("google_analytics_read"); !ok {
		t.Fatal("the engine's growth deps registered no google_analytics_read")
	}
	growth, dev := shownTools(t, c, "growth", reg), shownTools(t, c, "dev", reg)
	if !slices.Contains(growth, "google_analytics_read") || slices.Contains(growth, "google_analytics_operate") {
		t.Errorf("the seat at read was shown %v", growth)
	}
	if slices.Contains(dev, "google_analytics_read") {
		t.Errorf("the seat that is off was shown %v", dev)
	}
}
