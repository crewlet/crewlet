package datadog

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// What an operator has to supply before monitor alerts reach a seat.
//
// The engine does the rest of the wiring itself: with the organization
// credential pair below it registers the webhook definition at Datadog,
// points it at this deployment, writes the payload template the route
// decodes and attaches the shared token as a header. What is left for a
// person is the half only they can do, which is naming that webhook in the
// monitors they want an agent woken by.
//
// The webhook URL is deliberately NOT a requirement, and now for a stronger
// reason than before: it is not an input at all but `<public_base_url>` plus
// the inbound path, and the engine WRITES it at Datadog on every pass.
// Modelling it as a field would invite somebody to type one the next
// reconcile would overwrite.

// Requirements says what this company still needs for Datadog.
//
// A nil block is a company that has not started, and it gets the same list
// with everything absent rather than an empty one: the fields are what the
// screen renders a form from, so answering nothing would leave an operator a
// Connect button that opens an empty dialog.
func Requirements(in *config.Datadog, resolve func(string) (string, bool)) []setup.Requirement {
	var token, routeTo, handleTag string
	var enabled bool
	if in != nil {
		token, routeTo = in.WebhookToken, in.RouteTo
		handleTag = in.HandleTag
		enabled = in.Enabled
	}

	reqs := []setup.Requirement{
		{
			// THE ORGANIZATION CREDENTIALS, and they are what makes
			// every other field here work. They register the webhook
			// that carries the alerts and they create each agent's
			// account: an enabled block without them serves a route,
			// checks a token and receives nothing, because nothing at
			// Datadog was ever told this deployment exists. Config
			// validation refuses that shape rather than reporting it
			// connected.
			Field:      "site",
			Label:      "Datadog region",
			Kind:       setup.KindChoice,
			ConfigPath: "integrations.datadog.provisioning.site",
			Required:   true,
			Connect:    true,
			Choices:    siteChoices(),
			Default:    DefaultSite,
			Help:       "The region your organization is in.",
		},
		{
			Field:      "api_key",
			Label:      "API key",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.datadog.provisioning.api_key",
			SecretName: "DATADOG_API_KEY",
			Required:   true,
			Connect:    true,
			Help:       "Create one on your",
			LinkText:   "API keys page",
			VendorURL:  "https://app.datadoghq.com/organization-settings/api-keys",
		},
		{
			Field:      "app_key",
			Label:      "Application key",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.datadog.provisioning.app_key",
			SecretName: "DATADOG_APP_KEY",
			Required:   true,
			Connect:    true,
			Help:       "Create one on your",
			LinkText:   "application keys page",
			VendorURL:  "https://app.datadoghq.com/organization-settings/application-keys",
		},
		{
			// WITHOUT THIS, CONNECTING DOES NOTHING. Every check the
			// config makes on this block is gated on `enabled`, so a
			// company that filled in a token and a fallback seat and
			// left the toggle off has a block that validates, reports
			// itself complete, and serves no route. It is a requirement
			// rather than a side effect of submitting, because turning
			// an integration off without deleting it is a thing an
			// operator does on purpose and the same field is what does
			// it.
			Field:      "enabled",
			Label:      "Accept Datadog deliveries",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.datadog.enabled",
			Required:   true,
			Hidden:     true,
			Default:    "true",
		},
		{
			Field:      "webhook_token",
			Label:      "Shared token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.datadog.webhook_token",
			SecretName: "DATADOG_WEBHOOK_TOKEN",
			Required:   true,
			// MINTABLE, because both ends of this token are things the
			// operator sets: Datadog sends whatever header value they
			// paste into its webhook form, and the engine compares
			// against whatever the config names. There is nothing to
			// agree with, so asking a person to invent one is asking
			// them to invent a password.
			Mintable: true,
			Help: "Datadog signs nothing, so this token is the whole check " +
				"on a delivery. The engine attaches it to the webhook it " +
				"registers; nothing has to be pasted anywhere.",
			VendorURL: "https://app.datadoghq.com/integrations/webhooks",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			// THE ONE THING A PERSON STILL HAS TO WRITE DOWN AT DATADOG.
			// The engine keeps the definition; a monitor reaches it only
			// by naming it, so the form's job here is to tell the
			// operator the handle rather than to collect a value.
			//
			// Nameable because the name is Datadog's primary key for a
			// webhook: one organization watched by a staging deployment
			// and a production one needs two, or each rewrites the
			// other's address on every pass.
			Field:      "webhook_name",
			Label:      "Webhook name",
			Kind:       setup.KindText,
			ConfigPath: "integrations.datadog.webhook_name",
			Required:   false,
			Default:    DefaultWebhookName,
			Help: "The engine registers this webhook at Datadog. Add " +
				"\"@webhook-{webhook_name}\" to any monitor whose alerts " +
				"an agent should see.",
			VendorURL: "https://app.datadoghq.com/integrations/webhooks",
		},
		{
			Field:      "handle_tag",
			Label:      "Monitor tag key",
			Kind:       setup.KindText,
			ConfigPath: "integrations.datadog.handle_tag",
			// OPTIONAL, AND THE DEFAULT IS THE ANSWER FOR ALMOST EVERYONE.
			// Unlike the fallback seat, a blank here is not an unanswered
			// question: the engine reads [DefaultHandleTag], which is a
			// working setting rather than a hole. The field exists because
			// the key becomes a tag on the operator's own monitors, beside
			// conventions they already have.
			Required: false,
			Default:  DefaultHandleTag,
			// THE EXAMPLE FOLLOWS THE FIELD. `{handle_tag}` is substituted
			// by the form from what is typed, so somebody who changes the
			// key reads the example for the key they now have rather than
			// for the default they just replaced. The seat is left as a
			// placeholder because this package has no roster to draw a real
			// handle from, and an invented one would name a seat the
			// company may not have.
			Help: "The monitor tag key that names an owner. A monitor tagged " +
				"\"{handle_tag}:<seat handle>\" wakes that seat.",
		},
		{
			Field:      "route_to",
			Label:      "Fallback seat",
			Kind:       setup.KindHandle,
			ConfigPath: "integrations.datadog.route_to",
			Required:   true,
			// NONE IS AN ANSWER, and the roster is appended to it.
			//
			// A company may want only the monitors it has labelled to wake
			// anybody, and every other alert to stay with whatever Datadog
			// already does about it. That is a decision, and a decision is
			// not the same as leaving the field blank: blank is a question
			// nobody answered, and an alert reaching nobody through it is a
			// silent hole in the coverage.
			Choices: []setup.Choice{{
				Value: config.DatadogIgnore,
				Label: "None: dismiss alerts nobody owns",
				Hint:  "Only monitors tagged with a seat wake an agent.",
			}},
			// AND IT IS THE ONE OFFERED, because it is the only answer
			// that is right for a company the form knows nothing about.
			//
			// Every other choice here is one of THIS company's seats, and
			// picking one for them is picking whose phone rings for every
			// alert nobody labelled — which is a decision about somebody's
			// working life, made by a form, from a roster it has no basis
			// for ranking. "Choose one" made the field an unanswerable
			// question at exactly the moment an operator wanted to be
			// done; this makes it an answerable one, and the answer it
			// suggests is the conservative half of the pair: an alert that
			// wakes nobody is a gap somebody can close later, where an
			// alert waking the wrong agent is work already misrouted.
			Default: config.DatadogIgnore,
			Help:    "The seat an alert wakes when no monitor tag names an owner.",
			// A ROUTING FLOOR IS NOT A CREDENTIAL. It claimed
			// credential_missing, so a row reporting a Datadog key this
			// engine could not resolve offered the fallback seat as the
			// field that clears it — and the token that does declared
			// nothing. [setup.Requirement.Blocks] is the join a status
			// row picks its fields from.
			Blocks: integration.FindingIngressBlocked,
		},
	}

	var webhookName string
	if in != nil {
		webhookName = in.WebhookName
	}
	var site, apiKey, appKey string
	if in != nil && in.Provisioning != nil {
		site, apiKey, appKey = in.Provisioning.Site, in.Provisioning.APIKey, in.Provisioning.AppKey
	}

	// RESOLVED BY FIELD, not by position. This was seven index writes
	// against a literal above it, so reordering the form — which is a
	// copy decision — silently paired one field's value with another's
	// row: the API key would have reported the fallback seat's presence.
	values := map[string]struct {
		raw    string
		sealed bool
		toggle bool
	}{
		"site":          {raw: site},
		"api_key":       {raw: apiKey, sealed: true},
		"app_key":       {raw: appKey, sealed: true},
		"enabled":       {toggle: true},
		"webhook_token": {raw: token, sealed: true},
		"webhook_name":  {raw: webhookName},
		"handle_tag":    {raw: handleTag},
		"route_to":      {raw: routeTo},
	}
	for i := range reqs {
		v, ok := values[reqs[i].Field]
		if !ok {
			continue
		}
		switch {
		case v.toggle:
			// Present when it is ON. Reporting `false` as written down
			// would make a paused integration look complete, and what
			// is still missing is the whole question this list answers.
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(enabled)
		case v.sealed:
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(v.raw, resolve)
		default:
			// Not a secret, so what the document says IS the value and
			// there is nothing to resolve.
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Plain(v.raw)
		}
	}
	return reqs
}

// Summary is the sentence the connect form opens with.
//
// It says what connecting DOES, and this app has TWO halves that a reader
// has to be able to tell apart: the keys give each agent an identity at
// Datadog, and the routing fields below decide which agent an alert wakes.
// A company can have the second without the first, which is what the
// sentence has to leave room for.
func Summary() string {
	return "Crewlet registers a webhook at Datadog and gives each agent its own " +
		"account, so alerts arrive and every agent owns what it builds. " +
		"Connecting takes keys from someone who can manage users and roles."
}

// siteChoices offers Datadog's regions, from the client's own list so a
// region this build can reach and a region the form offers cannot diverge.
func siteChoices() []setup.Choice {
	regions := Sites()
	out := make([]setup.Choice, 0, len(regions))
	for _, site := range regions {
		out = append(out, setup.Choice{Value: site, Label: site})
	}
	return out
}
