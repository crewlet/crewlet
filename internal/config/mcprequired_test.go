package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp/catalogue"
)

// datadogDoc is a company on Datadog with one agent on at it, and servers.
const datadogDoc = `
name: Acme
seat_access_migrated: [datadog]
integrations:
  datadog:
    enabled: true
    webhook_token: EXAMPLEDATADOGTOKEN0000000
    route_to: ops
    provisioning:
      site: datadoghq.com
      api_key: "${DATADOG_API_KEY}"
      app_key: "${DATADOG_APP_KEY}"
roles:
  - name: Ops
    handle: ops
    integrations:
      datadog: {enabled: true, role: "dd-role-ops"}
mcp_servers:
`

// A REMOTE SERVER IS REFUSED WHEN NOTHING SENDS A HEADER IT REQUIRES; the
// integration's key, the entry's own header and an OAuth sign-in each send it.
// Mutation: drop the rule from validateAdmission, and the header sign-in passes.
func TestARemoteServerIsRefusedWithoutARequiredHeaderNothingSends(t *testing.T) {
	t.Parallel()
	perSeat := "  - {name: datadog, transport: http, url: \"https://mcp.datadoghq.com/v1/mcp\", shared: false}\n"
	mustCompany(t, datadogDoc+perSeat)
	mustCompany(t, datadogDoc+"  - {name: datadog, transport: http, url: \"https://mcp.datadoghq.com/v1/mcp\", "+
		"shared: false, headers: {DD_API_KEY: \"${OTHER_ORG_KEY}\"}}\n")
	mustCompany(t, "name: Acme\nmcp_servers:\n  - {name: datadog, transport: http, "+
		"url: \"https://mcp.datadoghq.com/v1/mcp\", oauth: {client: dcr}}\n")

	shared := "name: Acme\nmcp_servers:\n  - {name: datadog, transport: http, " +
		"url: \"https://mcp.datadoghq.com/v1/mcp\", headers: {DD_APPLICATION_KEY: \"${DD_APP}\"}"
	err := rejects(t, shared+"}\n", "mcp_servers[0].headers.DD_API_KEY")
	said := Problems(err)
	if !errors.Is(err, ErrMissing) || len(said) != 1 || !strings.Contains(said[0].Said, "Datadog needs DD_API_KEY") {
		t.Errorf("a header sign-in naming one key of the two: %v (the person is told %+v)", err, said)
	}
}

// A PER-AGENT SERVER IS SUPPLIED A HEADER BY ITS SEATS where a seat it reaches
// sends it (one without it is that seat's gap, on its instance), and refused
// where none does. One reaching nobody connects nowhere and lacks nothing.
func TestAPerAgentServersHeaderIsSuppliedByASeatItReaches(t *testing.T) {
	t.Parallel()
	doc := func(second string) string {
		return `
name: Acme
roles:
  - name: Ops
    handle: ops
    mcp_env:
      rovo: {Authorization: "Basic ${OPS_ROVO}"}
  - name: Dev
    handle: dev
` + second + `
mcp_servers:
  - {name: rovo, transport: http, url: "https://mcp.atlassian.com/v1/mcp", shared: false, access: {seats: [ops, dev]}}
`
	}
	mustCompany(t, doc(""))
	none := strings.Replace(doc(""), "    mcp_env:\n      rovo: {Authorization: \"Basic ${OPS_ROVO}\"}\n", "", 1)
	rejects(t, none, "mcp_servers[0].headers.Authorization")
	mustCompany(t, strings.Replace(none, "access: {seats: [ops, dev]}", "access: {seats: []}", 1))
}

// EVERY INTEGRATION VALUE THE CATALOGUE HANDS A SERVER IS READ OFF A BLOCK
// HERE, so a recognised server is never left waiting for a key nothing reads.
func TestEveryOrgKeyTheCatalogueNamesIsReadHere(t *testing.T) {
	t.Parallel()
	for _, b := range catalogue.Bindings() {
		for _, k := range b.Org {
			if _, ok := orgFields[b.Vendor][k.Field]; !ok {
				t.Errorf("%s's server reads its integration's %s, which config reads off no block", b.Vendor, k.Field)
			}
		}
	}
}
