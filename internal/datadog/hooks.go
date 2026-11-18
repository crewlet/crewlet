package datadog

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
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
// delivering at Datadog, in the shared finding vocabulary rather than prose.
//
// A NOTE WAS NOT ENOUGH: the two configuration cases (no public base URL, no
// webhook token) must produce a finding, or the pass reports nothing, the loop
// classifies the surface Ready, and every monitor fires into an address never
// registered; alerts reaching nobody look like coverage.
//
// The KIND travels with the sentence: [setup.Requirement.Blocks] is the join a
// status row picks a field from, and `webhook_token`'s requirement declares
// that its absence produces [integration.FindingCredentialMissing]. One kind
// for both cases would offer the fallback seat to fix a missing token.
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
// The engine owns this because an address written once by hand goes stale
// when the deployment moves: monitors keep firing, Datadog reports the webhook
// healthy, and alerts land nowhere. The organization credentials already
// carried for provisioning are the pair its webhook API takes.
//
// It reads before it writes, and writes only a difference: the definition is
// addressed by NAME, Datadog's own primary key, and a read hands back the
// whole shape including the header. So a pass tells "already correct" from
// "points elsewhere" exactly, and an unmoved company spends one
// GET per tick rather than a write.
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
		key := setup.BaseKey(opts.WebhookBaseKey)
		out.Blocked = &Unregistered{
			Kind:    integration.FindingIngressBlocked,
			Subject: key,
			Detail: "no webhook was registered at Datadog: this deployment has " +
				"no public base URL, so there is no address to point one at " +
				"and every monitor that fires reaches nobody. Set " + key,
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
		if opts.ReadOnly {
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
	case opts.ReadOnly:
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
		sameJSONText(current.Payload, want.Payload) &&
		sameJSONText(current.CustomHeaders, want.CustomHeaders) &&
		strings.EqualFold(current.EncodeAs, want.EncodeAs)
}

// sameJSONText compares two fields Datadog stores as TEXT and this engine
// writes as a JSON DOCUMENT, by the document rather than by the bytes.
//
// `custom_headers` and `payload` are authored as JSON and kept by Datadog in
// its own encoding (key order, spacing, a re-indent). Byte comparison would
// PUT on every tick for ever, an audit-log edit per tick over a correct
// definition, which [ensureWebhook]'s read-before-write exists to avoid and
// which the engine never sees, as it does not re-read what it wrote.
//
// Unreadable falls back to the text: a payload that is not JSON is rewritten,
// but two identical unparseable strings are the same string, and rewriting
// them is the every-tick loop again.
func sameJSONText(current, want string) bool {
	var here, there any
	if json.Unmarshal([]byte(current), &here) != nil ||
		json.Unmarshal([]byte(want), &there) != nil {
		return current == want
	}
	return reflect.DeepEqual(here, there)
}
