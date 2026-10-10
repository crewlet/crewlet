package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/setup"
)

// A SITE THIS NODE COULD NOT WRITE DOWN IS THE COMPANY ADMIN'S CONFIG FAULT.
//
// The Atlassian pass discovers the site and its cloud id and records them in
// the Jira and Confluence blocks. A node with no config surface cannot, and
// what it reports names the fields to set by hand — in this company's own
// document. It was a short grant: a seat holding less than its tier, which only
// a person AT Atlassian can widen, so it sent an Atlassian admin looking for
// access nobody there is withholding, asked about again from fifteen seconds
// on.
func TestASiteThisNodeCouldNotRecordIsAConfigFault(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
integrations:
  jira:
    url: https://acme.atlassian.net
    token: t
    webhook_secret: jira-secret
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}

	pass := &atlassianPass{engine: e}
	note := pass.recordSite(t.Context(), company,
		atlassian.Site{CloudID: "cloud-1", HostURL: "https://acme.atlassian.net"}, true)
	if note == "" {
		t.Fatal("the premise is wrong: a node with no config surface recorded the site")
	}
	finding := siteNotRecorded(note)
	if finding == nil {
		t.Fatalf("the note %q produced no finding", note)
	}
	integrationtest.AssertConfigFault(t, *finding)
	if !strings.Contains(finding.Detail, "integrations.jira.cloud_id") {
		t.Errorf("the finding does not name the field to set: %q", finding.Detail)
	}

	// AND A SITE THAT WAS RECORDED, OR NEEDED NOTHING, IS NO FINDING.
	if siteNotRecorded("") != nil {
		t.Error("an empty note became a finding")
	}
}

// A DATADOG REGION THAT NAMES NO REGION IS THE COMPANY ADMIN'S CONFIG FAULT.
//
// It was a missing credential: read as Failed, and offered the API and
// application keys as the fields that clear it, over a value that is neither —
// a region is this deployment's configuration. Reached through a ${VAR}, since
// config validation refuses a literal Datadog does not serve.
func TestADatadogRegionThatIsNoRegionIsAConfigFault(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
integrations:
  datadog:
    enabled: true
    webhook_token: EXAMPLEDATADOGTOKEN0000000
    route_to: ceo
    provisioning:
      site: "${DD_SITE_NOBODY_SET}"
      api_key: dd-api
      app_key: dd-app
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	e.epoch.current.Store(company)

	findings, err := (&datadogPass{engine: e}).Run(t.Context(), setup.PassInput{})
	if err != nil {
		t.Fatalf("a region that resolves to nothing faulted rather than reporting: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the one region", findings)
	}
	if findings[0].Subject != "integrations.datadog.provisioning.site" {
		t.Errorf("the finding names %q rather than the field to set", findings[0].Subject)
	}
	integrationtest.AssertConfigFault(t, findings[0])
}

// A DATADOG REGION HELD IN A VARIABLE IS READ AS THE REGION IT NAMES.
//
// Config validation accepts a whole ${VAR} for the region and leaves the
// membership check to the resolved value, saying so at the field. Both the
// pass and the teardown built the client from the field verbatim, so the
// region reached the check as the literal "${DD_SITE}": every pass reported a
// region Datadog does not serve and every disconnect failed to build a
// client. Resolved, the pass goes on to the keys — left unresolved here, so it
// stops at their finding before any request leaves the process.
func TestADatadogRegionHeldInAVariableIsResolved(t *testing.T) {
	t.Parallel()
	e := &Engine{environ: config.MapSource{"DD_SITE": "datadoghq.eu"}}
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
integrations:
  datadog:
    enabled: true
    webhook_token: EXAMPLEDATADOGTOKEN0000000
    route_to: ceo
    provisioning:
      site: "${DD_SITE}"
      api_key: "${DD_API_KEY_NOBODY_SET}"
      app_key: "${DD_APP_KEY_NOBODY_SET}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	e.epoch.current.Store(company)

	findings, err := (&datadogPass{engine: e}).Run(t.Context(), setup.PassInput{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, f := range findings {
		if f.Subject == "integrations.datadog.provisioning.site" {
			t.Fatalf("a region the variable resolves to was refused: %+v", f)
		}
	}
	if len(findings) != 1 || findings[0].Kind != integration.FindingCredentialMissing {
		t.Errorf("findings = %+v, want the pass to reach the unresolved keys", findings)
	}
	if got := datadogSite(company.Config.Integrations.Datadog, e.resolver()); got != "datadoghq.eu" {
		t.Errorf("the teardown's region is %q, want the one the variable names", got)
	}
}
