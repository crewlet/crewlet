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
)

// RoomKinds are every kind of room this build keeps.
var RoomKinds = []RoomKind{RoomArtifact}

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

// Focus is where in its room a member is looking or editing. An artifact's
// room reads every field: the version on show, a region of a page, and a
// datastore collection, document and field.
type Focus struct {
	Version    int    `json:"version,omitempty"`
	Region     string `json:"region,omitempty"`
	Collection string `json:"collection,omitempty"`
	Doc        string `json:"doc,omitempty"`
	Field      string `json:"field,omitempty"`
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

// Agent is one seat's tool session in a room, as its node touches it
// ([Hub.Touch]): the seat, the turn it is working in, and what it is doing
// there.
type Agent struct {
	Seat     string
	Turn     string
	Activity Activity
	Focus    Focus
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
	case !m.Kind.Valid():
		return fmt.Errorf("presence: a member of kind %q", m.Kind)
	case !m.Activity.Valid():
		return fmt.Errorf("presence: a member doing %q, which is not an activity", m.Activity)
	}
	return CheckFocus(k, m.Focus)
}

// CheckFocus reports why a focus a client stated cannot be shown in a room of
// kind k: a negative version, or a region, collection, document or field longer
// than the vocabulary's own bounds. A focus names where somebody is looking and
// is shown to everybody else in the room, so it is bounded where it enters.
func CheckFocus(k RoomKind, f Focus) error {
	if !k.Valid() {
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

	// maxFrameMembers bounds one frame's members: a full rebroadcast names
	// every member a node holds in one room, and a thousand people on one
	// node in one room is far past the rooms this is built for while still
	// a frame of a few hundred kilobytes.
	maxFrameMembers = 1000
)
