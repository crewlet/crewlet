// Package capability is what one agent seat can reach, as the seat is told it:
// a pure snapshot of its access, the closed list of what counts as a change to
// it, the difference between two snapshots, and the two sections of a turn's
// prompt that say them.
//
// # Told the truth on every turn, and the difference once
//
// A seat's executor continues its harness's session from turn to turn, and an
// earlier turn's statement about its access ("I cannot reach production")
// stays in that session after the access changes. So two things are rendered
// from the [Snapshot]: "## Your access" ([YourAccess]) in the system prompt of
// every turn, which is current and says so, and "## What changed since your
// last turn" ([ChangesBlock]) in the user message of the first turn after a
// change, which states the change and that it replaces anything remembered or
// said before. The baseline a change is measured against is what the seat was
// last TOLD, never the previous revision of the company document, so any
// number of revisions between two turns are told as one difference, a revert
// is told as a change back, and re-applying an unchanged revision (how a
// credential is rotated) is no change at all.
//
// # Pure, and holding no secret
//
// [Snapshot] reads its [Input] and nothing else: no clock, no store and no
// vendor. Level names and notes come from the vendors' own words, as their
// fixed sets or a live read hand them in ([Words]). A snapshot holds levels,
// permissions, scope, tool states, MCP tool names, policy text and setup step
// names, and never a credential, a variable's value, a sealed name or a setup
// step's files, commands or environment: what it holds is shown to the seat
// and written to the access log.
//
// # Canonical
//
// A snapshot encodes as canonical JSON (keys sorted at every level, every list
// sorted), so equal access is equal bytes and one [Snapshot.Digest] on every
// node, which is what makes "Your access" byte-stable while the access is.
package capability
