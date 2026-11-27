package steer

import (
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// The wire a note crosses to reach the node running its turn.
//
// A scatter, answered by one node: the request lands on whichever node serves
// the dashboard, and a turn with no executor yet runs on whichever holds its
// seat. So the note is scattered on the queue's ephemeral request verb and only
// the node whose box the turn id names answers; silence is a node the turn is
// not on, not an error. Ephemeral because a durable note would outlive its
// turn; the asker says `unknown`, not a delivery nobody made.
//
// Only a turn with no run record yet: from its executor's launch on, notes wait
// on the run's record in the coordination store. The first segment's node, and
// the ending one for a while, still answer a note racing either (`closed`).

// WireVersion is the shape this build writes. Additive only, on both halves,
// still 1: an older asker reads [StatusSpent] as an unknown status
// (`unknown`), never a decode error, across a rolling upgrade; unknown fields
// are ignored.
const WireVersion = 1

// Channel is the steer wire's name among the fleet's signed channels, and so
// the stem of the two key domains a note and its answer are signed under —
// its own, so no estate envelope, broker request or state-log record verifies
// as one.
//
// Signed because a note is an instruction offered AS THE PERSON IT NAMES, on
// the asking node's word. A broker's admission vouches for a connection, never
// a message (ADR-0094), so every request and reply is bound by an id and
// deadline ([Request.Binding]) the answer names ([Reply.Answers]). An unsigned,
// expired or replayed request is answered by nobody; one signed under a key
// the running node lacks is [Reply.Unverified], so the person hears the note
// was not taken.
const Channel = "seat.steer"

// Request asks the node running a turn to hand it one note.
//
// WHO SENT IT travels as [Note]'s three halves — By, ByKind and OperatorID,
// iam.ActorFor's author, kind and credential — so the running node records the
// note exactly as the serving node's guard resolved its sender, and never a
// second "bound seat" field beside the author that could name somebody else.
type Request struct {
	Version int `json:"version"`

	// ID and Deadline bind the request ([Request.Binding]): minted per ask,
	// so a probe and the offer after it are two requests, and a retry of
	// one note is another request carrying the same [Request.NoteID].
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`

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

// Binding is the request's id and deadline, as its envelope binds them
// (internal/signedask).
func (r Request) Binding() (string, time.Time) { return r.ID, r.Deadline }

// Writes is false, an offer included, because no replay of one can outlive
// the process that took it (internal/signedask): a note is offered only to a
// turn the running node's desk holds in memory, every turn runs under a run
// id minted for it, and a restarted desk holds none, so it answers a replayed
// note as not its own. A note filed on a run's record is kept once by its
// NoteID.
func (r Request) Writes() bool { return false }

// Reply is the running node's answer. Only the node holding the turn sends
// one.
//
// Agent is the turn's seat by its derived agent id, and AgentHandle that
// seat's handle, for a
// person reading the answer. An authority decision is taken on Agent; the
// handle is a label. Empty where the running node could not resolve the seat.
type Reply struct {
	Version int `json:"version"`
	// Request is the id of the request this answers ([Reply.Answers]).
	Request     string `json:"request,omitempty"`
	TurnID      string `json:"turn_id"`
	Agent       string `json:"agent_id,omitempty"`
	AgentHandle string `json:"agent_handle"`
	Status      Status `json:"status"`

	// Reads is when an accepted note will be read ([Reads]). Empty from a
	// peer on a build before it, and for a note a turn's first segment took
	// into its box, which its next step reads — [ReadsNextStep] either way.
	Reads Reads `json:"reads,omitempty"`

	// Unverified is set, and Status and the seat are empty, where the
	// running node took nothing because it could not verify the request,
	// signed under a key it does not hold. It names that node and the keys it
	// holds, for the asking node's log and the operator who reads it: an
	// operator adding the key there is what changes the answer, and the
	// person steering is told only that the note was not taken.
	Unverified *Unverified `json:"unverified,omitempty"`
}

// Unverified is the running node's account of a request it could not verify,
// as fields rather than a sentence: the asking node logs them as its own
// (running_node, held_keys), where a sentence the running node wrote about
// "this node" read, in the asking node's log, as the asking node.
type Unverified struct {
	// Node is the id of the node running the turn, which is where the key
	// has to be added.
	Node string `json:"node"`
	// HeldKeys are the keyring keys that node holds, none of which is the
	// one the note is signed under.
	HeldKeys []string `json:"held_keys"`
}

// Answers is the id of the request this reply answers (internal/signedask).
func (r Reply) Answers() string { return r.Request }

// ErrUnverified is a note the running node took nothing of because it could
// not verify it ([Reply.Unverified]): a condition of the fleet rather than of
// the request, cleared only by an operator giving the two nodes the same keys,
// never by a wait. The steer tool refuses on it, which is how every surface
// that tells a person a condition in their own words knows this one.
var ErrUnverified = errors.New("steer: the node running the turn could not verify the note")

// What a run's record says of the seat its turn serves, where its sealed
// anchor names none anybody can vouch for (internal/engine, runsteer.go, and
// ADR-0067): a note to a turn whose executor is out is decided on that seat,
// so such a record decides nothing, and the steer tool words each of these for
// its own readers.
//
// ONLY [ErrSeatKeyUnknown] IS A CONDITION, the record's side of
// [ErrUnverified]: the anchor is sealed under a key the reading node does not
// hold, which an operator giving the fleet's nodes the same keys clears and no
// wait does. A record that names no seat, and an anchor that does not verify,
// stay so: the launch sealed the anchor once.
var (
	// ErrSeatUnnamed is a run's record whose anchor names no seat.
	ErrSeatUnnamed = errors.New("steer: the run's record names no seat")

	// ErrSeatUnverified is an anchor that does not verify under the key it
	// names, or that was sealed for another turn.
	ErrSeatUnverified = errors.New("steer: the seat the run's record names does not verify")

	// ErrSeatKeyUnknown is an anchor sealed under a key the reading node does
	// not hold.
	ErrSeatKeyUnknown = errors.New("steer: the seat the run's record names is sealed " +
		"under a key this node does not hold")
)
