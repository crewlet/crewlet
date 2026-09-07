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
		token, routeTo, handleTag = in.WebhookToken, in.RouteTo, in.HandleTag
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
			Required:   false,
			Connect:    true,
			Choices:    siteChoices(),
			Help:       "The region your organization is in.",
		},
		{
			Field:      "api_key",
			Label:      "API key",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.datadog.provisioning.api_key",
			SecretName: "DATADOG_API_KEY",
			Required:   false,
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
			Required:   false,
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
			Help: "Off leaves the configuration in place and the route closed, " +
				"which is how you pause the integration without losing its setup.",
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
			Help: "Every delivery carries this in an X-Crewlet-Token header and " +
				"the engine compares it constant-time. Datadog signs nothing, " +
				"so this token is the whole check: treat it as a signing key.",
			Where: "Paste the generated value into the Datadog webhook's custom " +
				"headers as X-Crewlet-Token.",
			VendorURL: "https://app.datadoghq.com/integrations/webhooks",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			Field:      "route_to",
			Label:      "Fallback seat",
			Kind:       setup.KindHandle,
			ConfigPath: "integrations.datadog.route_to",
			Required:   true,
			Help: "The seat an alert wakes when no monitor tag names an owner. " +
				"Required, and it is the only routing floor in the whole config: " +
				"an alert is the one delivery that can legitimately name nobody, " +
				"and without a floor those alerts are accepted, verified, counted " +
				"and dropped, which looks exactly like coverage.",
			Blocks: integration.FindingCredentialMissing,
		},
		{
			Field:      "handle_tag",
			Label:      "Owner tag key",
			Kind:       setup.KindText,
			ConfigPath: "integrations.datadog.handle_tag",
			Required:   false,
			Help: "The monitor tag key that names the seat an alert wakes, so a " +
				"monitor tagged crewlet:sre-lead reaches that seat. Defaults to " +
				"crewlet; change it to match the ownership scheme your monitors " +
				"already use.",
			Format: "a Datadog tag key",
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
		"route_to":      {raw: routeTo},
		"handle_tag":    {raw: handleTag},
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
