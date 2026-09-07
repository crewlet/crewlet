package mattermost

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// What an operator has to supply before agents can talk on Mattermost.
//
// THE SHORTEST LIST HERE, and for a reason worth stating: this third-party app holds
// one outbound websocket per seat and verifies no inbound delivery, so it
// needs no public address, no webhook secret and no shared token. A
// self-hosted engine behind a firewall works with it unchanged, which is the
// whole point of the integration.
//
// What it does need is a bot account per agent, and creating one is an
// administrator's act. That credential is [AdminCredential], and it is held:
// disabling a bot again needs the same authority that created it.

// Requirements says what this company still needs for Mattermost.
func Requirements(in *config.Mattermost, resolve func(string) (string, bool)) []setup.Requirement {
	var url, team string
	var enabled bool
	if in != nil {
		enabled, url, team = in.Enabled, in.URL, in.Team
	}
	_ = resolve // nothing here is a credential; every field is a plain setting.

	reqs := []setup.Requirement{
		{
			Field:      "enabled",
			Label:      "Use Mattermost",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.mattermost.enabled",
			Required:   true,
			Help: "Off leaves the configuration in place and every seat's socket " +
				"closed, which is how you pause the integration without losing " +
				"its setup.",
		},
		{
			Field:      "url",
			Label:      "Mattermost instance",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.mattermost.url",
			Required:   true,
			Help: "Your server's address. The engine dials out to it, so it needs " +
				"no public address of its own for this integration.",
			Format: "https://chat.example.com",
			Blocks: integration.FindingCredentialMissing,
		},
		{
			Field:      "team",
			Label:      "Team",
			Kind:       setup.KindID,
			ConfigPath: "integrations.mattermost.team",
			Required:   true,
			Help:       "The team slug the agent bots belong to and post in.",
			Blocks:     integration.FindingCredentialMissing,
		},
	}

	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Toggle(enabled)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Plain(url)
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Plain(team)
	// The administrator credential, appended rather than declared inline
	// with the rest because it is the one whose value this function has to
	// resolve through the same seam every other secret uses.
	admin := AdminCredential("")
	if in != nil && in.Provisioning != nil {
		admin = AdminCredential(in.Provisioning.AdminToken)
		admin.Present, admin.Resolved, admin.Stored = setup.Held(in.Provisioning.AdminToken, resolve)
	}
	reqs = append(reqs, admin)
	return reqs
}

// AdminCredential is the system-administrator token this third-party app's
// provisioning and its teardown both authenticate with.
//
// Held rather than transient, for the reason [gitlab.AdminCredential] states
// at length: disabling a bot needs the authority that created it, so nothing
// held meant nothing could be taken away from here.
func AdminCredential(stored string) setup.Requirement {
	return setup.Requirement{
		Field:      "admin_token",
		Label:      "Administrator token",
		Kind:       setup.KindSecret,
		ConfigPath: "integrations.mattermost.provisioning.admin_token",
		Required:   true,
		Present:    stored != "",
		Stored:     stored,
		Help: "Creates each agent's bot account, mints its token and joins it " +
			"to the team, so it needs system administrator rights. It is " +
			"kept, sealed, because disabling those bots again needs the " +
			"same authority.",
		Where: "A personal access token belonging to a system administrator.",
	}
}
