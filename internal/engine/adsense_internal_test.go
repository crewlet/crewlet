package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/tools"
)

// adSenseToolsCompany has AdSense on through the company's own OAuth client, one
// seat at operate and one off; it needs no Google Cloud connection.
const adSenseToolsCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  adsense:
    enabled: true
    oauth:
      client_id: "123456789012-abc.apps.googleusercontent.com"
      client_secret: ${GOOGLE_OAUTH_CLIENT_SECRET}
roles:
  - name: Finance
    handle: finance
    llm: zulu
    integrations:
      adsense: {enabled: true, level: operate, accounts: ["pub-1234567890123456"]}
  - name: Dev
    handle: dev
    llm: zulu
`

// AN ENGINE OFFERS ADSENSE'S TOOLS TO THE SEATS ITS LANE GRANTS: the engine's
// own wiring registers the product, a seat at operate is shown the read,
// operate and publish tools and not the admin one, and a seat that is off none.
//
// Mutation: drop AdSense from the engine's growth products, and no adsense tool
// is registered.
func TestAnEngineOffersAdSensesToolsToTheSeatsItsLaneGrants(t *testing.T) {
	t.Parallel()
	c, err := NewCompany(parseCompany(t, adSenseToolsCompany))
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
	if _, ok := reg.Lookup("adsense_read"); !ok {
		t.Fatal("the engine's growth deps registered no adsense_read")
	}
	finance, dev := shownTools(t, c, "finance", reg), shownTools(t, c, "dev", reg)
	if !slices.Contains(finance, "adsense_report") || !slices.Contains(finance, "adsense_operate") ||
		!slices.Contains(finance, "adsense_publish") || slices.Contains(finance, "adsense_admin") {
		t.Errorf("the seat at operate was shown %v", finance)
	}
	if slices.Contains(dev, "adsense_read") {
		t.Errorf("the seat that is off was shown %v", dev)
	}
}
