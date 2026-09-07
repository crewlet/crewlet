package github

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// What an operator has to supply before GitHub events reach a seat, and
// before this engine can register a hook.
//
// THE ORGANIZATION LEADS THE FORM, because it is the one answer nobody but
// the operator has: it names where the repositories are, where each agent's
// own app is installed, and what a hook is registered on. Everything an agent
// authenticates with comes from that app instead. One app per seat, created
// and installed from the roster, its key sealed by the engine, so the connect
// form asks for no agent credential at all.
//
// The WEBHOOK SECRET is what the edge verifies a company-wide delivery
// against, and a route with nothing to check against answers 503 rather than
// accepting one. It is mintable because both ends of it belong to the engine.
//
// The ORG TOKEN is neither of those, and it no longer leads. It is read-only,
// optional, and buys exactly one thing: the list of who is participating in a
// thread, which no payload carries. Without it a review request still reaches
// its reviewer and the people merely watching hear nothing. Leading with it
// made a hand-minted personal access token the price of connecting at all, for
// a credential no agent ever acts as.

// Requirements says what this company still needs for GitHub.
func Requirements(in *config.GitHub, resolve func(string) (string, bool)) []setup.Requirement {
	var token, secret, url, org, orgHook string
	var enabled bool
	if in != nil {
		enabled = in.Enabled
		token, secret, url = in.Token, in.WebhookSecret, in.URL
		// The provisioning block is a POINTER and is absent on every
		// company that has not run a pass, which is precisely the
		// company this list is being built for.
		if p := in.Provisioning; p != nil {
			org, orgHook = p.Org, string(p.OrgWebhook)
		}
	}

	reqs := []setup.Requirement{
		{
			Field:      "enabled",
			Label:      "Accept GitHub deliveries",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.github.enabled",
			Required:   true,
			Hidden:     true,
			Default:    "true",
		},
		{
			Field:      "webhook_secret",
			Label:      "Webhook secret",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.github.webhook_secret",
			SecretName: "GITHUB_WEBHOOK_SECRET",
			Required:   true,
			// MINTABLE: GitHub signs with whatever string the hook was
			// registered with, so both ends of this value belong to the
			// engine. Running the setup pass registers the hook with the
			// value minted here, and nobody has to copy anything.
			Mintable: true,
			Help:     "Signs every delivery. Without it the route refuses all of them.",
			Blocks:   integration.FindingCredentialMissing,
		},
		{
			Field: "token",
			// NOT A CONNECT FIELD, because it establishes nothing. No
			// agent acts through it (each seat has its own app) and no
			// delivery is verified with it, so a form that opened with it
			// asked for a hand-minted personal access token before the
			// organization it would be read against had even been named.
			Label:      "Organization read token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.github.token",
			SecretName: "GITHUB_TOKEN",
			// OPTIONAL, and the consequence is specific rather than
			// total: routing still works, and participant fan-out does
			// not. Marking it required would put every company that
			// deliberately runs without one on a permanent list of
			// things to fix.
			Required: false,
			Help: "Optional and read-only: it reads who is participating in a thread. " +
				"Without it only the people a payload names are told.",
			Where:     "A token with read access to the repositories these agents work in.",
			VendorURL: "https://github.com/settings/tokens",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			Field: "url",
			// NOT A CONNECT FIELD. It is empty for github.com, which is
			// almost every company, and a form that opens with a blank
			// optional address asks an Enterprise question of everyone.
			Label:      "GitHub instance",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.github.url",
			Required:   false,
			Help:       "Leave this empty unless you run GitHub Enterprise Server.",
			Format:     "https://github.example.com",
		},
		{
			Field:      "provisioning.org",
			Connect:    true,
			Label:      "Organization",
			Kind:       setup.KindID,
			ConfigPath: "integrations.github.provisioning.org",
			Required:   true,
			Help:       "The organization whose repositories these agents work in.",
			Blocks:     integration.FindingIngressBlocked,
		},
		{
			Field:      "provisioning.org_webhook",
			Label:      "Where to register the hook",
			Kind:       setup.KindChoice,
			ConfigPath: "integrations.github.provisioning.org_webhook",
			Required:   false,
			// THE VENDOR'S OWN CLOSED SET, so a form cannot offer a
			// value the config validator refuses.
			Choices: choices(),
			Help:    "One hook for the organization, or one per repository.",
		},
	}

	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Toggle(enabled)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Held(secret, resolve)
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Held(token, resolve)
	reqs[3].Present, reqs[3].Resolved, reqs[3].Stored = setup.Plain(url)
	reqs[4].Present, reqs[4].Resolved, reqs[4].Stored = setup.Plain(org)
	reqs[5].Present, reqs[5].Resolved, reqs[5].Stored = setup.Plain(orgHook)
	return reqs
}

// choices are the org-webhook modes, read from the config package's own list
// so a value it accepts and a value this offers can never diverge.
func choices() []setup.Choice {
	labels := map[config.ContainerWebhookMode]setup.Choice{
		"auto": {
			Label: "Organization hook, falling back to each repository",
			Hint:  "One hook for everything, where the token may create one.",
		},
		"true": {
			Label: "Organization hook only",
			Hint:  "Fails rather than falling back, so a missing grant is visible.",
		},
		"false": {
			Label: "One hook per repository",
			Hint:  "For a token that may not administer the organization.",
		},
	}
	out := make([]setup.Choice, 0, len(config.ContainerWebhookModes))
	for _, mode := range config.ContainerWebhookModes {
		choice := labels[mode]
		choice.Value = string(mode)
		if choice.Label == "" {
			// A mode this file has no words for is still offered, by its
			// own name: refusing to show it would hide a setting the
			// config accepts.
			choice.Label = string(mode)
		}
		out = append(out, choice)
	}
	return out
}

// Summary is the sentence the connect form opens with.
//
// IT NAMES THE ORGANIZATION, because that is what the form now asks for. It
// used to promise that the engine "reads what each seat already has access
// to", which described a company pasting a token per agent into mcp_env and
// told an operator nothing about the two clicks that actually give an agent
// its identity here.
func Summary() string {
	return "Each agent gets its own GitHub App, so it reads, reviews and " +
		"comments as itself. Name the organization the repositories live in " +
		"and this engine registers the webhook that carries GitHub's events " +
		"to those agents."
}
