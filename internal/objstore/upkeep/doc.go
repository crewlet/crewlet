// Package upkeep keeps the fleet's chunks where the placement map says they
// belong: the duty that maintains the map, and the three passes every data
// node runs over its own disk — repair, which fetches what this node should
// hold and does not; collection, which deletes what nothing needs it to hold;
// and the scrub, which reads every chunk it holds once a week and removes the
// ones that rotted or will not read, for repair to replace.
//
// # The map is the company's, kept by whoever holds the duty
//
// ONE node at a time maintains the stored map ([Maintainer]), and the duty
// moves between nodes on a lease — so nothing the map depends on may be a fact
// about the node that happens to hold it. The copies every chunk keeps and the
// label they are spread across come from the COMPANY configuration
// (ADR-0020), stamped with the activation they came from so a holder a
// revision behind cannot set them back; absences are counted in the
// maintainer's own TICKS rather than timed on its clock
// ([membership.OutTicks]); and everything a tick has to remember rides in the
// stored record ([objstore.MapState]) rather than in the holder's memory.
// [Next] is the whole of the policy, pure, one tick at a time.
//
// WHO IS IN THE MAP is not this package's own: the estate map answers the same
// questions about the same data nodes, so the lifecycle is written once, in
// internal/membership (ADR-0008), and [Next] composes it — a member gone or
// failed for a grace is removed, but never onto nothing; one that FLAPS is
// removed too; a removed node seen back is a member again at once but ON
// PROBATION, read from and repaired from and placed on only after the same
// span of presence; an operator may take a member OUT ([Out]) so its data
// moves while it still serves, and HOLD the map ([HoldFor]) through planned
// maintenance. What is this map's own is what a change of members means for
// what it places: the change is balanced in an epoch of its own, and the
// groups SPLIT as the fleet grows, one bit per epoch and only on a clean
// fleet, the split's epoch changing nothing else — so every lower child keeps
// its parent's holders and a doubling moves half the data rather than all of
// it.
//
// # The inventory is derived, never kept
//
// No pass reads a list of chunks. Every data node holds the whole replicated
// estate, and every row that refers to an object names its chunks and their
// SLOTS ([Source]), so which chunks the company references in any run of slots
// is a query this node answers from its own tables — and which of them it
// should hold is a pure function of the map. A list kept beside the estate
// would be a second answer to that question, and the two would drift the first
// time a write landed in one and not the other.
//
// Both passes read by SLOT RANGE — a group at whatever group count the map
// has, since every group is a run of slots — never more than a 256th of the
// slots at once, so a node holding every group still holds one read's chunks
// in memory at a time.
//
// # Repair reads as new an estate as collection does
//
// A repair pins the estate first, on the barrier collection takes: a node back
// from being away repairs what was written while it was gone rather than what
// it last saw. Its status says whether it reached every group this node holds
// and, of the chunks placed here, how many it holds, fetched and still lacks —
// the PENDING the maintainer waits on before it splits groups, and an operator
// waits on before stopping the next node. What it could not fetch is told
// apart: MISSING is a chunk every member answered it does not hold, the one
// count that is data lost; UNREACHABLE is one a member that did not answer
// may hold, and the pass is retried for it.
//
// # Deletion is the only dangerous thing here, and it has two rules
//
// A chunk NOTHING REFERENCES is deleted once it is older than [PendingGrace]
// and the estate this node read was current and complete — current because
// the pass first waits for everything the log had committed when it started
// ([Source.Barrier]), complete because a record this node could not decode
// might be the one naming the chunk. The grace covers the other window: bytes
// are uploaded BEFORE the record that names them is written, so every chunk
// is unreferenced for a while at the start of its life.
//
// A chunk that IS referenced but that this node's map does not place here —
// left by an older map, by a write that went past a silent holder, or on a
// member taken out — is deleted only when EVERY member this node's map places
// it on says it holds an INTACT copy of the chunk — read and checked before it
// answers, so a copy that rotted never vouches for deleting a good one — AND
// that its OWN map places it there too, at the SAME epoch. The second half is
// what makes it safe: a member whose map places a chunk never deletes that
// copy, so the members vouching for a chunk are always members that keep it —
// where "it holds a copy" alone would let two nodes reading different maps
// each vouch for the other and both drop theirs. The epoch keeps the question
// about one map while a change is still reaching the fleet. And a node the map
// does not hold at all deletes no referenced copy: it has just come back and
// not yet been re-added, and what it holds may be the copy the members have
// not repaired yet. Nor does a member on probation, whose copies are most of
// what the map gives it back once it is trusted ([KeepsEveryCopy]).
//
// # The scrub finds rot nobody reads
//
// A chunk is checked against its name whenever it is read, but a chunk nobody
// reads is never read. The scrub ([Node.Scrub]) reads every chunk this node
// holds, in slot order, once every [ScrubInterval], at a rate paced to spread
// the cycle over the week between [ScrubFloor] and [ScrubCeiling], so it never
// competes with serving; its cursor persists in the chunk directory, so a
// restart resumes the week rather than starting it again.
//
// It finds two kinds of bad copy, and both are removed for repair to replace:
// ROTTEN, bytes that read but no longer match their name, and UNREADABLE,
// bytes the disk would not return at all — the latent sector error, which is
// the more common of the two and the one a scrub most exists to find. Neither
// stops it: one bad chunk is counted and stepped past, and only a walk the
// disk refuses — a directory it cannot list — has the scrub try the same
// slots again, since past that it cannot know which chunks there are.
package upkeep
