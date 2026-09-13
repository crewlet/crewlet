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
			// NEITHER REQUIRED NOR OPTIONAL: required BY AN ANSWER. It is
			// the one thing standing between "every repository in the
			// organization" and that being true, and a credential the
			// other answer never uses. Stated as Required it blocked a
			// connect that needed nothing; stated as optional it let the
			// API store a choice it could not carry out.
			RequiredWhen: &setup.Gate{
				Field: "provisioning.org_webhook", Equals: string(config.ContainerWebhookRequire),
			},
			Help: "GitHub only lets a user token register an organization-wide " +
				"hook, and it needs the admin:org_hook scope. No app can carry " +
				"this, however it is installed.",
			Blocks: integration.FindingIngressBlocked,
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
			// THE ARRANGEMENT MOST COMPANIES ARE ALREADY IN, and the one
			// that needs nothing further: each agent's app carries its own
			// webhook, so connecting and installing is the whole setup.
			//
			// It was `true` — the strictest value, demanding a scope most
			// connectors do not have — which is what produced a permanent
			// Action required on the ordinary connect. The operator was
			// then told to install the agents' apps, which can never carry
			// admin:org_hook, so doing exactly as the card asked changed
			// nothing.
			Default: string(config.ContainerWebhookNever),
			Help: "Each agent's own app already delivers what happens in the " +
				"repositories it is installed on.",
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
// # Two answers, from three modes, deliberately
//
// It offered the config's own closed set, worded as registration topology:
// "Organization hook, falling back to each repository" / "Organization hook
// only" / "One hook per repository". Every one of those is true and none of
// them is the question an operator has — which is whose activity their agents
// hear about. And the arrangement most dashboard connects actually land in
// was named by NONE of them, because it is not a hook at all: each agent's
// own app carries its own webhook, and that is expressed by
// `integrations.github.token` being empty, a different field, described in
// its help text.
//
// `auto` IS ABSENT because it is not an answer to this question. It means
// "try for the whole organization and quietly take less if you cannot", which
// leaves a company believing it has one hook when it has several — each one
// its own thing to maintain, and none of them covering a repository made
// tomorrow. Every state it reaches is reachable by picking one of these two
// and knowing which one you picked.
//
// `false` WITH A `repos` LIST is absent for a different reason: it is a real
// arrangement and a rare one, it needs a list of repositories this form has
// no way to help somebody build, and it is not narrowed away — YAML keeps it,
// [config.ContainerWebhookModes] still accepts all three, and the pass still
// hooks every repository a company names. What this list decides is which
// questions are worth putting to somebody connecting from a dashboard.
//
// THE VALUES ARE STILL THE CONFIG'S OWN, so a form cannot offer something the
// validator refuses, and a company that set `auto` by hand keeps it: nothing
// here rewrites a stored answer, because [setup.Requirement.Default] is a
// suggestion for a company that has none.
func choices(org string) []setup.Choice {
	where := "your organization"
	if org = strings.TrimSpace(org); org != "" {
		where = org
	}
	return []setup.Choice{
		{
			Value: string(config.ContainerWebhookNever),
			Label: "Repositories where an agent's app is installed",
			Hint: "Recommended. Nothing else to set up — each agent hears " +
				"about its own repositories.",
		},
		{
			Value: string(config.ContainerWebhookRequire),
			Label: "Every repository in " + where + ", including new ones",
			Hint:  "Needs a user token with admin:org_hook.",
		},
	}
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
