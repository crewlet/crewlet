package presence

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/signedask"
)

// Channel is the key domain every presence frame is sealed under: one of its
// own, so a presence frame never verifies as a state-log record or as any
// instruction channel's envelope, and none of theirs as a presence frame.
const Channel = "artifacts.presence"

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
	V        int          `json:"v"`
	Kind     FrameKind    `json:"kind"`
	Node     string       `json:"node"`
	Artifact artifacts.ID `json:"artifact"`

	Upsert []artifacts.PresenceMember `json:"upsert,omitempty"`
	Leave  []string                   `json:"leave,omitempty"`
	Full   bool                       `json:"full,omitempty"`

	// Until is when the frame stops being believed, inside the seal:
	// [artifacts.PresenceFrameValidity] after it was sealed.
	Until time.Time `json:"until"`
}

// Expires is the instant [signedask.OneWay] judges the frame by.
func (f Frame) Expires() time.Time { return f.Until }

// check reports why a frame the fleet sealed is still not one this build
// applies: a node, an artifact and a kind are what every frame names, and a
// member must carry a session, a valid activity and a focus inside the bounds.
func (f Frame) check() error {
	switch {
	case f.Node == "":
		return fmt.Errorf("presence: a frame names no node")
	case f.Artifact.IsZero():
		return fmt.Errorf("presence: a frame names no artifact")
	case f.Kind != FrameState && f.Kind != FrameAsk:
		return fmt.Errorf("presence: a frame of kind %q is not one this build reads", f.Kind)
	case len(f.Upsert)+len(f.Leave) > maxFrameMembers:
		return fmt.Errorf("presence: a frame names %d members, over the %d one frame may",
			len(f.Upsert)+len(f.Leave), maxFrameMembers)
	}
	for _, m := range f.Upsert {
		if err := checkMember(m); err != nil {
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

// checkMember reports why a member cannot be shown.
func checkMember(m artifacts.PresenceMember) error {
	switch {
	case m.Session == "" || len(m.Session) > maxSessionBytes:
		return fmt.Errorf("presence: a member's session is %d bytes", len(m.Session))
	case len(m.Seat) > maxSeatBytes:
		return fmt.Errorf("presence: a member's seat is %d bytes", len(m.Seat))
	case !m.Kind.Valid():
		return fmt.Errorf("presence: a member of kind %q", m.Kind)
	case !m.Activity.Valid():
		return fmt.Errorf("presence: a member doing %q, which is not an activity", m.Activity)
	}
	return CheckFocus(m.Focus)
}

// CheckFocus reports why a focus a client stated cannot be shown: a negative
// version, or a region, collection, document or field longer than the
// vocabulary's own bounds. A focus names where somebody is looking and is
// shown to everybody else in the room, so it is bounded where it enters.
func CheckFocus(f artifacts.Focus) error {
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

	// maxSeatBytes bounds a seat handle, comfortably over the handle
	// grammar's own length.
	maxSeatBytes = 128

	// maxFrameMembers bounds one frame's members: a full rebroadcast names
	// every member a node holds in one room, and a thousand people on one
	// node in one artifact is far past the rooms this is built for while
	// still a frame of a few hundred kilobytes.
	maxFrameMembers = 1000
)
