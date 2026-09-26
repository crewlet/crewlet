// Package upkeep keeps the fleet's chunks where the placement map says they
// belong: the duty that maintains the map, and the two passes every data node
// runs over its own disk — repair, which fetches what this node should hold
// and does not, and collection, which deletes what nothing needs it to hold.
//
// # The inventory is derived, never kept
//
// Neither pass reads a list of chunks. Every data node holds the whole
// replicated estate, and every row that refers to an object names its chunks
// ([Source]), so which chunks the company references is a query this node
// answers from its own tables — and which of them it should hold is a pure
// function of the map. A list kept beside the estate would be a second answer
// to that question, and the two would drift the first time a write landed in
// one and not the other.
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
// left by an older map, or by a write that went past a silent holder — is
// deleted only when EVERY member this node's map places it on says it holds
// the chunk AND that its OWN map places it there too, at the SAME epoch. The
// second half is what makes it safe: a member whose map places a chunk never
// deletes that copy, so the members vouching for a chunk are always members
// that keep it — where "it holds a copy" alone would let two nodes reading
// different maps each vouch for the other and both drop theirs. The epoch
// keeps the question about one map while a change is still reaching the
// fleet.
package upkeep
