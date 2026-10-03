package steer

import "github.com/crewlet/crewlet/internal/iam"

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
//
// WHO SENT IT travels as [Note]'s three halves — By, ByKind and OperatorID,
// iam.ActorFor's author, kind and credential — so the running node records the
// note exactly as the serving node's guard resolved its sender, and never a
// second "bound seat" field beside the author that could name somebody else.
type Request struct {
	Version    int           `json:"version"`
	TurnID     string        `json:"turn_id"`
	NoteID     string        `json:"note_id"`
	Note       string        `json:"note"`
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`

	// Probe asks WITHOUT offering anything: which seat runs this turn, and
	// could it take a note now. The note fields are empty and ignored.
	//
	// It exists because who may steer a turn is decided on the turn's SEAT
	// — its holder, its lead, or a fleet operator — and the node a person's
	// request reached cannot know that seat until the running node says.
	// Asked on a real offer, the answer would arrive with the note already
	// taken, leaving the authority nothing to decide on. A turn's seat never
	// changes for the life of the turn, so a probe and the offer after it
	// cannot disagree about whose turn it is; a turn that ends between them
	// answers the offer `closed`.
	Probe bool `json:"probe,omitempty"`
}

// Reply is the running node's answer. Only the node holding the turn sends
// one.
//
// Agent is the turn's seat by its derived agent id, which a rename does not
// move, and AgentHandle that seat's handle when the turn was opened, for a
// person reading the answer. An authority decision is taken on Agent; the
// handle is a label. Empty where the running node could not resolve the seat.
type Reply struct {
	Version     int    `json:"version"`
	TurnID      string `json:"turn_id"`
	Agent       string `json:"agent_id,omitempty"`
	AgentHandle string `json:"agent_handle"`
	Status      Status `json:"status"`
}
