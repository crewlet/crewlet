package types

import "github.com/crewlet/crewlet/internal/events"

// The inbound edge's envelope: one provider delivery, verified at the API and
// republished for the transports to route.

func init() {
	events.Register[RawWebhook]()
}

// RawWebhook is one authenticated provider delivery, on its way from the API's
// webhook edge to the transport that understands it.
//
// The BODY AND THE BYTES BOTH TRAVEL, and neither is redundant. The parsed
// body is what routing reads; the raw bytes are what a transport re-verifies
// against, and re-serializing the parsed form would not reproduce them — key
// order, whitespace and number formatting are all free in JSON and all inside
// the provider's HMAC. A transport handed only the parsed body could never
// check a signature again.
//
// Verification has ALREADY happened when this is published: the API refuses an
// unsigned delivery at the edge, before anything is persisted or published.
// The transports check again as defence in depth, which is what the bytes are
// for.
type RawWebhook struct {
	// Body is the delivery's JSON, always an object.
	Body map[string]any `json:"body"`

	// Headers are the request's, with the credential-bearing ones
	// redacted. Providers put half the delivery's meaning in headers —
	// X-GitHub-Event, X-Gitlab-Event, the signature a transport re-checks.
	Headers map[string]string `json:"headers"`

	// BodyRaw is the exact bytes signed. Marshals as base64.
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
