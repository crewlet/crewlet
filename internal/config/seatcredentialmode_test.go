package config_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// WHO HOLDS A SEAT'S CREDENTIAL IS READ PER INTEGRATION, FROM THE AUTHORITY THE
// ENGINE ACTUALLY HOLDS.
//
// The engine provisions an integration only where the company gave it what the
// vendor's account API takes; everywhere else a seat's credential was handed in
// from outside Crewlet. Each kind is held to both answers, and the shapes an
// operator renders (both the Slack and the GitHub blocks always, on a document
// only it writes) to external where they grant no creation. Mutation: call a
// block with no provisioning the engine's, an operator's GitHub provisioning
// block, or a Slack block without its app-configuration token, and a case here
// fails.
func TestSeatCredentialModeIsReadPerIntegration(t *testing.T) {
	t.Parallel()
	engine, external := config.SeatCredentialEngine, config.SeatCredentialExternal
	cloud, dc := config.AtlassianCloud, config.AtlassianDataCenter
	slackToken := &config.Slack{AppConfigToken: "${SLACK_APP_CONFIG_TOKEN}"}
	githubProvisioned := &config.GitHub{Enabled: true, Provisioning: &config.GitHubProvisioning{}}
	for name, tc := range map[string]struct {
		in      config.Integrations
		kind    string
		managed bool
		want    config.SeatCredentialMode
	}{
		"datadog provisioned": {in: config.Integrations{Datadog: &config.Datadog{
			Enabled: true, Provisioning: &config.DatadogProvisioning{}}}, kind: "datadog", want: engine},
		"datadog alerts only": {in: config.Integrations{Datadog: &config.Datadog{}},
			kind: "datadog", want: external},
		// SWITCHED OFF, its pass does not run: the authority is held and
		// not used.
		"datadog switched off": {in: config.Integrations{Datadog: &config.Datadog{
			Provisioning: &config.DatadogProvisioning{}}}, kind: "datadog", want: external},
		"gitlab provisioned": {in: config.Integrations{GitLab: &config.GitLab{Enabled: true,
			Provisioning: &config.GitLabProvisioning{}}}, kind: "gitlab", want: engine},
		"gitlab off": {in: config.Integrations{GitLab: &config.GitLab{
			Provisioning: &config.GitLabProvisioning{}}}, kind: "gitlab", want: external},
		"gitlab without provisioning": {in: config.Integrations{GitLab: &config.GitLab{
			Enabled: true}}, kind: "gitlab", want: external},
		"atlassian cloud": {in: config.Integrations{Atlassian: &config.Atlassian{
			Deployment: cloud}}, kind: "atlassian", want: engine},
		"atlassian data center": {in: config.Integrations{Atlassian: &config.Atlassian{
			Deployment: dc}}, kind: "atlassian", want: external},
		"jira without the organization": {in: config.Integrations{Jira: &config.Jira{}},
			kind: "atlassian", want: external},
		"mattermost provisioned": {in: config.Integrations{Mattermost: &config.Mattermost{
			Enabled: true, Provisioning: &config.MattermostProvisioning{}}}, kind: "mattermost", want: engine},
		"mattermost by hand": {in: config.Integrations{Mattermost: &config.Mattermost{}},
			kind: "mattermost", want: external},
		"github provisioned": {in: config.Integrations{GitHub: githubProvisioned},
			kind: "github", want: engine},
		"github apps by hand": {in: config.Integrations{GitHub: &config.GitHub{}},
			kind: "github", want: external},
		"slack with its app-configuration token": {in: config.Integrations{Slack: slackToken},
			kind: "slack", want: engine},
		"slack apps by hand": {in: config.Integrations{Slack: &config.Slack{}},
			kind: "slack", want: external},
		"slack not connected": {in: config.Integrations{}, kind: "slack", want: external},
		"a kind nobody provisions": {in: config.Integrations{Jira: &config.Jira{}},
			kind: "jira", want: external},
		// THE SHAPES AN OPERATOR RENDERS: both blocks always, for apps made
		// outside Crewlet, on a document only it writes. A GitHub block's
		// provisioning gives the engine no app it can record there, and a
		// Slack block without the token no app it can create; a Slack block
		// the operator renders with the token, and a Datadog provisioning
		// block, are the engine's as anywhere else.
		"operator github": {in: config.Integrations{GitHub: githubProvisioned},
			kind: "github", managed: true, want: external},
		"operator slack": {in: config.Integrations{Slack: &config.Slack{}},
			kind: "slack", managed: true, want: external},
		"operator slack with its token": {in: config.Integrations{Slack: slackToken},
			kind: "slack", managed: true, want: engine},
		"operator datadog": {in: config.Integrations{Datadog: &config.Datadog{
			Enabled: true, Provisioning: &config.DatadogProvisioning{}}}, kind: "datadog", managed: true,
			want: engine},
	} {
		c := &config.Company{Integrations: tc.in}
		if got := c.SeatCredentialMode(tc.kind, tc.managed); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
	if got := (*config.Company)(nil).SeatCredentialMode("datadog", false); got != external {
		t.Errorf("no company: %s, want external", got)
	}
}
