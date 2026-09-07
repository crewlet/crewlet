package datadog

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// What an operator has to supply before monitor alerts reach a seat.
//
// Datadog is the one surface here where "ready" is reachable purely by
// collecting inputs. The engine calls no Datadog API, creates nothing, and
// has no provisioner: it serves a route, checks a shared token, and routes by
// a tag the operator puts on their own monitors. So every requirement below
// is either something a person pastes into Datadog's own webhook form, or
// something the engine can generate.
//
// The webhook URL is deliberately NOT a requirement. It is not an input at
// all: it is `<public_base_url>/webhooks/datadog`, which the setup surface
// shows the operator to copy. Modelling it as a field would invite somebody
// to type a different one, which the engine would store and never serve.

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
			// THE PROVISIONING HALF, and every field in it is optional
			// on purpose. A company that pastes the engine's address
			// into Datadog's own webhook form needs none of it: alerts
			// arrive and route with the three fields above alone, which
			// is the whole of what this integration was before the
			// engine could call Datadog back. Filling these in is
			// asking for something more — an identity per agent — and
			// leaving them empty must not report a company incomplete.
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
			Mintable:  true,
			Help:      "Datadog signs nothing, so this token is the whole check on a delivery.",
			Where:     "Paste it into the Datadog webhook's X-Crewlet-Token header.",
			VendorURL: "https://app.datadoghq.com/integrations/webhooks",
			Blocks:    integration.FindingCredentialMissing,
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
			Help: "The monitor tag key that names an owner. Tag a monitor " +
				"\"crewlet:sre-lead\" and its alerts wake SRE Lead.",
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
			Help:   "The seat an alert wakes when no monitor tag names an owner.",
			Blocks: integration.FindingCredentialMissing,
		},
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
	return "Each agent gets its own Datadog account, so it owns what it builds. " +
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
