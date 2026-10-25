// Package artifactsapi is the HTTP surface for artifacts on the API listener:
// upload intents and the engine-streamed transfer, creates and publishes,
// rollbacks, renames and labels, the share dialog (grant, revoke, transfer and
// the effective access), links to tracker work items, trash, restore and
// purge, assets, pins, the frames the
// dashboard embeds a page in, a doc's source and the raw bytes of
// a version's files; and the one admission ([Gate]) the read questions in
// internal/api/queries take too.
//
// # Every route states its authority, and access comes first
//
// Every route is mounted through the authority table (authz.Router). A verb
// the table decides without the artifact is the route's own policy: a create
// asks the artifacts app, a purge is an administrator's with a fresh proof of
// identity and a person present. A verb that needs the artifact's owner and
// shares is mounted on the caller having resolved, and the handler reads the
// artifact's head and decides the verb on it before a version, a file, an
// upload or a byte is touched. A caller with no relation to an artifact is
// answered exactly as for an id nothing holds, so a private artifact's
// existence never leaks; a relation below the verb's role is refused naming the
// role, so the dashboard can say whom to ask for what.
//
// # The authority is stamped
//
// What the table concluded (the caller's exact role on the artifact, whether
// only the admin path admitted them, the plan quotas in force, the surface the
// request came through and the work item it was made for) is stamped into the
// record every write forms, and the domain's applier decides from the record
// alone. Writes carry an Idempotency-Key, from which every record id is
// derived, so a retry is the same operation; a publish names the version it was
// based on (`If-Match: "<n>"`) and one based on a head that has moved is
// refused `412 version_moved`, carrying the live head, its manifest and its
// entry's text to merge onto. Forcing past a moved head is a person's, with a
// person present.
//
// # The admin path is recorded before it is answered
//
// An administrator may open any artifact, because admin is host access and a
// refusal would promise a privacy the engine cannot keep. Every read only the
// admin path admitted writes an elevated-read record first, one per day,
// administrator, artifact and version, which the artifact's history shows its
// owner and editors; a read whose record cannot be written is refused. A
// listing never shows an artifact on the admin path at all.
//
// # The API origin never renders an artifact
//
// A page runs on the artifact origin, in a sandboxed frame, under a view
// ticket bound to the credential the viewer signed in through; this surface
// only mints the frame. A doc runs no author code, so its source
// is answered as JSON for the dashboard to render. A version's raw bytes are
// served here only as an attachment, text and anything a browser could run as
// plain text, under the API's own policy.
//
// # A link reads the work item from the caller's own create on
//
// Linking an artifact to a work item reads the item from Worklet first, so a
// link to an item nobody filed is refused. A caller that has just filed the
// item names the position the create answered with as `after`, and the item
// is read through Worklet's own read-your-writes floor, the one
// `min_position` sets on every Worklet read: the read waits up to the read
// budget for this node to apply the create, a node that does not is refused
// `503 unavailable` with the state log's `behind` refusal and its Retry-After,
// and a position on another log is refused as the request's mistake. Without
// `after` the item is read at the dashboard's own level, as every decision
// here reads.
//
// # What a reader is shown
//
// The owner and editors see an artifact's audience, its elevated reads and who
// has opened it (while the company shows that at all); a viewer and a commenter
// see the owner and how many shares there are.
package artifactsapi
