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

	// The toggle is present when it is ON. Reporting `false` as written
	// down would make a paused integration look complete, and the whole
	// question this list answers is what is still missing.
	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Toggle(enabled)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Held(token, resolve)
	// Neither of the last two is a secret, so what the document says IS
	// what this process has: present and resolved are the same fact, and
	// claiming otherwise would put a permanent "cannot say" on a field an
	// operator can read straight off GET /config.
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Plain(routeTo)
	reqs[3].Present, reqs[3].Resolved, reqs[3].Stored = setup.Plain(handleTag)
	return reqs
}

// Summary is the sentence the connect form opens with.
//
// It says what connecting DOES, which for this app is unlike every other:
// Datadog delivers to the engine and the engine calls nothing back, so
// there is no account to create and no key to hold. What it needs is a way
// to verify a delivery and a way to know whose alert it is.
func Summary() string {
	return "Datadog sends firing monitors to this engine. Alerts are routed by " +
		"the owner tag on the monitor, so no account is created and no " +
		"Datadog API key is held here."
}
