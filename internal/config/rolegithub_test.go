package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A SEAT WITH AN APP HALF BUILT IS NOT HELD. Creating the app and installing
// it are two acts by a person, and the second can be a day after the first,
// so the engine must be able to tell the states apart rather than minting
// against a record that cannot answer.
func TestASeatIsHeldOnlyWithAnAppAnInstallationAndAKey(t *testing.T) {
	t.Parallel()
	full := config.RoleGitHub{AppID: 7, InstallationID: 9, PrivateKey: "${SEAT_PEM}"}
	if !full.Held() {
		t.Error("a seat with an app, an installation and a key is not held")
	}
	for name, block := range map[string]config.RoleGitHub{
		"no installation yet": {AppID: 7, PrivateKey: "${SEAT_PEM}"},
		"no key":              {AppID: 7, InstallationID: 9},
		"nothing":             {},
	} {
		if block.Held() {
			t.Errorf("%s reports held", name)
		}
	}
}

// A BLOCK THAT SAYS ONLY `enabled`, OR ONLY ITS PERMISSIONS, IS NOT UNSET:
// dropped from a round trip, it would turn the agent's GitHub access off or
// forget what the agent asked for.
func TestAGitHubBlockWithOnlyItsSwitchOrPermissionsIsKept(t *testing.T) {
	t.Parallel()
	on := true
	for name, block := range map[string]*config.RoleGitHub{
		"enabled":     {Enabled: &on},
		"permissions": {Permissions: map[string]config.GitHubAccess{"contents": "read"}},
	} {
		if block.IsZero() {
			t.Errorf("a block with only its %s reads as unset", name)
		}
	}
	if !(&config.RoleGitHub{}).IsZero() || !(*config.RoleGitHub)(nil).IsZero() {
		t.Error("an empty block does not read as unset")
	}
}

// THE TIER AND THE WORKFLOWS OPT-IN ARE REFUSED BY NAME, each saying what
// replaces it: an agent's GitHub access is its App permissions, in GitHub's
// own terms.
func TestTheRetiredGitHubTierIsRefusedByName(t *testing.T) {
	t.Parallel()
	for key, replacement := range map[string]string{
		"tier: full_access": "integrations.github.permissions",
		"workflows: true":   "permissions.workflows: write",
	} {
		_, err := config.ParseCompany([]byte(`
name: Acme
providers:
  sandbox: {fake: true}
roles:
  - name: Builder
    handle: builder
    integrations:
      github:
        enabled: false
        ` + key + `
`))
		if err == nil || !strings.Contains(err.Error(), "was removed") ||
			!strings.Contains(err.Error(), replacement) {
			t.Errorf("%s: err = %v, want it refused by name, pointing at %s", key, err, replacement)
		}
	}
}
