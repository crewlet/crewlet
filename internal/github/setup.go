package github

import (
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// What an operator has to supply before GitHub events reach a seat, and
// before this engine can register a hook.
//
// THE ORGANIZATION LEADS THE FORM: the one answer only the operator has.
// Agents authenticate through per-seat apps the engine seals, so no agent
// credential is asked for. The WEBHOOK SECRET is mintable because both ends
// belong to the engine; with none the edge answers 503.
//
// THE ORG TOKEN IS ASKED FOR ONLY FOR THE HOOK: fan-out is answered by each
// agent's own app ([SeatLookup]), but without it `org_webhook: true` raises
// a finding naming `integrations.github.token`, and no app installation
// carries `admin:org_hook`. Optional, declaring the finding it clears.

// Requirements says what this company still needs for GitHub.
func Requirements(in *config.GitHub, resolve func(string) (string, bool)) []setup.Requirement {
	var secret, url, org, orgHook, token string
	var enabled bool
	if in != nil {
		enabled = in.Enabled
		secret, url, token = in.WebhookSecret, in.URL, in.Token
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
			Hint:     "Signs every delivery. Without it, the route refuses all of them.",
			// PROVED BY THE FIRST DELIVERY, and by nothing sooner: GitHub
			// never presents the secret, it signs with it.
			CheckNote: "GitHub proves it with the first delivery it signs.",
			// INGRESS, NOT CREDENTIAL, which jira and confluence already
			// declare for the same field. It said credential_missing, and
			// this pass emits that kind nowhere for the webhook secret —
			// every ingress state it reports is ingress_blocked — so the
			// join was dangling and the screen offered this field for
			// nothing that could ever ask for it.
			Blocks: integration.FindingIngressBlocked,
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
			Hint:       "Only for GitHub Enterprise Server.",
			Format:     "https://github.example.com",
		},
		{
			Field:      "provisioning.org",
			Connect:    true,
			Label:      "Organization",
			Kind:       setup.KindID,
			ConfigPath: "integrations.github.provisioning.org",
			Required:   true,
			Hint:       "The organization these agents work in.",
			Blocks:     integration.FindingIngressBlocked,
		},
		{
			Field:      "provisioning.org_webhook",
			Label:      "Which GitHub activity should reach your agents",
			Kind:       setup.KindChoice,
			ConfigPath: "integrations.github.provisioning.org_webhook",
			Required:   false,
			// THE VENDOR'S OWN CLOSED SET, so a form cannot offer a
			// value the config validator refuses.
			Choices: choices(org),
			// THE ARRANGEMENT MOST COMPANIES ARE ALREADY IN, needing nothing
			// further: each agent's app carries its own webhook, so connecting
			// and installing is the whole setup. Defaulting to `true` demands
			// a scope most connectors lack and, since no app can carry
			// admin:org_hook, raised a permanent Action required that
			// installing the apps could never clear (see [noRegistrarReason]).
			//
			// `true`, not `auto`, is the org-wide choice offered: auto falls
			// back to per-repository hooks silently, leaving a company that
			// thinks it has one hook with several to maintain.
			Default: string(config.ContainerWebhookNever),
			Hint:    "Which repositories send their events to the agents.",
		},
		{
			// THE TOKEN THE COVERAGE ANSWER ABOVE NEEDS, placed under it: a
			// field a choice reveals belongs beneath that choice.
			//
			// An organization-wide hook needs `admin:org_hook`, which only a
			// user token carries; no app installation grants it, so
			// installing every agent's app cannot clear the finding.
			//
			// OPTIONAL, because a company hooking each agent's own app needs
			// none. It blocks the same finding the warning carries, so the
			// form shows it to whoever reads that warning, not every connect.
			Field:      "token",
			Label:      "Organization token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.github.token",
			SecretName: "GITHUB_TOKEN",
			Checkable:  true,
			// NEITHER REQUIRED NOR OPTIONAL: required BY AN ANSWER. It is
			// the one thing standing between "every repository in the
			// organization" and that being true, and a credential the
			// other answer never uses. Stated as Required it blocked a
			// connect that needed nothing; stated as optional it let the
			// API store a choice it could not carry out.
			RequiredWhen: &setup.Gate{
				Field: "provisioning.org_webhook", Equals: string(config.ContainerWebhookRequire),
			},
			Hint:   "A user token with the " + orgHookScope + " scope.",
			More:   "GitHub lets only a user token register an organization-wide hook. No app can carry it.",
			Blocks: integration.FindingIngressBlocked,
		},
	}

	// RESOLVED BY FIELD, not by position, which datadog's own list says at
	// length and this one had not been given yet. Five index writes against
	// a literal above them means inserting a field — the organization token
	// here — silently pairs every later field's value with the row before
	// it: the hook choice would have reported the organization's presence,
	// and nothing would have failed to compile.
	values := map[string]struct {
		raw    string
		sealed bool
		toggle bool
	}{
		"enabled":                  {toggle: true},
		"webhook_secret":           {raw: secret, sealed: true},
		"url":                      {raw: url},
		"provisioning.org":         {raw: org},
		"token":                    {raw: token, sealed: true},
		"provisioning.org_webhook": {raw: orgHook},
	}
	for i := range reqs {
		v, ok := values[reqs[i].Field]
		if !ok {
			continue
		}
		switch {
		case v.toggle:
			// Present when it is ON: reporting `false` as written down
			// would make a paused integration look complete.
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(enabled)
		case v.sealed:
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(v.raw, resolve)
		default:
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Plain(v.raw)
		}
	}
	return reqs
}

// choices are the two coverage arrangements this form offers, named by what
// they COVER rather than by where a hook is registered.
//
// Two answers from three modes: the commonest arrangement (each agent's own
// app carries its webhook) is no hook mode at all. `auto` IS ABSENT: it
// quietly takes less than the whole organization, leaving a company thinking
// it has one hook when it has several. `false` WITH `repos` is absent as
// rare; YAML keeps it ([config.ContainerWebhookModes]).
//
// THE VALUES ARE THE CONFIG'S OWN, so the validator never refuses what the
// form offers, and a stored `auto` is never rewritten.
func choices(org string) []setup.Choice {
	where := "your organization"
	if org = strings.TrimSpace(org); org != "" {
		where = org
	}
	return []setup.Choice{
		{
			Value: string(config.ContainerWebhookNever),
			Label: "Repositories where an agent's app is installed",
			Hint: "Recommended. Nothing else to set up: each agent hears " +
				"about its own repositories.",
		},
		{
			Value: string(config.ContainerWebhookRequire),
			Label: "Every repository in " + where + ", including new ones",
			Hint:  "Needs a user token with " + orgHookScope + ".",
		},
	}
}

// orgHookScope is the user token scope an organization-wide hook needs, which
// no App installation carries at any permission.
const orgHookScope = "admin:org_hook"

// Guide is what the connect form says around its fields; the token step is
// only for an organization-wide hook, which only a user token can register.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Each agent gets its own GitHub App, so it reads, reviews and comments as itself. " +
			"Crewlet registers the webhook for you.",
		Prepare: []setup.Step{
			{Text: "Name the organization your repositories live in.", Yields: "provisioning.org"},
			{
				Text:   "Only for organization-wide events, create a user token with:",
				Grants: []string{orgHookScope}, Yields: "token",
			},
			{Text: "Connect, then create each agent's app from the Agents tab."},
		},
	}
}
