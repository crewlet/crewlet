package main

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
)

// THE STARTUP LINE STATES WHO REACHES WHAT (ADR-0031): how many keys of each
// role this node accepts, beside the anonymous posture. Counted off the GUARD,
// which decides, rather than off Tier A — so a disabled guard, which accepts
// no listed key at all, reports none of either beside `auth_disabled` instead
// of counting keys that authenticate nobody.
func TestTheStartupLineCountsKeysByRole(t *testing.T) {
	t.Parallel()
	boot := config.DefaultBootstrap()
	boot.API.Auth.Tokens = []config.APIToken{
		{ID: "ada", Role: config.RoleMember, Token: "a"},
		{ID: "grace", Role: config.RoleMember, Token: "b"},
		{ID: "founder", Role: config.RoleAdmin, Token: "c"},
	}
	if members, admins := keysByRole(auth.New(&boot)); members != 2 || admins != 1 {
		t.Errorf("keysByRole = %d members, %d admins; want 2 and 1", members, admins)
	}

	boot.API.Auth.Disabled = true
	if members, admins := keysByRole(auth.New(&boot)); members != 0 || admins != 0 {
		t.Errorf("a disabled guard counted %d members and %d admins; it accepts no key", members, admins)
	}
}
