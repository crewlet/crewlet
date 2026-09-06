package github

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// What an operator has to supply before GitHub events reach a seat, and
// before this engine can register a hook.
//
// Two credentials, and they do different jobs. The ORG TOKEN is read-only and
// optional: without it a delivery still routes, but the engine cannot look up
// who is participating in a thread, so a review request reaches its reviewer
// and nobody else. The WEBHOOK SECRET is what the edge verifies every
// delivery against, and a route with nothing to check against answers 503
// rather than accepting one, so it is the difference between an integration
// that works and one that silently refuses everything.
//
// GitHub issues no credential on a provisioner's behalf, which is why only
// the webhook secret is mintable: the engine invents that one because both
// ends of it are the engine's, and it cannot invent a personal access token
// no GitHub account has ever seen.

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
			Help: "Off leaves the configuration in place and the route closed, " +
				"which is how you pause the integration without losing its setup.",
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
			Help: "Every delivery is signed with this and checked at the edge. A " +
				"route with nothing to check against answers 503 rather than " +
				"accepting a delivery it cannot verify.",
			Blocks: integration.FindingCredentialMissing,
		},
		{
			Field:      "token",
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
			Help: "Read-only. Without it a review request still reaches its " +
				"reviewer, and nobody else participating in the thread hears " +
				"anything, because the engine cannot look them up.",
			Where: "A fine-grained token with read access to the repositories " +
				"these agents work in, or a classic token with repo:read.",
			VendorURL: "https://github.com/settings/tokens",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			Field:      "url",
			Label:      "GitHub instance",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.github.url",
			Required:   false,
			Help: "Leave this empty unless you run GitHub Enterprise Server. " +
				"The API and web addresses are both derived from it.",
			Format: "https://github.example.com",
		},
		{
			Field:      "provisioning.org",
			Label:      "Organization",
			Kind:       setup.KindID,
			ConfigPath: "integrations.github.provisioning.org",
			Required:   false,
			Help: "The organization whose repositories these agents work in. " +
				"Needed to register a webhook; without it the setup pass reads " +
				"and registers nothing.",
			Blocks: integration.FindingIngressBlocked,
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
			Help: "An organization hook is one registration for every repository. " +
				"Falling back to per-repository hooks is what a token without " +
				"organization admin can still do.",
		},
	}

	yes := enabled
	reqs[0].Present, reqs[0].Resolved = enabled, &yes
	reqs[1].Present, reqs[1].Resolved = setup.Resolution(secret, resolve)
	reqs[2].Present, reqs[2].Resolved = setup.Resolution(token, resolve)
	reqs[3].Present, reqs[3].Resolved = literal(url)
	reqs[4].Present, reqs[4].Resolved = literal(org)
	reqs[5].Present, reqs[5].Resolved = literal(orgHook)
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

// literal is the resolution of a plain config value: written down is the
// whole of it.
func literal(value string) (bool, *bool) {
	present := value != ""
	yes := present
	return present, &yes
}
