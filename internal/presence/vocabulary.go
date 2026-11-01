package presence

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE VOCABULARY every room speaks, whatever it is the presence of. It lived in
// internal/artifacts while an artifact was the only thing with a room, and it
// is here because a room is now named by its KIND and its id ([Room]): what a
// member is, what it can be doing and how a large room is counted are the same
// for every kind, and only where a member can be looking ([Focus]) and what
// names the room belong to the kind.
//
// PRESENCE IS TOLD, NEVER STORED: these values live in the memory of the node
// holding a socket and cross between nodes on an ephemeral channel. Nothing
// here is a row, a log record or a coordination key. "What changed, by whom,
// when" is not presence; it is the durable history.

// RoomKind is what a room is the presence of.
type RoomKind string

const (
	// RoomArtifact is an artifact's room: who has it open, what they are
	// looking at in it and whether they are editing it.
	RoomArtifact RoomKind = "artifact"

	// RoomChat is a chat room's: who has it open, who is typing in it (a
	// socket's member editing, under the thread its focus names), and which
	// seat is working on an answer there (an agent's member, editing under
	// the thread its answer will land in, its [Member.Status] the line the
	// room shows beside its name).
	RoomChat RoomKind = "chat"
)

// RoomKinds are every kind of room this build keeps.
var RoomKinds = []RoomKind{RoomArtifact, RoomChat}

// Valid reports whether k is a kind of room this build keeps.
func (k RoomKind) Valid() bool { return slices.Contains(RoomKinds, k) }

// Room names one room: the kind of thing it is the presence of, and that
// thing's id.
//
// A UUID, because everything with a room has one: it is a subject token as it
// is, so a room's presence subject is built from its value with nothing to
// escape, and a map key with no normalisation to forget.
type Room struct {
	Kind RoomKind  `json:"kind"`
	ID   uuid.UUID `json:"id"`
}

// ArtifactRoom is one artifact's room.
func ArtifactRoom(id artifacts.ID) Room { return Room{Kind: RoomArtifact, ID: uuid.UUID(id)} }

// ChatRoom is one chat room's, by the room's id (every room's id is a UUID:
// a named room's minted from its create, a direct one's derived from who is
// in it).
func ChatRoom(channel uuid.UUID) Room { return Room{Kind: RoomChat, ID: channel} }

// typing reports whether a SOCKET's editing member of a room of kind k stops
// editing on its own once nothing has said so for [TypingLapse].
//
// A CHAT ROOM'S EDITING IS TYPING, a gesture rather than a state: a tab
// re-asserts it while somebody types and says `viewing` when they stop, but a
// lid closed mid-word says nothing, so a typing line nobody re-asserted goes
// back to viewing on its own. An artifact's editor is editing for as long as
// the editor is open, which the tab says.
func (k RoomKind) typing() bool { return k == RoomChat }

// agentSlot is which of a seat's members in a room of kind k a touch focused
// on f is: one per seat in an artifact's room, where the agent's focus moves
// with its calls, and one per seat PER THREAD in a chat room's, because a seat
// can be answering in two threads of one room at once (a turn parked behind an
// executor keeps its line up while the seat's next turn answers elsewhere),
// and taking one down must not take the other with it.
func (k RoomKind) agentSlot(f Focus) string {
	if k == RoomChat {
		return f.Thread
	}
	return ""
}

// String names the room for a log line: its kind and its id.
func (r Room) String() string { return string(r.Kind) + ":" + r.ID.String() }

// check reports why r names no room this build keeps.
func (r Room) check() error {
	switch {
	case !r.Kind.Valid():
		return fmt.Errorf("presence: a room of kind %q, which this build keeps none of", r.Kind)
	case r.ID == uuid.Nil:
		return fmt.Errorf("presence: a %s room names nothing", r.Kind)
	}
	return nil
}

// subject is the room's presence subject: one per room, so a node listens on
// exactly the rooms its sockets are in.
func (r Room) subject() string { return topics.PresenceSubject(string(r.Kind), r.ID) }

// Activity is what a present member is doing.
type Activity string

// The activities a present member can report.
const (
	Viewing Activity = "viewing"
	Editing Activity = "editing"
	Idle    Activity = "idle"
)

// Activities are every activity this build fans out.
var Activities = []Activity{Viewing, Editing, Idle}

// Valid reports whether a is an activity this build fans out.
func (a Activity) Valid() bool { return slices.Contains(Activities, a) }

