package jira_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/org"
)

// THE SEATS' READS ARE THE AGENTS' JIRA CREDENTIALS. SeatCredentialReads is
// what the reconcile's identity scan reads across the organization: each
// agent seat's engine-owned token and address, then a pre-access document's
// Jira spellings and the address beside them, and nothing of a person's seat
// or of a Confluence-only credential in the shared block. A running node is
// asked for exactly these, each a reveal logged by name.
func TestSeatCredentialReadsAreTheAgentsJiraCredentials(t *testing.T) {
	t.Parallel()
	swe := &org.Role{Name: "Engineer", DeclaredHandle: "swe", MCPEnv: map[string]map[string]string{
		"atlassian": {
			"JIRA_PERSONAL_TOKEN":       "${JIRA_PAT_SWE}",
			"CONFLUENCE_PERSONAL_TOKEN": "${CONFLUENCE_PAT_SWE}",
		},
	}}
	pm := &org.Role{Name: "PM", DeclaredHandle: "pm", MCPEnv: map[string]map[string]string{
		"jira": {"JIRA_API_TOKEN": "${JIRA_TOKEN_PM}", "JIRA_EMAIL": "${JIRA_EMAIL_PM}"},
	}}
	founder := &org.Role{Name: "Founder", DeclaredHandle: "founder", Kind: org.KindHuman,
		MCPEnv: map[string]map[string]string{"jira": {"JIRA_API_TOKEN": "${JIRA_TOKEN_FOUNDER}"}}}
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{founder, pm},
		Units: []*org.Unit{{Name: "Platform", Roles: []*org.Role{swe}}}}

	want := []string{
		"ATLASSIAN_EMAIL_PM", "ATLASSIAN_EMAIL_SWE", "ATLASSIAN_TOKEN_PM", "ATLASSIAN_TOKEN_SWE",
		"JIRA_EMAIL_PM", "JIRA_PAT_SWE", "JIRA_TOKEN_PM",
	}
	if got := jira.SeatCredentialReads(o); !slices.Equal(got, want) {
		t.Errorf("SeatCredentialReads = %v, want %v", got, want)
	}
	if got := jira.SeatCredentialReads(nil); got != nil {
		t.Errorf("SeatCredentialReads(nil) = %v, want none", got)
	}
}
