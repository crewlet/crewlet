package datadog

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// DefaultWebhookName is the webhook definition this engine keeps at Datadog
// when a company names none.
//
// It is the same word as [DefaultHandleTag] and that is deliberate: an
// operator writes `@webhook-crewlet` in a monitor message and tags the same
// monitor `crewlet:<handle>`, so the two halves of wiring one monitor use one
// word rather than two to remember.
const DefaultWebhookName = "crewlet"

// InboundPath is where Datadog posts. It matches the route the API serves.
const InboundPath = "/webhooks/datadog"

// WebhookNameOf is the definition this company owns.
func WebhookNameOf(cfg *config.Datadog) string {
	if cfg == nil {
		return DefaultWebhookName
	}
	return cfg.WebhookNameOrDefault()
}

// Handle is what an operator writes in a monitor message to reach this
// engine.
//
// PUBLISHED RATHER THAN GUESSED. `@webhook-` is Datadog's own prefix for a
// webhook target, and a monitor naming anything else is refused by Datadog
// before a delivery is ever attempted — so the setup screen and the docs both
// render this rather than each spelling the prefix out.
func Handle(name string) string { return "@webhook-" + strings.TrimSpace(name) }

// WebhookTarget is the address Datadog posts to, or empty where this
// deployment has no public address to register.
func WebhookTarget(base string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if trimmed == "" {
		return ""
	}
	return trimmed + InboundPath
}

// WebhookResult is what one pass did to the webhook definition.
type WebhookResult struct {
	// Name is the definition's name, which is also the handle a monitor
	// writes.
	Name string
	// URL is where it now posts, empty where nothing was registered.
	URL string
	// Created and Updated report what this pass changed, both false for a
	// definition that was already right.
	Created bool
	Updated bool
	// Blocked is why nothing of this deployment's is delivering, for the
	// cases that are a configuration rather than a failure. Nil where a
	// definition is in place.
	Blocked *Unregistered
	// Err is why the definition could not be brought into line.
	Err error
}

// Unregistered is why a pass left NO definition of this deployment's
// delivering at Datadog, said in the shared finding vocabulary rather than as
// prose.
//
// # A NOTE WAS NOT ENOUGH, and what it cost was the whole integration
//
// This was a bare sentence on [WebhookResult], and [Result.Findings] reported
// only the FAILURE case beside it. So the two configuration cases — no public
// base URL to point a definition at, no webhook token for one to carry — left
// a pass with nothing to report, the loop classified the surface Ready, and
// every monitor this company has fired into an address that was never
// registered. This package's own doc calls that shape out by name: an
// alerting integration whose alerts reach nobody is strictly worse than one
// that is switched off, because it looks like coverage.
//
// # The KIND travels with the sentence, because the two are not one finding
//
// [setup.Requirement.Blocks] is the join a status row picks a field from, and
// `webhook_token`'s requirement declares that its absence produces
// [integration.FindingCredentialMissing] — which nothing in this package
// produced. A single kind for both cases would offer an operator the fallback
// seat to fix a missing token, which is what that requirement's own comment
// says the join exists to stop.
type Unregistered struct {
	// Kind is the finding this becomes.
	Kind integration.FindingKind
	// Subject is what has to change, named as the config path rather than
	// in prose, so the field the sentence tells somebody to set and the
	// field a status row offers them cannot drift apart.
	Subject string
	// Detail is the sentence, which is also what the pass carries as a
	// note in [Result.Notes].
	Detail string
}

// desiredWebhook is the definition this deployment wants.
//
// ONE PLACE DECIDES THE SHAPE, and [ensureWebhook] compares against exactly
// what it would write. Building the wanted state twice — once to create and
// once to compare — is how a pass ends up rewriting a definition that is
// already correct on every tick.
func desiredWebhook(name, target, token string) (Webhook, error) {
	headers, err := json.Marshal(map[string]string{TokenHeader: token})
	if err != nil {
		return Webhook{}, fmt.Errorf("datadog: encode the webhook headers: %w", err)
	}
	return Webhook{
		Name:          name,
		URL:           target,
		Payload:       WebhookPayload,
		CustomHeaders: string(headers),
		EncodeAs:      "json",
	}, nil
}

// TokenHeader is the header Datadog attaches and the route checks.
//
// Datadog signs nothing, so this header IS the authentication: the webhook
// definition carries a fixed value and the route compares against
// `integrations.datadog.webhook_token`. Named here because three places have
// to agree on the spelling and the route is the only one that would notice a
// disagreement, silently, as a refused delivery.
const TokenHeader = "X-Crewlet-Token"