// Focus is where in its room a member is looking or editing, and which fields
// a room reads is its kind's ([CheckFocus]): an artifact's room the version on
// show, a region of a page, and a datastore collection, document and field; a
// chat room's the thread, the message a member is composing or an agent is
// answering under, and the room itself while it is empty.
//
// ONE TYPE FOR EVERY KIND rather than one per kind, because a member is
// compared whole to tell a change from a repeat, and a focus that was an
// interface would compare by the box it came in.
type Focus struct {
	Version    int    `json:"version,omitempty"`
	Region     string `json:"region,omitempty"`
	Collection string `json:"collection,omitempty"`
	Doc        string `json:"doc,omitempty"`
	Field      string `json:"field,omitempty"`
	Thread     string `json:"thread,omitempty"`
}

// Member is one open viewer of one room.
type Member struct {
	// Session distinguishes two tabs of one person.
	Session string `json:"session"`

	// Seat is the name the member's own record is kept under: the seat handle
	// a person or an agent acts as, which the dashboard resolves to a name,
	// or the login of one acting under no seat, which it shows as written.
	Seat string `json:"seat"`

	// Kind is human or agent: an agent's tool session is present too.
	Kind iam.ActorKind `json:"kind"`

	Activity Activity  `json:"activity"`
	Focus    Focus     `json:"focus"`
	At       time.Time `json:"at"`

	// Turn is, for an agent, the run of the turn that last put it in the
	// room: what a person opens to see what the agent is doing there.
	// Empty for a person.
	Turn string `json:"turn,omitempty"`

	// Status is, for an agent, the line the room shows beside its name
	// while it works there ("is reading the thread…"), empty for the bare
	// indicator. A person's member names none: what a person is doing is
	// their activity.
	Status string `json:"status,omitempty"`
}

// Counts are the members of a large room not presented individually.
type Counts struct {
	Human int `json:"human"`
	Agent int `json:"agent"`
}

