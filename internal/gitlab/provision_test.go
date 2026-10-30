package gitlab_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
)

// enabledGitLab is a GitLab block that provisions.
func enabledGitLab() *config.GitLab {
	return &config.GitLab{
		Enabled: true, URL: "https://gitlab.example.com",
		SigningSecret: "${GITLAB_SIGNING_SECRET}",
		Provisioning:  &config.GitLabProvisioning{Group: "nimbus"},
	}
}

// A SEAT'S ACCOUNT IS NAMED FROM ITS HANDLE, which is immutable, because GitLab
// keeps no field of this engine's own: the name is how the next pass finds
// the account it made.
func TestAnAccountIsNamedFromItsSeatsHandle(t *testing.T) {
	t.Parallel()
	p := &config.GitLabProvisioning{Group: "nimbus"}
	if got := gitlab.Username(p, "sre"); got != "crewlet-sre" {
		t.Errorf("default prefix = %q", got)
	}
	p.UsernamePrefix = " agent "
	if got := gitlab.Username(p, "sre"); got != "agent-sre" {
		t.Errorf("configured prefix = %q", got)
	}
}
