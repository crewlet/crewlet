package types

import (
	"encoding/json"

	"github.com/crewlet/crewlet/internal/events"
)

// The inbound edge's envelope: one provider delivery, verified at the API and
// republished for the transports to route — and the record that it arrived.

func init() {
	events.Register[RawWebhook]()
	events.Register[InboundDelivery]()
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
// Nothing downstream checks again — an earlier version of this comment claimed
// the transports did, and no consumer reads a signature at all. The raw bytes
// are kept because a parser needs the delivery exactly as sent, not because a
// second verification runs on them.
type RawWebhook struct {
	// Body is the delivery's JSON, always an object.
	Body map[string]any `json:"body"`

	// Headers are the request's, with the credential-bearing ones
	// redacted. Providers put half the delivery's meaning in headers —
	// X-GitHub-Event, X-Gitlab-Event, and the signature that proves who
	// sent it.
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

	// Trigger is where on its log the record a FIRST-PARTY delivery was
	// derived from was committed — the engine's own change feed sets it,
	// as the position token (`<stream>@<generation>:<sequence>`), and no
	// vendor's delivery carries one. The seat it wakes reads no older than
	// it: the wake carries it on, and the turn hands it to its node's
	// read-your-writes floors before its first read.
	//
	// A TOKEN RATHER THAN A POSITION, because this package is below the
	// state log's and a position's three fields travel as one value.
	// Absent on a vendor's delivery, which has no floor to observe.
	Trigger string `json:"trigger,omitempty"`
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

// InboundDelivery is the RECORD that one inbound delivery reached this
// company: the row Settings › Integrations counts and lists as a surface's
// deliveries. [RawWebhook] is the wake; this is its audit row, and they are
// separate events because the wake is not stored (internal/events says why)
// while every delivery is.
//
// # One path for every surface
//
// Both inbound edges publish it — the webhook receiver for a delivery it
// verified and queued, and the Mattermost socket fleet for a post it claimed
// and queued — and the store's publish listener files it, inline on the
// publishing node or through custody on a node without `data`. The socket runs
// on every node, stateless ones included, so a row written straight into a
// node's own store would vanish with a stateless node's scratch database;
// publishing is what reaches a data node from anywhere. And with one producer
// there is one rule for what a delivery row looks like rather than one per
// edge, and every node's live projection hears a delivery rather than only the
// node that took it.
//
// # Filed under its label, with the provider's bytes
//
// The row a delivery becomes is filed under [InboundDelivery.Label] as its
// type and carries [InboundDelivery.Body] as its payload (internal/observe),
// because those are what a delivery row has always been: the provider's own
// event name an operator matches against their console, and the bytes the
// provider sent. The envelope's type names the PRODUCER's shape; the row's
// names the delivery.
//
// # Counted once
//
// A delivery is ONE PRESENTATION TO ONE SEAT, counted once across the fleet: a
// Slack or GitHub app is per seat, so a message two agents' apps both receive
// is two deliveries, and a Mattermost post two bots' sockets both read is two
// too — while every node reading the same bot's socket is still one, because
// the fleet claims the post before it is published or recorded.
type InboundDelivery struct {
	// Label is what the delivery is filed under: `webhook:<event>` from a
	// webhook route, `forge:<event>` from the Forge relay and
	// `socket:posted` from a Mattermost socket.
	Label string `json:"label"`

	// Route is the INGRESS that authenticated the delivery, which is not
	// always the integration it belongs to (the envelope's source): the
	// Forge relay hands Jira and Confluence events on under its own token,
	// so a relayed Jira event has source `jira` and route `forge`. A socket
	// delivery's route is its surface, `mattermost`.
	Route string `json:"route"`

	// Text is the one-line account a listing shows.
	Text string `json:"summary"`

	// Recipient is the seat a per-seat delivery was addressed to — a Slack
	// or GitHub app's seat, or the bot whose socket read a Mattermost post.
	// Empty for a company-wide delivery, which the notification spine
	// routes rather than the URL addresses.
	Recipient string `json:"recipient,omitempty"`

	// DeliveryKey is the provider's own identity for the delivery — the id
	// it sends in a header, or a Mattermost post id — which is what an
	// operator has in front of them in the provider's console. Empty when
	// the provider sends none.
	DeliveryKey string `json:"delivery_key,omitempty"`

	// Channel is the chat channel a socket delivery was posted in.
	Channel string `json:"channel,omitempty"`

	// Replayed marks a post a reconnecting socket read back from the gap it
	// was away for, rather than one it heard live.
	Replayed bool `json:"replayed,omitempty"`

	// Body is what the provider sent, exactly: a webhook's signed bytes, or
	// a socket post as the socket carried it.
	Body json.RawMessage `json:"body,omitempty"`
}

// EventType is the wire name.
func (InboundDelivery) EventType() string { return "inbound_delivery" }

// Summary is the delivery's own one-line account.
func (e InboundDelivery) Summary() string { return e.Text }
