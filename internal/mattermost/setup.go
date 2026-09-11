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
	reqs := []setup.Requirement{
		{
			Field:      "enabled",
			Label:      "Use Mattermost",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.mattermost.enabled",
			Required:   true,
			Hidden:     true,
			Default:    "true",
		},
		{
			Field:      "url",
			Connect:    true,
			Label:      "Mattermost instance",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.mattermost.url",
			Required:   true,
			Help:       "Your server's address. The engine dials out, so it needs no public address.",
			Format:     "https://chat.example.com",
			// AND IT CLEARS NO FINDING, which is why it declares none.
			//
			// It claimed credential_missing once, so a row reporting a
			// token this engine could not resolve offered the instance
			// URL as the field that clears it. The repair moved it to
			// identity_missing — a truer sentence, about a finding this
			// pass then stopped producing at all: the only thing that
			// ever raised it was a seat the -decommission sweep had just
			// disabled, and setting a URL never brought one of those
			// back. An absent URL does not raise a finding either; it
			// makes NewClient refuse, so the pass FAULTS rather than
			// reporting, and there is nothing here for the join to
			// offer this field against. An empty Blocks says that, where
			// a kind nothing produces reads as a join somebody checked.
		},
		{
			Field:      "team",
			Connect:    true,
			Label:      "Team",
			Kind:       setup.KindID,
			ConfigPath: "integrations.mattermost.team",
			Required:   true,
			Help:       "The team slug the agent bots belong to and post in.",
			// NO BLOCKS EITHER, and for the same reason the URL has
			// none: without a team PlanFor refuses, so the pass faults
			// instead of reporting a finding this field could clear.
		},
	}

	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Toggle(enabled)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Plain(url)
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Plain(team)
	// The administrator credential, appended rather than declared inline
	// with the rest because it is the one whose value this function has to
	// resolve through the same seam every other secret uses.
	admin := AdminCredential()
	var adminToken string
	if in != nil && in.Provisioning != nil {
		adminToken = in.Provisioning.AdminToken
	}
	// THROUGH Held ON BOTH BRANCHES. An absent provisioning block and an
	// empty token are the same answer here — nothing is stored, and
	// Resolved stays nil rather than claiming a value was looked up — so
	// there is no second path to keep in step with this one.
	admin.Present, admin.Resolved, admin.Stored = setup.Held(adminToken, resolve)
	reqs = append(reqs, admin)
	return reqs
}

// AdminCredential is the system-administrator token this third-party app's
// provisioning and its teardown both authenticate with.
//
// Held rather than transient, for the reason [gitlab.AdminCredential] states
// at length: disabling a bot needs the authority that created it, so nothing
// held meant nothing could be taken away from here.
func AdminCredential() setup.Requirement {
	return setup.Requirement{
		Field:      "admin_token",
		Connect:    true,
		Label:      "Administrator token",
		Kind:       setup.KindSecret,
		ConfigPath: "integrations.mattermost.provisioning.admin_token",
		Required:   true,
		// BLOCKS THE FINDING THIS FIELD ACTUALLY CLEARS. It declared
		// none, and `url` and `team` (or `signing_secret`) claimed
		// credential_missing instead — none of which is the credential.
		// [setup.Requirement.Blocks] is the join a status row uses to
		// offer "the fields whose Blocks says they clear it", so a row
		// reporting a missing group credential offered everything but
		// the token that supplies one.
		Blocks: integration.FindingCredentialMissing,
		// A PATH, NOT A LINK. Mattermost serves its whole web app from one
		// route and opens user settings as a modal, so there is no address
		// for this page: every candidate answers 200 with the same
		// document, and an anchor claiming to go there would land
		// somewhere else and say nothing about it.
		//
		// So it is carried in words, in the face this screen gives a
		// literal: a menu path is a thing to follow exactly, and prose
		// runs it into the sentence around it.
		Help: "Navigate to `Profile > Security > Personal Access Tokens` " +
			"and create a token.",
	}
}

// Summary is the sentence the connect form opens with.
func Summary() string {
	return "Each agent gets its own bot account and holds an outbound " +
		"connection, so nothing has to reach this engine from outside. " +
		"Connecting needs a system administrator token, and it is kept so " +
		"the bots can be disabled again."
}
