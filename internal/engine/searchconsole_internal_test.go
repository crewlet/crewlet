package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/tools"
)

// searchConsoleCompany has Search Console on, one seat at operate and one off.
const searchConsoleCompany = `
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
  search_console:
    enabled: true
roles:
  - name: Growth
    handle: growth
    llm: zulu
    integrations:
      search_console: {enabled: true, level: operate, sites: ["sc-domain:example.com"]}
  - name: Dev
    handle: dev
    llm: zulu
`

// AN ENGINE OFFERS SEARCH CONSOLE'S TOOLS TO THE SEATS ITS LANE GRANTS, AND
// CHECKS ITS COMPANY HALF: a seat at operate is shown the read and operate
// tools and never an admin one, a seat that is off none, and the setup pass
// runs for the kind.
//
// Mutation: leave Search Console out of growthProducts, and no Search Console tool is
// registered.
func TestAnEngineOffersSearchConsolesToolsToTheSeatsItsLaneGrants(t *testing.T) {
	t.Parallel()
	c, err := NewCompany(parseCompany(t, searchConsoleCompany))
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
	growth, dev := shownTools(t, c, "growth", reg), shownTools(t, c, "dev", reg)
	if !slices.Contains(growth, "search_console_sites_list") || !slices.Contains(growth, "search_console_sitemap_submit") ||
		slices.Contains(growth, "search_console_admin") || slices.Contains(growth, "search_console_verify_site") {
		t.Errorf("the seat at operate was shown %v", growth)
	}
	if slices.ContainsFunc(dev, func(name string) bool { return strings.HasPrefix(name, "search_console_") }) {
		t.Errorf("the seat that is off was shown %v", dev)
	}
	if !slices.ContainsFunc(e.setupPasses(), func(p setup.Pass) bool {
		return p.Kind() == integration.KindSearchConsole
	}) {
		t.Error("no setup pass runs for Search Console")
	}
}
