package config_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// ONLY A CONNECTED INTEGRATION WAITS ON THE MIGRATION.
//
// The access pass for an integration the migration has not moved acts on no
// seat and says so, but an integration the company does not connect is not
// waiting on anything: its pass is not configured, and saying the migration
// has not finished there would put a finding on every company for every kind
// it never set up. Mutation: answer pending for an unconnected kind, or for a
// marked one, and a case here fails.
func TestOnlyAConnectedIntegrationWaitsOnTheMigration(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		c    config.Company
		kind string
		want bool
	}{
		"connected, not migrated": {c: config.Company{Integrations: config.Integrations{
			Datadog: &config.Datadog{}}}, kind: "datadog", want: true},
		"connected and migrated": {c: config.Company{Integrations: config.Integrations{
			Datadog: &config.Datadog{}}, SeatAccessMigrated: []string{"datadog"}},
			kind: "datadog", want: false},
		"not connected": {c: config.Company{}, kind: "datadog", want: false},
		"not connected, another kind migrated": {c: config.Company{
			SeatAccessMigrated: []string{"github"}}, kind: "gitlab", want: false},
		"atlassian through jira": {c: config.Company{Integrations: config.Integrations{
			Jira: &config.Jira{}}}, kind: "atlassian", want: true},
		"atlassian with only the organization": {c: config.Company{Integrations: config.Integrations{
			Atlassian: &config.Atlassian{}}}, kind: "atlassian", want: false},
	} {
		if got := tc.c.SeatAccessPending(tc.kind); got != tc.want {
			t.Errorf("%s: pending = %v, want %v", name, got, tc.want)
		}
	}
}
