package config

import (
	"errors"
	"strings"
	"testing"
)

// AN AGENT TURNED ON AT A LANE THAT REQUIRES A SCOPE, WITH NONE CHOSEN, IS
// REFUSED AT ITS SCOPE'S KEY; one with an entry, and any lane that does not
// require one, is not.
func TestAnAgentOnALaneThatRequiresAScopeMustNameOne(t *testing.T) {
	t.Parallel()
	on := true
	role := func(channels ...string) *Role {
		return &Role{Name: "SRE", Handle: "sre", Integrations: RoleIntegrations{
			Mattermost: &RoleMattermost{Enabled: &on, Channels: channels}}}
	}
	mattermost := func(lane string) bool { return lane == "mattermost" }
	none := func(string) bool { return false }
	here := field("roles.integrations.mattermost")

	var p problems
	checkScopeChosen(&p, here, role(), "mattermost", mattermost)
	err := p.err()
	if !errors.Is(err, ErrMissing) || !strings.Contains(err.Error(), "channels") ||
		!strings.Contains(err.Error(), "reach nothing") {
		t.Fatalf("no channels: %v, want a missing scope at its channels", err)
	}
	for name, tc := range map[string]struct {
		role     *Role
		required func(string) bool
	}{
		"a channel chosen":              {role("ops"), mattermost},
		"a lane that requires no scope": {role(), none},
	} {
		var p problems
		checkScopeChosen(&p, here, tc.role, "mattermost", tc.required)
		if err := p.err(); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	if key, entries := role("ops").SeatScope("mattermost"); key != "channels" || len(entries) != 1 {
		t.Errorf("SeatScope = %q %v", key, entries)
	}
	if key, entries := role().SeatScope("datadog"); key != "" || entries != nil {
		t.Errorf("a lane with no scope has %q %v", key, entries)
	}
}
