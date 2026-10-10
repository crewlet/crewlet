package steer

import "github.com/crewlet/crewlet/internal/events/types"

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
// # Asking what a turn IS before offering it anything
//
// Who may steer a turn depends on the turn — the seat it runs, and the work
// item it is charged to (ADR-0022) — and the asker knows neither: the turn
// runs on another node, and the only thing the person sent is its id. So a
// request may DESCRIBE rather than offer: the node running the turn answers
// what it is ([StatusRunning], its seat, its item) and takes nothing, the
// asker decides from its own chart whether this person may, and only then
// offers the note. Two scatters rather than one, because the rule is the
// tool's and the chart it reads is the asker's; handing the running node the
// person's authority to judge would put the rule in two places. The answer
// cannot go stale between the two: a turn id names one run of one seat
// (ADR-0017), and the item it is charged to is fixed at dispatch and never
// changed by the turn.
//
// # Evolution
//
// Additive only, [WireVersion] stamped on both halves. Two builds share this
// wire during a rolling upgrade; an unknown field is ignored and an unknown
// [Status] is a value the asker refuses to act on rather than a decode error.

// WireVersion is the shape this build writes.
const WireVersion = 1

// Request asks the node running a turn to hand it one note — or, with
// Describe, only to say what the turn is.
type Request struct {
	Version int    `json:"version"`
	TurnID  string `json:"turn_id"`
	// Describe asks for the turn's seat and work item and offers nothing:
	// the note fields are empty, and a node answering it takes no note.
	Describe bool   `json:"describe,omitempty"`
	NoteID   string `json:"note_id,omitempty"`
	Note     string `json:"note,omitempty"`
	By       string `json:"by,omitempty"`
	BySeat   string `json:"by_seat,omitempty"`
}

// Reply is the running node's answer. Only the node holding the turn sends
// one.
type Reply struct {
	Version     int    `json:"version"`
	TurnID      string `json:"turn_id"`
	AgentHandle string `json:"agent_handle"`
	Status      Status `json:"status"`
	// WorkItem is the one work item the turn is charged to (ADR-0022),
	// answered to a describe of a running turn — nil for a turn charged to
	// none, which is the ordinary case for a chat wake.
	WorkItem *types.WorkItem `json:"work_item,omitempty"`
}
