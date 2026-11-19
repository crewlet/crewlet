package artifacts

import (
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
)

// Authority is what the API decided about the caller of one write, STAMPED
// into the record so the applier decides from the record and its own log
// alone.
//
// # Why the applier does not ask the authority table itself
//
// The identity estate, shares and org chart are other logs applied at
// independent paces, so a role read at apply time would be read at a different
// moment on every node, giving one record two verdicts. Whatever needs IAM or
// the chart (role, actor kind, limits) is decided once, where the write
// arrives, and carried; what is deterministic from the log is the applier's. A
// share revoked between decide and apply lets at most that one write through.
type Authority struct {
	// Actor is who wrote, as iam.ActorFor records them: a seat handle, never
	// a display name, so attribution can be pseudonymised without rewriting
	// a row.
	Actor iam.Actor `json:"actor"`

	// Role is the caller's effective role on the artifact when the API
	// decided: owner for the owner, for the lead of an agent owner and on
	// the admin path.
	Role Role `json:"role"`

	// Elevated is true when the admin path admitted a caller with no
	// relation, which the artifact's own history shows its owner and
	// editors.
	Elevated bool `json:"elevated,omitempty"`

	// Limits are the Tier A quotas in force on the deciding node.
	Limits Limits `json:"limits"`

	// Origin is the surface the write came through.
	Origin WriteOrigin `json:"origin"`

	// WorkItem is the tracker item the turn was charged to, for audit
	// linkage. It confers nothing.
	WorkItem string `json:"work_item,omitempty"`
}

// WriteOrigin is the surface a write came through.
type WriteOrigin string

const (
	// OriginTool is an agent's or an assistant's tool call.
	OriginTool WriteOrigin = "tool"

	// OriginDashboard is the dashboard's own screens.
	OriginDashboard WriteOrigin = "dashboard"

	// OriginBridge is a page script, through the host bridge, as the viewer.
	OriginBridge WriteOrigin = "bridge"

	// OriginAPI is a direct HTTP client: the CLI or a script.
	OriginAPI WriteOrigin = "api"
)

// WriteOrigins are every origin this build records.
var WriteOrigins = []WriteOrigin{OriginTool, OriginDashboard, OriginBridge, OriginAPI}

// Valid reports whether o is an origin this build records.
func (o WriteOrigin) Valid() bool { return slices.Contains(WriteOrigins, o) }