// ensureWebhook brings the inbound definition at Datadog into line.
//
// # Why the engine owns this at all
//
// Datadog delivers to whatever URL its Webhooks integration holds, and
// nothing else at Datadog decides whether an alert reaches this engine. Left
// to a person, that address is written once by hand and then goes stale the
// first time the deployment moves: the monitors keep firing, Datadog keeps
// reporting the webhook healthy, and the alerts land at an address that no
// longer answers. The organization credentials this block already carries for
// provisioning identities are the same pair Datadog's webhook API takes, so
// there was never a reason for a person to be the one holding this.
//
// # It reads before it writes, and writes only a difference
//
// The definition is addressed by NAME, which is Datadog's own primary key for
// one, and Datadog hands the whole shape back on a read including the header
// it carries. So a pass can tell "already correct" from "points somewhere
// else" exactly, and a company whose address has not moved spends one GET per
// tick rather than a write.
func ensureWebhook(ctx context.Context, opts Options) WebhookResult {
	name := WebhookNameOf(opts.Config)
	out := WebhookResult{Name: name}

	target := WebhookTarget(opts.WebhookBase)
	if target == "" {
		// NOT A FAILURE, AND NOT SILENCE EITHER. A pass runs with no
		// public base when this deployment has none to give, and
		// registering an address that cannot be reached is worse than
		// registering none: Datadog would report a healthy webhook over
		// deliveries that go nowhere. But neither is nothing to say —
		// this company's alerts reach nobody until somebody sets the
		// field, so it is reported as the blocked ingress it is.
		out.Blocked = &Unregistered{
			Kind:    integration.FindingIngressBlocked,
			Subject: "integrations.public_base_url",
			Detail: "no webhook was registered at Datadog: this deployment has " +
				"no public base URL, so there is no address to point one at " +
				"and every monitor that fires reaches nobody. Set " +
				"integrations.public_base_url",
		}
		return out
	}
	token := strings.TrimSpace(opts.WebhookToken)
	if token == "" {
		// A CREDENTIAL, not an ingress block, and the difference is the
		// field a status row offers: see [Unregistered].
		out.Blocked = &Unregistered{
			Kind:    integration.FindingCredentialMissing,
			Subject: "integrations.datadog.webhook_token",
			Detail: "no webhook was registered at Datadog: " +
				"integrations.datadog.webhook_token did not resolve, and a " +
				"definition carrying no token would have every delivery refused " +
				"by the route it posts to",
		}
		return out
	}

	want, err := desiredWebhook(name, target, token)
	if err != nil {
		out.Err = err
		return out
	}
	out.URL = target

	current, found, err := opts.Client.Webhook(ctx, opts.Creds, name)
	if err != nil {
		out.Err = fmt.Errorf("read the webhook named %q: %w", name,
			integration.Reject(err, Status(err)))
		return out
	}
	switch {
	case !found:
		if opts.Sink == nil {
			// A CHECK CREATES NOTHING, on the same terms as an account:
			// the missing definition is the fact a check exists to
			// report.
			out.URL = ""
			out.Blocked = &Unregistered{
				Kind:    integration.FindingIngressBlocked,
				Subject: name,
				Detail: "no webhook named " + name + " exists at Datadog yet, " +
					"so no monitor can reach this deployment",
			}
			return out
		}
		if err := opts.Client.CreateWebhook(ctx, opts.Creds, want); err != nil {
			out.Err = fmt.Errorf("register the webhook named %q: %w", name,
				integration.Reject(err, Status(err)))
			return out
		}
		out.Created = true
	case sameWebhook(current, want):
		// Already what this deployment wants, which is the ordinary
		// answer on every tick after the first.
	case opts.Sink == nil:
		out.Blocked = &Unregistered{
			Kind:    integration.FindingIngressBlocked,
			Subject: name,
			Detail: "the webhook named " + name + " at Datadog points at " +
				current.URL + " rather than " + target + ", so this " +
				"deployment receives none of the alerts it names",
		}
	default:
		if err := opts.Client.UpdateWebhook(ctx, opts.Creds, want); err != nil {
			out.Err = fmt.Errorf("move the webhook named %q to %s: %w",
				name, target, integration.Reject(err, Status(err)))
			return out
		}
		out.Updated = true
	}
	return out
}

// sameWebhook reports whether a definition is already what this deployment
// wants.
//
// EVERY FIELD THE ENGINE WRITES IS COMPARED, the header included. The token
// is rotated by writing a new value into the company document, and a
// comparison that skipped the header would leave Datadog carrying the old one
// with nothing anywhere saying so — the route would refuse every delivery and
// the integration would report itself connected.
func sameWebhook(current, want Webhook) bool {
	return current.URL == want.URL &&
		current.Payload == want.Payload &&
		sameHeaders(current.CustomHeaders, want.CustomHeaders) &&
		strings.EqualFold(current.EncodeAs, want.EncodeAs)
}

// sameHeaders compares two header sets by VALUE rather than by their text.
//
// Datadog stores the field as a JSON string and hands back its own encoding
// of it, which need not be byte-for-byte what was sent — a different key
// order or spacing would otherwise read as a difference and have every tick
// rewrite a definition that is already correct.
func sameHeaders(current, want string) bool {
	parse := func(raw string) map[string]string {
		out := map[string]string{}
		if strings.TrimSpace(raw) == "" {
			return out
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			// UNREADABLE COUNTS AS DIFFERENT. Something else wrote this,
			// and rewriting it with the shape the engine wants is the
			// only way back to a definition that delivers.
			return map[string]string{"": raw}
		}
		return out
	}
	a, b := parse(current), parse(want)
	if len(a) != len(b) {
		return false
	}
	for key, value := range b {
		if a[key] != value {
			return false
		}
	}
	return true
}
