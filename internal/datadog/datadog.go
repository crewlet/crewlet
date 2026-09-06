// Package datadog turns a firing monitor into a seat's work.
//
// # What is genuinely Datadog's own here
//
// Every other inbound surface in this tree routes by IDENTITY: a comment
// names a login, an issue names an account id, a chat message names a user.
// A Datadog alert names none of those. It is a monitor changing state, and
// the only thing on it that could say whose problem that is, is the
// MONITOR'S TAGS.
//
// So this is the one vendor whose routing is ownership rather than mention,
// and the difference is not a gap to close later. Datadog's `@` syntax in a
// monitor message is its OWN notification-target grammar, resolved by Datadog
// against its integrations before the delivery is ever made: `@webhook-x` is
// what reaches this engine at all, and an operator writing `@ceo` there gets
// a Datadog warning about an unknown target rather than a routed alert.
// Reading the message for mentions would be reading a field that structurally
// cannot carry one.
//
// Tags can, and already do. Ownership tags are how Datadog users say whose
// service a monitor watches, so routing on one asks an operator to write down
// something they have usually written down already.
//
// # Two tiers and a floor, rather than the usual four
//
//  1. A tag naming the seat, `crewlet:<handle>` by default, and a monitor may
//     carry several so one alert can wake a team.
//  2. Failing that, the company's `route_to` seat.
//
// The floor is not optional and that is deliberate: config validation refuses
// an enabled Datadog block with no `route_to`. An alerting integration whose
// alerts reach nobody is strictly worse than one that is switched off,
// because it looks like coverage. The tag is the override; the floor is the
// guarantee that a page at three in the morning wakes somebody.
//
// # The payload is a template, and the engine names the one it expects
//
// Datadog posts an EMPTY BODY unless the webhook definition carries a payload
// template, and the template is written by whoever creates the webhook rather
// than fixed by the vendor. So there is no canonical Datadog alert shape to
// decode: there is the shape this engine asks for, which
// docs/integrations/datadog.md publishes and [Alert] decodes. Every field is
// optional on the way in, because a template somebody edited is a
// configuration mistake rather than a reason to drop a firing monitor.
package datadog

// Backend is the source name on the wire.
//
// It matches the webhook route's own source, the config block's key, and
// integration.KindDatadog. Four places need this word and none of them may
// disagree: a parser registered under a name the route does not publish is a
// parser nothing ever reaches, and the failure is silent because an unrouted
// delivery is still verified, stored and counted.
const Backend = "datadog"
