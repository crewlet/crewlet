package steer

import (
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// The wire a note crosses to reach the node running its turn.
//
// # Why a scatter, and why only one node answers
//
// The person's request lands on whichever node serves their dashboard, and a
// turn that has not launched its coding run yet runs on whichever node holds
// its seat — which nothing on the first node knows without asking. So the note
// is SCATTERED on the queue's ephemeral request verb to every node, and the one
// node whose box the turn id names is the one that answers. Every other node
// answers nothing, which is what a scatter's "a server that did not answer"
// means: it is not an error to be reported, it is a node this turn is not on.
//
// ONLY A TURN WITH NO RUN RECORD YET. From its coding run's launch on, a turn's
// notes wait on the run's record in the coordination store, which every node
// reads: the asking node files the note there itself, and asks nobody. The one
// node that still answers for such a turn is the one that ran its first segment
// and handed its box to the record — for a note that read "no record" a moment
// before the launch wrote one — and the node that ran the segment that ended
// the turn, for a while, so a note racing the end is told `closed`.
//
// Ephemeral rather than a durable publish because a note is only worth
// anything to a turn that is running NOW. A durable note outlives the turn it
// was written for and would have to be discarded by somebody; a scatter that
// nobody answers leaves nothing behind, and the asker says so — `unknown`, not
// a delivery nobody made.
//
// # Signed, because a note is an instruction
//
// The running node offers a note to the turn AS THE PERSON IT NAMES, and it
// does so on the asking node's word: who may steer a turn is decided where
// the person's request landed, against the seat a probe named, and the
// running node decides nothing about it. So a note anything the fleet's
// broker admitted could write — its admission vouches for a connection,
// never a message (ADR-0087) — would put words in front of a running agent
// under anybody's name. Every request and every reply is therefore a channel
// of internal/signedask's ([Channel]), signed under the fleet keyring in
// domains of its own and bound by an id and a deadline ([Request.Binding])
// that the answer names ([Reply.Answers]). A request the fleet did not sign,
// one past its deadline and one whose id the node has taken already are
// answered by nobody — the asker hears what it hears from a node the turn is
// not on — and one signed under a key the running node does not hold is
// answered [Reply.Unverified], so the person is told the note was not taken
// rather than that nobody knows, and the operator reads in both nodes' logs
// which keys the running one holds.
//
// # Evolution
//
// Additive only, [WireVersion] stamped on both halves — still 1: the status set
// grew [StatusSpent], which an older asker reads as a status it does not know
// and reports as `unknown`, a value and never a decode error. Two builds share
// this wire during a rolling upgrade; an unknown field is ignored and an
// unknown [Status] is a value the asker refuses to act on.

// WireVersion is the shape this build writes.
const WireVersion = 1

// Channel is the steer wire's name among the fleet's signed channels, and so
// the stem of the two key domains a note and its answer are signed under —
// its own, so no estate envelope, broker request or state-log record verifies
// as one.
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
// ADR-0067): a note to a turn whose coding run is out is decided on that seat,
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
