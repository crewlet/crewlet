package types

import "github.com/crewlet/crewlet/internal/events"

// The inbound envelope: one delivery for the transports to route — a
// provider's, verified at the API, or a record a change feed relayed.

func init() {
	events.Register[RawWebhook]()
}

// RawWebhook is one delivery on its way to the transport that understands it:
// a provider's, authenticated at the API's webhook edge, or a record a change
// feed relayed from one of the engine's own logs.
//
// THE BODY AND ITS BYTES BOTH TRAVEL, and neither is redundant. The map is
// what routing reads, and every number in it decodes as a float64, the type a
// parser reading a number out of it asserts. The bytes are the delivery exactly as
// its publisher had it, a number among them as its writer's digits — which a
// parser reading an id or a version needs, and which re-encoding the map
// cannot give back once a float64 has rounded it.
//
// Verification has ALREADY happened when a provider's delivery is published:
// the API refuses an unsigned one at the edge, before anything is persisted or
// published, and nothing downstream checks a signature again.
type RawWebhook struct {
	// Body is the delivery's JSON, always an object.
	Body map[string]any `json:"body"`

	// Headers are the request's, with the credential-bearing ones
	// redacted. Providers put half the delivery's meaning in headers —
	// X-GitHub-Event, X-Gitlab-Event, and the signature that proves who
	// sent it.
	Headers map[string]string `json:"headers"`

	// BodyRaw is the delivery's exact bytes, where its publisher had them:
	// what the provider sent, for a delivery the webhook edge took — which
	// for one Forge relayed is Forge's own envelope around Body rather than
	// Body itself — and Body's own encoding, for a record a change feed
	// relayed. Empty for a delivery a chat socket read, and for a record
	// relayed by a change feed on a build that predates the feed's bytes:
	// a parser that needs the bytes then encodes Body, whose numbers are
	// float64s. Marshals as base64.
	BodyRaw []byte `json:"body_raw"`

	// Handle names the seat a per-seat delivery was addressed to. Slack
	// gives each agent its own app, so the URL path carries the seat and
	// nothing in the body does.
	Handle string `json:"handle,omitempty"`

	// ForgeAtlassianID is the Atlassian account behind a Forge-relayed
	// event. Forge strips the actor from the payload it relays and states
	// it once at the top level, so a transport that only read Body would
	// attribute every Cloud event to nobody.
	ForgeAtlassianID string `json:"forge_atlassian_id,omitempty"`
}

// EventType is the wire name every transport subscribes under.
func (RawWebhook) EventType() string { return "raw_webhook" }

// SummaryFor names the SOURCE rather than the actor, and takes it from the
// envelope: the delivery has not been routed yet, so no seat owns it and the
// actor resolves to the API itself.
//
// The source is deliberately not a payload field. "source" is an envelope key,
// and a payload field that collides with one is silently dropped on the wire —
// so the fact would have travelled on some builds and not on others, with
// nothing failing either way.
func (e RawWebhook) SummaryFor(actor string) string {
	if actor == "" {
		return "received a webhook"
	}
	return "received a " + actor + " webhook"
}
