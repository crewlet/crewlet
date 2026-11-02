package presence

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/signedask"
)

// Channel is the key domain every presence frame is sealed under: one of its
// own, so a presence frame never verifies as a state-log record or as any
// instruction channel's envelope, and none of theirs as a presence frame. ONE
// for every kind of room, because the room a frame is about is inside the seal
// ([Frame.Room]), so a frame about one room can never be opened as another's.
const Channel = "presence"

// Wire is the one-way sealed channel presence frames cross the fleet on. The
// engine builds it once from the fleet keyring
// (`signedask.NewOneWay[presence.Frame](presence.Channel, ring)`), and the hub
// seals what it sends and opens what it hears with it.
type Wire = signedask.OneWay[Frame]

// FrameKind is what one presence frame is.
type FrameKind string

const (
	// FrameState is a node's members of one room that changed (Upsert) or
	// left (Leave). With Full set it is every member the node holds there,
	// which is what the rebroadcast and the join-time answer send: a peer
	// replaces everything it held from that node with it, so a leave that
	// was lost costs one rebroadcast rather than the whole expiry.
	FrameState FrameKind = "state"

	// FrameAsk is a node that gained its first member of a room asking its
	// peers for the members they hold there.
	FrameAsk FrameKind = "ask"
)

// frameVersion is the wire shape this build writes. A frame of a later
// version is still read for the fields this build knows (JSON ignores the
// rest), because presence is a display and a peer one release ahead must not
// empty everybody's rooms.
const frameVersion = 1

// Frame is one presence frame between nodes: always sealed, never stored.
//
// EXPORTED ONLY SO THE ENGINE CAN BUILD THE [Wire] it travels in; nothing but
// this package builds or reads one.
type Frame struct {
	V    int       `json:"v"`
	Kind FrameKind `json:"kind"`
	Node string    `json:"node"`
	Room Room      `json:"room"`

	Upsert []Member `json:"upsert,omitempty"`
	Leave  []string `json:"leave,omitempty"`
	Full   bool     `json:"full,omitempty"`

	// Until is when the frame stops being believed, inside the seal:
	// [FrameValidity] after it was sealed.
	Until time.Time `json:"until"`
}

// Expires is the instant [signedask.OneWay] judges the frame by.
func (f Frame) Expires() time.Time { return f.Until }

// check reports why a frame the fleet sealed is still not one this build
// applies: a node, a room and a kind are what every frame names, and a member
// must carry a session, a valid activity and a focus inside the bounds of the
// room's kind.
func (f Frame) check() error {
	switch {
	case f.Node == "":
		return fmt.Errorf("presence: a frame names no node")
	case f.Kind != FrameState && f.Kind != FrameAsk:
		return fmt.Errorf("presence: a frame of kind %q is not one this build reads", f.Kind)
	case len(f.Upsert)+len(f.Leave) > maxFrameMembers:
		return fmt.Errorf("presence: a frame names %d members, over the %d one frame may",
			len(f.Upsert)+len(f.Leave), maxFrameMembers)
	}
	if err := f.Room.check(); err != nil {
		return err
	}
	for _, m := range f.Upsert {
		if err := checkMember(f.Room.Kind, m); err != nil {
			return err
		}
	}
	for _, s := range f.Leave {
		if s == "" || len(s) > maxSessionBytes {
			return fmt.Errorf("presence: a frame's leave names a session of %d bytes", len(s))
		}
	}
	return nil
}
