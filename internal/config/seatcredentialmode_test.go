package config_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// WHO HOLDS A SEAT'S CREDENTIAL IS READ FROM THE DOCUMENT, PER INTEGRATION.
//
// The engine provisions an integration only where the company gave it the
// authority the vendor's account API takes; everywhere else a seat's
// credential was handed in from outside Crewlet. Each kind is held to both
// answers. Mutation: call a block with no provisioning the engine's, or an
// Atlassian Data Center site, and a case here fails.
func TestSeatCredentialModeIsReadPerIntegration(t *testing.T) {
	t.Parallel()
	engine, external := config.SeatCredentialEngine, config.SeatCredentialExternal
	cloud, dc := config.AtlassianCloud, config.AtlassianDataCenter
	for name, tc := range map[string]struct {
		in   config.Integrations
		kind string
		want config.SeatCredentialMode
	}{
		"datadog provisioned": {config.Integrations{Datadog: &config.Datadog{
			Provisioning: &config.DatadogProvisioning{}}}, "datadog", engine},
		"datadog alerts only": {config.Integrations{Datadog: &config.Datadog{}}, "datadog", external},
		"gitlab provisioned": {config.Integrations{GitLab: &config.GitLab{Enabled: true,
			Provisioning: &config.GitLabProvisioning{}}}, "gitlab", engine},
		"gitlab off": {config.Integrations{GitLab: &config.GitLab{
			Provisioning: &config.GitLabProvisioning{}}}, "gitlab", external},
		"gitlab without provisioning": {config.Integrations{GitLab: &config.GitLab{Enabled: true}},
			"gitlab", external},
		"atlassian cloud": {config.Integrations{Atlassian: &config.Atlassian{Deployment: cloud}},
			"atlassian", engine},
		"atlassian data center": {config.Integrations{Atlassian: &config.Atlassian{Deployment: dc}},
			"atlassian", external},
		"jira without the organization": {config.Integrations{Jira: &config.Jira{}}, "atlassian",
			external},
		"mattermost provisioned": {config.Integrations{Mattermost: &config.Mattermost{
			Provisioning: &config.MattermostProvisioning{}}}, "mattermost", engine},
		"mattermost by hand": {config.Integrations{Mattermost: &config.Mattermost{}}, "mattermost",
			external},
		"github provisioned": {config.Integrations{GitHub: &config.GitHub{
			Provisioning: &config.GitHubProvisioning{}}}, "github", engine},
		"github apps by hand":      {config.Integrations{GitHub: &config.GitHub{}}, "github", external},
		"slack":                    {config.Integrations{Slack: &config.Slack{}}, "slack", engine},
		"slack not connected":      {config.Integrations{}, "slack", external},
		"a kind nobody provisions": {config.Integrations{Jira: &config.Jira{}}, "jira", external},
	} {
		c := &config.Company{Integrations: tc.in}
		if got := c.SeatCredentialMode(tc.kind); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
	if got := (*config.Company)(nil).SeatCredentialMode("datadog"); got != external {
		t.Errorf("no company: %s, want external", got)
	}
}
