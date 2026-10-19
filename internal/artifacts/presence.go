package artifacts

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// PRESENCE IS TOLD, NEVER STORED: these frames live in the memory of the node
// holding a socket and cross between nodes on an ephemeral channel. Nothing
// here is a row, a log record or a coordination key. "What changed, by whom,
// when" is not presence; it is the durable history.

// PresenceActivity is what a present member is doing.
type PresenceActivity string

// The activities a present member can report.
const (
	PresenceViewing PresenceActivity = "viewing"
	PresenceEditing PresenceActivity = "editing"
	PresenceIdle    PresenceActivity = "idle"
)

// PresenceActivities are every activity this build fans out.
var PresenceActivities = []PresenceActivity{PresenceViewing, PresenceEditing, PresenceIdle}

// Valid reports whether a is an activity this build fans out.
func (a PresenceActivity) Valid() bool { return slices.Contains(PresenceActivities, a) }

// Focus is where in the artifact a member is looking or editing.
type Focus struct {
	Version    int    `json:"version,omitempty"`
	Region     string `json:"region,omitempty"`
	Collection string `json:"collection,omitempty"`
	Doc        string `json:"doc,omitempty"`
	Field      string `json:"field,omitempty"`
}

// PresenceMember is one open viewer of one artifact.
type PresenceMember struct {
	// Session distinguishes two tabs of one person.
	Session string `json:"session"`

	// Seat is the name the member's own record is kept under: the seat handle
	// a person or an agent acts as, which the dashboard resolves to a name,
	// or the login of one acting under no seat, which it shows as written.
	Seat string `json:"seat"`

	// Kind is human or agent: an agent's tool session is present too.
	Kind iam.ActorKind `json:"kind"`

	Activity PresenceActivity `json:"activity"`
	Focus    Focus            `json:"focus"`
	At       time.Time        `json:"at"`

	// Turn is, for an agent, the run of the turn whose tool call last put
	// it in the room: what a person opens to see what the agent is doing
	// there. Empty for a person.
	Turn string `json:"turn,omitempty"`
}

// PresenceCounts are the members of a large room not presented individually.
type PresenceCounts struct {
	Human int `json:"human"`
	Agent int `json:"agent"`
}

// PresenceRoster is who is in one artifact's room. Past [PresenceRoomTop]
// members it presents the most recent ones and counts the rest.
type PresenceRoster struct {
	Artifact  ID               `json:"artifact"`
	Members   []PresenceMember `json:"members"`
	Counts    *PresenceCounts  `json:"counts,omitempty"`
	Truncated bool             `json:"truncated,omitempty"`
}
