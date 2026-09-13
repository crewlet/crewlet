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
// THE ORG TOKEN IS ASKED FOR, AND ONLY FOR THE HOOK. It led the form once,
// for a job it no longer has: participant fan-out — the list of who is in a
// thread — is answered by each agent's own app now ([SeatLookup]), scoped to
// what that agent may see rather than to whatever the person who minted the
// token could reach. So it was demoted to optional, and then removed from
// the form entirely.
//
// Removing it went one step too far. The field kept its OTHER job, which is
// the whole organization-level client: with none, the pass registers nothing
// at GitHub, and `org_webhook: true` says so in a finding naming
// `integrations.github.token`. A form with no box for that field left the
// warning pointing at a setting the dashboard could not set — so an operator
// installed every agent's app, as the card had been asking, and watched it
// sit there, because an App installation carries no `admin:org_hook` and
// never will. It is back, optional, declaring the finding it clears, so the
// screen offers it to whoever is reading that warning and to nobody else.

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
			Help:     "Signs every delivery. Without it the route refuses all of them.",
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
			// THE TOKEN THE HOOK CHOICE ABOVE NEEDS, asked for here because
			// the warning about it named a field the dashboard had no box
			// for.
			//
			// An organization-wide hook needs `admin:org_hook`, which only
			// a user token carries: an App installation grants nothing of
			// the sort, so installing every agent's app — the thing the
			// card spends its time asking for — cannot clear it, and an
			// operator who did exactly as they were told watched the same
			// finding sit there. Measured, and reported as the install not
			// having worked.
			//
			// OPTIONAL, because it genuinely is for the default shape: a
			// company hooking each agent's own app needs none. It blocks
			// the same finding the warning carries, so the form puts this
			// field in front of whoever is reading that warning rather
			// than every connect.
			Field:      "token",
			Label:      "Organization token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.github.token",
			SecretName: "GITHUB_TOKEN",
			Required:   false,
			Help: "A user token with admin:org_hook, for one organization-wide " +
				"hook. Leave empty to let each agent's own app carry its own.",
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
			// ONE ORGANIZATION HOOK IS WHAT MOST COMPANIES WANT, and it is
			// the one that keeps covering repositories created after this
			// run — the difference between a new repository routing on day
			// one and routing whenever somebody remembers.
			//
			// `true` rather than `auto`, which differs only in what
			// happens when the hook cannot be made: auto falls back to
			// per-repository hooks silently, and a fallback nobody was
			// told about is a company that thinks it has one hook and has
			// several, each needing the same maintenance. This one says
			// so. That visibility is only affordable because the token
			// above is now askable — defaulting to `true` over a form with
			// no box for it would have put a permanent finding on every
			// fresh connect, which is the bug [noRegistrarReason] was
			// narrowed to remove.
			Default: string(config.ContainerWebhookRequire),
			Help:    "One hook for the organization, or one per repository.",
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
