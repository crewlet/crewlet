// Package artifactsapi is the HTTP surface for artifacts on the API listener:
// upload intents and transfers, creates and publishes, rollbacks, metadata,
// shares, trash, restore and purge, assets, pins, view tickets and frames, and
// the raw bytes of a version's files, plus the read questions the dashboard and
// the socket ask.
//
// EVERY ROUTE STATES ITS AUTHORITY where it is mounted, through the authority
// table. A caller with no relation to an artifact is answered exactly as for an
// id nothing holds, so a private artifact's existence never leaks. Writes carry
// an Idempotency-Key and act as the session's principal; a publish names the
// version it was based on and a concurrent one is refused with the live version
// to merge onto.
//
// THE API ORIGIN NEVER RENDERS AN ARTIFACT. A version's raw bytes are served
// here only as an attachment, text as plain text, under the API's own policy;
// rendering happens on the artifact origin, inside a sandboxed frame.
package artifactsapi