// Roster is who is in one room, as a socket joining it is handed it. Past
// [RoomTop] members it presents the most recent ones and counts the rest.
type Roster struct {
	Members   []Member `json:"members"`
	Counts    *Counts  `json:"counts,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// Agent is one seat working in a room, as its node touches it ([Hub.Touch]):
// the seat, the turn it is working in, what it is doing there, and the line
// shown beside it.
type Agent struct {
	Seat     string
	Turn     string
	Activity Activity
	Focus    Focus
	Status   string
}

// The presence timings, every kind's: how often a node tells the fleet who it
// holds, how long a member nobody mentions is believed, and how a large room
// degrades.
const (
	Expiry        = 45 * time.Second // three missed rebroadcasts
	Rebroadcast   = 15 * time.Second
	FrameValidity = 60 * time.Second // inside the seal
	ClientRate    = 10               // client frames a second per socket
	RoomTop       = 50               // members a large room presents individually
	RoomFull      = 40               // below this a large room returns to full rosters

	// CoalesceRate is how many times a second a node fans one room out to
	// its sockets, and BroadcastRate how many times a second it tells the
	// fleet what its own members did there. A cursor crossing nodes waits
	// one broadcast window on its own node and one fan-out window on the
	// viewer's: at 20 and 10 that is at most 150 ms beside the broker's
	// transit, the bound presence is held to, where one window of 200 ms
	// on each side (5 a second for both) measured a median near 240 ms in
	// spike S6. Ten frames a second is also what a socket may send
	// ([ClientRate]).
	CoalesceRate  = 10
	BroadcastRate = 20

	// FrameMembers bounds the members one fan-out re-presents to a socket:
	// the most recently active first, the rest in the frames that follow. A
	// large room presents its fifty most recent members, and when everybody
	// moves, the fifty change constantly; spike S6 measured about 50 KiB a
	// second a socket at 200 viewers moving ten times a second, each frame
	// naming most of the fifty again. Ten members a frame at ten frames a
	// second bounds that near 20 KiB, and a room whose members are still
	// settles within a second.
	FrameMembers = 10

	// TypingLapse is how long a chat room's typing member stays typing with
	// no frame saying so again ([RoomKind.typing]). SIX SECONDS, and it
	// is sized against the tab's re-assertion rather than against latency:
	// a tab re-asserts at most every half of it (the dashboard's
	// TYPING_THROTTLE_MS, three seconds), so one frame lost on the way
	// never blinks the line off under somebody still typing, and a lid
	// closed mid-word takes it down within six.
	TypingLapse = 6 * time.Second
)

// checkMember reports why a member cannot be shown in a room of kind k.
func checkMember(k RoomKind, m Member) error {
	switch {
	case m.Session == "" || len(m.Session) > maxSessionBytes:
		return fmt.Errorf("presence: a member's session is %d bytes", len(m.Session))
	case len(m.Seat) > maxSeatBytes:
		return fmt.Errorf("presence: a member's seat is %d bytes", len(m.Seat))
	case len(m.Turn) > maxTurnBytes:
		return fmt.Errorf("presence: a member's turn is %d bytes", len(m.Turn))
	case m.Turn != "" && m.Kind != iam.ActorAgent:
		return errors.New("presence: a person's member names a turn, which only an agent's does")
	case len(m.Status) > maxStatusBytes:
		return fmt.Errorf("presence: a member's status is %d bytes, over %d", len(m.Status),
			maxStatusBytes)
	case m.Status != "" && m.Kind != iam.ActorAgent:
		return errors.New("presence: a person's member names a status, which only an agent's does")
	case !m.Kind.Valid():
		return fmt.Errorf("presence: a member of kind %q", m.Kind)
	case !m.Activity.Valid():
		return fmt.Errorf("presence: a member doing %q, which is not an activity", m.Activity)
	}
	return CheckFocus(k, m.Focus)
}

// CheckFocus reports why a focus a client stated cannot be shown in a room of
// kind k: a field the kind does not read, a negative version, or a value longer
// than the vocabulary's own bounds. A focus names where somebody is looking and
// is shown to everybody else in the room, so it is bounded where it enters.
func CheckFocus(k RoomKind, f Focus) error {
	switch k {
	case RoomArtifact:
		if f.Thread != "" {
			return errors.New("presence: an artifact's room has no thread to focus on")
		}
	case RoomChat:
		if f != (Focus{Thread: f.Thread}) {
			return errors.New("presence: a chat room's focus names a thread and nothing else")
		}
		if len(f.Thread) > maxThreadBytes {
			return fmt.Errorf("presence: focus thread is %d bytes, over %d", len(f.Thread),
				maxThreadBytes)
		}
		return nil
	default:
		return fmt.Errorf("presence: a focus in a room of kind %q", k)
	}
	switch {
	case f.Version < 0:
		return fmt.Errorf("presence: focus version %d is negative", f.Version)
	case len(f.Region) > maxRegionBytes:
		return fmt.Errorf("presence: focus region is %d bytes, over %d", len(f.Region), maxRegionBytes)
	case len(f.Collection) > artifacts.MaxCollectionNameBytes:
		return fmt.Errorf("presence: focus collection is %d bytes, over %d", len(f.Collection),
			artifacts.MaxCollectionNameBytes)
	case len(f.Doc) > artifacts.MaxDocIDBytes:
		return fmt.Errorf("presence: focus document is %d bytes, over %d", len(f.Doc),
			artifacts.MaxDocIDBytes)
	case len(f.Field) > artifacts.MaxFieldNameBytes:
		return fmt.Errorf("presence: focus field is %d bytes, over %d", len(f.Field),
			artifacts.MaxFieldNameBytes)
	}
	return nil
}

// The bounds a frame is held to. Each is generous against what an honest
// node sends and small against what a frame may weigh.
const (
	// maxRegionBytes bounds a focus region: a heading anchor or an element
	// id is tens of bytes, and a region is shown beside a name.
	maxRegionBytes = 256

	// maxSessionBytes bounds a session id. This build mints 26 characters
	// (crypto/rand's text form); the bound leaves room for another build's.
	maxSessionBytes = 64

	// maxSeatBytes bounds a seat handle or a login, comfortably over both
	// grammars' own length.
	maxSeatBytes = 128

	// maxTurnBytes bounds an agent's turn: a run id is a few dozen bytes.
	maxTurnBytes = 128

	// maxThreadBytes bounds a chat focus's thread: a message id is a UUID's
	// thirty-six characters.
	maxThreadBytes = 64

	// maxStatusBytes bounds an agent's status line, which is read beside a
	// name: the shipped phrases are under forty bytes.
	maxStatusBytes = 256

	// maxFrameMembers bounds one frame's members: a full rebroadcast names
	// every member a node holds in one room, and a thousand people on one
	// node in one room is far past the rooms this is built for while still
	// a frame of a few hundred kilobytes.
	maxFrameMembers = 1000
)
