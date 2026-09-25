package steer

// The wire a note crosses to reach the node running its turn.
//
// # Why a scatter, and why only one node answers
//
// The person's request lands on whichever node serves their dashboard, and the
// turn runs on whichever node holds its seat — which nothing on the first node
// knows without asking. So the note is SCATTERED on the queue's ephemeral
// request verb to every node, and the one node whose box the turn id names is
// the one that answers. Every other node answers nothing, which is what a
// scatter's "a server that did not answer" means: it is not an error to be
// reported, it is a node this turn is not on.
//
// Ephemeral rather than a durable publish because a note is only worth
// anything to a turn that is running NOW. A durable note outlives the turn it
// was written for and would have to be discarded by somebody; a scatter that
// nobody answers leaves nothing behind, and the asker says so — `unknown`, not
// a delivery nobody made.
//
// # Evolution
//
// Additive only, [WireVersion] stamped on both halves. Two builds share this
// wire during a rolling upgrade; an unknown field is ignored and an unknown
// [Status] is a value the asker refuses to act on rather than a decode error.

// WireVersion is the shape this build writes.
const WireVersion = 1

// Request asks the node running a turn to hand it one note.
type Request struct {
	Version int    `json:"version"`
	TurnID  string `json:"turn_id"`
	NoteID  string `json:"note_id"`
	Note    string `json:"note"`
	By      string `json:"by"`
	BySeat  string `json:"by_seat,omitempty"`
}

// Reply is the running node's answer. Only the node holding the turn sends
// one.
type Reply struct {
	Version     int    `json:"version"`
	TurnID      string `json:"turn_id"`
	AgentHandle string `json:"agent_handle"`
	Status      Status `json:"status"`
}
