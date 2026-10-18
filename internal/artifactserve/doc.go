// Package artifactserve is the artifact ORIGIN: the handler of the api.artifacts
// listener, which serves the bytes of artifact versions and nothing else.
//
// AN ARTIFACT RUNS WHERE NOTHING CAN BE STOLEN. An artifact is arbitrary HTML,
// SVG and script written by a person, an agent, or an agent somebody
// prompt-injected, so it is served from a listener that mounts no guard, reads
// no cookie, writes none, and answers no API route; the dashboard embeds it in a
// sandboxed frame and talks to it only through a message port. A path-borne view
// ticket names one principal, one artifact and one immutable version.
//
// REVOCATION IS THE NEXT REQUEST. A document is re-authorized on every request
// against a verdict cache that the artifacts and identity appliers invalidate
// the moment a share, a trash or a credential moves; a subresource may be
// served on a verdict at most thirty seconds old. A node whose own copy is stale
// or out of service decides through a healthy peer, and answers unavailable
// rather than serve from rows that may still hold a revoked share.
//
// THE SCRIPT POLICY IS STRICT AND CHEAP. Every document gets a fresh nonce by a
// byte substitution of the placeholder the linter wrote at publish, so the
// policy admits the page's own scripts and the scripts they load, and nothing an
// attacker injects.
package artifactserve
